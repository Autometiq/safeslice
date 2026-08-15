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

package profile

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const secret = "sup3rsecret"

func TestSanitiseRemovesThePassword(t *testing.T) {
	cases := map[string]string{
		"postgres://user:" + secret + "@db.internal:5432/shop":          "postgres://user@db.internal:5432/shop",
		"postgres://user@db.internal:5432/shop":                         "postgres://user@db.internal:5432/shop",
		"postgres://db.internal/shop":                                   "postgres://db.internal/shop",
		"host=db.internal user=app password=" + secret + " dbname=shop": "host=db.internal user=app  dbname=shop",
		"host=db.internal password='" + secret + "' dbname=shop":        "host=db.internal  dbname=shop",
	}
	for in, want := range cases {
		got := Sanitise(in)
		if strings.Contains(got, secret) {
			t.Fatalf("Sanitise(%q) leaked the password: %q", in, got)
		}
		if got != want {
			t.Errorf("Sanitise(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestSanitiseDropsAPasswordInTheQueryString(t *testing.T) {
	got := Sanitise("postgres://user@host/db?sslmode=require&password=" + secret)
	if strings.Contains(got, secret) {
		t.Errorf("Sanitise kept a query-string password: %q", got)
	}
	if !strings.Contains(got, "sslmode=require") {
		t.Errorf("Sanitise dropped an unrelated parameter: %q", got)
	}
}

func TestWithPasswordRoundTrips(t *testing.T) {
	dsn := WithPassword("postgres://user@host:5432/db", "p@ss word/1")
	if !strings.Contains(dsn, "user:") {
		t.Fatalf("WithPassword produced %q", dsn)
	}
	if Sanitise(dsn) != "postgres://user@host:5432/db" {
		t.Errorf("round trip = %q", Sanitise(dsn))
	}
	if strings.Contains(dsn, "p@ss word/1") {
		t.Errorf("the password was not escaped: %q", dsn)
	}
}

func TestResolveTakesThePasswordFromTheEnvironment(t *testing.T) {
	t.Setenv("SAFESLICE_TEST_PW", secret)
	c := Connection{DSN: "postgres://user@host/db", PasswordEnv: "SAFESLICE_TEST_PW"}
	if got := c.Resolve(); !strings.Contains(got, secret) {
		t.Errorf("Resolve = %q, want the password from the environment", got)
	}
	// The stored form still must not carry it.
	if strings.Contains(c.DSN, secret) {
		t.Error("Resolve mutated the stored connection")
	}
}

func TestSavedFilesNeverContainAPassword(t *testing.T) {
	// The whole reason this package exists. .safeslice gets committed by
	// accident; that must be embarrassing, not an incident.
	dir := t.TempDir()
	s := Open(filepath.Join(dir, ".safeslice"))

	dsn := "postgres://app:" + secret + "@prod.internal:5432/shop"
	if err := s.Save(Profile{Name: "Local development", Source: Connection{Name: "prod", DSN: dsn},
		Target: Connection{Name: "dev", DSN: "postgres://localhost/shop_dev"}, Root: "users", Limit: 1000}); err != nil {
		t.Fatal(err)
	}
	if err := s.AddConnection(Connection{Name: "prod", DSN: dsn}); err != nil {
		t.Fatal(err)
	}
	if err := s.Record(Run{Source: dsn, Target: "postgres://localhost/shop_dev", Rows: 42}); err != nil {
		t.Fatal(err)
	}

	found := 0
	err := filepath.Walk(s.Path(), func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return err
		}
		found++
		body, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if strings.Contains(string(body), secret) {
			t.Errorf("%s contains the password", path)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if found < 3 {
		t.Fatalf("wrote %d files, want config.yaml, a profile and a history entry", found)
	}
}

func TestProfilesRoundTrip(t *testing.T) {
	s := Open(filepath.Join(t.TempDir(), ".safeslice"))
	want := Profile{Name: "CI test database", Root: "orders", Limit: 500, ChildDepth: 2,
		Source: Connection{Name: "ci", DSN: "postgres://ci@host/app"}}
	if err := s.Save(want); err != nil {
		t.Fatal(err)
	}

	got, err := s.Load("CI test database")
	if err != nil {
		t.Fatal(err)
	}
	if got.Root != want.Root || got.Limit != want.Limit || got.ChildDepth != want.ChildDepth {
		t.Errorf("Load = %+v, want %+v", got, want)
	}
	if got.Updated.IsZero() {
		t.Error("Save did not stamp the profile")
	}
	list, err := s.Profiles()
	if err != nil || len(list) != 1 {
		t.Fatalf("Profiles = %d, %v; want 1", len(list), err)
	}
	if s.LastProfile() != want.Name {
		t.Errorf("LastProfile = %q", s.LastProfile())
	}
}

func TestMissingStoreIsNotAnError(t *testing.T) {
	// Every command reads this before anything has ever written it.
	s := Open(filepath.Join(t.TempDir(), "nothing-here"))
	if c, err := s.Connections(); err != nil || len(c) != 0 {
		t.Errorf("Connections = %v, %v", c, err)
	}
	if p, err := s.Profiles(); err != nil || len(p) != 0 {
		t.Errorf("Profiles = %v, %v", p, err)
	}
	if h, err := s.History(); err != nil || len(h) != 0 {
		t.Errorf("History = %v, %v", h, err)
	}
}

func TestAddConnectionReplacesByName(t *testing.T) {
	s := Open(filepath.Join(t.TempDir(), ".safeslice"))
	for _, dsn := range []string{"postgres://a@host/db", "postgres://b@host/db"} {
		if err := s.AddConnection(Connection{Name: "prod", DSN: dsn}); err != nil {
			t.Fatal(err)
		}
	}
	got, err := s.Connections()
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].DSN != "postgres://b@host/db" {
		t.Errorf("Connections = %+v, want one entry holding the newer DSN", got)
	}
}

func TestSlugMakesAFilename(t *testing.T) {
	cases := map[string]string{
		"Local development": "local-development",
		"Staging snapshot!": "staging-snapshot",
		"../../etc/passwd":  "etcpasswd",
		"":                  "profile",
	}
	for in, want := range cases {
		if got := Slug(in); got != want {
			t.Errorf("Slug(%q) = %q, want %q", in, got, want)
		}
	}
}
