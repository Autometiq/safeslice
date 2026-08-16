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
	"bytes"
	"context"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"os/user"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/Autometiq/safeslice/internal/catalog"
	"github.com/Autometiq/safeslice/internal/demo"
	"github.com/Autometiq/safeslice/internal/ui"
)

// Connection handling for the wizard: finding a database, creating one,
// and explaining what went wrong when neither works.
//
// A connection failure is the first thing a new user hits and the point at
// which most of them give up. `dial tcp 127.0.0.1:5432: connectex: No
// connection could be made because the target machine actively refused it` is
// accurate and tells a developer nothing they can act on, so every failure here
// is rendered as the four things that are actually wrong, with the raw error
// kept one keystroke away for the person who needs it.

// endpoint is a parsed connection string, without the password.
type endpoint struct {
	Host, Port, Database, User string
}

func parseEndpoint(dsn string) (endpoint, error) {
	cfg, err := pgx.ParseConfig(dsn)
	if err != nil {
		return endpoint{}, err
	}
	return endpoint{
		Host:     cfg.Host,
		Port:     fmt.Sprint(cfg.Port),
		Database: cfg.Database,
		User:     cfg.User,
	}, nil
}

// summarise prints where a connection points, never what it authenticates with.
func summarise(title, dsn string) {
	e, err := parseEndpoint(dsn)
	if err != nil {
		ui.Section(title)
		ui.Detail("(unreadable connection string)")
		return
	}
	ui.Section(title)
	ui.Detail("%s", ui.Field("Host", e.Host))
	ui.Detail("%s", ui.Field("Port", e.Port))
	ui.Detail("%s", ui.Field("Database", e.Database))
	ui.Detail("%s", ui.Field("User", e.User))
}

// connectAction is what the user chose to do about a failed connection.
type connectAction int

const (
	connectRetry connectAction = iota
	connectChange
	connectPassword
	connectCancel
)

// explainConnection renders a failure in terms a developer can act on and asks
// what to do next. The technical error is available but not in the way.
func explainConnection(dsn string, err error) connectAction {
	e, _ := parseEndpoint(dsn)
	ui.Fatal(ui.Hint(fmt.Errorf("could not connect to PostgreSQL"), causesFor(err)))
	ui.Detail("%s", ui.Field("Host", e.Host))
	ui.Detail("%s", ui.Field("Port", e.Port))
	ui.Detail("%s", ui.Field("Database", e.Database))
	ui.Detail("%s", ui.Field("User", e.User))
	ui.Detail("")

	// Once the driver's message is on screen there is no reason to offer it
	// again: leaving it in redraws the same list after every look and reads as
	// a loop with no way out. Dropping it guarantees the menu only ever gets
	// shorter, so every path leads somewhere.
	shown := false
	for {
		opts := []ui.Option{
			{Label: "Retry", Note: "try the same connection again"},
			{Label: "Change the connection", Note: "type a different connection string"},
			{Label: "Enter a password", Note: "keeps it out of your shell history"},
		}
		if !shown {
			opts = append(opts, ui.Option{
				Label: "Show the technical error", Note: "the driver's own message",
			})
		}
		opts = append(opts, ui.Option{Label: "Cancel", Note: "stop here; nothing has been read"})

		switch label(opts, ui.Select("What would you like to do?", opts, 1)) {
		case "Retry":
			return connectRetry
		case "Change the connection":
			return connectChange
		case "Enter a password":
			return connectPassword
		case "Show the technical error":
			shown = true
			ui.Detail("%s", err.Error())
			ui.Detail("")
		default:
			return connectCancel
		}
	}
}

