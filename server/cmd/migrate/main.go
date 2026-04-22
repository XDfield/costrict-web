package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/costrict/costrict-web/server/internal/config"
	"github.com/costrict/costrict-web/server/internal/database"
	"github.com/costrict/costrict-web/server/internal/models"
	"github.com/costrict/costrict-web/server/internal/services"
	migrations "github.com/costrict/costrict-web/server/migrations"
	"github.com/google/uuid"
	"github.com/pressly/goose/v3"
	"gorm.io/datatypes"
	"gorm.io/gorm"
)

var errDryRunRollback = errors.New("dry-run rollback")

const (
	publicRegistryID = "00000000-0000-0000-0000-000000000001"
	publicRepoID     = "public"
	importCreatedBy  = "system"
)

func main() {
	cfg := config.Load()

	db, err := database.Initialize(cfg.DatabaseURL)
	if err != nil {
		log.Fatalf("Failed to connect to database: %v", err)
	}

	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "help", "-h", "--help":
			printMigrateHelp()
			return
		case "user-subject-ids":
			dryRun := len(os.Args) > 2 && os.Args[2] == "--dry-run"
			if err := backfillLegacyUserReferences(db, dryRun); err != nil {
				if dryRun && errors.Is(err, errDryRunRollback) {
					log.Println("Legacy user reference dry-run completed successfully")
					return
				}
				log.Fatalf("Failed to backfill legacy user references: %v", err)
			}
			if dryRun {
				log.Println("Legacy user reference dry-run completed successfully")
			} else {
				log.Println("Legacy user reference backfill completed successfully")
			}
			return
		case "import-everything-ai-coding":
			sourcePath := ""
			dryRun := false
			for _, arg := range os.Args[2:] {
				if arg == "--dry-run" {
					dryRun = true
					continue
				}
				if strings.HasPrefix(arg, "--source=") {
					sourcePath = strings.TrimPrefix(arg, "--source=")
					continue
				}
				if !strings.HasPrefix(arg, "--") && sourcePath == "" {
					sourcePath = arg
				}
			}

			if sourcePath == "" {
				log.Fatalf("Missing source path. Use --source=<everything-ai-coding-path> or pass path as positional arg")
			}

			if err := importEverythingAICoding(db, sourcePath, dryRun); err != nil {
				if dryRun && errors.Is(err, errDryRunRollback) {
					log.Println("Everything-AI-Coding import dry-run completed successfully")
					return
				}
				log.Fatalf("Failed to import Everything-AI-Coding data: %v", err)
			}
			if dryRun {
				log.Println("Everything-AI-Coding import dry-run completed successfully")
			} else {
				log.Println("Everything-AI-Coding import completed successfully")
			}
			return
		case "backfill-capability-content-versioning":
			if err := backfillCapabilityContentVersioning(db); err != nil {
				log.Fatalf("Failed to backfill capability content versioning: %v", err)
			}
			log.Println("Capability content versioning backfill completed successfully")
			return
		default:
			log.Printf("Unknown command: %s", os.Args[1])
			printMigrateHelp()
			os.Exit(1)
		}
	}

	if err := runPreMigrations(db); err != nil {
		log.Fatalf("Failed to run pre-migrations: %v", err)
	}

	err = db.AutoMigrate(
		&models.UserSystemRole{},
		&models.Repository{},
		&models.RepoMember{},
		&models.RepoInvitation{},
		&models.Project{},
		&models.ProjectMember{},
		&models.ProjectInvitation{},
		&models.ProjectRepository{},
		&models.SyncLog{},
		&models.SyncJob{},
		&models.CapabilityRegistry{},
		&models.CapabilityItem{},
		&models.CapabilityVersion{},
		&models.CapabilityVersionAsset{},
		&models.CapabilityAsset{},
		&models.CapabilityArtifact{},
		&models.BehaviorLog{},
		&models.ItemFavorite{},
		&models.SecurityScan{},
		&models.ScanJob{},
		&models.Device{},
		&models.Workspace{},
		&models.WorkspaceDirectory{},
		&models.SystemNotificationChannel{},
		&models.UserNotificationChannel{},
		&models.UserConfig{},
		&models.NotificationLog{},
		&models.ItemCategory{},
		&models.DeviceRelease{},
	)
	if err != nil {
		log.Fatalf("Failed to auto-migrate database: %v", err)
	}

	if err := runGooseMigrations(db); err != nil {
		log.Fatalf("Failed to run goose migrations: %v", err)
	}

	if err := backfillCapabilityContentVersioning(db); err != nil {
		log.Fatalf("Failed to backfill capability content versioning: %v", err)
	}

	log.Println("All migrations completed successfully")
}

