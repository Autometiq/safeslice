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

package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Autometiq/safeslice/internal/catalog"
	"github.com/Autometiq/safeslice/internal/mask"
)

func write(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "safeslice.yaml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestMissingDefaultFileIsNotAnError(t *testing.T) {
	// The tool has to be useful before anyone has written any config.
	t.Chdir(t.TempDir())
	cfg, err := Load("")
	if err != nil {
		t.Fatalf("Load with no config file: %v", err)
	}
	if len(cfg.Source.Schemas) != 1 || cfg.Source.Schemas[0] != "public" {
		t.Errorf("default schemas = %v, want [public]", cfg.Source.Schemas)
	}
	if !cfg.StrictEnabled() {
		t.Error("strict must default to on; defaulting to off would make leaks the quiet path")
	}
}

func TestExplicitMissingFileIsAnError(t *testing.T) {
	// If the user names a file, a typo in the path must not silently fall back
	// to defaults with no masking rules.
	if _, err := Load(filepath.Join(t.TempDir(), "nope.yaml")); err == nil {
		t.Fatal("explicitly named missing config was accepted")
	}
}

func TestUnknownFieldIsRejected(t *testing.T) {
	// `pii_table` instead of `pii_tables` would otherwise be accepted and do
	// nothing, which for a masking setting means a silent leak.
	path := write(t, "version: 1\nmask:\n  pii_table: [users]\n")
	err := Load2(path)
	if err == nil {
		t.Fatal("typo in a config key was silently ignored")
	}
	if !strings.Contains(err.Error(), "pii_table") {
		t.Errorf("error %q should name the offending key", err)
	}
}

// Load2 is a helper so the test above reads cleanly.
func Load2(path string) error { _, err := Load(path); return err }

func TestUnknownMaskRuleIsRejected(t *testing.T) {
	path := write(t, "mask:\n  rules:\n    users.email: obfuscate\n")
	err := Load2(path)
	if err == nil {
		t.Fatal("unknown mask rule accepted; the column would have kept its real value")
	}
	if !strings.Contains(err.Error(), "obfuscate") {
		t.Errorf("error %q should name the bad rule", err)
	}
}

func TestVirtualKeyValidation(t *testing.T) {
	cases := map[string]string{
		"missing table":      "virtual_keys:\n  - columns: [x]\n    references: {table: posts, columns: [id]}\n",
		"missing reference":  "virtual_keys:\n  - table: comments\n    columns: [x]\n",
		"mismatched columns": "virtual_keys:\n  - table: comments\n    columns: [a, b]\n    references: {table: posts, columns: [id]}\n",
	}
	for name, body := range cases {
		if err := Load2(write(t, body)); err == nil {
			t.Errorf("%s: accepted an invalid virtual key", name)
		}
	}
}

func TestVirtualKeysBecomeGraphEdges(t *testing.T) {
	path := write(t, `
source:
  schemas: [app]
virtual_keys:
  - table: comments
    columns: [commentable_id]
    references:
      table: posts
      columns: [id]
    when: "commentable_type = 'Post'"
`)
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	fks := cfg.FKs()
	if len(fks) != 1 {
		t.Fatalf("got %d virtual keys, want 1", len(fks))
	}
	fk := fks[0]
	// Bare names must resolve against the configured schema, not "public":
	// writing `table: comments` in an app-schema project means app.comments.
	if fk.Table != (catalog.Ref{Schema: "app", Name: "comments"}) {
		t.Errorf("table = %v, want app.comments", fk.Table)
	}
	if fk.RefTable != (catalog.Ref{Schema: "app", Name: "posts"}) {
		t.Errorf("references = %v, want app.posts", fk.RefTable)
	}
	if !fk.Virtual || fk.When != "commentable_type = 'Post'" {
		t.Errorf("virtual flag or predicate lost: %+v", fk)
	}
	// Nothing enforces a virtual key at load time, so it must never drag the
	// loader onto the cycle-breaking path.
	if !fk.Deferrable {
		t.Error("virtual keys must not be treated as constraints needing deferral")
	}
}

func TestQualifiedNamesArePreserved(t *testing.T) {
	path := write(t, `
source:
  schemas: [app]
virtual_keys:
  - table: other.comments
    columns: [post_id]
    references: {table: app.posts, columns: [id]}
`)
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got := cfg.FKs()[0].Table; got.Schema != "other" {
		t.Errorf("explicit schema dropped: %v", got)
	}
}

