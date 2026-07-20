// Service is the giteasync state-machine layer (Phase E3a.1).
//
// Owns the user_gitea_binding row lifecycle: inserts a 'pending' row, calls
// the GiteaClient to provision the account, transitions the row to 'synced'
// or 'error' based on the outcome. Idempotent — if a binding is already
// 'synced', Provision is a no-op.
//
// Best-effort contract: callers (user.Service.GetOrCreateUser) MUST ignore
// the returned error. A Gitea outage must never fail a successful signup —
// the users row is already committed; the binding row stays 'pending' /
// 'error' for the reconciliation cron (E3a.2) to repair.
//
// Records a user.gitea_provisioned audit row on every terminal transition
// so ops have a regulator-visible trail of provisioning attempts.
package giteasync

import (
	"context"
	"errors"
	"fmt"
	"log"
	"regexp"
	"strings"
	"time"

	"github.com/costrict/costrict-web/cs-user/internal/auditlog"
	"github.com/costrict/costrict-web/cs-user/internal/gitserver"
	"github.com/costrict/costrict-web/cs-user/internal/models"
	"gorm.io/gorm"
)

// provisionTimeout caps a single Provision call's total Gitea roundtrip
// budget. The same deadline covers both the initial POST /admin/users and
// the GET /users/{name} recovery lookup. Generous enough to absorb a slow
// Gitea startup; tight enough that OAuth callbacks don't stall users.
const provisionTimeout = 5 * time.Second

// giteaUsernamePattern is the Gitea username rule (per Gitea docs):
// alphanumerics, dash, underscore, dot; max 40 chars (we cap at 30 to
// leave room for the "u-" prefix). We sanitize the cs-user username
// rather than reject — provisioning succeeds with a slightly different
// Gitea username; the binding table records the actual value.
var giteaUsernamePattern = regexp.MustCompile(`[^a-zA-Z0-9._-]`)

// Logger mirrors auditlog.Logger — minimal interface stdlib *log.Logger
// satisfies.
type Logger interface {
	Printf(format string, args ...any)
}

// Service is the production giteasync. Construct via NewService.
//
// Per Phase E3b.1.1, the service no longer holds a fixed *GiteaClient —
// instead it resolves the tenant's git_server endpoint on each Provision
// call via the injected gitserver.Resolver, and constructs a transient
// Client. This fixes the E3a.1 bug where every tenant's users were
// provisioned against the same global Gitea endpoint.
type Service struct {
	db       *gorm.DB
	resolver gitserver.Resolver
	audit    *auditlog.Service
	logger   Logger

	// clientFactory builds a GiteaUserProvisioner from a resolved endpoint
	// + token. Defaults to NewClient; tests override it to inject a stub
	// without spinning up a real HTTP server.
	clientFactory func(endpoint, adminToken string) GiteaUserProvisioner
}

// NewService binds a Service to its dependencies.
//
// resolver is the per-tenant Git server resolver (production: a
// *gitserver.DBResolver). It MUST be non-nil; the service cannot fall back
// to a global default anymore (that was the bug this refactor fixes).
//
// audit may be nil — best-effort audit trail. logger may be nil; a default
// *log.Logger is allocated lazily.
func NewService(db *gorm.DB, resolver gitserver.Resolver, audit *auditlog.Service, logger Logger) *Service {
	return &Service{
		db:            db,
		resolver:      resolver,
		audit:         audit,
		logger:        logger,
		clientFactory: defaultClientFactory,
	}
}

// defaultClientFactory is the production factory — a plain wrapper around
// NewClient so the field is overridable in tests.
func defaultClientFactory(endpoint, adminToken string) GiteaUserProvisioner {
	return NewClient(endpoint, adminToken)
}

func (s *Service) logf(format string, args ...any) {
	if s == nil {
		return
	}
	lg := s.logger
	if lg == nil {
		lg = log.Default()
	}
	lg.Printf(format, args...)
}

