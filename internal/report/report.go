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

// Package report writes the artifacts a run leaves behind: a README, an offline
// HTML report, a JSON summary, a metadata CSV, and the masking rules that were
// applied.
//
// Two rules govern everything here.
//
// No credentials, ever. These files get committed, emailed and attached to
// tickets. A connection string is reduced to host, port, database and user
// before it is written anywhere, and a test asserts the password never appears
// in any artifact.
//
// No row data. The CSV is per-table metadata, not exported records. Writing
// masked rows to a file that people share would recreate the problem safeslice
// exists to solve.
package report

import (
	"encoding/csv"
	"encoding/json"
	"fmt"
	"html"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// DefaultDir is where artifacts are written, relative to the working directory.
const DefaultDir = "safeslice-output"

// Endpoint is a database location with the credentials stripped out.
type Endpoint struct {
	Host     string `json:"host"`
	Port     string `json:"port"`
	Database string `json:"database"`
	User     string `json:"user"`
}

func (e Endpoint) String() string {
	if e.Host == "" {
		return e.Database
	}
	return fmt.Sprintf("%s:%s/%s", e.Host, e.Port, e.Database)
}

// URL renders a connection string a developer can paste. It never carries a
// password: the target is local and the reader supplies their own credentials.
func (e Endpoint) URL() string {
	host := e.Host
	if e.Port != "" && e.Port != "5432" {
		host += ":" + e.Port
	}
	return "postgres://" + host + "/" + e.Database
}

// Redact parses a connection string and discards everything secret.
//
// This is the only way a DSN is allowed to reach an artifact. Passing the raw
// string through anywhere would put production credentials into a file people
// commit.
func Redact(dsn string) Endpoint {
	u, err := url.Parse(dsn)
	if err != nil || u.Host == "" {
		// Unparseable, or a key=value DSN. Report nothing rather than risk
		// leaking a fragment that contains the password.
		return Endpoint{Database: "(connection details withheld)"}
	}
	port := u.Port()
	if port == "" {
		port = "5432"
	}
	user := ""
	if u.User != nil {
		user = u.User.Username()
	}
	return Endpoint{
		Host:     u.Hostname(),
		Port:     port,
		Database: strings.TrimPrefix(u.Path, "/"),
		User:     user,
	}
}

// Table is one table's contribution to the slice.
type Table struct {
	Name          string `json:"table"`
	SourceRows    int64  `json:"source_rows"`
	ExtractedRows int    `json:"extracted_rows"`
	MaskedColumns int    `json:"masked_columns"`
}

// Edge is one foreign key, table to table.
type Edge struct {
	From    string `json:"from"`
	To      string `json:"to"`
	Virtual bool   `json:"virtual,omitempty"`
}

// Rule is one masking decision.
type Rule struct {
	Column string `json:"column"`
	Rule   string `json:"rule"`
}

// Verification records what the scanner concluded, in language that does not
// overclaim.
type Verification struct {
	Ran      bool     `json:"ran"`
	Passed   bool     `json:"passed"`
	Findings []string `json:"findings,omitempty"`
	// Caveat is stated in every artifact. A scanner finding nothing is not a
	// proof that nothing is there, and a compliance reader must not be left
	// with that impression.
	Caveat string `json:"caveat"`
}

// Result is everything a run produced.
type Result struct {
	Version     string        `json:"safeslice_version"`
	GeneratedAt time.Time     `json:"generated_at"`
	Duration    time.Duration `json:"-"`
	DurationMS  int64         `json:"duration_ms"`

	Source Endpoint `json:"source"`
	Target Endpoint `json:"target"`

	RootTable  string `json:"root_table"`
	Where      string `json:"where,omitempty"`
	ChildDepth int    `json:"child_depth"`
	// Seed is recorded because masking is deterministic: the same seed against
	// the same source reproduces this database exactly.
	Seed string `json:"seed"`

	Tables    []Table `json:"tables"`
	TotalRows int     `json:"total_rows"`
	Rules     []Rule  `json:"masking_rules"`
	Redacted  []Rule  `json:"redacted_columns"`
	// Relationships are the foreign keys the slice followed, and Soft the
	// column pairs that look like relationships but nothing enforces. The
	// second list is the one that explains a slice somebody thinks is missing
	// rows: safeslice cannot follow what the schema does not declare.
	Relationships []string `json:"relationships,omitempty"`
	Soft          []string `json:"unenforced_relationships,omitempty"`
	// Edges is the same information as Relationships, structured, so the
	// report can draw the graph instead of describing it.
	Edges []Edge `json:"edges,omitempty"`
	// Unreviewed lists columns a human left as `keep` without judging them.
	Unreviewed []string `json:"unreviewed_columns,omitempty"`

	FKOrphans    int          `json:"foreign_key_orphans"`
	Verification Verification `json:"verification"`
	Warnings     []string     `json:"warnings,omitempty"`
}

// Normalise fills the derived fields and sorts collections so artifacts are
// byte-identical across runs with the same inputs.
func (r *Result) Normalise() {
	if r.GeneratedAt.IsZero() {
		r.GeneratedAt = time.Now().UTC()
	}
	r.DurationMS = r.Duration.Milliseconds()
	if r.Verification.Caveat == "" {
		r.Verification.Caveat = "A clean scan means no known pattern of personal data was " +
			"detected in the sampled rows. It is not proof that none exists."
	}
	sort.Slice(r.Tables, func(i, j int) bool { return r.Tables[i].Name < r.Tables[j].Name })
	sort.Slice(r.Rules, func(i, j int) bool { return r.Rules[i].Column < r.Rules[j].Column })
	sort.Slice(r.Redacted, func(i, j int) bool { return r.Redacted[i].Column < r.Redacted[j].Column })
	sort.Strings(r.Unreviewed)
	sort.Strings(r.Relationships)
	sort.Strings(r.Soft)
	if r.TotalRows == 0 {
		for _, t := range r.Tables {
			r.TotalRows += t.ExtractedRows
		}
	}
}

// Write produces every artifact in dir, returning the paths written.
func Write(dir string, r Result) ([]string, error) {
	r.Normalise()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("report: creating %s: %w", dir, err)
	}

	files := []struct {
		name string
		body func() ([]byte, error)
	}{
		{"README.md", func() ([]byte, error) { return []byte(readme(r)), nil }},
		{"report.html", func() ([]byte, error) { return []byte(reportHTML(r)), nil }},
		{"summary.json", func() ([]byte, error) { return summaryJSON(r) }},
		{"tables.csv", func() ([]byte, error) { return tablesCSV(r) }},
		{"masking-rules.yaml", func() ([]byte, error) { return []byte(rulesYAML(r)), nil }},
	}

	var written []string
	for _, f := range files {
		body, err := f.body()
		if err != nil {
			return written, fmt.Errorf("report: building %s: %w", f.name, err)
		}
		path := filepath.Join(dir, f.name)
		if err := os.WriteFile(path, body, 0o644); err != nil {
			return written, fmt.Errorf("report: writing %s: %w", path, err)
		}
		written = append(written, path)
	}
	return written, nil
}

