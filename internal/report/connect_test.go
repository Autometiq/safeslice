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
	"strings"
	"testing"
)

func TestSnippetsCoverTheFrameworksPeopleUse(t *testing.T) {
	got := Snippets(Endpoint{Host: "localhost", Port: "5432", Database: "myapp_dev"})
	want := []string{"Environment", "psql", "Prisma", "Rails", "Django", "Node.js", "Go"}
	if len(got) != len(want) {
		t.Fatalf("got %d snippets, want %d", len(got), len(want))
	}
	for i, name := range want {
		if got[i].Name != name {
			t.Errorf("snippet %d = %q, want %q", i, got[i].Name, name)
		}
		if strings.TrimSpace(got[i].Code) == "" {
			t.Errorf("snippet %q is empty", name)
		}
	}
}

func TestSnippetsCarryNoCredentials(t *testing.T) {
	// The target is a local database the reader owns. A generated file that
	// hands out credentials is a file nobody can commit.
	e := Redact("postgres://app:hunter2@localhost:5432/myapp_dev")
	for _, s := range Snippets(e) {
		if strings.Contains(s.Code, "hunter2") || strings.Contains(s.Code, "app:") {
			t.Errorf("snippet %q leaked credentials: %s", s.Name, s.Code)
		}
	}
}

func TestSnippetsSurviveAnUnparseableTarget(t *testing.T) {
	// A key=value DSN redacts to nothing but the database name; the snippets
	// still have to render something a reader can edit.
	for _, s := range Snippets(Endpoint{Database: "slice.sql"}) {
		if strings.TrimSpace(s.Code) == "" {
			t.Errorf("snippet %q is empty for a file target", s.Name)
		}
	}
}

func TestNonDefaultPortReachesTheSnippets(t *testing.T) {
	got := Snippets(Endpoint{Host: "localhost", Port: "55433", Database: "shop_dev"})
	for _, s := range got {
		if s.Name == "psql" && !strings.Contains(s.Code, "55433") {
			t.Errorf("psql snippet lost the port: %s", s.Code)
		}
	}
}
