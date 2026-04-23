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
	"github.com/costrict/costrict-web/server/internal/systemrole"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"gorm.io/datatypes"
	"gorm.io/gorm"
)

// ItemHandler handles item operations with indexing support
type ItemHandler struct {
	db          *gorm.DB
	indexerSvc  *services.IndexerService
	parserSvc   *services.ParserService
	archiveSvc  *services.ArchiveService
	categorySvc *services.CategoryService
	tagSvc      *services.TagService
	hashSvc     *services.ContentHashService
}

// NewItemHandler creates a new item handler
func NewItemHandler(db *gorm.DB, indexerSvc *services.IndexerService, parserSvc *services.ParserService, categorySvc *services.CategoryService, tagSvc *services.TagService) *ItemHandler {
	var archiveSvc *services.ArchiveService
	if parserSvc != nil {
		archiveSvc = &services.ArchiveService{Parser: parserSvc}
	}
	return &ItemHandler{
		db:          db,
		indexerSvc:  indexerSvc,
		parserSvc:   parserSvc,
		archiveSvc:  archiveSvc,
		categorySvc: categorySvc,
		tagSvc:      tagSvc,
		hashSvc:     services.NewContentHashService(),
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

func callerIsPlatformAdmin(c *gin.Context, db *gorm.DB) bool {
	userID := c.GetString(middleware.UserIDKey)
	if userID == "" || db == nil {
		return false
	}
	service := systemrole.NewSystemRoleService(db)
	hasRole, err := service.HasRole(userID, systemrole.SystemRolePlatformAdmin)
	return err == nil && hasRole
}

func resolveAssignableTags(tagSvc *services.TagService, slugs []string, createdBy string, allowSystem bool) ([]string, error) {
	if tagSvc == nil {
		return nil, nil
	}
	resolved, err := tagSvc.ResolveOrCreateForAssignment(slugs, createdBy)
	if err != nil {
		return nil, err
	}
	tagIDs := make([]string, 0, len(resolved)+1)
	for _, tag := range resolved {
		if tag.TagClass == services.TagClassSystem && !allowSystem {
			continue
		}
		tagIDs = append(tagIDs, tag.ID)
	}
	seen := make(map[string]struct{}, len(tagIDs))
	unique := make([]string, 0, len(tagIDs))
	for _, id := range tagIDs {
		if _, ok := seen[id]; ok || id == "" {
			continue
		}
		seen[id] = struct{}{}
		unique = append(unique, id)
	}
	return unique, nil
}

func assignTagsForItem(tagSvc *services.TagService, itemID string, tagIDs []string) error {
	if tagSvc == nil {
		return nil
	}
	return tagSvc.SetItemTags(itemID, tagIDs)
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
	ContentMD5  string
	Metadata    datatypes.JSON
	SourcePath  string
	SourceSHA   string
	SourceType  string
	Source      string
	CreatedBy   string
}

// createItemAssets holds asset and artifact records to be created alongside the item.
type createItemAssets struct {
	Records  []models.CapabilityAsset
	Artifact *models.CapabilityArtifact
}

type itemAssetPayload struct {
	RelPath     string  `json:"relPath"`
	TextContent *string `json:"textContent"`
	MimeType    string  `json:"mimeType"`
	FileSize    int64   `json:"fileSize"`
	ContentSHA  string  `json:"contentSha"`
}

func defaultSourcePathForItemType(itemType string) string {
	switch itemType {
	case "skill":
		return "SKILL.md"
	case "mcp":
		return ".mcp.json"
	default:
		return ""
	}
}

func buildTextAssetRecords(payloads []itemAssetPayload, mainPath string) ([]models.CapabilityAsset, []services.ArchiveAsset, error) {
	records := make([]models.CapabilityAsset, 0, len(payloads))
	archiveAssets := make([]services.ArchiveAsset, 0, len(payloads))
	seen := make(map[string]struct{}, len(payloads))

	for _, asset := range payloads {
		relPath := strings.TrimSpace(asset.RelPath)
		if relPath == "" {
			return nil, nil, fmt.Errorf("asset relPath is required")
		}
		if relPath == mainPath {
			return nil, nil, fmt.Errorf("asset relPath %q conflicts with sourcePath", relPath)
		}
		if _, ok := seen[relPath]; ok {
			return nil, nil, fmt.Errorf("duplicate asset relPath %q", relPath)
		}
		seen[relPath] = struct{}{}
		if asset.TextContent == nil {
			return nil, nil, fmt.Errorf("asset %q requires textContent for JSON requests", relPath)
		}

		text := *asset.TextContent
		mimeType := asset.MimeType
		if strings.TrimSpace(mimeType) == "" {
			mimeType = services.InferMimeType(relPath)
		}
		fileSize := asset.FileSize
		if fileSize <= 0 {
			fileSize = int64(len([]byte(text)))
		}

		records = append(records, models.CapabilityAsset{
			RelPath:     relPath,
			TextContent: &text,
			MimeType:    mimeType,
			FileSize:    fileSize,
			ContentSHA:  asset.ContentSHA,
		})
		archiveAssets = append(archiveAssets, services.ArchiveAsset{
			Path:     relPath,
			Content:  []byte(text),
			Size:     fileSize,
			MimeType: mimeType,
			Binary:   false,
		})
	}

	return records, archiveAssets, nil
}

func loadExistingTextAssets(db *gorm.DB, itemID string, mainPath string) ([]services.ArchiveAsset, error) {
	var assets []models.CapabilityAsset
	if err := db.Where("item_id = ?", itemID).Find(&assets).Error; err != nil {
		return nil, err
	}

	archiveAssets := make([]services.ArchiveAsset, 0, len(assets))
	for _, asset := range assets {
		if strings.TrimSpace(asset.RelPath) == "" || asset.RelPath == mainPath {
			continue
		}
		if asset.TextContent == nil {
			return nil, fmt.Errorf("item contains binary asset %q; update via archive upload is required", asset.RelPath)
		}
		archiveAssets = append(archiveAssets, services.ArchiveAsset{
			Path:     asset.RelPath,
			Content:  []byte(*asset.TextContent),
			Size:     asset.FileSize,
			MimeType: asset.MimeType,
			Binary:   false,
		})
	}

	return archiveAssets, nil
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

func isUniqueConstraintError(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "UNIQUE constraint failed") ||
		strings.Contains(msg, "duplicate key value violates unique constraint") ||
		strings.Contains(msg, "duplicated key not allowed")
}

// persistNewItem creates an item, its initial version, optional assets and artifact
// in a single DB transaction. No storage I/O or async work happens here.
func persistNewItem(db *gorm.DB, req createItemRequest, assets createItemAssets) (*models.CapabilityItem, error) {
	item := models.CapabilityItem{
		ID:              req.ID,
		RegistryID:      req.RegistryID,
		RepoID:          req.RepoID,
		Slug:            req.Slug,
		ItemType:        req.ItemType,
		Name:            req.Name,
		Description:     req.Description,
		Category:        req.Category,
		Version:         req.Version,
		Content:         req.Content,
		ContentMD5:      req.ContentMD5,
		CurrentRevision: 1,
		Metadata:        req.Metadata,
		SourcePath:      req.SourcePath,
		SourceSHA:       req.SourceSHA,
		SourceType:      req.SourceType,
		Source:          req.Source,
		Status:          "active",
		CreatedBy:       req.CreatedBy,
	}

	if item.Metadata == nil || len(item.Metadata) == 0 {
		item.Metadata = datatypes.JSON([]byte("{}"))
	}

	err := db.Transaction(func(tx *gorm.DB) error {
		if err := tx.Omit("Embedding").Create(&item).Error; err != nil {
			return err
		}

		version := models.CapabilityVersion{
			ID:          uuid.New().String(),
			ItemID:      item.ID,
			Revision:    1,
			Name:        item.Name,
			Description: item.Description,
			Category:    item.Category,
			Version:     item.Version,
			Content:     item.Content,
			ContentMD5:  item.ContentMD5,
			Metadata:    item.Metadata,
			SourcePath:  item.SourcePath,
			CommitMsg:   "Initial version",
			CreatedBy:   item.CreatedBy,
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

		for _, snapshotAsset := range cloneItemAssetsToVersionAssets(version.ID, assets.Records) {
			asset := snapshotAsset
			if err := tx.Create(&asset).Error; err != nil {
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
		if isUniqueConstraintError(err) {
			return nil, ErrSlugConflict
		}
		return nil, err
	}
	item.Assets = assets.Records
	return &item, nil
}

// ItemResponse wraps a CapabilityItem with optional repo visibility,
// keeping a flat JSON structure compatible with the previous ItemWithAuthor.
type ItemResponse struct {
	models.CapabilityItem
	Assets              []itemAssetPayload `json:"assets,omitempty"`
	RepoVisibility      string             `json:"repoVisibility,omitempty"`
	Favorited           bool               `json:"favorited"`
	CurrentVersionLabel string             `json:"currentVersionLabel"`
}

type VersionResponse struct {
	models.CapabilityVersion
	Assets       []itemAssetPayload `json:"assets,omitempty"`
	VersionLabel string             `json:"versionLabel"`
}

func cloneItemAssetsToVersionAssets(versionID string, assets []models.CapabilityAsset) []models.CapabilityVersionAsset {
	cloned := make([]models.CapabilityVersionAsset, 0, len(assets))
	for _, asset := range assets {
		cloned = append(cloned, models.CapabilityVersionAsset{
			ID:             uuid.New().String(),
			VersionID:      versionID,
			RelPath:        asset.RelPath,
			TextContent:    asset.TextContent,
			StorageBackend: asset.StorageBackend,
			StorageKey:     asset.StorageKey,
			MimeType:       asset.MimeType,
			FileSize:       asset.FileSize,
			ContentSHA:     asset.ContentSHA,
		})
	}
	return cloned
}

type ConsistencyCheckResponse struct {
	Matched             bool   `json:"matched"`
	ContentMD5          string `json:"contentMd5"`
	MatchedCurrent      bool   `json:"matchedCurrent"`
	MatchedRevision     int    `json:"matchedRevision,omitempty"`
	MatchedVersionLabel string `json:"matchedVersionLabel,omitempty"`
}

func newVersionResponse(version models.CapabilityVersion) VersionResponse {
	hashSvc := services.NewContentHashService()
	resp := VersionResponse{
		CapabilityVersion: version,
		VersionLabel:      hashSvc.BuildVersionLabel(version.Revision),
	}
	if len(version.Assets) > 0 {
		resp.Assets = make([]itemAssetPayload, 0, len(version.Assets))
		for _, asset := range version.Assets {
			resp.Assets = append(resp.Assets, itemAssetPayload{
				RelPath:     asset.RelPath,
				TextContent: asset.TextContent,
				MimeType:    asset.MimeType,
				FileSize:    asset.FileSize,
				ContentSHA:  asset.ContentSHA,
			})
		}
	}
	return resp
}

func reconcileItemCurrentRevision(db *gorm.DB, item *models.CapabilityItem) {
	if item == nil || item.ID == "" {
		return
	}

	var latestRevision int
	if err := db.Model(&models.CapabilityVersion{}).
		Where("item_id = ?", item.ID).
		Select("COALESCE(MAX(revision), 0)").
		Scan(&latestRevision).Error; err != nil {
		return
	}

	if latestRevision <= 0 || item.CurrentRevision == latestRevision {
		return
	}

	item.CurrentRevision = latestRevision
	_ = db.Model(&models.CapabilityItem{}).
		Where("id = ?", item.ID).
		Update("current_revision", latestRevision).Error
	}

func buildItemResponse(db *gorm.DB, item models.CapabilityItem, userID string) ItemResponse {
	reconcileItemCurrentRevision(db, &item)
	if TagSvc != nil && item.ID != "" && len(item.Tags) == 0 {
		if tagsMap, err := TagSvc.GetItemTags([]string{item.ID}); err == nil && tagsMap != nil {
			item.Tags = tagsMap[item.ID]
		}
	}
	resp := ItemResponse{CapabilityItem: item}
	resp.CurrentVersionLabel = services.NewContentHashService().BuildVersionLabel(item.CurrentRevision)
	if len(item.Assets) > 0 {
		resp.Assets = make([]itemAssetPayload, 0, len(item.Assets))
		for _, asset := range item.Assets {
			resp.Assets = append(resp.Assets, itemAssetPayload{
				RelPath:     asset.RelPath,
				TextContent: asset.TextContent,
				MimeType:    asset.MimeType,
				FileSize:    asset.FileSize,
				ContentSHA:  asset.ContentSHA,
			})
		}
	}
	if item.Registry != nil {
		resp.RepoVisibility = getRepoVisibility(item.Registry.RepoID)
	}
	if userID != "" {
		var count int64
		if err := db.Model(&models.ItemFavorite{}).
			Where("item_id = ? AND user_id = ?", item.ID, userID).
			Count(&count).Error; err == nil {
			resp.Favorited = count > 0
		}
	}
	return resp
}

func itemListSortOrder(sortBy, sortOrder string) string {
	column := map[string]string{
		"":              "updated_at",
		"createdAt":     "created_at",
		"updatedAt":     "updated_at",
		"previewCount":  "preview_count",
		"installCount":  "install_count",
		"favoriteCount": "favorite_count",
	}[sortBy]
	if column == "" {
		column = "updated_at"
	}

	direction := "DESC"
	if strings.EqualFold(sortOrder, "asc") {
		direction = "ASC"
	}

	if column == "updated_at" {
		return fmt.Sprintf("%s %s", column, direction)
	}

	return fmt.Sprintf("%s %s, updated_at DESC", column, direction)
}

func parseTagSlugs(raw string) []string {
	if raw == "" {
		return nil
	}
	parts := strings.Split(raw, ",")
	seen := make(map[string]struct{}, len(parts))
	tagSlugs := make([]string, 0, len(parts))
	for _, part := range parts {
		slug := strings.TrimSpace(part)
		if slug == "" {
			continue
		}
		if _, ok := seen[slug]; ok {
			continue
		}
		seen[slug] = struct{}{}
		tagSlugs = append(tagSlugs, slug)
	}
	return tagSlugs
}

func applyItemTagsFilter(query *gorm.DB, rawTags string) *gorm.DB {
	tagSlugs := parseTagSlugs(rawTags)
	if len(tagSlugs) == 0 {
		return query
	}

	return query.Where(`id IN (
		SELECT item_tags.item_id
		FROM item_tags
		JOIN item_tag_dicts ON item_tags.tag_id = item_tag_dicts.id
		WHERE item_tag_dicts.slug IN ?
		GROUP BY item_tags.item_id
		HAVING COUNT(DISTINCT item_tag_dicts.slug) = ?
	)`, tagSlugs, len(tagSlugs))
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
// @Param        sortBy    query     string  false  "Sort by updatedAt, createdAt, previewCount, installCount, or favoriteCount"
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
// @Success      201   {object}  ItemResponse
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
		Source      string          `json:"source"`
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
	hashSvc := services.NewContentHashService()
	contentMD5, err := hashSvc.HashTextContent(req.ItemType, req.Content)
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
		ContentMD5:  contentMD5,
		Metadata:    metadata,
		SourcePath:  req.SourcePath,
		Source:      req.Source,
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

	if CategorySvc != nil && req.Category != "" {
		CategorySvc.EnsureCategory(req.Category, req.CreatedBy)
	}

	c.JSON(http.StatusCreated, buildItemResponse(db, *item, c.GetString(middleware.UserIDKey)))
}

// GetItem godoc
// @Summary      Get item
// @Description  Get skill item by ID with registry, versions, artifacts, repo visibility, and populated tags
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
	result := db.Preload("Registry").Preload("Versions").Preload("Artifacts").Preload("Assets").First(&item, "id = ?", id)
	if result.Error != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Item not found"})
		return
	}
	c.JSON(http.StatusOK, buildItemResponse(db, item, c.GetString(middleware.UserIDKey)))
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
// @Success      200   {object}  ItemResponse
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
		Name        string             `json:"name"`
		Description string             `json:"description"`
		Category    string             `json:"category"`
		Version     string             `json:"version"`
		Content     *string            `json:"content"`
		SourcePath  string             `json:"sourcePath"`
		Source      string             `json:"source"`
		Assets      []itemAssetPayload `json:"assets"`
		Status      string             `json:"status"`
		UpdatedBy   string             `json:"updatedBy"`
		CommitMsg   string             `json:"commitMsg"`
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

	originalName := item.Name
	originalDescription := item.Description
	originalCategory := item.Category
	originalVersion := item.Version

	if req.Name != "" {
		item.Name = req.Name
	}
	if req.Description != "" {
		item.Description = req.Description
	}
	if req.Category != "" {
		item.Category = req.Category
		if h.categorySvc != nil {
			h.categorySvc.EnsureCategory(req.Category, uid)
		}
	}
	if req.Version != "" {
		item.Version = req.Version
	}
	contentChanged := false
	newRevision := item.CurrentRevision
	if req.Content != nil {
		mainPath := req.SourcePath
		if mainPath == "" {
			mainPath = item.SourcePath
		}
		if mainPath == "" {
			mainPath = defaultSourcePathForItemType(item.ItemType)
		}

		var newContentMD5 string
		var err error
		if req.Assets != nil {
			assetRecords, archiveAssets, err := buildTextAssetRecords(req.Assets, mainPath)
			if err != nil {
				c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
				return
			}
			newContentMD5, err = h.hashSvc.HashArchiveContent(mainPath, []byte(*req.Content), archiveAssets)
			if err != nil {
				c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
				return
			}
			item.SourcePath = mainPath
			_ = db.Where("item_id = ?", item.ID).Delete(&models.CapabilityAsset{})
			for i := range assetRecords {
				assetRecords[i].ID = uuid.New().String()
				assetRecords[i].ItemID = item.ID
				if err := db.Create(&assetRecords[i]).Error; err != nil {
					c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to update item assets"})
					return
				}
			}
			item.Assets = assetRecords
		} else {
			existingAssets, err := loadExistingTextAssets(db, item.ID, mainPath)
			if err != nil {
				c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
				return
			}
			if len(existingAssets) > 0 {
				newContentMD5, err = h.hashSvc.HashArchiveContent(mainPath, []byte(*req.Content), existingAssets)
			} else {
				newContentMD5, err = h.hashSvc.HashTextContent(item.ItemType, *req.Content)
			}
		}
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		contentChanged = newContentMD5 != item.ContentMD5
		item.Content = *req.Content
		item.ContentMD5 = newContentMD5
		if item.ItemType == "mcp" {
			meta, err := resolveMetadata("mcp", nil, *req.Content)
			if err != nil {
				c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
				return
			}
			item.Metadata = meta
		}
	}
	if req.Source != "" {
		item.Source = req.Source
	}
	if req.Status != "" {
		item.Status = req.Status
	}
	if req.UpdatedBy != "" {
		item.UpdatedBy = req.UpdatedBy
	}
	metadataChanged := item.Name != originalName || item.Description != originalDescription || item.Category != originalCategory || item.Version != originalVersion
	versionedChanged := contentChanged || metadataChanged
	if versionedChanged {
		item.CurrentRevision = item.CurrentRevision + 1
		newRevision = item.CurrentRevision
	}

	if versionedChanged {
		createdBy := item.UpdatedBy
		if createdBy == "" {
			createdBy = item.CreatedBy
		}
		commitMsg := req.CommitMsg
		itemID := item.ID
		itemContent := item.Content
		if err := db.Transaction(func(tx *gorm.DB) error {
			if err := tx.Save(&item).Error; err != nil {
				return err
			}
			var versionAssetSnapshots []models.CapabilityVersionAsset
			if tx.Migrator().HasTable(&models.CapabilityVersionAsset{}) {
				var currentAssets []models.CapabilityAsset
				if err := tx.Where("item_id = ?", itemID).Find(&currentAssets).Error; err != nil {
					return err
				}
				versionAssetSnapshots = cloneItemAssetsToVersionAssets("", currentAssets)
			}
			sv := models.CapabilityVersion{
				ID:          uuid.New().String(),
				ItemID:      itemID,
				Revision:    newRevision,
				Name:        item.Name,
				Description: item.Description,
				Category:    item.Category,
				Version:     item.Version,
				Content:     itemContent,
				ContentMD5:  item.ContentMD5,
				Metadata:    item.Metadata,
				SourcePath:  item.SourcePath,
				CommitMsg:   commitMsg,
				CreatedBy:   createdBy,
			}
			if err := tx.Create(&sv).Error; err != nil {
				return err
			}
			for _, snapshotAsset := range versionAssetSnapshots {
				asset := snapshotAsset
				asset.ID = uuid.New().String()
				asset.VersionID = sv.ID
				if err := tx.Create(&asset).Error; err != nil {
					return err
				}
			}
			return nil
		}); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to update item"})
			return
		}
		if contentChanged {
			enqueueScanAsync(itemID, newRevision, "update")
		}
	} else {
		if result := db.Save(&item); result.Error != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to update item"})
			return
		}
	}

	c.JSON(http.StatusOK, buildItemResponse(db, item, c.GetString(middleware.UserIDKey)))
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
	contentMD5, err := h.hashSvc.HashArchiveContent(result.MainPath, []byte(result.MainContent), result.Assets)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
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
	if h.categorySvc != nil && item.Category != "" {
		h.categorySvc.EnsureCategory(item.Category, updatedBy)
	}
	newVersion := c.PostForm("version")
	if newVersion == "" {
		newVersion = result.Parsed.Version
	}
	if newVersion == "" {
		newVersion = item.Version
	}
	contentChanged := contentMD5 != item.ContentMD5
	newRevision := item.CurrentRevision
	if contentChanged {
		newRevision = item.CurrentRevision + 1
	}

	if !contentChanged {
		item.Content = result.MainContent
		item.Metadata = datatypes.JSON(metadataJSON)
		item.SourcePath = result.MainPath
		item.SourceSHA = result.MainSHA
		item.SourceType = "archive"
		item.Version = newVersion
		item.UpdatedBy = updatedBy
		if err := db.Save(&item).Error; err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to update item"})
			return
		}
		c.JSON(http.StatusOK, buildItemResponse(db, item, c.GetString(middleware.UserIDKey)))
		return
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
	item.ContentMD5 = contentMD5
	item.CurrentRevision = newRevision

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
		sv := models.CapabilityVersion{
			ID:          uuid.New().String(),
			ItemID:      itemID,
			Revision:    newRevision,
			Name:        item.Name,
			Description: item.Description,
			Category:    item.Category,
			Version:     item.Version,
			Content:     item.Content,
			ContentMD5:  item.ContentMD5,
			Metadata:    item.Metadata,
			SourcePath:  item.SourcePath,
			CommitMsg:   commitMsg,
			CreatedBy:   updatedBy,
		}
		if err := tx.Create(&sv).Error; err != nil {
			return err
		}
		for _, snapshotAsset := range cloneItemAssetsToVersionAssets(sv.ID, assetRecords) {
			asset := snapshotAsset
			if err := tx.Create(&asset).Error; err != nil {
				return err
			}
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

	c.JSON(http.StatusOK, buildItemResponse(db, item, c.GetString(middleware.UserIDKey)))
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

	err := db.Transaction(func(tx *gorm.DB) error {
		// Some environments may not have all tables yet (older schemas/tests).
		// Delete dependencies in a defensive order when tables exist.
		deletions := []struct {
			model any
			name  string
		}{
			{model: &models.BehaviorLog{}, name: "behavior logs"},
			{model: &models.ItemFavorite{}, name: "item favorites"},
				{model: &models.ItemTag{}, name: "item tags"},
			{model: &models.ScanJob{}, name: "scan jobs"},
			{model: &models.SecurityScan{}, name: "security scans"},
			{model: &models.CapabilityVersionAsset{}, name: "capability version assets"},
			{model: &models.CapabilityAsset{}, name: "capability assets"},
			{model: &models.CapabilityArtifact{}, name: "capability artifacts"},
			{model: &models.CapabilityVersion{}, name: "capability versions"},
		}

		for _, d := range deletions {
			if !tx.Migrator().HasTable(d.model) {
				continue
			}
			query := tx.Where("item_id = ?", id)
			if _, ok := d.model.(*models.CapabilityVersionAsset); ok {
				query = tx.Where("version_id IN (?)", tx.Model(&models.CapabilityVersion{}).Select("id").Where("item_id = ?", id))
			}
			if err := query.Delete(d.model).Error; err != nil {
				return fmt.Errorf("failed to delete %s: %w", d.name, err)
			}
		}

		if err := tx.Delete(&models.CapabilityItem{}, "id = ?", id).Error; err != nil {
			return fmt.Errorf("failed to delete item: %w", err)
		}
		return nil
	})
	if err != nil {
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
// @Success      200  {object}  object{versions=[]VersionResponse}
// @Failure      500  {object}  object{error=string}
// @Router       /items/{id}/versions [get]
func ListItemVersions(c *gin.Context) {
	id := c.Param("id")
	db := database.GetDB()
	var versions []models.CapabilityVersion
	result := db.Where("item_id = ?", id).Order("revision asc").Find(&versions)
	if result.Error != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to fetch versions"})
		return
	}
	resp := make([]VersionResponse, 0, len(versions))
	for _, version := range versions {
		resp = append(resp, newVersionResponse(version))
	}
	c.JSON(http.StatusOK, gin.H{"versions": resp})
}

// GetItemVersion godoc
// @Summary      Get item version
// @Description  Get a specific version of a skill item
// @Tags         items
// @Produce      json
// @Param        id       path      string  true  "Item ID"
// @Param        version  path      integer true  "Version number"
// @Success      200      {object}  VersionResponse
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
	query := db
	if db.Migrator().HasTable(&models.CapabilityVersionAsset{}) {
		query = query.Preload("Assets")
	}
	result := query.Where("item_id = ? AND revision = ?", id, versionNum).First(&version)
	if result.Error != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Version not found"})
		return
	}
	c.JSON(http.StatusOK, newVersionResponse(version))
}

// CheckItemConsistency godoc
// @Summary      Check item consistency
// @Description  Check whether provided content or md5 matches the current item version or a historical revision.
// @Tags         items
// @Accept       json
// @Produce      json
// @Param        id    path      string  true  "Item ID"
// @Param        body  body      object{content=string,md5=string}  true  "Consistency check payload"
// @Success      200   {object}  ConsistencyCheckResponse
// @Failure      400   {object}  object{error=string}
// @Failure      404   {object}  object{error=string}
// @Router       /items/{id}/check-consistency [post]
func (h *ItemHandler) CheckItemConsistency(c *gin.Context) {
	id := c.Param("id")
	var req struct {
		Content *string `json:"content"`
		MD5     string  `json:"md5"`
	}

	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid request"})
		return
	}
	if req.Content != nil && strings.TrimSpace(req.MD5) != "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Provide either content or md5, not both"})
		return
	}
	if req.Content == nil && strings.TrimSpace(req.MD5) == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "content or md5 is required"})
		return
	}

	var item models.CapabilityItem
	if err := h.db.First(&item, "id = ?", id).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Item not found"})
		return
	}

	checkMD5 := strings.TrimSpace(req.MD5)
	if req.Content != nil {
		var err error
		checkMD5, err = h.hashSvc.HashTextContent(item.ItemType, *req.Content)
		if err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
	}

	resp := ConsistencyCheckResponse{Matched: false, ContentMD5: checkMD5, MatchedCurrent: false}
	if checkMD5 == item.ContentMD5 {
		resp.Matched = true
		resp.MatchedCurrent = true
		resp.MatchedRevision = item.CurrentRevision
		resp.MatchedVersionLabel = h.hashSvc.BuildVersionLabel(item.CurrentRevision)
		c.JSON(http.StatusOK, resp)
		return
	}

	var version models.CapabilityVersion
	if err := h.db.Where("item_id = ? AND content_md5 = ?", item.ID, checkMD5).Order("revision desc").First(&version).Error; err == nil {
		resp.Matched = true
		resp.MatchedRevision = version.Revision
		resp.MatchedVersionLabel = h.hashSvc.BuildVersionLabel(version.Revision)
	}

	c.JSON(http.StatusOK, resp)
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
// @Param        category    query     string   false  "Filter by category (legacy single value)"
// @Param        categories  query     string   false  "Filter by categories (comma-separated)"
// @Param        securityStatuses  query     string   false  "Filter by security statuses (comma-separated)"
// @Param        registryId  query     string   false  "Filter by registry ID"
// @Param        favorited   query     string   false  "Filter to only favorited items (requires auth)"
// @Param        page        query     int      false  "Page number (default: 1)"
// @Param        pageSize    query     int      false  "Page size (default: 20, max: 100)"
// @Param        sortBy      query     string   false  "Sort by updatedAt, createdAt, previewCount, installCount, or favoriteCount"
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
	if categoriesRaw := c.Query("categories"); categoriesRaw != "" {
		categories := make([]string, 0)
		for _, part := range strings.Split(categoriesRaw, ",") {
			part = strings.TrimSpace(part)
			if part != "" {
				categories = append(categories, part)
			}
		}
		if len(categories) > 0 {
			query = query.Where("category IN ?", categories)
		}
	} else if category := c.Query("category"); category != "" {
		query = query.Where("category = ?", category)
	}
	if securityStatusesRaw := c.Query("securityStatuses"); securityStatusesRaw != "" {
		securityStatuses := make([]string, 0)
		for _, part := range strings.Split(securityStatusesRaw, ",") {
			part = strings.TrimSpace(part)
			if part != "" {
				securityStatuses = append(securityStatuses, part)
			}
		}
		if len(securityStatuses) > 0 {
			query = query.Where("security_status IN ?", securityStatuses)
		}
	}
	if registryID := c.Query("registryId"); registryID != "" {
		query = query.Where("registry_id = ?", registryID)
	}
	if c.Query("favorited") == "true" && uid != "" {
		query = query.Where("id IN (SELECT item_id FROM item_favorites WHERE user_id = ?)", uid)
	}
	query = applyItemTagsFilter(query, c.Query("tags"))

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

	// Batch-fetch favorited status for current user
	favoritedSet := make(map[string]bool)
	if uid != "" && len(items) > 0 {
		itemIDs := make([]string, len(items))
		for i, item := range items {
			itemIDs[i] = item.ID
		}
		var favs []models.ItemFavorite
		db.Where("user_id = ? AND item_id IN ?", uid, itemIDs).Find(&favs)
		for _, fav := range favs {
			favoritedSet[fav.ItemID] = true
		}
	}

	// Batch-fetch tags for items
	var tagsMap map[string][]models.ItemTagDict
	if TagSvc != nil && len(items) > 0 {
		itemIDs := make([]string, len(items))
		for i, item := range items {
			itemIDs[i] = item.ID
		}
		tagsMap, _ = TagSvc.GetItemTags(itemIDs)
	}

	// Populate repoName and favorited into each item
	type ItemWithRepo struct {
		models.CapabilityItem
		RepoName  string `json:"repoName,omitempty"`
		Favorited bool   `json:"favorited"`
	}
	out := make([]ItemWithRepo, len(items))
	for i, item := range items {
		out[i] = ItemWithRepo{CapabilityItem: item, Favorited: favoritedSet[item.ID]}
		if item.Registry != nil {
			out[i].RepoName = repoNameMap[item.Registry.RepoID]
		}
		if tagsMap != nil {
			out[i].Tags = tagsMap[item.ID]
		}
	}

	c.JSON(http.StatusOK, gin.H{"items": out, "total": total, "page": page, "pageSize": pageSize, "hasMore": int64((page-1)*pageSize+pageSize) < total})
}