// causesFor lists the plausible reasons for a failure, narrowed by what the
// driver actually said. A list of four possibilities beats a stack trace, and a
// list of one beats the list of four.
func causesFor(err error) string {
	msg := strings.ToLower(err.Error())
	switch {
	case strings.Contains(msg, "password authentication failed"),
		strings.Contains(msg, "no password supplied"),
		strings.Contains(msg, "authentication"):
		return "The host answered, so the server is running -- the username or password was rejected. " +
			"Check the user, and supply the password if the server requires one."
	case strings.Contains(msg, "does not exist"):
		return "The server is running and accepted the credentials, but that database does not exist. " +
			"Check the name at the end of the connection string, or create it first."
	case strings.Contains(msg, "timeout"), strings.Contains(msg, "i/o timeout"):
		return "The host did not answer in time. It may be behind a VPN or firewall, or the host name may be wrong."
	default:
		return "Possible causes: PostgreSQL is not running, the port is wrong, " +
			"the database does not exist, or authentication failed."
	}
}

// tryConnect opens a connection without leaving one dangling on failure.
func tryConnect(ctx context.Context, dsn string) error {
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		return err
	}
	return conn.Close(ctx)
}

// serverVersion reports the PostgreSQL version, for the connection summary.
func serverVersion(ctx context.Context, conn *pgx.Conn) string {
	var v string
	if err := conn.QueryRow(ctx, "SHOW server_version").Scan(&v); err != nil {
		return ""
	}
	return strings.Fields(v)[0]
}

// adminDSN points at the same server's `postgres` database, which is where
// CREATE DATABASE has to be issued from -- you cannot create a database from
// inside the one you are creating.
func adminDSN(dsn string) (string, error) {
	cfg, err := pgx.ParseConfig(dsn)
	if err != nil {
		return "", err
	}
	return buildDSN(cfg.Host, int(cfg.Port), cfg.User, cfg.Password, "postgres"), nil
}

// withDatabase points a connection string at a different database on the same
// server.
func withDatabase(dsn, database string) (string, error) {
	cfg, err := pgx.ParseConfig(dsn)
	if err != nil {
		return "", err
	}
	return buildDSN(cfg.Host, int(cfg.Port), cfg.User, cfg.Password, database), nil
}

func buildDSN(host string, port int, user, password, database string) string {
	// url.UserPassword escapes credentials that contain punctuation, which is
	// how a perfectly good password turns into "authentication failed".
	auth := ""
	switch {
	case user != "" && password != "":
		auth = url.UserPassword(user, password).String() + "@"
	case user != "":
		auth = url.User(user).String() + "@"
	}
	return fmt.Sprintf("postgres://%s%s:%d/%s?sslmode=prefer", auth, host, port, database)
}

// localServer looks for a PostgreSQL a developer already has running, so the
// common case -- a local server, default port -- needs no typing at all.
func localServer(ctx context.Context) (string, bool) {
	names := []string{os.Getenv("PGUSER"), "postgres"}
	if u, err := user.Current(); err == nil && u.Username != "" {
		// Homebrew and the Postgres.app installers create a role named after
		// the account, and `postgres` often does not exist at all.
		names = append([]string{u.Username}, names...)
	}
	host, port := envOr("PGHOST", "localhost"), envOr("PGPORT", "5432")
	p := 5432
	fmt.Sscanf(port, "%d", &p)

	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	for _, name := range names {
		if name == "" {
			continue
		}
		dsn := buildDSN(host, p, name, os.Getenv("PGPASSWORD"), "postgres")
		if tryConnect(ctx, dsn) == nil {
			return dsn, true
		}
	}
	return "", false
}

func envOr(name, def string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return def
}

// databaseExists reports whether a database is already there. The answer
// decides whether the wizard is creating something or about to overwrite
// somebody's work.
func databaseExists(ctx context.Context, admin, name string) (bool, error) {
	conn, err := pgx.Connect(ctx, admin)
	if err != nil {
		return false, err
	}
	defer conn.Close(context.Background())
	var one int
	err = conn.QueryRow(ctx, "SELECT 1 FROM pg_database WHERE datname = $1", name).Scan(&one)
	if err == pgx.ErrNoRows {
		return false, nil
	}
	return err == nil, err
}

