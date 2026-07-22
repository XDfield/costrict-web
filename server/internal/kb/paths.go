// Package kb implements the KB repo path algorithm from
// KB_REPO_PATH_ALGORITHM.md v2.0.
//
// §A: kbRepoPath(code_repo_url, team_id) → "t-<team_short>/kb-<host>__<joined_segments>"
//
// Deterministic pure function — same inputs in / same outputs out, no DB, no
// cache. The HTTP handler layer composes the algorithm output with the
// tenant Gitea base_url to produce kb_clone_url / kb_web_url.

package kb

import (
	"crypto/sha1"
	"encoding/hex"
	"errors"
	"net/url"
	"regexp"
	"strings"
)

// AlgorithmVersion is reported in the ensure response so clients can detect
// future algorithm bumps. Bump only when the output format changes (see
// KB_REPO_PATH_ALGORITHM.md §7.2 — a v3 would require server-side alias
// table or migration).
const AlgorithmVersion = "v2"

// Sentinel errors. Handlers map these to HTTP codes via errors.Is.
var (
	// ErrInvalidTeamID — team_id wasn't a UUID we can derive team_short from.
	ErrInvalidTeamID = errors.New("kb: team_id must be a UUID")
	// ErrInvalidURL — code_repo_url missing scheme, non-http(s) scheme, or
	// bare host with no path.
	ErrInvalidURL = errors.New("kb: code_repo_url must be a valid http(s) URL with a path")
	// ErrHostTooLong — host component is so long the truncation budget goes
	// negative. Extremely rare in practice.
	ErrHostTooLong = errors.New("kb: code_repo_url host too long")
)

// uuidRe mirrors teamns.uuidRe / workflow.uuidRe; duplicated to keep this
// package leaf-level (no cross-package dep just for a regex).
var uuidRe = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

// allowedSegmentChar matches the per-char whitelist from §3.A.5 of the
// workflow spec; KB uses the same escape rules (see KB_REPO_PATH_ALGORITHM.md
// §3.5 step 5). Mirrors workflow.allowedSlugChar — same character set.
var allowedSegmentChar = regexp.MustCompile(`[a-z0-9._-]`)

// TeamShort derives the 8-hex team_short from a UUID team_id per §3.0.
// Strips hyphens, takes the first 8 hex chars, lowercases.
func TeamShort(teamID string) (string, error) {
	if !uuidRe.MatchString(teamID) {
		return "", ErrInvalidTeamID
	}
	hex := strings.ReplaceAll(teamID, "-", "")
	return strings.ToLower(hex[:8]), nil
}

// KBRepoPath returns "<owner>/<repo>" for the KB repo backing the given
// (code_repo_url, team_id). Implements the full §3 algorithm:
//
//  1. Derive team_short from team_id (§3.0).
//  2. Parse code_repo_url; require http(s) scheme + non-empty path (§3.1).
//  3. Lowercase host; lowercase path, strip .git suffix and trailing /
//     (§3.2-3.3).
//  4. Split path into segments, filter empty (§3.4 step 4).
//  5. Escape each segment per §3.5 step 5 ([a-z0-9._-] whitelist, "."
//     prefix → "_", empty → "_").
//  6. Join with "__", prepend "kb-<host>__", prepend "t-<team_short>/"
//     (§3.6 step 6).
//  7. Truncate if repo_part exceeds Gitea's 64-char limit (§6), keeping an
//     8-char SHA-1 suffix for collision safety.
func KBRepoPath(codeRepoURL, teamID string) (string, error) {
	short, err := TeamShort(teamID)
	if err != nil {
		return "", err
	}
	owner := "t-" + short

	parsed, err := url.Parse(codeRepoURL)
	if err != nil {
		return "", ErrInvalidURL
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return "", ErrInvalidURL
	}
	host := strings.ToLower(parsed.Host)
	if host == "" {
		return "", ErrInvalidURL
	}

	// §3.3 path normalize: lowercase, strip .git, strip trailing /.
	path := strings.ToLower(parsed.Path)
	path = strings.TrimSuffix(path, ".git")
	path = strings.TrimRight(path, "/")
	if path == "" || path == "/" {
		// Bare host, no path segments — reject per §3.3 / §4.5 X04/X05.
		return "", ErrInvalidURL
	}

	// §3.4 split + filter empty.
	rawSegments := strings.Split(path, "/")
	segments := make([]string, 0, len(rawSegments))
	for _, s := range rawSegments {
		if s != "" {
			segments = append(segments, s)
		}
	}
	if len(segments) == 0 {
		return "", ErrInvalidURL
	}

	// §3.5 escape each segment.
	escaped := make([]string, len(segments))
	for i, s := range segments {
		escaped[i] = escapeSegment(s)
	}
	joined := strings.Join(escaped, "__")

	// §6 length budget. Gitea repo name ≤ 64.
	const maxRepoLen = 64
	const prefix = "kb-"
	const hashSuffixLen = 8
	const truncSeparator = "~~"

	// repo_part = "kb-" + host + "__" + joined
	repoPart := prefix + host + "__" + joined
	if len(repoPart) > maxRepoLen {
		// Truncation budget: 3 (kb-) + len(host) + 2 (host sep) + slice + 2 (~~) + 8 (hash) ≤ 64
		// ⇒ slice ≤ 49 - len(host)
		sliceLen := 49 - len(host)
		if sliceLen < 0 {
			return "", ErrHostTooLong
		}
		sum := sha1.Sum([]byte(joined))
		hashSuffix := hex.EncodeToString(sum[:])[:hashSuffixLen]
		truncated := joined[:sliceLen] + truncSeparator + hashSuffix
		repoPart = prefix + host + "__" + truncated
	}

	return owner + "/" + repoPart, nil
}

// escapeSegment applies §3.5 step 5: keep [a-z0-9._-], replace anything else
// with _, prefix leading "." with "_", fill empty result with "_".
func escapeSegment(s string) string {
	if s == "" {
		return "_"
	}
	var b strings.Builder
	b.Grow(len(s))
	for _, ch := range s {
		if allowedSegmentChar.MatchString(string(ch)) {
			b.WriteRune(ch)
		} else {
			b.WriteByte('_')
		}
	}
	out := b.String()
	if strings.HasPrefix(out, ".") {
		out = "_" + out
	}
	if out == "" {
		out = "_"
	}
	return out
}
