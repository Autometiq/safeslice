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

package ui

import (
	"bytes"
	"errors"
	"strings"
	"testing"
	"time"
)

// capture renders in plain mode, which is what CI and redirected output get.
func capture(t *testing.T, fn func()) string {
	t.Helper()
	var buf bytes.Buffer
	oldOut, oldPlain := out, plain
	out, plain = &buf, true
	t.Cleanup(func() { out, plain = oldOut, oldPlain })
	fn()
	return buf.String()
}

func captureStyled(t *testing.T, fn func()) string {
	t.Helper()
	var buf bytes.Buffer
	oldOut, oldPlain := out, plain
	out, plain = &buf, false
	t.Cleanup(func() { out, plain = oldOut, oldPlain })
	fn()
	return buf.String()
}

func TestPlainOutputHasNoEscapeSequencesOrBoxDrawing(t *testing.T) {
	got := capture(t, func() {
		Banner("v1.2.3")
		Info("connected")
		Section("Extract")
		Summary(RunStats{Tables: 3, Rows: 42, MaskedColumns: 5, Duration: 2 * time.Second})
	})
	if strings.Contains(got, "\x1b[") {
		t.Error("plain output contains ANSI escapes; CI logs would need stripping")
	}
	// Box-drawing characters turn into mojibake in log aggregators that do not
	// assume UTF-8.
	for _, r := range []string{"─", "│", "╭", "╰", "┌", "└"} {
		if strings.Contains(got, r) {
			t.Errorf("plain output contains box-drawing character %q", r)
		}
	}
}

func TestPlainBadgesAreGreppable(t *testing.T) {
	// Log scrapers key off these; the bracketed form must survive plain mode.
	cases := map[string]func(){
		"[INFO]":    func() { Info("x") },
		"[PLAN]":    func() { PlanStep("x") },
		"[MASK]":    func() { Mask("x") },
		"[SUCCESS]": func() { Success("x") },
		"[WARN]":    func() { Warn("x") },
		"[ERROR]":   func() { Error(errors.New("x")) },
	}
	for want, fn := range cases {
		if got := capture(t, fn); !strings.Contains(got, want) {
			t.Errorf("plain output %q does not contain %s", got, want)
		}
	}
}

func TestStyledOutputIsColoured(t *testing.T) {
	got := captureStyled(t, func() { Success("done") })
	if !strings.Contains(got, "done") {
		t.Fatalf("message lost: %q", got)
	}
}

func TestErrorIgnoresNil(t *testing.T) {
	if got := capture(t, func() { Error(nil) }); got != "" {
		t.Errorf("Error(nil) printed %q", got)
	}
}

func TestSummaryStatesOnlyWhatWasProven(t *testing.T) {
	got := capture(t, func() {
		Summary(RunStats{Tables: 3, Rows: 42, MaskedColumns: 5,
			Duration: 1500 * time.Millisecond, Target: "app@db:5432/dev"})
	})
	for _, want := range []string{"Tables processed", "3", "42", "1.50s", "app@db:5432/dev"} {
		if !strings.Contains(got, want) {
			t.Errorf("summary missing %q:\n%s", want, got)
		}
	}
	// Foreign keys stay enforced through the load, so a successful commit is
	// Postgres itself confirming this. Claiming it is honest.
	if !strings.Contains(got, "Zero FK orphans") {
		t.Error("summary does not report referential integrity")
	}
	// Nothing in a run inspects the loaded rows for PII, so a clean bill of
	// health here would be a claim the tool has not earned.
	if strings.Contains(got, "Zero PII leaks") {
		t.Error("summary claims zero PII leaks, which run never verifies")
	}
	if !strings.Contains(got, "safeslice verify") {
		t.Error("summary should point at the command that can actually prove it")
	}
}

func TestSummaryFlagsAnUnmaskedRun(t *testing.T) {
	// A run that masked nothing is far more likely to be a misconfiguration
	// than an intent, and it must not read as a success.
	got := capture(t, func() { Summary(RunStats{Tables: 1, Rows: 10}) })
	if !strings.Contains(got, "No columns masked") {
		t.Errorf("a run with no masking was reported as clean:\n%s", got)
	}
}

func TestSummaryShowsWarnings(t *testing.T) {
	got := capture(t, func() {
		Summary(RunStats{Tables: 1, Rows: 1, MaskedColumns: 1,
			Warnings: []string{`skipped "ALTER TABLE users DISABLE TRIGGER USER": permission denied`}})
	})
	if !strings.Contains(got, "permission denied") {
		t.Error("skipped optional steps must reach the summary, not vanish")
	}
}

func TestSummaryDistinguishesFileFromDatabase(t *testing.T) {
	db := capture(t, func() { Summary(RunStats{Target: "app@db:5432/dev"}) })
	if !strings.Contains(db, "Loaded into") {
		t.Error("database target not reported")
	}
	file := capture(t, func() { Summary(RunStats{OutFile: "slice.sql"}) })
	if !strings.Contains(file, "Written to") || !strings.Contains(file, "slice.sql") {
		t.Error("file destination not reported")
	}
}

func TestBannerCarriesAttribution(t *testing.T) {
	got := capture(t, func() { Banner("v0.1.0") })
	for _, want := range []string{"safeslice", "v0.1.0", Mission, Vendor, VendorURL} {
		if !strings.Contains(got, want) {
			t.Errorf("banner missing %q:\n%s", want, got)
		}
	}
}

func TestFooterIsOneLine(t *testing.T) {
	got := capture(t, func() { Footer() })
	if n := strings.Count(strings.TrimSpace(got), "\n"); n != 0 {
		t.Errorf("footer spans %d extra lines; it must stay out of the way:\n%s", n+1, got)
	}
	if !strings.Contains(got, VendorURL) {
		t.Error("footer missing the vendor URL")
	}
}

func TestDurationReadsNaturally(t *testing.T) {
	cases := map[time.Duration]string{
		500 * time.Microsecond:        "500µs",
		42 * time.Millisecond:         "42ms",
		1500 * time.Millisecond:       "1.50s",
		90 * time.Second:              "1m30s",
		2*time.Minute + 5*time.Second: "2m05s",
	}
	for d, want := range cases {
		if got := Duration(d); got != want {
			t.Errorf("Duration(%v) = %q, want %q", d, got, want)
		}
	}
}

func TestTableAligns(t *testing.T) {
	got := capture(t, func() {
		Table([]string{"TABLE", "ROWS"}, [][]string{{"users", "12"}, {"a", "3"}})
	})
	lines := strings.Split(strings.TrimRight(got, "\n"), "\n")
	if len(lines) != 3 {
		t.Fatalf("got %d lines, want header plus two rows:\n%s", len(lines), got)
	}
	// The short name must be padded to the width of the long one, or the
	// second column will not line up.
	if !strings.Contains(lines[2], "a      3") {
		t.Errorf("columns not aligned: %q", lines[2])
	}
}

func TestSetPlainOnlyTightens(t *testing.T) {
	// --no-color must be able to disable styling, but must never re-enable it
	// for output that is already known to be redirected.
	old := plain
	t.Cleanup(func() { plain = old })
	plain = true
	SetPlain(false)
	if !plain {
		t.Error("SetPlain(false) re-enabled styling for a non-terminal writer")
	}
	plain = false
	SetPlain(true)
	if !plain {
		t.Error("SetPlain(true) did not disable styling")
	}
}
