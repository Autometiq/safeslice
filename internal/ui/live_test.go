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
	"bytes"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
)

// syncBuf is written by the spinner goroutine and read by the test.
type syncBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuf) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuf) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

func live(t *testing.T, isPlain bool) *syncBuf {
	t.Helper()
	buf := &syncBuf{}
	oldOut, oldPlain := out, plain
	out, plain = buf, isPlain
	t.Cleanup(func() {
		liveMu.Lock()
		liveOne = nil
		liveMu.Unlock()
		out, plain = oldOut, oldPlain
	})
	return buf
}

func TestLiveIsSilentInPlainMode(t *testing.T) {
	// A CI log must be one line per event. Redraw escapes would turn a long
	// extraction into thousands of unreadable carriage returns.
	buf := live(t, true)
	l := Start("walking the graph")
	l.Detail("1,000 rows")
	l.Detail("2,000 rows")
	time.Sleep(150 * time.Millisecond)
	l.Success("done")

	got := buf.String()
	if strings.Contains(got, "\r") || strings.Contains(got, "\x1b[") {
		t.Errorf("plain output contains redraw escapes:\n%q", got)
	}
	if !strings.Contains(got, "walking the graph") || !strings.Contains(got, "done") {
		t.Errorf("plain output lost the label or result:\n%s", got)
	}
	if n := strings.Count(got, "walking the graph"); n != 1 {
		t.Errorf("label printed %d times, want exactly once", n)
	}
}

func TestLiveRedrawsInPlaceWhenStyled(t *testing.T) {
	buf := live(t, false)
	l := Start("working")
	time.Sleep(250 * time.Millisecond) // several frames
	l.Stop()

	got := buf.String()
	if !strings.Contains(got, clearLine) {
		t.Error("styled output never clears the line, so frames would stack up")
	}
	if !strings.Contains(got, hideCursor) || !strings.Contains(got, showCursor) {
		t.Error("cursor must be hidden while spinning and restored on stop")
	}
	if strings.Count(got, "working") < 2 {
		t.Error("status line was not redrawn; the spinner would appear frozen")
	}
}

func TestNormalOutputDoesNotCollideWithTheStatusLine(t *testing.T) {
	// The whole point of routing writes through emit: a log line arriving while
	// the spinner is running must erase it first, then redraw it after.
	buf := live(t, false)
	l := Start("working")
	Info("a table finished")
	l.Stop()

	got := buf.String()
	i := strings.Index(got, "a table finished")
	if i < 0 {
		t.Fatal("the message never appeared")
	}
	if !strings.Contains(got[:i], clearLine) {
		t.Error("status line was not erased before writing, so output would overlap")
	}
	if !strings.Contains(got[i:], "working") {
		t.Error("status line was not redrawn after the message")
	}
}

func TestStopIsIdempotentAndNilSafe(t *testing.T) {
	live(t, false)
	var nilLive *Live
	nilLive.Stop() // deferred Stop on an unstarted spinner must not panic
	nilLive.Detail("x")
	nilLive.Label("x")
	if nilLive.Elapsed() != 0 {
		t.Error("nil spinner reported elapsed time")
	}

	l := Start("working")
	l.Stop()
	l.Stop() // defer plus explicit stop is the normal pattern
}

func TestRestoreCursorClearsAnActiveLine(t *testing.T) {
	// Ctrl-C mid-spinner must not leave the terminal without a cursor.
	buf := live(t, false)
	Start("working")
	RestoreCursor()
	if !strings.Contains(buf.String(), showCursor) {
		t.Error("cursor not restored")
	}
	liveMu.Lock()
	active := liveOne
	liveMu.Unlock()
	if active != nil {
		t.Error("status line still considered active after restore")
	}
}

func TestConcurrentDetailAndOutputAreRaceFree(t *testing.T) {
	// Discovered and Extracting fire from the extractor while the ticker
	// redraws; run with -race, this is what catches an unguarded field.
	live(t, false)
	l := Start("working")
	var wg sync.WaitGroup
	for i := range 4 {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			for j := range 25 {
				l.Detail("%d rows", n*j)
				if j%10 == 0 {
					Info("checkpoint %d", n)
				}
			}
		}(i)
	}
	wg.Wait()
	l.Stop()
}

func TestCountGroupsThousands(t *testing.T) {
	cases := map[int]string{0: "0", 42: "42", 999: "999", 1000: "1,000",
		80300: "80,300", 1240000: "1,240,000", -5: "-5"}
	for in, want := range cases {
		if got := Count(in); got != want {
			t.Errorf("Count(%d) = %q, want %q", in, got, want)
		}
	}
}

func TestRateScalesWithMagnitude(t *testing.T) {
	cases := []struct {
		n    int
		d    time.Duration
		want string
	}{
		{500, time.Second, "500 rows/s"},
		{50_000, time.Second, "50k rows/s"},
		{5_000_000, time.Second, "5.0M rows/s"},
		{0, time.Second, ""}, // nothing to report yet
		{100, 0, ""},         // no elapsed time; a division by zero
	}
	for _, c := range cases {
		if got := Rate(c.n, c.d); got != c.want {
			t.Errorf("Rate(%d, %v) = %q, want %q", c.n, c.d, got, c.want)
		}
	}
}

func TestFatalShowsTheFixAndTheCommand(t *testing.T) {
	buf := live(t, true)
	Fatal(HintCmd(errors.New("no root table"),
		"Generate a config that picks one for you.", "safeslice init"))

	got := buf.String()
	for _, want := range []string{"no root table", "Generate a config", "safeslice init"} {
		if !strings.Contains(got, want) {
			t.Errorf("error output missing %q:\n%s", want, got)
		}
	}
}

func TestFatalHandlesPlainAndNilErrors(t *testing.T) {
	buf := live(t, true)
	Fatal(nil)
	if buf.String() != "" {
		t.Errorf("Fatal(nil) printed %q", buf.String())
	}
	Fatal(errors.New("plain failure"))
	if !strings.Contains(buf.String(), "plain failure") {
		t.Error("an unhinted error must still be reported")
	}
}

func TestHintUnwrapsToTheOriginal(t *testing.T) {
	// Callers use errors.Is/As on these; wrapping must not hide the cause.
	base := errors.New("connection refused")
	wrapped := Hint(base, "check the host")
	if !errors.Is(wrapped, base) {
		t.Error("Hint broke the error chain")
	}
	if wrapped.Error() != base.Error() {
		t.Errorf("Error() = %q, want the original message", wrapped.Error())
	}
	if Hint(nil, "x") != nil {
		t.Error("Hint(nil) must stay nil so `return Hint(err, …)` is safe")
	}
}

func TestNextStepSuggestsACommand(t *testing.T) {
	buf := live(t, true)
	NextStep("safeslice verify --target %q", "postgres://localhost/dev")
	if !strings.Contains(buf.String(), "safeslice verify") {
		t.Errorf("next step missing:\n%s", buf.String())
	}
}
