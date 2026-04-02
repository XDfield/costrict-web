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
	"path/filepath"
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
	archiveSvc *services.ArchiveService
}

// NewItemHandler creates a new item handler
func NewItemHandler(db *gorm.DB, indexerSvc *services.IndexerService, parserSvc *services.ParserService) *ItemHandler {
	var archiveSvc *services.ArchiveService
	if parserSvc != nil {
		archiveSvc = &services.ArchiveService{Parser: parserSvc}
	}
	return &ItemHandler{
		db:         db,
		indexerSvc: indexerSvc,
		parserSvc:  parserSvc,
		archiveSvc: archiveSvc,
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
	RepoID      string
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
	SourceType  string
}

// createItemAssets holds asset and artifact records to be created alongside the item.
type createItemAssets struct {
	Records  []models.CapabilityAsset
	Artifact *models.CapabilityArtifact
}

// resolveMetadata returns the JSON-encoded metadata for an item.
// For MCP items it normalises the raw payload to standard MCP format.
// When metadata is absent but content holds a valid .mcp.json body,
// the metadata is derived from content automatically.
// Returns an error when MCP metadata cannot be normalised.
func resolveMetadata(itemType string, raw json.RawMessage, content string) (datatypes.JSON, error) {
	if itemType == "mcp" {
		src := json.RawMessage(raw)
		if len(src) == 0 && content != "" {
			src = json.RawMessage(content)
		}
		if len(src) == 0 {
			return nil, fmt.Errorf("MCP item requires metadata or .mcp.json content")
		}
		var m map[string]any
		if err := json.Unmarshal(src, &m); err != nil {
			return nil, fmt.Errorf("invalid metadata JSON: %w", err)
		}
		norm, err := services.NormalizeMCPMetadata(m)
		if err != nil {
			return nil, fmt.Errorf("invalid MCP metadata: %w", err)
		}
		b, err := json.Marshal(norm)
		if err != nil {
			return nil, fmt.Errorf("failed to marshal metadata: %w", err)
		}
		return datatypes.JSON(b), nil
	}
	if len(raw) > 0 {
		return datatypes.JSON(raw), nil
	}
	return datatypes.JSON([]byte("{}")), nil
}

// registryRepoID returns the repo_id associated with a registry.
// Falls back to "public" when the registry has no repo_id set.
func registryRepoID(db *gorm.DB, registryID string) string {
	var repoID string
	db.Model(&models.CapabilityRegistry{}).Select("repo_id").Where("id = ?", registryID).Scan(&repoID)
	if repoID == "" {
		return "public"
	}
	return repoID
}

// persistNewItem creates an item, its initial version, optional assets and artifact
// in a single DB transaction. No storage I/O or async work happens here.
func persistNewItem(db *gorm.DB, req createItemRequest, assets createItemAssets) (*models.CapabilityItem, error) {
	item := models.CapabilityItem{
		ID:          req.ID,
		RegistryID:  req.RegistryID,
		RepoID:      req.RepoID,
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
		SourceType:  req.SourceType,
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

// ItemResponse wraps a CapabilityItem with optional repo visibility,
// keeping a flat JSON structure compatible with the previous ItemWithAuthor.
type ItemResponse struct {
	models.CapabilityItem
	RepoVisibility string `json:"repoVisibility,omitempty"`
	Favorited      bool   `json:"favorited"`
}

func itemListSortOrder(sortBy, sortOrder string) string {
	column := map[string]string{
		"":              "created_at",
		"createdAt":     "created_at",
		"previewCount":  "preview_count",
		"installCount":  "install_count",
		"favoriteCount": "favorite_count",
	}[sortBy]
	if column == "" {
		column = "created_at"
	}

	direction := "DESC"
	if strings.EqualFold(sortOrder, "asc") {
		direction = "ASC"
	}

	return fmt.Sprintf("%s %s, created_at DESC", column, direction)
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
// @Param        sortBy    query     string  false  "Sort by createdAt, previewCount, installCount, or favoriteCount"
// @Param        sortOrder query     string  false  "Sort order: asc or desc (default: desc)"
// @Success      200       {object}  object{items=[]models.CapabilityItem,total=integer,page=integer,pageSize=integer,hasMore=boolean}
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
		like := database.ILike(db)
		query = query.Where(fmt.Sprintf("name %s ? OR description %s ?", like, like), "%"+search+"%", "%"+search+"%")
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

	result := query.Order(itemListSortOrder(c.Query("sortBy"), c.Query("sortOrder"))).Limit(pageSize).Offset((page - 1) * pageSize).Find(&items)
	if result.Error != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to fetch items"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"items": items, "total": total, "page": page, "pageSize": pageSize, "hasMore": int64((page-1)*pageSize+pageSize) < total})
}

// CreateItem godoc
// @Summary      Create item in registry
// @Description  Create a new skill item in a specific registry
// @Tags         items
// @Accept       json
// @Produce      json
// @Param        id    path      string  true  "Registry ID"
// @Param        body  body      object{slug=string,itemType=string,name=string,description=string,category=string,version=string,content=string,metadata=object,sourcePath=string,createdBy=string}  true  "Item data"
// @Success      201   {object}  models.CapabilityItem
// @Failure      400   {object}  object{error=string}
// @Failure      409   {object}  object{error=string}
// @Failure      500   {object}  object{error=string}
// @Router       /registries/{id}/items [post]
func CreateItem(c *gin.Context) {
	registryId := c.Param("id")
	var req struct {
		Slug        string          `json:"slug" binding:"required"`
		ItemType    string          `json:"itemType" binding:"required"`
		Name        string          `json:"name" binding:"required"`
		Description string          `json:"description"`
		Category    string          `json:"category"`
		Version     string          `json:"version"`
		Content     string          `json:"content"`
		Metadata    json.RawMessage `json:"metadata"`
		SourcePath  string          `json:"sourcePath"`
		CreatedBy   string          `json:"createdBy" binding:"required"`
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
	metadata, err := resolveMetadata(req.ItemType, req.Metadata, req.Content)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	item, err := persistNewItem(db, createItemRequest{
		ID:          uuid.New().String(),
		RegistryID:  registryId,
		RepoID:      registryRepoID(db, registryId),
		Slug:        req.Slug,
		ItemType:    req.ItemType,
		Name:        req.Name,
		Description: req.Description,
		Category:    req.Category,
		Version:     version,
		Content:     req.Content,
		Metadata:    metadata,
		SourcePath:  req.SourcePath,
		CreatedBy:   req.CreatedBy,
		SourceType:  "direct",
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
// @Description  Get skill item by ID with registry, versions, artifacts and repo visibility
// @Tags         items
// @Produce      json
// @Param        id   path      string  true  "Item ID"
// @Success      200  {object}  ItemResponse
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
	resp := ItemResponse{CapabilityItem: item}
	// Populate repo visibility from the parent repository
	if item.Registry != nil {
		resp.RepoVisibility = getRepoVisibility(item.Registry.RepoID)
	}
	if userID := c.GetString(middleware.UserIDKey); userID != "" {
		var count int64
		if err := db.Model(&models.ItemFavorite{}).
			Where("item_id = ? AND user_id = ?", item.ID, userID).
			Count(&count).Error; err == nil {
			resp.Favorited = count > 0
		}
	}

	c.JSON(http.StatusOK, resp)
}

// UpdateItem godoc
// @Summary      Update item
// @Description  Update skill item by ID. Accepts JSON for field updates or multipart/form-data with a .zip, .tar.gz, or .tgz archive. Creates a new version if content changes.
// @Tags         items
// @Accept       json,mpfd
// @Produce      json
// @Param        id    path      string  true  "Item ID"
// @Param        body  body      object{name=string,description=string,category=string,version=string,content=string,status=string,updatedBy=string,commitMsg=string}  false  "Item data (JSON)"
// @Param        file  formData  file    false "Archive file (multipart)"
// @Success      200   {object}  models.CapabilityItem
// @Failure      400   {object}  object{error=string}
// @Failure      404   {object}  object{error=string}
// @Failure      500   {object}  object{error=string}
// @Router       /items/{id} [put]
func (h *ItemHandler) UpdateItem(c *gin.Context) {
	if strings.HasPrefix(c.ContentType(), "multipart/form-data") {
		h.updateItemFromArchive(c)
	} else {
		h.updateItemFromJSON(c)
	}
}

// updateItemFromJSON handles the JSON-body item update path.
func (h *ItemHandler) updateItemFromJSON(c *gin.Context) {
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

	userID, _ := c.Get(middleware.UserIDKey)
	uid, _ := userID.(string)
	if req.UpdatedBy == "" {
		req.UpdatedBy = uid
	}

	db := h.db
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
		if item.ItemType == "mcp" {
			meta, err := resolveMetadata("mcp", nil, req.Content)
			if err != nil {
				c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
				return
			}
			item.Metadata = meta
		}
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

// updateItemFromArchive handles multipart/form-data archive upload item update.
func (h *ItemHandler) updateItemFromArchive(c *gin.Context) {
	id := c.Param("id")
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, services.MaxArchiveUploadSize)

	db := h.db
	var item models.CapabilityItem
	if db.First(&item, "id = ?", id).Error != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Item not found"})
		return
	}

	file, header, err := c.Request.FormFile("file")
	if err != nil {
		var maxBytesErr *http.MaxBytesError
		if errors.As(err, &maxBytesErr) {
			c.JSON(http.StatusRequestEntityTooLarge, gin.H{"error": "Archive upload exceeds maximum size"})
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

	if h.archiveSvc == nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Archive upload is not configured"})
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

	result, err := h.archiveSvc.ParseArchive(readerAt, header.Size, header.Filename, item.ItemType)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if result == nil || result.Parsed == nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Archive parser returned no item"})
		return
	}

	// Read optional form fields for overrides.
	userIDVal, _ := c.Get(middleware.UserIDKey)
	updatedBy, _ := userIDVal.(string)
	if v := c.PostForm("updatedBy"); v != "" {
		updatedBy = v
	}
	if updatedBy == "" {
		updatedBy = item.CreatedBy
	}
	commitMsg := c.PostForm("commitMsg")

	// Build metadata.
	metadataMap := result.Parsed.Metadata
	if item.ItemType == "mcp" {
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

	// Apply form overrides or parsed values.
	if v := c.PostForm("name"); v != "" {
		item.Name = v
	} else if result.Parsed.Name != "" {
		item.Name = result.Parsed.Name
	}
	if v := c.PostForm("description"); v != "" {
		item.Description = v
	} else if result.Parsed.Description != "" {
		item.Description = result.Parsed.Description
	}
	if v := c.PostForm("category"); v != "" {
		item.Category = v
	} else if result.Parsed.Category != "" {
		item.Category = result.Parsed.Category
	}
	newVersion := c.PostForm("version")
	if newVersion == "" {
		newVersion = result.Parsed.Version
	}
	if newVersion == "" {
		newVersion = item.Version
	}

	ctx := c.Request.Context()
	itemID := item.ID

	// Upload new binary assets to storage.
	uploadedKeys := make([]string, 0, len(result.Assets)+1)
	assetStorageKeys := make(map[string]string, len(result.Assets))

	for _, asset := range result.Assets {
		if !asset.Binary {
			continue
		}
		storageKey := fmt.Sprintf("%s/assets/%s", itemID, asset.Path)
		if err := StorageBackend.Put(ctx, storageKey, bytes.NewReader(asset.Content), asset.Size); err != nil {
			cleanupStorageKeys(uploadedKeys)
			c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to store archive assets"})
			return
		}
		uploadedKeys = append(uploadedKeys, storageKey)
		assetStorageKeys[asset.Path] = storageKey
	}

	// Upload the archive file itself.
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

	uploadedFilename := filepath.Base(header.Filename)
	zipKey := fmt.Sprintf("%s/%s", itemID, uploadedFilename)
	hasher := sha256.New()
	tee := io.TeeReader(file, hasher)
	if err := StorageBackend.Put(ctx, zipKey, tee, header.Size); err != nil {
		cleanupStorageKeys(uploadedKeys)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to store uploaded archive"})
		return
	}
	uploadedKeys = append(uploadedKeys, zipKey)
	checksum := hex.EncodeToString(hasher.Sum(nil))

	// Build new asset records.
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

	// Collect old asset storage keys for cleanup after successful transaction.
	var oldAssets []models.CapabilityAsset
	db.Where("item_id = ?", itemID).Find(&oldAssets)
	oldStorageKeys := make([]string, 0)
	for _, a := range oldAssets {
		if a.StorageKey != "" {
			oldStorageKeys = append(oldStorageKeys, a.StorageKey)
		}
	}

	// Single transaction: update item, replace assets, create version + artifact.
	item.Content = result.MainContent
	item.Metadata = datatypes.JSON(metadataJSON)
	item.SourcePath = result.MainPath
	item.SourceSHA = result.MainSHA
	item.SourceType = "archive"
	item.Version = newVersion
	item.UpdatedBy = updatedBy

	txErr := db.Transaction(func(tx *gorm.DB) error {
		if err := tx.Save(&item).Error; err != nil {
			return err
		}

		// Replace assets: delete old, insert new.
		if err := tx.Where("item_id = ?", itemID).Delete(&models.CapabilityAsset{}).Error; err != nil {
			return err
		}
		for i := range assetRecords {
			assetRecords[i].ID = uuid.New().String()
			assetRecords[i].ItemID = itemID
			if err := tx.Create(&assetRecords[i]).Error; err != nil {
				return err
			}
		}

		// Mark old artifacts as non-latest, create new one.
		tx.Model(&models.CapabilityArtifact{}).Where("item_id = ? AND is_latest = ?", itemID, true).Update("is_latest", false)
		artifact := models.CapabilityArtifact{
			ID:              uuid.New().String(),
			ItemID:          itemID,
			Filename:        uploadedFilename,
			FileSize:        header.Size,
			ChecksumSHA256:  checksum,
			MimeType:        services.ArchiveMimeType(header.Filename),
			StorageBackend:  "local",
			StorageKey:      zipKey,
			ArtifactVersion: newVersion,
			IsLatest:        true,
			SourceType:      "upload",
			UploadedBy:      updatedBy,
			CreatedAt:       time.Now(),
		}
		if err := tx.Create(&artifact).Error; err != nil {
			return err
		}

		// Create new version.
		var maxRevision int
		tx.Model(&models.CapabilityVersion{}).Where("item_id = ?", itemID).Select("COALESCE(MAX(revision), 0)").Scan(&maxRevision)
		newRevision := maxRevision + 1
		sv := models.CapabilityVersion{
			ID:        uuid.New().String(),
			ItemID:    itemID,
			Revision:  newRevision,
			Content:   item.Content,
			Metadata:  item.Metadata,
			CommitMsg: commitMsg,
			CreatedBy: updatedBy,
		}
		if err := tx.Create(&sv).Error; err != nil {
			return err
		}

		enqueueScanAsync(itemID, newRevision, "update")
		return nil
	})
	if txErr != nil {
		cleanupStorageKeys(uploadedKeys)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to update item"})
		return
	}

	// Cleanup old storage keys only after successful commit.
	cleanupStorageKeys(oldStorageKeys)

	if h.indexerSvc != nil {
		go func() {
			bgCtx := context.Background()
			if err := h.indexerSvc.IndexItem(bgCtx, &item); err != nil {
				log.Printf("Failed to index item %s: %v", item.ID, err)
			}
		}()
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

	// Registries belonging to public repos (including virtual "public" repo)
	var publicIDs []string
	db.Model(&models.CapabilityRegistry{}).
		Where("repo_id IN (SELECT CAST(id AS TEXT) FROM repositories WHERE visibility = 'public') OR repo_id = 'public'").
		Pluck("id", &publicIDs)
	ids = append(ids, publicIDs...)

	if userID != "" {
		// Registries belonging to private repos the user is a member of
		var repoIDs []string
		db.Model(&models.RepoMember{}).Where("user_id = ?", userID).Pluck("repo_id", &repoIDs)
		if len(repoIDs) > 0 {
			var repoRegs []string
			db.Model(&models.CapabilityRegistry{}).
				Where("repo_id IN ? AND repo_id NOT IN (SELECT CAST(id AS TEXT) FROM repositories WHERE visibility = 'public')", repoIDs).
				Pluck("id", &repoRegs)
			ids = append(ids, repoRegs...)
		}
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
// @Param        sortBy      query     string   false  "Sort by createdAt, previewCount, installCount, or favoriteCount"
// @Param        sortOrder   query     string   false  "Sort order: asc or desc (default: desc)"
// @Success      200         {object}  object{items=[]object,total=integer,page=integer,pageSize=integer,hasMore=boolean}
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
		c.JSON(http.StatusOK, gin.H{"items": []models.CapabilityItem{}, "total": 0, "page": page, "pageSize": pageSize, "hasMore": false})
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
		like := database.ILike(db)
		query = query.Where(fmt.Sprintf("name %s ? OR description %s ?", like, like), "%"+search+"%", "%"+search+"%")
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
	result := query.Preload("Registry").Order(itemListSortOrder(c.Query("sortBy"), c.Query("sortOrder"))).Limit(pageSize).Offset((page - 1) * pageSize).Find(&items)
	if result.Error != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to fetch items"})
		return
	}

	// Collect unique repo IDs from preloaded registries and batch-fetch repo names
	repoIDSet := make(map[string]bool)
	for _, item := range items {
		if item.Registry != nil && item.Registry.RepoID != "" {
			repoIDSet[item.Registry.RepoID] = true
		}
	}
	repoNameMap := make(map[string]string)
	if len(repoIDSet) > 0 {
		repoIDs := make([]string, 0, len(repoIDSet))
		for id := range repoIDSet {
			if id != "public" {
				repoIDs = append(repoIDs, id)
			}
		}
		if len(repoIDs) > 0 {
			var repos []models.Repository
			db.Select("id, name").Where("id IN ?", repoIDs).Find(&repos)
			for _, repo := range repos {
				repoNameMap[repo.ID] = repo.Name
			}
		}
	}

	// Populate repoName into each item
	type ItemWithRepo struct {
		models.CapabilityItem
		RepoName string `json:"repoName,omitempty"`
	}
	out := make([]ItemWithRepo, len(items))
	for i, item := range items {
		out[i] = ItemWithRepo{CapabilityItem: item}
		if item.Registry != nil {
			out[i].RepoName = repoNameMap[item.Registry.RepoID]
		}
	}

	c.JSON(http.StatusOK, gin.H{"items": out, "total": total, "page": page, "pageSize": pageSize, "hasMore": int64((page-1)*pageSize+pageSize) < total})
}

// CreateItemDirect godoc
// @Summary      Create item (direct)
// @Description  Create a skill item via JSON or upload a .zip, .tar.gz, or .tgz archive via multipart/form-data. Auto-selects public registry if registryId is omitted.
// @Tags         items
// @Accept       json,multipart/form-data
// @Produce      json
// @Param        body  body      object{registryId=string,slug=string,itemType=string,name=string,description=string,category=string,version=string,content=string,metadata=object,createdBy=string}  false  "Item data (JSON)"
// @Param        file        formData  file    false  "Archive file (.zip, .tar.gz, or .tgz) (multipart)"
// @Param        itemType    formData  string  false  "Item type: skill or mcp (multipart)"
// @Param        name        formData  string  false  "Item name (multipart)"
// @Success      201   {object}  models.CapabilityItem
// @Failure      400   {object}  object{error=string}
// @Failure      409   {object}  object{error=string}
// @Failure      500   {object}  object{error=string}
// @Router       /items [post]
func (h *ItemHandler) CreateItemDirect(c *gin.Context) {
	if strings.HasPrefix(c.ContentType(), "multipart/form-data") {
		h.createItemFromArchive(c)
	} else {
		h.createItemFromJSON(c)
	}
}

// createItemFromJSON handles the original JSON-body item creation path.
func (h *ItemHandler) createItemFromJSON(c *gin.Context) {
	var req struct {
		RegistryID  string          `json:"registryId"`
		Slug        string          `json:"slug"`
		ItemType    string          `json:"itemType" binding:"required"`
		Name        string          `json:"name" binding:"required"`
		Description string          `json:"description"`
		Category    string          `json:"category"`
		Version     string          `json:"version"`
		Content     string          `json:"content"`
		Metadata    json.RawMessage `json:"metadata"`
		CreatedBy   string          `json:"createdBy"`
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

	metadata, err := resolveMetadata(req.ItemType, req.Metadata, req.Content)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	item, err := persistNewItem(h.db, createItemRequest{
		ID:          uuid.New().String(),
		RegistryID:  registryID,
		RepoID:      registryRepoID(h.db, registryID),
		Slug:        req.Slug,
		ItemType:    req.ItemType,
		Name:        req.Name,
		Description: req.Description,
		Category:    req.Category,
		Version:     version,
		Content:     req.Content,
		Metadata:    metadata,
		CreatedBy:   req.CreatedBy,
		SourceType:  "direct",
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

// createItemFromArchive handles multipart/form-data archive upload item creation.
func (h *ItemHandler) createItemFromArchive(c *gin.Context) {
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, services.MaxArchiveUploadSize)

	file, header, err := c.Request.FormFile("file")
	if err != nil {
		var maxBytesErr *http.MaxBytesError
		if errors.As(err, &maxBytesErr) {
			c.JSON(http.StatusRequestEntityTooLarge, gin.H{"error": "Archive upload exceeds maximum size"})
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

	if h.archiveSvc == nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Archive upload is not configured"})
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

	result, err := h.archiveSvc.ParseArchive(readerAt, header.Size, header.Filename, itemType)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if result == nil || result.Parsed == nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Archive parser returned no item"})
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
			c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to store archive assets"})
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

	uploadedFilename := filepath.Base(header.Filename)
	zipKey := fmt.Sprintf("%s/%s", itemID, uploadedFilename)
	hasher := sha256.New()
	tee := io.TeeReader(file, hasher)
	if err := StorageBackend.Put(ctx, zipKey, tee, header.Size); err != nil {
		cleanupStorageKeys(uploadedKeys)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to store uploaded archive"})
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
		RepoID:      registryRepoID(h.db, registryID),
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
		SourceType:  "archive",
	}, createItemAssets{
		Records: assetRecords,
		Artifact: &models.CapabilityArtifact{
			Filename:        uploadedFilename,
			FileSize:        header.Size,
			ChecksumSHA256:  checksum,
			MimeType:        services.ArchiveMimeType(header.Filename),
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

	// Composite slug uniqueness: (repo_id, item_type, slug) must be unique in the target repo.
	targetRepoID := registryRepoID(db, req.TargetRegistryID)
	var conflictCount int64
	db.Model(&models.CapabilityItem{}).
		Where("repo_id = ? AND item_type = ? AND slug = ? AND id != ?", targetRepoID, item.ItemType, item.Slug, item.ID).
		Count(&conflictCount)
	if conflictCount > 0 {
		c.JSON(http.StatusConflict, gin.H{"error": "An item with this slug already exists", "slug": item.Slug})
		return
	}

	if err := db.Model(&models.CapabilityItem{}).Where("id = ?", item.ID).Updates(map[string]any{
		"registry_id": req.TargetRegistryID,
		"repo_id":     targetRepoID,
	}).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to move item"})
		return
	}

	item.RegistryID = req.TargetRegistryID
	item.RepoID = targetRepoID
	c.JSON(http.StatusOK, item)
}

// TransferItemToRepo godoc
// @Summary      Transfer item to another repository
// @Description  Transfer a capability item to a different repository. The system will automatically find the target repository's internal registry. Target repository must be a non-sync type. Caller must be the item creator, or owner/admin of the source repo. Caller must be a member of the target repo. When targetRepoId is "public", the item will be transferred to the default public registry; any authenticated user who is the item creator or source repo admin can perform this operation.
// @Tags         items
// @Accept       json
// @Produce      json
// @Param        id    path      string  true  "Item ID"
// @Param        body  body      object{targetRepoId=string}  true  "Target repository ID (use \"public\" for the default public registry)"
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

	// Special handling: transfer to the default public registry
	if req.TargetRepoID == "public" {
		if sourceReg.RepoID == "public" {
			c.JSON(http.StatusBadRequest, gin.H{"error": "Item already belongs to the public registry"})
			return
		}

		var publicReg models.CapabilityRegistry
		if db.First(&publicReg, "id = ?", PublicRegistryID).Error != nil {
			c.JSON(http.StatusNotFound, gin.H{"error": "Public registry not found"})
			return
		}

		if publicReg.ID == item.RegistryID {
			c.JSON(http.StatusBadRequest, gin.H{"error": "Item already belongs to the public registry"})
			return
		}

		var conflictCount int64
		db.Model(&models.CapabilityItem{}).
			Where("registry_id = ? AND item_type = ? AND slug = ? AND id != ?", publicReg.ID, item.ItemType, item.Slug, item.ID).
			Count(&conflictCount)
		if conflictCount > 0 {
			c.JSON(http.StatusConflict, gin.H{"error": "An item with the same slug and type already exists in the public registry", "slug": item.Slug})
			return
		}

		if err := db.Model(&models.CapabilityItem{}).Where("id = ?", item.ID).Updates(map[string]any{
			"registry_id": publicReg.ID,
			"repo_id":     "public",
		}).Error; err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to transfer item"})
			return
		}

		item.RegistryID = publicReg.ID
		item.RepoID = "public"
		c.JSON(http.StatusOK, item)
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

	// Composite slug uniqueness: (repo_id, item_type, slug) must be unique in the target repo.
	var conflictCount int64
	db.Model(&models.CapabilityItem{}).
		Where("repo_id = ? AND item_type = ? AND slug = ? AND id != ?", req.TargetRepoID, item.ItemType, item.Slug, item.ID).
		Count(&conflictCount)
	if conflictCount > 0 {
		c.JSON(http.StatusConflict, gin.H{"error": "An item with this slug already exists", "slug": item.Slug})
		return
	}

	// Must update registry_id and repo_id atomically.
	if err := db.Model(&models.CapabilityItem{}).Where("id = ?", item.ID).Updates(map[string]any{
		"registry_id": targetReg.ID,
		"repo_id":     req.TargetRepoID,
	}).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to transfer item"})
		return
	}

	item.RegistryID = targetReg.ID
	item.RepoID = req.TargetRepoID
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
