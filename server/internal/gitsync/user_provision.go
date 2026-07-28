// User provisioning layer (Git Ownership Refactor).
//
// Server is the single owner of the Git server user account lifecycle;
// cs-user emits user.created events (Phase 2) that this layer consumes
// (Phase 4 removed cs-user's giteasync package entirely — this is now the
// only Gitea user-provisioning code in the project).
//
// Lifecycle of a binding row:
//
//	pending ── POST /admin/users 201 ──► synced
//	   │
//	   └── 4xx / 5xx / network ──► error (timeout keeps pending for retry)
//
// Best-effort contract: callers (event consumer) MUST ignore the returned
// error — a provider outage must never fail the event-ACK. The binding row
// stays in pending/error for the reconciliation cron (future) to repair.
//
// Provider-agnostic via the GitProvider interface (see git_provider.go).
// The factory dispatches on GitServerConfig.Kind to construct the right
// provider implementation; today only the Gitea provider is wired, but
// adding gitlab / enterprise is a self-contained change in
// defaultProviderFactory + a sibling provider file.

package gitsync

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/costrict/costrict-web/server/internal/gitserver"
	"github.com/costrict/costrict-web/server/internal/models"
	"go.uber.org/zap"
	"gorm.io/gorm"
)

// provisionTimeout caps a single Provision call's total provider roundtrip.
const provisionTimeout = 5 * time.Second

// gitUsernamePattern — login rule for the common Gitea-compatible providers
// (alphanumerics, dash, underscore, dot); we sanitize rather than reject.
var gitUsernamePattern = regexp.MustCompile(`[^a-zA-Z0-9._-]`)

// UserProvisionAPI is retained as a backwards-compat alias for GitProvider
// so existing fake implementations (handlers/fake_gitserver_test.go,
// teamns/workflow_repo_test.go) keep compiling during the rename window.
//
// New code should reference GitProvider directly. This alias will be
// removed once the test fakes are also renamed (separate cleanup).
type UserProvisionAPI = GitProvider

// UserProvisionParams is the input shape for ProvisionUser.
type UserProvisionParams struct {
	SubjectID string
	TenantID  string
	Username  string
	Email     *string
}

// UserLogger mirrors *zap.Logger — minimal interface for test stubs.
type UserLogger = *zap.Logger

// UserProvisionService owns the user_git_binding row lifecycle.
//
// Holds a gitserver.Resolver (local DB queries, not RPC) and constructs a
// transient GitProvider per Provision call via providerFactory. The default
// factory dispatches on GitServerConfig.Kind; tests override it.
type UserProvisionService struct {
	db       *gorm.DB
	resolver gitserver.Resolver
	logger   UserLogger

	// providerFactory builds a GitProvider from a resolved server config.
	// Defaults to defaultProviderFactory which switches on cfg.Kind;
	// tests override it with a stub returning a fakeGitProvider.
	providerFactory func(cfg GitServerConfig) GitProvider
}

// NewUserProvisionService binds a Service to its dependencies. resolver
// MUST be non-nil — the service cannot fall back to a global default.
// logger may be nil; a nop logger is used in that case.
func NewUserProvisionService(db *gorm.DB, resolver gitserver.Resolver, logger UserLogger) *UserProvisionService {
	if logger == nil {
		logger = zap.NewNop()
	}
	return &UserProvisionService{
		db:              db,
		resolver:        resolver,
		logger:          logger,
		providerFactory: defaultProviderFactory,
	}
}

// defaultProviderFactory dispatches on cfg.Kind and returns the matching
// GitProvider impl. Unknown / empty Kind falls back to Gitea for backward
// compatibility with pre-Kind-configured tenants (their git_servers row
// predates the Kind column, or RPC did not populate it).
//
// Returns nil for empty endpoint / token so the caller can surface a
// configuration error rather than panic.
func defaultProviderFactory(cfg GitServerConfig) GitProvider {
	switch cfg.Kind {
	case GitServerKindGitea, "": // "" = backward compat
		c := NewClient(cfg.Endpoint, cfg.AdminToken)
		if c == nil {
			return nil
		}
		return c
	default:
		// Unknown Kind: no provider available. Caller surfaces config error.
		return nil
	}
}

func (s *UserProvisionService) logf(format string, args ...any) {
	if s == nil || s.logger == nil {
		return
	}
	s.logger.Info(fmt.Sprintf(format, args...))
}

