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

// Package e2e proves the whole pipeline against real PostgreSQL: walk the
// graph, mask in transit, load into a second database, and then verify the two
// properties that actually matter.
//
//  1. Zero foreign-key orphans. The slice is referentially valid.
//  2. Zero canaries. No personal data survived the masking.
//
// Neither can be established by unit tests. A subsetting tool that passes its
// unit tests and fails these is worse than no tool at all, because it looks
// like it worked.
package e2e

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/Autometiq/safeslice/internal/catalog"
	"github.com/Autometiq/safeslice/internal/extract"
	"github.com/Autometiq/safeslice/internal/keyset"
	"github.com/Autometiq/safeslice/internal/load"
	"github.com/Autometiq/safeslice/internal/mask"
	"github.com/Autometiq/safeslice/internal/sink"
	"github.com/Autometiq/safeslice/internal/verify"
)

const (
	srcDB = "safeslice_e2e_src"
	dstDB = "safeslice_e2e_dst"
)

// canaries are values planted in the source that must never appear in the
// target. "real.example" covers the email domain, the rest cover names and
// credentials.
var canaries = []string{"real.example", "Zcanaryfirst", "Zcanarylast", "hunter2-canary", "CanaryCorp"}

func adminDSN(t *testing.T) string {
	t.Helper()
	dsn := os.Getenv("SAFESLICE_TEST_DSN")
	if dsn == "" {
		t.Skip("set SAFESLICE_TEST_DSN to run end-to-end tests")
	}
	return dsn
}

func dsnFor(base, db string) string {
	cfg, err := pgx.ParseConfig(base)
	if err != nil {
		return base
	}
	return fmt.Sprintf("postgres://%s:%s@%s:%d/%s?sslmode=disable",
		cfg.User, cfg.Password, cfg.Host, cfg.Port, db)
}

func connect(t *testing.T, dsn string) *pgx.Conn {
	t.Helper()
	conn, err := pgx.Connect(context.Background(), dsn)
	if err != nil {
		t.Fatalf("connect %s: %v", dsn, err)
	}
	t.Cleanup(func() { conn.Close(context.Background()) })
	return conn
}

func readFixture(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "testdata", "schemas", name))
	if err != nil {
		t.Fatalf("read %s: %v", name, err)
	}
	return string(b)
}

// setup builds a populated source database and an empty target with the same
// schema, then returns connections to both.
func setup(t *testing.T) (src, dst *pgx.Conn) {
	t.Helper()
	ctx := context.Background()
	admin := connect(t, adminDSN(t))
	for _, db := range []string{srcDB, dstDB} {
		// Terminate stragglers so DROP DATABASE cannot hang the suite.
		admin.Exec(ctx, `SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE datname = $1`, db)
		if _, err := admin.Exec(ctx, `DROP DATABASE IF EXISTS `+pgx.Identifier{db}.Sanitize()); err != nil {
			t.Fatalf("drop %s: %v", db, err)
		}
		if _, err := admin.Exec(ctx, `CREATE DATABASE `+pgx.Identifier{db}.Sanitize()); err != nil {
			t.Fatalf("create %s: %v", db, err)
		}
	}

	schema := readFixture(t, "kitchen_sink.sql")
	src = connect(t, dsnFor(adminDSN(t), srcDB))
	dst = connect(t, dsnFor(adminDSN(t), dstDB))
	for name, conn := range map[string]*pgx.Conn{"source": src, "target": dst} {
		if _, err := conn.Exec(ctx, schema); err != nil {
			t.Fatalf("apply schema to %s: %v", name, err)
		}
	}
	if _, err := src.Exec(ctx, readFixture(t, "seed.sql")); err != nil {
		t.Fatalf("seed source: %v", err)
	}
	return src, dst
}

// classifier covers the columns the name heuristics cannot know about, exactly
// as a real safeslice.yaml would.
func classifier() mask.Classifier {
	return mask.Classifier{Overrides: map[string]mask.Rule{
		"companies.slug":            mask.GovID,
		"comments.body":             mask.Redact,
		"comments.commentable_type": mask.Keep,
		"order_items.sku":           mask.Keep,
	}}
}

