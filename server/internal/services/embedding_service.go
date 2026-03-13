package services

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/costrict/costrict-web/server/internal/config"
)

// EmbeddingService handles text vectorization
type EmbeddingService struct {
	cfg        *config.EmbeddingConfig
	httpClient *http.Client
}

// NewEmbeddingService creates a new embedding service
func NewEmbeddingService(cfg *config.EmbeddingConfig) *EmbeddingService {
	return &EmbeddingService{
		cfg: cfg,
		httpClient: &http.Client{
			Timeout: 60 * time.Second,
		},
	}
}

// EmbeddingRequest represents an embedding API request
type EmbeddingRequest struct {
	Model      string   `json:"model"`
	Input      []string `json:"input"`
	Dimensions int      `json:"dimensions,omitempty"`
}

// EmbeddingResponse represents an embedding API response
type EmbeddingResponse struct {
	Object string `json:"object"`
	Data   []struct {
		Object    string    `json:"object"`
		Index     int       `json:"index"`
		Embedding []float64 `json:"embedding"`
	} `json:"data"`
	Model string `json:"model"`
	Usage struct {
		PromptTokens int `json:"prompt_tokens"`
		TotalTokens  int `json:"total_tokens"`
	} `json:"usage"`
}

// GetEmbedding returns embeddings for the given texts
func (s *EmbeddingService) GetEmbedding(ctx context.Context, texts []string) ([][]float64, error) {
	if len(texts) == 0 {
		return nil, nil
	}

	// Handle mock provider for testing
	if s.cfg.Provider == "mock" {
		embeddings := make([][]float64, len(texts))
		for i := range texts {
			// Generate deterministic mock embedding based on text length
			embedding := make([]float64, s.cfg.Dimensions)
			for j := range embedding {
				embedding[j] = float64(len(texts[i])%100) / 100.0 * float64(j%10) / 10.0
			}
			embeddings[i] = embedding
		}
		return embeddings, nil
	}

	req := EmbeddingRequest{
		Model:      s.cfg.Model,
		Input:      texts,
		Dimensions: s.cfg.Dimensions,
	}

	body, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal request: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, "POST", s.cfg.BaseURL+"/embeddings", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}

	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Authorization", "Bearer "+s.cfg.APIKey)

	resp, err := s.httpClient.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("failed to send request: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to read response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("API error: %s - %s", resp.Status, string(respBody))
	}

	var embResp EmbeddingResponse
	if err := json.Unmarshal(respBody, &embResp); err != nil {
		return nil, fmt.Errorf("failed to parse response: %w", err)
	}

	embeddings := make([][]float64, len(embResp.Data))
	for _, d := range embResp.Data {
		embeddings[d.Index] = d.Embedding
	}

	return embeddings, nil
}

// GetSingleEmbedding returns embedding for a single text
func (s *EmbeddingService) GetSingleEmbedding(ctx context.Context, text string) ([]float64, error) {
	embeddings, err := s.GetEmbedding(ctx, []string{text})
	if err != nil {
		return nil, err
	}
	if len(embeddings) == 0 {
		return nil, fmt.Errorf("no embedding returned")
	}
	return embeddings[0], nil
}

// EmbedItem generates embedding for a capability item
func (s *EmbeddingService) EmbedItem(ctx context.Context, name, description, content, itemType string) ([]float64, error) {
	// Combine relevant fields into a single text for embedding
	text := s.buildItemText(name, description, content, itemType)
	return s.GetSingleEmbedding(ctx, text)
}

// buildItemText builds the text to embed from item fields
func (s *EmbeddingService) buildItemText(name, description, content, itemType string) string {
	var parts []string

	if name != "" {
		parts = append(parts, fmt.Sprintf("Name: %s", name))
	}

	if description != "" {
		parts = append(parts, fmt.Sprintf("Description: %s", description))
	}

	if itemType != "" {
		parts = append(parts, fmt.Sprintf("Type: %s", itemType))
	}

	// Include content but truncate if too long
	if content != "" {
		maxContentLen := 2000
		if len(content) > maxContentLen {
			content = content[:maxContentLen] + "..."
		}
		parts = append(parts, fmt.Sprintf("Content: %s", content))
	}

	return strings.Join(parts, "\n")
}

// FormatVectorForDB formats a vector for PostgreSQL storage
func FormatVectorForDB(embedding []float64) string {
	if embedding == nil || len(embedding) == 0 {
		return "[]"
	}

	parts := make([]string, len(embedding))
	for i, val := range embedding {
		parts[i] = fmt.Sprintf("%f", val)
	}
	return "[" + strings.Join(parts, ",") + "]"
}
