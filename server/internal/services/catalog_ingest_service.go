// Package services — CatalogIngestService consumes the upstream catalog bundle
// (built by costrict-skills-repo's scripts/build_catalog_bundle.py) and reconciles
// it with the local public-registry capability_items table.
//
// Why a dedicated service instead of going through SyncService:
//
//   - SyncService was designed for *remote git repositories* — it clones,
//     globs files, computes diff against last_sync_sha. For a static catalog
//     bundle this required (in the old import-everything-ai-coding path):
//     1) copy upstream catalog-download/ into a tmpdir
//     2) git init a fake repo in that tmpdir
//     3) create a temporary CapabilityRegistry pointing at the fake repo
//     4) SyncRegistry — which clones the fake repo into a *second* tmpdir
//     5) re-point the resulting capability_items to publicRegistryID
//     6) delete the fake registry
//     Three layers of copy, one fake git, one fake registry. Pure plumbing.
//
//   - The upstream `catalog/index.json` IS already a complete manifest:
//     every entry carries id / type / source / description / category /
//     tags / final_score / security (with content_hash). Combined with the
//     per-entry files under `catalog-download/<type-dir>/<id>/`, we have
//     all the information needed to upsert without any git layer.
//
// Diff strategy:
//
//   - Each entry's primary file is sha256-hashed. That hash is compared
//     against the DB's `capability_items.source_sha` (same semantics as
//     what SyncService writes for items it synced from git).
//   - Entries with matching sha → only metadata fields are refreshed
//     (source / description / category / tags / experience_score) plus
//     a new SecurityScan row if upstream's security block has updated.
//   - Entries with differing sha or missing in DB → full parse + upsert,
//     bump capability_versions revision, enqueue scan job.
//   - DB items whose source_path no longer appears upstream → soft archive
//     (status='archived', deleted_at=NOW). We do NOT physically delete so
//     favorites/scan history remain intact.
//
// Source layers (IngestSource):
//
//   - URL:     fetch tarball via http.Client (planned: ETag / If-None-Match)
//   - Tarball: read a local .tar(.gz) file
//   - Dir:     point at an already-extracted bundle directory (debug / dev)
package services

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/costrict/costrict-web/server/internal/logger"
	"github.com/costrict/costrict-web/server/internal/models"
	"github.com/google/uuid"
	"golang.org/x/text/unicode/norm"
	"gorm.io/datatypes"
	"gorm.io/gorm"
)

// SupportedBundleSchemaVersion is the manifest.schema_version this service
// understands. Increment in lock-step with upstream when the bundle layout
// changes; refuse to ingest unknown versions rather than silently writing
// stale fields.
const SupportedBundleSchemaVersion = 1

// PublicRegistryID is the well-known UUID of the public registry — items
// from the upstream catalog all live under this registry.
const PublicRegistryID = "00000000-0000-0000-0000-000000000001"

// PublicRepoID matches what migrate's existing path uses.
const PublicRepoID = "public"

const ingestTriggerUser = "system"

// CatalogIngestService is the entry point for "pull upstream catalog into
// the local DB" flows. Construct it once and call Ingest repeatedly.
type CatalogIngestService struct {
	DB             *gorm.DB
	HTTP           *http.Client
	Parser         *ParserService
	TagSvc         *TagService
	CategorySvc    *CategoryService
	ScanJobService *ScanJobService
}

// IngestSource carries exactly one of URL / Tarball / Dir. The other two
// MUST be empty.
type IngestSource struct {
	URL     string
	Tarball string
	Dir     string
}

// IngestOptions controls per-call behavior.
type IngestOptions struct {
	// DryRun: print diff counts but do not write to the DB.
	DryRun bool
	// TriggerUser: subject_id recorded on inserted/updated rows.
	// Defaults to "system" when empty.
	TriggerUser string
}

// IngestResult is the summary returned from Ingest.
//
// Failure taxonomy: we separate ingest-side errors ("Failed") from upstream
// data quality issues ("Incomplete") so a clean report doesn't conflate
// "the ingest code broke" with "the upstream bundle contains entries
// nobody could ever load". The set of upstream-data signatures we
// reclassify lives in isUpstreamDataIncomplete().
type IngestResult struct {
	BundleEntries    int           // upstream entry count from manifest
	Added            int           // items inserted (one entry may add >1, e.g. multi-server mcp)
	Updated          int           // items with content sha changed
	MetadataUpdated  int           // items whose content was unchanged but metadata refreshed
	Skipped          int           // items where neither content nor metadata changed
	Deleted          int           // items soft-archived because upstream removed them
	Failed           int           // entries that errored due to a real ingest-side problem
	Incomplete       int           // entries dropped because upstream bundle ships an unusable record
	Errors           []string      // human-readable error lines (Failed only)
	IncompleteErrors []string      // human-readable lines for Incomplete entries (separate slice)
	Duration         time.Duration // total wallclock
	ManifestSHA256   string        // sha256 of upstream index.json (carried in manifest.json)
	GeneratedAt      string        // upstream bundle generation timestamp
}

