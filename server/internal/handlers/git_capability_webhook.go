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

	"github.com/costrict/costrict-web/server/internal/gitcapability"
	"github.com/costrict/costrict-web/server/internal/gitserver"
	"github.com/costrict/costrict-web/server/internal/logger"
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

// Event types this ingress accepts, and nothing else.
//
// The set is closed by MEASUREMENT, not by documentation: every value here was
// observed arriving from the deployed Gitea 1.24.6 during the Phase A fixture
// capture (see the task's research/gitea-lifecycle-fixtures.md and the byte-
// exact payloads in testdata/gitea_lifecycle/). A signed event outside this set
// is acknowledged and audited, never applied.
//
// What is NOT here matters more than what is. Repository RENAME, TRANSFER,
// VISIBILITY change, DEFAULT-BRANCH change and ARCHIVE emit no webhook at all on
// 1.24.6 — verified through both the REST API and the web UI against a system
// hook subscribed to every event type Gitea offers. For those five the periodic
// reconcile is not a fallback, it is the only correctness path, and its
// freshness interval is the convergence bound this platform actually ships.
const (
	giteaEventPush       = "push"
	giteaEventRepository = "repository"
	giteaEventCreate     = "create"
	giteaEventDelete     = "delete"
)

// Actions carried by the `repository` event. `action` is meaningful on this
// event ONLY — push/create/delete have no such field — so routing stays on the
// X-Gitea-Event header and reads `action` afterwards.
const (
	giteaRepositoryActionCreated = "created"
	giteaRepositoryActionDeleted = "deleted"
)

// gitServerByIDResolver is intentionally narrower than gitserver.Resolver:
// a webhook is scoped by a Git server route parameter, not a tenant.
type gitServerByIDResolver interface {
	ResolveByServerID(ctx context.Context, serverID string) (*gitserver.Config, error)
}

// GitCapabilityWebhookAPI receives Gitea lifecycle webhooks and persists a
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

// giteaWebhookPayload is the strict subset of Gitea's payloads this ingress
// reads, across every accepted event type.
//
// Three shapes share one struct because three fields have to be read the same
// way regardless of which event delivered them, and two fields are traps:
//
//   - Ref is fully qualified on `push` (refs/heads/main) and SHORT on
//     `create`/`delete` (main). validWebhookRef demands a refs/ prefix and would
//     reject every create/delete payload, so the two forms are validated apart.
//   - `organization` is deliberately absent from this struct. It is present only
//     on the `repository` event and it is MISNAMED: it carries the repository
//     owner even when that owner is an ordinary user. Reading it as "this repo
//     belongs to an org" is wrong, and it does not exist on push/create/delete
//     at all. Repo.Owner.Login is the only owner path that works uniformly.
type giteaWebhookPayload struct {
	Action  string `json:"action"`   // `repository` only: created | deleted
	Ref     string `json:"ref"`      // push: refs/heads/x — create/delete: x
	RefType string `json:"ref_type"` // create/delete: branch | tag
	Before  string `json:"before"`   // push
	After   string `json:"after"`    // push
	Repo    struct {
		ID            int64  `json:"id"`
		FullName      string `json:"full_name"`
		DefaultBranch string `json:"default_branch"`
		Owner         struct {
			Login string `json:"login"`
		} `json:"owner"`
	} `json:"repository"`
}

// ownerLogin returns the repository owner, preferring the path that exists on
// every event type over the one derived from the mutable display name.
func (p giteaWebhookPayload) ownerLogin() string {
	if login := strings.TrimSpace(p.Repo.Owner.Login); login != "" {
		return login
	}
	return gitcapability.OwnerFromFullName(p.Repo.FullName)
}

