// Package userspace — credential encryption / decryption helpers.
//
// Mirrors teamns/credentials.go: AES-GCM encrypt+decrypt for user PATs
// stored in the user_credentials table.

package userspace

import (
	"context"
	"errors"
	"fmt"

	"github.com/costrict/costrict-web/server/internal/crypto"
	"github.com/costrict/costrict-web/server/internal/gitsync"
	"github.com/costrict/costrict-web/server/internal/models"
	"go.uber.org/zap"
	"gorm.io/gorm"
)

// DecryptUserToken returns the plaintext PAT for the given user. Callers
// MUST NOT log or persist the returned plaintext.
func (s *Service) DecryptUserToken(ctx context.Context, userSubjectID string) (string, error) {
	var creds models.UserCredentials
	if err := s.db.WithContext(ctx).
		Where("user_subject_id = ? AND revoked_at IS NULL", userSubjectID).
		First(&creds).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return "", ErrUserCredentialsMissing
		}
		return "", fmt.Errorf("userspace: lookup user_credentials: %w", err)
	}
	if s.crypt == nil {
		return "", fmt.Errorf("userspace: crypto not configured")
	}
	plaintext, err := s.crypt.Open(creds.TokenEncrypted)
	if err != nil {
		return "", fmt.Errorf("%w: %v", ErrTokenDecrypt, err)
	}
	return string(plaintext), nil
}

// LookupUserMeta fetches the active user credentials metadata (no plaintext).
// Returns (nil, nil) when no active credentials exist.
func (s *Service) LookupUserMeta(ctx context.Context, userSubjectID string) (*models.UserCredentials, error) {
	var creds models.UserCredentials
	err := s.db.WithContext(ctx).
		Where("user_subject_id = ? AND revoked_at IS NULL", userSubjectID).
		First(&creds).Error
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, nil
		}
		return nil, err
	}
	return &creds, nil
}

// ensurePAT checks if the user has active credentials. If not, it creates a
// PAT via the Gitea client and persists to user_credentials. Called as a
// fallback in EnsureUserRepo when ProvisionUser may have created the user
// but PAT creation was skipped (test path / key not configured at provision
// time).
func (s *Service) ensurePAT(
	ctx context.Context,
	gitUsername string,
	giteaUserID int64,
	cfg *gitsync.GitServerConfig,
	subjectID, tenantID string,
) error {
	if s.crypt == nil {
		return fmt.Errorf("userspace: crypto not configured, cannot create PAT")
	}

	// Check existing.
	var existing models.UserCredentials
	err := s.db.WithContext(ctx).
		Where("user_subject_id = ? AND revoked_at IS NULL", subjectID).
		First(&existing).Error
	if err == nil {
		return nil // already have credentials
	}
	if err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
		return fmt.Errorf("userspace: lookup existing user_credentials: %w", err)
	}

	// Build a provider with Basic Auth for the token-mint endpoint.
	provider := gitsync.NewClientWithBasicAuth(cfg.Endpoint, cfg.AdminToken, cfg.AdminUser, cfg.AdminPassword)
	if provider == nil {
		return fmt.Errorf("userspace: cannot create git client (configure admin_user/admin_password on git_server)")
	}

	sl := len(subjectID)
	if sl > 8 {
		sl = 8
	}
	tok, err := provider.CreateUserToken(ctx, gitUsername, gitsync.CreateUserTokenOptions{
		Name:   "user-pat-" + subjectID[:sl],
		Scopes: []string{"write:repository", "read:user"},
	})
	if err != nil {
		return fmt.Errorf("userspace: create user token: %w", err)
	}

	encrypted, err := s.crypt.Seal([]byte(tok.TokenPlaintext))
	if err != nil {
		return fmt.Errorf("userspace: encrypt token: %w", err)
	}

	creds := &models.UserCredentials{
		UserSubjectID:  subjectID,
		TenantID:       tenantID,
		GitServerID:    cfg.ServerID,
		GitUsername:  gitUsername,
		GitUserID:    giteaUserID,
		GitTokenID:   tok.ID,
		TokenEncrypted: encrypted,
		TokenSHA256:    crypto.SHA256Hex([]byte(tok.TokenPlaintext)),
	}
	if err := s.db.WithContext(ctx).Create(creds).Error; err != nil {
		return fmt.Errorf("userspace: persist user_credentials: %w", err)
	}

	s.log.Info("userspace: created PAT for user",
		zap.String("subject", subjectID),
		zap.String("git_username", gitUsername))
	return nil
}


