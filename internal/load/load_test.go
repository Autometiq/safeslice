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

package load

import (
	"strings"
	"testing"

	"github.com/Autometiq/safeslice/internal/catalog"
)

func ref(name string) catalog.Ref { return catalog.Ref{Schema: "app", Name: name} }

func col(name string, opts ...func(*catalog.Column)) catalog.Column {
	c := catalog.Column{Name: name, Type: "bigint", MaxLen: -1}
	for _, o := range opts {
		o(&c)
	}
	return c
}

func notNull(c *catalog.Column)         { c.NotNull = true }
func identityAlways(c *catalog.Column)  { c.Identity = catalog.IdentityAlways }
func generatedStored(c *catalog.Column) { c.Generated = catalog.GeneratedStored }

func TestGeneratedColumnsExcludedFromInsert(t *testing.T) {
	tbl := &catalog.Table{Ref: ref("users"), Columns: []catalog.Column{
		col("id"), col("first_name"), col("full_name", generatedStored),
	}}
	got := InsertColumns(tbl)
	for _, c := range got {
		if c == "full_name" {
			t.Fatal("stored generated column in the column list; Postgres rejects the whole INSERT")
		}
	}
	if len(got) != 2 {
		t.Errorf("InsertColumns = %v, want [id first_name]", got)
	}
}

func TestIdentityAlwaysGetsOverridingClause(t *testing.T) {
	tbl := &catalog.Table{Ref: ref("companies"), Columns: []catalog.Column{
		col("id", identityAlways), col("name"),
	}}
	prefix := InsertPrefix(tbl, InsertColumns(tbl))
	// Without this the load fails outright, and we cannot simply omit the id:
	// the foreign keys in the slice point at these exact values.
	if !strings.Contains(prefix, "OVERRIDING SYSTEM VALUE") {
		t.Errorf("prefix = %q, want OVERRIDING SYSTEM VALUE", prefix)
	}
}

func TestNoOverridingClauseWhenNotNeeded(t *testing.T) {
	tbl := &catalog.Table{Ref: ref("orders"), Columns: []catalog.Column{col("id"), col("total")}}
	if got := InsertPrefix(tbl, InsertColumns(tbl)); strings.Contains(got, "OVERRIDING") {
		t.Errorf("prefix = %q, want no OVERRIDING clause on a plain table", got)
	}
}

func TestIdentifiersAreQuoted(t *testing.T) {
	tbl := &catalog.Table{Ref: catalog.Ref{Schema: "app", Name: "order"}, // reserved word
		Columns: []catalog.Column{col("select")}} // reserved word
	got := InsertPrefix(tbl, InsertColumns(tbl))
	if !strings.Contains(got, `"app"."order"`) || !strings.Contains(got, `"select"`) {
		t.Errorf("prefix = %q, want quoted identifiers", got)
	}
	if Ident(`we"ird`) != `"we""ird"` {
		t.Errorf("Ident did not escape an embedded quote: %s", Ident(`we"ird`))
	}
}

func TestSequenceResetsEmittedForEveryOwnedSequence(t *testing.T) {
	c := &catalog.Catalog{Tables: map[string]*catalog.Table{
		"app.users": {Ref: ref("users"), Sequences: []catalog.Sequence{
			{Ref: ref("users_id_seq"), Table: ref("users"), Column: "id"},
		}},
	}}
	got := SequenceResets(c, []catalog.Ref{ref("users")})
	if len(got) != 1 {
		t.Fatalf("got %d resets, want 1; a stale sequence makes the next app insert collide", len(got))
	}
	stmt := got[0]
	for _, want := range []string{"setval", `"app"."users_id_seq"`, "MAX(\"id\")", "IS NOT NULL"} {
		if !strings.Contains(stmt, want) {
			t.Errorf("reset %q missing %q", stmt, want)
		}
	}
}

