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

package keyset

import (
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func open(t *testing.T) *Store {
	t.Helper()
	s, err := Open(":memory:")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestBigIntIdsSurviveRoundTrip(t *testing.T) {
	// 2^53+1 is where JSON encoding would start silently corrupting ids.
	// Bigserial reaches this range in real systems and the damage would look
	// like randomly missing rows, not like an error.
	big := int64(9007199254740993)
	got := Decode(Encode(Key{big}))
	if len(got) != 1 || got[0] != big {
		t.Fatalf("round trip gave %#v, want %d", got, big)
	}
	if _, ok := got[0].(int64); !ok {
		t.Errorf("id came back as %T, want int64", got[0])
	}
}

func TestEncodeDistinguishesTypes(t *testing.T) {
	// "1" and 1 are different keys. Encoding them the same way would make the
	// traversal skip real rows it had never actually visited.
	if Encode(Key{int64(1)}) == Encode(Key{"1"}) {
		t.Error("integer and string keys encode identically")
	}
	if Encode(Key{nil}) == Encode(Key{"n"}) {
		t.Error("NULL and the string \"n\" encode identically")
	}
}

func TestEncodeRoundTripsEveryKeyType(t *testing.T) {
	uuid := [16]byte{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16}
	cases := []Key{
		{int64(42)},
		{"tenant-a"},
		{nil},
		{true},
		{false},
		{3.5},
		{[]byte{0xde, 0xad}},
		{uuid},
		{int64(7), "line"}, // composite
		// Partitioned tables commonly key on (id, created_at); a timestamp in a
		// primary key must survive the round trip as a time.Time, or it is sent
		// back to Postgres as text and rejected as invalid date syntax.
		{int64(7), time.Date(2026, 3, 2, 0, 0, 0, 0, time.UTC)},
	}
	for _, want := range cases {
		got := Decode(Encode(want))
		if !reflect.DeepEqual(got, want) {
			t.Errorf("round trip of %#v gave %#v", want, got)
		}
	}
}

func TestEncodeHandlesAdversarialStrings(t *testing.T) {
	// Tenant slugs and natural keys are user-supplied. A key value containing
	// the encoder's own punctuation must still decode into the right number of
	// columns, or rows silently go missing from the slice.
	for _, k := range []Key{
		{"a\x1fb", int64(1)}, // the old separator byte
		{"3:xx", int64(1)},   // looks like a length prefix
		{"", int64(1)},       // empty string
		{"i:99", "s:hello"},  // looks like encoded values
		{strings.Repeat("x", 300), int64(1)},
	} {
		got := Decode(Encode(k))
		if !reflect.DeepEqual(got, k) {
			t.Errorf("round trip of %#v gave %#v", k, got)
		}
	}
}

func TestAddReturnsOnlyNewKeys(t *testing.T) {
	s := open(t)
	first, err := s.Add("users", []Key{{int64(1)}, {int64(2)}})
	if err != nil || len(first) != 2 {
		t.Fatalf("first Add = %v, %v; want 2 new keys", first, err)
	}
	// This is what terminates traversal on a cyclic schema: rediscovered rows
	// must not be expanded again.
	second, err := s.Add("users", []Key{{int64(1)}, {int64(2)}, {int64(3)}})
	if err != nil {
		t.Fatalf("second Add: %v", err)
	}
	if len(second) != 1 || second[0][0] != int64(3) {
		t.Errorf("second Add = %v, want just [3]", second)
	}
}

func TestAddCollapsesDuplicatesWithinOneBatch(t *testing.T) {
	s := open(t)
	got, err := s.Add("users", []Key{{int64(1)}, {int64(1)}, {int64(1)}})
	if err != nil {
		t.Fatalf("Add: %v", err)
	}
	if len(got) != 1 {
		t.Errorf("Add returned %d keys for one repeated id, want 1", len(got))
	}
}

func TestTablesAreIsolated(t *testing.T) {
	s := open(t)
	if _, err := s.Add("users", []Key{{int64(1)}}); err != nil {
		t.Fatal(err)
	}
	// Same id, different table: must not be treated as already seen.
	fresh, err := s.Add("orders", []Key{{int64(1)}})
	if err != nil || len(fresh) != 1 {
		t.Errorf("id 1 in orders = %v, %v; want it treated as new", fresh, err)
	}
	tables, err := s.Tables()
	if err != nil || !reflect.DeepEqual(tables, []string{"orders", "users"}) {
		t.Errorf("Tables = %v, %v; want [orders users]", tables, err)
	}
}

func TestCount(t *testing.T) {
	s := open(t)
	if _, err := s.Add("users", []Key{{int64(1)}, {int64(2)}, {int64(2)}}); err != nil {
		t.Fatal(err)
	}
	if n, err := s.Count("users"); err != nil || n != 2 {
		t.Errorf("Count = %d, %v; want 2", n, err)
	}
	if n, _ := s.Count("absent"); n != 0 {
		t.Errorf("Count of unknown table = %d, want 0", n)
	}
}

func TestEachStreamsEveryKeyExactlyOnce(t *testing.T) {
	s := open(t)
	var want []Key
	for i := range 250 {
		want = append(want, Key{int64(i)})
	}
	if _, err := s.Add("users", want); err != nil {
		t.Fatal(err)
	}
	seen := map[int64]int{}
	batches := 0
	// A small batch size is the point: memory stays bounded regardless of how
	// many keys the closure collected.
	if err := s.Each("users", 40, func(chunk []Key) error {
		batches++
		if len(chunk) > 40 {
			t.Errorf("batch of %d exceeds the requested 40", len(chunk))
		}
		for _, k := range chunk {
			seen[k[0].(int64)]++
		}
		return nil
	}); err != nil {
		t.Fatalf("Each: %v", err)
	}
	if len(seen) != 250 {
		t.Errorf("streamed %d distinct keys, want 250", len(seen))
	}
	for id, n := range seen {
		if n != 1 {
			t.Fatalf("key %d streamed %d times, want once", id, n)
		}
	}
	if batches < 6 {
		t.Errorf("got %d batches, want the work split across several", batches)
	}
}

func TestEachOnEmptyTableDoesNothing(t *testing.T) {
	s := open(t)
	called := false
	if err := s.Each("nope", 10, func([]Key) error { called = true; return nil }); err != nil {
		t.Fatalf("Each: %v", err)
	}
	if called {
		t.Error("callback ran for a table with no keys")
	}
}

func TestEachPropagatesCallbackError(t *testing.T) {
	s := open(t)
	if _, err := s.Add("users", []Key{{int64(1)}}); err != nil {
		t.Fatal(err)
	}
	boom := errString("extract failed")
	if err := s.Each("users", 10, func([]Key) error { return boom }); err != boom {
		t.Errorf("Each swallowed the callback error: %v", err)
	}
}

type errString string

func (e errString) Error() string { return string(e) }

func TestPersistsToDiskForResume(t *testing.T) {
	path := filepath.Join(t.TempDir(), "keys.db")
	s, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if _, err := s.Add("users", []Key{{int64(1)}, {int64(2)}}); err != nil {
		t.Fatal(err)
	}
	s.Close()

	// Reopening is what makes --resume possible after a crash mid-extraction.
	again, err := Open(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer again.Close()
	if n, err := again.Count("users"); err != nil || n != 2 {
		t.Errorf("after reopen Count = %d, %v; want 2", n, err)
	}
	fresh, err := again.Add("users", []Key{{int64(2)}, {int64(3)}})
	if err != nil || len(fresh) != 1 {
		t.Errorf("resumed Add = %v, %v; want only id 3 treated as new", fresh, err)
	}
}
