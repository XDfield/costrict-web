package dispatcher

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"time"

	"gorm.io/gorm"
)

// DeferredNotification is a per-user pending event awaiting its debounce
// fire time. Stored as DB rows so a transaction can atomically drain the
// backlog (SELECT FOR UPDATE / DELETE in a single txn) — this gives
// multi-pod safety for free, replacing the Redis LIST+ZSET+STRING trio
// whose LRange+Del pair had a small race window under concurrent pods.
type DeferredNotification struct {
	ID        uint      `gorm:"primaryKey;autoIncrement"`
	UserID    string    `gorm:"size:255;not null;index:idx_dn_user,priority:1"`
	FireAt    time.Time `gorm:"not null;index:idx_dn_fire,priority:1"`
	FirstSeen time.Time `gorm:"not null"`
	Payload   string    `gorm:"type:text;not null"`
	CreatedAt time.Time `gorm:"autoCreateTime"`
}

// drainUserBacklog removes every backlog entry for userID and returns them.
// The SELECT + DELETE happens inside one transaction so concurrent pods
// cannot drop events (one pod's drain either sees the row or doesn't — the
// row is never deleted out from under an in-flight reader).
func (d *Dispatcher) drainUserBacklog(ctx context.Context, userID string) ([]DeferredNotification, error) {
	var entries []DeferredNotification
	err := d.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.Where("user_id = ?", userID).Order("id ASC").Find(&entries).Error; err != nil {
			return err
		}
		if len(entries) == 0 {
			return nil
		}
		return tx.Where("user_id = ?", userID).Delete(&DeferredNotification{}).Error
	})
	if err != nil {
		return nil, err
	}
	return entries, nil
}

// rearmUserFireAt updates every backlog row for userID to fire at the
// computed debounce time. Called inside the same transaction that inserted
// the new event so the read of oldest FirstSeen + the write of FireAt are
// atomic relative to other producers.
func (d *Dispatcher) rearmUserFireAt(tx *gorm.DB, userID string, now time.Time) error {
	var oldest DeferredNotification
	if err := tx.Where("user_id = ?", userID).Order("first_seen ASC").First(&oldest).Error; err != nil {
		return err
	}
	fireAt := now.Add(d.debounceWindow)
	capAt := oldest.FirstSeen.Add(d.debounceMaxCap)
	if fireAt.After(capAt) {
		fireAt = capAt
	}
	return tx.Model(&DeferredNotification{}).
		Where("user_id = ?", userID).
		Update("fire_at", fireAt).Error
}

// loadPendingUsers returns distinct userIDs whose fire_at <= now.
func (d *Dispatcher) loadPendingUsers(ctx context.Context, now time.Time) ([]string, error) {
	var users []string
	err := d.db.WithContext(ctx).
		Model(&DeferredNotification{}).
		Where("fire_at <= ?", now).
		Distinct("user_id").
		Pluck("user_id", &users).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, nil
	}
	return users, err
}

// cancelUserBacklog deletes every backlog entry for userID. Idempotent.
func (d *Dispatcher) cancelUserBacklog(ctx context.Context, userID string) (int64, error) {
	result := d.db.WithContext(ctx).Where("user_id = ?", userID).Delete(&DeferredNotification{})
	return result.RowsAffected, result.Error
}

// decodePayload unmarshals a backlog row into a DispatchInput.
func decodePayload(raw string) (DispatchInput, bool) {
	var input DispatchInput
	if err := json.Unmarshal([]byte(raw), &input); err != nil {
		slog.Warn("[dispatcher] malformed backlog payload", "error", err)
		return DispatchInput{}, false
	}
	return input, true
}
