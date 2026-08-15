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

package graph

import (
	"fmt"
	"sort"
	"strings"

	"github.com/Autometiq/safeslice/internal/catalog"
)

// Relationships the database does not enforce.
//
// This is the single most common reason a slice comes out looking incomplete.
// Rails polymorphic associations and Django generic relations have no
// pg_constraint row, and plenty of teams simply never added the foreign key --
// so the graph cannot see the relationship, follows nothing, and the child rows
// arrive without their parents.
//
// Nothing here is acted on automatically. A guess is a guess, and silently
// following one would produce a slice whose shape nobody can explain. They are
// reported so a human can declare the ones that matter as virtual_keys.

// Soft is a column that looks like a foreign key but has no constraint.
type Soft struct {
	Table  catalog.Ref
	Column string
	// Guess is the table the name suggests, if one exists in the catalog.
	Guess catalog.Ref
	// TypeColumn is the companion discriminator of a polymorphic association,
	// e.g. commentable_type beside commentable_id.
	TypeColumn string
}

func (s Soft) String() string {
	out := s.Table.Name + "." + s.Column
	switch {
	case s.TypeColumn != "":
		out += fmt.Sprintf(" (polymorphic, with %s)", s.TypeColumn)
	case s.Guess.Name != "":
		out += " → " + s.Guess.Name + "?"
	}
	return out + " — no foreign key"
}

// SoftKeys reports columns that name a relationship the schema does not
// declare. fks should include any virtual keys already configured, so a
// relationship the user has already described stops being reported.
func SoftKeys(cat *catalog.Catalog, fks []catalog.FK) []Soft {
	covered := map[string]bool{}
	for _, fk := range fks {
		for _, c := range fk.Columns {
			covered[fk.Table.String()+"."+c] = true
		}
	}

	var out []Soft
	for _, ref := range cat.Refs() {
		t, ok := cat.Table(ref)
		if !ok || t.Partition {
			continue // a partition shares its parent's columns
		}
		pk := map[string]bool{}
		for _, c := range t.PK {
			pk[c] = true
		}
		for _, col := range t.Columns {
			name := strings.ToLower(col.Name)
			base, isRef := strings.CutSuffix(name, "_id")
			if !isRef || base == "" || pk[col.Name] || covered[ref.String()+"."+col.Name] {
				continue
			}
			s := Soft{Table: ref, Column: col.Name}
			if _, ok := t.Column(base + "_type"); ok {
				s.TypeColumn = base + "_type"
			}
			s.Guess = guessTarget(cat, ref.Schema, base)
			out = append(out, s)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Table != out[j].Table {
			return out[i].Table.String() < out[j].Table.String()
		}
		return out[i].Column < out[j].Column
	})
	return out
}

// guessTarget looks for the table a column name points at. Only the two forms
// that are actually conventions are tried; anything cleverer would produce
// confident nonsense.
func guessTarget(cat *catalog.Catalog, schema, base string) catalog.Ref {
	for _, name := range []string{base + "s", base, base + "es"} {
		ref := catalog.Ref{Schema: schema, Name: name}
		if _, ok := cat.Table(ref); ok {
			return ref
		}
	}
	return catalog.Ref{}
}
