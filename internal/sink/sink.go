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

// Package sink writes an extracted slice out: either as SQL text, or straight
// into a target database over the binary COPY protocol.
package sink

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/Autometiq/safeslice/internal/catalog"
	"github.com/Autometiq/safeslice/internal/extract"
	"github.com/Autometiq/safeslice/internal/load"
)

// Lit renders a Go value as a PostgreSQL literal.
func Lit(v any) string {
	switch t := v.(type) {
	case nil:
		return "NULL"
	case bool:
		if t {
			return "TRUE"
		}
		return "FALSE"
	case int64:
		return strconv.FormatInt(t, 10)
	case int32:
		return strconv.FormatInt(int64(t), 10)
	case int:
		return strconv.Itoa(t)
	case float64:
		return strconv.FormatFloat(t, 'g', -1, 64)
	case float32:
		return strconv.FormatFloat(float64(t), 'g', -1, 32)
	case string:
		return quote(t)
	case []byte:
		return "'\\x" + hex.EncodeToString(t) + "'::bytea"
	case [16]byte:
		u := hex.EncodeToString(t[:])
		return quote(u[0:8] + "-" + u[8:12] + "-" + u[12:16] + "-" + u[16:20] + "-" + u[20:32])
	case time.Time:
		return quote(t.Format(time.RFC3339Nano))
	case map[string]any, []any:
		b, err := json.Marshal(t)
		if err != nil {
			return "NULL"
		}
		return quote(string(b))
	default:
		return quote(fmt.Sprintf("%v", t))
	}
}

// quote escapes a string literal. standard_conforming_strings is on by default
// in every supported Postgres, so doubling the quote is sufficient and
// backslashes stay literal.
func quote(s string) string { return "'" + strings.ReplaceAll(s, "'", "''") + "'" }

// SQL writes the slice as a SQL script.
type SQL struct {
	w      io.Writer
	cat    *catalog.Catalog
	plan   load.CyclePlan
	tables []catalog.Ref
	err    error
	opened bool
}

func NewSQL(w io.Writer, cat *catalog.Catalog, plan load.CyclePlan) *SQL {
	return &SQL{w: w, cat: cat, plan: plan}
}

func (s *SQL) printf(format string, args ...any) {
	if s.err != nil {
		return
	}
	_, s.err = fmt.Fprintf(s.w, format, args...)
}

func (s *SQL) preamble() {
	if s.opened {
		return
	}
	s.opened = true
	s.printf("BEGIN;\n")
	if s.plan.Deferred {
		// Only meaningful for constraints declared DEFERRABLE; PlanCycles has
		// already confirmed that they all are.
		s.printf("SET CONSTRAINTS ALL DEFERRED;\n")
	}
}

func (s *SQL) Open(_ context.Context, t *catalog.Table, cols []string) (extract.RowWriter, error) {
	s.preamble()
	s.tables = append(s.tables, t.Ref)
	s.printf("\n-- %s\n", t.Ref)
	// Triggers are disabled per table rather than globally: an audit trigger
	// would otherwise overwrite the values that were just masked.
	s.printf("%s\n", load.DisableTriggers([]catalog.Ref{t.Ref})[0])
	return &sqlWriter{s: s, table: t, cols: cols}, s.err
}

func (s *SQL) Deferred(_ context.Context, b load.Break, rows [][]any) error {
	s.printf("\n-- restore %s.%s deferred during load\n", b.Table.Name, strings.Join(b.Columns, ", "))
	s.printf("%s\n", b.UpdatePrefix())
	for i, row := range rows {
		sep := ","
		if i == len(rows)-1 {
			sep = ""
		}
		s.printf("  (%s)%s\n", values(row), sep)
	}
	s.printf("%s\n", b.UpdateSuffix())
	return s.err
}

func (s *SQL) Close(_ context.Context) error {
	if s.err != nil {
		return s.err
	}
	s.preamble()
	for _, stmt := range load.EnableTriggers(s.tables) {
		s.printf("%s\n", stmt)
	}
	// Without this the application's next insert collides with a row that is
	// already in the slice.
	if resets := load.SequenceResets(s.cat, s.tables); len(resets) > 0 {
		s.printf("\n-- keep sequences ahead of the loaded rows\n")
		for _, stmt := range resets {
			s.printf("%s\n", stmt)
		}
	}
	s.printf("COMMIT;\n")
	return s.err
}

type sqlWriter struct {
	s      *SQL
	table  *catalog.Table
	cols   []string
	n      int
	closed bool
}

const sqlBatch = 500

func (w *sqlWriter) Write(_ context.Context, row []any) error {
	if w.n%sqlBatch == 0 {
		if w.n > 0 {
			w.s.printf(";\n")
		}
		w.s.printf("%s\n", load.InsertPrefix(w.table, w.cols))
	} else {
		w.s.printf(",\n")
	}
	w.s.printf("  (%s)", values(row))
	w.n++
	return w.s.err
}

func (w *sqlWriter) Close(_ context.Context) error {
	if w.closed {
		return w.s.err
	}
	w.closed = true
	if w.n > 0 {
		w.s.printf(";\n")
	}
	return w.s.err
}

func values(row []any) string {
	out := make([]string, len(row))
	for i, v := range row {
		out[i] = Lit(v)
	}
	return strings.Join(out, ", ")
}

// DB loads the slice straight into a target database using the binary COPY
// protocol, inside a single transaction so a failure leaves nothing behind.
type DB struct {
	tx     pgx.Tx
	cat    *catalog.Catalog
	plan   load.CyclePlan
	tables []catalog.Ref
	// Warnings records optional steps that were skipped, such as disabling
	// triggers without table ownership.
	Warnings []string
}

