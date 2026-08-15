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

package report

import (
	"fmt"
	"strings"
	"time"
)

// The HTML report is a single self-contained file: inline CSS, no fonts, no
// scripts, no CDN. It gets opened from disk on a locked-down laptop, attached to
// a compliance ticket, and read months later -- anything fetched over the
// network would be broken in all three cases.

const reportCSS = `
:root{--bg:#0f172a;--panel:#16203a;--line:#26314f;--text:#e6edf7;--dim:#93a2bd;
--emerald:#34d399;--amber:#fbbf24;--red:#f87171;--sky:#38bdf8}
@media(prefers-color-scheme:light){:root{--bg:#f6f8fc;--panel:#fff;--line:#dfe5ef;
--text:#0f172a;--dim:#5a6b88;--emerald:#047857;--amber:#b45309;--red:#b91c1c;--sky:#0369a1}}
*{box-sizing:border-box}
body{margin:0;background:var(--bg);color:var(--text);
font:15px/1.6 ui-sans-serif,system-ui,-apple-system,"Segoe UI",Roboto,Helvetica,Arial,sans-serif}
.wrap{max-width:980px;margin:0 auto;padding:40px 24px 72px}
h1{font-size:26px;margin:0 0 4px;letter-spacing:-.01em}
h2{font-size:15px;text-transform:uppercase;letter-spacing:.08em;color:var(--dim);
margin:40px 0 12px;font-weight:600}
.sub{color:var(--dim);margin:0 0 28px}
.grid{display:grid;grid-template-columns:repeat(auto-fit,minmax(180px,1fr));gap:12px}
.card{background:var(--panel);border:1px solid var(--line);border-radius:10px;padding:16px}
.card .k{color:var(--dim);font-size:12px;text-transform:uppercase;letter-spacing:.06em}
.card .v{font-size:24px;font-weight:600;margin-top:4px;letter-spacing:-.02em}
table{width:100%;border-collapse:collapse;background:var(--panel);
border:1px solid var(--line);border-radius:10px;overflow:hidden}
th,td{text-align:left;padding:10px 14px;border-bottom:1px solid var(--line);font-size:14px}
th{color:var(--dim);font-weight:600;font-size:12px;text-transform:uppercase;letter-spacing:.06em}
tr:last-child td{border-bottom:none}
td.n,th.n{text-align:right;font-variant-numeric:tabular-nums}
code,pre{font:13px/1.6 ui-monospace,SFMono-Regular,Menlo,Consolas,monospace}
pre{background:var(--panel);border:1px solid var(--line);border-radius:10px;
padding:14px 16px;overflow-x:auto;margin:8px 0}
.check{display:flex;gap:10px;align-items:flex-start;padding:8px 0}
.ok{color:var(--emerald)}.warn{color:var(--amber)}.bad{color:var(--red)}
.pill{display:inline-block;padding:2px 8px;border-radius:999px;font-size:12px;
border:1px solid var(--line);color:var(--dim)}
.note{border-left:3px solid var(--amber);background:var(--panel);padding:12px 16px;
border-radius:0 8px 8px 0;color:var(--dim);margin:12px 0}
footer{margin-top:56px;padding-top:20px;border-top:1px solid var(--line);color:var(--dim);font-size:13px}
a{color:var(--sky)}
`

