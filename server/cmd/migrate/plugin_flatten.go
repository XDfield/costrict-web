// `migrate flatten-plugins` — retire the package-derived Plugin child rows that
// the old "a repository/archive is a package of capabilities" model created.
//
// The flat model says one Cloud item is one explicit capability coordinate. The
// writers that violated that are already gone (catalog `bundled_in` linking,
// archive sub-skill promotion, fork child promotion, recursive Git discovery);
// this command deals with what they already wrote.
//
// What it does, and what it refuses to do
// ---------------------------------------
//   - Classifies each `parent_plugin_id` row by PROVENANCE only. Not by shape,
//     not by name, not by "it looks like a child". A row whose provenance does
//     not decide the question is reported and skipped, forever, until an
//     operator decides. Guessing on 6.7k rows is how a cleanup becomes an
//     incident.
//   - Archives and unlinks package-derived rows. It never hard-deletes a row, a
//     favorite, a distribution, or a Git repository, and it never moves a
//     favorite to the parent plugin (SD-3): the archived row stays addressable
//     so the relationship remains auditable.
//   - Preserves a row that carries its own explicit Git coordinate. That row is
//     a capability in its own right and only loses the parent link.
//
// Safety protocol
// ---------------
// Dry-run is the default and writes nothing to `capability_items`. It persists a
// row-level plan (see the 20260806000400 migration) and exports the same plan as
// a checksummed JSON artifact.
//
// Apply verifies the plan digest, then walks the plan in bounded transactions,
// writing each row with a compare-and-set on the status and parent link the plan
// inventoried. A row somebody changed in between fails the predicate, is recorded
// as a conflict, and is left exactly as it is. Re-running apply is a no-op for
// rows already applied, so a crash mid-run resumes by run id.
//
// Rollback is a first-class run of its own with the same two phases. It
// compare-and-sets against the POST-migration state, so a row legitimately
// changed after the migration is skipped rather than reverted underneath its
// new owner.

package main

import (
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"
)

// pluginFlattenSchemaVersion is bumped when the plan/artifact shape changes.
// Apply refuses a run it does not understand rather than reinterpreting a field.
const pluginFlattenSchemaVersion = 1

// pluginFlattenDefaultBatchSize bounds each applying transaction. Small enough
// that a crash loses little work and no lock is held long, large enough that
// 6.7k rows do not become 6.7k transactions.
const pluginFlattenDefaultBatchSize = 200

// pluginFlattenDefaultReportLimit caps the per-row lines printed to a terminal.
// The totals are always complete, and --artifact always carries every row, so
// truncating the console never truncates the evidence.
const pluginFlattenDefaultReportLimit = 20

// pluginFlattenDefaultRollbackWindow is the compatibility window during which a
// migrate run may still be rolled back. Past it, the deprecated parent link has
// been absent from responses for a full release and consumers may legitimately
// have moved on; reviving it needs an explicit human decision, which is what
// --force is.
const pluginFlattenDefaultRollbackWindow = 30 * 24 * time.Hour

// Classifications. Only provenance decides which one a row gets.
const (
	// Created by catalog ingest from an entry that declared `bundled_in`: it is
	// a file inside another entry's plugin package.
	flattenClassDerivedCatalog = "derived_catalog"
	// Promoted out of an uploaded plugin archive by the retired sub-skill
	// promotion path.
	flattenClassDerivedArchive = "derived_archive"
	// Minted by the retired fork-children path: a copy of a row that was itself
	// a package child, re-parented to the plugin fork.
	flattenClassDerivedFork = "derived_fork"
	// Carries a complete explicit Git coordinate. A capability in its own right,
	// whatever the parent link says.
	flattenClassIndependent = "independent"
	// Provenance does not decide. Reported, never touched.
	flattenClassAmbiguous = "ambiguous"
)

const (
	flattenActionArchiveAndUnlink = "archive_and_unlink"
	flattenActionUnlinkOnly       = "unlink_only"
	flattenActionRestore          = "restore"
	flattenActionSkip             = "skip"
)

