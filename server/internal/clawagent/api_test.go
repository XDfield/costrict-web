package clawagent

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/costrict/costrict-web/server/internal/config"
	"github.com/costrict/costrict-web/server/internal/middleware"
	"github.com/gin-gonic/gin"
)

func setupAPITest(t *testing.T) (*ClawAgentRuntime, *gin.Engine) {
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
		c.Set(middleware.UserIDKey, "test-user-id")
		c.Next()
	})

	rt.RegisterRoutes(authed)

	return rt, r
}

func TestAPI_GetMemory_Empty(t *testing.T) {
	_, r := setupAPITest(t)

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("GET", "/api/clawagent/memory", nil)
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("status = %d, want %d", w.Code, http.StatusOK)
	}

	var resp struct {
		Content     string `json:"content"`
		LengthBytes int    `json:"lengthBytes"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if resp.Content != "" {
		t.Errorf("Content = %q, want empty", resp.Content)
	}
}

func TestAPI_UpdateMemory(t *testing.T) {
	_, r := setupAPITest(t)

	body := map[string]string{"content": "用户偏好 Go 语言"}
	b, _ := json.Marshal(body)

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("PUT", "/api/clawagent/memory", bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("status = %d, want %d", w.Code, http.StatusOK)
	}

	var resp struct {
		Content     string `json:"content"`
		LengthBytes int    `json:"lengthBytes"`
	}
	json.Unmarshal(w.Body.Bytes(), &resp)
	if resp.Content != "用户偏好 Go 语言" {
		t.Errorf("Content = %q", resp.Content)
	}

	// Read it back
	w2 := httptest.NewRecorder()
	req2, _ := http.NewRequest("GET", "/api/clawagent/memory", nil)
	r.ServeHTTP(w2, req2)

	var resp2 struct {
		Content string `json:"content"`
	}
	json.Unmarshal(w2.Body.Bytes(), &resp2)
	if resp2.Content != "用户偏好 Go 语言" {
		t.Errorf("read back = %q", resp2.Content)
	}
}

func TestAPI_MemoryTruncation(t *testing.T) {
	_, r := setupAPITest(t)

	overLimit := string(make([]byte, MaxMemoryBytes+100))
	body := map[string]string{"content": overLimit}
	b, _ := json.Marshal(body)

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("PUT", "/api/clawagent/memory", bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("status = %d, want %d", w.Code, http.StatusOK)
	}

	var resp struct {
		Content     string `json:"content"`
		LengthBytes int    `json:"lengthBytes"`
		Truncated   bool   `json:"truncated"`
	}
	json.Unmarshal(w.Body.Bytes(), &resp)
	if !resp.Truncated {
		t.Error("expected truncation flag")
	}
	if resp.LengthBytes > MaxMemoryBytes {
		t.Errorf("LengthBytes = %d, want <= %d", resp.LengthBytes, MaxMemoryBytes)
	}
}

func TestAPI_CreateListPersonas(t *testing.T) {
	_, r := setupAPITest(t)

	// Create a persona
	personaBody := map[string]any{
		"name":        "tech-advisor",
		"soulContent": "你是一位技术顾问。",
		"isDefault":   true,
	}
	b, _ := json.Marshal(personaBody)

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("POST", "/api/clawagent/personas", bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(w, req)

	if w.Code != http.StatusCreated {
		t.Errorf("create status = %d, want %d. Body: %s", w.Code, http.StatusCreated, w.Body.String())
	}

	// List personas
	w2 := httptest.NewRecorder()
	req2, _ := http.NewRequest("GET", "/api/clawagent/personas", nil)
	r.ServeHTTP(w2, req2)

	if w2.Code != http.StatusOK {
		t.Errorf("list status = %d, want %d", w2.Code, http.StatusOK)
	}

	var listResp struct {
		Personas []Persona `json:"personas"`
	}
	if err := json.Unmarshal(w2.Body.Bytes(), &listResp); err != nil {
		t.Fatalf("unmarshal personas: %v", err)
	}

	if len(listResp.Personas) < 1 {
		t.Fatalf("expected at least 1 persona, got %d", len(listResp.Personas))
	}

	// Verify our created persona is in the list (first one since it's default)
	found := false
	for _, p := range listResp.Personas {
		if p.Name == "tech-advisor" {
			found = true
			break
		}
	}
	if !found {
		t.Error("created persona not found in list")
	}
}

func TestAPI_CreateListProviders(t *testing.T) {
	_, r := setupAPITest(t)

	// Create a provider
	providerBody := map[string]any{
		"name":         "my-deepseek",
		"providerType": "deepseek",
		"apiKey":       "sk-test-key",
		"baseURL":      "https://api.deepseek.com/v1",
		"modelName":    "deepseek-chat",
		"isDefault":    true,
	}
	b, _ := json.Marshal(providerBody)

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("POST", "/api/clawagent/providers", bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(w, req)

	if w.Code != http.StatusCreated {
		t.Errorf("create status = %d, want %d. Body: %s", w.Code, http.StatusCreated, w.Body.String())
	}

	// List providers
	w2 := httptest.NewRecorder()
	req2, _ := http.NewRequest("GET", "/api/clawagent/providers", nil)
	r.ServeHTTP(w2, req2)

	if w2.Code != http.StatusOK {
		t.Errorf("list status = %d, want %d", w2.Code, http.StatusOK)
	}

	var listResp struct {
		Providers []providerView `json:"providers"`
	}
	if err := json.Unmarshal(w2.Body.Bytes(), &listResp); err != nil {
		t.Fatalf("unmarshal providers: %v", err)
	}

	// Should have at least 1 (our created one, since platform default needs cfg)
	found := false
	for _, p := range listResp.Providers {
		if p.Name == "my-deepseek" {
			found = true
			break
		}
	}
	// Note: the real test may also include platform-default depending on cfg
	if !found {
		t.Error("created provider not found in list")
	}
}

func TestAPI_Sessions_Empty(t *testing.T) {
	_, r := setupAPITest(t)

	w := httptest.NewRecorder()
	req, _ := http.NewRequest("GET", "/api/clawagent/sessions", nil)
	r.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("status = %d, want %d", w.Code, http.StatusOK)
	}

	var resp struct {
		Sessions []any `json:"sessions"`
	}
	json.Unmarshal(w.Body.Bytes(), &resp)
	// No sessions created yet - should be empty (or contain auto-created ones)
	if resp.Sessions == nil {
		t.Error("sessions should not be nil (should be empty array)")
	}
}

func TestAPI_UpdatePersona_Partial(t *testing.T) {
	_, r := setupAPITest(t)

	// Create a persona first
	createBody := map[string]any{
		"name":        "original-name",
		"soulContent": "原始内容",
		"isDefault":   true,
	}
	b, _ := json.Marshal(createBody)
	w := httptest.NewRecorder()
	req, _ := http.NewRequest("POST", "/api/clawagent/personas", bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(w, req)

	if w.Code != http.StatusCreated {
		t.Fatalf("create status = %d", w.Code)
	}

	var createResp struct {
		ID string `json:"id"`
	}
	json.Unmarshal(w.Body.Bytes(), &createResp)

	// Partial update - only change soulContent
	updateBody := map[string]any{
		"soulContent": "新内容",
	}
	b2, _ := json.Marshal(updateBody)
	w2 := httptest.NewRecorder()
	req2, _ := http.NewRequest("PUT", "/api/clawagent/personas/"+createResp.ID, bytes.NewReader(b2))
	req2.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(w2, req2)

	if w2.Code != http.StatusOK {
		t.Fatalf("update status = %d, body: %s", w2.Code, w2.Body.String())
	}

	var updated Persona
	json.Unmarshal(w2.Body.Bytes(), &updated)

	if updated.Name != "original-name" {
		t.Errorf("Name after partial update = %q, want %q (should not be overwritten)", updated.Name, "original-name")
	}
	if updated.SoulContent != "新内容" {
		t.Errorf("SoulContent = %q, want %q", updated.SoulContent, "新内容")
	}
}

func TestAPI_Unauthenticated(t *testing.T) {
	gin.SetMode(gin.TestMode)
	r := gin.New()

	// No auth middleware, just the raw routes
	rt, err := New(setupTestDB(t), &config.Config{
		ClawAgent: config.ClawAgentConfig{
			EncryptionKey: "test-key-32-bytes-long-for-testing!",
		},
		LLM: config.LLMConfig{
			Provider: "openai",
			Model:    "gpt-4",
			BaseURL:  "https://api.openai.com/v1",
			APIKey:   "sk-test",
		},
	}, nil, nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	// Register on a group without auth
	rt.RegisterRoutes(r.Group("/api"))

	// Without userID in context, should return 401
	w := httptest.NewRecorder()
	req, _ := http.NewRequest("GET", "/api/clawagent/memory", nil)
	r.ServeHTTP(w, req)

	if w.Code != http.StatusUnauthorized {
		t.Errorf("unauthenticated request status = %d, want %d", w.Code, http.StatusUnauthorized)
	}
}

// providerView mirrors the provider response struct from api.go
type providerView struct {
	ID           uint   `json:"id"`
	Name         string `json:"name"`
	ProviderType string `json:"providerType"`
	ModelName    string `json:"modelName"`
	IsDefault    bool   `json:"isDefault"`
}
