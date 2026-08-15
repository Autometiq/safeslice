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

package report

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const secret = "sup3r-s3cret-pw"

func sample() Result {
	return Result{
		Version:     "v0.1.0",
		GeneratedAt: time.Date(2026, 8, 15, 9, 30, 0, 0, time.UTC),
		Duration:    786 * time.Millisecond,
		Source:      Redact("postgres://app:" + secret + "@prod-replica.internal:55432/shop"),
		Target:      Redact("postgres://dev:" + secret + "@localhost:5432/myapp_dev"),
		RootTable:   "public.users",
		Where:       "created_at > '2026-01-01'",
		ChildDepth:  1,
		Seed:        "team-seed",
		Tables: []Table{
			{Name: "public.users", SourceRows: 4000, ExtractedRows: 4000, MaskedColumns: 4},
			{Name: "public.orders", SourceRows: 8000, ExtractedRows: 2000},
			{Name: "public.events", SourceRows: 12000, ExtractedRows: 3000},
		},
		Rules: []Rule{
			{Column: "users.email", Rule: "email"},
			{Column: "users.password_hash", Rule: "secret"},
		},
		Redacted:     []Rule{{Column: "notes.body", Rule: "redact"}},
		Unreviewed:   []string{"shipments.carrier"},
		FKOrphans:    0,
		Verification: Verification{Ran: true, Passed: true},
		Warnings:     []string{"triggers could not be disabled on public.users"},
	}
}

func writeAll(t *testing.T, r Result) (dir string, byName map[string]string) {
	t.Helper()
	dir = t.TempDir()
	paths, err := Write(dir, r)
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	byName = map[string]string{}
	for _, p := range paths {
		b, err := os.ReadFile(p)
		if err != nil {
			t.Fatalf("reading %s: %v", p, err)
		}
		byName[filepath.Base(p)] = string(b)
	}
	return dir, byName
}

func TestWriteProducesEveryArtifact(t *testing.T) {
	_, files := writeAll(t, sample())
	for _, want := range []string{"README.md", "report.html", "summary.json",
		"tables.csv", "masking-rules.yaml"} {
		if files[want] == "" {
			t.Errorf("%s missing or empty", want)
		}
	}
}

// The single most important test in this package. These files get committed,
// emailed and attached to tickets; a password in one is a credential leak that
// outlives the database it opened.
func TestNoArtifactContainsThePassword(t *testing.T) {
	_, files := writeAll(t, sample())
	for name, body := range files {
		if strings.Contains(body, secret) {
			t.Errorf("CREDENTIAL LEAK: %s contains the source password", name)
		}
		for _, forbidden := range []string{"app:sup3r", "dev:sup3r", "@prod-replica.internal:55432/shop?"} {
			if strings.Contains(body, forbidden) {
				t.Errorf("%s contains credential fragment %q", name, forbidden)
			}
		}
	}
}

func TestRedactKeepsLocationAndDropsSecrets(t *testing.T) {
	e := Redact("postgres://app:hunter2@db.example.com:6543/shop?sslmode=require")
	if e.Host != "db.example.com" || e.Port != "6543" || e.Database != "shop" || e.User != "app" {
		t.Errorf("Redact lost location detail: %+v", e)
	}
	if strings.Contains(e.String(), "hunter2") || strings.Contains(e.URL(), "hunter2") {
		t.Error("password survived redaction")
	}
	// The target URL is for a reader to paste; it must never carry credentials.
	if strings.Contains(e.URL(), "app") {
		t.Errorf("URL() included a username: %s", e.URL())
	}
}

func TestRedactDefaultsThePort(t *testing.T) {
	if got := Redact("postgres://localhost/dev").Port; got != "5432" {
		t.Errorf("port = %q, want 5432", got)
	}
}

func TestRedactWithholdsUnparseableStrings(t *testing.T) {
	// A key=value DSN cannot be parsed apart safely. Reporting nothing beats
	// echoing a fragment that might carry the password.
	e := Redact("host=db user=app password=" + secret + " dbname=shop")
	if strings.Contains(e.Database+e.Host+e.User, secret) {
		t.Errorf("unparseable DSN leaked its password: %+v", e)
	}
}

func TestVerificationNeverClaimsProof(t *testing.T) {
	// A scanner finding nothing is not proof that nothing is there, and a
	// compliance reader must not be left with that impression.
	_, files := writeAll(t, sample())
	for _, name := range []string{"README.md", "report.html"} {
		body := files[name]
		if !strings.Contains(body, "not proof") {
			t.Errorf("%s does not qualify the scan result", name)
		}
		for _, overclaim := range []string{"zero PII", "guaranteed", "no personal data exists"} {
			if strings.Contains(body, overclaim) {
				t.Errorf("%s overclaims with %q", name, overclaim)
			}
		}
	}
}