const (
	flattenRowPending = "pending"
	flattenRowApplied = "applied"
	flattenRowSkipped = "skipped"
	flattenRowFailed  = "failed"
)

const (
	flattenRunPlanned    = "planned"
	flattenRunApplying   = "applying"
	flattenRunApplied    = "applied"
	flattenRunPartial    = "partial"
	flattenRunRolledBack = "rolled_back"
)

const (
	flattenModeMigrate  = "migrate"
	flattenModeRollback = "rollback"
)

type pluginFlattenOptions struct {
	// Confirm inverts the default: planning is free, changing data takes a word.
	Confirm bool
	// RunID selects an existing plan for apply/rollback/status. Empty on plan.
	RunID string
	// ArtifactPath, when set, is where the checksummed plan JSON is written
	// (plan) or read and verified against the stored digest (apply/rollback).
	ArtifactPath string
	BatchSize    int
	// ReportLimit caps printed per-row lines. Totals are always complete.
	ReportLimit int
	// Force overrides the rollback window. It cannot override a digest mismatch
	// or a compare-and-set failure; those are correctness, not policy.
	Force     bool
	CreatedBy string
}

// flattenCandidate is one live `parent_plugin_id` row plus everything the
// classifier is allowed to look at. Read once, under one snapshot.
type flattenCandidate struct {
	ItemID            string
	ItemType          string
	Slug              string
	RegistryID        string
	SourceType        string
	ContentBackend    string
	CatalogEntryDir   string
	SourcePath        string
	SourceSHA         string
	Metadata          []byte
	GitServerID       string
	GitRepoID         int64
	GitRepoPath       string
	GitEntryKey       string
	ForkedFromItemID  *string
	Status            string
	ParentPluginID    string
	ParentExists      bool
	ParentItemType    string
	ParentSourceType  string
	ForkSourceLinked  bool
	FavoriteCount     int
	DistributionCount int
}

// flattenVerdict is what the classifier produced for one candidate.
type flattenVerdict struct {
	Classification string
	Action         string
	Reason         string
	AfterStatus    string
	AfterParent    *string
}

