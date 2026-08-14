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
	"testing"

	"github.com/Autometiq/safeslice/internal/catalog"
)

func ref(name string) catalog.Ref { return catalog.Ref{Schema: "public", Name: name} }

func fk(name, table string, cols []string, refTable string, refCols []string) catalog.FK {
	return catalog.FK{Name: name, Table: ref(table), Columns: cols,
		RefTable: ref(refTable), RefColumns: refCols}
}

// users <-> companies is a cycle, employees.manager_id is a self-reference,
// and orders -> users is an ordinary edge.
var fixture = []catalog.FK{
	fk("users_company_fk", "users", []string{"company_id"}, "companies", []string{"id"}),
	fk("companies_owner_fk", "companies", []string{"owner_id"}, "users", []string{"id"}),
	fk("orders_user_fk", "orders", []string{"user_id"}, "users", []string{"id"}),
	fk("emp_manager_fk", "employees", []string{"manager_id"}, "employees", []string{"id"}),
}

func names(refs []catalog.Ref) []string {
	out := make([]string, len(refs))
	for i, r := range refs {
		out[i] = r.Name
	}
	return out
}

func indexOf(refs []catalog.Ref, name string) int {
	for i, r := range refs {
		if r.Name == name {
			return i
		}
	}
	return -1
}

func TestParentsAndChildrenBothIndexed(t *testing.T) {
	g := New(&catalog.Catalog{Tables: map[string]*catalog.Table{}, FKs: fixture}, nil)
	if got := len(g.Parents[ref("orders").String()]); got != 1 {
		t.Errorf("orders declares %d FKs, want 1", got)
	}
	if got := len(g.Children[ref("users").String()]); got != 2 {
		t.Errorf("users is referenced by %d FKs, want 2 (companies, orders)", got)
	}
	// A self-reference is genuinely both: the row's parent and its children
	// live in the same table.
	if len(g.Parents[ref("employees").String()]) != 1 || len(g.Children[ref("employees").String()]) != 1 {
		t.Error("self-referencing FK must appear as both parent and child edge")
	}
}

func TestVirtualKeysJoinTheGraph(t *testing.T) {
	// Rails polymorphic association: no pg_constraint row exists, so without
	// this the comments->posts edge is invisible and the slice is incomplete.
	virtual := []catalog.FK{{
		Name: "comments_post_vk", Table: ref("comments"), Columns: []string{"commentable_id"},
		RefTable: ref("posts"), RefColumns: []string{"id"},
		Virtual: true, When: "commentable_type = 'Post'",
	}}
	g := New(&catalog.Catalog{Tables: map[string]*catalog.Table{}, FKs: fixture}, virtual)
	got := g.Parents[ref("comments").String()]
	if len(got) != 1 || !got[0].Virtual || got[0].When != "commentable_type = 'Post'" {
		t.Fatalf("virtual key not wired into the graph: %+v", got)
	}
	if len(g.Children[ref("posts").String()]) != 1 {
		t.Error("virtual key missing from the child index")
	}
}

func TestTopoOrderPutsParentsFirst(t *testing.T) {
	acyclic := []catalog.FK{fixture[0], fixture[2]} // drop the cycle-closing edge
	order := TopoOrder([]catalog.Ref{ref("orders"), ref("users"), ref("companies")}, acyclic)
	if indexOf(order, "companies") > indexOf(order, "users") ||
		indexOf(order, "users") > indexOf(order, "orders") {
		t.Errorf("order = %v, want companies before users before orders", names(order))
	}
}

func TestTopoOrderSurvivesCyclesAndIsDeterministic(t *testing.T) {
	tables := []catalog.Ref{ref("users"), ref("companies"), ref("orders")}
	first := names(TopoOrder(tables, fixture))
	if len(first) != 3 {
		t.Fatalf("cycle dropped tables: %v", first)
	}
	for range 5 {
		if got := names(TopoOrder(tables, fixture)); !equal(got, first) {
			t.Fatalf("order not deterministic: %v then %v", first, got)
		}
	}
}

func TestTopoOrderIgnoresSelfReferenceForOrdering(t *testing.T) {
	// A table cannot precede itself; treating this as a dependency would make
	// the table permanently unsatisfiable and force the cycle-breaking path.
	order := TopoOrder([]catalog.Ref{ref("employees")}, fixture)
	if len(order) != 1 || order[0].Name != "employees" {
		t.Errorf("order = %v, want [employees]", names(order))
	}
}

func TestTopoOrderIgnoresEdgesToAbsentTables(t *testing.T) {
	// A slice often omits tables entirely; their edges must not block ordering.
	order := TopoOrder([]catalog.Ref{ref("orders")}, fixture)
	if len(order) != 1 || order[0].Name != "orders" {
		t.Errorf("order = %v, want [orders]", names(order))
	}
}

func TestCyclesDetected(t *testing.T) {
	got := Cycles([]catalog.Ref{ref("users"), ref("companies"), ref("orders")}, fixture)
	if len(got) != 1 {
		t.Fatalf("found %d cycles, want 1: %v", len(got), got)
	}
	if n := names(got[0]); !equal(n, []string{"companies", "users"}) {
		t.Errorf("cycle = %v, want [companies users]", n)
	}
}

func TestSelfReferenceNotReportedAsCycle(t *testing.T) {
	// It needs deferred constraints, but it is not a multi-table cycle and
	// reporting it as one would confuse the plan output.
	if got := Cycles([]catalog.Ref{ref("employees")}, fixture); len(got) != 0 {
		t.Errorf("self-reference reported as cycle: %v", got)
	}
	if got := SelfReferences(fixture); len(got) != 1 || got[0].Name != "employees" {
		t.Errorf("SelfReferences = %v, want [employees]", names(got))
	}
}

func TestNeedsDeferredConstraints(t *testing.T) {
	if !NeedsDeferredConstraints([]catalog.Ref{ref("users"), ref("companies")}, fixture) {
		t.Error("cyclic pair must require deferred constraints")
	}
	if !NeedsDeferredConstraints([]catalog.Ref{ref("employees")}, fixture) {
		t.Error("self-reference must require deferred constraints")
	}
	plain := []catalog.FK{fixture[2]}
	if NeedsDeferredConstraints([]catalog.Ref{ref("orders"), ref("users")}, plain) {
		t.Error("acyclic graph must not require deferred constraints")
	}
}

func TestReadTargetResolvesPartitionToParent(t *testing.T) {
	c := &catalog.Catalog{Tables: map[string]*catalog.Table{
		"public.events":      {Ref: ref("events"), Partitioned: true},
		"public.events_2026": {Ref: ref("events_2026"), Partition: true, Parent: ref("events")},
	}}
	g := New(c, nil)
	if got := g.ReadTarget(ref("events_2026")); got != ref("events") {
		t.Errorf("ReadTarget = %v, want events; reading one partition yields an incomplete slice", got)
	}
	if got := g.ReadTarget(ref("events")); got != ref("events") {
		t.Errorf("ReadTarget on a parent = %v, want itself", got)
	}
}

func TestReadTargetTerminatesOnBrokenParentage(t *testing.T) {
	// Defensive: a catalog claiming a table is its own parent must not hang.
	c := &catalog.Catalog{Tables: map[string]*catalog.Table{
		"public.loop": {Ref: ref("loop"), Partition: true, Parent: ref("loop")},
	}}
	if got := New(c, nil).ReadTarget(ref("loop")); got != ref("loop") {
		t.Errorf("ReadTarget = %v, want loop", got)
	}
}

func equal(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
