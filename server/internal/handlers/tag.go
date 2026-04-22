package handlers

import (
	"errors"
	"net/http"

	"github.com/costrict/costrict-web/server/internal/middleware"
	"github.com/costrict/costrict-web/server/internal/models"
	"github.com/costrict/costrict-web/server/internal/services"
	"github.com/gin-gonic/gin"
)

// ListTagsHandler returns all tags, optionally filtered by tagClass.
func ListTagsHandler(svc *services.TagService) gin.HandlerFunc {
	return func(c *gin.Context) {
		tagClass := c.Query("tagClass")
		tags, err := svc.List(tagClass)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to list tags"})
			return
		}
		c.JSON(http.StatusOK, gin.H{"tags": tags})
	}
}

// GetTagHandler returns a single tag by ID.
func GetTagHandler(svc *services.TagService) gin.HandlerFunc {
	return func(c *gin.Context) {
		tag, err := svc.Get(c.Param("id"))
		if err != nil {
			if errors.Is(err, services.ErrTagNotFound) {
				c.JSON(http.StatusNotFound, gin.H{"error": "Tag not found"})
				return
			}
			c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to get tag"})
			return
		}
		c.JSON(http.StatusOK, gin.H{"tag": tag})
	}
}

// CreateTagHandler creates a new tag.
func CreateTagHandler(svc *services.TagService) gin.HandlerFunc {
	return func(c *gin.Context) {
		var req services.CreateTagReq
		if err := c.ShouldBindJSON(&req); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid request"})
			return
		}

		userID := c.GetString(middleware.UserIDKey)
		tag, err := svc.Create(req, userID)
		if err != nil {
			if errors.Is(err, services.ErrTagSlugTaken) {
				c.JSON(http.StatusConflict, gin.H{"error": "Tag slug already exists", "slug": req.Slug})
				return
			}
			c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to create tag"})
			return
		}
		c.JSON(http.StatusCreated, gin.H{"tag": tag})
	}
}

// UpdateTagHandler updates an existing tag.
func UpdateTagHandler(svc *services.TagService) gin.HandlerFunc {
	return func(c *gin.Context) {
		var req services.UpdateTagReq
		if err := c.ShouldBindJSON(&req); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid request"})
			return
		}

		tag, err := svc.Update(c.Param("id"), req)
		if err != nil {
			if errors.Is(err, services.ErrTagNotFound) {
				c.JSON(http.StatusNotFound, gin.H{"error": "Tag not found"})
				return
			}
			c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to update tag"})
			return
		}
		c.JSON(http.StatusOK, gin.H{"tag": tag})
	}
}

// DeleteTagHandler deletes a tag and all its item associations.
func DeleteTagHandler(svc *services.TagService) gin.HandlerFunc {
	return func(c *gin.Context) {
		err := svc.Delete(c.Param("id"))
		if err != nil {
			if errors.Is(err, services.ErrTagNotFound) {
				c.JSON(http.StatusNotFound, gin.H{"error": "Tag not found"})
				return
			}
			c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to delete tag"})
			return
		}
		c.Status(http.StatusNoContent)
	}
}

// SetItemTagsHandler replaces all tags on an item.
// Accepts {"tagIds": ["uuid1", "uuid2"]} or {"tags": ["slug1", "slug2"]}.
func SetItemTagsHandler(svc *services.TagService) gin.HandlerFunc {
	return func(c *gin.Context) {
		itemID := c.Param("id")

		var body struct {
			TagIDs []string `json:"tagIds"`
			Tags   []string `json:"tags"`
		}
		if err := c.ShouldBindJSON(&body); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid request"})
			return
		}

		var tagIDs []string
		var err error

		if len(body.Tags) > 0 {
			// Resolve slugs to IDs via EnsureTags
			userID := c.GetString(middleware.UserIDKey)
			resolved, err := svc.EnsureTags(body.Tags, services.TagClassCustom, userID)
			if err != nil {
				c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to resolve tags"})
				return
			}
			for _, t := range resolved {
				tagIDs = append(tagIDs, t.ID)
			}
		} else if len(body.TagIDs) > 0 {
			tagIDs = body.TagIDs
		}

		// Validate tag IDs exist
		if len(tagIDs) > 0 {
			var count int64
			svc.DB.Table("item_tag_dicts").Where("id IN ?", tagIDs).Count(&count)
			if int(count) != len(tagIDs) {
				// Filter to only valid IDs
				var validIDs []string
				svc.DB.Table("item_tag_dicts").Where("id IN ?", tagIDs).Pluck("id", &validIDs)
				tagIDs = validIDs
			}
		}

		if err = svc.SetItemTags(itemID, tagIDs); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to set item tags"})
			return
		}

		// Return updated tags
		itemTags, _ := svc.GetItemTags([]string{itemID})
		tags := itemTags[itemID]
		if tags == nil {
			tags = []models.ItemTagDict{}
		}
		c.JSON(http.StatusOK, gin.H{"tags": tags})
	}
}
