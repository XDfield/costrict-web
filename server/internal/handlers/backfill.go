// Admin-triggered backfill of user_git_binding for存量 cs-user users.
//
// Existing user.created event flow only fires for users created AFTER
// USER_CREATED_EVENT_PROCESSING_ENABLED was turned on. Users that existed
// in cs-user before cutover have no binding row and no Git server account.
// This handler exposes a one-shot admin surface to reconcile them:
//
//	POST /api/admin/users/provision/backfill
//	  { "tenant_id": "...", "page_size": 100, "max_users": 500 }
//
// The handler pages through cs-user's ListUsers RPC, hands the slice to
// gitsync.UserProvisionService.BackfillMissingBindings (one Git API call
// per missing user), and returns the aggregate counts + per-user failures.
//
// Best-effort + idempotent: ProvisionUser already handles 409 recovery and
// synced-state no-op, so re-running this endpoint after a partial failure
// only re-attempts users still missing a synced binding.
//
// Auth: caller is already behind RequirePlatformAdmin (main.go's /admin
// group). Optional body — empty body uses defaults.

package handlers

import (
	"context"
	"errors"
	"io"
	"net/http"

	"github.com/costrict/costrict-web/server/internal/gitsync"
	userpkg "github.com/costrict/costrict-web/server/internal/user"
	"github.com/gin-gonic/gin"
)

// AdminUserListRPC is the minimal cs-user surface the backfill handler
// needs. *userpkg.RPCClient satisfies it; tests substitute a stub.
type AdminUserListRPC interface {
	Configured() bool
	ListUsers(ctx context.Context, p userpkg.AdminUserListParams) (*userpkg.AdminUserListResult, error)
}

// backfillRequest is the JSON body for POST /admin/users/provision/backfill.
// All fields optional; empty body applies defaults.
type backfillRequest struct {
	TenantID          string `json:"tenant_id"`
	PageSize          int    `json:"page_size"`
	MaxUsers          int    `json:"max_users"`
	UpdateDisplayName bool   `json:"update_display_name"`
}

const (
	backfillDefaultPageSize = 100
	backfillDefaultMaxUsers = 500
	backfillHardMaxPageSize = 500
	backfillHardMaxUsers    = 2000
)

// BackfillGitBindingsHandler godoc
//
//	@Summary		Backfill missing Git bindings (admin)
//	@Description	Reconcile existing cs-user users against user_git_binding by invoking ProvisionUser for every user without a synced binding. Intended for one-off migration of users created before user.created event processing was enabled. Idempotent — safe to retry.
//	@Tags			admin/users
//	@Accept			json
//	@Produce		json
//	@Security		BearerAuth
//	@Param			body	body		object{tenant_id=string,page_size=int,max_users=int,update_display_name=bool}	false	"optional filters; update_display_name pushes cs-user display_name to existing Gitea users"
//	@Success		200		{object}	gitsync.BackfillResult
//	@Failure		400		{object}	object{error=string}
//	@Failure		500		{object}	object{error=string}
//	@Failure		503		{object}	object{error=string}
//	@Router			/admin/users/provision/backfill [post]
func BackfillGitBindingsHandler(rpc AdminUserListRPC) gin.HandlerFunc {
	return func(c *gin.Context) {
		svc := GetUserProvisionService()
		if svc == nil {
			// Provisioning wiring not initialised in this build — operator
			// must enable USER_CREATED_EVENT_PROCESSING_ENABLED + restart.
			c.JSON(http.StatusServiceUnavailable, gin.H{"error": "user provision service unavailable"})
			return
		}
		if rpc == nil || !rpc.Configured() {
			c.JSON(http.StatusServiceUnavailable, gin.H{"error": "user service unavailable"})
			return
		}

		var req backfillRequest
		if err := c.ShouldBindJSON(&req); err != nil && !errors.Is(err, io.EOF) {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request body: " + err.Error()})
			return
		}

		tenantID := req.TenantID
		if tenantID == "" {
			tenantID = "default"
		}
		pageSize := clampBackfillInt(req.PageSize, 1, backfillHardMaxPageSize, backfillDefaultPageSize)
		maxUsers := clampBackfillInt(req.MaxUsers, 1, backfillHardMaxUsers, backfillDefaultMaxUsers)

		ctx := c.Request.Context()

		// Page through cs-user's ListUsers. cs-user is tenant-scoped via
		// X-Tenant-Id header (forwarded by RPCClient from context); the
		// tenant_id body field only routes the ProvisionUser side.
		var collected []userpkg.AdminUser
		page := 1
		for len(collected) < maxUsers {
			result, err := rpc.ListUsers(ctx, userpkg.AdminUserListParams{
				Page:     page,
				PageSize: pageSize,
			})
			if err != nil {
				if errors.Is(err, userpkg.ErrRPCUnavailable) {
					c.JSON(http.StatusServiceUnavailable, gin.H{"error": "user service unavailable"})
					return
				}
				c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to list users"})
				return
			}
			if len(result.Users) == 0 {
				break
			}
			collected = append(collected, result.Users...)
			if len(result.Users) < pageSize {
				break
			}
			page++
		}
		if len(collected) > maxUsers {
			collected = collected[:maxUsers]
		}

		users := make([]gitsync.BackfillUser, 0, len(collected))
		for _, u := range collected {
			users = append(users, gitsync.BackfillUser{
				SubjectID:   u.SubjectID,
				ShortID:     u.ShortID,
				Username:    u.Username,
				DisplayName: u.DisplayName,
				Email:       u.Email,
			})
		}

		result := svc.BackfillMissingBindings(ctx, tenantID, users, gitsync.BackfillOptions{
			UpdateDisplayName: req.UpdateDisplayName,
		})
		c.JSON(http.StatusOK, result)
	}
}

// clampBackfillInt returns v bounded to [lo, hi]; out-of-range or zero
// yields def. Used for page_size / max_users request params.
func clampBackfillInt(v, lo, hi, def int) int {
	if v < lo {
		return def
	}
	if v > hi {
		return hi
	}
	return v
}
