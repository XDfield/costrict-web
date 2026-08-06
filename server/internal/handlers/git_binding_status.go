// Git binding status endpoint (Gitea fork integration FI-2).
//
// This is the endpoint the CoStrict Gitea fork calls from its CoStrictJWT
// authentication method before it will admit a JWT-bearing user. The fork side
// is `checkBinding` in services/auth/jwt.go; every property below is fixed by
// THAT code, not chosen here:
//
//   - GET, with NO headers of any kind. The fork sends no Authorization, no
//     token, no signature. The endpoint is therefore unauthenticated by
//     protocol necessity and must be safe when publicly reachable.
//   - The URL is url.JoinPath(BindingCheckURL, PathEscape(user_id)) — the fork
//     appends exactly one path segment and never identifies itself, so the Git
//     server id is baked into the configured prefix to give us a discriminator:
//
//     app.ini  BINDING_CHECK_URL = https://host/api/internal/git-binding/status/<git_server_id>
//     request  GET .../api/internal/git-binding/status/<git_server_id>/<user_subject_id>
//
//   - The response body is decoded into a struct with a SINGLE field,
//     `sync_status`. Treat that shape as frozen public surface: no username, no
//     uid, no email, no tenant, no timestamps, no error detail. Ever.
//   - The admit value is the byte-exact, case-sensitive string "synced". The
//     fork compares with ==; "Synced" and "SYNCED" are rejections.
//   - The fork rejects on ANY non-200 identically (→ the user sees 503 with
//     Retry-After: 5). A 404 therefore buys no behavioural difference over a
//     200 carrying a non-synced status — while 404-vs-200 on user input would
//     hand an unauthenticated caller a free user-existence oracle. Hence the
//     deliberate asymmetry below: unknown USER is 200/pending, unknown SERVER
//     (configuration, not user data) is 404.
//   - The fork's client timeout is 3 seconds and it fails CLOSED: a timeout,
//     a connection error or a decode error all reject the user. This handler is
//     consequently an indexed read and nothing else — no outbound calls, no
//     Gitea probe, no writes.
//
// Deliberately NOT done here: verifying that the Git account named by the
// binding still exists upstream. This endpoint is a mirror of
// user_git_binding.sync_status, not a validator. A live Gitea probe would blow
// the 3s budget and couple our availability to the very server asking us. A row
// that claims "synced" while its Git account has been deleted is a data defect
// to be repaired in the binding table (the fork then answers with its own
// distinct 503, "gitea account not yet provisioned").

package handlers

import (
	"errors"
	"net/http"
	"strings"

	"github.com/costrict/costrict-web/server/internal/logger"
	"github.com/costrict/costrict-web/server/internal/models"
	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
)

// gitBindingStatusResponse is the ENTIRE response body of a successful binding
// status lookup.
//
// It is a named struct rather than a gin.H literal on purpose: the one-field
// contract is then enforced by the type at every call site, instead of relying
// on each future edit to remember not to add a "helpful" debugging field.
type gitBindingStatusResponse struct {
	SyncStatus string `json:"sync_status"`
}

// GitBindingStatusAPI serves the Git fork's binding pre-check.
type GitBindingStatusAPI struct {
	DB *gorm.DB
}

// NewGitBindingStatusAPI binds the handler to the supplied pool.
func NewGitBindingStatusAPI(db *gorm.DB) *GitBindingStatusAPI {
	return &GitBindingStatusAPI{DB: db}
}

