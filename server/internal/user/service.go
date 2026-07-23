package user

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/costrict/costrict-web/server/internal/authidentity"
	"github.com/costrict/costrict-web/server/internal/logger"
	"github.com/costrict/costrict-web/server/internal/middleware"
	"github.com/costrict/costrict-web/server/internal/models"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// JWTClaims represents the parsed JWT token claims from Casdoor
type JWTClaims struct {
	ID                string `json:"id,omitempty"`
	Sub               string `json:"sub,omitempty"`
	UniversalID       string `json:"universal_id,omitempty"`
	Name              string `json:"name,omitempty"`
	PreferredUsername string `json:"preferred_username,omitempty"`
	Email             string `json:"email,omitempty"`
	Picture           string `json:"picture,omitempty"`
	Owner             string `json:"owner,omitempty"`
	Provider          string `json:"provider,omitempty"`
	ProviderUserID    string `json:"provider_user_id,omitempty"`
	Phone             string `json:"phone,omitempty"`
	// ExternalClaims carries the raw IdP userinfo map (Profile.Raw from the
	// Casdoor OAuth callback) so cs-user can run field_map extraction on the
	// tenant's employment_providers config.
	ExternalClaims map[string]any `json:"external_claims,omitempty"`
}

// UserService provides user data operations
type UserService struct {
	db            *gorm.DB
	syncInterval  time.Duration
	onUserUpdated func(userSubjectID string)
	// writeMode gates write methods. Defaults to WriteModeLocal (writes go through).
	// When WriteModeReadonly, every write method returns ErrWriteBlocked before
	// touching the DB — kill switch for the P0-8 READONLY cutover.
	writeMode string
	// postLoginHook runs after a user is successfully fetched or created via
	// GetOrCreateUser, which is reserved for genuine login paths (the OAuth
	// callback and the JWKS auth-middleware path) where the bearer has proven they
	// own the identity. It deliberately does NOT fire for read-only background sync
	// (SyncUser, e.g. user-search backfill), so login-only side effects such as
	// bootstrap platform-admin granting never trigger when a third party merely
	// looks up a user. Injected from main.go (mirroring SetSubjectResolver) so the
	// user package needs no systemrole dependency. Must be best-effort and never
	// block login. nil = no hook (default behaviour unchanged).
	postLoginHook func(*models.User)
}

// NewUserService creates a new UserService instance
func NewUserService(db *gorm.DB) *UserService {
	return &UserService{db: db, syncInterval: 15 * time.Minute, writeMode: WriteModeLocal}
}

func NewUserServiceWithConfig(db *gorm.DB, syncIntervalMinutes int) *UserService {
	interval := time.Duration(syncIntervalMinutes) * time.Minute
	if syncIntervalMinutes <= 0 {
		interval = 15 * time.Minute
	}
	return &UserService{db: db, syncInterval: interval, writeMode: WriteModeLocal}
}

// SetWriteMode toggles the write gate. Wire from Module.NewWithConfig based on
// UserServiceConfig.WriteMode. Default is local (writes go through); readonly
// makes every write method return ErrWriteBlocked.
func (s *UserService) SetWriteMode(mode string) {
	s.writeMode = mode
}

func (s *UserService) SetOnUserUpdated(fn func(userSubjectID string)) {
	s.onUserUpdated = fn
}

func (s *UserService) notifyUserUpdated(userSubjectID string) {
	if s.onUserUpdated != nil {
		s.onUserUpdated(userSubjectID)
	}
}

// SetPostLoginHook installs a hook run after every successful GetOrCreateUser.
// Used to wire bootstrap platform-admin granting without the user package
// importing systemrole (cycle avoidance). The hook must be best-effort.
func (s *UserService) SetPostLoginHook(fn func(*models.User)) {
	s.postLoginHook = fn
}

func (s *UserService) runPostLoginHook(u *models.User) {
	if s.postLoginHook != nil && u != nil {
		s.postLoginHook(u)
	}
}

// GetUserByID retrieves a user by ID
func (s *UserService) GetUserByID(ctx context.Context, userID string) (*models.User, error) {
	var user models.User
	err := s.db.WithContext(ctx).Where("subject_id = ?", userID).Take(&user).Error
	if err != nil {
		return nil, err
	}
	return &user, nil
}

// GetUsersByIDs retrieves multiple users by their IDs
func (s *UserService) GetUsersByIDs(ctx context.Context, userIDs []string) (map[string]*models.User, error) {
	if len(userIDs) == 0 {
		return make(map[string]*models.User), nil
	}

	var users []*models.User
	err := s.db.WithContext(ctx).Where("subject_id IN ?", userIDs).Find(&users).Error
	if err != nil {
		return nil, err
	}

	userMap := make(map[string]*models.User, len(users))
	for _, user := range users {
		userMap[user.SubjectID] = user
	}
	return userMap, nil
}

