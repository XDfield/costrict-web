package llm

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"github.com/costrict/costrict-web/server/internal/config"
)

// TestNewClient tests client creation
func TestNewClient(t *testing.T) {
	cfg := &config.LLMConfig{
		Provider:    "openai",
		APIKey:      "test-key",
		Model:       "glm-4-plus",
		BaseURL:     "https://api.example.com/v1",
		MaxTokens:   4096,
		Temperature: 0.7,
	}

	client := NewClient(cfg)
	if client == nil {
		t.Fatal("Expected non-nil client")
	}
	if client.cfg != cfg {
		t.Error("Config not set correctly")
	}
	if client.httpClient == nil {
		t.Error("HTTP client not initialized")
	}
}

// TestChat_Success tests successful chat completion
func TestChat_Success(t *testing.T) {
	// Create mock server
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Verify request
		if r.Method != "POST" {
			t.Errorf("Expected POST, got %s", r.Method)
		}
		if r.URL.Path != "/chat/completions" {
			t.Errorf("Expected /chat/completions, got %s", r.URL.Path)
		}
		if r.Header.Get("Authorization") != "Bearer test-api-key" {
			t.Errorf("Expected Bearer token, got %s", r.Header.Get("Authorization"))
		}

		// Return mock response
		resp := ChatResponse{
			ID:      "chat-123",
			Object:  "chat.completion",
			Created: 1234567890,
			Model:   "glm-4-plus",
			Choices: []struct {
				Index        int         `json:"index"`
				Message      ChatMessage `json:"message"`
				FinishReason string      `json:"finish_reason"`
			}{
				{
					Index:        0,
					Message:      ChatMessage{Role: "assistant", Content: "Hello! How can I help you?"},
					FinishReason: "stop",
				},
			},
			Usage: struct {
				PromptTokens     int `json:"prompt_tokens"`
				CompletionTokens int `json:"completion_tokens"`
				TotalTokens      int `json:"total_tokens"`
			}{
				PromptTokens:     10,
				CompletionTokens: 8,
				TotalTokens:      18,
			},
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp)
	}))
	defer server.Close()

	cfg := &config.LLMConfig{
		APIKey:      "test-api-key",
		Model:       "glm-4-plus",
		BaseURL:     server.URL,
		MaxTokens:   4096,
		Temperature: 0.7,
	}
	client := NewClient(cfg)

	messages := []ChatMessage{
		{Role: "user", Content: "Hello"},
	}

	resp, err := client.Chat(messages)
	if err != nil {
		t.Fatalf("Chat failed: %v", err)
	}

	if len(resp.Choices) != 1 {
		t.Errorf("Expected 1 choice, got %d", len(resp.Choices))
	}
	if resp.Choices[0].Message.Content != "Hello! How can I help you?" {
		t.Errorf("Unexpected response: %s", resp.Choices[0].Message.Content)
	}
	if resp.Usage.TotalTokens != 18 {
		t.Errorf("Expected 18 tokens, got %d", resp.Usage.TotalTokens)
	}
}

// TestChat_APIError tests handling of API errors
func TestChat_APIError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		w.Write([]byte(`{"error": "Invalid API key"}`))
	}))
	defer server.Close()

	cfg := &config.LLMConfig{
		APIKey:  "invalid-key",
		Model:   "glm-4-plus",
		BaseURL: server.URL,
	}
	client := NewClient(cfg)

	_, err := client.Chat([]ChatMessage{{Role: "user", Content: "test"}})
	if err == nil {
		t.Fatal("Expected error for API failure")
	}
}

// TestChatSimple_Success tests ChatSimple method
func TestChatSimple_Success(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		resp := ChatResponse{
			ID:    "chat-123",
			Model: "glm-4-plus",
			Choices: []struct {
				Index        int         `json:"index"`
				Message      ChatMessage `json:"message"`
				FinishReason string      `json:"finish_reason"`
			}{
				{
					Index:        0,
					Message:      ChatMessage{Role: "assistant", Content: "Simple response"},
					FinishReason: "stop",
				},
			},
		}
		json.NewEncoder(w).Encode(resp)
	}))
	defer server.Close()

	cfg := &config.LLMConfig{
		APIKey:  "test-key",
		Model:   "glm-4-plus",
		BaseURL: server.URL,
	}
	client := NewClient(cfg)

	content, err := client.ChatSimple("You are helpful", "Say hello")
	if err != nil {
		t.Fatalf("ChatSimple failed: %v", err)
	}
	if content != "Simple response" {
		t.Errorf("Expected 'Simple response', got '%s'", content)
	}
}

