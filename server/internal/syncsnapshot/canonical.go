// Package syncsnapshot holds the csc snapshot v2 wire contract: the canonical
// serialization the whole-snapshot digest is computed over, and the document
// shapes both sides must agree on byte-for-byte.
//
// Why a hand-written serializer instead of encoding/json
// ------------------------------------------------------
// The digest is a cross-implementation contract: a Go server computes it, a
// TypeScript client recomputes it, and a single differing byte makes the client
// discard a snapshot it should have applied. encoding/json cannot be used as
// the reference because it disagrees with RFC 8785 in ways that are invisible
// until real data hits them:
//
//   - it HTML-escapes `<`, `>` and `&` by default (SetEscapeHTML(false) fixes
//     this one),
//   - it escapes U+2028/U+2029 UNCONDITIONALLY — even with escaping disabled —
//     while JCS emits them literally, and
//   - map keys are sorted by Go's byte-wise string order, while JCS sorts by
//     UTF-16 code unit.
//
// The first two are reachable from any user-supplied capability name. So this
// package writes the bytes itself and keeps the deviation surface zero.
//
// Deliberate narrowing of RFC 8785
// --------------------------------
// JCS inherits ECMAScript's Number::toString, which is a genuinely difficult
// algorithm (shortest round-trip decimal, exponent thresholds at 1e21 and
// 1e-7). Rather than implement it and hope both sides agree, this contract
// FORBIDS non-integer numbers: every number in a snapshot is a count, a
// generation, or an index. Integers within ±2^53 have exactly one JCS
// representation and it is plain decimal. A fractional value is rejected at
// serialization time rather than silently producing bytes csc might format
// differently.
package syncsnapshot

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"sort"
	"strconv"
	"unicode/utf16"
)

// MaxSafeInteger is ECMAScript's Number.MAX_SAFE_INTEGER. Beyond it a JSON
// number cannot survive a JavaScript round-trip, so it cannot be part of a
// contract a TypeScript client must reproduce.
const MaxSafeInteger = int64(1)<<53 - 1

// ErrNonIntegerNumber is returned for any fractional or out-of-range number.
var ErrNonIntegerNumber = errors.New("syncsnapshot: only integers within the ECMAScript safe range may appear in a canonical snapshot")

// Object is a JSON object. Key order in the map is irrelevant — Canonicalize
// sorts by UTF-16 code unit, as RFC 8785 requires.
type Object map[string]any

// Array is a JSON array. Element order IS significant and is preserved: the
// contract fixes it (items by item id, tombstones by (itemId, eventId)), so
// the serializer must not reorder.
type Array []any

// Raw is a fragment that is ALREADY canonical and is emitted verbatim.
//
// It exists for one job: a stored snapshot is sliced into pages at the element
// level, and a client reassembling those pages must be able to rebuild the
// exact bytes that were hashed. Re-encoding each element would work only if the
// round-trip were perfect; passing the original bytes through makes it true by
// construction. Nothing that has not already been through Canonicalize may be
// wrapped in Raw.
type Raw []byte

// Canonicalize serializes a value to RFC 8785 canonical JSON.
//
// Accepted: nil, bool, string, int/int32/int64, Object, map[string]any, Array,
// []any, Raw, and json.Number holding an integer. Anything else — notably
// float64 and struct types — is an error rather than a guess.
func Canonicalize(value any) ([]byte, error) {
	var buf bytes.Buffer
	if err := writeCanonical(&buf, value); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// CanonicalizeJSON parses arbitrary JSON and re-serializes it canonically.
//
// This is the operation a client performs: it receives pages, parses them, and
// must arrive at the same bytes the server hashed. Numbers are decoded with
// json.Number so an integer never round-trips through float64 — 2^53+1 would
// otherwise come back as 2^53 and produce a digest that silently matches
// nothing.
func CanonicalizeJSON(raw []byte) ([]byte, error) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		return nil, fmt.Errorf("syncsnapshot: parse json: %w", err)
	}
	// Reject trailing content: "{} {}" must not canonicalize to "{}".
	if _, err := decoder.Token(); err != io.EOF {
		return nil, errors.New("syncsnapshot: canonical input must contain exactly one JSON value")
	}
	return Canonicalize(value)
}

// Digest is the contract's digest primitive: lowercase SHA-256 hex over the
// canonical UTF-8 bytes. It lives next to the serializer so no caller can
// accidentally hash a non-canonical encoding.
func Digest(value any) (canonical []byte, digest string, err error) {
	canonical, err = Canonicalize(value)
	if err != nil {
		return nil, "", err
	}
	return canonical, DigestBytes(canonical), nil
}

