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
	"bytes"
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Autometiq/safeslice/internal/catalog"
	"github.com/Autometiq/safeslice/internal/config"
	"github.com/Autometiq/safeslice/internal/mask"
	"github.com/Autometiq/safeslice/internal/profile"
	"github.com/Autometiq/safeslice/internal/report"
	"github.com/Autometiq/safeslice/internal/ui"
)

// script drives the wizard from a canned set of answers, the way a user's
// keystrokes would. Output is captured so a test can assert on what the screen
// said as well as on what the answers produced.
func script(t *testing.T, answers string) *bytes.Buffer {
	t.Helper()
	buf := &bytes.Buffer{}
	ui.SetPlain(true)
	ui.SetOutput(buf)
	ui.SetInput(strings.NewReader(answers))
	return buf
}

func col(name, typ string, maxLen int) catalog.Column {
	return catalog.Column{Name: name, Type: typ, MaxLen: maxLen}
}

// wizardFixture is a two-table schema with one obvious PII column, one free-text
// column and one short coded column -- the three cases classification has to
// tell apart.
func wizardFixture() *catalog.Catalog {
	users := &catalog.Table{
		Ref: catalog.Ref{Schema: "public", Name: "users"},
		PK:  []string{"id"},
		Columns: []catalog.Column{
			col("id", "integer", -1),
			col("email", "text", -1),
			col("bio", "text", -1),
			col("status", "character varying(16)", 16),
		},
	}
	notes := &catalog.Table{
		Ref: catalog.Ref{Schema: "public", Name: "notes"},
		PK:  []string{"id"},
		Columns: []catalog.Column{
			col("id", "integer", -1),
			col("user_id", "integer", -1),
			col("bio", "text", -1),
		},
	}
	return &catalog.Catalog{
		Tables: map[string]*catalog.Table{users.Ref.String(): users, notes.Ref.String(): notes},
		FKs: []catalog.FK{{Name: "notes_user_fk", Table: notes.Ref, Columns: []string{"user_id"},
			RefTable: users.Ref, RefColumns: []string{"id"}}},
	}
}

func newTestWizard(t *testing.T) *wizard {
	t.Helper()
	return &wizard{
		ctx:       context.Background(),
		cfg:       config.Default(),
		cat:       wizardFixture(),
		store:     profile.Open(filepath.Join(t.TempDir(), ".safeslice")),
		decided:   map[string]mask.Rule{},
		reportDir: filepath.Join(t.TempDir(), "out"),
		source:    "postgres://app@prod.internal:5432/shop",
		target:    "postgres://localhost:5432/shop_dev",
	}
}

func TestRecommendationsFollowTheColumn(t *testing.T) {
	cases := []struct {
		column catalog.Column
		want   mask.Rule
	}{
		{col("bio", "text", -1), mask.Redact},                   // free text by name
		{col("ticket_body", "text", -1), mask.Redact},           // free text by name
		{col("status", "character varying(16)", 16), mask.Keep}, // short and constrained
		{col("col_7", "text", -1), mask.Redact},                 // unknown, unbounded: fail safe
	}
	for _, c := range cases {
		got, why := recommend(undecided{Table: catalog.Ref{Schema: "public", Name: "t"}, Column: c.column})
		if got != c.want {
			t.Errorf("recommend(%s %s) = %s, want %s", c.column.Name, c.column.Type, got, c.want)
		}
		if why.short == "" || len(why.long) == 0 {
			t.Errorf("recommend(%s) gave no reason", c.column.Name)
		}
	}
}

func TestUnclassifiedColumnsAreNeverSilentlyKept(t *testing.T) {
	// The property that matters: nothing reaches the config as `keep` without a
	// human choosing it. Accepting the recommendations must still write a rule
	// for every column, including the ones it recommends keeping.
	script(t, "1\n")
	w := newTestWizard(t)
	if err := w.classify(); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"users.bio", "users.status", "notes.bio"} {
		if _, ok := w.cfg.Mask.Rules[key]; !ok {
			t.Errorf("%s was left without a rule", key)
		}
	}
	if w.cfg.Mask.Rules["users.bio"] != string(mask.Redact) {
		t.Errorf("users.bio = %q, want redact", w.cfg.Mask.Rules["users.bio"])
	}
}

