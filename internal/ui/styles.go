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

// Package ui renders safeslice's terminal output.
//
// Two rules shape everything here.
//
// Everything goes to stderr. stdout belongs to the data: `safeslice run --out -`
// pipes SQL, and a status line mixed into that stream produces a file that
// looks fine and will not replay.
//
// Plain mode is not a downgrade. When output is redirected, or NO_COLOR is set,
// or --no-color is passed, the same information is printed without escape
// sequences or box-drawing characters. A CI log full of mojibake is worse than
// no styling at all, and log scrapers should not have to strip ANSI.
package ui

import (
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/charmbracelet/lipgloss"
)

// Autometiq attribution, shown in the banner and the closing call to action.
const (
	Vendor    = "Autometiq"
	VendorURL = "https://autometiq.com"
	Mission   = "Fast, referentially-intact database subsetting & masking"
)

// Adaptive colours so the palette stays legible on light and dark terminals.
var (
	emerald = lipgloss.AdaptiveColor{Light: "#047857", Dark: "#34D399"}
	slate   = lipgloss.AdaptiveColor{Light: "#475569", Dark: "#94A3B8"}
	amber   = lipgloss.AdaptiveColor{Light: "#B45309", Dark: "#FBBF24"}
	red     = lipgloss.AdaptiveColor{Light: "#B91C1C", Dark: "#F87171"}
	sky     = lipgloss.AdaptiveColor{Light: "#0369A1", Dark: "#38BDF8"}
	ink     = lipgloss.AdaptiveColor{Light: "#F8FAFC", Dark: "#0F172A"}
)

var (
	out   io.Writer = os.Stderr
	plain           = !isTerminal(os.Stderr)
)

// SetPlain forces unstyled output. Called for --no-color and NO_COLOR.
func SetPlain(v bool) {
	if v {
		plain = true
	}
}

// SetOutput redirects UI output. Used by tests.
func SetOutput(w io.Writer) { out = w }

// Plain reports whether styling is suppressed.
func Plain() bool { return plain }

func isTerminal(f *os.File) bool {
	if os.Getenv("NO_COLOR") != "" || os.Getenv("TERM") == "dumb" {
		return false
	}
	info, err := f.Stat()
	return err == nil && info.Mode()&os.ModeCharDevice != 0
}

// badge renders a bold, high-contrast tag. In plain mode it degrades to the
// bracketed form, which is what a log scraper wants anyway.
func badge(label string, bg lipgloss.TerminalColor) string {
	if plain {
		return "[" + label + "]"
	}
	return lipgloss.NewStyle().
		Bold(true).Foreground(ink).Background(bg).
		Padding(0, 1).Render(label)
}

func tint(s string, c lipgloss.TerminalColor) string {
	if plain {
		return s
	}
	return lipgloss.NewStyle().Foreground(c).Render(s)
}

func bold(s string) string {
	if plain {
		return s
	}
	return lipgloss.NewStyle().Bold(true).Render(s)
}

func line(b, msg string) {
	emitf("%s %s\n", b, msg)
}

// Info reports ordinary progress.
func Info(format string, a ...any) { line(badge("INFO", sky), fmt.Sprintf(format, a...)) }

// Plan reports what a run intends to do.
func PlanStep(format string, a ...any) { line(badge("PLAN", slate), fmt.Sprintf(format, a...)) }

// Mask reports masking decisions.
func Mask(format string, a ...any) { line(badge("MASK", emerald), fmt.Sprintf(format, a...)) }

// Success reports a completed step.
func Success(format string, a ...any) { line(badge("SUCCESS", emerald), fmt.Sprintf(format, a...)) }

// Warn reports something the user should read but which does not stop the run.
func Warn(format string, a ...any) { line(badge("WARN", amber), fmt.Sprintf(format, a...)) }

// Error reports a failure. Cobra prints the returned error itself, so this is
// for failures surfaced mid-run.
func Error(err error) {
	if err == nil {
		return
	}
	line(badge("ERROR", red), err.Error())
}

// Detail prints an indented supporting line, dimmed so it reads as subordinate
// to the badge above it.
func Detail(format string, a ...any) {
	emitf("  %s\n", tint(fmt.Sprintf(format, a...), slate))
}

// Section prints a heading between phases.
func Section(title string) {
	if plain {
		emitf("\n== %s ==\n", title)
		return
	}
	emitf("\n%s\n", lipgloss.NewStyle().Bold(true).Foreground(emerald).Render(title))
}

// Banner prints the startup header.
func Banner(version string) {
	name := bold(tint("safeslice", emerald)) + " " + tint(version, slate)
	attribution := tint(fmt.Sprintf("by %s • %s", Vendor, VendorURL), slate)
	if plain {
		emitf("safeslice %s\n%s\nby %s • %s\n\n", version, Mission, Vendor, VendorURL)
		return
	}
	emitf("\n%s\n%s\n%s\n\n", name, tint(Mission, slate), attribution)
}

