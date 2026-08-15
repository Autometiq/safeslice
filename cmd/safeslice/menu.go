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
	"os"

	"github.com/Autometiq/safeslice/internal/config"
	"github.com/Autometiq/safeslice/internal/demo"
	"github.com/Autometiq/safeslice/internal/report"
	"github.com/Autometiq/safeslice/internal/ui"
)

// The demo: a complete run against a database safeslice creates itself.
//
// It exists because the fastest way to trust a tool that touches production is
// to watch it work on something that is not production. Every step prints the
// command it just ran, so the walkthrough teaches the CLI rather than replacing
// it.

// demoFlow runs a complete slice against a database safeslice creates itself.
func demoFlow(ctx context.Context) error {
	ui.Stage(1, 4, "Starting the demo database")

	if err := demo.DockerAvailable(ctx); err != nil {
		return ui.HintCmd(err,
			"The demo runs PostgreSQL in a container. Install Docker Desktop and "+
				"start it, or point the wizard at a database you already have.",
			"https://docs.docker.com/get-started/get-docker/")
	}

	live := ui.Start("preparing the demo database")
	err := demo.Start(ctx, func(msg string) { live.Detail("%s", msg) })
	if err == nil {
		// Makes the walkthrough repeatable: a second run would otherwise hit a
		// duplicate key from the first.
		live.Detail("resetting the target database")
		err = demo.ResetTarget(ctx)
	}
	live.Stop()
	if err != nil {
		return err
	}

	counts, _ := demo.Counts(ctx)
	ui.Success("demo database ready")
	if counts != "" {
		ui.Detail("%s, with real-looking emails, phone numbers and card numbers", counts)
	}

	// A working directory of its own, so the generated config never lands on top
	// of one the user already has.
	dir := "safeslice-demo"
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("creating %s: %w", dir, err)
	}
	if err := os.Chdir(dir); err != nil {
		return fmt.Errorf("entering %s: %w", dir, err)
	}
	defer os.Chdir("..") //nolint:errcheck

	ui.Stage(2, 4, "Reading the schema and classifying columns")
	ui.Detail("$ safeslice init --from %s", demo.SourceDSN())
	cfg, err := demoConfig(ctx)
	if err != nil {
		return err
	}

	ui.Stage(3, 4, "Slicing and masking")
	ui.Detail("$ safeslice run --to %s", demo.TargetDSN())
	// Reporting runs verification itself and writes the artifacts, so the
	// walkthrough does not ask the user to run anything afterwards.
	ui.Stage(4, 4, "Verifying and writing the report")
	if err := doRunReport(ctx, cfg, demo.SourceDSN(), demo.TargetDSN(), "", "", 0,
		report.DefaultDir); err != nil {
		return err
	}

	ui.Success("that is the whole workflow")
	ui.Detail("The config it generated is in ./%s/safeslice.yaml — open it to see", dir)
	ui.Detail("which columns were classified automatically and which were left to you.")
	showCommands()
	ui.NextStep("safeslice demo stop   # remove the demo database when you are done")
	return nil
}

// demoConfig generates a config for the demo and pre-answers the review, since
// the point here is to show the workflow rather than to teach the YAML.
func demoConfig(ctx context.Context) (*config.Config, error) {
	if _, err := writeInitConfig(ctx, demo.SourceDSN(), config.DefaultPath, []string{"public"}, "", true); err != nil {
		return nil, err
	}
	cfg, err := config.Load(config.DefaultPath)
	if err != nil {
		return nil, err
	}
	// notes.body is free text holding names, phone numbers and card digits. The
	// classifier cannot know that, which is exactly the decision the tool asks a
	// real user to make; the demo makes it so the run is clean end to end.
	if cfg.Mask.Rules == nil {
		cfg.Mask.Rules = map[string]string{}
	}
	cfg.Mask.Rules["notes.body"] = "redact"
	cfg.Mask.Rules["payments.card_number"] = "govid"
	cfg.Mask.Rules["payments.billing_address"] = "address"
	ui.Detail("classified %d columns; notes.body redacted because it is free text",
		maskedColumnsForDemo(cfg))
	return cfg, nil
}

func maskedColumnsForDemo(cfg *config.Config) int { return len(cfg.Mask.Rules) }

// showCommands lists the CLI equivalents, printed once a guided run finishes so
// the user leaves knowing how to repeat it without the wizard.
func showCommands() {
	ui.Section("The same thing without the wizard")
	ui.Table([]string{"COMMAND", "PURPOSE"}, [][]string{
		{"safeslice init", "read the schema, generate safeslice.yaml"},
		{"safeslice plan", "show what a run would do; reads no table data"},
		{"safeslice run", "extract, mask, and load into a target"},
		{"safeslice verify", "audit a database for surviving personal data"},
		{"safeslice report", "show and open the last run's report"},
		{"safeslice demo", "start or stop the throwaway demo database"},
		{"safeslice profiles", "list saved profiles and past runs"},
		{"safeslice connections", "list saved connections"},
	})
	ui.Detail("")
	ui.Detail("Every command takes --from, or reads SAFESLICE_SOURCE from the environment.")
	ui.Detail("")
	ui.Detail("For CI, the two that matter are:")
	ui.Detail("  safeslice run --config safeslice.yaml --to \"$DATABASE_URL\"")
	ui.Detail("  safeslice verify --target \"$DATABASE_URL\"")
	ui.NextStep("safeslice <command> --help")
}
