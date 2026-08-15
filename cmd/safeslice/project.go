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
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/spf13/cobra"

	"github.com/Autometiq/safeslice/internal/profile"
	"github.com/Autometiq/safeslice/internal/report"
	"github.com/Autometiq/safeslice/internal/ui"
)

// The three commands that read what the wizard remembered. Each one is a
// listing: they exist so the state on disk is inspectable without opening YAML
// in an editor, not to grow a second way of doing the work.

func reportCmd() *cobra.Command {
	var dir string
	cmd := &cobra.Command{
		Use:   "report",
		Short: "Show the report from the last run, and open it in a browser",
		Long: `report points at the artifacts a run left behind: the README, the offline
HTML report, the JSON summary, the CSV and the masking rules.

It reads what is already on disk. Nothing is regenerated, and no database is
contacted.`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) == 1 {
				dir = args[0]
			}
			return showReport(dir)
		},
	}
	cmd.Flags().StringVar(&dir, "dir", report.DefaultDir, "artifact directory")
	return cmd
}

func showReport(dir string) error {
	summary := filepath.Join(dir, "summary.json")
	data, err := os.ReadFile(summary)
	if os.IsNotExist(err) {
		return ui.HintCmd(fmt.Errorf("no report in %s", dir),
			"Artifacts are written by a run. Point --dir at another directory, or make one.",
			"safeslice")
	}
	if err != nil {
		return err
	}
	var res report.Result
	if err := json.Unmarshal(data, &res); err != nil {
		return fmt.Errorf("reading %s: %w", summary, err)
	}

	ui.Section("Last run")
	ui.Table([]string{"", ""}, [][]string{
		{"Generated", res.GeneratedAt.Format(time.RFC1123)},
		{"Source", res.Source.String()},
		{"Target", res.Target.String()},
		{"Rows", ui.Count(res.TotalRows)},
		{"Tables", fmt.Sprint(len(res.Tables))},
		{"Masked columns", fmt.Sprint(len(res.Rules))},
		{"Verification", verdict(res)},
	})
	if res.Verification.Ran {
		ui.Detail("%s", res.Verification.Caveat)
	}

	ui.Section("Connect")
	for _, s := range report.Snippets(res.Target) {
		ui.Detail("%-14s %s", s.Name, firstLine(s.Code))
	}

	ui.Section("Artifacts")
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		ui.Detail("%s", filepath.Join(dir, e.Name()))
	}
	html := filepath.Join(dir, "report.html")
	if flagNoOpen {
		ui.NextStep("open %s", html)
		return nil
	}
	if err := report.Open(html); err != nil {
		ui.NextStep("open %s", html)
	}
	return nil
}

func verdict(res report.Result) string {
	switch {
	case !res.Verification.Ran:
		return "did not run"
	case res.Verification.Passed:
		return "passed"
	default:
		return fmt.Sprintf("flagged %d columns", len(res.Verification.Findings))
	}
}

func connectionsCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "connections",
		Short: "List the connections saved by the wizard",
		Long: `connections lists what is in .safeslice/config.yaml.

Passwords are never stored there. A connection that needs one records the name
of an environment variable instead.`,
		Args: cobra.NoArgs,
		RunE: func(*cobra.Command, []string) error {
			store := profile.Open("")
			saved, err := store.Connections()
			if err != nil {
				return err
			}
			if len(saved) == 0 {
				return ui.HintCmd(errors.New("no saved connections"),
					"The wizard offers to save one at the end of a run.", "safeslice")
			}
			var rows [][]string
			for _, c := range saved {
				rows = append(rows, []string{c.Name, c.DSN, c.PasswordEnv})
			}
			ui.Section("Saved connections")
			ui.Table([]string{"NAME", "CONNECTION", "PASSWORD FROM"}, rows)
			ui.Detail("Stored in %s. No passwords are written there.", store.Path())
			return nil
		},
	}
}

func profilesCmd() *cobra.Command {
	var history bool
	cmd := &cobra.Command{
		Use:   "profiles",
		Short: "List the saved wizard profiles and past runs",
		Args:  cobra.NoArgs,
		RunE: func(*cobra.Command, []string) error {
			store := profile.Open("")
			if history {
				return showHistory(store)
			}
			profiles, err := store.Profiles()
			if err != nil {
				return err
			}
			if len(profiles) == 0 {
				return ui.HintCmd(errors.New("no saved profiles"),
					"The wizard offers to save one at the end of a run.", "safeslice")
			}
			var rows [][]string
			for _, p := range profiles {
				rows = append(rows, []string{p.Name, p.Source.DSN, p.Target.DSN,
					p.Root, ui.Count(p.Limit)})
			}
			ui.Section("Profiles")
			ui.Table([]string{"NAME", "SOURCE", "TARGET", "ROOT", "LIMIT"}, rows)
			ui.Detail("Stored in %s. Choose one at the start of a wizard run.", store.Path())
			return nil
		},
	}
	cmd.Flags().BoolVar(&history, "history", false, "show past runs instead")
	return cmd
}

func showHistory(store *profile.Store) error {
	runs, err := store.History()
	if err != nil {
		return err
	}
	if len(runs) == 0 {
		return ui.HintCmd(errors.New("no runs recorded"),
			"The wizard records every run it completes.", "safeslice")
	}
	var rows [][]string
	for _, r := range runs {
		rows = append(rows, []string{
			r.At.Local().Format("2006-01-02 15:04"),
			r.Target, ui.Count(r.Rows), fmt.Sprint(r.Tables),
			map[bool]string{true: "passed", false: "not verified"}[r.Verified],
		})
	}
	ui.Section("History")
	ui.Table([]string{"WHEN", "TARGET", "ROWS", "TABLES", "PRIVACY SCAN"}, rows)
	return nil
}
