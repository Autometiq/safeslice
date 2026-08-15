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

package report

import (
	"fmt"
	"os/exec"
	"runtime"
)

// What to do with the database once it exists.
//
// The snippets are deliberately the boring ones: an environment variable and
// the five framework settings that read it. None of them carries a password --
// the target is a local database the reader owns, and a generated file that
// hands out credentials is a file nobody can commit.

// Snippet is one way to point an application at the new database.
type Snippet struct {
	Name string
	Code string
}

// Snippets renders connection examples for the target.
func Snippets(e Endpoint) []Snippet {
	url := e.URL()
	db := e.Database
	if db == "" {
		db = "myapp_dev"
	}
	host := e.Host
	if host == "" {
		host = "localhost"
	}
	return []Snippet{
		{"Environment", "DATABASE_URL=" + url},
		{"psql", "psql " + url},
		{"Prisma", "# .env\nDATABASE_URL=\"" + url + "\""},
		{"Rails", fmt.Sprintf("# config/database.yml\ndevelopment:\n  adapter: postgresql\n"+
			"  host: %s\n  port: %s\n  database: %s", host, portOr(e), db)},
		{"Django", fmt.Sprintf("DATABASES = {\n    \"default\": {\n"+
			"        \"ENGINE\": \"django.db.backends.postgresql\",\n"+
			"        \"NAME\": \"%s\",\n        \"HOST\": \"%s\",\n        \"PORT\": \"%s\",\n    }\n}",
			db, host, portOr(e))},
		{"Node.js", "import { Pool } from \"pg\";\n" +
			"const pool = new Pool({ connectionString: process.env.DATABASE_URL });"},
		{"Go", "conn, err := pgx.Connect(ctx, os.Getenv(\"DATABASE_URL\"))"},
	}
}

func portOr(e Endpoint) string {
	if e.Port == "" {
		return "5432"
	}
	return e.Port
}

// Open shows a file in the user's browser.
//
// Best effort by design: a headless server or a locked-down laptop has nothing
// to open it with, and that is not a reason to fail a run that succeeded. The
// path is printed either way.
func Open(path string) error {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "windows":
		// rundll32 rather than `cmd /c start`, which treats the first quoted
		// argument as a window title and would open a blank shell instead.
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", path)
	case "darwin":
		cmd = exec.Command("open", path)
	default:
		cmd = exec.Command("xdg-open", path)
	}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("opening %s: %w", path, err)
	}
	go cmd.Wait() //nolint:errcheck // reap the child; the result is not interesting
	return nil
}