// webhookDecision is what the router resolved a delivery into.
type webhookDecision struct {
	queue bool
	// reason names why a delivery was ignored, or which lifecycle signal caused
	// it to be queued. It is returned to Gitea and written to the audit log.
	reason string
	// ref/branch/before/after describe the job to enqueue. Lifecycle triggers
	// leave the SHAs empty: they carry no commit, and a 40-zero AfterSHA would be
	// read by the worker as a default-branch deletion delivery.
	ref    string
	before string
	after  string
}

// ReceiveGiteaEvent godoc
// @Summary  Receive a Gitea capability repository lifecycle webhook
// @Tags     git-capability-sync
// @Accept   json
// @Produce  json
// @Param    git_server_id       path    string  true  "Configured git_servers.server_id"
// @Param    X-Gitea-Event       header  string  true  "Gitea event type; push, repository, create and delete are accepted, anything else is acknowledged and ignored"
// @Param    X-Gitea-Delivery    header  string  true  "Gitea delivery identifier; idempotency key within this git server"
// @Param    X-Gitea-Signature   header  string  true  "HMAC-SHA256 hex over the raw request body"
// @Param    body                body    handlers.giteaWebhookPayload  true  "Gitea event payload"
// @Success  202  {object}  object{status=string,job_id=string,duplicate=bool,ignored=bool,reason=string}
// @Failure  400  {object}  object{error=string}
// @Failure  401  {object}  object{error=string}
// @Failure  413  {object}  object{error=string}
// @Failure  500  {object}  object{error=string}
// @Failure  503  {object}  object{error=string}
// @Router   /internal/git-sync/{git_server_id} [post]
func (a *GitCapabilityWebhookAPI) ReceiveGiteaEvent(c *gin.Context) {
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

	// Unchanged, and deliberately so: X-Gitea-Signature is bare lowercase hex
	// HMAC-SHA256 over the exact raw body bytes on 1.24.6, which is what this
	// already verifies. Widening the event surface changes what is ROUTED, never
	// what is TRUSTED.
	if !verifyGiteaSignature(cfg.WebhookSecret, body, c.GetHeader("X-Gitea-Signature")) {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "invalid webhook signature"})
		return
	}

	event := strings.ToLower(strings.TrimSpace(c.GetHeader("X-Gitea-Event")))
	if event == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "X-Gitea-Event is required"})
		return
	}
	// Gitea expects a successful acknowledgement for event types this ingress
	// does not own; otherwise it will keep retrying irrelevant deliveries.
	switch event {
	case giteaEventPush, giteaEventRepository, giteaEventCreate, giteaEventDelete:
	default:
		a.auditIgnored(cfg.ServerID, event, "", 0, "unsupported_event")
		c.JSON(http.StatusAccepted, gin.H{"status": "ignored", "ignored": true, "reason": "unsupported_event"})
		return
	}

	deliveryID := strings.TrimSpace(c.GetHeader("X-Gitea-Delivery"))
	if deliveryID == "" || len(deliveryID) > 128 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "X-Gitea-Delivery is required"})
		return
	}

	var payload giteaWebhookPayload
	if err := json.Unmarshal(body, &payload); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid Gitea webhook payload"})
		return
	}
	if err := payload.validateIdentity(); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	decision, err := a.route(c.Request.Context(), cfg, event, payload)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if !decision.queue {
		a.auditIgnored(cfg.ServerID, event, payload.Repo.FullName, payload.Repo.ID, decision.reason)
		c.JSON(http.StatusAccepted, gin.H{"status": "ignored", "ignored": true, "reason": decision.reason})
		return
	}

	// Owner exclusion is applied after routing so it reads identically for every
	// event type: a mirror namespace is skipped unless something is already bound
	// to that repository, in which case its lifecycle still has to converge.
	if gitcapability.DiscoveryOwnerExcluded(payload.ownerLogin()) {
		var bound int64
		if err := a.DB.WithContext(c.Request.Context()).Model(&models.CapabilityItem{}).
			Where("content_backend = ? AND source_git_server_id = ? AND source_git_repo_id = ?",
				models.ContentBackendGit, cfg.ServerID, payload.Repo.ID).
			Count(&bound).Error; err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "check git capability binding failed"})
			return
		}
		if bound == 0 {
			a.auditIgnored(cfg.ServerID, event, payload.Repo.FullName, payload.Repo.ID, "discovery_owner_excluded")
			c.JSON(http.StatusAccepted, gin.H{
				"status": "ignored", "ignored": true, "reason": "discovery_owner_excluded",
			})
			return
		}
	}

	now := time.Now().UTC()
	job := &models.GitCapabilitySyncJob{
		ID:          uuid.NewString(),
		GitServerID: cfg.ServerID,
		DeliveryID:  deliveryID,
		RepoID:      payload.Repo.ID,
		// repo_full_name and default_branch are stale display labels from the
		// moment they are written — rename and transfer are silent on 1.24.6 — so
		// the worker resolves current state by (git_server_id, numeric repo id)
		// before it applies anything. They are kept for operator readability only.
		RepoFullName:  payload.Repo.FullName,
		DefaultBranch: payload.Repo.DefaultBranch,
		Ref:           decision.ref,
		BeforeSHA:     decision.before,
		AfterSHA:      decision.after,
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
		"reason":    decision.reason,
	})
}