// GetUsersByUniversalIDs retrieves multiple users by their Casdoor universal IDs.
func (s *UserService) GetUsersByUniversalIDs(universalIDs []string) (map[string]*models.User, error) {
	if len(universalIDs) == 0 {
		return make(map[string]*models.User), nil
	}

	var users []*models.User
	err := s.db.Where("casdoor_universal_id IN ?", universalIDs).Find(&users).Error
	if err != nil {
		return nil, err
	}

	userMap := make(map[string]*models.User, len(users))
	for _, user := range users {
		if user == nil || user.CasdoorUniversalID == nil || *user.CasdoorUniversalID == "" {
			continue
		}
		userMap[*user.CasdoorUniversalID] = user
	}
	return userMap, nil
}

// ResolveSubjectID resolves JWT/Casdoor claims to the stable local subject_id.
// This is a read-only lookup and does NOT trigger user creation or identity binding.
func (s *UserService) ResolveSubjectID(claims *JWTClaims) (string, string, error) {
	if claims != nil {
		logger.Info("[auth-debug] ResolveSubjectID in: id=%q sub=%q universal_id=%q name=%q provider=%q",
			claims.ID, claims.Sub, claims.UniversalID, claims.Name, claims.Provider)
	}
	user, err := s.FindUserByClaims(claims)
	if err != nil {
		logger.Warn("[auth-debug] ResolveSubjectID FindUserByClaims err=%v", err)
		return "", "", err
	}
	name := user.Username
	if user.DisplayName != nil && *user.DisplayName != "" {
		name = *user.DisplayName
	}
	logger.Info("[auth-debug] ResolveSubjectID out: subject_id=%q username=%q display_name=%q external_key=%q casdoor_universal_id=%q",
		user.SubjectID, user.Username, name, user.ExternalKey, user.CasdoorUniversalID)
	return user.SubjectID, name, nil
}

// FindUserByClaims performs a read-only lookup to find an existing user by JWT claims.
func (s *UserService) FindUserByClaims(claims *JWTClaims) (*models.User, error) {
	if claims == nil {
		return nil, fmt.Errorf("nil JWT claims")
	}
	claims = normalizeJWTClaims(claims)
	externalKey := buildExternalKey(claims)

	if claims.ID == "" && claims.Sub == "" && claims.UniversalID == "" {
		return nil, fmt.Errorf("no valid user identifier in JWT claims")
	}

	var user models.User

	lookupKeys := []string{externalKey}
	if legacy := legacyExternalKey(claims); legacy != "" && legacy != externalKey {
		lookupKeys = append(lookupKeys, legacy)
	}

	// Try identities first
	for _, key := range lookupKeys {
		if key == "" {
			break
		}
		var identity models.UserAuthIdentity
		if err := s.db.Where("external_key = ?", key).Take(&identity).Error; err == nil {
			if err := s.db.Where("subject_id = ?", identity.UserSubjectID).Take(&user).Error; err == nil {
				return &user, nil
			}
		}
	}
	// Try user table directly by external_key
	for _, key := range lookupKeys {
		if key == "" {
			break
		}
		if err := s.db.Where("external_key = ?", key).Take(&user).Error; err == nil {
			return &user, nil
		}
	}
	if claims.UniversalID != "" {
		if err := s.db.Where("casdoor_universal_id = ?", claims.UniversalID).Take(&user).Error; err == nil {
			return &user, nil
		}
	}
	if claims.ID != "" {
		if err := s.db.Where("casdoor_id = ?", claims.ID).Take(&user).Error; err == nil {
			return &user, nil
		}
	}
	if claims.Sub != "" {
		if err := s.db.Where("casdoor_sub = ?", claims.Sub).Take(&user).Error; err == nil {
			return &user, nil
		}
	}
	if claims.Name != "" {
		if err := s.db.Where("username = ?", claims.Name).Take(&user).Error; err == nil {
			return &user, nil
		}
	}

	return nil, fmt.Errorf("user not found")
}

// BindIdentityOptions controls the behavior of BindIdentityToUser.
type BindIdentityOptions struct {
	ForceRebind     bool
	UpdateLastLogin bool
}

// UpdateUserLastLogin updates the last_login_at timestamp for a user.
func (s *UserService) UpdateUserLastLogin(subjectID string) error {
	now := time.Now()
	return s.db.Model(&models.User{}).Where("subject_id = ?", subjectID).
		Update("last_login_at", now).Error
}

func (s *UserService) ListUserIdentities(ctx context.Context, userSubjectID string) ([]*models.UserAuthIdentity, error) {
	var identities []*models.UserAuthIdentity
	err := s.db.WithContext(ctx).Where("user_subject_id = ?", userSubjectID).Order("is_primary DESC, id ASC").Find(&identities).Error
	return identities, err
}

