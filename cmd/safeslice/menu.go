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

	"github.com/spf13/cobra"

	"github.com/Autometiq/safeslice/internal/config"
	"github.com/Autometiq/safeslice/internal/demo"
	"github.com/Autometiq/safeslice/internal/ui"
)

// Running `safeslice` with no arguments opens a guided menu rather than
// printing usage.
//
// The commands are the product and stay the interface for anyone scripting it,
// but a first-time user faced with five commands and a config file has to read
// documentation before anything happens. The menu carries them through one
// working slice, and every step prints the command it just ran so they leave
// knowing how to do it without the menu.

func menuCmd() *cobra.Command {
	return &cobra.Command{
		Use:    "menu",
		Short:  "Guided walkthrough (also what runs when safeslice is given no arguments)",
		Hidden: true,
		RunE:   func(cmd *cobra.Command, _ []string) error { return runMenu(cmd.Context()) },
	}
}

func runMenu(ctx context.Context) error {
	ui.Splash(resolveVersion())

	for {
		// Two paths only. `--help` already lists the commands, and a menu entry
		// that just prints help is a step between the user and a working slice.
		choice := ui.Menu("What would you like to do?", []ui.Choice{
			{Key: "1", Label: "Try it with demo data", Note: "throwaway database — nothing of yours is touched"},
			{Key: "2", Label: "Slice my own database", Note: "pull a masked slice of a Postgres you already have"},
			{Key: "q", Label: "Quit", Note: ""},
		})

		switch choice {
		case "1":
			if err := demoFlow(ctx); err != nil {
				ui.Fatal(err)
			}
		case "2":
			if err := ownFlow(ctx); err != nil {
				ui.Fatal(err)
			}
		case "q", "":
			return nil
		}

		if !ui.Confirm("\nBack to the menu?") {
			return nil
		}
	}
}

// demoFlow runs a complete slice against a database safeslice creates itself.
func demoFlow(ctx context.Context) error {
	ui.Stage(1, 4, "Starting the demo database")

	if err := demo.DockerAvailable(ctx); err != nil {
		return ui.HintCmd(err,
			"The demo runs PostgreSQL in a container. Install Docker Desktop and "+
				"start it, or choose option 2 to use a database you already have.",
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
	if err := doRun(ctx, cfg, demo.SourceDSN(), demo.TargetDSN(), "", "", 0); err != nil {
		return err
	}

	ui.Stage(4, 4, "Proving nothing leaked")
	ui.Detail("$ safeslice verify --target %s", demo.TargetDSN())
	if err := doVerify(ctx, demo.TargetDSN(), 1000, nil); err != nil {
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
	if err := writeInitConfig(ctx, demo.SourceDSN(), config.DefaultPath, []string{"public"}, "", true); err != nil {
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

// ownFlow points the same walkthrough at a database the user already has.
func ownFlow(ctx context.Context) error {
	ui.Stage(1, 3, "Connecting")
	ui.Detail("Use a read replica if you have one. safeslice opens the source")
	ui.Detail("read-only and never writes to it.")

	src := ui.Ask("\nSource connection string:", os.Getenv("SAFESLICE_SOURCE"))
	if src == "" {
		return ui.Hint(fmt.Errorf("no source given"),
			"A connection string looks like postgres://user:password@host:5432/dbname")
	}

	ui.Stage(2, 3, "Reading the schema")
	ui.Detail("$ safeslice init --from %s", describe(src))
	if err := writeInitConfig(ctx, src, config.DefaultPath, []string{"public"}, "", true); err != nil {
		return err
	}

	ui.Success("wrote %s", config.DefaultPath)
	ui.Detail("Open it and decide what each flagged column holds before running a slice.")

	ui.Stage(3, 3, "Next")
	ui.Detail("Nothing has been read from your tables yet — only the schema.")
	ui.NextStep("safeslice plan")
	ui.Detail("then, once the config looks right:")
	ui.NextStep(`safeslice run --to "postgres://localhost/myapp_dev"`)
	return nil
}

// showCommands lists the CLI equivalents, printed once a guided run finishes so
// the user leaves knowing how to repeat it without the menu.
func showCommands() {
	ui.Section("The same thing without the menu")
	ui.Table([]string{"COMMAND", "PURPOSE"}, [][]string{
		{"safeslice init", "read the schema, generate safeslice.yaml"},
		{"safeslice plan", "show what a run would do; reads no table data"},
		{"safeslice run", "extract, mask, and load into a target"},
		{"safeslice verify", "audit a database for surviving personal data"},
		{"safeslice demo", "start or stop the throwaway demo database"},
	})
	ui.Detail("")
	ui.Detail("Every command takes --from, or reads SAFESLICE_SOURCE from the environment.")
	ui.NextStep("safeslice <command> --help")
}
