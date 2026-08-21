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

package mask

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/Autometiq/safeslice/internal/catalog"
)

var users = catalog.Ref{Schema: "public", Name: "users"}

func col(name, typ string, opts ...func(*catalog.Column)) catalog.Column {
	c := catalog.Column{Name: name, Type: typ, MaxLen: -1}
	for _, o := range opts {
		o(&c)
	}
	return c
}

func notNull(c *catalog.Column) { c.NotNull = true }
func maxLen(n int) func(*catalog.Column) {
	return func(c *catalog.Column) { c.MaxLen = n }
}

func s(v string) *string { return &v }

func mustValue(t *testing.T, m Masker, r Rule, c catalog.Column, in *string) *string {
	t.Helper()
	out, err := m.Value(r, c, in, 0)
	if err != nil {
		t.Fatalf("Value(%s, %s): %v", r, c.Name, err)
	}
	return out
}

func TestDeterministic(t *testing.T) {
	m := Masker{Seed: "team-seed"}
	c := col("email", "text")
	a := mustValue(t, m, Email, c, s("real@corp.com"))
	b := mustValue(t, m, Email, c, s("real@corp.com"))
	if *a != *b {
		t.Fatalf("same input gave %q then %q; joins on this column would break", *a, *b)
	}
	other := Masker{Seed: "different"}
	if *mustValue(t, other, Email, c, s("real@corp.com")) == *a {
		t.Error("different seeds must produce different values")
	}
}

func TestConsistentAcrossTablesAndColumns(t *testing.T) {
	// users.email and invoices.billing_email hold the same address; if they
	// mask differently, every join between them silently returns nothing.
	m := Masker{Seed: "s"}
	a := mustValue(t, m, Email, col("email", "text"), s("dup@corp.com"))
	b := mustValue(t, m, Email, col("billing_email", "varchar"), s("dup@corp.com"))
	if *a != *b {
		t.Errorf("same value masked to %q and %q across columns", *a, *b)
	}
}

func TestNullStaysNull(t *testing.T) {
	m := Masker{Seed: "s"}
	if got := mustValue(t, m, Email, col("email", "text"), nil); got != nil {
		t.Errorf("NULL became %q; that changes query results", *got)
	}
}

func TestNothingRoutableEscapes(t *testing.T) {
	m := Masker{Seed: "s"}
	email := *mustValue(t, m, Email, col("email", "text"), s("victim@real-company.com"))
	if !strings.HasSuffix(email, "@example.invalid") {
		t.Errorf("email = %q, want an .invalid domain that cannot receive mail", email)
	}
	if strings.Contains(email, "real-company") {
		t.Error("source domain leaked into the masked value")
	}
	if got := *mustValue(t, m, Phone, col("phone", "text"), s("+441234567890")); !strings.HasPrefix(got, "+1555") {
		t.Errorf("phone = %q, want the reserved 555 range", got)
	}
	if got := *mustValue(t, m, IP, col("ip", "text"), s("8.8.8.8")); !strings.HasPrefix(got, "203.0.113.") {
		t.Errorf("ip = %q, want RFC 5737 TEST-NET-3", got)
	}
	if got := *mustValue(t, m, Secret, col("password", "text"), s("hunter2")); got != "REDACTED" {
		t.Errorf("secret = %q, want REDACTED", got)
	}
}

func TestTypeIsPreserved(t *testing.T) {
	m := Masker{Seed: "s"}
	// A phone number stored as bigint still needs masking, but writing
	// "+1555..." into an integer column fails the load outright.
	got := *mustValue(t, m, Phone, col("phone", "bigint"), s("441234567890"))
	for _, r := range got {
		if r < '0' || r > '9' {
			t.Fatalf("numeric column got non-numeric value %q", got)
		}
	}
	uuid := *mustValue(t, m, GovID, col("external_id", "uuid"), s("6f1c..."))
	if len(uuid) != 36 || strings.Count(uuid, "-") != 4 {
		t.Errorf("uuid column got %q, which is not a uuid", uuid)
	}
	inet := *mustValue(t, m, IP, col("addr", "inet"), s("8.8.8.8"))
	if !strings.HasPrefix(inet, "203.0.113.") {
		t.Errorf("inet column got %q", inet)
	}
}

