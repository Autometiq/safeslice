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
	"strings"
	"testing"
)

func TestLuhnSeparatesCardsFromOrderIds(t *testing.T) {
	// Without the checksum, every long numeric id in the database gets flagged
	// and the scanner becomes noise nobody runs.
	valid := []string{
		"4111111111111111", // Visa test number
		"5500005555555559", // Mastercard test number
		"378282246310005",  // Amex test number
	}
	for _, c := range valid {
		if !luhn(c) {
			t.Errorf("luhn(%s) = false, want true", c)
		}
	}
	invalid := []string{
		"4111111111111112", // one digit off
		"1234567890123456", // a plausible order reference
		"9999999999999999",
	}
	for _, c := range invalid {
		if luhn(c) {
			t.Errorf("luhn(%s) = true; an order id would be reported as a card", c)
		}
	}
}

func TestLuhnRejectsNonDigits(t *testing.T) {
	if luhn("4111-1111-1111-1111") {
		t.Error("non-digit input accepted")
	}
	if luhn("18") {
		t.Error("a two-digit number passing the modulo test was treated as a card")
	}
	if luhn("") {
		// An empty string sums to zero, which is divisible by ten.
		t.Error("empty string treated as a valid card number")
	}
}

func TestContainsLuhnValidFindsEmbeddedCards(t *testing.T) {
	// Cards hide inside free text far more often than they sit in a tidy column.
	if !containsLuhnValid("customer paid with 4111111111111111 on tuesday") {
		t.Error("embedded card number not detected")
	}
	if containsLuhnValid("order 1234567890123456 shipped") {
		t.Error("order reference reported as a card number")
	}
	if containsLuhnValid("no digits here") {
		t.Error("text with no digit run reported as a card")
	}
}

func TestRedactKeepsEnoughToLocateAndNoMore(t *testing.T) {
	// The scanner's own output goes into terminals and CI logs. Printing the
	// values it found would make the leak report a second leak.
	got := redact("alice.smith@realcompany.com")
	if strings.Contains(got, "realcompany") || strings.Contains(got, "alice.smith") {
		t.Errorf("redact leaked the value: %q", got)
	}
	if !strings.HasPrefix(got, "ali") {
		t.Errorf("redact = %q, want a short prefix so the row can be found", got)
	}
	if got := redact("abc"); got != "***" {
		t.Errorf("short value redacted as %q, want full masking", got)
	}
}

func TestRedactFlattensNewlines(t *testing.T) {
	// A multi-line value would otherwise break the results table apart.
	if got := redact("line one\nline two here"); strings.Contains(got, "\n") {
		t.Errorf("redact kept a newline: %q", got)
	}
}

func TestChecksIgnoreSafesliceOwnOutput(t *testing.T) {
	// A scanner that flags the values the tool itself generates is one nobody
	// will keep in their pipeline.
	for _, c := range Checks() {
		switch c.Kind {
		case "email":
			if !strings.Contains(c.SQL, "example\\.(invalid") {
				t.Error("email check does not exclude the .invalid domain safeslice emits")
			}
		case "phone":
			if !strings.Contains(c.SQL, `\+1555`) {
				t.Error("phone check does not exclude the reserved 555 range")
			}
		case "ip":
			if !strings.Contains(c.SQL, "203\\.0\\.113\\.") {
				t.Error("ip check does not exclude RFC 5737 TEST-NET-3")
			}
		}
	}
}

func TestEveryCheckHasAPlaceholder(t *testing.T) {
	// The predicate is formatted with the column expression; a check missing the
	// verb would silently scan the wrong thing.
	for _, c := range Checks() {
		if !strings.Contains(c.SQL, "%[1]s") {
			t.Errorf("check %q has no column placeholder", c.Kind)
		}
	}
}

func TestCardCheckRequiresConfirmation(t *testing.T) {
	for _, c := range Checks() {
		if c.Kind == "payment card" && c.Confirm == nil {
			t.Error("payment card check has no Luhn confirmation; it would flag every long id")
		}
	}
}

func TestNewDetectorsPresent(t *testing.T) {
	kinds := map[string]bool{}
	for _, c := range Checks() {
		kinds[c.Kind] = true
	}
	if !kinds["secret token"] {
		t.Error("missing secret token check")
	}
	if !kinds["national id"] {
		t.Error("missing national id check")
	}
}
