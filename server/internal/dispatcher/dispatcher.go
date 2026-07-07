package dispatcher

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/costrict/costrict-web/server/internal/gateway"
	"github.com/costrict/costrict-web/server/internal/models"
	"github.com/costrict/costrict-web/server/internal/notification"
	"gorm.io/gorm"
)

type DispatchInput struct {
	UserID      string
	WorkspaceID string
	EventType   string
	SessionID   string
	DeviceID    string
	Path        string
	SessionURL  string
	ActionData  map[string]any
}

// AIEventHandler is invoked by the polling goroutine after the debounce
// window elapses AND the backlog contains at least one still-pending event.
// Receives a batch — the typical case is one event, but back-to-back events
// within the debounce window arrive as a single call so the AI can produce
// one consolidated notification. Return true when the AI notification was
// successfully delivered; false falls back to the standard notification
// service for each event in the batch.
type AIEventHandler func(ctx context.Context, inputs []DispatchInput) bool

// Debounce defaults. The window is reset on every new event for a user (so
// back-to-back events coalesce into one notification); maxCap bounds the
// worst-case delay so a sustained event stream can't defer forever.
const (
	defaultDebounceWindow = 30 * time.Second
	defaultDebounceMaxCap = 60 * time.Second
	defaultPollInterval   = 1 * time.Second
)

type Dispatcher struct {
	db              *gorm.DB
	store           *notification.Store
	notificationSvc *notification.NotificationService
	gwClient        *gateway.Client
	gwRegistry      *gateway.GatewayRegistry
	appURL          string
	aiEventHandler  AIEventHandler

	// Per-event-type managers. Before firing, the dispatcher polls each
	// backlog entry: events reported as no longer pending are suppressed.
	eventManagers map[string]EventManager

	// Debounce tuning (override via NewDispatcherWithDebounce in tests).
	debounceWindow time.Duration
	debounceMaxCap time.Duration
	pollInterval   time.Duration

	stopCh chan struct{}
}

// NewDispatcher constructs a DB-backed Dispatcher. Deferred notification
// state lives in the deferred_notifications table — polling + drain happen
// inside DB transactions, giving multi-pod safety without sticky routing.
func NewDispatcher(db *gorm.DB, notificationSvc *notification.NotificationService, store *notification.Store, appURL string, gwClient *gateway.Client, gwRegistry *gateway.GatewayRegistry) *Dispatcher {
	return NewDispatcherWithDebounce(db, notificationSvc, store, appURL, gwClient, gwRegistry, defaultDebounceWindow, defaultDebounceMaxCap)
}

// NewDispatcherWithDebounce exposes debounce tuning for tests; production
// code should call NewDispatcher.
func NewDispatcherWithDebounce(db *gorm.DB, notificationSvc *notification.NotificationService, store *notification.Store, appURL string, gwClient *gateway.Client, gwRegistry *gateway.GatewayRegistry, window, maxCap time.Duration) *Dispatcher {
	return NewDispatcherWithPolling(db, notificationSvc, store, appURL, gwClient, gwRegistry, window, maxCap, defaultPollInterval)
}

// NewDispatcherWithPolling exposes all timing knobs for tests that need
// sub-second polling.
func NewDispatcherWithPolling(db *gorm.DB, notificationSvc *notification.NotificationService, store *notification.Store, appURL string, gwClient *gateway.Client, gwRegistry *gateway.GatewayRegistry, window, maxCap, poll time.Duration) *Dispatcher {
	if window <= 0 {
		window = defaultDebounceWindow
	}
	if maxCap <= 0 {
		maxCap = defaultDebounceMaxCap
	}
	if maxCap < window {
		maxCap = window
	}
	if poll <= 0 {
		poll = defaultPollInterval
	}
	return &Dispatcher{
		db:              db,
		store:           store,
		notificationSvc: notificationSvc,
		gwClient:        gwClient,
		gwRegistry:      gwRegistry,
		appURL:          appURL,
		eventManagers:   make(map[string]EventManager),
		debounceWindow:  window,
		debounceMaxCap:  maxCap,
		pollInterval:    poll,
		stopCh:          make(chan struct{}),
	}
}