func printMigrateHelp() {
	fmt.Println("Usage:")
	fmt.Println("  go run ./cmd/migrate                          Run schema migrations")
	fmt.Println("  go run ./cmd/migrate backfill-capability-content-versioning")
	fmt.Println("                                                Backfill content_md5 and current_revision for capability items")
	fmt.Println("  go run ./cmd/migrate user-subject-ids [--dry-run]")
	fmt.Println("                                                Backfill legacy user IDs to subject_id")
	fmt.Println("  go run ./cmd/migrate import-everything-ai-coding <source-path> [--dry-run]")
	fmt.Println("                                                Import MCP/command/skills/agent data")
	fmt.Println("  go run ./cmd/migrate import-everything-ai-coding --source=<source-path> [--dry-run]")
	fmt.Println("")
	fmt.Println("Examples:")
	fmt.Println("  go run ./cmd/migrate")
	fmt.Println("  go run ./cmd/migrate backfill-capability-content-versioning")
	fmt.Println("  go run ./cmd/migrate user-subject-ids --dry-run")
	fmt.Println("  go run ./cmd/migrate import-everything-ai-coding /Users/linkai/code/.../everything-ai-coding --dry-run")
	fmt.Println("  go run ./cmd/migrate import-everything-ai-coding --source=/Users/linkai/code/.../everything-ai-coding")
}

func backfillCapabilityContentVersioning(db *gorm.DB) error {
	hashSvc := services.NewContentHashService()

	return db.Transaction(func(tx *gorm.DB) error {
		var items []models.CapabilityItem
		if err := tx.Preload("Assets").Find(&items).Error; err != nil {
			return fmt.Errorf("load capability items: %w", err)
		}

		for _, item := range items {
			contentMD5 := strings.TrimSpace(item.ContentMD5)
			if contentMD5 == "" {
				var err error
				contentMD5, err = hashCurrentItemContent(hashSvc, item)
				if err != nil {
					return fmt.Errorf("hash item %s: %w", item.ID, err)
				}
			}

			currentRevision := item.CurrentRevision
			if currentRevision < 1 {
				if err := tx.Model(&models.CapabilityVersion{}).Where("item_id = ?", item.ID).Select("COALESCE(MAX(revision), 1)").Scan(&currentRevision).Error; err != nil {
					return fmt.Errorf("query current revision for item %s: %w", item.ID, err)
				}
				if currentRevision < 1 {
					currentRevision = 1
				}
			}

			if err := tx.Model(&models.CapabilityItem{}).Where("id = ?", item.ID).Updates(map[string]any{
				"content_md5":      contentMD5,
				"current_revision": currentRevision,
			}).Error; err != nil {
				return fmt.Errorf("update item %s: %w", item.ID, err)
			}
		}

		var versions []models.CapabilityVersion
		if err := tx.Find(&versions).Error; err != nil {
			return fmt.Errorf("load capability versions: %w", err)
		}
		for _, version := range versions {
			if strings.TrimSpace(version.ContentMD5) != "" {
				continue
			}

			var item models.CapabilityItem
			if err := tx.Unscoped().Select("id", "item_type").First(&item, "id = ?", version.ItemID).Error; err != nil {
				return fmt.Errorf("load item %s for version %s: %w", version.ItemID, version.ID, err)
			}

			contentMD5, err := hashSvc.HashTextContent(item.ItemType, version.Content)
			if err != nil {
				return fmt.Errorf("hash version %s: %w", version.ID, err)
			}
			if err := tx.Model(&models.CapabilityVersion{}).Where("id = ?", version.ID).Update("content_md5", contentMD5).Error; err != nil {
				return fmt.Errorf("update version %s: %w", version.ID, err)
			}
		}

		return nil
	})
}

func hashCurrentItemContent(hashSvc *services.ContentHashService, item models.CapabilityItem) (string, error) {
	if item.SourceType == "archive" {
		entries := make([]services.ArchiveManifestEntry, 0, len(item.Assets)+1)
		mainHash, err := hashSvc.HashTextContent(item.ItemType, item.Content)
		if err != nil {
			return "", err
		}
		mainPath := item.SourcePath
		if strings.TrimSpace(mainPath) == "" {
			switch item.ItemType {
			case "mcp":
				mainPath = ".mcp.json"
			default:
				mainPath = "SKILL.md"
			}
		}
		entries = append(entries, services.ArchiveManifestEntry{Path: mainPath, ContentHash: mainHash})

		for _, asset := range item.Assets {
			assetHash := strings.TrimSpace(asset.ContentSHA)
			if assetHash == "" {
				if asset.TextContent != nil {
					var err error
					assetHash, err = hashSvc.HashTextContent("text", *asset.TextContent)
					if err != nil {
						return "", err
					}
				} else {
					continue
				}
			}
			entries = append(entries, services.ArchiveManifestEntry{Path: asset.RelPath, ContentHash: assetHash})
		}

		return hashSvc.HashArchiveManifest(entries), nil
	}

	return hashSvc.HashTextContent(item.ItemType, item.Content)
}

type externalCatalogEntry struct {
	ID          string         `json:"id"`
	Name        string         `json:"name"`
	Type        string         `json:"type"`
	Description string         `json:"description"`
	Category    string         `json:"category"`
	Version     string         `json:"version"`
	SourceURL   string         `json:"source_url"`
	Install     map[string]any `json:"install"`
	Tags        []string       `json:"tags"`
	TechStack   []string       `json:"tech_stack"`
	Source      string         `json:"source"`
}

