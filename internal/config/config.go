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

// Package config loads safeslice.yaml.
//
// The file is meant to be committed to the repository. That is the adoption
// mechanism: once one engineer commits a config, every teammate and every CI
// job gets the same masking rules and the same slice shape for free.
package config

import (
	"fmt"
	"os"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/Autometiq/safeslice/internal/catalog"
	"github.com/Autometiq/safeslice/internal/mask"
)

const DefaultPath = "safeslice.yaml"

type Config struct {
	Version int    `yaml:"version"`
	Source  Source `yaml:"source"`
	Slice   Slice  `yaml:"slice"`
	Mask    Mask   `yaml:"mask"`

	// Verify specifies custom post-slice leak detection checks and column ignores.
	Verify Verify `yaml:"verify"`

	// VirtualKeys declare relationships Postgres does not enforce. Rails
	// polymorphic associations and Django generic relations have no
	// pg_constraint row, so without these the graph cannot see them and the
	// slice comes out silently incomplete.
	VirtualKeys []VirtualKey `yaml:"virtual_keys"`
}

type Verify struct {
	Checks []CustomCheck `yaml:"checks"`
	Ignore []string      `yaml:"ignore"`
}

type CustomCheck struct {
	Name    string `yaml:"name"`
	Pattern string `yaml:"pattern"`
}

type Source struct {
	Schemas []string `yaml:"schemas"`
}

type Slice struct {
	Root          string  `yaml:"root"`
	Where         string  `yaml:"where"`
	Limit         int     `yaml:"limit"`
	SamplePercent float64 `yaml:"sample_percent"`
	ChildDepth    int     `yaml:"child_depth"`
}

type Mask struct {
	// Seed makes a run reproducible. Sharing it across a team makes everyone's
	// snapshots agree on what a given customer was renamed to.
	Seed string `yaml:"seed"`

	// Strict halts when a text column in a PII table has no rule. Silent
	// pass-through is exactly how a leak reaches a laptop, so the default is on.
	Strict *bool `yaml:"strict"`

	// PIITables limits strict mode to the tables that actually hold personal
	// data. Empty means every table is checked.
	PIITables []string `yaml:"pii_tables"`

	// Rules override the name-based classifier, keyed "schema.table.column",
	// "table.column" or bare "column".
	Rules map[string]string `yaml:"rules"`
}

type VirtualKey struct {
	Table      string    `yaml:"table"`
	Columns    []string  `yaml:"columns"`
	References Reference `yaml:"references"`
	// When scopes the relationship, e.g. "commentable_type = 'Post'".
	When string `yaml:"when"`
}

type Reference struct {
	Table   string   `yaml:"table"`
	Columns []string `yaml:"columns"`
}

// Default returns the configuration used when no file is present.
func Default() *Config {
	strict := true
	return &Config{
		Version: 1,
		Source:  Source{Schemas: []string{"public"}},
		Slice:   Slice{Limit: 1000, ChildDepth: 1},
		Mask:    Mask{Seed: "safeslice", Strict: &strict},
	}
}

// Load reads a config file. A missing file at the default path is not an error:
// the tool has to be useful before anyone has written any configuration.
func Load(path string) (*Config, error) {
	cfg := Default()
	explicit := path != ""
	if path == "" {
		path = DefaultPath
	}
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) && !explicit {
		return cfg, nil
	}
	if err != nil {
		return nil, fmt.Errorf("config: %w", err)
	}
	// KnownFields turns a typo like `pii_table:` into an error instead of a
	// setting that silently does nothing -- which, for a masking rule, means a
	// silent leak.
	dec := yaml.NewDecoder(strings.NewReader(string(data)))
	dec.KnownFields(true)
	if err := dec.Decode(cfg); err != nil {
		return nil, fmt.Errorf("config %s: %w", path, err)
	}
	if err := cfg.Validate(); err != nil {
		return nil, fmt.Errorf("config %s: %w", path, err)
	}
	return cfg, nil
}

