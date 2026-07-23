// Package user implements cs-user's data access — both the read-side
// methods consumed by the read-through RPC client that costrict-web installs
// in P0-7, and the write-side methods that ship in Phase 2 to unblock the
// P0-8b operational cutover.
//
// Write paths do NOT verify JWT signatures — cs-user trusts the
// X-Internal-Token middleware (cs-user/internal/middleware/internal_auth.go)
// and treats JWTClaims as a pure data shape. No post-login hook fires here:
// the systemrole / bootstrap logic stays in server, and the cache
// invalidation responsibility is owned by server's RPCWriter (P0-8b) which
// calls CachedService.InvalidateCache after every successful RPC write.
package user

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/costrict/costrict-web/cs-user/internal/models"
	"github.com/costrict/costrict-web/cs-user/internal/tenant"
	"github.com/google/uuid"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// Default cap on SearchUsers when the caller doesn't pass one. Keeps a
// runaway query from yanking the whole table into memory if the RPC client
// forgets to clamp.
const defaultSearchLimit = 50

// syncInterval bounds how often GetOrCreateUser re-syncs an existing user's
// denormalized fields. Repeat logins inside this window skip the update
// query — keeps the write path off the hot path on every page view.
// Server's default is 15 minutes (server:557 region); we mirror it.
const syncInterval = 15 * time.Minute

// ErrLastIdentity signals an unbind refused because it would leave the user
// with zero identities (which would orphan the user row — login flow has no
// identity to re-bind on next visit). Maps to HTTP 409.
var ErrLastIdentity = errors.New("cannot unbind last identity")

// ErrExplicitlyUnbound signals a re-bind refused because the identity was
// previously explicitly unbound and ForceRebind is not set. Maps to HTTP 409.
// Server's same path returns nil silently; we surface a sentinel so the
// caller (RPCWriter) can distinguish "skipped" from "succeeded".
var ErrExplicitlyUnbound = errors.New("identity explicitly unbound; requires force_rebind")

// EventPublisher is the outbox writer surface (Git Ownership Refactor
// Phase 2). Declared here so user.Service can fire user.created events
// without importing eventbus (avoids an import cycle).
//
// nil means "feature disabled" — Publish is a no-op skip.
type EventPublisher interface {
	PublishUserCreated(ctx context.Context, user *models.User) error
}

// Service exposes the read-side operations costrict-web needs.
//
// Constructed once at boot (main.go wires it to the handlers); tests inject
// a gorm-backed sqlite DB so the same code path runs against real SQL.
type Service struct {
	db *gorm.DB

	// eventBus is the optional outbox writer (Phase 2). nil = feature
	// disabled. Set via SetEventPublisher after construction.
	eventBus EventPublisher
}

// NewService returns a Service bound to the supplied gorm pool. Callers own
// the pool's lifecycle (typically cs-user/internal/storage.Pool.Gorm).
func NewService(db *gorm.DB) *Service {
	return &Service{db: db}
}

// SetEventPublisher wires the optional outbox publisher (Phase 2). Pass
// nil to disable. Idempotent.
func (s *Service) SetEventPublisher(p EventPublisher) {
	if s == nil {
		return
	}
	s.eventBus = p
}

// GetUserByID returns the user with the given subject_id, or
// gorm.ErrRecordNotFound when no such row exists. The error is wrapped by
// the caller-facing layer so handlers can map it to HTTP 404 without
// importing gorm.
//
// B5: applies tenant.Scope(ctx) so the lookup is auto-filtered by
// tenant_id (Defaults to "default" when ctx carries no tenant — single-tenant
// safe).
func (s *Service) GetUserByID(ctx context.Context, subjectID string) (*models.User, error) {
	if s == nil || s.db == nil {
		return nil, errors.New("user.Service: nil db")
	}
	if subjectID == "" {
		return nil, ErrEmptySubjectID
	}

	var u models.User
	if err := s.db.WithContext(ctx).Scopes(tenant.Scope(ctx)).Where("subject_id = ?", subjectID).Take(&u).Error; err != nil {
		return nil, err
	}
	return &u, nil
}

