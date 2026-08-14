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

// Package graph turns the foreign keys in a catalog into the traversal order
// and dependency structure the extractor and loader need.
//
// Three shapes have to survive here, and all three occur in ordinary schemas:
// cycles (users <-> companies), self-references (employees.manager_id), and
// relationships the database does not know about at all (Rails polymorphic
// associations), which arrive as virtual keys from config.
package graph

import (
	"sort"
	"strings"

	"github.com/Autometiq/safeslice/internal/catalog"
)

type Graph struct {
	// Parents maps a table to the foreign keys it declares. Every one of these
	// must be followed: a row cannot be inserted without its referenced rows.
	Parents map[string][]catalog.FK
	// Children maps a table to the foreign keys pointing at it. Following these
	// is optional context and must be bounded, or the walk covers the database.
	Children map[string][]catalog.FK

	catalog *catalog.Catalog
}

// New builds the graph from real foreign keys plus any virtual keys declared in
// config. Self-referencing keys appear in both directions, which is correct:
// a row's parent and its children both live in the same table.
func New(c *catalog.Catalog, virtual []catalog.FK) *Graph {
	g := &Graph{
		Parents:  map[string][]catalog.FK{},
		Children: map[string][]catalog.FK{},
		catalog:  c,
	}
	all := append(append([]catalog.FK{}, c.FKs...), virtual...)
	sort.SliceStable(all, func(i, j int) bool { return all[i].Name < all[j].Name })
	seen := map[string]bool{}
	for _, fk := range all {
		// A foreign key declared on a partitioned table is recorded again on
		// every partition. Left alone, the traversal collects the same rows
		// under both names and the load inserts them twice -- which surfaces as
		// a duplicate-key error on the partition's primary key, a long way from
		// the actual cause.
		fk.Table = g.ReadTarget(fk.Table)
		fk.RefTable = g.ReadTarget(fk.RefTable)
		sig := fk.Table.String() + "(" + strings.Join(fk.Columns, ",") + ")->" +
			fk.RefTable.String() + "(" + strings.Join(fk.RefColumns, ",") + ")"
		if seen[sig] {
			continue
		}
		seen[sig] = true
		g.Parents[fk.Table.String()] = append(g.Parents[fk.Table.String()], fk)
		g.Children[fk.RefTable.String()] = append(g.Children[fk.RefTable.String()], fk)
	}
	return g
}