// classifyFlattenCandidate is the whole decision. It is deliberately a pure
// function of one already-loaded row so the rules can be read in one place and
// tested without a database.
//
// The order of the checks is part of the contract:
//
//  1. Structural impossibilities first. A dangling or non-plugin parent, a self
//     link, or a plugin claiming a parent means the data does not describe the
//     relationship this migration knows how to retire. Provenance below would
//     "recognise" some of these and act on them, which is precisely the guessing
//     the contract forbids.
//  2. Moderation state next. A banned row's status was set by a human decision;
//     overwriting it with `archived` would erase that decision's visibility.
//  3. Independent identity before derived provenance. A row with its own Git
//     coordinate is a capability whatever else is true of it, and demoting one
//     to package content is the only unrecoverable mistake available here.
//  4. Derived provenance last, each requiring positive evidence.
func classifyFlattenCandidate(c flattenCandidate) flattenVerdict {
	skip := func(reason string) flattenVerdict {
		return flattenVerdict{
			Classification: flattenClassAmbiguous,
			Action:         flattenActionSkip,
			Reason:         reason,
			AfterStatus:    c.Status,
			AfterParent:    &c.ParentPluginID,
		}
	}

	switch {
	case c.ParentPluginID == c.ItemID:
		return skip("row is its own parent; the link is corrupt, not derived")
	case !c.ParentExists:
		return skip("parent row " + c.ParentPluginID + " does not exist; provenance cannot be confirmed")
	case c.ParentItemType != "plugin":
		return skip("parent is item_type=" + c.ParentItemType + ", not a plugin")
	case c.ItemType == "plugin":
		return skip("a plugin claiming a parent plugin is not a package child this migration understands")
	case c.Status == "banned":
		return skip("status=banned is a moderation decision; archiving would hide it")
	}

	if hasCompleteGitCoordinate(c) {
		// The parent link is the only thing wrong with this row.
		return flattenVerdict{
			Classification: flattenClassIndependent,
			Action:         flattenActionUnlinkOnly,
			Reason:         "row owns an explicit Git coordinate and stands alone once unlinked",
			AfterStatus:    c.Status,
			AfterParent:    nil,
		}
	}
	if c.ContentBackend == "git" || c.GitServerID != "" || c.GitRepoID > 0 || c.GitRepoPath != "" {
		// Some Git identity, but not a usable coordinate. Either half-migrated
		// or mid-provision; a wrong call here either strands a real capability
		// or preserves a duplicate.
		return skip("partial Git identity (server=" + c.GitServerID +
			" repo=" + strconv.FormatInt(c.GitRepoID, 10) + " path=" + c.GitRepoPath +
			"); neither an explicit coordinate nor plainly DB-backed")
	}

	archive := func(class, reason string) flattenVerdict {
		return flattenVerdict{
			Classification: class,
			Action:         flattenActionArchiveAndUnlink,
			Reason:         reason,
			AfterStatus:    "archived",
			AfterParent:    nil,
		}
	}

	switch c.SourceType {
	case "direct":
		// Catalog provenance requires all three: the registry-managed origin,
		// the catalog entry key, and the upstream bundling annotation. Two out
		// of three is a row that merely resembles a catalog child.
		if c.CatalogEntryDir != "" && metadataBundledIn(c.Metadata) != "" {
			return archive(flattenClassDerivedCatalog,
				"catalog entry "+c.CatalogEntryDir+" declared bundled_in="+metadataBundledIn(c.Metadata))
		}
		return skip("db-backed row with a parent link but no catalog bundling evidence (entry_dir=" +
			c.CatalogEntryDir + ", bundled_in=" + metadataBundledIn(c.Metadata) + ")")
	case "archive":
		// The archive path only ever produced children under a plugin it had
		// just written, so the plugin parent IS the generated-child provenance.
		return archive(flattenClassDerivedArchive,
			"promoted out of the archive of plugin "+c.ParentPluginID)
	case "fork":
		// A fork is a user's own copy, so the bar is higher: it counts as
		// generated only when the row it was forked FROM was itself a package
		// child. Otherwise the user may have forked a standalone capability and
		// something else attached the parent link.
		if c.ForkedFromItemID == nil || *c.ForkedFromItemID == "" {
			return skip("fork row without forked_from_item_id; cannot prove it was generated by a plugin fork")
		}
		if !c.ForkSourceLinked {
			return skip("forked from " + *c.ForkedFromItemID +
				", which is not itself a package child; may be a user's fork of a standalone capability")
		}
		return archive(flattenClassDerivedFork,
			"generated by forking plugin child "+*c.ForkedFromItemID)
	default:
		return skip("unrecognised source_type=" + c.SourceType)
	}
}

// hasCompleteGitCoordinate mirrors the uq_capability_items_git_manifest partial
// index: those four columns together are what makes a Git-backed row
// addressable. Anything less cannot be resynced from Git and is therefore not
// an independent capability, whatever content_backend claims.
func hasCompleteGitCoordinate(c flattenCandidate) bool {
	return c.ContentBackend == "git" &&
		c.GitServerID != "" &&
		c.GitRepoID > 0 &&
		c.GitRepoPath != ""
}

func metadataBundledIn(raw []byte) string {
	if len(raw) == 0 {
		return ""
	}
	var meta map[string]any
	if err := json.Unmarshal(raw, &meta); err != nil {
		return ""
	}
	value, _ := meta["bundled_in"].(string)
	return strings.TrimSpace(value)
}

// ---------------------------------------------------------------------------
// Plan
// ---------------------------------------------------------------------------