func status(ok bool) string {
	if ok {
		return "✓"
	}
	return "✗"
}

func readme(r Result) string {
	var b strings.Builder
	p := func(format string, a ...any) { fmt.Fprintf(&b, format, a...) }

	p("# Safeslice development database\n\n")
	p("Generated by safeslice %s on %s.\n\n", r.Version, r.GeneratedAt.Format(time.RFC1123))

	p("## Result\n\n")
	p("- %s %s rows loaded\n", status(r.TotalRows > 0), comma(r.TotalRows))
	p("- %s %d foreign-key orphans\n", status(r.FKOrphans == 0), r.FKOrphans)
	if r.Verification.Ran {
		p("- %s privacy scan %s\n", status(r.Verification.Passed),
			map[bool]string{true: "passed", false: "found personal data"}[r.Verification.Passed])
	} else {
		p("- ⚠ privacy scan did not run\n")
	}
	p("- Completed in %s\n\n", r.Duration.Round(time.Millisecond))

	p("> %s\n\n", r.Verification.Caveat)

	p("## Source\n\n")
	p("Read-only. safeslice never writes to the source.\n\n")
	p("| | |\n|---|---|\n")
	p("| Host | %s |\n| Port | %s |\n| Database | %s |\n| User | %s |\n\n",
		r.Source.Host, r.Source.Port, r.Source.Database, r.Source.User)

	p("## Slice\n\n")
	p("| | |\n|---|---|\n")
	p("| Root table | %s |\n", r.RootTable)
	if r.Where != "" {
		p("| Filter | `%s` |\n", r.Where)
	}
	p("| Child depth | %d |\n| Masking seed | `%s` |\n\n", r.ChildDepth, r.Seed)
	p("The same seed against the same source reproduces this database exactly.\n\n")

	p("## Tables\n\n| Table | Rows |\n|---|---:|\n")
	for _, t := range r.Tables {
		p("| %s | %s |\n", t.Name, comma(t.ExtractedRows))
	}
	p("| **Total** | **%s** |\n\n", comma(r.TotalRows))

	if len(r.Rules) > 0 {
		p("## Masking\n\n| Column | Rule |\n|---|---|\n")
		for _, rule := range r.Rules {
			p("| %s | %s |\n", rule.Column, rule.Rule)
		}
		p("\n%d columns transformed.\n\n", len(r.Rules))
	}
	if len(r.Redacted) > 0 {
		p("### Redacted\n\nThese values were dropped rather than replaced.\n\n")
		for _, rule := range r.Redacted {
			p("- %s\n", rule.Column)
		}
		p("\n")
	}
	if len(r.Unreviewed) > 0 {
		p("### Not reviewed\n\n")
		p("These text columns were left unchanged without being classified. ")
		p("They may contain personal data.\n\n")
		for _, c := range r.Unreviewed {
			p("- %s\n", c)
		}
		p("\n")
	}

	if len(r.Relationships) > 0 {
		p("## Relationships\n\nThe slice followed these foreign keys:\n\n")
		for _, rel := range r.Relationships {
			p("- %s\n", rel)
		}
		p("\n")
	}
	if len(r.Soft) > 0 {
		p("### Not enforced by the database\n\n")
		p("These columns look like relationships but have no foreign key, so ")
		p("safeslice could not follow them. Declare any that matter as ")
		p("`virtual_keys` in safeslice.yaml.\n\n")
		for _, s := range r.Soft {
			p("- %s\n", s)
		}
		p("\n")
	}

	p("## Connect\n\n")
	for _, s := range Snippets(r.Target) {
		p("**%s**\n\n```\n%s\n```\n\n", s.Name, s.Code)
	}

	if len(r.Warnings) > 0 {
		p("## Warnings\n\n")
		for _, w := range r.Warnings {
			p("- %s\n", w)
		}
		p("\n")
	}

	p("---\n\nsafeslice %s — https://autometiq.com\n", r.Version)
	return b.String()
}

