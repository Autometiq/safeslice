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

// Package extract walks the foreign-key graph, collects the rows that belong in
// the slice, masks them, and hands them to a sink.
//
// The whole traversal runs inside one REPEATABLE READ transaction. Without
// that, a parent row can be deleted between the query that found a child and
// the query that fetches the parent, and the slice then fails foreign-key
// validation on restore for reasons nobody can reproduce afterwards.
package extract

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/Autometiq/safeslice/internal/catalog"
	"github.com/Autometiq/safeslice/internal/graph"
	"github.com/Autometiq/safeslice/internal/keyset"
	"github.com/Autometiq/safeslice/internal/load"
	"github.com/Autometiq/safeslice/internal/mask"
)

// RowWriter receives the masked rows of one table.
type RowWriter interface {
	Write(ctx context.Context, row []any) error
	Close(ctx context.Context) error
}

// Sink is where a slice ends up: a SQL file, or a target database.
type Sink interface {
	Open(ctx context.Context, t *catalog.Table, cols []string) (RowWriter, error)
	// Deferred receives the rows for a cycle-breaking UPDATE, in PK-then-value
	// order, once every table has been written.
	Deferred(ctx context.Context, b load.Break, rows [][]any) error
	Close(ctx context.Context) error
}

type Options struct {
	Root       catalog.Ref
	Where      string
	Limit      int
	ChildDepth int
	// ChildLimit caps how many child rows a single parent batch may pull in.
	// Without it one popular row drags in a whole table.
	ChildLimit int
	BatchSize  int
	Seed       string
	Classifier mask.Classifier
	// Progress is called after each table is written.
	Progress func(table catalog.Ref, rows int)
	// Discovered is called as the graph walk finds rows, so a traversal that
	// takes minutes on a large database is visibly making headway rather than
	// looking hung.
	Discovered func(table catalog.Ref, keys, tables int)
	// Extracting is called periodically while a table streams, with the running
	// row count. One big table would otherwise print nothing until it finished.
	Extracting func(table catalog.Ref, rows int)
}

// progressEvery bounds how often streaming reports back.
const progressEvery = 500

func (o *Options) defaults() {
	if o.BatchSize <= 0 {
		o.BatchSize = 500
	}
	if o.ChildLimit <= 0 {
		o.ChildLimit = 100_000
	}
	if o.Limit <= 0 {
		o.Limit = 1000
	}
}

type Extractor struct {
	tx    pgx.Tx
	cat   *catalog.Catalog
	graph *graph.Graph
	keys  *keyset.Store
	fks   []catalog.FK
	opt   Options
	found int             // rows discovered so far, for progress
	seen  map[string]bool // tables touched so far, for progress
}

// Begin opens the consistent read transaction the whole extraction runs in.
// The caller keeps the returned Extractor until Close.
func Begin(ctx context.Context, conn *pgx.Conn, cat *catalog.Catalog, virtual []catalog.FK,
	keys *keyset.Store, opt Options) (*Extractor, error) {
	opt.defaults()
	tx, err := conn.BeginTx(ctx, pgx.TxOptions{
		IsoLevel:   pgx.RepeatableRead,
		AccessMode: pgx.ReadOnly,
	})
	if err != nil {
		return nil, fmt.Errorf("extract: opening the consistent snapshot: %w", err)
	}
	g := graph.New(cat, virtual)
	return &Extractor{
		tx:    tx,
		cat:   cat,
		graph: g,
		keys:  keys,
		seen:  map[string]bool{},
		// Partition-normalised and deduplicated: ordering off the raw catalog
		// list would reintroduce partitions as independent tables.
		fks: g.Edges(),
		opt: opt,
	}, nil
}

// Close ends the read transaction. It never commits: nothing was written.
func (e *Extractor) Close(ctx context.Context) error { return e.tx.Rollback(ctx) }

// Collect walks the graph and fills the key store.
//
// Parents are followed unconditionally and transitively: a row cannot be
// inserted without the rows it references. Children are optional context and
// stop at ChildDepth, because following them without a bound reaches the whole
// database through any popular table.
// report notifies the caller that the walk found rows, for live progress.
func (e *Extractor) report(ref catalog.Ref, n int) {
	if e.opt.Discovered == nil || n == 0 {
		return
	}
	e.found += n
	e.seen[ref.String()] = true
	e.opt.Discovered(ref, e.found, len(e.seen))
}