type importPayload struct {
	SourcePath  string
	Slug        string
	ItemType    string
	Name        string
	Description string
	Category    string
	Version     string
	Content     string
	Metadata    datatypes.JSON
}

type importStats struct {
	CreatedByType map[string]int
	UpdatedByType map[string]int
	SkippedByType map[string]int
	FailedByType  map[string]int
}

func newImportStats() *importStats {
	return &importStats{
		CreatedByType: map[string]int{},
		UpdatedByType: map[string]int{},
		SkippedByType: map[string]int{},
		FailedByType:  map[string]int{},
	}
}

func (s *importStats) addCreated(itemType string) { s.CreatedByType[itemType]++ }
func (s *importStats) addUpdated(itemType string) { s.UpdatedByType[itemType]++ }
func (s *importStats) addSkipped(itemType string) { s.SkippedByType[itemType]++ }
func (s *importStats) addFailed(itemType string)  { s.FailedByType[itemType]++ }

func (s *importStats) logSummary(dryRun bool) {
	log.Printf("everything-ai-coding import summary (dry-run=%v)", dryRun)
	log.Printf("  created: %s", formatTypeCounters(s.CreatedByType))
	log.Printf("  updated: %s", formatTypeCounters(s.UpdatedByType))
	log.Printf("  skipped: %s", formatTypeCounters(s.SkippedByType))
	if len(s.FailedByType) > 0 {
		log.Printf("  failed:  %s", formatTypeCounters(s.FailedByType))
	}
}

func formatTypeCounters(m map[string]int) string {
	if len(m) == 0 {
		return "none"
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, fmt.Sprintf("%s=%d", k, m[k]))
	}
	return strings.Join(parts, ", ")
}

func importEverythingAICoding(db *gorm.DB, sourcePath string, dryRun bool) error {
	if strings.TrimSpace(sourcePath) == "" {
		return fmt.Errorf("source path is required")
	}
	absSource, err := filepath.Abs(sourcePath)
	if err != nil {
		return fmt.Errorf("resolve source path: %w", err)
	}
	if stat, err := os.Stat(absSource); err != nil || !stat.IsDir() {
		if err != nil {
			return fmt.Errorf("source path check failed: %w", err)
		}
		return fmt.Errorf("source path is not a directory: %s", absSource)
	}

	payloads, err := buildImportPayloads(absSource)
	if err != nil {
		return err
	}
	if len(payloads) == 0 {
		return fmt.Errorf("no importable entries found in %s", absSource)
	}

	return db.Transaction(func(tx *gorm.DB) error {
		if err := ensurePublicRegistry(tx); err != nil {
			return err
		}

		stats := newImportStats()
		for _, payload := range payloads {
			if err := upsertImportedItem(tx, payload, dryRun, stats); err != nil {
				stats.addFailed(payload.ItemType)
				return fmt.Errorf("import %s (%s): %w", payload.SourcePath, payload.ItemType, err)
			}
		}

		stats.logSummary(dryRun)
		if dryRun {
			return errDryRunRollback
		}
		return nil
	})
}

func ensurePublicRegistry(db *gorm.DB) error {
	var exists int
	if err := db.Raw(`SELECT 1 FROM capability_registries WHERE id = ?`, publicRegistryID).Scan(&exists).Error; err != nil {
		return fmt.Errorf("check public registry: %w", err)
	}
	if exists == 1 {
		return nil
	}

	now := db.NowFunc()
	reg := models.CapabilityRegistry{
		ID:          publicRegistryID,
		Name:        "public",
		Description: "Default public registry",
		SourceType:  "internal",
		RepoID:      publicRepoID,
		OwnerID:     importCreatedBy,
		CreatedAt:   now,
		UpdatedAt:   now,
	}
	if err := db.Create(&reg).Error; err != nil {
		return fmt.Errorf("create public registry: %w", err)
	}
	return nil
}

func buildImportPayloads(sourceRoot string) ([]importPayload, error) {
	payloads := make([]importPayload, 0, 2048)

	mcpEntries, err := readCatalogEntries(filepath.Join(sourceRoot, "catalog", "mcp", "index.json"))
	if err != nil {
		return nil, err
	}
	for _, entry := range mcpEntries {
		payloads = append(payloads, buildCatalogPayload(entry, "mcp"))
	}

	skillEntries, err := readCatalogEntries(filepath.Join(sourceRoot, "catalog", "skills", "index.json"))
	if err != nil {
		return nil, err
	}
	for _, entry := range skillEntries {
		payloads = append(payloads, buildCatalogPayload(entry, "skill"))
	}

	// Import concrete SKILL.md files and let them override catalog summary records
	// with the same (itemType, slug), so skill content is actual markdown.
	skillMarkdownPayloads, err := buildMarkdownPayloads(sourceRoot, "platforms/*/skills/**/SKILL.md", "skill")
	if err != nil {
		return nil, err
	}
	payloads = append(payloads, skillMarkdownPayloads...)

	commandPayloads, err := buildMarkdownPayloads(sourceRoot, "platforms/*/commands/**/*.md", "command")
	if err != nil {
		return nil, err
	}
	payloads = append(payloads, commandPayloads...)

	agentPayloads, err := buildMarkdownPayloads(sourceRoot, "platforms/*/agents/**/*.md", "subagent")
	if err != nil {
		return nil, err
	}
	payloads = append(payloads, agentPayloads...)

	return dedupePayloads(payloads), nil
}