// GetUsersByIDs returns a subject_id → User map for the given IDs. Missing
// rows are silently omitted (callers compare len(returned) vs len(input) to
// detect partial misses). Empty input returns an empty map without touching
// the DB — saves a round-trip on degenerate RPC calls.
//
// B5: applies tenant.Scope(ctx).
func (s *Service) GetUsersByIDs(ctx context.Context, subjectIDs []string) (map[string]*models.User, error) {
	out := make(map[string]*models.User)
	if s == nil || s.db == nil {
		return nil, errors.New("user.Service: nil db")
	}
	if len(subjectIDs) == 0 {
		return out, nil
	}

	var users []*models.User
	if err := s.db.WithContext(ctx).Scopes(tenant.Scope(ctx)).Where("subject_id IN ?", subjectIDs).Find(&users).Error; err != nil {
		return nil, err
	}
	for _, u := range users {
		out[u.SubjectID] = u
	}
	return out, nil
}

// SearchUsers returns active users whose username / display_name / email
// match the keyword (LIKE %keyword%, case-insensitive on Postgres via ILIKE
// is intentionally NOT used — server's existing search uses plain LIKE, so
// we match its behaviour to keep result sets comparable during cutover).
//
// limit ≤ 0 falls back to defaultSearchLimit.
//
// B5: applies tenant.Scope(ctx).
func (s *Service) SearchUsers(ctx context.Context, keyword string, limit int) ([]*models.User, error) {
	if s == nil || s.db == nil {
		return nil, errors.New("user.Service: nil db")
	}
	if limit <= 0 {
		limit = defaultSearchLimit
	}

	query := s.db.WithContext(ctx).Scopes(tenant.Scope(ctx)).Where("is_active = ?", true)
	if keyword != "" {
		pattern := "%" + keyword + "%"
		// Parens are load-bearing: without them SQL's AND binds tighter than
		// OR and the keyword filter leaks inactive rows back in.
		query = query.Where(
			"(username LIKE ? OR display_name LIKE ? OR email LIKE ?)",
			pattern, pattern, pattern,
		)
	}

	var users []*models.User
	err := query.Limit(limit).Find(&users).Error
	return users, err
}

// SearchUsersByEmployeeNumber 反查路径：用 employment_identities.employee_number
// 解析物理用户，供 team-namespace workflow 的 UserRef（doc v1.1 §5.2）使用。
//
// 与 SearchUsers 不同——后者基于 users 表的 username/display_name/email LIKE；
// 本方法走 employment_identities JOIN users，因为 employee_number 字段不在
// users 表上，而是 IdP sync 落到 employment_identities（Phase A4b）。
//
// tenant 范围由 ctx 上的 tenant.Scope(ctx) 决定，匹配 B5 约定。
//
// 非唯一性：本期 (tenant_id, employee_number) 唯一索引未落地（需要 enterprise_uid
// 字段先落地，见 EmploymentIdentity 模型注释）。命中多行时按 last_synced_at DESC
// 取最近一条；doc 中 `ambiguous` 严格语义推迟到 Phase B。
//
// 不返回错误：找不到匹配行时返回 (nil, nil) —— server 侧据此映射 HTTP 404。
func (s *Service) SearchUsersByEmployeeNumber(ctx context.Context, employeeNumber string, limit int) ([]*models.User, error) {
	if s == nil || s.db == nil {
		return nil, errors.New("user.Service: nil db")
	}
	if employeeNumber == "" {
		return nil, ErrEmptyEmployeeNumber
	}
	if limit <= 0 {
		limit = 1
	}

	tenantID := tenant.IDFromContext(ctx)

	// 子查询：每个 tenant 内匹配 employee_number 的最近 last_synced_at 行。
	// GORM 不擅长写 PARTITION BY，直接落原始 SQL — Postgres / sqlite 都支持
	// 这种 ROW_NUMBER 模式，避免在 Go 内存里去重。
	//
	// SQL 形如：
	//   SELECT u.* FROM users u
	//   JOIN employment_identities e
	//     ON e.user_subject_id = u.subject_id
	//    AND e.tenant_id = u.tenant_id
	//   WHERE u.tenant_id = ?
	//     AND u.is_active = true
	//     AND e.deleted_at IS NULL
	//     AND e.employee_number = ?
	//   ORDER BY e.last_synced_at DESC
	//   LIMIT ?
	var users []*models.User
	err := s.db.WithContext(ctx).
		Table("users AS u").
		Joins("JOIN employment_identities AS e "+
			"ON e.user_subject_id = u.subject_id "+
			"AND e.tenant_id = u.tenant_id").
		Where("u.tenant_id = ?", tenantID).
		Where("u.is_active = ?", true).
		Where("e.deleted_at IS NULL").
		Where("e.employee_number = ?", employeeNumber).
		Order("e.last_synced_at DESC, u.subject_id ASC").
		Limit(limit).
		Find(&users).Error
	if err != nil {
		return nil, err
	}
	return users, nil
}