// Edges returns the deduplicated, partition-normalised foreign keys. Callers
// that order or stream tables must use these rather than the raw catalog list,
// or partitions reappear as separate tables.
func (g *Graph) Edges() []catalog.FK {
	seen := map[string]bool{}
	var out []catalog.FK
	for _, fks := range g.Parents {
		for _, fk := range fks {
			if !seen[fk.Name] {
				seen[fk.Name] = true
				out = append(out, fk)
			}
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// ReadTarget resolves a partition to its parent table. Reading a partition
// directly is legal but yields an incomplete slice, because sibling partitions
// hold the rest of the rows for the same logical table.
func (g *Graph) ReadTarget(ref catalog.Ref) catalog.Ref {
	if g.catalog == nil {
		return ref
	}
	seen := map[string]bool{}
	for {
		t, ok := g.catalog.Table(ref)
		if !ok || !t.Partition || seen[ref.String()] {
			return ref
		}
		seen[ref.String()] = true
		ref = t.Parent
	}
}

// TopoOrder returns tables with parents before children, so a plain sequential
// load satisfies foreign keys without deferring anything.
//
// Cycles cannot be ordered by definition; they are broken deterministically by
// name and the loader relies on SET CONSTRAINTS ALL DEFERRED to make the
// transaction valid. Self-references are ignored for ordering, since a table
// cannot precede itself.
func TopoOrder(tables []catalog.Ref, fks []catalog.FK) []catalog.Ref {
	present := map[string]catalog.Ref{}
	for _, t := range tables {
		present[t.String()] = t
	}
	deps := map[string]map[string]bool{}
	for k := range present {
		deps[k] = map[string]bool{}
	}
	for _, fk := range fks {
		child, parent := fk.Table.String(), fk.RefTable.String()
		if child == parent {
			continue
		}
		if _, ok := present[child]; !ok {
			continue
		}
		if _, ok := present[parent]; !ok {
			continue
		}
		deps[child][parent] = true
	}

	names := make([]string, 0, len(present))
	for k := range present {
		names = append(names, k)
	}
	sort.Strings(names)

	done := map[string]bool{}
	out := make([]catalog.Ref, 0, len(names))
	for len(out) < len(names) {
		var ready []string
		for _, n := range names {
			if done[n] {
				continue
			}
			if satisfied(deps[n], done) {
				ready = append(ready, n)
			}
		}
		if len(ready) == 0 {
			// Everything left is in a cycle. Break it at the first name so the
			// result stays deterministic across runs.
			for _, n := range names {
				if !done[n] {
					ready = []string{n}
					break
				}
			}
		}
		for _, n := range ready {
			out = append(out, present[n])
			done[n] = true
		}
	}
	return out
}

func satisfied(deps, done map[string]bool) bool {
	for d := range deps {
		if !done[d] {
			return false
		}
	}
	return true
}

// Cycles returns each group of two or more tables that reference each other,
// sorted for stable output. The loader must defer constraints when any exist,
// and `safeslice plan` reports them so the schema owner is not surprised.
func Cycles(tables []catalog.Ref, fks []catalog.FK) [][]catalog.Ref {
	present := map[string]catalog.Ref{}
	for _, t := range tables {
		present[t.String()] = t
	}
	adj := map[string][]string{}
	for _, fk := range fks {
		child, parent := fk.Table.String(), fk.RefTable.String()
		if child == parent {
			continue
		}
		if _, ok := present[child]; !ok {
			continue
		}
		if _, ok := present[parent]; !ok {
			continue
		}
		adj[child] = append(adj[child], parent)
	}

	// Tarjan's strongly connected components.
	var (
		index   = map[string]int{}
		low     = map[string]int{}
		onStack = map[string]bool{}
		stack   []string
		next    int
		out     [][]catalog.Ref
	)
	names := make([]string, 0, len(present))
	for k := range present {
		names = append(names, k)
	}
	sort.Strings(names)

	var strongConnect func(string)
	strongConnect = func(v string) {
		index[v], low[v] = next, next
		next++
		stack = append(stack, v)
		onStack[v] = true

		succ := append([]string{}, adj[v]...)
		sort.Strings(succ)
		for _, w := range succ {
			if _, seen := index[w]; !seen {
				strongConnect(w)
				if low[w] < low[v] {
					low[v] = low[w]
				}
			} else if onStack[w] && index[w] < low[v] {
				low[v] = index[w]
			}
		}
		if low[v] != index[v] {
			return
		}
		var group []catalog.Ref
		for {
			w := stack[len(stack)-1]
			stack = stack[:len(stack)-1]
			onStack[w] = false
			group = append(group, present[w])
			if w == v {
				break
			}
		}
		if len(group) > 1 {
			sort.Slice(group, func(i, j int) bool { return group[i].String() < group[j].String() })
			out = append(out, group)
		}
	}
	for _, n := range names {
		if _, seen := index[n]; !seen {
			strongConnect(n)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i][0].String() < out[j][0].String() })
	return out
}

// SelfReferences lists tables holding a foreign key onto themselves. These do
// not block table ordering, but rows within the table still have to be loaded
// with constraints deferred, since a parent row may follow its child in the
// same batch.
func SelfReferences(fks []catalog.FK) []catalog.Ref {
	seen := map[string]bool{}
	var out []catalog.Ref
	for _, fk := range fks {
		if fk.Table == fk.RefTable && !seen[fk.Table.String()] {
			seen[fk.Table.String()] = true
			out = append(out, fk.Table)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].String() < out[j].String() })
	return out
}

// NeedsDeferredConstraints reports whether a plain ordered load is impossible.
func NeedsDeferredConstraints(tables []catalog.Ref, fks []catalog.FK) bool {
	return len(Cycles(tables, fks)) > 0 || len(SelfReferences(fks)) > 0
}
