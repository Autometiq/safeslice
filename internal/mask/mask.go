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
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

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
	DateShift Rule = "date_shift" // shift date/timestamp by deterministic offset (+/- 30 days)
	Date      Rule = "date"       // replace with a deterministic fake date
)

var known = map[Rule]bool{
	Keep: true, Redact: true, Secret: true, Email: true,
	Phone: true, GovID: true, FirstName: true, LastName: true, FullName: true,
	Address: true, IP: true, DateShift: true, Date: true,
}

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
	// Birth dates and sensitive date indicators default to deterministic date shifting
	{regexp.MustCompile(`(^|_)(dob|birth_?date|date_of_birth|birthday)($|_)`), DateShift},
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
	kindDate
	kindTime
	kindJSON
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
	case t == "date":
		return kindDate
	case strings.Contains(t, "timestamp"), strings.Contains(t, "timestamptz"), strings.Contains(t, "time"):
		return kindTime
	case t == "json", t == "jsonb":
		return kindJSON
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
			if kindOf(col) == kindJSON {
				empty = "{}"
			}
			return &empty, nil
		}
		return nil, nil
	}

	sum := m.digest(*in, salt)
	hexs := hex.EncodeToString(sum)
	n := binary.BigEndian.Uint64(sum[:8])

	out, err := m.render(rule, col, hexs, n, salt, in)
	if err != nil {
		return nil, err
	}
	if t, ok := out.(time.Time); ok {
		var formatted string
		if kindOf(col) == kindDate {
			formatted = t.Format("2006-01-02")
		} else {
			formatted = t.Format(time.RFC3339)
		}
		return &formatted, nil
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
		if kindOf(col) == kindDate || kindOf(col) == kindTime {
			return time.Time{}, nil
		}
		if kindOf(col) == kindJSON {
			return "{}", nil
		}
		return "", nil
	}
	var strVal string
	if t, ok := v.(time.Time); ok {
		strVal = t.Format(time.RFC3339Nano)
	} else {
		strVal = fmt.Sprintf("%v", v)
	}
	sum := m.digest(strVal, salt)
	out, err := m.render(rule, col, hex.EncodeToString(sum), binary.BigEndian.Uint64(sum[:8]), salt, v)
	if err != nil {
		return nil, err
	}
	if s, ok := out.(string); ok {
		return clamp(s, col.MaxLen), nil
	}
	return out, nil
}

// dateLayouts are common ISO/SQL string layouts tested when shifting text-based dates.
var dateLayouts = []string{
	time.RFC3339Nano,
	time.RFC3339,
	"2006-01-02 15:04:05.999999-07",
	"2006-01-02 15:04:05.999999",
	"2006-01-02 15:04:05-07",
	"2006-01-02 15:04:05",
	"2006-01-02",
}

// renderDateShift deterministically offsets a time.Time or date string by +/- 30 days.
func (m Masker) renderDateShift(n uint64, salt int, origVal any) (any, error) {
	// Deterministic offset between -30 and +30 days, adjusting if salt > 0
	offsetDays := int(n%61) - 30
	if salt > 0 {
		offsetDays += salt
	}

	if t, ok := origVal.(time.Time); ok {
		return t.AddDate(0, 0, offsetDays), nil
	}
	if strPtr, ok := origVal.(*string); ok && strPtr != nil {
		return shiftDateString(*strPtr, offsetDays, n)
	}
	if str, ok := origVal.(string); ok {
		return shiftDateString(str, offsetDays, n)
	}
	// Fallback to reference date if value type is unrecognized
	base := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	return base.AddDate(0, 0, offsetDays), nil
}

func shiftDateString(s string, offsetDays int, n uint64) (string, error) {
	for _, layout := range dateLayouts {
		if t, err := time.Parse(layout, s); err == nil {
			shifted := t.AddDate(0, 0, offsetDays)
			return shifted.Format(layout), nil
		}
	}
	// Unparseable string fallback: return deterministic ISO date
	year := int(2020 + (n % 6))
	month := time.Month(1 + (n % 12))
	day := int(1 + (n % 28))
	return fmt.Sprintf("%04d-%02d-%02d", year, month, day), nil
}

// renderJSON parses structured JSON / JSONB and deeply masks sensitive nested keys.
func (m Masker) renderJSON(origVal any, salt int) (any, error) {
	if origVal == nil {
		return nil, nil
	}
	var raw []byte
	isBytes := false
	switch v := origVal.(type) {
	case []byte:
		raw = v
		isBytes = true
	case string:
		raw = []byte(v)
	case *string:
		if v == nil {
			return nil, nil
		}
		raw = []byte(*v)
	default:
		b, err := json.Marshal(v)
		if err != nil {
			return "{}", nil
		}
		raw = b
	}

	if len(raw) == 0 {
		if isBytes {
			return []byte("{}"), nil
		}
		return "{}", nil
	}

	var parsed any
	if err := json.Unmarshal(raw, &parsed); err != nil {
		// Fallback for non-JSON content
		if isBytes {
			return []byte("{}"), nil
		}
		return "{}", nil
	}

	masked := m.maskJSONNode(parsed, salt)
	out, err := json.Marshal(masked)
	if err != nil {
		if isBytes {
			return []byte("{}"), nil
		}
		return "{}", nil
	}
	if isBytes {
		return out, nil
	}
	return string(out), nil
}

