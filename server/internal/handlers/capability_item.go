package handlers

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/costrict/costrict-web/server/internal/database"
	"github.com/costrict/costrict-web/server/internal/middleware"
	"github.com/costrict/costrict-web/server/internal/models"
	"github.com/costrict/costrict-web/server/internal/services"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"gorm.io/datatypes"
	"gorm.io/gorm"
)

// ItemHandler handles item operations with indexing support
type ItemHandler struct {
	db         *gorm.DB
	indexerSvc *services.IndexerService
	parserSvc  *services.ParserService
	zipSvc     *services.ZipService
}

// NewItemHandler creates a new item handler
func NewItemHandler(db *gorm.DB, indexerSvc *services.IndexerService, parserSvc *services.ParserService) *ItemHandler {
	var zipSvc *services.ZipService
	if parserSvc != nil {
		zipSvc = &services.ZipService{Parser: parserSvc}
	}
	return &ItemHandler{
		db:         db,
		indexerSvc: indexerSvc,
		parserSvc:  parserSvc,
		zipSvc:     zipSvc,
	}
}

func enqueueScanAsync(itemID string, revision int, triggerType string) {
	svc, ok := any(ScanJobService).(*services.ScanJobService)
	if !ok || svc == nil {
		return
	}
	go func() {
		if _, err := svc.Enqueue(itemID, revision, triggerType, "", services.ScanEnqueueOptions{}); err != nil {
			log.Printf("scan enqueue failed for item %s: %v", itemID, err)
		}
	}()
}

// ---------------------------------------------------------------------------
// Shared item creation kernel
// ---------------------------------------------------------------------------

// ErrSlugConflict is returned when an item with the same slug already exists.
var ErrSlugConflict = errors.New("slug conflict")

// createItemRequest contains all fields needed to persist a new item.
type createItemRequest struct {
	ID          string
	RegistryID  string
	Slug        string
	ItemType    string
	Name        string
	Description string
	Category    string
	Version     string
	Content     string
	Metadata    datatypes.JSON
	SourcePath  string
	SourceSHA   string
	CreatedBy   string
}

// createItemAssets holds asset and artifact records to be created alongside the item.
type createItemAssets struct {
	Records  []models.CapabilityAsset
	Artifact *models.CapabilityArtifact
}

// persistNewItem creates an item, its initial version, optional assets and artifact
// in a single DB transaction. No storage I/O or async work happens here.
func persistNewItem(db *gorm.DB, req createItemRequest, assets createItemAssets) (*models.CapabilityItem, error) {
	item := models.CapabilityItem{
		ID:          req.ID,
		RegistryID:  req.RegistryID,
		Slug:        req.Slug,
		ItemType:    req.ItemType,
		Name:        req.Name,
		Description: req.Description,
		Category:    req.Category,
		Version:     req.Version,
		Content:     req.Content,
		Metadata:    req.Metadata,
		SourcePath:  req.SourcePath,
		SourceSHA:   req.SourceSHA,
		Status:      "active",
		CreatedBy:   req.CreatedBy,
	}

	if item.Metadata == nil || len(item.Metadata) == 0 {
		item.Metadata = datatypes.JSON([]byte("{}"))
	}

	err := db.Transaction(func(tx *gorm.DB) error {
		if err := tx.Omit("Embedding").Create(&item).Error; err != nil {
			return err
		}

		version := models.CapabilityVersion{
			ID:        uuid.New().String(),
			ItemID:    item.ID,
			Revision:  1,
			Content:   item.Content,
			Metadata:  item.Metadata,
			CommitMsg: "Initial version",
			CreatedBy: item.CreatedBy,
		}
		if err := tx.Create(&version).Error; err != nil {
			return err
		}

		for i := range assets.Records {
			if assets.Records[i].ID == "" {
				assets.Records[i].ID = uuid.New().String()
			}
			assets.Records[i].ItemID = item.ID
			if err := tx.Create(&assets.Records[i]).Error; err != nil {
				return err
			}
		}

		if assets.Artifact != nil {
			// Mark existing latest artifacts as non-latest
			tx.Model(&models.CapabilityArtifact{}).Where("item_id = ? AND is_latest = ?", item.ID, true).Update("is_latest", false)
			if assets.Artifact.ID == "" {
				assets.Artifact.ID = uuid.New().String()
			}
			assets.Artifact.ItemID = item.ID
			if err := tx.Create(assets.Artifact).Error; err != nil {
				return err
			}
		}

		return nil
	})
	if err != nil {
		if strings.Contains(err.Error(), "UNIQUE constraint failed") ||
			strings.Contains(err.Error(), "duplicate key value violates unique constraint") {
			return nil, ErrSlugConflict
		}
		return nil, err
	}
	return &item, nil
}

// ItemWithAuthor represents a capability item with author information
type ItemWithAuthor struct {
	models.CapabilityItem
	CreatedByName string `json:"createdByName"`
	UpdatedByName string `json:"updatedByName"`
}

