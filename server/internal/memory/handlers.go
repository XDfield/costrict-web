package memory

import (
	"net/http"
	"strconv"

	"github.com/gin-gonic/gin"
)

// CreateMemoryHandler 上报记忆
// @Summary      上报记忆
// @Description  创建新记忆或更新已存在的记忆（按 userID + projectPath + slug 去重）
// @Tags         memories
// @Accept       json
// @Produce      json
// @Param        body  body  CreateMemoryRequest  true  "记忆数据"
// @Success      200  {object}  models.MemoryFile
// @Router       /memories [post]
func CreateMemoryHandler(svc *Service) gin.HandlerFunc {
	return func(c *gin.Context) {
		userID := c.GetString("userId")
		if userID == "" {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "unauthorized"})
			return
		}

		var req CreateMemoryRequest
		if err := c.ShouldBindJSON(&req); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}

		memory, err := svc.CreateOrUpdateMemory(c.Request.Context(), userID, &req)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}

		c.JSON(http.StatusOK, memory)
	}
}

// UpdateMemoryHandler 更新记忆
// @Summary      更新记忆
// @Description  更新指定记忆的内容，可选择创建新版本或覆盖当前版本
// @Tags         memories
// @Accept       json
// @Produce      json
// @Param        id    path  string  true  "记忆ID"
// @Param        body  body  UpdateMemoryRequest  true  "更新数据"
// @Success      200  {object}  models.MemoryFile
// @Router       /memories/{id} [put]
func UpdateMemoryHandler(svc *Service) gin.HandlerFunc {
	return func(c *gin.Context) {
		userID := c.GetString("userId")
		if userID == "" {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "unauthorized"})
			return
		}

		memoryID := c.Param("id")
		var req UpdateMemoryRequest
		if err := c.ShouldBindJSON(&req); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}

		memory, err := svc.UpdateMemory(c.Request.Context(), userID, memoryID, &req)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}

		c.JSON(http.StatusOK, memory)
	}
}

// ListMemoriesHandler 查询记忆列表
// @Summary      查询记忆列表
// @Description  按项目路径、工作目录、类型、关键词过滤查询当前用户的记忆列表
// @Tags         memories
// @Produce      json
// @Param        projectPath  query  string  false  "项目路径"
// @Param        workDir      query  string  false  "工作目录"
// @Param        type         query  string  false  "记忆类型"
// @Param        keyword      query  string  false  "关键词搜索"
// @Success      200  {object}  object{items=[]models.MemoryFile}
// @Router       /memories [get]
func ListMemoriesHandler(svc *Service) gin.HandlerFunc {
	return func(c *gin.Context) {
		userID := c.GetString("userId")
		if userID == "" {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "unauthorized"})
			return
		}

		var req ListMemoriesRequest
		if err := c.ShouldBindQuery(&req); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}

		memories, err := svc.ListMemories(userID, &req)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}

		c.JSON(http.StatusOK, gin.H{"items": memories})
	}
}

// GetMemoryHandler 获取记忆详情
// @Summary      获取记忆详情
// @Description  获取指定记忆的元信息（不含内容）
// @Tags         memories
// @Produce      json
// @Param        id  path  string  true  "记忆ID"
// @Success      200  {object}  models.MemoryFile
// @Router       /memories/{id} [get]
func GetMemoryHandler(svc *Service) gin.HandlerFunc {
	return func(c *gin.Context) {
		userID := c.GetString("userId")
		if userID == "" {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "unauthorized"})
			return
		}

		memoryID := c.Param("id")
		memory, err := svc.GetMemory(userID, memoryID)
		if err != nil {
			c.JSON(http.StatusNotFound, gin.H{"error": "memory not found"})
			return
		}

		c.JSON(http.StatusOK, memory)
	}
}

// GetMemoryContentHandler 获取记忆内容
// @Summary      获取记忆内容
// @Description  获取指定记忆当前版本的 markdown 内容
// @Tags         memories
// @Produce      text/plain
// @Param        id  path  string  true  "记忆ID"
// @Success      200  {string}  string
// @Router       /memories/{id}/content [get]
func GetMemoryContentHandler(svc *Service) gin.HandlerFunc {
	return func(c *gin.Context) {
		userID := c.GetString("userId")
		if userID == "" {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "unauthorized"})
			return
		}

		memoryID := c.Param("id")
		content, err := svc.GetMemoryContent(c.Request.Context(), userID, memoryID)
		if err != nil {
			c.JSON(http.StatusNotFound, gin.H{"error": "memory not found"})
			return
		}

		c.Header("Content-Type", "text/markdown; charset=utf-8")
		c.String(http.StatusOK, content)
	}
}

// ListVersionsHandler 获取版本列表
// @Summary      获取版本列表
// @Description  获取指定记忆的所有历史版本
// @Tags         memories
// @Produce      json
// @Param        id  path  string  true  "记忆ID"
// @Success      200  {object}  object{items=[]models.MemoryVersion}
// @Router       /memories/{id}/versions [get]
func ListVersionsHandler(svc *Service) gin.HandlerFunc {
	return func(c *gin.Context) {
		userID := c.GetString("userId")
		if userID == "" {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "unauthorized"})
			return
		}

		memoryID := c.Param("id")
		versions, err := svc.ListVersions(userID, memoryID)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}

		c.JSON(http.StatusOK, gin.H{"items": versions})
	}
}

// GetVersionContentHandler 获取指定版本内容
// @Summary      获取指定版本内容
// @Description  获取指定记忆指定版本的 markdown 内容
// @Tags         memories
// @Produce      text/plain
// @Param        id       path  string  true  "记忆ID"
// @Param        version  path  int     true  "版本号"
// @Success      200  {string}  string
// @Router       /memories/{id}/versions/{version}/content [get]
func GetVersionContentHandler(svc *Service) gin.HandlerFunc {
	return func(c *gin.Context) {
		userID := c.GetString("userId")
		if userID == "" {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "unauthorized"})
			return
		}

		memoryID := c.Param("id")
		versionNum, err := strconv.Atoi(c.Param("version"))
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid version number"})
			return
		}

		content, err := svc.GetVersionContent(c.Request.Context(), userID, memoryID, versionNum)
		if err != nil {
			c.JSON(http.StatusNotFound, gin.H{"error": "version not found"})
			return
		}

		c.Header("Content-Type", "text/markdown; charset=utf-8")
		c.String(http.StatusOK, content)
	}
}

// DeleteMemoryHandler 删除记忆
// @Summary      删除记忆
// @Description  软删除指定记忆及其所有版本记录
// @Tags         memories
// @Produce      json
// @Param        id  path  string  true  "记忆ID"
// @Success      200  {object}  object{success=boolean}
// @Router       /memories/{id} [delete]
func DeleteMemoryHandler(svc *Service) gin.HandlerFunc {
	return func(c *gin.Context) {
		userID := c.GetString("userId")
		if userID == "" {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "unauthorized"})
			return
		}

		memoryID := c.Param("id")
		if err := svc.DeleteMemory(userID, memoryID); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
			return
		}

		c.JSON(http.StatusOK, gin.H{"success": true})
	}
}