func runSlice(t *testing.T, src, dst *pgx.Conn, rootLimit, childDepth int) *catalog.Catalog {
	t.Helper()
	ctx := context.Background()

	cat, err := catalog.Load(ctx, src, []string{"public"})
	if err != nil {
		t.Fatalf("load catalog: %v", err)
	}
	plan, err := load.PlanCycles(cat, cat.Refs(), cat.FKs)
	if err != nil {
		t.Fatalf("plan cycles: %v", err)
	}

	keys, err := keyset.Open(filepath.Join(t.TempDir(), "keys.db"))
	if err != nil {
		t.Fatalf("open keyset: %v", err)
	}
	defer keys.Close()

	ex, err := extract.Begin(ctx, src, cat, nil, keys, extract.Options{
		Root:       catalog.Ref{Schema: "public", Name: "users"},
		Limit:      rootLimit,
		ChildDepth: childDepth,
		Seed:       "e2e-seed",
		Classifier: classifier(),
	})
	if err != nil {
		t.Fatalf("begin extract: %v", err)
	}
	defer ex.Close(ctx)

	if err := ex.Collect(ctx); err != nil {
		t.Fatalf("collect: %v", err)
	}
	out, err := sink.NewDB(ctx, dst, cat, plan)
	if err != nil {
		t.Fatalf("open target sink: %v", err)
	}
	if err := ex.Stream(ctx, out, plan); err != nil {
		out.Rollback(ctx)
		t.Fatalf("stream: %v", err)
	}
	if err := out.Close(ctx); err != nil {
		t.Fatalf("commit load: %v", err)
	}
	return cat
}

func count(t *testing.T, conn *pgx.Conn, q string, args ...any) int {
	t.Helper()
	var n int
	if err := conn.QueryRow(context.Background(), q, args...).Scan(&n); err != nil {
		t.Fatalf("query %q: %v", q, err)
	}
	return n
}

func TestSliceLoadsAndIsReferentiallyValid(t *testing.T) {
	src, dst := setup(t)
	cat := runSlice(t, src, dst, 5, 1)

	users := count(t, dst, `SELECT count(*) FROM users`)
	if users == 0 {
		t.Fatal("no users loaded; the slice is empty")
	}
	if users > 12 {
		t.Errorf("loaded %d users, more than the source holds", users)
	}
	// Parents are mandatory: every loaded user needs its company.
	if c := count(t, dst, `SELECT count(*) FROM companies`); c == 0 {
		t.Error("no companies loaded; parent rows were not followed")
	}

	// The gate that proves the whole traversal: not one dangling reference.
	for _, fk := range cat.FKs {
		child, parent := fk.Table.Name, fk.RefTable.Name
		on := make([]string, len(fk.Columns))
		notNull := make([]string, len(fk.Columns))
		for i := range fk.Columns {
			on[i] = fmt.Sprintf("c.%q = p.%q", fk.Columns[i], fk.RefColumns[i])
			notNull[i] = fmt.Sprintf("c.%q IS NOT NULL", fk.Columns[i])
		}
		q := fmt.Sprintf(
			`SELECT count(*) FROM %q c LEFT JOIN %q p ON %s WHERE %s AND p.%q IS NULL`,
			child, parent, strings.Join(on, " AND "), strings.Join(notNull, " AND "), fk.RefColumns[0])
		if n := count(t, dst, q); n != 0 {
			t.Errorf("%s: %d orphan rows reference a parent that was not included", fk.Name, n)
		}
	}
}