func reportHTML(r Result) string {
	var b strings.Builder
	p := func(format string, a ...any) { fmt.Fprintf(&b, format, a...) }

	p("<!doctype html>\n<html lang=\"en\">\n<head>\n<meta charset=\"utf-8\">\n")
	p("<meta name=\"viewport\" content=\"width=device-width,initial-scale=1\">\n")
	p("<title>safeslice report — %s</title>\n<style>%s</style>\n</head>\n<body>\n<div class=\"wrap\">\n",
		esc(r.Target.Database), reportCSS)

	p("<h1>Safeslice development database</h1>\n")
	p("<p class=\"sub\">%s → <strong>%s</strong> · %s · safeslice %s</p>\n",
		esc(r.Source.Database), esc(r.Target.Database),
		esc(r.GeneratedAt.Format(time.RFC1123)), esc(r.Version))

	// Headline numbers.
	p("<div class=\"grid\">\n")
	card(&b, "Rows loaded", comma(r.TotalRows))
	card(&b, "Tables", fmt.Sprint(len(r.Tables)))
	card(&b, "Columns masked", fmt.Sprint(len(r.Rules)))
	card(&b, "Duration", r.Duration.Round(time.Millisecond).String())
	p("</div>\n")

	// Safety and privacy, stated without overclaiming.
	p("<h2>Checks</h2>\n")
	check(&b, true, "Source opened read-only — nothing was written to it")
	check(&b, r.FKOrphans == 0, fmt.Sprintf("%d foreign-key orphans", r.FKOrphans))
	if r.Verification.Ran {
		if r.Verification.Passed {
			check(&b, true, "Privacy scan found no personal data in the sampled rows")
		} else {
			check(&b, false, fmt.Sprintf("Privacy scan flagged %d columns", len(r.Verification.Findings)))
			for _, f := range r.Verification.Findings {
				p("<div class=\"check\"><span class=\"bad\">•</span><span>%s</span></div>\n", esc(f))
			}
		}
	} else {
		p("<div class=\"check\"><span class=\"warn\">⚠</span><span>Privacy scan did not run</span></div>\n")
	}
	p("<div class=\"note\">%s</div>\n", esc(r.Verification.Caveat))

	// Slice definition, including the seed that makes it reproducible.
	p("<h2>Slice</h2>\n<table>\n")
	row2(&b, "Root table", esc(r.RootTable))
	if r.Where != "" {
		row2(&b, "Filter", "<code>"+esc(r.Where)+"</code>")
	}
	row2(&b, "Child depth", fmt.Sprint(r.ChildDepth))
	row2(&b, "Masking seed", "<code>"+esc(r.Seed)+"</code>")
	row2(&b, "Source", esc(r.Source.String()))
	row2(&b, "Target", esc(r.Target.String()))
	p("</table>\n")
	p("<p class=\"sub\">The same seed against the same source reproduces this database exactly.</p>\n")

	// Tables.
	p("<h2>Tables</h2>\n<table>\n<tr><th>Table</th><th class=\"n\">Source rows</th>")
	p("<th class=\"n\">Extracted</th><th class=\"n\">Masked columns</th></tr>\n")
	for _, t := range r.Tables {
		src := "—"
		if t.SourceRows > 0 {
			src = comma(int(t.SourceRows))
		}
		p("<tr><td>%s</td><td class=\"n\">%s</td><td class=\"n\">%s</td><td class=\"n\">%d</td></tr>\n",
			esc(t.Name), src, comma(t.ExtractedRows), t.MaskedColumns)
	}
	p("<tr><td><strong>Total</strong></td><td class=\"n\"></td>")
	p("<td class=\"n\"><strong>%s</strong></td><td class=\"n\"></td></tr>\n", comma(r.TotalRows))
	p("</table>\n")

	// Masking.
	if len(r.Rules) > 0 || len(r.Redacted) > 0 {
		p("<h2>Masking</h2>\n<table>\n<tr><th>Column</th><th>Rule</th></tr>\n")
		for _, rule := range r.Rules {
			p("<tr><td><code>%s</code></td><td><span class=\"pill\">%s</span></td></tr>\n",
				esc(rule.Column), esc(rule.Rule))
		}
		for _, rule := range r.Redacted {
			p("<tr><td><code>%s</code></td><td><span class=\"pill\">%s</span></td></tr>\n",
				esc(rule.Column), esc(rule.Rule))
		}
		p("</table>\n")
	}
	if len(r.Unreviewed) > 0 {
		p("<h2>Not reviewed</h2>\n")
		p("<div class=\"note\">These text columns passed through unchanged without being ")
		p("classified. They may contain personal data.</div>\n<table>\n")
		for _, c := range r.Unreviewed {
			p("<tr><td><code>%s</code></td></tr>\n", esc(c))
		}
		p("</table>\n")
	}

	// Relationships: what held the slice together, and what could not.
	if len(r.Relationships) > 0 || len(r.Soft) > 0 {
		p("<h2>Relationships</h2>\n")
		if len(r.Relationships) > 0 {
			p("<table>\n<tr><th>Foreign key</th></tr>\n")
			for _, rel := range r.Relationships {
				p("<tr><td><code>%s</code></td></tr>\n", esc(rel))
			}
			p("</table>\n")
		}
		if len(r.Soft) > 0 {
			p("<div class=\"note\">%d column pairs look like relationships but have no ", len(r.Soft))
			p("foreign key, so they were not followed. Declare the ones that matter as ")
			p("<code>virtual_keys</code>.</div>\n<table>\n")
			for _, s := range r.Soft {
				p("<tr><td><code>%s</code></td></tr>\n", esc(s))
			}
			p("</table>\n")
		}
	}

	// Connect.
	p("<h2>Connect</h2>\n")
	for _, s := range Snippets(r.Target) {
		p("<h3 class=\"sub\">%s</h3>\n<pre>%s</pre>\n", esc(s.Name), esc(s.Code))
	}

	if len(r.Warnings) > 0 {
		p("<h2>Warnings</h2>\n")
		for _, w := range r.Warnings {
			p("<div class=\"check\"><span class=\"warn\">⚠</span><span>%s</span></div>\n", esc(w))
		}
	}

	p("<footer>Generated by safeslice %s — <a href=\"https://autometiq.com\">Autometiq</a>. ",
		esc(r.Version))
	p("This report contains no credentials and no row data.</footer>\n")
	p("</div>\n</body>\n</html>\n")
	return b.String()
}

func card(b *strings.Builder, k, v string) {
	fmt.Fprintf(b, "<div class=\"card\"><div class=\"k\">%s</div><div class=\"v\">%s</div></div>\n",
		esc(k), esc(v))
}

func row2(b *strings.Builder, k, vHTML string) {
	fmt.Fprintf(b, "<tr><th>%s</th><td>%s</td></tr>\n", esc(k), vHTML)
}

func check(b *strings.Builder, ok bool, text string) {
	class, mark := "ok", "✓"
	if !ok {
		class, mark = "bad", "✗"
	}
	fmt.Fprintf(b, "<div class=\"check\"><span class=\"%s\">%s</span><span>%s</span></div>\n",
		class, mark, esc(text))
}
