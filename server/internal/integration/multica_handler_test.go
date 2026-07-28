package integration

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	gormlogger "gorm.io/gorm/logger"

	"github.com/costrict/costrict-web/server/internal/models"
	"github.com/costrict/costrict-web/server/internal/notification/sender"
)

const testSecret = "test-secret"

// fakeTrigger captures TriggerMessage calls.
type fakeTrigger struct {
	calls []triggerCall
}

type triggerCall struct {
	userID    string
	eventType string
	msg       sender.NotificationMessage
}

func (f *fakeTrigger) TriggerMessage(userID, eventType string, msg sender.NotificationMessage) {
	f.calls = append(f.calls, triggerCall{userID: userID, eventType: eventType, msg: msg})
}

func setupTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{
		Logger: gormlogger.Default.LogMode(gormlogger.Silent),
	})
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	if err := db.AutoMigrate(&IntegrationEvent{}, &models.User{}); err != nil {
		t.Fatalf("automigrate: %v", err)
	}
	return db
}

func seedUser(t *testing.T, db *gorm.DB, subjectID, email string) {
	t.Helper()
	u := models.User{
		SubjectID: subjectID,
		Username:  subjectID,
		Email:     &email,
	}
	if err := db.Create(&u).Error; err != nil {
		t.Fatalf("seed user: %v", err)
	}
}

func signBody(secret string, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

func envelopeBody(t *testing.T, mut func(*Envelope)) []byte {
	t.Helper()
	env := Envelope{
		Version:    1,
		EventID:    fmt.Sprintf("evt-%d", time.Now().UnixNano()),
		Type:       EventMulticaIssueStatusChanged,
		OccurredAt: time.Date(2026, 7, 28, 10, 0, 0, 0, time.UTC),
		Workspace:  WorkspaceRef{ID: "ws-1", Name: "Acme"},
		Actor:      ActorRef{Type: "system", Name: ""},
		Issue: IssueRef{
			ID:         "issue-1",
			Identifier: "MUL-123",
			Title:      "修复通知",
			PrevStatus: "in_progress",
			Status:     "done",
			URL:        "https://multica.example.com/acme/issues/MUL-123",
		},
		Recipients: []string{"alice@corp.com"},
	}
	if mut != nil {
		mut(&env)
	}
	body, err := json.Marshal(env)
	if err != nil {
		t.Fatalf("marshal envelope: %v", err)
	}
	return body
}

func postEnvelope(t *testing.T, handler gin.HandlerFunc, body []byte, sig string) *httptest.ResponseRecorder {
	t.Helper()
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.POST("/api/integrations/multica/events", handler)
	req := httptest.NewRequest(http.MethodPost, "/api/integrations/multica/events", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if sig != "" {
		req.Header.Set("X-Multica-Signature", sig)
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

func TestMulticaEvents_ValidEnvelopeTriggersNotification(t *testing.T) {
	db := setupTestDB(t)
	seedUser(t, db, "subject-alice", "alice@corp.com")
	trigger := &fakeTrigger{}

	body := envelopeBody(t, nil)
	w := postEnvelope(t, MulticaEventsHandler(db, trigger, testSecret), body, signBody(testSecret, body))

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}
	if len(trigger.calls) != 1 {
		t.Fatalf("trigger calls = %d, want 1", len(trigger.calls))
	}
	call := trigger.calls[0]
	if call.userID != "subject-alice" {
		t.Fatalf("userID = %q, want subject-alice", call.userID)
	}
	if call.eventType != EventMulticaIssueStatusChanged {
		t.Fatalf("eventType = %q", call.eventType)
	}
	if call.msg.Title == "" || call.msg.Body == "" {
		t.Fatalf("empty message: %+v", call.msg)
	}
	if !bytes.Contains([]byte(call.msg.Body), []byte("MUL-123")) {
		t.Fatalf("body missing identifier: %q", call.msg.Body)
	}
	if !bytes.Contains([]byte(call.msg.Body), []byte("https://multica.example.com/acme/issues/MUL-123")) {
		t.Fatalf("body missing issue link: %q", call.msg.Body)
	}
}

func TestMulticaEvents_RejectsBadSignature(t *testing.T) {
	db := setupTestDB(t)
	seedUser(t, db, "subject-alice", "alice@corp.com")
	trigger := &fakeTrigger{}

	body := envelopeBody(t, nil)
	w := postEnvelope(t, MulticaEventsHandler(db, trigger, testSecret), body, signBody("wrong-secret", body))

	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", w.Code)
	}
	if len(trigger.calls) != 0 {
		t.Fatalf("trigger called despite bad signature")
	}
}

func TestMulticaEvents_RejectsMissingSignature(t *testing.T) {
	db := setupTestDB(t)
	trigger := &fakeTrigger{}

	body := envelopeBody(t, nil)
	w := postEnvelope(t, MulticaEventsHandler(db, trigger, testSecret), body, "")

	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", w.Code)
	}
}

func TestMulticaEvents_RejectsMalformedJSON(t *testing.T) {
	db := setupTestDB(t)
	trigger := &fakeTrigger{}

	body := []byte("{not json")
	w := postEnvelope(t, MulticaEventsHandler(db, trigger, testSecret), body, signBody(testSecret, body))

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", w.Code)
	}
}

