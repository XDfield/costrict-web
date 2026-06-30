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
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/costrict/costrict-web/server/internal/database"
	"github.com/costrict/costrict-web/server/internal/itemdelete"
	"github.com/costrict/costrict-web/server/internal/middleware"
	"github.com/costrict/costrict-web/server/internal/models"
	"github.com/costrict/costrict-web/server/internal/services"
	"github.com/costrict/costrict-web/server/internal/systemrole"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"gorm.io/datatypes"
	"gorm.io/gorm"
)

// ItemHandler handles item operations
type ItemHandler struct {
	db          *gorm.DB
	parserSvc   *services.ParserService
	archiveSvc  *services.ArchiveService
	categorySvc *services.CategoryService
	tagSvc      *services.TagService
	hashSvc     *services.ContentHashService
}

type SecurityStatusOption struct {
	Value string            `json:"value"`
	Names map[string]string `json:"names"`
}

type SecurityRiskGroupOption struct {
	Value string            `json:"value"`
	Names map[string]string `json:"names"`
}

type SourceOption struct {
	Value string `json:"value"`
	Label string `json:"label"`
	URL   string `json:"url"`
}

type ItemFilterOptionsResponse struct {
	Categories         []models.ItemCategory     `json:"categories"`
	SecurityStatuses   []SecurityStatusOption    `json:"securityStatuses"`
	SecurityRiskGroups []SecurityRiskGroupOption `json:"securityRiskGroups"`
	Sources            []SourceOption            `json:"sources"`
}

var securityStatusGroups = map[string][]string{
	"unknown": {"unscanned", "pending", "scanning", "error", "skipped"},
	"low":     {"clean", "low"},
	"medium":  {"medium"},
	"high":    {"high", "extreme"},
}

func expandSecurityStatusFilters(values []string) []string {
	expanded := make([]string, 0, len(values))
	seen := make(map[string]struct{})
	for _, value := range values {
		group, ok := securityStatusGroups[value]
		if !ok {
			group = []string{value}
		}
		for _, status := range group {
			if _, exists := seen[status]; exists {
				continue
			}
			seen[status] = struct{}{}
			expanded = append(expanded, status)
		}
	}
	return expanded
}

