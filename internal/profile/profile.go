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

// Package profile remembers what the wizard was told, so returning to it a
// week later is a menu choice rather than the same twenty questions.
//
// One rule governs the whole package: a password is never written to disk.
// Stored connection strings are stripped of their credentials on the way in,
// and a saved connection can instead name the environment variable that holds
// the password. Anything the tool persists here is committable by accident
// without becoming an incident.
package profile

import (
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Dir is the per-project state directory, relative to the working directory.
const Dir = ".safeslice"

// Connection is a saved database location. DSN never carries a password.
type Connection struct {
	Name string `yaml:"name" json:"name"`
	DSN  string `yaml:"dsn" json:"dsn"`
	// PasswordEnv names an environment variable holding the password, for
	// databases that need one. The value itself stays out of the file.
	PasswordEnv string `yaml:"password_env,omitempty" json:"password_env,omitempty"`
}

// Resolve returns a connection string ready to use, taking the password from
// the named environment variable when one was recorded.
func (c Connection) Resolve() string {
	if c.PasswordEnv == "" {
		return c.DSN
	}
	pw := os.Getenv(c.PasswordEnv)
	if pw == "" {
		return c.DSN
	}
	return WithPassword(c.DSN, pw)
}

// Profile is a remembered wizard run: where the data came from, where it went,
// and how much of it was taken.
type Profile struct {
	Name       string     `yaml:"name"`
	Source     Connection `yaml:"source"`
	Target     Connection `yaml:"target"`
	Config     string     `yaml:"config,omitempty"`
	Root       string     `yaml:"root,omitempty"`
	Where      string     `yaml:"where,omitempty"`
	Limit      int        `yaml:"limit,omitempty"`
	ChildDepth int        `yaml:"child_depth"`
	Seed       string     `yaml:"seed,omitempty"`
	Updated    time.Time  `yaml:"updated"`
}

// Run is one entry in the history: enough to answer "what did we load into
// that database, and did it pass?" without keeping any of the data.
type Run struct {
	At        time.Time `json:"at"`
	Profile   string    `json:"profile,omitempty"`
	Source    string    `json:"source"`
	Target    string    `json:"target"`
	Tables    int       `json:"tables"`
	Rows      int       `json:"rows"`
	Masked    int       `json:"masked_columns"`
	Verified  bool      `json:"verification_passed"`
	Artifacts string    `json:"artifacts,omitempty"`
}

// settings is .safeslice/config.yaml: the saved connections and which profile
// was used last.
type settings struct {
	Version     int          `yaml:"version"`
	LastProfile string       `yaml:"last_profile,omitempty"`
	Connections []Connection `yaml:"connections,omitempty"`
}

// Store is a .safeslice directory. The zero value is unusable; call Open.
type Store struct{ dir string }

// Open returns a store rooted at dir, defaulting to ./.safeslice. Nothing is
// created until something is written.
func Open(dir string) *Store {
	if dir == "" {
		dir = Dir
	}
	return &Store{dir: dir}
}

// Path reports the directory the store writes to.
func (s *Store) Path() string { return s.dir }

func (s *Store) settingsPath() string { return filepath.Join(s.dir, "config.yaml") }

func (s *Store) load() (settings, error) {
	var set settings
	data, err := os.ReadFile(s.settingsPath())
	if os.IsNotExist(err) {
		return settings{Version: 1}, nil
	}
	if err != nil {
		return set, fmt.Errorf("profile: reading %s: %w", s.settingsPath(), err)
	}
	if err := yaml.Unmarshal(data, &set); err != nil {
		return set, fmt.Errorf("profile: %s: %w", s.settingsPath(), err)
	}
	return set, nil
}

func (s *Store) write(name string, body []byte) error {
	if err := os.MkdirAll(filepath.Dir(filepath.Join(s.dir, name)), 0o755); err != nil {
		return fmt.Errorf("profile: creating %s: %w", s.dir, err)
	}
	// 0600: nothing secret is written here by design, but the file records
	// which hosts hold production data, which is not for every account on a
	// shared machine.
	if err := os.WriteFile(filepath.Join(s.dir, name), body, 0o600); err != nil {
		return fmt.Errorf("profile: writing %s: %w", name, err)
	}
	return nil
}

func (s *Store) save(set settings) error {
	set.Version = 1
	body, err := yaml.Marshal(set)
	if err != nil {
		return err
	}
	return s.write("config.yaml", append([]byte(header), body...))
}

const header = "# safeslice project state. Written by the wizard.\n" +
	"# Passwords are never stored here: use password_env to name the\n" +
	"# environment variable that holds one.\n"

// Connections lists the saved connections, most recently added last.
func (s *Store) Connections() ([]Connection, error) {
	set, err := s.load()
	return set.Connections, err
}

// AddConnection saves a connection, replacing any with the same name. The DSN
// is stripped of its password first.
func (s *Store) AddConnection(c Connection) error {
	if c.Name == "" {
		return fmt.Errorf("profile: a saved connection needs a name")
	}
	c.DSN = Sanitise(c.DSN)
	set, err := s.load()
	if err != nil {
		return err
	}
	replaced := false
	for i := range set.Connections {
		if set.Connections[i].Name == c.Name {
			set.Connections[i] = c
			replaced = true
		}
	}
	if !replaced {
		set.Connections = append(set.Connections, c)
	}
	return s.save(set)
}

// LastProfile reports the profile used most recently, if any.
func (s *Store) LastProfile() string {
	set, _ := s.load()
	return set.LastProfile
}

// Profiles lists the saved profiles by name.
func (s *Store) Profiles() ([]Profile, error) {
	entries, err := os.ReadDir(filepath.Join(s.dir, "profiles"))
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("profile: reading profiles: %w", err)
	}
	var out []Profile
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".yaml") {
			continue
		}
		p, err := s.Load(strings.TrimSuffix(e.Name(), ".yaml"))
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// Load reads one profile by name.
func (s *Store) Load(name string) (Profile, error) {
	var p Profile
	data, err := os.ReadFile(filepath.Join(s.dir, "profiles", Slug(name)+".yaml"))
	if err != nil {
		return p, fmt.Errorf("profile: %w", err)
	}
	if err := yaml.Unmarshal(data, &p); err != nil {
		return p, fmt.Errorf("profile %s: %w", name, err)
	}
	return p, nil
}

