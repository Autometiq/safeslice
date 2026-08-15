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
	"github.com/Autometiq/safeslice/internal/graph"
	"github.com/Autometiq/safeslice/internal/keyset"
	"github.com/Autometiq/safeslice/internal/load"
	"github.com/Autometiq/safeslice/internal/report"
	"github.com/Autometiq/safeslice/internal/sink"
	"github.com/Autometiq/safeslice/internal/ui"
)

func runCmd() *cobra.Command {
	var (
		from        string
		to          string
		out         string
		table       string
		where       string
		limit       int
		childDepth  int
		childLimit  int
		keysPath    string
		seed        string
		reportDir   string
		noReport    bool
		interactive bool
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
			if interactive {
				return createFlow(cmd.Context())
			}
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
			dir := reportDir
			if noReport {
				dir = ""
			}
			return doRunReport(cmd.Context(), cfg, dsn, to, out, keysPath, childLimit, dir)
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
	f.StringVar(&reportDir, "report", report.DefaultDir, "directory for the generated README, HTML report and summaries")
	f.BoolVar(&noReport, "no-report", false, "skip generating artifacts")
	f.BoolVarP(&interactive, "interactive", "i", false,
		"ask for anything missing instead of failing (same as running `safeslice`)")
	return cmd
}

// runStage names a point in the pipeline. The wizard maps these onto its
// progress checklist; the CLI ignores them and prints its own output.
type runStage int

const (
	stageConnected runStage = iota
	stageSchema
	stageRelationships
	stageSelected
	stageExtracted
	stageLoaded
	stageVerified
	stageReported
)

// runHooks lets the wizard drive the same pipeline the CLI uses, rather than
// growing a second implementation of it. Every field is optional, and a nil
// runHooks leaves the CLI's behaviour exactly as it was.
type runHooks struct {
	// stage reports progress. When set, the pipeline prints no chrome of its
	// own: the caller owns the display.
	stage func(runStage)
	// detail reports a line of progress within the current stage.
	detail func(string)
	// rows reports streaming progress against the row count the walk selected,
	// which is what makes a progress bar honest rather than decorative.
	rows func(done, total int)
	// confirm runs after the foreign-key walk and before any row is read or
	// written, with the exact row count per table. Returning an error aborts
	// the run with nothing written to the target.
	confirm func(counts map[string]int, total int) error
	// verbose keeps the pipeline's own output even though a caller is
	// listening: --verbose in the wizard, where the tidy checklist is exactly
	// what you do not want while debugging.
	verbose bool
}

// quiet reports whether the caller has taken over the display.
func (h *runHooks) quiet() bool { return h != nil && !h.verbose }

func (h *runHooks) at(s runStage) {
	if h != nil && h.stage != nil {
		h.stage(s)
	}
}

func (h *runHooks) say(format string, a ...any) {
	if h != nil && h.detail != nil {
		h.detail(fmt.Sprintf(format, a...))
	}
}

func (h *runHooks) moved(done, total int) {
	if h != nil && h.rows != nil {
		h.rows(done, total)
	}
}

// runRequest is one slice, start to finish.
type runRequest struct {
	cfg        *config.Config
	source     string
	target     string
	outFile    string
	keysPath   string
	childLimit int
	reportDir  string
	hooks      *runHooks
}

func doRunReport(ctx context.Context, cfg *config.Config, dsn, to, out, keysPath string, childLimit int, reportDir string) error {
	_, err := execute(ctx, runRequest{
		cfg: cfg, source: dsn, target: to, outFile: out,
		keysPath: keysPath, childLimit: childLimit, reportDir: reportDir,
	})
	return err
}

// execute runs the slice and returns what it produced, so a caller that wants
// to render its own summary does not have to recompute any of it.
func execute(ctx context.Context, r runRequest) (report.Result, error) {
	cfg, h := r.cfg, r.hooks
	var res report.Result

	started := time.Now()
	conn, err := connectSource(ctx, r.source)
	if err != nil {
		return res, err
	}
	defer conn.Close(context.Background())
	if !h.quiet() {
		ui.Info("source %s (read-only session)", describe(r.source))
	}
	h.at(stageConnected)

	cat, err := catalog.Load(ctx, conn, cfg.Source.Schemas)
	if err != nil {
		return res, err
	}
	h.at(stageSchema)
	root := cfg.Root()
	if _, ok := cat.Table(root); !ok {
		return res, fmt.Errorf("root table %s not found in schemas %s",
			root, strings.Join(cfg.Source.Schemas, ", "))
	}

	// Fail closed before touching any data. Finding an unclassified column after
	// half the slice is on disk is too late to be useful.
	if v := strictViolations(cat, cfg); len(v) > 0 {
		if !h.quiet() {
			for _, c := range v {
				ui.Detail("%s", c)
			}
		}
		return res, ui.Hint(
			fmt.Errorf("strict mode: %d text columns hold unreviewed text", len(v)),
			"Give each one a rule in "+config.DefaultPath+". Use `keep` if it holds no "+
				"personal data, `redact` to drop it, or `secret` to replace it.")
	}

	virtual := cfg.FKs()
	allFKs := append(append([]catalog.FK{}, cat.FKs...), virtual...)
	plan, err := load.PlanCycles(cat, cat.Refs(), allFKs)
	if err != nil {
		return res, err
	}
	h.at(stageRelationships)

	keys, cleanup, err := openKeys(r.keysPath)
	if err != nil {
		return res, err
	}
	defer cleanup()

	total, tables := 0, 0
	perTable := map[string]int{}
	planned := 0 // rows the walk selected, for the progress bar
	var live *ui.Live
	extractStart := time.Now()
	opt := extract.Options{
		Root:       root,
		Where:      cfg.Slice.Where,
		Limit:      cfg.Slice.Limit,
		ChildDepth: cfg.Slice.ChildDepth,
		ChildLimit: r.childLimit,
		Seed:       cfg.Mask.Seed,
		Classifier: cfg.Classifier(),
		Discovered: func(ref catalog.Ref, keys, tbls int) {
			live.Detail("%s rows across %d tables", ui.Count(keys), tbls)
			h.say("%s rows across %d tables", ui.Count(keys), tbls)
		},
		Extracting: func(ref catalog.Ref, rows int) {
			live.Label("extracting %s", ref)
			live.Detail("%s rows  %s", ui.Count(total+rows),
				ui.Rate(total+rows, time.Since(extractStart)))
			h.moved(total+rows, planned)
		},
		Progress: func(ref catalog.Ref, rows int) {
			total += rows
			tables++
			perTable[ref.String()] = rows
			if !h.quiet() {
				ui.Step(rows, ref.String())
			}
			h.moved(total, planned)
		},
	}
	ex, err := extract.Begin(ctx, conn, cat, virtual, keys, opt)
	if err != nil {
		return res, err
	}
	defer ex.Close(context.Background())

	if !h.quiet() {
		live = ui.Start("walking the foreign-key graph")
	}
	if err := ex.Collect(ctx); err != nil {
		live.Stop()
		return res, err
	}
	if !h.quiet() {
		live.Success("foreign-key closure complete in %s", ui.Duration(live.Elapsed()))
	}

	// Everything up to here is read-only catalog and key work. This is the last
	// point at which a caller can still call the whole thing off, and the first
	// at which the row counts are real rather than estimates.
	counts, err := keyCounts(keys)
	if err != nil {
		return res, err
	}
	for _, n := range counts {
		planned += n
	}
	h.at(stageSelected)
	if h != nil && h.confirm != nil {
		if err := h.confirm(counts, planned); err != nil {
			return res, err
		}
	}

	if !h.quiet() {
		ui.Section("Extract")
		live = ui.Start("extracting")
	}
	extractStart = time.Now()

	dest, closeDest, err := openSink(ctx, cat, plan, r.target, r.outFile)
	if err != nil {
		return res, err
	}
	if err := ex.Stream(ctx, dest, plan); err != nil {
		live.Stop()
		closeDest(false)
		return res, hintLoadFailure(err)
	}
	h.at(stageExtracted)
	live.Label("committing")
	live.Detail("")
	h.say("committing")
	if err := closeDest(true); err != nil {
		live.Stop()
		return res, err
	}
	live.Stop()
	h.at(stageLoaded)

	stats := ui.RunStats{
		Tables:        tables,
		Rows:          total,
		MaskedColumns: maskedColumns(cat, cfg),
		Duration:      time.Since(started),
		Warnings:      warnings(),
	}
	if r.target != "" {
		stats.Target = describe(r.target)
	} else if r.outFile != "-" {
		stats.OutFile = r.outFile
	}
	if !flagQuiet && !h.quiet() {
		ui.Summary(stats)
	}

	if r.reportDir == "" {
		if !flagQuiet && !h.quiet() && r.target != "" {
			ui.NextStep("safeslice verify --target %q", r.target)
		}
		h.at(stageVerified)
		h.at(stageReported)
		return res, nil
	}
	return writeArtifacts(ctx, artifactInput{
		cfg: cfg, cat: cat, source: r.source, target: r.target, outFile: r.outFile,
		perTable: perTable, total: total, duration: time.Since(started),
		warnings: warnings(), dir: r.reportDir, hooks: h, fks: allFKs,
	})
}

// keyCounts reports how many rows the walk selected per table.
func keyCounts(keys *keyset.Store) (map[string]int, error) {
	tables, err := keys.Tables()
	if err != nil {
		return nil, err
	}
	out := make(map[string]int, len(tables))
	for _, t := range tables {
		n, err := keys.Count(t)
		if err != nil {
			return nil, err
		}
		out[t] = n
	}
	return out, nil
}

type artifactInput struct {
	cfg      *config.Config
	cat      *catalog.Catalog
	source   string
	target   string
	outFile  string
	perTable map[string]int
	total    int
	duration time.Duration
	warnings []string
	dir      string
	fks      []catalog.FK
	hooks    *runHooks
}

// writeArtifacts verifies the loaded database and writes the README, HTML
// report, JSON summary, CSV and rules file.
//
// Verification runs here rather than being left to the user: a report that omits
// it would imply the slice was checked when it was not.
func writeArtifacts(ctx context.Context, in artifactInput) (report.Result, error) {
	h := in.hooks
	rules, redacted, unreviewed := maskingRules(in.cat, in.cfg)
	est := sourceRowEstimates(in.cat)

	var tables []report.Table
	for name, rows := range in.perTable {
		masked := 0
		for _, r := range rules {
			if strings.HasPrefix(r.Column, shortName(name)+".") {
				masked++
			}
		}
		tables = append(tables, report.Table{
			Name: name, SourceRows: est[name], ExtractedRows: rows, MaskedColumns: masked,
		})
	}

	res := report.Result{
		Version:       resolveVersion(),
		Duration:      in.duration,
		Source:        report.Redact(in.source),
		RootTable:     in.cfg.Root().String(),
		Where:         in.cfg.Slice.Where,
		ChildDepth:    in.cfg.Slice.ChildDepth,
		Seed:          in.cfg.Mask.Seed,
		Tables:        tables,
		TotalRows:     in.total,
		Rules:         rules,
		Redacted:      redacted,
		Unreviewed:    unreviewed,
		Warnings:      in.warnings,
		Relationships: describeFKs(in.fks),
		Soft:          describeSoft(graph.SoftKeys(in.cat, in.fks)),
	}
	if in.target != "" {
		res.Target = report.Redact(in.target)
	} else {
		res.Target = report.Endpoint{Database: in.outFile}
	}

	if in.target != "" {
		var live *ui.Live
		if !h.quiet() {
			live = ui.Start("verifying the loaded database")
		}
		findings, err := scanTarget(ctx, in.target)
		live.Stop()
		switch {
		case err != nil:
			res.Warnings = append(res.Warnings, "verification could not run: "+err.Error())
		default:
			res.Verification = report.Verification{Ran: true, Passed: len(findings) == 0, Findings: findings}
			if h.quiet() {
				break
			}
			if len(findings) == 0 {
				ui.Success("privacy scan found no personal data in the sampled rows")
			} else {
				ui.Warn("privacy scan flagged %d columns", len(findings))
				for _, f := range findings {
					ui.Detail("%s", f)
				}
			}
		}
	}
	h.at(stageVerified)

	paths, err := report.Write(in.dir, res)
	if err != nil {
		return res, err
	}
	res.Normalise() // the same fields the artifacts were rendered from
	h.at(stageReported)
	if !flagQuiet && !h.quiet() {
		// Absolute, always. A relative path is only meaningful from the
		// directory the run happened in, and the demo changes directory while
		// it works -- so `open safeslice-output/report.html` was a command that
		// failed for the person most likely to paste it.
		ui.Section("Artifacts")
		for _, p := range paths {
			ui.Detail("%s", absOr(p))
		}
		ui.NextStep("open %s", absOr(filepath.Join(in.dir, "report.html")))
	}
	return res, nil
}

// absOr resolves a path for display, falling back to what it was given. A
// path that cannot be resolved is still better printed than dropped.
func absOr(path string) string {
	if abs, err := filepath.Abs(path); err == nil {
		return abs
	}
	return path
}

// describeFKs renders the relationships a slice followed, for the artifacts.
func describeFKs(fks []catalog.FK) []string {
	out := make([]string, 0, len(fks))
	for _, fk := range fks {
		line := fmt.Sprintf("%s(%s) → %s(%s)", fk.Table.Name, strings.Join(fk.Columns, ", "),
			fk.RefTable.Name, strings.Join(fk.RefColumns, ", "))
		if fk.Virtual {
			line += "  [virtual]"
		}
		out = append(out, line)
	}
	return out
}

func describeSoft(soft []graph.Soft) []string {
	out := make([]string, 0, len(soft))
	for _, s := range soft {
		out = append(out, s.String())
	}
	return out
}

// shortName drops the schema qualifier, because masking rules are keyed on the
// bare table name.
func shortName(qualified string) string {
	if i := strings.IndexByte(qualified, '.'); i >= 0 {
		return qualified[i+1:]
	}
	return qualified
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
