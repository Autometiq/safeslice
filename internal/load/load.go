// Copyright 2026 Autometiq
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

// Package load generates the SQL that puts a slice into a target database.
//
// This is where most subsetting tools quietly break. Four things go wrong on
// real schemas and none of them show up on a toy one:
//
//   - GENERATED ALWAYS AS IDENTITY rejects a plain INSERT.
//   - GENERATED ALWAYS AS (...) STORED cannot appear in a column list at all.
//   - Sequences are left stale, so the application's next insert collides on
//     the primary key. The data looks fine until someone clicks "create".
//   - Cyclic foreign keys need deferral, but SET CONSTRAINTS only works on
//     constraints declared DEFERRABLE, and most schemas do not declare them.
package load

import (
	"fmt"
	"sort"
	"strings"

	"github.com/Autometiq/safeslice/internal/catalog"
	"github.com/Autometiq/safeslice/internal/graph"
)

// Ident quotes an SQL identifier.
func Ident(s string) string { return `"` + strings.ReplaceAll(s, `"`, `""`) + `"` }

// Qualified renders a schema-qualified relation name.
func Qualified(r catalog.Ref) string { return Ident(r.Schema) + "." + Ident(r.Name) }

// InsertColumns lists the columns that may appear in an INSERT.
// Stored generated columns are computed by Postgres and rejected if supplied.
func InsertColumns(t *catalog.Table) []string {
	var out []string
	for _, c := range t.Columns {
		if c.Insertable() {
			out = append(out, c.Name)
		}
	}
	return out
}

// needsOverriding reports whether any column being inserted is GENERATED ALWAYS
// AS IDENTITY. Postgres rejects such an INSERT unless OVERRIDING SYSTEM VALUE
// is given, and we must supply the original ids to keep foreign keys valid.
func needsOverriding(t *catalog.Table, cols []string) bool {
	want := map[string]bool{}
	for _, c := range cols {
		want[c] = true
	}
	for _, c := range t.Columns {
		if want[c.Name] && c.Identity == catalog.IdentityAlways {
			return true
		}
	}
	return false
}

// InsertPrefix builds everything up to the VALUES keyword.
func InsertPrefix(t *catalog.Table, cols []string) string {
	quoted := make([]string, len(cols))
	for i, c := range cols {
		quoted[i] = Ident(c)
	}
	prefix := fmt.Sprintf("INSERT INTO %s (%s)", Qualified(t.Ref), strings.Join(quoted, ", "))
	if needsOverriding(t, cols) {
		prefix += " OVERRIDING SYSTEM VALUE"
	}
	return prefix + " VALUES"
}

// SequenceResets returns one setval per owned sequence, so the application's
// next insert continues after the highest id in the slice instead of colliding
// with it. is_called is false when the table is empty, which makes the next
// value 1 rather than 2.
func SequenceResets(c *catalog.Catalog, refs []catalog.Ref) []string {
	var out []string
	for _, ref := range refs {
		t, ok := c.Table(ref)
		if !ok {
			continue
		}
		seqs := append([]catalog.Sequence{}, t.Sequences...)
		sort.Slice(seqs, func(i, j int) bool { return seqs[i].Ref.String() < seqs[j].Ref.String() })
		for _, s := range seqs {
			name := strings.ReplaceAll(Qualified(s.Ref), "'", "''")
			out = append(out, fmt.Sprintf(
				"SELECT setval('%s', COALESCE(MAX(%s), 1), MAX(%s) IS NOT NULL) FROM %s;",
				name, Ident(s.Column), Ident(s.Column), Qualified(s.Table)))
		}
	}
	return out
}

// DisableTriggers / EnableTriggers stop application triggers from rewriting the
// rows during load. Audit triggers in particular will happily overwrite the
// values that were just masked.
//
// DISABLE TRIGGER USER leaves foreign-key enforcement in place, which is
// deliberate: the whole point is to prove the slice is referentially valid.
func DisableTriggers(refs []catalog.Ref) []string {
	return triggerStmts(refs, "DISABLE")
}

