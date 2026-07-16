package handlers

import (
	"errors"
	"net/http"

	"github.com/costrict/costrict-web/cs-user/internal/models"
	"github.com/costrict/costrict-web/cs-user/internal/user"
	"github.com/gin-gonic/gin"
)

// AuthIdentitiesAPI exposes the per-user auth-identity listing endpoint.
// bind / unbind / transfer are deferred to Phase A (JWT self-sign) — they
// need claims plumbing that doesn't exist in P0-3 yet.
type AuthIdentitiesAPI struct {
	Svc AuthIdentityService
}

// AuthIdentityService is the subset of *user.Service the auth-identities
// handlers need. Mirrors the UserService seam in users.go.
type AuthIdentityService interface {
	ListIdentities(userSubjectID string) ([]*models.UserAuthIdentity, error)
}

// ListIdentities godoc
//
//	@Summary		List a user's auth identities
//	@Description	Returns every external login identity bound to the user, ordered so the primary identity surfaces first. Empty list when the user has none.
//	@Tags			auth-identities
//	@Produce		json
//	@Security		InternalToken
//	@Param			subject_id	path		string	true	"User subject_id"
//	@Success		200			{object}	object{identities=[]models.UserAuthIdentity}
//	@Failure		400			{object}	object{error=string}
//	@Failure		500			{object}	object{error=string}
//	@Router			/api/internal/users/{subject_id}/auth-identities [get]
func (a *AuthIdentitiesAPI) ListIdentities(c *gin.Context) {
	subjectID := c.Param("subject_id")
	if subjectID == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "subject_id is required"})
		return
	}

	identities, err := a.Svc.ListIdentities(subjectID)
	if err != nil {
		if errors.Is(err, user.ErrEmptySubjectID) {
			c.JSON(http.StatusBadRequest, gin.H{"error": "subject_id is required"})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": "internal error"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"identities": identities})
}