// route decides what one authenticated delivery means. It never mutates
// capability state — a webhook is a latency trigger, and the worker re-reads
// current Gitea state before it applies anything, which is what makes a delayed,
// duplicated or reordered delivery converge instead of replaying stale state.
func (a *GitCapabilityWebhookAPI) route(
	ctx context.Context, cfg *gitserver.Config, event string, payload giteaWebhookPayload,
) (webhookDecision, error) {
	switch event {
	case giteaEventPush:
		return payload.routePush()
	case giteaEventRepository:
		return payload.routeRepository()
	case giteaEventCreate, giteaEventDelete:
		return a.routeRefChange(ctx, cfg, event, payload)
	}
	return webhookDecision{reason: "unsupported_event"}, nil
}

func (p giteaWebhookPayload) routePush() (webhookDecision, error) {
	if !validWebhookRef(p.Ref) {
		return webhookDecision{}, errors.New("ref is invalid")
	}
	if !validWebhookSHA(p.Before) || !validWebhookSHA(p.After) {
		return webhookDecision{}, errors.New("before and after must be 40-character hexadecimal commit SHAs")
	}
	if !validWebhookBranch(p.Repo.DefaultBranch) {
		return webhookDecision{}, errors.New("repository.default_branch is invalid")
	}
	if p.Ref != "refs/heads/"+p.Repo.DefaultBranch {
		return webhookDecision{reason: "non_default_branch"}, nil
	}
	return webhookDecision{queue: true, reason: "default_branch_push", ref: p.Ref, before: p.Before, after: p.After}, nil
}

// routeRepository handles the only lifecycle event 1.24.6 actually emits.
//
// `deleted` is the whole point of widening this ingress, and it reaches SYSTEM
// webhooks only — a repository-level hook is deleted together with its
// repository and therefore observes nothing. If the production hook is ever
// re-created as a repository-level or "default" hook, deletion detection
// silently disappears and only reconcile is left.
//
// `created` is acknowledged without a job on purpose. A repository that has just
// been created has no commit on its default branch, so a convergence job could
// only fail and retry until it exhausted its attempts; and onboarding creates
// the capability binding explicitly rather than discovering an unbound
// repository as a fallback.
func (p giteaWebhookPayload) routeRepository() (webhookDecision, error) {
	action := strings.ToLower(strings.TrimSpace(p.Action))
	switch action {
	case giteaRepositoryActionDeleted:
		return webhookDecision{queue: true, reason: "repository_deleted", ref: p.Repo.DefaultBranch}, nil
	case giteaRepositoryActionCreated:
		return webhookDecision{reason: "repository_created"}, nil
	default:
		return webhookDecision{reason: "unsupported_repository_action"}, nil
	}
}

