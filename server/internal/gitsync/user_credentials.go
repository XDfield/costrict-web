// Package gitsync — user credential encryption / decryption helpers.
//
// Provider-agnostic helpers for user_credentials table: decrypt PAT,
// lookup metadata, and fallback PAT creation.

package gitsync

import (
	"context"
	"errors"
	"fmt"

	"github.com/costrict/costrict-web/server/internal/crypto"
	"github.com/costrict/costrict-web/server/internal/models"
	"go.uber.org/zap"
	"gorm.io/gorm"
)

// Sentinel errors for user credential operations.
var (
	// ErrUserCredentialsMissing — no PAT on file.
	ErrUserCredentialsMissing = errors.New("gitsync: user credentials missing")
	// ErrUserTokenDecrypt — AES-GCM Open failed (key drift or row corruption).
	ErrUserTokenDecrypt = errors.New("gitsync: token decrypt failed")
)

// DecryptUserToken returns the plaintext PAT for the given user. Callers
// MUST NOT log or persist the returned plaintext.
func DecryptUserToken(ctx context.Context, db *gorm.DB, crypt *crypto.AESGCM, userSubjectID string) (string, error) {
	var creds models.UserCredentials
	if err := db.WithContext(ctx).
		Where("user_subject_id = ? AND revoked_at IS NULL", userSubjectID).
		First(&creds).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return "", ErrUserCredentialsMissing
		}
		return "", fmt.Errorf("gitsync: lookup user_credentials: %w", err)
	}
	if crypt == nil {
		return "", fmt.Errorf("gitsync: crypto not configured")
	}
	plaintext, err := crypt.Open(creds.TokenEncrypted)
	if err != nil {
		return "", fmt.Errorf("%w: %v", ErrUserTokenDecrypt, err)
	}
	return string(plaintext), nil
}

// LookupUserMeta fetches the active user credentials metadata (no plaintext).
// Returns (nil, nil) when no active credentials exist.
func LookupUserMeta(ctx context.Context, db *gorm.DB, userSubjectID string) (*models.UserCredentials, error) {
	var creds models.UserCredentials
	err := db.WithContext(ctx).
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

// EnsureUserPAT checks if the user has active credentials. If not, it creates
// a PAT via the Gitea client and persists to user_credentials. Called as a
// fallback when ProvisionUser may have created the Gitea account but PAT
// creation was skipped.
func EnsureUserPAT(
	ctx context.Context,
	db *gorm.DB,
	crypt *crypto.AESGCM,
	log *zap.Logger,
	cfg GitServerConfig,
	subjectID, tenantID, gitUsername string,
	gitUserID int64,
) error {
	if crypt == nil {
		return fmt.Errorf("gitsync: crypto not configured, cannot create PAT")
	}

	// Check existing.
	var existing models.UserCredentials
	err := db.WithContext(ctx).
		Where("user_subject_id = ? AND revoked_at IS NULL", subjectID).
		First(&existing).Error
	if err == nil {
		return nil // already have credentials
	}
	if err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
		return fmt.Errorf("gitsync: lookup existing user_credentials: %w", err)
	}

	// Build a provider with Basic Auth for the token-mint endpoint.
	provider := NewClientWithBasicAuth(cfg.Endpoint, cfg.AdminToken, cfg.AdminUser, cfg.AdminPassword)
	if provider == nil {
		return fmt.Errorf("gitsync: cannot create git client (configure admin_user/admin_password on git_server)")
	}

	sl := len(subjectID)
	if sl > 8 {
		sl = 8
	}
	tok, err := provider.CreateUserToken(ctx, gitUsername, CreateUserTokenOptions{
		Name:   "user-pat-" + subjectID[:sl],
		Scopes: []string{"write:repository", "read:user"},
	})
	if err != nil {
		return fmt.Errorf("gitsync: create user token: %w", err)
	}

	encrypted, err := crypt.Seal([]byte(tok.TokenPlaintext))
	if err != nil {
		return fmt.Errorf("gitsync: encrypt token: %w", err)
	}

	creds := &models.UserCredentials{
		UserSubjectID:  subjectID,
		TenantID:       tenantID,
		GitServerID:    cfg.ServerID,
		GitUsername:    gitUsername,
		GitUserID:      gitUserID,
		GitTokenID:     tok.ID,
		TokenEncrypted: encrypted,
		TokenSHA256:    crypto.SHA256Hex([]byte(tok.TokenPlaintext)),
	}
	if err := db.WithContext(ctx).Create(creds).Error; err != nil {
		return fmt.Errorf("gitsync: persist user_credentials: %w", err)
	}

	if log != nil {
		log.Info("gitsync: created PAT for user",
			zap.String("subject", subjectID),
			zap.String("git_username", gitUsername))
	}
	return nil
}
