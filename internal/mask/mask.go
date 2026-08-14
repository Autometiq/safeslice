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

// Package mask replaces personal data with deterministic fakes.
//
// Two properties matter more than the quality of the fakes:
//
//   - Determinism. The same input always produces the same output, so a value
//     appearing in two tables (users.email and invoices.billing_email) masks
//     identically and any join on it still works.
//   - Key safety. Primary- and foreign-key columns are never masked. Rewriting
//     them would destroy the referential integrity the subsetter just built.
//
// Callers enforce key safety by passing the key column set to Table.
package mask

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"regexp"
	"strings"

	"github.com/Autometiq/safeslice/internal/catalog"
)

type Rule string

const (
	Keep      Rule = "keep"   // leave the value alone
	Redact    Rule = "redact" // drop the value entirely
	Secret    Rule = "secret"
	Email     Rule = "email"
	Phone     Rule = "phone"
	GovID     Rule = "govid"
	FirstName Rule = "first_name"
	LastName  Rule = "last_name"
	FullName  Rule = "name"
	Address   Rule = "address"
	IP        Rule = "ip"
)

var known = map[Rule]bool{Keep: true, Redact: true, Secret: true, Email: true,
	Phone: true, GovID: true, FirstName: true, LastName: true, FullName: true,
	Address: true, IP: true}

func ParseRule(s string) (Rule, error) {
	r := Rule(s)
	if !known[r] {
		return "", fmt.Errorf("unknown mask rule %q", s)
	}
	return r, nil
}

// defaultRules classify columns by name. First match wins, so the credential
// patterns are listed before the broad name patterns.
var defaultRules = []struct {
	re   *regexp.Regexp
	rule Rule
}{
	{regexp.MustCompile(`(^|_)(password|passwd|pwd|secret|token|api_?key|private_?key|access_?key)($|_)`), Secret},
	{regexp.MustCompile(`(^|_)e?mail($|_)`), Email},
	{regexp.MustCompile(`(^|_)(phone|mobile|tel|telephone|msisdn|fax)($|_)`), Phone},
	{regexp.MustCompile(`(^|_)(ssn|sin|nino|national_?id|tax_?id|passport|licen[cs]e_?no)($|_)`), GovID},
	{regexp.MustCompile(`(^|_)(iban|bic|swift|card_?number|cc_?num|pan|cvv|account_?number)($|_)`), GovID},
	{regexp.MustCompile(`(^|_)(first_?name|given_?name|forename)($|_)`), FirstName},
	{regexp.MustCompile(`(^|_)(last_?name|surname|family_?name)($|_)`), LastName},
	{regexp.MustCompile(`(^|_)(full_?name|display_?name|name)($|_)`), FullName},
	// IP must precede Address: "ip_address" matches both, and the address
	// pattern would otherwise fill an IP column with a street address.
	{regexp.MustCompile(`(^|_)ip(_?addr(ess)?)?($|_)`), IP},
	{regexp.MustCompile(`(^|_)(address|street|address_?line_?\d*|post_?code|zip_?code|postal_?code)($|_)`), Address},
}

// Classifier decides which rule applies to a column. Overrides are keyed either
// "schema.table.column", "table.column" or bare "column", most specific first.
type Classifier struct {
	Overrides map[string]Rule
}

// Rule returns the rule for a column, or Keep when nothing applies.
func (c Classifier) Rule(table catalog.Ref, column string) Rule {
	for _, key := range []string{
		table.String() + "." + column,
		table.Name + "." + column,
		column,
	} {
		if r, ok := c.Overrides[key]; ok {
			return r
		}
	}
	for _, d := range defaultRules {
		if d.re.MatchString(strings.ToLower(column)) {
			return d.rule
		}
	}
	return Keep
}

// Classified reports whether a human has made a decision about this column.
//
// An explicit `keep` counts. Strict mode is about unreviewed columns, not
// unmasked ones -- treating a deliberate `keep` as unclassified would make the
// escape hatch the error message recommends impossible to use.
func (c Classifier) Classified(table catalog.Ref, column string) bool {
	for _, key := range []string{
		table.String() + "." + column,
		table.Name + "." + column,
		column,
	} {
		if _, ok := c.Overrides[key]; ok {
			return true
		}
	}
	return false
}