func TestUnmaskableTypeIsAnErrorNotCorruption(t *testing.T) {
	m := Masker{Seed: "s"}
	// Writing a fake string into a geometry or array column would corrupt the
	// row. Failing loudly is the only safe option.
	if _, err := m.Value(FullName, col("shape", "geometry"), s("x"), 0); err == nil {
		t.Error("masking an unsupported type must fail, not guess")
	}
	if _, err := m.Value(FullName, col("tags", "text[]"), s("x"), 0); err == nil {
		t.Error("array columns must not be masked silently")
	}
}

func TestVarcharLengthRespected(t *testing.T) {
	m := Masker{Seed: "s"}
	got := *mustValue(t, m, Email, col("email", "character varying(20)", maxLen(20)), s("a@b.com"))
	if len(got) > 20 {
		t.Errorf("masked value %q is %d chars, exceeds varchar(20)", got, len(got))
	}
}

func TestRedactRespectsNotNull(t *testing.T) {
	m := Masker{Seed: "s"}
	if got := mustValue(t, m, Redact, col("body", "text"), s("secret note")); got != nil {
		t.Errorf("redact on a nullable column = %q, want NULL", *got)
	}
	got := mustValue(t, m, Redact, col("body", "text", notNull), s("secret note"))
	if got == nil || *got != "" {
		t.Error("redact on a NOT NULL column must emit empty string, not NULL")
	}
}

func TestClassifierDefaults(t *testing.T) {
	c := Classifier{}
	cases := map[string]Rule{
		"email": Email, "user_email": Email, "billing_email": Email,
		"phone": Phone, "mobile_number": Phone, "phone_number": Phone,
		"ssn": GovID, "national_id": GovID, "card_number": GovID,
		"first_name": FirstName, "surname": LastName, "full_name": FullName,
		"street_address": Address, "postcode": Address,
		"ip_address": IP, "api_key": Secret, "password": Secret,
		"id": Keep, "created_at": Keep, "amount": Keep,
	}
	for column, want := range cases {
		if got := c.Rule(users, column); got != want {
			t.Errorf("Rule(%q) = %q, want %q", column, got, want)
		}
	}
}

func TestClassifierOverridePrecedence(t *testing.T) {
	c := Classifier{Overrides: map[string]Rule{
		"companies.name":     Keep,   // a company name is not personal data
		"users.notes":        Secret, // a column the heuristics cannot know about
		"public.users.email": Redact, // fully qualified wins
	}}
	if got := c.Rule(catalog.Ref{Schema: "public", Name: "companies"}, "name"); got != Keep {
		t.Errorf("table-qualified override ignored: got %q", got)
	}
	if got := c.Rule(users, "notes"); got != Secret {
		t.Errorf("override for unclassified column ignored: got %q", got)
	}
	if got := c.Rule(users, "email"); got != Redact {
		t.Errorf("schema-qualified override should beat the default rule: got %q", got)
	}
}

func TestUnclassifiedFindsLeakRisks(t *testing.T) {
	tbl := &catalog.Table{Ref: users, Columns: []catalog.Column{
		col("id", "bigint"),
		col("email", "text"),
		col("support_note", "text"), // free text: the classic silent leak
		col("created_at", "timestamptz"),
	}}
	got := Classifier{}.Unclassified(tbl, map[string]bool{"id": true})
	if len(got) != 1 || got[0] != "support_note" {
		t.Errorf("Unclassified = %v, want [support_note]", got)
	}
}

func TestExplicitKeepSatisfiesStrictMode(t *testing.T) {
	// Strict mode is about unreviewed columns, not unmasked ones. The error it
	// raises tells the user to mark harmless columns `keep`; if that did not
	// clear the error, the advice would be impossible to follow.
	tbl := &catalog.Table{Ref: users, Columns: []catalog.Column{
		col("id", "bigint"),
		col("sku", "text"),
		col("support_note", "text"),
	}}
	cl := Classifier{Overrides: map[string]Rule{"users.sku": Keep}}
	got := cl.Unclassified(tbl, map[string]bool{"id": true})
	for _, c := range got {
		if c == "sku" {
			t.Error("a column explicitly marked `keep` is still reported as unclassified")
		}
	}
	if len(got) != 1 || got[0] != "support_note" {
		t.Errorf("Unclassified = %v, want just [support_note]", got)
	}
}

