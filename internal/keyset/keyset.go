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

// Package keyset stores the primary keys collected during traversal.
//
// This is the memory bottleneck of the whole tool. Slicing a large database
// means tracking tens of millions of ids, and holding those in a Go map
// exhausts RAM long before the extraction finishes. Keys therefore live in a
// local SQLite file, which gives deduplication (PRIMARY KEY), bounded memory,
// and a resume point after a crash for the price of one dependency.
//
// Server-side temp tables would be faster, but CREATE TEMP TABLE writes system
// catalogs and so fails inside a read-only transaction and on a hot standby --
// and reading from a replica is the setup we actually want to encourage.
package keyset

import (
	"database/sql"
	"encoding/hex"
	"fmt"
	"strconv"
	"strings"
	"time"

	_ "modernc.org/sqlite" // pure Go: keeps the binary static, no cgo
)

// Key is one primary-key tuple. Composite keys have more than one element.
type Key []any

// Encode renders a key as a string that is unique per value *and* per type.
//
// Each element is length-prefixed rather than delimited. A separator byte would
// be ambiguous the moment a text primary key contained it -- tenant slugs and
// natural keys are user-supplied, so that is a matter of time, and the failure
// mode is a key silently decoding into the wrong number of columns.
//
// JSON is not usable here either: it turns int64 into float64 on the way back,
// corrupting any id above 2^53. Bigserial reaches that range in real systems.
func Encode(k Key) string {
	var b strings.Builder
	for _, v := range k {
		e := encodeValue(v)
		b.WriteString(strconv.Itoa(len(e)))
		b.WriteByte(':')
		b.WriteString(e)
	}
	return b.String()
}

func encodeValue(v any) string {
	switch t := v.(type) {
	case nil:
		return "n"
	case int64:
		return "i:" + strconv.FormatInt(t, 10)
	case int32:
		return "i:" + strconv.FormatInt(int64(t), 10)
	case int:
		return "i:" + strconv.FormatInt(int64(t), 10)
	case string:
		return "s:" + t
	case []byte:
		return "b:" + hex.EncodeToString(t)
	case [16]byte: // pgx returns uuid this way
		return "u:" + hex.EncodeToString(t[:])
	case bool:
		if t {
			return "T"
		}
		return "F"
	case float64:
		return "f:" + strconv.FormatFloat(t, 'g', -1, 64)
	case time.Time:
		// Partitioned tables routinely key on (id, created_at), so a date or
		// timestamp in a primary key is ordinary, not exotic. Without this case
		// the value came back as a Go-formatted string and Postgres rejected it
		// as invalid date syntax.
		return "t:" + t.Format(time.RFC3339Nano)
	default:
		return "x:" + fmt.Sprintf("%v", t)
	}
}

// Decode reverses Encode, restoring the original Go types so the values can be
// used as query parameters again.
func Decode(s string) Key {
	var out Key
	for len(s) > 0 {
		colon := strings.IndexByte(s, ':')
		if colon < 0 {
			break
		}
		n, err := strconv.Atoi(s[:colon])
		if err != nil || colon+1+n > len(s) {
			break
		}
		out = append(out, decodeValue(s[colon+1:colon+1+n]))
		s = s[colon+1+n:]
	}
	return out
}

func decodeValue(p string) any {
	if p == "n" {
		return nil
	}
	if p == "T" {
		return true
	}
	if p == "F" {
		return false
	}
	if len(p) < 2 {
		return p
	}
	body := p[2:]
	switch p[0] {
	case 'i':
		n, err := strconv.ParseInt(body, 10, 64)
		if err != nil {
			return body
		}
		return n
	case 's':
		return body
	case 'b':
		b, err := hex.DecodeString(body)
		if err != nil {
			return body
		}
		return b
	case 'u':
		b, err := hex.DecodeString(body)
		if err != nil || len(b) != 16 {
			return body
		}
		var a [16]byte
		copy(a[:], b)
		return a
	case 'f':
		f, err := strconv.ParseFloat(body, 64)
		if err != nil {
			return body
		}
		return f
	case 't':
		ts, err := time.Parse(time.RFC3339Nano, body)
		if err != nil {
			return body
		}
		return ts
	default:
		return body
	}
}