// planPluginFlatten inventories every live `parent_plugin_id` row, classifies
// it, and persists the result as a new `planned` run. It writes nothing to
// capability_items.
//
// The whole inventory is read inside one REPEATABLE READ transaction: the plan
// is a compare-and-set predicate, so every row's before-state has to come from
// one consistent view of the database. Rows read across a moving snapshot would
// produce a plan that was never simultaneously true.
func planPluginFlatten(db *gorm.DB, opts pluginFlattenOptions, out io.Writer) (string, error) {
	if err := ensurePluginFlattenTables(db); err != nil {
		return "", err
	}
	runID := uuid.NewString()
	batch := opts.BatchSize
	if batch <= 0 {
		batch = pluginFlattenDefaultBatchSize
	}

	var candidates []flattenCandidate
	err := db.Transaction(func(tx *gorm.DB) error {
		if err := setRepeatableRead(tx); err != nil {
			return err
		}
		loaded, err := loadFlattenCandidates(tx)
		if err != nil {
			return err
		}
		candidates = loaded
		return nil
	})
	if err != nil {
		return "", fmt.Errorf("inventory parent-linked rows: %w", err)
	}

	sort.Slice(candidates, func(i, j int) bool { return candidates[i].ItemID < candidates[j].ItemID })

	rows := make([]pluginFlattenPlanRow, 0, len(candidates))
	for i, c := range candidates {
		verdict := classifyFlattenCandidate(c)
		parent := c.ParentPluginID
		// A row the classifier declined is decided, not outstanding. Leaving it
		// `pending` would make every run report `partial` forever and would hide
		// a genuinely unfinished run behind the noise.
		rowState := flattenRowPending
		if verdict.Action == flattenActionSkip {
			rowState = flattenRowSkipped
		}
		rows = append(rows, pluginFlattenPlanRow{
			Seq:                  int64(i + 1),
			ItemID:               c.ItemID,
			ItemType:             c.ItemType,
			ItemSlug:             c.Slug,
			RegistryID:           c.RegistryID,
			SourceType:           c.SourceType,
			ContentBackend:       c.ContentBackend,
			CatalogEntryDir:      c.CatalogEntryDir,
			BundledIn:            metadataBundledIn(c.Metadata),
			SourcePath:           c.SourcePath,
			SourceManifestSHA:    c.SourceSHA,
			GitServerID:          c.GitServerID,
			GitRepoID:            c.GitRepoID,
			GitRepoPath:          c.GitRepoPath,
			GitEntryKey:          c.GitEntryKey,
			ForkedFromItemID:     c.ForkedFromItemID,
			ParentItemID:         &parent,
			ParentExists:         c.ParentExists,
			ParentItemType:       c.ParentItemType,
			ParentSourceType:     c.ParentSourceType,
			FavoriteCount:        c.FavoriteCount,
			DistributionCount:    c.DistributionCount,
			BeforeStatus:         c.Status,
			BeforeParentPluginID: &parent,
			AfterStatus:          verdict.AfterStatus,
			AfterParentPluginID:  verdict.AfterParent,
			Classification:       verdict.Classification,
			Action:               verdict.Action,
			Reason:               verdict.Reason,
			RowState:             rowState,
		})
	}

	digest := pluginFlattenPlanDigest(pluginFlattenSchemaVersion, flattenModeMigrate, rows)
	totals := pluginFlattenTotals(rows)

	if err := persistPluginFlattenRun(db, pluginFlattenRunRecord{
		ID:            runID,
		SchemaVersion: pluginFlattenSchemaVersion,
		Mode:          flattenModeMigrate,
		Status:        flattenRunPlanned,
		BatchSize:     batch,
		PlanDigest:    digest,
		Totals:        totals,
		CreatedBy:     opts.CreatedBy,
	}, rows); err != nil {
		return "", err
	}

	if opts.ArtifactPath != "" {
		if err := writePluginFlattenArtifact(opts.ArtifactPath, pluginFlattenArtifact{
			SchemaVersion: pluginFlattenSchemaVersion,
			RunID:         runID,
			Mode:          flattenModeMigrate,
			PlanDigest:    digest,
			GeneratedAt:   time.Now().UTC().Format(time.RFC3339),
			Totals:        totals,
			Rows:          rows,
		}); err != nil {
			return "", err
		}
	}

	reportPluginFlattenPlan(out, runID, digest, totals, rows, opts)
	return runID, nil
}

