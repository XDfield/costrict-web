// Package handlers exposes cs-user's REST endpoints under
// /api/internal/users. All routes are gated by X-Internal-Token (registered
// at the route-group level in internal/app); handlers themselves assume the
// caller is already authenticated and focus on input parsing + DB calls.
package handlers

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/costrict/costrict-web/cs-user/internal/models"
	"github.com/costrict/costrict-web/cs-user/internal/user"
	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
)

// UsersAPI wraps a user.Service so handlers stay thin and testable. The
// dependency is an interface to keep tests honest (sqlite-backed fakes can
// substitute without spinning a real Service).
type UsersAPI struct {
	Svc UserService
}

// UserService is the subset of *user.Service the users handlers need.
// Declared as an interface so the handler package doesn't import user
// directly into its tests via a concrete type — keeps the substitution
// seam explicit.
type UserService interface {
	// Reads (Phase 1) — B5: ctx carries the tenant signal used by
	// tenant.Scope for auto-filtering.
	GetUserByID(ctx context.Context, subjectID string) (*models.User, error)
	GetUsersByIDs(ctx context.Context, subjectIDs []string) (map[string]*models.User, error)
	SearchUsers(ctx context.Context, keyword string, limit int) ([]*models.User, error)
	// Writes (Phase 2) — RPCWriter on costrict-web server side calls these.
	GetOrCreateUser(ctx context.Context, claims *models.JWTClaims) (*models.User, error)
	BindIdentityToUser(ctx context.Context, userSubjectID string, claims *models.JWTClaims, opts ...models.BindIdentityOptions) error
	TransferIdentityToUser(ctx context.Context, targetUserSubjectID, externalKey, sourceUserSubjectID string) error
	UnbindIdentityByProvider(ctx context.Context, userSubjectID, provider string) error
	// Phase A4b: enterprise mapping refresh — server's OAuth callback fires
	// this after GetOrCreateUser. cs-user is authoritative for
	// employment_identities (server has no such table); ApplyEnterpriseMapping
	// is the single write path.
	ApplyEnterpriseMapping(ctx context.Context, params user.EmploymentMappingParams) error
}

// byIDsRequest is the body shape for POST /api/internal/users/by-ids.
// We accept POST (not GET with query params) because the typical caller is
// a batch resolver passing dozens of subject_ids — GET query strings cap
// out around 2KB and look ugly in access logs.
type byIDsRequest struct {
	IDs []string `json:"ids" binding:"required,min=1,max=500"`
}

// GetUser godoc
//
//	@Summary		Get a user by subject_id
//	@Description	Returns the user record matching the given subject_id. 404 when not found. Internal-only — requires the shared secret.
//	@Tags			users
//	@Produce		json
//	@Security		InternalToken
//	@Param			subject_id	path		string	true	"User subject_id (stable application-level identifier)"
//	@Success		200			{object}	models.User
//	@Failure		400			{object}	object{error=string}
//	@Failure		404			{object}	object{error=string}
//	@Failure		500			{object}	object{error=string}
//	@Router			/api/internal/users/{subject_id} [get]
func (a *UsersAPI) GetUser(c *gin.Context) {
	subjectID := c.Param("subject_id")
	if subjectID == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "subject_id is required"})
		return
	}

	u, err := a.Svc.GetUserByID(c.Request.Context(), subjectID)
	if err != nil {
		switch {
		case errors.Is(err, user.ErrEmptySubjectID):
			c.JSON(http.StatusBadRequest, gin.H{"error": "subject_id is required"})
		case errors.Is(err, gorm.ErrRecordNotFound):
			c.JSON(http.StatusNotFound, gin.H{"error": "user not found"})
		default:
			c.JSON(http.StatusInternalServerError, gin.H{"error": "internal error"})
		}
		return
	}
	c.JSON(http.StatusOK, u)
}

// GetUsersByIDs godoc
//
//	@Summary		Batch-resolve users by subject_id
//	@Description	Returns a subject_id → user map. Missing IDs are silently omitted; callers compare lengths to detect partial misses. Capped at 500 IDs per request.
//	@Tags			users
//	@Accept			json
//	@Produce		json
//	@Security		InternalToken
//	@Param			body	body		byIDsRequest	true	"Subject IDs to resolve"
//	@Success		200		{object}	object{users=map[string]models.User}
//	@Failure		400		{object}	object{error=string}
//	@Failure		500		{object}	object{error=string}
//	@Router			/api/internal/users/by-ids [post]
func (a *UsersAPI) GetUsersByIDs(c *gin.Context) {
	var req byIDsRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid body: " + err.Error()})
		return
	}

	users, err := a.Svc.GetUsersByIDs(c.Request.Context(), req.IDs)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "internal error"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"users": users})
}

