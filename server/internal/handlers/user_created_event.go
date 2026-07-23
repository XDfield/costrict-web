// User-created event consumer endpoint (Git Ownership Refactor Phase 2 + 3).
//
// Single endpoint that cs-user's outbox worker POSTs to:
//
//	POST /api/internal/users/created
//	  X-Internal-Token: <token>
//	  X-Event-ID: <uuid>
//	  { "event_id": "...", "event_type": "user.created", "user": {...} }
//
// Phase 2: log-only — returns 2xx so cs-user's outbox marks the row 'delivered'.
//
// Phase 3 (this file): when USER_CREATED_EVENT_PROCESSING_ENABLED is true and
// a UserProvisionService is registered, the handler dispatches into
// gitsync.ProvisionUser. An idempotency table (user_created_event_log) keyed
// by event_id guarantees at-least-once delivery from cs-user's outbox does
// not double-provision. Duplicate event_id replays return 2xx with
// status='duplicate' and do NOT invoke ProvisionUser again.
//
// ProvisionUser is best-effort. The handler always ACKs the event with 2xx
// unless the event payload itself is malformed — a Gitea outage must never
// block the outbox. Transient ProvisionUser failures still mark the event
// 'processed' in the log so we don't keep retrying the same provisioning
// call; the existing binding row's 'pending'/'error' state machine is the
// reconciler's source of truth.
//
// Auth: InternalAuth middleware (X-Internal-Token). Same gate as other
// /api/internal/* routes.

package handlers

import (
	"errors"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/costrict/costrict-web/server/internal/gitsync"
	"github.com/costrict/costrict-web/server/internal/models"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"go.uber.org/zap"
	"gorm.io/gorm"
)

// UserCreatedEventAPI receives user.created events from cs-user's outbox.
type UserCreatedEventAPI struct {
	Log *zap.Logger
	DB  *gorm.DB
}

// userCreatedEventRequest is the JSON body cs-user's outbox sends.
// Fields mirror cs-user/internal/eventbus.EventPayload.
type userCreatedEventRequest struct {
	EventID    string        `json:"event_id" binding:"required"`
	EventType  string        `json:"event_type" binding:"required"`
	SubjectID  string        `json:"subject_id" binding:"required"`
	TenantID   string        `json:"tenant_id"`
	OccurredAt string        `json:"occurred_at"`
	User       eventUserBody `json:"user" binding:"required"`
}

type eventUserBody struct {
	SubjectID   string  `json:"subject_id"`
	TenantID    string  `json:"tenant_id"`
	Username    string  `json:"username,omitempty"`
	DisplayName *string `json:"display_name,omitempty"`
	Email       *string `json:"email,omitempty"`
}

// ReceiveUserCreated godoc
// @Summary  Receive a user.created event from cs-user outbox
// @Tags     user-events
// @Accept   json
// @Produce  json
// @Security InternalToken
// @Param    X-Event-ID  header  string  true  "cs-user event id (UUID)"
// @Param    body        body    handlers.userCreatedEventRequest  true  "event payload"
// @Success  202  {object}  object{event_id=string,status=string}
// @Failure  400  {object}  object{error=string}
// @Router   /api/internal/users/created [post]
func (a *UserCreatedEventAPI) ReceiveUserCreated(c *gin.Context) {
	log := a.logOrDefault()

	var req userCreatedEventRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid user.created payload: " + err.Error()})
		return
	}

	headerID := strings.TrimSpace(c.GetHeader("X-Event-ID"))
	if headerID != "" && headerID != req.EventID {
		c.JSON(http.StatusBadRequest, gin.H{"error": "X-Event-ID header does not match body event_id"})
		return
	}
	if _, err := uuid.Parse(req.EventID); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "event_id is not a valid UUID"})
		return
	}
	if req.EventType != "user.created" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "unexpected event_type: " + req.EventType})
		return
	}

	// Phase 2 path: when the feature flag is off (default), just log + 202.
	// This keeps the endpoint forward-compatible with cs-user deployments
	// that haven't migrated to server-side provisioning yet.
	svc := GetUserProvisionService()
	if svc == nil || !userCreatedEventProcessingEnabled() {
		log.Info("user.created event received (log-only)",
			zap.String("event_id", req.EventID),
			zap.String("subject_id", req.SubjectID),
			zap.String("tenant_id", req.TenantID),
			zap.String("username", req.User.Username),
		)
		c.JSON(http.StatusAccepted, gin.H{
			"event_id": req.EventID,
			"status":   "accepted_log_only",
		})
		return
	}

	a.dispatchUserCreated(c, svc, req)
}

