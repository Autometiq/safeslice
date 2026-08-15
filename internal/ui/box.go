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

	"github.com/charmbracelet/lipgloss"
)

// A titled panel, used for the two screens that decide something: the review
// shown before any data moves, and the result shown after.
//
// Drawn by hand rather than with lipgloss's border helper because the title bar
// needs a divider under it, and because padding has to be measured with
// lipgloss.Width -- the content carries colour, and len() on an ANSI string
// counts escape bytes and tears the right-hand edge.

const boxWidth = 58

// mark renders a status glyph and message, degrading to a bracketed tag in
// plain mode where a log scraper has to read it.
func mark(glyph, plainGlyph string, c lipgloss.TerminalColor, format string, a ...any) string {
	msg := fmt.Sprintf(format, a...)
	if plain {
		return plainGlyph + " " + msg
	}
	return tint(glyph, c) + " " + msg
}

// Box prints a titled panel. An empty line in body renders as a blank row.
func Box(title string, body []string) {
	inner := boxWidth - 2
	for _, l := range body {
		if w := lipgloss.Width(l) + 2; w > inner {
			inner = w
		}
	}
	if w := len(title) + 2; w > inner {
		inner = w
	}

	if plain {
		emitf("\n%s\n", strings.Repeat("=", inner))
		emitf("%s\n", title)
		emitf("%s\n", strings.Repeat("-", inner))
		for _, l := range body {
			emitf("%s\n", l)
		}
		emitf("%s\n", strings.Repeat("=", inner))
		return
	}

	edge := func(s string) string { return tint(s, emerald) }
	var b strings.Builder
	b.WriteString("\n" + edge("╭"+strings.Repeat("─", inner)+"╮") + "\n")
	b.WriteString(edge("│") + center(bold(title), inner) + edge("│") + "\n")
	b.WriteString(edge("├"+strings.Repeat("─", inner)+"┤") + "\n")
	for _, l := range body {
		b.WriteString(edge("│") + " " + pad(l, inner-1) + edge("│") + "\n")
	}
	b.WriteString(edge("╰" + strings.Repeat("─", inner) + "╯"))
	emit1(b.String())
}

func pad(s string, width int) string {
	if n := width - lipgloss.Width(s); n > 0 {
		return s + strings.Repeat(" ", n)
	}
	return s
}

func center(s string, width int) string {
	gap := width - lipgloss.Width(s)
	if gap <= 0 {
		return s
	}
	left := gap / 2
	return strings.Repeat(" ", left) + s + strings.Repeat(" ", gap-left)
}
