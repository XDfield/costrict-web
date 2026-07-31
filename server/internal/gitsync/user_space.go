// Package gitsync — user personal-space status helpers.

package gitsync

import (
	"context"
	"errors"
	"fmt"

	"github.com/costrict/costrict-web/server/internal/models"
	"gorm.io/gorm"
)

// UserSpaceInfo describes the current user's personal space readiness.
type UserSpaceInfo struct {
	UserSubjectID string `json:"user_subject_id"`
	GitUsername   string `json:"git_username"`
	SyncStatus    string `json:"sync_status"` // "synced" | "pending" | "error" | ""
	Ready         bool   `json:"ready"`
}

// GetUserSpace returns the personal-space overview for the given user.
// Returns (nil, nil) when no UserGitBinding exists yet.
func GetUserSpace(ctx context.Context, db *gorm.DB, userSubjectID, tenantID string) (*UserSpaceInfo, error) {
	var binding models.UserGitBinding
	err := db.WithContext(ctx).
		Where("user_subject_id = ? AND tenant_id = ?", userSubjectID, tenantID).
		First(&binding).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("gitsync: lookup user_git_binding: %w", err)
	}

	return &UserSpaceInfo{
		UserSubjectID: binding.UserSubjectID,
		GitUsername:   binding.GitUsername,
		SyncStatus:    binding.SyncStatus,
		Ready:         binding.SyncStatus == models.GitSyncStatusSynced,
	}, nil
}
