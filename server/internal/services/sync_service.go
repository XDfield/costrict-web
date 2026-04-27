package services

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/costrict/costrict-web/server/internal/logger"
	"github.com/costrict/costrict-web/server/internal/models"
	"github.com/google/uuid"
	"gorm.io/datatypes"
	"gorm.io/gorm"
)

type SyncService struct {
	DB             *gorm.DB
	Git            *GitService
	Parser         *ParserService
	ScanJobService *ScanJobService
	CategorySvc    *CategoryService
	TagSvc         *TagService
}

type SyncOptions struct {
	TriggerType string
	TriggerUser string
	DryRun      bool
}

type SyncResult struct {
	LogID       string
	CommitSHA   string
	PreviousSHA string
	Status      string
	Added       int
	Updated     int
	Deleted     int
	Skipped     int
	Failed      int
	Errors      []string
	Duration    time.Duration
}

type syncConfig struct {
	IncludePatterns  []string `json:"includePatterns"`
	ExcludePatterns  []string `json:"excludePatterns"`
	ConflictStrategy string   `json:"conflictStrategy"`
}

// syncRepoID returns repoID with a fallback to "public" for empty values.
func syncRepoID(repoID string) string {
	if repoID == "" {
		return "public"
	}
	return repoID
}

func (s *SyncService) applyPluginJSON(content []byte, registry *models.CapabilityRegistry) {
	item, err := s.Parser.ParsePluginJSON(content, ".claude-plugin/plugin.json")
	if err != nil {
		return
	}
	updates := map[string]any{}
	if item.Name != "" && registry.Name == "" {
		updates["name"] = item.Name
	}
	if item.Description != "" && registry.Description == "" {
		updates["description"] = item.Description
	}
	if len(updates) > 0 {
		s.DB.Model(registry).Updates(updates)
	}
}

// parseFile returns one or more ParsedItems for the given file content.
func (s *SyncService) parseFile(content []byte, relPath string) ([]*ParsedItem, error) {
	lower := strings.ToLower(relPath)
	base := strings.ToLower(filepath.Base(relPath))

	switch {
	case base == "hooks.json":
		item, err := s.Parser.ParseHooksJSON(content, relPath)
		if err != nil {
			return nil, err
		}
		return []*ParsedItem{item}, nil
	case base == ".mcp.json":
		return s.Parser.ParseMCPJSON(content, relPath)
	case base == "agents.md" || strings.HasSuffix(lower, "/agents.md"):
		items, err := s.Parser.ParseAgentsMD(content, relPath)
		if err != nil || len(items) == 0 {
			item, err2 := s.Parser.ParseSKILLMD(content, relPath)
			if err2 != nil {
				return nil, err2
			}
			return []*ParsedItem{item}, nil
		}
		return items, nil
	default:
		item, err := s.Parser.ParseSKILLMD(content, relPath)
		if err != nil {
			return nil, err
		}
		return []*ParsedItem{item}, nil
	}
}

func (s *SyncService) enqueueScan(itemID string, revision int) {
	if s.ScanJobService == nil {
		return
	}
	go func() {
		_, _ = s.ScanJobService.Enqueue(itemID, revision, "sync", "", ScanEnqueueOptions{})
	}()
}

func sanitizeSyncContent(content []byte) []byte {
	return bytes.ReplaceAll(content, []byte{0}, nil)
}

func metadataJSON(m map[string]any) datatypes.JSON {
	if len(m) == 0 {
		return datatypes.JSON([]byte("{}"))
	}
	b, err := json.Marshal(m)
	if err != nil {
		return datatypes.JSON([]byte("{}"))
	}
	return datatypes.JSON(b)
}