func summaryJSON(r Result) ([]byte, error) {
	b, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(b, '\n'), nil
}

// tablesCSV writes per-table metadata only. Exporting rows would put the data
// safeslice just masked into a file people attach to tickets.
func tablesCSV(r Result) ([]byte, error) {
	var b strings.Builder
	w := csv.NewWriter(&b)
	if err := w.Write([]string{"table", "source_rows", "extracted_rows", "masked_columns"}); err != nil {
		return nil, err
	}
	for _, t := range r.Tables {
		if err := w.Write([]string{
			t.Name,
			fmt.Sprint(t.SourceRows),
			fmt.Sprint(t.ExtractedRows),
			fmt.Sprint(t.MaskedColumns),
		}); err != nil {
			return nil, err
		}
	}
	w.Flush()
	return []byte(b.String()), w.Error()
}

func rulesYAML(r Result) string {
	var b strings.Builder
	p := func(format string, a ...any) { fmt.Fprintf(&b, format, a...) }
	p("# Masking rules applied by safeslice %s on %s.\n", r.Version, r.GeneratedAt.Format(time.RFC3339))
	p("# Drop this into safeslice.yaml to reproduce the same masking.\n")
	p("mask:\n  seed: %s\n  rules:\n", r.Seed)
	if len(r.Rules) == 0 && len(r.Redacted) == 0 {
		p("    {}\n")
		return b.String()
	}
	for _, rule := range r.Rules {
		p("    %s: %s\n", rule.Column, rule.Rule)
	}
	for _, rule := range r.Redacted {
		p("    %s: %s\n", rule.Column, rule.Rule)
	}
	if len(r.Unreviewed) > 0 {
		p("\n    # Left unchanged without being classified -- decide on these.\n")
		for _, c := range r.Unreviewed {
			p("    # %s: keep\n", c)
		}
	}
	return b.String()
}

func comma(n int) string {
	s := fmt.Sprint(n)
	var b strings.Builder
	for i, r := range s {
		if i > 0 && (len(s)-i)%3 == 0 {
			b.WriteByte(',')
		}
		b.WriteRune(r)
	}
	return b.String()
}

func esc(s string) string { return html.EscapeString(s) }