// ErrEmptyEmployeeNumber signals a caller-programming error (empty
// employee_number). Surfaced as a sentinel so handlers map it to 400.
var ErrEmptyEmployeeNumber = errors.New("employee_number must not be empty")

// ListIdentities returns every auth identity bound to the user, ordered so
// the primary identity surfaces first (callers building "linked accounts"
// UI render it at the top).
//
// B5: applies tenant.Scope(ctx).
func (s *Service) ListIdentities(ctx context.Context, userSubjectID string) ([]*models.UserAuthIdentity, error) {
	if s == nil || s.db == nil {
		return nil, errors.New("user.Service: nil db")
	}
	if userSubjectID == "" {
		return nil, ErrEmptySubjectID
	}

	var identities []*models.UserAuthIdentity
	err := s.db.WithContext(ctx).
		Scopes(tenant.Scope(ctx)).
		Where("user_subject_id = ?", userSubjectID).
		Order("is_primary DESC, id ASC").
		Find(&identities).Error
	return identities, err
}

// ErrEmptySubjectID signals a caller-programming error (empty subject_id).
// Surfaced as a sentinel so handlers can map it to 400 without sniffing
// strings.
var ErrEmptySubjectID = errors.New("subject_id must not be empty")

// --- Write API (Phase 2) ---
//
// The 4 write methods below mirror server/internal/user/service.go's write
// surface 1:1, with three deliberate divergences:
//
//  1. No writeMode gate — cs-user has no kill switch. Server-side RPCWriter
//     owns the readonly gate via USER_SERVICE_WRITE_MODE.
//  2. No notifyUserUpdated / runPostLoginHook calls — cache invalidation
//     and the systemrole bootstrap hook live in server. RPCWriter calls
//     CachedService.InvalidateCache after every successful RPC write.
//  3. No createWecomChannelStateOnIDTrustBind / deleteWecomChannelStateOnIDTrustUnbind
//     — wecom channel state is a server-side notification concern.
//
// All external-key derivations and identity-selection ranks are byte-identical
// to server's helpers (see claims.go / identity.go) — load-bearing for the
// P0-8b dual-write canary, where a divergence would split a user's
// identities across DBs.