func (s *SyncService) SyncRegistry(ctx context.Context, registryID string, opts SyncOptions) (*SyncResult, error) {
	var registry models.CapabilityRegistry
	if err := s.DB.First(&registry, "id = ?", registryID).Error; err != nil {
		return nil, fmt.Errorf("registry not found: %w", err)
	}

	if registry.ExternalURL == "" {
		return nil, fmt.Errorf("registry has no external URL configured")
	}

	// Resolve the user who triggered this sync; fall back to "sync" for
	// automated / webhook triggers where no user context is available.
	triggerUser := opts.TriggerUser
	if triggerUser == "" {
		triggerUser = "sync"
	}

	syncLog := &models.SyncLog{
		ID:          uuid.New().String(),
		RegistryID:  registryID,
		TriggerType: opts.TriggerType,
		TriggerUser: opts.TriggerUser,
		PreviousSHA: registry.LastSyncSHA,
		Status:      "running",
		StartedAt:   time.Now(),
	}
	s.DB.Create(syncLog)

	logger.Info("sync started registry=%s trigger=%s dryRun=%v logID=%s",
		registryID, opts.TriggerType, opts.DryRun, syncLog.ID)

	if !opts.DryRun {
		s.DB.Model(&registry).Updates(map[string]any{"sync_status": "syncing"})
	}

	result := &SyncResult{LogID: syncLog.ID}
	startTime := time.Now()

	defer func() {
		result.Duration = time.Since(startTime)
		finishedAt := time.Now()
		syncLog.FinishedAt = &finishedAt
		syncLog.DurationMs = result.Duration.Milliseconds()
		syncLog.CommitSHA = result.CommitSHA
		syncLog.AddedItems = result.Added
		syncLog.UpdatedItems = result.Updated
		syncLog.DeletedItems = result.Deleted
		syncLog.SkippedItems = result.Skipped
		syncLog.FailedItems = result.Failed

		if result.Status == "" {
			result.Status = "success"
		}
		syncLog.Status = result.Status

		if len(result.Errors) > 0 {
			if errBytes, err := json.Marshal(result.Errors); err == nil {
				syncLog.ErrorMessage = string(errBytes)
			} else {
				syncLog.ErrorMessage = result.Errors[0]
			}
		}

		if !opts.DryRun {
			s.DB.Save(syncLog)
			newStatus := "idle"
			if result.Status == "failed" {
				newStatus = "error"
			}
			s.DB.Model(&registry).Updates(map[string]any{"sync_status": newStatus, "last_sync_log_id": syncLog.ID})
		}

		logger.Info("sync finished registry=%s status=%s duration=%s added=%d updated=%d deleted=%d skipped=%d failed=%d logID=%s",
			registryID, result.Status, result.Duration.Round(time.Millisecond),
			result.Added, result.Updated, result.Deleted, result.Skipped, result.Failed,
			syncLog.ID)
	}()

	branch := registry.ExternalBranch
	if branch == "" {
		branch = "main"
	}

	cloneResult, err := s.Git.Clone(registry.ExternalURL, branch)
	if err != nil {
		result.Status = "failed"
		result.Errors = append(result.Errors, err.Error())
		return result, err
	}
	defer s.Git.Cleanup(cloneResult.LocalPath)

	result.CommitSHA = cloneResult.CommitSHA
	result.PreviousSHA = registry.LastSyncSHA

	if cloneResult.CommitSHA == registry.LastSyncSHA && registry.LastSyncSHA != "" {
		logger.Info("sync skipped registry=%s sha=%s (no changes)", registryID, cloneResult.CommitSHA)
		result.Status = "success"
		return result, nil
	}

	logger.Info("sync cloned registry=%s sha=%s previousSha=%s files to process",
		registryID, cloneResult.CommitSHA, registry.LastSyncSHA)

	var cfg syncConfig
	if len(registry.SyncConfig) > 0 {
		_ = json.Unmarshal(registry.SyncConfig, &cfg)
	}
	if len(cfg.IncludePatterns) == 0 {
		cfg.IncludePatterns = []string{
			"skills/**/SKILL.md",
			"commands/**/*.md",
			"agents/**/*.md",
			".claude-plugin/plugin.json",
			"hooks/hooks.json",
			".mcp.json",
		}
	}
	if cfg.ConflictStrategy == "" {
		cfg.ConflictStrategy = "keep_remote"
	}

	files, err := s.Git.ListFiles(cloneResult.LocalPath, cfg.IncludePatterns, cfg.ExcludePatterns)
	if err != nil {
		result.Status = "failed"
		result.Errors = append(result.Errors, err.Error())
		return result, err
	}

	var existingItems []models.CapabilityItem
	s.DB.Where("registry_id = ? AND status = 'active'", registryID).Find(&existingItems)
	existingByPath := make(map[string]*models.CapabilityItem, len(existingItems))
	for i := range existingItems {
		existingByPath[existingItems[i].SourcePath] = &existingItems[i]
	}

	// Index globally existing items by repo+type+slug so that cross-registry
	// duplicates are treated as updates instead of inserts.
	var globalItems []models.CapabilityItem
	s.DB.Where("repo_id = ? AND status = 'active'", syncRepoID(registry.RepoID)).Find(&globalItems)
	for i := range globalItems {
		key := globalItems[i].ItemType + ":" + globalItems[i].Slug
		if _, exists := existingByPath[key]; !exists {
			existingByPath[key] = &globalItems[i]
		}
	}

	seenPaths := make(map[string]bool)

	for _, relPath := range files {
		select {
		case <-ctx.Done():
			result.Status = "failed"
			result.Errors = append(result.Errors, "context cancelled")
			return result, ctx.Err()
		default:
		}

		content, err := s.Git.ReadFile(cloneResult.LocalPath, relPath)
		if err != nil {
			result.Failed++
			result.Errors = append(result.Errors, fmt.Sprintf("read %s: %v", relPath, err))
			continue
		}
		content = sanitizeSyncContent(content)

		if strings.ToLower(filepath.Base(relPath)) == "plugin.json" {
			if !opts.DryRun {
				s.applyPluginJSON(content, &registry)
			}
			continue
		}

		contentHash := s.Git.ContentHash(content)
		seenPaths[relPath] = true

		parsedItems, err := s.parseFile(content, relPath)
		if err != nil {
			result.Failed++
			result.Errors = append(result.Errors, fmt.Sprintf("parse %s: %v", relPath, err))
			continue
		}

		for _, parsed := range parsedItems {
			parsed.ContentHash = contentHash

			itemKey := relPath
			if len(parsedItems) > 1 {
				itemKey = relPath + "#" + parsed.Slug
			}

			existing, exists := existingByPath[itemKey]
			if !exists && len(parsedItems) > 1 {
				existing = existingByPath[relPath]
				exists = existing != nil && existing.Slug == parsed.Slug
			}
			if !exists {
				existing = existingByPath[parsed.ItemType+":"+parsed.Slug]
				exists = existing != nil
			}

			if exists && existing.SourceSHA == contentHash {
				result.Skipped++
				continue
			}

			if exists && cfg.ConflictStrategy == "keep_local" {
				localContentHash := s.Git.ContentHash([]byte(existing.Content))
				if localContentHash != existing.SourceSHA {
					result.Skipped++
					continue
				}
			}

			if opts.DryRun {
				if exists {
					result.Updated++
				} else {
					result.Added++
				}
				continue
			}

			if exists {
				var maxRevision int
				s.DB.Model(&models.CapabilityVersion{}).Where("item_id = ?", existing.ID).Select("COALESCE(MAX(revision), 0)").Scan(&maxRevision)

				existing.Name = parsed.Name
				existing.Description = parsed.Description
				existing.Category = parsed.Category
				existing.Version = parsed.Version
				existing.Content = parsed.Content

				if s.CategorySvc != nil && parsed.Category != "" {
					s.CategorySvc.EnsureCategory(parsed.Category, triggerUser)
				}
				if s.TagSvc != nil && len(parsed.Tags) > 0 {
					tags, err := s.TagSvc.EnsureTags(parsed.Tags, TagClassCustom, triggerUser)
					if err == nil {
						var tagIDs []string
						for _, t := range tags {
							tagIDs = append(tagIDs, t.ID)
						}
						s.TagSvc.SetItemTags(existing.ID, tagIDs)
					}
				}
				meta := parsed.Metadata
				if parsed.ItemType == "mcp" {
					normalized, err := NormalizeMCPMetadata(meta)
					if err != nil {
						result.Failed++
						result.Errors = append(result.Errors, fmt.Sprintf("normalize mcp %s: %v", relPath, err))
						continue
					}
					meta = normalized
				}
				existing.Metadata = metadataJSON(meta)
				existing.SourceSHA = contentHash
				existing.UpdatedBy = triggerUser

				if err := s.DB.Save(existing).Error; err != nil {
					result.Failed++
					result.Errors = append(result.Errors, fmt.Sprintf("update %s: %v", relPath, err))
					continue
				}

				ver := &models.CapabilityVersion{
					ID:        uuid.New().String(),
					ItemID:    existing.ID,
					Revision:  maxRevision + 1,
					Content:   parsed.Content,
					Metadata:  metadataJSON(meta),
					CommitMsg: fmt.Sprintf("sync: %s", cloneResult.CommitSHA[:8]),
					CreatedBy: triggerUser,
				}
				s.DB.Create(ver)
				s.enqueueScan(existing.ID, maxRevision+1)
				result.Updated++
			} else {
				meta := parsed.Metadata
				if parsed.ItemType == "mcp" {
					normalized, err := NormalizeMCPMetadata(meta)
					if err != nil {
						result.Failed++
						result.Errors = append(result.Errors, fmt.Sprintf("normalize mcp %s: %v", relPath, err))
						continue
					}
					meta = normalized
				}
				newItem := &models.CapabilityItem{
					ID:          uuid.New().String(),
					RegistryID:  registryID,
					RepoID:      syncRepoID(registry.RepoID),
					Slug:        parsed.Slug,
					ItemType:    parsed.ItemType,
					Name:        parsed.Name,
					Description: parsed.Description,
					Category:    parsed.Category,
					Version:     parsed.Version,
					Content:     parsed.Content,
					Metadata:    metadataJSON(meta),
					SourcePath:  relPath,
					SourceSHA:   contentHash,
				Status:      "active",
				CreatedBy:   triggerUser,
				UpdatedBy:   triggerUser,
				}
				if err := s.DB.Create(newItem).Error; err != nil {
					result.Failed++
					result.Errors = append(result.Errors, fmt.Sprintf("create %s: %v", relPath, err))
					continue
				}

				// Index newly created item so later files with the same slug are
				// treated as updates instead of inserts.
				existingByPath[newItem.SourcePath] = newItem
				existingByPath[newItem.ItemType+":"+newItem.Slug] = newItem

				if s.CategorySvc != nil && parsed.Category != "" {
					s.CategorySvc.EnsureCategory(parsed.Category, triggerUser)
				}
				if s.TagSvc != nil && len(parsed.Tags) > 0 {
					tags, err := s.TagSvc.EnsureTags(parsed.Tags, TagClassCustom, triggerUser)
					if err == nil {
						var tagIDs []string
						for _, t := range tags {
							tagIDs = append(tagIDs, t.ID)
						}
						s.TagSvc.SetItemTags(newItem.ID, tagIDs)
					}
				}

				ver := &models.CapabilityVersion{
					ID:        uuid.New().String(),
					ItemID:    newItem.ID,
					Revision:  1,
					Content:   parsed.Content,
					Metadata:  metadataJSON(meta),
					CommitMsg: fmt.Sprintf("sync: initial import from %s", cloneResult.CommitSHA[:8]),
					CreatedBy: triggerUser,
				}
				s.DB.Create(ver)

				s.syncAssets(cloneResult.LocalPath, relPath, newItem.ID, &result.Errors)
				s.enqueueScan(newItem.ID, 1)
				result.Added++
			}
		}
	}

	if !opts.DryRun {
		seenItemIDs := make(map[string]bool)
		for path, item := range existingByPath {
			if seenPaths[path] || item.RegistryID != registryID {
				continue
			}
			if seenItemIDs[item.ID] {
				continue
			}
			seenItemIDs[item.ID] = true
			s.DB.Model(item).Updates(map[string]any{"status": "archived"})
			s.DB.Where("item_id = ?", item.ID).Delete(&models.CapabilityAsset{})
			result.Deleted++
		}

		now := time.Now()
		shaUpdates := map[string]any{"last_synced_at": now}
		if result.Failed == 0 {
			shaUpdates["last_sync_sha"] = cloneResult.CommitSHA
		}
		s.DB.Model(&registry).Updates(shaUpdates)
	}

	total := result.Added + result.Updated + result.Deleted + result.Skipped + result.Failed
	syncLog.TotalItems = total
	result.Status = "success"

	return result, nil
}