func TestNoCanarySurvives(t *testing.T) {
	src, dst := setup(t)
	runSlice(t, src, dst, 12, 1)

	// Confirm the canaries were actually there to begin with; a masking test
	// that passes because the source was empty proves nothing.
	if n := count(t, src, `SELECT count(*) FROM users WHERE email LIKE '%real.example%'`); n == 0 {
		t.Fatal("source holds no canary emails; the test would pass vacuously")
	}

	for _, table := range []string{"users", "companies", "comments"} {
		cols := count(t, dst, `SELECT count(*) FROM information_schema.columns
			WHERE table_name = $1 AND data_type IN ('text','character varying','character')`, table)
		if cols == 0 {
			continue
		}
		for _, canary := range canaries {
			q := fmt.Sprintf(`SELECT count(*) FROM %q t WHERE t::text LIKE '%%' || $1 || '%%'`, table)
			if n := count(t, dst, q, canary); n != 0 {
				t.Errorf("PII LEAK: %d rows in %s still contain %q", n, table, canary)
			}
		}
	}
}

func TestMaskedValuesAreUsableAndDeterministic(t *testing.T) {
	src, dst := setup(t)
	runSlice(t, src, dst, 12, 1)

	// A unique index sits on users.email; a hash collision that slipped through
	// would have failed the load, but check the outcome directly too.
	total := count(t, dst, `SELECT count(*) FROM users`)
	distinct := count(t, dst, `SELECT count(DISTINCT email) FROM users`)
	if total != distinct {
		t.Errorf("%d users share %d distinct emails; the unique index was violated", total, distinct)
	}
	if n := count(t, dst, `SELECT count(*) FROM users WHERE email NOT LIKE '%@example.invalid'`); n != 0 {
		t.Errorf("%d masked emails are not on the unroutable .invalid domain", n)
	}
	// NULLs must stay NULL: inventing values would change query results.
	srcNulls := count(t, src, `SELECT count(*) FROM users WHERE last_name IS NULL`)
	if srcNulls > 0 {
		if n := count(t, dst, `SELECT count(*) FROM users WHERE last_name IS NULL`); n == 0 {
			t.Error("NULL names were replaced with values")
		}
	}
}

func TestCycleAndSelfReferenceAreRestored(t *testing.T) {
	src, dst := setup(t)
	runSlice(t, src, dst, 12, 1)

	// companies.owner_id and users.manager_id are both nulled during insert to
	// break non-deferrable cycles. If the follow-up UPDATE never ran, the data
	// loads cleanly but is silently wrong.
	if n := count(t, dst, `SELECT count(*) FROM companies WHERE owner_id IS NOT NULL`); n == 0 {
		t.Error("companies.owner_id is NULL everywhere; the deferred update did not run")
	}
	if n := count(t, dst, `SELECT count(*) FROM users WHERE manager_id IS NOT NULL`); n == 0 {
		t.Error("users.manager_id is NULL everywhere; the self-reference was not restored")
	}
}

func TestSequencesContinueAfterTheSlice(t *testing.T) {
	src, dst := setup(t)
	runSlice(t, src, dst, 12, 1)

	// The classic silent failure: rows load fine, then the application's very
	// first insert collides on the primary key.
	maxID := count(t, dst, `SELECT COALESCE(max(id), 0) FROM users`)
	next := count(t, dst, `SELECT nextval('users_id_seq')::int`)
	if next <= maxID {
		t.Errorf("users_id_seq is at %d but the highest loaded id is %d; the next insert would collide",
			next, maxID)
	}

	var id int
	err := dst.QueryRow(context.Background(),
		`INSERT INTO users (company_id, email) VALUES ((SELECT min(id) FROM companies), 'fresh@example.invalid') RETURNING id`).Scan(&id)
	if err != nil {
		t.Fatalf("inserting a new row after the load failed: %v", err)
	}
}

func TestIdentityColumnsLoad(t *testing.T) {
	src, dst := setup(t)
	runSlice(t, src, dst, 12, 1)

	// companies.id is GENERATED ALWAYS AS IDENTITY. The ids must match the
	// source exactly, because the foreign keys in the slice point at them.
	var srcIDs, dstIDs []int64
	rows, err := src.Query(context.Background(), `SELECT id FROM companies ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var id int64
		rows.Scan(&id)
		srcIDs = append(srcIDs, id)
	}
	rows.Close()
	rows, err = dst.Query(context.Background(), `SELECT id FROM companies ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var id int64
		rows.Scan(&id)
		dstIDs = append(dstIDs, id)
	}
	rows.Close()
	if len(dstIDs) == 0 {
		t.Fatal("no identity rows loaded")
	}
	set := map[int64]bool{}
	for _, id := range srcIDs {
		set[id] = true
	}
	for _, id := range dstIDs {
		if !set[id] {
			t.Errorf("company id %d in the target does not exist in the source; identity values were regenerated", id)
		}
	}
}

