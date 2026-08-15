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
	"path/filepath"
	"runtime"
	"strings"
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

// Open shows a file with whatever the system associates with it: the browser
// for HTML, an editor for Markdown, a spreadsheet for CSV.
//
// Best effort by design: a headless server or a locked-down laptop has nothing
// to open it with, and that is not a reason to fail a run that succeeded. The
// path is printed either way.
func Open(path string) error {
	abs, err := filepath.Abs(path)
	if err == nil {
		path = abs // relative paths break once the handler changes directory
	}
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

// Reveal opens a directory in the system file manager.
func Reveal(dir string) error {
	abs, err := filepath.Abs(dir)
	if err == nil {
		dir = abs
	}
	switch runtime.GOOS {
	case "windows":
		// explorer.exe exits non-zero even when it succeeds, so the error is
		// deliberately not checked -- reporting a failure that did not happen
		// is worse than reporting nothing.
		cmd := exec.Command("explorer", dir)
		_ = cmd.Start()
		go cmd.Wait() //nolint:errcheck
		return nil
	default:
		return Open(dir)
	}
}

// Clipboard copies text using whatever the platform provides.
//
// Reports whether it worked, because the caller has to print the value instead
// when it did not: a "copied!" message that copied nothing is worse than no
// message at all. Linux has no clipboard without a display server, which is
// exactly the headless case where the printed string is what matters.
func Clipboard(text string) bool {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "windows":
		cmd = exec.Command("clip")
	case "darwin":
		cmd = exec.Command("pbcopy")
	default:
		if _, err := exec.LookPath("wl-copy"); err == nil {
			cmd = exec.Command("wl-copy")
		} else if _, err := exec.LookPath("xclip"); err == nil {
			cmd = exec.Command("xclip", "-selection", "clipboard")
		} else {
			return false
		}
	}
	cmd.Stdin = strings.NewReader(text)
	return cmd.Run() == nil
}
