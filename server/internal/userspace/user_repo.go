// Package userspace — per-user KB repo provisioning.
//
// EnsureUserRepo is the Gitea-side provisioning entry point for personal-space
// KB repos. It mirrors teamns.EnsureKBRepo with the owner being
// {gitea_username} instead of t-<team_short>.

package userspace

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/costrict/costrict-web/server/internal/gitsync"
	"github.com/costrict/costrict-web/server/internal/kb"
	"github.com/costrict/costrict-web/server/internal/models"
	"gorm.io/gorm"
)

// UserRepoResult carries per-call flags for the response.
type UserRepoResult struct {
	KbRepoPath          string
	KbRepoCreated       bool
	BranchProtectionSet bool
	UserNSReady         bool
}

// EnsureUserRepo runs the full personal-space KB provisioning pipeline.
//
// Steps:
//  1. Resolve user's Gitea username from UserGitBinding; fallback-provision
//     if missing or not synced.
//  2. Resolve tenant's GitServer.
//  3. Determine deterministic repo path via kb.KBRepoPathForUser.
//  4. Get-or-create the KB repo under the user's namespace.
//  5. Apply branch protection on main.
//
// All steps are idempotent — re-running for the same (user, code_repo_url)
// is a no-op with KbRepoCreated=false.
func (s *Service) EnsureUserRepo(
	ctx context.Context,
	userSubjectID, tenantID, codeRepoURL string,
) (*UserRepoResult, error) {
	// 1. Resolve user's Gitea identity.
	binding, err := s.resolveBinding(ctx, userSubjectID, tenantID)
	if err != nil {
		return nil, err
	}

	// 2. Resolve tenant's Git server.
	cfg, gitcli, err := s.resolveGitServer(ctx, tenantID)
	if err != nil {
		return nil, err
	}
	_ = gitcli // unused for now; repo ops use the gitsync.Client

	// 3. Compute deterministic repo path.
	kbRepoPath, err := kb.KBRepoPathForUser(codeRepoURL, binding.GitUsername)
	if err != nil {
		return nil, fmt.Errorf("userspace: %w", err)
	}
	owner, repoName := splitOwnerRepo(kbRepoPath)

	// 4. Ensure PAT exists (fallback for when ProvisionUser skipped PAT creation).
	if err := s.ensurePAT(ctx, binding.GitUsername, *binding.GitUID,
		&gitsync.GitServerConfig{
			Endpoint:      cfg.Endpoint,
			AdminToken:    cfg.AdminToken,
			AdminUser:     cfg.AdminUser,
			AdminPassword: cfg.AdminPassword,
		},
		userSubjectID, tenantID,
	); err != nil {
		return nil, fmt.Errorf("%w: ensure PAT: %v", ErrUserRepoProvisioning, err)
	}

	result := &UserRepoResult{
		KbRepoPath:  kbRepoPath,
		UserNSReady: true,
	}

	// 5. KB repo get-or-create.
	repo, err := gitcli.GetRepo(ctx, owner, repoName)
	if err != nil {
		return nil, fmt.Errorf("%w: get repo: %v", ErrUserRepoProvisioning, err)
	}
	if repo == nil {
		if _, err = gitcli.CreateRepo(ctx, owner, gitsync.CreateRepoOptions{
			Name:          repoName,
			Description:   "KB repo for " + codeRepoURL,
			Private:       true,
			AutoInit:      true,
			DefaultBranch: "main",
		}); err != nil {
			// Race: another concurrent ensure may have won the create.
			repo, lookupErr := gitcli.GetRepo(ctx, owner, repoName)
			if lookupErr != nil || repo == nil {
				return nil, fmt.Errorf("%w: create repo: %v (post-create lookup: %v)",
					ErrUserRepoProvisioning, err, lookupErr)
			}
		} else {
			result.KbRepoCreated = true
		}
	}

	// 6. Protect main.
	if err := s.applyBranchProtection(ctx, gitcli, owner, repoName, "main"); err != nil {
		return nil, err
	}
	result.BranchProtectionSet = true

	return result, nil
}

// resolveBinding looks up the UserGitBinding and falls back to ProvisionUser
// when missing or not synced.
func (s *Service) resolveBinding(ctx context.Context, subjectID, tenantID string) (*models.UserGitBinding, error) {
	var binding models.UserGitBinding
	err := s.db.WithContext(ctx).
		Where("user_subject_id = ? AND tenant_id = ?", subjectID, tenantID).
		First(&binding).Error
	if err == nil && binding.SyncStatus == models.GitSyncStatusSynced {
		return &binding, nil
	}

	// Not ready — try fallback provisioning.
	if s.userProvisionSvc != nil {
		// Best-effort: ignore errors (ProvisionUser logs internally).
		short := strings.ReplaceAll(subjectID, "-", "")
		if len(short) > 8 {
			short = short[:8]
		}
		_ = s.userProvisionSvc.ProvisionUser(ctx, gitsync.UserProvisionParams{
			SubjectID: subjectID,
			TenantID:  tenantID,
			Username:  "u-" + short,
		})
	}

	// Re-read after fallback.
	err = s.db.WithContext(ctx).
		Where("user_subject_id = ? AND tenant_id = ?", subjectID, tenantID).
		First(&binding).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, fmt.Errorf("%w: no UserGitBinding for subject=%q", ErrUserSpaceUnavailable, subjectID)
	}
	if err != nil {
		return nil, fmt.Errorf("userspace: re-read binding: %w", err)
	}
	if binding.SyncStatus != models.GitSyncStatusSynced {
		return nil, fmt.Errorf("%w: status=%s", ErrUserSpaceUnavailable, binding.SyncStatus)
	}
	return &binding, nil
}

// applyBranchProtection installs a "no direct push, no force push" rule
// on the given branch. Mirrors teamns.applyBranchProtection.
// Tolerates already-exists (idempotent).
func (s *Service) applyBranchProtection(ctx context.Context, gitcli *gitsync.Client, owner, repo, rule string) error {
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
	return fmt.Errorf("%w: branch protection %q: %v", ErrUserRepoProvisioning, rule, err)
}

// splitOwnerRepo splits "owner/repo" into its two components.
func splitOwnerRepo(path string) (string, string) {
	for i := 0; i < len(path); i++ {
		if path[i] == '/' {
			return path[:i], path[i+1:]
		}
	}
	return path, ""
}
