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

package graph

import (
	"strings"
	"testing"

	"github.com/Autometiq/safeslice/internal/catalog"
)

// table builds a catalog entry from column names, with `id` as the key.
func table(name string, cols ...string) *catalog.Table {
	t := &catalog.Table{Ref: ref(name), PK: []string{"id"}}
	for _, c := range append([]string{"id"}, cols...) {
		t.Columns = append(t.Columns, catalog.Column{Name: c, Type: "integer", Num: len(t.Columns) + 1})
	}
	return t
}

// A Rails-shaped schema: comments are polymorphic, orders.user_id has a real
// foreign key, and events.account_id has nothing behind it at all.
func softFixture() *catalog.Catalog {
	c := &catalog.Catalog{Tables: map[string]*catalog.Table{}}
	for _, t := range []*catalog.Table{
		table("users"),
		table("posts", "user_id"),
		table("orders", "user_id"),
		table("events", "account_id"),
		table("comments", "commentable_id", "commentable_type"),
	} {
		c.Tables[t.Ref.String()] = t
	}
	c.FKs = []catalog.FK{
		fk("orders_user_fk", "orders", []string{"user_id"}, "users", []string{"id"}),
		fk("posts_user_fk", "posts", []string{"user_id"}, "users", []string{"id"}),
	}
	return c
}

func TestSoftKeysFindsWhatTheSchemaDoesNotDeclare(t *testing.T) {
	got := SoftKeys(softFixture(), softFixture().FKs)

	var found []string
	for _, s := range got {
		found = append(found, s.Table.Name+"."+s.Column)
	}
	want := []string{"comments.commentable_id", "events.account_id"}
	if len(found) != len(want) {
		t.Fatalf("SoftKeys = %v, want %v", found, want)
	}
	for i := range want {
		if found[i] != want[i] {
			t.Errorf("SoftKeys[%d] = %q, want %q", i, found[i], want[i])
		}
	}
}

func TestSoftKeysNamesThePolymorphicDiscriminator(t *testing.T) {
	// This is the Rails case that silently produces an incomplete slice, so the
	// report has to say what the column pairs with.
	for _, s := range SoftKeys(softFixture(), softFixture().FKs) {
		if s.Column != "commentable_id" {
			continue
		}
		if s.TypeColumn != "commentable_type" {
			t.Errorf("TypeColumn = %q, want commentable_type", s.TypeColumn)
		}
		if !strings.Contains(s.String(), "polymorphic") {
			t.Errorf("String() = %q, want it to say polymorphic", s.String())
		}
		return
	}
	t.Fatal("comments.commentable_id was not reported")
}

func TestSoftKeysGuessesTheTargetTable(t *testing.T) {
	c := softFixture()
	c.Tables[ref("accounts").String()] = table("accounts")
	for _, s := range SoftKeys(c, c.FKs) {
		if s.Column == "account_id" && s.Guess.Name != "accounts" {
			t.Errorf("Guess = %q, want accounts", s.Guess.Name)
		}
	}
}

func TestSoftKeysStopsReportingWhatAVirtualKeyCovers(t *testing.T) {
	// Once a user has declared the relationship, repeating the warning is noise.
	c := softFixture()
	fks := append(append([]catalog.FK{}, c.FKs...),
		catalog.FK{Name: "virtual_0", Table: ref("comments"), Columns: []string{"commentable_id"},
			RefTable: ref("posts"), RefColumns: []string{"id"}, Virtual: true})

	for _, s := range SoftKeys(c, fks) {
		if s.Column == "commentable_id" {
			t.Error("a declared virtual key is still reported as unenforced")
		}
	}
}

func TestSoftKeysIgnoresPrimaryKeys(t *testing.T) {
	c := &catalog.Catalog{Tables: map[string]*catalog.Table{}}
	// A join table keyed on the two columns that are also its foreign keys.
	jt := &catalog.Table{Ref: ref("memberships"), PK: []string{"user_id", "team_id"},
		Columns: []catalog.Column{{Name: "user_id", Type: "integer"}, {Name: "team_id", Type: "integer"}}}
	c.Tables[jt.Ref.String()] = jt
	if got := SoftKeys(c, nil); len(got) != 0 {
		t.Errorf("SoftKeys = %v, want nothing: those columns are the key", got)
	}
}