// GetOrCreateUser is the upsert entry point for OAuth login. It runs the
// full multi-lookup strategy (external_key → universal_id → casdoor_id →
// sub → username) and either updates an existing user or creates a new one
// plus a primary identity row. Mirrors server:557-870 — SyncUser collapses
// into this method because cs-user has no post-login hook to suppress.
//
// Idempotent: a second call with the same claim inside syncInterval skips
// the update query.
//
// Slice 2 (2026-07-23): both success paths trigger ApplyEnterpriseMapping
// as a best-effort post-login hook, harvesting claims.ExternalClaims via
// the tenant's employment_providers.field_map config. Failures are silently
// swallowed — enterprise mapping is a bonus feature and must not block
// login (a future refresh-on-relogin will retry).
func (s *Service) GetOrCreateUser(ctx context.Context, claims *models.JWTClaims) (*models.User, bool, error) {
	if s == nil || s.db == nil {
		return nil, false, errors.New("user.Service: nil db")
	}
	if claims == nil {
		return nil, false, fmt.Errorf("nil JWT claims")
	}
	claims = normalizeJWTClaims(claims)

	// 1. SubjectID is always generated locally and remains stable afterward.
	subjectID := "usr_" + uuid.NewString()
	externalKey := buildExternalKey(claims)

	if claims.ID == "" && claims.Sub == "" && claims.UniversalID == "" {
		return nil, false, fmt.Errorf("no valid user identifier in JWT claims")
	}

	// B5 write scoping: capture the tenant scope once. Applied per-query
	// rather than on the db handle itself — GORM's Statement propagation
	// between Scopes() and Create() is murky, and we don't want the tenant
	// WHERE clause bleeding into INSERTs (Create ignores WHERE in principle,
	// but the Statement is shared, so applying scope to the handle adds risk
	// for no benefit).
	db := s.db.WithContext(ctx)
	tenantScope := tenant.Scope(ctx)

	// 2. Try to get existing user by external identities first.
	var user models.User
	found := false

	lookupKeys := []string{externalKey}
	if legacy := legacyExternalKey(claims); legacy != "" && legacy != externalKey {
		lookupKeys = append(lookupKeys, legacy)
	}

	for _, key := range lookupKeys {
		if key == "" || found {
			break
		}
		var identity models.UserAuthIdentity
		if err := db.Scopes(tenantScope).Where("external_key = ?", key).Take(&identity).Error; err == nil {
			if err := db.Scopes(tenantScope).Where("subject_id = ?", identity.UserSubjectID).Take(&user).Error; err == nil {
				found = true
			}
		} else if !errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, false, fmt.Errorf("failed to query identity by external_key: %w", err)
		}
	}
	if !found {
		for _, key := range lookupKeys {
			if key == "" || found {
				break
			}
			err := db.Scopes(tenantScope).Where("external_key = ?", key).Take(&user).Error
			if err == nil {
				found = true
			} else if !errors.Is(err, gorm.ErrRecordNotFound) {
				return nil, false, fmt.Errorf("failed to query user by external_key: %w", err)
			}
		}
	}
	if claims.UniversalID != "" {
		err := db.Scopes(tenantScope).Where("casdoor_universal_id = ?", claims.UniversalID).Take(&user).Error
		if err == nil {
			found = true
		} else if !errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, false, fmt.Errorf("failed to query user by universal_id: %w", err)
		}
	}
	if !found && claims.ID != "" {
		err := db.Scopes(tenantScope).Where("casdoor_id = ?", claims.ID).Take(&user).Error
		if err == nil {
			found = true
		} else if !errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, false, fmt.Errorf("failed to query user by id: %w", err)
		}
	}
	if !found && claims.Sub != "" {
		err := db.Scopes(tenantScope).Where("casdoor_sub = ?", claims.Sub).Take(&user).Error
		if err == nil {
			found = true
		} else if !errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, false, fmt.Errorf("failed to query user by sub: %w", err)
		}
	}
	if !found && claims.Name != "" {
		err := db.Scopes(tenantScope).Where("username = ?", claims.Name).Take(&user).Error
		if err == nil {
			found = true
		} else if !errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, false, fmt.Errorf("failed to query user by username: %w", err)
		}
	}

	now := time.Now()

	if found {
		// Existing-user login refresh (REGISTRATION_PROFILE_DESIGN §1 / §7).
		// Only sync provider-tracking + Casdoor-linking fields. User-facing
		// profile (DisplayName / Email / Phone / AvatarURL / Organization /
		// Username) is user-owned — auto-clobbering on every re-login would
		// race the upcoming self-edit and registration-complete flows.
		shouldUpdate := false
		if user.LastSyncAt == nil || now.Sub(*user.LastSyncAt) > syncInterval {
			shouldUpdate = true
		}

		if user.SubjectID == "" {
			user.SubjectID = subjectID
			shouldUpdate = true
		}
		if !user.IsActive {
			user.IsActive = true
			shouldUpdate = true
		}
		if claims.ID != "" && (user.CasdoorID == nil || *user.CasdoorID != claims.ID) {
			user.CasdoorID = &claims.ID
			shouldUpdate = true
		}
		if externalKey != "" && (user.ExternalKey == nil || *user.ExternalKey != externalKey) {
			user.ExternalKey = &externalKey
			shouldUpdate = true
		}
		if claims.Provider != "" && (user.AuthProvider == nil || *user.AuthProvider != claims.Provider) {
			user.AuthProvider = &claims.Provider
			shouldUpdate = true
		}
		if claims.ProviderUserID != "" && (user.ProviderUserID == nil || *user.ProviderUserID != claims.ProviderUserID) {
			user.ProviderUserID = &claims.ProviderUserID
			shouldUpdate = true
		}
		if claims.UniversalID != "" && (user.CasdoorUniversalID == nil || *user.CasdoorUniversalID != claims.UniversalID) {
			user.CasdoorUniversalID = &claims.UniversalID
			shouldUpdate = true
		}
		if claims.Sub != "" && (user.CasdoorSub == nil || *user.CasdoorSub != claims.Sub) {
			user.CasdoorSub = &claims.Sub
			shouldUpdate = true
		}

		if shouldUpdate {
			user.LastSyncAt = &now
			if err := db.Omit("subject_id").Save(&user).Error; err != nil {
				return nil, false, fmt.Errorf("failed to update user: %w", err)
			}
		}
		s.applyEnterpriseMappingOnLogin(ctx, user.SubjectID, claims)
		return &user, false, nil
	}

	// 3. User doesn't exist, create new user.
	// B5 write scoping: stamp the ctx tenant onto the new row explicitly so
	// it doesn't fall through to the column default ('default') — that would
	// mis-file a user logging in via tenant 'acme' into the default tenant.
	user = models.User{
		TenantID:           tenant.IDFromContext(ctx),
		SubjectID:          subjectID,
		Username:           claims.Name,
		DisplayName:        stringPtr(claims.PreferredUsername),
		Email:              stringPtr(claims.Email),
		Phone:              stringPtr(claims.Phone),
		AvatarURL:          stringPtr(claims.Picture),
		AuthProvider:       stringPtr(claims.Provider),
		ExternalKey:        stringPtr(externalKey),
		ProviderUserID:     stringPtr(claims.ProviderUserID),
		CasdoorID:          stringPtr(claims.ID),
		CasdoorUniversalID: stringPtr(claims.UniversalID),
		CasdoorSub:         stringPtr(claims.Sub),
		Organization:       stringPtr(claims.Owner),
		IsActive:           true,
		LastLoginAt:        &now,
		LastSyncAt:         &now,
	}

	if err := db.Create(&user).Error; err != nil {
		// Race: another caller created the same user concurrently. Try to
		// resolve the winner via the same lookup chain so the caller still
		// gets a usable row. Must scope to ctx tenant — without this, a
		// casdoor_id collision across tenants would let this race recovery
		// return another tenant's user (cross-tenant leak under concurrency).
		var existing models.User
		query := db.Clauses(clause.Locking{Strength: "UPDATE"}).Scopes(tenantScope)
		if externalKey != "" {
			query = query.Where("external_key = ?", externalKey)
			if legacy := legacyExternalKey(claims); legacy != "" && legacy != externalKey {
				query = query.Or("external_key = ?", legacy)
			}
		}
		query = query.Or("casdoor_universal_id = ?", claims.UniversalID).
			Or("casdoor_id = ?", claims.ID).
			Or("casdoor_sub = ?", claims.Sub)
		if err := query.Take(&existing).Error; err == nil {
			return &existing, false, nil
		}
		return nil, false, fmt.Errorf("failed to create user: %w", err)
	}

	// Bind identity for newly created user.
	if err := s.BindIdentityToUser(ctx, user.SubjectID, claims); err != nil && !errors.Is(err, ErrIdentityAlreadyBound) {
		// Don't fail user creation if identity binding fails — the user row
		// exists and is usable; the next login will retry the bind.
		return nil, false, fmt.Errorf("failed to bind identity for new user: %w", err)
	}

	// Phase 2: enqueue a user.created event into the outbox so server can
	// take over Gitea provisioning asynchronously. Best-effort — failures
	// are logged by the publisher, not surfaced to the signup path. The
	// reconciliation cron (Phase 5) backfills rows that miss here.
	if s.eventBus != nil {
		_ = s.eventBus.PublishUserCreated(ctx, &user)
	}

	s.applyEnterpriseMappingOnLogin(ctx, user.SubjectID, claims)

	if refreshed, err := s.GetUserByID(ctx, user.SubjectID); err == nil {
		return refreshed, true, nil
	}
	return &user, true, nil
}