// try runs a statement that is allowed to fail, inside a savepoint.
//
// In PostgreSQL any failed statement aborts the whole transaction, so a
// best-effort ALTER cannot simply have its error ignored: everything after it
// fails with "current transaction is aborted" and the real cause is lost.
func (d *DB) try(ctx context.Context, stmt string) error {
	if _, err := d.tx.Exec(ctx, "SAVEPOINT ss_optional"); err != nil {
		return fmt.Errorf("sink: savepoint: %w", err)
	}
	if _, err := d.tx.Exec(ctx, stmt); err != nil {
		if _, rb := d.tx.Exec(ctx, "ROLLBACK TO SAVEPOINT ss_optional"); rb != nil {
			return fmt.Errorf("sink: rolling back optional step: %w", rb)
		}
		d.Warnings = append(d.Warnings, fmt.Sprintf("skipped %q: %v", stmt, err))
		return nil
	}
	if _, err := d.tx.Exec(ctx, "RELEASE SAVEPOINT ss_optional"); err != nil {
		return fmt.Errorf("sink: releasing savepoint: %w", err)
	}
	return nil
}

func NewDB(ctx context.Context, conn *pgx.Conn, cat *catalog.Catalog, plan load.CyclePlan) (*DB, error) {
	tx, err := conn.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("sink: starting the load transaction: %w", err)
	}
	if plan.Deferred {
		if _, err := tx.Exec(ctx, "SET CONSTRAINTS ALL DEFERRED"); err != nil {
			tx.Rollback(ctx)
			return nil, fmt.Errorf("sink: deferring constraints: %w", err)
		}
	}
	return &DB{tx: tx, cat: cat, plan: plan}, nil
}

func (d *DB) Open(ctx context.Context, t *catalog.Table, cols []string) (extract.RowWriter, error) {
	d.tables = append(d.tables, t.Ref)
	// Best effort: disabling triggers needs table ownership. A slice still
	// loads correctly without it, so a permission failure must not abort the
	// run -- but an audit trigger may then rewrite masked values, which is why
	// the skip is recorded rather than hidden.
	if err := d.try(ctx, load.DisableTriggers([]catalog.Ref{t.Ref})[0]); err != nil {
		return nil, err
	}
	w := &dbWriter{
		rows: make(chan []any, 256),
		done: make(chan error, 1),
		ref:  t.Ref,
	}
	go func() {
		_, err := d.tx.CopyFrom(ctx, pgx.Identifier{t.Ref.Schema, t.Ref.Name}, cols,
			pgx.CopyFromFunc(func() ([]any, error) {
				row, ok := <-w.rows
				if !ok {
					return nil, nil // nil row with nil error ends the copy
				}
				return row, nil
			}))
		w.done <- err
	}()
	return w, nil
}

func (d *DB) Deferred(ctx context.Context, b load.Break, rows [][]any) error {
	var sb strings.Builder
	sb.WriteString(b.UpdatePrefix())
	for i, row := range rows {
		if i > 0 {
			sb.WriteString(",")
		}
		sb.WriteString(" (" + values(row) + ")")
	}
	sb.WriteString(b.UpdateSuffix())
	if _, err := d.tx.Exec(ctx, sb.String()); err != nil {
		return fmt.Errorf("sink: restoring %s.%s: %w", b.Table, strings.Join(b.Columns, ", "), err)
	}
	return nil
}

func (d *DB) Close(ctx context.Context) error {
	// Fire any deferred foreign-key checks now, before re-enabling triggers.
	//
	// Two reasons. ALTER TABLE refuses to run while a table has pending trigger
	// events, so without this the re-enable fails and the load commits with the
	// target's triggers left switched off for good. And forcing the checks here
	// means a referential problem surfaces as a foreign-key error naming the
	// constraint, rather than as an opaque failure at COMMIT.
	if _, err := d.tx.Exec(ctx, "SET CONSTRAINTS ALL IMMEDIATE"); err != nil {
		d.tx.Rollback(ctx)
		return fmt.Errorf("sink: the slice failed foreign-key validation: %w", err)
	}
	for _, stmt := range load.EnableTriggers(d.tables) {
		if err := d.try(ctx, stmt); err != nil {
			d.tx.Rollback(ctx)
			return err
		}
	}
	for _, stmt := range load.SequenceResets(d.cat, d.tables) {
		if _, err := d.tx.Exec(ctx, stmt); err != nil {
			d.tx.Rollback(ctx)
			return fmt.Errorf("sink: resetting sequences: %w", err)
		}
	}
	if err := d.tx.Commit(ctx); err != nil {
		return fmt.Errorf("sink: committing the load: %w", err)
	}
	return nil
}

// Rollback abandons a partial load. Callers use it on any error so the target
// is never left half-populated.
func (d *DB) Rollback(ctx context.Context) { _ = d.tx.Rollback(ctx) }

type dbWriter struct {
	rows   chan []any
	done   chan error
	ref    catalog.Ref
	closed bool
	err    error
}

func (w *dbWriter) Write(ctx context.Context, row []any) error {
	if w.closed {
		return w.err
	}
	select {
	case w.rows <- row:
		return nil
	case err := <-w.done:
		// COPY failed early and stopped reading; without this branch the send
		// above would block forever.
		w.closed, w.err = true, fmt.Errorf("sink: copying into %s: %w", w.ref, err)
		return w.err
	case <-ctx.Done():
		w.closed, w.err = true, ctx.Err()
		return w.err
	}
}

func (w *dbWriter) Close(ctx context.Context) error {
	if w.closed {
		return w.err
	}
	w.closed = true
	close(w.rows)
	select {
	case err := <-w.done:
		if err != nil {
			w.err = fmt.Errorf("sink: copying into %s: %w", w.ref, err)
		}
	case <-ctx.Done():
		w.err = ctx.Err()
	}
	return w.err
}
