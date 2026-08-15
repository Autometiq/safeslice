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
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/spf13/cobra"

	"github.com/Autometiq/safeslice/internal/catalog"
	"github.com/Autometiq/safeslice/internal/config"
	"github.com/Autometiq/safeslice/internal/extract"
	"github.com/Autometiq/safeslice/internal/keyset"
	"github.com/Autometiq/safeslice/internal/load"
	"github.com/Autometiq/safeslice/internal/sink"
	"github.com/Autometiq/safeslice/internal/ui"
)

func runCmd() *cobra.Command {
	var (
		from       string
		to         string
		out        string
		table      string
		where      string
		limit      int
		childDepth int
		childLimit int
		keysPath   string
		seed       string
	)
	cmd := &cobra.Command{
		Use:     "run",
		Aliases: []string{"pull"},
		Short:   "Extract a masked slice and load it into a target database",
		Long: `run reads the slice from the source, masks personal data in transit, and
either loads it into a target database or writes SQL to a file.

The source is opened read-only inside a REPEATABLE READ transaction, so nothing
is written to it and every table is read from the same consistent snapshot.`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := config.Load(flagConfig)
			if err != nil {
				return err
			}
			applyOverrides(cfg, flagSchemas, table, where, limit, childDepth, cmd)
			if cmd.Flags().Changed("seed") {
				cfg.Mask.Seed = seed
			}
			dsn, err := sourceDSN(from)
			if err != nil {
				return err
			}
			if to == "" && out == "" {
				return ui.HintCmd(
					errors.New("nothing to do: no destination given"),
					"Load into a database with --to, or write SQL to a file with --out.",
					`safeslice run --to "postgres://localhost/myapp_dev"`)
			}
			if to != "" {
				if err := refuseSameDatabase(dsn, to); err != nil {
					return err
				}
			}
			if cfg.Slice.Root == "" {
				return ui.HintCmd(
					errors.New("no root table: the slice needs somewhere to start"),
					"Generate a config that picks one for you, or pass --table.",
					"safeslice init")
			}
			return doRun(cmd.Context(), cfg, dsn, to, out, keysPath, childLimit)
		},
	}
	f := cmd.Flags()
	f.StringVar(&from, "from", "", "source connection string (or SAFESLICE_SOURCE / DATABASE_URL)")
	f.StringVar(&to, "to", "", "target connection string to load into")
	f.StringVar(&out, "out", "", "write SQL to this file instead of loading ('-' for stdout)")
	f.StringVar(&table, "table", "", "root table to slice from")
	f.StringVar(&where, "where", "", "SQL predicate applied to the root table")
	f.IntVar(&limit, "limit", 0, "maximum root rows")
	f.IntVar(&childDepth, "child-depth", 0, "levels of child rows to follow")
	f.IntVar(&childLimit, "child-limit", 0, "cap on child rows pulled per batch (default 100000)")
	f.StringVar(&keysPath, "keys", "", "key-set file to keep (enables resume); default is a temp file")
	f.StringVar(&seed, "seed", "", "masking seed; the same seed always produces the same fakes")
	return cmd
}

func doRun(ctx context.Context, cfg *config.Config, dsn, to, out, keysPath string, childLimit int) error {
	started := time.Now()
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
	root := cfg.Root()
	if _, ok := cat.Table(root); !ok {
		return fmt.Errorf("root table %s not found in schemas %s",
			root, strings.Join(cfg.Source.Schemas, ", "))
	}

	// Fail closed before touching any data. Finding an unclassified column after
	// half the slice is on disk is too late to be useful.
	if v := strictViolations(cat, cfg); len(v) > 0 {
		for _, c := range v {
			ui.Detail("%s", c)
		}
		return ui.Hint(
			fmt.Errorf("strict mode: %d text columns hold unreviewed text", len(v)),
			"Give each one a rule in "+config.DefaultPath+". Use `keep` if it holds no "+
				"personal data, `redact` to drop it, or `secret` to replace it.")
	}

	virtual := cfg.FKs()
	allFKs := append(append([]catalog.FK{}, cat.FKs...), virtual...)
	plan, err := load.PlanCycles(cat, cat.Refs(), allFKs)
	if err != nil {
		return err
	}

	keys, cleanup, err := openKeys(keysPath)
	if err != nil {
		return err
	}
	defer cleanup()

	total, tables := 0, 0
	var live *ui.Live
	extractStart := time.Now()
	opt := extract.Options{
		Root:       root,
		Where:      cfg.Slice.Where,
		Limit:      cfg.Slice.Limit,
		ChildDepth: cfg.Slice.ChildDepth,
		ChildLimit: childLimit,
		Seed:       cfg.Mask.Seed,
		Classifier: cfg.Classifier(),
		Discovered: func(ref catalog.Ref, keys, tbls int) {
			live.Detail("%s rows across %d tables", ui.Count(keys), tbls)
		},
		Extracting: func(ref catalog.Ref, rows int) {
			live.Label("extracting %s", ref)
			live.Detail("%s rows  %s", ui.Count(total+rows),
				ui.Rate(total+rows, time.Since(extractStart)))
		},
		Progress: func(ref catalog.Ref, rows int) {
			total += rows
			tables++
			ui.Step(rows, ref.String())
		},
	}
	ex, err := extract.Begin(ctx, conn, cat, virtual, keys, opt)
	if err != nil {
		return err
	}
	defer ex.Close(context.Background())

	live = ui.Start("walking the foreign-key graph")
	if err := ex.Collect(ctx); err != nil {
		live.Stop()
		return err
	}
	live.Success("foreign-key closure complete in %s", ui.Duration(live.Elapsed()))

	ui.Section("Extract")
	live = ui.Start("extracting")
	extractStart = time.Now()

	dest, closeDest, err := openSink(ctx, cat, plan, to, out)
	if err != nil {
		return err
	}
	if err := ex.Stream(ctx, dest, plan); err != nil {
		live.Stop()
		closeDest(false)
		return hintLoadFailure(err)
	}
	live.Label("committing")
	live.Detail("")
	if err := closeDest(true); err != nil {
		live.Stop()
		return err
	}
	live.Stop()

	stats := ui.RunStats{
		Tables:        tables,
		Rows:          total,
		MaskedColumns: maskedColumns(cat, cfg),
		Duration:      time.Since(started),
		Warnings:      warnings(),
	}
	if to != "" {
		stats.Target = describe(to)
	} else if out != "-" {
		stats.OutFile = out
	}
	if !flagQuiet {
		ui.Summary(stats)
		if to != "" {
			ui.NextStep("safeslice verify --target %q", to)
		}
	}
	return nil
}

