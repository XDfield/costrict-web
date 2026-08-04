// Package gitsync raw file reads — the read-through path for git-backed
// capability content.
//
// ReadFile (repo_ops.go) speaks the contents API, which wraps the bytes in a
// base64 JSON envelope. That shape suits drift comparison, where the payload is
// a snapshot the caller already holds. Serving a capability's content is the
// opposite case: the bytes go straight to the HTTP response, byte for byte, so
// the envelope only costs a decode and a second copy. This file adds the raw
// endpoint instead:
//
//	GET /api/v1/repos/{owner}/{repo}/raw/{filepath}?ref={ref}

package gitsync

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
)

// maxRawFileBytes caps a single raw read. Capability manifests are text files
// of a few KiB; the cap exists so a repository holding a multi-gigabyte blob at
// the manifest path cannot turn one HTTP request into an unbounded allocation.
const maxRawFileBytes = 8 << 20 // 8 MiB

// ReadRawFile returns the bytes of filePath at ref.
//
// Unlike ReadFile, a missing file is an error (ErrGiteaNotFound), not
// (nil, nil). The caller serves this content to end users and must be able to
// tell "the repository no longer has this file" from "the file is empty" —
// collapsing the two is how a vanished capability turns into a blank page.
func (c *Client) ReadRawFile(ctx context.Context, owner, repo, ref, filePath string) ([]byte, error) {
	if c == nil {
		return nil, ErrGiteaUnreachable
	}
	if owner == "" || repo == "" || ref == "" || filePath == "" {
		return nil, fmt.Errorf("gitsync: owner, repo, ref, and filePath are required")
	}

	reqPath := repoPath(owner, repo) + "/raw/" + escapeGitFilePath(filePath) +
		"?ref=" + url.QueryEscape(ref)
	resp, err := c.doJSON(ctx, http.MethodGet, reqPath, nil, http.StatusOK)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	// One byte past the cap distinguishes "exactly at the limit" from "over it".
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxRawFileBytes+1))
	if err != nil {
		return nil, fmt.Errorf("%w: read raw file %s: %v", ErrGiteaUnreachable, filePath, err)
	}
	if len(raw) > maxRawFileBytes {
		return nil, fmt.Errorf("%w: file %s exceeds %d bytes", ErrGiteaUnreachable, filePath, maxRawFileBytes)
	}
	return raw, nil
}

// escapeGitFilePath percent-encodes each path segment and rejoins them with
// literal separators. Escaping the whole path would turn its separators into
// %2F, which the server then has to unescape before it can resolve the path —
// behaviour that has varied across Gitea versions. Encoding per segment keeps
// the request unambiguous either way.
func escapeGitFilePath(filePath string) string {
	segments := strings.Split(filePath, "/")
	for i, segment := range segments {
		segments[i] = url.PathEscape(segment)
	}
	return strings.Join(segments, "/")
}
