package clawagent

import (
	"fmt"
	"net/http"
	"io"
	"strings"

	"github.com/costrict/costrict-web/server/internal/config"
	"github.com/costrict/costrict-web/server/internal/gateway"
	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
)

// NewFromMain creates a new ClawAgentRuntime from main.go components.
// This handles the wiring that main.go needs.
func NewFromMain(
	db *gorm.DB,
	cfg *config.Config,
	gwRegistry *gateway.GatewayRegistry,
	gwClient *gateway.Client,
) (*ClawAgentRuntime, error) {
	if cfg.ClawAgent.EncryptionKey == "" {
		return nil, fmt.Errorf("CLAWAGENT_ENCRYPTION_KEY is required")
	}

	return New(db, cfg, gwRegistry, gwClient)
}

// SetupOpenAIHandler sets up the OpenAI-compatible API endpoint.
func (rt *ClawAgentRuntime) SetupOpenAIHandler(r *gin.RouterGroup, authMiddleware gin.HandlerFunc) {
	openaiGroup := r.Group("/v1")
	openaiGroup.Use(authMiddleware)
	openaiGroup.Any("/chat/completions", rt.openAIHandler())
}

func (rt *ClawAgentRuntime) openAIHandler() gin.HandlerFunc {
	return func(c *gin.Context) {
		userID := rt.resolveUserID(c)
		if userID == "" {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "unauthorized"})
			return
		}

		var req struct {
			Model    string `json:"model"`
			Messages []struct {
				Role    string `json:"role"`
				Content string `json:"content"`
			} `json:"messages"`
			Stream bool `json:"stream"`
		}
		if err := c.ShouldBindJSON(&req); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}

		if len(req.Messages) == 0 {
			c.JSON(http.StatusBadRequest, gin.H{"error": "no messages"})
			return
		}

		lastMsg := req.Messages[len(req.Messages)-1].Content

		baseKey := fmt.Sprintf("agent:clawagent:openai:%s:%s", c.ClientIP(), userID)
		sessionID, err := rt.resolveActiveSession(userID, baseKey, "direct")
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}

		eventCh, err := rt.runner.Run(c.Request.Context(), userID, sessionID, lastMsg)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}

		if req.Stream {
			c.Header("Content-Type", "text/event-stream")
			c.Header("Cache-Control", "no-cache")
			c.Header("Connection", "keep-alive")

			c.Stream(func(w io.Writer) bool {
				for evt := range eventCh {
					if evt.IsFinal {
						c.SSEvent("", "[DONE]")
						return false
					}
					if evt.Content != "" {
						c.SSEvent("", fmt.Sprintf(`{"choices":[{"delta":{"content":%q},"index":0}]}`, evt.Content))
					}
				}
				return false
			})
		} else {
			var fullContent string
			for evt := range eventCh {
				fullContent += evt.Content
				if evt.IsFinal {
					break
				}
			}

			content := strings.TrimSpace(fullContent)
			c.JSON(http.StatusOK, gin.H{
				"id":      "chatcmpl-" + uuidString(),
				"object":  "chat.completion",
				"model":   req.Model,
				"choices": []gin.H{{"index": 0, "message": gin.H{"role": "assistant", "content": content}}},
			})
		}
	}
}