func readCatalogEntries(path string) ([]externalCatalogEntry, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read catalog file %s: %w", path, err)
	}
	var entries []externalCatalogEntry
	if err := json.Unmarshal(b, &entries); err != nil {
		return nil, fmt.Errorf("parse catalog file %s: %w", path, err)
	}
	return entries, nil
}

func buildCatalogPayload(entry externalCatalogEntry, fallbackType string) importPayload {
	itemType := strings.TrimSpace(entry.Type)
	if itemType == "" {
		itemType = fallbackType
	}

	metadata := map[string]any{
		"externalId": entry.ID,
		"sourceUrl":  entry.SourceURL,
		"source":     entry.Source,
		"install":    entry.Install,
		"tags":       entry.Tags,
		"techStack":  entry.TechStack,
	}
	metadataJSON := mustJSON(metadata)

	contentBytes, _ := json.MarshalIndent(map[string]any{
		"id":          entry.ID,
		"name":        entry.Name,
		"type":        itemType,
		"description": entry.Description,
		"source_url":  entry.SourceURL,
		"category":    entry.Category,
		"install":     entry.Install,
	}, "", "  ")

	version := strings.TrimSpace(entry.Version)
	if version == "" {
		version = "1.0.0"
	}

	name := strings.TrimSpace(entry.Name)
	if name == "" {
		name = entry.ID
	}

	slug := normalizeSlug(entry.ID)
	if slug == "" {
		slug = normalizeSlug(entry.Name)
	}

	return importPayload{
		SourcePath:  filepath.ToSlash(filepath.Join("catalog", itemType, "index.json#", entry.ID)),
		Slug:        slug,
		ItemType:    itemType,
		Name:        name,
		Description: entry.Description,
		Category:    entry.Category,
		Version:     version,
		Content:     string(contentBytes),
		Metadata:    metadataJSON,
	}
}

func buildMarkdownPayloads(sourceRoot, pattern, forceItemType string) ([]importPayload, error) {
	parser := &services.ParserService{}
	files, err := filepath.Glob(filepath.Join(sourceRoot, filepath.FromSlash(pattern)))
	if err != nil {
		return nil, fmt.Errorf("glob %s: %w", pattern, err)
	}
	payloads := make([]importPayload, 0, len(files))
	for _, absPath := range files {
		relPath, err := filepath.Rel(sourceRoot, absPath)
		if err != nil {
			return nil, fmt.Errorf("build relative path for %s: %w", absPath, err)
		}
		relPath = filepath.ToSlash(relPath)

		b, err := os.ReadFile(absPath)
		if err != nil {
			return nil, fmt.Errorf("read markdown %s: %w", absPath, err)
		}

		parsed, err := parser.ParseSKILLMD(b, relPath)
		if err != nil {
			return nil, fmt.Errorf("parse markdown %s: %w", relPath, err)
		}

		itemType := forceItemType
		if itemType == "" {
			itemType = parsed.ItemType
		}

		name := strings.TrimSpace(parsed.Name)
		if name == "" {
			name = inferName(relPath)
		}

		description := strings.TrimSpace(parsed.Description)
		if description == "" {
			description = fmt.Sprintf("Imported from %s", relPath)
		}

		version := strings.TrimSpace(parsed.Version)
		if version == "" {
			version = "1.0.0"
		}

		metadata := map[string]any(parsed.Metadata)
		metadata["sourcePath"] = relPath

		payloads = append(payloads, importPayload{
			SourcePath:  relPath,
			Slug:        normalizeSlug(parsed.Slug),
			ItemType:    itemType,
			Name:        name,
			Description: description,
			Category:    strings.TrimSpace(parsed.Category),
			Version:     version,
			Content:     string(b),
			Metadata:    mustJSON(metadata),
		})
	}
	return payloads, nil
}

func dedupePayloads(payloads []importPayload) []importPayload {
	byKey := make(map[string]importPayload, len(payloads))
	order := make([]string, 0, len(payloads))
	for _, p := range payloads {
		if p.ItemType == "agent" {
			p.ItemType = "subagent"
		}
		if p.ItemType == "skills" {
			p.ItemType = "skill"
		}
		if p.ItemType == "commands" {
			p.ItemType = "command"
		}
		if p.Slug == "" {
			p.Slug = normalizeSlug(p.Name)
		}
		if p.Slug == "" {
			p.Slug = uuid.New().String()
		}
		key := p.ItemType + ":" + p.Slug
		if _, exists := byKey[key]; !exists {
			order = append(order, key)
		}
		byKey[key] = p
	}
	out := make([]importPayload, 0, len(order))
	for _, key := range order {
		out = append(out, byKey[key])
	}
	return out
}

