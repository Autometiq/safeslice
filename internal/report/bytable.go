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
	"sort"
	"strings"
)

// Regrouping the flat rule list by table.
//
// The artifacts record masking as a list of "table.column -> rule", which is
// the right shape for a machine and the wrong shape for a person. Nobody opens
// a report asking "which columns were masked"; they ask "what happened to my
// users table", and then "did anything get missed".

type tableGroup struct {
	name     string
	rows     int
	masked   int
	redacted int
	cols     []Rule
}

// byTable groups the masking rules by table and folds in the row counts, so a
// reader sees the whole picture for one table in one place. Every table in the
// slice appears, including the ones nothing touched.
func byTable(r Result) []tableGroup {
	groups := map[string]*tableGroup{}
	get := func(name string) *tableGroup {
		g, ok := groups[name]
		if !ok {
			g = &tableGroup{name: name}
			groups[name] = g
		}
		return g
	}

	// Row counts arrive schema-qualified ("public.users") while rules do not
	// ("users.email"), so the table name is normalised to the bare form.
	for _, t := range r.Tables {
		g := get(bare(t.Name))
		g.rows = t.ExtractedRows
	}
	add := func(rules []Rule, redacted bool) {
		for _, rule := range rules {
			table, column, found := strings.Cut(rule.Column, ".")
			if !found {
				continue // a bare column name has no table to group under
			}
			g := get(table)
			g.cols = append(g.cols, Rule{Column: column, Rule: rule.Rule})
			if redacted {
				g.redacted++
			} else {
				g.masked++
			}
		}
	}
	add(r.Rules, false)
	add(r.Redacted, true)

	out := make([]tableGroup, 0, len(groups))
	for _, g := range groups {
		sort.Slice(g.cols, func(i, j int) bool { return g.cols[i].Column < g.cols[j].Column })
		out = append(out, *g)
	}
	// Tables that had something done to them first, then by size: the reader is
	// looking for the masking, and the untouched tables are the appendix.
	sort.Slice(out, func(i, j int) bool {
		ai, aj := out[i].masked+out[i].redacted, out[j].masked+out[j].redacted
		if (ai == 0) != (aj == 0) {
			return ai > aj
		}
		if out[i].rows != out[j].rows {
			return out[i].rows > out[j].rows
		}
		return out[i].name < out[j].name
	})
	return out
}

// plural picks the right noun for a count. "1 columns masked" is the kind of
// detail a reader does not consciously notice and does quietly distrust.
func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}

// ruleMeaning says in words what a rule does to a value, so the report explains
// itself to someone who has never read the documentation.
func ruleMeaning(rule string) string {
	switch rule {
	case "email":
		return "an address at example.invalid, unique per input"
	case "phone":
		return "a +1555 number, the range reserved for fiction"
	case "name":
		return "a first and last name from a fixed list"
	case "first_name":
		return "a first name from a fixed list"
	case "last_name":
		return "a surname from a fixed list"
	case "address":
		return "a street address that does not exist"
	case "govid":
		return "uppercase hex — for SSNs, cards and tax IDs"
	case "ip":
		return "an address in the documentation range 203.0.113.0/24"
	case "secret":
		return "the literal REDACTED"
	case "redact":
		return "dropped — NULL, or empty when the column is NOT NULL"
	case "keep":
		return "left exactly as it was"
	default:
		return ""
	}
}
