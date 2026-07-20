// Tenant-admin user listing handlers (Phase C3.1 step B).
//
// C3 sub-slice 1 (本 tenant 用户列表): tenant_admin lists users within
// their own tenant. Single public endpoint:
//
//	GET /api/tenant/users?keyword=alice&limit=25
//
// Auth: Bearer JWT carrying tenant_admin role (owner / admin) on
// AuthClaims.TenantID. middleware.RequireTenantAdmin enforces the role
// gate; this handler derives X-Tenant-Id from AuthClaims.TenantSlug
// (falling back to TenantID for legacy tokens) so the RPC client
// forwards it and cs-user's ResolveTenant middleware pins the query.
//
// This is the FIRST real wiring of middleware.RequireTenantAdmin —
// previously test-only (just like RequirePlatformAdmin was before C2).
//
// Returns: 200 + array of users on success; 502 when cs-user is
// unreachable or returns 4xx/5xx; 401/403 from middleware.

package handlers

import (
	"context"
	"errors"
	"net/http"
	"strconv"

	"github.com/costrict/costrict-web/server/internal/middleware"
	"github.com/costrict/costrict-web/server/internal/tenant"
	userpkg "github.com/costrict/costrict-web/server/internal/user"
	"github.com/gin-gonic/gin"
)

// TenantUserService is the cs-user-side surface the handler consumes.
// Declared as interface for test substitution; production wires the
// *userpkg.RPCClient (same instance as Module.TenantResolver in rpc
// backend mode).
type TenantUserService interface {
	ListTenantUsers(ctx context.Context, keyword string, limit int) ([]userpkg.TenantUser, error)
	// SetUserStatus proxies cs-user's POST /api/internal/users/:subject_id/status.
	// cs-user enforces tenant scoping via tenant.Scope(ctx) keyed off the
	// forwarded X-Tenant-Id header — a tenant_admin from tenant X targeting
	// a user in tenant Y surfaces as ErrAdminUserRPCNotFound (row invisible
	// under the scope filter).
	SetUserStatus(ctx context.Context, subjectID, status, operatorID string) (*userpkg.AdminSetUserStatusResult, error)
}

// TenantUserAPI is the receiver for tenant-admin user handlers.
type TenantUserAPI struct {
	Svc TenantUserService
}

// tenantUserListMaxLimit caps the per-request limit. cs-user already
// caps at 200; we replicate here so an obvious-out-of-range value
// surfaces as 400 at the server before the round trip.
const tenantUserListMaxLimit = 200

// ListTenantUsers godoc
// @Summary      List users in my tenant (tenant admin)
// @Description  Returns active users within the caller's tenant. Optional keyword filters by username / display_name / email prefix; limit caps at 200. Tenant scoping is enforced by cs-user's ResolveTenant middleware via the forwarded X-Tenant-Id header — caller cannot escape their own tenant.
// @Tags         tenant-users
// @Produce      json
// @Security     BearerAuth
// @Param        keyword  query     string  false  "Substring filter (username / display_name / email prefix)"
// @Param        limit    query     int     false  "Page size (default 50, max 200)"
// @Success      200      {array}   userpkg.TenantUser
// @Failure      400      {object}  object{error=string}
// @Failure      401      {object}  object{error=string}
// @Failure      403      {object}  object{error=string}
// @Failure      502      {object}  object{error=string}
// @Router       /api/tenant/users [get]
func (a *TenantUserAPI) ListTenantUsers(c *gin.Context) {
	if a.Svc == nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": "tenant user service unavailable"})
		return
	}

	keyword := c.Query("keyword")
	limit, _ := strconv.Atoi(c.Query("limit"))
	if limit < 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "limit must be non-negative"})
		return
	}
	if limit > tenantUserListMaxLimit {
		c.JSON(http.StatusBadRequest, gin.H{"error": "limit exceeds max (" + strconv.Itoa(tenantUserListMaxLimit) + ")"})
		return
	}

	// Inject the caller's tenant slug into ctx so the RPC client forwards
	// it as X-Tenant-Id. Prefer TenantSlug (Phase B / A7 JWT claim) and
	// fall back to TenantID (canonical PK) for legacy tokens — cs-user's
	// ResolveBySlug query accepts both via `WHERE tenant_id = ? OR slug = ?`.
	ac, ok := readAuthClaims(c)
	if !ok {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "authentication required"})
		return
	}
	slug := ac.TenantSlug
	if slug == "" {
		slug = ac.TenantID
	}
	if slug == "" {
		// Should be impossible behind RequireTenantAdmin (which already
		// verified TenantRoles non-empty), but defensive — surface as
		// 403 not 500 to keep the contract obvious.
		c.JSON(http.StatusForbidden, gin.H{"error": "tenant binding required"})
		return
	}
	ctx := tenant.WithSlug(c.Request.Context(), slug)

	users, err := a.Svc.ListTenantUsers(ctx, keyword, limit)
	if err != nil {
		respondTenantUserErr(c, err)
		return
	}
	c.JSON(http.StatusOK, users)
}

// setTenantUserStatusRequest is the request body for SetTenantUserStatus.
// Mirrors the platform_admin shape (adminuser.setStatusRequest) one-to-one
// so the frontend can share the same form across both surfaces.
type setTenantUserStatusRequest struct {
	Status string `json:"status" binding:"required"`
}