// NewItemHandler creates a new item handler
func NewItemHandler(db *gorm.DB, parserSvc *services.ParserService, categorySvc *services.CategoryService, tagSvc *services.TagService) *ItemHandler {
	var archiveSvc *services.ArchiveService
	if parserSvc != nil {
		archiveSvc = &services.ArchiveService{Parser: parserSvc}
	}
	return &ItemHandler{
		db:          db,
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

// isItemOwnerOrAdmin returns true if the authenticated user is the creator of
// the item or holds the platform-admin role. This is the central ownership
// gate for item mutation APIs to prevent IDOR / supply-chain attacks.
func isItemOwnerOrAdmin(c *gin.Context, db *gorm.DB, item models.CapabilityItem) bool {
	userID := c.GetString(middleware.UserIDKey)
	if userID == "" {
		return false
	}
	if item.CreatedBy == userID {
		return true
	}
	return callerIsPlatformAdmin(c, db)
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
	// Fork provenance (optional)
	ForkedFromItemID  *string
	ForkedFromOwnerID *string
	// ParentPluginID links a promoted sub-skill back to its parent plugin item (optional).
	ParentPluginID *string
	IsBuiltIn      bool
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
	case "plugin":
		return ".plugin.json"
	case "rule":
		return "RULE.md"
	case "template":
		return "TEMPLATE.md"
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
// For plugin items it canonicalises the {install, bundle} envelope through
// an unmarshal + marshal round-trip to stabilise key order.
// When metadata is absent but content holds a valid JSON body,
// the metadata is derived from content automatically.
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
	if itemType == "plugin" {
		src := json.RawMessage(raw)
		if len(src) == 0 && content != "" {
			src = json.RawMessage(content)
		}
		if len(src) == 0 {
			return datatypes.JSON([]byte("{}")), nil
		}
		var m map[string]any
		if err := json.Unmarshal(src, &m); err != nil {
			// content may be Markdown rather than JSON metadata — tolerate and return empty metadata.
			return datatypes.JSON([]byte("{}")), nil
		}
		b, err := json.Marshal(m)
		if err != nil {
			return nil, fmt.Errorf("failed to marshal plugin metadata: %w", err)
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

// getRepoName returns the name of a repository for the given repoID.
// For the virtual "public" repo it returns "public".
func getRepoName(repoID string) string {
	if repoID == "public" {
		return "public"
	}
	db := database.GetDB()
	var repo models.Repository
	if db.Select("name").Where("id = ?", repoID).First(&repo).Error != nil {
		return ""
	}
	return repo.Name
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
		ID:                req.ID,
		RegistryID:        req.RegistryID,
		RepoID:            req.RepoID,
		Slug:              req.Slug,
		ItemType:          req.ItemType,
		Name:              req.Name,
		Description:       req.Description,
		Category:          req.Category,
		Version:           req.Version,
		Content:           req.Content,
		ContentMD5:        req.ContentMD5,
		CurrentRevision:   1,
		Metadata:          req.Metadata,
		SourcePath:        req.SourcePath,
		SourceSHA:         req.SourceSHA,
		SourceType:        req.SourceType,
		Source:            req.Source,
		ForkedFromItemID:  req.ForkedFromItemID,
		ForkedFromOwnerID: req.ForkedFromOwnerID,
		ParentPluginID:    req.ParentPluginID,
		IsBuiltIn:         req.IsBuiltIn,
		Status:            "active",
		CreatedBy:         req.CreatedBy,
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
	ID                  string                      `json:"id"`
	RegistryID          string                      `json:"registryId"`
	RepoID              string                      `json:"repoId"`
	Slug                string                      `json:"slug"`
	ItemType            string                      `json:"itemType"`
	Name                string                      `json:"name"`
	Description         string                      `json:"description"`                       // Resolved per `?lang=` query or Accept-Language header; en is the default. Use `descriptions` for raw locale map.
	Descriptions        datatypes.JSON              `json:"descriptions" swaggertype:"object"` // Raw locale → text map, e.g. {"en":"...","zh":"..."}.
	Category            string                      `json:"category"`
	Version             string                      `json:"version"`
	Content             string                      `json:"content"`
	ContentMD5          string                      `json:"contentMd5"`
	CurrentRevision     int                         `json:"currentRevision"`
	Metadata            datatypes.JSON              `json:"metadata" swaggertype:"object"`
	Health              datatypes.JSON              `json:"health,omitempty" swaggertype:"object"`
	Evaluation          datatypes.JSON              `json:"evaluation,omitempty" swaggertype:"object"`
	SourcePath          string                      `json:"sourcePath"`
	SourceSHA           string                      `json:"sourceSha"`
	SourceType          string                      `json:"sourceType"`
	Source              string                      `json:"source"`
	ForkedFromItemID    *string                     `json:"forkedFromItemId,omitempty"`
	ForkedFromOwnerID   *string                     `json:"forkedFromOwnerId,omitempty"`
	ParentPluginID      *string                     `json:"parentPluginId,omitempty"`   // sub-skill: 所属父 plugin item ID
	ParentPluginName    string                      `json:"parentPluginName,omitempty"` // 父 plugin 展示名（供「来自插件 X」徽章）
	ParentPluginSlug    string                      `json:"parentPluginSlug,omitempty"` // 父 plugin slug（供跳转）
	PreviewCount        int                         `json:"previewCount"`
	InstallCount        int                         `json:"installCount"`
	FavoriteCount       int                         `json:"favoriteCount"`
	Status              string                      `json:"status"`
	SecurityStatus      string                      `json:"securityStatus"`
	LastScanID          *string                     `json:"lastScanId,omitempty"`
	CreatedBy           string                      `json:"createdBy"`
	UpdatedBy           string                      `json:"updatedBy"`
	Registry            *models.CapabilityRegistry  `json:"registry,omitempty"`
	Artifacts           []models.CapabilityArtifact `json:"artifacts,omitempty"`
	CreatedAt           time.Time                   `json:"createdAt"`
	UpdatedAt           time.Time                   `json:"updatedAt"`
	ExperienceScore     float64                     `json:"experienceScore"`
	Tags                []models.ItemTagDict        `json:"tags,omitempty"`
	RepoVisibility      string                      `json:"repoVisibility,omitempty"`
	RepoName            string                      `json:"repoName,omitempty"`
	Favorited           bool                        `json:"favorited"`
	IsBuiltIn           bool                        `json:"isBuiltIn"`
	CurrentVersionLabel string                      `json:"currentVersionLabel"`
	ForkCount           int                         `json:"forkCount"`              // 本 item 被 fork 的次数
	MyForkItemID        *string                     `json:"myForkItemId,omitempty"` // 当前登录用户对本 item 的已有 fork（用于「查看我的 fork」三态）
	MCPConfig           *MCPConfigStatus            `json:"mcpConfig,omitempty"`    // per-user MCP 占位参数配置状态（掩码；仅 mcp + 登录用户已配置时出现）
}

type ItemAssetsResponse struct {
	Assets []itemAssetPayload `json:"assets"`
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

func buildItemResponse(c *gin.Context, db *gorm.DB, item models.CapabilityItem, userID string) ItemResponse {
	reconcileItemCurrentRevision(db, &item)
	if TagSvc != nil && item.ID != "" && len(item.Tags) == 0 {
		if tagsMap, err := TagSvc.GetItemTags([]string{item.ID}); err == nil && tagsMap != nil {
			item.Tags = tagsMap[item.ID]
		}
	}
	locale := ResolveLocale(c)

	// For plugin items without an existing install metadata, inject a zip_download
	// install guide so the frontend can show local installation instructions.
	metadata := item.Metadata
	if item.ItemType == "plugin" && len(metadata) > 0 {
		var metaMap map[string]any
		if err := json.Unmarshal(metadata, &metaMap); err == nil && metaMap != nil {
			if _, hasInstall := metaMap["install"]; !hasInstall {
				slug := item.Slug
				host := origin(c)
				metaMap["install"] = map[string]any{
					"method": "zip_download",
					"url":    fmt.Sprintf("%s/api/plugins/%s/download", host, slug),
					"commands": []string{
						fmt.Sprintf("curl -L -o %s.zip %s/api/plugins/%s/download", slug, host, slug),
						fmt.Sprintf("unzip %s.zip -d ./%s", slug, slug),
						fmt.Sprintf("csc --plugin-dir ./%s", slug),
					},
				}
				if b, err := json.Marshal(metaMap); err == nil {
					metadata = datatypes.JSON(b)
				}
			}
		}
	}

	resp := ItemResponse{
		ID:                  item.ID,
		RegistryID:          item.RegistryID,
		RepoID:              item.RepoID,
		Slug:                item.Slug,
		ItemType:            item.ItemType,
		Name:                item.Name,
		Description:         PickDescription(item.Descriptions, item.Description, locale),
		Descriptions:        item.Descriptions,
		Category:            item.Category,
		Version:             item.Version,
		Content:             item.Content,
		ContentMD5:          item.ContentMD5,
		CurrentRevision:     item.CurrentRevision,
		Metadata:            metadata,
		Health:              item.Health,
		Evaluation:          item.Evaluation,
		SourcePath:          item.SourcePath,
		SourceSHA:           item.SourceSHA,
		SourceType:          item.SourceType,
		Source:              item.Source,
		ForkedFromItemID:    item.ForkedFromItemID,
		ForkedFromOwnerID:   item.ForkedFromOwnerID,
		ParentPluginID:      item.ParentPluginID,
		PreviewCount:        item.PreviewCount,
		InstallCount:        item.InstallCount,
		FavoriteCount:       item.FavoriteCount,
		Status:              item.Status,
		SecurityStatus:      item.SecurityStatus,
		LastScanID:          item.LastScanID,
		CreatedBy:           item.CreatedBy,
		UpdatedBy:           item.UpdatedBy,
		IsBuiltIn:           item.IsBuiltIn,
		Registry:            item.Registry,
		Artifacts:           item.Artifacts,
		CreatedAt:           item.CreatedAt,
		UpdatedAt:           item.UpdatedAt,
		ExperienceScore:     item.ExperienceScore,
		Tags:                item.Tags,
		CurrentVersionLabel: services.NewContentHashService().BuildVersionLabel(item.CurrentRevision),
	}
	if item.Registry != nil {
		resp.RepoVisibility = getRepoVisibility(item.Registry.RepoID)
		resp.RepoName = getRepoName(item.Registry.RepoID)
	}
	if userID != "" {
		var count int64
		if err := db.Model(&models.ItemFavorite{}).
			Where("item_id = ? AND user_id = ?", item.ID, userID).
			Count(&count).Error; err == nil {
			resp.Favorited = count > 0
		}
	}
	if item.ID != "" {
		var forkCount int64
		if err := db.Model(&models.CapabilityItem{}).
			Where("forked_from_item_id = ?", item.ID).
			Count(&forkCount).Error; err == nil {
			resp.ForkCount = int(forkCount)
		}
		if userID != "" {
			var myFork models.CapabilityItem
			if err := db.Select("id").
				Where("forked_from_item_id = ? AND created_by = ?", item.ID, userID).
				First(&myFork).Error; err == nil && myFork.ID != "" {
				forkID := myFork.ID
				resp.MyForkItemID = &forkID
			}
		}
	}
	if item.ItemType == "mcp" && userID != "" {
		if fields := loadMCPUserFields(db, userID, item.ID); len(fields) > 0 {
			// Per-user resolve: overlay filled values onto a copy of Content for
			// this caller only. Never mutate the shared row / Metadata / ContentMD5.
			resp.Content = resolveMCPContent(item.Content, fields)
			resp.MCPConfig = buildMCPConfigStatus(fields)
		}
	}
	// Sub-skill: resolve the parent plugin's display name/slug for the badge.
	// Single-item path only — list handlers MUST batch this (see ListAllItems).
	if item.ParentPluginID != nil && *item.ParentPluginID != "" {
		var parent models.CapabilityItem
		if err := db.Select("name, slug").First(&parent, "id = ?", *item.ParentPluginID).Error; err == nil {
			resp.ParentPluginName = parent.Name
			resp.ParentPluginSlug = parent.Slug
		}
	}
	return resp
}

// fetchForkCounts 批量统计 itemIDs 各自被 fork 的次数（GROUP BY），避免列表逐条查询的 N+1。
func fetchForkCounts(db *gorm.DB, itemIDs []string) map[string]int {
	counts := make(map[string]int, len(itemIDs))
	if len(itemIDs) == 0 {
		return counts
	}
	type forkAgg struct {
		ForkedFromItemID string
		Cnt              int64
	}
	var aggs []forkAgg
	db.Model(&models.CapabilityItem{}).
		Select("forked_from_item_id, COUNT(*) AS cnt").
		Where("forked_from_item_id IN ?", itemIDs).
		Group("forked_from_item_id").
		Scan(&aggs)
	for _, a := range aggs {
		counts[a.ForkedFromItemID] = int(a.Cnt)
	}
	return counts
}

// parentPluginInfo carries a parent plugin's display fields for sub-skill badges.
type parentPluginInfo struct {
	Name string
	Slug string
}

// fetchParentPluginInfo batch-loads parent plugin name/slug for the given sub-skill
// rows in ONE query (keyed by parent_plugin_id), avoiding per-row N+1 lookups in list endpoints.
func fetchParentPluginInfo(db *gorm.DB, items []models.CapabilityItem) map[string]parentPluginInfo {
	info := make(map[string]parentPluginInfo)
	parentIDSet := make(map[string]struct{})
	for _, item := range items {
		if item.ParentPluginID != nil && *item.ParentPluginID != "" {
			parentIDSet[*item.ParentPluginID] = struct{}{}
		}
	}
	if len(parentIDSet) == 0 {
		return info
	}
	parentIDs := make([]string, 0, len(parentIDSet))
	for id := range parentIDSet {
		parentIDs = append(parentIDs, id)
	}
	var parents []models.CapabilityItem
	db.Select("id, name, slug").Where("id IN ?", parentIDs).Find(&parents)
	for _, p := range parents {
		info[p.ID] = parentPluginInfo{Name: p.Name, Slug: p.Slug}
	}
	return info
}

func itemListSortOrder(sortBy, sortOrder string) string {
	column := map[string]string{
		"":                "updated_at",
		"createdAt":       "created_at",
		"updatedAt":       "updated_at",
		"previewCount":    "preview_count",
		"installCount":    "install_count",
		"favoriteCount":   "favorite_count",
		"experienceScore": "experience_score",
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

func parseCSVQueryValues(raw string) []string {
	if raw == "" {
		return nil
	}

	values := make([]string, 0)
	for _, part := range strings.Split(raw, ",") {
		part = strings.TrimSpace(part)
		if part != "" {
			values = append(values, part)
		}
	}

	return values
}

type itemListFilterOptions struct {
	DefaultActiveStatus bool
	AllowRegistryID     bool
	AllowFavorited      bool
	UserID              string
}

// applyItemSearchFilter applies a keyword search over name and description. On
// PostgreSQL it also searches every value of the descriptions jsonb (locale ->
// localized text) so non-English queries (e.g. Chinese) match the text users see.
func applyItemSearchFilter(query *gorm.DB, db *gorm.DB, search string) *gorm.DB {
	if search == "" {
		return query
	}
	like := database.ILike(db)
	isPostgres := db.Dialector.Name() == "postgres"
	for _, kw := range database.SplitSearchKeywords(search) {
		pattern := "%" + kw + "%"
		if isPostgres {
			query = query.Where(
				fmt.Sprintf("name %s ? OR description %s ? OR EXISTS (SELECT 1 FROM jsonb_each_text(capability_items.descriptions) je WHERE je.value %s ?)", like, like, like),
				pattern, pattern, pattern,
			)
		} else {
			query = query.Where(fmt.Sprintf("name %s ? OR description %s ?", like, like), pattern, pattern)
		}
	}
	return query
}

func applySharedItemListFilters(query *gorm.DB, db *gorm.DB, c *gin.Context, opts itemListFilterOptions) *gorm.DB {
	if itemType := c.Query("type"); itemType != "" {
		query = query.Where("item_type = ?", itemType)
	}
	if status := c.Query("status"); status != "" {
		query = query.Where("status = ?", status)
	} else if opts.DefaultActiveStatus {
		query = query.Where("status = 'active'")
	}
	query = applyItemSearchFilter(query, db, c.Query("search"))
	if categoriesRaw := c.Query("categories"); categoriesRaw != "" {
		categories := parseCSVQueryValues(categoriesRaw)
		if len(categories) > 0 {
			query = query.Where("category IN ?", categories)
		}
	} else if category := c.Query("category"); category != "" {
		query = query.Where("category = ?", category)
	}
	if securityStatusesRaw := c.Query("securityStatuses"); securityStatusesRaw != "" {
		securityStatuses := expandSecurityStatusFilters(parseCSVQueryValues(securityStatusesRaw))
		if len(securityStatuses) > 0 {
			query = query.Where("security_status IN ?", securityStatuses)
		}
	}
	if sourcesRaw := c.Query("source"); sourcesRaw != "" {
		sources := parseCSVQueryValues(sourcesRaw)
		if len(sources) > 0 {
			query = query.Where("source IN ?", sources)
		}
	}
	if opts.AllowRegistryID {
		if registryID := c.Query("registryId"); registryID != "" {
			query = query.Where("registry_id = ?", registryID)
		}
	}
	if opts.AllowFavorited && c.Query("favorited") == "true" && opts.UserID != "" {
		query = query.Where("EXISTS (SELECT 1 FROM item_favorites f WHERE f.item_id = capability_items.id AND f.user_id = ?) OR EXISTS (SELECT 1 FROM item_distribution_receipts idr JOIN item_distributions id ON id.id = idr.distribution_id WHERE idr.user_id = ? AND idr.receipt_status != ? AND id.status = ? AND id.item_id = capability_items.id)", opts.UserID, opts.UserID, "dismissed", "active")
	}
	// sub-skill filters: list a plugin's bundled sub-skills, or hide all sub-skills.
	if parentPluginID := c.Query("parentPluginId"); parentPluginID != "" {
		query = query.Where("parent_plugin_id = ?", parentPluginID)
	}
	if v := c.Query("excludeSubSkills"); v == "true" || v == "1" {
		query = query.Where("parent_plugin_id IS NULL")
	}

	return applyItemTagsFilter(query, c.Query("tags"))
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
	query = applyItemSearchFilter(query, db, c.Query("search"))

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
	ResolveItemListLocale(c, items)
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
		// CreatedBy removed: always derived from authenticated user
	}

	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid request"})
		return
	}

	createdBy := c.GetString(middleware.UserIDKey)
	if createdBy == "" {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "Authentication required"})
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
		CreatedBy:   createdBy,
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
		CategorySvc.EnsureCategory(req.Category, createdBy)
	}

	c.JSON(http.StatusCreated, buildItemResponse(c, db, *item, c.GetString(middleware.UserIDKey)))
}

// GetItem godoc
// @Summary      Get item
// @Description  Get skill item by ID with registry, artifacts, repo visibility, and populated tags
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
	result := db.Preload("Registry").Preload("Artifacts").First(&item, "id = ?", id)
	if result.Error != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Item not found"})
		return
	}
	c.JSON(http.StatusOK, buildItemResponse(c, db, item, c.GetString(middleware.UserIDKey)))
}

// ListItemAssets godoc
// @Summary      List item assets
// @Description  Get current text assets of a skill item for on-demand editor loading
// @Tags         items
// @Produce      json
// @Param        id   path      string  true  "Item ID"
// @Success      200  {object}  ItemAssetsResponse
// @Failure      404  {object}  object{error=string}
// @Failure      500  {object}  object{error=string}
// @Router       /items/{id}/assets [get]
func ListItemAssets(c *gin.Context) {
	id := c.Param("id")
	db := database.GetDB()

	var item models.CapabilityItem
	if err := db.Select("id").First(&item, "id = ?", id).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Item not found"})
		return
	}

	var assets []models.CapabilityAsset
	if err := db.Where("item_id = ?", id).Order("rel_path asc").Find(&assets).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to fetch item assets"})
		return
	}

	resp := ItemAssetsResponse{Assets: make([]itemAssetPayload, 0, len(assets))}
	for _, asset := range assets {
		resp.Assets = append(resp.Assets, itemAssetPayload{
			RelPath:     asset.RelPath,
			TextContent: asset.TextContent,
			MimeType:    asset.MimeType,
			FileSize:    asset.FileSize,
			ContentSHA:  asset.ContentSHA,
		})
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
		IsBuiltIn   *bool              `json:"isBuiltIn"`
	}

	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid request"})
		return
	}

	uid := c.GetString(middleware.UserIDKey)
	req.UpdatedBy = uid

	db := h.db
	var item models.CapabilityItem
	result := db.First(&item, "id = ?", id)
	if result.Error != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Item not found"})
		return
	}

	// Ownership guard: only the item's author or a platform admin may edit it.
	// (Repo-scoped admins go through MoveItem/TransferItem, which keep their own
	// checks; this closes the previously-unguarded bare PUT path.)
	if item.CreatedBy != uid && !callerIsPlatformAdmin(c, db) {
		c.JSON(http.StatusForbidden, gin.H{"error": "Only the item creator or a platform admin can edit this item"})
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
		if item.ItemType == "mcp" || item.ItemType == "plugin" {
			meta, err := resolveMetadata(item.ItemType, json.RawMessage(item.Metadata), *req.Content)
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
	if req.IsBuiltIn != nil {
		if !callerIsPlatformAdmin(c, db) {
			c.JSON(http.StatusForbidden, gin.H{"error": "Only platform admins can change built-in status"})
			return
		}
		item.IsBuiltIn = *req.IsBuiltIn
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

	c.JSON(http.StatusOK, buildItemResponse(c, db, item, c.GetString(middleware.UserIDKey)))
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

	// Ownership guard: only the item's author or a platform admin may edit it.
	callerID := c.GetString(middleware.UserIDKey)
	if item.CreatedBy != callerID && !callerIsPlatformAdmin(c, db) {
		c.JSON(http.StatusForbidden, gin.H{"error": "Only the item creator or a platform admin can edit this item"})
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
		// The contentChanged short-circuit only hashes the plugin main content, so the
		// bundled skills/ set may have changed even when the parent is byte-identical.
		// Reconcile sub-skills unconditionally for plugins (idempotent).
		if item.ItemType == "plugin" {
			_ = reconcilePluginSubSkills(h, &item, result.Assets, updatedBy)
		}
		c.JSON(http.StatusOK, buildItemResponse(c, db, item, c.GetString(middleware.UserIDKey)))
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

	// Cleanup old storage keys only after successful commit. Binary assets keep
	// stable storage keys across replacement, so do not delete keys reused by the
	// new asset records.
	cleanupStorageKeys(staleStorageKeys(oldStorageKeys, assetRecords))

	// Reconcile bundled sub-skills (create/update/archive) for plugin updates.
	if item.ItemType == "plugin" {
		_ = reconcilePluginSubSkills(h, &item, result.Assets, updatedBy)
	}

	c.JSON(http.StatusOK, buildItemResponse(c, db, item, c.GetString(middleware.UserIDKey)))
}

// DeleteItem godoc
// @Summary      Delete item
// @Description  Delete skill item by ID
// @Tags         items
// @Produce      json
// @Param        id   path      string  true  "Item ID"
// @Success      200  {object}  object{message=string}
// @Failure      403  {object}  object{error=string}
// @Failure      404  {object}  object{error=string}
// @Failure      500  {object}  object{error=string}
// @Router       /items/{id} [delete]
func DeleteItem(c *gin.Context) {
	id := c.Param("id")
	db := database.GetDB()

	// Ownership guard: only the item's author or a platform admin may delete it.
	// This closes the previously-unguarded bare DELETE path (any logged-in user
	// could delete any item). Platform admins moderate via /admin/items, but the
	// admin check here keeps this path safe for repo-scoped admin tooling too.
	var existing models.CapabilityItem
	if err := db.Select("id, created_by").First(&existing, "id = ?", id).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			c.JSON(http.StatusNotFound, gin.H{"error": "Item not found"})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to delete item"})
		return
	}
	callerID := c.GetString(middleware.UserIDKey)
	if existing.CreatedBy != callerID && !callerIsPlatformAdmin(c, db) {
		c.JSON(http.StatusForbidden, gin.H{"error": "Only the item creator or a platform admin can delete this item"})
		return
	}

	// Shared cascade (see internal/itemdelete): hard-deletes bundled sub-skills
	// recursively, clears dependent rows + distribution/mcp-config orphans, then
	// the item itself. Forks owned by other users are left intact.
	err := db.Transaction(func(tx *gorm.DB) error {
		return itemdelete.CascadeDelete(tx, id)
	})
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to delete item"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"message": "Item deleted"})
}

// maxBatchItems bounds a single batch operation so an accidental "select all →
// delete" can't wipe an unbounded slice in one transaction. The frontend's
// "select all matching" path pulls ids in pages and respects the same ceiling.
const maxBatchItems = 200

// BatchDeleteItems godoc
// @Summary      Batch delete items
// @Description  Delete up to 200 of the caller's own items (or any items for a platform admin) and their dependent records in a single transaction. All authorized deletes succeed or none do. Items the caller may not delete are reported in `forbidden`; ids that no longer exist are reported in `skipped`.
// @Tags         items
// @Accept       json
// @Produce      json
// @Param        body  body      object{ids=[]string}  true  "Item ids to delete"
// @Success      200   {object}  object{deleted=int,skipped=int,forbidden=int}
// @Failure      400   {object}  object{error=string}
// @Failure      401   {object}  object{error=string}
// @Failure      500   {object}  object{error=string}
// @Router       /items [delete]
func BatchDeleteItems(c *gin.Context) {
	var req struct {
		IDs []string `json:"ids" binding:"required"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request body"})
		return
	}
	callerID := c.GetString(middleware.UserIDKey)
	if callerID == "" {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "Authentication required"})
		return
	}

	// Normalize: trim, drop blanks, de-duplicate while preserving order.
	seen := make(map[string]bool, len(req.IDs))
	ids := make([]string, 0, len(req.IDs))
	for _, raw := range req.IDs {
		id := strings.TrimSpace(raw)
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		ids = append(ids, id)
	}
	if len(ids) == 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "no item ids provided"})
		return
	}
	if len(ids) > maxBatchItems {
		c.JSON(http.StatusBadRequest, gin.H{"error": fmt.Sprintf("too many items: %d (max %d)", len(ids), maxBatchItems)})
		return
	}

	db := database.GetDB()
	isAdmin := callerIsPlatformAdmin(c, db)

	// Ownership filter: only the item's author (or a platform admin) may delete it.
	// Batch-load the owners in one query rather than per-id.
	var rows []struct {
		ID        string
		CreatedBy string
	}
	if err := db.Model(&models.CapabilityItem{}).Select("id, created_by").Where("id IN ?", ids).Find(&rows).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to delete items"})
		return
	}
	owner := make(map[string]string, len(rows))
	for _, r := range rows {
		owner[r.ID] = r.CreatedBy
	}

	authorized := make([]string, 0, len(ids))
	skipped := make([]string, 0)
	forbidden := make([]string, 0)
	for _, id := range ids {
		createdBy, exists := owner[id]
		if !exists {
			skipped = append(skipped, id) // already gone / never existed
			continue
		}
		if createdBy != callerID && !isAdmin {
			forbidden = append(forbidden, id) // not the caller's to delete
			continue
		}
		authorized = append(authorized, id)
	}

	deleted := make([]string, 0)
	if len(authorized) > 0 {
		var batchSkipped []string
		err := db.Transaction(func(tx *gorm.DB) error {
			var e error
			deleted, batchSkipped, e = itemdelete.CascadeDeleteMany(tx, authorized)
			return e
		})
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to delete items"})
			return
		}
		skipped = append(skipped, batchSkipped...)
	}

	c.JSON(http.StatusOK, gin.H{
		"deleted":      len(deleted),
		"skipped":      len(skipped),
		"forbidden":    len(forbidden),
		"deletedIds":   deleted,
		"forbiddenIds": forbidden,
	})
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
// @Param        source      query     string   false  "Filter by sources (comma-separated, OR logic)"
// @Param        registryId  query     string   false  "Filter by registry ID"
// @Param        favorited   query     string   false  "Filter to only favorited items (requires auth)"
// @Param        parentPluginId query   string   false  "Filter to sub-skills bundled by the given parent plugin item ID"
// @Param        excludeSubSkills query string   false  "Hide bundled sub-skills from the result when true"
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
	isFavoritedQuery := c.Query("favorited") == "true" && uid != ""
	if c.Query("favorited") == "true" && uid == "" {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "Authentication required for favorited items"})
		return
	}
	isPaginatedFavorited := isFavoritedQuery && c.Query("paginated") == "true"
	if len(registryIDs) == 0 && !isFavoritedQuery {
		c.JSON(http.StatusOK, gin.H{"items": []models.CapabilityItem{}, "total": 0, "page": page, "pageSize": pageSize, "hasMore": false})
		return
	}

	var query *gorm.DB
	var total int64
	var items []models.CapabilityItem

	if isFavoritedQuery {
		var favoriteItemIDs []string
		db.Model(&models.ItemFavorite{}).Where("user_id = ?", uid).Pluck("item_id", &favoriteItemIDs)

		var distributedItemIDs []string
		db.Model(&models.ItemDistributionReceipt{}).
			Joins("JOIN item_distributions ON item_distributions.id = item_distribution_receipts.distribution_id").
			Where("item_distribution_receipts.user_id = ? AND item_distribution_receipts.receipt_status != ? AND item_distributions.status = ?",
				uid, "dismissed", "active").
			Select("item_distributions.item_id").
			Scan(&distributedItemIDs)

		itemIDSet := make(map[string]struct{}, len(favoriteItemIDs)+len(distributedItemIDs))
		for _, id := range favoriteItemIDs {
			itemIDSet[id] = struct{}{}
		}
		for _, id := range distributedItemIDs {
			itemIDSet[id] = struct{}{}
		}
		if len(itemIDSet) == 0 {
			c.JSON(http.StatusOK, gin.H{"items": []models.CapabilityItem{}, "total": 0, "page": page, "pageSize": pageSize, "hasMore": false})
			return
		}
		allItemIDs := make([]string, 0, len(itemIDSet))
		for id := range itemIDSet {
			allItemIDs = append(allItemIDs, id)
		}
		query = db.Where("id IN ?", allItemIDs)
		if len(registryIDs) > 0 {
			query = query.Where("registry_id IN ?", registryIDs)
		}
		query = applySharedItemListFilters(query, db, c, itemListFilterOptions{
			DefaultActiveStatus: true,
			AllowRegistryID:     true,
			AllowFavorited:      false,
			UserID:              uid,
		})
	} else {
		query = db.Where("registry_id IN ?", registryIDs)
		query = applySharedItemListFilters(query, db, c, itemListFilterOptions{
			DefaultActiveStatus: true,
			AllowRegistryID:     true,
			AllowFavorited:      true,
			UserID:              uid,
		})
		// Public browse hides forked copies by default (GitHub-style); opt in with includeForks=true.
		if c.Query("includeForks") != "true" {
			query = query.Where("forked_from_item_id IS NULL")
		}
		query.Model(&models.CapabilityItem{}).Count(&total)
	}

	var result *gorm.DB
	if isFavoritedQuery && !isPaginatedFavorited {
		result = query.Preload("Registry").Order(itemListSortOrder(c.Query("sortBy"), c.Query("sortOrder"))).Find(&items)
		total = int64(len(items))
	} else {
		// Paginated favorited path: the non-favorited branch counts total above, but
		// the favorited branch never does, so total would stay 0 while a page of items
		// is still returned (sidebar badge / "共 X 个" then show 0 against a non-empty
		// list). Count the filtered favorited+distributed set before limit/offset.
		if isPaginatedFavorited {
			query.Model(&models.CapabilityItem{}).Count(&total)
		}
		result = query.Preload("Registry").Order(itemListSortOrder(c.Query("sortBy"), c.Query("sortOrder"))).Limit(pageSize).Offset((page - 1) * pageSize).Find(&items)
	}
	if result.Error != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to fetch items"})
		return
	}
	ResolveItemListLocale(c, items)

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

	// Batch-fetch fork counts for items (avoid N+1)
	var forkCountMap map[string]int
	if len(items) > 0 {
		itemIDs := make([]string, len(items))
		for i, item := range items {
			itemIDs[i] = item.ID
		}
		forkCountMap = fetchForkCounts(db, itemIDs)
	}

	// Batch-fetch parent plugin name/slug for sub-skill rows (avoid N+1)
	parentPluginMap := fetchParentPluginInfo(db, items)

	// Populate repoName and favorited into each item
	type ItemWithRepo struct {
		models.CapabilityItem
		RepoName         string `json:"repoName,omitempty"`
		Favorited        bool   `json:"favorited"`
		ForkCount        int    `json:"forkCount"`
		ParentPluginName string `json:"parentPluginName,omitempty"`
		ParentPluginSlug string `json:"parentPluginSlug,omitempty"`
	}
	out := make([]ItemWithRepo, len(items))
	for i, item := range items {
		out[i] = ItemWithRepo{CapabilityItem: item, Favorited: favoritedSet[item.ID], ForkCount: forkCountMap[item.ID]}
		if item.Registry != nil {
			out[i].RepoName = repoNameMap[item.Registry.RepoID]
		}
		if tagsMap != nil {
			out[i].Tags = tagsMap[item.ID]
		}
		if item.ParentPluginID != nil {
			if pp, ok := parentPluginMap[*item.ParentPluginID]; ok {
				out[i].ParentPluginName = pp.Name
				out[i].ParentPluginSlug = pp.Slug
			}
		}
	}

	hasMore := false
	if !isFavoritedQuery || isPaginatedFavorited {
		hasMore = int64((page-1)*pageSize+pageSize) < total
	}

	c.JSON(http.StatusOK, gin.H{"items": out, "total": total, "page": page, "pageSize": pageSize, "hasMore": hasMore})
}

// ListItemFilterOptions godoc
// @Summary      List item filter options
// @Description  Get filter options for item list, including categories, security statuses, security risk groups, and sources
// @Tags         items
// @Produce      json
// @Success      200  {object}  ItemFilterOptionsResponse
// @Failure      500  {object}  object{error=string}
// @Router       /items/filter-options [get]
func ListItemFilterOptions(c *gin.Context) {
	db := database.GetDB()

	var categories []models.ItemCategory
	if err := db.Order("sort_order ASC, slug ASC").Find(&categories).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to load item filter options"})
		return
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

	securityRiskGroups := []SecurityRiskGroupOption{
		{Value: "unknown", Names: map[string]string{"zh": "未知", "en": "Unknown"}},
		{Value: "low", Names: map[string]string{"zh": "低风险", "en": "Low Risk"}},
		{Value: "medium", Names: map[string]string{"zh": "中风险", "en": "Medium Risk"}},
		{Value: "high", Names: map[string]string{"zh": "高风险", "en": "High Risk"}},
	}

	sources := []SourceOption{
		{Value: "awesome-mcp-servers", Label: "awesome-mcp-servers", URL: "https://github.com/punkpeye/awesome-mcp-servers"},
		{Value: "mcp.so", Label: "mcp.so", URL: "https://mcp.so"},
		{Value: "anthropics-skills", Label: "Anthropic Skills", URL: "https://github.com/anthropics/skills"},
		{Value: "ai-agent-skills", Label: "Ai-Agent-Skills", URL: "https://github.com/skillcreatorai/Ai-Agent-Skills"},
		{Value: "antigravity-skills", Label: "antigravity-skills", URL: "https://github.com/antigravities/awesome-claude-code-skills"},
		{Value: "awesome-cursorrules", Label: "awesome-cursorrules", URL: "https://github.com/PatrickJS/awesome-cursorrules"},
		{Value: "rules-2.1-optimized", Label: "Rules 2.1", URL: "https://github.com/Mr-chen-05/rules-2.1-optimized"},
		{Value: "prompts-chat", Label: "prompts.chat", URL: "https://github.com/f/prompts.chat"},
		{Value: "wonderful-prompts", Label: "wonderful-prompts", URL: "https://github.com/langgptai/wonderful-prompts"},
	}

	c.JSON(http.StatusOK, ItemFilterOptionsResponse{
		Categories:         categories,
		SecurityStatuses:   securityStatuses,
		SecurityRiskGroups: securityRiskGroups,
		Sources:            sources,
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
		// CreatedBy removed: always derived from authenticated user
		Tags        []string           `json:"tags"`
	}

	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Invalid request"})
		return
	}

	uid := c.GetString(middleware.UserIDKey)
		if uid == "" {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "Authentication required"})
			return
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
	resolvedTagIDs, err := resolveAssignableTags(h.tagSvc, req.Tags, uid, callerIsPlatformAdmin(c, h.db))
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
		CreatedBy:   uid,
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

	enqueueScanAsync(item.ID, 1, "create")

	if h.categorySvc != nil && req.Category != "" {
		h.categorySvc.EnsureCategory(req.Category, uid)
	}

	if h.tagSvc != nil {
		if err := assignTagsForItem(h.tagSvc, item.ID, resolvedTagIDs); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to assign item tags"})
			return
		}
	}

	c.JSON(http.StatusCreated, buildItemResponse(c, h.db, *item, c.GetString(middleware.UserIDKey)))
}

// ForkItem godoc
// @Summary      Fork an item
// @Description  Create a personal copy of a public capability item under the current user, placed in the public registry, recording fork provenance (forkedFromItemId/ownerId). Only public, non-archive items not owned by the caller can be forked; each user may fork a given source item once.
// @Tags         items
// @Produce      json
// @Param        id   path      string  true  "Source item ID"
// @Success      201  {object}  ItemResponse
// @Failure      400  {object}  object{error=string}
// @Failure      401  {object}  object{error=string}
// @Failure      403  {object}  object{error=string}
// @Failure      404  {object}  object{error=string}
// @Failure      409  {object}  object{error=string}
// @Failure      500  {object}  object{error=string}
// @Router       /items/{id}/fork [post]
func (h *ItemHandler) ForkItem(c *gin.Context) {
	userID := c.GetString(middleware.UserIDKey)
	if userID == "" {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "authentication required"})
		return
	}
	srcID := c.Param("id")

	var src models.CapabilityItem
	if err := h.db.Preload("Registry").First(&src, "id = ?", srcID).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			c.JSON(http.StatusNotFound, gin.H{"error": "Item not found"})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to load item"})
		return
	}

	// Inactive/archived items are hidden from listings; forking must not republish them
	// as a fresh active public copy. Treat as not found to avoid leaking hidden items.
	if src.Status != "active" {
		c.JSON(http.StatusNotFound, gin.H{"error": "Item not found"})
		return
	}

	// Only public items can be forked (forks land in the public registry; forking a
	// private item would leak its content).
	repoID := src.RepoID
	if src.Registry != nil {
		repoID = src.Registry.RepoID
	}
	if getRepoVisibility(repoID) != "public" {
		c.JSON(http.StatusForbidden, gin.H{"error": "Only public items can be forked"})
		return
	}

	// Cannot fork your own item.
	if src.CreatedBy == userID {
		c.JSON(http.StatusBadRequest, gin.H{"error": "You cannot fork your own item", "code": "fork_self"})
		return
	}

	// Archive (binary bundle) items are not forkable in MVP — persistNewItem does no storage I/O.
	if src.SourceType == "archive" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Forking packaged (archive) capabilities is not supported yet", "code": "fork_archive_unsupported"})
		return
	}

	// One fork per user per source item: if already forked, return the existing fork.
	var existing models.CapabilityItem
	if err := h.db.Where("forked_from_item_id = ? AND created_by = ?", src.ID, userID).First(&existing).Error; err == nil && existing.ID != "" {
		c.JSON(http.StatusOK, buildItemResponse(c, h.db, existing, userID))
		return
	}

	// Read source text assets to copy (IDs/ItemID re-assigned by persistNewItem).
	var srcAssets []models.CapabilityAsset
	h.db.Where("item_id = ?", src.ID).Order("rel_path asc").Find(&srcAssets)
	forkedAssets := make([]models.CapabilityAsset, 0, len(srcAssets))
	for _, a := range srcAssets {
		forkedAssets = append(forkedAssets, models.CapabilityAsset{
			RelPath:        a.RelPath,
			TextContent:    a.TextContent,
			StorageBackend: a.StorageBackend,
			StorageKey:     a.StorageKey,
			MimeType:       a.MimeType,
			FileSize:       a.FileSize,
			ContentSHA:     a.ContentSHA,
		})
	}

	// Read source tags to carry over.
	var tagIDs []string
	if h.tagSvc != nil {
		if tagsMap, err := h.tagSvc.GetItemTags([]string{src.ID}); err == nil && tagsMap != nil {
			for _, t := range tagsMap[src.ID] {
				tagIDs = append(tagIDs, t.ID)
			}
		}
	}

	publicRepoID := registryRepoID(h.db, PublicRegistryID)
	srcItemID := src.ID
	srcOwnerID := src.CreatedBy

	// Fork slug: srcSlug-fork-<hash8>, with -N suffix retry on collision.
	// Hash the FULL user ID (not userID[:8]) so distinct users forking the same
	// source never collide on the base slug and exhaust the small retry range.
	uidSum := sha256.Sum256([]byte(userID))
	baseSlug := fmt.Sprintf("%s-fork-%x", src.Slug, uidSum[:4])

	var item *models.CapabilityItem
	var err error
	for attempt := 0; attempt < 10; attempt++ {
		slug := baseSlug
		if attempt > 0 {
			slug = fmt.Sprintf("%s-%d", baseSlug, attempt+1)
		}
		// Fresh copy of asset records each attempt (persistNewItem mutates ID/ItemID).
		records := make([]models.CapabilityAsset, len(forkedAssets))
		copy(records, forkedAssets)
		item, err = persistNewItem(h.db, createItemRequest{
			ID:                uuid.New().String(),
			RegistryID:        PublicRegistryID,
			RepoID:            publicRepoID,
			Slug:              slug,
			ItemType:          src.ItemType,
			Name:              src.Name,
			Description:       src.Description,
			Category:          src.Category,
			Version:           src.Version,
			Content:           src.Content,
			ContentMD5:        src.ContentMD5,
			Metadata:          src.Metadata,
			SourcePath:        src.SourcePath,
			SourceSHA:         src.SourceSHA,
			SourceType:        "fork",
			Source:            srcItemID,
			CreatedBy:         userID,
			ForkedFromItemID:  &srcItemID,
			ForkedFromOwnerID: &srcOwnerID,
		}, createItemAssets{Records: records})
		if err == nil {
			break
		}
		if errors.Is(err, ErrSlugConflict) {
			// A unique-constraint violation here is either a slug collision OR the
			// (forked_from_item_id, created_by) uniqueness from a concurrent fork by
			// the same user. If a fork now exists for this user+source, return it
			// (the concurrent winner) instead of retrying — enforces "fork once".
			var raced models.CapabilityItem
			if e := h.db.Where("forked_from_item_id = ? AND created_by = ?", src.ID, userID).First(&raced).Error; e == nil && raced.ID != "" {
				c.JSON(http.StatusOK, buildItemResponse(c, h.db, raced, userID))
				return
			}
			continue
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to fork item"})
		return
	}
	if err != nil {
		c.JSON(http.StatusConflict, gin.H{"error": "Could not allocate a unique slug for fork"})
		return
	}

	// Copy multilingual descriptions (persistNewItem doesn't carry Descriptions).
	if len(src.Descriptions) > 0 && string(src.Descriptions) != "{}" {
		h.db.Model(&models.CapabilityItem{}).Where("id = ?", item.ID).Update("descriptions", src.Descriptions)
		h.db.Model(&models.CapabilityVersion{}).Where("item_id = ? AND revision = ?", item.ID, 1).Update("descriptions", src.Descriptions)
		// Keep the in-memory copy in sync so the 201 response carries localized descriptions.
		item.Descriptions = src.Descriptions
	}

	enqueueScanAsync(item.ID, 1, "fork")

	// Carry over tags.
	if h.tagSvc != nil && len(tagIDs) > 0 {
		if err := assignTagsForItem(h.tagSvc, item.ID, tagIDs); err != nil {
			log.Printf("Failed to assign tags to forked item %s: %v", item.ID, err)
		}
	}

	// Plugins bundle their sub-skills/MCPs as first-class child items
	// (parent_plugin_id). Fork those too so the user gets their own editable
	// copies, re-linked to the new plugin fork. Best-effort: per-child failures
	// are logged and skipped — the plugin fork itself already succeeded.
	if src.ItemType == "plugin" {
		h.forkPluginChildren(srcItemID, item.ID, userID)
	}

	c.JSON(http.StatusCreated, buildItemResponse(c, h.db, *item, userID))
}

// forkPluginChildren forks each active sub-skill/MCP bundled under the source
// plugin (parent_plugin_id = srcPluginID) into the new plugin fork, re-linking
// them via parent_plugin_id = newPluginID. Idempotent per (child, user); a child
// that fails to fork is logged and skipped.
func (h *ItemHandler) forkPluginChildren(srcPluginID, newPluginID, userID string) {
	var children []models.CapabilityItem
	if err := h.db.Where("parent_plugin_id = ? AND status = ?", srcPluginID, "active").
		Order("slug asc").Find(&children).Error; err != nil {
		log.Printf("fork: load sub-skills of plugin %s failed: %v", srcPluginID, err)
		return
	}
	if len(children) == 0 {
		return
	}

	publicRepoID := registryRepoID(h.db, PublicRegistryID)
	uidSum := sha256.Sum256([]byte(userID))

	for i := range children {
		child := children[i]

		// One fork per user per source child.
		var existing models.CapabilityItem
		if err := h.db.Where("forked_from_item_id = ? AND created_by = ?", child.ID, userID).
			First(&existing).Error; err == nil && existing.ID != "" {
			continue
		}

		var childAssets []models.CapabilityAsset
		h.db.Where("item_id = ?", child.ID).Order("rel_path asc").Find(&childAssets)
		assetTpl := make([]models.CapabilityAsset, 0, len(childAssets))
		for _, a := range childAssets {
			assetTpl = append(assetTpl, models.CapabilityAsset{
				RelPath:        a.RelPath,
				TextContent:    a.TextContent,
				StorageBackend: a.StorageBackend,
				StorageKey:     a.StorageKey,
				MimeType:       a.MimeType,
				FileSize:       a.FileSize,
				ContentSHA:     a.ContentSHA,
			})
		}

		childSrcID := child.ID
		childOwnerID := child.CreatedBy
		parentID := newPluginID
		baseSlug := fmt.Sprintf("%s-fork-%x", child.Slug, uidSum[:4])

		var forked *models.CapabilityItem
		var ferr error
		for attempt := 0; attempt < 10; attempt++ {
			slug := baseSlug
			if attempt > 0 {
				slug = fmt.Sprintf("%s-%d", baseSlug, attempt+1)
			}
			records := make([]models.CapabilityAsset, len(assetTpl))
			copy(records, assetTpl)
			forked, ferr = persistNewItem(h.db, createItemRequest{
				ID:                uuid.New().String(),
				RegistryID:        PublicRegistryID,
				RepoID:            publicRepoID,
				Slug:              slug,
				ItemType:          child.ItemType,
				Name:              child.Name,
				Description:       child.Description,
				Category:          child.Category,
				Version:           child.Version,
				Content:           child.Content,
				ContentMD5:        child.ContentMD5,
				Metadata:          child.Metadata,
				SourcePath:        child.SourcePath,
				SourceSHA:         child.SourceSHA,
				SourceType:        "fork",
				Source:            childSrcID,
				CreatedBy:         userID,
				ForkedFromItemID:  &childSrcID,
				ForkedFromOwnerID: &childOwnerID,
				ParentPluginID:    &parentID,
			}, createItemAssets{Records: records})
			if ferr == nil {
				break
			}
			if errors.Is(ferr, ErrSlugConflict) {
				// Concurrent fork by the same user won the race — treat as done.
				var raced models.CapabilityItem
				if e := h.db.Where("forked_from_item_id = ? AND created_by = ?", childSrcID, userID).
					First(&raced).Error; e == nil && raced.ID != "" {
					ferr, forked = nil, nil
					break
				}
				continue
			}
			break
		}
		if ferr != nil {
			log.Printf("fork: sub-skill %s of plugin %s failed: %v", child.ID, srcPluginID, ferr)
			continue
		}
		if forked == nil {
			continue // a concurrent fork already created it
		}

		// Carry localized descriptions (persistNewItem doesn't).
		if len(child.Descriptions) > 0 && string(child.Descriptions) != "{}" {
			h.db.Model(&models.CapabilityItem{}).Where("id = ?", forked.ID).Update("descriptions", child.Descriptions)
			h.db.Model(&models.CapabilityVersion{}).Where("item_id = ? AND revision = ?", forked.ID, 1).Update("descriptions", child.Descriptions)
		}

		enqueueScanAsync(forked.ID, 1, "fork")

		if h.tagSvc != nil {
			if tagsMap, err := h.tagSvc.GetItemTags([]string{child.ID}); err == nil {
				var tids []string
				for _, t := range tagsMap[child.ID] {
					tids = append(tids, t.ID)
				}
				if len(tids) > 0 {
					_ = assignTagsForItem(h.tagSvc, forked.ID, tids)
				}
			}
		}
	}
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

// pluginChildAsset is a skill or MCP item extracted from a plugin archive and
// promoted into a normal capability_items row.
type pluginChildAsset struct {
	ItemType    string
	Name        string
	SlugSuffix  string
	Description string
	Version     string
	SourcePath  string
	Content     string
	Metadata    datatypes.JSON
	Assets      []services.ArchiveAsset // files under the child directory, with paths relative to that directory
}

// extractSubSkillAssets returns the directory-type children bundled inside a
// plugin archive: skills/<name>/SKILL.md and evaluators/<name>/SKILL.md. Both
// are SKILL.md-shaped directory items (a SKILL.md plus sibling files under the
// same directory) and become item_type=skill rows. For deeper nesting (e.g.
// "skills/a/b/SKILL.md") the directory immediately above SKILL.md ("b") is used
// as the name, matching how the device installs it.
//
// The source_path is the verbatim archive path so the plugin "work tree"
// mirrors the original repository layout (skills/... and evaluators/... live
// under different roots and never collide).
func extractSubSkillAssets(assets []services.ArchiveAsset) []pluginChildAsset {
	out := make([]pluginChildAsset, 0)
	out = append(out, extractSkillDirChildren(assets, "skills/")...)
	out = append(out, extractSkillDirChildren(assets, "evaluators/")...)
	return out
}

// extractSkillDirChildren returns directory-type skill children whose SKILL.md
// lives under the given top-level prefix ("skills/" or "evaluators/"). Both map
// to item_type=skill; only the prefix differs, keeping evaluators generic
// rather than hardcoded to cospower.
func extractSkillDirChildren(assets []services.ArchiveAsset, prefix string) []pluginChildAsset {
	out := make([]pluginChildAsset, 0)
	seen := make(map[string]struct{})
	for _, asset := range assets {
		if asset.Binary {
			continue
		}
		p := asset.Path
		if !strings.HasPrefix(p, prefix) || !strings.HasSuffix(p, "/SKILL.md") {
			continue
		}
		dir := path.Dir(p) // "<prefix-root>/<...>/<name>"
		name := path.Base(dir)
		if name == "" || dir == strings.TrimSuffix(prefix, "/") {
			continue
		}
		if _, ok := seen[p]; ok {
			continue
		}
		seen[p] = struct{}{}
		childPrefix := dir + "/"
		childAssets := make([]services.ArchiveAsset, 0)
		for _, candidate := range assets {
			if candidate.Path == p || !strings.HasPrefix(candidate.Path, childPrefix) {
				continue
			}
			relPath := strings.TrimPrefix(candidate.Path, childPrefix)
			if relPath == "" || relPath == "SKILL.md" {
				continue
			}
			copied := candidate
			copied.Path = relPath
			childAssets = append(childAssets, copied)
		}
		out = append(out, pluginChildAsset{
			ItemType:   "skill",
			Name:       name,
			SlugSuffix: name,
			Version:    "1.0.0",
			SourcePath: p,
			Content:    string(asset.Content),
			Metadata:   datatypes.JSON([]byte("{}")),
			Assets:     childAssets,
		})
	}
	return out
}

// pluginFileChildSpec describes a single-file plugin-child extractor: every
// non-binary .md file under <prefix> becomes one child of the given item_type,
// with a path-faithful source_path. Used for commands/agents/rules/templates,
// which (unlike skills/evaluators) are individual files, not directories.
type pluginFileChildSpec struct {
	prefix   string // top-level dir prefix, e.g. "commands/"
	itemType string // resulting capability_items.item_type
}

// pluginFileChildSpecs lists the single-file child types in a stable order. The
// path→type mapping is the cross-repo contract shared with the upstream catalog
// pipeline and the frontend TYPE_META:
//
//	commands/<f>.md          -> command
//	agents/<f>.md            -> subagent
//	rules/<group>/<f>.md     -> rule       (nestable; group segment kept in slug)
//	templates/<f>.md         -> template
var pluginFileChildSpecs = []pluginFileChildSpec{
	{prefix: "commands/", itemType: "command"},
	{prefix: "agents/", itemType: "subagent"},
	{prefix: "rules/", itemType: "rule"},
	{prefix: "templates/", itemType: "template"},
}

// extractPluginFileChildren returns the single-file children (commands, agents,
// rules, templates) bundled inside a plugin archive. Each matching .md file
// becomes one child with item_type per pluginFileChildSpecs and a verbatim
// source_path. rules/ may be nested (rules/<group>/<file>.md); the intermediate
// segments are folded into the slug suffix so siblings sharing a leaf filename
// (including non-ASCII ones) stay unique while the source_path remains faithful.
func extractPluginFileChildren(assets []services.ArchiveAsset) []pluginChildAsset {
	out := make([]pluginChildAsset, 0)
	seen := make(map[string]struct{})
	for _, asset := range assets {
		if asset.Binary {
			continue
		}
		p := asset.Path
		if !strings.HasSuffix(strings.ToLower(p), ".md") {
			continue
		}
		var spec *pluginFileChildSpec
		for i := range pluginFileChildSpecs {
			if strings.HasPrefix(p, pluginFileChildSpecs[i].prefix) {
				spec = &pluginFileChildSpecs[i]
				break
			}
		}
		if spec == nil {
			continue
		}
		if _, ok := seen[p]; ok {
			continue
		}
		seen[p] = struct{}{}

		// Inner segments (between the top-level dir and the file) plus the
		// leaf filename without extension form the slug suffix; this keeps
		// nested rules unique. Falls back to the leaf when only one segment.
		rel := strings.TrimPrefix(p, spec.prefix)
		leaf := strings.TrimSuffix(path.Base(rel), path.Ext(rel))
		dirPart := path.Dir(rel)
		slugSuffix := leaf
		if dirPart != "." && dirPart != "" {
			slugSuffix = strings.ReplaceAll(dirPart, "/", "-") + "-" + leaf
		}

		out = append(out, pluginChildAsset{
			ItemType:   spec.itemType,
			Name:       pluginChildDisplayName(p),
			SlugSuffix: slugSuffix,
			Version:    "1.0.0",
			SourcePath: p,
			Content:    string(asset.Content),
			Metadata:   datatypes.JSON([]byte("{}")),
		})
	}
	return out
}

// pluginChildDisplayName derives a human-friendly name from a child file path,
// e.g. "rules/dfx/安全.md" -> "安全", "commands/run-tests.md" -> "Run tests".
// Non-ASCII leaves (Chinese filenames) are returned as-is; ASCII leaves are
// title-cased with separators turned into spaces.
func pluginChildDisplayName(filePath string) string {
	base := path.Base(filePath)
	if strings.EqualFold(base, "SKILL.md") {
		base = path.Base(path.Dir(filePath))
	} else {
		base = strings.TrimSuffix(base, path.Ext(base))
	}
	name := strings.ReplaceAll(base, "-", " ")
	name = strings.ReplaceAll(name, "_", " ")
	if name == "" {
		return base
	}
	r := []rune(name)
	if r[0] >= 'a' && r[0] <= 'z' {
		r[0] = r[0] - 32
	}
	return string(r)
}

func hasObjectMCPServers(content []byte) bool {
	var data map[string]any
	if err := json.Unmarshal(content, &data); err != nil {
		return false
	}
	_, ok := data["mcpServers"].(map[string]any)
	return ok
}

func singleMCPContent(parsed *services.ParsedItem, fallback []byte) string {
	key, _ := parsed.Metadata["key"].(string)
	if key == "" {
		return string(fallback)
	}
	server := make(map[string]any, len(parsed.Metadata))
	for k, v := range parsed.Metadata {
		if k == "key" {
			continue
		}
		server[k] = v
	}
	content := map[string]any{
		"mcpServers": map[string]any{
			key: server,
		},
	}
	b, err := json.Marshal(content)
	if err != nil {
		return string(fallback)
	}
	return string(b)
}

func buildPluginMCPChildrenFromContent(parser *services.ParserService, sourcePath string, content []byte) ([]pluginChildAsset, error) {
	parsedItems, err := parser.ParseMCPJSON(content, sourcePath)
	if err != nil {
		return nil, err
	}
	children := make([]pluginChildAsset, 0, len(parsedItems))
	for _, parsed := range parsedItems {
		normalized, err := services.NormalizeMCPMetadata(parsed.Metadata)
		if err != nil {
			return nil, err
		}
		metaBytes, err := json.Marshal(normalized)
		if err != nil {
			return nil, err
		}
		name := parsed.Name
		if strings.TrimSpace(name) == "" {
			name = "MCP Config"
		}
		slugSuffix := parsed.Slug
		if strings.TrimSpace(slugSuffix) == "" {
			slugSuffix = name
		}
		version := parsed.Version
		if version == "" {
			version = "1.0.0"
		}
		childSourcePath := sourcePath + "#" + slugSuffix
		childContent := singleMCPContent(parsed, content)
		children = append(children, pluginChildAsset{
			ItemType:    "mcp",
			Name:        name,
			SlugSuffix:  slugSuffix,
			Description: parsed.Description,
			Version:     version,
			SourcePath:  childSourcePath,
			Content:     childContent,
			Metadata:    datatypes.JSON(metaBytes),
		})
	}
	return children, nil
}

func extractPluginMCPAssets(parser *services.ParserService, pluginItem *models.CapabilityItem, assets []services.ArchiveAsset) ([]pluginChildAsset, error) {
	manifestChildren := make([]pluginChildAsset, 0)
	rootChildren := make([]pluginChildAsset, 0)
	if pluginItem.SourcePath == ".claude-plugin/plugin.json" && hasObjectMCPServers([]byte(pluginItem.Content)) {
		children, err := buildPluginMCPChildrenFromContent(parser, pluginItem.SourcePath, []byte(pluginItem.Content))
		if err != nil {
			return nil, err
		}
		manifestChildren = append(manifestChildren, children...)
	}
	for _, asset := range assets {
		if asset.Binary || (asset.Path != ".mcp.json" && asset.Path != ".claude-plugin/plugin.json") {
			continue
		}
		if asset.Path == ".claude-plugin/plugin.json" && !hasObjectMCPServers(asset.Content) {
			continue
		}
		children, err := buildPluginMCPChildrenFromContent(parser, asset.Path, asset.Content)
		if err != nil {
			return nil, err
		}
		if asset.Path == ".mcp.json" {
			rootChildren = append(rootChildren, children...)
		} else {
			manifestChildren = append(manifestChildren, children...)
		}
	}
	return mergePluginMCPChildren(manifestChildren, rootChildren), nil
}

func mergePluginMCPChildren(manifestChildren, rootChildren []pluginChildAsset) []pluginChildAsset {
	out := make([]pluginChildAsset, 0, len(manifestChildren)+len(rootChildren))
	bySlug := make(map[string]int, len(manifestChildren)+len(rootChildren))
	appendOrReplace := func(child pluginChildAsset) {
		key := child.ItemType + ":" + child.SlugSuffix
		if idx, ok := bySlug[key]; ok {
			out[idx] = child
			return
		}
		bySlug[key] = len(out)
		out = append(out, child)
	}
	for _, child := range manifestChildren {
		appendOrReplace(child)
	}
	// A root .mcp.json is the executable MCP config. When the manifest also
	// repeats mcpServers, prefer the root config and avoid duplicate children.
	for _, child := range rootChildren {
		appendOrReplace(child)
	}
	return out
}

func archiveAssetContentSHA(asset services.ArchiveAsset) string {
	if asset.ContentSHA != "" {
		return asset.ContentSHA
	}
	sum := sha256.Sum256(asset.Content)
	return hex.EncodeToString(sum[:])
}

// buildSubSkillAssetRecords uploads the child's binary assets under a
// revision-scoped key prefix. The prefix is what makes the UPDATE path safe:
// keys never collide with the previous revision's live objects, so a Put
// failure can neither truncate them (LocalBackend.Put = os.Create) nor can the
// failure-path cleanupStorageKeys(uploadedKeys) delete objects still
// referenced by the current asset rows. Old-revision objects are removed only
// on the success path via staleStorageKeys.
func buildSubSkillAssetRecords(childID string, revision int, ss pluginChildAsset) ([]models.CapabilityAsset, []string, error) {
	records := make([]models.CapabilityAsset, 0, len(ss.Assets))
	uploadedKeys := make([]string, 0)
	for _, asset := range ss.Assets {
		relPath := strings.TrimSpace(asset.Path)
		if relPath == "" || relPath == "SKILL.md" || strings.Contains(relPath, "..") {
			continue
		}
		record := models.CapabilityAsset{
			RelPath:    relPath,
			MimeType:   asset.MimeType,
			FileSize:   asset.Size,
			ContentSHA: archiveAssetContentSHA(asset),
		}
		if record.MimeType == "" {
			record.MimeType = services.InferMimeType(relPath)
		}
		if record.FileSize <= 0 {
			record.FileSize = int64(len(asset.Content))
		}
		if asset.Binary {
			if StorageBackend == nil {
				return records, uploadedKeys, fmt.Errorf("storage backend is not configured")
			}
			storageKey := fmt.Sprintf("%s/assets/r%d/%s", childID, revision, relPath)
			if err := StorageBackend.Put(context.Background(), storageKey, bytes.NewReader(asset.Content), record.FileSize); err != nil {
				return records, uploadedKeys, err
			}
			uploadedKeys = append(uploadedKeys, storageKey)
			record.StorageBackend = "local"
			record.StorageKey = storageKey
		} else {
			text := string(asset.Content)
			record.TextContent = &text
		}
		records = append(records, record)
	}
	return records, uploadedKeys, nil
}

func subSkillAssetsMatch(db *gorm.DB, childID string, expected []services.ArchiveAsset) bool {
	var current []models.CapabilityAsset
	if err := db.Where("item_id = ?", childID).Find(&current).Error; err != nil {
		return false
	}
	if len(current) != len(expected) {
		return false
	}
	currentByPath := make(map[string]models.CapabilityAsset, len(current))
	for _, asset := range current {
		currentByPath[asset.RelPath] = asset
	}
	for _, expectedAsset := range expected {
		relPath := strings.TrimSpace(expectedAsset.Path)
		cur, ok := currentByPath[relPath]
		if !ok {
			return false
		}
		if cur.ContentSHA != archiveAssetContentSHA(expectedAsset) || cur.FileSize != expectedAsset.Size {
			return false
		}
		if expectedAsset.Binary && cur.StorageKey == "" {
			return false
		}
		if !expectedAsset.Binary && cur.TextContent == nil {
			return false
		}
	}
	return true
}

func replaceSubSkillAssets(tx *gorm.DB, childID string, records []models.CapabilityAsset) error {
	if err := tx.Where("item_id = ?", childID).Delete(&models.CapabilityAsset{}).Error; err != nil {
		return err
	}
	for i := range records {
		records[i].ID = uuid.New().String()
		records[i].ItemID = childID
		if err := tx.Create(&records[i]).Error; err != nil {
			return err
		}
	}
	return nil
}

func existingAssetStorageKeys(db *gorm.DB, childID string) []string {
	var assets []models.CapabilityAsset
	db.Select("storage_key").Where("item_id = ? AND storage_key <> ''", childID).Find(&assets)
	keys := make([]string, 0, len(assets))
	for _, asset := range assets {
		if asset.StorageKey != "" {
			keys = append(keys, asset.StorageKey)
		}
	}
	return keys
}

func staleStorageKeys(oldKeys []string, records []models.CapabilityAsset) []string {
	current := make(map[string]struct{}, len(records))
	for _, record := range records {
		if record.StorageKey != "" {
			current[record.StorageKey] = struct{}{}
		}
	}
	stale := make([]string, 0, len(oldKeys))
	for _, key := range oldKeys {
		if _, ok := current[key]; !ok {
			stale = append(stale, key)
		}
	}
	return stale
}

func reconcileExistingPluginSubSkill(h *ItemHandler, pluginItem *models.CapabilityItem, child models.CapabilityItem, ss pluginChildAsset, contentMD5 string, createdBy string) (*models.CapabilityItem, bool) {
	parentID := pluginItem.ID
	updates := map[string]any{}
	newRevision := child.CurrentRevision
	contentChanged := child.ContentMD5 != contentMD5
	assetsChanged := !subSkillAssetsMatch(h.db, child.ID, ss.Assets)
	sourcePathChanged := child.SourcePath != ss.SourcePath
	if contentChanged || assetsChanged {
		newRevision = child.CurrentRevision + 1
		updates["content"] = ss.Content
		updates["content_md5"] = contentMD5
		updates["current_revision"] = newRevision
		updates["updated_by"] = createdBy
	}
	if sourcePathChanged {
		updates["source_path"] = ss.SourcePath
	}
	if len(ss.Metadata) > 0 && string(child.Metadata) != string(ss.Metadata) {
		updates["metadata"] = ss.Metadata
	}
	if child.Status != "active" {
		updates["status"] = "active"
	}
	if child.ParentPluginID == nil || *child.ParentPluginID != parentID {
		updates["parent_plugin_id"] = parentID
	}
	if child.RegistryID != pluginItem.RegistryID {
		updates["registry_id"] = pluginItem.RegistryID
	}
	if child.RepoID != pluginItem.RepoID {
		updates["repo_id"] = pluginItem.RepoID
	}
	if len(updates) == 0 {
		return nil, false
	}

	oldStorageKeys := existingAssetStorageKeys(h.db, child.ID)
	var records []models.CapabilityAsset
	var uploadedKeys []string
	if contentChanged || assetsChanged {
		var err error
		// Revision-scoped keys: failure cleanup below only ever touches the
		// new revision's objects, never the live ones (see buildSubSkillAssetRecords).
		records, uploadedKeys, err = buildSubSkillAssetRecords(child.ID, newRevision, ss)
		if err != nil {
			cleanupStorageKeys(uploadedKeys)
			log.Printf("sub-skill reconcile: asset build failed for %s: %v", child.ID, err)
			return nil, false
		}
	}
	if err := h.db.Transaction(func(tx *gorm.DB) error {
		if err := tx.Model(&models.CapabilityItem{}).Where("id = ?", child.ID).Updates(updates).Error; err != nil {
			return err
		}
		if contentChanged || assetsChanged {
			if err := replaceSubSkillAssets(tx, child.ID, records); err != nil {
				return err
			}
			version := models.CapabilityVersion{
				ID:          uuid.New().String(),
				ItemID:      child.ID,
				Revision:    newRevision,
				Name:        child.Name,
				Description: child.Description,
				Category:    child.Category,
				Version:     child.Version,
				Content:     ss.Content,
				ContentMD5:  contentMD5,
				Metadata:    ss.Metadata,
				SourcePath:  ss.SourcePath,
				CommitMsg:   "Sub-skill content updated",
				CreatedBy:   createdBy,
			}
			if err := tx.Create(&version).Error; err != nil {
				return err
			}
			for _, snapshotAsset := range cloneItemAssetsToVersionAssets(version.ID, records) {
				asset := snapshotAsset
				if err := tx.Create(&asset).Error; err != nil {
					return err
				}
			}
		}
		return nil
	}); err != nil {
		cleanupStorageKeys(uploadedKeys)
		log.Printf("sub-skill reconcile: update failed for %s: %v", child.ID, err)
		return nil, false
	}
	if contentChanged || assetsChanged {
		cleanupStorageKeys(staleStorageKeys(oldStorageKeys, records))
		enqueueScanAsync(child.ID, newRevision, "update")
	}

	child.Content = ss.Content
	child.ContentMD5 = contentMD5
	child.Metadata = ss.Metadata
	child.CurrentRevision = newRevision
	child.SourcePath = ss.SourcePath
	child.Status = "active"
	child.ParentPluginID = &parentID
	child.RegistryID = pluginItem.RegistryID
	child.RepoID = pluginItem.RepoID
	return &child, true
}

func legacySingleMCPSourcePath(sourcePath string) string {
	if !strings.HasPrefix(sourcePath, ".mcp.json#") {
		return ""
	}
	return ".mcp.json"
}

func pluginChildBaseSlug(pluginSlug string, child pluginChildAsset) string {
	baseSlug := slugify(pluginSlug + "-" + child.SlugSuffix)
	if baseSlug == "" {
		baseSlug = slugify(child.Name)
	}
	return baseSlug
}

// reconcilePluginSubSkills promotes each sub-skill bundled in a plugin archive into
// a first-class item_type=skill row linked back via parent_plugin_id, reusing the
// existing skill download/install pipeline. It is idempotent: re-running with the
// same archive neither duplicates rows nor needlessly bumps revisions.
//
//   - new sub-skill (source_path not yet present)      -> create a child skill item
//   - existing sub-skill, content changed              -> update Content/ContentMD5 (bump revision)
//   - existing sub-skill, content unchanged            -> no-op (idempotent)
//   - existing sub-skill, previously archived          -> re-activate
//   - existing child whose source_path is gone in zip  -> archive (status='archived')
//
// It returns the child items that were created so callers can trigger async indexing.
// Child failures never roll back the parent plugin; they are logged and skipped so a
// subsequent re-upload can reconcile them.
func reconcilePluginSubSkills(h *ItemHandler, pluginItem *models.CapabilityItem, assets []services.ArchiveAsset, createdBy string) []*models.CapabilityItem {
	subSkills := extractSubSkillAssets(assets)
	subSkills = append(subSkills, extractPluginFileChildren(assets)...)
	if h.parserSvc != nil {
		mcpChildren, err := extractPluginMCPAssets(h.parserSvc, pluginItem, assets)
		if err != nil {
			log.Printf("plugin child promote: parse MCP config failed for plugin %s: %v", pluginItem.ID, err)
		} else {
			subSkills = append(subSkills, mcpChildren...)
		}
	}

	// Existing children of this plugin, keyed by source_path and by slug.
	var existing []models.CapabilityItem
	h.db.Where("parent_plugin_id = ?", pluginItem.ID).Find(&existing)
	existingByPath := make(map[string]models.CapabilityItem, len(existing))
	existingBySlug := make(map[string]models.CapabilityItem, len(existing))
	for _, ch := range existing {
		existingByPath[ch.SourcePath] = ch
		existingBySlug[ch.Slug] = ch
	}

	// Full path set up-front: the slug-adoption fallback below must only treat
	// a row as "migrated" when its old path is truly absent from THIS archive
	// (otherwise a later iteration would path-match the same row).
	newPaths := make(map[string]struct{}, len(subSkills))
	for _, ss := range subSkills {
		newPaths[ss.SourcePath] = struct{}{}
	}
	reconciledIDs := make(map[string]struct{}, len(subSkills))
	created := make([]*models.CapabilityItem, 0, len(subSkills))
	hashSvc := services.NewContentHashService()

	for _, ss := range subSkills {
		contentMD5, err := hashSvc.HashArchiveContent(ss.SourcePath, []byte(ss.Content), ss.Assets)
		if err != nil {
			log.Printf("sub-skill promote: hash failed for %s of plugin %s: %v", ss.SourcePath, pluginItem.ID, err)
			continue
		}

		if child, ok := existingByPath[ss.SourcePath]; ok {
			if updated, ok := reconcileExistingPluginSubSkill(h, pluginItem, child, ss, contentMD5, createdBy); ok {
				created = append(created, updated)
			}
			reconciledIDs[child.ID] = struct{}{}
			continue
		}
		if legacyPath := legacySingleMCPSourcePath(ss.SourcePath); legacyPath != "" {
			if child, ok := existingByPath[legacyPath]; ok {
				if child.Slug == pluginChildBaseSlug(pluginItem.Slug, ss) {
					if updated, ok := reconcileExistingPluginSubSkill(h, pluginItem, child, ss, contentMD5, createdBy); ok {
						created = append(created, updated)
					}
					reconciledIDs[child.ID] = struct{}{}
					continue
				}
			}
		}
		// Path migration (e.g. skills/foo → skills/sub/foo, or an MCP server
		// moving across config files): same logical child, same slug, new
		// path. Adopt the existing row instead of letting the create loop hit
		// the unique slug index and mint a -2 suffixed duplicate while the old
		// row gets archived.
		if child, ok := existingBySlug[pluginChildBaseSlug(pluginItem.Slug, ss)]; ok {
			_, claimed := reconciledIDs[child.ID]
			_, oldPathStillShipped := newPaths[child.SourcePath]
			if !claimed && !oldPathStillShipped {
				if updated, ok := reconcileExistingPluginSubSkill(h, pluginItem, child, ss, contentMD5, createdBy); ok {
					created = append(created, updated)
				}
				reconciledIDs[child.ID] = struct{}{}
				continue
			}
		}

		// New sub-skill: create a child skill item with slug collision retry.
		baseSlug := pluginChildBaseSlug(pluginItem.Slug, ss)
		parentID := pluginItem.ID
		var childItem *models.CapabilityItem
		var persistErr error
		for attempt := 0; attempt < 10; attempt++ {
			slug := baseSlug
			if attempt > 0 {
				slug = fmt.Sprintf("%s-%d", baseSlug, attempt+1)
			}
			childID := uuid.New().String()
			assetRecords, uploadedKeys, assetErr := buildSubSkillAssetRecords(childID, 1, ss)
			if assetErr != nil {
				cleanupStorageKeys(uploadedKeys)
				persistErr = assetErr
				log.Printf("sub-skill promote: asset build failed for %q of plugin %s: %v", ss.Name, pluginItem.ID, assetErr)
				break
			}
			childItem, persistErr = persistNewItem(h.db, createItemRequest{
				ID:             childID,
				RegistryID:     pluginItem.RegistryID,
				RepoID:         pluginItem.RepoID,
				Slug:           slug,
				ItemType:       ss.ItemType,
				Name:           ss.Name,
				Description:    ss.Description,
				Version:        ss.Version,
				Content:        ss.Content,
				ContentMD5:     contentMD5,
				Metadata:       ss.Metadata,
				SourcePath:     ss.SourcePath,
				SourceType:     "archive",
				CreatedBy:      createdBy,
				ParentPluginID: &parentID,
			}, createItemAssets{Records: assetRecords})
			if persistErr == nil {
				enqueueScanAsync(childItem.ID, 1, "create")
				break
			}
			cleanupStorageKeys(uploadedKeys)
			if errors.Is(persistErr, ErrSlugConflict) {
				// Own-child adoption first: the slot holder being THIS plugin's
				// child (any status, any source_path) means we raced another
				// upload or migrated paths — update that row instead of
				// suffixing a duplicate.
				var own models.CapabilityItem
				if err := h.db.Where("repo_id = ? AND item_type = ? AND slug = ? AND parent_plugin_id = ?", pluginItem.RepoID, ss.ItemType, slug, pluginItem.ID).First(&own).Error; err == nil {
					if updated, ok := reconcileExistingPluginSubSkill(h, pluginItem, own, ss, contentMD5, createdBy); ok {
						childItem = updated
						persistErr = nil
						reconciledIDs[own.ID] = struct{}{}
					}
					break
				}
				// Legacy: an archived foreign row left on the exact same path.
				var archived models.CapabilityItem
				if err := h.db.Where("repo_id = ? AND item_type = ? AND slug = ? AND status = ? AND source_path = ?", pluginItem.RepoID, ss.ItemType, slug, "archived", ss.SourcePath).First(&archived).Error; err == nil {
					if updated, ok := reconcileExistingPluginSubSkill(h, pluginItem, archived, ss, contentMD5, createdBy); ok {
						childItem = updated
						persistErr = nil
						reconciledIDs[archived.ID] = struct{}{}
					}
					break
				}
				continue
			}
			break
		}
		if persistErr != nil {
			log.Printf("sub-skill promote: failed to create child skill %q for plugin %s: %v", ss.Name, pluginItem.ID, persistErr)
			continue
		}
		created = append(created, childItem)
	}

	// Archive children whose source_path no longer exists in the uploaded archive.
	for _, ch := range existing {
		if _, ok := reconciledIDs[ch.ID]; ok {
			continue
		}
		if _, ok := newPaths[ch.SourcePath]; ok {
			continue
		}
		if ch.Status == "archived" {
			continue
		}
		if err := h.db.Model(&models.CapabilityItem{}).Where("id = ?", ch.ID).Update("status", "archived").Error; err != nil {
			log.Printf("sub-skill reconcile: archive failed for %s: %v", ch.ID, err)
		}
	}

	return created
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
	if itemType == "" {
		itemType = c.GetString("defaultItemType")
	}
	name := c.PostForm("name")
	slug := c.PostForm("slug")
	registryID := c.PostForm("registryId")
	description := c.PostForm("description")
	category := c.PostForm("category")
	version := c.PostForm("version")

	if itemType == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "itemType is required"})
		return
	}

	switch itemType {
	case "skill", "mcp", "plugin":
	default:
		c.JSON(http.StatusBadRequest, gin.H{"error": "itemType must be skill, mcp or plugin"})
		return
	}

	if registryID == "" {
		registryID = PublicRegistryID
	}

	createdBy := c.GetString(middleware.UserIDKey)
	if createdBy == "" {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "Authentication required"})
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

	result, err := h.archiveSvc.ParseArchive(readerAt, header.Size, header.Filename, itemType)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	if result == nil || result.Parsed == nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Archive parser returned no item"})
		return
	}
	if name == "" {
		name = result.Parsed.Name
	}
	if name == "" && header != nil {
		name = strings.TrimSuffix(header.Filename, filepath.Ext(header.Filename))
	}
	if name == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "name is required"})
		return
	}
	if slug == "" {
		slug = slugify(name)
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

	// For plugin type, extract cospowers.config.json from assets as metadata
	if itemType == "plugin" {
		for _, asset := range result.Assets {
			if asset.Path == "cospowers.config.json" && !asset.Binary {
				var config map[string]any
				if err := json.Unmarshal(asset.Content, &config); err == nil {
					metadataMap = config
				}
				break
			}
		}
		// Extract description from CLAUDE.md if empty.
		if description == "" && result.MainPath == "CLAUDE.md" && result.MainContent != "" {
			description = extractFirstParagraph(result.MainContent)
		}
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

	// Promote bundled sub-skills (skills/<name>/SKILL.md) into standalone skill items
	// linked to this plugin via parent_plugin_id. Best-effort: child failures don't
	// roll back the parent plugin (a later re-upload reconciles them).
	if itemType == "plugin" {
		_ = reconcilePluginSubSkills(h, item, result.Assets, createdBy)
	}

	if h.tagSvc != nil {
		if err := assignTagsForItem(h.tagSvc, item.ID, resolvedTagIDs); err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to assign item tags"})
			return
		}
	}

	enqueueScanAsync(item.ID, 1, "create")
	c.JSON(http.StatusCreated, buildItemResponse(c, h.db, *item, c.GetString(middleware.UserIDKey)))
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

// extractFirstParagraph extracts the first non-empty, non-heading paragraph from markdown content.
func extractFirstParagraph(content string) string {
	lines := strings.Split(content, "\n")
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line != "" && !strings.HasPrefix(line, "#") {
			return line
		}
	}
	return ""
}