// populateItemAuthors fetches user information for items and populates author names
func populateItemAuthors(c *gin.Context, items []models.CapabilityItem) []ItemWithAuthor {
	if CasdoorClient == nil || len(items) == 0 {
		// Return items without author names if Casdoor is not configured
		result := make([]ItemWithAuthor, len(items))
		for i, item := range items {
			result[i] = ItemWithAuthor{CapabilityItem: item}
		}
		return result
	}

	// Collect unique user IDs
	userIDSet := make(map[string]bool)
	for _, item := range items {
		if item.CreatedBy != "" {
			userIDSet[item.CreatedBy] = true
		}
		if item.UpdatedBy != "" {
			userIDSet[item.UpdatedBy] = true
		}
	}

	if len(userIDSet) == 0 {
		result := make([]ItemWithAuthor, len(items))
		for i, item := range items {
			result[i] = ItemWithAuthor{CapabilityItem: item}
		}
		return result
	}

	// Convert set to slice
	userIDs := make([]string, 0, len(userIDSet))
	for id := range userIDSet {
		userIDs = append(userIDs, id)
	}

	// Get token from context
	token, _ := c.Get("accessToken")
	tokenStr, _ := token.(string)

	// Debug logging
	log.Printf("[DEBUG] populateItemAuthors: userIDs=%v, token present=%v", userIDs, tokenStr != "")

	// Fetch user information
	userMap, err := CasdoorClient.GetUsersByIDs(tokenStr, userIDs)
	if err != nil {
		log.Printf("failed to fetch users: %v", err)
		// Return items without author names on error
		result := make([]ItemWithAuthor, len(items))
		for i, item := range items {
			result[i] = ItemWithAuthor{CapabilityItem: item}
		}
		return result
	}

	log.Printf("[DEBUG] populateItemAuthors: returned %d users from Casdoor", len(userMap))

	// Populate author names
	result := make([]ItemWithAuthor, len(items))
	for i, item := range items {
		itemWithAuthor := ItemWithAuthor{CapabilityItem: item}

		if user, ok := userMap[item.CreatedBy]; ok {
			itemWithAuthor.CreatedByName = user.Name
			if user.PreferredUsername != "" {
				itemWithAuthor.CreatedByName = user.PreferredUsername
			}
		}

		if user, ok := userMap[item.UpdatedBy]; ok {
			itemWithAuthor.UpdatedByName = user.Name
			if user.PreferredUsername != "" {
				itemWithAuthor.UpdatedByName = user.PreferredUsername
			}
		}

		result[i] = itemWithAuthor
	}

	return result
}

// ListItems godoc
// @Summary      List registry items
// @Description  Get all items in a registry with author information
// @Tags         items
// @Produce      json
// @Param        id        path      string  true   "Registry ID"
// @Param        type      query     string  false  "Filter by item type"
// @Param        status    query     string  false  "Filter by status"
// @Param        search    query     string  false  "Search by name or description"
// @Param        page      query     int     false  "Page number (default: 1)"
// @Param        pageSize  query     int     false  "Page size (default: 20, max: 100)"
// @Success      200       {object}  object{items=[]ItemWithAuthor,total=integer,page=integer,pageSize=integer,hasMore=boolean}
// @Failure      500       {object}  object{error=string}
// @Router       /registries/{id}/items [get]
func ListItems(c *gin.Context) {
	registryId := c.Param("id")
	db := database.GetDB()
	var items []models.CapabilityItem
	query := db.Where("registry_id = ?", registryId)
	if itemType := c.Query("type"); itemType != "" {
		query = query.Where("item_type = ?", itemType)
	}
	if status := c.Query("status"); status != "" {
		query = query.Where("status = ?", status)
	}
	if search := c.Query("search"); search != "" {
		query = query.Where("name LIKE ? OR description LIKE ?", "%"+search+"%", "%"+search+"%")
	}

	page, _ := strconv.Atoi(c.DefaultQuery("page", "1"))
	pageSize, _ := strconv.Atoi(c.DefaultQuery("pageSize", "20"))
	if page < 1 {
		page = 1
	}
	if pageSize < 1 || pageSize > 100 {
		pageSize = 20
	}

	var total int64
	query.Model(&models.CapabilityItem{}).Count(&total)

	result := query.Order("created_at DESC").Limit(pageSize).Offset((page - 1) * pageSize).Find(&items)
	if result.Error != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to fetch items"})
		return
	}
	itemsWithAuthors := populateItemAuthors(c, items)
	c.JSON(http.StatusOK, gin.H{"items": itemsWithAuthors, "total": total, "page": page, "pageSize": pageSize, "hasMore": int64((page-1)*pageSize+pageSize) < total})
}