// SearchUsers godoc
//
//	@Summary		Search active users
//	@Description	Returns active users whose username / display_name / email match the keyword (LIKE %keyword%). Limit defaults to 50, capped at 200.
//	@Tags			users
//	@Produce		json
//	@Security		InternalToken
//	@Param			keyword	query		string	false	"Search keyword (matched against username / display_name / email)"
//	@Param			limit	query		int		false	"Max results (default 50, max 200)"
//	@Success		200		{object}	object{users=[]models.User}
//	@Failure		400		{object}	object{error=string}
//	@Failure		500		{object}	object{error=string}
//	@Router			/api/internal/users/search [get]
func (a *UsersAPI) SearchUsers(c *gin.Context) {
	keyword := c.Query("keyword")

	limit := 0
	if v := c.Query("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 {
			c.JSON(http.StatusBadRequest, gin.H{"error": "limit must be a non-negative integer"})
			return
		}
		if n > 200 {
			n = 200
		}
		limit = n
	}

	users, err := a.Svc.SearchUsers(c.Request.Context(), keyword, limit)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "internal error"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"users": users})
}

// --- Phase 2: Write API ---
//
// Each handler maps a service error to a stable HTTP code per the matrix:
//   gorm.ErrRecordNotFound            → 404
//   ErrEmptySubjectID / missing field → 400
//   ErrLastIdentity                   → 409
//   ErrExplicitlyUnbound              → 409
//   ErrIdentityAlreadyBound           → 409
//   other                             → 500

// getOrCreateRequest is the body shape for POST /api/internal/users/get-or-create.
// server's RPCWriter posts the JWTClaims it parsed from the OAuth callback;
// the response is the upserted user row.
type getOrCreateRequest struct {
	models.JWTClaims
}

// GetOrCreate godoc
//
//	@Summary		Upsert user from JWT claims (login entry point)
//	@Description	Idempotent upsert driven by the OAuth callback's parsed JWT claims. Multi-lookup strategy (external_key → universal_id → casdoor_id → sub → username). Creates a primary identity row when a new user is created.
//	@Tags			users
//	@Accept			json
//	@Produce		json
//	@Security		InternalToken
//	@Param			body	body		models.JWTClaims	true	"JWT claim payload (parsed — cs-user does not verify JWT signatures; the X-Internal-Token middleware authenticates the caller)"
//	@Success		200		{object}	models.User
//	@Failure		400		{object}	object{error=string}
//	@Failure		500		{object}	object{error=string}
//	@Router			/api/internal/users/get-or-create [post]
func (a *UsersAPI) GetOrCreate(c *gin.Context) {
	var req getOrCreateRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid body: " + err.Error()})
		return
	}

	u, err := a.Svc.GetOrCreateUser(c.Request.Context(), &req.JWTClaims)
	if err != nil {
		// GetOrCreateUser returns plain fmt.Errorf for arg validation; the
		// only way to distinguish 400 vs 500 is sniffing for the known
		// "no valid user identifier" / "nil JWT claims" prefixes.
		msg := err.Error()
		switch {
		case msg == "nil JWT claims" || strings.HasPrefix(msg, "no valid user identifier"):
			c.JSON(http.StatusBadRequest, gin.H{"error": msg})
		default:
			c.JSON(http.StatusInternalServerError, gin.H{"error": "internal error"})
		}
		return
	}
	c.JSON(http.StatusOK, u)
}

// bindIdentityRequest is the body shape for POST /api/internal/users/:subject_id/bind-identity.
type bindIdentityRequest struct {
	Claims  *models.JWTClaims           `json:"claims" binding:"required"`
	Options *models.BindIdentityOptions `json:"options,omitempty"`
}

