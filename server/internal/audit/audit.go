// Package audit records admin write-operation audit logs into admin_audit_logs.
//
// The write path is intentionally fire-and-forget: Record never blocks the
// caller and never propagates errors. Audit logging is a side observation of a
// management action — it must never fail or slow down the primary request. A
// failed insert is logged at warn level and otherwise swallowed.
//
// Usage:
//
//	audit.Init(db)                                   // once, at startup
//	audit.Record(actorID, "enterprise.create", "enterprise_customer", id, req)
//
// If Init has not run (or db is nil), Record is a safe no-op. This keeps tests
// and any non-API entrypoints from having to wire the singleton.
package audit

import (
	"encoding/json"
	"log/slog"

	"github.com/costrict/costrict-web/server/internal/models"
	"gorm.io/datatypes"
	"gorm.io/gorm"
)

// Common action constants for the management write-operations we instrument.
// Values are stable, dot-namespaced strings stored verbatim in the action
// column and used by the frontend filter dropdown.
const (
	ActionEnterpriseCreate = "enterprise.create"
	ActionEnterpriseUpdate = "enterprise.update"
	ActionEnterpriseDelete = "enterprise.delete"

	ActionSystemRoleGrant  = "system_role.grant"
	ActionSystemRoleRevoke = "system_role.revoke"

	ActionUserStatusChange = "user.status_change"

	ActionResourcePermissionUpdate = "resource_permission.update"

	ActionDistributionCreate = "distribution.create"
	ActionDistributionUpdate = "distribution.update"
	ActionDistributionRevoke = "distribution.revoke"

	ActionNotificationChannelCreate = "notification_channel.create"
	ActionNotificationChannelUpdate = "notification_channel.update"
	ActionNotificationChannelDelete = "notification_channel.delete"

	ActionSettingUpdate = "setting.update"

	ActionAnnouncementSend = "announcement.send"
)

// Common target-type constants for the target_type column.
const (
	TargetEnterpriseCustomer  = "enterprise_customer"
	TargetUser                = "user"
	TargetResourcePermission  = "resource_permission"
	TargetDistribution        = "distribution"
	TargetNotificationChannel = "notification_channel"
	TargetSetting             = "setting"
	TargetAnnouncement        = "announcement"
)

// pkgLogger is the process-wide audit logger. nil until Init runs.
var pkgLogger *Logger

// Logger writes audit records to the database. It is safe for concurrent use.
type Logger struct {
	db *gorm.DB
}

// New constructs a Logger bound to db. Most callers should use Init + the
// package-level Record instead; New is exposed for tests and explicit wiring.
func New(db *gorm.DB) *Logger {
	return &Logger{db: db}
}

// Init installs the process-wide audit logger. Call once at startup. Passing a
// nil db disables auditing (Record becomes a no-op).
func Init(db *gorm.DB) {
	if db == nil {
		pkgLogger = nil
		return
	}
	pkgLogger = New(db)
}

// Record writes a single audit entry asynchronously. It never blocks, never
// returns an error, and is a no-op when the package logger is uninitialized.
func Record(actorID, action, targetType, targetID string, payload any) {
	if pkgLogger == nil {
		return
	}
	pkgLogger.Record(actorID, action, targetType, targetID, payload)
}

// Record writes a single audit entry. The insert runs on a background goroutine
// with a panic guard so that neither a DB error nor a marshalling panic can ever
// affect the calling request.
func (l *Logger) Record(actorID, action, targetType, targetID string, payload any) {
	if l == nil || l.db == nil {
		return
	}

	// Marshal on the calling goroutine so we capture the payload value before
	// the caller mutates it; the (cheap, deterministic) work won't block.
	raw := datatypes.JSON("{}")
	if payload != nil {
		if b, err := json.Marshal(payload); err == nil && len(b) > 0 {
			raw = datatypes.JSON(b)
		}
	}

	entry := models.AdminAuditLog{
		ActorID:    actorID,
		Action:     action,
		TargetType: targetType,
		TargetID:   targetID,
		Payload:    raw,
	}

	go func() {
		defer func() {
			if r := recover(); r != nil {
				slog.Warn("audit record panicked", "action", action, "recover", r)
			}
		}()
		if err := l.db.Create(&entry).Error; err != nil {
			slog.Warn("failed to write audit log", "action", action, "actorId", actorID, "error", err)
		}
	}()
}
