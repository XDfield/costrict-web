// Package services — "is this capability's repository still public?"
//
// This is an AUTHORIZATION probe, not a content read, and it is deliberately
// separate from the read-through in git_capability_content.go even though both
// speak to the same repository through the same client.
//
// It exists because the local answer is structurally stale. Measured against
// the deployed Gitea 1.24.6 on 2026-08-06, changing a repository's visibility
// emits NO webhook of any kind (see the task's gitea-lifecycle-fixtures
// research). `repositories.visibility` in our database can therefore only be
// refreshed by the periodic reconcile, so between two reconcile passes a
// repository that went private on Gitea still reads as public here. That is not
// a rare race — it is the normal state of the row for the whole interval.
//
// So every path that would serve a Git-backed capability's content, history, or
// repository coordinate to a caller who is authorized ONLY by "the repository
// is public" has to ask the Git server directly. The answer is per repository,
// not per capability: one repository holds up to 55 bound capabilities locally,
// and they all share its visibility.
package services

import (
	"context"
	"fmt"
	"strings"

	"github.com/costrict/costrict-web/server/internal/models"
)

// ItemRepositoryIsPublic reports whether the repository this Git-backed item is
// bound to is, right now, readable by an anonymous visitor of the Git server.
//
// "Public" means BOTH of Gitea's visibility axes are open: not private, and not
// internal (an internal repository sits in a limited organisation and is
// refused to anonymous callers even though `private` is false). Checking only
// `private` would read a limited-org repository as world-readable.
//
// Three answers, and callers must keep them apart:
//
//   - (true, nil)  — verified public. Serving is allowed.
//   - (false, nil) — verified NOT public, including "the repository no longer
//     exists". Serving must be refused; this is a definite answer, not a
//     failure.
//   - (_, err)     — the question could not be answered (unreachable, timeout,
//     server misconfigured, coordinate unusable). Callers MUST fail closed. An
//     unanswered visibility question is not permission to serve: treating it as
//     "probably still public" would make a Gitea outage the most reliable way
//     to read a repository that was taken private.
//
// The bool is never a substitute for the local check. It only decides whether a
// caller who relies on public visibility may proceed; owners, repository
// members and platform operators are authorized locally and never reach here.
func (s *GitCapabilityContentService) ItemRepositoryIsPublic(
	ctx context.Context, item *models.CapabilityItem,
) (bool, error) {
	if s == nil || s.Resolver == nil {
		return false, fmt.Errorf("%w: visibility verification is not configured", ErrGitContentServer)
	}
	if item == nil || item.ContentBackend != models.ContentBackendGit {
		return false, fmt.Errorf("%w: item is not git-backed", ErrGitContentCoordinate)
	}

	serverID := strings.TrimSpace(item.SourceGitServerID)
	if serverID == "" || item.SourceGitRepoID <= 0 {
		// A Git-backed row that cannot name its repository cannot be verified,
		// and an unverifiable row is not publicly serveable. This is the same
		// coordinate rule the content read applies, reached one step earlier.
		return false, fmt.Errorf("%w: server=%q repo=%d",
			ErrGitContentCoordinate, serverID, item.SourceGitRepoID)
	}

	cfg, err := s.Resolver.ResolveByServerID(ctx, serverID)
	if err != nil {
		return false, fmt.Errorf("%w: %v", ErrGitContentServer, err)
	}
	if cfg == nil {
		return false, fmt.Errorf("%w: server %q resolved to no configuration", ErrGitContentServer, serverID)
	}
	reader := s.reader(cfg)
	if reader == nil {
		return false, fmt.Errorf("%w: server %q has no usable client", ErrGitContentServer, serverID)
	}

	// Looked up by NUMERIC id, like every other Git-sync path: the id survives
	// rename and transfer, while source_repo_url carries owner/name as they were
	// at bind time. A renamed repository must still be verifiable.
	repo, err := reader.GetRepoByID(ctx, item.SourceGitRepoID)
	if err != nil {
		return false, classifyGitContentError(err)
	}
	if repo == nil {
		// 404 from the admin token: the repository is gone (or hidden even from
		// it). Either way it is not public, and this is a definite answer — the
		// lifecycle convergence job is what turns it into an archive.
		return false, nil
	}
	return !repo.Private && !repo.Internal, nil
}
