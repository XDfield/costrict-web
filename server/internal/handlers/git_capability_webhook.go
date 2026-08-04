package handlers

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"
	"unicode"

	"github.com/costrict/costrict-web/server/internal/gitserver"
	"github.com/costrict/costrict-web/server/internal/models"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// maxGitCapabilityWebhookBytes caps a single Gitea delivery. Push payloads
// contain the commit list and can grow quickly, but the ingress only needs
// repository metadata and the before/after SHAs; 1 MiB is ample headroom.
const maxGitCapabilityWebhookBytes = 1 << 20

// gitServerByIDResolver is intentionally narrower than gitserver.Resolver:
// a webhook is scoped by a Git server route parameter, not a tenant.
type gitServerByIDResolver interface {
	ResolveByServerID(ctx context.Context, serverID string) (*gitserver.Config, error)
}

// GitCapabilityWebhookAPI receives Gitea push webhooks and persists a
// durable sync job. It never updates capability_items in the request path;
// the dedicated worker consumes pending jobs after the HTTP response.
type GitCapabilityWebhookAPI struct {
	DB       *gorm.DB
	Resolver gitServerByIDResolver
}

// NewGitCapabilityWebhookAPI creates the webhook ingress handler. resolver
// must be backed by the same git_servers source of truth as the worker.
func NewGitCapabilityWebhookAPI(db *gorm.DB, resolver gitServerByIDResolver) *GitCapabilityWebhookAPI {
	return &GitCapabilityWebhookAPI{DB: db, Resolver: resolver}
}

// giteaPushWebhookPayload is the strict subset of Gitea's push payload used
// to create a durable sync job. The X-Gitea-Event header is the event-type
// authority; action is intentionally not used for routing.
type giteaPushWebhookPayload struct {
	Ref    string `json:"ref"`
	Before string `json:"before"`
	After  string `json:"after"`
	Repo   struct {
		ID            int64  `json:"id"`
		FullName      string `json:"full_name"`
		DefaultBranch string `json:"default_branch"`
	} `json:"repository"`
}

// ReceiveGiteaPush godoc
// @Summary  Receive a Gitea capability repository push webhook
// @Tags     git-capability-sync
// @Accept   json
// @Produce  json
// @Param    git_server_id       path    string  true  "Configured git_servers.server_id"
// @Param    X-Gitea-Event       header  string  true  "Gitea event type; only push is queued"
// @Param    X-Gitea-Delivery    header  string  true  "Gitea delivery identifier; idempotency key within this git server"
// @Param    X-Gitea-Signature   header  string  true  "HMAC-SHA256 hex over the raw request body"
// @Param    body                body    handlers.giteaPushWebhookPayload  true  "Gitea push payload"
// @Success  202  {object}  object{status=string,job_id=string,duplicate=bool,ignored=bool,reason=string}
// @Failure  400  {object}  object{error=string}
// @Failure  401  {object}  object{error=string}
// @Failure  413  {object}  object{error=string}
// @Failure  500  {object}  object{error=string}
// @Failure  503  {object}  object{error=string}
// @Router   /internal/git-sync/{git_server_id} [post]
func (a *GitCapabilityWebhookAPI) ReceiveGiteaPush(c *gin.Context) {
	if a == nil || a.DB == nil || a.Resolver == nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "git capability webhook unavailable"})
		return
	}

	// Bound body consumption before resolving the server. Otherwise a large
	// request returns 413 only for an enabled server and 401 for an unknown
	// one, turning the route parameter into a configuration-existence probe.
	body, err := io.ReadAll(http.MaxBytesReader(c.Writer, c.Request.Body, maxGitCapabilityWebhookBytes))
	if err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			c.JSON(http.StatusRequestEntityTooLarge, gin.H{"error": "body too large"})
			return
		}
		c.JSON(http.StatusBadRequest, gin.H{"error": "read body failed"})
		return
	}

	serverID := strings.TrimSpace(c.Param("git_server_id"))
	cfg, err := a.Resolver.ResolveByServerID(c.Request.Context(), serverID)
	if err != nil {
		// Do not disclose whether a server exists, is disabled, or lacks a
		// secret. To an untrusted sender all of these are authentication
		// failures.
		if errors.Is(err, gitserver.ErrGitServerNotFound) || errors.Is(err, gitserver.ErrGitServerDisabled) {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "invalid webhook signature"})
			return
		}
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "git server configuration unavailable"})
		return
	}
	if cfg == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "git server configuration unavailable"})
		return
	}
	if strings.TrimSpace(cfg.WebhookSecret) == "" {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "invalid webhook signature"})
		return
	}

	if !verifyGiteaSignature(cfg.WebhookSecret, body, c.GetHeader("X-Gitea-Signature")) {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "invalid webhook signature"})
		return
	}

	// Gitea expects a successful acknowledgement for event types this ingress
	// does not own; otherwise it will keep retrying irrelevant deliveries.
	event := strings.TrimSpace(c.GetHeader("X-Gitea-Event"))
	if event == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "X-Gitea-Event is required"})
		return
	}
	if !strings.EqualFold(event, "push") {
		c.JSON(http.StatusAccepted, gin.H{"status": "ignored", "ignored": true})
		return
	}
	deliveryID := strings.TrimSpace(c.GetHeader("X-Gitea-Delivery"))
	if deliveryID == "" || len(deliveryID) > 128 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "X-Gitea-Delivery is required"})
		return
	}

	var payload giteaPushWebhookPayload
	if err := json.Unmarshal(body, &payload); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid Gitea push payload"})
		return
	}
	if err := payload.validate(); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	if payload.Ref != "refs/heads/"+payload.Repo.DefaultBranch {
		c.JSON(http.StatusAccepted, gin.H{
			"status":  "ignored",
			"ignored": true,
			"reason":  "non_default_branch",
		})
		return
	}
	now := time.Now().UTC()
	job := &models.GitCapabilitySyncJob{
		ID:            uuid.NewString(),
		GitServerID:   cfg.ServerID,
		DeliveryID:    deliveryID,
		RepoID:        payload.Repo.ID,
		RepoFullName:  payload.Repo.FullName,
		DefaultBranch: payload.Repo.DefaultBranch,
		Ref:           payload.Ref,
		BeforeSHA:     payload.Before,
		AfterSHA:      payload.After,
		Status:        models.GitCapabilitySyncJobStatusPending,
		RetryCount:    0,
		MaxAttempts:   3,
		ScheduledAt:   now,
		CreatedAt:     now,
	}
	result := a.DB.WithContext(c.Request.Context()).Clauses(clause.OnConflict{
		Columns: []clause.Column{
			{Name: "git_server_id"},
			{Name: "delivery_id"},
		},
		DoNothing: true,
	}).Create(job)
	if result.Error != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "persist git sync job failed"})
		return
	}
	if result.RowsAffected == 0 {
		c.JSON(http.StatusAccepted, gin.H{"status": "duplicate", "duplicate": true})
		return
	}

	c.JSON(http.StatusAccepted, gin.H{
		"status":    "queued",
		"job_id":    job.ID,
		"duplicate": false,
	})
}

