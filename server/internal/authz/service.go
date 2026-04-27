package authz

import (
	"errors"
	"fmt"
	"strings"

	"github.com/costrict/costrict-web/server/internal/middleware"
	"gorm.io/gorm"
)

// PermissionResult is the unified permission snapshot for a user.
type PermissionResult struct {
	Menus        []string `json:"menus"`
	APIs         []string `json:"apis"`
	Capabilities []string `json:"capabilities"`
}

// RoleProvider abstracts the source of user system roles.
type RoleProvider interface {
	ListRoles(userID string) ([]string, error)
	GetExpandedRoles(userID string) ([]string, error)
}

// CapabilityProvider abstracts the source of capability mappings.
type CapabilityProvider interface {
	CapabilitiesForRoles(roles []string) []string
}

type Service struct {
	db                 *gorm.DB
	roleProvider       RoleProvider
	capabilityProvider CapabilityProvider
	casdoorEndpoint    string
	jwksProvider       *middleware.JWKSProvider
}

func NewService(db *gorm.DB, roleProvider RoleProvider, capabilityProvider CapabilityProvider, casdoorEndpoint string, jwksProvider *middleware.JWKSProvider) *Service {
	return &Service{
		db:                 db,
		roleProvider:       roleProvider,
		capabilityProvider: capabilityProvider,
		casdoorEndpoint:    casdoorEndpoint,
		jwksProvider:       jwksProvider,
	}
}

// GetUserPermissions builds the full permission snapshot for a user.
func (s *Service) GetUserPermissions(userID string) (*PermissionResult, error) {
	roles, err := s.roleProvider.ListRoles(userID)
	if err != nil {
		return nil, fmt.Errorf("list roles: %w", err)
	}
	expanded, err := s.roleProvider.GetExpandedRoles(userID)
	if err != nil {
		return nil, fmt.Errorf("expand roles: %w", err)
	}
	var caps []string
	if s.capabilityProvider != nil {
		caps = s.capabilityProvider.CapabilitiesForRoles(roles)
	}

	menus := make([]string, 0, len(MenuResources))
	for code, allowed := range MenuResources {
		if len(allowed) == 0 || hasAny(expanded, allowed) {
			menus = append(menus, code)
		}
	}

	apis := make([]string, 0, len(APIResources))
	for code, allowed := range APIResources {
		if len(allowed) == 0 || hasAny(expanded, allowed) {
			apis = append(apis, code)
		}
	}

	return &PermissionResult{
		Menus:        menus,
		APIs:         apis,
		Capabilities: caps,
	}, nil
}

// HasPermission checks whether a user has access to a specific resource code.
func (s *Service) HasPermission(userID, resourceCode string) (bool, error) {
	// Try menu resources first, then API resources.
	allowed, ok := MenuResources.AllowedRoles(resourceCode)
	if !ok {
		allowed, ok = APIResources.AllowedRoles(resourceCode)
	}
	if !ok {
		return false, nil // unknown resource = deny by default
	}
	if len(allowed) == 0 {
		return true, nil // open to all authenticated users
	}

	roles, err := s.roleProvider.GetExpandedRoles(userID)
	if err != nil {
		return false, fmt.Errorf("list roles: %w", err)
	}
	return hasAny(roles, allowed), nil
}

// VerifyToken parses a bearer token to resolve the userID and then checks permission.
func (s *Service) VerifyToken(token, resourceCode string) (bool, *PermissionResult, error) {
	token = strings.TrimPrefix(token, "Bearer ")
	if token == "" {
		return false, nil, errors.New("empty token")
	}

	userInfo, err := middleware.ParseToken(token, s.casdoorEndpoint, s.jwksProvider)
	if err != nil {
		return false, nil, fmt.Errorf("parse token: %w", err)
	}

	userID := userInfo.Sub
	if resolver := middleware.GetSubjectResolver(); resolver != nil {
		resolvedID, _, err := resolver(middleware.AuthClaims{
			ID:                userInfo.ID,
			Sub:               userInfo.Sub,
			UniversalID:       userInfo.UniversalID,
			Name:              userInfo.Name,
			PreferredUsername: userInfo.PreferredUsername,
			Email:             userInfo.Email,
			Provider:          userInfo.Provider,
			ProviderUserID:    userInfo.ProviderUserID,
			Phone:             userInfo.Phone,
		})
		if err == nil && resolvedID != "" {
			userID = resolvedID
		}
	}

	allowed, err := s.HasPermission(userID, resourceCode)
	if err != nil {
		return false, nil, err
	}
	if !allowed {
		return false, nil, nil
	}

	perms, err := s.GetUserPermissions(userID)
	if err != nil {
		return false, nil, err
	}
	return true, perms, nil
}

func hasAny(have, want []string) bool {
	set := make(map[string]struct{}, len(have))
	for _, h := range have {
		set[h] = struct{}{}
	}
	for _, w := range want {
		if _, ok := set[w]; ok {
			return true
		}
	}
	return false
}