// createDatabase creates an empty database.
func createDatabase(ctx context.Context, admin, name string) error {
	conn, err := pgx.Connect(ctx, admin)
	if err != nil {
		return err
	}
	defer conn.Close(context.Background())
	// CREATE DATABASE takes no parameters, so the name is quoted as an
	// identifier rather than passed as an argument.
	_, err = conn.Exec(ctx, "CREATE DATABASE "+quoteIdent(name))
	return err
}

// dropDatabase removes a database and everything in it. Only ever called after
// an explicit confirmation naming the database.
func dropDatabase(ctx context.Context, admin, name string) error {
	conn, err := pgx.Connect(ctx, admin)
	if err != nil {
		return err
	}
	defer conn.Close(context.Background())
	// Sessions left open by an editor or a running app would otherwise make
	// this fail with "database is being accessed by other users".
	if _, err := conn.Exec(ctx,
		"SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE datname = $1 AND pid <> pg_backend_pid()",
		name); err != nil {
		return err
	}
	_, err = conn.Exec(ctx, "DROP DATABASE IF EXISTS "+quoteIdent(name))
	return err
}

func quoteIdent(s string) string { return `"` + strings.ReplaceAll(s, `"`, `""`) + `"` }

// missingTables reports which of the source's tables the target does not have.
//
// safeslice loads rows, not schema. Pointed at an empty database it fails
// halfway through with `relation "users" does not exist`, so the wizard checks
// first and offers to fix it.
func missingTables(ctx context.Context, target string, cat *catalog.Catalog, schemas []string) ([]string, error) {
	conn, err := pgx.Connect(ctx, target)
	if err != nil {
		return nil, err
	}
	defer conn.Close(context.Background())
	got, err := catalog.Load(ctx, conn, schemas)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, ref := range cat.Refs() {
		if t, ok := cat.Table(ref); ok && t.Partition {
			continue // arrives with its parent
		}
		if _, ok := got.Table(ref); !ok {
			out = append(out, ref.String())
		}
	}
	return out, nil
}

// copySchema reproduces the source's structure in the target with pg_dump.
//
// Structure only -- no rows ever leave the source through this path. It exists
// because "create a database for me" is useless if the database has no tables,
// and the alternative is telling a first-time user to go and run their
// migrations before they have seen the tool do anything.
func copySchema(ctx context.Context, source, target string) error {
	for _, bin := range []string{"pg_dump", "psql"} {
		if _, err := exec.LookPath(bin); err != nil {
			return ui.Hint(fmt.Errorf("%s is not installed", bin),
				"safeslice copies rows, not table definitions. Create the schema with your "+
					"own migrations and choose \"Use an existing database\", or install the "+
					"PostgreSQL client tools so this can copy the structure for you.")
		}
	}

	srcEnv, err := libpqEnv(source)
	if err != nil {
		return err
	}
	dump := exec.CommandContext(ctx, "pg_dump",
		"--schema-only", "--no-owner", "--no-privileges", "--no-comments")
	dump.Env = srcEnv
	var dumpErr strings.Builder
	dump.Stderr = &dumpErr
	body, err := dump.Output()
	if err != nil {
		return ui.Hint(fmt.Errorf("pg_dump failed: %s", firstLine(dumpErr.String())),
			"This is usually a version mismatch: pg_dump must be at least as new as the "+
				"server. Create the schema with your own migrations instead and choose "+
				"\"Use an existing database\".")
	}

	tgtEnv, err := libpqEnv(target)
	if err != nil {
		return err
	}
	// psql rather than the pgx connection: a dump carries psql's own
	// meta-commands (\restrict, \connect), which are not SQL and which the
	// driver rightly refuses.
	load := exec.CommandContext(ctx, "psql", "--quiet", "--no-psqlrc",
		"-v", "ON_ERROR_STOP=1", "-f", "-")
	load.Env = tgtEnv
	load.Stdin = bytes.NewReader(body)
	var loadErr strings.Builder
	load.Stderr = &loadErr
	if err := load.Run(); err != nil {
		return fmt.Errorf("creating the schema in the target: %s", firstLine(loadErr.String()))
	}
	return nil
}