// applyEnterpriseMappingOnLogin is the best-effort post-login hook that
// refreshes the user's employment_identities snapshot from the OAuth claims.
// All errors are swallowed: enterprise mapping is a bonus feature and must
// not block login — malformed tenant config, missing tenant_configs row, or
// a transient DB error all leave the user row intact and let the next login
// retry. TenantID is read from ctx (multi-tenant); empty falls through to
// ApplyEnterpriseMapping's "default" fallback for Phase A single-tenant mode.
//
// claims.Provider empty (legacy Casdoor path without provider routing) skips
// the call — ApplyEnterpriseMapping would return a validation error anyway.
func (s *Service) applyEnterpriseMappingOnLogin(ctx context.Context, userSubjectID string, claims *models.JWTClaims) {
	if claims == nil || claims.Provider == "" {
		return
	}
	_ = s.ApplyEnterpriseMapping(ctx, EmploymentMappingParams{
		TenantID:       tenant.IDFromContext(ctx),
		UserSubjectID:  userSubjectID,
		Provider:       claims.Provider,
		ExternalClaims: claims.ExternalClaims,
	})
}

// ErrIdentityAlreadyBound signals a bind refused because the identity row
// already belongs to a different (non-deleted) user. Maps to HTTP 409.
// Server uses a bare fmt.Errorf("identity_already_bound"); promoting to a
// sentinel so handlers + tests can match without sniffing strings.
var ErrIdentityAlreadyBound = errors.New("identity already bound to another user")

