package handlers

import (
	"errors"
	"net/http"
	"strconv"

	"github.com/costrict/costrict-web/server/internal/database"
	"github.com/costrict/costrict-web/server/internal/middleware"
	"github.com/costrict/costrict-web/server/internal/models"
	"github.com/costrict/costrict-web/server/internal/services"
	"github.com/costrict/costrict-web/server/internal/systemrole"
	"github.com/gin-gonic/gin"
)

type listTagsResponse struct {
	Tags     []models.ItemTagDict `json:"tags"`
	Total    int64                `json:"total"`
	Page     int                  `json:"page"`
	PageSize int                  `json:"pageSize"`
	HasMore  bool                 `json:"hasMore"`
}

type tagResponse struct {
	Tag *models.ItemTagDict `json:"tag"`
}

type setItemTagsRequest struct {
	TagIDs []string `json:"tagIds"`
	Tags   []string `json:"tags"`
}

type setItemTagsResponse struct {
	Tags []models.ItemTagDict `json:"tags"`
}

func isPlatformAdmin(userID string) bool {
	if userID == "" {
		return false
	}
	db := database.GetDB()
	if db == nil {
		return false
	}
	service := systemrole.NewSystemRoleService(db)
	hasRole, err := service.HasRole(userID, systemrole.SystemRolePlatformAdmin)
	return err == nil && hasRole
}

// ListTagsHandler godoc
// @Summary      List tags
// @Description  List tags with optional slug keyword search, tag class filtering, and pagination
// @Tags         tags
// @Produce      json
// @Param        q         query     string  false  "Keyword matched against tag slug"
// @Param        tagClass  query     string  false  "Filter by tag class: system or custom"
// @Param        page      query     int     false  "Page number (default: 1)"
// @Param        pageSize  query     int     false  "Page size (default: 20, max: 100)"
// @Success      200       {object}  listTagsResponse
// @Failure      500       {object}  object{error=string}
// @Router       /tags [get]
func ListTagsHandler(svc *services.TagService) gin.HandlerFunc {
	return func(c *gin.Context) {
		page, _ := strconv.Atoi(c.DefaultQuery("page", "1"))
		pageSize, _ := strconv.Atoi(c.DefaultQuery("pageSize", "20"))
		tags, total, err := svc.List(services.ListTagsOptions{
			Query:    c.Query("q"),
			TagClass: c.Query("tagClass"),
			Page:     page,
			PageSize: pageSize,
		})
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to list tags"})
			return
		}
		if page < 1 {
			page = 1
		}
		if pageSize <= 0 {
			pageSize = 20
		}
		if pageSize > 100 {
			pageSize = 100
		}
		c.JSON(http.StatusOK, gin.H{
			"tags":     tags,
			"total":    total,
			"page":     page,
			"pageSize": pageSize,
			"hasMore":  int64((page-1)*pageSize+pageSize) < total,
		})
	}
}

func GetTagHandler(svc *services.TagService) gin.HandlerFunc {
	// GetTagHandler godoc
	// @Summary      Get tag
	// @Description  Get a tag by ID
	// @Tags         tags
	// @Produce      json
	// @Param        id   path      string  true  "Tag ID"
	// @Success      200  {object}  tagResponse
	// @Failure      404  {object}  object{error=string}
	// @Failure      500  {object}  object{error=string}
	// @Router       /tags/{id} [get]
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

// CreateTagHandler godoc
// @Summary      Create tag
// @Description  Create a new tag (platform admin only)
// @Tags         tags
// @Accept       json
// @Produce      json
// @Security     BearerAuth
// @Param        body  body      services.CreateTagReq  true  "Create tag request"
// @Success      201   {object}  tagResponse
// @Failure      400   {object}  object{error=string,code=string}
// @Failure      401   {object}  object{error=string}
// @Failure      403   {object}  object{error=string}
// @Failure      409   {object}  object{error=string,slug=string}
// @Failure      500   {object}  object{error=string}
// @Router       /tags [post]
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
			switch {
			case errors.Is(err, services.ErrTagSlugTaken):
				c.JSON(http.StatusConflict, gin.H{"error": "Tag slug already exists", "slug": req.Slug})
			case errors.Is(err, services.ErrInvalidTagSlug):
				c.JSON(http.StatusBadRequest, gin.H{"error": "Tag slug may only contain lowercase letters, numbers, hyphens, and underscores", "code": "invalid_tag_slug"})
			default:
				c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to create tag"})
			}
			return
		}
		c.JSON(http.StatusCreated, gin.H{"tag": tag})
	}
}