func upsertImportedItem(db *gorm.DB, payload importPayload, dryRun bool, stats *importStats) error {
	if payload.ItemType == "" {
		return fmt.Errorf("missing item type")
	}
	if payload.Slug == "" {
		return fmt.Errorf("missing slug")
	}

	hashSvc := services.NewContentHashService()
	contentMD5, err := hashSvc.HashTextContent(payload.ItemType, payload.Content)
	if err != nil {
		return fmt.Errorf("hash imported content: %w", err)
	}

	var existing models.CapabilityItem
	err = db.Where("repo_id = ? AND item_type = ? AND slug = ?", publicRepoID, payload.ItemType, payload.Slug).First(&existing).Error
	if err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
		return fmt.Errorf("query existing item: %w", err)
	}

	if errors.Is(err, gorm.ErrRecordNotFound) {
		stats.addCreated(payload.ItemType)
		if dryRun {
			return nil
		}
		item := models.CapabilityItem{
			ID:              uuid.New().String(),
			RegistryID:      publicRegistryID,
			RepoID:          publicRepoID,
			Slug:            payload.Slug,
			ItemType:        payload.ItemType,
			Name:            payload.Name,
			Description:     payload.Description,
			Category:        payload.Category,
			Version:         payload.Version,
			Content:         payload.Content,
			ContentMD5:      contentMD5,
			CurrentRevision: 1,
			Metadata:        payload.Metadata,
			SourcePath:      payload.SourcePath,
			SourceSHA:       sourceSHA(payload.Content),
			SourceType:      "direct",
			Status:          "active",
			CreatedBy:       importCreatedBy,
			UpdatedBy:       importCreatedBy,
		}
		if err := db.Omit("Embedding").Create(&item).Error; err != nil {
			return fmt.Errorf("create capability item: %w", err)
		}
		version := models.CapabilityVersion{
			ID:         uuid.New().String(),
			ItemID:     item.ID,
			Revision:   1,
			Content:    item.Content,
			ContentMD5: contentMD5,
			Metadata:   item.Metadata,
			CommitMsg:  "Import from everything-ai-coding",
			CreatedBy:  importCreatedBy,
		}
		if err := db.Create(&version).Error; err != nil {
			return fmt.Errorf("create capability version: %w", err)
		}
		return nil
	}

	if existing.Content == payload.Content && string(existing.Metadata) == string(payload.Metadata) && existing.Name == payload.Name && existing.Description == payload.Description && existing.Category == payload.Category {
		stats.addSkipped(payload.ItemType)
		return nil
	}

	stats.addUpdated(payload.ItemType)
	if dryRun {
		return nil
	}

	updates := map[string]any{
		"name":        payload.Name,
		"description": payload.Description,
		"category":    payload.Category,
		"version":     payload.Version,
		"content":     payload.Content,
		"content_md5": contentMD5,
		"metadata":    payload.Metadata,
		"source_path": payload.SourcePath,
		"source_sha":  sourceSHA(payload.Content),
		"updated_by":  importCreatedBy,
	}
	if contentMD5 != existing.ContentMD5 {
		updates["current_revision"] = existing.CurrentRevision + 1
	}
	if err := db.Model(&models.CapabilityItem{}).Where("id = ?", existing.ID).Updates(updates).Error; err != nil {
		return fmt.Errorf("update capability item: %w", err)
	}

	if contentMD5 == existing.ContentMD5 {
		return nil
	}

	nextRevision := existing.CurrentRevision + 1
	version := models.CapabilityVersion{
		ID:         uuid.New().String(),
		ItemID:     existing.ID,
		Revision:   nextRevision,
		Content:    payload.Content,
		ContentMD5: contentMD5,
		Metadata:   payload.Metadata,
		CommitMsg:  "Sync from everything-ai-coding",
		CreatedBy:  importCreatedBy,
	}
	if err := db.Create(&version).Error; err != nil {
		return fmt.Errorf("create capability version: %w", err)
	}

	return nil
}

func normalizeSlug(v string) string {
	v = strings.TrimSpace(strings.ToLower(v))
	v = strings.ReplaceAll(v, "_", "-")
	v = strings.ReplaceAll(v, " ", "-")
	var b strings.Builder
	b.Grow(len(v))
	lastDash := false
	for _, r := range v {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
			lastDash = false
			continue
		}
		if r == '-' || r == '/' || r == '.' {
			if !lastDash {
				b.WriteByte('-')
				lastDash = true
			}
		}
	}
	return strings.Trim(b.String(), "-")
}

func inferName(path string) string {
	base := filepath.Base(path)
	ext := filepath.Ext(base)
	name := strings.TrimSuffix(base, ext)
	name = strings.ReplaceAll(name, "-", " ")
	name = strings.ReplaceAll(name, "_", " ")
	name = strings.TrimSpace(name)
	if name == "" {
		return "Imported Item"
	}
	return strings.ToUpper(name[:1]) + name[1:]
}

