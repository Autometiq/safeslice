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

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/Autometiq/safeslice/internal/report"
)

// The wizard against a real database.
//
// Every screen in it is answered from a script, which is the same thing a user
// does with a keyboard. What this proves is the part unit tests cannot: that
// the answers reach the pipeline, that the run ends with rows in a database and
// artifacts on disk, and that none of it carries the source password.

const (
	wizSrcDB = "safeslice_wizard_src"
	wizDstDB = "safeslice_wizard_dst"
)

func wizardAdminDSN(t *testing.T) string {
	t.Helper()
	dsn := os.Getenv("SAFESLICE_TEST_DSN")
	if dsn == "" {
		t.Skip("set SAFESLICE_TEST_DSN to run the wizard integration test")
	}
	return dsn
}

func dsnForDB(base, db string) string {
	cfg, err := pgx.ParseConfig(base)
	if err != nil {
		return base
	}
	return fmt.Sprintf("postgres://%s:%s@%s:%d/%s?sslmode=disable",
		cfg.User, cfg.Password, cfg.Host, cfg.Port, db)
}

// wizardSetup builds a seeded source and an empty, migrated target.
func wizardSetup(t *testing.T) (src, dst string) {
	t.Helper()
	// The destination menu offers to create a local database only when one
	// answers, so point the probe at a dead port. Otherwise the options are
	// numbered differently on a developer's laptop than in CI, and a scripted
	// answer means two different things.
	t.Setenv("PGHOST", "127.0.0.1")
	t.Setenv("PGPORT", "1")
	ctx := context.Background()
	admin, err := pgx.Connect(ctx, wizardAdminDSN(t))
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer admin.Close(ctx)

	for _, db := range []string{wizSrcDB, wizDstDB} {
		admin.Exec(ctx, `SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE datname = $1`, db)
		if _, err := admin.Exec(ctx, `DROP DATABASE IF EXISTS `+pgx.Identifier{db}.Sanitize()); err != nil {
			t.Fatalf("drop %s: %v", db, err)
		}
		if _, err := admin.Exec(ctx, `CREATE DATABASE `+pgx.Identifier{db}.Sanitize()); err != nil {
			t.Fatalf("create %s: %v", db, err)
		}
	}

	read := func(name string) string {
		b, err := os.ReadFile(filepath.Join("..", "..", "testdata", "schemas", name))
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		return string(b)
	}
	schema, seed := read("kitchen_sink.sql"), read("seed.sql")

	src, dst = dsnForDB(wizardAdminDSN(t), wizSrcDB), dsnForDB(wizardAdminDSN(t), wizDstDB)
	for _, target := range []struct{ dsn, sql string }{{src, schema}, {dst, schema}, {src, seed}} {
		conn, err := pgx.Connect(ctx, target.dsn)
		if err != nil {
			t.Fatalf("connect: %v", err)
		}
		_, err = conn.Exec(ctx, target.sql)
		conn.Close(ctx)
		if err != nil {
			t.Fatalf("apply fixture: %v", err)
		}
	}
	return src, dst
}

// answers is one keystroke sequence through the whole wizard.
type answers struct {
	source      string // 1 = type a connection string
	sourceDSN   string
	classify    string // 1 = accept all recommended
	sliceSize   string
	root        string
	filter      string
	destination string // 2 = use an existing database
	targetDSN   string
	review      string // 1 = create
	saveProfile string
}

func (a answers) script() string {
	return strings.Join([]string{
		a.source, a.sourceDSN, a.classify, a.sliceSize, a.root, a.filter,
		a.destination, a.targetDSN, a.review, a.saveProfile,
	}, "\n") + "\n"
}

func TestWizardRunsTheWholeWorkflow(t *testing.T) {
	src, dst := wizardSetup(t)
	dir := t.TempDir()
	chdir(t, dir)

	flagNoOpen = true
	t.Cleanup(func() { flagNoOpen = false })

	buf := script(t, answers{
		source: "1", sourceDSN: src,
		classify:    "1", // accept every recommendation
		sliceSize:   "1", // small
		root:        "1", // the most-referenced table
		filter:      "",
		destination: "2", targetDSN: dst,
		review:      "1", // create the database
		saveProfile: "n",
	}.script())

	if err := createFlow(context.Background()); err != nil {
		t.Fatalf("createFlow: %v\n%s", err, buf.String())
	}

	// Rows arrived.
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, dst)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)
	var users int
	if err := conn.QueryRow(ctx, "SELECT count(*) FROM users").Scan(&users); err != nil {
		t.Fatal(err)
	}
	if users == 0 {
		t.Fatalf("no rows loaded:\n%s", buf.String())
	}

	// Artifacts were written without being asked for.
	for _, name := range []string{"README.md", "report.html", "summary.json", "tables.csv", "masking-rules.yaml"} {
		if _, err := os.Stat(filepath.Join(dir, report.DefaultDir, name)); err != nil {
			t.Errorf("missing artifact %s", name)
		}
	}
	// And a config the CLI can rerun.
	if _, err := os.Stat(filepath.Join(dir, "safeslice.yaml")); err != nil {
		t.Errorf("the wizard wrote no config: %v", err)
	}

	// Verification ran on its own, without a second command.
	body, err := os.ReadFile(filepath.Join(dir, report.DefaultDir, "summary.json"))
	if err != nil {
		t.Fatal(err)
	}
	var res report.Result
	if err := json.Unmarshal(body, &res); err != nil {
		t.Fatal(err)
	}
	if !res.Verification.Ran {
		t.Error("the wizard did not verify the database it just created")
	}
	if res.Verification.Caveat == "" {
		t.Error("a clean scan was reported without the caveat that it is not a proof")
	}
	if res.TotalRows == 0 {
		t.Error("summary.json reports no rows")
	}
}