// ProvisionUser creates / refreshes the Git binding for one cs-user user.
//
// Idempotent: re-entry for a synced binding is a no-op.
// Best-effort: callers (event consumer) MUST ignore the returned error.
func (s *UserProvisionService) ProvisionUser(ctx context.Context, params UserProvisionParams) error {
	if s == nil {
		return errors.New("gitsync: nil user provision service")
	}
	if s.db == nil {
		return errors.New("gitsync: nil db")
	}
	if s.resolver == nil {
		return errors.New("gitsync: nil resolver")
	}
	if params.SubjectID == "" {
		return errors.New("gitsync: SubjectID is required")
	}

	tenantID := params.TenantID
	if tenantID == "" {
		tenantID = "default"
	}
	gitUsername := buildGitUsername(params.Username, params.SubjectID)

	// Insert (or fetch) the binding row in 'pending'. If a row already
	// exists in 'synced', short-circuit — idempotent.
	binding, freshlyInserted, err := s.upsertPendingBinding(ctx, params.SubjectID, tenantID, gitUsername)
	if err != nil {
		s.logf("gitsync.ProvisionUser: upsertPendingBinding failed subject=%q tenant=%q err=%v",
			params.SubjectID, tenantID, err)
		return fmt.Errorf("gitsync: upsert pending: %w", err)
	}
	if !freshlyInserted && binding.SyncStatus == models.GitSyncStatusSynced {
		return nil
	}

	// Resolve the tenant's git_server. Tenants without a bound server are
	// soft-skipped: leave the pending row for reconciliation, return nil so
	// the caller (event consumer) ACKs the event without retry.
	serverCfg, err := s.resolver.Resolve(ctx, tenantID)
	if err != nil {
		isSoftSkip := errors.Is(err, gitserver.ErrTenantMissingGitServer) ||
			errors.Is(err, gitserver.ErrGitServerNotFound) ||
			errors.Is(err, gitserver.ErrGitServerDisabled)
		if isSoftSkip {
			s.logf("gitsync.ProvisionUser: tenant %q has no bound git_server, skipping (user=%s)",
				tenantID, params.SubjectID)
			return nil
		}
		s.logf("gitsync.ProvisionUser: resolve git server failed subject=%q tenant=%q err=%v",
			params.SubjectID, tenantID, err)
		return fmt.Errorf("gitsync: resolve git server for tenant %q: %w", tenantID, err)
	}

	provCtx, cancel := context.WithTimeout(ctx, provisionTimeout)
	defer cancel()

	provider := s.providerFactory(GitServerConfig{
		Kind:       serverCfg.Kind,
		Endpoint:   serverCfg.Endpoint,
		AdminToken: serverCfg.AdminToken,
	})
	if provider == nil {
		s.logf("gitsync.ProvisionUser: nil provider for kind=%q subject=%q", serverCfg.Kind, params.SubjectID)
		return fmt.Errorf("gitsync: nil git provider for tenant %q (kind=%q)", tenantID, serverCfg.Kind)
	}

	providerUser, provErr := provider.CreateUser(provCtx, CreateUserOptions{
		Login:              binding.GitUsername,
		Email:              userEmail(params),
		FullName:           "",
		Password:           randomProvisioningPassword(),
		SourceID:           0,
		SendNotify:         false,
		MustChangePassword: false,
	})

	if provErr == nil {
		if err := s.markSynced(ctx, binding, providerUser.ID, serverCfg.Kind); err != nil {
			s.logf("gitsync.ProvisionUser: markSynced failed subject=%q err=%v", params.SubjectID, err)
			return fmt.Errorf("gitsync: mark synced: %w", err)
		}
		return nil
	}

	// 409 / 422 recovery: provider already has this user.
	if errors.Is(provErr, ErrUsernameTaken) {
		existing, lookupErr := provider.GetUserByName(provCtx, binding.GitUsername)
		if lookupErr == nil && existing != nil {
			if err := s.markSynced(ctx, binding, existing.ID, serverCfg.Kind); err != nil {
				s.logf("gitsync.ProvisionUser: markSynced (post-409) failed subject=%q err=%v", params.SubjectID, err)
				return fmt.Errorf("gitsync: mark synced (post-409): %w", err)
			}
			return nil
		}
		provErr = fmt.Errorf("%w; lookup also failed: %v", ErrUsernameTaken, lookupErr)
	}

	// Timeout keeps binding in 'pending' for retry; everything else → 'error'.
	if errors.Is(provErr, ErrGiteaTimeout) {
		s.logf("gitsync.ProvisionUser: timeout subject=%q username=%q — binding stays pending",
			params.SubjectID, binding.GitUsername)
		return provErr
	}

	if err := s.markError(ctx, binding, provErr.Error()); err != nil {
		s.logf("gitsync.ProvisionUser: markError failed subject=%q err=%v", params.SubjectID, err)
		return fmt.Errorf("gitsync: mark error: %w", err)
	}
	return provErr
}