func (s *UserService) BindIdentityToUser(ctx context.Context, userSubjectID string, claims *JWTClaims, opts ...BindIdentityOptions) error {
	if s.writeMode == WriteModeReadonly {
		return ErrWriteBlocked
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
	var opt BindIdentityOptions
	if len(opts) > 0 {
		opt = opts[0]
	}

	return s.db.Transaction(func(tx *gorm.DB) error {
		var existing models.UserAuthIdentity
		err := tx.Unscoped().Where("external_key = ?", externalKey).Take(&existing).Error
		if err == nil {
			if existing.UserSubjectID != userSubjectID {
				// Allow claiming if the identity was unbound (soft-deleted).
				if !existing.DeletedAt.Valid {
					return fmt.Errorf("identity_already_bound")
				}
				updates := s.buildIdentityUpdates(&existing, claims)
				updates["user_subject_id"] = userSubjectID
				updates["deleted_at"] = nil
				updates["explicitly_unbound"] = false
				if opt.UpdateLastLogin {
					updates["last_login_at"] = time.Now()
				}
				if len(updates) > 0 {
					if err := tx.Model(&existing).Unscoped().Updates(updates).Error; err != nil {
						return err
					}
				}
				return s.refreshUserProfileFromIdentitiesTx(tx, userSubjectID)
			}
			// Skip restoring explicitly unbound identities unless forceRebind is set
			if existing.ExplicitlyUnbound && !opt.ForceRebind {
				return nil
			}
			updates := s.buildIdentityUpdates(&existing, claims)
			if opt.UpdateLastLogin {
				updates["last_login_at"] = time.Now()
			}
			if existing.DeletedAt.Valid {
				updates["deleted_at"] = nil
			}
			if existing.ExplicitlyUnbound {
				updates["explicitly_unbound"] = false
			}
			if len(updates) > 0 {
				if err := tx.Model(&existing).Unscoped().Updates(updates).Error; err != nil {
					return err
				}
			}
			return s.refreshUserProfileFromIdentitiesTx(tx, userSubjectID)
		}
		if err != gorm.ErrRecordNotFound {
			return err
		}

		if legacy := legacyExternalKey(claims); legacy != "" && legacy != externalKey {
			err := tx.Unscoped().Where("external_key = ? AND user_subject_id = ?", legacy, userSubjectID).Take(&existing).Error
			if err == nil {
				if existing.ExplicitlyUnbound && !opt.ForceRebind {
					return nil
				}
				updates := s.buildIdentityUpdates(&existing, claims)
				updates["external_key"] = externalKey
				if opt.UpdateLastLogin {
					updates["last_login_at"] = time.Now()
				}
				if existing.DeletedAt.Valid {
					updates["deleted_at"] = nil
				}
				if existing.ExplicitlyUnbound {
					updates["explicitly_unbound"] = false
				}
				if err := tx.Model(&existing).Unscoped().Updates(updates).Error; err != nil {
					return err
				}
				return s.refreshUserProfileFromIdentitiesTx(tx, userSubjectID)
			}
			if err != gorm.ErrRecordNotFound {
				return err
			}
		}

		identity := buildUserAuthIdentity(userSubjectID, claims)
		var currentPrimary models.UserAuthIdentity
		primaryExists := tx.Where("user_subject_id = ? AND is_primary = ?", userSubjectID, true).Take(&currentPrimary).Error == nil
		if !primaryExists {
			identity.IsPrimary = true
		} else if providerRank(identity.Provider) > providerRank(currentPrimary.Provider) {
			if err := tx.Model(&models.UserAuthIdentity{}).Where("user_subject_id = ?", userSubjectID).Update("is_primary", false).Error; err != nil {
				return err
			}
			identity.IsPrimary = true
		}

		if err := tx.Create(&identity).Error; err != nil {
			return err
		}

		// IDTrust绑定时自动创建企微应用channel状态
		if identity.Provider == "idtrust" {
			if err := s.createWecomChannelStateOnIDTrustBind(tx, userSubjectID); err != nil {
				// 记录错误但不影响绑定流程
				fmt.Printf("Warning: failed to create wecom channel state: %v\n", err)
			}
		}

		return s.refreshUserProfileFromIdentitiesTx(tx, userSubjectID)
	})
}

// TransferIdentityToUser transfers an identity (identified by externalKey) from its current
// owner to targetUserSubjectID. This is used for account merging when a user explicitly
// confirms that they want to claim an identity already bound to another account.
func (s *UserService) TransferIdentityToUser(ctx context.Context, targetUserSubjectID string, externalKey string, _ string) error {
	if s.writeMode == WriteModeReadonly {
		return ErrWriteBlocked
	}
	if targetUserSubjectID == "" || externalKey == "" {
		return fmt.Errorf("target_user_subject_id and external_key are required")
	}

	return s.db.Transaction(func(tx *gorm.DB) error {
		var identity models.UserAuthIdentity
		if err := tx.Unscoped().Where("external_key = ?", externalKey).Take(&identity).Error; err != nil {
			if err == gorm.ErrRecordNotFound {
				return fmt.Errorf("identity_not_found")
			}
			return err
		}

		oldUserSubjectID := identity.UserSubjectID
		if oldUserSubjectID == targetUserSubjectID {
			return nil // Already owned by this user
		}

		// Transfer the identity: update user_subject_id and restore if soft-deleted
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
		if err := tx.Model(&identity).Unscoped().Updates(updates).Error; err != nil {
			return err
		}

		// Refresh both users' profiles
		if err := s.refreshUserProfileFromIdentitiesTx(tx, targetUserSubjectID); err != nil {
			return err
		}
		if oldUserSubjectID != targetUserSubjectID {
			if err := s.refreshUserProfileFromIdentitiesTx(tx, oldUserSubjectID); err != nil {
				return err
			}
		}

		s.notifyUserUpdated(targetUserSubjectID)
		if oldUserSubjectID != targetUserSubjectID {
			s.notifyUserUpdated(oldUserSubjectID)
		}

		return nil
	})
}

func (s *UserService) UnbindIdentityByProvider(ctx context.Context, userSubjectID string, provider string) error {
	if s.writeMode == WriteModeReadonly {
		return ErrWriteBlocked
	}
	provider = strings.ToLower(strings.TrimSpace(provider))
	if provider == "" {
		return fmt.Errorf("provider is required")
	}

	return s.db.Transaction(func(tx *gorm.DB) error {
		var matched []*models.UserAuthIdentity
		if err := tx.Where("user_subject_id = ? AND provider = ?", userSubjectID, provider).Find(&matched).Error; err != nil {
			return err
		}
		if len(matched) == 0 {
			return fmt.Errorf("identity not found")
		}

		var count int64
		if err := tx.Model(&models.UserAuthIdentity{}).Where("user_subject_id = ?", userSubjectID).Count(&count).Error; err != nil {
			return err
		}
		if count <= int64(len(matched)) {
			return fmt.Errorf("cannot unbind last identity")
		}

		hadPrimary := false
		for _, id := range matched {
			if id.IsPrimary {
				hadPrimary = true
			}
			// Soft delete and mark as explicitly unbound to prevent auto-rebinding
			if err := tx.Model(&models.UserAuthIdentity{}).
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
			if err := tx.Where("user_subject_id = ?", userSubjectID).Find(&remaining).Error; err != nil {
				return err
			}
			best := selectBestPrimary(remaining)
			if best != nil {
				if err := tx.Model(&models.UserAuthIdentity{}).Where("user_subject_id = ?", userSubjectID).Update("is_primary", false).Error; err != nil {
					return err
				}
				if err := tx.Model(&models.UserAuthIdentity{}).Where("id = ?", best.ID).Update("is_primary", true).Error; err != nil {
					return err
				}
			}
		}

		return s.refreshUserProfileFromIdentitiesTx(tx, userSubjectID)
	})
}

// createWecomChannelStateOnIDTrustBind 在IDTrust绑定时创建企微应用channel状态
func (s *UserService) createWecomChannelStateOnIDTrustBind(tx *gorm.DB, userSubjectID string) error {
	// 检查是否已存在企微应用channel
	var existingChannel models.ChannelConfig
	err := tx.Where("user_id = ? AND channel_type = ? AND deleted_at IS NULL", userSubjectID, "wecom").
		First(&existingChannel).Error

	if err == nil {
		// 已存在，只更新enabled状态
		return tx.Model(&existingChannel).Update("enabled", true).Error
	} else if err != gorm.ErrRecordNotFound {
		return err
	}

	// 不存在则创建新的channel状态
	// Config字段留空，userId会从IDTrust绑定中动态获取
	channel := models.ChannelConfig{
		UserID:      userSubjectID,
		ChannelType: "wecom",
		Name:        "企微应用通知",
		Enabled:     true,
		Config:      nil, // 留空，由查询时动态组合
	}
	return tx.Create(&channel).Error
}

// deleteWecomChannelStateOnIDTrustUnbind 在IDTrust解绑时删除企微应用channel状态
func (s *UserService) deleteWecomChannelStateOnIDTrustUnbind(tx *gorm.DB, userSubjectID string) error {
	return tx.Where("user_id = ? AND channel_type = ?", userSubjectID, "wecom").
		Delete(&models.ChannelConfig{}).Error
}

// SearchUsers searches users by username or email keyword
func (s *UserService) SearchUsers(ctx context.Context, keyword string, limit int) ([]*models.User, error) {
	var users []*models.User
	query := s.db.WithContext(ctx).Where("is_active = ?", true)

	if keyword != "" {
		pattern := "%" + keyword + "%"
		query = query.Where(
			"username LIKE ? OR display_name LIKE ? OR email LIKE ?",
			pattern, pattern, pattern,
		)
	}

	if limit > 0 {
		query = query.Limit(limit)
	}

	err := query.Find(&users).Error
	return users, err
}

// GetOrCreateUser retrieves or creates a user based on JWT claims, then runs the
// post-login hook. It must only be called for a genuine login by the user
// themselves — the OAuth callback (handlers.go) and the JWKS auth path — not on
// every API request and NOT for read-only background sync.
//
// A successfully resolved user is passed through the injected post-login hook
// (SetPostLoginHook) — used for bootstrap platform-admin granting — so the hook
// covers every genuine login path regardless of which internal branch resolved
// the user. The hook is best-effort and must never block login.
//
// For read-only reconciliation (e.g. user-search backfill, where the caller is
// not the user being synced) use SyncUser instead, which performs the same upsert
// without firing the hook.
func (s *UserService) GetOrCreateUser(ctx context.Context, claims *JWTClaims) (*models.User, bool, error) {
	if s.writeMode == WriteModeReadonly {
		return nil, false, ErrWriteBlocked
	}
	u, isNew, err := s.getOrCreateUser(claims)
	if err != nil {
		return nil, false, err
	}
	s.runPostLoginHook(u)
	return u, isNew, nil
}

func (s *UserService) getOrCreateUser(claims *JWTClaims) (*models.User, bool, error) {
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
		if err := s.db.Where("external_key = ?", key).Take(&identity).Error; err == nil {
			if err := s.db.Where("subject_id = ?", identity.UserSubjectID).Take(&user).Error; err == nil {
				found = true
			}
		} else if err != gorm.ErrRecordNotFound {
			return nil, false, fmt.Errorf("failed to query identity by external_key: %w", err)
		}
	}
	if !found {
		for _, key := range lookupKeys {
			if key == "" || found {
				break
			}
			err := s.db.Where("external_key = ?", key).Take(&user).Error
			if err == nil {
				found = true
			} else if err != gorm.ErrRecordNotFound {
				return nil, false, fmt.Errorf("failed to query user by external_key: %w", err)
			}
		}
	}
	if claims.UniversalID != "" {
		err := s.db.Where("casdoor_universal_id = ?", claims.UniversalID).Take(&user).Error
		if err == nil {
			found = true
		} else if err != gorm.ErrRecordNotFound {
			return nil, false, fmt.Errorf("failed to query user by universal_id: %w", err)
		}
	}
	if !found && claims.ID != "" {
		err := s.db.Where("casdoor_id = ?", claims.ID).Take(&user).Error
		if err == nil {
			found = true
		} else if err != gorm.ErrRecordNotFound {
			return nil, false, fmt.Errorf("failed to query user by id: %w", err)
		}
	}
	if !found && claims.Sub != "" {
		err := s.db.Where("casdoor_sub = ?", claims.Sub).Take(&user).Error
		if err == nil {
			found = true
		} else if err != gorm.ErrRecordNotFound {
			return nil, false, fmt.Errorf("failed to query user by sub: %w", err)
		}
	}
	if !found && claims.Name != "" {
		err := s.db.Where("username = ?", claims.Name).Take(&user).Error
		if err == nil {
			found = true
		} else if err != gorm.ErrRecordNotFound {
			return nil, false, fmt.Errorf("failed to query user by username: %w", err)
		}
	}

	now := time.Now()

	if found {
		// Existing user login refresh. Only sync provider-tracking and
		// Casdoor-linking fields — user-facing profile (DisplayName, Email,
		// Phone, AvatarURL, Organization) is user-owned now and must not be
		// clobbered by re-login or by binding additional identities. Initial
		// values for those fields are populated at CREATE time below.
		shouldUpdate := false

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
			if err := s.db.Omit("subject_id").Save(&user).Error; err != nil {
				if errors.Is(err, gorm.ErrDuplicatedKey) {
					// Duplicate subject_id — another user row has the same value.
					// This is a data integrity issue; log a warning and reload
					// so the caller can proceed rather than failing outright.
					fmt.Printf("[WARN] Duplicate subject_id for user id=%d (subject_id=%q): %v\n", user.ID, user.SubjectID, err)
					var reloaded models.User
					if reloadErr := s.db.Where("id = ?", user.ID).Take(&reloaded).Error; reloadErr == nil {
						s.notifyUserUpdated(reloaded.SubjectID)
						return &reloaded, false, nil
					}
				}
				return nil, false, fmt.Errorf("failed to update user: %w", err)
			}
		}

		s.notifyUserUpdated(user.SubjectID)
		return &user, false, nil
	}

	// 3. User doesn't exist, create new user
	user = models.User{
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

	if err := s.db.Create(&user).Error; err != nil {
		if externalKey != "" || claims.UniversalID != "" || claims.ID != "" || claims.Sub != "" {
			var existing models.User
			query := s.db.Clauses(clause.Locking{Strength: "UPDATE"})
			conditions := s.db
			if externalKey != "" {
				conditions = conditions.Where("external_key = ?", externalKey)
				if legacy := legacyExternalKey(claims); legacy != "" && legacy != externalKey {
					conditions = conditions.Or("external_key = ?", legacy)
				}
			} else {
				conditions = conditions.Where("casdoor_universal_id = ?", claims.UniversalID)
			}
			query = conditions.Or("casdoor_universal_id = ?", claims.UniversalID).
				Or("casdoor_id = ?", claims.ID).
				Or("casdoor_sub = ?", claims.Sub)
			err := query.Take(&existing).Error
			if err == nil {
				s.notifyUserUpdated(existing.SubjectID)
				return &existing, true, nil
			}
		}
		return nil, false, fmt.Errorf("failed to create user: %w", err)
	}
		if err := s.db.Create(&user).Error; err != nil {
			if externalKey != "" || claims.UniversalID != "" || claims.ID != "" || claims.Sub != "" {
				var existing models.User
				query := s.db.Clauses(clause.Locking{Strength: "UPDATE"})
				conditions := s.db
				if externalKey != "" {
					conditions = conditions.Where("external_key = ?", externalKey)
					if legacy := legacyExternalKey(claims); legacy != "" && legacy != externalKey {
						conditions = conditions.Or("external_key = ?", legacy)
					}
				} else {
					conditions = conditions.Where("casdoor_universal_id = ?", claims.UniversalID)
				}
				query = conditions.Or("casdoor_universal_id = ?", claims.UniversalID).
					Or("casdoor_id = ?", claims.ID).
					Or("casdoor_sub = ?", claims.Sub)
				err := query.Take(&existing).Error
				if err == nil {
					s.notifyUserUpdated(existing.SubjectID)
					return &existing, true, nil
				}
			}
			return nil, false, fmt.Errorf("failed to create user: %w", err)
		}
		// Bind identity for newly created user
		if err := s.BindIdentityToUser(context.Background(), user.SubjectID, claims); err != nil && err.Error() != "identity_already_bound" {
			// Log but don't fail user creation if identity binding fails
			fmt.Printf("[WARN] Failed to bind identity for new user: %v\n", err)
		}
		s.notifyUserUpdated(user.SubjectID)
		if refreshed, err := s.GetUserByID(context.Background(), user.SubjectID); err == nil {
			return refreshed, true, nil
		}

		return &user, false, nil
	}