// loadFlattenCandidates reads the inventory in four bounded queries instead of
// one join-everything statement: the parent lookup, the fork-source lookup and
// the two entitlement counts each have a different cardinality, and keeping them
// apart is what lets the parent/fork resolution report "absent" rather than
// silently dropping the row the way an INNER JOIN would.
func loadFlattenCandidates(tx *gorm.DB) ([]flattenCandidate, error) {
	type itemRow struct {
		ID                string
		ItemType          string
		Slug              string
		RegistryID        string
		SourceType        string
		ContentBackend    string
		CatalogEntryDir   string
		SourcePath        string
		SourceSHA         string
		Metadata          []byte
		SourceGitServerID string
		SourceGitRepoID   int64
		SourceRepoPath    string
		SourceGitEntryKey string
		ForkedFromItemID  *string
		Status            string
		ParentPluginID    string
	}
	var items []itemRow
	if err := tx.Table("capability_items").
		Select(`id, item_type, slug, registry_id::text AS registry_id, source_type, content_backend,
		        COALESCE(catalog_entry_dir, '') AS catalog_entry_dir, COALESCE(source_path, '') AS source_path,
		        COALESCE(source_sha, '') AS source_sha, metadata,
		        source_git_server_id, source_git_repo_id, source_repo_path, source_git_entry_key,
		        forked_from_item_id::text AS forked_from_item_id,
		        COALESCE(status, '') AS status, parent_plugin_id::text AS parent_plugin_id`).
		Where("parent_plugin_id IS NOT NULL").
		Order("id ASC").
		Scan(&items).Error; err != nil {
		return nil, err
	}
	if len(items) == 0 {
		return nil, nil
	}

	parentIDs := make([]string, 0, len(items))
	forkSourceIDs := make([]string, 0, len(items))
	itemIDs := make([]string, 0, len(items))
	for _, it := range items {
		parentIDs = append(parentIDs, it.ParentPluginID)
		itemIDs = append(itemIDs, it.ID)
		if it.ForkedFromItemID != nil && *it.ForkedFromItemID != "" {
			forkSourceIDs = append(forkSourceIDs, *it.ForkedFromItemID)
		}
	}

	type parentRow struct {
		ID         string
		ItemType   string
		SourceType string
	}
	parents := map[string]parentRow{}
	if err := scanInChunks(tx, parentIDs, func(chunk []string) error {
		var got []parentRow
		if err := tx.Table("capability_items").
			Select("id::text AS id, item_type, source_type").
			Where("id IN ?", chunk).Scan(&got).Error; err != nil {
			return err
		}
		for _, p := range got {
			parents[p.ID] = p
		}
		return nil
	}); err != nil {
		return nil, err
	}

	// A fork's source counts as generated provenance only while it is itself
	// parent-linked. Checked against the live column, not against this run's
	// candidate set, so the answer does not depend on ordering within the plan.
	forkSourceLinked := map[string]bool{}
	if err := scanInChunks(tx, forkSourceIDs, func(chunk []string) error {
		var got []string
		if err := tx.Table("capability_items").
			Where("id IN ? AND parent_plugin_id IS NOT NULL", chunk).
			Pluck("id::text", &got).Error; err != nil {
			return err
		}
		for _, id := range got {
			forkSourceLinked[id] = true
		}
		return nil
	}); err != nil {
		return nil, err
	}

	favorites, err := countByItem(tx, "item_favorites", itemIDs, "")
	if err != nil {
		return nil, fmt.Errorf("count favorites: %w", err)
	}
	// Live distributions only. The line is labelled as consumer impact, and a
	// revoked distribution has no consumer left to impact; the revoked rows stay
	// in the table, so nothing is lost to audit by not counting them here.
	distributions, err := countByItem(tx, "item_distributions", itemIDs, "status = 'active' AND revoked_at IS NULL")
	if err != nil {
		return nil, fmt.Errorf("count distributions: %w", err)
	}

	out := make([]flattenCandidate, 0, len(items))
	for _, it := range items {
		parent, exists := parents[it.ParentPluginID]
		linked := false
		if it.ForkedFromItemID != nil {
			linked = forkSourceLinked[*it.ForkedFromItemID]
		}
		out = append(out, flattenCandidate{
			ItemID:            it.ID,
			ItemType:          it.ItemType,
			Slug:              it.Slug,
			RegistryID:        it.RegistryID,
			SourceType:        it.SourceType,
			ContentBackend:    it.ContentBackend,
			CatalogEntryDir:   it.CatalogEntryDir,
			SourcePath:        it.SourcePath,
			SourceSHA:         it.SourceSHA,
			Metadata:          it.Metadata,
			GitServerID:       it.SourceGitServerID,
			GitRepoID:         it.SourceGitRepoID,
			GitRepoPath:       it.SourceRepoPath,
			GitEntryKey:       it.SourceGitEntryKey,
			ForkedFromItemID:  it.ForkedFromItemID,
			Status:            it.Status,
			ParentPluginID:    it.ParentPluginID,
			ParentExists:      exists,
			ParentItemType:    parent.ItemType,
			ParentSourceType:  parent.SourceType,
			ForkSourceLinked:  linked,
			FavoriteCount:     favorites[it.ID],
			DistributionCount: distributions[it.ID],
		})
	}
	return out, nil
}