// CreateItem godoc
// @Summary      Create item in registry
// @Description  Create a new skill item in a specific registry
// @Tags         items
// @Accept       json
// @Produce      json
// @Param        id    path      string  true  "Registry ID"
// @Param        body  body      object{slug=string,itemType=string,name=string,description=string,category=string,version=string,content=string,sourcePath=string,createdBy=string}  true  "Item data"
// @Success      201   {object}  models.CapabilityItem
// @Failure      400   {object}  object{error=string}
// @Failure      409   {object}  object{error=string}
// @Failure      500   {object}  object{error=string}
// @Router       /registries/{id}/items [post]
func CreateItem(c *gin.Context) {
	registryId := c.Param("id")
	var req struct {
		Slug        string `json:"slug" binding:"required"`
		ItemType    string `json:"itemType" binding:"required"`
		Name        string `json:"name" binding:"required"`
		Description string `json:"description"`
		Category    string `json:"category"`
		Version     string `json:"version"`
		Content     string `json:"content"`
		SourcePath  string `json:"sourcePath"`
		CreatedBy   string `json:"createdBy" binding:"required"`
	}

	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid request"})
		return
	}

	version := req.Version
	if version == "" {
		version = "1.0.0"
	}

	db := database.GetDB()
	item, err := persistNewItem(db, createItemRequest{
		ID:          uuid.New().String(),
		RegistryID:  registryId,
		Slug:        req.Slug,
		ItemType:    req.ItemType,
		Name:        req.Name,
		Description: req.Description,
		Category:    req.Category,
		Version:     version,
		Content:     req.Content,
		Metadata:    datatypes.JSON([]byte("{}")),
		SourcePath:  req.SourcePath,
		CreatedBy:   req.CreatedBy,
	}, createItemAssets{})
	if err != nil {
		if errors.Is(err, ErrSlugConflict) {
			c.JSON(http.StatusConflict, gin.H{"error": "An item with this slug already exists", "slug": req.Slug})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to create item"})
		return
	}

	enqueueScanAsync(item.ID, 1, "create")
	c.JSON(http.StatusCreated, *item)
}

// GetItem godoc
// @Summary      Get item
// @Description  Get skill item by ID with registry, versions and artifacts
// @Tags         items
// @Produce      json
// @Param        id   path      string  true  "Item ID"
// @Success      200  {object}  ItemWithAuthor
// @Failure      404  {object}  object{error=string}
// @Router       /items/{id} [get]
func GetItem(c *gin.Context) {
	id := c.Param("id")
	db := database.GetDB()
	var item models.CapabilityItem
	result := db.Preload("Registry").Preload("Versions").Preload("Artifacts").First(&item, "id = ?", id)
	if result.Error != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Item not found"})
		return
	}
	itemsWithAuthor := populateItemAuthors(c, []models.CapabilityItem{item})
	c.JSON(http.StatusOK, itemsWithAuthor[0])
}

// UpdateItem godoc
// @Summary      Update item
// @Description  Update skill item by ID; creates a new version if content changes
// @Tags         items
// @Accept       json
// @Produce      json
// @Param        id    path      string  true  "Item ID"
// @Param        body  body      object{name=string,description=string,category=string,version=string,content=string,status=string,updatedBy=string,commitMsg=string}  false  "Item data"
// @Success      200   {object}  models.CapabilityItem
// @Failure      400   {object}  object{error=string}
// @Failure      404   {object}  object{error=string}
// @Failure      500   {object}  object{error=string}
// @Router       /items/{id} [put]
func UpdateItem(c *gin.Context) {
	id := c.Param("id")
	var req struct {
		Name        string `json:"name"`
		Description string `json:"description"`
		Category    string `json:"category"`
		Version     string `json:"version"`
		Content     string `json:"content"`
		Status      string `json:"status"`
		UpdatedBy   string `json:"updatedBy"`
		CommitMsg   string `json:"commitMsg"`
	}

	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid request"})
		return
	}

	db := database.GetDB()
	var item models.CapabilityItem
	result := db.First(&item, "id = ?", id)
	if result.Error != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Item not found"})
		return
	}

	if req.Name != "" {
		item.Name = req.Name
	}
	if req.Description != "" {
		item.Description = req.Description
	}
	if req.Category != "" {
		item.Category = req.Category
	}
	if req.Version != "" {
		item.Version = req.Version
	}
	if req.Content != "" {
		item.Content = req.Content
	}
	if req.Status != "" {
		item.Status = req.Status
	}
	if req.UpdatedBy != "" {
		item.UpdatedBy = req.UpdatedBy
	}

	if result := db.Save(&item); result.Error != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to update item"})
		return
	}

	if req.Content != "" {
		createdBy := item.UpdatedBy
		if createdBy == "" {
			createdBy = item.CreatedBy
		}
		commitMsg := req.CommitMsg
		itemID := item.ID
		itemContent := item.Content
		newRevision := 1
		_ = db.Transaction(func(tx *gorm.DB) error {
			var maxRevision int
			tx.Model(&models.CapabilityVersion{}).Where("item_id = ?", itemID).Select("COALESCE(MAX(revision), 0)").Scan(&maxRevision)
			newRevision = maxRevision + 1
			sv := models.CapabilityVersion{
				ID:        uuid.New().String(),
				ItemID:    itemID,
				Revision:  newRevision,
				Content:   itemContent,
				Metadata:  datatypes.JSON([]byte("{}")),
				CommitMsg: commitMsg,
				CreatedBy: createdBy,
			}
			return tx.Create(&sv).Error
		})
		enqueueScanAsync(itemID, newRevision, "update")
	}

	c.JSON(http.StatusOK, item)
}