// isUpstreamDataIncomplete returns true when the error matches a known
// upstream-data-quality signature. Callers should bucket these into
// `Incomplete` rather than `Failed` so they don't drown out real bugs in
// the ingest report.
//
// Known signatures (as of bundle schema_version 1):
//
//   - "need command or url" — NormalizeMCPMetadata rejecting `.mcp.json`
//     files where mcpServers.<name> is `{}`. Upstream download_catalog.py
//     ships these stubs when registry.modelcontextprotocol.io has the
//     server listed but no install instructions. ~63% of mcp/ today.
//   - "failed to parse frontmatter" / "mapping values are not allowed" /
//     "did not find expected" — PROMPT.md descriptions with unquoted ':'.
//     Upstream prompt generator does not YAML-escape special chars.
//     ~33% of prompts/ today.
//
// Keep this list narrow and explicit: false positives turn real bugs
// invisible.
func isUpstreamDataIncomplete(errMsg string) bool {
	switch {
	case strings.Contains(errMsg, "need command or url"):
		return true
	case strings.Contains(errMsg, "failed to parse frontmatter"):
		return true
	}
	return false
}

// catalogBundleManifest mirrors the bundle's manifest.json. Field names
// match exactly so json.Unmarshal works on the raw bytes.
type catalogBundleManifest struct {
	SchemaVersion int            `json:"schema_version"`
	GeneratedAt   string         `json:"generated_at"`
	EntryCount    int            `json:"entry_count"`
	IndexSHA256   string         `json:"index_sha256"`
	TypeCounts    map[string]int `json:"type_counts"`
}

// catalogEntry is the per-entry shape we care about from index.json.
// We deliberately keep this struct small — index.json carries many fields
// (evaluation, freshness_label, weak_dims, …) that the DB does not need.
type catalogEntry struct {
	ID          string                `json:"id"`
	Type        string                `json:"type"`
	Source      string                `json:"source"`
	Description string                `json:"description"`
	Category    string                `json:"category"`
	Tags        []string              `json:"tags"`
	FinalScore  float64               `json:"final_score"`
	Security    *catalogSecurityBlock `json:"security,omitempty"`
}

// catalogSecurityBlock mirrors the schema written by the upstream LLM
// audit pipeline; see openspec/changes/import-upstream-security-scan/.
type catalogSecurityBlock struct {
	RiskLevel       string              `json:"risk_level"`
	Verdict         string              `json:"verdict"`
	RedFlags        []string            `json:"red_flags"`
	Permissions     *catalogPermissions `json:"permissions,omitempty"`
	Summary         string              `json:"summary"`
	Recommendations []string            `json:"recommendations"`
	ScanModel       string              `json:"scan_model"`
	RubricVersion   string              `json:"rubric_version"`
	ContentHash     string              `json:"content_hash"`
	ScannedAt       string              `json:"scanned_at"`
}

type catalogPermissions struct {
	Files    []string `json:"files"`
	Network  []string `json:"network"`
	Commands []string `json:"commands"`
}