func mustJSON(v any) datatypes.JSON {
	b, err := json.Marshal(v)
	if err != nil {
		return datatypes.JSON([]byte("{}"))
	}
	if len(b) == 0 {
		return datatypes.JSON([]byte("{}"))
	}
	return datatypes.JSON(b)
}

func sourceSHA(content string) string {
	sum := sha256.Sum256([]byte(content))
	return hex.EncodeToString(sum[:])
}

func runGooseMigrations(db *gorm.DB) error {
	sqlDB, err := db.DB()
	if err != nil {
		return fmt.Errorf("failed to get underlying sql.DB: %w", err)
	}

	goose.SetBaseFS(migrations.FS)

	if err := goose.SetDialect("postgres"); err != nil {
		return fmt.Errorf("failed to set goose dialect: %w", err)
	}

	if err := goose.Up(sqlDB, ".", goose.WithAllowMissing()); err != nil {
		return fmt.Errorf("goose migration failed: %w", err)
	}

	log.Println("Goose migrations completed successfully")
	return nil
}

func runPreMigrations(db *gorm.DB) error {
	bootstrapStmts := []string{
		`CREATE TABLE IF NOT EXISTS sync_logs (
			id uuid DEFAULT gen_random_uuid() PRIMARY KEY,
			registry_id uuid NOT NULL,
			trigger_type text NOT NULL,
			trigger_user text,
			status text NOT NULL DEFAULT 'running',
			commit_sha text,
			previous_sha text,
			total_items bigint DEFAULT 0,
			added_items bigint DEFAULT 0,
			updated_items bigint DEFAULT 0,
			deleted_items bigint DEFAULT 0,
			skipped_items bigint DEFAULT 0,
			failed_items bigint DEFAULT 0,
			error_message text,
			duration_ms bigint,
			started_at timestamptz NOT NULL DEFAULT now(),
			finished_at timestamptz,
			created_at timestamptz
		)`,
		`CREATE TABLE IF NOT EXISTS capability_registries (
			id uuid DEFAULT gen_random_uuid() PRIMARY KEY,
			name text NOT NULL,
			description text,
			source_type text NOT NULL DEFAULT 'internal',
			external_url text,
			external_branch text DEFAULT 'main',
			sync_enabled boolean DEFAULT false,
			sync_interval bigint DEFAULT 3600,
			last_synced_at timestamptz,
			last_sync_sha text,
			sync_status text DEFAULT 'idle',
			sync_config JSONB DEFAULT '{}',
			last_sync_log_id uuid,
			visibility text DEFAULT 'repo',
			repo_id text,
			owner_id text NOT NULL,
			created_at timestamptz,
			updated_at timestamptz
		)`,
	}
	for _, stmt := range bootstrapStmts {
		if err := db.Exec(stmt).Error; err != nil {
			return fmt.Errorf("bootstrap failed: %w", err)
		}
	}

	preMigrations := []struct {
		check string
		stmts []string
	}{
		{
			check: `SELECT 1 FROM information_schema.columns WHERE table_name='capability_versions' AND column_name='version'`,
			stmts: []string{
				`ALTER TABLE capability_versions ADD COLUMN IF NOT EXISTS revision bigint`,
				`UPDATE capability_versions SET revision = version WHERE revision IS NULL`,
				`ALTER TABLE capability_versions ALTER COLUMN revision SET NOT NULL`,
				`ALTER TABLE capability_versions ALTER COLUMN revision SET DEFAULT 1`,
			},
		},
		{
			check: `SELECT 1 FROM information_schema.columns WHERE table_name='capability_items' AND column_name='visibility'`,
			stmts: []string{
				`ALTER TABLE capability_items DROP COLUMN IF EXISTS visibility`,
			},
		},
		// Drop visibility column from capability_registries — visibility is now
		// derived from the parent repository's visibility field.
		{
			check: `SELECT 1 FROM information_schema.columns WHERE table_name='capability_registries' AND column_name='visibility'`,
			stmts: []string{
				`ALTER TABLE capability_registries DROP COLUMN IF EXISTS visibility`,
			},
		},
		{
			check: `SELECT 1 FROM information_schema.columns WHERE table_name='security_scans' AND column_name='revision_id'`,
			stmts: []string{
				`ALTER TABLE security_scans DROP COLUMN IF EXISTS revision_id`,
			},
		},
		{
			check: `SELECT 1 FROM pg_indexes WHERE indexname = 'idx_item_slug'`,
			stmts: []string{
				`DROP INDEX IF EXISTS idx_item_slug`,
			},
		},
		{
			check: `SELECT 1 FROM pg_indexes WHERE indexname = 'idx_item_slug_global'`,
			stmts: []string{
				`DROP INDEX IF EXISTS idx_item_slug_global`,
			},
		},
		{
			check: `SELECT 1 FROM information_schema.columns WHERE table_name='capability_items' AND column_name='id' AND NOT EXISTS (SELECT 1 FROM information_schema.columns WHERE table_name='capability_items' AND column_name='repo_id')`,
			stmts: []string{
				`ALTER TABLE capability_items ADD COLUMN repo_id text NOT NULL DEFAULT 'public'`,
			},
		},
	}

	for _, m := range preMigrations {
		var exists int
		if err := db.Raw(m.check).Scan(&exists).Error; err != nil {
			return fmt.Errorf("pre-migration check failed (%s): %w", m.check, err)
		}
		if exists != 1 {
			continue
		}
		for _, stmt := range m.stmts {
			if err := db.Exec(stmt).Error; err != nil {
				return fmt.Errorf("pre-migration failed (%s): %w", stmt, err)
			}
		}
	}

	if err := backfillCapabilityItemRepoIDs(db); err != nil {
		return fmt.Errorf("failed to backfill capability_items.repo_id before migrations: %w", err)
	}

	if err := deduplicateSlugs(db); err != nil {
		return fmt.Errorf("failed to deduplicate slugs before composite unique index: %w", err)
	}
	return nil
}