// SetAIEventHandler registers the AI-driven event handler invoked when the
// debounce timer fires and at least one backlog event is still pending.
func (d *Dispatcher) SetAIEventHandler(h AIEventHandler) {
	d.aiEventHandler = h
}

func (d *Dispatcher) SetEventManager(eventType string, mgr EventManager) {
	d.eventManagers[eventType] = mgr
}

// AutoMigrate ensures the deferred_notifications table exists. Call once at
// startup before Start.
func (d *Dispatcher) AutoMigrate() error {
	return d.db.AutoMigrate(&DeferredNotification{})
}

// Start launches the polling goroutine that watches for fire times in the
// deferred_notifications table. Returns immediately; goroutine respects
// Close().
func (d *Dispatcher) Start(ctx context.Context) error {
	if err := d.AutoMigrate(); err != nil {
		return fmt.Errorf("dispatcher automigrate: %w", err)
	}
	go d.pollDeferredTimers()
	return nil
}

// Close stops background goroutines. Idempotent.
func (d *Dispatcher) Close() {
	select {
	case <-d.stopCh:
	default:
		close(d.stopCh)
	}
}

// CancelDeferredNotification drains per-user pending state so the timer will
// not fire for the in-flight backlog. Called by the clawagent runtime when
// the user replies before the debounce window elapses. Keyed by userID
// because the dispatcher debounces per user, not per device session.
func (d *Dispatcher) CancelDeferredNotification(userID string) {
	if userID == "" {
		return
	}
	ctx := context.Background()
	rows, err := d.cancelUserBacklog(ctx, userID)
	if err != nil {
		slog.Warn("[dispatcher] CancelDeferredNotification: delete failed", "userID", userID, "error", err)
		return
	}
	if rows > 0 {
		slog.Info("[dispatcher] cancelled deferred notification", "userID", userID, "rows", rows)
	}
}

// startDeferredNotification records the event in the per-user backlog and
// (re)arms the debounce timer. The timer fires after debounceWindow of
// silence OR debounceMaxCap from the first event, whichever comes first.
// Multiple events within the window coalesce into a single fire.
//
// The insert + FireAt recompute happen in one transaction so the FirstSeen
// anchor read is atomic relative to concurrent producers — no risk of two
// pods reading different anchors and computing different FireAt values.
func (d *Dispatcher) startDeferredNotification(input DispatchInput) {
	ctx := context.Background()
	payload, err := json.Marshal(input)
	if err != nil {
		slog.Error("[dispatcher] startDeferredNotification: marshal failed", "error", err)
		return
	}
	userID := input.UserID
	now := time.Now()

	entry := &DeferredNotification{
		UserID:    userID,
		FireAt:    now.Add(d.debounceWindow), // tentative; rearmUserFireAt overrides
		FirstSeen: now,
		Payload:   string(payload),
	}

	err = d.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.Create(entry).Error; err != nil {
			return err
		}
		return d.rearmUserFireAt(tx, userID, now)
	})
	if err != nil {
		slog.Error("[dispatcher] startDeferredNotification: txn failed", "userID", userID, "error", err)
		return
	}

	slog.Info("[dispatcher] armed deferred notification",
		"userID", userID, "eventType", input.EventType, "deviceSessionID", input.SessionID,
		"window", d.debounceWindow)
}

// pollDeferredTimers wakes every pollInterval and queries for users whose
// fire_at has passed. For each: invokes fireDeferredNotification, which
// atomically drains the backlog inside a transaction.
func (d *Dispatcher) pollDeferredTimers() {
	ctx := context.Background()
	ticker := time.NewTicker(d.pollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-d.stopCh:
			return
		case <-ticker.C:
		}
		now := time.Now()
		users, err := d.loadPendingUsers(ctx, now)
		if err != nil {
			slog.Warn("[dispatcher] poll: query failed", "error", err)
			continue
		}
		for _, userID := range users {
			d.fireDeferredNotification(userID)
		}
	}
}

