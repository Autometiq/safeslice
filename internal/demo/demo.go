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
	"bufio"
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
	// Every docker command here is expected to answer in seconds. A daemon that
	// is wedged, or a Docker Desktop still starting up, otherwise leaves the
	// spinner turning with nothing behind it -- which reads as safeslice
	// hanging. The one genuinely slow step, pulling the image, is not run
	// through this: see EnsureImage.
	dockerTimeout = 60 * time.Second
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
// It is idempotent rather than skipped: an existing container is reused, but
// every run still checks that PostgreSQL answers, that the source is seeded and
// that the target exists. That is what makes the menu option safe to pick twice
// -- including after a first attempt was interrupted part way through.
func Start(ctx context.Context, progress func(string)) error {
	say := func(s string) {
		if progress != nil {
			progress(s)
		}
	}
	if err := DockerAvailable(ctx); err != nil {
		return err
	}
	switch {
	case Running(ctx):
		// Reuse it, but still fall through to the readiness and seed checks
		// below. A container being up says nothing about the database inside it
		// being ready, or about a first run having been interrupted before it
		// finished seeding -- and returning early here handed both of those to
		// the next step as a working demo.
		say("reusing the running demo database")
	case exists(ctx):
		say("restarting the demo database")
		if _, err := run(ctx, "docker", "start", Container); err != nil {
			// A container left behind by an older safeslice cannot necessarily
			// be started by this one: earlier versions bind-mounted the schema
			// from disk, and that mount no longer resolves. The demo database
			// is disposable by definition, so recreate it rather than hand the
			// user a wall of Docker internals about rootfs mounts.
			say("the existing container cannot start; recreating it")
			if _, rmErr := run(ctx, "docker", "rm", "-f", Container); rmErr != nil {
				return fmt.Errorf("removing the unusable demo container %s: %w", Container, rmErr)
			}
			// Falls through to the readiness wait and seeding below: a fresh
			// container is empty, however the previous one died.
			if err := create(ctx, say); err != nil {
				return err
			}
		}
	default:
		if err := create(ctx, say); err != nil {
			return err
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
		return ensureTarget(ctx)
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

// EnsureImage downloads the PostgreSQL image unless it is already local.
//
// `docker run` would pull it anyway, but silently and as part of a command
// that otherwise returns in a second: the first run then sits on one frozen
// status line for however long a few hundred megabytes take, with no way to
// tell a slow download from a stalled one. Pulling explicitly puts docker's
// own progress on the status line.
//
// This is the one docker call not given a timeout. A pull is legitimately
// minutes long, and it now reports progress, so a user can see it moving and
// Ctrl-C if it is not.
func EnsureImage(ctx context.Context, say func(string)) error {
	if say == nil {
		say = func(string) {}
	}
	if _, err := run(ctx, "docker", "image", "inspect", Image); err == nil {
		return nil
	}
	say("downloading " + Image + " (first run only)")

	cmd := exec.CommandContext(ctx, "docker", "pull", Image)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("pulling %s: %w", Image, err)
	}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("pulling %s: %w", Image, err)
	}
	for sc := bufio.NewScanner(stdout); sc.Scan(); {
		if line := progressLine(sc.Text()); line != "" {
			say("downloading " + Image + " -- " + line)
		}
	}
	if err := cmd.Wait(); err != nil {
		return fmt.Errorf("pulling %s: %w: %s", Image, err, strings.TrimSpace(stderr.String()))
	}
	return nil
}

// progressLine trims a `docker pull` line down to what fits beside a spinner.
// The layer id leading each line is twelve characters of noise, and a line
// long enough to wrap breaks the status line's redraw.
func progressLine(s string) string {
	s = strings.TrimSpace(s)
	// Only a hex prefix is a layer id. "Status: " and "Digest: " lead lines
	// worth keeping whole.
	if i := strings.Index(s, ": "); i > 0 && strings.Trim(s[:i], "0123456789abcdef") == "" {
		s = s[i+2:]
	}
	if len(s) > 48 {
		s = s[:48]
	}
	return s
}

// create starts a fresh demo container. No bind mounts: the schema and seed
// are embedded in the binary and piped in through `docker exec`, so the
// container never depends on a path that exists only on the machine that
// created it.
func create(ctx context.Context, say func(string)) error {
	if err := EnsureImage(ctx, say); err != nil {
		return err
	}
	say("starting the container")
	if _, err := run(ctx, "docker", "run", "-d",
		"--name", Container,
		"-e", "POSTGRES_PASSWORD="+Password,
		"-e", "POSTGRES_DB="+SourceDB,
		"-p", Port+":5432",
		Image); err != nil {
		return fmt.Errorf("starting the demo container: %w", err)
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

// ensureTarget creates the target database if it is missing. A first run
// interrupted between seeding the source and creating the target otherwise
// leaves a container that looks complete, and every later command that writes
// to the target fails on a database that is not there.
func ensureTarget(ctx context.Context) error {
	out, err := run(ctx, "docker", "exec", Container, "psql", "-U", "postgres", "-Atc",
		"SELECT 1 FROM pg_database WHERE datname = '"+TargetDB+"'")
	if err != nil {
		return fmt.Errorf("checking for %s: %w", TargetDB, err)
	}
	if strings.TrimSpace(out) == "1" {
		return nil
	}
	if _, err := run(ctx, "docker", "exec", Container, "psql", "-U", "postgres", "-c",
		"CREATE DATABASE "+TargetDB); err != nil {
		return fmt.Errorf("creating %s: %w", TargetDB, err)
	}
	return psql(ctx, TargetDB, schemaSQL)
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
	ctx, cancel := context.WithTimeout(ctx, dockerTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, name, args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		return stdout.String(), fmt.Errorf("%s: %w: %s", name, err, strings.TrimSpace(stderr.String()))
	}
	return stdout.String(), nil
}