// GetBindingStatus reports whether a user's Git account provisioning has
// completed, for the Git server identified by the route prefix.
//
// Responses:
//
//	200 {"sync_status":"synced"}   — provisioned; the fork admits the user
//	200 {"sync_status":"pending"}  — not provisioned yet, OR no binding row at
//	                                 all (unknown user). Indistinguishable on
//	                                 purpose: see the package comment.
//	200 {"sync_status":"error"}    — provisioning failed for this user
//	404                            — unknown or disabled git_server_id. This is
//	                                 a configuration fault, not user data, so it
//	                                 is loud.
//	500                            — database fault. Fail closed: never
//	                                 synthesise "pending" to paper over an
//	                                 outage, and never synthesise "synced".
//
// @Summary  Report a user's Git account binding status
// @Tags     internal
// @Produce  json
// @Param    git_server_id     path  string  true  "Git server id (baked into the fork's BINDING_CHECK_URL)"
// @Param    user_subject_id   path  string  true  "cs-user subject id (the JWT user_id claim)"
// @Success  200  {object}  gitBindingStatusResponse
// @Failure  404  {object}  map[string]string
// @Failure  500  {object}  map[string]string
// @Router   /internal/git-binding/status/{git_server_id}/{user_subject_id} [get]
func (a *GitBindingStatusAPI) GetBindingStatus(c *gin.Context) {
	if a == nil || a.DB == nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "binding status unavailable"})
		return
	}

	// An intermediary caching a "pending" would pin a user out for the life of
	// that cache entry, on top of the fork's own 30s in-process cache. The fork
	// ignores HTTP caching semantics, so this costs nothing and removes a
	// failure mode we cannot see from here.
	c.Header("Cache-Control", "no-store")

	serverID := strings.TrimSpace(c.Param("git_server_id"))
	subjectID := strings.TrimSpace(c.Param("user_subject_id"))

	ctx := c.Request.Context()

	// Selective read: this endpoint needs the server's kind and whether it is in
	// service, and must not pull its credentials into a request that carries no
	// authentication. (That is also why gitserver.DBResolver is not used here —
	// it loads and validates admin_token, so a server with no admin token would
	// fail this lookup for a reason that has nothing to do with the question
	// being asked.)
	var server models.GitServer
	err := a.DB.WithContext(ctx).
		Select("server_id, kind, enabled").
		First(&server, "server_id = ?", serverID).Error
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			// Deliberately loud. A Git server asking about a server id we have
			// never heard of means BINDING_CHECK_URL is misconfigured, and every
			// user of that instance is about to be locked out; a 200 would hide
			// that behind an ordinary-looking "pending".
			logger.Warn("[git-binding-status] unknown git_server_id=%q", serverID)
			c.JSON(http.StatusNotFound, gin.H{"error": "unknown git server"})
			return
		}
		logger.Error("[git-binding-status] load git server %q failed: %v", serverID, err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "binding status unavailable"})
		return
	}
	if !server.Enabled {
		// A disabled server is out of service; declining to vouch for its users
		// is the fail-closed reading, and it is the same class of answer as an
		// unknown id (configuration, not user state).
		logger.Warn("[git-binding-status] git_server_id=%q is disabled", serverID)
		c.JSON(http.StatusNotFound, gin.H{"error": "unknown git server"})
		return
	}

	// The fork identifies the user but not the tenant, so the tenant comes from
	// whichever tenants are bound to this Git server. Normally exactly one; the
	// slice keeps a many-tenants-one-server deployment correct instead of
	// silently answering for an arbitrary tenant.
	var tenantIDs []string
	if err := a.DB.WithContext(ctx).
		Model(&models.TenantGitServerBinding{}).
		Where("git_server_id = ?", serverID).
		Order("tenant_id").
		Pluck("tenant_id", &tenantIDs).Error; err != nil {
		logger.Error("[git-binding-status] load tenant bindings for %q failed: %v", serverID, err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "binding status unavailable"})
		return
	}
	if len(tenantIDs) == 0 {
		// The server row exists but nothing is bound to it, so no user binding
		// can belong to it. Not an error to the caller — but it is a
		// configuration smell worth a line in the log.
		logger.Warn("[git-binding-status] git_server_id=%q has no tenant binding", serverID)
		writeGitBindingStatus(c, models.GitSyncStatusPending)
		return
	}

	if subjectID == "" {
		writeGitBindingStatus(c, models.GitSyncStatusPending)
		return
	}

	// Primary-key prefix scan on user_git_binding (user_subject_id, tenant_id):
	// a handful of rows at most, well inside the fork's 3s budget.
	var statuses []string
	if err := a.DB.WithContext(ctx).
		Model(&models.UserGitBinding{}).
		Where("user_subject_id = ? AND tenant_id IN ? AND provider_kind = ?",
			subjectID, tenantIDs, server.Kind).
		Order("tenant_id").
		Pluck("sync_status", &statuses).Error; err != nil {
		// No subject id in the message: this endpoint takes an opaque user
		// identifier from an unauthenticated caller and should not scatter it
		// through the logs.
		logger.Error("[git-binding-status] load user binding on %q failed: %v", serverID, err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "binding status unavailable"})
		return
	}

	writeGitBindingStatus(c, resolveGitBindingStatus(statuses))
}

// resolveGitBindingStatus collapses the rows found for one user into the single
// status the fork asked for.
//
// No rows → "pending", which is also the answer for a user who does not exist:
// the fork rejects both identically, and keeping them indistinguishable denies
// an unauthenticated caller a user-existence oracle.
//
// With rows, "synced" wins over anything else: the user demonstrably has a
// working account on this Git server through at least one tenant, and the fork's
// question is precisely "may this account in". Otherwise the first row in
// tenant order is reported verbatim — this endpoint mirrors stored state, it
// does not normalise or reinterpret it.
func resolveGitBindingStatus(statuses []string) string {
	first := ""
	for _, s := range statuses {
		if s == models.GitSyncStatusSynced {
			return models.GitSyncStatusSynced
		}
		if first == "" {
			first = s
		}
	}
	if first == "" {
		return models.GitSyncStatusPending
	}
	return first
}

// writeGitBindingStatus emits the one-field body. Every 200 goes through here.
func writeGitBindingStatus(c *gin.Context, status string) {
	c.JSON(http.StatusOK, gitBindingStatusResponse{SyncStatus: status})
}