// fireDeferredNotification is invoked when the per-user fire_at has passed.
// Drains the backlog inside a transaction (atomic SELECT + DELETE — no race
// even with concurrent pods), filters out events that were resolved through
// other channels during the window, then invokes the AI handler for the
// survivors (or the notification service as fallback).
func (d *Dispatcher) fireDeferredNotification(userID string) {
	ctx := context.Background()

	entries, err := d.drainUserBacklog(ctx, userID)
	if err != nil {
		slog.Error("[dispatcher] fire: drain failed", "userID", userID, "error", err)
		return
	}
	if len(entries) == 0 {
		return
	}

	var pending []DispatchInput
	for _, e := range entries {
		input, ok := decodePayload(e.Payload)
		if !ok {
			continue
		}
		if mgr, ok := d.eventManagers[input.EventType]; ok {
			still, perr := mgr.IsStillPending(ctx, input)
			if perr != nil {
				slog.Warn("[dispatcher] fire: IsStillPending errored, treating as pending",
					"eventType", input.EventType, "deviceSessionID", input.SessionID, "error", perr)
				pending = append(pending, input)
			} else if !still {
				slog.Info("[dispatcher] fire: dropping resolved event",
					"eventType", input.EventType, "deviceSessionID", input.SessionID)
			} else {
				pending = append(pending, input)
			}
		} else {
			// No manager → conservatively fire.
			pending = append(pending, input)
		}
	}

	if len(pending) == 0 {
		slog.Info("[dispatcher] fire: all backlog events resolved, nothing to send", "userID", userID)
		return
	}

	slog.Info("[dispatcher] fire: invoking AI handler", "userID", userID, "count", len(pending))
	d.invokeAIHandler(pending)
}

// invokeAIHandler runs the registered AI event handler with the full batch
// of surviving events. If no handler is registered or it declines, falls
// back to the standard notification service per event so the user still
// gets some signal.
func (d *Dispatcher) invokeAIHandler(inputs []DispatchInput) {
	if len(inputs) == 0 {
		return
	}
	if d.aiEventHandler != nil {
		handled := d.aiEventHandler(context.Background(), inputs)
		if handled {
			return
		}
	}
	for _, input := range inputs {
		slog.Info("[dispatcher] AI handler declined, falling back to notification service",
			"eventType", input.EventType, "sessionID", input.SessionID)
		d.dispatchNotification(input)
	}
}

// --- WeCom UserID Resolution ---

func (d *Dispatcher) resolveWeComUserID(appUserID string) string {
	var identity models.UserAuthIdentity
	if err := d.db.Where("user_subject_id = ? AND provider = ? AND deleted_at IS NULL", appUserID, "idtrust").
		First(&identity).Error; err != nil {
		slog.Error("[dispatcher] failed to resolve wecom user id from idtrust", "appUserID", appUserID, "error", err)
		return ""
	}
	if identity.ProviderUserID != nil {
		return *identity.ProviderUserID
	}
	return ""
}

// --- Public Interface ---

func (d *Dispatcher) Dispatch(input DispatchInput) {
	slog.Info("[dispatcher] Dispatch received",
		"eventType", input.EventType, "sessionID", input.SessionID,
		"hasStore", d.store != nil, "hasAIHandler", d.aiEventHandler != nil,
		"hasManager", d.eventManagers[input.EventType] != nil)
	if d.store == nil {
		return
	}

	if (input.EventType == "permission" || input.EventType == "permission_batch") && d.isAutoAccept(input) {
		slog.Info("[dispatcher] auto-accept enabled, auto-approving permission(s)",
			"eventType", input.EventType, "sessionID", input.SessionID)
		if input.EventType == "permission" {
			d.autoApprovePermission(input)
		} else {
			d.batchApprovePermissions(input)
		}
		return
	}

	if isDeferrable(input.EventType) {
		d.startDeferredNotification(input)
		return
	}

	d.dispatchNotification(input)
}

// --- Event Classification ---

func isDeferrable(eventType string) bool {
	return eventType == "permission" || eventType == "permission_batch" || eventType == "question"
}

// needsInteraction retained for compatibility — tests reference it. Equivalent
// to "permission or question" (not batch) after the card path removal.
func needsInteraction(eventType string) bool {
	return eventType == "permission" || eventType == "question"
}

