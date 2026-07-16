// Package handlers exposes cs-user's read-side REST endpoints under
// /api/internal/users. All routes are gated by X-Internal-Token (registered
// at the route-group level in internal/app); handlers themselves assume the
// caller is already authenticated and focus on input parsing + DB calls.
package handlers

import (
	"errors"
	"net/http"
	"strconv"

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
	GetUserByID(subjectID string) (*models.User, error)
	GetUsersByIDs(subjectIDs []string) (map[string]*models.User, error)
	SearchUsers(keyword string, limit int) ([]*models.User, error)
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

	u, err := a.Svc.GetUserByID(subjectID)
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

	users, err := a.Svc.GetUsersByIDs(req.IDs)
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

	users, err := a.Svc.SearchUsers(keyword, limit)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "internal error"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"users": users})
}