// BindIdentityToUser attaches an identity to a user. Idempotent for the
// same (user, identity) pair. Recovers soft-deleted identities rather than
// creating duplicates (preserves audit history). Re-binding an
// ExplicitlyUnbound identity requires ForceRebind — matches server:246-367.
func (s *Service) BindIdentityToUser(ctx context.Context, userSubjectID string, claims *models.JWTClaims, opts ...models.BindIdentityOptions) error {
	if s == nil || s.db == nil {
		return errors.New("user.Service: nil db")
	}
	if strings.TrimSpace(userSubjectID) == "" {
		return fmt.Errorf("user_subject_id is required")
	}
	claims = normalizeJWTClaims(claims)
	if claims == nil {
		return fmt.Errorf("nil JWT claims")
	}
	externalKey := buildExternalKey(claims)
	if externalKey == "" {
		return fmt.Errorf("external key is required")
	}
	var opt models.BindIdentityOptions
	if len(opts) > 0 {
		opt = opts[0]
	}

	return s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		// B5 write scoping: every read / update / create in this tx is scoped
		// to tenant.IDFromContext(ctx). Captured once for clarity and reused
		// across all query chains — the scope is a pure func, so reapplying
		// the same closure to multiple chains is safe.
		scope := tenant.Scope(ctx)
		var existing models.UserAuthIdentity
		err := tx.Unscoped().Scopes(scope).Where("external_key = ?", externalKey).Take(&existing).Error
		if err == nil {
			if existing.UserSubjectID != userSubjectID {
				// Allow claiming if the identity was unbound (soft-deleted).
				if !existing.DeletedAt.Valid {
					return ErrIdentityAlreadyBound
				}
				updates := buildIdentityUpdates(&existing, claims)
				updates["user_subject_id"] = userSubjectID
				updates["deleted_at"] = nil
				updates["explicitly_unbound"] = false
				if len(updates) > 0 {
					if err := tx.Model(&existing).Unscoped().Scopes(scope).Updates(updates).Error; err != nil {
						return err
					}
				}
				return refreshUserProfileFromIdentitiesTx(ctx, tx, userSubjectID)
			}
			// Same user — skip restoring explicitly-unbound identities
			// unless ForceRebind is set. Server returns nil silently here;
			// cs-user surfaces ErrExplicitlyUnbound so RPCWriter can
			// distinguish "skipped" from "succeeded".
			if existing.ExplicitlyUnbound && !opt.ForceRebind {
				return ErrExplicitlyUnbound
			}
			updates := buildIdentityUpdates(&existing, claims)
			if existing.DeletedAt.Valid {
				updates["deleted_at"] = nil
			}
			if existing.ExplicitlyUnbound {
				updates["explicitly_unbound"] = false
			}
			if len(updates) > 0 {
				if err := tx.Model(&existing).Unscoped().Scopes(scope).Updates(updates).Error; err != nil {
					return err
				}
			}
			return refreshUserProfileFromIdentitiesTx(ctx, tx, userSubjectID)
		}
		if !errors.Is(err, gorm.ErrRecordNotFound) {
			return err
		}

		if legacy := legacyExternalKey(claims); legacy != "" && legacy != externalKey {
			err := tx.Unscoped().Scopes(scope).Where("external_key = ? AND user_subject_id = ?", legacy, userSubjectID).Take(&existing).Error
			if err == nil {
				if existing.ExplicitlyUnbound && !opt.ForceRebind {
					return ErrExplicitlyUnbound
				}
				updates := buildIdentityUpdates(&existing, claims)
				updates["external_key"] = externalKey
				if existing.DeletedAt.Valid {
					updates["deleted_at"] = nil
				}
				if existing.ExplicitlyUnbound {
					updates["explicitly_unbound"] = false
				}
				if err := tx.Model(&existing).Unscoped().Scopes(scope).Updates(updates).Error; err != nil {
					return err
				}
				return refreshUserProfileFromIdentitiesTx(ctx, tx, userSubjectID)
			}
			if !errors.Is(err, gorm.ErrRecordNotFound) {
				return err
			}
		}

		identity := buildUserAuthIdentity(userSubjectID, claims)
		// Stamp ctx tenant onto the new identity row — without this the
		// column default ('default') would mis-file an acme-tenant identity.
		identity.TenantID = tenant.IDFromContext(ctx)
		var currentPrimary models.UserAuthIdentity
		primaryExists := tx.Scopes(scope).Where("user_subject_id = ? AND is_primary = ?", userSubjectID, true).Take(&currentPrimary).Error == nil
		if !primaryExists {
			identity.IsPrimary = true
		} else if providerRank(identity.Provider) > providerRank(currentPrimary.Provider) {
			if err := tx.Model(&models.UserAuthIdentity{}).Scopes(scope).Where("user_subject_id = ?", userSubjectID).Update("is_primary", false).Error; err != nil {
				return err
			}
			identity.IsPrimary = true
		}

		if err := tx.Create(&identity).Error; err != nil {
			return err
		}
		return refreshUserProfileFromIdentitiesTx(ctx, tx, userSubjectID)
	})
}