// --- Notification Dispatch ---

func (d *Dispatcher) dispatchNotification(input DispatchInput) {
	if d.notificationSvc != nil {
		d.notificationSvc.TriggerNotifications(input.UserID, input.EventType, input.SessionID,
			input.DeviceID, input.Path, input.ActionData)
	}
}

// --- Auto-Accept ---

func (d *Dispatcher) isAutoAccept(input DispatchInput) bool {
	if input.Path == "" || input.DeviceID == "" {
		return false
	}
	normalizedPath := strings.ReplaceAll(input.Path, "\\", "/")
	var dev models.Device
	if err := d.db.Where("device_id = ?", input.DeviceID).First(&dev).Error; err != nil {
		return false
	}
	var ws models.Workspace
	if err := d.db.
		Joins("JOIN workspace_directories ON workspace_directories.workspace_id = workspaces.id").
		Where("workspaces.user_id = ? AND workspaces.device_id = ?", input.UserID, dev.ID).
		Where("REPLACE(workspace_directories.path, chr(92), chr(47)) = ?", normalizedPath).
		Where("workspace_directories.deleted_at IS NULL").
		First(&ws).Error; err != nil {
		return false
	}
	var settings map[string]any
	if ws.Settings != nil {
		json.Unmarshal(ws.Settings, &settings)
	}
	if v, ok := settings["autoAccept"]; ok {
		if b, ok := v.(bool); ok {
			return b
		}
	}
	return false
}

func (d *Dispatcher) autoApprovePermission(input DispatchInput) {
	if d.gwClient == nil || d.gwRegistry == nil {
		return
	}
	if input.ActionData == nil {
		return
	}
	id, _ := input.ActionData["id"].(string)
	if id == "" {
		return
	}
	proxyPath := fmt.Sprintf("/api/v1/permissions/%s/reply", id)
	bodyBytes, _ := json.Marshal(map[string]any{"approved": true})
	var result map[string]any
	directory := input.Path
	if err := gateway.ProxyDeviceSessionRequest(d.gwClient, d.gwRegistry, input.UserID, input.DeviceID, directory, "POST", proxyPath, bodyBytes, &result); err != nil {
		slog.Error("[dispatcher] auto-approve permission failed", "error", err)
		return
	}
	slog.Info("[dispatcher] auto-approved permission", "sessionID", input.SessionID, "permissionID", id)
}

func (d *Dispatcher) batchApprovePermissions(input DispatchInput) {
	if d.gwClient == nil || d.gwRegistry == nil {
		return
	}
	if input.ActionData == nil {
		return
	}
	perms, ok := input.ActionData["permissions"].([]any)
	if !ok {
		return
	}
	approved := 0
	for _, p := range perms {
		m, ok := p.(map[string]any)
		if !ok {
			continue
		}
		id, _ := m["id"].(string)
		if id == "" {
			continue
		}
		proxyPath := fmt.Sprintf("/api/v1/permissions/%s/reply", id)
		bodyBytes, _ := json.Marshal(map[string]any{"approved": true})
		var result map[string]any
		if err := gateway.ProxyDeviceSessionRequest(d.gwClient, d.gwRegistry, input.UserID, input.DeviceID, input.Path, "POST", proxyPath, bodyBytes, &result); err != nil {
			slog.Error("[dispatcher] batch auto-approve permission failed", "permissionID", id, "error", err)
			continue
		}
		approved++
	}
	slog.Info("[dispatcher] batch auto-approved permissions",
		"sessionID", input.SessionID, "total", len(perms), "approved", approved)
}

// --- Helpers ---

func mapEventTypeToTitle(eventType string) string {
	switch eventType {
	case "session.completed":
		return "会话已完成"
	case "session.failed":
		return "会话失败"
	case "session.aborted":
		return "会话已中断"
	case "permission":
		return "权限请求"
	case "question":
		return "问题"
	case "idle":
		return "空闲超时"
	default:
		return eventType
	}
}

func mustMarshal(v any) []byte {
	b, _ := json.Marshal(v)
	return b
}
