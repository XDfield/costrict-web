package services

import (
	"fmt"
	"net/url"
	"strings"
)

// parseSourceURL splits an upstream catalog source_url into the pieces needed for
// a server-side lazy clone-and-pack.
//
// The catalog persists source_url in the GitHub "tree" form, e.g.
//
//	https://github.com/owner/repo/tree/main/a/b/c
//	  → cloneURL=https://github.com/owner/repo, branch=main, subPath=a/b/c
//	https://github.com/owner/repo
//	  → cloneURL=https://github.com/owner/repo, branch="", subPath=""
//
// When the URL has no "/tree/<branch>[/<subPath>]" segment the repo root is cloned
// on the host's default branch (branch=="" lets GitService.Clone fall back to it).
//
// Empty or syntactically invalid input returns an error so callers never attempt
// to clone "".
func parseSourceURL(raw string) (cloneURL, branch, subPath string, err error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return "", "", "", fmt.Errorf("source_url is empty")
	}

	u, perr := url.Parse(trimmed)
	if perr != nil {
		return "", "", "", fmt.Errorf("parse source_url %q: %w", raw, perr)
	}

	// file:// URLs point at a local repo directory (GitService.Clone copies it).
	// There is no owner/repo/tree structure: the whole path is the clone target.
	if u.Scheme == "file" {
		p := u.Path
		if p == "" {
			return "", "", "", fmt.Errorf("source_url %q has empty file path", raw)
		}
		return p, "", "", nil
	}

	if u.Scheme == "" || u.Host == "" {
		return "", "", "", fmt.Errorf("source_url %q is missing scheme or host", raw)
	}

	// Normalise the path into clean, slash-separated segments.
	segments := splitPathSegments(u.Path)
	if len(segments) < 2 {
		return "", "", "", fmt.Errorf("source_url %q must contain owner/repo", raw)
	}

	owner := segments[0]
	repo := strings.TrimSuffix(segments[1], ".git")
	if owner == "" || repo == "" {
		return "", "", "", fmt.Errorf("source_url %q has empty owner or repo", raw)
	}

	cloneURL = fmt.Sprintf("%s://%s/%s/%s", u.Scheme, u.Host, owner, repo)

	rest := segments[2:]
	// Forms without a "/tree/<branch>..." marker clone the repo root on the default branch.
	if len(rest) == 0 {
		return cloneURL, "", "", nil
	}
	if rest[0] != "tree" {
		// Unrecognised trailing path (e.g. /blob/...): treat as repo root rather
		// than guessing, so we never clone the wrong ref.
		return cloneURL, "", "", nil
	}
	if len(rest) < 2 {
		// "/tree" with no branch — degrade to repo root default branch.
		return cloneURL, "", "", nil
	}

	branch = rest[1]
	if len(rest) > 2 {
		subPath = strings.Join(rest[2:], "/")
	}
	return cloneURL, branch, subPath, nil
}

// validateCloneURL is a defense-in-depth guard applied right before a server-side
// lazy clone. source_url is operator-controlled (it comes from the ingested catalog,
// not from an end-user HTTP request), but the resulting bundle is publicly
// downloadable, so in production we refuse to clone anything that is not a plain
// http(s) remote.
//
// This rejects:
//   - file:// (parseSourceURL maps these to a bare local path) — would let a
//     malicious/compromised catalog entry clone a git repo off the server's own
//     filesystem and republish it.
//   - any other scheme (ssh://, git://, ...) and scheme/host-less local paths.
//
// It intentionally does NOT enforce a host allowlist: legitimate sources span
// github.com and the self-hosted gitea mirror, and the GIT_MIRROR_BASE rewrite may
// point anywhere. Restricting to http(s) is the cheap, high-value guard.
//
// allowLocal opens an escape hatch for local-directory clone targets (used only by
// unit tests, which simulate a clone from a temp git repo without network). It is
// false on every production code path (BundlePackService.AllowLocalClone defaults to
// false), so file:// / local paths are refused in production.
func validateCloneURL(cloneURL string, allowLocal bool) error {
	u, err := url.Parse(strings.TrimSpace(cloneURL))
	if err != nil {
		return fmt.Errorf("invalid clone url %q: %w", cloneURL, err)
	}
	scheme := strings.ToLower(u.Scheme)
	if scheme == "http" || scheme == "https" {
		if u.Host == "" {
			return fmt.Errorf("clone url %q has no host", cloneURL)
		}
		return nil
	}
	if allowLocal && (scheme == "file" || scheme == "") {
		return nil
	}
	return fmt.Errorf("refusing to clone non-http(s) source %q (scheme %q)", cloneURL, u.Scheme)
}

// splitPathSegments splits a URL path into non-empty, slash-separated segments.
func splitPathSegments(p string) []string {
	parts := strings.Split(strings.Trim(p, "/"), "/")
	out := make([]string, 0, len(parts))
	for _, seg := range parts {
		if seg == "" {
			continue
		}
		out = append(out, seg)
	}
	return out
}

// mapToMirror optionally rewrites a GitHub clone URL to a self-hosted Gitea mirror
// so the backend can clone from an in-China-reachable host instead of github.com.
//
// When mirrorBase is empty the URL is returned unchanged (direct GitHub clone),
// which is the testable, no-op default and does not block PR2a.
//
// TODO(PR2b): confirm the exact Gitea per-plugin repo naming scheme. The current
// mapping assumes one flat repo per "<owner>-<repo>" under mirrorBase. Until the
// real layout is confirmed, keep this isolated so the naming convention can change
// in one place without touching the pack pipeline.
func mapToMirror(cloneURL, mirrorBase string) string {
	base := strings.TrimSpace(mirrorBase)
	if base == "" {
		return cloneURL
	}

	u, err := url.Parse(strings.TrimSpace(cloneURL))
	if err != nil || u.Host == "" {
		return cloneURL
	}
	// Only rewrite github.com clone URLs; leave anything else (already-mirrored,
	// other hosts) untouched.
	if !strings.EqualFold(u.Host, "github.com") {
		return cloneURL
	}

	segments := splitPathSegments(u.Path)
	if len(segments) < 2 {
		return cloneURL
	}
	owner := segments[0]
	repo := strings.TrimSuffix(segments[1], ".git")

	// TODO(PR2b): confirm naming. Default flat scheme: <mirrorBase>/<owner>-<repo>.
	return fmt.Sprintf("%s/%s-%s", strings.TrimRight(base, "/"), owner, repo)
}
