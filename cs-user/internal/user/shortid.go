// ShortID generation: platform-wide compact handle derived from SubjectID.
//
// Single source of truth — every subsystem (Gitea login, future integrations)
// reads this from the user row / user.created payload rather than recomputing
// its own. Algorithm matches server's pre-refactor buildGitUsername exactly,
// so backfill / cross-system joins are stable.
//
// Shape: "u-" + first 16 hex chars of SHA256(subject_id) = 18 chars total.

package user

import (
	"crypto/sha256"
	"encoding/hex"
)

// shortIDHashBytes controls hash truncation. 8 bytes = 64 bits → 16 hex chars.
// 64-bit floor gives ~4 billion before birthday collisions; the users table
// has a UNIQUE index on short_id so any real collision surfaces as an insert
// error rather than silent mis-bind.
const shortIDHashBytes = 8

// BuildShortID derives the platform short_id from a subject_id. Deterministic;
// same subject_id always produces the same short_id. Caller MUST validate the
// subject_id is non-empty before calling.
func BuildShortID(subjectID string) string {
	if subjectID == "" {
		subjectID = "anonymous"
	}
	sum := sha256.Sum256([]byte(subjectID))
	return "u-" + hex.EncodeToString(sum[:shortIDHashBytes])
}
