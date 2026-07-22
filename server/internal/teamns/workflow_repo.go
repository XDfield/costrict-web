// Package teamns workflow repo provisioning — Phase 2.2.
//
// EnsureWorkflowRepo is the Gitea-side provisioning entry point consumed by
// the workflow_init handler. It executes the §8.4 contract:
//
//  1. Resolve tenant's GitServer (platform-agnostic backend).
//  2. Get-or-create the type repo `wf-<escaped_slug>` under the team's org.
//  3. Get-or-write `definition_snapshot.json` on main; SHA256 mismatch on
//     existing file returns ErrDefinitionDrift (handler maps to 409).
//  4. Apply branch protection: `main` (direct push denied) + `inst-*` glob
//     (instance branches protected by wildcard). Idempotent on already-exists.
//  5. Get-or-create the instance branch `inst-<inst_short>` from main HEAD.
//
// Return value carries Created flags so workflow_init's response reports
// whether each sub-step ran this call vs. was already in place. The handler
// forwards these verbatim to clients, who use them for observability.
//
// All idempotent: re-running EnsureWorkflowRepo for an already-provisioned
// repo is a no-op (Created flags false, no drift error if snapshot matches).

package teamns

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"

	"github.com/costrict/costrict-web/server/internal/gitsync"
	"github.com/costrict/costrict-web/server/internal/workflow"
	"gorm.io/gorm"
)

// Sentinel errors for workflow repo provisioning.
var (
	// ErrDefinitionDrift — the existing definition_snapshot.json on main
	// does not hash-match the caller-supplied snapshot. HTTP 409.
	ErrDefinitionDrift = errors.New("teamns: definition snapshot drift")
	// ErrWorkflowRepoProvisioning — generic wrapper for git-side failures
	// during repo / branch / file / protection ops. HTTP 502.
	ErrWorkflowRepoProvisioning = errors.New("teamns: workflow repo provisioning failed")
)

// DefinitionSnapshotPath is the canonical path within the type repo where
// the orchestrator-stamped workflow definition lands. Both server (write)
// and orchestrator (read) reference this constant.
const DefinitionSnapshotPath = "definition_snapshot.json"

// WorkflowRepoResult carries per-call flags for the workflow_init response.
// `Created` semantics: true only when the sub-op created a new resource
// this call; false when the resource already existed (idempotent path).
type WorkflowRepoResult struct {
	TypeRepoCreated       bool
	InstanceBranchCreated bool
	BranchProtectionSet   bool
	SnapshotHash          string
}

// EnsureWorkflowRepo runs the full provisioning pipeline. teamID selects
// the team_ns + bot creds (must exist); defSlug + definitionSnapshot +
// instanceID drive the deterministic path computation
// (workflow.WfRepoPath / WfBranchName).
func (s *Service) EnsureWorkflowRepo(
	ctx context.Context,
	teamID, defSlug, definitionSnapshot, instanceID string,
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
	_ = ns // referenced for existence check; future drift audits may use it

	// 3. Resolve platform-agnostic backend via the injectable hook
	// (tests override; production delegates to gitsync.Service).
	gitcli, err := s.gitServerFor(ctx, ns.TenantID)
	if err != nil {
		return nil, err
	}
	if gitcli == nil {
		return nil, ErrTenantGitServerUnresolved
	}

	result := &WorkflowRepoResult{
		SnapshotHash: hashSnapshot(definitionSnapshot),
	}

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

	// 5. Definition snapshot drift check + write.
	existing, err := gitcli.ReadFile(ctx, owner, repoName, "main", DefinitionSnapshotPath)
	if err != nil {
		return nil, fmt.Errorf("%w: read snapshot: %v", ErrWorkflowRepoProvisioning, err)
	}
	if existing != nil {
		// Drift detection: if existing snapshot doesn't hash-match the
		// caller's, return 409 — caller must reconcile before continuing.
		if hashSnapshot(string(existing)) != result.SnapshotHash {
			return nil, ErrDefinitionDrift
		}
		// Hashes match — no rewrite needed.
	} else {
		if err := gitcli.WriteFile(ctx, owner, repoName, "main",
			DefinitionSnapshotPath, []byte(definitionSnapshot),
			"Initialize definition_snapshot"); err != nil {
			return nil, fmt.Errorf("%w: write snapshot: %v", ErrWorkflowRepoProvisioning, err)
		}
	}

	// 6. Protect main (the snapshot branch — created by repo init / our
	// WriteFile above; no further direct pushes are expected). Tolerate
	// already-exists because re-running EnsureWorkflowRepo should be safe.
	if err := applyBranchProtection(ctx, gitcli, owner, repoName, "main", ErrWorkflowRepoProvisioning); err != nil {
		return nil, err
	}

	// 7. Instance branch get-or-create. MUST happen before the inst-* glob
	// protection rule is applied, otherwise the glob would catch inst-<short>
	// and reject the API-initiated push Gitea uses internally for CreateBranch.
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

	// 8. Protect the inst-* glob now that inst-<short> exists.
	if err := applyBranchProtection(ctx, gitcli, owner, repoName, "inst-*", ErrWorkflowRepoProvisioning); err != nil {
		return nil, err
	}
	result.BranchProtectionSet = true

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

// hashSnapshot returns the SHA256 hex of a definition_snapshot string.
// Used for both drift comparison (existing vs. caller-supplied) and for
// the response's SnapshotHash field (observability / dedup).
func hashSnapshot(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}