func (e *Extractor) Collect(ctx context.Context) error {
	root := e.graph.ReadTarget(e.opt.Root)
	rootKeys, err := e.rootKeys(ctx, root)
	if err != nil {
		return err
	}
	if len(rootKeys) == 0 {
		return fmt.Errorf("extract: no rows matched in %s; check --where", root)
	}

	type work struct {
		ref   catalog.Ref
		keys  []keyset.Key
		depth int
	}
	fresh, err := e.keys.Add(root.String(), rootKeys)
	if err != nil {
		return err
	}
	e.report(root, len(fresh))
	queue := []work{{root, fresh, 0}}

	for len(queue) > 0 {
		w := queue[0]
		queue = queue[1:]

		for _, fk := range e.graph.Parents[w.ref.String()] {
			found, err := e.parentKeys(ctx, fk, w.keys)
			if err != nil {
				return err
			}
			// Only genuinely new keys are enqueued. That is what turns a cyclic
			// schema into a fixpoint instead of an infinite walk.
			newKeys, err := e.keys.Add(fk.RefTable.String(), found)
			if err != nil {
				return err
			}
			if len(newKeys) > 0 {
				e.report(fk.RefTable, len(newKeys))
				queue = append(queue, work{fk.RefTable, newKeys, w.depth})
			}
		}

		if w.depth >= e.opt.ChildDepth {
			continue
		}
		for _, fk := range e.graph.Children[w.ref.String()] {
			found, err := e.childKeys(ctx, fk, w.keys)
			if err != nil {
				return err
			}
			newKeys, err := e.keys.Add(fk.Table.String(), found)
			if err != nil {
				return err
			}
			if len(newKeys) > 0 {
				e.report(fk.Table, len(newKeys))
				queue = append(queue, work{fk.Table, newKeys, w.depth + 1})
			}
		}
	}
	return nil
}

func (e *Extractor) pk(ref catalog.Ref) ([]string, error) {
	t, ok := e.cat.Table(ref)
	if !ok {
		return nil, fmt.Errorf("extract: table %s is not in the catalog", ref)
	}
	if len(t.PK) == 0 {
		return nil, fmt.Errorf("extract: table %s has no primary key, so its rows cannot be "+
			"identified; exclude it or add one", ref)
	}
	return t.PK, nil
}

func (e *Extractor) rootKeys(ctx context.Context, root catalog.Ref) ([]keyset.Key, error) {
	pk, err := e.pk(root)
	if err != nil {
		return nil, err
	}
	q := fmt.Sprintf("SELECT %s FROM %s", cols("t", pk), load.Qualified(root))
	if e.opt.Where != "" {
		q += " WHERE " + e.opt.Where
	}
	q += fmt.Sprintf(" LIMIT %d", e.opt.Limit)
	// The alias makes `cols` uniform; the predicate stays unqualified so a user
	// can write --where "id = 42" and have it mean what they expect.
	q = strings.Replace(q, cols("t", pk), cols("", pk), 1)
	return e.scanKeys(ctx, q, nil)
}

// parentKeys resolves, for a batch of child rows, the primary keys of the rows
// they reference. The join handles the case where a foreign key points at a
// UNIQUE column rather than the primary key.
func (e *Extractor) parentKeys(ctx context.Context, fk catalog.FK, keys []keyset.Key) ([]keyset.Key, error) {
	parentPK, err := e.pk(fk.RefTable)
	if err != nil {
		return nil, err
	}
	childPK, err := e.pk(fk.Table)
	if err != nil {
		return nil, err
	}
	var out []keyset.Key
	err = e.eachBatch(keys, func(batch []keyset.Key) error {
		where, args := inClause("c", childPK, batch, 1)
		notNull := make([]string, len(fk.Columns))
		for i, c := range fk.Columns {
			notNull[i] = fmt.Sprintf("c.%s IS NOT NULL", load.Ident(c))
		}
		q := fmt.Sprintf("SELECT DISTINCT %s FROM %s AS p JOIN %s ON %s WHERE %s AND %s",
			cols("p", parentPK), load.Qualified(fk.RefTable), childRelation(fk),
			joinOn("c", fk.Columns, "p", fk.RefColumns), where, strings.Join(notNull, " AND "))
		found, err := e.scanKeys(ctx, q, args)
		if err != nil {
			return err
		}
		out = append(out, found...)
		return nil
	})
	return out, err
}

// childKeys finds rows referencing a batch of parent rows.
func (e *Extractor) childKeys(ctx context.Context, fk catalog.FK, keys []keyset.Key) ([]keyset.Key, error) {
	childPK, err := e.pk(fk.Table)
	if err != nil {
		return nil, err
	}
	parentPK, err := e.pk(fk.RefTable)
	if err != nil {
		return nil, err
	}
	var out []keyset.Key
	err = e.eachBatch(keys, func(batch []keyset.Key) error {
		where, args := inClause("p", parentPK, batch, 1)
		q := fmt.Sprintf("SELECT DISTINCT %s FROM %s JOIN %s AS p ON %s WHERE %s LIMIT %d",
			cols("c", childPK), childRelation(fk), load.Qualified(fk.RefTable),
			joinOn("c", fk.Columns, "p", fk.RefColumns), where, e.opt.ChildLimit)
		found, err := e.scanKeys(ctx, q, args)
		if err != nil {
			return err
		}
		out = append(out, found...)
		return nil
	})
	return out, err
}