// DeleteItem godoc
// @Summary      Delete item
// @Description  Delete skill item by ID
// @Tags         items
// @Produce      json
// @Param        id   path      string  true  "Item ID"
// @Success      200  {object}  object{message=string}
// @Failure      500  {object}  object{error=string}
// @Router       /items/{id} [delete]
func DeleteItem(c *gin.Context) {
	id := c.Param("id")
	db := database.GetDB()
	result := db.Delete(&models.CapabilityItem{}, "id = ?", id)
	if result.Error != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to delete item"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"message": "Item deleted"})
}

// ListItemVersions godoc
// @Summary      List item versions
// @Description  Get all versions of a skill item
// @Tags         items
// @Produce      json
// @Param        id   path      string  true  "Item ID"
// @Success      200  {object}  object{versions=[]models.CapabilityVersion}
// @Failure      500  {object}  object{error=string}
// @Router       /items/{id}/versions [get]
func ListItemVersions(c *gin.Context) {
	id := c.Param("id")
	db := database.GetDB()
	var versions []models.CapabilityVersion
	result := db.Where("item_id = ?", id).Find(&versions)
	if result.Error != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to fetch versions"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"versions": versions})
}

// GetItemVersion godoc
// @Summary      Get item version
// @Description  Get a specific version of a skill item
// @Tags         items
// @Produce      json
// @Param        id       path      string  true  "Item ID"
// @Param        version  path      integer true  "Version number"
// @Success      200      {object}  models.CapabilityVersion
// @Failure      400      {object}  object{error=string}
// @Failure      404      {object}  object{error=string}
// @Router       /items/{id}/versions/{version} [get]
func GetItemVersion(c *gin.Context) {
	id := c.Param("id")
	versionStr := c.Param("version")
	versionNum, err := strconv.Atoi(versionStr)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid version number"})
		return
	}
	db := database.GetDB()
	var version models.CapabilityVersion
	result := db.Where("item_id = ? AND revision = ?", id, versionNum).First(&version)
	if result.Error != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Version not found"})
		return
	}
	c.JSON(http.StatusOK, version)
}

func buildVisibleRegistryIDs(db *gorm.DB, userID string) []string {
	var ids []string

	var publicIDs []string
	db.Model(&models.CapabilityRegistry{}).Where("visibility = 'public'").Pluck("id", &publicIDs)
	ids = append(ids, publicIDs...)

	if userID != "" {
		var repoIDs []string
		db.Model(&models.RepoMember{}).Where("user_id = ?", userID).Pluck("repo_id", &repoIDs)
		if len(repoIDs) > 0 {
			var repoRegs []string
			db.Model(&models.CapabilityRegistry{}).Where("repo_id IN ? AND visibility = 'repo'", repoIDs).Pluck("id", &repoRegs)
			ids = append(ids, repoRegs...)
		}

		var personalRegs []string
		db.Model(&models.CapabilityRegistry{}).Where("owner_id = ?", userID).Pluck("id", &personalRegs)
		ids = append(ids, personalRegs...)
	}

	seen := make(map[string]bool)
	unique := make([]string, 0, len(ids))
	for _, id := range ids {
		if !seen[id] {
			seen[id] = true
			unique = append(unique, id)
		}
	}
	return unique
}

