package integration

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
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

// maxEnvelopeBytes caps the inbound body. An envelope carries one issue
// status change; 1MB is generous headroom and bounds memory use from a
// misbehaving (or compromised) sender.
const maxEnvelopeBytes = 1 << 20

// MulticaEventsHandler receives status-change envelopes from a Multica server
// and fans them out to the matched users' subscribed channels (WeCom app /
// WeCom group bot / webhook) via the existing notification pipeline.
//
// Auth is the shared HMAC secret, not a user session, so this route must be
// registered OUTSIDE the authenticated groups. Register only when
// MULTICA_INTEGRATION_SECRET is configured.
//
// Delivery semantics are at-most-once: TriggerMessage is fire-and-forget, so
// a crash after queueing cannot be recovered. Transient failures BEFORE
// queueing (recipient lookup) roll back the idempotency record and return
// 500, letting the sender's retry reprocess the event.
func MulticaEventsHandler(db *gorm.DB, trigger MessageTrigger, secret string) gin.HandlerFunc {
	return func(c *gin.Context) {
		body, err := io.ReadAll(http.MaxBytesReader(c.Writer, c.Request.Body, maxEnvelopeBytes))
		if err != nil {
			var maxErr *http.MaxBytesError
			if errors.As(err, &maxErr) {
				c.JSON(http.StatusRequestEntityTooLarge, gin.H{"error": "body too large"})
			} else {
				c.JSON(http.StatusBadRequest, gin.H{"error": "read body failed"})
			}
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
		if env.Version != 1 {
			// Unknown contract version: fail fast. The sender does not retry
			// 4xx, and silently parsing a newer envelope as v1 could
			// misinterpret its semantics.
			c.JSON(http.StatusBadRequest, gin.H{"error": "unsupported envelope version"})
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
		queued := 0
		if emails := normalizedEmails(env.Recipients); len(emails) > 0 {
			// One batch lookup for all recipients. Email match is
			// case-insensitive; banned/disabled users are excluded. GORM
			// also filters soft-deleted rows by default.
			var users []models.User
			if err := db.Where("LOWER(email) IN ? AND is_active = ? AND status = ?", emails, true, "active").
				Find(&users).Error; err != nil {
				// Roll back the idempotency record so the sender's retry
				// reprocesses the event instead of being ACKed as a
				// duplicate.
				if delErr := db.Delete(&record).Error; delErr != nil {
					slog.Error("integration: rollback idempotency record failed",
						"event_id", env.EventID, "error", delErr)
				}
				slog.Error("integration: recipient lookup failed", "event_id", env.EventID, "error", err)
				c.JSON(http.StatusInternalServerError, gin.H{"error": "recipient lookup failed"})
				return
			}
			for _, u := range users {
				trigger.TriggerMessage(u.SubjectID, env.Type, msg)
				queued++
			}
		}
		c.JSON(http.StatusOK, gin.H{"queued": queued})
	}
}

// normalizedEmails lowercases, trims, and dedupes recipient emails for the
// case-insensitive batch lookup.
func normalizedEmails(recipients []string) []string {
	seen := make(map[string]struct{}, len(recipients))
	out := make([]string, 0, len(recipients))
	for _, e := range recipients {
		e = strings.ToLower(strings.TrimSpace(e))
		if e == "" {
			continue
		}
		if _, dup := seen[e]; dup {
			continue
		}
		seen[e] = struct{}{}
		out = append(out, e)
	}
	return out
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