func TestReviewingIndividuallyRecordsEachAnswer(t *testing.T) {
	// "review individually", then: notes.bio -> keep, users.bio -> enter (the
	// remembered answer), users.status -> redact.
	script(t, "2\n3\n\n1\n")
	w := newTestWizard(t)
	if err := w.classify(); err != nil {
		t.Fatal(err)
	}
	if got := w.cfg.Mask.Rules["notes.bio"]; got != string(mask.Keep) {
		t.Errorf("notes.bio = %q, want keep", got)
	}
	if got := w.cfg.Mask.Rules["users.bio"]; got != string(mask.Keep) {
		t.Errorf("users.bio = %q, want the remembered answer for bio", got)
	}
	if got := w.cfg.Mask.Rules["users.status"]; got != string(mask.Redact) {
		t.Errorf("users.status = %q, want redact", got)
	}
}

func TestClassifyCanStopBeforeAnythingIsRead(t *testing.T) {
	script(t, "3\n")
	w := newTestWizard(t)
	err := w.classify()
	if err == nil {
		t.Fatal("classify continued after the user asked to stop")
	}
	var hinted *ui.Hinted
	if !errors.As(err, &hinted) {
		t.Errorf("error carries no next step: %v", err)
	}
}

func TestReclassifyAsksAgain(t *testing.T) {
	// "Change masking" on the review screen has to take back the decisions this
	// session made, or the second pass finds nothing left to ask about.
	script(t, "1\n")
	w := newTestWizard(t)
	if err := w.classify(); err != nil {
		t.Fatal(err)
	}
	before := len(w.cfg.Mask.Rules)

	buf := script(t, "1\n")
	if err := w.reclassify(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), "need a decision") {
		t.Errorf("reclassify did not ask again:\n%s", buf.String())
	}
	if len(w.cfg.Mask.Rules) != before {
		t.Errorf("rules = %d, want the same %d after re-answering", len(w.cfg.Mask.Rules), before)
	}
}

func TestReviewOffersAWayBackToEveryAnswer(t *testing.T) {
	counts := map[string]int{"public.users": 4000, "public.notes": 120}
	cases := map[string]planChange{
		"2\n": planMasking,
		"3\n": planSlice,
		"4\n": planTarget,
		"5\n": planCancel,
	}
	for answer, want := range cases {
		script(t, answer)
		w := newTestWizard(t)
		err := w.review(counts, 4120)
		var chg changePlan
		if !errors.As(err, &chg) {
			t.Fatalf("review(%q) = %v, want a change request", answer, err)
		}
		if chg.what != want {
			t.Errorf("review(%q) = %v, want %v", answer, chg.what, want)
		}
	}
}

func TestReviewProceedsOnlyWhenAsked(t *testing.T) {
	script(t, "1\n")
	w := newTestWizard(t)
	if err := w.review(map[string]int{"public.users": 10}, 10); err != nil {
		t.Errorf("review = %v, want nil so the run can continue", err)
	}
}

func TestReviewShowsTheNumbersBeforeAnyDataMoves(t *testing.T) {
	buf := script(t, "1\n")
	w := newTestWizard(t)
	w.cfg.Slice.Root = "users"
	_ = w.review(map[string]int{"public.users": 4000, "public.notes": 120}, 4120)

	got := buf.String()
	for _, want := range []string{"4,000", "120", "4,120", "SAFESLICE REVIEW", "read-only"} {
		if !strings.Contains(got, want) {
			t.Errorf("the review screen never showed %q:\n%s", want, got)
		}
	}
}

func TestReviewNeverPrintsTheSourcePassword(t *testing.T) {
	buf := script(t, "1\n")
	w := newTestWizard(t)
	w.source = "postgres://app:hunter2@prod.internal:5432/shop"
	_ = w.review(map[string]int{"public.users": 1}, 1)
	if strings.Contains(buf.String(), "hunter2") {
		t.Errorf("the review screen leaked the password:\n%s", buf.String())
	}
}

func TestConnectionFailuresAreExplainedInPlainLanguage(t *testing.T) {
	cases := map[string]string{
		`failed to connect: password authentication failed for user "app"`:          "username or password",
		`database "myapp_dev" does not exist`:                                       "does not exist",
		"dial tcp 127.0.0.1:5432: i/o timeout":                                      "did not answer",
		"dial tcp 127.0.0.1:5432: connectex: refused":                               "PostgreSQL is not running",
		`FATAL: no pg_hba.conf entry for host "1.2.3.4", user "app", no encryption`: "sslmode=require",
		"failed to connect: server does not support SSL":                            "sslmode=require",
	}
	for msg, want := range cases {
		if got := causesFor(errors.New(msg)); !strings.Contains(got, want) {
			t.Errorf("causesFor(%q) = %q, want it to mention %q", msg, got, want)
		}
	}
}