// TestChatSimple_NoChoices tests error when no choices returned
func TestChatSimple_NoChoices(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		resp := ChatResponse{
			ID:      "chat-123",
			Model:   "glm-4-plus",
			Choices: []struct {
				Index        int         `json:"index"`
				Message      ChatMessage `json:"message"`
				FinishReason string      `json:"finish_reason"`
			}{},
		}
		json.NewEncoder(w).Encode(resp)
	}))
	defer server.Close()

	cfg := &config.LLMConfig{
		APIKey:  "test-key",
		Model:   "glm-4-plus",
		BaseURL: server.URL,
	}
	client := NewClient(cfg)

	_, err := client.ChatSimple("system", "user")
	if err == nil {
		t.Fatal("Expected error for no choices")
	}
}

// TestGetEmbedding_Success tests embedding generation
func TestGetEmbedding_Success(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/embeddings" {
			t.Errorf("Expected /embeddings, got %s", r.URL.Path)
		}

		resp := EmbeddingResponse{
			Object: "list",
			Data: []struct {
				Object    string    `json:"object"`
				Index     int       `json:"index"`
				Embedding []float64 `json:"embedding"`
			}{
				{
					Object:    "embedding",
					Index:     0,
					Embedding: []float64{0.1, 0.2, 0.3, 0.4, 0.5},
				},
				{
					Object:    "embedding",
					Index:     1,
					Embedding: []float64{0.6, 0.7, 0.8, 0.9, 1.0},
				},
			},
			Model: "embedding-3",
			Usage: struct {
				PromptTokens int `json:"prompt_tokens"`
				TotalTokens  int `json:"total_tokens"`
			}{
				PromptTokens: 5,
				TotalTokens:  5,
			},
		}
		json.NewEncoder(w).Encode(resp)
	}))
	defer server.Close()

	cfg := &config.LLMConfig{
		APIKey:  "test-key",
		Model:   "embedding-3",
		BaseURL: server.URL,
	}
	client := NewClient(cfg)

	embeddings, err := client.GetEmbedding([]string{"hello", "world"}, 1024)
	if err != nil {
		t.Fatalf("GetEmbedding failed: %v", err)
	}
	if len(embeddings) != 2 {
		t.Errorf("Expected 2 embeddings, got %d", len(embeddings))
	}
	if len(embeddings[0]) != 5 {
		t.Errorf("Expected 5 dimensions, got %d", len(embeddings[0]))
	}
}

// TestGetEmbedding_APIError tests embedding API error handling
func TestGetEmbedding_APIError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte(`{"error": "Internal server error"}`))
	}))
	defer server.Close()

	cfg := &config.LLMConfig{
		APIKey:  "test-key",
		Model:   "embedding-3",
		BaseURL: server.URL,
	}
	client := NewClient(cfg)

	_, err := client.GetEmbedding([]string{"test"}, 1024)
	if err == nil {
		t.Fatal("Expected error for API failure")
	}
}

// ============================================================
// Integration Tests (require real API key)
// Run with: go test -tags=integration ./...
// Or set LLM_API_KEY environment variable
// ============================================================

