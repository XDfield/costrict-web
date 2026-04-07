package systemrole

import (
	"errors"

	"github.com/costrict/costrict-web/server/internal/models"
	"github.com/google/uuid"
	"gorm.io/gorm"
)

var (
	ErrInvalidSystemRole           = errors.New("invalid system role")
	ErrSystemRoleUserNotFound      = errors.New("user not found")
	ErrCannotRevokeLastPlatformAdmin = errors.New("cannot revoke last platform admin")
)

type SystemRoleService struct {
	db *gorm.DB
}

func NewSystemRoleService(db *gorm.DB) *SystemRoleService {
	return &SystemRoleService{db: db}
}

func (s *SystemRoleService) ListRoles(userID string) ([]string, error) {
	var roles []string
	if err := s.db.Model(&models.UserSystemRole{}).
		Where("user_id = ? AND deleted_at IS NULL", userID).
		Order("created_at ASC").
		Pluck("role", &roles).Error; err != nil {
		return nil, err
	}
	return roles, nil
}

func (s *SystemRoleService) GetExpandedRoles(userID string) ([]string, error) {
	roles, err := s.ListRoles(userID)
	if err != nil {
		return nil, err
	}
	return ExpandRoles(roles), nil
}

func (s *SystemRoleService) GetCapabilities(userID string) ([]string, error) {
	roles, err := s.ListRoles(userID)
	if err != nil {
		return nil, err
	}
	return CapabilitiesForRoles(roles), nil
}

func (s *SystemRoleService) HasRole(userID, role string) (bool, error) {
	roles, err := s.GetExpandedRoles(userID)
	if err != nil {
		return false, err
	}
	for _, item := range roles {
		if item == role {
			return true, nil
		}
	}
	return false, nil
}

func (s *SystemRoleService) HasAnyRole(userID string, roles ...string) (bool, error) {
	expanded, err := s.GetExpandedRoles(userID)
	if err != nil {
		return false, err
	}
	set := make(map[string]struct{}, len(expanded))
	for _, role := range expanded {
		set[role] = struct{}{}
	}
	for _, role := range roles {
		if _, ok := set[role]; ok {
			return true, nil
		}
	}
	return false, nil
}

func (s *SystemRoleService) GrantRole(userID, role, operatorID string) error {
	if !IsValidRole(role) {
		return ErrInvalidSystemRole
	}

	var user models.User
	if err := s.db.Where("id = ?", userID).First(&user).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return ErrSystemRoleUserNotFound
		}
		return err
	}

	var existing models.UserSystemRole
	if err := s.db.Where("user_id = ? AND role = ? AND deleted_at IS NULL", userID, role).First(&existing).Error; err == nil {
		return nil
	} else if !errors.Is(err, gorm.ErrRecordNotFound) {
		return err
	}

	grant := models.UserSystemRole{
		ID:     uuid.NewString(),
		UserID: userID,
		Role:   role,
	}
	if operatorID != "" {
		grant.GrantedBy = &operatorID
	}
	return s.db.Create(&grant).Error
}

func (s *SystemRoleService) RevokeRole(userID, role, operatorID string) error {
	_ = operatorID
	if !IsValidRole(role) {
		return ErrInvalidSystemRole
	}

	return s.db.Transaction(func(tx *gorm.DB) error {
		var grant models.UserSystemRole
		if err := tx.Where("user_id = ? AND role = ? AND deleted_at IS NULL", userID, role).First(&grant).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return nil
			}
			return err
		}

		if role == SystemRolePlatformAdmin {
			var count int64
			if err := tx.Model(&models.UserSystemRole{}).
				Where("role = ? AND deleted_at IS NULL", SystemRolePlatformAdmin).
				Count(&count).Error; err != nil {
				return err
			}
			if count <= 1 {
				return ErrCannotRevokeLastPlatformAdmin
			}
		}

		return tx.Delete(&grant).Error
	})
}

func (s *SystemRoleService) ListUsersByRole(role string) ([]models.User, error) {
	if !IsValidRole(role) {
		return nil, ErrInvalidSystemRole
	}

	var users []models.User
	query := s.db.Model(&models.User{}).
		Distinct("users.*").
		Joins("JOIN user_system_roles usr ON usr.user_id = users.id AND usr.deleted_at IS NULL")

	if role == SystemRoleBusinessAdmin {
		query = query.Where("usr.role IN ?", []string{SystemRoleBusinessAdmin, SystemRolePlatformAdmin})
	} else {
		query = query.Where("usr.role = ?", role)
	}

	if err := query.Order("users.created_at ASC").Find(&users).Error; err != nil {
		return nil, err
	}
	return users, nil
}