func TestGeneratedColumnIsRecomputedNotCopied(t *testing.T) {
	src, dst := setup(t)
	runSlice(t, src, dst, 12, 1)

	// users.full_name is GENERATED ALWAYS AS (first_name || ' ' || last_name).
	// It must reflect the *masked* names, not the source's.
	var full, first string
	err := dst.QueryRow(context.Background(),
		`SELECT full_name, first_name FROM users WHERE first_name IS NOT NULL LIMIT 1`).Scan(&full, &first)
	if err != nil {
		t.Skipf("no row with a name to check: %v", err)
	}
	if !strings.HasPrefix(full, first) {
		t.Errorf("full_name %q does not derive from the masked first_name %q", full, first)
	}
	for _, canary := range canaries {
		if strings.Contains(full, canary) {
			t.Errorf("PII LEAK: generated column still contains %q", canary)
		}
	}
}

// TestVerifyFlagsSourceAndClearsTarget is the compliance gate: the scanner must
// find the personal data in the unmasked source, and find none of it after the
// slice has been masked and loaded. A scanner that passes both is broken; one
// that fails both is useless.
func TestVerifyFlagsSourceAndClearsTarget(t *testing.T) {
	src, dst := setup(t)
	runSlice(t, src, dst, 12, 1)
	ctx := context.Background()

	srcCat, err := catalog.Load(ctx, src, []string{"public"})
	if err != nil {
		t.Fatal(err)
	}
	found, err := verify.Scan(ctx, src, srcCat, verify.Options{})
	if err != nil {
		t.Fatalf("scan source: %v", err)
	}
	if len(found) == 0 {
		t.Fatal("scanner found nothing in the unmasked source; it cannot detect anything")
	}
	var sawEmail bool
	for _, f := range found {
		if f.Kind == "email" && f.Table.Name == "users" {
			sawEmail = true
		}
		if strings.Contains(f.Sample, "real.example") {
			t.Errorf("finding sample leaked the value it reported: %q", f.Sample)
		}
	}
	if !sawEmail {
		t.Errorf("scanner missed the canary emails in users.email; found %+v", found)
	}

	dstCat, err := catalog.Load(ctx, dst, []string{"public"})
	if err != nil {
		t.Fatal(err)
	}
	clean, err := verify.Scan(ctx, dst, dstCat, verify.Options{})
	if err != nil {
		t.Fatalf("scan target: %v", err)
	}
	for _, f := range clean {
		t.Errorf("PII LEAK: %s.%s still holds %s (%d rows)", f.Table.Name, f.Column, f.Kind, f.Matches)
	}
}

// TestTriggersAreLeftEnabled guards a failure that commits successfully and
// still damages the target: safeslice disables user triggers during the load,
// and if the re-enable does not happen the developer database silently keeps
// them off forever.
func TestTriggersAreLeftEnabled(t *testing.T) {
	src, dst := setup(t)
	ctx := context.Background()
	if _, err := dst.Exec(ctx, `
		CREATE FUNCTION note() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RETURN NEW; END $$;
		CREATE TRIGGER users_note AFTER INSERT ON users FOR EACH ROW EXECUTE FUNCTION note();`); err != nil {
		t.Fatalf("create trigger: %v", err)
	}
	runSlice(t, src, dst, 12, 1)

	var enabled string
	if err := dst.QueryRow(ctx,
		`SELECT tgenabled::text FROM pg_trigger WHERE tgname = 'users_note'`).Scan(&enabled); err != nil {
		t.Fatalf("read trigger state: %v", err)
	}
	// 'D' means disabled; 'O' is the normal origin-enabled state.
	if enabled == "D" {
		t.Error("user trigger left disabled after the load; the target database is damaged")
	}
}