// SetTenantUserStatus godoc
// @Summary      Set member status (tenant admin)
// @Description  Enable/disable/ban a member within the caller's own tenant. Refuses self-lock. Tenant scoping is enforced by cs-user via the forwarded X-Tenant-Id header — a tenant_admin cannot reach users in another tenant (the row is invisible under the scope filter and surfaces as 404). Mirrors the platform_admin endpoint (PUT /admin/users/:id/status) so the frontend can reuse the same form.
// @Tags         tenant-users
// @Accept       json
// @Produce      json
// @Security     BearerAuth
// @Param        id    path   string                          true  "Member subject id"
// @Param        body  body   object{status=string}           true  "New status (active|disabled|banned)"
// @Success      200   {object}  object{success=bool,from_status=string,to_status=string}
// @Failure      400   {object}  object{error=string}
// @Failure      401   {object}  object{error=string}
// @Failure      403   {object}  object{error=string}
// @Failure      404   {object}  object{error=string}
// @Failure      500   {object}  object{error=string}
// @Failure      502   {object}  object{error=string}
// @Router       /api/tenant/users/{id}/status [put]
func (a *TenantUserAPI) SetTenantUserStatus(c *gin.Context) {
	if a.Svc == nil {
		c.JSON(http.StatusBadGateway, gin.H{"error": "tenant user service unavailable"})
		return
	}

	// Operator = caller's subject id from verified JWT claims. The auth
	// middleware sets AuthClaimsKey alongside UserIDKey; we read from
	// claims here for consistency with ListTenantUsers above.
	ac, ok := readAuthClaims(c)
	if !ok || ac.Sub == "" {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "authentication required"})
		return
	}

	subjectID := c.Param("id")

	var req setTenantUserStatusRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request body"})
		return
	}

	// Same slug-injection pattern as ListTenantUsers — prefer TenantSlug
	// (Phase B / A7 JWT claim), fall back to TenantID for legacy tokens.
	// cs-user's ResolveBySlug query accepts both via `WHERE tenant_id = ? OR slug = ?`.
	slug := ac.TenantSlug
	if slug == "" {
		slug = ac.TenantID
	}
	if slug == "" {
		// Should be impossible behind RequireTenantAdmin, but defensive.
		c.JSON(http.StatusForbidden, gin.H{"error": "tenant binding required"})
		return
	}
	ctx := tenant.WithSlug(c.Request.Context(), slug)

	res, err := a.Svc.SetUserStatus(ctx, subjectID, req.Status, ac.Sub)
	if err != nil {
		respondTenantUserStatusErr(c, err)
		return
	}

	// Drop any cached status so the new banned/disabled/active state is
	// enforced immediately rather than after the status-cache TTL
	// elapses. Same call the platform_admin handler makes.
	middleware.InvalidateStatusCache(subjectID)

	c.JSON(http.StatusOK, gin.H{
		"success":     true,
		"from_status": res.FromStatus,
		"to_status":   res.ToStatus,
	})
}

// respondTenantUserStatusErr maps RPC client errors for the status-write
// path. Sentinel mapping mirrors adminuser.SetUserStatusHandler so the
// frontend gets identical HTTP codes for the same failure mode regardless
// of which surface it hits.
func respondTenantUserStatusErr(c *gin.Context, err error) {
	switch {
	case errors.Is(err, userpkg.ErrAdminUserRPCInvalidStatus):
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid status"})
	case errors.Is(err, userpkg.ErrAdminUserRPCCannotChangeOwn):
		// Preserve legacy HTTP code (400) for self-lock to avoid breaking
		// frontend expectations; cs-user itself returns 409. Same as the
		// platform_admin handler.
		c.JSON(http.StatusBadRequest, gin.H{"error": "cannot change your own status"})
	case errors.Is(err, userpkg.ErrAdminUserRPCNotFound):
		c.JSON(http.StatusNotFound, gin.H{"error": "user not found"})
	case errors.Is(err, userpkg.ErrRPCUnavailable),
		errors.Is(err, userpkg.ErrTenantUserUnavailable),
		errors.Is(err, userpkg.ErrNotConfigured):
		c.JSON(http.StatusBadGateway, gin.H{"error": "tenant user service unavailable"})
	default:
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
	}
}

// respondTenantUserErr maps RPC client errors to HTTP. Only two
// meaningful classes for this endpoint:
//   - ErrRPCUnavailable / ErrTenantUserUnavailable / ErrNotConfigured → 502
//   - anything else → 500 (should not happen; defensive)
func respondTenantUserErr(c *gin.Context, err error) {
	switch {
	case errors.Is(err, userpkg.ErrRPCUnavailable),
		errors.Is(err, userpkg.ErrTenantUserUnavailable),
		errors.Is(err, userpkg.ErrNotConfigured):
		c.JSON(http.StatusBadGateway, gin.H{"error": "tenant user service unavailable"})
	default:
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
	}
}

// readAuthClaims pulls the middleware.AuthClaims value from the gin
// context that middleware.Auth sets after JWT verification. Returns
// ok=false if the middleware didn't run (programmer error — every
// /api/tenant/* route should sit behind Auth + RequireTenantAdmin).
func readAuthClaims(c *gin.Context) (middleware.AuthClaims, bool) {
	v, exists := c.Get(middleware.AuthClaimsKey)
	if !exists {
		return middleware.AuthClaims{}, false
	}
	ac, ok := v.(middleware.AuthClaims)
	return ac, ok
}