func TestTargetNameDefaultsAwayFromProduction(t *testing.T) {
	cases := map[string]string{"shop": "shop_dev", "shop_prod": "shop_dev", "": "myapp_dev"}
	for in, want := range cases {
		if got := defaultTargetName(in); got != want {
			t.Errorf("defaultTargetName(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestWriteConfigRoundTripsThroughTheLoader(t *testing.T) {
	// The wizard's output is an ordinary safeslice.yaml. If it did not load, a
	// user could not repeat the run with `safeslice run`, which is the point of
	// writing it at all.
	dir := t.TempDir()
	path := filepath.Join(dir, "safeslice.yaml")
	old := flagConfig
	flagConfig = path
	t.Cleanup(func() { flagConfig = old })

	script(t, "1\n")
	w := newTestWizard(t)
	w.cfg.Slice.Root = "users"
	w.cfg.Slice.Limit = 5000
	if err := w.classify(); err != nil {
		t.Fatal(err)
	}
	if err := w.writeConfig(); err != nil {
		t.Fatal(err)
	}

	got, err := config.Load(path)
	if err != nil {
		t.Fatalf("the generated config does not load: %v", err)
	}
	if got.Slice.Root != "users" || got.Slice.Limit != 5000 {
		t.Errorf("slice = %+v, want the wizard's answers", got.Slice)
	}
	if got.Mask.Rules["users.bio"] != string(mask.Redact) {
		t.Errorf("masking decisions did not survive: %+v", got.Mask.Rules)
	}
	if !got.StrictEnabled() {
		t.Error("strict masking was turned off by the wizard")
	}
}

func TestMaskingPreviewInventsItsExample(t *testing.T) {
	// Demonstrating masking with a real row would mean reading and printing
	// production data to prove production data is not printed.
	buf := script(t, "")
	w := newTestWizard(t)
	w.cfg.Mask.Rules = map[string]string{"users.email": "email"}
	w.maskingPreview()

	got := buf.String()
	if !strings.Contains(got, "john@example.com") {
		t.Errorf("no worked example:\n%s", got)
	}
	if !strings.Contains(got, "invented") {
		t.Errorf("the example does not say it is invented:\n%s", got)
	}
}

func TestSoftRelationshipsReachTheReport(t *testing.T) {
	got := describeSoft(nil)
	if len(got) != 0 {
		t.Errorf("describeSoft(nil) = %v", got)
	}
	fks := []catalog.FK{{Table: catalog.Ref{Schema: "public", Name: "notes"},
		Columns: []string{"user_id"}, RefTable: catalog.Ref{Schema: "public", Name: "users"},
		RefColumns: []string{"id"}, Virtual: true}}
	if lines := describeFKs(fks); len(lines) != 1 || !strings.Contains(lines[0], "[virtual]") {
		t.Errorf("describeFKs = %v, want the virtual marker", lines)
	}
}

func TestResultsScreenExitsCleanly(t *testing.T) {
	// "Done" is the last option. Choosing it must leave without opening
	// anything -- a test that launches a browser is a test nobody runs twice.
	buf := script(t, "8\n")
	w := newTestWizard(t)
	showResults(w.reportDir, report.Endpoint{Host: "localhost", Port: "5432", Database: "shop_dev"})

	got := buf.String()
	if !strings.Contains(got, w.reportDir) {
		t.Errorf("the results screen never said where the artifacts are:\n%s", got)
	}
}

func TestResultsScreenOffersEveryArtifact(t *testing.T) {
	buf := script(t, "8\n")
	w := newTestWizard(t)
	showResults(w.reportDir, report.Endpoint{Host: "localhost", Database: "shop_dev"})

	got := buf.String()
	for _, want := range []string{
		"HTML report", "output folder", "connection string",
		"README.md", "tables.csv", "summary.json", "masking-rules.yaml",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("results screen is missing %q:\n%s", want, got)
		}
	}
}

func TestResultsScreenTerminatesOnClosedInput(t *testing.T) {
	// The bug this pins: with stdin exhausted the menu took its default,
	// re-prompted, read EOF again and looped forever. Under `go test` that is
	// a ten-minute timeout, and in a piped run it is a process that never
	// exits. The test itself would hang without the fix, so the timeout is
	// the assertion.
	done := make(chan struct{})
	go func() {
		defer close(done)
		script(t, "")
		w := newTestWizard(t)
		showResults(w.reportDir, report.Endpoint{Host: "localhost", Database: "shop_dev"})
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the results screen never returned with input closed")
	}
}