// Provision creates / refreshes the Gitea binding for one cs-user user.
// Returns nil on success or if the binding is already synced.
//
// Idempotent: re-entry for a user whose binding is already 'synced' is a
// no-op. A 'pending' or 'error' row triggers a fresh provisioning attempt.
//
// Best-effort: the returned error MUST be ignored by callers. The
// structured logger captures failures at WARN for ops visibility.
func (s *Service) Provision(ctx context.Context, user *models.User) error {
	if s == nil {
		return errors.New("giteasync: nil service")
	}
	if s.db == nil {
		return errors.New("giteasync: nil db")
	}
	if s.resolver == nil {
		return errors.New("giteasync: nil resolver")
	}
	if user == nil || user.SubjectID == "" {
		return errors.New("giteasync: user.SubjectID is required")
	}

	tenantID := user.TenantID
	if tenantID == "" {
		tenantID = "default"
	}
	giteaUsername := buildGiteaUsername(user)

	// Insert (or fetch) the binding row in 'pending' state. If a row
	// already exists in 'synced' state, short-circuit — idempotent.
	binding, freshlyInserted, err := s.upsertPendingBinding(ctx, user.SubjectID, tenantID, giteaUsername)
	if err != nil {
		s.logf("giteasync.Provision: upsertPendingBinding failed subject=%q tenant=%q err=%v",
			user.SubjectID, tenantID, err)
		return fmt.Errorf("giteasync: upsert pending: %w", err)
	}
	if !freshlyInserted && binding.SyncStatus == models.GiteaSyncStatusSynced {
		// Already provisioned — nothing to do.
		return nil
	}

	// Cap the Gitea roundtrip with a dedicated timeout so a slow / hung
	// Gitea does not stall the OAuth callback indefinitely.
	provCtx, cancel := context.WithTimeout(ctx, provisionTimeout)
	defer cancel()

	// Resolve the per-tenant Git server endpoint + admin token, then build
	// a transient GiteaClient scoped to that server. Constructing a client
	// is cheap (just a struct + *http.Client), and provisioning is a cold
	// signup path — caching here would be premature (YAGNI).
	serverCfg, err := s.resolver.Resolve(provCtx, tenantID)
	if err != nil {
		s.logf("giteasync.Provision: resolve git server failed subject=%q tenant=%q err=%v",
			user.SubjectID, tenantID, err)
		return fmt.Errorf("giteasync: resolve git server for tenant %q: %w", tenantID, err)
	}
	client := s.clientFactory(serverCfg.Endpoint, serverCfg.AdminToken)

	giteaUser, provErr := client.ProvisionGiteaUser(provCtx, GiteaUserParams{
		Username:           binding.GiteaUsername,
		Email:              userEmail(user),
		Password:           randomProvisioningPassword(),
		SourceID:           0,
		MustChangePassword: false,
	})

	if provErr == nil {
		// Happy path: 201 from POST /admin/users. Stamp synced + UID.
		if err := s.markSynced(ctx, binding, giteaUser.ID); err != nil {
			s.logf("giteasync.Provision: markSynced failed subject=%q err=%v", user.SubjectID, err)
			return fmt.Errorf("giteasync: mark synced: %w", err)
		}
		s.recordAudit(ctx, user, tenantID, models.GiteaSyncStatusSynced, giteaUser.ID, "")
		return nil
	}

	// 409 recovery: Gitea already has this user. Look it up to recover
	// the UID and mark the binding synced — idempotent outcome.
	if errors.Is(provErr, ErrGiteaUserExists) {
		existing, lookupErr := client.LookupUserByName(provCtx, binding.GiteaUsername)
		if lookupErr == nil && existing != nil {
			if err := s.markSynced(ctx, binding, existing.ID); err != nil {
				s.logf("giteasync.Provision: markSynced (post-409) failed subject=%q err=%v", user.SubjectID, err)
				return fmt.Errorf("giteasync: mark synced (post-409): %w", err)
			}
			s.recordAudit(ctx, user, tenantID, models.GiteaSyncStatusSynced, existing.ID, "")
			return nil
		}
		// Lookup failed too — fall through to error-marking with the
		// composite reason.
		provErr = fmt.Errorf("%w; lookup also failed: %v", ErrGiteaUserExists, lookupErr)
	}

	// Timeout keeps binding in 'pending' so the reconciliation cron
	// (E3a.2) picks it up; everything else lands in 'error' for ops.
	if errors.Is(provErr, ErrGiteaTimeout) {
		s.logf("giteasync.Provision: timeout subject=%q username=%q — binding stays pending",
			user.SubjectID, binding.GiteaUsername)
		s.recordAudit(ctx, user, tenantID, models.GiteaSyncStatusPending, 0, provErr.Error())
		return provErr
	}

	// Non-timeout failure → terminal 'error' state.
	if err := s.markError(ctx, binding, provErr.Error()); err != nil {
		s.logf("giteasync.Provision: markError failed subject=%q err=%v", user.SubjectID, err)
		return fmt.Errorf("giteasync: mark error: %w", err)
	}
	s.recordAudit(ctx, user, tenantID, models.GiteaSyncStatusError, 0, provErr.Error())
	return provErr
}

