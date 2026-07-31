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
	"strings"
	"time"

	"github.com/costrict/costrict-web/server/internal/gitserver"
	"github.com/costrict/costrict-web/server/internal/models"
	"go.uber.org/zap"
	"gorm.io/gorm"
)

// provisionTimeout caps a single Provision call's total provider roundtrip.
const provisionTimeout = 5 * time.Second

// UserProvisionAPI is retained as a backwards-compat alias for GitProvider
// so existing fake implementations (handlers/fake_gitserver_test.go,
// teamns/workflow_repo_test.go) keep compiling during the rename window.
//
// New code should reference GitProvider directly. This alias will be
// removed once the test fakes are also renamed (separate cleanup).
type UserProvisionAPI = GitProvider

// UserProvisionParams is the input shape for ProvisionUser.
type UserProvisionParams struct {
	SubjectID   string
	TenantID    string
	ShortID     string
	Username    string
	DisplayName *string
	Email       *string
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
	if params.ShortID == "" {
		// ShortID is the platform-wide compact handle (cs-user-owned).
		// Missing it means the user.created payload is from a stale
		// deployment — refuse rather than invent a placeholder username.
		return errors.New("gitsync: ShortID is required")
	}

	tenantID := params.TenantID
	if tenantID == "" {
		tenantID = "default"
	}
	gitUsername := params.ShortID

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
		FullName:           userFullName(params),
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

	// 422 validation failure (email collision, password policy, ...): the
	// user was NOT created on the provider, so GetUserByName recovery is
	// pointless. Surface a clear error to the operator — the binding row
	// goes to 'error' state for human follow-up (the cs-user payload needs
	// fixing, not a retry). Falls through to markError below.
	if errors.Is(provErr, ErrGiteaValidationFailed) {
		s.logf("gitsync.ProvisionUser: provider rejected payload subject=%q username=%q err=%v",
			params.SubjectID, binding.GitUsername, provErr)
	}

	// 409 recovery: provider already has this username.
	if errors.Is(provErr, ErrUsernameTaken) {
		existing, lookupErr := provider.GetUserByName(provCtx, binding.GitUsername)
		if lookupErr == nil && existing != nil {
			if err := s.markSynced(ctx, binding, existing.ID, serverCfg.Kind); err != nil {
				s.logf("gitsync.ProvisionUser: markSynced (post-409) failed subject=%q err=%v", params.SubjectID, err)
				return fmt.Errorf("gitsync: mark synced (post-409): %w", err)
			}
			// Push current cs-user display_name to the pre-existing Gitea
			// account — without this, a user provisioned via 409-recovery
			// keeps Gitea's stale full_name forever. Best-effort: failure
			// to update display_name does NOT fail the provision itself.
			if newName := strings.TrimSpace(userFullName(params)); newName != "" && newName != existing.FullName {
				loginName := binding.GitUsername
				if _, editErr := provider.EditUser(provCtx, binding.GitUsername, EditUserOptions{
					LoginName: &loginName,
					FullName:  &newName,
				}); editErr != nil {
					s.logf("gitsync.ProvisionUser: post-409 display_name update failed subject=%q err=%v",
						params.SubjectID, editErr)
				}
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

// buildGitUsername was removed: Gitea login is now supplied by cs-user as
// ShortID (a platform-wide compact handle). See user.created payload.

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

// provisioningEmailDomain is the synthetic mailbox domain used for every
// Gitea account provisioned by this service. cs-user's email field is NOT
// globally unique (multiple Casdoor sources, or one source with shared
// mailboxes, can yield the same address), so forwarding it verbatim
// triggered Gitea 422 email-collision rejections under real workloads
// (see TestClient_CreateUser_EmailConflictReturnsValidationFailed for the
// original repro). ShortID is platform-unique by construction, so
// {short_id}@costrict.com is guaranteed collision-free.
const provisioningEmailDomain = "costrict.com"

func userEmail(params UserProvisionParams) string {
	// ShortID is the platform-unique handle (cs-user guarantees this) and
	// also the Gitea login name — using it as the email local-part keeps
	// the two consistent. SubjectID fallback is defensive; ProvisionUser
	// should never be invoked without ShortID since the binding row's
	// GitUsername (= ShortID) is what we POST as Login in the same call.
	handle := params.ShortID
	if handle == "" {
		handle = params.SubjectID
	}
	return handle + "@" + provisioningEmailDomain
}

// userFullName surfaces the human-readable name from cs-user's payload. Gitea
// login is a hash (buildGitUsername), so without full_name the Gitea web UI
// is unusable. Empty / nil → "" (Gitea treats blank as no full name).
func userFullName(params UserProvisionParams) string {
	if params.DisplayName != nil {
		return strings.TrimSpace(*params.DisplayName)
	}
	return ""
}

// randomProvisioningPassword returns a 32-byte random hex string. Throwaway
// — provider JWT / PAT middleware is the auth path, not passwords.
func randomProvisioningPassword() string {
	b := make([]byte, 32)
	_, _ = rand.Read(b)
	return fmt.Sprintf("%x", b)
}

// BackfillUser is one entry in a BackfillMissingBindings batch. Caller
// (admin handler) fills it from cs-user's user list; ShortID is mandatory
// because ProvisionUser uses it as the Git login.
type BackfillUser struct {
	SubjectID   string
	ShortID     string
	Username    string
	DisplayName *string
	Email       *string
}

// BackfillFailure records one user whose provisioning attempt returned an
// error. SubjectID + Error are surfaced to the admin for follow-up.
type BackfillFailure struct {
	SubjectID string `json:"subject_id"`
	Error     string `json:"error"`
}

// BackfillResult is the aggregate outcome of BackfillMissingBindings.
type BackfillResult struct {
	Total              int               `json:"total"`
	AlreadyBound       int               `json:"already_bound"`
	Provisioned        int               `json:"provisioned"`
	DisplayNameUpdated int               `json:"display_name_updated"`
	Skipped            int               `json:"skipped"`
	Failed             int               `json:"failed"`
	Failures           []BackfillFailure `json:"failures,omitempty"`
}

// BackfillOptions tweaks BackfillMissingBindings behaviour. Zero-value is
// the safe default (idempotent short-circuit on synced bindings, no extra
// Git API calls).
type BackfillOptions struct {
	// UpdateDisplayName, when true, pushes each user's current cs-user
	// display_name to the existing Gitea user via EditUser (PATCH
	// /admin/users/{username}). Applies to users whose binding was already
	// synced before this run — newly provisioned users already get the
	// display_name set via CreateUser. Each update is one synchronous Git
	// API call; cap input slice accordingly.
	UpdateDisplayName bool
}

// BackfillMissingBindings reconciles existing cs-user users against
// user_git_binding for one tenant. Used to migrate users created before
// user.created event processing was enabled (the "存量用户开户" path).
//
// For each input user:
//
//	ShortID empty                       → skipped (cannot derive git login)
//	synced binding already exists       → already_bound (idempotent short-circuit)
//	    + opts.UpdateDisplayName        → also pushes display_name to Gitea
//	otherwise ProvisionUser invoked     → provisioned (nil err) / failed (non-nil)
//
// One user's failure never aborts the batch — ProvisionUser is best-effort
// and the underlying binding row stays pending/error for retry. Caller is
// responsible for capping the input slice (admin handler enforces a hard
// max_users) since each ProvisionUser makes a synchronous Git API call.
func (s *UserProvisionService) BackfillMissingBindings(ctx context.Context, tenantID string, users []BackfillUser, opts BackfillOptions) BackfillResult {
	result := BackfillResult{Total: len(users)}
	if tenantID == "" {
		tenantID = "default"
	}

	// Pre-fetch this tenant's bindings once: (a) avoids N+1 lookups inside
	// ProvisionUser's upsertPendingBinding, (b) lets us distinguish
	// already-synced (idempotent no-op) from freshly provisioned, (c) gives
	// us the GitUsername needed for EditUser when UpdateDisplayName is set.
	type bindingInfo struct {
		Status      string
		GitUsername string
	}
	bound := make(map[string]bindingInfo, len(users))
	if s != nil && s.db != nil {
		var rows []models.UserGitBinding
		if err := s.db.WithContext(ctx).
			Where("tenant_id = ?", tenantID).
			Find(&rows).Error; err == nil {
			for _, r := range rows {
				bound[r.UserSubjectID] = bindingInfo{Status: r.SyncStatus, GitUsername: r.GitUsername}
			}
		}
	}

	for _, u := range users {
		if u.SubjectID == "" || u.ShortID == "" {
			result.Skipped++
			continue
		}
		if info := bound[u.SubjectID]; info.Status == models.GitSyncStatusSynced {
			result.AlreadyBound++
			if opts.UpdateDisplayName && shouldSyncDisplayName(u.DisplayName) {
				if err := s.syncDisplayName(ctx, tenantID, info.GitUsername, u.DisplayName); err == nil {
					result.DisplayNameUpdated++
				} else {
					result.Failed++
					result.Failures = append(result.Failures, BackfillFailure{
						SubjectID: u.SubjectID,
						Error:     "display_name sync: " + err.Error(),
					})
				}
			}
			continue
		}
		err := s.ProvisionUser(ctx, UserProvisionParams{
			SubjectID:   u.SubjectID,
			TenantID:    tenantID,
			ShortID:     u.ShortID,
			Username:    u.Username,
			DisplayName: u.DisplayName,
			Email:       u.Email,
		})
		if err == nil {
			result.Provisioned++
			continue
		}
		result.Failed++
		result.Failures = append(result.Failures, BackfillFailure{
			SubjectID: u.SubjectID,
			Error:     err.Error(),
		})
	}
	return result
}

// shouldSyncDisplayName returns true iff display_name is meaningfully set.
// nil / blank values are no-ops: we don't force-clear Gitea's existing
// full_name during reconciliation (would wipe data on users whose cs-user
// profile legitimately lacks display_name).
func shouldSyncDisplayName(displayName *string) bool {
	if displayName == nil {
		return false
	}
	return strings.TrimSpace(*displayName) != ""
}

// syncDisplayName resolves the tenant's git_server and PATCHes the user's
// full_name. Soft-skips (returns nil) when the tenant has no git_server —
// consistent with ProvisionUser's missing-server semantics. Caller must
// guard with shouldSyncDisplayName to avoid no-op EditUser calls; this
// helper additionally defends against nil/blank for direct callers.
func (s *UserProvisionService) syncDisplayName(ctx context.Context, tenantID, gitUsername string, displayName *string) error {
	if s == nil || s.resolver == nil || gitUsername == "" {
		return errors.New("gitsync: invalid syncDisplayName args")
	}
	if displayName == nil {
		return nil
	}
	name := strings.TrimSpace(*displayName)
	if name == "" {
		return nil
	}

	serverCfg, err := s.resolver.Resolve(ctx, tenantID)
	if err != nil {
		isSoftSkip := errors.Is(err, gitserver.ErrTenantMissingGitServer) ||
			errors.Is(err, gitserver.ErrGitServerNotFound) ||
			errors.Is(err, gitserver.ErrGitServerDisabled)
		if isSoftSkip {
			return nil
		}
		return fmt.Errorf("resolve git server: %w", err)
	}

	provCtx, cancel := context.WithTimeout(ctx, provisionTimeout)
	defer cancel()

	provider := s.providerFactory(GitServerConfig{
		Kind:       serverCfg.Kind,
		Endpoint:   serverCfg.Endpoint,
		AdminToken: serverCfg.AdminToken,
	})
	if provider == nil {
		return fmt.Errorf("nil git provider for kind=%q", serverCfg.Kind)
	}

	loginName := gitUsername
	if _, err := provider.EditUser(provCtx, gitUsername, EditUserOptions{
		LoginName: &loginName,
		FullName:  &name,
	}); err != nil {
		return fmt.Errorf("edit user %q: %w", gitUsername, err)
	}
	return nil
}