// BindIdentity godoc
//
//	@Summary		Bind an identity to a user
//	@Description	Idempotent bind. Recovers soft-deleted identities instead of duplicating. Re-binding an explicitly-unbound identity requires options.force_rebind.
//	@Tags			users
//	@Accept			json
//	@Produce		json
//	@Security		InternalToken
//	@Param			subject_id	path		string				true	"Target user subject_id"
//	@Param			body		body		bindIdentityRequest	true	"Claims to bind + optional BindIdentityOptions"
//	@Success		204			{object}	nil
//	@Failure		400			{object}	object{error=string}
//	@Failure		409			{object}	object{error=string}
//	@Failure		500			{object}	object{error=string}
//	@Router			/api/internal/users/{subject_id}/bind-identity [post]
func (a *UsersAPI) BindIdentity(c *gin.Context) {
	userSubjectID := c.Param("subject_id")
	if userSubjectID == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "subject_id is required"})
		return
	}
	var req bindIdentityRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid body: " + err.Error()})
		return
	}

	var opts []models.BindIdentityOptions
	if req.Options != nil {
		opts = append(opts, *req.Options)
	}
	if err := a.Svc.BindIdentityToUser(c.Request.Context(), userSubjectID, req.Claims, opts...); err != nil {
		switch {
		case errors.Is(err, user.ErrExplicitlyUnbound), errors.Is(err, user.ErrIdentityAlreadyBound):
			c.JSON(http.StatusConflict, gin.H{"error": err.Error()})
		case isBindArgError(err):
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		default:
			c.JSON(http.StatusInternalServerError, gin.H{"error": "internal error"})
		}
		return
	}
	c.Status(http.StatusNoContent)
}

// transferIdentityRequest is the body for POST /api/internal/users/transfer-identity.
type transferIdentityRequest struct {
	TargetUserSubjectID string `json:"target_user_subject_id" binding:"required"`
	ExternalKey         string `json:"external_key" binding:"required"`
	// SourceUserSubjectID is accepted for forwards compatibility with
	// server's signature; cs-user identifies the identity purely by
	// external_key.
	SourceUserSubjectID string `json:"source_user_subject_id,omitempty"`
}

// TransferIdentity godoc
//
//	@Summary		Transfer an identity to another user
//	@Description	Account-merge primitive. Moves the identity identified by external_key to the target user. No-op if the target already owns it.
//	@Tags			users
//	@Accept			json
//	@Produce		json
//	@Security		InternalToken
//	@Param			body	body		transferIdentityRequest	true	"Transfer target + identity key"
//	@Success		204		{object}	nil
//	@Failure		400		{object}	object{error=string}
//	@Failure		404		{object}	object{error=string}
//	@Failure		500		{object}	object{error=string}
//	@Router			/api/internal/users/transfer-identity [post]
func (a *UsersAPI) TransferIdentity(c *gin.Context) {
	var req transferIdentityRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid body: " + err.Error()})
		return
	}

	if err := a.Svc.TransferIdentityToUser(c.Request.Context(), req.TargetUserSubjectID, req.ExternalKey, req.SourceUserSubjectID); err != nil {
		msg := err.Error()
		switch {
		case msg == "identity_not_found":
			c.JSON(http.StatusNotFound, gin.H{"error": msg})
		case strings.Contains(msg, "are required"):
			c.JSON(http.StatusBadRequest, gin.H{"error": msg})
		default:
			c.JSON(http.StatusInternalServerError, gin.H{"error": "internal error"})
		}
		return
	}
	c.Status(http.StatusNoContent)
}