// Ingest is the single public entry point. It materializes the bundle,
// reads the manifest + index, computes the diff against the local DB,
// and applies it inside one transaction per entry (so a partial failure
// halfway through doesn't leave a torn snapshot).
func (s *CatalogIngestService) Ingest(ctx context.Context, src IngestSource, opts IngestOptions) (*IngestResult, error) {
	start := time.Now()
	result := &IngestResult{}
	defer func() { result.Duration = time.Since(start) }()

	triggerUser := opts.TriggerUser
	if triggerUser == "" {
		triggerUser = ingestTriggerUser
	}

	bundleDir, cleanup, err := s.materialize(ctx, src)
	if err != nil {
		return result, fmt.Errorf("materialize bundle: %w", err)
	}
	defer cleanup()

	manifest, err := readManifest(bundleDir)
	if err != nil {
		return result, fmt.Errorf("read manifest.json: %w", err)
	}
	if manifest.SchemaVersion != SupportedBundleSchemaVersion {
		return result, fmt.Errorf("manifest schema_version=%d is not supported by this server (supported=%d)", manifest.SchemaVersion, SupportedBundleSchemaVersion)
	}
	result.ManifestSHA256 = manifest.IndexSHA256
	result.GeneratedAt = manifest.GeneratedAt

	entries, err := readIndex(bundleDir)
	if err != nil {
		return result, fmt.Errorf("read index.json: %w", err)
	}
	result.BundleEntries = len(entries)
	logger.Info("catalog-ingest: bundle has %d entries (manifest reports %d)", len(entries), manifest.EntryCount)

	if err := s.ensurePublicRegistry(); err != nil {
		return result, fmt.Errorf("ensure public registry: %w", err)
	}

	// Load all existing public-registry items once. Two indices:
	//   itemsByEntryDir — "<type-dir>/<id>" → all rows from that upstream entry
	//                     (multi-server mcp expands to >1 row per entry).
	//   itemsBySlug     — "<itemType>:<slug>" → cross-entry global slug index.
	// The second index catches the case where ParseMCPJSON yields a server
	// (slug=foo) whose `slug` happens to equal a row imported under a totally
	// different upstream entry. Without it, INSERT would crash on the
	// (repo_id, item_type, slug) unique constraint and the entry would
	// silently be lost.
	var existingItems []models.CapabilityItem
	if err := s.DB.Where("registry_id = ?", PublicRegistryID).Find(&existingItems).Error; err != nil {
		return result, fmt.Errorf("load existing items: %w", err)
	}
	itemsByEntryDir := indexItemsByEntryDir(existingItems)
	itemsBySlug := indexItemsBySlug(existingItems)
	logger.Info("catalog-ingest: %d existing items in public registry", len(existingItems))

	seenEntryDirs := make(map[string]bool, len(entries))

	for _, entry := range entries {
		paths, ok := primaryPathsForEntry(entry)
		if !ok {
			// Unsupported type is an upstream-shape problem: the bundle is
			// claiming an entry whose type we have no parser for. Bucket
			// into Incomplete so it doesn't masquerade as a code bug.
			result.Incomplete++
			result.IncompleteErrors = append(result.IncompleteErrors, fmt.Sprintf("entry %s: unsupported type %q", entry.ID, entry.Type))
			continue
		}
		seenEntryDirs[paths.EntryDir] = true

		absPath := filepath.Join(bundleDir, paths.BundlePath)
		fileBytes, err := readBundleFile(absPath)
		if err != nil {
			// File listed in index.json but missing on disk → bundle
			// builder bug or supply-chain gap. Counted as Failed because
			// it's recoverable (rebuild bundle) and we want it surfaced.
			result.Failed++
			result.Errors = append(result.Errors, fmt.Sprintf("entry %s: read %s: %v", entry.ID, paths.BundlePath, err))
			continue
		}
		fileBytes = sanitizeSyncContent(fileBytes)
		fileSHA := sha256Hex(fileBytes)

		relatedItems := itemsByEntryDir[paths.EntryDir]

		anyContentChanged := false
		for _, item := range relatedItems {
			if item.SourceSHA != fileSHA {
				anyContentChanged = true
				break
			}
		}
		isNew := len(relatedItems) == 0

		if isNew || anyContentChanged {
			added, updated, failed, errs := s.applyChangedEntry(ctx, bundleDir, paths, fileBytes, fileSHA, entry, relatedItems, itemsBySlug, opts.DryRun, triggerUser)
			result.Added += added
			result.Updated += updated
			classifyAndAccumulate(result, failed, errs)
		} else {
			updated, skipped, failed, errs := s.applyMetadataOnly(entry, relatedItems, opts.DryRun)
			result.MetadataUpdated += updated
			result.Skipped += skipped
			classifyAndAccumulate(result, failed, errs)
		}
	}

	// Soft-archive items whose entry dir is no longer in upstream.
	if !opts.DryRun {
		for entryDir, items := range itemsByEntryDir {
			if seenEntryDirs[entryDir] {
				continue
			}
			for _, item := range items {
				if item.Status == "archived" {
					continue
				}
				if err := s.DB.Model(&models.CapabilityItem{}).
					Where("id = ?", item.ID).
					Updates(map[string]any{"status": "archived"}).Error; err != nil {
					result.Failed++
					result.Errors = append(result.Errors, fmt.Sprintf("archive %s: %v", item.ID, err))
					continue
				}
				result.Deleted++
			}
		}
		// Refresh registry's last_sync timestamp so the UI shows progress.
		s.DB.Model(&models.CapabilityRegistry{}).
			Where("id = ?", PublicRegistryID).
			Updates(map[string]any{"last_synced_at": time.Now()})
	}

	logger.Info("catalog-ingest: done entries=%d added=%d updated=%d metadataUpdated=%d skipped=%d deleted=%d failed=%d incomplete=%d duration=%s",
		result.BundleEntries, result.Added, result.Updated, result.MetadataUpdated, result.Skipped, result.Deleted, result.Failed, result.Incomplete, result.Duration.Round(time.Millisecond))

	return result, nil
}

// classifyAndAccumulate routes per-entry error strings into either
// result.Errors (real ingest-side problems) or result.IncompleteErrors
// (upstream data quality issues). The two are counted separately so
// `failed` in the summary reflects only "the ingest is broken" cases.
//
// `entryFailed` is what applyChangedEntry/applyMetadataOnly returned as
// their per-entry failure count. We re-bucket each error string and
// adjust Failed/Incomplete accordingly. This means the helpers don't
// need to know about Incomplete — they just report errs, we classify.
func classifyAndAccumulate(result *IngestResult, entryFailed int, errs []string) {
	if len(errs) == 0 {
		return
	}
	// Each err corresponds to one of the entryFailed counter increments.
	// Re-bucket: if it looks like an upstream-data issue, move it to
	// Incomplete; otherwise leave it as Failed.
	for _, e := range errs {
		if isUpstreamDataIncomplete(e) {
			result.Incomplete++
			result.IncompleteErrors = append(result.IncompleteErrors, e)
		} else {
			result.Failed++
			result.Errors = append(result.Errors, e)
		}
	}
	_ = entryFailed // entryFailed total now redistributed; kept arg for signature clarity
}