// Unclassified returns text columns nobody has ruled on. Strict mode turns this
// into an error: silently passing an unreviewed column through is exactly how a
// leak happens.
func (c Classifier) Unclassified(t *catalog.Table, keys map[string]bool) []string {
	var out []string
	for _, col := range t.Columns {
		if keys[col.Name] || !col.Insertable() || c.Classified(t.Ref, col.Name) {
			continue
		}
		if kindOf(col) == kindText && c.Rule(t.Ref, col.Name) == Keep {
			out = append(out, col.Name)
		}
	}
	return out
}

type kind int

const (
	kindText kind = iota
	kindInt
	kindFloat
	kindDecimal
	kindUUID
	kindInet
	kindOther
)

// numeric groups the kinds that must not receive a text value.
func (k kind) numeric() bool { return k == kindInt || k == kindFloat || k == kindDecimal }

// kindOf classifies by type name rather than OID, because extension types
// (citext, hstore, PostGIS) get different OIDs in every database.
//
// The integer/float/decimal split matters at load time: the binary COPY
// protocol encodes by exact type, so handing an int64 to a double precision
// column is a wire-format error, not an implicit cast.
func kindOf(col catalog.Column) kind {
	t := strings.ToLower(col.Type)
	switch {
	case strings.HasSuffix(t, "[]"):
		return kindOther
	case strings.Contains(t, "char"), strings.Contains(t, "text"), t == "citext":
		return kindText
	case strings.Contains(t, "int"), strings.Contains(t, "serial"):
		return kindInt
	case strings.Contains(t, "double"), strings.Contains(t, "real"), strings.Contains(t, "float"):
		return kindFloat
	case strings.Contains(t, "numeric"), strings.Contains(t, "decimal"), strings.Contains(t, "money"):
		return kindDecimal
	case t == "uuid":
		return kindUUID
	case t == "inet", t == "cidr":
		return kindInet
	default:
		return kindOther
	}
}

var (
	firstNames = strings.Fields("Alex Sam Jordan Riley Casey Morgan Avery Quinn Rowan Skyler Emery Finley Harper Kai Noor Zaid")
	lastNames  = strings.Fields("Rivera Chen Okafor Novak Haddad Silva Kowalski Nguyen Ferrari Andersen Costa Mbeki Yilmaz Park Reyes Dubois")
	streets    = strings.Fields("Example Placeholder Sample Fixture Sandbox Reference")
)

// Masker generates replacement values. Seed is what makes a run reproducible;
// sharing the seed across a team makes everyone's snapshots line up.
type Masker struct {
	Seed string
}

func (m Masker) digest(value string, salt int) []byte {
	h := sha256.New()
	h.Write([]byte(m.Seed))
	h.Write([]byte{0})
	h.Write([]byte(value))
	if salt > 0 {
		var b [8]byte
		binary.BigEndian.PutUint64(b[:], uint64(salt))
		h.Write(b[:])
	}
	return h.Sum(nil)
}

// Value returns the replacement for one value.
//
// salt is 0 normally and increments only when a unique constraint forces a
// retry. nil input stays nil: a NULL is not personal data, and inventing a
// value for it would change query results.
func (m Masker) Value(rule Rule, col catalog.Column, in *string, salt int) (*string, error) {
	if in == nil || rule == Keep {
		return in, nil
	}
	if rule == Redact {
		if col.NotNull {
			empty := ""
			return &empty, nil
		}
		return nil, nil
	}

	sum := m.digest(*in, salt)
	hexs := hex.EncodeToString(sum)
	n := binary.BigEndian.Uint64(sum[:8])

	out, err := m.render(rule, col, hexs, n)
	if err != nil {
		return nil, err
	}
	s := clamp(fmt.Sprintf("%v", out), col.MaxLen)
	return &s, nil
}

// Apply masks a value straight from the driver and returns it in a Go type the
// destination column can accept. Value is the string-oriented view used by
// tests; this is the one the extractor calls.
func (m Masker) Apply(rule Rule, col catalog.Column, v any, salt int) (any, error) {
	if v == nil || rule == Keep {
		return v, nil
	}
	if rule == Redact {
		if !col.NotNull {
			return nil, nil
		}
		if kindOf(col).numeric() {
			return int64(0), nil
		}
		return "", nil
	}
	sum := m.digest(fmt.Sprintf("%v", v), salt)
	out, err := m.render(rule, col, hex.EncodeToString(sum), binary.BigEndian.Uint64(sum[:8]))
	if err != nil {
		return nil, err
	}
	if s, ok := out.(string); ok {
		return clamp(s, col.MaxLen), nil
	}
	return out, nil
}

