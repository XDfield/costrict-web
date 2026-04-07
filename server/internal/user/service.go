package user

import (
	"fmt"
	"time"

	"github.com/costrict/costrict-web/server/internal/models"
	"github.com/costrict/costrict-web/server/internal/middleware"
	"github.com/gin-gonic/gin"
	"github.com/golang-jwt/jwt/v4"
	"gorm.io/gorm"
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
}

// UserService provides user data operations
type UserService struct {
	db *gorm.DB
}

// NewUserService creates a new UserService instance
func NewUserService(db *gorm.DB) *UserService {
	return &UserService{db: db}
}

// GetUserByID retrieves a user by ID
func (s *UserService) GetUserByID(userID string) (*models.User, error) {
	var user models.User
	err := s.db.Where("id = ?", userID).First(&user).Error
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
	err := s.db.Where("id IN ?", userIDs).Find(&users).Error
	if err != nil {
		return nil, err
	}

	userMap := make(map[string]*models.User, len(users))
	for _, user := range users {
		userMap[user.ID] = user
	}
	return userMap, nil
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
	// 1. Determine user identifier (priority: id > sub > universal_id)
	userID := claims.ID
	if userID == "" {
		userID = claims.Sub
	}
	if userID == "" {
		userID = claims.UniversalID
	}

	if userID == "" {
		return nil, fmt.Errorf("no valid user identifier in JWT claims")
	}

	// 2. Try to get existing user
	var user models.User
	err := s.db.Where("id = ?", userID).First(&user).Error

	now := time.Now()

	if err == nil {
		// User exists, update last login time and mutable fields
		user.LastLoginAt = &now
		user.LastSyncAt = &now
		user.IsActive = true

		if claims.ID != "" {
			user.CasdoorID = &claims.ID
		}
		if claims.UniversalID != "" {
			user.CasdoorUniversalID = &claims.UniversalID
		}
		if claims.Sub != "" {
			user.CasdoorSub = &claims.Sub
		}
		if claims.Owner != "" {
			user.Organization = &claims.Owner
		}

		// Update fields that may change
		if claims.PreferredUsername != "" {
			user.DisplayName = &claims.PreferredUsername
		}
		if claims.Email != "" {
			user.Email = &claims.Email
		}
		if claims.Picture != "" {
			user.AvatarURL = &claims.Picture
		}

		if err := s.db.Save(&user).Error; err != nil {
			return nil, fmt.Errorf("failed to update user: %w", err)
		}

		return &user, nil
	}

	if err != gorm.ErrRecordNotFound {
		return nil, fmt.Errorf("failed to query user: %w", err)
	}

	// 3. User doesn't exist, create new user
	user = models.User{
		ID:                   userID,
		Username:             claims.Name,
		DisplayName:          stringPtr(claims.PreferredUsername),
		Email:                stringPtr(claims.Email),
		AvatarURL:            stringPtr(claims.Picture),
		CasdoorID:            stringPtr(claims.ID),
		CasdoorUniversalID:   stringPtr(claims.UniversalID),
		CasdoorSub:           stringPtr(claims.Sub),
		Organization:         stringPtr(claims.Owner),
		IsActive:             true,
		LastLoginAt:          &now,
		LastSyncAt:           &now,
	}

	if err := s.db.Create(&user).Error; err != nil {
		return nil, fmt.Errorf("failed to create user: %w", err)
	}

	return &user, nil
}

// ParseJWTClaimsFromMiddleware extracts JWT claims from gin.Context
// This is a helper to convert middleware context to JWTClaims
func ParseJWTClaimsFromMiddleware(c *gin.Context) (*JWTClaims, error) {
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
		ID:                userIDStr,
		Sub:               userIDStr,
		Name:              userNameStr,
		PreferredUsername: userNameStr,
	}, nil
}

// ParseJWTClaimsFromAccessToken extracts relevant Casdoor claims from an access token.
// The token is obtained directly from Casdoor during login, so this helper only decodes
// claims to enrich profile data when /api/userinfo omits fields like id/universal_id.
func ParseJWTClaimsFromAccessToken(tokenString string) (*JWTClaims, error) {
	parser := jwt.Parser{}
	claims := jwt.MapClaims{}

	if _, _, err := parser.ParseUnverified(tokenString, claims); err != nil {
		return nil, fmt.Errorf("failed to parse access token claims: %w", err)
	}

	str := func(keys ...string) string {
		for _, key := range keys {
			if v, ok := claims[key]; ok {
				if s, ok := v.(string); ok && s != "" {
					return s
				}
			}
		}
		return ""
	}

	result := &JWTClaims{
		ID:                str("id"),
		Sub:               str("sub"),
		UniversalID:       str("universal_id"),
		Name:              str("name"),
		PreferredUsername: str("preferred_username"),
		Email:             str("email"),
		Picture:           str("picture", "avatar"),
		Owner:             str("owner"),
	}

	if result.ID == "" && result.Sub == "" && result.UniversalID == "" {
		return nil, fmt.Errorf("no user identifiers found in access token")
	}

	return result, nil
}

// stringPtr returns a pointer to string if non-empty, otherwise nil
func stringPtr(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}
