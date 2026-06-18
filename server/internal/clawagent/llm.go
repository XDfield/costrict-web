package clawagent

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// LLMClient handles communication with OpenAI-compatible LLM APIs.
type LLMClient struct {
	httpClient *http.Client
}

// ChatMessage represents a message in the chat completion request.
type ChatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// ChatCompletionRequest is the request body for chat completions.
type ChatCompletionRequest struct {
	Model       string        `json:"model"`
	Messages    []ChatMessage `json:"messages"`
	Temperature float64       `json:"temperature,omitempty"`
	MaxTokens   int           `json:"max_tokens,omitempty"`
	Stream      bool          `json:"stream,omitempty"`
}

// ChatCompletionResponse is the response from chat completions.
type ChatCompletionResponse struct {
	Choices []struct {
		Index        int `json:"index"`
		Message      ChatMessage `json:"message"`
		FinishReason string      `json:"finish_reason"`
	} `json:"choices"`
	Usage struct {
		PromptTokens     int `json:"prompt_tokens"`
		CompletionTokens int `json:"completion_tokens"`
		TotalTokens      int `json:"total_tokens"`
	} `json:"usage"`
}

// StreamEvent represents a single streaming response chunk.
type StreamEvent struct {
	Choices []struct {
		Delta struct {
			Content string `json:"content"`
		} `json:"delta"`
		FinishReason string `json:"finish_reason"`
	} `json:"choices"`
}

// SSEMessage represents a server-sent event.
type SSEMessage struct {
	Event string
	Data  string
}

// NewLLMClient creates a new LLM client.
func NewLLMClient() *LLMClient {
	return &LLMClient{
		httpClient: &http.Client{
			Timeout: 180 * time.Second,
		},
	}
}

// Generate sends a non-streaming completion request.
func (c *LLMClient) Generate(ctx context.Context, cfg ProviderConfig, messages []ChatMessage) (*ChatCompletionResponse, error) {
	req := ChatCompletionRequest{
		Model:    cfg.ModelName,
		Messages: messages,
	}

	body, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("marshal request: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, "POST", cfg.BaseURL+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Authorization", "Bearer "+cfg.APIKey)

	resp, err := c.httpClient.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("http request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("API error (status %d): %s", resp.StatusCode, string(respBody))
	}

	var result ChatCompletionResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("decode response: %w", err)
	}
	return &result, nil
}

// GenerateStream sends a streaming completion request and returns a channel of stream events.
func (c *LLMClient) GenerateStream(ctx context.Context, cfg ProviderConfig, messages []ChatMessage) (<-chan StreamEvent, <-chan error) {
	eventCh := make(chan StreamEvent, 64)
	errCh := make(chan error, 1)

	go func() {
		defer close(eventCh)
		defer close(errCh)

		req := ChatCompletionRequest{
			Model:    cfg.ModelName,
			Messages: messages,
			Stream:   true,
		}

		body, err := json.Marshal(req)
		if err != nil {
			errCh <- fmt.Errorf("marshal request: %w", err)
			return
		}

		httpReq, err := http.NewRequestWithContext(ctx, "POST", cfg.BaseURL+"/chat/completions", bytes.NewReader(body))
		if err != nil {
			errCh <- fmt.Errorf("create request: %w", err)
			return
		}
		httpReq.Header.Set("Content-Type", "application/json")
		httpReq.Header.Set("Authorization", "Bearer "+cfg.APIKey)

		resp, err := c.httpClient.Do(httpReq)
		if err != nil {
			errCh <- fmt.Errorf("http request: %w", err)
			return
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusOK {
			respBody, _ := io.ReadAll(resp.Body)
			errCh <- fmt.Errorf("API error (status %d): %s", resp.StatusCode, string(respBody))
			return
		}

		scanner := NewSSEScanner(resp.Body)
		for scanner.Scan() {
			msg := scanner.Message()
			if msg == nil {
				continue
			}
			if msg.Data == "[DONE]" {
				return
			}
			var evt StreamEvent
			if err := json.Unmarshal([]byte(msg.Data), &evt); err != nil {
				continue
			}
			select {
			case eventCh <- evt:
			case <-ctx.Done():
				return
			}
		}

		if err := scanner.Err(); err != nil {
			errCh <- fmt.Errorf("scan SSE: %w", err)
		}
	}()

	return eventCh, errCh
}

// SSEParser parses Server-Sent Events from a stream.
type SSEScanner struct {
	reader   *bufReader
	current  *SSEMessage
	err      error
	done     bool
}

type bufReader struct {
	reader io.Reader
	buf    []byte
	pos    int
	end    int
}

func newBufReader(r io.Reader, size int) *bufReader {
	return &bufReader{
		reader: r,
		buf:    make([]byte, size),
	}
}

func (b *bufReader) ReadLine() (string, error) {
	for {
		// Scan for newline in buffer
		for i := b.pos; i < b.end; i++ {
			if b.buf[i] == '\n' {
				line := string(b.buf[b.pos:i])
				// Skip \r if present
				if len(line) > 0 && line[len(line)-1] == '\r' {
					line = line[:len(line)-1]
				}
				b.pos = i + 1
				return line, nil
			}
		}

		if b.pos > 0 && b.pos < b.end {
			// Move remaining data to front
			copy(b.buf, b.buf[b.pos:b.end])
			b.end -= b.pos
			b.pos = 0
		} else if b.pos >= b.end {
			b.pos = 0
			b.end = 0
		}

		n, err := b.reader.Read(b.buf[b.end:])
		if err != nil {
			return "", err
		}
		b.end += n
	}
}

// NewSSEScanner creates a new SSE scanner.
func NewSSEScanner(r io.Reader) *SSEScanner {
	return &SSEScanner{
		reader: newBufReader(r, 4096),
	}
}

// Scan advances to the next SSE message.
func (s *SSEScanner) Scan() bool {
	if s.done {
		return false
	}

	var msg SSEMessage
	for {
		line, err := s.reader.ReadLine()
		if err != nil {
			if err == io.EOF {
				s.done = true
				if msg.Data != "" {
					s.current = &msg
					return true
				}
				return false
			}
			s.err = err
			return false
		}

		if line == "" {
			// Empty line = end of message
			if msg.Data != "" {
				s.current = &msg
				return true
			}
			continue
		}

		if strings.HasPrefix(line, "data: ") {
			msg.Data += strings.TrimPrefix(line, "data: ")
		} else if strings.HasPrefix(line, "event: ") {
			msg.Event = strings.TrimPrefix(line, "event: ")
		}
	}
}

// Message returns the last parsed SSE message.
func (s *SSEScanner) Message() *SSEMessage {
	return s.current
}

// Err returns any error encountered during scanning.
func (s *SSEScanner) Err() error {
	return s.err
}

// ProviderConfig holds resolved provider configuration for LLM calls.
type ProviderConfig struct {
	ProviderType string
	APIKey       string
	BaseURL      string
	ModelName    string
}
