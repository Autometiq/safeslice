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

// Package verify scans a database for values that still look like real personal
// data.
//
// This is the check a compliance team asks for and no competing open-source
// tool ships: proof, after the fact, that the slice on someone's laptop is
// clean. It runs against the target, so it catches leaks whatever their cause
// -- a column nobody classified, a rule that did not match, a free-text field
// with an address buried in it.
package verify

import (
	"context"
	"fmt"
	"regexp"
	"sort"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/Autometiq/safeslice/internal/catalog"
)

// Finding is one column holding values that look like live personal data.
type Finding struct {
	Table   catalog.Ref
	Column  string
	Kind    string
	Matches int
	Sample  string // redacted: enough to locate the problem, not to leak it
}

// Check is one detector. The SQL predicate narrows candidates in the database;
// confirm (when set) makes the final decision in Go, for things a regular
// expression cannot decide.
type Check struct {
	Kind    string
	SQL     string
	Confirm func(string) bool
}

// Checks are deliberately tuned to ignore the values safeslice itself produces:
// a scanner that flags its own masked output is a scanner nobody will run.
func Checks() []Check {
	return []Check{
		{
			Kind: "email",
			// .invalid can never resolve, and .example is reserved for docs.
			SQL: `%[1]s ~* '[[:alnum:]._%%+-]+@[[:alnum:].-]+\.[[:alpha:]]{2,}'
			      AND %[1]s !~* '@(example\.(invalid|com|org|net)|test|localhost)$'`,
		},
		{
			Kind: "phone",
			// The left boundary is essential. Without it, "00" followed by digits
			// matches inside any hex-ish token -- including safeslice's own masked
			// emails, e.g. user_97e00477401de7b4@example.invalid, where 00477401
			// looks like an international prefix. A scanner that flags its own
			// output is one nobody keeps in a pipeline.
			//
			// Separators are allowed because real numbers are written
			// "+44 7700 900123", which a contiguous-digit pattern misses entirely.
			//
			// +1555 is the reserved fictional range safeslice emits.
			SQL: `%[1]s ~ '(^|[^[:alnum:]_])(\+|00)[0-9][0-9 ()._-]{5,16}[0-9]'
			      AND %[1]s !~ '\+1555'`,
		},
		{
			Kind: "ip",
			// 203.0.113.0/24 is RFC 5737 TEST-NET-3, what safeslice emits.
			// Private ranges are not personal data.
			SQL: `%[1]s ~ '\m([0-9]{1,3}\.){3}[0-9]{1,3}\M'
			      AND %[1]s !~ '\m(203\.0\.113\.|10\.|127\.|192\.168\.|172\.(1[6-9]|2[0-9]|3[01])\.)'`,
		},
		{
			Kind: "payment card",
			// Any long digit run is a candidate; Luhn decides. Without the
			// checksum this flags every order id in the database.
			SQL:     `%[1]s ~ '\m[0-9]{13,19}\M'`,
			Confirm: containsLuhnValid,
		},
		{
			Kind: "secret token",
			// Checks for AWS keys, Stripe live keys, GitHub tokens, and JWT tokens
			SQL: `%[1]s ~ '(AKIA[0-9A-Z]{16}|sk_live_[0-9a-zA-Z]{24}|ghp_[0-9a-zA-Z]{36}|eyJ[A-Za-z0-9_-]{10,}\.[A-Za-z0-9_-]{10,})'
			      AND %[1]s !~ 'REDACTED'`,
		},
		{
			Kind: "national id",
			// Checks for standard US SSN format (excluding known invalid prefixes 000, 666, 900-999)
			SQL: `%[1]s ~ '(^|[^0-9])([0-8][0-9]{2}-[0-9]{2}-[0-9]{4})([^0-9]|$)'
			      AND %[1]s !~ '(^|[^0-9])(000|666|9[0-9]{2})-'`,
		},
	}
}

var digitRun = regexp.MustCompile(`[0-9]{13,19}`)

// containsLuhnValid reports whether any long digit run in s passes the Luhn
// checksum, which is what separates a card number from an order reference.
func containsLuhnValid(s string) bool {
	for _, run := range digitRun.FindAllString(s, -1) {
		if luhn(run) {
			return true
		}
	}
	return false
}

