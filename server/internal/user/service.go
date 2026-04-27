package user

import (
	"fmt"
	"time"

	"github.com/costrict/costrict-web/server/internal/authidentity"
	"github.com/costrict/costrict-web/server/internal/middleware"
	"github.com/costrict/costrict-web/server/internal/models"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// JWTClaims represents the parsed JWT token claims from Casdoor
type JWTClaims struct {
	ID                string
	Sub               string
	UniversalID       string
	Name              string
	PreferredUsername string
	Email             string
	Picture           string
	Owner             string
	Provider          string
	ProviderUserID    string
	Phone             string
}

// UserService provides user data operations
type UserService struct {
	db           *gorm.DB
	syncInterval time.Duration
}

// NewUserService creates a new UserService instance
func NewUserService(db *gorm.DB) *UserService {
	return &UserService{db: db, syncInterval: 15 * time.Minute}
}

// NewUserServiceWithConfig creates a new UserService instance with config
func NewUserServiceWithConfig(db *gorm.DB, syncIntervalMinutes int) *UserService {
	interval := time.Duration(syncIntervalMinutes) * time.Minute
	if syncIntervalMinutes <= 0 {
		interval = 15 * time.Minute
	}
	return &UserService{db: db, syncInterval: interval}
}

// GetUserByID retrieves a user by ID
func (s *UserService) GetUserByID(userID string) (*models.User, error) {
	var user models.User
	err := s.db.Where("subject_id = ?", userID).Take(&user).Error
	if err != nil {
		return nil, err
	}
	return &user, nil
}

// GetUsersByIDs retrieves multiple users by their IDs
func (s *UserService) GetUsersByIDs(userIDs []string) (map[string]*models.User, error) {
	if len(userIDs) == 0 {
		return make(map[string]*models.User), nil
	}

	var users []*models.User
	err := s.db.Where("subject_id IN ?", userIDs).Find(&users).Error
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
func (s *UserService) ResolveSubjectID(claims *JWTClaims) (string, string, error) {
	user, err := s.GetOrCreateUser(claims)
	if err != nil {
		return "", "", err
	}
	name := user.Username
	if user.DisplayName != nil && *user.DisplayName != "" {
		name = *user.DisplayName
	}
	return user.SubjectID, name, nil
}

// SearchUsers searches users by username or email keyword
func (s *UserService) SearchUsers(keyword string, limit int) ([]*models.User, error) {
	var users []*models.User
	query := s.db.Where("is_active = ?", true)

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

// GetOrCreateUser retrieves or creates a user based on JWT claims
// This should be called during login callback, not on every API request
func (s *UserService) GetOrCreateUser(claims *JWTClaims) (*models.User, error) {
	if claims == nil {
		return nil, fmt.Errorf("nil JWT claims")
	}
	claims = normalizeJWTClaims(claims)

	// 1. SubjectID is always generated locally and remains stable afterward.
	subjectID := "usr_" + uuid.NewString()
	externalKey := buildExternalKey(claims)

	if claims.ID == "" && claims.Sub == "" && claims.UniversalID == "" {
		return nil, fmt.Errorf("no valid user identifier in JWT claims")
	}

	// 2. Try to get existing user by external identities first.
	var user models.User
	found := false
	if externalKey != "" {
		err := s.db.Where("external_key = ?", externalKey).Take(&user).Error
		if err == nil {
			found = true
		} else if err != gorm.ErrRecordNotFound {
			return nil, fmt.Errorf("failed to query user by external_key: %w", err)
		}
	}
	if claims.UniversalID != "" {
		err := s.db.Where("casdoor_universal_id = ?", claims.UniversalID).Take(&user).Error
		if err == nil {
			found = true
		} else if err != gorm.ErrRecordNotFound {
			return nil, fmt.Errorf("failed to query user by universal_id: %w", err)
		}
	}
	if !found && claims.ID != "" {
		err := s.db.Where("casdoor_id = ?", claims.ID).Take(&user).Error
		if err == nil {
			found = true
		} else if err != gorm.ErrRecordNotFound {
			return nil, fmt.Errorf("failed to query user by id: %w", err)
		}
	}
	if !found && claims.Sub != "" {
		err := s.db.Where("casdoor_sub = ?", claims.Sub).Take(&user).Error
		if err == nil {
			found = true
		} else if err != gorm.ErrRecordNotFound {
			return nil, fmt.Errorf("failed to query user by sub: %w", err)
		}
	}
	if !found && claims.Name != "" {
		err := s.db.Where("username = ?", claims.Name).Take(&user).Error
		if err == nil {
			found = true
		} else if err != gorm.ErrRecordNotFound {
			return nil, fmt.Errorf("failed to query user by username: %w", err)
		}
	}

	now := time.Now()

	if found {
		// User exists, check if we need to update
		// Only update if it's been more than syncInterval since last sync to reduce DB writes
		shouldUpdate := false
		if user.LastSyncAt == nil || now.Sub(*user.LastSyncAt) > s.syncInterval {
			shouldUpdate = true
		}

		// Check if any critical fields need updating
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
		if claims.Owner != "" && (user.Organization == nil || *user.Organization != claims.Owner) {
			user.Organization = &claims.Owner
			shouldUpdate = true
		}
		if claims.PreferredUsername != "" && (user.DisplayName == nil || *user.DisplayName != claims.PreferredUsername) {
			user.DisplayName = &claims.PreferredUsername
			shouldUpdate = true
		}
		if claims.Email != "" && (user.Email == nil || *user.Email != claims.Email) {
			user.Email = &claims.Email
			shouldUpdate = true
		}
		if claims.Phone != "" && (user.Phone == nil || *user.Phone != claims.Phone) {
			user.Phone = &claims.Phone
			shouldUpdate = true
		}
		if claims.Picture != "" && (user.AvatarURL == nil || *user.AvatarURL != claims.Picture) {
			user.AvatarURL = &claims.Picture
			shouldUpdate = true
		}

		if shouldUpdate {
			user.LastLoginAt = &now
			user.LastSyncAt = &now
			if err := s.db.Save(&user).Error; err != nil {
				return nil, fmt.Errorf("failed to update user: %w", err)
			}
		}

		return &user, nil
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
			if externalKey != "" {
				query = query.Where("external_key = ?", externalKey)
			} else {
				query = query.Where("casdoor_universal_id = ?", claims.UniversalID)
			}
			query = query.Or("casdoor_universal_id = ?", claims.UniversalID).
				Or("casdoor_id = ?", claims.ID).
				Or("casdoor_sub = ?", claims.Sub)
			err := query.Take(&existing).Error
			if err == nil {
				return &existing, nil
			}
		}
		return nil, fmt.Errorf("failed to create user: %w", err)
	}

	return &user, nil
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
	if merged.UniversalID == "" {
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
	if claims.UniversalID != "" {
		return "casdoor:" + claims.UniversalID
	}
	if claims.Sub != "" {
		return "casdoor-sub:" + claims.Sub
	}
	if claims.ID != "" {
		return "casdoor-id:" + claims.ID
	}
	return ""
}

// stringPtr returns a pointer to string if non-empty, otherwise nil
func stringPtr(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}
