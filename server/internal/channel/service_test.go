package channel

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/costrict/costrict-web/server/internal/models"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// captureHandler captures the sender's ReplyContext for assertions.
type captureHandler struct {
	lastRC *ReplyContext
}

func (h *captureHandler) Handle(_ context.Context, _ *InboundMessage, sender Sender) error {
	rc := sender.ReplyContext()
	h.lastRC = &rc
	return nil
}

// mockAdapter is a minimal ChannelAdapter for testing HandleWebhook.
type mockAdapter struct {
	channelType string
}

func (a *mockAdapter) Type() string { return a.channelType }
func (a *mockAdapter) Capabilities() ChannelCapabilities {
	return ChannelCapabilities{InboundMessages: true, OutboundMessages: true}
}
func (a *mockAdapter) ValidateConfig(_ json.RawMessage) error { return nil }
func (a *mockAdapter) ConfigSchema() []ConfigField             { return nil }
func (a *mockAdapter) ParseInbound(r *http.Request, _ json.RawMessage) (*InboundMessage, error) {
	var msg InboundMessage
	if err := json.NewDecoder(r.Body).Decode(&msg); err != nil {
		return nil, err
	}
	return &msg, nil
}
func (a *mockAdapter) HandleVerification(_ *http.Request, _ json.RawMessage) (string, bool, error) {
	return "", false, nil
}
func (a *mockAdapter) Reply(_ context.Context, _ json.RawMessage, _ ReplyTarget, _ OutboundMessage) error {
	return nil
}

func setupServiceTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{
		Logger: logger.Default.LogMode(logger.Silent),
	})
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	// Raw DDL — avoid AutoMigrate since models use postgres-specific GORM tags
	statements := []string{
		`CREATE TABLE channel_configs (
			id TEXT PRIMARY KEY,
			user_id TEXT NOT NULL DEFAULT '',
			channel_type TEXT NOT NULL,
			name TEXT NOT NULL DEFAULT '',
			config TEXT DEFAULT '{}',
			enabled INTEGER NOT NULL DEFAULT 1,
			webhook_verified INTEGER NOT NULL DEFAULT 0,
			last_active_at DATETIME,
			last_error TEXT DEFAULT '',
			deleted_at DATETIME,
			created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
			updated_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
		)`,
		`CREATE TABLE user_auth_identities (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			user_subject_id TEXT NOT NULL,
			provider TEXT NOT NULL,
			issuer TEXT,
			external_key TEXT NOT NULL UNIQUE,
			external_subject TEXT,
			external_user_id TEXT,
			provider_user_id TEXT,
			display_name TEXT,
			email TEXT,
			phone TEXT,
			avatar_url TEXT,
			organization TEXT,
			is_primary INTEGER DEFAULT 0,
			explicitly_unbound INTEGER DEFAULT 0,
			last_login_at DATETIME,
			created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
			updated_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
			deleted_at DATETIME
		)`,
	}
	for _, stmt := range statements {
		if err := db.Exec(stmt).Error; err != nil {
			t.Fatalf("create table: %v", err)
		}
	}
	return db
}

func insertIdentity(t *testing.T, db *gorm.DB, userSubjectID, providerUserID string) {
	t.Helper()
	pid := providerUserID
	identity := models.UserAuthIdentity{
		UserSubjectID:  userSubjectID,
		Provider:       "idtrust",
		ExternalKey:    fmt.Sprintf("%s:%s", "idtrust", providerUserID),
		ProviderUserID: &pid,
	}
	if err := db.Create(&identity).Error; err != nil {
		t.Fatalf("insert identity: %v", err)
	}
}