// UpdateTagHandler godoc
// @Summary      Update tag
// @Description  Update an existing tag (platform admin only)
// @Tags         tags
// @Accept       json
// @Produce      json
// @Security     BearerAuth
// @Param        id    path      string                 true  "Tag ID"
// @Param        body  body      services.UpdateTagReq  true  "Update tag request"
// @Success      200   {object}  tagResponse
// @Failure      400   {object}  object{error=string}
// @Failure      401   {object}  object{error=string}
// @Failure      403   {object}  object{error=string}
// @Failure      404   {object}  object{error=string}
// @Failure      500   {object}  object{error=string}
// @Router       /tags/{id} [put]
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

// DeleteTagHandler godoc
// @Summary      Delete tag
// @Description  Delete a tag and its item associations (platform admin only)
// @Tags         tags
// @Produce      json
// @Security     BearerAuth
// @Param        id   path  string  true  "Tag ID"
// @Success      204  {string}  string  "No Content"
// @Failure      401  {object}  object{error=string}
// @Failure      403  {object}  object{error=string}
// @Failure      404  {object}  object{error=string}
// @Failure      500  {object}  object{error=string}
// @Router       /tags/{id} [delete]
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
// SetItemTagsHandler godoc
// @Summary      Set item tags
// @Description  Replace all tags on an item. Non-admin users may assign builtin and custom tags; submitted system tags are silently filtered.
// @Tags         tags
// @Accept       json
// @Produce      json
// @Security     BearerAuth
// @Param        id    path      string              true  "Item ID"
// @Param        body  body      setItemTagsRequest  true  "Set item tags request"
// @Success      200   {object}  setItemTagsResponse
// @Failure      400   {object}  object{error=string,code=string}
// @Failure      401   {object}  object{error=string}
// @Failure      500   {object}  object{error=string}
// @Router       /items/{id}/tags [post]
func SetItemTagsHandler(svc *services.TagService) gin.HandlerFunc {
	return func(c *gin.Context) {
		itemID := c.Param("id")
		userID := c.GetString(middleware.UserIDKey)
		admin := isPlatformAdmin(userID)

		var body struct {
			TagIDs []string `json:"tagIds"`
			Tags   []string `json:"tags"`
		}
		if err := c.ShouldBindJSON(&body); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid request"})
			return
		}

		var tags []models.ItemTagDict
		var err error
		if len(body.Tags) > 0 {
			tags, err = svc.ResolveOrCreateForAssignment(body.Tags, userID)
			if err != nil {
				if errors.Is(err, services.ErrInvalidTagSlug) {
					c.JSON(http.StatusBadRequest, gin.H{"error": "Tag slug may only contain lowercase letters, numbers, hyphens, and underscores", "code": "invalid_tag_slug"})
					return
				}
				c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to resolve tags"})
				return
			}
		} else if len(body.TagIDs) > 0 {
			if err := svc.DB.Where("id IN ?", body.TagIDs).Find(&tags).Error; err != nil {
				c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to load tags"})
				return
			}
		}

		tagIDs := make([]string, 0, len(tags))
		for _, tag := range tags {
			if tag.TagClass == services.TagClassSystem && !admin {
				continue
			}
			tagIDs = append(tagIDs, tag.ID)
		}

		if err = svc.SetItemTags(itemID, tagIDs); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to set item tags"})
			return
		}

		itemTags, _ := svc.GetItemTags([]string{itemID})
		resolved := itemTags[itemID]
		if resolved == nil {
			resolved = []models.ItemTagDict{}
		}
		c.JSON(http.StatusOK, gin.H{"tags": resolved})
	}
}