// Save writes a profile and records it as the most recent one.
func (s *Store) Save(p Profile) error {
	if p.Name == "" {
		return fmt.Errorf("profile: a profile needs a name")
	}
	p.Source.DSN = Sanitise(p.Source.DSN)
	p.Target.DSN = Sanitise(p.Target.DSN)
	p.Updated = time.Now().UTC()
	body, err := yaml.Marshal(p)
	if err != nil {
		return err
	}
	if err := s.write(filepath.Join("profiles", Slug(p.Name)+".yaml"),
		append([]byte(header), body...)); err != nil {
		return err
	}
	set, err := s.load()
	if err != nil {
		return err
	}
	set.LastProfile = p.Name
	return s.save(set)
}

// Record appends a run to the history.
func (s *Store) Record(r Run) error {
	if r.At.IsZero() {
		r.At = time.Now().UTC()
	}
	r.Source, r.Target = Sanitise(r.Source), Sanitise(r.Target)
	body, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return err
	}
	name := r.At.UTC().Format("20060102-150405") + ".json"
	return s.write(filepath.Join("history", name), append(body, '\n'))
}

// History returns past runs, newest first.
func (s *Store) History() ([]Run, error) {
	entries, err := os.ReadDir(filepath.Join(s.dir, "history"))
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("profile: reading history: %w", err)
	}
	var out []Run
	for _, e := range entries {
		data, err := os.ReadFile(filepath.Join(s.dir, "history", e.Name()))
		if err != nil {
			continue // a half-written history entry must not break the tool
		}
		var r Run
		if json.Unmarshal(data, &r) == nil {
			out = append(out, r)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].At.After(out[j].At) })
	return out, nil
}

var keyValuePassword = regexp.MustCompile(`(?i)(^|\s)password\s*=\s*('[^']*'|[^\s]*)`)

// Sanitise removes the password from a connection string.
//
// This is the only form of a DSN allowed to be written down. Both syntaxes
// Postgres accepts are handled: the URL form and the key=value form.
func Sanitise(dsn string) string {
	if dsn == "" {
		return ""
	}
	if u, err := url.Parse(dsn); err == nil && u.Scheme != "" && u.Host != "" {
		if u.User != nil {
			if name := u.User.Username(); name != "" {
				u.User = url.User(name)
			} else {
				u.User = nil
			}
		}
		q := u.Query()
		if q.Has("password") {
			q.Del("password")
			u.RawQuery = q.Encode()
		}
		return u.String()
	}
	return strings.TrimSpace(keyValuePassword.ReplaceAllString(dsn, "$1"))
}

// WithPassword puts a password back into a URL-form connection string. A
// key=value DSN is returned unchanged: rewriting one by hand is how quoting
// bugs turn into "authentication failed" reports nobody can reproduce.
func WithPassword(dsn, password string) string {
	if password == "" {
		return dsn
	}
	u, err := url.Parse(dsn)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return dsn
	}
	name := ""
	if u.User != nil {
		name = u.User.Username()
	}
	u.User = url.UserPassword(name, password)
	return u.String()
}

// Slug turns a profile name into a filename.
func Slug(name string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(strings.TrimSpace(name)) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
		case r == '-', r == '_', r == ' ':
			b.WriteByte('-')
		}
	}
	s := strings.Trim(b.String(), "-")
	if s == "" {
		return "profile"
	}
	return s
}
