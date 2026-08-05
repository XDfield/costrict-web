// Package handlers — the `version` field an item projection reports.
//
// Every list and detail projection routes through itemWireVersion below, and it
// is the only place allowed to decide that value, because the device compares
// the two against each other: it reads the version off the LIST, and it writes
// the version it got from the DETAIL into its local item.json. Two projections
// that disagree do not merely look inconsistent — they make the device reinstall
// the same capability on every poll, forever.

package handlers

import (
	"strings"

	"github.com/costrict/costrict-web/server/internal/models"
)

// gitBackedVersionSHALen is how much of the commit id the wire version carries.
// Seven is git's own abbreviation length, which is what the repository UI and
// the commit links next to it already show.
const gitBackedVersionSHALen = 7

// itemWireVersion is the `version` field an item projection reports.
//
// DB-backed rows get the stored column, byte for byte. Nothing about them
// changes.
//
// A Git-backed row gets the manifest's version with the bound commit appended
// as semver build metadata — `1.2.0+e7dea05`. The suffix is there because of
// what the device does with this field: it is the ONLY signal it consults to
// decide whether an installed capability is out of date. csc compares the list
// value against the version in its local item.json as an opaque string
// (src/costrict/favorite/favorite.ts:1180-1186) and consults neither updatedAt
// nor contentMd5 nor gitSha. Under the Git-backed rules an author is expected
// to change a capability's body without touching the manifest's `version` —
// that is the stated semantics, not an oversight — so reporting the manifest
// value alone leaves every device pinned to the copy it installed first,
// silently and with no signal that anything is behind.
//
// The value is a pure function of (manifest version, git_sha): identical while
// the row's commit is, different once it moves. That determinism is a
// requirement, not a nicety — a value that varied per request would make the
// device tear down and reinstall the capability every poll.
//
// This is a projection only. capability_items.version keeps the manifest value,
// which is the repository's truth; the column is Git-owned (see
// models.gitOwnedCapabilityColumns), so a client that echoes this string back
// through an update is refused rather than allowed to persist the suffix.
func itemWireVersion(item *models.CapabilityItem) string {
	if !isGitBacked(item) {
		return item.Version
	}
	if len(item.GitSHA) < gitBackedVersionSHALen {
		// Bound but never synced: there is no commit to anchor to yet, and an
		// invented anchor would point the device at a version that means
		// nothing. The row has no servable content at this stage either.
		return item.Version
	}
	short := item.GitSHA[:gitBackedVersionSHALen]
	if item.Version == "" {
		return short
	}
	if strings.HasSuffix(item.Version, "+"+short) || strings.HasSuffix(item.Version, "."+short) {
		return item.Version
	}
	// Semver permits one `+`, followed by dot-separated identifiers. A manifest
	// that already carries build metadata gets the commit added to it rather
	// than a second `+`, so the result stays parseable for anyone who parses it.
	if strings.Contains(item.Version, "+") {
		return item.Version + "." + short
	}
	return item.Version + "+" + short
}