func (c *Config) Validate() error {
	if c.Version != 0 && c.Version != 1 {
		return fmt.Errorf("unsupported version %d", c.Version)
	}
	if len(c.Source.Schemas) == 0 {
		c.Source.Schemas = []string{"public"}
	}
	if c.Slice.Limit < 0 {
		return fmt.Errorf("slice.limit must not be negative")
	}
	if c.Slice.ChildDepth < 0 {
		return fmt.Errorf("slice.child_depth must not be negative")
	}
	if c.Slice.SamplePercent < 0 || c.Slice.SamplePercent > 100 {
		return fmt.Errorf("slice.sample_percent must be between 0 and 100")
	}
	for key, rule := range c.Mask.Rules {
		if _, err := mask.ParseRule(rule); err != nil {
			return fmt.Errorf("mask.rules[%q]: %w", key, err)
		}
	}
	for i, vk := range c.VirtualKeys {
		switch {
		case vk.Table == "":
			return fmt.Errorf("virtual_keys[%d]: table is required", i)
		case vk.References.Table == "":
			return fmt.Errorf("virtual_keys[%d]: references.table is required", i)
		case len(vk.Columns) == 0:
			return fmt.Errorf("virtual_keys[%d]: columns is required", i)
		case len(vk.Columns) != len(vk.References.Columns):
			return fmt.Errorf("virtual_keys[%d]: %d columns reference %d columns; they must match",
				i, len(vk.Columns), len(vk.References.Columns))
		}
	}
	return nil
}

// StrictEnabled reports whether strict masking applies. Defaults to true.
func (c *Config) StrictEnabled() bool { return c.Mask.Strict == nil || *c.Mask.Strict }

// StrictFor reports whether a table is subject to strict masking. An empty
// pii_tables list means every table is.
func (c *Config) StrictFor(ref catalog.Ref) bool {
	if !c.StrictEnabled() {
		return false
	}
	if len(c.Mask.PIITables) == 0 {
		return true
	}
	for _, t := range c.Mask.PIITables {
		if t == ref.Name || t == ref.String() {
			return true
		}
	}
	return false
}

// Classifier builds the masking classifier from the configured overrides.
func (c *Config) Classifier() mask.Classifier {
	out := mask.Classifier{Overrides: map[string]mask.Rule{}}
	for key, rule := range c.Mask.Rules {
		r, err := mask.ParseRule(rule)
		if err != nil {
			continue // Validate already rejected these
		}
		out.Overrides[key] = r
	}
	return out
}

// FKs converts the declared virtual keys into graph edges. Unqualified table
// names resolve against the first configured schema, which is what a developer
// writing `table: comments` means.
func (c *Config) FKs() []catalog.FK {
	schema := "public"
	if len(c.Source.Schemas) > 0 {
		schema = c.Source.Schemas[0]
	}
	out := make([]catalog.FK, 0, len(c.VirtualKeys))
	for i, vk := range c.VirtualKeys {
		out = append(out, catalog.FK{
			Name:       fmt.Sprintf("virtual_%d_%s", i, vk.Table),
			Table:      qualify(vk.Table, schema),
			Columns:    vk.Columns,
			RefTable:   qualify(vk.References.Table, schema),
			RefColumns: vk.References.Columns,
			Virtual:    true,
			When:       vk.When,
			// A virtual key has no constraint behind it, so nothing enforces it
			// at load time and nothing needs deferring.
			Deferrable: true,
		})
	}
	return out
}

func qualify(name, schema string) catalog.Ref {
	if i := strings.IndexByte(name, '.'); i >= 0 {
		return catalog.Ref{Schema: name[:i], Name: name[i+1:]}
	}
	return catalog.Ref{Schema: schema, Name: name}
}

// Root returns the configured root table, qualified.
func (c *Config) Root() catalog.Ref {
	schema := "public"
	if len(c.Source.Schemas) > 0 {
		schema = c.Source.Schemas[0]
	}
	return qualify(c.Slice.Root, schema)
}