// upsertPendingBinding inserts a 'pending' row if none exists, or returns
// the existing row. freshlyInserted=false on existing-row path.
func (s *UserProvisionService) upsertPendingBinding(ctx context.Context, subjectID, tenantID, gitUsername string) (*models.UserGitBinding, bool, error) {
	now := time.Now()
	row := &models.UserGitBinding{
		UserSubjectID: subjectID,
		TenantID:      tenantID,
		GitUsername:   gitUsername,
		SyncStatus:    models.GitSyncStatusPending,
		CreatedAt:     now,
		UpdatedAt:     now,
	}
	tx := s.db.WithContext(ctx).Create(row)
	if tx.Error == nil {
		return row, true, nil
	}
	if !isDuplicatePK(tx.Error) {
		return nil, false, tx.Error
	}
	var existing models.UserGitBinding
	if err := s.db.WithContext(ctx).
		Where("user_subject_id = ? AND tenant_id = ?", subjectID, tenantID).
		First(&existing).Error; err != nil {
		return nil, false, fmt.Errorf("race-recovery First: %w", err)
	}
	return &existing, false, nil
}

func (s *UserProvisionService) markSynced(ctx context.Context, b *models.UserGitBinding, gitUID int64, providerKind string) error {
	now := time.Now()
	updates := map[string]any{
		"sync_status":    models.GitSyncStatusSynced,
		"git_uid":        gitUID,
		"provider_kind":  providerKind,
		"last_synced_at": now,
		"last_error":     nil,
		"updated_at":     now,
	}
	if err := s.db.WithContext(ctx).Model(&models.UserGitBinding{}).
		Where("user_subject_id = ? AND tenant_id = ?", b.UserSubjectID, b.TenantID).
		Updates(updates).Error; err != nil {
		return err
	}
	b.SyncStatus = models.GitSyncStatusSynced
	b.GitUID = &gitUID
	b.ProviderKind = providerKind
	b.LastSyncedAt = &now
	b.LastError = nil
	b.UpdatedAt = now
	return nil
}

func (s *UserProvisionService) markError(ctx context.Context, b *models.UserGitBinding, reason string) error {
	now := time.Now()
	updates := map[string]any{
		"sync_status": models.GitSyncStatusError,
		"last_error":  reason,
		"updated_at":  now,
	}
	if err := s.db.WithContext(ctx).Model(&models.UserGitBinding{}).
		Where("user_subject_id = ? AND tenant_id = ?", b.UserSubjectID, b.TenantID).
		Updates(updates).Error; err != nil {
		return err
	}
	b.SyncStatus = models.GitSyncStatusError
	b.LastError = &reason
	b.UpdatedAt = now
	return nil
}

// buildGitUsername derives the provider login name from a cs-user username.
// "u-" + sanitized, truncated to 40 chars (Gitea hard limit; conservative
// across providers).
func buildGitUsername(username, subjectID string) string {
	raw := username
	if raw == "" {
		raw = subjectID
	}
	sanitized := gitUsernamePattern.ReplaceAllString(raw, "-")
	if sanitized == "" {
		sanitized = "user"
	}
	name := "u-" + sanitized
	if len(name) > 40 {
		name = name[:40]
	}
	return name
}

func isDuplicatePK(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "UNIQUE constraint failed") ||
		strings.Contains(msg, "duplicate key value") ||
		strings.Contains(msg, "duplicated key not allowed") ||
		strings.Contains(msg, "23505")
}

func userEmail(params UserProvisionParams) string {
	if params.Email != nil && *params.Email != "" {
		return *params.Email
	}
	return params.SubjectID + "@no-email.local"
}

// randomProvisioningPassword returns a 32-byte random hex string. Throwaway
// — provider JWT / PAT middleware is the auth path, not passwords.
func randomProvisioningPassword() string {
	b := make([]byte, 32)
	_, _ = rand.Read(b)
	return fmt.Sprintf("%x", b)
}
