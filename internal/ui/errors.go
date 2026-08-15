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
	"errors"
	"fmt"
	"strings"

	"github.com/charmbracelet/lipgloss"
)

// Errors carry the fix, not just the complaint.
//
// "relation \"users\" does not exist" is accurate and useless: it does not say
// that the target needs its schema created by your own migrations first. Every
// failure a user can actually cause should name the command that resolves it,
// because the alternative is that they open an issue or give up.

// Hinted is an error with a suggested next step.
type Hinted struct {
	Err  error
	Fix  string // one sentence: what to do about it
	Cmd  string // optional command to run
	Docs string // optional link
}

func (h *Hinted) Error() string { return h.Err.Error() }
func (h *Hinted) Unwrap() error { return h.Err }

// Hint attaches a suggested fix to an error.
func Hint(err error, fix string) error {
	if err == nil {
		return nil
	}
	return &Hinted{Err: err, Fix: fix}
}

// HintCmd attaches a suggested fix and the command that applies it.
func HintCmd(err error, fix, cmd string) error {
	if err == nil {
		return nil
	}
	return &Hinted{Err: err, Fix: fix, Cmd: cmd}
}

// Fatal renders a failure. Cobra's default is a bare "Error: ..." line, which
// buries the message in whatever came before it.
func Fatal(err error) {
	if err == nil {
		return
	}
	var h *Hinted
	hinted := errors.As(err, &h)

	msg := err.Error()
	if hinted {
		msg = h.Err.Error()
	}

	if plain {
		emitf("[ERROR] %s\n", msg)
		if hinted {
			if h.Fix != "" {
				emitf("  fix: %s\n", h.Fix)
			}
			if h.Cmd != "" {
				emitf("  run: %s\n", h.Cmd)
			}
			if h.Docs != "" {
				emitf("  see: %s\n", h.Docs)
			}
		}
		return
	}

	body := []string{bold(tint("✗ "+firstLine(msg), red))}
	if rest := remainingLines(msg); rest != "" {
		body = append(body, tint(rest, slate))
	}
	if hinted && (h.Fix != "" || h.Cmd != "") {
		body = append(body, "")
		if h.Fix != "" {
			body = append(body, tint(h.Fix, slate))
		}
		if h.Cmd != "" {
			body = append(body, "  "+bold(tint(h.Cmd, emerald)))
		}
		if h.Docs != "" {
			body = append(body, tint(h.Docs, slate))
		}
	}
	emitf("\n%s\n", lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).BorderForeground(red).
		Padding(0, 2).Render(strings.Join(body, "\n")))
}

// NextStep suggests what to run after a command succeeds. A tool that ends by
// telling you the next command is far easier to learn than one that stops dead.
func NextStep(format string, a ...any) {
	cmd := fmt.Sprintf(format, a...)
	if plain {
		emitf("next: %s\n", cmd)
		return
	}
	emitf("%s %s\n", tint("next", slate), bold(tint(cmd, emerald)))
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	// Long single-line errors read better split at the first clause.
	if i := strings.Index(s, "; "); i > 0 && len(s) > 90 {
		return s[:i]
	}
	return s
}

func remainingLines(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return strings.TrimSpace(s[i+1:])
	}
	if i := strings.Index(s, "; "); i > 0 && len(s) > 90 {
		return strings.TrimSpace(s[i+2:])
	}
	return ""
}