// ListItemFilterOptions godoc
// @Summary      List item filter options
// @Description  Get filter options for item list, including categories and security statuses with i18n names
// @Tags         items
// @Produce      json
// @Success      200  {object}  object{categories=[]models.ItemCategory,securityStatuses=[]object}
// @Failure      500  {object}  object{error=string}
// @Router       /items/filter-options [get]
func ListItemFilterOptions(c *gin.Context) {
	db := database.GetDB()

	var categories []models.ItemCategory
	if err := db.Order("sort_order ASC, slug ASC").Find(&categories).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to load item filter options"})
		return
	}

	type SecurityStatusOption struct {
		Value string            `json:"value"`
		Names map[string]string `json:"names"`
	}

	securityStatuses := []SecurityStatusOption{
		{Value: "unscanned", Names: map[string]string{"zh": "待扫描", "en": "Unscanned"}},
		{Value: "pending", Names: map[string]string{"zh": "等待扫描", "en": "Pending Scan"}},
		{Value: "scanning", Names: map[string]string{"zh": "扫描中", "en": "Scanning"}},
		{Value: "clean", Names: map[string]string{"zh": "无风险", "en": "No Risk"}},
		{Value: "low", Names: map[string]string{"zh": "低风险", "en": "Low Risk"}},
		{Value: "medium", Names: map[string]string{"zh": "中风险", "en": "Medium Risk"}},
		{Value: "high", Names: map[string]string{"zh": "高风险", "en": "High Risk"}},
		{Value: "extreme", Names: map[string]string{"zh": "极高风险", "en": "Extreme Risk"}},
		{Value: "error", Names: map[string]string{"zh": "扫描失败", "en": "Scan Failed"}},
		{Value: "skipped", Names: map[string]string{"zh": "已跳过", "en": "Skipped"}},
	}

	c.JSON(http.StatusOK, gin.H{
		"categories":       categories,
		"securityStatuses": securityStatuses,
	})
}

