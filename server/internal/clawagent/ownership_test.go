package clawagent

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"

	"github.com/costrict/costrict-web/server/internal/config"
	"github.com/costrict/costrict-web/server/internal/middleware"
	"github.com/gin-gonic/gin"
)

// setupAPITestWithUserSwap builds a runtime whose auth middleware reads the
// caller's user ID from the X-Test-User-ID header, so a single test can issue
// requests as different users to verify ownership scoping (IDOR regression).
func setupAPITestWithUserSwap(t *testing.T) (*ClawAgentRuntime, *gin.Engine) {
	t.Helper()

	db := setupTestDB(t)
	cfg := &config.Config{
		ClawAgent: config.ClawAgentConfig{
			EncryptionKey: "test-key-32-bytes-long-for-testing!",
			Session: config.ClawAgentSessionConfig{
				DailyResetHour: 4,
			},
		},
		LLM: config.LLMConfig{
			Provider: "openai",
			Model:    "gpt-4",
			BaseURL:  "https://api.openai.com/v1",
			APIKey:   "sk-test",
		},
	}

	rt, err := New(db, cfg, nil, nil)
	if err != nil {
		t.Fatalf("New ClawAgentRuntime: %v", err)
	}

	gin.SetMode(gin.TestMode)
	r := gin.New()

	authed := r.Group("/api")
	authed.Use(func(c *gin.Context) {
		// Test-only identity switcher. Header value plays the role that the
		// real RequireAuth middleware derives from the JWT.
		uid := c.GetHeader("X-Test-User-ID")
		if uid == "" {
			uid = "default-user"
		}
		c.Set(middleware.UserIDKey, uid)
		c.Next()
	})

	rt.RegisterRoutes(authed)

	return rt, r
}

