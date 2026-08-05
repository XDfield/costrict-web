// Package gitsync repo / branch / file ops — Phase 2.2.
//
// These methods extend *Client with the surface needed for workflow repo
// provisioning (P0 #2/#3/#4 in the team-namespace API roadmap):
//
//   - CreateRepo + GetRepo       — type repo bootstrap under team org
//   - CreateBranch + GetBranch   — instance branch from main HEAD
//   - SetBranchProtection        — main branch rule (inst-* intentionally unprotected)
//   - WriteFile + ReadFile       — definition_snapshot lifecycle + drift
//
// All methods route through Client.doJSON; status-code semantics follow the
// existing convention (404 → ErrGiteaTeamNotFound, 4xx/5xx wrapped as
// ErrGiteaUnreachable). Idempotency helpers isHTTPNotFound / isHTTPConflict
// sniff the wrapped error string since doJSON packs the raw status into the
// message text.

package gitsync

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
)

const (
	gitTreePageSize   = 100
	gitTreeMaxEntries = 100000
)

// GitTreeEntry is the subset of Gitea's recursive tree payload needed by
// capability discovery. Only blob paths are later considered as manifests.
type GitTreeEntry struct {
	Path string `json:"path"`
	Type string `json:"type"`
	SHA  string `json:"sha"`
	Size int64  `json:"size"`
}

// ErrGiteaNotFound is the generic 404 sentinel. doJSON currently maps every
// 404 to ErrGiteaTeamNotFound (legacy from the team-member sync surface);
// consumers that need a Kind-agnostic 404 sniff call isHTTPNotFound on the
// returned error instead of errors.Is.
var ErrGiteaNotFound = ErrGiteaTeamNotFound

// isHTTPNotFound returns true if err was produced by doJSON in response to
// a 404 status. Used by callers that implement get-or-create idempotency:
// GetRepo returns (nil, nil) when the repo simply doesn't exist yet.
func isHTTPNotFound(err error) bool {
	if err == nil {
		return false
	}
	return strings.Contains(err.Error(), "status=404")
}

// isHTTPConflict returns true for HTTP 409. Used by CreateBranch to detect
// "branch already exists" so the caller can treat it as success.
func isHTTPConflict(err error) bool {
	if err == nil {
		return false
	}
	return strings.Contains(err.Error(), "status=409")
}

// repoPath returns /repos/{owner}/{repo}. owner and repo are path-escaped
// since they may contain dots or other URL-unsafe chars in theory (in
// practice they're team_short / escaped def slug, both safe).
func repoPath(owner, repo string) string {
	return "/api/v1/repos/" + url.PathEscape(owner) + "/" + url.PathEscape(repo)
}

// CreateRepo creates a repository owned by `owner` using the admin path
// POST /admin/users/{owner}/repos. Works for any owner type (user or org)
// because the admin PAT bypasses the "must be a member" check that the
// /orgs/{org}/repos path enforces.
//
// On conflict (409 — repo already exists), returns ErrGiteaUsernameTaken
// so the caller can decide between fail-or-fetch-existing.
func (c *Client) CreateRepo(ctx context.Context, owner string, opts CreateRepoOptions) (*Repo, error) {
	if c == nil {
		return nil, ErrGiteaUnreachable
	}
	if owner == "" {
		return nil, fmt.Errorf("gitsync: repo owner is required")
	}
	if opts.Name == "" {
		return nil, fmt.Errorf("gitsync: repo name is required")
	}

	path := "/api/v1/admin/users/" + url.PathEscape(owner) + "/repos"
	resp, err := c.doJSON(ctx, http.MethodPost, path, opts, http.StatusCreated)
	if err != nil {
		if isConflictError(err) {
			return nil, fmt.Errorf("%w: repo %s/%s already exists: %v",
				ErrGiteaUsernameTaken, owner, opts.Name, err)
		}
		return nil, err
	}
	defer resp.Body.Close()

	var r Repo
	if err := json.NewDecoder(resp.Body).Decode(&r); err != nil {
		return nil, fmt.Errorf("%w: decode response: %v", ErrGiteaUnreachable, err)
	}
	return &r, nil
}

// GetRepo fetches a single repository. Returns (nil, nil) when the repo
// does not exist, so callers can write `if r, _ := GetRepo(...); r == nil { CreateRepo(...) }`
// without unwrapping sentinel errors.
func (c *Client) GetRepo(ctx context.Context, owner, name string) (*Repo, error) {
	if c == nil {
		return nil, ErrGiteaUnreachable
	}
	if owner == "" || name == "" {
		return nil, fmt.Errorf("gitsync: owner and repo name are required")
	}

	resp, err := c.doJSON(ctx, http.MethodGet, repoPath(owner, name), nil, http.StatusOK)
	if err != nil {
		if isHTTPNotFound(err) {
			return nil, nil
		}
		return nil, err
	}
	defer resp.Body.Close()

	var r Repo
	if err := json.NewDecoder(resp.Body).Decode(&r); err != nil {
		return nil, fmt.Errorf("%w: decode response: %v", ErrGiteaUnreachable, err)
	}
	return &r, nil
}