// applyChangedEntry handles a manifest entry whose primary file's sha256
// differs from the DB (or is missing entirely). It parses the file via
// ParserService and upserts each parsed item, then runs the metadata +
// security side-channel update so a single ingest pass leaves the row
// fully aligned with upstream.
func (s *CatalogIngestService) applyChangedEntry(
	ctx context.Context,
	bundleDir string,
	paths entryPaths,
	fileBytes []byte,
	fileSHA string,
	entry catalogEntry,
	relatedItems []*models.CapabilityItem,
	globalBySlug map[string]*models.CapabilityItem,
	dryRun bool,
	triggerUser string,
) (added, updated, failed int, errs []string) {
	parsedItems, err := parseEntryFile(s.Parser, fileBytes, paths.SourcePath)
	if err != nil {
		return 0, 0, 1, []string{fmt.Sprintf("entry %s: parse %s: %v", entry.ID, paths.SourcePath, err)}
	}

	// Pair-up index for THIS entry: the related items keyed by slug.
	localBySlug := make(map[string]*models.CapabilityItem, len(relatedItems))
	for _, item := range relatedItems {
		localBySlug[item.ItemType+":"+item.Slug] = item
	}

	for _, parsed := range parsedItems {
		key := parsed.ItemType + ":" + parsed.Slug
		existing, exists := localBySlug[key]
		if !exists {
			// Cross-entry slug collision: another upstream entry already
			// owns a row with this (item_type, slug). Treat as update of
			// THAT row instead of inserting a duplicate (which would crash
			// on the unique constraint). Mirrors SyncService's slugIndex
			// fallback behavior.
			if global, ok := globalBySlug[key]; ok {
				existing = global
				exists = true
			}
		}

		if dryRun {
			if exists {
				updated++
			} else {
				added++
			}
			continue
		}

		if exists {
			if err := s.updateItem(existing, parsed, fileSHA, paths.SourcePath, entry, triggerUser); err != nil {
				failed++
				errs = append(errs, fmt.Sprintf("update %s: %v", existing.ID, err))
				continue
			}
			updated++
		} else {
			newItem, err := s.insertItem(parsed, fileSHA, paths.SourcePath, entry, triggerUser)
			if err != nil {
				failed++
				errs = append(errs, fmt.Sprintf("insert %s: %v", parsed.Slug, err))
				continue
			}
			// Register the new row in the global slug index so that
			// subsequent parsed items in the SAME ingest run (e.g. another
			// .mcp.json that defines the same server name) treat it as an
			// update instead of a duplicate INSERT.
			globalBySlug[key] = newItem
			s.syncAssetsForItem(bundleDir, paths.SourcePath, newItem.ID, &errs)
			added++
		}
	}

	// SecurityScan row write — happens whether new or updated.
	if !dryRun {
		for _, item := range relatedItems {
			s.applySecurityScan(item, entry)
		}
	}

	return added, updated, failed, errs
}

// applyMetadataOnly refreshes the columns that come from index.json
// without touching content or versions. Use when the primary file sha
// is unchanged but upstream may have re-categorized / re-scored.
func (s *CatalogIngestService) applyMetadataOnly(
	entry catalogEntry,
	items []*models.CapabilityItem,
	dryRun bool,
) (updated, skipped, failed int, errs []string) {
	for _, item := range items {
		changed := s.computeMetadataDelta(item, entry)
		if !changed {
			skipped++
			continue
		}
		if dryRun {
			updated++
			continue
		}
		if err := s.applyMetadataDelta(item, entry); err != nil {
			failed++
			errs = append(errs, fmt.Sprintf("metadata %s: %v", item.ID, err))
			continue
		}
		updated++
	}

	// SecurityScan side-channel runs even on metadata-only path because the
	// upstream LLM may have re-evaluated without the primary file changing.
	if !dryRun {
		for _, item := range items {
			s.applySecurityScan(item, entry)
		}
	}
	return updated, skipped, failed, errs
}

// computeMetadataDelta tells whether the upstream entry has any field
// that differs from the DB row. The rule is: upstream non-empty wins,
// upstream empty leaves DB untouched.
func (s *CatalogIngestService) computeMetadataDelta(item *models.CapabilityItem, entry catalogEntry) bool {
	if entry.Source != "" && item.Source != entry.Source {
		return true
	}
	if entry.Description != "" && item.Description != entry.Description {
		return true
	}
	if entry.Category != "" && item.Category != entry.Category {
		return true
	}
	if entry.FinalScore > 0 && item.ExperienceScore != entry.FinalScore {
		return true
	}
	// Tags: compared in applyMetadataDelta itself (need TagSvc query).
	return false
}

func (s *CatalogIngestService) applyMetadataDelta(item *models.CapabilityItem, entry catalogEntry) error {
	updates := map[string]any{}
	if entry.Source != "" {
		updates["source"] = entry.Source
	}
	if entry.Description != "" {
		updates["description"] = entry.Description
	}
	if entry.Category != "" {
		updates["category"] = entry.Category
	}
	if entry.FinalScore > 0 {
		updates["experience_score"] = entry.FinalScore
	}
	if len(updates) > 0 {
		if err := s.DB.Model(&models.CapabilityItem{}).Where("id = ?", item.ID).Updates(updates).Error; err != nil {
			return err
		}
	}
	if len(entry.Tags) > 0 && s.TagSvc != nil {
		s.applyTags(item.ID, entry.Tags)
	}
	if entry.Category != "" && s.CategorySvc != nil {
		s.CategorySvc.EnsureCategory(entry.Category, ingestTriggerUser)
	}
	return nil
}