// ParseJWTClaimsFromMiddleware extracts JWT claims from gin.Context
// This is a helper to convert middleware context to JWTClaims
func ParseJWTClaimsFromMiddleware(c *gin.Context) (*JWTClaims, error) {
	if rawClaims, exists := c.Get(middleware.AuthClaimsKey); exists && rawClaims != nil {
		if authClaims, ok := rawClaims.(middleware.AuthClaims); ok {
			return normalizeJWTClaims(&JWTClaims{
				ID:                authClaims.ID,
				Sub:               authClaims.Sub,
				UniversalID:       authClaims.UniversalID,
				Name:              authClaims.Name,
				PreferredUsername: authClaims.PreferredUsername,
				Email:             authClaims.Email,
				Provider:          authClaims.Provider,
				ProviderUserID:    authClaims.ProviderUserID,
				Phone:             authClaims.Phone,
			}), nil
		}
	}

	userID, exists := c.Get(middleware.UserIDKey)
	if !exists || userID == nil {
		return nil, fmt.Errorf("user ID not found in context")
	}

	userIDStr, ok := userID.(string)
	if !ok {
		return nil, fmt.Errorf("invalid user ID type")
	}

	userName, _ := c.Get(middleware.UserNameKey)
	userNameStr, _ := userName.(string)

	// Extract from accessToken if available for more complete data
	// Otherwise use basic info from context
	return &JWTClaims{
		Sub:               userIDStr,
		Name:              userNameStr,
		PreferredUsername: userNameStr,
	}, nil
}