func TestTriggerToggleIsSymmetric(t *testing.T) {
	refs := []catalog.Ref{ref("users"), ref("orders")}
	off, on := DisableTriggers(refs), EnableTriggers(refs)
	if len(off) != 2 || len(on) != 2 {
		t.Fatalf("got %d disable and %d enable statements, want 2 each", len(off), len(on))
	}
	// USER, not ALL: foreign-key enforcement must stay on so an invalid slice
	// fails loudly instead of loading corrupt data.
	if !strings.Contains(off[0], "DISABLE TRIGGER USER") {
		t.Errorf("disable = %q, want DISABLE TRIGGER USER", off[0])
	}
	if strings.Contains(off[0], "TRIGGER ALL") {
		t.Error("DISABLE TRIGGER ALL would switch off FK checks and hide a broken slice")
	}
}

// users <-> companies, with companies.owner_id nullable.
func cyclicCatalog(deferrable bool) (*catalog.Catalog, []catalog.Ref, []catalog.FK) {
	c := &catalog.Catalog{Tables: map[string]*catalog.Table{
		"app.users": {Ref: ref("users"), PK: []string{"id"}, Columns: []catalog.Column{
			col("id"), col("company_id", notNull),
		}},
		"app.companies": {Ref: ref("companies"), PK: []string{"id"}, Columns: []catalog.Column{
			col("id"), col("owner_id"), // nullable: the breakable edge
		}},
	}}
	fks := []catalog.FK{
		{Name: "users_company_fk", Table: ref("users"), Columns: []string{"company_id"},
			RefTable: ref("companies"), RefColumns: []string{"id"}, Deferrable: deferrable},
		{Name: "companies_owner_fk", Table: ref("companies"), Columns: []string{"owner_id"},
			RefTable: ref("users"), RefColumns: []string{"id"}, Deferrable: deferrable},
	}
	return c, []catalog.Ref{ref("users"), ref("companies")}, fks
}

func TestDeferrableCycleUsesDeferral(t *testing.T) {
	c, refs, fks := cyclicCatalog(true)
	plan, err := PlanCycles(c, refs, fks)
	if err != nil {
		t.Fatalf("PlanCycles: %v", err)
	}
	if !plan.Deferred || len(plan.Breaks) != 0 {
		t.Errorf("plan = %+v, want deferral with no breaks", plan)
	}
}

func TestNonDeferrableCycleFallsBackToNullPass(t *testing.T) {
	// This is the case that matters: SET CONSTRAINTS ALL DEFERRED silently does
	// nothing for constraints that were never declared DEFERRABLE, and most
	// ORM-generated schemas do not declare them.
	c, refs, fks := cyclicCatalog(false)
	plan, err := PlanCycles(c, refs, fks)
	if err != nil {
		t.Fatalf("PlanCycles: %v", err)
	}
	if plan.Deferred {
		t.Fatal("claimed deferral for non-deferrable constraints; the load would fail")
	}
	if len(plan.Breaks) != 1 {
		t.Fatalf("got %d breaks, want 1: %+v", len(plan.Breaks), plan.Breaks)
	}
	b := plan.Breaks[0]
	if b.Table != ref("companies") || b.Columns[0] != "owner_id" {
		t.Errorf("break = %+v, want the nullable companies.owner_id edge", b)
	}
	if !b.Nulled()["owner_id"] {
		t.Error("break must null the column during INSERT")
	}
}

func TestBreakReasonNamesTheActualBlocker(t *testing.T) {
	// Mixed cycle: the edge we break is DEFERRABLE, the other one is not.
	// Reporting the break edge as the blocker sends the reader to a constraint
	// that is already correct.
	c, refs, fks := cyclicCatalog(false)
	for i := range fks {
		if fks[i].Name == "companies_owner_fk" {
			fks[i].Deferrable = true // the breakable edge is fine
		}
	}
	plan, err := PlanCycles(c, refs, fks)
	if err != nil {
		t.Fatalf("PlanCycles: %v", err)
	}
	if len(plan.Breaks) != 1 {
		t.Fatalf("got %d breaks, want 1", len(plan.Breaks))
	}
	reason := plan.Breaks[0].Reason
	if !strings.Contains(reason, "users_company_fk") {
		t.Errorf("reason = %q, want it to name users_company_fk, the constraint actually blocking deferral", reason)
	}
	if strings.Contains(reason, "companies_owner_fk") {
		t.Errorf("reason = %q blames a constraint that is already DEFERRABLE", reason)
	}
}