// GetRepoByID fetches a repository through Gitea's stable numeric identity.
// It remains valid when its owner or name has changed since an earlier webhook
// delivery. A 404 returns (nil, nil); transport, authentication, status, and
// decode failures remain errors so callers can retry safely.
func (c *Client) GetRepoByID(ctx context.Context, repoID int64) (*Repo, error) {
	if c == nil {
		return nil, ErrGiteaUnreachable
	}
	if repoID <= 0 {
		return nil, fmt.Errorf("gitsync: repository ID must be positive")
	}

	resp, err := c.doJSON(ctx, http.MethodGet, fmt.Sprintf("/api/v1/repositories/%d", repoID), nil, http.StatusOK)
	if err != nil {
		if isHTTPNotFound(err) {
			return nil, nil
		}
		return nil, err
	}
	defer resp.Body.Close()

	var r Repo
	if err := json.NewDecoder(resp.Body).Decode(&r); err != nil {
		return nil, fmt.Errorf("%w: decode response: %v", ErrGiteaUnreachable, err)
	}
	return &r, nil
}

// DeleteRepo removes a repository created by an orchestration attempt that
// failed before it could publish a usable coordinate. A missing repository is
// idempotent success.
func (c *Client) DeleteRepo(ctx context.Context, owner, repo string) error {
	if c == nil {
		return ErrGiteaUnreachable
	}
	if owner == "" || repo == "" {
		return fmt.Errorf("gitsync: owner and repo are required")
	}
	resp, err := c.doJSON(ctx, http.MethodDelete, repoPath(owner, repo), nil, http.StatusNoContent)
	if err != nil {
		if isHTTPNotFound(err) {
			return nil
		}
		return err
	}
	_ = resp.Body.Close()
	return nil
}

// ListTree returns the complete recursive file tree at ref. Gitea paginates
// large trees, so callers must not assume a single response is exhaustive.
// A hard entry cap prevents a malformed server from making discovery allocate
// without bound.
func (c *Client) ListTree(ctx context.Context, owner, repo, ref string) ([]GitTreeEntry, error) {
	if c == nil {
		return nil, ErrGiteaUnreachable
	}
	if owner == "" || repo == "" || ref == "" {
		return nil, fmt.Errorf("gitsync: owner, repo, and ref are required")
	}

	entries := make([]GitTreeEntry, 0)
	expectedTotal := -1
	for page := 1; ; page++ {
		reqPath := repoPath(owner, repo) + "/git/trees/" + url.PathEscape(ref) +
			"?recursive=true&page=" + fmt.Sprintf("%d", page) +
			"&per_page=" + fmt.Sprintf("%d", gitTreePageSize)
		resp, err := c.doJSON(ctx, http.MethodGet, reqPath, nil, http.StatusOK)
		if err != nil {
			return nil, err
		}
		var payload struct {
			Tree       []GitTreeEntry `json:"tree"`
			Truncated  bool           `json:"truncated"`
			TotalCount *int           `json:"total_count"`
		}
		decodeErr := json.NewDecoder(resp.Body).Decode(&payload)
		_ = resp.Body.Close()
		if decodeErr != nil {
			return nil, fmt.Errorf("%w: decode tree response: %v", ErrGiteaUnreachable, decodeErr)
		}
		if payload.TotalCount != nil {
			if *payload.TotalCount < 0 || *payload.TotalCount > gitTreeMaxEntries {
				return nil, fmt.Errorf("gitsync: repository tree reports invalid total_count %d", *payload.TotalCount)
			}
			if expectedTotal >= 0 && expectedTotal != *payload.TotalCount {
				return nil, fmt.Errorf("gitsync: repository tree total_count changed from %d to %d during pagination", expectedTotal, *payload.TotalCount)
			}
			expectedTotal = *payload.TotalCount
		}
		if len(entries)+len(payload.Tree) > gitTreeMaxEntries {
			return nil, fmt.Errorf("gitsync: repository tree exceeds %d entries", gitTreeMaxEntries)
		}
		entries = append(entries, payload.Tree...)

		if expectedTotal >= 0 {
			if len(entries) > expectedTotal {
				return nil, fmt.Errorf("gitsync: repository tree returned %d entries, exceeding total_count %d", len(entries), expectedTotal)
			}
			if len(entries) == expectedTotal {
				if payload.Truncated {
					return nil, fmt.Errorf("gitsync: repository tree is truncated after reported total_count %d", expectedTotal)
				}
				break
			}
			if len(payload.Tree) == 0 || !payload.Truncated {
				return nil, fmt.Errorf("gitsync: incomplete repository tree: received %d of %d entries", len(entries), expectedTotal)
			}
		} else {
			if !payload.Truncated {
				break
			}
			if len(payload.Tree) == 0 {
				return nil, fmt.Errorf("gitsync: repository tree is truncated but returned no entries")
			}
		}
		if page >= gitTreeMaxEntries/gitTreePageSize {
			return nil, fmt.Errorf("gitsync: repository tree pagination exceeded limit")
		}
	}
	return entries, nil
}

