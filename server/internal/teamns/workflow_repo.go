// Package teamns workflow repo provisioning — Phase 2.2.
//
// EnsureWorkflowRepo is the Gitea-side provisioning entry point consumed by
// the workflow_init handler. It executes the §8.4 contract:
//
//  1. Resolve tenant's GitServer (platform-agnostic backend).
//  2. Get-or-create the type repo `wf-<escaped_slug>` under the team's org.
//  3. Apply branch protection on `main` (direct push denied); inst-* is left
//     unprotected so instance branches can be created/pushed directly.
//     Idempotent on already-exists.
//  4. Unless skipInstanceBranch, get-or-create the instance branch
//     `inst-<inst_short>` from main HEAD. Workspace/workflow activation
//     provisions the repo without an instance branch; the per-run branch is
//     created at issue/run time (initWorkflowNamespace) instead.
//
// Return value carries Created flags so workflow_init's response reports
// whether each sub-step ran this call vs. was already in place. The handler
// forwards these verbatim to clients, who use them for observability.
//
// All idempotent: re-running EnsureWorkflowRepo for an already-provisioned
// repo is a no-op (Created flags false).

package teamns

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/costrict/costrict-web/server/internal/gitsync"
	"github.com/costrict/costrict-web/server/internal/workflow"
	"gorm.io/gorm"
)

// Sentinel errors for workflow repo provisioning.
var (
	// ErrWorkflowRepoProvisioning — generic wrapper for git-side failures
	// during repo / branch / protection ops. HTTP 502.
	ErrWorkflowRepoProvisioning = errors.New("teamns: workflow repo provisioning failed")
)

// WorkflowRepoResult carries per-call flags for the workflow_init response.
// `Created` semantics: true only when the sub-op created a new resource
// this call; false when the resource already existed (idempotent path).
type WorkflowRepoResult struct {
	TypeRepoCreated       bool
	InstanceBranchCreated bool
	BranchProtectionSet   bool
}

// EnsureWorkflowRepo runs the full provisioning pipeline. teamID selects
// the team_ns + bot creds (must exist); defSlug + instanceID drive the
// deterministic path computation (workflow.WfRepoPath / WfBranchName).
// skipInstanceBranch omits the per-instance branch — used by workspace /
// workflow activation (no run yet); the per-run branch is created at
// issue/run time instead.
func (s *Service) EnsureWorkflowRepo(
	ctx context.Context,
	teamID, defSlug, instanceID string,
	skipInstanceBranch bool,
) (*WorkflowRepoResult, error) {
	if s == nil {
		return nil, ErrTenantGitServerUnresolved
	}
	// 1. Validate inputs via the pure-path helpers. They already raise
	// workflow.ErrInvalidSlug / ErrInvalidTeamID / ErrInvalidInstanceID
	// — we project those onto teamns.ErrInvalidRequest for consistency
	// with the rest of the teamns surface.
	wfRepoPath, err := workflow.WfRepoPath(defSlug, teamID)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidRequest, err)
	}
	instanceBranch, err := workflow.WfBranchName(instanceID)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrInvalidRequest, err)
	}
	owner, repoName := splitOwnerRepo(wfRepoPath)

	// 2. Look up team_ns to confirm provisioning context (tenant, org).
	// LookupTeamNS returns gorm.ErrRecordNotFound on miss — map to
	// ErrTeamNotFound so callers see a stable 404 sentinel regardless of
	// which DB layer raised it.
	ns, err := s.LookupTeamNS(ctx, teamID)
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, ErrTeamNotFound
		}
		return nil, err
	}

	// 3. Resolve platform-agnostic backend via the injectable hook
	// (tests override; production delegates to gitsync.Service).
	gitcli, err := s.gitServerFor(ctx, ns.TenantID)
	if err != nil {
		return nil, err
	}
	if gitcli == nil {
		return nil, ErrTenantGitServerUnresolved
	}

	result := &WorkflowRepoResult{}

	// 4. Type repo get-or-create.
	repo, err := gitcli.GetRepo(ctx, owner, repoName)
	if err != nil {
		return nil, fmt.Errorf("%w: get repo: %v", ErrWorkflowRepoProvisioning, err)
	}
	if repo == nil {
		repo, err = gitcli.CreateRepo(ctx, owner, gitsync.CreateRepoOptions{
			Name:          repoName,
			Description:   "Workflow type repo for " + defSlug,
			Private:       true,
			AutoInit:      true,
			DefaultBranch: "main",
		})
		if err != nil {
			return nil, fmt.Errorf("%w: create repo: %v", ErrWorkflowRepoProvisioning, err)
		}
		result.TypeRepoCreated = true
	}

	// 5. Protect main (the repo's default branch, created by repo init; no
	// direct pushes are expected). Tolerate already-exists because re-running
	// EnsureWorkflowRepo should be safe. Only main is protected: inst-* MUST
	// stay unprotected so the orchestrator can create inst branches and push
	// deliverable content to them directly (per-node changes still gate via
	// node branches + PRs off inst); some Gitea versions also reject API
	// branch creation matching a protected glob (status 500 PushRejected),
	// which would self-block provisioning.
	if err := applyBranchProtection(ctx, gitcli, owner, repoName, "main", ErrWorkflowRepoProvisioning); err != nil {
		return nil, err
	}
	result.BranchProtectionSet = true

	// 6. Instance branch get-or-create from main HEAD. Skipped for
	// workspace/workflow activation (no run yet) — the per-run branch is
	// created at issue/run time instead.
	if skipInstanceBranch {
		return result, nil
	}
	br, err := gitcli.GetBranch(ctx, owner, repoName, instanceBranch)
	if err != nil {
		return nil, fmt.Errorf("%w: get branch: %v", ErrWorkflowRepoProvisioning, err)
	}
	if br == nil {
		if err := gitcli.CreateBranch(ctx, owner, repoName, instanceBranch, "main"); err != nil {
			return nil, fmt.Errorf("%w: create branch: %v", ErrWorkflowRepoProvisioning, err)
		}
		result.InstanceBranchCreated = true
	}

	return result, nil
}

// applyBranchProtection installs a "no direct push, no force push" rule
// for the given branch name (or glob pattern). Already-exists (409) is
// treated as success — branch protection is idempotent config.
//
// Other failures are wrapped with the supplied wrap sentinel (callers pass
// their provisioning-specific sentinel — ErrWorkflowRepoProvisioning or
// ErrKBRepoProvisioning) for 502 at the handler layer.
func applyBranchProtection(ctx context.Context, gitcli gitsync.GitServer, owner, repo, rule string, wrap error) error {
	err := gitcli.SetBranchProtection(ctx, owner, repo, gitsync.BranchProtectionOptions{
		RuleName:          rule,
		EnablePush:        false,
		EnableForcePush:   false,
		RequiredApprovals: 0,
	})
	if err == nil {
		return nil
	}
	if errors.Is(err, gitsync.ErrGiteaUsernameTaken) {
		// Rule already exists — idempotent success.
		return nil
	}
	return fmt.Errorf("%w: branch protection %q: %v", wrap, rule, err)
}

// splitOwnerRepo splits "owner/repo" into ("owner", "repo"). Caller has
// already validated the input via workflow.WfRepoPath so we don't double-
// validate; a missing slash is a programming error (panics).
func splitOwnerRepo(path string) (string, string) {
	idx := strings.IndexByte(path, '/')
	if idx < 0 {
		// Defensive — workflow.WfRepoPath always returns "owner/repo".
		panic(fmt.Sprintf("teamns: invalid wf_repo_path %q (expected owner/repo)", path))
	}
	return path[:idx], path[idx+1:]
}
