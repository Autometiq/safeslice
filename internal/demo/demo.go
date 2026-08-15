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

// Package demo provides a throwaway PostgreSQL database so safeslice can be
// tried without pointing it at anything real.
//
// The schema and seed data are embedded in the binary rather than shipped as
// files, so `safeslice` started from any directory can run the demo. Requiring
// a git clone to evaluate a tool loses most of the people who were willing to
// try it.
package demo

import (
	"bytes"
	"context"
	_ "embed"
	"fmt"
	"os/exec"
	"strings"
	"time"
)

//go:embed schema.sql
var schemaSQL string

//go:embed seed.sql
var seedSQL string

const (
	Container = "safeslice-demo"
	Image     = "postgres:17"
	// 55433 rather than 5432, so this cannot collide with a Postgres the user
	// already runs locally.
	Port     = "55433"
	Password = "demo"
	SourceDB = "shop"     // the "production" database, seeded
	TargetDB = "shop_dev" // the local target, schema only
)

// SourceDSN is the seeded database safeslice reads from.
func SourceDSN() string {
	return fmt.Sprintf("postgres://postgres:%s@localhost:%s/%s?sslmode=disable", Password, Port, SourceDB)
}

// TargetDSN is the empty database safeslice writes to.
func TargetDSN() string {
	return fmt.Sprintf("postgres://postgres:%s@localhost:%s/%s?sslmode=disable", Password, Port, TargetDB)
}

// DockerAvailable reports whether docker is installed and its daemon is up.
// Both are checked: the CLI being on PATH says nothing about Docker Desktop
// actually running, and that distinction is the difference between a useful
// message and a confusing one.
func DockerAvailable(ctx context.Context) error {
	if _, err := exec.LookPath("docker"); err != nil {
		return fmt.Errorf("docker is not installed")
	}
	if out, err := run(ctx, "docker", "info", "--format", "{{.ServerVersion}}"); err != nil {
		_ = out
		return fmt.Errorf("the docker daemon is not running")
	}
	return nil
}

// Running reports whether the demo container is already up.
func Running(ctx context.Context) bool {
	out, err := run(ctx, "docker", "ps", "--filter", "name=^/"+Container+"$", "--format", "{{.Names}}")
	return err == nil && strings.TrimSpace(out) == Container
}

// exists reports whether the container exists in any state.
func exists(ctx context.Context) bool {
	out, err := run(ctx, "docker", "ps", "-a", "--filter", "name=^/"+Container+"$", "--format", "{{.Names}}")
	return err == nil && strings.TrimSpace(out) == Container
}

// Start brings up the demo database and seeds it. Progress is reported through
// the callback so the caller can drive a status line.
//
// Calling it when the demo is already running is a no-op, which makes the menu
// option safe to pick twice.
func Start(ctx context.Context, progress func(string)) error {
	say := func(s string) {
		if progress != nil {
			progress(s)
		}
	}
	if err := DockerAvailable(ctx); err != nil {
		return err
	}
	if Running(ctx) {
		say("demo database already running")
		return nil
	}
	if exists(ctx) {
		say("restarting the demo database")
		if _, err := run(ctx, "docker", "start", Container); err != nil {
			return fmt.Errorf("starting the existing demo container: %w", err)
		}
	} else {
		say("pulling " + Image + " (first run only)")
		if _, err := run(ctx, "docker", "run", "-d",
			"--name", Container,
			"-e", "POSTGRES_PASSWORD="+Password,
			"-e", "POSTGRES_DB="+SourceDB,
			"-p", Port+":5432",
			Image); err != nil {
			return fmt.Errorf("starting the demo container: %w", err)
		}
	}

	say("waiting for PostgreSQL")
	if err := waitReady(ctx, 90*time.Second); err != nil {
		return err
	}

	// Seeding is skipped when the tables already exist, so restarting a stopped
	// container reuses its data instead of failing on duplicates.
	if seeded(ctx) {
		say("demo data already present")
		return nil
	}

	say("creating schema")
	if err := psql(ctx, SourceDB, schemaSQL); err != nil {
		return fmt.Errorf("creating the demo schema: %w", err)
	}
	if _, err := run(ctx, "docker", "exec", Container, "psql", "-U", "postgres",
		"-c", "CREATE DATABASE "+TargetDB); err != nil {
		return fmt.Errorf("creating the target database: %w", err)
	}
	// The target gets structure but no rows, which is what a developer's machine
	// looks like after running their own migrations.
	if err := psql(ctx, TargetDB, schemaSQL); err != nil {
		return fmt.Errorf("creating the target schema: %w", err)
	}

	say("loading demo customers")
	if err := psql(ctx, SourceDB, seedSQL); err != nil {
		return fmt.Errorf("loading the demo data: %w", err)
	}
	return nil
}