// ListAllItems godoc
// @Summary      List all visible items
// @Description  Get all skill items visible to the current user with pagination and author information
// @Tags         items
// @Produce      json
// @Param        type        query     string   false  "Filter by item type"
// @Param        status      query     string   false  "Filter by status (default: active)"
// @Param        search      query     string   false  "Search by name or description"
// @Param        category    query     string   false  "Filter by category"
// @Param        registryId  query     string   false  "Filter by registry ID"
// @Param        page        query     int      false  "Page number (default: 1)"
// @Param        pageSize    query     int      false  "Page size (default: 20, max: 100)"
// @Success      200         {object}  object{items=[]ItemWithAuthor,total=integer,page=integer,pageSize=integer,hasMore=boolean}
// @Failure      500         {object}  object{error=string}
// @Router       /items [get]
func ListAllItems(c *gin.Context) {
	db := database.GetDB()
	userID, _ := c.Get(middleware.UserIDKey)
	uid, _ := userID.(string)

	page, _ := strconv.Atoi(c.DefaultQuery("page", "1"))
	pageSize, _ := strconv.Atoi(c.DefaultQuery("pageSize", "20"))
	if page < 1 {
		page = 1
	}
	if pageSize < 1 || pageSize > 100 {
		pageSize = 20
	}

	registryIDs := buildVisibleRegistryIDs(db, uid)
	if len(registryIDs) == 0 {
		c.JSON(http.StatusOK, gin.H{"items": []ItemWithAuthor{}, "total": 0, "page": page, "pageSize": pageSize, "hasMore": false})
		return
	}

	query := db.Where("registry_id IN ?", registryIDs)

	if itemType := c.Query("type"); itemType != "" {
		query = query.Where("item_type = ?", itemType)
	}
	if status := c.Query("status"); status != "" {
		query = query.Where("status = ?", status)
	} else {
		query = query.Where("status = 'active'")
	}
	if search := c.Query("search"); search != "" {
		query = query.Where("name ILIKE ? OR description ILIKE ?", "%"+search+"%", "%"+search+"%")
	}
	if category := c.Query("category"); category != "" {
		query = query.Where("category = ?", category)
	}
	if registryID := c.Query("registryId"); registryID != "" {
		query = query.Where("registry_id = ?", registryID)
	}

	var total int64
	query.Model(&models.CapabilityItem{}).Count(&total)

	var items []models.CapabilityItem
	result := query.Preload("Registry").Order("created_at DESC").Limit(pageSize).Offset((page - 1) * pageSize).Find(&items)
	if result.Error != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to fetch items"})
		return
	}
	itemsWithAuthors := populateItemAuthors(c, items)
	c.JSON(http.StatusOK, gin.H{"items": itemsWithAuthors, "total": total, "page": page, "pageSize": pageSize, "hasMore": int64((page-1)*pageSize+pageSize) < total})
}

// CreateItemDirect godoc
// @Summary      Create item (direct)
// @Description  Create a skill item via JSON or upload a .zip archive via multipart/form-data. Auto-selects public registry if registryId is omitted.
// @Tags         items
// @Accept       json,multipart/form-data
// @Produce      json
// @Param        body  body      object{registryId=string,slug=string,itemType=string,name=string,description=string,category=string,version=string,content=string,createdBy=string}  false  "Item data (JSON)"
// @Param        file        formData  file    false  "Zip archive (multipart)"
// @Param        itemType    formData  string  false  "Item type: skill or mcp (multipart)"
// @Param        name        formData  string  false  "Item name (multipart)"
// @Success      201   {object}  models.CapabilityItem
// @Failure      400   {object}  object{error=string}
// @Failure      409   {object}  object{error=string}
// @Failure      500   {object}  object{error=string}
// @Router       /items [post]
func (h *ItemHandler) CreateItemDirect(c *gin.Context) {
	if strings.HasPrefix(c.ContentType(), "multipart/form-data") {
		h.createItemFromZip(c)
	} else {
		h.createItemFromJSON(c)
	}
}

// createItemFromJSON handles the original JSON-body item creation path.
func (h *ItemHandler) createItemFromJSON(c *gin.Context) {
	var req struct {
		RegistryID  string `json:"registryId"`
		Slug        string `json:"slug"`
		ItemType    string `json:"itemType" binding:"required"`
		Name        string `json:"name" binding:"required"`
		Description string `json:"description"`
		Category    string `json:"category"`
		Version     string `json:"version"`
		Content     string `json:"content"`
		CreatedBy   string `json:"createdBy"`
	}

	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid request"})
		return
	}

	userID, _ := c.Get(middleware.UserIDKey)
	uid, _ := userID.(string)
	if req.CreatedBy == "" {
		req.CreatedBy = uid
	}
	if req.CreatedBy == "" {
		req.CreatedBy = "anonymous"
	}

	registryID := req.RegistryID
	if registryID == "" {
		registryID = PublicRegistryID
	}

	if req.Slug == "" {
		req.Slug = slugify(req.Name)
	}

	version := req.Version
	if version == "" {
		version = "1.0.0"
	}

	item, err := persistNewItem(h.db, createItemRequest{
		ID:          uuid.New().String(),
		RegistryID:  registryID,
		Slug:        req.Slug,
		ItemType:    req.ItemType,
		Name:        req.Name,
		Description: req.Description,
		Category:    req.Category,
		Version:     version,
		Content:     req.Content,
		Metadata:    datatypes.JSON([]byte("{}")),
		CreatedBy:   req.CreatedBy,
	}, createItemAssets{})
	if err != nil {
		if errors.Is(err, ErrSlugConflict) {
			c.JSON(http.StatusConflict, gin.H{"error": "An item with this slug already exists", "slug": req.Slug})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to create item"})
		return
	}

	// Async index the item for semantic search
	if h.indexerSvc != nil {
		go func() {
			ctx := context.Background()
			if err := h.indexerSvc.IndexItem(ctx, item); err != nil {
				log.Printf("Failed to index item %s: %v", item.ID, err)
			}
		}()
	}

	enqueueScanAsync(item.ID, 1, "create")
	c.JSON(http.StatusCreated, *item)
}