// UnbindIdentity godoc
//
//	@Summary		Unbind all identities for a provider
//	@Description	Soft-deletes every identity matching the provider on the user and marks them explicitly_unbound. Refuses to unbind the user's last identity. Promotes the next best-rank identity to primary if the unbind removed the primary.
//	@Tags			users
//	@Produce		json
//	@Security		InternalToken
//	@Param			subject_id	path		string	true	"User subject_id"
//	@Param			provider	path		string	true	"Provider to unbind (e.g. github, phone, idtrust)"
//	@Success		204			{object}	nil
//	@Failure		400			{object}	object{error=string}
//	@Failure		404			{object}	object{error=string}
//	@Failure		409			{object}	object{error=string}
//	@Failure		500			{object}	object{error=string}
//	@Router			/api/internal/users/{subject_id}/identities/{provider} [delete]
func (a *UsersAPI) UnbindIdentity(c *gin.Context) {
	userSubjectID := c.Param("subject_id")
	if userSubjectID == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "subject_id is required"})
		return
	}
	provider := c.Param("provider")
	if provider == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "provider is required"})
		return
	}

	if err := a.Svc.UnbindIdentityByProvider(c.Request.Context(), userSubjectID, provider); err != nil {
		switch {
		case errors.Is(err, user.ErrLastIdentity):
			c.JSON(http.StatusConflict, gin.H{"error": err.Error()})
		case errors.Is(err, user.ErrEmptySubjectID):
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		default:
			msg := err.Error()
			switch {
			case msg == "provider is required", msg == "identity not found":
				// 400 / 404 distinction: missing-provider is a caller error
				// (400); identity-not-found is closer to 404 but server
				// returns 400 historically. Keep 404 to match REST semantics.
				if msg == "identity not found" {
					c.JSON(http.StatusNotFound, gin.H{"error": msg})
				} else {
					c.JSON(http.StatusBadRequest, gin.H{"error": msg})
				}
			default:
				c.JSON(http.StatusInternalServerError, gin.H{"error": "internal error"})
			}
		}
		return
	}
	c.Status(http.StatusNoContent)
}

// isBindArgError returns true for the bind-related service errors that
// indicate a caller programming error (400) rather than a server-side fault
// (500). BindIdentityToUser uses plain fmt.Errorf for arg validation; the
// messages are part of the contract.
func isBindArgError(err error) bool {
	msg := err.Error()
	return msg == "user_subject_id is required" ||
		msg == "nil JWT claims" ||
		msg == "external key is required"
}

// --- Phase A4b: Enterprise mapping ---

// applyEnterpriseMappingRequest is the body for POST
// /api/internal/users/apply-enterprise-mapping. TenantID is optional (the
// service falls back to "default" when empty); UserSubjectID + Provider are
// required.
type applyEnterpriseMappingRequest struct {
	TenantID      string `json:"tenant_id,omitempty"`
	UserSubjectID string `json:"user_subject_id" binding:"required"`
	Provider      string `json:"provider" binding:"required"`
}

// ApplyEnterpriseMapping godoc
//
//	@Summary		Refresh employment_identities snapshot (login hook)
//	@Description	Loads tenant_configs.employment_providers for the tenant and upserts the user's employment_identities row when the login provider is enabled. Returns 200 with `{"applied": false}` when the provider is not enabled (treated as a no-op success — login must not break on tenant config); returns 200 with `{"applied": true}` when a row was written or refreshed. Malformed tenant YAML surfaces as 500 (operator-visible); missing tenant_configs row is the same as disabled (200, applied=false).
//	@Tags			users
//	@Accept			json
//	@Produce		json
//	@Security		InternalToken
//	@Param			body	body		applyEnterpriseMappingRequest	true	"User + provider + optional tenant_id"
//	@Success		200		{object}	object{applied=bool}
//	@Failure		400		{object}	object{error=string}
//	@Failure		500		{object}	object{error=string}
//	@Router			/api/internal/users/apply-enterprise-mapping [post]
func (a *UsersAPI) ApplyEnterpriseMapping(c *gin.Context) {
	var req applyEnterpriseMappingRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid body: " + err.Error()})
		return
	}

	err := a.Svc.ApplyEnterpriseMapping(c.Request.Context(), user.EmploymentMappingParams{
		TenantID:      req.TenantID,
		UserSubjectID: req.UserSubjectID,
		Provider:      req.Provider,
	})
	switch {
	case err == nil:
		c.JSON(http.StatusOK, gin.H{"applied": true})
	case errors.Is(err, user.ErrEnterpriseMappingDisabled):
		// Disabled is success — the OAuth callback treats it as "skipped"
		// rather than a login failure. applied=false lets the caller
		// distinguish (e.g. for metrics) without inspecting error strings.
		c.JSON(http.StatusOK, gin.H{"applied": false})
	case isEnterpriseMappingArgError(err):
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
	default:
		c.JSON(http.StatusInternalServerError, gin.H{"error": "internal error"})
	}
}

// isEnterpriseMappingArgError returns true for the validation errors
// ApplyEnterpriseMapping emits before touching the DB. These are caller bugs
// (400), not server faults (500).
func isEnterpriseMappingArgError(err error) bool {
	msg := err.Error()
	return msg == "ApplyEnterpriseMapping: empty UserSubjectID" ||
		msg == "ApplyEnterpriseMapping: empty Provider"
}