func backfillCapabilityItemRepoIDs(db *gorm.DB) error {
	var tableExists int
	if err := db.Raw(`SELECT 1 FROM information_schema.tables WHERE table_schema = CURRENT_SCHEMA() AND table_name = 'capability_items'`).Scan(&tableExists).Error; err != nil {
		return fmt.Errorf("checking capability_items existence: %w", err)
	}
	if tableExists != 1 {
		return nil
	}

	var needsBackfill int
	if err := db.Raw(`SELECT 1 FROM capability_items WHERE repo_id = 'public' LIMIT 1`).Scan(&needsBackfill).Error; err != nil {
		return fmt.Errorf("checking capability_items repo_id backfill: %w", err)
	}
	if needsBackfill != 1 {
		return nil
	}

	if err := db.Exec(`UPDATE capability_items SET repo_id = COALESCE(
		(SELECT COALESCE(NULLIF(cr.repo_id,''), 'public')
		 FROM capability_registries cr
		 WHERE cr.id = capability_items.registry_id),
		'public'
	) WHERE repo_id = 'public'`).Error; err != nil {
		return fmt.Errorf("backfilling capability_items.repo_id: %w", err)
	}

	return nil
}

func backfillLegacyUserReferences(db *gorm.DB, dryRun bool) error {
	type userMapping struct {
		SubjectID          string
		CasdoorUniversalID *string
		CasdoorID          *string
		CasdoorSub         *string
	}

	var users []userMapping
	if err := db.Table("users").Select("subject_id, casdoor_universal_id, casdoor_id, casdoor_sub").Find(&users).Error; err != nil {
		return fmt.Errorf("load user mappings: %w", err)
	}

	mapping := make(map[string]string, len(users)*4)
	for _, user := range users {
		if user.SubjectID == "" {
			continue
		}
		mapping[user.SubjectID] = user.SubjectID
		if user.CasdoorUniversalID != nil && *user.CasdoorUniversalID != "" {
			mapping[*user.CasdoorUniversalID] = user.SubjectID
		}
		if user.CasdoorID != nil && *user.CasdoorID != "" {
			mapping[*user.CasdoorID] = user.SubjectID
		}
		if user.CasdoorSub != nil && *user.CasdoorSub != "" {
			mapping[*user.CasdoorSub] = user.SubjectID
		}
	}

	updates := []struct {
		table  string
		column string
	}{
		{"system_notification_channels", "created_by"},
		{"user_notification_channels", "user_id"},
		{"user_configs", "user_id"},
		{"notification_logs", "user_id"},
		{"devices", "user_id"},
		{"repositories", "owner_id"},
		{"repo_members", "user_id"},
		{"repo_invitations", "inviter_id"},
		{"repo_invitations", "invitee_id"},
		{"projects", "creator_id"},
		{"project_members", "user_id"},
		{"project_invitations", "inviter_id"},
		{"project_invitations", "invitee_id"},
		{"project_repositories", "bound_by_user_id"},
		{"user_system_roles", "user_id"},
		{"user_system_roles", "granted_by"},
		{"capability_registries", "owner_id"},
		{"sync_jobs", "trigger_user"},
		{"sync_logs", "trigger_user"},
		{"capability_items", "created_by"},
		{"capability_items", "updated_by"},
		{"item_categories", "created_by"},
		{"item_favorites", "user_id"},
		{"capability_versions", "created_by"},
		{"capability_artifacts", "uploaded_by"},
		{"scan_jobs", "trigger_user"},
		{"workspaces", "user_id"},
		{"behavior_logs", "user_id"},
	}

	type tableStat struct {
		UpdatedRows int
		Unresolved  map[string]int
	}
	stats := make(map[string]*tableStat, len(updates))
	getStat := func(table, column string) *tableStat {
		key := table + "." + column
		if stats[key] == nil {
			stats[key] = &tableStat{Unresolved: map[string]int{}}
		}
		return stats[key]
	}

	return db.Transaction(func(tx *gorm.DB) error {
		for _, target := range updates {
			rows, err := tx.Table(target.table).Select(target.column).Where(target.column + " IS NOT NULL AND " + target.column + " <> ''").Rows()
			if err != nil {
				return fmt.Errorf("scan %s.%s: %w", target.table, target.column, err)
			}
			values := map[string]struct{}{}
			for rows.Next() {
				var value string
				if err := rows.Scan(&value); err != nil {
					rows.Close()
					return fmt.Errorf("read %s.%s: %w", target.table, target.column, err)
				}
				if value != "" {
					values[value] = struct{}{}
				}
			}
			rows.Close()

			for legacyValue := range values {
				subjectID, ok := mapping[legacyValue]
				if !ok {
					getStat(target.table, target.column).Unresolved[legacyValue]++
					continue
				}
				if subjectID == legacyValue {
					continue
				}

				var affected int64
				if dryRun {
					if err := tx.Table(target.table).Where(target.column+" = ?", legacyValue).Count(&affected).Error; err != nil {
						return fmt.Errorf("count %s.%s for %s: %w", target.table, target.column, legacyValue, err)
					}
				} else {
					result := tx.Exec(
						fmt.Sprintf("UPDATE %s SET %s = ? WHERE %s = ?", target.table, target.column, target.column),
						subjectID,
						legacyValue,
					)
					if result.Error != nil {
						return fmt.Errorf("update %s.%s from %s to %s: %w", target.table, target.column, legacyValue, subjectID, result.Error)
					}
					affected = result.RowsAffected
				}
				getStat(target.table, target.column).UpdatedRows += int(affected)
			}
		}

		log.Printf("user-subject-ids summary (dry-run=%v)", dryRun)
		for _, target := range updates {
			key := target.table + "." + target.column
			stat := stats[key]
			if stat == nil {
				continue
			}
			if stat.UpdatedRows > 0 {
				log.Printf("  [%s] updated rows: %d", key, stat.UpdatedRows)
			}
			if len(stat.Unresolved) > 0 {
				log.Printf("  [%s] unresolved identifiers:", key)
				for legacyValue, seenCount := range stat.Unresolved {
					log.Printf("    - %s (seen %d times)", legacyValue, seenCount)
				}
			}
		}

		if dryRun {
			return errDryRunRollback
		}
		return nil
	})
}