func TestUniqueSetForcesFreshValues(t *testing.T) {
	u := NewUniqueSet()
	// Simulate a hash collision: the generator returns the same value until
	// salted. Without the retry this violates the unique index on restore.
	gen := func(salt int) (*string, error) {
		if salt == 0 {
			return s("collide"), nil
		}
		return s("unique-" + string(rune('a'+salt))), nil
	}
	first, err := u.Ensure(func(int) (*string, error) { return s("collide"), nil })
	if err != nil || *first != "collide" {
		t.Fatalf("first value = %v, %v", first, err)
	}
	second, err := u.Ensure(gen)
	if err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	if *second == "collide" {
		t.Error("duplicate value emitted; the unique index would reject the load")
	}
}

func TestUniqueSetIgnoresNull(t *testing.T) {
	u := NewUniqueSet()
	for range 2 {
		got, err := u.Ensure(func(int) (*string, error) { return nil, nil })
		if err != nil || got != nil {
			t.Fatalf("NULL must pass through; multiple NULLs are legal under a unique index")
		}
	}
}

func TestUniqueColumnsFromPKAndConstraints(t *testing.T) {
	tbl := &catalog.Table{
		Ref:     users,
		PK:      []string{"id"},
		Uniques: [][]string{{"email"}, {"tenant_id", "slug"}},
	}
	got := UniqueColumns(tbl)
	if !got["id"] || !got["email"] {
		t.Errorf("UniqueColumns = %v, want id and email", got)
	}
	if got["tenant_id"] || got["slug"] {
		t.Error("composite unique columns must not be treated as individually unique")
	}
}

func TestParseRule(t *testing.T) {
	if _, err := ParseRule("email"); err != nil {
		t.Errorf("valid rule rejected: %v", err)
	}
	if _, err := ParseRule("obfuscate"); err == nil {
		t.Error("a typo in config must fail loudly, not silently keep the real value")
	}
}

func TestSecretFillsAUniqueColumn(t *testing.T) {
	// The bug this pins: `secret` used to return the same literal for every
	// row, so a UNIQUE api_key or token column failed the load on its second
	// row -- after the slice had already been extracted and masked.
	m := Masker{Seed: "team-seed"}
	c := col("api_key", "text", notNull)
	u := NewUniqueSet()

	seen := map[string]bool{}
	for _, in := range []string{"key-a", "key-b", "key-c"} {
		got, err := u.Ensure(func(salt int) (*string, error) { return m.Value(Secret, c, s(in), salt) })
		if err != nil {
			t.Fatalf("Ensure(%s): %v", in, err)
		}
		if seen[*got] {
			t.Fatalf("secret produced %q twice; a unique constraint would reject it", *got)
		}
		seen[*got] = true
	}
	if !seen["REDACTED"] {
		t.Error("the first row should still read REDACTED; only collisions need a suffix")
	}
}

func TestRedactCannotFillAUniqueNotNullColumn(t *testing.T) {
	// No salt escapes an empty string, so this has to be refused up front
	// rather than discovered a thousand retries into a load.
	if err := SatisfiesUnique(Redact, col("slug", "text", notNull)); err == nil {
		t.Error("redact on a UNIQUE NOT NULL column was accepted")
	}
	// Nullable is fine: NULLs do not collide in a unique index.
	if err := SatisfiesUnique(Redact, col("slug", "text")); err != nil {
		t.Errorf("redact on a nullable unique column was refused: %v", err)
	}
	if err := SatisfiesUnique(Email, col("email", "text", notNull)); err != nil {
		t.Errorf("email on a unique column was refused: %v", err)
	}
}

func TestUniqueSetGivesUpOnASaltInvariantRule(t *testing.T) {
	u := NewUniqueSet()
	constant := func(int) (*string, error) { return s("same"), nil }
	if _, err := u.Ensure(constant); err != nil {
		t.Fatalf("first value: %v", err)
	}
	_, err := u.Ensure(constant)
	if err == nil {
		t.Fatal("a constant generator was allowed to collide silently")
	}
	if !strings.Contains(err.Error(), "secret") {
		t.Errorf("error does not say what to do instead: %v", err)
	}
}