// countByItem counts entitlement rows per candidate.
//
// There is deliberately no "table missing -> zero" fallback. These counts are
// the consumer-impact half of the report an operator signs off on, and a
// silently-zero column reads as "nobody is affected". A missing table is a
// broken environment and must say so. (This is not hypothetical: a first cut
// guarded on Migrator().HasTable, which resolves against current_schema() and so
// reported zero favorites for the whole fleet whenever the tool ran under a
// non-default search_path.)
func countByItem(tx *gorm.DB, table string, itemIDs []string, extraWhere string) (map[string]int, error) {
	counts := map[string]int{}
	type row struct {
		ItemID string
		N      int
	}
	err := scanInChunks(tx, itemIDs, func(chunk []string) error {
		q := tx.Table(table).Select("item_id::text AS item_id, count(*) AS n").
			Where("item_id IN ?", chunk)
		if extraWhere != "" {
			q = q.Where(extraWhere)
		}
		var got []row
		if err := q.Group("item_id").Scan(&got).Error; err != nil {
			return err
		}
		for _, r := range got {
			counts[r.ItemID] = r.N
		}
		return nil
	})
	return counts, err
}

// scanInChunks keeps every IN list well under the parameter limit, so an
// inventory of any size stays a bounded number of bounded statements.
func scanInChunks(tx *gorm.DB, ids []string, fn func([]string) error) error {
	const chunk = 500
	seen := make(map[string]struct{}, len(ids))
	unique := make([]string, 0, len(ids))
	for _, id := range ids {
		if id == "" {
			continue
		}
		if _, dup := seen[id]; dup {
			continue
		}
		seen[id] = struct{}{}
		unique = append(unique, id)
	}
	for start := 0; start < len(unique); start += chunk {
		end := start + chunk
		if end > len(unique) {
			end = len(unique)
		}
		if err := fn(unique[start:end]); err != nil {
			return err
		}
	}
	return nil
}

// setRepeatableRead is a no-op on drivers that do not support it, so the same
// code path works against the SQLite-backed unit tests. PostgreSQL is where the
// guarantee matters and where it is exercised.
func setRepeatableRead(tx *gorm.DB) error {
	if tx.Dialector == nil || tx.Dialector.Name() != "postgres" {
		return nil
	}
	return tx.Exec("SET TRANSACTION ISOLATION LEVEL REPEATABLE READ").Error
}
