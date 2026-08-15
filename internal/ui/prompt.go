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
	"bufio"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
)

// Prompts are typed, not arrow-driven.
//
// Arrow-key menus need raw terminal mode, which behaves differently across
// Windows conhost, Windows Terminal, PowerShell ISE, tmux and CI. A typed
// choice works identically everywhere, survives being piped, and can be
// scripted -- `echo 2 | safeslice` does what you would expect. The presentation
// is still styled; only the input mechanism is boring on purpose.

// One shared reader for the whole process.
//
// A bufio.Reader reads ahead. Creating a fresh one per prompt means the first
// prompt swallows every remaining line into a buffer that is then discarded,
// and every later prompt sees EOF -- so a piped or scripted run answers the
// first question and abandons the rest.
var (
	in     io.Reader = os.Stdin
	reader *bufio.Reader
)

// SetInput redirects prompt input. Used by tests.
func SetInput(r io.Reader) {
	in = r
	reader = nil
}

func stdin() *bufio.Reader {
	if reader == nil {
		reader = bufio.NewReader(in)
	}
	return reader
}

// Choice is one selectable option.
type Choice struct {
	Key   string // what the user types
	Label string
	Note  string // dimmed explanation
}

// Menu renders the choices and returns the Key that was selected. It reprompts
// on unrecognised input, and returns "" if the input stream ends -- which is
// what happens when stdin is closed, and must not become an infinite loop.
func Menu(title string, choices []Choice) string {
	emitf("%s\n\n", bold(title))
	width := 0
	for _, c := range choices {
		if len(c.Label) > width {
			width = len(c.Label)
		}
	}
	for _, c := range choices {
		label := c.Label + strings.Repeat(" ", width-len(c.Label))
		row := "  " + bold(tint(c.Key, emerald)) + "  " + label
		if c.Note != "" {
			row += "   " + tint(c.Note, slate)
		}
		emitf("%s\n", row)
	}
	emitf("\n")

	valid := map[string]bool{}
	for _, c := range choices {
		valid[strings.ToLower(c.Key)] = true
	}

	r := stdin()
	for {
		emitf("%s ", tint("›", emerald))
		text, err := r.ReadString('\n')
		if err != nil && strings.TrimSpace(text) == "" {
			emitf("\n")
			return ""
		}
		got := strings.ToLower(strings.TrimSpace(text))
		if valid[got] {
			emitf("\n")
			return got
		}
		Warn("pick one of the options above")
	}
}

// Ask reads a line of free text, returning def when the user just presses enter.
func Ask(question, def string) string {
	if def != "" {
		emitf("%s %s ", question, tint("["+def+"]", slate))
	} else {
		emitf("%s ", question)
	}
	r := stdin()
	text, err := r.ReadString('\n')
	got := strings.TrimSpace(text)
	if got == "" || (err != nil && got == "") {
		return def
	}
	return got
}

// Confirm asks a yes/no question. Anything other than an explicit yes is a no:
// this gates writes to a database, so silence must not mean consent.
func Confirm(question string) bool {
	emitf("%s %s ", question, tint("[y/N]", slate))
	r := stdin()
	text, _ := r.ReadString('\n')
	got := strings.ToLower(strings.TrimSpace(text))
	return got == "y" || got == "yes"
}

// AskInt reads a number, falling back to def on anything unparseable.
func AskInt(question string, def int) int {
	got := Ask(question, strconv.Itoa(def))
	n, err := strconv.Atoi(strings.TrimSpace(got))
	if err != nil || n < 0 {
		return def
	}
	return n
}

// Step prints a numbered stage heading inside a guided flow.
func Stage(n int, total int, title string) {
	if plain {
		emitf("\n== [%d/%d] %s ==\n", n, total, title)
		return
	}
	emitf("\n%s %s\n", tint(fmt.Sprintf("[%d/%d]", n, total), slate),
		bold(tint(title, emerald)))
}