// TransferIdentityToUser moves an identity (identified by external_key)
// from its current owner to targetUserSubjectID. Used for account merging
// when a user explicitly claims an identity bound to another account.
// Mirrors server:372-427 — no notifyUserUpdated; RPCWriter handles cache.
func (s *Service) TransferIdentityToUser(ctx context.Context, targetUserSubjectID string, externalKey string, _ string) error {
	if s == nil || s.db == nil {
		return errors.New("user.Service: nil db")
	}
	if targetUserSubjectID == "" || externalKey == "" {
		return fmt.Errorf("target_user_subject_id and external_key are required")
	}

	return s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		// B5 write scoping: scoped to ctx tenant — cross-tenant identity
		// transfer is not supported (the lookup would have to reach into
		// another tenant's identity row, which would be a security boundary
		// violation). An identity_not_found here thus means either "no such
		// external_key" or "exists in another tenant"; both surface the same.
		scope := tenant.Scope(ctx)
		var identity models.UserAuthIdentity
		if err := tx.Unscoped().Scopes(scope).Where("external_key = ?", externalKey).Take(&identity).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return fmt.Errorf("identity_not_found")
			}
			return err
		}

		oldUserSubjectID := identity.UserSubjectID
		if oldUserSubjectID == targetUserSubjectID {
			return nil // Already owned by this user.
		}

		now := time.Now()
		updates := map[string]interface{}{
			"user_subject_id": targetUserSubjectID,
			"updated_at":      now,
		}
		if identity.DeletedAt.Valid {
			updates["deleted_at"] = nil
		}
		if identity.ExplicitlyUnbound {
			updates["explicitly_unbound"] = false
		}
		if err := tx.Model(&identity).Unscoped().Scopes(scope).Updates(updates).Error; err != nil {
			return err
		}

		if err := refreshUserProfileFromIdentitiesTx(ctx, tx, targetUserSubjectID); err != nil {
			return err
		}
		if oldUserSubjectID != targetUserSubjectID {
			if err := refreshUserProfileFromIdentitiesTx(ctx, tx, oldUserSubjectID); err != nil {
				return err
			}
		}
		return nil
	})
}