// ParseJWTClaimsFromAccessToken extracts relevant Casdoor claims from an access token.
// The token is obtained directly from Casdoor during login, so this helper only decodes
// claims to enrich profile data when /api/userinfo omits fields like id/universal_id.
func ParseJWTClaimsFromAccessToken(tokenString string) (*JWTClaims, error) {
	rawClaims, err := authidentity.ParseUnverifiedTokenClaims(tokenString)
	if err != nil {
		return nil, err
	}
	normalized := authidentity.NormalizeClaimsMap(rawClaims)

	result := &JWTClaims{
		ID:                normalized.ID,
		Sub:               normalized.Sub,
		UniversalID:       normalized.UniversalID,
		Name:              normalized.Name,
		PreferredUsername: normalized.PreferredUsername,
		Email:             normalized.Email,
		Picture:           normalized.Picture,
		Owner:             normalized.Owner,
		Provider:          normalized.Provider,
		ProviderUserID:    normalized.ProviderUserID,
		Phone:             normalized.Phone,
	}

	if result.ID == "" && result.Sub == "" && result.UniversalID == "" {
		return nil, fmt.Errorf("no user identifiers found in access token")
	}

	// Surface the raw Casdoor token payload (properties, signupApplication,
	// user, ...) as ExternalClaims so cs-user's employment_providers.field_map
	// can extract per-provider enterprise fields without server hard-coding
	// each IdP's property namespace. Lets field_map configs like
	//   properties.oauth_Custom.id → enterprise_uid
	// work for IdPs routed through Casdoor (idtrust, custom OAuth apps, ...).
	// We pass the whole raw map rather than cherry-picking keys so future
	// field_map configs can reach any token field without another server-side
	// change. cs-user's applyFieldMap walks dotted paths to get inside nested
	// sub-maps.
	result.ExternalClaims = rawClaims

	return result, nil
}

