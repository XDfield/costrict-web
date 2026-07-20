// Phase E3a.1: GET /api/internal/users/:subject_id/gitea-binding.
//
// Read-only row exposing the user_gitea_binding state for one user. Used
// by:
//   - ops / dashboards (manual verification that provisioning succeeded);
//   - the future Gitea JWT fork middleware (E3a.3) — the middleware will
//     gate access on sync_status='synced' before letting the user hit
//     Gitea. E3a.1 ships the read endpoint so the integration contract is
//     testable in isolation;
//   - @server's GitServerAdapter (E3b) for team-level sync correlation.
//
// Returns:
//   - 200 + binding JSON on success;
//   - 400 when subject_id is empty;
//   - 404 when no binding exists (user never went through signup with
//     Gitea provisioning enabled, or the binding row was deleted out-of-band);
//   - 500 for unexpected errors (DB connection loss etc.) — generic
//     "internal error" message, details logged.

package handlers

import (
	"errors"
	"net/http"

	"github.com/costrict/costrict-web/cs-user/internal/user"
	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
)

// GetGiteaBinding godoc
//
//	@Summary		Get a user's Gitea binding
//	@Description	Returns the user_gitea_binding row for the given subject_id. 404 when the user has no binding (Gitea provisioning not yet run). Drives ops visibility + future fork JWT middleware (E3a.3).
//	@Tags			users
//	@Produce		json
//	@Security		InternalToken
//	@Param			subject_id	path		string	true	"User subject_id"
//	@Success		200			{object}	models.UserGiteaBinding
//	@Failure		400			{object}	object{error=string}
//	@Failure		404			{object}	object{error=string}
//	@Failure		500			{object}	object{error=string}
//	@Router			/api/internal/users/{subject_id}/gitea-binding [get]
func (a *UsersAPI) GetGiteaBinding(c *gin.Context) {
	subjectID := c.Param("subject_id")
	if subjectID == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "subject_id is required"})
		return
	}

	binding, err := a.Svc.GetGiteaBinding(c.Request.Context(), subjectID)
	if err != nil {
		switch {
		case errors.Is(err, gorm.ErrRecordNotFound):
			c.JSON(http.StatusNotFound, gin.H{"error": "gitea binding not found"})
		case errors.Is(err, user.ErrEmptySubjectID):
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		default:
			c.JSON(http.StatusInternalServerError, gin.H{"error": "internal error"})
		}
		return
	}
	c.JSON(http.StatusOK, binding)
}