func TestStrictScoping(t *testing.T) {
	users := catalog.Ref{Schema: "public", Name: "users"}
	logs := catalog.Ref{Schema: "public", Name: "logs"}

	all, err := Load(write(t, "mask:\n  strict: true\n"))
	if err != nil {
		t.Fatal(err)
	}
	if !all.StrictFor(users) || !all.StrictFor(logs) {
		t.Error("empty pii_tables must mean every table is checked")
	}

	scoped, err := Load(write(t, "mask:\n  strict: true\n  pii_tables: [users]\n"))
	if err != nil {
		t.Fatal(err)
	}
	if !scoped.StrictFor(users) {
		t.Error("listed table not covered by strict mode")
	}
	if scoped.StrictFor(logs) {
		t.Error("unlisted table wrongly covered by strict mode")
	}

	off, err := Load(write(t, "mask:\n  strict: false\n"))
	if err != nil {
		t.Fatal(err)
	}
	if off.StrictEnabled() || off.StrictFor(users) {
		t.Error("strict: false was not honoured")
	}
}

func TestClassifierCarriesOverrides(t *testing.T) {
	cfg, err := Load(write(t, "mask:\n  rules:\n    companies.name: keep\n    users.notes: secret\n"))
	if err != nil {
		t.Fatal(err)
	}
	cl := cfg.Classifier()
	companies := catalog.Ref{Schema: "public", Name: "companies"}
	if got := cl.Rule(companies, "name"); got != mask.Keep {
		t.Errorf("companies.name = %q, want keep", got)
	}
	users := catalog.Ref{Schema: "public", Name: "users"}
	if got := cl.Rule(users, "notes"); got != mask.Secret {
		t.Errorf("users.notes = %q, want secret", got)
	}
	// Defaults must still apply to columns the config says nothing about.
	if got := cl.Rule(users, "email"); got != mask.Email {
		t.Errorf("users.email = %q, want the built-in email rule", got)
	}
}

func TestNegativeLimitsRejected(t *testing.T) {
	if err := Load2(write(t, "slice:\n  limit: -1\n")); err == nil {
		t.Error("negative limit accepted")
	}
	if err := Load2(write(t, "slice:\n  child_depth: -2\n")); err == nil {
		t.Error("negative child_depth accepted")
	}
}

func TestUnsupportedVersionRejected(t *testing.T) {
	if err := Load2(write(t, "version: 99\n")); err == nil {
		t.Error("future config version accepted; it would be interpreted with the wrong semantics")
	}
}

func TestRootQualification(t *testing.T) {
	cfg, err := Load(write(t, "source:\n  schemas: [app]\nslice:\n  root: users\n"))
	if err != nil {
		t.Fatal(err)
	}
	if got := cfg.Root(); got != (catalog.Ref{Schema: "app", Name: "users"}) {
		t.Errorf("Root = %v, want app.users", got)
	}
}

func TestVerifyConfigParsing(t *testing.T) {
	body := `
version: 1
verify:
  checks:
    - name: employee_id
      pattern: 'EMP-[0-9]{6}'
  ignore:
    - public.users.id
`
	cfg, err := Load(write(t, body))
	if err != nil {
		t.Fatalf("Load verify config: %v", err)
	}
	if len(cfg.Verify.Checks) != 1 || cfg.Verify.Checks[0].Name != "employee_id" {
		t.Errorf("Verify.Checks = %v, want 1 custom check", cfg.Verify.Checks)
	}
	if len(cfg.Verify.Ignore) != 1 || cfg.Verify.Ignore[0] != "public.users.id" {
		t.Errorf("Verify.Ignore = %v, want 1 ignore entry", cfg.Verify.Ignore)
	}
}

func TestSamplePercentValidation(t *testing.T) {
	if err := Load2(write(t, "slice:\n  sample_percent: -5\n")); err == nil {
		t.Error("negative sample_percent accepted")
	}
	if err := Load2(write(t, "slice:\n  sample_percent: 150\n")); err == nil {
		t.Error("sample_percent > 100 accepted")
	}
	cfg, err := Load(write(t, "slice:\n  sample_percent: 25.5\n"))
	if err != nil {
		t.Fatalf("valid sample_percent rejected: %v", err)
	}
	if cfg.Slice.SamplePercent != 25.5 {
		t.Errorf("SamplePercent = %f, want 25.5", cfg.Slice.SamplePercent)
	}
}