func MergeJWTClaims(base, override *JWTClaims) *JWTClaims {
	if base == nil {
		if override == nil {
			return nil
		}
		merged := *override
		return normalizeJWTClaims(&merged)
	}
	merged := *base
	if override == nil {
		return normalizeJWTClaims(&merged)
	}

	if merged.ID == "" {
		merged.ID = override.ID
	}
	if merged.Sub == "" {
		merged.Sub = override.Sub
	}
	// UniversalID: prefer override (raw Casdoor JWT) when present — the
	// signed JWT is the authoritative source for the user's stable id;
	// /api/getUserInfo (base) is best-effort and may omit or mismatch
	// universal_id for OAuth-brokered users (idtrust etc.). Downstream
	// (cs-user) treats universal_id as a hard dependency per MULTI_TENANCY
	// §12.1 — letting the JWT value win avoids the API path silently
	// dropping it.
	if override.UniversalID != "" {
		merged.UniversalID = override.UniversalID
	}
	if merged.Owner == "" {
		merged.Owner = override.Owner
	}
	if merged.Provider == "" {
		merged.Provider = override.Provider
	}
	if merged.ProviderUserID == "" {
		merged.ProviderUserID = override.ProviderUserID
	}
	if merged.Phone == "" {
		merged.Phone = override.Phone
	}

	if shouldPreferOverrideName(merged, *override) {
		merged.Name = override.Name
	}
	if override.PreferredUsername != "" {
		merged.PreferredUsername = override.PreferredUsername
	}
	if override.Email != "" {
		merged.Email = override.Email
	}
	if override.Picture != "" {
		merged.Picture = override.Picture
	}

	// ExternalClaims is the raw token-payload bag (properties.oauth_Custom_*,
	// signupApplication, ...). Always prefer override's when present — base
	// is the manually-constructed claims struct from AuthCallback and never
	// carries ExternalClaims itself. Shallow-assign on nil-base is enough;
	// deep-merge when both sides have entries so a future caller that
	// pre-populates ExternalClaims (e.g. trusted upstream) keeps its keys.
	if override.ExternalClaims != nil {
		if merged.ExternalClaims == nil {
			merged.ExternalClaims = override.ExternalClaims
		} else {
			for k, v := range override.ExternalClaims {
				if _, present := merged.ExternalClaims[k]; !present {
					merged.ExternalClaims[k] = v
				}
			}
		}
	}

	return normalizeJWTClaims(&merged)
}

