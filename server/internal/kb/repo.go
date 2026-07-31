// Package kb — per-user KB repo provisioning.
//
// EnsureUserRepo creates or reuses a KB repo under a user's personal
// Git namespace. Mirrors teamns.EnsureKBRepo with the owner being
// {git_username} instead of t-<team_short>.

package kb

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/costrict/costrict-web/server/internal/crypto"
	"github.com/costrict/costrict-web/server/internal/gitserver"
	"github.com/costrict/costrict-web/server/internal/gitsync"
	"github.com/costrict/costrict-web/server/internal/models"
	"go.uber.org/zap"
	"gorm.io/gorm"
)

// Sentinel errors for user repo provisioning.
var (
	ErrUserSpaceUnavailable = errors.New("kb: user git account not ready")
	ErrUserRepoProvisioning = errors.New("kb: user repo provisioning failed")
)

// UserRepoResult carries per-call flags for the response.
type UserRepoResult struct {
	KbRepoPath    string
	KbRepoCreated bool
	UserNSReady   bool
}

// EnsureUserRepo runs the full personal-space KB provisioning pipeline.
//
// Steps:
//  1. Resolve user's Gitea username from UserGitBinding; fallback-provision
//     if missing or not synced.
//  2. Resolve tenant's GitServer.
//  3. Determine deterministic repo path via KBRepoPathForUser.
//  4. Ensure PAT exists (fallback for when ProvisionUser skipped PAT creation).
//  5. Get-or-create the KB repo under the user's namespace.
//
// All steps are idempotent — re-running for the same (user, code_repo_url)
// is a no-op with KbRepoCreated=false.
func EnsureUserRepo(
	ctx context.Context,
	db *gorm.DB,
	gres gitserver.Resolver,
	crypt *crypto.AESGCM,
	log *zap.Logger,
	userProvisionSvc *gitsync.UserProvisionService,
	userSubjectID, tenantID, codeRepoURL string,
) (*UserRepoResult, error) {
	// 1. Resolve user's Gitea identity.
	binding, err := resolveBinding(ctx, db, userProvisionSvc, userSubjectID, tenantID)
	if err != nil {
		return nil, err
	}

	// 2. Resolve tenant's Git server.
	cfg, gitcli, err := resolveGitServer(ctx, gres, tenantID)
	if err != nil {
		return nil, err
	}

	// 3. Compute deterministic repo path.
	kbRepoPath, err := KBRepoPathForUser(codeRepoURL, binding.GitUsername)
	if err != nil {
		return nil, fmt.Errorf("kb: %w", err)
	}
	owner, repoName := splitOwnerRepo(kbRepoPath)

	// 4. Ensure PAT exists.
	if err := gitsync.EnsureUserPAT(ctx, db, crypt, log,
		gitsync.GitServerConfig{
			Endpoint:      cfg.Endpoint,
			AdminToken:    cfg.AdminToken,
			AdminUser:     cfg.AdminUser,
			AdminPassword: cfg.AdminPassword,
			ServerID:      cfg.ServerID,
		},
		userSubjectID, tenantID, binding.GitUsername, *binding.GitUID,
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
			repo, lookupErr := gitcli.GetRepo(ctx, owner, repoName)
			if lookupErr != nil || repo == nil {
				return nil, fmt.Errorf("%w: create repo: %v (post-create lookup: %v)",
					ErrUserRepoProvisioning, err, lookupErr)
			}
		} else {
			result.KbRepoCreated = true
		}
	}

	return result, nil
}

// resolveBinding looks up the UserGitBinding and falls back to ProvisionUser
// when missing or not synced.
func resolveBinding(ctx context.Context, db *gorm.DB, userProvisionSvc *gitsync.UserProvisionService, subjectID, tenantID string) (*models.UserGitBinding, error) {
	var binding models.UserGitBinding
	err := db.WithContext(ctx).
		Where("user_subject_id = ? AND tenant_id = ?", subjectID, tenantID).
		First(&binding).Error
	if err == nil && binding.SyncStatus == models.GitSyncStatusSynced {
		return &binding, nil
	}

	// Not ready — try fallback provisioning.
	if userProvisionSvc != nil {
		short := strings.ReplaceAll(subjectID, "-", "")
		if len(short) > 8 {
			short = short[:8]
		}
		_ = userProvisionSvc.ProvisionUser(ctx, gitsync.UserProvisionParams{
			SubjectID: subjectID,
			TenantID:  tenantID,
			ShortID:   "u-" + short,
		})
	}

	// Re-read after fallback.
	err = db.WithContext(ctx).
		Where("user_subject_id = ? AND tenant_id = ?", subjectID, tenantID).
		First(&binding).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, fmt.Errorf("%w: no UserGitBinding for subject=%q", ErrUserSpaceUnavailable, subjectID)
	}
	if err != nil {
		return nil, fmt.Errorf("kb: re-read binding: %w", err)
	}
	if binding.SyncStatus != models.GitSyncStatusSynced {
		return nil, fmt.Errorf("%w: status=%s", ErrUserSpaceUnavailable, binding.SyncStatus)
	}
	return &binding, nil
}

// resolveGitServer resolves the tenant's git server config and builds a client.
func resolveGitServer(ctx context.Context, gres gitserver.Resolver, tenantID string) (*gitserver.Config, *gitsync.Client, error) {
	cfg, err := gres.Resolve(ctx, tenantID)
	if err != nil {
		return nil, nil, fmt.Errorf("kb: resolve git server: %w", err)
	}
	client := gitsync.NewClient(cfg.Endpoint, cfg.AdminToken)
	if client == nil {
		return nil, nil, fmt.Errorf("kb: cannot create git client for tenant %s", tenantID)
	}
	return cfg, client, nil
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