// CreateBranch creates newBranch from fromRef (a branch name, tag, or commit
// SHA). If fromRef is empty, Gitea uses the repo's default branch.
//
// On conflict (branch already exists), returns nil — idempotent success.
// This matches the workflow_repo orchestration contract: re-running
// EnsureWorkflowRepo for an existing instance must be a no-op.
func (c *Client) CreateBranch(ctx context.Context, owner, repo, newBranch, fromRef string) error {
	if c == nil {
		return ErrGiteaUnreachable
	}
	if owner == "" || repo == "" || newBranch == "" {
		return fmt.Errorf("gitsync: owner, repo, and newBranch are required")
	}

	body := map[string]string{
		"new_branch_name": newBranch,
	}
	if fromRef != "" {
		body["old_ref_name"] = fromRef
	}

	_, err := c.doJSON(ctx, http.MethodPost, repoPath(owner, repo)+"/branches", body, http.StatusCreated)
	if err != nil {
		if isHTTPConflict(err) {
			return nil // idempotent: branch already exists (409)
		}
		// Gitea returns 500 PushRejected when a concurrent CreateBranch raced —
		// the ref exists by now, so treat as idempotent success (same pattern
		// as the 409 path, just a different Gitea error shape).
		msg := err.Error()
		if strings.Contains(msg, "reference already exists") || strings.Contains(msg, "cannot lock ref") {
			return nil
		}
		return err
	}
	return nil
}