func TestMulticaEvents_DuplicateEventIDDeliveredOnce(t *testing.T) {
	db := setupTestDB(t)
	seedUser(t, db, "subject-alice", "alice@corp.com")
	trigger := &fakeTrigger{}
	handler := MulticaEventsHandler(db, trigger, testSecret)

	// Fixed event_id: the second delivery is a retry of the first.
	body := envelopeBody(t, func(e *Envelope) { e.EventID = "evt-fixed" })
	sig := signBody(testSecret, body)

	if w := postEnvelope(t, handler, body, sig); w.Code != http.StatusOK {
		t.Fatalf("first delivery status = %d", w.Code)
	}
	if w := postEnvelope(t, handler, body, sig); w.Code != http.StatusOK {
		t.Fatalf("duplicate delivery status = %d, want 200 (idempotent ack)", w.Code)
	}
	if len(trigger.calls) != 1 {
		t.Fatalf("trigger calls = %d, want 1 (duplicate suppressed)", len(trigger.calls))
	}
}

func TestMulticaEvents_UnknownTypeAckedWithoutDelivery(t *testing.T) {
	db := setupTestDB(t)
	seedUser(t, db, "subject-alice", "alice@corp.com")
	trigger := &fakeTrigger{}

	body := envelopeBody(t, func(e *Envelope) { e.Type = "multica.issue.commented" })
	w := postEnvelope(t, MulticaEventsHandler(db, trigger, testSecret), body, signBody(testSecret, body))

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (forward-compatible ack)", w.Code)
	}
	if len(trigger.calls) != 0 {
		t.Fatalf("trigger called for unknown event type")
	}
}

func TestMulticaEvents_UnmatchedEmailSkipped(t *testing.T) {
	db := setupTestDB(t)
	trigger := &fakeTrigger{}

	body := envelopeBody(t, func(e *Envelope) { e.Recipients = []string{"ghost@corp.com"} })
	w := postEnvelope(t, MulticaEventsHandler(db, trigger, testSecret), body, signBody(testSecret, body))

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	if len(trigger.calls) != 0 {
		t.Fatalf("trigger called for unmatched email")
	}
}

func TestMulticaEvents_MultipleRecipientsPartialMatch(t *testing.T) {
	db := setupTestDB(t)
	seedUser(t, db, "subject-alice", "alice@corp.com")
	trigger := &fakeTrigger{}

	body := envelopeBody(t, func(e *Envelope) {
		e.Recipients = []string{"alice@corp.com", "ghost@corp.com"}
	})
	w := postEnvelope(t, MulticaEventsHandler(db, trigger, testSecret), body, signBody(testSecret, body))

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d", w.Code)
	}
	if len(trigger.calls) != 1 {
		t.Fatalf("trigger calls = %d, want 1", len(trigger.calls))
	}
	if trigger.calls[0].userID != "subject-alice" {
		t.Fatalf("userID = %q", trigger.calls[0].userID)
	}
}
