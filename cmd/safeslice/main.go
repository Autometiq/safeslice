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

// Command safeslice pulls a small, PII-scrubbed, referentially-intact slice of
// a PostgreSQL database.
package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"runtime/debug"
	"syscall"

	"github.com/jackc/pgx/v5"
	"github.com/spf13/cobra"

	"github.com/Autometiq/safeslice/internal/ui"
)

// version is overridden at build time: -ldflags "-X main.version=v1.2.3"
var version = "dev"

var (
	flagConfig  string
	flagSchemas []string
	flagNoColor bool
	flagQuiet   bool
	flagNoOpen  bool
	flagVerbose bool
)

func main() {
	// A Ctrl-C during a spinner would otherwise leave the terminal with the
	// cursor hidden until the user runs `reset`.
	defer ui.RestoreCursor()
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-sig
		ui.RestoreCursor()
		os.Exit(130)
	}()

	if err := rootCmd().Execute(); err != nil {
		ui.RestoreCursor()
		ui.Fatal(err)
		os.Exit(1)
	}
}

// resolveVersion reports the build version. Binaries built by goreleaser carry
// it in ldflags; `go install` builds do not, and reporting "dev" makes every bug
// report untraceable. The module's own build info fills that gap.
func resolveVersion() string {
	if version != "dev" {
		return version
	}
	info, ok := debug.ReadBuildInfo()
	if !ok || info.Main.Version == "" || info.Main.Version == "(devel)" {
		return version
	}
	return info.Main.Version
}

func rootCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "safeslice",
		Short: ui.Mission,
		Long: `safeslice copies a subset of a production database to a local one.

Every selected row brings its parent rows with it, so the slice restores without
foreign-key violations. Personal data is replaced with deterministic fakes on
the way out, so nothing identifying reaches a developer laptop.

by ` + ui.Vendor + ` • ` + ui.VendorURL,
		Version:       resolveVersion(),
		SilenceUsage:  true,
		SilenceErrors: true,
		Args:          cobra.NoArgs,
		// No arguments opens the wizard. Printing usage to someone who has just
		// installed the tool tells them what exists, not what to do.
		RunE: func(cmd *cobra.Command, _ []string) error { return runWizard(cmd.Context()) },
		PersistentPreRun: func(cmd *cobra.Command, _ []string) {
			ui.SetPlain(flagNoColor)
			// The banner is chrome. It has no place in generated shell
			// completion scripts, and no place when the user asked for quiet.
			if flagQuiet || isCompletion(cmd) || cmd.Name() == "safeslice" || cmd.Name() == "wizard" {
				return // the wizard draws its own splash
			}
			ui.Banner(resolveVersion())
		},
	}
	pf := cmd.PersistentFlags()
	pf.StringVarP(&flagConfig, "config", "c", "", "config file (default: ./safeslice.yaml if present)")
	pf.StringSliceVar(&flagSchemas, "schema", nil, "schema to include (repeatable; overrides config)")
	pf.BoolVar(&flagNoColor, "no-color", false, "disable colour and box drawing")
	pf.BoolVarP(&flagQuiet, "quiet", "q", false, "suppress the banner and footer")
	pf.BoolVar(&flagNoOpen, "no-open", false, "never open the generated report in a browser")
	pf.BoolVarP(&flagVerbose, "verbose", "v", false,
		"show the full log instead of the wizard's progress display")

	cmd.AddCommand(initCmd(), planCmd(), runCmd(), verifyCmd(), demoCmd(),
		wizardCmd(), reportCmd(), connectionsCmd(), profilesCmd())
	return cmd
}

// wizardCmd names the default behaviour, so it can be found in --help and run
// explicitly from a script that has other arguments.
func wizardCmd() *cobra.Command {
	return &cobra.Command{
		Use:     "wizard",
		Aliases: []string{"menu"},
		Short:   "Guided walkthrough (also what runs when safeslice is given no arguments)",
		Args:    cobra.NoArgs,
		RunE:    func(cmd *cobra.Command, _ []string) error { return runWizard(cmd.Context()) },
	}
}

func isCompletion(cmd *cobra.Command) bool {
	for c := cmd; c != nil; c = c.Parent() {
		if c.Name() == "completion" {
			return true
		}
	}
	return false
}

// sourceDSN resolves the source connection string from the flag or, failing
// that, the usual environment variables.
func sourceDSN(flag string) (string, error) {
	for _, v := range []string{flag, os.Getenv("SAFESLICE_SOURCE"), os.Getenv("DATABASE_URL")} {
		if v != "" {
			return v, nil
		}
	}
	return "", errors.New("no source database given: pass --from, or set SAFESLICE_SOURCE or DATABASE_URL")
}

// connectSource opens the production connection.
//
// The session is made read-only before anything else runs. safeslice points at
// production by definition, and a read-only session means no bug in this tool --
// or in a predicate a user pasted into --where -- can write to it.
func connectSource(ctx context.Context, dsn string) (*pgx.Conn, error) {
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		return nil, fmt.Errorf("connecting to source: %w", err)
	}
	if _, err := conn.Exec(ctx, "SET default_transaction_read_only = on"); err != nil {
		conn.Close(ctx)
		return nil, fmt.Errorf("making the source session read-only: %w", err)
	}
	return conn, nil
}

// describe renders a DSN without its password, for logs and error messages.
// Connection strings routinely carry production credentials, and printing one
// into a CI log is its own incident.
func describe(dsn string) string {
	cfg, err := pgx.ParseConfig(dsn)
	if err != nil {
		return "(unparseable connection string)"
	}
	return fmt.Sprintf("%s@%s:%d/%s", cfg.User, cfg.Host, cfg.Port, cfg.Database)
}
