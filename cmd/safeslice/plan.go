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

package main

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/spf13/cobra"

	"github.com/Autometiq/safeslice/internal/catalog"
	"github.com/Autometiq/safeslice/internal/config"
	"github.com/Autometiq/safeslice/internal/graph"
	"github.com/Autometiq/safeslice/internal/load"
	"github.com/Autometiq/safeslice/internal/mask"
	"github.com/Autometiq/safeslice/internal/ui"
)

func planCmd() *cobra.Command {
	var (
		from       string
		table      string
		where      string
		limit      int
		childDepth int
	)
	cmd := &cobra.Command{
		Use:   "plan",
		Short: "Show what a slice would contain, without reading any data",
		Long: `plan connects to the source, reads its schema, and reports exactly what a
run would do: which relationships were found, how cycles will be handled, and
which columns will be masked.

It reads catalog metadata only. No table data is fetched.`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx := cmd.Context()
			cfg, err := config.Load(flagConfig)
			if err != nil {
				return err
			}
			applyOverrides(cfg, flagSchemas, table, where, limit, childDepth, cmd)

			dsn, err := sourceDSN(from)
			if err != nil {
				return err
			}
			conn, err := connectSource(ctx, dsn)
			if err != nil {
				return err
			}
			defer conn.Close(context.Background())

			ui.Info("source %s (read-only session)", describe(dsn))
			cat, err := catalog.Load(ctx, conn, cfg.Source.Schemas)
			if err != nil {
				return err
			}
			return report(cat, cfg)
		},
	}
	f := cmd.Flags()
	f.StringVar(&from, "from", "", "source connection string (or SAFESLICE_SOURCE / DATABASE_URL)")
	f.StringVar(&table, "table", "", "root table to slice from")
	f.StringVar(&where, "where", "", "SQL predicate applied to the root table")
	f.IntVar(&limit, "limit", 0, "maximum root rows")
	f.IntVar(&childDepth, "child-depth", 0, "levels of child rows to follow")
	return cmd
}

// applyOverrides lets flags win over the config file, but only when the flag was
// actually set -- otherwise a zero-valued flag would silently wipe a configured
// limit.
func applyOverrides(cfg *config.Config, schemas []string, table, where string, limit, childDepth int, cmd *cobra.Command) {
	if len(schemas) > 0 {
		cfg.Source.Schemas = schemas
	}
	if table != "" {
		cfg.Slice.Root = table
	}
	if cmd.Flags().Changed("where") {
		cfg.Slice.Where = where
	}
	if cmd.Flags().Changed("limit") {
		cfg.Slice.Limit = limit
	}
	if cmd.Flags().Changed("child-depth") {
		cfg.Slice.ChildDepth = childDepth
	}
}

func report(cat *catalog.Catalog, cfg *config.Config) error {
	virtual := cfg.FKs()
	g := graph.New(cat, virtual)
	refs := cat.Refs()
	allFKs := append(append([]catalog.FK{}, cat.FKs...), virtual...)

	reportSchema(cat, cfg, refs, virtual)
	if err := reportRoot(cat, cfg, g); err != nil {
		return err
	}
	if err := reportLoadStrategy(cat, refs, allFKs); err != nil {
		return err
	}
	if err := reportMasking(cat, cfg, refs); err != nil {
		return err
	}
	if !flagQuiet {
		ui.Footer()
	}
	return nil
}

func reportSchema(cat *catalog.Catalog, cfg *config.Config, refs []catalog.Ref, virtual []catalog.FK) {
	ui.Section("Schema")
	rows := [][]string{
		{"Schemas", strings.Join(cfg.Source.Schemas, ", ")},
		{"Tables", fmt.Sprint(len(refs))},
		{"Foreign keys", fmt.Sprint(len(cat.FKs))},
	}
	if len(virtual) > 0 {
		rows = append(rows, []string{"Virtual keys", fmt.Sprint(len(virtual))})
	}
	ui.Table([]string{"", ""}, rows)

	for _, fk := range virtual {
		note := ""
		if fk.When != "" {
			note = " when " + fk.When
		}
		ui.Detail("virtual  %s(%s) -> %s(%s)%s", fk.Table, strings.Join(fk.Columns, ", "),
			fk.RefTable, strings.Join(fk.RefColumns, ", "), note)
	}
}

func reportRoot(cat *catalog.Catalog, cfg *config.Config, g *graph.Graph) error {
	if cfg.Slice.Root == "" {
		ui.Warn("no root table set; pass --table or set slice.root in the config")
		return nil
	}
	root := cfg.Root()
	if _, ok := cat.Table(root); !ok {
		return fmt.Errorf("root table %s not found in schemas %s",
			root, strings.Join(cfg.Source.Schemas, ", "))
	}
	// Slicing one partition yields an incomplete result: its siblings hold the
	// rest of the rows for the same logical table.
	if target := g.ReadTarget(root); target != root {
		ui.Warn("%s is a partition; reading %s instead", root, target)
		root = target
	}
	ui.Section("Slice")
	rows := [][]string{{"Root table", root.String()}}
	if cfg.Slice.Where != "" {
		rows = append(rows, []string{"Where", cfg.Slice.Where})
	}
	rows = append(rows,
		[]string{"Root row limit", fmt.Sprint(cfg.Slice.Limit)},
		[]string{"Child depth", fmt.Sprint(cfg.Slice.ChildDepth)})
	ui.Table([]string{"", ""}, rows)
	return nil
}