func TestWizardCancelsWithoutWriting(t *testing.T) {
	src, dst := wizardSetup(t)
	chdir(t, t.TempDir())

	buf := script(t, answers{
		source: "1", sourceDSN: src,
		classify: "1", sliceSize: "1", root: "1", filter: "",
		destination: "2", targetDSN: dst,
		review:      "5", // cancel at the review screen
		saveProfile: "n",
	}.script())

	if err := createFlow(context.Background()); err != nil {
		t.Fatalf("createFlow: %v\n%s", err, buf.String())
	}

	ctx := context.Background()
	conn, err := pgx.Connect(ctx, dst)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)
	var users int
	if err := conn.QueryRow(ctx, "SELECT count(*) FROM users").Scan(&users); err != nil {
		t.Fatal(err)
	}
	if users != 0 {
		t.Errorf("cancelling still wrote %d rows", users)
	}
}

func TestWizardArtifactsNeverCarryTheSourcePassword(t *testing.T) {
	src, dst := wizardSetup(t)
	dir := t.TempDir()
	chdir(t, dir)
	flagNoOpen = true
	t.Cleanup(func() { flagNoOpen = false })

	// Force a password into the source DSN, whatever the test server uses.
	cfg, err := pgx.ParseConfig(src)
	if err != nil {
		t.Fatal(err)
	}
	// A short password is indistinguishable from ordinary prose: "ci" appears
	// inside "decision" and "specific" in the report, and a substring search
	// would report a leak that is not there. Anything this short cannot be
	// checked this way, so say so rather than failing misleadingly.
	if len(cfg.Password) < 8 {
		t.Skipf("test server password is %d characters; too short to search for meaningfully",
			len(cfg.Password))
	}

	script(t, answers{
		source: "1", sourceDSN: src,
		classify: "1", sliceSize: "1", root: "1", filter: "",
		destination: "2", targetDSN: dst,
		review:      "1",
		saveProfile: "y\nLocal development",
	}.script())
	if err := createFlow(context.Background()); err != nil {
		t.Fatalf("createFlow: %v", err)
	}

	err = filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return err
		}
		body, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if strings.Contains(string(body), cfg.Password) {
			t.Errorf("%s contains the source password", path)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// chdir moves into dir for the duration of a test. The wizard writes
// safeslice.yaml, .safeslice and safeslice-output relative to the working
// directory, which is exactly what has to be exercised.
func chdir(t *testing.T, dir string) {
	t.Helper()
	old, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chdir(old) })
}

func TestWizardSecondRunCanRefreshTheTarget(t *testing.T) {
	// safeslice inserts; it does not upsert. Running the wizard twice into the
	// same database used to collide on the primary key with no way out but a
	// DROP -- which is the most likely thing to happen to anyone who uses it
	// more than once.
	src, dst := wizardSetup(t)
	chdir(t, t.TempDir())
	flagNoOpen = true
	t.Cleanup(func() { flagNoOpen = false })

	first := answers{
		source: "1", sourceDSN: src,
		classify: "1", sliceSize: "1", root: "1", filter: "",
		destination: "2", targetDSN: dst,
		review:      "1",
		saveProfile: "n",
	}
	script(t, first.script())
	if err := createFlow(context.Background()); err != nil {
		t.Fatalf("first run: %v", err)
	}

	ctx := context.Background()
	conn, err := pgx.Connect(ctx, dst)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)
	var before int
	if err := conn.QueryRow(ctx, "SELECT count(*) FROM users").Scan(&before); err != nil {
		t.Fatal(err)
	}
	if before == 0 {
		t.Fatal("first run loaded nothing, so the rerun proves nothing")
	}

	// A fresh directory for the second run: the safeslice.yaml the first one
	// wrote would otherwise mean no classification question, shifting every
	// later answer by one.
	chdir(t, t.TempDir())

	// The target now holds rows, so the wizard must offer to empty it. "1"
	// takes that option; without it the load fails on a duplicate key.
	second := first
	second.targetDSN = dst + "\n1"
	buf := script(t, second.script())
	if err := createFlow(context.Background()); err != nil {
		t.Fatalf("second run: %v\n%s", err, buf.String())
	}

	var after int
	if err := conn.QueryRow(ctx, "SELECT count(*) FROM users").Scan(&after); err != nil {
		t.Fatal(err)
	}
	if after != before {
		t.Errorf("after refresh %d rows, want the same %d as a single run", after, before)
	}
}
