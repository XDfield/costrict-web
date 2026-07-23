// Package handlers — user-facing registration + profile endpoints (R2 of
// REGISTRATION_PROFILE_DESIGN). Three handlers, all behind RequireAuth:
//
//	GET   /api/users/me/username-available
//	POST  /api/users/me/complete-registration
//	PATCH /api/users/me/profile
//
// All three route through UserModule.Writer so the request honours the
// configured backend (local / rpc / dual-write). The handler layer is thin:
// it validates input shape, delegates to the writer, and maps the service's
// sentinel errors to HTTP 4xx bodies with stable tokens the frontend can
// branch on.
package handlers

import (
	"errors"
	"net/http"
	"strings"

	"github.com/costrict/costrict-web/server/internal/middleware"
	userpkg "github.com/costrict/costrict-web/server/internal/user"
	"github.com/gin-gonic/gin"
)

// usernameAvailableResponse mirrors the cs-user RPC shape so the frontend
// gets consistent reason tokens across local / rpc backends.
type usernameAvailableResponse struct {
	Available bool   `json:"available"`
	Reason    string `json:"reason,omitempty"`
}

// UsernameAvailable godoc
// @Summary      Check username availability
// @Description  Real-time validation for the registration form. Returns available=true when free, available=false with reason ∈ {taken, invalid_format, reserved} otherwise.
// @Tags         me
// @Produce      json
// @Security     BearerAuth
// @Param        username  query     string  true  "Candidate username"
// @Success      200       {object}  usernameAvailableResponse
// @Failure      400       {object}  object{error=string}
// @Failure      401       {object}  object{error=string}
// @Failure      500       {object}  object{error=string}
// @Router       /users/me/username-available [get]
func UsernameAvailable(c *gin.Context) {
	currentUserID := c.GetString(middleware.UserIDKey)
	if currentUserID == "" {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "Authentication required"})
		return
	}
	if UserModule == nil || UserModule.Writer == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "User service unavailable"})
		return
	}
	username := strings.TrimSpace(c.Query("username"))
	if username == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "username query param is required"})
		return
	}
	available, err := UserModule.Writer.IsUsernameAvailable(c.Request.Context(), username, currentUserID)
	if err != nil {
		switch {
		case errors.Is(err, userpkg.ErrUsernameInvalid):
			c.JSON(http.StatusOK, usernameAvailableResponse{Available: false, Reason: "invalid_format"})
		case errors.Is(err, userpkg.ErrUsernameReserved):
			c.JSON(http.StatusOK, usernameAvailableResponse{Available: false, Reason: "reserved"})
		default:
			c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to check username"})
		}
		return
	}
	if !available {
		c.JSON(http.StatusOK, usernameAvailableResponse{Available: false, Reason: "taken"})
		return
	}
	c.JSON(http.StatusOK, usernameAvailableResponse{Available: true})
}

// completeRegistrationRequest is the body for POST
// /api/users/me/complete-registration. username is required; display_name
// is optional (empty preserved as NULL on the row).
type completeRegistrationRequest struct {
	Username    string `json:"username" binding:"required"`
	DisplayName string `json:"display_name"`
}

// CompleteRegistration godoc
// @Summary      Finish first-time registration
// @Description  One-shot. Sets username + display_name and marks profile_completed_at = NOW(). Subsequent calls return 409. username is immutable from the user side after this call.
// @Tags         me
// @Accept       json
// @Produce      json
// @Security     BearerAuth
// @Param        body  body      completeRegistrationRequest  true  "Username + optional display_name"
// @Success      200   {object}  object{user=meUserDTO}
// @Failure      400   {object}  object{error=string}
// @Failure      401   {object}  object{error=string}
// @Failure      409   {object}  object{error=string}
// @Failure      500   {object}  object{error=string}
// @Router       /users/me/complete-registration [post]
func CompleteRegistration(c *gin.Context) {
	currentUserID := c.GetString(middleware.UserIDKey)
	if currentUserID == "" {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "Authentication required"})
		return
	}
	if UserModule == nil || UserModule.Writer == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "User service unavailable"})
		return
	}
	var req completeRegistrationRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid body: " + err.Error()})
		return
	}
	user, err := UserModule.Writer.CompleteRegistration(c.Request.Context(), currentUserID, req.Username, req.DisplayName)
	if err != nil {
		switch {
		case errors.Is(err, userpkg.ErrUsernameInvalid),
			errors.Is(err, userpkg.ErrUsernameReserved),
			err.Error() == "invalid_display_name",
			err.Error() == "subject_id is required":
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		case errors.Is(err, userpkg.ErrUsernameTaken):
			c.JSON(http.StatusConflict, gin.H{"error": "username_taken"})
		case errors.Is(err, userpkg.ErrRegistrationComplete):
			c.JSON(http.StatusConflict, gin.H{"error": "registration_already_complete"})
		case err.Error() == "user_not_found":
			c.JSON(http.StatusNotFound, gin.H{"error": "user_not_found"})
		default:
			c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to complete registration"})
		}
		return
	}
	// Clear the post-OAuth reg_pending cookie on success so subsequent
	// requests don't get gated by the frontend's intermediate check.
	c.SetCookie("reg_pending", "", -1, "/", "", cookieSecure, false)
	c.JSON(http.StatusOK, gin.H{"user": buildMeUserDTO(user)})
}

// updateMyProfileRequest is the body for PATCH /api/users/me/profile.
// Only display_name is accepted — username is user-side immutable. Empty
// string preserves NULL on the row.
type updateMyProfileRequest struct {
	DisplayName *string `json:"display_name"`
}

// UpdateMyProfile godoc
// @Summary      Update display_name (user self-edit)
// @Description  username is NOT editable from the user side; admin overrides go through a separate admin RPC.
// @Tags         me
// @Accept       json
// @Produce      json
// @Security     BearerAuth
// @Param        body  body      updateMyProfileRequest  true  "display_name to set"
// @Success      200   {object}  object{user=meUserDTO}
// @Failure      400   {object}  object{error=string}
// @Failure      401   {object}  object{error=string}
// @Failure      500   {object}  object{error=string}
// @Router       /users/me/profile [patch]
func UpdateMyProfile(c *gin.Context) {
	currentUserID := c.GetString(middleware.UserIDKey)
	if currentUserID == "" {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "Authentication required"})
		return
	}
	if UserModule == nil || UserModule.Writer == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "User service unavailable"})
		return
	}
	var req updateMyProfileRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid body: " + err.Error()})
		return
	}
	displayName := ""
	if req.DisplayName != nil {
		displayName = *req.DisplayName
	}
	user, err := UserModule.Writer.UpdateMyProfile(c.Request.Context(), currentUserID, displayName)
	if err != nil {
		switch {
		case err.Error() == "invalid_display_name",
			err.Error() == "subject_id is required":
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		case err.Error() == "user_not_found":
			c.JSON(http.StatusNotFound, gin.H{"error": "user_not_found"})
		default:
			c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to update profile"})
		}
		return
	}
	c.JSON(http.StatusOK, gin.H{"user": buildMeUserDTO(user)})
}