func writeCanonical(buf *bytes.Buffer, value any) error {
	switch v := value.(type) {
	case nil:
		buf.WriteString("null")
		return nil
	case bool:
		if v {
			buf.WriteString("true")
		} else {
			buf.WriteString("false")
		}
		return nil
	case string:
		writeCanonicalString(buf, v)
		return nil
	case Raw:
		if len(v) == 0 {
			return errors.New("syncsnapshot: empty Raw fragment")
		}
		buf.Write(v)
		return nil
	case json.RawMessage:
		if len(v) == 0 {
			return errors.New("syncsnapshot: empty RawMessage fragment")
		}
		buf.Write(v)
		return nil
	case int:
		return writeCanonicalInt(buf, int64(v))
	case int32:
		return writeCanonicalInt(buf, int64(v))
	case int64:
		return writeCanonicalInt(buf, v)
	case json.Number:
		parsed, err := strconv.ParseInt(v.String(), 10, 64)
		if err != nil {
			return fmt.Errorf("%w: %q", ErrNonIntegerNumber, v.String())
		}
		return writeCanonicalInt(buf, parsed)
	case float64:
		// Reachable only when a caller bypasses CanonicalizeJSON's UseNumber.
		// Accept an exact integral value, refuse anything else; the point is to
		// fail loudly rather than emit a formatting both sides must guess.
		if v != math.Trunc(v) || math.Abs(v) > float64(MaxSafeInteger) {
			return fmt.Errorf("%w: %v", ErrNonIntegerNumber, v)
		}
		return writeCanonicalInt(buf, int64(v))
	case Array:
		return writeCanonicalArray(buf, v)
	case []any:
		return writeCanonicalArray(buf, v)
	case Object:
		return writeCanonicalObject(buf, v)
	case map[string]any:
		return writeCanonicalObject(buf, v)
	default:
		return fmt.Errorf("syncsnapshot: %T cannot appear in a canonical snapshot", value)
	}
}

func writeCanonicalInt(buf *bytes.Buffer, v int64) error {
	if v > MaxSafeInteger || v < -MaxSafeInteger {
		return fmt.Errorf("%w: %d", ErrNonIntegerNumber, v)
	}
	buf.WriteString(strconv.FormatInt(v, 10))
	return nil
}

func writeCanonicalArray(buf *bytes.Buffer, values []any) error {
	buf.WriteByte('[')
	for i, element := range values {
		if i > 0 {
			buf.WriteByte(',')
		}
		if err := writeCanonical(buf, element); err != nil {
			return err
		}
	}
	buf.WriteByte(']')
	return nil
}

func writeCanonicalObject(buf *bytes.Buffer, object map[string]any) error {
	keys := make([]string, 0, len(object))
	for key := range object {
		keys = append(keys, key)
	}
	sortKeysByUTF16(keys)

	buf.WriteByte('{')
	for i, key := range keys {
		if i > 0 {
			buf.WriteByte(',')
		}
		writeCanonicalString(buf, key)
		buf.WriteByte(':')
		if err := writeCanonical(buf, object[key]); err != nil {
			return err
		}
	}
	buf.WriteByte('}')
	return nil
}

// sortKeysByUTF16 orders keys by UTF-16 code unit, which is what RFC 8785
// specifies and is NOT the same as Go's byte-wise string order.
//
// The two agree for everything below U+10000 and disagree above it: a
// supplementary character encodes as a surrogate pair starting at 0xD800, so
// UTF-16 sorts it BELOW U+E000..U+FFFF, while UTF-8 byte order sorts it above.
// Snapshot keys are ASCII today; this is here so that stays a fact about the
// data rather than an assumption baked into the serializer.
func sortKeysByUTF16(keys []string) {
	encoded := make(map[string][]uint16, len(keys))
	for _, key := range keys {
		encoded[key] = utf16.Encode([]rune(key))
	}
	sort.Slice(keys, func(i, j int) bool {
		return lessUTF16(encoded[keys[i]], encoded[keys[j]])
	})
}

func lessUTF16(a, b []uint16) bool {
	n := len(a)
	if len(b) < n {
		n = len(b)
	}
	for i := 0; i < n; i++ {
		if a[i] != b[i] {
			return a[i] < b[i]
		}
	}
	return len(a) < len(b)
}

// canonicalStringEscapes is RFC 8785 §3.2.2.2's short-escape table. Every other
// control character uses the \u00xx form with LOWERCASE hex; everything at or
// above U+0020 (except `"` and `\`) is emitted as literal UTF-8 — including
// U+2028 and U+2029, which encoding/json escapes and JCS does not.
var canonicalStringEscapes = map[byte]string{
	'"':  `\"`,
	'\\': `\\`,
	'\b': `\b`,
	'\f': `\f`,
	'\n': `\n`,
	'\r': `\r`,
	'\t': `\t`,
}

func writeCanonicalString(buf *bytes.Buffer, s string) {
	buf.WriteByte('"')
	for i := 0; i < len(s); i++ {
		c := s[i]
		if escape, ok := canonicalStringEscapes[c]; ok {
			buf.WriteString(escape)
			continue
		}
		if c < 0x20 {
			buf.WriteString(`\u00`)
			const hexDigits = "0123456789abcdef"
			buf.WriteByte(hexDigits[c>>4])
			buf.WriteByte(hexDigits[c&0x0f])
			continue
		}
		buf.WriteByte(c)
	}
	buf.WriteByte('"')
}