// hintLoadFailure turns the two failures users actually hit into instructions.
//
// safeslice loads data, not schema. Pointed at a freshly created database it
// fails with "relation \"users\" does not exist", which is accurate and tells
// the reader nothing about running their migrations first.
func hintLoadFailure(err error) error {
	msg := err.Error()
	switch {
	case strings.Contains(msg, "42P01"), strings.Contains(msg, "does not exist"):
		return ui.HintCmd(err,
			"The target has no schema. safeslice copies rows, not table definitions, "+
				"so create them with your usual migrations first.",
			"createdb myapp_dev && <your migration command>")
	case strings.Contains(msg, "permission denied"):
		return ui.Hint(err,
			"The target role cannot write to these tables. Loading needs INSERT, and "+
				"disabling triggers during the load needs table ownership.")
	}
	return err
}

// warnings is set by the sink so skipped optional steps -- most often disabling
// triggers without table ownership -- reach the summary instead of vanishing.
var warnings = func() []string { return nil }

// openKeys returns the key store plus a cleanup function. A temp file is used
// unless the caller asked to keep one, so a large closure never sits in memory
// but also never litters the working directory.
func openKeys(path string) (*keyset.Store, func(), error) {
	temp := path == ""
	if temp {
		path = filepath.Join(os.TempDir(), fmt.Sprintf("safeslice-keys-%d.db", os.Getpid()))
	}
	store, err := keyset.Open(path)
	if err != nil {
		return nil, func() {}, err
	}
	return store, func() {
		store.Close()
		if temp {
			os.Remove(path)
		}
	}, nil
}

// openSink returns the destination and a closer taking whether the run
// succeeded, so a failed load is rolled back rather than left half-applied.
func openSink(ctx context.Context, cat *catalog.Catalog, plan load.CyclePlan, to, out string) (
	extract.Sink, func(ok bool) error, error) {

	if to != "" {
		target, err := pgx.Connect(ctx, to)
		if err != nil {
			return nil, nil, fmt.Errorf("connecting to target: %w", err)
		}
		s, err := sink.NewDB(ctx, target, cat, plan)
		if err != nil {
			target.Close(ctx)
			return nil, nil, err
		}
		warnings = func() []string { return s.Warnings }
		return s, func(ok bool) error {
			defer target.Close(context.Background())
			if !ok {
				s.Rollback(context.Background())
				return nil
			}
			return s.Close(ctx)
		}, nil
	}

	w := os.Stdout
	if out != "-" {
		f, err := os.Create(out)
		if err != nil {
			return nil, nil, fmt.Errorf("creating %s: %w", out, err)
		}
		w = f
	}
	s := sink.NewSQL(w, cat, plan)
	return s, func(ok bool) error {
		defer func() {
			if w != os.Stdout {
				w.Close()
			}
		}()
		if !ok {
			return nil
		}
		return s.Close(ctx)
	}, nil
}

// refuseSameDatabase blocks the accident that would end this project's
// reputation: pointing --to at production and overwriting it with masked data.
//
// Host, port and database name are compared after parsing, so an alias like
// `localhost` versus `127.0.0.1` is not treated as different when it resolves
// to the same target. Different ports are genuinely different databases and are
// allowed through.
func refuseSameDatabase(source, target string) error {
	s, err := pgx.ParseConfig(source)
	if err != nil {
		return fmt.Errorf("parsing source connection string: %w", err)
	}
	t, err := pgx.ParseConfig(target)
	if err != nil {
		return fmt.Errorf("parsing target connection string: %w", err)
	}
	if normaliseHost(s.Host) == normaliseHost(t.Host) && s.Port == t.Port && s.Database == t.Database {
		return fmt.Errorf("refusing to run: --to points at the same database as --from (%s); "+
			"that would overwrite the source with masked data", describe(target))
	}
	return nil
}

func normaliseHost(h string) string {
	switch h {
	case "127.0.0.1", "::1", "localhost":
		return "localhost"
	default:
		return h
	}
}
