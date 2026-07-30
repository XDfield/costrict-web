// ShortID generation: platform-wide compact handle derived from SubjectID.
//
// Single source of truth — every subsystem (Gitea login, future integrations)
// reads this from the user row / user.created payload rather than recomputing
// its own. Deterministic; same SubjectID always produces the same ShortID.
//
// Shape: "u-" + base62(SHA256(subject_id)[:6] mod 62^8) = 10 chars total.
//
// Why these numbers:
//   * 6 bytes of SHA256 → 48 bits of input entropy.
//   * 8 base62 chars cover 62^8 ≈ 2.18e14 values ≈ 47.6 bits. Taking the
//     input mod 62^8 loses ~0.4 bits (collision ceiling drops from 16.7M to
//     ~13M users) but guarantees fixed-length output — a property every
//     downstream consumer (Gitea username, column width, log scrape) relies
//     on. The partial UNIQUE INDEX on short_id (WHERE short_id <> '') catches
//     any real collision as an INSERT error rather than silent mis-bind.

package user

import (
	"crypto/sha256"
)

// shortIDHashBytes controls hash truncation. 6 bytes = 48 bits of input.
const shortIDHashBytes = 6

// shortIDEncodedLen is the fixed output length in base62 chars. 8 chars hold
// 62^8 values; combined with the mod in BuildShortID this pins output length.
const shortIDEncodedLen = 8

// alphabet is base62 (0-9 a-z A-Z). Chosen over base58 because Gitea usernames
// allow alnum + `-`/`_` — no need to strip ambiguous chars.
const alphabet = "0123456789abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ"

// shortIDMod caps the input so the encoder always emits exactly
// shortIDEncodedLen chars. Equal to 62^shortIDEncodedLen.
const shortIDMod uint64 = 218340105584896 // 62^8

// BuildShortID derives the platform short_id from a subject_id. Deterministic;
// same subject_id always produces the same short_id. Caller MUST validate the
// subject_id is non-empty before calling.
func BuildShortID(subjectID string) string {
	if subjectID == "" {
		subjectID = "anonymous"
	}
	sum := sha256.Sum256([]byte(subjectID))
	// Read N bytes as big-endian uint64, then mod into the representable
	// range. uint64 holds up to 8 bytes so 6 is well within capacity.
	var n uint64
	for _, b := range sum[:shortIDHashBytes] {
		n = (n << 8) | uint64(b)
	}
	n %= shortIDMod
	out := make([]byte, shortIDEncodedLen)
	const base uint64 = 62
	for i := shortIDEncodedLen - 1; i >= 0; i-- {
		out[i] = alphabet[n%base]
		n /= base
	}
	return "u-" + string(out)
}
