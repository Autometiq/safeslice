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
	"fmt"
	"os"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"
)

// A live status line: a spinner that redraws in place while long work runs.
//
// This exists because silence reads as a hang. Walking the foreign-key graph of
// a large database, or streaming one big table, can take minutes during which
// the old output printed nothing at all -- and a terminal that has shown no
// output for two minutes gets Ctrl-C'd, however healthy the process is.
//
// In plain mode there is no redraw at all: the label prints once as an ordinary
// line and updates are dropped. A CI log should be one line per event, not
// thousands of carriage returns.

const (
	clearLine  = "\r\x1b[2K"
	hideCursor = "\x1b[?25l"
	showCursor = "\x1b[?25h"
	frameRate  = 90 * time.Millisecond
)

// Braille frames read as motion at small sizes. Legacy Windows consoles cannot
// render them, so those fall back to ASCII rather than printing boxes.
var spinFrames = func() []string {
	if runtime.GOOS == "windows" && os.Getenv("WT_SESSION") == "" && os.Getenv("TERM_PROGRAM") == "" {
		return []string{"|", "/", "-", "\\"}
	}
	return []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}
}()

// painter is whatever currently owns the bottom of the screen: a status line or
// a step block. Finished output has to step around it, and only one of them may
// be drawing at a time.
type painter interface {
	clear() // erase what is on screen
	paint() // draw it again
}

var (
	liveMu sync.Mutex // guards writes to out and the fields of the active painter
	active painter
)

// Live is a handle to the active status line.
type Live struct {
	label   string
	detail  string
	started time.Time
	frame   int
	stop    chan struct{}
	done    chan struct{}
}

// Start begins a status line. Always pair it with Stop, Success or Fail --
// deferring Stop is the safe pattern, since it is idempotent.
func Start(format string, a ...any) *Live {
	l := &Live{label: fmt.Sprintf(format, a...), started: time.Now()}
	if plain {
		line(badge("PLAN", slate), l.label)
		return l
	}
	l.stop, l.done = make(chan struct{}), make(chan struct{})

	liveMu.Lock()
	if active != nil {
		active.clear()
	}
	active = l
	fmt.Fprint(out, hideCursor)
	l.render()
	liveMu.Unlock()

	go l.loop()
	return l
}

func (l *Live) loop() {
	t := time.NewTicker(frameRate)
	defer t.Stop()
	for {
		select {
		case <-l.stop:
			close(l.done)
			return
		case <-t.C:
			liveMu.Lock()
			if active == painter(l) {
				l.frame++
				l.render()
			}
			liveMu.Unlock()
		}
	}
}

// clear and paint implement painter. Callers must hold liveMu.
func (l *Live) clear() { fmt.Fprint(out, clearLine) }
func (l *Live) paint() { l.render() }

// render draws the status line. Callers must hold liveMu.
func (l *Live) render() {
	spin := tint(spinFrames[l.frame%len(spinFrames)], emerald)
	s := clearLine + spin + " " + l.label
	if l.detail != "" {
		s += tint("  "+l.detail, slate)
	}
	s += tint("  "+Duration(time.Since(l.started)), slate)
	fmt.Fprint(out, s)
}

// Detail replaces the dimmed text to the right of the label -- row counts, a
// rate, whatever tells the reader work is still happening.
func (l *Live) Detail(format string, a ...any) {
	if l == nil || plain {
		return
	}
	liveMu.Lock()
	l.detail = fmt.Sprintf(format, a...)
	if active == painter(l) {
		l.render()
	}
	liveMu.Unlock()
}

// Label changes the description of what is happening.
func (l *Live) Label(format string, a ...any) {
	if l == nil {
		return
	}
	if plain {
		return
	}
	liveMu.Lock()
	l.label = fmt.Sprintf(format, a...)
	if active == painter(l) {
		l.render()
	}
	liveMu.Unlock()
}

// Elapsed reports how long this status line has been running.
func (l *Live) Elapsed() time.Duration {
	if l == nil {
		return 0
	}
	return time.Since(l.started)
}

// Stop erases the status line. Safe to call more than once, and safe on nil.
func (l *Live) Stop() {
	if l == nil || plain || l.stop == nil {
		return
	}
	liveMu.Lock()
	if active != painter(l) {
		liveMu.Unlock()
		return
	}
	active = nil
	liveMu.Unlock()

	// Signalling outside the lock: loop takes liveMu on every tick, so holding
	// it here while waiting for the goroutine to exit would deadlock.
	close(l.stop)
	<-l.done

	liveMu.Lock()
	fmt.Fprint(out, clearLine+showCursor)
	liveMu.Unlock()
}

// Success stops the status line and replaces it with a success message.
func (l *Live) Success(format string, a ...any) {
	l.Stop()
	Success(format, a...)
}

// Fail stops the status line and replaces it with a warning.
func (l *Live) Fail(format string, a ...any) {
	l.Stop()
	Warn(format, a...)
}

// RestoreCursor unhides the cursor. main defers this and calls it from the
// signal handler: a Ctrl-C during a spinner would otherwise leave the user's
// terminal with no cursor until they run `reset`.
func RestoreCursor() {
	if plain {
		return
	}
	liveMu.Lock()
	defer liveMu.Unlock()
	if active != nil {
		active.clear()
		active = nil
	}
	fmt.Fprint(out, showCursor)
}

// emit writes a finished line, stepping around any active status line so the
// two never overwrite each other.
func emit(s string) {
	liveMu.Lock()
	defer liveMu.Unlock()
	if active != nil {
		active.clear()
	}
	fmt.Fprint(out, s)
	if active != nil {
		active.paint()
	}
}

func emitf(format string, a ...any) { emit(fmt.Sprintf(format, a...)) }

// Count formats a row count with thousands separators. 1240000 is a wall of
// digits; 1,240,000 is a number.
func Count(n int) string {
	s := strconv.Itoa(n)
	if n < 0 {
		return s
	}
	var b strings.Builder
	for i, r := range s {
		if i > 0 && (len(s)-i)%3 == 0 {
			b.WriteByte(',')
		}
		b.WriteRune(r)
	}
	return b.String()
}

// Rate formats a throughput figure for the status line.
func Rate(n int, d time.Duration) string {
	if d <= 0 || n <= 0 {
		return ""
	}
	perSec := float64(n) / d.Seconds()
	switch {
	case perSec >= 1_000_000:
		return fmt.Sprintf("%.1fM rows/s", perSec/1_000_000)
	case perSec >= 1_000:
		return fmt.Sprintf("%.0fk rows/s", perSec/1_000)
	default:
		return fmt.Sprintf("%.0f rows/s", perSec)
	}
}

// emit1 writes a value followed by a newline, spinner-aware.
func emit1(v any) { emit(fmt.Sprint(v) + "\n") }