// routeRefChange turns a branch create/delete into the one thing it is good
// for: evidence that this platform's stored default branch may be wrong.
//
// Deleting the CURRENT default branch is not reachable on 1.24.6 — the REST API
// answers 403 and `git push origin :main` is declined by the pre-receive hook.
// So a `delete` whose ref equals the branch we believe is the default is proof
// that the default was already moved WITHOUT NOTICE (changing it emits nothing),
// and the correct response is to re-read the repository, never to archive its
// capabilities.
//
// The same payload also carries the repository's CURRENT default branch, which
// makes these two events the only push-time opportunity to notice an otherwise
// silent default-branch change before the next reconcile.
func (a *GitCapabilityWebhookAPI) routeRefChange(
	ctx context.Context, cfg *gitserver.Config, event string, payload giteaWebhookPayload,
) (webhookDecision, error) {
	// Short-form ref, NOT refs/heads/... — see giteaWebhookPayload.
	if !validWebhookBranch(payload.Ref) {
		return webhookDecision{}, errors.New("ref is invalid")
	}
	if refType := strings.ToLower(strings.TrimSpace(payload.RefType)); refType != "branch" {
		return webhookDecision{reason: "unsupported_ref_type"}, nil
	}
	if payload.Repo.DefaultBranch != "" && !validWebhookBranch(payload.Repo.DefaultBranch) {
		return webhookDecision{}, errors.New("repository.default_branch is invalid")
	}

	var binding models.GitCapabilityRepository
	err := a.DB.WithContext(ctx).
		Select("id", "default_branch").
		Where("git_server_id = ? AND git_repo_id = ?", cfg.ServerID, payload.Repo.ID).
		First(&binding).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		// No binding means nothing to converge, and an unbound repository is
		// deliberately not discovered off a ref event (LH-4).
		return webhookDecision{reason: "unbound_repository"}, nil
	}
	if err != nil {
		return webhookDecision{}, errors.New("load git capability binding failed")
	}

	stored := strings.TrimSpace(binding.DefaultBranch)
	switch {
	case event == giteaEventDelete && stored != "" && payload.Ref == stored:
		return webhookDecision{queue: true, reason: "stored_default_branch_deleted", ref: stored}, nil
	case payload.Repo.DefaultBranch != "" && stored != "" && payload.Repo.DefaultBranch != stored:
		return webhookDecision{queue: true, reason: "default_branch_changed", ref: payload.Repo.DefaultBranch}, nil
	case event == giteaEventCreate && payload.Repo.DefaultBranch != "" && payload.Ref == payload.Repo.DefaultBranch:
		// The default branch exists again; a row archived as
		// default_branch_missing can recover without waiting for reconcile.
		return webhookDecision{queue: true, reason: "default_branch_created", ref: payload.Ref}, nil
	}
	return webhookDecision{reason: "non_default_branch"}, nil
}

// auditIgnored records every signed delivery this ingress declined to act on.
// An unsupported or unroutable event must leave an operator trail precisely
// because it changes no state: without the log, "Gitea started emitting X and we
// silently dropped it for a month" is indistinguishable from "X never arrived".
func (a *GitCapabilityWebhookAPI) auditIgnored(serverID, event, fullName string, repoID int64, reason string) {
	logger.Info("Git capability webhook ignored serverID=%s event=%s repoID=%d repo=%s reason=%s",
		serverID, event, repoID, fullName, reason)
}

// validateIdentity checks the fields every accepted event must carry. Per-event
// validation (ref form, commit SHAs) belongs to the routers, because the same
// field is legal in different shapes depending on the event.
func (p giteaWebhookPayload) validateIdentity() error {
	if p.Repo.ID <= 0 {
		return errors.New("repository.id is required")
	}
	if !validWebhookRepoFullName(p.Repo.FullName) {
		return errors.New("repository.full_name is invalid")
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