func TestHandleWebhook_WecomBot_ResolvesPlatformUserID(t *testing.T) {
	db := setupServiceTestDB(t)

	// Insert idtrust identity: WeCom externalUserID "WecomUser-A" → platformUserID "platform-user-A"
	insertIdentity(t, db, "platform-user-A", "WecomUser-A")

	// Create system-level wecom-bot config with empty UserID (as production does)
	sysCfg := models.ChannelConfig{
		ID:          "cfg-wecom-bot-1",
		ChannelType: "wecom-bot",
		Name:        "wecom-bot 系统",
		Enabled:     true,
	}
	if err := db.Create(&sysCfg).Error; err != nil {
		t.Fatalf("create config: %v", err)
	}

	// Register mock adapter
	adapter := &mockAdapter{channelType: "wecom-bot"}

	// Build a ChannelService with the test db and a capture handler
	handler := &captureHandler{}
	svc := &ChannelService{
		db:             db,
		adapters:       map[string]ChannelAdapter{"wecom-bot": adapter},
		messageHandler: handler,
		sessionStore:   NewReplyContextStore(),
		weComBotEnabled: true,
	}

	// Simulate inbound message from WecomUser-A
	body, _ := json.Marshal(InboundMessage{
		ExternalChatID:   "WecomUser-A",
		ExternalChatType: "single",
		ExternalUserID:   "WecomUser-A",
		Content:          "hello",
		ContentType:      "text",
	})
	req := httptest.NewRequest(http.MethodPost, "/api/webhooks/channels/wecom-bot", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")

	_, _, err := svc.HandleWebhook("wecom-bot", req)
	if err != nil {
		t.Fatalf("HandleWebhook error: %v", err)
	}

	if handler.lastRC == nil {
		t.Fatal("message handler was not invoked")
	}

	// The key assertion: ReplyContext.UserID should be the resolved platformUserID,
	// not the WeCom externalUserID.
	if handler.lastRC.UserID != "platform-user-A" {
		t.Errorf("ReplyContext.UserID = %q, want %q", handler.lastRC.UserID, "platform-user-A")
	}
	if handler.lastRC.Target.ExternalUserID != "WecomUser-A" {
		t.Errorf("Target.ExternalUserID = %q, want %q", handler.lastRC.Target.ExternalUserID, "WecomUser-A")
	}
}

func TestHandleWebhook_WecomBot_DifferentUsersIsolated(t *testing.T) {
	db := setupServiceTestDB(t)

	insertIdentity(t, db, "platform-user-A", "WecomUser-A")
	insertIdentity(t, db, "platform-user-B", "WecomUser-B")

	sysCfg := models.ChannelConfig{
		ID:          "cfg-wecom-bot-2",
		ChannelType: "wecom-bot",
		Name:        "wecom-bot 系统",
		Enabled:     true,
	}
	db.Create(&sysCfg)

	adapter := &mockAdapter{channelType: "wecom-bot"}

	var capturedRCs []ReplyContext
	handler := &captureListHandler{onHandle: func(rc ReplyContext) { capturedRCs = append(capturedRCs, rc) }}

	svc := &ChannelService{
		db:              db,
		adapters:        map[string]ChannelAdapter{"wecom-bot": adapter},
		messageHandler:  handler,
		sessionStore:    NewReplyContextStore(),
		weComBotEnabled: true,
	}

	// Message from user A
	bodyA, _ := json.Marshal(InboundMessage{
		ExternalChatID:   "WecomUser-A",
		ExternalChatType: "single",
		ExternalUserID:   "WecomUser-A",
		Content:          "from A",
		ContentType:      "text",
	})
	reqA := httptest.NewRequest(http.MethodPost, "/api/webhooks/channels/wecom-bot", bytes.NewReader(bodyA))
	reqA.Header.Set("Content-Type", "application/json")
	svc.HandleWebhook("wecom-bot", reqA)

	// Message from user B
	bodyB, _ := json.Marshal(InboundMessage{
		ExternalChatID:   "WecomUser-B",
		ExternalChatType: "single",
		ExternalUserID:   "WecomUser-B",
		Content:          "from B",
		ContentType:      "text",
	})
	reqB := httptest.NewRequest(http.MethodPost, "/api/webhooks/channels/wecom-bot", bytes.NewReader(bodyB))
	reqB.Header.Set("Content-Type", "application/json")
	svc.HandleWebhook("wecom-bot", reqB)

	if len(capturedRCs) != 2 {
		t.Fatalf("expected 2 captured contexts, got %d", len(capturedRCs))
	}

	// Verify user A and B are resolved to different platform user IDs
	if capturedRCs[0].UserID != "platform-user-A" {
		t.Errorf("first message UserID = %q, want %q", capturedRCs[0].UserID, "platform-user-A")
	}
	if capturedRCs[1].UserID != "platform-user-B" {
		t.Errorf("second message UserID = %q, want %q", capturedRCs[1].UserID, "platform-user-B")
	}

	// Verify sessionStore can look them up by platformUserID
	store := svc.sessionStore
	if ctxs := store.LookupByUser("platform-user-A"); len(ctxs) != 1 {
		t.Errorf("expected 1 context for platform-user-A, got %d", len(ctxs))
	}
	if ctxs := store.LookupByUser("platform-user-B"); len(ctxs) != 1 {
		t.Errorf("expected 1 context for platform-user-B, got %d", len(ctxs))
	}
}

func TestHandleWebhook_WecomBot_NoIdentity_SkipsHandler(t *testing.T) {
	db := setupServiceTestDB(t)

	// No identity inserted — resolution should fail gracefully.
	sysCfg := models.ChannelConfig{
		ID:          "cfg-wecom-bot-3",
		ChannelType: "wecom-bot",
		Name:        "wecom-bot 系统",
		Enabled:     true,
	}
	db.Create(&sysCfg)

	adapter := &mockAdapter{channelType: "wecom-bot"}
	handler := &captureHandler{}
	svc := &ChannelService{
		db:              db,
		adapters:        map[string]ChannelAdapter{"wecom-bot": adapter},
		messageHandler:  handler,
		sessionStore:    NewReplyContextStore(),
		weComBotEnabled: true,
	}

	body, _ := json.Marshal(InboundMessage{
		ExternalChatID:   "UnknownUser",
		ExternalChatType: "single",
		ExternalUserID:   "UnknownUser",
		Content:          "hello",
		ContentType:      "text",
	})
	req := httptest.NewRequest(http.MethodPost, "/api/webhooks/channels/wecom-bot", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")

	respBody, _, err := svc.HandleWebhook("wecom-bot", req)
	if err != nil {
		t.Fatalf("HandleWebhook should not return error: %v", err)
	}

	// Handler MUST NOT be invoked when identity resolution failed — otherwise
	// an unresolved identity would create a spurious agent session and produce
	// a confusing second reply alongside the error notice.
	if handler.lastRC != nil {
		t.Fatalf("message handler should not be invoked on unresolved identity; got UserID=%q", handler.lastRC.UserID)
	}

	// Response body should carry resolveErr so the proxy can surface it.
	if !strings.Contains(respBody, "未找到企微账号绑定信息") {
		t.Errorf("response body should carry resolveErr; got %q", respBody)
	}
}

func TestHandleWebhook_ConfigWithUserID_NotOverridden(t *testing.T) {
	db := setupServiceTestDB(t)

	// Non-wecom-bot channel: config has a real UserID that should NOT be overridden.
	// The sender's externalUserID must resolve via idtrust to that same UserID,
	// otherwise the targeted lookup misses and the handler is never invoked.
	insertIdentity(t, db, "original-user", "WecomUser-X")
	sysCfg := models.ChannelConfig{
		ID:          "cfg-wecom-1",
		ChannelType: "wecom",
		Name:        "WeCom",
		Enabled:     true,
		UserID:      "original-user",
	}
	db.Create(&sysCfg)

	adapter := &mockAdapter{channelType: "wecom"}
	handler := &captureHandler{}
	svc := &ChannelService{
		db:             db,
		adapters:       map[string]ChannelAdapter{"wecom": adapter},
		messageHandler: handler,
		sessionStore:   NewReplyContextStore(),
	}

	body, _ := json.Marshal(InboundMessage{
		ExternalChatID:   "some-chat",
		ExternalChatType: "single",
		ExternalUserID:   "WecomUser-X",
		Content:          "test",
		ContentType:      "text",
	})
	req := httptest.NewRequest(http.MethodPost, "/api/webhooks/channels/wecom", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")

	svc.HandleWebhook("wecom", req)

	if handler.lastRC == nil {
		t.Fatal("message handler was not invoked")
	}
	if handler.lastRC.UserID != "original-user" {
		t.Errorf("ReplyContext.UserID = %q, want %q (should not be overridden for non-wecom-bot)",
			handler.lastRC.UserID, "original-user")
	}
}

// captureListHandler captures all ReplyContexts and optionally calls a callback.
type captureListHandler struct {
	captured  []ReplyContext
	onHandle  func(rc ReplyContext)
}

func (h *captureListHandler) Handle(_ context.Context, msg *InboundMessage, sender Sender) error {
	rc := sender.ReplyContext()
	h.captured = append(h.captured, rc)
	if h.onHandle != nil {
		h.onHandle(rc)
	}
	return nil
}

// Ensure captureListHandler implements MessageHandler
var _ MessageHandler = (*captureListHandler)(nil)