// dispatchUserCreated is the Phase 3 dispatch path. It performs:
//  1. Idempotency check: if event_id is already in user_created_event_log,
//     return 202 'duplicate' without invoking ProvisionUser.
//  2. Otherwise call svc.ProvisionUser, log the result, then return 202.
//
// ProvisionUser errors are swallowed (event still ACKed) — the binding row
// itself records the failure via its 'error' state. cs-user's outbox will
// keep the row 'delivered'; reconciler repairs later.
//
// Only handler-level infra failures (db unreachable) surface as 5xx so
// cs-user retries. Those rows are NOT inserted into the log table.
func (a *UserCreatedEventAPI) dispatchUserCreated(c *gin.Context, svc *gitsync.UserProvisionService, req userCreatedEventRequest) {
	log := a.logOrDefault()
	ctx := c.Request.Context()

	if a.DB == nil {
		// Misconfiguration: feature enabled without DB wiring. Treat as
		// transient infra failure so cs-user retries with backoff.
		log.Error("user.created dispatch: DB not wired on UserCreatedEventAPI",
			zap.String("event_id", req.EventID))
		c.JSON(http.StatusInternalServerError, gin.H{"error": "event log DB not configured"})
		return
	}

	// Step 1: idempotency check.
	var existing models.UserCreatedEventLog
	err := a.DB.WithContext(ctx).
		Where("event_id = ?", req.EventID).
		First(&existing).Error
	if err == nil {
		log.Info("user.created duplicate event_id (already processed) — skipping",
			zap.String("event_id", req.EventID),
			zap.String("prior_status", existing.Status),
		)
		c.JSON(http.StatusAccepted, gin.H{
			"event_id": req.EventID,
			"status":   "duplicate",
		})
		return
	}
	if !errors.Is(err, gorm.ErrRecordNotFound) {
		log.Error("user.created idempotency lookup failed",
			zap.String("event_id", req.EventID), zap.Error(err))
		c.JSON(http.StatusInternalServerError, gin.H{"error": "idempotency check failed"})
		return
	}

	// Step 2: invoke ProvisionUser. Tenant defaults to 'default' to mirror
	// gitsync.ProvisionUser's own fallback.
	tenantID := req.TenantID
	if tenantID == "" {
		tenantID = req.User.TenantID
	}
	if tenantID == "" {
		tenantID = "default"
	}

	provErr := svc.ProvisionUser(ctx, gitsync.UserProvisionParams{
		SubjectID: req.SubjectID,
		TenantID:  tenantID,
		Username:  req.User.Username,
		Email:     req.User.Email,
	})

	// Step 3: record in event log so duplicates short-circuit future calls.
	// ProvisionUser's own soft-skip (tenant has no git_server) still counts
	// as 'processed' — we don't want cs-user to re-deliver forever.
	status := models.UserCreatedEventStatusProcessed
	var errMsg *string
	if provErr != nil {
		// ProvisionUser best-effort: we still mark processed so the outbox
		// stops retrying. The binding row carries the failure state.
		msg := provErr.Error()
		errMsg = &msg
		log.Warn("user.created ProvisionUser returned error (event still ACKed)",
			zap.String("event_id", req.EventID),
			zap.String("subject_id", req.SubjectID),
			zap.Error(provErr),
		)
	} else {
		log.Info("user.created dispatched to ProvisionUser",
			zap.String("event_id", req.EventID),
			zap.String("subject_id", req.SubjectID),
			zap.String("tenant_id", tenantID),
		)
	}

	row := &models.UserCreatedEventLog{
		EventID:      req.EventID,
		EventType:    req.EventType,
		SubjectID:    req.SubjectID,
		TenantID:     tenantID,
		Status:       status,
		ErrorMessage: errMsg,
		ProcessedAt:  time.Now(),
	}
	if insertErr := a.DB.WithContext(ctx).Create(row).Error; insertErr != nil {
		// Race with another delivery; treat as success since ProvisionUser
		// is idempotent and the binding row is authoritative.
		log.Warn("user.created event_log insert failed (race? row may already exist)",
			zap.String("event_id", req.EventID), zap.Error(insertErr))
	}

	c.JSON(http.StatusAccepted, gin.H{
		"event_id": req.EventID,
		"status":   status,
	})
}

// userCreatedEventProcessingEnabled reads USER_CREATED_EVENT_PROCESSING_ENABLED.
// Default false (Phase 2 log-only). Phase 3 operators set this to true once
// the idempotency table is in place and the cutover is ready.
func userCreatedEventProcessingEnabled() bool {
	v := strings.ToLower(strings.TrimSpace(os.Getenv("USER_CREATED_EVENT_PROCESSING_ENABLED")))
	return v == "1" || v == "true" || v == "yes" || v == "on"
}

// logOrDefault returns a.Log, or a fresh nop logger if nil.
func (a *UserCreatedEventAPI) logOrDefault() *zap.Logger {
	if a != nil && a.Log != nil {
		return a.Log
	}
	return zap.NewNop()
}

// NewUserCreatedEventAPI constructs a UserCreatedEventAPI. log may be nil.
// db may be nil in Phase 2 deployments (log-only path never touches DB).
func NewUserCreatedEventAPI(log *zap.Logger, db *gorm.DB) *UserCreatedEventAPI {
	return &UserCreatedEventAPI{Log: log, DB: db}
}