func shouldPreferOverrideName(base, override JWTClaims) bool {
	if override.Name == "" {
		return false
	}
	if base.Name == "" {
		return true
	}
	if override.Provider == "idtrust" {
		return true
	}
	return false
}

func normalizeJWTClaims(claims *JWTClaims) *JWTClaims {
	if claims == nil {
		return nil
	}
	if claims.PreferredUsername == "" {
		claims.PreferredUsername = claims.Name
	}
	if claims.Name == "" && claims.PreferredUsername != "" {
		claims.Name = claims.PreferredUsername
	}
	if claims.Name == "" {
		if claims.Phone != "" {
			claims.Name = "phone_" + claims.Phone
		} else if claims.ProviderUserID != "" {
			claims.Name = claims.ProviderUserID
		}
	}
	return claims
}

func buildExternalKey(claims *JWTClaims) string {
	if claims == nil {
		return ""
	}
	provider := strings.ToLower(strings.TrimSpace(claims.Provider))
	if claims.UniversalID != "" {
		if provider != "" {
			return "casdoor:" + provider + ":" + claims.UniversalID
		}
		return "casdoor:" + claims.UniversalID
	}
	if claims.Sub != "" {
		if provider != "" {
			return "casdoor-sub:" + provider + ":" + claims.Sub
		}
		return "casdoor-sub:" + claims.Sub
	}
	if claims.ID != "" {
		return "casdoor-id:" + claims.ID
	}
	return ""
}

// BuildExternalKey is the public wrapper for buildExternalKey.
func BuildExternalKey(claims *JWTClaims) string {
	return buildExternalKey(claims)
}

func legacyExternalKey(claims *JWTClaims) string {
	if claims == nil {
		return ""
	}
	if claims.UniversalID != "" {
		return "casdoor:" + claims.UniversalID
	}
	if claims.Sub != "" {
		return "casdoor-sub:" + claims.Sub
	}
	return ""
}

func buildUserAuthIdentity(userSubjectID string, claims *JWTClaims) models.UserAuthIdentity {
	now := time.Now()
	externalKey := buildExternalKey(claims)
	provider := strings.ToLower(strings.TrimSpace(claims.Provider))
	if provider == "" {
		provider = "casdoor"
	}
	return models.UserAuthIdentity{
		UserSubjectID:   userSubjectID,
		Provider:        provider,
		ExternalKey:     externalKey,
		ExternalSubject: stringPtr(firstNonEmptyString(claims.UniversalID, claims.Sub)),
		ExternalUserID:  stringPtr(claims.ID),
		ProviderUserID:  stringPtr(claims.ProviderUserID),
		DisplayName:     stringPtr(claims.PreferredUsername),
		Email:           stringPtr(claims.Email),
		Phone:           stringPtr(claims.Phone),
		AvatarURL:       stringPtr(claims.Picture),
		Organization:    stringPtr(claims.Owner),
		LastLoginAt:     &now,
	}
}

