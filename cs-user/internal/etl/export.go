package etl

import (
	"context"
	"errors"
	"fmt"

	"github.com/costrict/costrict-web/cs-user/internal/models"
	"gorm.io/gorm"
)

// ErrInvalidBatchSize is returned by Export* when batchSize <= 0.
var ErrInvalidBatchSize = errors.New("etl: batch size must be > 0")

// ExportUsers streams all user rows (including soft-deleted) from db in
// id-ordered batches, invoking fn once per batch. Pagination is keyset on
// the auto-increment id column — stable under concurrent writes and cheaper
// than OFFSET for large tables. The callback runs inside the read cursor;
// long-running work should buffer outside.
//
// fn may return ErrAbort to stop iteration early without propagating an error.
func ExportUsers(ctx context.Context, db *gorm.DB, batchSize int, fn func([]*models.User) error) error {
	if batchSize <= 0 {
		return ErrInvalidBatchSize
	}
	if db == nil {
		return ErrNilDB
	}
	var lastID uint = 0
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		var batch []*models.User
		if err := db.WithContext(ctx).Unscoped().
			Where("id > ?", lastID).
			Order("id ASC").
			Limit(batchSize).
			Find(&batch).Error; err != nil {
			return fmt.Errorf("etl.ExportUsers: read batch starting at id=%d: %w", lastID, err)
		}
		if len(batch) == 0 {
			return nil
		}
		if err := fn(batch); err != nil {
			if errors.Is(err, ErrAbort) {
				return nil
			}
			return err
		}
		lastID = batch[len(batch)-1].ID
		if len(batch) < batchSize {
			return nil
		}
	}
}

// ExportAuthIdentities mirrors ExportUsers for user_auth_identities, keyset
// paginated on id. The callback signature is the same shape.
func ExportAuthIdentities(ctx context.Context, db *gorm.DB, batchSize int, fn func([]*models.UserAuthIdentity) error) error {
	if batchSize <= 0 {
		return ErrInvalidBatchSize
	}
	if db == nil {
		return ErrNilDB
	}
	var lastID uint = 0
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		var batch []*models.UserAuthIdentity
		if err := db.WithContext(ctx).Unscoped().
			Where("id > ?", lastID).
			Order("id ASC").
			Limit(batchSize).
			Find(&batch).Error; err != nil {
			return fmt.Errorf("etl.ExportAuthIdentities: read batch starting at id=%d: %w", lastID, err)
		}
		if len(batch) == 0 {
			return nil
		}
		if err := fn(batch); err != nil {
			if errors.Is(err, ErrAbort) {
				return nil
			}
			return err
		}
		lastID = batch[len(batch)-1].ID
		if len(batch) < batchSize {
			return nil
		}
	}
}

// CountUsers returns the total row count (including soft-deleted) for the
// users table. Used by the report to assert source/target parity.
func CountUsers(ctx context.Context, db *gorm.DB) (int64, error) {
	if db == nil {
		return 0, ErrNilDB
	}
	var n int64
	if err := db.WithContext(ctx).Unscoped().Model(&models.User{}).Count(&n).Error; err != nil {
		return 0, fmt.Errorf("etl.CountUsers: %w", err)
	}
	return n, nil
}

// CountAuthIdentities mirrors CountUsers for user_auth_identities.
func CountAuthIdentities(ctx context.Context, db *gorm.DB) (int64, error) {
	if db == nil {
		return 0, ErrNilDB
	}
	var n int64
	if err := db.WithContext(ctx).Unscoped().Model(&models.UserAuthIdentity{}).Count(&n).Error; err != nil {
		return 0, fmt.Errorf("etl.CountAuthIdentities: %w", err)
	}
	return n, nil
}
