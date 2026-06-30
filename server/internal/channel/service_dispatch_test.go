package channel

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/costrict/costrict-web/server/internal/models"
)

// TestHandleWebhook_WecomBot_IdentityResolvedFromSender verifies that an
// inbound wecom-bot message resolves the platform userID from the sender's
// externalUserID (via idtrust), NOT from configs[0].UserID.
//
// Regression for the "B receives A's task notification" bug: when multiple
// users had wecom-bot configs, HandleWebhook blindly took configs[0].UserID
// as the resolved identity, so B's message was stamped with A's userID.
// The handler then processed A's session/events but replied to B's target —
// leaking A's notifications to B.
func TestHandleWebhook_WecomBot_IdentityResolvedFromSender(t *testing.T) {
	db := setupServiceTestDB(t)

	insertIdentity(t, db, "platform-A", "WecomUser-A")
	insertIdentity(t, db, "platform-B", "WecomUser-B")
	insertIdentity(t, db, "platform-C", "WecomUser-C")
	insertIdentity(t, db, "platform-D", "WecomUser-D")

	configs := []models.ChannelConfig{
		{ID: "cfg-A", ChannelType: "wecom-bot", UserID: "platform-A", Name: "bot-A", Enabled: true},
		{ID: "cfg-B", ChannelType: "wecom-bot", UserID: "platform-B", Name: "bot-B", Enabled: true},
		{ID: "cfg-C", ChannelType: "wecom-bot", UserID: "platform-C", Name: "bot-C", Enabled: true},
		{ID: "cfg-D", ChannelType: "wecom-bot", UserID: "platform-D", Name: "bot-D", Enabled: true},
	}
	for _, cfg := range configs {
		if err := db.Create(&cfg).Error; err != nil {
			t.Fatalf("create config %s: %v", cfg.ID, err)
		}
	}

	adapter := &mockAdapter{channelType: "wecom-bot"}

	var capturedRCs []ReplyContext
	handler := &captureListHandler{
		onHandle: func(rc ReplyContext) {
			capturedRCs = append(capturedRCs, rc)
		},
	}

	svc := &ChannelService{
		db:              db,
		adapters:        map[string]ChannelAdapter{"wecom-bot": adapter},
		messageHandler:  handler,
		sessionStore:    NewReplyContextStore(),
		weComBotEnabled: true,
	}

	// === B sends ONE message ===
	body, _ := json.Marshal(InboundMessage{
		ExternalChatID:   "WecomUser-B",
		ExternalChatType: "single",
		ExternalUserID:   "WecomUser-B",
		Content:          "hello from B",
		ContentType:      "text",
	})
	req := httptest.NewRequest(http.MethodPost, "/api/webhooks/channels/wecom-bot", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")

	if _, _, err := svc.HandleWebhook("wecom-bot", req); err != nil {
		t.Fatalf("HandleWebhook error: %v", err)
	}

	// === Handler invoked exactly once (no fan-out) ===
	t.Run("handler_invoked_once", func(t *testing.T) {
		if len(capturedRCs) != 1 {
			t.Errorf("handler invoked %d times for 1 inbound; want 1", len(capturedRCs))
		}
	})

	// === UserID resolved from sender via idtrust, not from configs[0] ===
	t.Run("userid_resolved_from_sender", func(t *testing.T) {
		if len(capturedRCs) == 0 {
			t.Fatal("no captures")
		}
		if capturedRCs[0].UserID != "platform-B" {
			t.Errorf("UserID = %q, want platform-B (resolved from WecomUser-B via idtrust, "+
				"not configs[0].UserID)", capturedRCs[0].UserID)
		}
	})

	// === Target matches sender ===
	t.Run("target_matches_sender", func(t *testing.T) {
		if len(capturedRCs) == 0 {
			t.Fatal("no captures")
		}
		if capturedRCs[0].Target.ExternalUserID != "WecomUser-B" {
			t.Errorf("Target = %q, want WecomUser-B", capturedRCs[0].Target.ExternalUserID)
		}
	})

	// === Only cfg-B processed (the sender's own config) ===
	t.Run("only_sender_config_processed", func(t *testing.T) {
		if len(capturedRCs) == 0 {
			t.Fatal("no captures")
		}
		if capturedRCs[0].ChannelConfigID != "cfg-B" {
			t.Errorf("ChannelConfigID = %q, want cfg-B (sender's own config)", capturedRCs[0].ChannelConfigID)
		}
	})

	// === sessionStore: only B has a context, exactly one ===
	t.Run("session_store_isolated", func(t *testing.T) {
		bCtxs := svc.sessionStore.LookupByUser("platform-B")
		if len(bCtxs) != 1 {
			t.Errorf("LookupByUser(platform-B) = %d contexts, want 1", len(bCtxs))
		}
		for _, user := range []string{"platform-A", "platform-C", "platform-D"} {
			if ctxs := svc.sessionStore.LookupByUser(user); len(ctxs) != 0 {
				t.Errorf("LookupByUser(%s) = %d contexts, want 0 (never sent)", user, len(ctxs))
			}
		}
	})
}

// TestHandleWebhook_WecomBot_MultipleUsersIsolated verifies that when A and B
// both send messages, each handler invocation carries the correct sender's
// identity, with no cross-contamination.
func TestHandleWebhook_WecomBot_MultipleUsersIsolated(t *testing.T) {
	db := setupServiceTestDB(t)

	insertIdentity(t, db, "platform-A", "WecomUser-A")
	insertIdentity(t, db, "platform-B", "WecomUser-B")

	db.Create(&models.ChannelConfig{
		ID: "cfg-A", ChannelType: "wecom-bot", UserID: "platform-A", Name: "bot-A", Enabled: true,
	})
	db.Create(&models.ChannelConfig{
		ID: "cfg-B", ChannelType: "wecom-bot", UserID: "platform-B", Name: "bot-B", Enabled: true,
	})

	adapter := &mockAdapter{channelType: "wecom-bot"}
	var capturedRCs []ReplyContext
	handler := &captureListHandler{
		onHandle: func(rc ReplyContext) { capturedRCs = append(capturedRCs, rc) },
	}
	svc := &ChannelService{
		db:              db,
		adapters:        map[string]ChannelAdapter{"wecom-bot": adapter},
		messageHandler:  handler,
		sessionStore:    NewReplyContextStore(),
		weComBotEnabled: true,
	}

	// A sends a message
	bodyA, _ := json.Marshal(InboundMessage{
		ExternalChatID: "WecomUser-A", ExternalChatType: "single",
		ExternalUserID: "WecomUser-A", Content: "from A", ContentType: "text",
	})
	reqA := httptest.NewRequest(http.MethodPost, "/api/webhooks/channels/wecom-bot", bytes.NewReader(bodyA))
	reqA.Header.Set("Content-Type", "application/json")
	svc.HandleWebhook("wecom-bot", reqA)

	// B sends a message
	bodyB, _ := json.Marshal(InboundMessage{
		ExternalChatID: "WecomUser-B", ExternalChatType: "single",
		ExternalUserID: "WecomUser-B", Content: "from B", ContentType: "text",
	})
	reqB := httptest.NewRequest(http.MethodPost, "/api/webhooks/channels/wecom-bot", bytes.NewReader(bodyB))
	reqB.Header.Set("Content-Type", "application/json")
	svc.HandleWebhook("wecom-bot", reqB)

	// Exactly 2 invocations (one per message, no fan-out)
	if len(capturedRCs) != 2 {
		t.Fatalf("handler invoked %d times, want 2", len(capturedRCs))
	}

	// First invocation: A's identity
	if capturedRCs[0].UserID != "platform-A" {
		t.Errorf("first message UserID = %q, want platform-A", capturedRCs[0].UserID)
	}
	if capturedRCs[0].Target.ExternalUserID != "WecomUser-A" {
		t.Errorf("first message Target = %q, want WecomUser-A", capturedRCs[0].Target.ExternalUserID)
	}

	// Second invocation: B's identity
	if capturedRCs[1].UserID != "platform-B" {
		t.Errorf("second message UserID = %q, want platform-B", capturedRCs[1].UserID)
	}
	if capturedRCs[1].Target.ExternalUserID != "WecomUser-B" {
		t.Errorf("second message Target = %q, want WecomUser-B", capturedRCs[1].Target.ExternalUserID)
	}

	// Each user has exactly one context in the store
	if ctxs := svc.sessionStore.LookupByUser("platform-A"); len(ctxs) != 1 {
		t.Errorf("LookupByUser(platform-A) = %d, want 1", len(ctxs))
	}
	if ctxs := svc.sessionStore.LookupByUser("platform-B"); len(ctxs) != 1 {
		t.Errorf("LookupByUser(platform-B) = %d, want 1", len(ctxs))
	}
}

// TestHandleWebhook_Wecom_IdentityResolvedFromSender verifies the generalized
// fix applies to the wecom channel too (not just wecom-bot). When multiple
// users have wecom configs, an inbound message from user B must resolve B's
// identity via idtrust — not hijack configs[0].UserID.
func TestHandleWebhook_Wecom_IdentityResolvedFromSender(t *testing.T) {
	db := setupServiceTestDB(t)

	insertIdentity(t, db, "platform-A", "WecomUser-A")
	insertIdentity(t, db, "platform-B", "WecomUser-B")

	db.Create(&models.ChannelConfig{
		ID: "cfg-wecom-A", ChannelType: "wecom", UserID: "platform-A", Name: "wecom-A", Enabled: true,
	})
	db.Create(&models.ChannelConfig{
		ID: "cfg-wecom-B", ChannelType: "wecom", UserID: "platform-B", Name: "wecom-B", Enabled: true,
	})

	adapter := &mockAdapter{channelType: "wecom"}
	var capturedRCs []ReplyContext
	handler := &captureListHandler{
		onHandle: func(rc ReplyContext) { capturedRCs = append(capturedRCs, rc) },
	}
	svc := &ChannelService{
		db:             db,
		adapters:       map[string]ChannelAdapter{"wecom": adapter},
		messageHandler: handler,
		sessionStore:   NewReplyContextStore(),
		weComEnabled:   true,
	}

	body, _ := json.Marshal(InboundMessage{
		ExternalChatID: "WecomUser-B", ExternalChatType: "single",
		ExternalUserID: "WecomUser-B", Content: "hello from B", ContentType: "text",
	})
	req := httptest.NewRequest(http.MethodPost, "/api/webhooks/channels/wecom", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	svc.HandleWebhook("wecom", req)

	// Handler invoked exactly once (no fan-out across both wecom configs)
	if len(capturedRCs) != 1 {
		t.Fatalf("handler invoked %d times, want 1 (wecom must also narrow configs)", len(capturedRCs))
	}

	// UserID resolved from sender via idtrust — NOT from configs[0] (which is A)
	if capturedRCs[0].UserID != "platform-B" {
		t.Errorf("UserID = %q, want platform-B (resolved from sender via idtrust, "+
			"not configs[0].UserID which would be platform-A)", capturedRCs[0].UserID)
	}

	// Only B's config processed
	if capturedRCs[0].ChannelConfigID != "cfg-wecom-B" {
		t.Errorf("ChannelConfigID = %q, want cfg-wecom-B", capturedRCs[0].ChannelConfigID)
	}

	// sessionStore isolation: B has 1 context, A has 0
	if ctxs := svc.sessionStore.LookupByUser("platform-B"); len(ctxs) != 1 {
		t.Errorf("LookupByUser(platform-B) = %d, want 1", len(ctxs))
	}
	if ctxs := svc.sessionStore.LookupByUser("platform-A"); len(ctxs) != 0 {
		t.Errorf("LookupByUser(platform-A) = %d, want 0 (A never sent)", len(ctxs))
	}
}

// TestHandleWebhook_WecomBot_SingleSystemConfig verifies the legacy shape:
// a single system-level config (empty UserID) still works correctly via
// idtrust resolution.
func TestHandleWebhook_WecomBot_SingleSystemConfig(t *testing.T) {
	db := setupServiceTestDB(t)

	insertIdentity(t, db, "platform-B", "WecomUser-B")

	db.Create(&models.ChannelConfig{
		ID:          "cfg-system",
		ChannelType: "wecom-bot",
		Name:        "wecom-bot 系统",
		Enabled:     true,
	})

	adapter := &mockAdapter{channelType: "wecom-bot"}
	var capturedRCs []ReplyContext
	handler := &captureListHandler{
		onHandle: func(rc ReplyContext) { capturedRCs = append(capturedRCs, rc) },
	}
	svc := &ChannelService{
		db:              db,
		adapters:        map[string]ChannelAdapter{"wecom-bot": adapter},
		messageHandler:  handler,
		sessionStore:    NewReplyContextStore(),
		weComBotEnabled: true,
	}

	body, _ := json.Marshal(InboundMessage{
		ExternalChatID: "WecomUser-B", ExternalChatType: "single",
		ExternalUserID: "WecomUser-B", Content: "hello", ContentType: "text",
	})
	req := httptest.NewRequest(http.MethodPost, "/api/webhooks/channels/wecom-bot", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	svc.HandleWebhook("wecom-bot", req)

	if len(capturedRCs) != 1 {
		t.Fatalf("handler invoked %d times, want 1", len(capturedRCs))
	}
	if capturedRCs[0].UserID != "platform-B" {
		t.Errorf("UserID = %q, want platform-B", capturedRCs[0].UserID)
	}
}

// msgCapturingHandler wraps a MessageHandler to also capture inbound messages.
type msgCapturingHandler struct {
	inner    MessageHandler
	captured *[]string
}

func (h *msgCapturingHandler) Handle(ctx context.Context, msg *InboundMessage, sender Sender) error {
	*h.captured = append(*h.captured, msg.Content)
	return h.inner.Handle(ctx, msg, sender)
}
