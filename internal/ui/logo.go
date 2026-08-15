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
	"runtime"
	"strings"
)

// The splash screen: a database cylinder wearing a lock, which is the mark from
// the project logo reduced to something a terminal can draw.
//
// Deliberately small. A twenty-line ASCII banner is charming once and tiresome
// every time after, so this stays to five lines and never appears when output is
// piped, when --quiet is passed, or on a command that produces data.

var logoMark = []string{
	" ╭───────╮ ",
	" │▁▁▁▁▁▁▁│ ",
	"┌─────────┐",
	"│    ▐▌   │",
	" ╰───────╯ ",
}

// asciiMark is used where box-drawing characters would render as garbage.
var asciiMark = []string{
	"  ,-----.  ",
	" |=======| ",
	" +-------+ ",
	" |   |_|  |",
	"  `-----'  ",
}

// unicodeOK reports whether the terminal can be trusted with box drawing.
// Legacy Windows conhost cannot; Windows Terminal and everything else can.
func unicodeOK() bool {
	if runtime.GOOS != "windows" {
		return true
	}
	return os.Getenv("WT_SESSION") != "" || os.Getenv("TERM_PROGRAM") != "" ||
		os.Getenv("TERM") != ""
}

// Splash prints the logo, name and one-line mission. Used by the interactive
// menu, where the screen is the product's first impression.
func Splash(version string) {
	if plain {
		emitf("safeslice %s\n%s\nby %s • %s\n\n", version, Mission, Vendor, VendorURL)
		return
	}

	mark := logoMark
	if !unicodeOK() {
		mark = asciiMark
	}

	// Text sits to the right of the mark, vertically centred against it.
	text := []string{
		"",
		bold(tint("safeslice", emerald)) + "  " + tint(version, slate),
		tint("Shrink & mask production Postgres", slate),
		tint("by "+Vendor+" • "+VendorURL, slate),
		"",
	}

	var b strings.Builder
	b.WriteString("\n")
	for i, row := range mark {
		b.WriteString("  " + tint(row, emerald) + "   " + text[i] + "\n")
	}
	emit(b.String() + "\n")
}