// CreateItemDirect godoc
// @Summary      Create item (direct)
// @Description  Create a skill item via JSON or upload a .zip, .tar.gz, or .tgz archive via multipart/form-data. Auto-selects public registry if registryId is omitted. Successful responses include populated tags when available.
// @Tags         items
// @Accept       json,multipart/form-data
// @Produce      json
// @Param        body  body      object{registryId=string,slug=string,itemType=string,name=string,description=string,category=string,version=string,content=string,metadata=object,createdBy=string,tags=[]string}  false  "Item data (JSON)"
// @Param        file        formData  file    false  "Archive file (.zip, .tar.gz, or .tgz) (multipart)"
// @Param        itemType    formData  string  false  "Item type: skill or mcp (multipart)"
// @Param        name        formData  string  false  "Item name (multipart)"
// @Success      201   {object}  ItemResponse
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
		RegistryID  string             `json:"registryId"`
		Slug        string             `json:"slug"`
		ItemType    string             `json:"itemType" binding:"required"`
		Name        string             `json:"name" binding:"required"`
		Description string             `json:"description"`
		Category    string             `json:"category"`
		Version     string             `json:"version"`
		Content     string             `json:"content"`
		Metadata    json.RawMessage    `json:"metadata"`
		SourcePath  string             `json:"sourcePath"`
		Source      string             `json:"source"`
		Assets      []itemAssetPayload `json:"assets"`
		CreatedBy   string             `json:"createdBy"`
		Tags        []string           `json:"tags"`
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

	if req.SourcePath == "" {
		req.SourcePath = defaultSourcePathForItemType(req.ItemType)
	}
	resolvedTagIDs, err := resolveAssignableTags(h.tagSvc, req.Tags, req.CreatedBy, callerIsPlatformAdmin(c, h.db))
	if err != nil {
		if errors.Is(err, services.ErrInvalidTagSlug) {
			c.JSON(http.StatusBadRequest, gin.H{"error": "Tag slug may only contain lowercase letters, numbers, hyphens, and underscores", "code": "invalid_tag_slug"})
			return
		}
	}
	assetRecords, archiveAssets, err := buildTextAssetRecords(req.Assets, req.SourcePath)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
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
	var contentMD5 string
	if len(archiveAssets) > 0 {
		contentMD5, err = h.hashSvc.HashArchiveContent(req.SourcePath, []byte(req.Content), archiveAssets)
	} else {
		contentMD5, err = h.hashSvc.HashTextContent(req.ItemType, req.Content)
	}
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
		ContentMD5:  contentMD5,
		Metadata:    metadata,
		CreatedBy:   req.CreatedBy,
		SourcePath:  req.SourcePath,
		Source:      req.Source,
		SourceType:  "direct",
	}, createItemAssets{Records: assetRecords})
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

	if h.categorySvc != nil && req.Category != "" {
		h.categorySvc.EnsureCategory(req.Category, req.CreatedBy)
	}

	if h.tagSvc != nil {
		if err := assignTagsForItem(h.tagSvc, item.ID, resolvedTagIDs); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to assign item tags"})
			return
		}
	}

	c.JSON(http.StatusCreated, buildItemResponse(h.db, *item, c.GetString(middleware.UserIDKey)))
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
	contentMD5, err := h.hashSvc.HashArchiveContent(result.MainPath, []byte(result.MainContent), result.Assets)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
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
	if h.categorySvc != nil && category != "" {
		h.categorySvc.EnsureCategory(category, createdBy)
	}
	if version == "" {
		version = result.Parsed.Version
	}
	if version == "" {
		version = "1.0.0"
	}
	resolvedTagIDs, err := resolveAssignableTags(h.tagSvc, result.Parsed.Tags, createdBy, callerIsPlatformAdmin(c, h.db))
	if err != nil {
		if errors.Is(err, services.ErrInvalidTagSlug) {
			c.JSON(http.StatusBadRequest, gin.H{"error": "Tag slug may only contain lowercase letters, numbers, hyphens, and underscores", "code": "invalid_tag_slug"})
			return
		}
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
		ContentMD5:  contentMD5,
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

	if h.tagSvc != nil {
		if err := assignTagsForItem(h.tagSvc, item.ID, resolvedTagIDs); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to assign item tags"})
			return
		}
	}

	enqueueScanAsync(item.ID, 1, "create")
	c.JSON(http.StatusCreated, buildItemResponse(h.db, *item, c.GetString(middleware.UserIDKey)))
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