func (p giteaPushWebhookPayload) validate() error {
	if !validWebhookRef(p.Ref) {
		return errors.New("ref is invalid")
	}
	if !validWebhookSHA(p.Before) || !validWebhookSHA(p.After) {
		return errors.New("before and after must be 40-character hexadecimal commit SHAs")
	}
	if p.Repo.ID <= 0 {
		return errors.New("repository.id is required")
	}
	if !validWebhookRepoFullName(p.Repo.FullName) {
		return errors.New("repository.full_name is invalid")
	}
	if !validWebhookBranch(p.Repo.DefaultBranch) {
		return errors.New("repository.default_branch is invalid")
	}
	return nil
}

func validWebhookSHA(value string) bool {
	if len(value) != 40 {
		return false
	}
	for _, r := range value {
		if !((r >= '0' && r <= '9') || (r >= 'a' && r <= 'f') || (r >= 'A' && r <= 'F')) {
			return false
		}
	}
	return true
}

func validWebhookRepoFullName(value string) bool {
	parts := strings.Split(value, "/")
	return len(parts) == 2 && validWebhookRepoComponent(parts[0]) && validWebhookRepoComponent(parts[1])
}

func validWebhookRepoComponent(value string) bool {
	if value == "" || value == "." || value == ".." {
		return false
	}
	for i, r := range value {
		if !((r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '.' || r == '-' || r == '_') {
			return false
		}
		if i == 0 && !(('a' <= r && r <= 'z') || ('A' <= r && r <= 'Z') || ('0' <= r && r <= '9')) {
			return false
		}
	}
	return true
}

func validWebhookBranch(value string) bool {
	return validWebhookRefParts(value)
}

func validWebhookRef(value string) bool {
	return strings.HasPrefix(value, "refs/") && validWebhookRefParts(strings.TrimPrefix(value, "refs/"))
}

func validWebhookRefParts(value string) bool {
	if value == "" || strings.TrimSpace(value) != value || strings.HasPrefix(value, "/") || strings.HasSuffix(value, "/") ||
		strings.Contains(value, "//") || strings.Contains(value, "..") || strings.Contains(value, "@{") || strings.HasSuffix(value, ".lock") {
		return false
	}
	for _, r := range value {
		if unicode.IsControl(r) || unicode.IsSpace(r) || strings.ContainsRune("~^:?*[\\", r) {
			return false
		}
	}
	return true
}

func verifyGiteaSignature(secret string, body []byte, signature string) bool {
	if strings.TrimSpace(secret) == "" {
		return false
	}
	received, err := hex.DecodeString(strings.TrimSpace(signature))
	if err != nil {
		return false
	}
	mac := hmac.New(sha256.New, []byte(secret))
	_, _ = mac.Write(body)
	return hmac.Equal(mac.Sum(nil), received)
}