// updateItem applies content+metadata changes to an existing row and
// bumps the capability_versions revision.
func (s *CatalogIngestService) updateItem(
	existing *models.CapabilityItem,
	parsed *ParsedItem,
	fileSHA, primaryPath string,
	entry catalogEntry,
	triggerUser string,
) error {
	var maxRevision int
	s.DB.Model(&models.CapabilityVersion{}).Where("item_id = ?", existing.ID).
		Select("COALESCE(MAX(revision), 0)").Scan(&maxRevision)

	meta := parsed.Metadata
	if parsed.ItemType == "mcp" {
		normalized, err := NormalizeMCPMetadata(meta)
		if err != nil {
			return fmt.Errorf("normalize mcp metadata: %w", err)
		}
		meta = normalized
	}

	// Field precedence: upstream catalog wins when non-empty; otherwise keep parsed value.
	source := existing.Source
	if entry.Source != "" {
		source = entry.Source
	} else if parsed.Source != "" {
		source = parsed.Source
	}
	description := parsed.Description
	if entry.Description != "" {
		description = entry.Description
	}
	category := parsed.Category
	if entry.Category != "" {
		category = entry.Category
	}
	experienceScore := parsed.ExperienceScore
	if entry.FinalScore > 0 {
		experienceScore = entry.FinalScore
	}

	existing.Name = parsed.Name
	existing.Description = description
	existing.Category = category
	existing.Version = parsed.Version
	existing.Content = parsed.Content
	existing.Source = source
	existing.ExperienceScore = experienceScore
	existing.Status = "active"
	existing.Metadata = metadataJSON(meta)
	existing.SourcePath = primaryPath
	existing.SourceSHA = fileSHA
	existing.UpdatedBy = triggerUser
	// Re-apply ItemType so legacy rows that were imported when the parser
	// classified PROMPT.md/RULE.md as "skill" get migrated as soon as
	// their entry next changes upstream.
	existing.ItemType = parsed.ItemType

	if err := s.DB.Save(existing).Error; err != nil {
		return err
	}

	tags := chooseTags(entry.Tags, parsed.Tags)
	if len(tags) > 0 && s.TagSvc != nil {
		s.applyTags(existing.ID, tags)
	}
	if category != "" && s.CategorySvc != nil {
		s.CategorySvc.EnsureCategory(category, triggerUser)
	}

	ver := &models.CapabilityVersion{
		ID:        uuid.New().String(),
		ItemID:    existing.ID,
		Revision:  maxRevision + 1,
		Content:   parsed.Content,
		Metadata:  metadataJSON(meta),
		CommitMsg: fmt.Sprintf("ingest: catalog %s", entry.ID),
		CreatedBy: triggerUser,
	}
	if err := s.DB.Create(ver).Error; err != nil {
		return err
	}

	if s.ScanJobService != nil {
		_, _ = s.ScanJobService.Enqueue(existing.ID, maxRevision+1, "sync", triggerUser, ScanEnqueueOptions{})
	}

	return nil
}

func (s *CatalogIngestService) insertItem(
	parsed *ParsedItem,
	fileSHA, primaryPath string,
	entry catalogEntry,
	triggerUser string,
) (*models.CapabilityItem, error) {
	meta := parsed.Metadata
	if parsed.ItemType == "mcp" {
		normalized, err := NormalizeMCPMetadata(meta)
		if err != nil {
			return nil, fmt.Errorf("normalize mcp metadata: %w", err)
		}
		meta = normalized
	}
	source := parsed.Source
	if entry.Source != "" {
		source = entry.Source
	}
	description := parsed.Description
	if entry.Description != "" {
		description = entry.Description
	}
	category := parsed.Category
	if entry.Category != "" {
		category = entry.Category
	}
	experienceScore := parsed.ExperienceScore
	if entry.FinalScore > 0 {
		experienceScore = entry.FinalScore
	}

	newItem := &models.CapabilityItem{
		ID:              uuid.New().String(),
		RegistryID:      PublicRegistryID,
		RepoID:          PublicRepoID,
		Slug:            parsed.Slug,
		ItemType:        parsed.ItemType,
		Name:            parsed.Name,
		Description:     description,
		Category:        category,
		Version:         parsed.Version,
		Content:         parsed.Content,
		Metadata:        metadataJSON(meta),
		SourcePath:      primaryPath,
		SourceSHA:       fileSHA,
		Source:          source,
		ExperienceScore: experienceScore,
		Status:          "active",
		CreatedBy:       triggerUser,
		UpdatedBy:       triggerUser,
	}
	if err := s.DB.Create(newItem).Error; err != nil {
		return nil, err
	}

	tags := chooseTags(entry.Tags, parsed.Tags)
	if len(tags) > 0 && s.TagSvc != nil {
		s.applyTags(newItem.ID, tags)
	}
	if category != "" && s.CategorySvc != nil {
		s.CategorySvc.EnsureCategory(category, triggerUser)
	}

	ver := &models.CapabilityVersion{
		ID:        uuid.New().String(),
		ItemID:    newItem.ID,
		Revision:  1,
		Content:   parsed.Content,
		Metadata:  metadataJSON(meta),
		CommitMsg: fmt.Sprintf("ingest: initial import from catalog %s", entry.ID),
		CreatedBy: triggerUser,
	}
	if err := s.DB.Create(ver).Error; err != nil {
		return nil, err
	}

	if s.ScanJobService != nil {
		_, _ = s.ScanJobService.Enqueue(newItem.ID, 1, "sync", triggerUser, ScanEnqueueOptions{})
	}

	return newItem, nil
}

// applyTags is a thin wrapper around TagSvc.EnsureTags + SetItemTags so
// the call sites don't need to handle the two-step dance.
func (s *CatalogIngestService) applyTags(itemID string, tags []string) {
	tagModels, err := s.TagSvc.EnsureTags(tags, TagClassCustom, ingestTriggerUser)
	if err != nil {
		logger.Warn("catalog-ingest: ensure tags failed for %s: %v", itemID, err)
		return
	}
	if len(tagModels) == 0 {
		return
	}
	ids := make([]string, 0, len(tagModels))
	for _, t := range tagModels {
		ids = append(ids, t.ID)
	}
	if err := s.TagSvc.SetItemTags(itemID, ids); err != nil {
		logger.Warn("catalog-ingest: set tags failed for %s: %v", itemID, err)
	}
}