// ResetTarget empties the target database and recreates its schema.
//
// The demo has to be runnable twice. Without this the second attempt dies on a
// duplicate primary key, because the rows from the first are still there -- an
// error that says nothing about the demo simply needing a clean target.
func ResetTarget(ctx context.Context) error {
	// Existing sessions would block the drop.
	if _, err := run(ctx, "docker", "exec", Container, "psql", "-U", "postgres", "-c",
		"SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE datname = '"+TargetDB+"'"); err != nil {
		return fmt.Errorf("clearing connections to %s: %w", TargetDB, err)
	}
	if _, err := run(ctx, "docker", "exec", Container, "psql", "-U", "postgres", "-c",
		"DROP DATABASE IF EXISTS "+TargetDB); err != nil {
		return fmt.Errorf("dropping %s: %w", TargetDB, err)
	}
	if _, err := run(ctx, "docker", "exec", Container, "psql", "-U", "postgres", "-c",
		"CREATE DATABASE "+TargetDB); err != nil {
		return fmt.Errorf("creating %s: %w", TargetDB, err)
	}
	if err := psql(ctx, TargetDB, schemaSQL); err != nil {
		return fmt.Errorf("creating the target schema: %w", err)
	}
	return nil
}

// Stop removes the demo container and its data.
func Stop(ctx context.Context) error {
	if !exists(ctx) {
		return nil
	}
	if _, err := run(ctx, "docker", "rm", "-f", "-v", Container); err != nil {
		return fmt.Errorf("removing the demo container: %w", err)
	}
	return nil
}

// Counts summarises what is in the seeded database, for display.
func Counts(ctx context.Context) (string, error) {
	const q = `SELECT (SELECT count(*) FROM users) || ' users, ' ||
	                 (SELECT count(*) FROM orders) || ' orders, ' ||
	                 (SELECT count(*) FROM events) || ' events'`
	out, err := run(ctx, "docker", "exec", Container, "psql", "-U", "postgres",
		"-d", SourceDB, "-Atc", q)
	return strings.TrimSpace(out), err
}

func seeded(ctx context.Context) bool {
	out, err := run(ctx, "docker", "exec", Container, "psql", "-U", "postgres",
		"-d", SourceDB, "-Atc", "SELECT to_regclass('public.users') IS NOT NULL")
	return err == nil && strings.TrimSpace(out) == "t"
}

func waitReady(ctx context.Context, limit time.Duration) error {
	deadline := time.Now().Add(limit)
	for time.Now().Before(deadline) {
		if _, err := run(ctx, "docker", "exec", Container,
			"pg_isready", "-U", "postgres", "-d", SourceDB); err == nil {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Second):
		}
	}
	return fmt.Errorf("the demo database did not become ready within %s", limit)
}

// psql pipes SQL into the container rather than passing it as an argument,
// which keeps it clear of shell quoting and command-length limits.
func psql(ctx context.Context, db, sql string) error {
	cmd := exec.CommandContext(ctx, "docker", "exec", "-i", Container,
		"psql", "-v", "ON_ERROR_STOP=1", "-q", "-U", "postgres", "-d", db)
	cmd.Stdin = strings.NewReader(sql)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("%w: %s", err, strings.TrimSpace(stderr.String()))
	}
	return nil
}

func run(ctx context.Context, name string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		return stdout.String(), fmt.Errorf("%s: %w: %s", name, err, strings.TrimSpace(stderr.String()))
	}
	return stdout.String(), nil
}