func EnableTriggers(refs []catalog.Ref) []string {
	return triggerStmts(refs, "ENABLE")
}

func triggerStmts(refs []catalog.Ref, verb string) []string {
	out := make([]string, 0, len(refs))
	for _, r := range refs {
		out = append(out, fmt.Sprintf("ALTER TABLE %s %s TRIGGER USER;", Qualified(r), verb))
	}
	return out
}

// Break records a foreign key that must be loaded as NULL and filled in after
// every row exists.
type Break struct {
	Table   catalog.Ref
	Columns []string // set to NULL during INSERT
	PK      []string // used to match rows in the follow-up UPDATE
	Reason  string
}

// CyclePlan describes how a cyclic schema will be loaded.
type CyclePlan struct {
	// Deferred is true when every cycle-participating key is DEFERRABLE, so a
	// single transaction with SET CONSTRAINTS ALL DEFERRED is enough.
	Deferred bool
	// Breaks are the null-then-update fixes needed when deferral is unavailable.
	Breaks []Break
}

// PlanCycles decides how to load a schema whose foreign keys form cycles.
//
// SET CONSTRAINTS ALL DEFERRED is the clean path, but it silently does nothing
// for constraints that were not declared DEFERRABLE — and most ORMs do not
// declare them. When that happens the cycle is broken by inserting the closing
// column as NULL and updating it once all rows are present, which needs no
// superuser and no schema change.
func PlanCycles(c *catalog.Catalog, refs []catalog.Ref, fks []catalog.FK) (CyclePlan, error) {
	groups := graph.Cycles(refs, fks)
	for _, r := range graph.SelfReferences(fks) {
		groups = append(groups, []catalog.Ref{r})
	}
	if len(groups) == 0 {
		return CyclePlan{}, nil
	}

	involved := involvedFKs(groups, fks)
	allDeferrable := len(involved) > 0
	for _, fk := range involved {
		if !fk.Deferrable {
			allDeferrable = false
			break
		}
	}
	if allDeferrable {
		return CyclePlan{Deferred: true}, nil
	}

	plan := CyclePlan{}
	for _, group := range groups {
		fk, err := breakableEdge(c, group, involved)
		if err != nil {
			return CyclePlan{}, err
		}
		t, _ := c.Table(fk.Table)
		var pk []string
		if t != nil {
			pk = t.PK
		}
		if len(pk) == 0 {
			return CyclePlan{}, fmt.Errorf(
				"cannot break the cycle through %s: the table has no primary key, "+
					"so the deferred values cannot be matched back to their rows", fk.Table)
		}
		// Name the constraint that actually blocks deferral, which is often not
		// the edge we chose to break: picking the breakable edge and the
		// blocking edge are independent decisions, and reporting the wrong one
		// sends the reader to a constraint that is already DEFERRABLE.
		plan.Breaks = append(plan.Breaks, Break{
			Table:   fk.Table,
			Columns: fk.Columns,
			PK:      pk,
			Reason:  blockerReason(group, involved),
		})
	}
	sort.Slice(plan.Breaks, func(i, j int) bool {
		return plan.Breaks[i].Table.String() < plan.Breaks[j].Table.String()
	})
	return plan, nil
}

// blockerReason lists the non-deferrable constraints inside one cycle. These
// are what the user must alter if they want the single-pass load instead.
func blockerReason(group []catalog.Ref, involved []catalog.FK) string {
	member := map[string]bool{}
	for _, r := range group {
		member[r.String()] = true
	}
	var names []string
	for _, fk := range involved {
		if !fk.Deferrable && member[fk.Table.String()] && member[fk.RefTable.String()] {
			names = append(names, fk.Name)
		}
	}
	if len(names) == 0 {
		return "cycle cannot be deferred"
	}
	sort.Strings(names)
	verb := " is not DEFERRABLE"
	if len(names) > 1 {
		verb = " are not DEFERRABLE"
	}
	return strings.Join(names, ", ") + verb
}