// syncAssets collects and upserts non-primary files in the same skill directory.
func (s *SyncService) syncAssets(localPath, relPath, itemID string, errs *[]string) {
	dir := filepath.Dir(relPath)
	if dir == "." {
		return
	}

	allFiles, err := s.Git.ListFiles(localPath, []string{dir + "/**"}, nil)
	if err != nil {
		return
	}

	primaryBase := strings.ToUpper(filepath.Base(relPath))

	var existingAssets []models.CapabilityAsset
	s.DB.Where("item_id = ?", itemID).Find(&existingAssets)
	assetByPath := make(map[string]*models.CapabilityAsset, len(existingAssets))
	for i := range existingAssets {
		assetByPath[existingAssets[i].RelPath] = &existingAssets[i]
	}

	for _, f := range allFiles {
		if strings.ToUpper(filepath.Base(f)) == primaryBase {
			continue
		}
		assetRelPath, _ := filepath.Rel(dir, f)
		assetRelPath = filepath.ToSlash(assetRelPath)

		content, err := s.Git.ReadFile(localPath, f)
		if err != nil {
			*errs = append(*errs, fmt.Sprintf("asset read %s: %v", f, err))
			continue
		}
		content = sanitizeSyncContent(content)

		contentSHA := s.Git.ContentHash(content)

		if existing, ok := assetByPath[assetRelPath]; ok {
			if existing.ContentSHA == contentSHA {
				continue
			}
			text := string(content)
			s.DB.Model(existing).Updates(map[string]any{
				"text_content": text,
				"content_sha":  contentSHA,
				"file_size":    int64(len(content)),
			})
		} else {
			text := string(content)
			asset := &models.CapabilityAsset{
				ID:          uuid.New().String(),
				ItemID:      itemID,
				RelPath:     assetRelPath,
				TextContent: &text,
				MimeType:    InferMimeType(f),
				FileSize:    int64(len(content)),
				ContentSHA:  contentSHA,
			}
			s.DB.Create(asset)
		}
	}
}

func InferMimeType(filePath string) string {
	ext := strings.ToLower(filepath.Ext(filePath))
	switch ext {
	case ".md":
		return "text/markdown"
	case ".json":
		return "application/json"
	case ".yaml", ".yml":
		return "application/yaml"
	case ".sh":
		return "text/x-sh"
	case ".py":
		return "text/x-python"
	case ".js":
		return "text/javascript"
	case ".ts":
		return "text/typescript"
	case ".png":
		return "image/png"
	case ".jpg", ".jpeg":
		return "image/jpeg"
	case ".svg":
		return "image/svg+xml"
	default:
		return "application/octet-stream"
	}
}
