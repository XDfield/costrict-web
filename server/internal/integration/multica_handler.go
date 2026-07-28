package integration

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"github.com/costrict/costrict-web/server/internal/models"
	"github.com/costrict/costrict-web/server/internal/notification/sender"
)

// MessageTrigger is the slice of notification.NotificationService the handler
// needs; an interface so tests can capture deliveries without a channel setup.
type MessageTrigger interface {
	TriggerMessage(userID, eventType string, msg sender.NotificationMessage)
}

// statusLabels maps Multica issue statuses to the Chinese labels used across
// the other notification messages. Unknown statuses pass through raw.
var statusLabels = map[string]string{
	"backlog":     "Backlog",
	"todo":        "待办",
	"in_progress": "进行中",
	"in_review":   "评审中",
	"done":        "已完成",
	"blocked":     "已阻塞",
	"cancelled":   "已取消",
}

func statusLabel(s string) string {
	if l, ok := statusLabels[s]; ok {
		return l
	}
	return s
}

// MulticaEventsHandler receives status-change envelopes from a Multica server
// and fans them out to the matched users' subscribed channels (WeCom app /
// WeCom group bot / webhook) via the existing notification pipeline.
//
// Auth is the shared HMAC secret, not a user session, so this route must be
// registered OUTSIDE the authenticated groups. Register only when
// MULTICA_INTEGRATION_SECRET is configured.
func MulticaEventsHandler(db *gorm.DB, trigger MessageTrigger, secret string) gin.HandlerFunc {
	return func(c *gin.Context) {
		body, err := io.ReadAll(c.Request.Body)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "read body failed"})
			return
		}
		if !verifySignature(secret, body, c.GetHeader("X-Multica-Signature")) {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "invalid signature"})
			return
		}

		var env Envelope
		if err := json.Unmarshal(body, &env); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid envelope"})
			return
		}
		if env.EventID == "" || env.Type == "" {
			c.JSON(http.StatusBadRequest, gin.H{"error": "event_id and type are required"})
			return
		}

		// Idempotency: first writer wins; retries with the same event_id are
		// ACKed without re-delivery.
		record := IntegrationEvent{
			ID:        uuid.NewString(),
			Source:    "multica",
			EventID:   env.EventID,
			EventType: env.Type,
		}
		result := db.Clauses(clause.OnConflict{DoNothing: true}).Create(&record)
		if result.Error != nil {
			slog.Error("integration: persist event failed", "event_id", env.EventID, "error", result.Error)
			c.JSON(http.StatusInternalServerError, gin.H{"error": "persist failed"})
			return
		}
		if result.RowsAffected == 0 {
			c.JSON(http.StatusOK, gin.H{"duplicate": true})
			return
		}

		// Forward compatibility: acknowledge event types we don't handle yet
		// so the sender doesn't retry pointlessly.
		if env.Type != EventMulticaIssueStatusChanged {
			c.JSON(http.StatusOK, gin.H{"ignored": true})
			return
		}

		msg := buildStatusChangedMessage(env)
		delivered := 0
		for _, email := range env.Recipients {
			var user models.User
			if err := db.Where("email = ?", email).First(&user).Error; err != nil {
				continue // unknown on this side — skip silently
			}
			trigger.TriggerMessage(user.SubjectID, env.Type, msg)
			delivered++
		}
		c.JSON(http.StatusOK, gin.H{"delivered": delivered})
	}
}

// verifySignature checks the "sha256=<hex>" HMAC header in constant time.
func verifySignature(secret string, body []byte, header string) bool {
	if secret == "" || !strings.HasPrefix(header, "sha256=") {
		return false
	}
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	want := mac.Sum(nil)
	got, err := hex.DecodeString(strings.TrimPrefix(header, "sha256="))
	if err != nil {
		return false
	}
	return subtle.ConstantTimeCompare(want, got) == 1
}

// buildStatusChangedMessage renders the envelope as a markdown notification,
// following the style of the other notification messages.
func buildStatusChangedMessage(env Envelope) sender.NotificationMessage {
	title := fmt.Sprintf("%s 状态变更为 %s", env.Issue.Identifier, statusLabel(env.Issue.Status))

	linkText := env.Issue.Identifier
	if env.Issue.Title != "" {
		linkText = fmt.Sprintf("%s %s", env.Issue.Identifier, env.Issue.Title)
	}
	issue := linkText
	if env.Issue.URL != "" {
		issue = fmt.Sprintf("[%s](%s)", linkText, env.Issue.URL)
	}

	bodyParts := []string{fmt.Sprintf("**任务**: %s", issue)}
	if env.Issue.PrevStatus != "" {
		bodyParts = append(bodyParts,
			fmt.Sprintf("**状态**: %s → %s", statusLabel(env.Issue.PrevStatus), statusLabel(env.Issue.Status)))
	} else {
		bodyParts = append(bodyParts, fmt.Sprintf("**状态**: %s", statusLabel(env.Issue.Status)))
	}
	if env.Actor.Name != "" {
		bodyParts = append(bodyParts, fmt.Sprintf("**操作者**: %s", env.Actor.Name))
	}
	if env.Workspace.Name != "" {
		bodyParts = append(bodyParts, fmt.Sprintf("**工作区**: %s", env.Workspace.Name))
	}

	return sender.NotificationMessage{
		Title:     title,
		Body:      strings.Join(bodyParts, "\n"),
		EventType: env.Type,
		Metadata: map[string]any{
			"multica": map[string]any{
				"eventId":     env.EventID,
				"issueId":     env.Issue.ID,
				"identifier":  env.Issue.Identifier,
				"url":         env.Issue.URL,
				"workspaceId": env.Workspace.ID,
			},
		},
	}
}
