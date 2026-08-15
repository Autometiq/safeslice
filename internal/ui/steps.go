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
	"strings"
	"time"
)

// The wizard's progress display: the whole checklist stays on screen and
// redraws in place, so the reader can see what is done, what is running and
// what is still to come without a scrolling wall of log lines.
//
// The detailed log is not lost -- it is what the CLI commands print, and what
// --verbose restores here. This view is for the person watching it happen.

const (
	cursorUp = "\x1b[%dA"
	barWidth = 24
)

type stepState int

const (
	stepPending stepState = iota
	stepActive
	stepDone
	stepFailed
	stepSkipped
)

// Steps is a redrawing checklist. Create it with NewSteps, advance it as work
// completes, and always finish with Done -- deferring it is the safe pattern.
type Steps struct {
	title   string
	labels  []string
	state   []stepState
	detail  string
	pct     int // -1 when the active step has no measurable progress
	started time.Time
	drawn   int
	stop    chan struct{}
	fin     chan struct{}
}

// NewSteps prints the checklist and starts the first step.
func NewSteps(title string, labels ...string) *Steps {
	s := &Steps{
		title:   title,
		labels:  labels,
		state:   make([]stepState, len(labels)),
		pct:     -1,
		started: time.Now(),
	}
	if len(labels) > 0 {
		s.state[0] = stepActive
	}
	if plain {
		if title != "" {
			Section(title)
		}
		if len(labels) > 0 {
			Info("%s", labels[0])
		}
		return s
	}
	s.stop, s.fin = make(chan struct{}), make(chan struct{})

	liveMu.Lock()
	if active != nil {
		active.clear()
	}
	active = s
	fmt.Fprint(out, hideCursor)
	s.paint()
	liveMu.Unlock()

	go s.loop()
	return s
}

// loop redraws on a timer so the elapsed clock and spinner keep moving even
// when a single step runs for minutes without reporting anything.
func (s *Steps) loop() {
	t := time.NewTicker(frameRate)
	defer t.Stop()
	for {
		select {
		case <-s.stop:
			close(s.fin)
			return
		case <-t.C:
			liveMu.Lock()
			if active == painter(s) {
				s.clear()
				s.paint()
			}
			liveMu.Unlock()
		}
	}
}

// Advance completes the running step and starts the next one.
func (s *Steps) Advance() {
	s.set(stepDone)
}

// Skip marks the running step as not applicable and moves on.
func (s *Steps) Skip() {
	s.set(stepSkipped)
}

// Fail marks the running step as failed and stops the display, leaving the
// checklist on screen so the reader can see how far the run got.
func (s *Steps) Fail() {
	if s == nil {
		return
	}
	s.finish(stepFailed)
}

// Done completes any running step and leaves the finished checklist on screen.
// Safe to call more than once, and safe on nil.
func (s *Steps) Done() {
	if s == nil {
		return
	}
	s.finish(stepDone)
}

func (s *Steps) finish(last stepState) {
	if s == nil || s.stop == nil {
		if s != nil && plain {
			s.markPlain(last)
		}
		return
	}
	liveMu.Lock()
	if active != painter(s) {
		liveMu.Unlock()
		return
	}
	for i := range s.state {
		if s.state[i] == stepActive {
			s.state[i] = last
		}
	}
	s.detail, s.pct = "", -1
	active = nil
	liveMu.Unlock()

	// Signalled outside the lock: loop takes it on every tick.
	close(s.stop)
	<-s.fin

	liveMu.Lock()
	s.clear()
	s.paint()
	fmt.Fprint(out, showCursor)
	s.drawn = 0 // the block is now permanent output, not something to redraw over
	liveMu.Unlock()
}

func (s *Steps) set(done stepState) {
	if s == nil {
		return
	}
	if plain {
		s.markPlain(done)
		return
	}
	liveMu.Lock()
	defer liveMu.Unlock()
	for i := range s.state {
		if s.state[i] == stepActive {
			s.state[i] = done
			if i+1 < len(s.state) {
				s.state[i+1] = stepActive
			}
			break
		}
	}
	s.detail, s.pct = "", -1
	if active == painter(s) {
		s.clear()
		s.paint()
	}
}