// TestIntegration_Chat tests real API call
func TestIntegration_Chat(t *testing.T) {
	apiKey := os.Getenv("LLM_API_KEY")
	if apiKey == "" {
		t.Skip("LLM_API_KEY not set, skipping integration test")
	}

	cfg := &config.LLMConfig{
		Provider:    "openai",
		APIKey:      apiKey,
		Model:       getEnvOrDefault("LLM_MODEL", "glm-4-flash"),
		BaseURL:     getEnvOrDefault("LLM_BASE_URL", "https://open.bigmodel.cn/api/paas/v4"),
		MaxTokens:   100,
		Temperature: 0.7,
	}
	client := NewClient(cfg)

	messages := []ChatMessage{
		{Role: "user", Content: "Say 'Hello, World!' and nothing else."},
	}

	resp, err := client.Chat(messages)
	if err != nil {
		t.Fatalf("Chat failed: %v", err)
	}

	if len(resp.Choices) == 0 {
		t.Fatal("No choices in response")
	}

	content := resp.Choices[0].Message.Content
	t.Logf("Response: %s", content)
	t.Logf("Tokens used: prompt=%d, completion=%d, total=%d",
		resp.Usage.PromptTokens, resp.Usage.CompletionTokens, resp.Usage.TotalTokens)

	// finish_reason can be "stop" or "length" depending on token limit
	if resp.Choices[0].FinishReason != "stop" && resp.Choices[0].FinishReason != "length" {
		t.Errorf("Unexpected finish reason: %s", resp.Choices[0].FinishReason)
	}
}

// TestIntegration_ChatSimple tests real simple chat
func TestIntegration_ChatSimple(t *testing.T) {
	apiKey := os.Getenv("LLM_API_KEY")
	if apiKey == "" {
		t.Skip("LLM_API_KEY not set, skipping integration test")
	}

	cfg := &config.LLMConfig{
		Provider:    "openai",
		APIKey:      apiKey,
		Model:       getEnvOrDefault("LLM_MODEL", "glm-4-flash"),
		BaseURL:     getEnvOrDefault("LLM_BASE_URL", "https://open.bigmodel.cn/api/paas/v4"),
		MaxTokens:   50,
		Temperature: 0.7,
	}
	client := NewClient(cfg)

	content, err := client.ChatSimple(
		"You are a helpful assistant. Be very brief.",
		"What is 2+2? Answer with just the number.",
	)
	if err != nil {
		t.Fatalf("ChatSimple failed: %v", err)
	}

	t.Logf("Response: %s", content)
	if content == "" {
		t.Error("Empty response")
	}
}

// TestIntegration_GetEmbedding tests real embedding generation
func TestIntegration_GetEmbedding(t *testing.T) {
	apiKey := os.Getenv("LLM_API_KEY")
	if apiKey == "" {
		t.Skip("LLM_API_KEY not set, skipping integration test")
	}

	cfg := &config.LLMConfig{
		Provider: "openai",
		APIKey:   apiKey,
		Model:    getEnvOrDefault("EMBEDDING_MODEL", "embedding-3"),
		BaseURL:  getEnvOrDefault("EMBEDDING_BASE_URL", "https://open.bigmodel.cn/api/paas/v4"),
	}
	client := NewClient(cfg)

	texts := []string{"Hello world", "Test embedding"}
	embeddings, err := client.GetEmbedding(texts, 1024)
	if err != nil {
		t.Fatalf("GetEmbedding failed: %v", err)
	}

	if len(embeddings) != 2 {
		t.Errorf("Expected 2 embeddings, got %d", len(embeddings))
	}

	for i, emb := range embeddings {
		if len(emb) == 0 {
			t.Errorf("Empty embedding for text %d", i)
		} else {
			t.Logf("Embedding %d: %d dimensions, first 5 values: %v...", i, len(emb), emb[:min(5, len(emb))])
		}
	}
}

// TestIntegration_Connection tests basic connectivity
func TestIntegration_Connection(t *testing.T) {
	apiKey := os.Getenv("LLM_API_KEY")
	if apiKey == "" {
		t.Skip("LLM_API_KEY not set, skipping integration test")
	}

	cfg := &config.LLMConfig{
		Provider:    "openai",
		APIKey:      apiKey,
		Model:       getEnvOrDefault("LLM_MODEL", "glm-4-flash"),
		BaseURL:     getEnvOrDefault("LLM_BASE_URL", "https://open.bigmodel.cn/api/paas/v4"),
		MaxTokens:   10,
		Temperature: 0.7,
	}
	client := NewClient(cfg)

	// Simple connectivity test
	_, err := client.Chat([]ChatMessage{{Role: "user", Content: "Hi"}})
	if err != nil {
		t.Errorf("Connection test failed: %v", err)
	} else {
		t.Log("Connection successful!")
	}
}

func getEnvOrDefault(key, defaultVal string) string {
	if val := os.Getenv(key); val != "" {
		return val
	}
	return defaultVal
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