// render produces the replacement in the Go type matching the column.
func (m Masker) render(rule Rule, col catalog.Column, hexs string, n uint64) (any, error) {
	switch k := kindOf(col); k {
	case kindOther:
		// Writing a fake string into a geometry, array or enum column would
		// corrupt the row. Refusing is the only safe answer.
		return nil, fmt.Errorf("column %s has type %s, which safeslice cannot mask safely; "+
			"set it to `keep` or `redact` in config", col.Name, col.Type)
	case kindInt:
		// A phone or account number held as an integer still needs masking, but
		// it has to stay an integer.
		return int64(n % 1_000_000_000_000), nil
	case kindFloat:
		return float64(n%1_000_000_000) / 100, nil
	case kindDecimal:
		// Passed as text: pgx encodes numeric from a string exactly, with no
		// float rounding on the way.
		return fmt.Sprintf("%d.%02d", n%1_000_000_000, n%100), nil
	case kindUUID:
		return fmt.Sprintf("%s-%s-4%s-8%s-%s",
			hexs[0:8], hexs[8:12], hexs[13:16], hexs[17:20], hexs[20:32]), nil
	case kindInet:
		return fmt.Sprintf("203.0.113.%d", n%254+1), nil // RFC 5737 TEST-NET-3
	default:
		return m.text(rule, hexs, n), nil
	}
}

func (m Masker) text(rule Rule, hexs string, n uint64) string {
	switch rule {
	case Secret:
		return "REDACTED"
	case Email:
		// 16 hex chars is 64 bits. At 10M rows the chance of two distinct
		// addresses colliding is ~3e-6; the unique-constraint retry covers the
		// rest. A shorter hash would collide often enough to matter.
		return "user_" + hexs[:16] + "@example.invalid"
	case Phone:
		return fmt.Sprintf("+1555%07d", n%10_000_000) // 555 is reserved fiction
	case GovID:
		return strings.ToUpper(hexs[:11])
	case FirstName:
		return firstNames[n%uint64(len(firstNames))]
	case LastName:
		return lastNames[n%uint64(len(lastNames))]
	case FullName:
		return firstNames[n%uint64(len(firstNames))] + " " +
			lastNames[(n/uint64(len(firstNames)))%uint64(len(lastNames))]
	case Address:
		return fmt.Sprintf("%d %s St", n%900+100, streets[n%uint64(len(streets))])
	case IP:
		return fmt.Sprintf("203.0.113.%d", n%254+1)
	default:
		return "REDACTED"
	}
}

// clamp keeps output inside a varchar(n) limit. Truncating a hash-derived value
// costs entropy, which is why the unique retry exists.
func clamp(s string, maxLen int) string {
	if maxLen <= 0 || len(s) <= maxLen {
		return s
	}
	return s[:maxLen]
}

// UniqueSet tracks values already emitted for one unique constraint and forces
// a fresh value on collision. Without this, two distinct emails hashing to the
// same fake violate the unique index and the whole restore fails.
type UniqueSet struct {
	seen map[string]bool
}

func NewUniqueSet() *UniqueSet { return &UniqueSet{seen: map[string]bool{}} }

// Ensure calls gen with increasing salts until the value has not been used.
func (u *UniqueSet) Ensure(gen func(salt int) (*string, error)) (*string, error) {
	for salt := range 1000 {
		v, err := gen(salt)
		if err != nil {
			return nil, err
		}
		if v == nil {
			return nil, nil // NULL does not participate in a unique constraint
		}
		if !u.seen[*v] {
			u.seen[*v] = true
			return v, nil
		}
	}
	return nil, fmt.Errorf("could not generate a unique masked value after 1000 attempts")
}

// UniqueColumns returns the single-column unique constraints on a table. Only
// single-column constraints are handled: a composite unique constraint stays
// satisfied as long as at least one of its columns is untouched, and columns
// that are all masked would need joint generation, which is not supported yet.
//
// ponytail: single-column only; add joint generation if a real schema needs it.
func UniqueColumns(t *catalog.Table) map[string]bool {
	out := map[string]bool{}
	for _, set := range t.Uniques {
		if len(set) == 1 {
			out[set[0]] = true
		}
	}
	if len(t.PK) == 1 {
		out[t.PK[0]] = true
	}
	return out
}