// markPlain writes one line per transition, which is what a CI log wants.
func (s *Steps) markPlain(done stepState) {
	for i := range s.state {
		if s.state[i] != stepActive {
			continue
		}
		s.state[i] = done
		switch done {
		case stepFailed:
			// Nothing follows a failure: announcing the next step would imply
			// the run carried on.
			Error(fmt.Errorf("%s", s.labels[i]))
			return
		case stepSkipped:
			Info("skipped: %s", s.labels[i])
		default:
			Success("%s", s.labels[i])
		}
		if i+1 < len(s.state) {
			s.state[i+1] = stepActive
			Info("%s", s.labels[i+1])
		}
		return
	}
}

// Detail sets the dimmed text beside the running step: a row count, a rate,
// whatever shows the work is moving.
func (s *Steps) Detail(format string, a ...any) {
	if s == nil || plain {
		return
	}
	liveMu.Lock()
	s.detail = fmt.Sprintf(format, a...)
	if active == painter(s) {
		s.clear()
		s.paint()
	}
	liveMu.Unlock()
}

// Percent draws a progress bar under the running step. Values outside 0-100
// are clamped; anything below zero hides the bar again.
func (s *Steps) Percent(p int) {
	if s == nil || plain {
		return
	}
	liveMu.Lock()
	switch {
	case p > 100:
		s.pct = 100
	default:
		s.pct = p
	}
	if active == painter(s) {
		s.clear()
		s.paint()
	}
	liveMu.Unlock()
}

// Elapsed reports how long the checklist has been running.
func (s *Steps) Elapsed() time.Duration {
	if s == nil {
		return 0
	}
	return time.Since(s.started)
}

// clear erases the block. Callers must hold liveMu.
func (s *Steps) clear() {
	if s.drawn == 0 {
		return
	}
	// paint leaves the cursor on the empty line below the block, so walk back up
	// to the first line, wipe on the way down, then return to the top.
	fmt.Fprintf(out, cursorUp, s.drawn)
	for range s.drawn {
		fmt.Fprint(out, clearLine+"\n")
	}
	fmt.Fprintf(out, cursorUp, s.drawn)
	s.drawn = 0
}

// paint draws the block. Callers must hold liveMu.
func (s *Steps) paint() {
	var b strings.Builder
	lines := 0
	if s.title != "" {
		b.WriteString(bold(tint(s.title, emerald)))
		b.WriteString("\n\n")
		lines += 2
	}
	for i, label := range s.labels {
		switch s.state[i] {
		case stepDone:
			b.WriteString(Check("%s", label))
		case stepFailed:
			b.WriteString(Cross("%s", label))
		case stepSkipped:
			b.WriteString(Alert("%s  (skipped)", label))
		case stepActive:
			spin := tint(spinFrames[int(time.Since(s.started)/frameRate)%len(spinFrames)], emerald)
			row := spin + " " + bold(label)
			if s.detail != "" {
				row += tint("  "+s.detail, slate)
			}
			b.WriteString(row)
			if s.pct >= 0 {
				b.WriteString("\n  " + bar(s.pct))
				lines++
			}
		default:
			b.WriteString(Pending("%s", label))
		}
		b.WriteString("\n")
		lines++
	}
	fmt.Fprint(out, b.String())
	s.drawn = lines
}

// bar renders the filled progress indicator.
func bar(pct int) string {
	if pct < 0 {
		pct = 0
	}
	if pct > 100 {
		pct = 100
	}
	filled := pct * barWidth / 100
	full, empty := "█", "░"
	if !unicodeOK() {
		full, empty = "#", "."
	}
	return tint(strings.Repeat(full, filled), emerald) +
		tint(strings.Repeat(empty, barWidth-filled), slate) +
		fmt.Sprintf(" %3d%%", pct)
}