// cleanupStorageKeys deletes uploaded objects after a later step fails.
func cleanupStorageKeys(keys []string) {
	if StorageBackend == nil {
		return
	}
	ctx := context.Background()
	for _, key := range keys {
		_ = StorageBackend.Delete(ctx, key)
	}
}

// createItemFromZip handles multipart/form-data zip upload item creation.
func (h *ItemHandler) createItemFromZip(c *gin.Context) {
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, services.MaxZipUploadSize)

	file, header, err := c.Request.FormFile("file")
	if err != nil {
		var maxBytesErr *http.MaxBytesError
		if errors.As(err, &maxBytesErr) {
			c.JSON(http.StatusRequestEntityTooLarge, gin.H{"error": "Zip upload exceeds maximum size"})
			return
		}
		c.JSON(http.StatusBadRequest, gin.H{"error": "Failed to read file"})
		return
	}
	defer file.Close()

	if header.Size <= 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "File is empty"})
		return
	}

	itemType := c.PostForm("itemType")
	name := c.PostForm("name")
	slug := c.PostForm("slug")
	registryID := c.PostForm("registryId")
	description := c.PostForm("description")
	category := c.PostForm("category")
	version := c.PostForm("version")
	createdByForm := c.PostForm("createdBy")

	if itemType == "" || name == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "itemType and name are required"})
		return
	}

	switch itemType {
	case "skill", "mcp":
	default:
		c.JSON(http.StatusBadRequest, gin.H{"error": "itemType must be either skill or mcp"})
		return
	}

	if registryID == "" {
		registryID = PublicRegistryID
	}

	userIDVal, _ := c.Get(middleware.UserIDKey)
	createdBy, _ := userIDVal.(string)
	if createdBy == "" {
		createdBy = createdByForm
	}
	if createdBy == "" {
		createdBy = "anonymous"
	}

	if slug == "" {
		slug = slugify(name)
	}

	if h.zipSvc == nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Zip upload is not configured"})
		return
	}
	if StorageBackend == nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Storage backend is not configured"})
		return
	}

	readerAt, ok := file.(io.ReaderAt)
	if !ok {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Uploaded file is not seekable"})
		return
	}

	result, err := h.zipSvc.ParseZip(readerAt, header.Size, itemType)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if result == nil || result.Parsed == nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Zip parser returned no item"})
		return
	}

	metadataMap := result.Parsed.Metadata
	if itemType == "mcp" {
		metadataMap = result.NormalizedMeta
	}
	if metadataMap == nil {
		metadataMap = map[string]any{}
	}
	metadataJSON, err := json.Marshal(metadataMap)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to encode metadata"})
		return
	}

	if description == "" {
		description = result.Parsed.Description
	}
	if category == "" {
		category = result.Parsed.Category
	}
	if version == "" {
		version = result.Parsed.Version
	}
	if version == "" {
		version = "1.0.0"
	}

	itemID := uuid.New().String()
	ctx := context.Background()
	uploadedKeys := make([]string, 0, len(result.Assets)+1)
	assetStorageKeys := make(map[string]string, len(result.Assets))

	for _, asset := range result.Assets {
		if !asset.Binary {
			continue
		}
		storageKey := fmt.Sprintf("%s/assets/%s", itemID, asset.Path)
		if err := StorageBackend.Put(ctx, storageKey, bytes.NewReader(asset.Content), asset.Size); err != nil {
			cleanupStorageKeys(uploadedKeys)
			c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to store zip assets"})
			return
		}
		uploadedKeys = append(uploadedKeys, storageKey)
		assetStorageKeys[asset.Path] = storageKey
	}

	seeker, ok := file.(io.Seeker)
	if !ok {
		cleanupStorageKeys(uploadedKeys)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Uploaded file is not seekable"})
		return
	}
	if _, err := seeker.Seek(0, io.SeekStart); err != nil {
		cleanupStorageKeys(uploadedKeys)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to rewind uploaded file"})
		return
	}

	zipKey := fmt.Sprintf("%s/_package.zip", itemID)
	hasher := sha256.New()
	tee := io.TeeReader(file, hasher)
	if err := StorageBackend.Put(ctx, zipKey, tee, header.Size); err != nil {
		cleanupStorageKeys(uploadedKeys)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to store uploaded zip"})
		return
	}
	uploadedKeys = append(uploadedKeys, zipKey)
	checksum := hex.EncodeToString(hasher.Sum(nil))

	assetRecords := make([]models.CapabilityAsset, 0, len(result.Assets))
	for _, asset := range result.Assets {
		if asset.Binary {
			assetRecords = append(assetRecords, models.CapabilityAsset{
				RelPath:        asset.Path,
				StorageBackend: "local",
				StorageKey:     assetStorageKeys[asset.Path],
				MimeType:       asset.MimeType,
				FileSize:       asset.Size,
				ContentSHA:     asset.ContentSHA,
			})
			continue
		}
		text := string(asset.Content)
		assetRecords = append(assetRecords, models.CapabilityAsset{
			RelPath:     asset.Path,
			TextContent: &text,
			MimeType:    asset.MimeType,
			FileSize:    asset.Size,
			ContentSHA:  asset.ContentSHA,
		})
	}

	item, err := persistNewItem(h.db, createItemRequest{
		ID:          itemID,
		RegistryID:  registryID,
		Slug:        slug,
		ItemType:    itemType,
		Name:        name,
		Description: description,
		Category:    category,
		Version:     version,
		Content:     result.MainContent,
		Metadata:    datatypes.JSON(metadataJSON),
		SourcePath:  result.MainPath,
		SourceSHA:   result.MainSHA,
		CreatedBy:   createdBy,
	}, createItemAssets{
		Records: assetRecords,
		Artifact: &models.CapabilityArtifact{
			Filename:        "_package.zip",
			FileSize:        header.Size,
			ChecksumSHA256:  checksum,
			MimeType:        "application/zip",
			StorageBackend:  "local",
			StorageKey:      zipKey,
			ArtifactVersion: version,
			IsLatest:        true,
			SourceType:      "upload",
			UploadedBy:      createdBy,
			CreatedAt:       time.Now(),
		},
	})
	if err != nil {
		cleanupStorageKeys(uploadedKeys)
		if errors.Is(err, ErrSlugConflict) {
			c.JSON(http.StatusConflict, gin.H{"error": "An item with this slug already exists", "slug": slug})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to create item"})
		return
	}

	if h.indexerSvc != nil {
		go func() {
			ctx := context.Background()
			if err := h.indexerSvc.IndexItem(ctx, item); err != nil {
				log.Printf("Failed to index item %s: %v", item.ID, err)
			}
		}()
	}

	enqueueScanAsync(item.ID, 1, "create")
	c.JSON(http.StatusCreated, *item)
}

