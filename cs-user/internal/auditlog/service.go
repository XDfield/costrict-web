// Package auditlog implements the Phase C4.1 audit-log writer.
//
// The Service.Record method writes a single row to user_center_audit_log
// capturing the actor + action + target + payload + network context of one
// admin write operation. Best-effort: returns the write error but callers
// MUST ignore it — the user-visible operation has already committed; audit
// failure must not bubble back into a 500.
//
// The structured logger captures any failure at WARN level with the full
// RecordParams so ops can correlate spikes with DB incidents.
package auditlog

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"time"

	"github.com/costrict/costrict-web/cs-user/internal/models"
	"gorm.io/gorm"
)

// ErrEmptyAction is returned when RecordParams.Action is the empty string.
// Action is the only required field (migration column is NOT NULL); other
// fields are all nullable. Caller-programming error, not user-facing.
var ErrEmptyAction = errors.New("auditlog: action is required")

// ErrNilDB is returned when the Service was constructed without a *gorm.DB.
// Construction-time invariant; surfaces at call time so callers can build a
// nil-safe Deps graph (test stubs / 503 fallbacks).
var ErrNilDB = errors.New("auditlog: nil db")

// Logger is the minimal interface Service needs to emit WARN lines on write
// failure. The stdlib *log.Logger satisfies this; tests inject a capturing
// fake. Avoids pulling a structured logger dep into this package.
type Logger interface {
	Printf(format string, args ...any)
}

// RecordParams is the input shape for Service.Record. Field semantics:
//
//   - TenantID, ActorSubjectID, ActorTenantRole, ActorPlatformScope: empty
//     string → NULL column (platform-level events have no tenant; system
//     cron actions have no actor).
//   - Action: required (NOT NULL column); empty string → ErrEmptyAction.
//   - TargetType, TargetID: empty string → NULL column.
//   - Payload: map[string]any → JSONB marshaled; nil → NULL column.
//   - IP, UserAgent: empty string → NULL column.
type RecordParams struct {
	TenantID           string
	ActorSubjectID     string
	ActorTenantRole    string
	ActorPlatformScope string
	Action             string
	TargetType         string
	TargetID           string
	Payload            map[string]any
	IP                 string
	UserAgent          string
}

// Service writes audit-log rows. Construct once via NewService and inject
// into tenant.Admin / tenantconfig.Service for use after successful commits.
type Service struct {
	db     *gorm.DB
	logger Logger
}

// NewService returns a *Service bound to the given gorm.DB. logger may be nil
// — a default *log.Logger is allocated lazily; pass an explicit one in tests
// to capture WARN output.
func NewService(db *gorm.DB, logger Logger) *Service {
	return &Service{db: db, logger: logger}
}

func (s *Service) logf(format string, args ...any) {
	if s == nil {
		return
	}
	lg := s.logger
	if lg == nil {
		lg = log.Default()
	}
	lg.Printf(format, args...)
}

// Record writes one audit row. Best-effort: returns the write error but
// callers MUST ignore it (see package doc). Empty action short-circuits
// before touching the DB so callers cannot accidentally create malformed
// rows by passing incomplete params.
//
// ctx flows into gorm's WithContext so the write respects the caller's
// deadline / tracing baggage.
func (s *Service) Record(ctx context.Context, p RecordParams) error {
	if s == nil || s.db == nil {
		// Construction-time invariant violated — surface to caller (which
		// is expected to ignore). Log at WARN for ops visibility.
		s.logf("auditlog.Record: nil service/db; action=%q target=%q", p.Action, p.TargetID)
		return ErrNilDB
	}
	if p.Action == "" {
		return ErrEmptyAction
	}

	row := &models.AuditLog{
		Action:    p.Action,
		Payload:   marshalPayload(p.Payload),
		CreatedAt: time.Now(),
	}
	if p.TenantID != "" {
		row.TenantID = &p.TenantID
	}
	if p.ActorSubjectID != "" {
		row.ActorSubjectID = &p.ActorSubjectID
	}
	if p.ActorTenantRole != "" {
		row.ActorTenantRole = &p.ActorTenantRole
	}
	if p.ActorPlatformScope != "" {
		row.ActorPlatformScope = &p.ActorPlatformScope
	}
	if p.TargetType != "" {
		row.TargetType = &p.TargetType
	}
	if p.TargetID != "" {
		row.TargetID = &p.TargetID
	}
	if p.IP != "" {
		row.IP = &p.IP
	}
	if p.UserAgent != "" {
		row.UserAgent = &p.UserAgent
	}

	if err := s.db.WithContext(ctx).Create(row).Error; err != nil {
		s.logf("auditlog.Record: write failed action=%q target=%q tenant=%q actor=%q err=%v",
			p.Action, p.TargetID, p.TenantID, p.ActorSubjectID, err)
		return fmt.Errorf("auditlog: write: %w", err)
	}
	return nil
}

// marshalPayload serializes the params map to JSONB-ready bytes. A nil map
// or marshal error returns nil (NULL column) — the audit row still lands;
// payload is best-effort.
func marshalPayload(p map[string]any) []byte {
	if p == nil {
		return nil
	}
	raw, err := json.Marshal(p)
	if err != nil {
		// Fall back to a sentinel JSON object rather than dropping the
		// whole audit row — ops will see "<auditlog: payload marshal
		// failed>" in the JSONB column and can investigate.
		return []byte(`{"_error":"auditlog: payload marshal failed"}`)
	}
	return raw
}
