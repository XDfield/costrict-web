package syncsnapshot

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func TestCanonicalize_SortsKeysAndOmitsWhitespace(t *testing.T) {
	got, err := Canonicalize(Object{"b": 2, "a": 1, "C": 3, "": 0})
	if err != nil {
		t.Fatalf("canonicalize: %v", err)
	}
	// Uppercase sorts before lowercase in both UTF-16 and ASCII; the empty key
	// sorts first because it is a prefix of everything.
	if want := `{"":0,"C":3,"a":1,"b":2}`; string(got) != want {
		t.Fatalf("canonical = %s, want %s", got, want)
	}
}

// The three encoding/json deviations that make it unusable as the reference
// implementation. Each of these is reachable from a user-chosen capability
// name, so each would produce a digest csc could not reproduce.
func TestCanonicalize_DivergesFromEncodingJSONWhereRFC8785Requires(t *testing.T) {
	cases := []struct {
		name  string
		input string
		want  string
	}{
		{"HTML characters are literal", "<a> & </a>", "\"<a> & </a>\""},
		{"U+2028 and U+2029 are literal", "a b c", "\"a b c\""},
		{"short escapes", "q\" b\\ t\t n\n r\r bs\b ff\f", `"q\" b\\ t\t n\n r\r bs\b ff\f"`},
		{"other control characters use lowercase \\u00xx", "\x00\x1f", `"\u0000\u001f"`},
		{"DEL and U+0080 are literal", "\u007f\u0080", "\"\u007f\u0080\""},
		{"non-ASCII passes through as UTF-8", "中文 \U0001D11E", "\"中文 \U0001D11E\""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := Canonicalize(tc.input)
			if err != nil {
				t.Fatalf("canonicalize: %v", err)
			}
			if string(got) != tc.want {
				t.Fatalf("canonical = %q, want %q", got, tc.want)
			}
		})
	}

	// Demonstrate the deviation rather than assert it in prose: encoding/json
	// with HTML escaping disabled still escapes U+2028, so a snapshot digest
	// computed with the stdlib encoder would not match csc's.
	var buf bytes.Buffer
	encoder := json.NewEncoder(&buf)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode("a b"); err != nil {
		t.Fatalf("encode: %v", err)
	}
	if stdlib := strings.TrimSpace(buf.String()); stdlib == "\"a b\"" {
		t.Fatalf("encoding/json no longer escapes U+2028 (%q); the hand-written serializer's rationale needs revisiting", stdlib)
	}
}

func TestCanonicalize_RejectsNonIntegerNumbers(t *testing.T) {
	for _, value := range []any{1.5, float64(MaxSafeInteger) * 4, json.Number("1.5"), MaxSafeInteger + 1} {
		if _, err := Canonicalize(value); !errors.Is(err, ErrNonIntegerNumber) {
			t.Fatalf("Canonicalize(%v) err = %v, want ErrNonIntegerNumber", value, err)
		}
	}
}

func TestCanonicalize_RejectsUnsupportedTypes(t *testing.T) {
	type unexpected struct{ A int }
	if _, err := Canonicalize(unexpected{A: 1}); err == nil {
		t.Fatal("a struct must not silently canonicalize; the contract only carries explicit Object/Array values")
	}
}

// The client's operation: parse the wire bytes, re-canonicalize, and land on
// the same bytes. If this were not idempotent, csc could only verify a digest
// by preserving raw text, which no JSON client does.
func TestCanonicalizeJSON_IsIdempotentOverTheFixture(t *testing.T) {
	fixture, err := LoadDigestFixture()
	if err != nil {
		t.Fatalf("load fixture: %v", err)
	}
	reparsed, err := CanonicalizeJSON([]byte(fixture.Expected.Canonical))
	if err != nil {
		t.Fatalf("re-canonicalize: %v", err)
	}
	if string(reparsed) != fixture.Expected.Canonical {
		t.Fatalf("parse+recanonicalize changed the bytes:\n got %s\nwant %s", reparsed, fixture.Expected.Canonical)
	}
	if got := DigestBytes(reparsed); got != fixture.Expected.SnapshotDigest {
		t.Fatalf("digest after reparse = %s, want %s", got, fixture.Expected.SnapshotDigest)
	}
}

// A 2^53-scale integer must survive parsing. With encoding/json's default
// float64 decoding it would not, and the resulting digest would differ from the
// server's while looking perfectly well-formed.
func TestCanonicalizeJSON_PreservesLargeIntegers(t *testing.T) {
	got, err := CanonicalizeJSON([]byte(`{"generation":9007199254740991}`))
	if err != nil {
		t.Fatalf("canonicalize: %v", err)
	}
	if string(got) != `{"generation":9007199254740991}` {
		t.Fatalf("canonical = %s, want the integer unchanged", got)
	}
	if _, err := CanonicalizeJSON([]byte(`{"generation":9007199254740992}`)); !errors.Is(err, ErrNonIntegerNumber) {
		t.Fatalf("a number beyond the safe range must be rejected, got %v", err)
	}
}

func TestCanonicalizeJSON_RejectsTrailingContent(t *testing.T) {
	if _, err := CanonicalizeJSON([]byte(`{} {}`)); err == nil {
		t.Fatal("two concatenated documents must not canonicalize to one")
	}
}