// MoveItem godoc
// @Summary      Move item to another registry
// @Description  Move a capability item to a different registry. Target registry must belong to a non-sync repository. Caller must be the item creator, or owner/admin of the source repo. Caller must be a member of the target repo.
// @Tags         items
// @Accept       json
// @Produce      json
// @Param        id    path      string  true  "Item ID"
// @Param        body  body      object{targetRegistryId=string}  true  "Target registry ID"
// @Success      200   {object}  models.CapabilityItem
// @Failure      400   {object}  object{error=string}
// @Failure      403   {object}  object{error=string}
// @Failure      404   {object}  object{error=string}
// @Failure      409   {object}  object{error=string}
// @Failure      500   {object}  object{error=string}
// @Router       /items/{id}/move [put]
func MoveItem(c *gin.Context) {
	id := c.Param("id")

	var req struct {
		TargetRegistryID string `json:"targetRegistryId" binding:"required"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid request"})
		return
	}

	callerIDVal, _ := c.Get(middleware.UserIDKey)
	callerID, _ := callerIDVal.(string)
	if callerID == "" {
		c.JSON(http.StatusForbidden, gin.H{"error": "Authentication required"})
		return
	}

	db := database.GetDB()

	var item models.CapabilityItem
	if db.Preload("Registry").First(&item, "id = ?", id).Error != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Item not found"})
		return
	}

	sourceReg := item.Registry
	if sourceReg == nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Source registry not found"})
		return
	}

	isCreator := item.CreatedBy == callerID
	isSourceRepoAdmin := sourceReg.RepoID != "" && isRepoAdmin(getCallerRepoRole(c, sourceReg.RepoID))
	if !isCreator && !isSourceRepoAdmin {
		c.JSON(http.StatusForbidden, gin.H{"error": "Only the item creator or source repo admin can move this item"})
		return
	}

	if req.TargetRegistryID == item.RegistryID {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Item already belongs to the target registry"})
		return
	}

	var targetReg models.CapabilityRegistry
	if db.First(&targetReg, "id = ?", req.TargetRegistryID).Error != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Target registry not found"})
		return
	}

	if targetReg.SourceType == "external" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Cannot move item to a sync-type registry"})
		return
	}

	if targetReg.RepoID != "" && targetReg.RepoID != "public" {
		var targetRepo models.Repository
		if db.First(&targetRepo, "id = ?", targetReg.RepoID).Error != nil {
			c.JSON(http.StatusNotFound, gin.H{"error": "Target repository not found"})
			return
		}
		if targetRepo.RepoType == "sync" {
			c.JSON(http.StatusBadRequest, gin.H{"error": "Cannot move item to a sync-type repository"})
			return
		}

		var targetMember models.RepoMember
		if db.Where("repo_id = ? AND user_id = ?", targetReg.RepoID, callerID).First(&targetMember).Error != nil {
			c.JSON(http.StatusForbidden, gin.H{"error": "You must be a member of the target repository"})
			return
		}
	}

	// Global slug uniqueness: slug must be unique across all registries
	var conflictCount int64
	db.Model(&models.CapabilityItem{}).
		Where("slug = ? AND id != ?", item.Slug, item.ID).
		Count(&conflictCount)
	if conflictCount > 0 {
		c.JSON(http.StatusConflict, gin.H{"error": "An item with this slug already exists", "slug": item.Slug})
		return
	}

	if err := db.Model(&item).Update("registry_id", req.TargetRegistryID).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to move item"})
		return
	}

	item.RegistryID = req.TargetRegistryID
	c.JSON(http.StatusOK, item)
}

// TransferItemToRepo godoc
// @Summary      Transfer item to another repository
// @Description  Transfer a capability item to a different repository. The system will automatically find the target repository's internal registry. Target repository must be a non-sync type. Caller must be the item creator, or owner/admin of the source repo. Caller must be a member of the target repo.
// @Tags         items
// @Accept       json
// @Produce      json
// @Param        id    path      string  true  "Item ID"
// @Param        body  body      object{targetRepoId=string}  true  "Target repository ID"
// @Success      200   {object}  models.CapabilityItem
// @Failure      400   {object}  object{error=string}
// @Failure      403   {object}  object{error=string}
// @Failure      404   {object}  object{error=string}
// @Failure      409   {object}  object{error=string}
// @Failure      500   {object}  object{error=string}
// @Router       /items/{id}/transfer [put]
func TransferItemToRepo(c *gin.Context) {
	id := c.Param("id")

	var req struct {
		TargetRepoID string `json:"targetRepoId" binding:"required"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid request"})
		return
	}

	callerIDVal, _ := c.Get(middleware.UserIDKey)
	callerID, _ := callerIDVal.(string)
	if callerID == "" {
		c.JSON(http.StatusForbidden, gin.H{"error": "Authentication required"})
		return
	}

	db := database.GetDB()

	var item models.CapabilityItem
	if db.Preload("Registry").First(&item, "id = ?", id).Error != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Item not found"})
		return
	}

	sourceReg := item.Registry
	if sourceReg == nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Source registry not found"})
		return
	}

	isCreator := item.CreatedBy == callerID
	isSourceRepoAdmin := sourceReg.RepoID != "" && isRepoAdmin(getCallerRepoRole(c, sourceReg.RepoID))
	if !isCreator && !isSourceRepoAdmin {
		c.JSON(http.StatusForbidden, gin.H{"error": "Only the item creator or source repo admin can transfer this item"})
		return
	}

	var targetRepo models.Repository
	if db.First(&targetRepo, "id = ?", req.TargetRepoID).Error != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Target repository not found"})
		return
	}

	if targetRepo.RepoType == "sync" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Cannot transfer item to a sync-type repository"})
		return
	}

	if sourceReg.RepoID == req.TargetRepoID {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Item already belongs to the target repository"})
		return
	}

	var targetMember models.RepoMember
	if db.Where("repo_id = ? AND user_id = ?", req.TargetRepoID, callerID).First(&targetMember).Error != nil {
		c.JSON(http.StatusForbidden, gin.H{"error": "You must be a member of the target repository"})
		return
	}

	var targetReg models.CapabilityRegistry
	if db.Where("repo_id = ? AND source_type = 'internal'", req.TargetRepoID).First(&targetReg).Error != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Target repository does not have an internal registry"})
		return
	}

	if targetReg.ID == item.RegistryID {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Item already belongs to the target registry"})
		return
	}

	// Global slug uniqueness: slug must be unique across all registries
	var conflictCount int64
	db.Model(&models.CapabilityItem{}).
		Where("slug = ? AND id != ?", item.Slug, item.ID).
		Count(&conflictCount)
	if conflictCount > 0 {
		c.JSON(http.StatusConflict, gin.H{"error": "An item with this slug already exists", "slug": item.Slug})
		return
	}

	if err := db.Model(&item).Update("registry_id", targetReg.ID).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to transfer item"})
		return
	}

	item.RegistryID = targetReg.ID
	c.JSON(http.StatusOK, item)
}

func slugify(name string) string {
	result := make([]byte, 0, len(name))
	prevDash := false
	for i := 0; i < len(name); i++ {
		c := name[i]
		if (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') {
			result = append(result, c)
			prevDash = false
		} else if c >= 'A' && c <= 'Z' {
			result = append(result, c+32)
			prevDash = false
		} else if !prevDash && len(result) > 0 {
			result = append(result, '-')
			prevDash = true
		}
	}
	if len(result) > 0 && result[len(result)-1] == '-' {
		result = result[:len(result)-1]
	}
	return string(result)
}

// GetPublicRegistry godoc
// @Summary      Get public registry
// @Description  Get the global public skill registry
// @Tags         registries
// @Produce      json
// @Success      200  {object}  models.CapabilityRegistry
// @Failure      404  {object}  object{error=string}
// @Router       /registries/public [get]
func GetPublicRegistry(c *gin.Context) {
	db := database.GetDB()
	var registry models.CapabilityRegistry
	result := db.First(&registry, "id = ?", PublicRegistryID)
	if result.Error != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Public registry not found"})
		return
	}
	c.JSON(http.StatusOK, registry)
}
