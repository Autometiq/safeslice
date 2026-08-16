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

var wizardOptions = []Option{
	{Label: "Create a safe development database"},
	{Label: "Inspect an existing configuration"},
	{Label: "Quit"},
}

func TestSelectReturnsTheTypedChoice(t *testing.T) {
	prompts(t, "2\n")
	if got := Select("what now?", wizardOptions, 0); got != 1 {
		t.Errorf("Select = %d, want 1", got)
	}
}

func TestSelectTakesTheDefaultOnEnter(t *testing.T) {
	// The whole point of the marked default: the common path is one keystroke.
	prompts(t, "\n")
	if got := Select("what now?", wizardOptions, 0); got != 0 {
		t.Errorf("Select = %d, want the default 0", got)
	}
}

func TestSelectRepromptsOnNonsense(t *testing.T) {
	prompts(t, "banana\n9\n0\n3\n")
	if got := Select("what now?", wizardOptions, -1); got != 2 {
		t.Errorf("Select = %d, want it to keep asking until a valid number", got)
	}
}

func TestSelectGivesUpOnClosedInput(t *testing.T) {
	// A wizard run under CI with no terminal must exit, not spin.
	prompts(t, "")
	if got := Select("what now?", wizardOptions, -1); got != -1 {
		t.Errorf("Select = %d, want -1 so the caller can exit", got)
	}
}

func TestSelectGivesUpWhenInputEndsEvenWithADefault(t *testing.T) {
	// A closed stream must not be read as "took the default". A menu that
	// re-prompts would otherwise never terminate under a pipe -- which is
	// exactly how the results screen hung CI for ten minutes.
	prompts(t, "")
	if got := Select("what now?", wizardOptions, 1); got != -1 {
		t.Errorf("Select = %d, want -1 so a re-prompting caller can stop", got)
	}
}

func TestSelectMarksTheDefault(t *testing.T) {
	prompts(t, "\n")
	buf := out.(*syncBuf)
	Select("what now?", wizardOptions, 1)
	got := buf.String()
	if !strings.Contains(got, "> 2  Inspect") {
		t.Errorf("the default is not marked:\n%s", got)
	}
	if !strings.Contains(got, "enter for 2") {
		t.Errorf("the prompt does not say what enter does:\n%s", got)
	}
}

func TestPasswordIsReadWithoutBeingEchoed(t *testing.T) {
	// Piped input has no terminal to switch echo off on, so the value is read
	// as an ordinary line -- but it must never be printed back.
	prompts(t, "hunter2\n")
	buf := out.(*syncBuf)
	if got := Password("Password:"); got != "hunter2" {
		t.Fatalf("Password = %q", got)
	}
	if strings.Contains(buf.String(), "hunter2") {
		t.Errorf("the password was echoed:\n%s", buf.String())
	}
}

func TestBoxDrawsEveryLine(t *testing.T) {
	prompts(t, "")
	buf := out.(*syncBuf)
	Box("SAFESLICE REVIEW", []string{Field("Source", "demo_src"), "", Check("Read-only")})
	got := buf.String()
	for _, want := range []string{"SAFESLICE REVIEW", "Source", "demo_src", "Read-only"} {
		if !strings.Contains(got, want) {
			t.Errorf("box is missing %q:\n%s", want, got)
		}
	}
}

func TestStepsAreOneLinePerEventInPlainMode(t *testing.T) {
	// A CI log wants one line per transition, not a redrawing block.
	prompts(t, "")
	buf := out.(*syncBuf)
	s := NewSteps("Creating", "Connecting", "Reading", "Loading")
	s.Percent(50) // no-op in plain mode, must not panic
	s.Advance()
	s.Advance()
	s.Done()

	got := buf.String()
	if strings.Contains(got, "\x1b[") {
		t.Errorf("plain mode emitted escape sequences:\n%q", got)
	}
	for _, want := range []string{"Connecting", "Reading", "Loading"} {
		if !strings.Contains(got, want) {
			t.Errorf("plain steps missing %q:\n%s", want, got)
		}
	}
}

func TestStepsSurviveBeingFinishedTwice(t *testing.T) {
	prompts(t, "")
	s := NewSteps("Creating", "Connecting")
	s.Done()
	s.Done() // deferring Done alongside an explicit one is the normal pattern
}

func TestBarIsClamped(t *testing.T) {
	for _, pct := range []int{-10, 0, 50, 100, 400} {
		if got := bar(pct); got == "" {
			t.Errorf("bar(%d) rendered nothing", pct)
		}
	}
}
