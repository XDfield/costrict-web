// Package userspace orchestrates personal-space KB repository provisioning.
//
// Mirrors the teamns package pattern but for user-owned repositories under
// the user's own Gitea namespace (not a team org). The Service ties together:
//
//   - UserGitBinding + UserCredentials tables (state),
//   - gitsync.UserProvisionService (Gitea user + PAT provisioning),
//   - gitserver.Resolver (per-tenant Git server discovery),
//   - crypto.AESGCM (encrypt/decrypt user PATs at rest),
//   - kb.KBRepoPathForUser (deterministic repo path algorithm).
//
// All operations are idempotent — the same (user, code_repo_url) pair always
// produces the same repo path and the same credentials.

package userspace

import (
	"context"
	"errors"
	"fmt"

	"github.com/costrict/costrict-web/server/internal/crypto"
	"github.com/costrict/costrict-web/server/internal/gitserver"
	"github.com/costrict/costrict-web/server/internal/gitsync"
	"github.com/costrict/costrict-web/server/internal/kb"
	"github.com/costrict/costrict-web/server/internal/models"
	"go.uber.org/zap"
	"gorm.io/gorm"
)

// Sentinel errors. Handlers map these to HTTP codes via errors.Is.
var (
	// ErrUserSpaceUnavailable — user's Gitea account is not ready (pending/error).
	ErrUserSpaceUnavailable = errors.New("userspace: user git account not ready")
	// ErrUserCredentialsMissing — no PAT on file (shouldn't happen after ProvisionUser).
	ErrUserCredentialsMissing = errors.New("userspace: user credentials missing")
	// ErrUserRepoProvisioning — Gitea-side repo create / branch protection failed.
	ErrUserRepoProvisioning = errors.New("userspace: user repo provisioning failed")
	// ErrGitServerUnresolved — tenant has no bound / enabled git server.
	ErrGitServerUnresolved = errors.New("userspace: tenant git server unresolved")
	// ErrTokenDecrypt — AES-GCM Open failed (key drift or row corruption).
	ErrTokenDecrypt = errors.New("userspace: token decrypt failed")
)

// Service owns the personal-space provisioning lifecycle.
type Service struct {
	db    *gorm.DB
	gres  gitserver.Resolver
	crypt *crypto.AESGCM
	log   *zap.Logger

	// userProvisionSvc is used as a fallback in EnsureUserRepo to create
	// the Gitea user + PAT when it hasn't been provisioned yet.
	userProvisionSvc *gitsync.UserProvisionService
}

// NewService wires a Service. gres MUST be non-nil. crypt may be nil (test
// path — credential ops will be skipped). logger may be nil.
func NewService(
	db *gorm.DB,
	gres gitserver.Resolver,
	userProvisionSvc *gitsync.UserProvisionService,
	crypt *crypto.AESGCM,
	logger *zap.Logger,
) *Service {
	if logger == nil {
		logger = zap.NewNop()
	}
	return &Service{
		db:               db,
		gres:             gres,
		crypt:            crypt,
		log:              logger,
		userProvisionSvc: userProvisionSvc,
	}
}

// UserSpaceInfo is returned in discovery mode to describe the current user's
// personal space readiness.
type UserSpaceInfo struct {
	UserSubjectID string `json:"user_subject_id"`
	GitUsername string `json:"git_username"`
	SyncStatus    string `json:"sync_status"` // "synced" | "pending" | "error" | ""
	Ready         bool   `json:"ready"`
}

// GetUserSpace returns the personal-space overview for the given user.
// Returns (nil, nil) when no UserGitBinding exists yet — the user hasn't been
// provisioned; callers surface an empty space rather than an error.
func (s *Service) GetUserSpace(ctx context.Context, userSubjectID, tenantID string) (*UserSpaceInfo, error) {
	var binding models.UserGitBinding
	err := s.db.WithContext(ctx).
		Where("user_subject_id = ? AND tenant_id = ?", userSubjectID, tenantID).
		First(&binding).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, nil // never provisioned
	}
	if err != nil {
		return nil, fmt.Errorf("userspace: lookup user_git_binding: %w", err)
	}

	return &UserSpaceInfo{
		UserSubjectID: binding.UserSubjectID,
		GitUsername: binding.GitUsername,
		SyncStatus:    binding.SyncStatus,
		Ready:         binding.SyncStatus == models.GitSyncStatusSynced,
	}, nil
}

// resolveGitServer resolves this tenant's backend config and builds a
// Gitea client. Returns ErrGitServerUnresolved when no server is bound.
func (s *Service) resolveGitServer(ctx context.Context, tenantID string) (*gitserver.Config, *gitsync.Client, error) {
	cfg, err := s.gres.Resolve(ctx, tenantID)
	if err != nil {
		return nil, nil, fmt.Errorf("%w: %v", ErrGitServerUnresolved, err)
	}
	client := gitsync.NewClient(cfg.Endpoint, cfg.AdminToken)
	if client == nil {
		return nil, nil, ErrGitServerUnresolved
	}
	return cfg, client, nil
}

// kbRepoPathForUser is the package-level wrapper around kb.KBRepoPathForUser.
func (s *Service) kbRepoPathForUser(codeRepoURL, gitUsername string) (string, error) {
	return kb.KBRepoPathForUser(codeRepoURL, gitUsername)
}
