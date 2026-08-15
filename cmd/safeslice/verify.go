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

	"github.com/jackc/pgx/v5"
	"github.com/spf13/cobra"

	"github.com/Autometiq/safeslice/internal/catalog"
	"github.com/Autometiq/safeslice/internal/ui"
	"github.com/Autometiq/safeslice/internal/verify"
)

func verifyCmd() *cobra.Command {
	var (
		target string
		sample int
		ignore []string
	)
	cmd := &cobra.Command{
		Use:   "verify",
		Short: "Scan a database for values that still look like real personal data",
		Long: `verify checks a database -- normally the one you just loaded a slice into
-- for live email addresses, phone numbers, IP addresses and Luhn-valid payment
card numbers.

It exits non-zero when it finds anything, so it can gate a CI job or serve as
evidence for a compliance review.`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			ctx := cmd.Context()
			if target == "" {
				return fmt.Errorf("no database given: pass --target")
			}
			conn, err := pgx.Connect(ctx, target)
			if err != nil {
				return fmt.Errorf("connecting to %s: %w", describe(target), err)
			}
			defer conn.Close(context.Background())

			schemas := flagSchemas
			if len(schemas) == 0 {
				schemas = []string{"public"}
			}
			cat, err := catalog.Load(ctx, conn, schemas)
			if err != nil {
				return err
			}
			skip := map[string]bool{}
			for _, s := range ignore {
				skip[s] = true
			}
			ui.Info("scanning %s", describe(target))
			findings, err := verify.Scan(ctx, conn, cat, verify.Options{Sample: sample, Ignore: skip})
			if err != nil {
				return err
			}
			if len(findings) == 0 {
				ui.Success("no personal data found")
				if !flagQuiet {
					ui.Footer()
				}
				return nil
			}
			ui.Section("Findings")
			var rows [][]string
			for _, f := range findings {
				rows = append(rows, []string{f.Table.Name, f.Column, f.Kind,
					fmt.Sprint(f.Matches), f.Sample})
			}
			ui.Table([]string{"TABLE", "COLUMN", "LOOKS LIKE", "ROWS", "SAMPLE"}, rows)
			return ui.Hint(
				fmt.Errorf("found personal data in %d columns", len(findings)),
				"Add a rule for each in safeslice.yaml, then run again. If a match is a "+
					"false positive, exclude it with --ignore table.column.")
		},
	}
	f := cmd.Flags()
	f.StringVar(&target, "target", "", "database to scan")
	f.IntVar(&sample, "sample", 1000, "rows to examine per column")
	f.StringSliceVar(&ignore, "ignore", nil, "columns to skip, as table.column (repeatable)")
	return cmd
}