// TestIDOR_ProviderOwnership verifies that the four provider mutation/read
// handlers cannot operate on a provider owned by another user. Regression for
// the auth-bypass fixed by requireOwnedProvider (and removal of the
// "prov.UserID = userID" ownership-overwrite line in handleUpdateProvider).
func TestIDOR_ProviderOwnership(t *testing.T) {
	_, r := setupAPITestWithUserSwap(t)

	// Alice creates a provider.
	createBody, _ := json.Marshal(map[string]any{
		"name":         "alice-llm",
		"providerType": "openai",
		"apiKey":       "sk-alice-secret",
		"baseURL":      "https://api.openai.com/v1",
		"modelName":    "gpt-4",
	})
	w := httptest.NewRecorder()
	req, _ := http.NewRequest("POST", "/api/clawagent/providers", bytes.NewReader(createBody))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Test-User-ID", "alice")
	r.ServeHTTP(w, req)
	if w.Code != http.StatusCreated {
		t.Fatalf("alice create provider: status=%d body=%s", w.Code, w.Body.String())
	}

	// Resolve the created provider ID via Alice's list.
	w = httptest.NewRecorder()
	req, _ = http.NewRequest("GET", "/api/clawagent/providers", nil)
	req.Header.Set("X-Test-User-ID", "alice")
	r.ServeHTTP(w, req)
	var listResp struct {
		Providers []providerView `json:"providers"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &listResp); err != nil {
		t.Fatalf("list: %v body=%s", err, w.Body.String())
	}
	var aliceProviderID uint
	for _, p := range listResp.Providers {
		if p.Name == "alice-llm" {
			aliceProviderID = p.ID
			break
		}
	}
	if aliceProviderID == 0 {
		t.Fatalf("alice provider not found in list: %+v", listResp.Providers)
	}
	aliceProviderIDStr := strconv.FormatUint(uint64(aliceProviderID), 10)

	// Bob tries to UPDATE alice's provider — must be blocked AND must not
	// overwrite alice's ownership (the historical bug).
	updateBody, _ := json.Marshal(map[string]any{
		"name":      "stolen-by-bob",
		"modelName": "gpt-4o",
	})
	w = httptest.NewRecorder()
	req, _ = http.NewRequest("PUT", "/api/clawagent/providers/"+aliceProviderIDStr, bytes.NewReader(updateBody))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Test-User-ID", "bob")
	r.ServeHTTP(w, req)
	if w.Code != http.StatusNotFound {
		t.Errorf("bob update alice's provider: expected 404, got %d body=%s", w.Code, w.Body.String())
	}

	// Bob tries to DELETE alice's provider — must be blocked.
	w = httptest.NewRecorder()
	req, _ = http.NewRequest("DELETE", "/api/clawagent/providers/"+aliceProviderIDStr, nil)
	req.Header.Set("X-Test-User-ID", "bob")
	r.ServeHTTP(w, req)
	if w.Code != http.StatusNotFound {
		t.Errorf("bob delete alice's provider: expected 404, got %d", w.Code)
	}

	// Bob tries to TEST alice's provider — must be blocked (would otherwise
	// decrypt alice's APIKey server-side and POST it out).
	w = httptest.NewRecorder()
	req, _ = http.NewRequest("POST", "/api/clawagent/providers/"+aliceProviderIDStr+"/test", nil)
	req.Header.Set("X-Test-User-ID", "bob")
	r.ServeHTTP(w, req)
	if w.Code != http.StatusNotFound {
		t.Errorf("bob test alice's provider: expected 404, got %d", w.Code)
	}

	// Alice still owns and can update her own provider.
	w = httptest.NewRecorder()
	req, _ = http.NewRequest("PUT", "/api/clawagent/providers/"+aliceProviderIDStr, bytes.NewReader(updateBody))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Test-User-ID", "alice")
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Errorf("alice update own provider: expected 200, got %d body=%s", w.Code, w.Body.String())
	}
}

// TestIDOR_PersonaOwnership verifies persona mutation handlers reject
// cross-user access. Regression for the same IDOR pattern that affected
// providers but was unreported for personas.
func TestIDOR_PersonaOwnership(t *testing.T) {
	_, r := setupAPITestWithUserSwap(t)

	// Alice creates a persona.
	createBody, _ := json.Marshal(map[string]any{
		"name":        "alice-persona",
		"soulContent": "alice's advisor",
	})
	w := httptest.NewRecorder()
	req, _ := http.NewRequest("POST", "/api/clawagent/personas", bytes.NewReader(createBody))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Test-User-ID", "alice")
	r.ServeHTTP(w, req)
	if w.Code != http.StatusCreated {
		t.Fatalf("alice create persona: status=%d body=%s", w.Code, w.Body.String())
	}

	// Resolve persona ID via Alice's list.
	w = httptest.NewRecorder()
	req, _ = http.NewRequest("GET", "/api/clawagent/personas", nil)
	req.Header.Set("X-Test-User-ID", "alice")
	r.ServeHTTP(w, req)
	var listResp struct {
		Personas []Persona `json:"personas"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &listResp); err != nil {
		t.Fatalf("list personas: %v", err)
	}
	var personaID string
	for _, p := range listResp.Personas {
		if p.Name == "alice-persona" {
			personaID = p.ID
			break
		}
	}
	if personaID == "" {
		t.Fatalf("alice persona not found: %+v", listResp.Personas)
	}

	// Bob cannot update.
	updateBody, _ := json.Marshal(map[string]any{"soulContent": "hijacked"})
	w = httptest.NewRecorder()
	req, _ = http.NewRequest("PUT", "/api/clawagent/personas/"+personaID, bytes.NewReader(updateBody))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Test-User-ID", "bob")
	r.ServeHTTP(w, req)
	if w.Code != http.StatusNotFound {
		t.Errorf("bob update alice's persona: expected 404, got %d", w.Code)
	}

	// Bob cannot delete.
	w = httptest.NewRecorder()
	req, _ = http.NewRequest("DELETE", "/api/clawagent/personas/"+personaID, nil)
	req.Header.Set("X-Test-User-ID", "bob")
	r.ServeHTTP(w, req)
	if w.Code != http.StatusNotFound {
		t.Errorf("bob delete alice's persona: expected 404, got %d", w.Code)
	}
}

// TestIDOR_SessionOwnership verifies session read/archive is scoped.
func TestIDOR_SessionOwnership(t *testing.T) {
	rt, r := setupAPITestWithUserSwap(t)

	// Create a session owned by alice directly via the runtime. SessionID is
	// formatted as "<baseKey>:v<version>".
	meta, err := rt.SessionMeta.Create(context.Background(), "alice", "alice-session-1", 1, "daily")
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	sid := meta.SessionID

	// Bob cannot read.
	w := httptest.NewRecorder()
	req, _ := http.NewRequest("GET", "/api/clawagent/sessions/"+sid, nil)
	req.Header.Set("X-Test-User-ID", "bob")
	r.ServeHTTP(w, req)
	if w.Code != http.StatusNotFound {
		t.Errorf("bob read alice's session: expected 404, got %d body=%s", w.Code, w.Body.String())
	}

	// Bob cannot delete.
	w = httptest.NewRecorder()
	req, _ = http.NewRequest("DELETE", "/api/clawagent/sessions/"+sid, nil)
	req.Header.Set("X-Test-User-ID", "bob")
	r.ServeHTTP(w, req)
	if w.Code != http.StatusNotFound {
		t.Errorf("bob delete alice's session: expected 404, got %d", w.Code)
	}

	// Alice can read.
	w = httptest.NewRecorder()
	req, _ = http.NewRequest("GET", "/api/clawagent/sessions/"+sid, nil)
	req.Header.Set("X-Test-User-ID", "alice")
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Errorf("alice read own session: expected 200, got %d", w.Code)
	}
}