func TestDateShift(t *testing.T) {
	m := Masker{Seed: "team-seed"}
	c := col("birth_date", "date")

	inDate := "1990-05-15"
	out1 := mustValue(t, m, DateShift, c, s(inDate))
	out2 := mustValue(t, m, DateShift, c, s(inDate))

	if *out1 != *out2 {
		t.Errorf("DateShift is non-deterministic: got %q and %q", *out1, *out2)
	}
	if *out1 == inDate {
		t.Errorf("DateShift did not change the input date: %q", *out1)
	}

	// Apply with actual time.Time
	parsedTime, _ := time.Parse("2006-01-02", inDate)
	applied, err := m.Apply(DateShift, c, parsedTime, 0)
	if err != nil {
		t.Fatalf("Apply(DateShift): %v", err)
	}
	shiftedTime, ok := applied.(time.Time)
	if !ok {
		t.Fatalf("Apply(DateShift) expected time.Time, got %T", applied)
	}
	if shiftedTime.Equal(parsedTime) {
		t.Errorf("shifted time equals original time: %v", shiftedTime)
	}
}

func TestDateRule(t *testing.T) {
	m := Masker{Seed: "team-seed"}
	c := col("event_date", "date")

	out := mustValue(t, m, Date, c, s("2023-01-01"))
	if *out == "" {
		t.Error("Date rule produced an empty string")
	}
}

func TestDateDefaultClassification(t *testing.T) {
	cl := Classifier{}
	r := catalog.Ref{Schema: "public", Name: "users"}
	if got := cl.Rule(r, "date_of_birth"); got != DateShift {
		t.Errorf("date_of_birth rule = %v, want %v", got, DateShift)
	}
	if got := cl.Rule(r, "dob"); got != DateShift {
		t.Errorf("dob rule = %v, want %v", got, DateShift)
	}
	if got := cl.Rule(r, "birthday"); got != DateShift {
		t.Errorf("birthday rule = %v, want %v", got, DateShift)
	}
}

func TestJSONDeepMasking(t *testing.T) {
	m := Masker{Seed: "team-seed"}
	c := col("metadata", "jsonb")

	inJSON := `{"customer":{"email":"real@customer.com","name":"Real Customer"},"tags":["vip","active"],"count":42}`
	out1 := mustValue(t, m, Keep, c, s(inJSON))
	if *out1 != inJSON {
		t.Errorf("Keep rule modified JSON: %v", *out1)
	}

	// Default masking rule on JSON column
	masked := mustValue(t, m, Rule(""), c, s(inJSON))
	if strings.Contains(*masked, "real@customer.com") {
		t.Errorf("nested email leaked in JSON payload: %s", *masked)
	}
	if strings.Contains(*masked, "Real Customer") {
		t.Errorf("nested name leaked in JSON payload: %s", *masked)
	}
	if !strings.Contains(*masked, "vip") || !strings.Contains(*masked, "42") {
		t.Errorf("non-sensitive structure corrupted: %s", *masked)
	}

	// Redact rule
	redacted := mustValue(t, m, Redact, c, s(inJSON))
	if redacted != nil {
		t.Errorf("Redact on nullable jsonb want nil, got %v", *redacted)
	}

	// Redact on NOT NULL
	cNotNull := col("metadata", "jsonb", notNull)
	redactedNotNull := mustValue(t, m, Redact, cNotNull, s(inJSON))
	if *redactedNotNull != "{}" {
		t.Errorf("Redact on NOT NULL jsonb want '{}', got %v", *redactedNotNull)
	}
}

func TestCompositeUniquesAndSet(t *testing.T) {
	tbl := &catalog.Table{
		Ref:     users,
		PK:      []string{"id"},
		Uniques: [][]string{{"email"}, {"tenant_id", "slug"}, {"org_id", "project_id", "key"}},
	}
	comp := CompositeUniques(tbl)
	if len(comp) != 2 {
		t.Fatalf("CompositeUniques len = %d, want 2", len(comp))
	}

	cus := NewCompositeUniqueSet()
	tupleGen := func(salt int) ([]string, error) {
		if salt == 0 {
			return []string{"org_1", "slug_a"}, nil
		}
		return []string{"org_1", fmt.Sprintf("slug_a_%d", salt)}, nil
	}

	first, err := cus.Ensure(tupleGen)
	if err != nil || first[1] != "slug_a" {
		t.Fatalf("first tuple error: %v, got %v", err, first)
	}

	second, err := cus.Ensure(tupleGen)
	if err != nil || second[1] != "slug_a_1" {
		t.Fatalf("second tuple error: %v, got %v", err, second)
	}
}