func reportLoadStrategy(cat *catalog.Catalog, refs []catalog.Ref, fks []catalog.FK) error {
	ui.Section("Load strategy")
	for _, c := range graph.Cycles(refs, fks) {
		names := make([]string, len(c))
		for i, r := range c {
			names[i] = r.Name
		}
		ui.Detail("cycle    %s", strings.Join(names, " -> "))
	}
	for _, r := range graph.SelfReferences(fks) {
		ui.Detail("self-ref %s", r.Name)
	}

	plan, err := load.PlanCycles(cat, refs, fks)
	if err != nil {
		// Better to say so now than to fail halfway through writing to the
		// target database.
		return err
	}
	switch {
	case plan.Deferred:
		ui.Success("cycles are DEFERRABLE: one transaction with SET CONSTRAINTS ALL DEFERRED")
	case len(plan.Breaks) > 0:
		// The case that breaks every other tool: SET CONSTRAINTS silently does
		// nothing for constraints that were never declared DEFERRABLE.
		ui.Warn("cycles are not DEFERRABLE; breaking them with a second pass:")
		for _, b := range plan.Breaks {
			ui.Detail("%s.%s inserted as NULL, updated after load (%s)",
				b.Table.Name, strings.Join(b.Columns, ", "), b.Reason)
		}
	default:
		ui.Success("no cycles: a plain ordered load is enough")
	}
	return nil
}

// maskedColumns counts the columns a run will actually rewrite.
func maskedColumns(cat *catalog.Catalog, cfg *config.Config) int {
	cl := cfg.Classifier()
	n := 0
	for _, ref := range cat.Refs() {
		t, ok := cat.Table(ref)
		if !ok || t.Partition {
			continue // counted once, on the parent
		}
		keys := cat.KeyColumns(ref)
		for _, col := range t.Columns {
			if keys[col.Name] || !col.Insertable() {
				continue
			}
			if cl.Rule(ref, col.Name) != mask.Keep {
				n++
			}
		}
	}
	return n
}

// strictViolations lists text columns in PII tables that no rule covers.
//
// This is the fail-closed check. A column nobody classified is exactly the one
// that leaks, so both `plan` and `run` refuse to continue while any remain.
func strictViolations(cat *catalog.Catalog, cfg *config.Config) []string {
	cl := cfg.Classifier()
	var out []string
	for _, ref := range cat.Refs() {
		t, ok := cat.Table(ref)
		// Partitions inherit their parent's columns and are read through the
		// parent, so a rule on the parent covers them. Checking them separately
		// made `init` emit a config that `run` then rejected.
		if !ok || t.Partition || !cfg.StrictFor(ref) {
			continue
		}
		for _, c := range cl.Unclassified(t, cat.KeyColumns(ref)) {
			out = append(out, ref.Name+"."+c)
		}
	}
	sort.Strings(out)
	return out
}

func reportMasking(cat *catalog.Catalog, cfg *config.Config, refs []catalog.Ref) error {
	ui.Section("Masking")
	cl := cfg.Classifier()

	var rows [][]string
	for _, ref := range refs {
		t, ok := cat.Table(ref)
		if !ok || t.Partition {
			continue // listed once, on the parent
		}
		keys := cat.KeyColumns(ref)
		for _, col := range t.Columns {
			// Key columns are never masked: rewriting them would destroy the
			// referential integrity the slice exists to preserve.
			if keys[col.Name] || !col.Insertable() {
				continue
			}
			if r := cl.Rule(ref, col.Name); r != mask.Keep {
				rows = append(rows, []string{ref.Name, col.Name, string(r)})
			}
		}
	}

	if len(rows) == 0 {
		ui.Warn("no columns matched a masking rule")
	} else {
		ui.Table([]string{"TABLE", "COLUMN", "RULE"}, rows)
		ui.Mask("%d columns will be masked", len(rows))
	}

	violations := strictViolations(cat, cfg)
	if len(violations) == 0 {
		return nil
	}
	ui.Error(fmt.Errorf("strict mode: %d text columns in PII tables have no rule", len(violations)))
	for _, v := range violations {
		ui.Detail("%s", v)
	}
	// Fail closed. A column nobody classified is the one that leaks.
	return fmt.Errorf("strict mode: classify these columns in %s (use `keep` if they hold no personal data), "+
		"or set mask.strict: false", config.DefaultPath)
}