func (m Masker) maskJSONNode(node any, salt int) any {
	switch v := node.(type) {
	case map[string]any:
		result := make(map[string]any, len(v))
		for k, val := range v {
			rule := ruleForJSONKey(k)
			if rule != Keep && val != nil {
				if s, ok := val.(string); ok {
					maskedVal, err := m.Value(rule, catalog.Column{Name: k, Type: "text", MaxLen: -1}, &s, salt)
					if err == nil && maskedVal != nil {
						result[k] = *maskedVal
						continue
					}
				}
			}
			result[k] = m.maskJSONNode(val, salt)
		}
		return result
	case []any:
		result := make([]any, len(v))
		for i, elem := range v {
			result[i] = m.maskJSONNode(elem, salt)
		}
		return result
	default:
		return node
	}
}

func ruleForJSONKey(key string) Rule {
	low := strings.ToLower(key)
	for _, d := range defaultRules {
		if d.re.MatchString(low) {
			return d.rule
		}
	}
	return Keep
}

// render produces the replacement in the Go type matching the column.
func (m Masker) render(rule Rule, col catalog.Column, hexs string, n uint64, salt int, origVal any) (any, error) {
	switch k := kindOf(col); k {
	case kindOther:
		// Writing a fake string into a geometry, array or enum column would
		// corrupt the row. Refusing is the only safe answer.
		return nil, fmt.Errorf("column %s has type %s, which safeslice cannot mask safely; "+
			"set it to `keep` or `redact` in config", col.Name, col.Type)
	case kindJSON:
		if rule == Redact {
			if col.NotNull {
				return "{}", nil
			}
			return nil, nil
		}
		if rule == Secret {
			return "{}", nil
		}
		return m.renderJSON(origVal, salt)
	case kindDate, kindTime:
		if rule == DateShift || rule == Date {
			return m.renderDateShift(n, salt, origVal)
		}
		// If another rule (e.g. Secret) was applied to a date column, shift it safely
		return m.renderDateShift(n, salt, origVal)
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
		if rule == DateShift || rule == Date {
			return m.renderDateShift(n, salt, origVal)
		}
		return m.text(rule, hexs, n, salt), nil
	}
}

func (m Masker) text(rule Rule, hexs string, n uint64, salt int) string {
	switch rule {
	case Secret:
		// One literal reads best in a dev database, but a unique column needs
		// distinct values -- an api_key or token column is routinely UNIQUE, and
		// a constant would fail the load on its second row. The salt only rises
		// when the unique retry asks for another value.
		if salt == 0 {
			return "REDACTED"
		}
		return "REDACTED-" + hexs[:12]
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
	var prev *string
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
		// A rule that ignores the salt cannot escape a collision, so spinning
		// to 1000 only delays an error and hides its cause.
		if prev != nil && *prev == *v {
			return nil, errSaltInvariant
		}
		prev = v
	}
	return nil, fmt.Errorf("could not generate a unique masked value after 1000 attempts")
}

var errSaltInvariant = errors.New(
	"this rule produces the same value for every row, so it cannot satisfy a unique constraint; " +
		"use `secret` to replace the value instead")

// SatisfiesUnique reports whether a rule can fill a unique column.
//
// `redact` cannot fill a NOT NULL one: it writes the same empty value into
// every row, and no amount of salting changes that, so the load fails on the
// second row. Caught here, before a table is read, rather than three minutes
// into a run.
func SatisfiesUnique(rule Rule, col catalog.Column) error {
	if rule == Redact && col.NotNull {
		return fmt.Errorf("%s is UNIQUE and NOT NULL: `redact` would write the same empty "+
			"value into every row, which the constraint rejects. Use `secret` to replace "+
			"the value while keeping it unique", col.Name)
	}
	return nil
}

// UniqueColumns returns the single-column unique constraints on a table.
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

// CompositeUniques returns the multi-column (composite) unique constraints on a table.
func CompositeUniques(t *catalog.Table) [][]string {
	var out [][]string
	for _, set := range t.Uniques {
		if len(set) > 1 {
			out = append(out, set)
		}
	}
	if len(t.PK) > 1 {
		out = append(out, t.PK)
	}
	return out
}

// CompositeUniqueSet tracks composite tuples emitted for multi-column unique constraints
// and ensures joint values never collide under unique constraints.
type CompositeUniqueSet struct {
	seen map[string]bool
}

func NewCompositeUniqueSet() *CompositeUniqueSet {
	return &CompositeUniqueSet{seen: map[string]bool{}}
}

// Ensure calls gen with increasing salts until the composite tuple has not been used.
func (u *CompositeUniqueSet) Ensure(gen func(salt int) ([]string, error)) ([]string, error) {
	var prevKey string
	for salt := range 1000 {
		tuple, err := gen(salt)
		if err != nil {
			return nil, err
		}
		key := strings.Join(tuple, "\x00")
		if !u.seen[key] {
			u.seen[key] = true
			return tuple, nil
		}
		if prevKey != "" && prevKey == key {
			return nil, errSaltInvariant
		}
		prevKey = key
	}
	return nil, fmt.Errorf("could not generate a unique composite masked tuple after 1000 attempts")
}
