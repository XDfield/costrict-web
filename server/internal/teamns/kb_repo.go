// Package teamns KB repo provisioning.
//
// EnsureKBRepo is the Gitea-side provisioning entry point consumed by the
// kb_ensure handler (POST /api/kb/ensure, user-facing). It executes the
// §接口 9 contract:
//
//  1. Resolve tenant's GitServer (platform-agnostic backend).
//  2. Validate inputs via kb.KBRepoPath (pure-path algorithm).
//  3. Get-or-create the kb repo `kb-<host>__<segs>` under the team's org.
//  4. Apply branch protection: `main` (direct push denied). Idempotent on
//     already-exists.
//
// Differences vs. EnsureWorkflowRepo (see TEAM_NAMESPACE_API_REFERENCE.md
// §接口 9 章首引言):
//   - No `definition_snapshot` write / drift check (kb main content is
//     user-owned via `csc kb push`; server writes no canonical file).
//   - No instance branch concept (kb has only one layer: code_repo_url ×
//     team → kb repo).
//   - No inst-* glob protection (consequence of the above).
//
// All idempotent: re-running EnsureKBRepo for an already-provisioned repo
// is a no-op (KbRepoCreated=false, branch protection stays).

package teamns

import (
	"context"
	"errors"
	"fmt"

	"github.com/costrict/costrict-web/server/internal/gitsync"
	"github.com/costrict/costrict-web/server/internal/kb"
	"gorm.io/gorm"
)

// Sentinel errors for KB repo provisioning.
var (
	// ErrKBRepoProvisioning — generic wrapper for git-side failures during
	// kb repo create / branch protection ops. HTTP 502.
	ErrKBRepoProvisioning = errors.New("teamns: kb repo provisioning failed")
)

// KBRepoResult carries per-call flags for the kb_ensure response. KbRepoPath
// is also returned so the handler doesn't need to recompute it.
type KBRepoResult struct {
	KbRepoPath          string
	KbRepoCreated       bool
	BranchProtectionSet bool
}

// EnsureKBRepo runs the full kb provisioning pipeline. teamID selects the
// team_ns + bot creds (must exist); codeRepoURL drives the deterministic
// path computation (kb.KBRepoPath).
//
// On success the caller (handler) is responsible for:
//   - composing kb_clone_url / kb_web_url from the returned KbRepoPath and
//     the tenant's Gitea base_url,
//   - decrypting the bot token plaintext (via DecryptBotToken),
//   - returning bot_credentials in the response.
func (s *Service) EnsureKBRepo(
	ctx context.Context,
	teamID, codeRepoURL string,
) (*KBRepoResult, error) {
	if s == nil {
		return nil, ErrTenantGitServerUnresolved
	}

	// 1. Validate inputs via the pure-path helper. kb.KBRepoPath raises
	// kb.ErrInvalidURL / kb.ErrInvalidTeamID — project onto teamns.ErrInvalidRequest
	// for consistency with the rest of the teamns surface.
	kbRepoPath, err := kb.KBRepoPath(codeRepoURL, teamID)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidRequest, err)
	}
	owner, repoName := splitOwnerRepo(kbRepoPath)

	// 2. Look up team_ns to confirm provisioning context (tenant, org).
	ns, err := s.LookupTeamNS(ctx, teamID)
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, ErrTeamNotFound
		}
		return nil, err
	}
	_ = ns // referenced for existence check; future audits may use it

	// 3. Resolve platform-agnostic backend via the injectable hook
	// (tests override; production delegates to gitsync.Service).
	gitcli, err := s.gitServerFor(ctx, ns.TenantID)
	if err != nil {
		return nil, err
	}
	if gitcli == nil {
		return nil, ErrTenantGitServerUnresolved
	}

	result := &KBRepoResult{KbRepoPath: kbRepoPath}

	// 4. KB repo get-or-create. AutoInit=true ensures main branch exists
	// before branch protection is applied (matches EnsureWorkflowRepo step 4).
	repo, err := gitcli.GetRepo(ctx, owner, repoName)
	if err != nil {
		return nil, fmt.Errorf("%w: get repo: %v", ErrKBRepoProvisioning, err)
	}
	if repo == nil {
		if _, err = gitcli.CreateRepo(ctx, owner, gitsync.CreateRepoOptions{
			Name:          repoName,
			Description:   "KB repo for " + codeRepoURL,
			Private:       true,
			AutoInit:      true,
			DefaultBranch: "main",
		}); err != nil {
			return nil, fmt.Errorf("%w: create repo: %v", ErrKBRepoProvisioning, err)
		}
		result.KbRepoCreated = true
	}

	// 5. Protect main. Tolerate already-exists (idempotent). Reuses
	// applyBranchProtection from workflow_repo.go — same "no direct push,
	// no force push" rule.
	if err := applyBranchProtection(ctx, gitcli, owner, repoName, "main", ErrKBRepoProvisioning); err != nil {
		return nil, err
	}
	result.BranchProtectionSet = true

	return result, nil
}
