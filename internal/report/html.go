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
:root{--bg:#0b0f19;--panel:#1c2333;--line:#2a344a;--text:#f1f5f9;--dim:#94a3b8;
--emerald:#10b981;--amber:#f59e0b;--red:#ef4444;--sky:#0ea5e9;
--shadow:0 10px 15px -3px rgba(0,0,0,0.5), 0 4px 6px -2px rgba(0,0,0,0.25)}
@media(prefers-color-scheme:light){:root{--bg:#f8fafc;--panel:#ffffff;--line:#e2e8f0;
--text:#0f172a;--dim:#64748b;--emerald:#059669;--amber:#d97706;--red:#dc2626;--sky:#0284c7;
--shadow:0 10px 15px -3px rgba(0,0,0,0.1), 0 4px 6px -2px rgba(0,0,0,0.05)}}
*{box-sizing:border-box}
body{margin:0;background:var(--bg);color:var(--text);
font:15px/1.6 'Inter',-apple-system,BlinkMacSystemFont,"Segoe UI",Roboto,Helvetica,Arial,sans-serif;
-webkit-font-smoothing:antialiased}
.wrap{max-width:1080px;margin:0 auto;padding:48px 24px 80px}
h1{font-size:30px;font-weight:700;margin:0 0 6px;letter-spacing:-0.02em}
h2{font-size:16px;text-transform:uppercase;letter-spacing:0.1em;color:var(--dim);
margin:48px 0 16px;font-weight:700}
.sub{color:var(--dim);margin:0 0 32px;font-size:16px}
.grid{display:grid;grid-template-columns:repeat(auto-fit,minmax(200px,1fr));gap:16px}
.card{background:var(--panel);border:1px solid var(--line);border-radius:12px;padding:24px;box-shadow:var(--shadow);transition:transform 0.2s}
.card:hover{transform:translateY(-2px)}
.card .k{color:var(--dim);font-size:12px;font-weight:600;text-transform:uppercase;letter-spacing:0.08em}
.card .v{font-size:32px;font-weight:700;margin-top:8px;letter-spacing:-0.03em;color:var(--text)}
table{width:100%;border-collapse:separate;border-spacing:0;background:var(--panel);
border:1px solid var(--line);border-radius:12px;overflow:hidden;box-shadow:var(--shadow)}
th,td{text-align:left;padding:12px 16px;border-bottom:1px solid var(--line);font-size:14px}
th{color:var(--dim);font-weight:700;font-size:12px;text-transform:uppercase;letter-spacing:0.08em;background:rgba(127,127,127,0.03)}
tr:last-child td{border-bottom:none}
td.n,th.n{text-align:right;font-variant-numeric:tabular-nums}
code,pre{font:13px/1.6 'JetBrains Mono',ui-monospace,SFMono-Regular,Menlo,monospace}
pre{background:var(--panel);border:1px solid var(--line);border-radius:12px;
padding:16px 20px;overflow-x:auto;margin:12px 0;box-shadow:var(--shadow)}
.check{display:flex;gap:12px;align-items:center;padding:14px 20px;background:var(--panel);
border:1px solid var(--line);border-radius:8px;margin-bottom:10px;font-weight:500;box-shadow:var(--shadow)}
.ok{color:var(--emerald)}.warn{color:var(--amber)}.bad{color:var(--red)}
.pill{display:inline-flex;align-items:center;padding:4px 10px;border-radius:999px;font-size:12px;font-weight:600;
border:1px solid var(--line);color:var(--dim);background:rgba(127,127,127,0.06)}
.pill--ok{color:var(--emerald);background:rgba(16,185,129,0.1);border-color:transparent}
.pill--warn{color:var(--amber);background:rgba(245,158,11,0.1);border-color:transparent}
.note{border-left:4px solid var(--sky);background:var(--panel);padding:16px 20px;
border-radius:0 12px 12px 0;color:var(--text);margin:16px 0;box-shadow:var(--shadow);font-size:15px}
footer{margin-top:64px;padding-top:24px;border-top:1px solid var(--line);color:var(--dim);font-size:14px;display:flex;justify-content:space-between}
a{color:var(--sky);text-decoration:none;font-weight:500}
a:hover{text-decoration:underline}
.graphwrap{background:var(--panel);border:1px solid var(--line);border-radius:12px;
padding:12px 8px;overflow-x:auto;color:var(--dim);box-shadow:var(--shadow)}
.graph{display:block;min-width:520px}
.graph .gn{fill:var(--text);font:700 13px 'Inter',-apple-system,sans-serif}
.graph .gr{fill:var(--dim);font:12px 'JetBrains Mono',ui-monospace,Menlo,monospace}
.barcell{width:34%;padding-right:18px}
.bar{display:block;height:8px;border-radius:999px;background:var(--emerald);min-width:2px}
details{background:var(--panel);border:1px solid var(--line);border-radius:12px;margin:10px 0;box-shadow:var(--shadow);transition:all 0.2s}
details[open]{box-shadow:0 20px 25px -5px rgba(0,0,0,0.1), 0 8px 10px -6px rgba(0,0,0,0.1)}
details>summary{cursor:pointer;padding:16px 20px;display:flex;align-items:center;gap:12px;
font-size:15px;list-style:none;user-select:none;border-radius:12px}
details>summary::-webkit-details-marker{display:none}
details>summary::before{content:"▸";color:var(--dim);font-size:12px;transition:transform .15s}
details[open]>summary::before{transform:rotate(90deg)}
details>summary:hover{background:rgba(127,127,127,.04)}
details>*:not(summary){margin:0 20px 18px}
details table{border:0;border-radius:0;background:none;box-shadow:none}
.tname{font-weight:700;color:var(--text)}
.tmeta{color:var(--dim);font-size:13px;font-variant-numeric:tabular-nums;margin-left:auto}
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
	p("<th class=\"n\">Extracted</th><th></th><th class=\"n\">Masked columns</th></tr>\n")
	// The bar makes the shape of the slice readable at a glance: which table
	// dominates it, and which came along as a handful of parent rows.
	widest := 1
	for _, t := range r.Tables {
		if t.ExtractedRows > widest {
			widest = t.ExtractedRows
		}
	}
	for _, t := range r.Tables {
		src := "—"
		if t.SourceRows > 0 {
			src = comma(int(t.SourceRows))
		}
		p("<tr><td>%s</td><td class=\"n\">%s</td><td class=\"n\">%s</td>", esc(t.Name), src,
			comma(t.ExtractedRows))
		p("<td class=\"barcell\"><span class=\"bar\" style=\"width:%.1f%%\"></span></td>",
			float64(t.ExtractedRows)*100/float64(widest))
		p("<td class=\"n\">%d</td></tr>\n", t.MaskedColumns)
	}
	p("<tr><td><strong>Total</strong></td><td class=\"n\"></td>")
	p("<td class=\"n\"><strong>%s</strong></td><td></td><td class=\"n\"></td></tr>\n", comma(r.TotalRows))
	p("</table>\n")

	// Masking, table by table.
	//
	// A flat list of forty columns answers "what was masked" but not the
	// question people actually open the report with, which is "what happened to
	// my users table". Every table gets an entry, including the ones nothing
	// touched -- absence is the part worth checking.
	if len(r.Rules) > 0 || len(r.Redacted) > 0 {
		p("<h2>What happened to each table</h2>\n")
		p("<p class=\"sub\">Click a table to see its columns. Tables with nothing masked ")
		p("are listed too — an empty one is worth noticing.</p>\n")
		for _, g := range byTable(r) {
			open := ""
			if g.masked+g.redacted > 0 && len(r.Tables) <= 8 {
				open = " open" // a small slice fits on screen; do not make them click
			}
			p("<details class=\"tbl\"%s><summary><span class=\"tname\">%s</span>", open, esc(g.name))
			p("<span class=\"tmeta\">%s rows</span>", comma(g.rows))
			switch {
			case g.masked+g.redacted == 0:
				p("<span class=\"pill\">nothing masked</span>")
			default:
				if g.masked > 0 {
					p("<span class=\"pill pill--ok\">%d %s masked</span>", g.masked, plural(g.masked, "column", "columns"))
				}
				if g.redacted > 0 {
					p("<span class=\"pill pill--warn\">%d dropped</span>", g.redacted)
				}
			}
			p("</summary>\n")
			if len(g.cols) == 0 {
				p("<p class=\"sub\" style=\"margin:10px 0 0\">No column in this table matched a ")
				p("masking rule. Its rows were copied unchanged.</p>\n")
			} else {
				p("<table>\n<tr><th>Column</th><th>Rule</th><th>What it becomes</th></tr>\n")
				for _, c := range g.cols {
					p("<tr><td><code>%s</code></td><td><span class=\"pill\">%s</span></td>"+
						"<td class=\"sub\">%s</td></tr>\n",
						esc(c.Column), esc(c.Rule), esc(ruleMeaning(c.Rule)))
				}
				p("</table>\n")
			}
			p("</details>\n")
		}
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
		if svg := relationshipSVG(r); svg != "" {
			p("<div class=\"graphwrap\">%s</div>\n", svg)
			p("<p class=\"sub\">The root table is outlined in green. A dashed line is a ")
			p("relationship declared in configuration rather than by the schema.</p>\n")
		}
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
