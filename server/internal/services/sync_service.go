package services

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/costrict/costrict-web/server/internal/models"
	"github.com/google/uuid"
	"gorm.io/gorm"
)

type SyncService struct {
	DB     *gorm.DB
	Git    *GitService
	Parser *ParserService
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

func (s *SyncService) parseFile(content []byte, relPath string) (*ParsedItem, error) {
	lower := strings.ToLower(relPath)
	base := strings.ToLower(filepath.Base(relPath))

	switch {
	case base == "plugin.json":
		return s.Parser.ParsePluginJSON(content, relPath)
	case base == "agents.md" || strings.HasSuffix(lower, "/agents.md"):
		items, err := s.Parser.ParseAgentsMD(content, relPath)
		if err != nil || len(items) == 0 {
			return s.Parser.ParseSKILLMD(content, relPath)
		}
		return items[0], nil
	default:
		return s.Parser.ParseSKILLMD(content, relPath)
	}
}

func (s *SyncService) SyncRegistry(ctx context.Context, registryID string, opts SyncOptions) (*SyncResult, error) {
	var registry models.CapabilityRegistry
	if err := s.DB.First(&registry, "id = ?", registryID).Error; err != nil {
		return nil, fmt.Errorf("registry not found: %w", err)
	}

	if registry.ExternalURL == "" {
		return nil, fmt.Errorf("registry has no external URL configured")
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
			syncLog.ErrorMessage = result.Errors[0]
		}

		if !opts.DryRun {
			s.DB.Save(syncLog)
			newStatus := "idle"
			if result.Status == "failed" {
				newStatus = "error"
			}
			s.DB.Model(&registry).Updates(map[string]any{"sync_status": newStatus, "last_sync_log_id": syncLog.ID})
		}
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
		result.Status = "success"
		return result, nil
	}

	var cfg syncConfig
	if len(registry.SyncConfig) > 0 {
		_ = json.Unmarshal(registry.SyncConfig, &cfg)
	}
	if len(cfg.IncludePatterns) == 0 {
		cfg.IncludePatterns = []string{"**/*.md", "**/SKILL.md", "**/plugin.json", "**/.claude-plugin/plugin.json"}
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

		contentHash := s.Git.ContentHash(content)
		seenPaths[relPath] = true

		parsed, err := s.parseFile(content, relPath)
		if err != nil {
			result.Failed++
			result.Errors = append(result.Errors, fmt.Sprintf("parse %s: %v", relPath, err))
			continue
		}
		parsed.ContentHash = contentHash

		existing, exists := existingByPath[relPath]

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
			var maxVer int
			s.DB.Model(&models.CapabilityVersion{}).Where("item_id = ?", existing.ID).Select("COALESCE(MAX(version), 0)").Scan(&maxVer)

			existing.Name = parsed.Name
			existing.Description = parsed.Description
			existing.Category = parsed.Category
			existing.Version = parsed.Version
			existing.Content = parsed.Content
			existing.SourceSHA = contentHash
			existing.UpdatedBy = "sync"

			if err := s.DB.Save(existing).Error; err != nil {
				result.Failed++
				result.Errors = append(result.Errors, fmt.Sprintf("update %s: %v", relPath, err))
				continue
			}

			ver := &models.CapabilityVersion{
				ID:        uuid.New().String(),
				ItemID:    existing.ID,
				Version:   maxVer + 1,
				Content:   parsed.Content,
				CommitMsg: fmt.Sprintf("sync: %s", cloneResult.CommitSHA[:8]),
				CreatedBy: "sync",
			}
			s.DB.Create(ver)
			result.Updated++
		} else {
			newItem := &models.CapabilityItem{
				ID:          uuid.New().String(),
				RegistryID:  registryID,
				Slug:        parsed.Slug,
				ItemType:    parsed.ItemType,
				Name:        parsed.Name,
				Description: parsed.Description,
				Category:    parsed.Category,
				Version:     parsed.Version,
				Content:     parsed.Content,
				SourcePath:  relPath,
				SourceSHA:   contentHash,
				Visibility:  registry.Visibility,
				Status:      "active",
				CreatedBy:   "sync",
				UpdatedBy:   "sync",
			}
			if err := s.DB.Create(newItem).Error; err != nil {
				result.Failed++
				result.Errors = append(result.Errors, fmt.Sprintf("create %s: %v", relPath, err))
				continue
			}

			ver := &models.CapabilityVersion{
				ID:        uuid.New().String(),
				ItemID:    newItem.ID,
				Version:   1,
				Content:   parsed.Content,
				CommitMsg: fmt.Sprintf("sync: initial import from %s", cloneResult.CommitSHA[:8]),
				CreatedBy: "sync",
			}
			s.DB.Create(ver)
			result.Added++
		}
	}

	if !opts.DryRun {
		for path, item := range existingByPath {
			if !seenPaths[path] {
				s.DB.Model(item).Updates(map[string]any{"status": "archived"})
				result.Deleted++
			}
		}

		now := time.Now()
		s.DB.Model(&registry).Updates(map[string]any{
			"last_sync_sha":  cloneResult.CommitSHA,
			"last_synced_at": now,
		})
	}

	total := result.Added + result.Updated + result.Deleted + result.Skipped + result.Failed
	syncLog.TotalItems = total
	result.Status = "success"

	return result, nil
}