// childRelation renders the child side of a join. A virtual key carries a
// predicate (commentable_type = 'Post'), and wrapping the table in a subquery
// lets that predicate stay unqualified, which is how a user would write it.
func childRelation(fk catalog.FK) string {
	if fk.When == "" {
		return load.Qualified(fk.Table) + " AS c"
	}
	return fmt.Sprintf("(SELECT * FROM %s WHERE %s) AS c", load.Qualified(fk.Table), fk.When)
}

func (e *Extractor) eachBatch(keys []keyset.Key, fn func([]keyset.Key) error) error {
	for i := 0; i < len(keys); i += e.opt.BatchSize {
		end := min(i+e.opt.BatchSize, len(keys))
		if err := fn(keys[i:end]); err != nil {
			return err
		}
	}
	return nil
}

func (e *Extractor) scanKeys(ctx context.Context, q string, args []any) ([]keyset.Key, error) {
	rows, err := e.tx.Query(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("extract: %w\nquery: %s", err, q)
	}
	defer rows.Close()
	var out []keyset.Key
	for rows.Next() {
		vals, err := rows.Values()
		if err != nil {
			return nil, err
		}
		out = append(out, keyset.Key(vals))
	}
	return out, rows.Err()
}

// Stream writes every collected table to the sink, in an order that satisfies
// foreign keys, masking values as they pass through.
func (e *Extractor) Stream(ctx context.Context, sink Sink, plan load.CyclePlan) error {
	tables, err := e.collectedRefs()
	if err != nil {
		return err
	}
	order := graph.TopoOrder(tables, e.fks)

	breaks := map[string]load.Break{}
	for _, b := range plan.Breaks {
		breaks[b.Table.String()] = b
	}
	deferred := map[string][][]any{}

	for _, ref := range order {
		rows, err := e.streamTable(ctx, sink, ref, breaks, deferred)
		if err != nil {
			return err
		}
		if e.opt.Progress != nil {
			e.opt.Progress(ref, rows)
		}
	}

	// Cycle-breaking updates run last, when every referenced row exists.
	for _, b := range plan.Breaks {
		if rows := deferred[b.Table.String()]; len(rows) > 0 {
			if err := sink.Deferred(ctx, b, rows); err != nil {
				return err
			}
		}
	}
	return nil
}

func (e *Extractor) collectedRefs() ([]catalog.Ref, error) {
	names, err := e.keys.Tables()
	if err != nil {
		return nil, err
	}
	out := make([]catalog.Ref, 0, len(names))
	for _, n := range names {
		schema, name, _ := strings.Cut(n, ".")
		out = append(out, catalog.Ref{Schema: schema, Name: name})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].String() < out[j].String() })
	return out, nil
}

func (e *Extractor) streamTable(ctx context.Context, sink Sink, ref catalog.Ref,
	breaks map[string]load.Break, deferred map[string][][]any) (int, error) {
	t, ok := e.cat.Table(ref)
	if !ok {
		return 0, fmt.Errorf("extract: table %s is not in the catalog", ref)
	}
	insertCols := load.InsertColumns(t)
	w, err := sink.Open(ctx, t, insertCols)
	if err != nil {
		return 0, err
	}
	defer w.Close(ctx)

	m := mask.Masker{Seed: e.opt.Seed}
	keyCols := e.cat.KeyColumns(ref)
	uniqueCols := mask.UniqueColumns(t)
	uniques := map[string]*mask.UniqueSet{}
	rules := map[string]mask.Rule{}
	for _, name := range insertCols {
		if keyCols[name] {
			continue // never mask a key: it is what holds the slice together
		}
		if r := e.opt.Classifier.Rule(ref, name); r != mask.Keep {
			rules[name] = r
			if uniqueCols[name] {
				col, _ := t.Column(name)
				// Fail before reading a row: a rule that cannot satisfy the
				// constraint fails the load either way, and it is far cheaper
				// to say so now.
				if err := mask.SatisfiesUnique(r, col); err != nil {
					return 0, fmt.Errorf("extract: %s.%s: %w", ref, name, err)
				}
				uniques[name] = mask.NewUniqueSet()
			}
		}
	}

	br, breaking := breaks[ref.String()]
	nulled := map[string]bool{}
	if breaking {
		nulled = br.Nulled()
	}

	pk := t.PK
	count := 0
	err = e.keys.Each(ref.String(), e.opt.BatchSize, func(batch []keyset.Key) error {
		where, args := inClause("t", pk, batch, 1)
		q := fmt.Sprintf("SELECT %s FROM %s AS t WHERE %s",
			cols("t", insertCols), load.Qualified(ref), where)
		rows, err := e.tx.Query(ctx, q, args...)
		if err != nil {
			return fmt.Errorf("extract: reading %s: %w", ref, err)
		}
		defer rows.Close()
		for rows.Next() {
			vals, err := rows.Values()
			if err != nil {
				return err
			}
			out, held, err := e.maskRow(t, insertCols, vals, rules, uniques, m, nulled, pk, breaking, br)
			if err != nil {
				return err
			}
			if held != nil {
				deferred[ref.String()] = append(deferred[ref.String()], held)
			}
			if err := w.Write(ctx, out); err != nil {
				return err
			}
			count++
			// Throttled: a callback per row would cost more than the copy.
			if e.opt.Extracting != nil && count%progressEvery == 0 {
				e.opt.Extracting(ref, count)
			}
		}
		return rows.Err()
	})
	if err != nil {
		return count, err
	}
	return count, w.Close(ctx)
}