// UnbindIdentityByProvider removes (soft-delete + ExplicitlyUnbound marker)
// every identity matching the provider on the user. Refuses to unbind the
// user's last identity — that would orphan the user row. Promotes the next
// best-rank identity to primary if the unbind removed the primary.
// Mirrors server:429-489 — no notifyUserUpdated; RPCWriter handles cache.
func (s *Service) UnbindIdentityByProvider(ctx context.Context, userSubjectID string, provider string) error {
	if s == nil || s.db == nil {
		return errors.New("user.Service: nil db")
	}
	provider = strings.ToLower(strings.TrimSpace(provider))
	if provider == "" {
		return fmt.Errorf("provider is required")
	}

	return s.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		// B5 write scoping: scoped to ctx tenant. The "last identity" guard,
		// the soft-delete, and the primary-promotion all operate only on
		// in-tenant identities — a cross-tenant caller must not be able to
		// unbind another tenant's identities, and equally must not see
		// out-of-tenant identities in its "remaining" promotion pool.
		scope := tenant.Scope(ctx)
		var matched []*models.UserAuthIdentity
		if err := tx.Scopes(scope).Where("user_subject_id = ? AND provider = ?", userSubjectID, provider).Find(&matched).Error; err != nil {
			return err
		}
		if len(matched) == 0 {
			return fmt.Errorf("identity not found")
		}

		var count int64
		if err := tx.Model(&models.UserAuthIdentity{}).Scopes(scope).Where("user_subject_id = ?", userSubjectID).Count(&count).Error; err != nil {
			return err
		}
		if count <= int64(len(matched)) {
			return ErrLastIdentity
		}

		hadPrimary := false
		for _, id := range matched {
			if id.IsPrimary {
				hadPrimary = true
			}
			// Soft delete + ExplicitlyUnbound prevents auto-rebinding on
			// next login (which would silently undo the user's intent).
			if err := tx.Model(&models.UserAuthIdentity{}).
				Scopes(scope).
				Where("id = ?", id.ID).
				Updates(map[string]interface{}{
					"explicitly_unbound": true,
					"deleted_at":         time.Now(),
				}).Error; err != nil {
				return err
			}
		}

		if hadPrimary {
			var remaining []*models.UserAuthIdentity
			if err := tx.Scopes(scope).Where("user_subject_id = ?", userSubjectID).Find(&remaining).Error; err != nil {
				return err
			}
			best := selectBestPrimary(remaining)
			if best != nil {
				if err := tx.Model(&models.UserAuthIdentity{}).Scopes(scope).Where("user_subject_id = ?", userSubjectID).Update("is_primary", false).Error; err != nil {
					return err
				}
				if err := tx.Model(&models.UserAuthIdentity{}).Scopes(scope).Where("id = ?", best.ID).Update("is_primary", true).Error; err != nil {
					return err
				}
			}
		}

		return refreshUserProfileFromIdentitiesTx(ctx, tx, userSubjectID)
	})
}