// libpqEnv renders a connection string as the environment pg_dump and psql
// expect.
//
// Deliberately not an argument: a DSN on the command line is visible to every
// `ps` on the machine, and this tool's whole claim is that it does not leak
// production credentials.
func libpqEnv(dsn string) ([]string, error) {
	cfg, err := pgx.ParseConfig(dsn)
	if err != nil {
		return nil, fmt.Errorf("parsing connection string: %w", err)
	}
	env := append(os.Environ(),
		"PGHOST="+cfg.Host,
		"PGPORT="+fmt.Sprint(cfg.Port),
		"PGUSER="+cfg.User,
		"PGDATABASE="+cfg.Database,
	)
	if cfg.Password != "" {
		env = append(env, "PGPASSWORD="+cfg.Password)
	}
	if u, err := url.Parse(dsn); err == nil {
		if mode := u.Query().Get("sslmode"); mode != "" {
			env = append(env, "PGSSLMODE="+mode)
		}
	}
	return env, nil
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return strings.TrimSpace(s[:i])
	}
	return strings.TrimSpace(s)
}

// dockerTarget starts a throwaway PostgreSQL for people who have Docker but no
// local server. It is a container of their own, kept separate from the demo's,
// so stopping the demo never takes their development database with it.
const (
	targetContainer = "safeslice-target"
	targetPort      = "55434"
	targetPassword  = "safeslice"
)

func dockerTarget(ctx context.Context, database string, say func(string)) (string, error) {
	if err := demo.DockerAvailable(ctx); err != nil {
		return "", ui.HintCmd(err,
			"This option runs PostgreSQL in a container. Install Docker Desktop and start it, "+
				"or choose another destination.",
			"https://docs.docker.com/get-started/get-docker/")
	}
	dsn := fmt.Sprintf("postgres://postgres:%s@localhost:%s/%s?sslmode=disable",
		targetPassword, targetPort, database)

	running, _ := exec.CommandContext(ctx, "docker", "ps", "--filter",
		"name=^/"+targetContainer+"$", "--format", "{{.Names}}").Output()
	if strings.TrimSpace(string(running)) != targetContainer {
		exists, _ := exec.CommandContext(ctx, "docker", "ps", "-a", "--filter",
			"name=^/"+targetContainer+"$", "--format", "{{.Names}}").Output()
		if strings.TrimSpace(string(exists)) == targetContainer {
			say("restarting the container")
			if out, err := exec.CommandContext(ctx, "docker", "start", targetContainer).CombinedOutput(); err != nil {
				return "", fmt.Errorf("starting %s: %s", targetContainer, firstLine(string(out)))
			}
		} else {
			say("starting postgres:17 (first run pulls the image)")
			if out, err := exec.CommandContext(ctx, "docker", "run", "-d",
				"--name", targetContainer,
				"-e", "POSTGRES_PASSWORD="+targetPassword,
				"-p", targetPort+":5432",
				demo.Image).CombinedOutput(); err != nil {
				return "", fmt.Errorf("starting %s: %s", targetContainer, firstLine(string(out)))
			}
		}
	}

	say("waiting for PostgreSQL")
	admin, err := adminDSN(dsn)
	if err != nil {
		return "", err
	}
	deadline := time.Now().Add(90 * time.Second)
	for {
		if tryConnect(ctx, admin) == nil {
			break
		}
		if time.Now().After(deadline) {
			return "", fmt.Errorf("the container did not become ready in 90s")
		}
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(time.Second):
		}
	}

	ok, err := databaseExists(ctx, admin, database)
	if err != nil {
		return "", err
	}
	if !ok {
		say("creating " + database)
		if err := createDatabase(ctx, admin, database); err != nil {
			return "", err
		}
	}
	return dsn, nil
}