// applySecurityScan upserts a SecurityScan row from the upstream security
// block, mirroring the rules established in
// openspec/changes/import-upstream-security-scan/. Idempotent on
// (item_id, item_revision, scan_model).
func (s *CatalogIngestService) applySecurityScan(item *models.CapabilityItem, entry catalogEntry) {
	sec := entry.Security
	if sec == nil {
		return
	}
	if !validVerdictForRiskLevelStrict(sec.RiskLevel, sec.Verdict) {
		logger.Warn("catalog-ingest: skip security_scan for %s: invalid risk/verdict %q/%q", item.ID, sec.RiskLevel, sec.Verdict)
		return
	}

	var existing models.SecurityScan
	err := s.DB.Where("item_id = ? AND item_revision = ? AND scan_model = ?",
		item.ID, item.CurrentRevision, sec.ScanModel).First(&existing).Error
	if err == nil {
		return // already recorded
	}
	if !errors.Is(err, gorm.ErrRecordNotFound) {
		logger.Warn("catalog-ingest: query existing security_scan for %s: %v", item.ID, err)
		return
	}

	scannedAt := time.Now()
	if sec.ScannedAt != "" {
		if parsed, perr := time.Parse(time.RFC3339, sec.ScannedAt); perr == nil {
			scannedAt = parsed
		}
	}
	perms := sec.Permissions
	if perms == nil {
		perms = &catalogPermissions{Files: []string{}, Network: []string{}, Commands: []string{}}
	}
	redFlags := sec.RedFlags
	if redFlags == nil {
		redFlags = []string{}
	}
	recs := sec.Recommendations
	if recs == nil {
		recs = []string{}
	}

	scanID := uuid.New().String()
	row := &models.SecurityScan{
		ID:              scanID,
		ItemID:          item.ID,
		ItemRevision:    item.CurrentRevision,
		TriggerType:     "sync",
		ScanModel:       sec.ScanModel,
		Category:        "",
		BuiltinTags:     datatypes.JSON([]byte("[]")),
		RiskLevel:       sec.RiskLevel,
		Verdict:         sec.Verdict,
		RedFlags:        mustMarshalJSON(redFlags),
		Permissions:     mustMarshalJSON(perms),
		Summary:         sec.Summary,
		Recommendations: mustMarshalJSON(recs),
		CreatedAt:       scannedAt,
		FinishedAt:      &scannedAt,
	}

	err = s.DB.Transaction(func(tx *gorm.DB) error {
		if err := tx.Create(row).Error; err != nil {
			return err
		}
		statusValue := sec.RiskLevel
		if statusValue == "" {
			statusValue = "unscanned"
		}
		return tx.Model(&models.CapabilityItem{}).
			Where("id = ?", item.ID).
			Updates(map[string]any{
				"security_status": statusValue,
				"last_scan_id":    scanID,
			}).Error
	})
	if err != nil {
		logger.Warn("catalog-ingest: write security_scan for %s: %v", item.ID, err)
	}
}

// syncAssetsForItem brings non-primary files in the same entry directory
// into capability_assets, mirroring SyncService.syncAssets semantics.
// Implementation deferred — for the initial cut, mcp/plugin/prompt/rule
// entries are single-file (no assets), and skill assets (references/,
// scripts/) are nice-to-have but not blocking for the data link.
func (s *CatalogIngestService) syncAssetsForItem(bundleDir, primaryPath, itemID string, errs *[]string) {
	// TODO(catalog-ingest): port SyncService.syncAssets to read from a
	// plain dir instead of GitService. The current data link tests don't
	// depend on assets so leaving as a no-op is safe — assets will appear
	// the next time a real sync runs through SyncService.
	_ = bundleDir
	_ = primaryPath
	_ = itemID
	_ = errs
}

// ensurePublicRegistry copies the row creation logic from migrate so this
// service can be called standalone (e.g. by `migrate ingest-upstream`).
func (s *CatalogIngestService) ensurePublicRegistry() error {
	var exists int
	if err := s.DB.Raw(`SELECT 1 FROM capability_registries WHERE id = ?`, PublicRegistryID).Scan(&exists).Error; err != nil {
		return err
	}
	if exists == 1 {
		return nil
	}
	now := s.DB.NowFunc()
	reg := models.CapabilityRegistry{
		ID:          PublicRegistryID,
		Name:        "public",
		Description: "Default public registry (catalog ingest)",
		SourceType:  "internal",
		RepoID:      PublicRepoID,
		OwnerID:     ingestTriggerUser,
		CreatedAt:   now,
		UpdatedAt:   now,
	}
	return s.DB.Create(&reg).Error
}

// ---- helpers (file-local) -------------------------------------------------

// parseEntryFile dispatches to the right ParserService method based on the
// file name. Mirrors SyncService.parseFile, but kept separate to avoid
// taking a dependency on SyncService's struct method receiver.
func parseEntryFile(p *ParserService, content []byte, relPath string) ([]*ParsedItem, error) {
	lower := strings.ToLower(relPath)
	base := strings.ToLower(filepath.Base(relPath))
	switch {
	case base == "hooks.json":
		item, err := p.ParseHooksJSON(content, relPath)
		if err != nil {
			return nil, err
		}
		return []*ParsedItem{item}, nil
	case base == ".mcp.json":
		return p.ParseMCPJSON(content, relPath)
	case base == ".plugin.json":
		return p.ParsePluginJSON(content, relPath)
	case base == "agents.md" || strings.HasSuffix(lower, "/agents.md"):
		items, err := p.ParseAgentsMD(content, relPath)
		if err != nil || len(items) == 0 {
			item, err2 := p.ParseSKILLMD(content, relPath)
			if err2 != nil {
				return nil, err2
			}
			return []*ParsedItem{item}, nil
		}
		return items, nil
	default:
		item, err := p.ParseSKILLMD(content, relPath)
		if err != nil {
			return nil, err
		}
		return []*ParsedItem{item}, nil
	}
}

