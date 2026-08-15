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
	"os"
	"strconv"
	"strings"

	"github.com/charmbracelet/x/term"
)

// Select is the wizard's primary control: a list with a marked default that
// Enter accepts.
//
// Selection is typed, not arrow-driven, for the reason given in prompt.go --
// raw mode behaves differently across Windows conhost, tmux and CI, and a typed
// answer survives being piped. The default carries the pointer, so the common
// path is a single Enter and nobody has to read the numbers.

// Option is one entry in a Select.
type Option struct {
	Label string
	Note  string   // dimmed, on the same line: what this choice means
	Body  []string // longer explanation, indented beneath the label
}

// Select renders the options and returns the index chosen.
//
// def is pre-selected: it carries the pointer and is what Enter returns. Pass
// -1 for no default, in which case Enter reprompts. Returns -1 when the input
// stream ends, which must never become an infinite loop.
func Select(title string, opts []Option, def int) int {
	if len(opts) == 0 {
		return -1
	}
	if def >= len(opts) {
		def = -1
	}
	if title != "" {
		emitf("%s\n\n", bold(title))
	}

	width := 0
	for _, o := range opts {
		if len(o.Label) > width {
			width = len(o.Label)
		}
	}
	for i, o := range opts {
		pointer := "  "
		if i == def {
			pointer = tint("❯ ", emerald)
			if plain {
				pointer = "> "
			}
		}
		label := o.Label + strings.Repeat(" ", width-len(o.Label))
		row := pointer + bold(tint(strconv.Itoa(i+1), emerald)) + "  " + label
		if o.Note != "" {
			row += "   " + tint(o.Note, slate)
		}
		emitf("%s\n", strings.TrimRight(row, " "))
		for _, b := range o.Body {
			emitf("       %s\n", tint(b, slate))
		}
	}
	emitf("\n")

	r := stdin()
	for {
		hint := ""
		if def >= 0 {
			hint = tint(" [enter for "+strconv.Itoa(def+1)+"]", slate)
		}
		emitf("%s%s ", tint("›", emerald), hint)
		text, err := r.ReadString('\n')
		got := strings.TrimSpace(text)
		if got == "" {
			if err != nil && def < 0 {
				emitf("\n")
				return -1 // stream ended with no answer and nothing to fall back on
			}
			if def >= 0 {
				emitf("\n")
				return def
			}
			if err != nil {
				return -1
			}
			continue
		}
		if n, convErr := strconv.Atoi(got); convErr == nil && n >= 1 && n <= len(opts) {
			emitf("\n")
			return n - 1
		}
		Warn("type a number between 1 and %d", len(opts))
	}
}

// Password reads a secret without echoing it.
//
// Terminal echo is only suppressed when stdin really is a terminal and the
// shared reader holds nothing buffered -- reading the raw descriptor while
// bufio has already consumed part of the stream would silently drop input from
// a piped or scripted run.
func Password(question string) string {
	emitf("%s ", question)
	f, ok := in.(*os.File)
	if ok && stdin().Buffered() == 0 && term.IsTerminal(f.Fd()) {
		b, err := term.ReadPassword(f.Fd())
		emitf("\n")
		if err != nil {
			return ""
		}
		return strings.TrimSpace(string(b))
	}
	text, _ := stdin().ReadString('\n')
	return strings.TrimSpace(text)
}

// Field formats a label and value for a Box or a summary block.
func Field(label, value string) string {
	pad := 14
	if len(label) < pad {
		label += strings.Repeat(" ", pad-len(label))
	}
	return tint(label, slate) + " " + value
}

// Check renders a satisfied condition.
func Check(format string, a ...any) string { return mark("✓", "[ok]", emerald, format, a...) }

// Cross renders a failed condition.
func Cross(format string, a ...any) string { return mark("✗", "[!!]", red, format, a...) }

// Alert renders a condition the reader must judge for themselves.
func Alert(format string, a ...any) string { return mark("⚠", "[??]", amber, format, a...) }

// Pending renders work not yet done.
func Pending(format string, a ...any) string { return mark("○", "[  ]", slate, format, a...) }