func TestNonDeferrableCycleWithNoNullableEdgeIsAnError(t *testing.T) {
	c, refs, fks := cyclicCatalog(false)
	// Make the only nullable edge NOT NULL: now the cycle is genuinely unloadable.
	c.Tables["app.companies"].Columns[1].NotNull = true
	_, err := PlanCycles(c, refs, fks)
	if err == nil {
		t.Fatal("must fail loudly rather than emit SQL that cannot succeed")
	}
	if !strings.Contains(err.Error(), "DEFERRABLE") {
		t.Errorf("error %q should tell the user how to fix it", err)
	}
}

func TestAcyclicSchemaNeedsNoSpecialHandling(t *testing.T) {
	c := &catalog.Catalog{Tables: map[string]*catalog.Table{
		"app.users":  {Ref: ref("users"), PK: []string{"id"}},
		"app.orders": {Ref: ref("orders"), PK: []string{"id"}},
	}}
	fks := []catalog.FK{{Name: "o_u", Table: ref("orders"), Columns: []string{"user_id"},
		RefTable: ref("users"), RefColumns: []string{"id"}}}
	plan, err := PlanCycles(c, []catalog.Ref{ref("users"), ref("orders")}, fks)
	if err != nil || plan.Deferred || len(plan.Breaks) != 0 {
		t.Errorf("plan = %+v, err = %v; want a plain ordered load", plan, err)
	}
}

func TestSelfReferenceIsHandled(t *testing.T) {
	c := &catalog.Catalog{Tables: map[string]*catalog.Table{
		"app.employees": {Ref: ref("employees"), PK: []string{"id"},
			Columns: []catalog.Column{col("id"), col("manager_id")}},
	}}
	fks := []catalog.FK{{Name: "emp_mgr", Table: ref("employees"), Columns: []string{"manager_id"},
		RefTable: ref("employees"), RefColumns: []string{"id"}, Deferrable: false}}
	plan, err := PlanCycles(c, []catalog.Ref{ref("employees")}, fks)
	if err != nil {
		t.Fatalf("PlanCycles: %v", err)
	}
	// A manager row can follow its report inside the same batch, so a
	// non-deferrable self-reference needs the same null-then-update treatment.
	if len(plan.Breaks) != 1 || plan.Breaks[0].Columns[0] != "manager_id" {
		t.Errorf("breaks = %+v, want manager_id nulled and updated afterwards", plan.Breaks)
	}
}

func TestBreakUpdateStatementShape(t *testing.T) {
	b := Break{Table: ref("companies"), Columns: []string{"owner_id"}, PK: []string{"id"}}
	stmt := b.UpdatePrefix() + " (1, 42) " + b.UpdateSuffix()
	for _, want := range []string{
		`UPDATE "app"."companies" AS tgt`,
		`SET "owner_id" = src."owner_id"`,
		`AS src("id", "owner_id")`,
		`WHERE tgt."id" = src."id"`,
	} {
		if !strings.Contains(stmt, want) {
			t.Errorf("statement missing %q:\n%s", want, stmt)
		}
	}
}

func TestBreakUpdateHandlesCompositePK(t *testing.T) {
	b := Break{Table: ref("items"), Columns: []string{"parent_id"}, PK: []string{"order_id", "line_no"}}
	suffix := b.UpdateSuffix()
	if !strings.Contains(suffix, `tgt."order_id" = src."order_id" AND tgt."line_no" = src."line_no"`) {
		t.Errorf("composite PK match clause wrong: %s", suffix)
	}
}