// maskRow applies the masking rules and, when this table is a cycle break,
// replaces the closing foreign key with NULL while keeping the real value for
// the follow-up UPDATE.
func (e *Extractor) maskRow(t *catalog.Table, colNames []string, vals []any,
	rules map[string]mask.Rule, uniques map[string]*mask.UniqueSet, m mask.Masker,
	nulled map[string]bool, pk []string, breaking bool, br load.Break) ([]any, []any, error) {

	out := make([]any, len(vals))
	copy(out, vals)
	index := map[string]int{}
	for i, name := range colNames {
		index[name] = i
	}

	for i, name := range colNames {
		rule, ok := rules[name]
		if !ok {
			continue
		}
		col, _ := t.Column(name)
		if u := uniques[name]; u != nil {
			// A unique index turns a hash collision into a failed load, so retry
			// with a salt until the generated value is free.
			v, err := u.Ensure(func(salt int) (*string, error) {
				got, err := m.Apply(rule, col, vals[i], salt)
				if err != nil || got == nil {
					return nil, err
				}
				s := fmt.Sprintf("%v", got)
				return &s, nil
			})
			if err != nil {
				return nil, nil, fmt.Errorf("masking %s.%s: %w", t.Ref, name, err)
			}
			if v == nil {
				out[i] = nil
				continue
			}
			typed, err := m.Apply(rule, col, vals[i], 0)
			if err != nil {
				return nil, nil, err
			}
			if fmt.Sprintf("%v", typed) != *v {
				typed = any(*v) // a salted retry won; keep the value that is unique
			}
			out[i] = typed
			continue
		}
		v, err := m.Apply(rule, col, vals[i], 0)
		if err != nil {
			return nil, nil, fmt.Errorf("masking %s.%s: %w", t.Ref, name, err)
		}
		out[i] = v
	}

	if !breaking {
		return out, nil, nil
	}
	held := make([]any, 0, len(pk)+len(br.Columns))
	anySet := false
	for _, name := range pk {
		held = append(held, out[index[name]])
	}
	for _, name := range br.Columns {
		i := index[name]
		held = append(held, out[i])
		if out[i] != nil {
			anySet = true
		}
		if nulled[name] {
			out[i] = nil
		}
	}
	if !anySet {
		return out, nil, nil // nothing to restore afterwards
	}
	return out, held, nil
}

// cols renders a qualified column list.
func cols(alias string, names []string) string {
	out := make([]string, len(names))
	for i, n := range names {
		if alias == "" {
			out[i] = load.Ident(n)
		} else {
			out[i] = alias + "." + load.Ident(n)
		}
	}
	return strings.Join(out, ", ")
}

func joinOn(a string, aCols []string, b string, bCols []string) string {
	parts := make([]string, len(aCols))
	for i := range aCols {
		parts[i] = fmt.Sprintf("%s.%s = %s.%s", a, load.Ident(aCols[i]), b, load.Ident(bCols[i]))
	}
	return strings.Join(parts, " AND ")
}

// inClause builds a positional IN clause for single or composite keys.
// Placeholders are used throughout: key values come from the database, but a
// literal-concatenating query would still be one bad column name away from
// injection, and placeholders cost nothing.
func inClause(alias string, keyCols []string, keys []keyset.Key, start int) (string, []any) {
	var args []any
	tuples := make([]string, 0, len(keys))
	n := start
	for _, k := range keys {
		holes := make([]string, len(keyCols))
		for i := range keyCols {
			holes[i] = fmt.Sprintf("$%d", n)
			n++
			if i < len(k) {
				args = append(args, k[i])
			} else {
				args = append(args, nil)
			}
		}
		tuples = append(tuples, "("+strings.Join(holes, ", ")+")")
	}
	return fmt.Sprintf("(%s) IN (%s)", cols(alias, keyCols), strings.Join(tuples, ", ")), args
}