// upsertPendingBinding inserts a 'pending' row if none exists, or returns
// the existing row. freshlyInserted=false on existing-row path so the
// caller can short-circuit on already-synced bindings.
//
// On INSERT of a duplicate (subject_id, tenant_id) PK we treat it as
// "another caller won the race" and fetch the winning row.
func (s *Service) upsertPendingBinding(ctx context.Context, subjectID, tenantID, giteaUsername string) (*models.UserGiteaBinding, bool, error) {
	now := time.Now()
	row := &models.UserGiteaBinding{
		UserSubjectID: subjectID,
		TenantID:      tenantID,
		GiteaUsername: giteaUsername,
		SyncStatus:    models.GiteaSyncStatusPending,
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
	// Race: another caller inserted first. Fetch their row.
	var existing models.UserGiteaBinding
	if err := s.db.WithContext(ctx).
		Where("user_subject_id = ? AND tenant_id = ?", subjectID, tenantID).
		First(&existing).Error; err != nil {
		return nil, false, fmt.Errorf("race-recovery First: %w", err)
	}
	return &existing, false, nil
}

// markSynced transitions a binding to 'synced' with the Gitea UID + a fresh
// last_synced_at stamp.
func (s *Service) markSynced(ctx context.Context, b *models.UserGiteaBinding, giteaUID int64) error {
	now := time.Now()
	updates := map[string]any{
		"sync_status":    models.GiteaSyncStatusSynced,
		"gitea_uid":      giteaUID,
		"last_synced_at": now,
		"last_error":     nil,
		"updated_at":     now,
	}
	if err := s.db.WithContext(ctx).Model(&models.UserGiteaBinding{}).
		Where("user_subject_id = ? AND tenant_id = ?", b.UserSubjectID, b.TenantID).
		Updates(updates).Error; err != nil {
		return err
	}
	b.SyncStatus = models.GiteaSyncStatusSynced
	b.GiteaUID = &giteaUID
	b.LastSyncedAt = &now
	b.LastError = nil
	b.UpdatedAt = now
	return nil
}

// markError transitions a binding to 'error' with the failure reason.
// last_synced_at is left untouched (NULL or its previous value).
func (s *Service) markError(ctx context.Context, b *models.UserGiteaBinding, reason string) error {
	now := time.Now()
	updates := map[string]any{
		"sync_status": models.GiteaSyncStatusError,
		"last_error":  reason,
		"updated_at":  now,
	}
	if err := s.db.WithContext(ctx).Model(&models.UserGiteaBinding{}).
		Where("user_subject_id = ? AND tenant_id = ?", b.UserSubjectID, b.TenantID).
		Updates(updates).Error; err != nil {
		return err
	}
	b.SyncStatus = models.GiteaSyncStatusError
	b.LastError = &reason
	b.UpdatedAt = now
	return nil
}

// recordAudit emits one user.gitea_provisioned row. Best-effort — errors
// are logged but do not bubble. Mirrors the C4.1 contract: audit failure
// must not change the user-visible outcome.
func (s *Service) recordAudit(ctx context.Context, user *models.User, tenantID, status string, giteaUID int64, errMsg string) {
	if s.audit == nil {
		return
	}
	payload := map[string]any{
		"sync_status":    status,
		"gitea_username": buildGiteaUsername(user),
	}
	if giteaUID > 0 {
		payload["gitea_uid"] = giteaUID
	}
	if errMsg != "" {
		payload["error"] = errMsg
	}
	subjectID := user.SubjectID
	targetID := fmt.Sprintf("user_gitea_binding:%s", subjectID)
	_ = s.audit.Record(ctx, auditlog.RecordParams{
		TenantID:       tenantID,
		ActorSubjectID: subjectID,
		Action:         models.ActionUserGiteaProvisioned,
		TargetType:     models.TargetTypeUserGiteaBinding,
		TargetID:       targetID,
		Payload:        payload,
	})
}

// buildGiteaUsername derives the Gitea login name from a cs-user user.
// Strategy: "u-" + sanitized username, truncated to 40 chars (Gitea hard
// limit). The sanitization replaces any character outside
// [a-zA-Z0-9._-] with a dash so the result always satisfies Gitea's
// username rule.
func buildGiteaUsername(user *models.User) string {
	raw := user.Username
	if raw == "" {
		raw = user.SubjectID
	}
	sanitized := giteaUsernamePattern.ReplaceAllString(raw, "-")
	if sanitized == "" {
		sanitized = "user"
	}
	// "u-" prefix namespaces auto-provisioned accounts away from any
	// human-named ones in the same Gitea instance.
	name := "u-" + sanitized
	if len(name) > 40 {
		name = name[:40]
	}
	return name
}

// isDuplicatePK returns true if err looks like a primary-key / unique
// constraint violation. GORM surfaces these as *pgconn.PgError (code
// 23505) on Postgres or as a generic "UNIQUE constraint failed" on
// sqlite; checking the error message keeps us driver-agnostic without
// pulling in pg-specific deps.
func isDuplicatePK(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "UNIQUE constraint failed") ||
		strings.Contains(msg, "duplicate key value") ||
		strings.Contains(msg, "23505")
}

// userEmail extracts the user's email, falling back to a synthetic
// placeholder when NULL (Gitea requires the field non-empty).
func userEmail(user *models.User) string {
	if user.Email != nil && *user.Email != "" {
		return *user.Email
	}
	return user.SubjectID + "@no-email.local"
}

// randomProvisioningPassword returns a 32-byte random hex string. The
// password is throwaway — the Gitea JWT middleware (E3a.3) is the auth
// path, not passwords. crypto/rand is used so the password is
// cryptographically strong even though it's never used.
func randomProvisioningPassword() string {
	b := make([]byte, 32)
	_, _ = randReader.Read(b)
	return fmt.Sprintf("%x", b)
}
