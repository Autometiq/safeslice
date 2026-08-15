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
	"strconv"
	"strings"

	"github.com/charmbracelet/x/term"
)

// Arrow-key selection.
//
// The typed prompt in prompt.go is still the fallback, and it is the one that
// runs under a pipe, in CI and in the tests -- raw mode there would either fail
// outright or swallow a scripted answer. But on a real terminal a list you can
// only drive by typing a number reads as broken: people press Down, nothing
// moves, and they conclude the tool is unfinished.
//
// One implementation covers every platform because MakeRaw turns on
// ENABLE_VIRTUAL_TERMINAL_INPUT on Windows, so arrow keys arrive as the same
// ANSI sequences there as everywhere else.

const (
	keyEsc   = 0x1b
	keyEnter = '\r'
	keyLF    = '\n'
	keyCtrlC = 0x03
	keyCtrlD = 0x04
)

// selectKeys drives the list with the keyboard. The second return reports
// whether it ran at all: false means the terminal cannot support it and the
// caller must fall back to the typed prompt.
func selectKeys(title string, opts []Option, def int) (int, bool) {
	f, isFile := in.(*os.File)
	// Buffered input means something is already piping answers in; taking the
	// descriptor raw at that point would strip the rest of the script.
	if !isFile || plain || !term.IsTerminal(f.Fd()) || stdin().Buffered() > 0 {
		return 0, false
	}
	state, err := term.MakeRaw(f.Fd())
	if err != nil {
		return 0, false // an unusual console; the typed prompt still works
	}
	defer term.Restore(f.Fd(), state) //nolint:errcheck

	cursor := def
	if cursor < 0 {
		cursor = 0
	}

	if title != "" {
		emit(bold(title) + "\r\n\r\n")
	}
	emit(hideCursor)
	defer emit(showCursor)

	// The hint sits on the line directly after the last option, with no blank
	// row between: redraw counts on the block being exactly block.lines tall
	// plus this one, and an extra newline here makes the whole thing crawl up
	// the screen one row per keypress.
	block := renderChoices(opts, cursor)
	emit(block.text)
	emit(hintLine)

	buf := make([]byte, 8)
	for {
		n, err := f.Read(buf)
		if err != nil || n == 0 {
			return cursor, true // the terminal went away; take what is selected
		}

		switch {
		case buf[0] == keyCtrlC, buf[0] == keyCtrlD:
			// Raw mode means no SIGINT was raised, so this has to be honoured
			// here or Ctrl-C would do nothing at all.
			term.Restore(f.Fd(), state) //nolint:errcheck
			emit(showCursor + "\r\n")
			os.Exit(130)

		case buf[0] == keyEnter, buf[0] == keyLF:
			finish(block.lines, renderChoices(opts, cursor).text)
			return cursor, true

		case buf[0] == 'q', buf[0] == 'Q':
			finish(block.lines, renderChoices(opts, cursor).text)
			return -1, true

		case buf[0] == keyEsc && n >= 3 && buf[1] == '[':
			switch buf[2] {
			case 'A': // up
				cursor = wrap(cursor-1, len(opts))
			case 'B': // down
				cursor = wrap(cursor+1, len(opts))
			case 'H': // home
				cursor = 0
			case 'F': // end
				cursor = len(opts) - 1
			default:
				continue
			}

		case buf[0] == 'k': // vim, for the people who will try it
			cursor = wrap(cursor-1, len(opts))
		case buf[0] == 'j':
			cursor = wrap(cursor+1, len(opts))

		case buf[0] >= '1' && buf[0] <= '9':
			// Typing the number moves the pointer rather than selecting
			// outright, so the screen confirms the choice before enter commits
			// it -- and `2` then enter still lands on 2.
			if i := int(buf[0] - '1'); i < len(opts) {
				cursor = i
			}

		default:
			continue
		}

		next := renderChoices(opts, cursor)
		redraw(block.lines, next.text)
		block = next
	}
}

func wrap(i, n int) int {
	if n == 0 {
		return 0
	}
	return ((i % n) + n) % n
}

type choiceBlock struct {
	text  string
	lines int
}

// renderChoices draws the list with the pointer at cursor. Lines end in \r\n
// because the terminal is in raw mode, where a bare newline drops down a row
// without returning to column zero and the list comes out as a staircase.
func renderChoices(opts []Option, cursor int) choiceBlock {
	width := 0
	for _, o := range opts {
		if len(o.Label) > width {
			width = len(o.Label)
		}
	}

	var b strings.Builder
	lines := 0
	for i, o := range opts {
		pointer, label := "  ", o.Label+strings.Repeat(" ", width-len(o.Label))
		number := tint(strconv.Itoa(i+1), slate)
		if i == cursor {
			pointer = tint("❯ ", emerald)
			label = bold(label)
			number = bold(tint(strconv.Itoa(i+1), emerald))
		}
		row := pointer + number + "  " + label
		if o.Note != "" {
			row += "   " + tint(o.Note, slate)
		}
		b.WriteString("\x1b[2K" + strings.TrimRight(row, " ") + "\r\n")
		lines++

		// Body lines belong to the selected option only: showing every
		// explanation at once turns a six-item menu into a page of prose.
		if i == cursor {
			for _, l := range o.Body {
				b.WriteString("\x1b[2K       " + tint(l, slate) + "\r\n")
				lines++
			}
		}
	}
	return choiceBlock{text: b.String(), lines: lines}
}

// hintLine is drawn immediately below the options and rewritten on every
// redraw, so the number of rows the block occupies never changes.
var hintLine = tint("↑↓ to move · enter to choose · q to quit", slate)

// redraw replaces the previous block in place.
//
// The cursor sits on the hint line, which is row prevLines+1 counting the
// first option as row 1 -- so moving up prevLines rows lands on the first
// option, which is where the next block has to start.
func redraw(prevLines int, next string) {
	emit(fmt.Sprintf("\r\x1b[%dA", prevLines))
	emit(next)
	emit("\x1b[2K" + hintLine)
}

// finish leaves the chosen list on screen and clears the key hint, which has
// done its job the moment a choice is made.
func finish(prevLines int, next string) {
	emit(fmt.Sprintf("\r\x1b[%dA", prevLines))
	emit(next)
	emit("\x1b[2K\r\n")
}