func deduplicateSlugs(db *gorm.DB) error {
	var tableExists int
	if err := db.Raw(`SELECT 1 WHERE to_regclass('public.capability_items') IS NOT NULL`).Scan(&tableExists).Error; err != nil {
		return fmt.Errorf("checking capability_items existence: %w", err)
	}
	if tableExists != 1 {
		return nil
	}

	var idxExists int
	db.Raw(`SELECT 1 FROM pg_indexes WHERE indexname = 'idx_item_repo_type_slug'`).Scan(&idxExists)
	if idxExists == 1 {
		return nil
	}

	type row struct {
		ID       string
		RepoID   string `gorm:"column:repo_id"`
		ItemType string `gorm:"column:item_type"`
		Slug     string
	}

	var rows []row
	err := db.Raw(`
		SELECT id, repo_id, item_type, slug
		FROM capability_items
		WHERE (repo_id, item_type, slug) IN (
			SELECT repo_id, item_type, slug
			FROM capability_items
			GROUP BY repo_id, item_type, slug HAVING COUNT(*) > 1
		)
		ORDER BY repo_id, item_type, slug, created_at ASC, id ASC`,
	).Scan(&rows).Error
	if err != nil {
		return fmt.Errorf("querying duplicate slugs: %w", err)
	}
	if len(rows) == 0 {
		return nil
	}

	type groupKey struct{ RepoID, ItemType, Slug string }
	type group struct {
		ids []string
	}
	groups := make(map[groupKey]*group)
	var keys []groupKey
	for _, r := range rows {
		k := groupKey{r.RepoID, r.ItemType, r.Slug}
		g, ok := groups[k]
		if !ok {
			groups[k] = &group{ids: []string{r.ID}}
			keys = append(keys, k)
		} else {
			g.ids = append(g.ids, r.ID)
		}
	}

	return db.Transaction(func(tx *gorm.DB) error {
		for _, k := range keys {
			g := groups[k]
			for i := 1; i < len(g.ids); i++ {
				candidate := ""
				for n := i + 1; ; n++ {
					candidate = fmt.Sprintf("%s-%d", k.Slug, n)
					var count int64
					if err := tx.Raw(
						`SELECT COUNT(*) FROM capability_items WHERE repo_id = ? AND item_type = ? AND slug = ?`,
						k.RepoID, k.ItemType, candidate,
					).Scan(&count).Error; err != nil {
						return fmt.Errorf("checking slug %q: %w", candidate, err)
					}
					if count == 0 {
						break
					}
				}
				log.Printf("[deduplicateSlugs] renaming item %s slug %q -> %q (repo=%s, type=%s)",
					g.ids[i], k.Slug, candidate, k.RepoID, k.ItemType)
				if err := tx.Exec(
					`UPDATE capability_items SET slug = ? WHERE id = ?`, candidate, g.ids[i],
				).Error; err != nil {
					return fmt.Errorf("renaming slug for item %s: %w", g.ids[i], err)
				}
			}
		}
		return nil
	})
}
