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

package ui

import (
	"strings"
	"testing"
)

func prompts(t *testing.T, input string) {
	t.Helper()
	oldIn, oldOut, oldPlain := in, out, plain
	SetInput(strings.NewReader(input))
	out, plain = &syncBuf{}, true
	t.Cleanup(func() {
		in, out, plain = oldIn, oldOut, oldPlain
		reader = nil
	})
}

func TestPromptsShareOneReader(t *testing.T) {
	// The bug this pins: every prompt used to build its own bufio.Reader. The
	// first one read ahead, swallowed the remaining lines into a buffer that was
	// then thrown away, and every later prompt saw EOF -- so a scripted run
	// answered question one and abandoned the rest.
	prompts(t, "2\npostgres://user@host/db\ny\n500\n")

	if got := Menu("pick", []Choice{{Key: "1"}, {Key: "2"}}); got != "2" {
		t.Fatalf("Menu = %q, want 2", got)
	}
	if got := Ask("source?", ""); got != "postgres://user@host/db" {
		t.Fatalf("Ask = %q; the menu consumed the line", got)
	}
	if !Confirm("sure?") {
		t.Fatal("Confirm did not see its answer")
	}
	if got := AskInt("how many?", 1000); got != 500 {
		t.Fatalf("AskInt = %d, want 500", got)
	}
}

func TestAskFallsBackToTheDefault(t *testing.T) {
	prompts(t, "\n")
	if got := Ask("source?", "postgres://localhost/dev"); got != "postgres://localhost/dev" {
		t.Errorf("Ask on an empty line = %q, want the default", got)
	}
}

func TestConfirmTreatsSilenceAsNo(t *testing.T) {
	// This gates writes to a database, so anything short of an explicit yes
	// must be a no.
	for _, answer := range []string{"\n", "no\n", "maybe\n", "Y E S\n", ""} {
		prompts(t, answer)
		if Confirm("load into production?") {
			t.Errorf("Confirm(%q) returned true", answer)
		}
	}
	for _, answer := range []string{"y\n", "Y\n", "yes\n", "YES\n"} {
		prompts(t, answer)
		if !Confirm("ok?") {
			t.Errorf("Confirm(%q) returned false", answer)
		}
	}
}

func TestMenuRepromptsOnBadInput(t *testing.T) {
	prompts(t, "banana\n9\n1\n")
	if got := Menu("pick", []Choice{{Key: "1"}, {Key: "2"}}); got != "1" {
		t.Errorf("Menu = %q, want it to keep asking until a valid key", got)
	}
}

func TestMenuGivesUpOnClosedInput(t *testing.T) {
	// Without this the menu spins forever when stdin is closed, which is what
	// happens under a CI runner with no terminal attached.
	prompts(t, "")
	if got := Menu("pick", []Choice{{Key: "1"}}); got != "" {
		t.Errorf("Menu = %q, want empty so the caller can exit", got)
	}
}

func TestAskIntRejectsNonsense(t *testing.T) {
	for _, answer := range []string{"abc\n", "-5\n", "\n"} {
		prompts(t, answer)
		if got := AskInt("rows?", 1000); got != 1000 {
			t.Errorf("AskInt(%q) = %d, want the default", answer, got)
		}
	}
}
