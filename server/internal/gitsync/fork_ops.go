// Package gitsync repo fork ops.
//
// ForkRepo is the one Gitea call that CANNOT run with the admin PAT: Gitea's
// CreateForkOption only carries `name` and `organization`, so the fork target
// is always the *authenticated identity* (or an org it belongs to). There is
// no "fork into user X" parameter. To land a fork in an end user's personal
// namespace, the request must therefore be signed with that user's own PAT —
// see NewUserClient below and gitsync.DecryptUserToken for the credential
// source.
//
// Status semantics follow the rest of the package: doJSON maps non-expected
// codes to the ErrGitea* sentinels, and the idempotency helpers sniff the
// packed status out of the wrapped error string.

package gitsync

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
)

// NewUserClient returns a Client authenticated as an end user via their
// personal access token instead of the tenant admin token.
//
// The token is carried in the same Authorization header slot as the admin
// PAT (Gitea does not distinguish), so only the *identity* differs. Callers
// MUST NOT invoke admin-only endpoints (/api/v1/admin/**, CreateUserToken,
// ...) on a user client — Gitea answers those with 403/404.
//
// Empty baseURL or token returns nil, matching NewClient's "feature not
// configured" convention.
func NewUserClient(baseURL, userToken string) *Client {
	return NewClient(baseURL, userToken)
}

// ForkRepoOptions is the input shape for ForkRepo.
//
// There is deliberately no "target owner" knob: the fork always lands under
// the identity that owns the token the Client was built with. TargetOwner is
// the caller's *expectation* of that identity (server-derived, e.g. from
// UserGitBinding.git_username) and is used only for the idempotent lookup
// when Gitea reports the fork already exists.
type ForkRepoOptions struct {
	// TargetOwner is the namespace the fork is expected to land in. Required —
	// without it the conflict path cannot resolve the existing repo.
	TargetOwner string
	// Name overrides the forked repo's name. Empty keeps the source name,
	// which is what makes "fork twice" collide (and therefore idempotent).
	Name string
}

// ForkRepo forks srcOwner/srcRepo into the authenticated user's namespace.
//
// Idempotency: Gitea answers 409 (documented as "The repository with the same
// name already exists") — and, on some versions, 422 — when the target
// namespace already holds a repo of that name. Both are treated as "already
// forked": the existing repo is fetched and returned, so a retry after a
// partial failure reuses the same repository instead of creating a second one.
//
// A 404 means the *source* repo is not visible to this token; it is returned
// as-is (ErrGiteaTeamNotFound, sniffable via isHTTPNotFound) so callers can
// distinguish "not mirrored here" from a transport failure.
func (c *Client) ForkRepo(ctx context.Context, srcOwner, srcRepo string, opts ForkRepoOptions) (*Repo, error) {
	if c == nil {
		return nil, ErrGiteaUnreachable
	}
	if srcOwner == "" || srcRepo == "" {
		return nil, fmt.Errorf("gitsync: source owner and repo are required")
	}
	if opts.TargetOwner == "" {
		return nil, fmt.Errorf("gitsync: fork target owner is required")
	}

	targetName := opts.Name
	if targetName == "" {
		targetName = srcRepo
	}

	// Body carries `name` only when the caller renamed the fork. `organization`
	// is never set: it would redirect the fork to an org namespace, which is
	// exactly what this API must not do for personal forks.
	body := map[string]string{}
	if opts.Name != "" {
		body["name"] = opts.Name
	}

	path := repoPath(srcOwner, srcRepo) + "/forks"
	resp, err := c.doJSON(ctx, http.MethodPost, path, body, http.StatusAccepted, http.StatusCreated, http.StatusOK)
	if err != nil {
		if isConflictError(err) || isForkAlreadyExists(err) {
			existing, lookupErr := c.GetRepo(ctx, opts.TargetOwner, targetName)
			if lookupErr != nil {
				return nil, fmt.Errorf("%w: fork conflict, lookup %s/%s failed: %v",
					ErrGiteaUnreachable, opts.TargetOwner, targetName, lookupErr)
			}
			if existing == nil {
				// Conflict but nothing under the expected owner — the clashing
				// repo lives somewhere we can't see. Surfacing the original
				// error beats inventing a coordinate.
				return nil, fmt.Errorf("%w: fork rejected as duplicate but %s/%s does not exist: %v",
					ErrGiteaUnreachable, opts.TargetOwner, targetName, err)
			}
			// A fork lands under the source's bare name, so the conflict may just
			// as well be an UNRELATED repo of the caller's that shares that name
			// (bare names like "mcp-server" are common across upstreams). Gitea
			// reports both cases identically, so verify lineage before accepting
			// the repo as "already forked" — otherwise this item would be wired
			// to somebody else's content and the DB would record it as truth.
			if !repoIsForkOf(existing, srcOwner, srcRepo) {
				return nil, fmt.Errorf("%w: %s/%s already exists and is not a fork of %s/%s",
					ErrGiteaUsernameTaken, opts.TargetOwner, targetName, srcOwner, srcRepo)
			}
			return existing, nil
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

// isForkAlreadyExists catches the Gitea builds that answer a duplicate fork
// with 500 + "repository already exists" instead of the documented 409 (same
// shape of quirk CreateBranch handles for concurrent ref creation). Without
// it, a retry after a half-written fork would keep failing instead of
// converging on the existing repository.
func isForkAlreadyExists(err error) bool {
	if err == nil || isHTTPNotFound(err) {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "already exists") || strings.Contains(msg, "alreadyexist")
}

// repoIsForkOf reports whether repo is a fork of srcOwner/srcRepo, per Gitea's
// `parent` payload. Used to tell an idempotent re-fork apart from a plain name
// collision with an unrelated repository.
//
// Fails closed: a repo that does not advertise a parent (parent omitted, or
// fork=false) is NOT accepted as the fork we were looking for.
func repoIsForkOf(repo *Repo, srcOwner, srcRepo string) bool {
	if repo == nil || repo.Parent == nil {
		return false
	}
	return strings.EqualFold(repo.Parent.FullName, srcOwner+"/"+srcRepo)
}