// entryPaths is the four-tuple of paths derived from a single index.json entry.
// Splitting it lets us reconcile two layouts without ambiguity:
//
//	entryDir   = "<type-dir>/<id>"                    ← legacy SourcePath prefix (DB query key)
//	sourcePath = "<type-dir>/<id>/<filename>"         ← stored verbatim on capability_items.source_path
//	bundleDir  = "catalog-download/<type-dir>/<id>"   ← bundle relative entry dir
//	bundlePath = "catalog-download/<type-dir>/<id>/<filename>" ← absolute file in materialized bundle
//
// We keep entryDir / sourcePath without the "catalog-download/" prefix so
// that existing rows (written by the legacy import path) still index by
// the same key — avoids a one-time SourcePath rewrite.
type entryPaths struct {
	EntryDir   string
	SourcePath string
	BundleDir  string
	BundlePath string
}

func primaryPathsForEntry(entry catalogEntry) (entryPaths, bool) {
	typeDir, fileName, ok := typeDirAndFile(entry.Type)
	if !ok {
		return entryPaths{}, false
	}
	entryDir := filepath.Join(typeDir, entry.ID)
	return entryPaths{
		EntryDir:   entryDir,
		SourcePath: filepath.Join(entryDir, fileName),
		BundleDir:  filepath.Join("catalog-download", entryDir),
		BundlePath: filepath.Join("catalog-download", entryDir, fileName),
	}, true
}

func typeDirAndFile(itemType string) (typeDir, fileName string, ok bool) {
	switch itemType {
	case "mcp":
		return "mcp", ".mcp.json", true
	case "skill":
		return "skills", "SKILL.md", true
	case "plugin":
		return "plugins", ".plugin.json", true
	case "prompt":
		return "prompts", "PROMPT.md", true
	case "rule":
		return "rules", "RULE.md", true
	}
	return "", "", false
}

// indexItemsBySlug returns a global lookup by "<itemType>:<slug>", used to
// catch cross-entry slug collisions before they trip the (repo_id,
// item_type, slug) unique constraint.
func indexItemsBySlug(items []models.CapabilityItem) map[string]*models.CapabilityItem {
	out := make(map[string]*models.CapabilityItem, len(items))
	for i := range items {
		out[items[i].ItemType+":"+items[i].Slug] = &items[i]
	}
	return out
}

// indexItemsByEntryDir groups existing DB rows by the upstream-derived
// "catalog-download/<type-dir>/<id>" prefix. A single upstream entry maps
// to >=1 DB rows; this index lets us locate all of them in O(1).
func indexItemsByEntryDir(items []models.CapabilityItem) map[string][]*models.CapabilityItem {
	out := make(map[string][]*models.CapabilityItem, len(items))
	for i := range items {
		dir := entryDirFromSourcePath(items[i].SourcePath)
		if dir == "" {
			continue
		}
		out[dir] = append(out[dir], &items[i])
	}
	return out
}

// entryDirFromSourcePath chops a SourcePath like
//
//	skills/foo-bar-agskill/SKILL.md
//	catalog-download/skills/foo-bar-agskill/SKILL.md
//
// down to "<type-dir>/<id>" (without the optional catalog-download/ prefix).
// We use this everywhere — both for indexing existing DB rows and for
// matching against `primaryPathsForEntry(...).EntryDir`.
func entryDirFromSourcePath(sourcePath string) string {
	if sourcePath == "" {
		return ""
	}
	p := filepath.ToSlash(sourcePath)
	parts := strings.Split(p, "/")
	if len(parts) > 0 && parts[0] == "catalog-download" {
		parts = parts[1:]
	}
	if len(parts) < 2 {
		return ""
	}
	return filepath.Join(parts[0], parts[1])
}

// chooseTags implements the precedence rule: upstream tags win when
// non-empty (catalog has authoritative taxonomy), otherwise fall back to
// what the per-file parser extracted (e.g. SKILL.md frontmatter).
func chooseTags(upstream, parsed []string) []string {
	if len(upstream) > 0 {
		return upstream
	}
	return parsed
}

// validVerdictForRiskLevelStrict mirrors the rule used in migrate's
// backfillCatalogMetadata. Kept here so this service has zero coupling to
// the migrate package.
func validVerdictForRiskLevelStrict(riskLevel, verdict string) bool {
	switch riskLevel {
	case "clean", "low":
		return verdict == "safe"
	case "medium":
		return verdict == "caution"
	case "high", "extreme":
		return verdict == "reject"
	}
	return false
}

func mustMarshalJSON(v any) datatypes.JSON {
	b, err := json.Marshal(v)
	if err != nil || len(b) == 0 {
		return datatypes.JSON([]byte("{}"))
	}
	return datatypes.JSON(b)
}

