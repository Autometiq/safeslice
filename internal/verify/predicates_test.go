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

package verify

import (
	"context"
	"fmt"
	"os"
	"testing"

	"github.com/jackc/pgx/v5"
)

// The detectors are SQL predicates, so PostgreSQL's own regex engine is the only
// thing that can tell us whether they are right. Testing them in Go would be
// testing a different implementation.
//
// This exists because of a false positive found only at volume: masked emails
// look like user_97e00477401de7b4@example.invalid, and the phone pattern matched
// "00477401" inside the hex. A twelve-row fixture never hit it; five thousand
// rows produced eight. These cases pin the boundary behaviour deterministically.
func TestPredicatesAgainstPostgres(t *testing.T) {
	dsn := os.Getenv("SAFESLICE_TEST_DSN")
	if dsn == "" {
		t.Skip("set SAFESLICE_TEST_DSN to run predicate tests")
	}
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer conn.Close(ctx)

	cases := []struct {
		kind  string
		value string
		want  bool
		why   string
	}{
		// Real personal data must be found.
		{"email", "reach me at alice@realcompany.com", true, "live address"},
		{"phone", "call +44 7700 900123 tomorrow", true, "spaced international number"},
		{"phone", "+447700900123", true, "compact international number"},
		{"phone", "dial 00447700900123", true, "00 international prefix"},
		{"phone", "tel: 0044 20 7946 0958", true, "spaced 00 prefix"},
		{"ip", "request from 8.8.8.8", true, "routable address"},
		{"payment card", "paid with 4111111111111111", true, "Luhn-valid card"},

		// safeslice's own output must never be flagged.
		{"email", "user_97e00477401de7b4@example.invalid", false, "masked email"},
		{"phone", "user_97e00477401de7b4@example.invalid", false, "hex containing 00 plus digits"},
		{"phone", "user_0400758452f28ba7@example.invalid", false, "hex beginning 04007"},
		{"phone", "user_217265c00807015b@example.invalid", false, "hex containing 00807015"},
		{"phone", "+15551234567", false, "reserved 555 range safeslice emits"},
		{"ip", "203.0.113.42", false, "RFC 5737 TEST-NET-3 safeslice emits"},

		// Ordinary data must not be flagged.
		{"phone", "order 1234567890 shipped", false, "order reference"},
		{"ip", "internal host 10.0.0.5", false, "private range"},
		{"ip", "localhost 127.0.0.1", false, "loopback"},
		{"payment card", "order 1234567890123456", false, "long id failing Luhn"},
		{"email", "no contact details here", false, "plain prose"},
	}

	checks := map[string]Check{}
	for _, c := range Checks() {
		checks[c.Kind] = c
	}

	for _, tc := range cases {
		check, ok := checks[tc.kind]
		if !ok {
			t.Fatalf("no check named %q", tc.kind)
		}
		q := fmt.Sprintf("SELECT (%s)", fmt.Sprintf(check.SQL, "$1::text"))
		var matched bool
		if err := conn.QueryRow(ctx, q, tc.value).Scan(&matched); err != nil {
			t.Fatalf("%s / %s: %v", tc.kind, tc.why, err)
		}
		// Luhn is decided in Go, not SQL, so mirror what Scan does.
		if matched && check.Confirm != nil {
			matched = check.Confirm(tc.value)
		}
		if matched != tc.want {
			t.Errorf("%s check on %q (%s): matched=%v, want %v",
				tc.kind, tc.value, tc.why, matched, tc.want)
		}
	}
}
