package handlers

import (
	"errors"
	"net/http"

	"github.com/costrict/costrict-web/server/internal/middleware"
	"github.com/costrict/costrict-web/server/internal/services"
	"github.com/gin-gonic/gin"
)

// ListCategoriesHandler godoc
// @Summary      List all categories
// @Description  Get all item categories with i18n names
// @Tags         categories
// @Produce      json
// @Success      200  {object}  object{categories=[]object}
// @Failure      500  {object}  object{error=string}
// @Router       /categories [get]
func ListCategoriesHandler(svc *services.CategoryService) gin.HandlerFunc {
	return func(c *gin.Context) {
		categories, err := svc.List()
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to list categories"})
			return
		}
		c.JSON(http.StatusOK, gin.H{"categories": categories})
	}
}

// GetCategoryHandler godoc
// @Summary      Get category
// @Description  Get a category by ID
// @Tags         categories
// @Produce      json
// @Param        id   path      string  true  "Category ID"
// @Success      200  {object}  object{category=object}
// @Failure      404  {object}  object{error=string}
// @Router       /categories/{id} [get]
func GetCategoryHandler(svc *services.CategoryService) gin.HandlerFunc {
	return func(c *gin.Context) {
		cat, err := svc.Get(c.Param("id"))
		if err != nil {
			if errors.Is(err, services.ErrCategoryNotFound) {
				c.JSON(http.StatusNotFound, gin.H{"error": "Category not found"})
				return
			}
			c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to get category"})
			return
		}
		c.JSON(http.StatusOK, gin.H{"category": cat})
	}
}

// CreateCategoryHandler godoc
// @Summary      Create category
// @Description  Create a new item category with i18n names
// @Tags         categories
// @Accept       json
// @Produce      json
// @Param        body  body      object{slug=string,icon=string,sortOrder=int,names=object,descriptions=object}  true  "Category data"
// @Success      201   {object}  object{category=object}
// @Failure      400   {object}  object{error=string}
// @Failure      409   {object}  object{error=string}
// @Failure      500   {object}  object{error=string}
// @Router       /categories [post]
func CreateCategoryHandler(svc *services.CategoryService) gin.HandlerFunc {
	return func(c *gin.Context) {
		var req services.CreateCategoryReq
		if err := c.ShouldBindJSON(&req); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid request"})
			return
		}

		userID := c.GetString(middleware.UserIDKey)
		cat, err := svc.Create(req, userID)
		if err != nil {
			if errors.Is(err, services.ErrCategorySlugTaken) {
				c.JSON(http.StatusConflict, gin.H{"error": "Category slug already exists", "slug": req.Slug})
				return
			}
			c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to create category"})
			return
		}
		c.JSON(http.StatusCreated, gin.H{"category": cat})
	}
}

// UpdateCategoryHandler godoc
// @Summary      Update category
// @Description  Update an existing item category
// @Tags         categories
// @Accept       json
// @Produce      json
// @Param        id    path      string  true  "Category ID"
// @Param        body  body      object{icon=string,sortOrder=int,names=object,descriptions=object}  true  "Category update data"
// @Success      200   {object}  object{category=object}
// @Failure      400   {object}  object{error=string}
// @Failure      404   {object}  object{error=string}
// @Failure      500   {object}  object{error=string}
// @Router       /categories/{id} [put]
func UpdateCategoryHandler(svc *services.CategoryService) gin.HandlerFunc {
	return func(c *gin.Context) {
		var req services.UpdateCategoryReq
		if err := c.ShouldBindJSON(&req); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid request"})
			return
		}

		cat, err := svc.Update(c.Param("id"), req)
		if err != nil {
			if errors.Is(err, services.ErrCategoryNotFound) {
				c.JSON(http.StatusNotFound, gin.H{"error": "Category not found"})
				return
			}
			c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to update category"})
			return
		}
		c.JSON(http.StatusOK, gin.H{"category": cat})
	}
}

// DeleteCategoryHandler godoc
// @Summary      Delete category
// @Description  Delete an item category by ID
// @Tags         categories
// @Param        id   path      string  true  "Category ID"
// @Success      204
// @Failure      404  {object}  object{error=string}
// @Failure      500  {object}  object{error=string}
// @Router       /categories/{id} [delete]
func DeleteCategoryHandler(svc *services.CategoryService) gin.HandlerFunc {
	return func(c *gin.Context) {
		err := svc.Delete(c.Param("id"))
		if err != nil {
			if errors.Is(err, services.ErrCategoryNotFound) {
				c.JSON(http.StatusNotFound, gin.H{"error": "Category not found"})
				return
			}
			c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to delete category"})
			return
		}
		c.Status(http.StatusNoContent)
	}
}