func (s *UserService) buildIdentityUpdates(existing *models.UserAuthIdentity, claims *JWTClaims) map[string]interface{} {
	updates := make(map[string]interface{})

	if claims.PreferredUsername != "" && (existing.DisplayName == nil || *existing.DisplayName != claims.PreferredUsername) {
		updates["display_name"] = claims.PreferredUsername
	}
	if claims.Email != "" && (existing.Email == nil || *existing.Email != claims.Email) {
		updates["email"] = claims.Email
	}
	if claims.Phone != "" && (existing.Phone == nil || *existing.Phone != claims.Phone) {
		updates["phone"] = claims.Phone
	}
	if claims.Picture != "" && (existing.AvatarURL == nil || *existing.AvatarURL != claims.Picture) {
		updates["avatar_url"] = claims.Picture
	}
	if claims.Owner != "" && (existing.Organization == nil || *existing.Organization != claims.Owner) {
		updates["organization"] = claims.Owner
	}
	if claims.ProviderUserID != "" && (existing.ProviderUserID == nil || *existing.ProviderUserID != claims.ProviderUserID) {
		updates["provider_user_id"] = claims.ProviderUserID
	}
	if claims.UniversalID != "" && (existing.ExternalSubject == nil || *existing.ExternalSubject != claims.UniversalID) {
		updates["external_subject"] = claims.UniversalID
	}

	return updates
}

func providerRank(provider string) int {
	switch strings.ToLower(strings.TrimSpace(provider)) {
	case "idtrust":
		return 300
	case "github":
		return 200
	case "phone":
		return 100
	default:
		return 0
	}
}

func selectBestPrimary(identities []*models.UserAuthIdentity) *models.UserAuthIdentity {
	var best *models.UserAuthIdentity
	for _, identity := range identities {
		if identity == nil {
			continue
		}
		if best == nil || providerRank(identity.Provider) > providerRank(best.Provider) || (providerRank(identity.Provider) == providerRank(best.Provider) && identity.ID < best.ID) {
			best = identity
		}
	}
	return best
}

func (s *UserService) refreshUserProfileFromIdentitiesTx(tx *gorm.DB, userSubjectID string) error {
	var user models.User
	if err := tx.Where("subject_id = ?", userSubjectID).Take(&user).Error; err != nil {
		return err
	}
	var identities []*models.UserAuthIdentity
	if err := tx.Where("user_subject_id = ?", userSubjectID).Order("is_primary DESC, id ASC").Find(&identities).Error; err != nil {
		return err
	}
	if len(identities) == 0 {
		return nil
	}
	primary := selectBestPrimary(identities)
	if primary == nil {
		return nil
	}
	if !primary.IsPrimary {
		if err := tx.Model(&models.UserAuthIdentity{}).Where("user_subject_id = ?", userSubjectID).Update("is_primary", false).Error; err != nil {
			return err
		}
		if err := tx.Model(&models.UserAuthIdentity{}).Where("id = ?", primary.ID).Update("is_primary", true).Error; err != nil {
			return err
		}
	}

	// Refresh only provider-tracking fields to mirror the current primary
	// identity. User-facing profile fields (DisplayName, AvatarURL, Email,
	// Phone, Organization, Username) are intentionally NOT synced from
	// identity data — those are now considered user-owned, with enterprise
	// identity info living separately (cs-user employment_identities). The
	// follow-up plan is to let users self-edit display_name; auto-clobbering
	// it on every bind would race that flow.
	newAuthProvider := stringPtr(primary.Provider)
	newExternalKey := stringPtr(primary.ExternalKey)
	newProviderUserID := primary.ProviderUserID

	changed := !equalStringPtr(user.AuthProvider, newAuthProvider) ||
		!equalStringPtr(user.ExternalKey, newExternalKey) ||
		!equalStringPtr(user.ProviderUserID, newProviderUserID)

	if !changed {
		return nil
	}

	user.AuthProvider = newAuthProvider
	user.ExternalKey = newExternalKey
	user.ProviderUserID = newProviderUserID
	now := time.Now()
	user.LastSyncAt = &now
	// Omit columns with UNIQUE constraints (immutable after creation)
	if err := tx.Omit("subject_id", "username", "external_key").Save(&user).Error; err != nil {
		return err
	}
	return nil
}

func equalStringPtr(a, b *string) bool {
	if a == nil && b == nil {
		return true
	}
	if a == nil || b == nil {
		return false
	}
	return *a == *b
}

func firstNonEmptyString(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

func ptrString(v *string) string {
	if v == nil {
		return ""
	}
	return strings.TrimSpace(*v)
}

// stringPtr returns a pointer to string if non-empty, otherwise nil
func stringPtr(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}