func luhn(s string) bool {
	// An empty string sums to zero, which passes the modulo test. Card numbers
	// are 13-19 digits; anything shorter is not one.
	if len(s) < 13 || len(s) > 19 {
		return false
	}
	sum, double := 0, false
	for i := len(s) - 1; i >= 0; i-- {
		d := int(s[i] - '0')
		if d < 0 || d > 9 {
			return false
		}
		if double {
			if d *= 2; d > 9 {
				d -= 9
			}
		}
		sum += d
		double = !double
	}
	return sum%10 == 0
}

// CustomCheck defines a user-specified regex check.
type CustomCheck struct {
	Name    string
	Pattern string
}

// Options controls the scan.
type Options struct {
	// Sample caps how many rows per column are pulled back for checks that need
	// confirmation in Go. Zero means a sensible default.
	Sample int
	// Extra columns to skip, as "table.column".
	Ignore map[string]bool
	// Custom user-defined checks to evaluate.
	CustomChecks []CustomCheck
}

// Scan checks every text column of every table and returns what it found.
func Scan(ctx context.Context, conn *pgx.Conn, cat *catalog.Catalog, opt Options) ([]Finding, error) {
	if opt.Sample <= 0 {
		opt.Sample = 1000
	}
	var allChecks []Check
	allChecks = append(allChecks, Checks()...)
	for _, cc := range opt.CustomChecks {
		if cc.Pattern != "" {
			name := cc.Name
			if name == "" {
				name = "custom"
			}
			allChecks = append(allChecks, Check{
				Kind: name,
				SQL:  fmt.Sprintf(`%%[1]s ~ '%s'`, strings.ReplaceAll(cc.Pattern, `'`, `''`)),
			})
		}
	}

	var findings []Finding
	for _, ref := range cat.Refs() {
		t, ok := cat.Table(ref)
		if !ok || t.Partitioned {
			continue // rows are counted through the partitions themselves
		}
		for _, col := range t.Columns {
			if !isTextual(col) || opt.Ignore[ref.Name+"."+col.Name] {
				continue
			}
			for _, check := range allChecks {
				f, err := runCheck(ctx, conn, ref, col.Name, check, opt.Sample)
				if err != nil {
					return nil, err
				}
				if f != nil {
					findings = append(findings, *f)
				}
			}
		}
	}
	sort.Slice(findings, func(i, j int) bool {
		if findings[i].Matches != findings[j].Matches {
			return findings[i].Matches > findings[j].Matches
		}
		return findings[i].Table.String()+findings[i].Column <
			findings[j].Table.String()+findings[j].Column
	})
	return findings, nil
}

func isTextual(col catalog.Column) bool {
	t := strings.ToLower(col.Type)
	if strings.HasSuffix(t, "[]") {
		return false
	}
	return strings.Contains(t, "char") || strings.Contains(t, "text") ||
		t == "citext" || strings.Contains(t, "json")
}

func runCheck(ctx context.Context, conn *pgx.Conn, ref catalog.Ref, column string,
	check Check, sample int) (*Finding, error) {

	qualified := quoteIdent(ref.Schema) + "." + quoteIdent(ref.Name)
	expr := quoteIdent(column) + "::text"
	pred := fmt.Sprintf(check.SQL, expr)

	q := fmt.Sprintf("SELECT %s FROM %s WHERE %s IS NOT NULL AND (%s) LIMIT %d",
		expr, qualified, quoteIdent(column), pred, sample)
	rows, err := conn.Query(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("verify: scanning %s.%s for %s: %w", ref, column, check.Kind, err)
	}
	defer rows.Close()

	matches, sampleVal := 0, ""
	for rows.Next() {
		var v string
		if err := rows.Scan(&v); err != nil {
			return nil, err
		}
		if check.Confirm != nil && !check.Confirm(v) {
			continue
		}
		matches++
		if sampleVal == "" {
			sampleVal = redact(v)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if matches == 0 {
		return nil, nil
	}
	return &Finding{Table: ref, Column: column, Kind: check.Kind,
		Matches: matches, Sample: sampleVal}, nil
}

// redact shows enough of a value to find it, without reprinting the personal
// data into a terminal or a CI log -- which would be its own leak.
func redact(s string) string {
	s = strings.TrimSpace(strings.ReplaceAll(s, "\n", " "))
	if len(s) <= 6 {
		return strings.Repeat("*", len(s))
	}
	return s[:3] + strings.Repeat("*", min(len(s)-6, 12)) + s[len(s)-3:]
}

func quoteIdent(s string) string { return `"` + strings.ReplaceAll(s, `"`, `""`) + `"` }