func involvedFKs(groups [][]catalog.Ref, fks []catalog.FK) []catalog.FK {
	member := map[string]bool{}
	for _, g := range groups {
		for _, r := range g {
			member[r.String()] = true
		}
	}
	var out []catalog.FK
	for _, fk := range fks {
		if member[fk.Table.String()] && member[fk.RefTable.String()] {
			out = append(out, fk)
		}
	}
	return out
}

// breakableEdge picks an edge inside a cycle whose child columns are all
// nullable. A cycle always has at least one such edge in practice — otherwise
// the schema could never have had its first row inserted either.
func breakableEdge(c *catalog.Catalog, group []catalog.Ref, involved []catalog.FK) (catalog.FK, error) {
	member := map[string]bool{}
	for _, r := range group {
		member[r.String()] = true
	}
	var candidates []catalog.FK
	for _, fk := range involved {
		if member[fk.Table.String()] && member[fk.RefTable.String()] && nullable(c, fk) {
			candidates = append(candidates, fk)
		}
	}
	if len(candidates) == 0 {
		names := make([]string, len(group))
		for i, r := range group {
			names[i] = r.String()
		}
		return catalog.FK{}, fmt.Errorf(
			"tables %s form a foreign-key cycle with no DEFERRABLE and no nullable key; "+
				"declare one of the constraints DEFERRABLE, or exclude one of these tables",
			strings.Join(names, ", "))
	}
	sort.Slice(candidates, func(i, j int) bool { return candidates[i].Name < candidates[j].Name })
	return candidates[0], nil
}

func nullable(c *catalog.Catalog, fk catalog.FK) bool {
	t, ok := c.Table(fk.Table)
	if !ok {
		return false
	}
	for _, name := range fk.Columns {
		col, ok := t.Column(name)
		if !ok || col.NotNull {
			return false
		}
	}
	return true
}

// UpdatePrefix builds the statement that restores the columns a Break nulled
// out, once every referenced row exists.
//
//	UPDATE "s"."companies" AS tgt SET "owner_id" = src."owner_id"
//	FROM (VALUES ...) AS src("id", "owner_id") WHERE tgt."id" = src."id";
//
// The caller supplies the VALUES rows in PK-then-column order.
func (b Break) UpdatePrefix() string {
	sets := make([]string, len(b.Columns))
	for i, c := range b.Columns {
		sets[i] = fmt.Sprintf("%s = src.%s", Ident(c), Ident(c))
	}
	cols := make([]string, 0, len(b.PK)+len(b.Columns))
	for _, c := range append(append([]string{}, b.PK...), b.Columns...) {
		cols = append(cols, Ident(c))
	}
	return fmt.Sprintf("UPDATE %s AS tgt SET %s FROM (VALUES",
		Qualified(b.Table), strings.Join(sets, ", "))
}

// UpdateSuffix closes the statement opened by UpdatePrefix.
func (b Break) UpdateSuffix() string {
	cols := make([]string, 0, len(b.PK)+len(b.Columns))
	for _, c := range append(append([]string{}, b.PK...), b.Columns...) {
		cols = append(cols, Ident(c))
	}
	match := make([]string, len(b.PK))
	for i, c := range b.PK {
		match[i] = fmt.Sprintf("tgt.%s = src.%s", Ident(c), Ident(c))
	}
	return fmt.Sprintf(") AS src(%s) WHERE %s;", strings.Join(cols, ", "), strings.Join(match, " AND "))
}

// Nulled reports the columns this Break replaces with NULL during INSERT.
func (b Break) Nulled() map[string]bool {
	out := map[string]bool{}
	for _, c := range b.Columns {
		out[c] = true
	}
	return out
}