// Store accumulates keys per table, deduplicating as it goes.
type Store struct {
	db   *sql.DB
	path string
}

// Open creates or reopens a key store. Pass ":memory:" for tests; pass a file
// path for real runs so a large closure spills to disk and a crashed run can be
// resumed rather than restarted.
func Open(path string) (*Store, error) {
	dsn := path
	if path == ":memory:" {
		dsn = ":memory:"
	} else {
		dsn = path + "?_pragma=journal_mode(WAL)&_pragma=synchronous(NORMAL)"
	}
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("keyset: open %s: %w", path, err)
	}
	// One connection: SQLite writers serialise anyway, and a shared in-memory
	// database would otherwise appear empty to a second connection.
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(`
		CREATE TABLE IF NOT EXISTS keys (
			tbl TEXT NOT NULL,
			k   TEXT NOT NULL,
			PRIMARY KEY (tbl, k)
		) WITHOUT ROWID;`); err != nil {
		db.Close()
		return nil, fmt.Errorf("keyset: init schema: %w", err)
	}
	return &Store{db: db, path: path}, nil
}

func (s *Store) Close() error { return s.db.Close() }

// Add records keys for a table and returns only those not already present.
//
// Returning just the new keys is what makes the traversal terminate: a cyclic
// schema keeps rediscovering the same rows, and only expanding the genuinely
// new ones turns that into a fixpoint instead of an infinite loop.
func (s *Store) Add(table string, keys []Key) ([]Key, error) {
	if len(keys) == 0 {
		return nil, nil
	}
	tx, err := s.db.Begin()
	if err != nil {
		return nil, fmt.Errorf("keyset: begin: %w", err)
	}
	defer tx.Rollback()

	stmt, err := tx.Prepare(`INSERT INTO keys (tbl, k) VALUES (?, ?) ON CONFLICT DO NOTHING`)
	if err != nil {
		return nil, fmt.Errorf("keyset: prepare: %w", err)
	}
	defer stmt.Close()

	var fresh []Key
	seen := make(map[string]bool, len(keys)) // collapse duplicates within the batch
	for _, k := range keys {
		enc := Encode(k)
		if seen[enc] {
			continue
		}
		seen[enc] = true
		res, err := stmt.Exec(table, enc)
		if err != nil {
			return nil, fmt.Errorf("keyset: insert: %w", err)
		}
		if n, err := res.RowsAffected(); err == nil && n > 0 {
			fresh = append(fresh, k)
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("keyset: commit: %w", err)
	}
	return fresh, nil
}

// Count returns how many distinct keys are held for a table.
func (s *Store) Count(table string) (int, error) {
	var n int
	err := s.db.QueryRow(`SELECT count(*) FROM keys WHERE tbl = ?`, table).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("keyset: count %s: %w", table, err)
	}
	return n, nil
}

// Tables lists every table holding at least one key, in stable order.
func (s *Store) Tables() ([]string, error) {
	rows, err := s.db.Query(`SELECT DISTINCT tbl FROM keys ORDER BY tbl`)
	if err != nil {
		return nil, fmt.Errorf("keyset: tables: %w", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var t string
		if err := rows.Scan(&t); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// Each streams a table's keys in batches, so the caller never holds more than
// `batch` of them at once. Ordering is stable, which is what makes a resumed
// run pick up where it left off.
func (s *Store) Each(table string, batch int, fn func([]Key) error) error {
	if batch <= 0 {
		batch = 1000
	}
	last := ""
	for {
		rows, err := s.db.Query(
			`SELECT k FROM keys WHERE tbl = ? AND k > ? ORDER BY k LIMIT ?`,
			table, last, batch)
		if err != nil {
			return fmt.Errorf("keyset: scan %s: %w", table, err)
		}
		var chunk []Key
		for rows.Next() {
			var enc string
			if err := rows.Scan(&enc); err != nil {
				rows.Close()
				return err
			}
			last = enc
			chunk = append(chunk, Decode(enc))
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return err
		}
		if len(chunk) == 0 {
			return nil
		}
		if err := fn(chunk); err != nil {
			return err
		}
		if len(chunk) < batch {
			return nil
		}
	}
}