// RunStats is what a completed run reports.
type RunStats struct {
	Tables        int
	Rows          int
	MaskedColumns int
	Duration      time.Duration
	// Target is the redacted destination description, or empty when writing SQL.
	Target string
	// OutFile is set when SQL was written instead of loaded.
	OutFile string
	// Warnings are optional steps that were skipped, such as disabling triggers
	// without table ownership.
	Warnings []string
}

// Summary prints the closing report.
//
// The integrity claim is deliberately narrow. safeslice disables user triggers
// during the load but never the system triggers that enforce foreign keys, so a
// successful COMMIT is PostgreSQL itself confirming there are no orphans -- that
// is a fact worth stating. Masking carries no such proof: nothing here inspects
// the loaded rows, so this reports how many columns were masked and points at
// `safeslice verify` rather than claiming a clean bill of health it has not
// earned.
func Summary(s RunStats) {
	rows := [][2]string{
		{"Tables processed", fmt.Sprint(s.Tables)},
		{"Rows extracted & masked", fmt.Sprint(s.Rows)},
		{"Columns masked", fmt.Sprint(s.MaskedColumns)},
		{"Execution time", Duration(s.Duration)},
	}
	switch {
	case s.Target != "":
		rows = append(rows, [2]string{"Loaded into", s.Target})
	case s.OutFile != "":
		rows = append(rows, [2]string{"Written to", s.OutFile})
	}

	body := make([]string, 0, len(rows)+3)
	for _, r := range rows {
		body = append(body, fmt.Sprintf("%-26s %s", r[0]+":", bold(r[1])))
	}
	body = append(body, "")
	body = append(body, tint("Zero FK orphans", emerald)+
		tint("  — foreign keys stayed enforced through the load", slate))
	if s.MaskedColumns > 0 {
		body = append(body, tint(fmt.Sprintf("%d columns masked", s.MaskedColumns), emerald)+
			tint("  — run `safeslice verify` to confirm no PII remains", slate))
	} else {
		body = append(body, tint("No columns masked", amber)+
			tint("  — check your rules before trusting this slice", slate))
	}

	content := strings.Join(body, "\n")
	if plain {
		emitf("\n%s\n%s\n%s\n", strings.Repeat("-", 60), content, strings.Repeat("-", 60))
	} else {
		emit1("\n" + lipgloss.NewStyle().
			Border(lipgloss.RoundedBorder()).BorderForeground(emerald).
			Padding(0, 2).Render(content))
	}

	for _, w := range s.Warnings {
		Warn("%s", w)
	}
	Footer()
}

// Footer prints the closing call to action. One line, after the result, never
// in the way of it.
func Footer() {
	msg := "Need custom masking rules or enterprise pipeline integration? " + VendorURL
	if plain {
		emitf("%s\n", msg)
		return
	}
	emitf("%s\n", tint(msg, slate))
}

// Duration formats an elapsed time the way a developer reads it: milliseconds
// while that is meaningful, seconds once it is not.
func Duration(d time.Duration) string {
	switch {
	case d < time.Millisecond:
		return fmt.Sprintf("%dµs", d.Microseconds())
	case d < time.Second:
		return fmt.Sprintf("%dms", d.Milliseconds())
	case d < time.Minute:
		return fmt.Sprintf("%.2fs", d.Seconds())
	default:
		return fmt.Sprintf("%dm%02ds", int(d.Minutes()), int(d.Seconds())%60)
	}
}

// Table renders aligned columns. lipgloss box-drawing is skipped in plain mode
// so redirected output stays greppable.
func Table(headers []string, rows [][]string) {
	widths := make([]int, len(headers))
	for i, h := range headers {
		widths[i] = len(h)
	}
	for _, r := range rows {
		for i, c := range r {
			if i < len(widths) && len(c) > widths[i] {
				widths[i] = len(c)
			}
		}
	}
	render := func(cells []string, header bool) {
		parts := make([]string, len(cells))
		for i, c := range cells {
			pad := c
			if i < len(widths) {
				pad = c + strings.Repeat(" ", widths[i]-len(c))
			}
			if header {
				pad = bold(pad)
			}
			parts[i] = pad
		}
		emitf("  %s\n", strings.TrimRight(strings.Join(parts, "  "), " "))
	}
	render(headers, true)
	for _, r := range rows {
		render(r, false)
	}
}

// Step prints a progress line for one unit of work.
func Step(count int, label string) {
	emitf("  %s  %s\n", tint(fmt.Sprintf("%8d", count), emerald), label)
}