func TestUnreviewedColumnsAreSurfaced(t *testing.T) {
	// Silence here is the failure mode: a column nobody judged must be visible
	// in the artifact a reviewer reads.
	_, files := writeAll(t, sample())
	for _, name := range []string{"README.md", "report.html", "masking-rules.yaml"} {
		if !strings.Contains(files[name], "shipments.carrier") {
			t.Errorf("%s does not mention the unreviewed column", name)
		}
	}
}

func TestCSVCarriesMetadataNotRows(t *testing.T) {
	_, files := writeAll(t, sample())
	csv := files["tables.csv"]
	if !strings.HasPrefix(csv, "table,source_rows,extracted_rows,masked_columns\n") {
		t.Errorf("unexpected CSV header:\n%s", csv)
	}
	if !strings.Contains(csv, "public.users,4000,4000,4") {
		t.Errorf("CSV missing the users row:\n%s", csv)
	}
	// Four columns only. Anything wider suggests row data crept in.
	for i, line := range strings.Split(strings.TrimSpace(csv), "\n") {
		if n := strings.Count(line, ",") + 1; n != 4 {
			t.Errorf("line %d has %d fields, want 4: %s", i, n, line)
		}
	}
}

func TestSummaryJSONRoundTrips(t *testing.T) {
	_, files := writeAll(t, sample())
	var got Result
	if err := json.Unmarshal([]byte(files["summary.json"]), &got); err != nil {
		t.Fatalf("summary.json is not valid JSON: %v", err)
	}
	if got.TotalRows != 9000 {
		t.Errorf("total rows = %d, want 9000 (summed from tables)", got.TotalRows)
	}
	if got.DurationMS != 786 {
		t.Errorf("duration = %dms, want 786", got.DurationMS)
	}
	if got.Seed != "team-seed" {
		t.Error("the seed must be recorded; it is what makes the run reproducible")
	}
}

func TestHTMLIsSelfContained(t *testing.T) {
	// The report is opened from disk, on locked-down laptops, months later.
	// Anything fetched over the network is broken in all three cases.
	_, files := writeAll(t, sample())
	h := files["report.html"]
	for _, external := range []string{"http://", "https://cdn", "<script", "src=", "@import"} {
		if strings.Contains(h, external) && external != "http://" {
			t.Errorf("report.html references something external: %q", external)
		}
	}
	// The one permitted link is the vendor site in the footer.
	if n := strings.Count(h, "https://"); n != 1 {
		t.Errorf("report.html has %d https references, want only the footer link", n)
	}
	if !strings.Contains(h, "<style>") {
		t.Error("CSS is not inlined")
	}
}

func TestHTMLEscapesInjectedValues(t *testing.T) {
	// Table and column names come from a database someone else controls.
	r := sample()
	r.RootTable = `users"><script>alert(1)</script>`
	r.Tables = []Table{{Name: `<img src=x onerror=alert(1)>`, ExtractedRows: 1}}
	_, files := writeAll(t, r)
	h := files["report.html"]
	if strings.Contains(h, "<script>alert(1)</script>") || strings.Contains(h, "<img src=x") {
		t.Error("report.html did not escape a hostile identifier")
	}
	if !strings.Contains(h, "&lt;script&gt;") {
		t.Error("expected the script tag to be escaped")
	}
}

func TestArtifactsAreDeterministic(t *testing.T) {
	// Same inputs, same bytes -- so these can be committed and diffed.
	r := sample()
	_, a := writeAll(t, r)
	_, b := writeAll(t, r)
	for name := range a {
		if a[name] != b[name] {
			t.Errorf("%s differs between runs with identical input", name)
		}
	}
}

func TestNormaliseSortsAndSums(t *testing.T) {
	r := Result{Tables: []Table{
		{Name: "zebra", ExtractedRows: 1},
		{Name: "alpha", ExtractedRows: 2},
	}}
	r.Normalise()
	if r.Tables[0].Name != "alpha" {
		t.Error("tables not sorted; artifacts would differ run to run")
	}
	if r.TotalRows != 3 {
		t.Errorf("total = %d, want 3", r.TotalRows)
	}
	if r.Verification.Caveat == "" {
		t.Error("the caveat must always be present, even when unset by the caller")
	}
}

func TestFailedVerificationIsReportedPlainly(t *testing.T) {
	r := sample()
	r.Verification = Verification{Ran: true, Passed: false,
		Findings: []string{"users.email — 12 rows look like live addresses"}}
	r.FKOrphans = 3
	_, files := writeAll(t, r)
	for _, name := range []string{"README.md", "report.html"} {
		body := files[name]
		if !strings.Contains(body, "users.email") {
			t.Errorf("%s hides the finding", name)
		}
		if !strings.Contains(body, "3") {
			t.Errorf("%s hides the orphan count", name)
		}
	}
}

func TestWriteCreatesTheDirectory(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "nested", DefaultDir)
	if _, err := Write(dir, sample()); err != nil {
		t.Fatalf("Write into a missing directory: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "README.md")); err != nil {
		t.Errorf("README not written: %v", err)
	}
}
