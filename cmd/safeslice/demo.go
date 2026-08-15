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
	"github.com/spf13/cobra"

	"github.com/Autometiq/safeslice/internal/demo"
	"github.com/Autometiq/safeslice/internal/ui"
)

func demoCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "demo",
		Short: "Start or stop a throwaway database to try safeslice against",
		Long: `demo runs a PostgreSQL container holding a small SaaS schema seeded with
fabricated customers -- routable-looking emails, international phone numbers and
Luhn-valid card numbers.

Nothing of yours is touched. It listens on port 55433 so it cannot collide with
a Postgres you already run.`,
	}

	start := &cobra.Command{
		Use:   "start",
		Short: "Create and seed the demo database",
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx := cmd.Context()
			if err := demo.DockerAvailable(ctx); err != nil {
				return ui.HintCmd(err,
					"The demo runs PostgreSQL in a container. Install Docker Desktop and start it.",
					"https://docs.docker.com/get-started/get-docker/")
			}
			live := ui.Start("preparing the demo database")
			err := demo.Start(ctx, func(m string) { live.Detail("%s", m) })
			live.Stop()
			if err != nil {
				return err
			}
			counts, _ := demo.Counts(ctx)
			ui.Success("demo database ready")
			if counts != "" {
				ui.Detail("%s", counts)
			}
			ui.Detail("source  %s", demo.SourceDSN())
			ui.Detail("target  %s", demo.TargetDSN())
			ui.NextStep("safeslice init --from %q", demo.SourceDSN())
			return nil
		},
	}

	stop := &cobra.Command{
		Use:   "stop",
		Short: "Remove the demo database and its data",
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := demo.Stop(cmd.Context()); err != nil {
				return err
			}
			ui.Success("demo database removed")
			return nil
		},
	}

	cmd.AddCommand(start, stop)
	return cmd
}