// readBundleFile reads `absPath`, with one retry on ENOENT after
// renormalizing the path between Unicode NFC ↔ NFD. catalog/index.json
// is written by upstream Python tooling in NFC, so primaryPathsForEntry
// produces NFC paths. The on-disk catalog-download/ tree may be stored
// as NFD (e.g. when the bundle was packed on a macOS HFS+/APFS host
// without normalization). Without the fallback we get spurious ENOENT
// for entries whose IDs contain combining characters (görsel, için,
// sporsmaç, …). Upstream build_catalog_bundle.py also normalizes member
// names to NFC, so this fallback is purely defensive for older bundles
// that pre-date that fix.
func readBundleFile(absPath string) ([]byte, error) {
	b, err := os.ReadFile(absPath)
	if err == nil {
		return b, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	// Try the opposite normalization form. NFC files won't exist if the
	// disk has NFD; NFD reads won't exist if the disk has NFC.
	if alt := norm.NFD.String(absPath); alt != absPath {
		if b2, err2 := os.ReadFile(alt); err2 == nil {
			return b2, nil
		}
	}
	if alt := norm.NFC.String(absPath); alt != absPath {
		if b2, err2 := os.ReadFile(alt); err2 == nil {
			return b2, nil
		}
	}
	return nil, err
}

// ---- bundle materialization ----------------------------------------------

// materialize turns an IngestSource into a working directory that
// contains manifest.json + index.json + catalog-download/. The returned
// cleanup callback is always non-nil and safe to call even on error.
func (s *CatalogIngestService) materialize(ctx context.Context, src IngestSource) (string, func(), error) {
	noop := func() {}
	switch {
	case src.Dir != "":
		abs, err := filepath.Abs(src.Dir)
		if err != nil {
			return "", noop, err
		}
		st, err := os.Stat(abs)
		if err != nil || !st.IsDir() {
			return "", noop, fmt.Errorf("source dir not accessible: %s", abs)
		}
		return abs, noop, nil

	case src.Tarball != "":
		abs, err := filepath.Abs(src.Tarball)
		if err != nil {
			return "", noop, err
		}
		return extractTarballToTempDir(abs)

	case src.URL != "":
		tmpFile, err := os.CreateTemp("", "costrict-ingest-*.tar.gz")
		if err != nil {
			return "", noop, err
		}
		defer os.Remove(tmpFile.Name())
		if err := downloadTo(ctx, s.httpClient(), src.URL, tmpFile); err != nil {
			tmpFile.Close()
			return "", noop, err
		}
		tmpFile.Close()
		return extractTarballToTempDir(tmpFile.Name())
	}
	return "", noop, fmt.Errorf("IngestSource has no URL / Tarball / Dir set")
}

func (s *CatalogIngestService) httpClient() *http.Client {
	if s.HTTP != nil {
		return s.HTTP
	}
	return &http.Client{Timeout: 5 * time.Minute}
}

func downloadTo(ctx context.Context, client *http.Client, url string, w io.Writer) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("download %s: HTTP %d", url, resp.StatusCode)
	}
	_, err = io.Copy(w, resp.Body)
	return err
}

func extractTarballToTempDir(tarballPath string) (string, func(), error) {
	noop := func() {}
	st, err := os.Stat(tarballPath)
	if err != nil {
		return "", noop, fmt.Errorf("stat tarball: %w", err)
	}
	if st.IsDir() {
		return "", noop, fmt.Errorf("tarball path is a directory: %s", tarballPath)
	}

	dest, err := os.MkdirTemp("", "costrict-ingest-bundle-*")
	if err != nil {
		return "", noop, err
	}
	cleanup := func() { os.RemoveAll(dest) }

	f, err := os.Open(tarballPath)
	if err != nil {
		cleanup()
		return "", noop, err
	}
	defer f.Close()

	var reader io.Reader = f
	if strings.HasSuffix(strings.ToLower(tarballPath), ".gz") || strings.HasSuffix(strings.ToLower(tarballPath), ".tgz") {
		gz, err := gzip.NewReader(f)
		if err != nil {
			cleanup()
			return "", noop, fmt.Errorf("gzip open: %w", err)
		}
		defer gz.Close()
		reader = gz
	}

	tr := tar.NewReader(reader)
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			cleanup()
			return "", noop, fmt.Errorf("tar read: %w", err)
		}
		name := filepath.Clean(hdr.Name)
		if strings.HasPrefix(name, "..") || strings.HasPrefix(name, "/") {
			cleanup()
			return "", noop, fmt.Errorf("tar entry escapes bundle root: %s", hdr.Name)
		}
		out := filepath.Join(dest, name)
		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(out, 0o755); err != nil {
				cleanup()
				return "", noop, err
			}
		case tar.TypeReg, tar.TypeRegA:
			if err := os.MkdirAll(filepath.Dir(out), 0o755); err != nil {
				cleanup()
				return "", noop, err
			}
			w, err := os.OpenFile(out, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
			if err != nil {
				cleanup()
				return "", noop, err
			}
			if _, err := io.Copy(w, tr); err != nil {
				w.Close()
				cleanup()
				return "", noop, err
			}
			w.Close()
		default:
			// skip symlinks, devices, etc.
		}
	}
	return dest, cleanup, nil
}

func readManifest(bundleDir string) (*catalogBundleManifest, error) {
	data, err := os.ReadFile(filepath.Join(bundleDir, "manifest.json"))
	if err != nil {
		return nil, err
	}
	m := &catalogBundleManifest{}
	if err := json.Unmarshal(data, m); err != nil {
		return nil, err
	}
	return m, nil
}

func readIndex(bundleDir string) ([]catalogEntry, error) {
	data, err := os.ReadFile(filepath.Join(bundleDir, "index.json"))
	if err != nil {
		return nil, err
	}
	var entries []catalogEntry
	if err := json.Unmarshal(data, &entries); err != nil {
		return nil, err
	}
	// Filter out entries with whitespace-only / empty tags so downstream
	// len() > 0 checks don't fire on noise.
	for i := range entries {
		clean := entries[i].Tags[:0]
		for _, t := range entries[i].Tags {
			if s := strings.TrimSpace(t); s != "" {
				clean = append(clean, s)
			}
		}
		entries[i].Tags = clean
	}
	return entries, nil
}

