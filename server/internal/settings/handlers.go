package settings

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/costrict/costrict-web/server/internal/audit"
	appmiddleware "github.com/costrict/costrict-web/server/internal/middleware"
	"github.com/gin-gonic/gin"
)

// ListSettingsHandler godoc
//
//	@Summary		List system settings
//	@Description	Return every system-level setting as a key→value map (platform admin only)
//	@Tags			admin/settings
//	@Produce		json
//	@Security		BearerAuth
//	@Success		200	{object}	object{settings=object}
//	@Failure		401	{object}	object{error=string}
//	@Failure		500	{object}	object{error=string}
//	@Router			/admin/settings [get]
func ListSettingsHandler(svc *Service) gin.HandlerFunc {
	return func(c *gin.Context) {
		all, err := svc.GetAll()
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to list settings"})
			return
		}
		c.JSON(http.StatusOK, gin.H{"settings": all})
	}
}

// UpdateSettingHandler godoc
//
//	@Summary		Update a system setting
//	@Description	Upsert a single system setting by key (platform admin only)
//	@Tags			admin/settings
//	@Accept			json
//	@Produce		json
//	@Security		BearerAuth
//	@Param			key		path		string					true	"Setting key"
//	@Param			body	body		object{value=object}	true	"Setting value (any JSON)"
//	@Success		200		{object}	object{setting=object}
//	@Failure		400		{object}	object{error=string}
//	@Failure		401		{object}	object{error=string}
//	@Failure		403		{object}	object{error=string}
//	@Failure		500		{object}	object{error=string}
//	@Router			/admin/settings/{key} [put]
func UpdateSettingHandler(svc *Service) gin.HandlerFunc {
	return func(c *gin.Context) {
		operatorID := c.GetString(appmiddleware.UserIDKey)
		if operatorID == "" {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "Authentication required"})
			return
		}

		key := c.Param("key")
		if key == "" {
			c.JSON(http.StatusBadRequest, gin.H{"error": "setting key is required"})
			return
		}

		// value is any JSON (bool / string / number / object). Use RawMessage so we
		// store the value verbatim without round-tripping through a Go type.
		var req struct {
			Value json.RawMessage `json:"value"`
		}
		if err := c.ShouldBindJSON(&req); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request body"})
			return
		}

		setting, err := svc.Set(key, req.Value, operatorID)
		if err != nil {
			switch {
			case errors.Is(err, ErrInvalidKey):
				c.JSON(http.StatusBadRequest, gin.H{"error": "invalid setting key"})
			case errors.Is(err, ErrInvalidValue):
				c.JSON(http.StatusBadRequest, gin.H{"error": "invalid setting value"})
			default:
				c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to update setting"})
			}
			return
		}

		audit.Record(operatorID, audit.ActionSettingUpdate, audit.TargetSetting, key, gin.H{"value": json.RawMessage(setting.Value)})

		c.JSON(http.StatusOK, gin.H{"setting": setting})
	}
}
