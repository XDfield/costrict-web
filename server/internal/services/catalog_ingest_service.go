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
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
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
	// Reparse: route every entry through the full parse/update path even when
	// the primary file SHA is unchanged. Needed when the PARSER's derived
	// output changes (e.g. the plugin install-copy template) — content is
	// generated at parse time, so a template change otherwise never reaches
	// rows whose upstream file didn't move.
	Reparse bool
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
// The catalog also carries health (freshness/popularity/source_trust signals)
// and evaluation (6 rubric dimensions + final decision) blocks, which we
// persist VERBATIM into dedicated jsonb columns. They are captured as
// json.RawMessage rather than decoded into fixed structs so that any extra
// upstream fields (e.g. health.signals.install_popularity,
// evaluation.evaluation_mode) survive the ingest unmodified — see design.md
// decision 1 ("store the whole upstream JSON shape"). Other top-level fields
// index.json carries (e.g. weak_dims) remain intentionally unmapped.
type catalogEntry struct {
	ID            string                `json:"id"`
	Type          string                `json:"type"`
	Source        string                `json:"source"`
	Description   string                `json:"description"`
	DescriptionZh string                `json:"description_zh"`
	Category      string                `json:"category"`
	Tags          []string              `json:"tags"`
	FinalScore    float64               `json:"final_score"`
	Security      *catalogSecurityBlock `json:"security,omitempty"`
	Health        json.RawMessage       `json:"health,omitempty"`
	Evaluation    json.RawMessage       `json:"evaluation,omitempty"`
	// BundledIn marks a sub-skill that upstream expanded out of a plugin. Its
	// value is the parent plugin's catalog entry id (== catalogEntry.ID of the
	// plugin entry, NOT a DB uuid). Resolved to capability_items.parent_plugin_id
	// in the second reconcile pass; mirrored into metadata.bundled_in for tracing.
	BundledIn string `json:"bundled_in,omitempty"`
	// SourcePath is the faithful, plugin-root-relative path of this entry in the
	// original repository (e.g. "rules/dfx/安全.md", "skills/foo/SKILL.md"),
	// emitted by the upstream catalog pipeline. When present it is stored
	// verbatim on capability_items.source_path so the plugin "work tree" mirrors
	// the real repo layout. When empty the ingest falls back to the synthetic
	// "<type-dir>/<id>/<file>" path. The file CONTENT is always read from the
	// bundle's physical "<type-dir>/<id>/<file>" location regardless of this
	// field. NOT used for MCP children, which keep their "<path>#<key>" form.
	SourcePath string `json:"source_path,omitempty"`
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

	// seenSourcePaths is the set of EXACT primary file paths the bundle carries
	// this run (normalized). The soft-archive pass keys on this, not on the
	// 2-segment entryDir: a nested sub-skill row
	// (skills/<parent>/<child>/SKILL.md) collapses to the same entryDir as a
	// still-present top-level skills/<parent>/SKILL.md, so entryDir-keying never
	// archived it. Comparing the full source_path keeps the parent and archives
	// the orphaned child.
	seenSourcePaths := make(map[string]bool, len(entries))

	// Second-pass reconcile state (plugin child → parent plugin linking).
	//
	//   pluginChildEntryDirs — entryDir of every skill/MCP entry we saw this run,
	//     mapped to its declared bundled_in (parent plugin's upstream entry id;
	//     "" when the item is independent, i.e. un-bundled).
	//   pluginEntryIDsSeen — set of plugin entry ids present in this bundle, so
	//     the second pass can resolve bundled_in even when the parent plugin and
	//     its sub-skills first appear together in the same batch (order-independent).
	// We resolve entry id → DB item via the source_path entry-dir prefix in the
	// second pass (after all writes land), so same-batch inserts are visible.
	pluginChildEntryDirs := make(map[string]string)
	pluginEntryIDsSeen := make(map[string]bool)

	for _, entry := range entries {
		if pluginBundledChildTypes[entry.Type] {
			if paths, ok := primaryPathsForEntry(entry); ok {
				// Record even an empty bundled_in: an item that previously was a
				// plugin child but is no longer bundled must have its parent link
				// cleared (un-link), which the second pass does on "".
				pluginChildEntryDirs[paths.EntryDir] = entry.BundledIn
			}
		}
		if entry.Type == "plugin" {
			pluginEntryIDsSeen[entry.ID] = true
		}

		paths, ok := primaryPathsForEntry(entry)
		if !ok {
			// Unsupported type is an upstream-shape problem: the bundle is
			// claiming an entry whose type we have no parser for. Bucket
			// into Incomplete so it doesn't masquerade as a code bug.
			result.Incomplete++
			result.IncompleteErrors = append(result.IncompleteErrors, fmt.Sprintf("entry %s: unsupported type %q", entry.ID, entry.Type))
			continue
		}
		// Seed the seen-set under EVERY form a DB row may store this entry's
		// path as, so the soft-archive predicate below recognizes the row as
		// "still shipped" regardless of when it was last written:
		//   - synthetic "<type-dir>/<id>/<file>" — legacy catalog rows (and MCP
		//     children, which always keep the synthetic form) store this.
		//   - faithful repo-relative path — rows written/converged after the
		//     path-faithful change store this on source_path.
		// Pre-faithful rows whose primary file is gone upstream match NEITHER
		// form and are archived (ebdb4ad's nested-orphan fix), while a surviving
		// sibling under the same entryDir is kept because its own path is seeded.
		seenSourcePaths[normalizeSourcePath(paths.SourcePath)] = true
		if entry.Type != "mcp" {
			seenSourcePaths[normalizeSourcePath(faithfulSourcePath(entry, paths.SourcePath))] = true
		}

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

		anyContentChanged := opts.Reparse
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
			updated, skipped, failed, errs := s.applyMetadataOnly(entry, relatedItems, paths.SourcePath, paths.EntryDir, opts.DryRun)
			result.MetadataUpdated += updated
			result.Skipped += skipped
			classifyAndAccumulate(result, failed, errs)
		}
	}

	// Soft-archive items whose EXACT primary file is no longer in upstream. Runs
	// BEFORE the parent-link reconcile so rows archived this round are already
	// excluded from its status filter — otherwise an active child could be
	// linked to a parent plugin that this very loop is about to archive.
	//
	// Scope is deliberately `itemsByEntryDir` (rows whose catalog_entry_dir /
	// source_path parses to a catalog `<type-dir>/<id>` shape), NOT all public
	// rows: user-authored items carry an empty / single-segment source_path that
	// drops out of the index, so they never enter this map and are never
	// archived here. The archive PREDICATE keys on the full source_path
	// (seenSourcePaths, seeded above under both the synthetic and the faithful
	// form) rather than the 2-segment entryDir, so a nested sub-skill orphan
	// whose parent skill is still present upstream is archived (its own path is
	// gone) while the parent stays active.
	if !opts.DryRun {
		for _, items := range itemsByEntryDir {
			for _, item := range items {
				if item.Status == "archived" {
					continue
				}
				// Still shipped upstream under its exact path → keep.
				if seenSourcePaths[normalizeSourcePath(item.SourcePath)] {
					continue
				}
				// Never sweep user-owned rows: forks and uploaded (archive)
				// items can carry a catalog-shaped source_path but are not
				// catalog-managed. Mirrors reconcileParentPluginLinks' exclusion.
				if item.SourceType == "archive" || item.SourceType == "fork" {
					continue
				}
				if err := s.DB.Model(&models.CapabilityItem{}).
					Where("id = ?", item.ID).
					Updates(map[string]any{"status": "archived"}).Error; err != nil {
					result.Failed++
					result.Errors = append(result.Errors, fmt.Sprintf("archive %s: %v", item.ID, err))
					continue
				}
				// Archiving a plugin cascades an UNLINK (not archive) to its
				// children: children that vanished upstream archive via their
				// own entryDirs in this same loop; children still shipped
				// upstream stay active but must not point at an archived
				// parent. If the parent later resurrects, the reconcile pass
				// re-links them from bundled_in.
				if item.ItemType == "plugin" {
					if err := s.DB.Model(&models.CapabilityItem{}).
						Where("parent_plugin_id = ?", item.ID).
						Update("parent_plugin_id", nil).Error; err != nil {
						result.Failed++
						result.Errors = append(result.Errors, fmt.Sprintf("unlink children of archived plugin %s: %v", item.ID, err))
					}
				}
				result.Deleted++
			}
		}
		// Refresh registry's last_sync timestamp so the UI shows progress.
		s.DB.Model(&models.CapabilityRegistry{}).
			Where("id = ?", PublicRegistryID).
			Updates(map[string]any{"last_synced_at": time.Now()})
	}

	// Second pass: resolve each sub-skill's `bundled_in` (parent plugin's
	// upstream entry id) to the parent plugin's DB item id and write
	// capability_items.parent_plugin_id. This runs after every entry has been
	// written so a parent plugin and its sub-skills appearing in the SAME batch
	// (in any order) both resolve — we look the rows up fresh by source_path
	// entry-dir prefix rather than relying on a mid-loop in-memory index —
	// and after the soft-archive loop so archived rows are out of scope.
	if !opts.DryRun && len(pluginChildEntryDirs) > 0 {
		s.reconcileParentPluginLinks(pluginChildEntryDirs, pluginEntryIDsSeen, result)
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

	// Items eligible for SecurityScan write at the bottom of this function.
	// MUST include both the pre-existing rows we just updated AND the rows
	// we freshly inserted. Pre-fix this slice was just `relatedItems`,
	// which silently skipped SecurityScan for every newly-added entry —
	// the symptom was 633 plugins all rendering with an empty Risk Level
	// column on the staging frontend after the first ingest, because
	// applySecurityScan never fired for them.
	itemsForScan := make([]*models.CapabilityItem, 0, len(relatedItems)+len(parsedItems))
	itemsForScan = append(itemsForScan, relatedItems...)

	// Normalize all parsed items first so the adoption pre-pass below sees
	// final (scoped) slugs/types.
	scopedParsedItems := make([]*ParsedItem, 0, len(parsedItems))
	for _, parsed := range parsedItems {
		parsed = scopeBundledMCPParsedItem(parsed, entry)
		// For plugin-bundled children the upstream entry type is authoritative:
		// InferItemType's content heuristics otherwise re-classify e.g. a
		// SKILL.md named "webhooks" as hook / "using-tmux-..." as command,
		// which orphans the row from parent-link reconciliation (skill/mcp only).
		if isPluginBundledChild(entry) && parsed.ItemType != entry.Type {
			parsed.ItemType = entry.Type
		}
		scopedParsedItems = append(scopedParsedItems, parsed)
	}
	// Rows that will be matched by slug belong to their parsed item; the
	// flip-adoption fallback must not claim them for a different sibling.
	adoptedRowIDs := map[string]bool{}
	{
		parsedKeys := map[string]bool{}
		for _, p := range scopedParsedItems {
			parsedKeys[p.ItemType+":"+p.Slug] = true
		}
		for _, old := range relatedItems {
			if parsedKeys[old.ItemType+":"+old.Slug] {
				adoptedRowIDs[old.ID] = true
			}
		}
	}

	for _, parsed := range scopedParsedItems {
		key := parsed.ItemType + ":" + parsed.Slug
		existing, exists := localBySlug[key]
		if !exists && !isPluginBundledChild(entry) {
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
		if !exists && isPluginBundledChild(entry) {
			// independent→bundled flip: scopeBundledMCPParsedItem rewrote the
			// parsed slug, so the entry's OWN pre-flip row (same entryDir,
			// indexed under the old slug) no longer matches by slug. Adopt it
			// by entryDir + item_type instead of inserting a duplicate — the
			// old row would otherwise never be archived (its entryDir stays
			// seen) and its stale SourceSHA would force the full update path
			// every round, minting a version + scan job on zero upstream
			// change. updateItem migrates the adopted row's slug.
			for _, old := range relatedItems {
				if old.ItemType != parsed.ItemType {
					continue
				}
				if adoptedRowIDs[old.ID] {
					continue // already claimed by another parsed item this entry
				}
				existing = old
				exists = true
				adoptedRowIDs[old.ID] = true
				break
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

		// Path the row stores on source_path. The whole work tree mirrors the
		// real repo, so every bundled child type prefers the upstream entry's
		// faithful repo-relative path; MCP children keep the synthetic
		// "<type-dir>/<id>/<file>" form (their identity is "<path>#<key>",
		// never a real on-disk path). The match key (catalog_entry_dir) stays
		// synthetic in all cases so re-ingest still locates the row.
		displayPath := paths.SourcePath
		if parsed.ItemType != "mcp" {
			displayPath = faithfulSourcePath(entry, paths.SourcePath)
		}

		if exists {
			if err := s.updateItem(existing, parsed, fileSHA, displayPath, paths.EntryDir, entry, triggerUser); err != nil {
				failed++
				errs = append(errs, fmt.Sprintf("update %s: %v", existing.ID, err))
				continue
			}
			updated++
		} else {
			newItem, err := s.insertItem(parsed, fileSHA, displayPath, paths.EntryDir, entry, triggerUser)
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
			// Also queue the new row for the SecurityScan side-channel
			// below, otherwise its risk_level / last_scan_id would stay
			// NULL until a future ingest cycle re-classifies it as
			// metadata-only.
			itemsForScan = append(itemsForScan, newItem)
			s.syncAssetsForItem(bundleDir, paths.SourcePath, newItem.ID, &errs)
			added++
		}
	}

	// SecurityScan row write — happens whether new or updated.
	if !dryRun {
		for _, item := range itemsForScan {
			s.applySecurityScan(item, entry)
		}
	}

	return added, updated, failed, errs
}

// pluginBundledChildTypes is the set of item types that can appear as a plugin's
// bundled children in the catalog. It must stay in sync with the path→type
// contract shared across the upstream catalog pipeline, the archive-upload
// extractors (handlers.pluginFileChildSpecs / extractSubSkillAssets), and the
// frontend TYPE_META: skill / mcp / command / subagent / rule / template.
// evaluators are synthesized upstream as item_type=skill, so they're covered by
// the "skill" entry.
var pluginBundledChildTypes = map[string]bool{
	"skill":    true,
	"mcp":      true,
	"command":  true,
	"subagent": true,
	"rule":     true,
	"template": true,
}

func isPluginBundledChild(entry catalogEntry) bool {
	return entry.BundledIn != "" && pluginBundledChildTypes[entry.Type]
}

// isUniqueViolationErr matches unique-constraint violations across the
// sqlite (tests) and postgres (prod) drivers. Mirrors the handlers-side
// isUniqueConstraintError; kept local to avoid a cross-package dependency.
func isUniqueViolationErr(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "UNIQUE constraint failed") ||
		strings.Contains(msg, "duplicate key value violates unique constraint") ||
		strings.Contains(msg, "duplicated key not allowed")
}

func scopeBundledMCPParsedItem(parsed *ParsedItem, entry catalogEntry) *ParsedItem {
	if entry.BundledIn == "" || entry.Type != "mcp" || parsed.ItemType != "mcp" {
		return parsed
	}
	scoped := *parsed
	scoped.Slug = slugifyKey(entry.ID)
	return &scoped
}

// applyMetadataOnly refreshes the columns that come from index.json
// without touching content or versions. Use when the primary file sha
// is unchanged but upstream may have re-categorized / re-scored.
//
// syntheticPath/entryDir are the entry's synthetic "<type-dir>/<id>/<file>" and
// "<type-dir>/<id>" derived paths. They let this path also converge a row's
// source_path to the upstream faithful path (and backfill catalog_entry_dir)
// when only metadata changed — otherwise a first P3 rollout where existing
// cospower skills are content-identical would keep their stale synthetic
// source_path forever (the content-changed path never runs for them).
func (s *CatalogIngestService) applyMetadataOnly(
	entry catalogEntry,
	items []*models.CapabilityItem,
	syntheticPath, entryDir string,
	dryRun bool,
) (updated, skipped, failed int, errs []string) {
	for _, item := range items {
		// Faithful source_path for this row (MCP keeps the synthetic form, same
		// rule as the content-changed path).
		displayPath := syntheticPath
		if item.ItemType != "mcp" {
			displayPath = faithfulSourcePath(entry, syntheticPath)
		}

		changed := s.computeMetadataDelta(item, entry, displayPath, entryDir)
		if !changed {
			skipped++
			continue
		}
		if dryRun {
			updated++
			continue
		}
		if err := s.applyMetadataDelta(item, entry, displayPath, entryDir); err != nil {
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
func (s *CatalogIngestService) computeMetadataDelta(item *models.CapabilityItem, entry catalogEntry, displayPath, entryDir string) bool {
	// An archived row whose entry re-appeared upstream must be resurrected
	// even when nothing else changed. The content-changed path already does
	// this (updateItem sets Status="active"); this covers the unchanged path.
	if item.Status == "archived" {
		return true
	}
	// source_path / catalog_entry_dir convergence: a row imported before the
	// faithful-path change (or before catalog_entry_dir existed) keeps its stale
	// synthetic source_path even when content is identical. Detect drift so the
	// work tree mirrors the real repo for these unchanged rows too.
	if displayPath != "" && item.SourcePath != displayPath {
		return true
	}
	if entryDir != "" && item.CatalogEntryDir != entryDir {
		return true
	}
	// Bundled children mis-typed by an earlier ingest (InferItemType path
	// heuristics) must converge to the authoritative entry type even when
	// content is unchanged, else parent-link reconciliation keeps skipping them.
	if isPluginBundledChild(entry) && item.ItemType != entry.Type {
		return true
	}
	if entry.Source != "" && item.Source != entry.Source {
		return true
	}
	if entry.Description != "" && item.Description != entry.Description {
		return true
	}
	// descriptions JSONB drift: upstream brought a new zh translation, etc.
	if !descriptionsJSONEqual(item.Descriptions, buildDescriptionsJSON(entry)) {
		return true
	}
	if entry.Category != "" && item.Category != entry.Category {
		return true
	}
	if entry.FinalScore > 0 && item.ExperienceScore != entry.FinalScore {
		return true
	}
	// health/evaluation JSONB drift: detected so a backfill run (file SHA +
	// other metadata unchanged) still routes through the metadata-only path.
	if !jsonbObjectEqual(item.Health, healthJSON(entry.Health)) {
		return true
	}
	if !jsonbObjectEqual(item.Evaluation, evaluationJSON(entry.Evaluation)) {
		return true
	}
	if !bundledInMirrorEqual(item.Metadata, entry.BundledIn) {
		return true
	}
	// Tags: compared in applyMetadataDelta itself (need TagSvc query).
	return false
}

// jsonbObjectEqual compares two jsonb payloads for semantic equality,
// tolerating the pre-migration column states (empty bytes, `null`, `{}`)
// which all count as "empty". Byte comparison alone would be wrong because
// json.Marshal does not guarantee key order, and an empty column may be
// stored as any of those three forms.
func jsonbObjectEqual(a, b datatypes.JSON) bool {
	var va, vb any
	emptyA := len(a) == 0
	if !emptyA {
		_ = json.Unmarshal(a, &va)
		emptyA = isEmptyJSONValue(va)
	}
	emptyB := len(b) == 0
	if !emptyB {
		_ = json.Unmarshal(b, &vb)
		emptyB = isEmptyJSONValue(vb)
	}
	if emptyA || emptyB {
		return emptyA && emptyB
	}
	return reflect.DeepEqual(va, vb)
}

// isEmptyJSONValue reports whether a decoded JSON value is "empty" for the
// purposes of jsonb drift detection: JSON null, or an empty object.
func isEmptyJSONValue(v any) bool {
	if v == nil {
		return true
	}
	if m, ok := v.(map[string]any); ok {
		return len(m) == 0
	}
	return false
}

// descriptionsJSONEqual compares two locale → text JSON maps for semantic
// equality. Byte comparison would be incorrect because json.Marshal does
// not guarantee key order across Go versions.
func descriptionsJSONEqual(a, b datatypes.JSON) bool {
	var ma, mb map[string]string
	if len(a) > 0 {
		_ = json.Unmarshal(a, &ma)
	}
	if len(b) > 0 {
		_ = json.Unmarshal(b, &mb)
	}
	if len(ma) != len(mb) {
		return false
	}
	for k, v := range ma {
		if mb[k] != v {
			return false
		}
	}
	return true
}

func (s *CatalogIngestService) applyMetadataDelta(item *models.CapabilityItem, entry catalogEntry, displayPath, entryDir string) error {
	updates := map[string]any{}
	if item.Status == "archived" {
		updates["status"] = "active"
	}
	// Converge source_path to the faithful repo-relative path and backfill the
	// decoupled match key, even when only metadata changed.
	if displayPath != "" && item.SourcePath != displayPath {
		updates["source_path"] = displayPath
	}
	if entryDir != "" && item.CatalogEntryDir != entryDir {
		updates["catalog_entry_dir"] = entryDir
	}
	if isPluginBundledChild(entry) && item.ItemType != entry.Type {
		updates["item_type"] = entry.Type
	}
	if entry.Source != "" {
		updates["source"] = entry.Source
	}
	if entry.Description != "" {
		updates["description"] = entry.Description
	}
	// descriptions JSONB is rewritten unconditionally on each ingest pass so
	// that a removed upstream zh translation also clears from the DB row.
	// Spec: integral replacement, no merge with prior content.
	newDescs := buildDescriptionsJSON(entry)
	if !descriptionsJSONEqual(item.Descriptions, newDescs) {
		updates["descriptions"] = newDescs
	}
	if entry.Category != "" {
		updates["category"] = entry.Category
	}
	if entry.FinalScore > 0 {
		updates["experience_score"] = entry.FinalScore
	}
	// health/evaluation are always written on this path so a metadata-only
	// upstream change (no primary-file diff) still backfills these columns.
	// Nil blocks normalize to an empty object, so this never clobbers with null.
	updates["health"] = healthJSON(entry.Health)
	updates["evaluation"] = evaluationJSON(entry.Evaluation)
	if !bundledInMirrorEqual(item.Metadata, entry.BundledIn) {
		updates["metadata"] = metadataWithBundledInMirror(item.Metadata, entry.BundledIn)
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
	fileSHA, primaryPath, entryDir string,
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
	meta = withBundledInMirror(meta, entry.BundledIn)

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
	existing.Descriptions = buildDescriptionsJSON(entry)
	existing.Category = category
	existing.Version = parsed.Version
	existing.Content = parsed.Content
	existing.Source = source
	existing.ExperienceScore = experienceScore
	existing.Health = healthJSON(entry.Health)
	existing.Evaluation = evaluationJSON(entry.Evaluation)
	existing.Status = "active"
	// Slug migration for adopted rows (independent→bundled flip rewrote the
	// parsed slug; the row was matched by entryDir instead). No-op when the
	// row was matched by slug — parsed.Slug equals the existing slug then.
	if parsed.Slug != "" {
		existing.Slug = parsed.Slug
	}
	existing.Metadata = metadataJSON(meta)
	existing.SourcePath = primaryPath
	existing.CatalogEntryDir = entryDir
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
		ID:           uuid.New().String(),
		ItemID:       existing.ID,
		Revision:     maxRevision + 1,
		Name:         parsed.Name,
		Description:  description,
		Descriptions: buildDescriptionsJSON(entry),
		Category:     category,
		Version:      parsed.Version,
		Content:      parsed.Content,
		Metadata:     metadataJSON(meta),
		CommitMsg:    fmt.Sprintf("ingest: catalog %s", entry.ID),
		CreatedBy:    triggerUser,
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
	fileSHA, primaryPath, entryDir string,
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
	meta = withBundledInMirror(meta, entry.BundledIn)
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
		Descriptions:    buildDescriptionsJSON(entry),
		Category:        category,
		Version:         parsed.Version,
		Content:         parsed.Content,
		Metadata:        metadataJSON(meta),
		Health:          healthJSON(entry.Health),
		Evaluation:      evaluationJSON(entry.Evaluation),
		SourcePath:      primaryPath,
		CatalogEntryDir: entryDir,
		SourceSHA:       fileSHA,
		Source:          source,
		ExperienceScore: experienceScore,
		Status:          "active",
		CreatedBy:       triggerUser,
		UpdatedBy:       triggerUser,
	}
	// Unique-constraint fallback: the (repo_id, item_type, slug) index is shared
	// with user-uploaded / promoted / forked rows and with other catalog entries
	// whose ids fold to the same slug. Bundled children deliberately skip the
	// cross-entry slug adoption (they must not hijack a foreign row), so without
	// a retry a collision would make the entry fail EVERY ingest round forever
	// (the conflicting row keeps the index slot even when archived). Mirrors the
	// -2..-10 suffix convention of the upload-promotion path.
	baseSlug := newItem.Slug
	createErr := s.DB.Create(newItem).Error
	for attempt := 2; createErr != nil && isUniqueViolationErr(createErr) && attempt <= 10; attempt++ {
		newItem.Slug = fmt.Sprintf("%s-%d", baseSlug, attempt)
		createErr = s.DB.Create(newItem).Error
	}
	if createErr != nil {
		return nil, createErr
	}

	tags := chooseTags(entry.Tags, parsed.Tags)
	if len(tags) > 0 && s.TagSvc != nil {
		s.applyTags(newItem.ID, tags)
	}
	if category != "" && s.CategorySvc != nil {
		s.CategorySvc.EnsureCategory(category, triggerUser)
	}

	ver := &models.CapabilityVersion{
		ID:           uuid.New().String(),
		ItemID:       newItem.ID,
		Revision:     1,
		Name:         parsed.Name,
		Description:  description,
		Descriptions: buildDescriptionsJSON(entry),
		Category:     category,
		Version:      parsed.Version,
		Content:      parsed.Content,
		Metadata:     metadataJSON(meta),
		CommitMsg:    fmt.Sprintf("ingest: initial import from catalog %s", entry.ID),
		CreatedBy:    triggerUser,
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

// typeDirAndFile maps a catalog entry type to its (type-dir, primary-file) pair
// under catalog-download/. It MUST stay in lock-step with the upstream
// TYPE_DIR_AND_FILE (costrict-skills-repo/scripts/build_catalog_bundle.py) and
// _PRIMARY_FILE_BY_TYPE (download_catalog.py): the bundle lays each entry out at
// catalog-download/<type-dir>/<id>/<file>, so a mismatch here means ingest can't
// find the file. command/subagent/template follow the established
// "<type>s dir + <TYPE>.md file" convention used by skill/prompt/rule.
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
	case "template":
		return "templates", "TEMPLATE.md", true
	case "command":
		return "commands", "COMMAND.md", true
	case "subagent":
		// Upstream writes subagent children to subagents/<id>/AGENT.md (see
		// build_catalog_bundle.TYPE_DIR_AND_FILE / download_catalog
		// _SINGLE_FILE_TYPE_SPEC). This filename MUST match it byte-for-byte or
		// the bundle file can't be located and the child fails to ingest.
		return "subagents", "AGENT.md", true
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

// indexItemsByEntryDir groups existing DB rows by the synthetic
// "<type-dir>/<id>" entry key. A single upstream entry maps to >=1 DB rows;
// this index lets us locate all of them in O(1) on re-ingest.
//
// The key is taken from catalog_entry_dir when set (the decoupled match key,
// so source_path can carry the faithful repo path). For legacy rows ingested
// before catalog_entry_dir existed it falls back to deriving the key from
// source_path — those rows still have the synthetic "<type-dir>/<id>/<file>"
// source_path, so the derivation is correct until the next ingest backfills
// catalog_entry_dir and rewrites source_path to the faithful form.
func indexItemsByEntryDir(items []models.CapabilityItem) map[string][]*models.CapabilityItem {
	out := make(map[string][]*models.CapabilityItem, len(items))
	for i := range items {
		dir := catalogEntryDirForRow(items[i])
		if dir == "" {
			continue
		}
		out[dir] = append(out[dir], &items[i])
	}
	return out
}

// catalogEntryDirForRow returns the synthetic match key for a DB row, preferring
// the stored catalog_entry_dir and falling back to deriving it from source_path
// for legacy rows that predate the column.
func catalogEntryDirForRow(item models.CapabilityItem) string {
	if item.CatalogEntryDir != "" {
		return item.CatalogEntryDir
	}
	return entryDirFromSourcePath(item.SourcePath)
}

// faithfulSourcePath returns the path to store on capability_items.source_path:
// the upstream entry's verbatim repo-relative path when provided, else the
// synthetic "<type-dir>/<id>/<file>" fallback. Used for every bundled child
// type (including skill) so the whole work tree mirrors the real repo. MCP
// children are excluded by the caller (they keep their "<path>#<key>" form).
func faithfulSourcePath(entry catalogEntry, synthetic string) string {
	if sp := strings.TrimSpace(entry.SourcePath); sp != "" {
		return filepath.ToSlash(sp)
	}
	return synthetic
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

// normalizeSourcePath canonicalizes a SourcePath for exact comparison against
// the bundle's primary file paths (`primaryPathsForEntry(...).SourcePath`):
// forward slashes, with the optional leading "catalog-download/" stripped.
// Both the DB rows (written by this service) and the bundle paths are normally
// already in this form; the prefix strip defends against legacy rows that
// stored the bundle-relative path — same tolerance as entryDirFromSourcePath.
func normalizeSourcePath(sourcePath string) string {
	if sourcePath == "" {
		return ""
	}
	return strings.TrimPrefix(filepath.ToSlash(sourcePath), "catalog-download/")
}

// buildDescriptionsJSON packs the upstream entry's per-locale descriptions
// into a JSONB map. en/zh are written only when the corresponding upstream
// field is non-empty; the resulting map fully replaces the column on write
// so a removed upstream translation also disappears from the DB row.
func buildDescriptionsJSON(entry catalogEntry) datatypes.JSON {
	m := map[string]string{}
	if entry.Description != "" {
		m["en"] = entry.Description
	}
	if entry.DescriptionZh != "" {
		m["zh"] = entry.DescriptionZh
	}
	if len(m) == 0 {
		return datatypes.JSON([]byte("{}"))
	}
	b, err := json.Marshal(m)
	if err != nil {
		return datatypes.JSON([]byte("{}"))
	}
	return datatypes.JSON(b)
}

// healthJSON normalizes the upstream health block into a jsonb payload,
// preserving the raw bytes verbatim (see catalogEntry doc / design.md
// decision 1). Empty/nil/null/empty-object input collapses to "{}" so the
// column always holds a valid, non-null JSON object.
func healthJSON(raw json.RawMessage) datatypes.JSON {
	return rawBlockJSON(raw)
}

// evaluationJSON mirrors healthJSON for the evaluation block. Extra upstream
// fields (e.g. evaluation_mode) and missing rubric dimensions are preserved
// exactly as upstream emitted them — no field is dropped or defaulted.
func evaluationJSON(raw json.RawMessage) datatypes.JSON {
	return rawBlockJSON(raw)
}

// rawBlockJSON passes an upstream jsonb block through unchanged (lossless),
// only normalizing to a canonical "{}" when the payload is not a usable
// object. It compacts whitespace so byte-identical re-ingests stay stable, but
// never reshapes or drops keys of a real object.
//
// The column contract is "JSON object" (see the swagger `object` type on
// CapabilityItem.Health/Evaluation). So anything that isn't a NON-EMPTY JSON
// object — empty bytes, invalid JSON, null, empty object, OR a valid-but-wrong
// scalar/array (e.g. `health: []`, `evaluation: "x"`) — collapses to "{}"
// rather than persisting a malformed shape the frontend can't consume.
func rawBlockJSON(raw json.RawMessage) datatypes.JSON {
	empty := datatypes.JSON([]byte("{}"))
	if len(raw) == 0 {
		return empty
	}
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return empty
	}
	// Enforce the object contract: only a non-empty JSON object passes through.
	obj, ok := v.(map[string]any)
	if !ok || len(obj) == 0 {
		return empty
	}
	var buf bytes.Buffer
	if err := json.Compact(&buf, raw); err != nil {
		// Already validated above; fall back to the raw bytes rather than dropping data.
		return datatypes.JSON([]byte(raw))
	}
	return datatypes.JSON(buf.Bytes())
}

// reconcileParentPluginLinks performs the second pass of sub-skill linking.
//
// pluginChildEntryDirs maps every skill/MCP entry's entryDir ("skills/<id>" or
// "mcp/<id>") seen this run to its declared bundled_in (parent plugin's upstream
// entry id, or "" when the child is independent / was un-bundled).
// pluginEntryIDsSeen is the set of plugin entry ids present in the bundle.
//
// For each plugin child it resolves bundled_in → parent plugin DB id and writes
// parent_plugin_id. Children whose bundled_in is empty (independent / un-bundled)
// have any stale parent_plugin_id cleared. All reads are batched (no N+1) and use
// only portable GORM/SQL so the SQLite test path and Postgres prod path agree.
func (s *CatalogIngestService) reconcileParentPluginLinks(
	pluginChildEntryDirs map[string]string,
	pluginEntryIDsSeen map[string]bool,
	result *IngestResult,
) {
	// 1) Load every public skill + plugin row once. We index by entryDir
	//    (derived from source_path) so same-batch inserts — invisible to the
	//    pre-loop existingItems snapshot — are included via this fresh read.
	//
	//    Scope guards (both matter — silent data corruption otherwise):
	//    - source_type: the public registry also holds zip-promoted sub-skills
	//      (source_type='archive') and forks, whose source_path can collide
	//      byte-for-byte with a catalog entryDir (skills/<name>/SKILL.md). The
	//      catalog reconcile must never link/unlink rows it doesn't own.
	//    - status: archived rows must neither receive new links (a child would
	//      end up pointing at a parent invisible in the market) nor occupy an
	//      entryDir slot that shadows the active row.
	// Load every public plugin row plus every row of a type that can be a
	// bundled child. Derived from pluginBundledChildTypes so adding a new child
	// type (rule/template/command/subagent) automatically widens this scope.
	childAndPluginTypes := make([]string, 0, len(pluginBundledChildTypes)+1)
	for t := range pluginBundledChildTypes {
		childAndPluginTypes = append(childAndPluginTypes, t)
	}
	childAndPluginTypes = append(childAndPluginTypes, "plugin")

	var rows []models.CapabilityItem
	if err := s.DB.
		Where("registry_id = ? AND item_type IN ? AND source_type NOT IN ? AND status <> 'archived'",
			PublicRegistryID, childAndPluginTypes, []string{"archive", "fork"}).
		Select("id", "item_type", "source_path", "catalog_entry_dir", "parent_plugin_id").
		Find(&rows).Error; err != nil {
		logger.Warn("catalog-ingest: load rows for parent-plugin reconcile: %v", err)
		return
	}

	// entryDir → DB item ids. Multi-valued: one upstream entry can legitimately
	// own several rows (e.g. a multi-server .mcp.json); a single-valued map
	// would link/unlink only the arbitrary last row.
	childIDsByEntryDir := make(map[string][]string)
	pluginIDByEntryDir := make(map[string]string)
	// current parent_plugin_id per child DB id (to detect stale links to clear).
	childParentByID := make(map[string]*string)
	for i := range rows {
		dir := catalogEntryDirForRow(rows[i])
		if dir == "" {
			continue
		}
		if rows[i].ItemType == "plugin" {
			pluginIDByEntryDir[dir] = rows[i].ID
			continue
		}
		// Any bundled-child type (skill/mcp/rule/template/command/subagent).
		if pluginBundledChildTypes[rows[i].ItemType] {
			childIDsByEntryDir[dir] = append(childIDsByEntryDir[dir], rows[i].ID)
			childParentByID[rows[i].ID] = rows[i].ParentPluginID
		}
	}

	// 2) Resolve each plugin child's target parent and collect grouped updates:
	//    parentID → []childID to link, plus a list of childIDs to unlink.
	toLink := make(map[string][]string)
	var toUnlink []string
	for entryDir, bundledIn := range pluginChildEntryDirs {
		childIDs, ok := childIDsByEntryDir[entryDir]
		if !ok {
			continue // child row not found (parse/insert failed upstream of here)
		}
		for _, childID := range childIDs {
			curParent := childParentByID[childID]

			if bundledIn == "" {
				// Independent item: clear any stale parent link (un-bundled upstream).
				if curParent != nil && *curParent != "" {
					toUnlink = append(toUnlink, childID)
				}
				continue
			}

			parentID, ok := pluginIDByEntryDir[filepath.Join("plugins", bundledIn)]
			if !ok {
				// Parent plugin not present/active this run (orphan, archived, or
				// arrives later). Leave parent_plugin_id as-is; a future ingest
				// with the parent active links it.
				if !pluginEntryIDsSeen[bundledIn] {
					logger.Warn("catalog-ingest: plugin child %s bundled_in=%q has no parent plugin in this bundle; leaving parent_plugin_id unset", childID, bundledIn)
				}
				continue
			}
			if curParent != nil && *curParent == parentID {
				continue // already linked correctly
			}
			toLink[parentID] = append(toLink[parentID], childID)
		}
	}

	// 3) Apply grouped updates — one UPDATE per distinct parent, one for unlinks.
	for parentID, skillIDs := range toLink {
		if err := s.DB.Model(&models.CapabilityItem{}).
			Where("id IN ?", skillIDs).
			Update("parent_plugin_id", parentID).Error; err != nil {
			result.Failed++
			result.Errors = append(result.Errors, fmt.Sprintf("link sub-skills to plugin %s: %v", parentID, err))
		}
	}
	if len(toUnlink) > 0 {
		if err := s.DB.Model(&models.CapabilityItem{}).
			Where("id IN ?", toUnlink).
			Update("parent_plugin_id", nil).Error; err != nil {
			result.Failed++
			result.Errors = append(result.Errors, fmt.Sprintf("unlink sub-skills: %v", err))
		}
	}
}

// withBundledInMirror returns a copy of meta with a "bundled_in" key set to
// bundledIn (the parent plugin's upstream entry id) when bundledIn is non-empty.
// This mirrors the upstream sub-skill annotation into the row's metadata jsonb
// so it survives alongside parent_plugin_id for tracing/debugging, matching the
// catalog's own `bundled_in` field. When bundledIn is empty the original map is
// returned untouched (sub-skills that were un-bundled upstream clear the link in
// the second reconcile pass; here we simply don't re-add the mirror).
func withBundledInMirror(meta map[string]any, bundledIn string) map[string]any {
	if bundledIn == "" {
		return meta
	}
	out := make(map[string]any, len(meta)+1)
	for k, v := range meta {
		out[k] = v
	}
	out["bundled_in"] = bundledIn
	return out
}

func bundledInMirrorEqual(meta datatypes.JSON, bundledIn string) bool {
	var m map[string]any
	if len(meta) > 0 {
		_ = json.Unmarshal(meta, &m)
	}
	if len(m) == 0 {
		return bundledIn == ""
	}
	cur, _ := m["bundled_in"].(string)
	return cur == bundledIn
}

func metadataWithBundledInMirror(meta datatypes.JSON, bundledIn string) datatypes.JSON {
	m := map[string]any{}
	if len(meta) > 0 {
		_ = json.Unmarshal(meta, &m)
	}
	if bundledIn == "" {
		delete(m, "bundled_in")
	} else {
		m["bundled_in"] = bundledIn
	}
	b, err := json.Marshal(m)
	if err != nil {
		return datatypes.JSON([]byte("{}"))
	}
	return datatypes.JSON(b)
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
