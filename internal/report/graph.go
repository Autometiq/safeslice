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
	"sort"
	"strings"
)

// The relationship diagram.
//
// A list of foreign keys tells you the slice is connected. A picture tells you
// its shape -- which table everything hangs off, how deep the closure went, and
// which tables came along only because something referenced them. That is the
// question people actually ask when they look at a slice.
//
// Drawn as inline SVG on the server: the report has to open from a file:// URL
// on a locked-down laptop months from now, so it cannot depend on a layout
// library, a font, or any script running at all.

const (
	nodeW   = 168
	nodeH   = 46
	gapX    = 96
	gapY    = 22
	padding = 24
	// Past this many tables the picture stops being a picture. The list under
	// it still says everything, so the diagram bows out rather than drawing a
	// hairball nobody can read.
	maxNodes = 28
)

type graphNode struct {
	name  string
	rows  int
	depth int
	slot  int
}

// relationshipSVG renders the slice as a layered diagram, left to right, with
// the root table first. Returns "" when there is nothing worth drawing.
func relationshipSVG(r Result) string {
	if len(r.Edges) == 0 || len(r.Tables) < 2 || len(r.Tables) > maxNodes {
		return ""
	}

	rows := make(map[string]int, len(r.Tables))
	for _, t := range r.Tables {
		rows[bare(t.Name)] = t.ExtractedRows
	}

	// Depth by breadth-first walk from the root, following edges in either
	// direction: a parent pulled in by the closure is as much a part of the
	// shape as a child, and the reader does not care which way the constraint
	// points when they are looking for the shape.
	adj := map[string][]string{}
	for _, e := range r.Edges {
		from, to := bare(e.From), bare(e.To)
		if from == to {
			continue // a self-reference draws as a loop; the list covers it
		}
		adj[from] = append(adj[from], to)
		adj[to] = append(adj[to], from)
	}

	root := bare(r.RootTable)
	if _, ok := rows[root]; !ok {
		return ""
	}
	depth := map[string]int{root: 0}
	queue := []string{root}
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		next := adj[cur]
		sort.Strings(next)
		for _, n := range next {
			if _, seen := depth[n]; seen {
				continue
			}
			if _, inSlice := rows[n]; !inSlice {
				continue // referenced but not extracted; not part of this slice
			}
			depth[n] = depth[cur] + 1
			queue = append(queue, n)
		}
	}

	// Anything the walk never reached still belongs on the canvas, or the
	// picture would quietly omit a table the slice actually contains.
	for name := range rows {
		if _, ok := depth[name]; !ok {
			depth[name] = 0
		}
	}

	names := make([]string, 0, len(depth))
	for n := range depth {
		names = append(names, n)
	}
	sort.Slice(names, func(i, j int) bool {
		if depth[names[i]] != depth[names[j]] {
			return depth[names[i]] < depth[names[j]]
		}
		return names[i] < names[j]
	})

	nodes := map[string]*graphNode{}
	perDepth := map[int]int{}
	maxDepth, maxSlot := 0, 0
	for _, n := range names {
		d := depth[n]
		nodes[n] = &graphNode{name: n, rows: rows[n], depth: d, slot: perDepth[d]}
		perDepth[d]++
		maxDepth = max(maxDepth, d)
		maxSlot = max(maxSlot, perDepth[d])
	}

	width := padding*2 + (maxDepth+1)*nodeW + maxDepth*gapX
	height := padding*2 + maxSlot*nodeH + (maxSlot-1)*gapY
	if height < nodeH+padding*2 {
		height = nodeH + padding*2
	}

	var b strings.Builder
	fmt.Fprintf(&b, `<svg class="graph" viewBox="0 0 %d %d" width="100%%" height="%d" `+
		`xmlns="http://www.w3.org/2000/svg" role="img" `+
		`aria-label="Foreign-key relationships between the tables in this slice">`,
		width, height, height)
	b.WriteString(`<defs><marker id="a" viewBox="0 0 8 8" refX="7" refY="4" markerWidth="7" ` +
		`markerHeight="7" orient="auto"><path d="M0 0 L8 4 L0 8 z" fill="currentColor" ` +
		`opacity=".55"/></marker></defs>`)

	// Edges first so the boxes sit on top of the lines.
	drawn := map[string]bool{}
	for _, e := range r.Edges {
		from, to := nodes[bare(e.From)], nodes[bare(e.To)]
		if from == nil || to == nil || from == to {
			continue
		}
		key := from.name + "\x00" + to.name
		if drawn[key] {
			continue // one line per pair, however many columns join them
		}
		drawn[key] = true

		x1, y1 := nodeX(from)+nodeW, nodeY(from)+nodeH/2
		x2, y2 := nodeX(to), nodeY(to)+nodeH/2
		if to.depth <= from.depth {
			x1, y1, x2, y2 = nodeX(from), nodeY(from)+nodeH/2, nodeX(to)+nodeW, nodeY(to)+nodeH/2
		}
		dash := ""
		if e.Virtual {
			dash = ` stroke-dasharray="5 4"` // declared in config, not by the schema
		}
		mid := (x1 + x2) / 2
		fmt.Fprintf(&b, `<path d="M%d %d C%d %d %d %d %d %d" fill="none" stroke="currentColor" `+
			`stroke-width="1.5" opacity=".38" marker-end="url(#a)"%s/>`,
			x1, y1, mid, y1, mid, y2, x2, y2, dash)
	}

	for _, n := range names {
		g := nodes[n]
		fill, stroke := "var(--panel)", "var(--line)"
		if g.depth == 0 && n == root {
			fill, stroke = "var(--panel)", "var(--emerald)"
		}
		fmt.Fprintf(&b, `<g><rect x="%d" y="%d" rx="8" width="%d" height="%d" fill="%s" `+
			`stroke="%s" stroke-width="1.5"/>`, nodeX(g), nodeY(g), nodeW, nodeH, fill, stroke)
		fmt.Fprintf(&b, `<text x="%d" y="%d" class="gn">%s</text>`,
			nodeX(g)+14, nodeY(g)+20, esc(n))
		label := comma(g.rows) + " rows"
		if n == root {
			label += " · root"
		}
		fmt.Fprintf(&b, `<text x="%d" y="%d" class="gr">%s</text></g>`,
			nodeX(g)+14, nodeY(g)+36, esc(label))
	}
	b.WriteString(`</svg>`)
	return b.String()
}

func nodeX(g *graphNode) int { return padding + g.depth*(nodeW+gapX) }
func nodeY(g *graphNode) int { return padding + g.slot*(nodeH+gapY) }

// bare strips the schema qualifier. The diagram has no room for "public." on
// every box, and a slice spanning two schemas with colliding table names is
// not a shape this picture could clarify anyway.
func bare(name string) string {
	if i := strings.LastIndexByte(name, '.'); i >= 0 {
		return name[i+1:]
	}
	return name
}
