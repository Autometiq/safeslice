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

package catalog

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/jackc/pgx/v5"
)

// Set SAFESLICE_TEST_DSN to run. CI provides a Postgres service container;
// locally: docker run -e POSTGRES_PASSWORD=pw -p 5432:5432 -d postgres:17
func load(t *testing.T) *Catalog {
	t.Helper()
	dsn := os.Getenv("SAFESLICE_TEST_DSN")
	if dsn == "" {
		t.Skip("set SAFESLICE_TEST_DSN to run catalog tests")
	}
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(func() { conn.Close(ctx) })

	schema, err := os.ReadFile(filepath.Join("..", "..", "testdata", "schemas", "kitchen_sink.sql"))
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	if _, err := conn.Exec(ctx, "DROP SCHEMA IF EXISTS ss_test CASCADE; CREATE SCHEMA ss_test; SET search_path = ss_test"); err != nil {
		t.Fatalf("reset schema: %v", err)
	}
	if _, err := conn.Exec(ctx, string(schema)); err != nil {
		t.Fatalf("apply fixture: %v", err)
	}
	c, err := Load(ctx, conn, []string{"ss_test"})
	if err != nil {
		t.Fatalf("load catalog: %v", err)
	}
	return c
}

func table(t *testing.T, c *Catalog, name string) *Table {
	t.Helper()
	tbl, ok := c.Table(Ref{"ss_test", name})
	if !ok {
		t.Fatalf("table %q missing from catalog", name)
	}
	return tbl
}

func column(t *testing.T, tbl *Table, name string) Column {
	t.Helper()
	col, ok := tbl.Column(name)
	if !ok {
		t.Fatalf("column %s.%s missing", tbl.Ref.Name, name)
	}
	return col
}

func TestKeysAndUniques(t *testing.T) {
	c := load(t)
	users := table(t, c, "users")
	if got := users.PK; len(got) != 1 || got[0] != "id" {
		t.Errorf("users PK = %v, want [id]", got)
	}
	// The unique index has no constraint behind it, but masking email without
	// noticing it would produce duplicate-key failures on restore.
	if !containsCols(users.Uniques, []string{"email"}) {
		t.Errorf("users uniques = %v, want to include [email]", users.Uniques)
	}
	if got := table(t, c, "order_items").PK; !sameCols(got, []string{"order_id", "line_no"}) {
		t.Errorf("order_items PK = %v, want [order_id line_no]", got)
	}
}

func TestIdentityAndGeneratedColumns(t *testing.T) {
	c := load(t)
	if got := column(t, table(t, c, "companies"), "id").Identity; got != IdentityAlways {
		t.Errorf("companies.id identity = %q, want %q", got, IdentityAlways)
	}
	full := column(t, table(t, c, "users"), "full_name")
	if full.Generated != GeneratedStored {
		t.Errorf("users.full_name generated = %q, want %q", full.Generated, GeneratedStored)
	}
	if full.Insertable() {
		t.Error("stored generated column must be excluded from INSERT column lists")
	}
	if column(t, table(t, c, "users"), "email").Insertable() != true {
		t.Error("ordinary column reported as not insertable")
	}
}

func TestVarcharLengthCaptured(t *testing.T) {
	c := load(t)
	if got := column(t, table(t, c, "companies"), "name").MaxLen; got != 120 {
		t.Errorf("companies.name maxlen = %d, want 120", got)
	}
	if got := column(t, table(t, c, "users"), "email").MaxLen; got != 255 {
		t.Errorf("users.email maxlen = %d, want 255", got)
	}
	if got := column(t, table(t, c, "users"), "password").MaxLen; got != -1 {
		t.Errorf("unbounded text maxlen = %d, want -1", got)
	}
}

func findFK(c *Catalog, tbl, ref string, cols ...string) *FK {
	for i := range c.FKs {
		fk := &c.FKs[i]
		if fk.Table.Name == tbl && fk.RefTable.Name == ref && (len(cols) == 0 || sameCols(fk.Columns, cols)) {
			return fk
		}
	}
	return nil
}

func TestForeignKeyShapes(t *testing.T) {
	c := load(t)
	if findFK(c, "users", "companies") == nil || findFK(c, "companies", "users") == nil {
		t.Error("cyclic FK pair users<->companies not discovered")
	}
	if findFK(c, "users", "users", "manager_id") == nil {
		t.Error("self-referencing FK users.manager_id not discovered")
	}
	fk := findFK(c, "shipments", "order_items")
	if fk == nil {
		t.Fatal("composite FK shipments->order_items not discovered")
	}
	// WITH ORDINALITY guarantees this pairing; array_agg alone does not, and a
	// swapped pair would silently join the wrong columns.
	if !sameCols(fk.Columns, []string{"order_id", "line_no"}) ||
		!sameCols(fk.RefColumns, []string{"order_id", "line_no"}) {
		t.Errorf("composite FK column order wrong: %v -> %v", fk.Columns, fk.RefColumns)
	}
	if findFK(c, "comments", "posts") != nil {
		t.Error("polymorphic association must not appear as a real FK")
	}
}

func TestKeyColumnsCoverBothSidesOfEveryFK(t *testing.T) {
	c := load(t)
	keys := c.KeyColumns(Ref{"ss_test", "users"})
	for _, want := range []string{"id", "company_id", "manager_id"} {
		if !keys[want] {
			t.Errorf("users.%s not reported as a key column; masking it would break the restore", want)
		}
	}
	if keys["email"] {
		t.Error("users.email wrongly reported as a key column; it would never get masked")
	}
}

func TestSequencesAreDiscovered(t *testing.T) {
	c := load(t)
	users := table(t, c, "users")
	if len(users.Sequences) != 1 || users.Sequences[0].Column != "id" {
		t.Fatalf("users sequences = %+v, want one owned by id", users.Sequences)
	}
	// Identity columns own a sequence too, and it needs resetting just the same.
	if len(table(t, c, "companies").Sequences) != 1 {
		t.Error("identity column sequence not discovered")
	}
}

func TestPartitionParentage(t *testing.T) {
	c := load(t)
	parent := table(t, c, "events")
	if !parent.Partitioned {
		t.Error("events not marked as a partition parent")
	}
	part := table(t, c, "events_2026")
	if !part.Partition || part.Parent != (Ref{"ss_test", "events"}) {
		t.Errorf("events_2026 parentage = %+v/%v, want partition of ss_test.events", part.Partition, part.Parent)
	}
}