// GetBranch fetches a single branch's metadata. Returns (nil, nil) when
// the branch does not exist (idempotent lookup).
func (c *Client) GetBranch(ctx context.Context, owner, repo, branch string) (*Branch, error) {
	if c == nil {
		return nil, ErrGiteaUnreachable
	}
	if owner == "" || repo == "" || branch == "" {
		return nil, fmt.Errorf("gitsync: owner, repo, and branch are required")
	}

	path := repoPath(owner, repo) + "/branches/" + url.PathEscape(branch)
	resp, err := c.doJSON(ctx, http.MethodGet, path, nil, http.StatusOK)
	if err != nil {
		if isHTTPNotFound(err) {
			return nil, nil
		}
		return nil, err
	}
	defer resp.Body.Close()

	// Gitea returns { "name":..., "commit": { "id": "sha" } }.
	var raw struct {
		Name   string `json:"name"`
		Commit struct {
			ID string `json:"id"`
		} `json:"commit"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&raw); err != nil {
		return nil, fmt.Errorf("%w: decode response: %v", ErrGiteaUnreachable, err)
	}
	return &Branch{
		Name:      raw.Name,
		CommitSHA: raw.Commit.ID,
	}, nil
}

// SetBranchProtection installs a branch protection rule. Gitea supports
// glob rule_name (e.g. "inst-*"); callers may invoke once per rule per repo.
//
// On conflict (rule already exists), returns ErrGiteaUsernameTaken so the
// caller can choose to skip rather than re-apply.
func (c *Client) SetBranchProtection(ctx context.Context, owner, repo string, opts BranchProtectionOptions) error {
	if c == nil {
		return ErrGiteaUnreachable
	}
	if owner == "" || repo == "" {
		return fmt.Errorf("gitsync: owner and repo are required")
	}
	if opts.RuleName == "" {
		return fmt.Errorf("gitsync: BranchProtectionOptions.RuleName is required")
	}

	path := repoPath(owner, repo) + "/branch_protections"
	_, err := c.doJSON(ctx, http.MethodPost, path, opts, http.StatusCreated)
	if err != nil {
		if isConflictError(err) || isBranchProtectionAlreadyExists(err) {
			return fmt.Errorf("%w: protection rule %q already exists: %v",
				ErrGiteaUsernameTaken, opts.RuleName, err)
		}
		return err
	}
	return nil
}

// isBranchProtectionAlreadyExists detects Gitea's idiosyncratic 403 response
// for duplicate branch-protection rules. Per Gitea v1.27, POSTing a rule that
// already exists returns 403 (not 409) with body "Branch protection already
// exist". This is a per-endpoint quirk — the generic isConflictError helper
// only catches 409/422, so SetBranchProtection also calls this helper.
func isBranchProtectionAlreadyExists(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "status=403") && strings.Contains(msg, "Branch protection already exist")
}

// fileRequestBody is the JSON shape for POST /contents/{path}.
type fileRequestBody struct {
	Message   string `json:"message"`
	Branch    string `json:"branch"`
	Content   string `json:"content"` // base64-encoded
	NewBranch string `json:"new_branch,omitempty"`
}

// WriteFile creates or updates a file via the Gitea contents API. content
// is base64-encoded internally. The caller is responsible for drift
// detection (read-then-compare) before invoking — Gitea's contents API
// returns 200 on update, 201 on create, both accepted here.
func (c *Client) WriteFile(ctx context.Context, owner, repo, branch, path string, content []byte, message string) error {
	if c == nil {
		return ErrGiteaUnreachable
	}
	if owner == "" || repo == "" || branch == "" || path == "" {
		return fmt.Errorf("gitsync: owner, repo, branch, and path are required")
	}
	if message == "" {
		message = "Update " + path
	}

	body := fileRequestBody{
		Message: message,
		Branch:  branch,
		Content: base64.StdEncoding.EncodeToString(content),
	}

	// Per-segment escaping, not url.PathEscape over the whole path: escaping the
	// separators into %2F leaves the server to unescape them before it can
	// resolve the path, which has varied across Gitea versions (see
	// escapeGitFilePath). Identical output for the single-segment paths this
	// used to write; correct for the nested ones capability migration writes.
	reqPath := repoPath(owner, repo) + "/contents/" + escapeGitFilePath(path)
	resp, err := c.doJSON(ctx, http.MethodPost, reqPath, body, http.StatusCreated, http.StatusOK)
	if err != nil {
		// Idempotent re-provisioning: if the file already exists (409/422), the
		// snapshot is already written. Treat as success — the definition snapshot
		// is immutable per workflow version, so re-writing the same content is a
		// no-op. Without this, every re-provision of an already-provisioned
		// workspace fails at write-snapshot.
		if isConflictError(err) {
			return nil
		}
		return err
	}
	_ = resp.Body.Close()
	return nil
}

// fileResponse is the projected Gitea contents payload. Only Content is
// load-bearing for drift comparison.
type fileResponse struct {
	Content  string `json:"content"`
	Encoding string `json:"encoding"`
}

// ReadFile fetches a file's content. Returns (nil, nil) when the file does
// not exist (idempotent lookup for drift detection: "no snapshot yet").
func (c *Client) ReadFile(ctx context.Context, owner, repo, branch, path string) ([]byte, error) {
	if c == nil {
		return nil, ErrGiteaUnreachable
	}
	if owner == "" || repo == "" || branch == "" || path == "" {
		return nil, fmt.Errorf("gitsync: owner, repo, branch, and path are required")
	}

	reqPath := repoPath(owner, repo) + "/contents/" + escapeGitFilePath(path) + "?ref=" + url.QueryEscape(branch)
	resp, err := c.doJSON(ctx, http.MethodGet, reqPath, nil, http.StatusOK)
	if err != nil {
		if isHTTPNotFound(err) {
			return nil, nil
		}
		return nil, err
	}
	defer resp.Body.Close()

	var fr fileResponse
	if err := json.NewDecoder(resp.Body).Decode(&fr); err != nil {
		return nil, fmt.Errorf("%w: decode response: %v", ErrGiteaUnreachable, err)
	}
	// Empty content is a legitimate "no snapshot" state (multica M2 leaves the
	// workflow DefinitionSnapshot empty — DB is the source of truth). Treat it as
	// a missing snapshot, not a transport error. The previous check collapsed
	// empty-content into "unsupported file encoding", which was misleading and
	// blocked provisioning for every empty-snapshot workflow.
	if fr.Content == "" {
		return nil, nil
	}
	if fr.Encoding != "base64" {
		return nil, fmt.Errorf("%w: unsupported file encoding %q", ErrGiteaUnreachable, fr.Encoding)
	}
	// Gitea returns base64 content with embedded newlines every 76 chars
	// (RFC 2045 MIME); StdEncoding.DecodeString tolerates them.
	raw, err := base64.StdEncoding.DecodeString(fr.Content)
	if err != nil {
		return nil, fmt.Errorf("%w: decode base64 content: %v", ErrGiteaUnreachable, err)
	}
	return raw, nil
}
