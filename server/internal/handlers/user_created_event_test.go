// Tests for user.created event consumer (Git Ownership Refactor Phase 2).
//
// Phase 2: endpoint validates payload + event_id shape, returns 202, does
// not invoke ProvisionUser. Phase 3 tests will cover the dispatch branch
// (gated by USER_CREATED_EVENT_PROCESSING_ENABLED).

package handlers

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"go.uber.org/zap"
)

func newEventRouter() *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	api := &UserCreatedEventAPI{Log: zap.NewNop()}
	r.POST("/api/internal/users/created", api.ReceiveUserCreated)
	return r
}

func postEvent(t *testing.T, r *gin.Engine, body string, eventIDHeader string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/api/internal/users/created", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	if eventIDHeader != "" {
		req.Header.Set("X-Event-ID", eventIDHeader)
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

func TestReceiveUserCreated_HappyPathReturns202(t *testing.T) {
	r := newEventRouter()
	body := `{
		"event_id": "12345678-1234-4234-8234-123456789012",
		"event_type": "user.created",
		"subject_id": "usr-1",
		"tenant_id": "t1",
		"occurred_at": "2026-07-22T12:00:00Z",
		"user": {"subject_id": "usr-1", "username": "alice"}
	}`
	w := postEvent(t, r, body, "12345678-1234-4234-8234-123456789012")
	if w.Code != http.StatusAccepted {
		t.Fatalf("status = %d, body=%s", w.Code, w.Body.String())
	}
}

func TestReceiveUserCreated_MalformedJSON(t *testing.T) {
	r := newEventRouter()
	w := postEvent(t, r, `{not json`, "")
	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", w.Code)
	}
}

func TestReceiveUserCreated_BadUUID(t *testing.T) {
	r := newEventRouter()
	body := `{"event_id": "not-a-uuid", "event_type": "user.created", "subject_id": "x", "user": {"subject_id":"x"}}`
	w := postEvent(t, r, body, "")
	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400 (bad uuid)", w.Code)
	}
}

func TestReceiveUserCreated_HeaderBodyMismatch(t *testing.T) {
	r := newEventRouter()
	body := `{"event_id": "11111111-1111-4111-8111-111111111111", "event_type": "user.created", "subject_id": "x", "user": {"subject_id":"x"}}`
	w := postEvent(t, r, body, "22222222-2222-4222-8222-222222222222")
	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400 (header/body event_id mismatch)", w.Code)
	}
}

func TestReceiveUserCreated_WrongEventType(t *testing.T) {
	r := newEventRouter()
	body := `{"event_id": "11111111-1111-4111-8111-111111111111", "event_type": "user.deleted", "subject_id": "x", "user": {"subject_id":"x"}}`
	w := postEvent(t, r, body, "")
	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", w.Code)
	}
}
