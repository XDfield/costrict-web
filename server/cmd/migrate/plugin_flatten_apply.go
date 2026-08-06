// Persistence, apply, rollback, artifact export and reporting for
// `migrate flatten-plugins`. The classification rules live in plugin_flatten.go.

package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"
)

// pluginFlattenPlanRow is one planned row. Field order and JSON names are part
// of the artifact contract: the digest is computed over this serialization, so
// renaming a field without bumping pluginFlattenSchemaVersion would invalidate
// every artifact already in an operator's hands.
type pluginFlattenPlanRow struct {
	Seq                  int64   `json:"seq"`
	ItemID               string  `json:"itemId"`
	ItemType             string  `json:"itemType"`
	ItemSlug             string  `json:"itemSlug"`
	RegistryID           string  `json:"registryId"`
	SourceType           string  `json:"sourceType"`
	ContentBackend       string  `json:"contentBackend"`
	CatalogEntryDir      string  `json:"catalogEntryDir"`
	BundledIn            string  `json:"bundledIn"`
	SourcePath           string  `json:"sourcePath"`
	SourceManifestSHA    string  `json:"sourceManifestSha"`
	GitServerID          string  `json:"gitServerId"`
	GitRepoID            int64   `json:"gitRepoId"`
	GitRepoPath          string  `json:"gitRepoPath"`
	GitEntryKey          string  `json:"gitEntryKey"`
	ForkedFromItemID     *string `json:"forkedFromItemId"`
	ParentItemID         *string `json:"parentItemId"`
	ParentExists         bool    `json:"parentExists"`
	ParentItemType       string  `json:"parentItemType"`
	ParentSourceType     string  `json:"parentSourceType"`
	FavoriteCount        int     `json:"favoriteCount"`
	DistributionCount    int     `json:"distributionCount"`
	BeforeStatus         string  `json:"beforeStatus"`
	BeforeParentPluginID *string `json:"beforeParentPluginId"`
	AfterStatus          string  `json:"afterStatus"`
	AfterParentPluginID  *string `json:"afterParentPluginId"`
	Classification       string  `json:"classification"`
	Action               string  `json:"action"`
	Reason               string  `json:"reason"`
	Conflict             string  `json:"conflict"`
	RowState             string  `json:"rowState"`
}

type pluginFlattenRunRecord struct {
	ID            string
	SchemaVersion int
	Mode          string
	Status        string
	SourceRunID   *string
	BatchSize     int
	PlanDigest    string
	Totals        map[string]int
	CreatedBy     string
	PlannedAt     time.Time
}

type pluginFlattenArtifact struct {
	SchemaVersion int                    `json:"schemaVersion"`
	RunID         string                 `json:"runId"`
	Mode          string                 `json:"mode"`
	SourceRunID   string                 `json:"sourceRunId,omitempty"`
	PlanDigest    string                 `json:"planDigest"`
	GeneratedAt   string                 `json:"generatedAt"`
	Totals        map[string]int         `json:"totals"`
	Rows          []pluginFlattenPlanRow `json:"rows"`
}

// pluginFlattenPlanDigest is the artifact's integrity contract, and it covers
// exactly the fields apply and rollback act on — identity, the compare-and-set
// predicate, and the intended end state. Mutable outcome fields (`conflict`,
// `rowState`) are excluded on purpose: they change as the run progresses, and a
// digest that moved during apply could not be verified on resume, which is the
// moment verification matters most.
func pluginFlattenPlanDigest(schemaVersion int, mode string, rows []pluginFlattenPlanRow) string {
	h := sha256.New()
	fmt.Fprintf(h, "v%d\n%s\n%d\n", schemaVersion, mode, len(rows))
	ordered := make([]pluginFlattenPlanRow, len(rows))
	copy(ordered, rows)
	sort.Slice(ordered, func(i, j int) bool { return ordered[i].ItemID < ordered[j].ItemID })
	for _, r := range ordered {
		fmt.Fprintf(h, "%s\x1f%s\x1f%s\x1f%s\x1f%s\x1f%s\x1f%s\x1f%s\x1e",
			r.ItemID,
			derefOr(r.BeforeParentPluginID, "<nil>"),
			r.BeforeStatus,
			derefOr(r.AfterParentPluginID, "<nil>"),
			r.AfterStatus,
			r.Classification,
			r.Action,
			r.SourceManifestSHA,
		)
	}
	return hex.EncodeToString(h.Sum(nil))
}

func derefOr(v *string, fallback string) string {
	if v == nil {
		return fallback
	}
	return *v
}

func pluginFlattenTotals(rows []pluginFlattenPlanRow) map[string]int {
	totals := map[string]int{
		"candidates":                              len(rows),
		"class_" + flattenClassDerivedCatalog:     0,
		"class_" + flattenClassDerivedArchive:     0,
		"class_" + flattenClassDerivedFork:        0,
		"class_" + flattenClassIndependent:        0,
		"class_" + flattenClassAmbiguous:          0,
		"action_" + flattenActionArchiveAndUnlink: 0,
		"action_" + flattenActionUnlinkOnly:       0,
		"action_" + flattenActionRestore:          0,
		"action_" + flattenActionSkip:             0,
		"state_" + flattenRowPending:              0,
		"state_" + flattenRowApplied:              0,
		"state_" + flattenRowSkipped:              0,
		"state_" + flattenRowFailed:               0,
		"favorites_on_candidates":                 0,
		"distributions_on_candidates":             0,
		// Rows the plan intended to write but apply refused. `state_skipped`
		// alone cannot be read as trouble, because it also contains every row the
		// classifier declined at plan time.
		"apply_conflicts": 0,
	}
	for _, r := range rows {
		totals["class_"+r.Classification]++
		totals["action_"+r.Action]++
		totals["state_"+r.RowState]++
		totals["favorites_on_candidates"] += r.FavoriteCount
		totals["distributions_on_candidates"] += r.DistributionCount
		if r.Conflict != "" {
			totals["apply_conflicts"]++
		}
	}
	return totals
}

// ensurePluginFlattenTables creates the run/row tables when the goose migration
// has not run yet. Subcommands return before prepareSchema, matching how the
// existing backfills guard their own dependencies; the DDL is identical to the
// 20260806000400 migration and idempotent.
func ensurePluginFlattenTables(db *gorm.DB) error {
	statements := []string{
		`CREATE TABLE IF NOT EXISTS plugin_flatten_migration_runs (
			id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
			schema_version INTEGER NOT NULL DEFAULT 1,
			mode TEXT NOT NULL,
			status TEXT NOT NULL DEFAULT 'planned',
			source_run_id UUID REFERENCES plugin_flatten_migration_runs(id),
			batch_size INTEGER NOT NULL DEFAULT 200,
			plan_digest VARCHAR(64) NOT NULL DEFAULT '',
			totals JSONB NOT NULL DEFAULT '{}'::jsonb,
			created_by TEXT NOT NULL DEFAULT '',
			notes TEXT NOT NULL DEFAULT '',
			planned_at TIMESTAMPTZ NOT NULL DEFAULT now(),
			started_at TIMESTAMPTZ,
			finished_at TIMESTAMPTZ,
			created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
			updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
			CONSTRAINT chk_plugin_flatten_runs_mode CHECK (mode IN ('migrate','rollback')),
			CONSTRAINT chk_plugin_flatten_runs_status
				CHECK (status IN ('planned','applying','applied','partial','rolled_back')),
			CONSTRAINT chk_plugin_flatten_runs_digest_format
				CHECK (plan_digest = '' OR plan_digest ~ '^[0-9a-f]{64}$'),
			CONSTRAINT chk_plugin_flatten_runs_source
				CHECK ((mode = 'rollback') = (source_run_id IS NOT NULL))
		)`,
		`CREATE INDEX IF NOT EXISTS idx_plugin_flatten_runs_status
			ON plugin_flatten_migration_runs (status, planned_at DESC)`,
		`CREATE TABLE IF NOT EXISTS plugin_flatten_migration_rows (
			run_id UUID NOT NULL REFERENCES plugin_flatten_migration_runs(id) ON DELETE CASCADE,
			seq BIGINT NOT NULL,
			item_id UUID NOT NULL,
			item_type TEXT NOT NULL DEFAULT '',
			item_slug TEXT NOT NULL DEFAULT '',
			registry_id TEXT NOT NULL DEFAULT '',
			source_type TEXT NOT NULL DEFAULT '',
			content_backend TEXT NOT NULL DEFAULT '',
			catalog_entry_dir TEXT NOT NULL DEFAULT '',
			bundled_in TEXT NOT NULL DEFAULT '',
			source_path TEXT NOT NULL DEFAULT '',
			source_manifest_sha TEXT NOT NULL DEFAULT '',
			git_server_id TEXT NOT NULL DEFAULT '',
			git_repo_id BIGINT NOT NULL DEFAULT 0,
			git_repo_path TEXT NOT NULL DEFAULT '',
			git_entry_key TEXT NOT NULL DEFAULT '',
			forked_from_item_id UUID,
			parent_item_id UUID,
			parent_exists BOOLEAN NOT NULL DEFAULT false,
			parent_item_type TEXT NOT NULL DEFAULT '',
			parent_source_type TEXT NOT NULL DEFAULT '',
			favorite_count INTEGER NOT NULL DEFAULT 0,
			distribution_count INTEGER NOT NULL DEFAULT 0,
			before_status TEXT NOT NULL,
			before_parent_plugin_id UUID,
			after_status TEXT NOT NULL,
			after_parent_plugin_id UUID,
			classification TEXT NOT NULL,
			action TEXT NOT NULL,
			reason TEXT NOT NULL DEFAULT '',
			conflict TEXT NOT NULL DEFAULT '',
			row_state TEXT NOT NULL DEFAULT 'pending',
			applied_at TIMESTAMPTZ,
			PRIMARY KEY (run_id, item_id),
			CONSTRAINT chk_plugin_flatten_rows_classification
				CHECK (classification IN ('derived_catalog','derived_archive','derived_fork','independent','ambiguous')),
			CONSTRAINT chk_plugin_flatten_rows_action
				CHECK (action IN ('archive_and_unlink','unlink_only','restore','skip')),
			CONSTRAINT chk_plugin_flatten_rows_state
				CHECK (row_state IN ('pending','applied','skipped','failed')),
			CONSTRAINT chk_plugin_flatten_rows_reason
				CHECK (action <> 'skip' OR reason <> '')
		)`,
		`CREATE UNIQUE INDEX IF NOT EXISTS uq_plugin_flatten_rows_seq
			ON plugin_flatten_migration_rows (run_id, seq)`,
		`CREATE INDEX IF NOT EXISTS idx_plugin_flatten_rows_pending
			ON plugin_flatten_migration_rows (run_id, seq) WHERE row_state = 'pending'`,
	}
	for _, ddl := range statements {
		if err := db.Exec(ddl).Error; err != nil {
			return fmt.Errorf("ensure plugin flatten tables: %w", err)
		}
	}
	return nil
}

func persistPluginFlattenRun(db *gorm.DB, run pluginFlattenRunRecord, rows []pluginFlattenPlanRow) error {
	totalsJSON, err := json.Marshal(run.Totals)
	if err != nil {
		return fmt.Errorf("encode run totals: %w", err)
	}
	return db.Transaction(func(tx *gorm.DB) error {
		if err := tx.Exec(`INSERT INTO plugin_flatten_migration_runs
			(id, schema_version, mode, status, source_run_id, batch_size, plan_digest, totals, created_by, planned_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?::jsonb, ?, now())`,
			run.ID, run.SchemaVersion, run.Mode, run.Status, run.SourceRunID,
			run.BatchSize, run.PlanDigest, string(totalsJSON), run.CreatedBy).Error; err != nil {
			return fmt.Errorf("insert run: %w", err)
		}
		const chunk = 200
		for start := 0; start < len(rows); start += chunk {
			end := start + chunk
			if end > len(rows) {
				end = len(rows)
			}
			for _, r := range rows[start:end] {
				if err := tx.Exec(`INSERT INTO plugin_flatten_migration_rows
					(run_id, seq, item_id, item_type, item_slug, registry_id, source_type, content_backend,
					 catalog_entry_dir, bundled_in, source_path, source_manifest_sha,
					 git_server_id, git_repo_id, git_repo_path, git_entry_key, forked_from_item_id,
					 parent_item_id, parent_exists, parent_item_type, parent_source_type,
					 favorite_count, distribution_count,
					 before_status, before_parent_plugin_id, after_status, after_parent_plugin_id,
					 classification, action, reason, row_state)
					VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
					run.ID, r.Seq, r.ItemID, r.ItemType, r.ItemSlug, r.RegistryID, r.SourceType, r.ContentBackend,
					r.CatalogEntryDir, r.BundledIn, r.SourcePath, r.SourceManifestSHA,
					r.GitServerID, r.GitRepoID, r.GitRepoPath, r.GitEntryKey, r.ForkedFromItemID,
					r.ParentItemID, r.ParentExists, r.ParentItemType, r.ParentSourceType,
					r.FavoriteCount, r.DistributionCount,
					r.BeforeStatus, r.BeforeParentPluginID, r.AfterStatus, r.AfterParentPluginID,
					r.Classification, r.Action, r.Reason, r.RowState).Error; err != nil {
					return fmt.Errorf("insert plan row %s: %w", r.ItemID, err)
				}
			}
		}
		return nil
	})
}

// ---------------------------------------------------------------------------
// Apply
// ---------------------------------------------------------------------------

var errPluginFlattenDigestMismatch = errors.New("plan digest does not match the stored run")

// applyPluginFlatten executes a planned run. Every write is a compare-and-set on
// the before-state the plan recorded, in bounded transactions, resumable by run
// id.
//
// Verification order matters. The digest is checked BEFORE any write, against
// the plan as it exists in the database (and, when an artifact is supplied,
// against that file too). An operator who edited the artifact, or who is
// pointing at a plan that was regenerated under them, is stopped before the
// first row moves rather than halfway through.
func applyPluginFlatten(db *gorm.DB, opts pluginFlattenOptions, out io.Writer) error {
	if err := ensurePluginFlattenTables(db); err != nil {
		return err
	}
	run, err := loadPluginFlattenRun(db, opts.RunID)
	if err != nil {
		return err
	}
	if run.SchemaVersion != pluginFlattenSchemaVersion {
		return fmt.Errorf("run %s has schema version %d; this binary understands %d",
			run.ID, run.SchemaVersion, pluginFlattenSchemaVersion)
	}
	switch run.Status {
	case flattenRunPlanned, flattenRunApplying, flattenRunPartial:
		// planned starts, applying/partial resume.
	case flattenRunApplied:
		fmt.Fprintf(out, "run %s is already applied; nothing pending\n", run.ID)
		return nil
	default:
		return fmt.Errorf("run %s is %s and cannot be applied", run.ID, run.Status)
	}

	rows, err := loadPluginFlattenRows(db, run.ID)
	if err != nil {
		return err
	}
	if got := pluginFlattenPlanDigest(run.SchemaVersion, run.Mode, rows); got != run.PlanDigest {
		return fmt.Errorf("%w: stored=%s recomputed=%s", errPluginFlattenDigestMismatch, run.PlanDigest, got)
	}
	if opts.ArtifactPath != "" {
		artifact, err := readPluginFlattenArtifact(opts.ArtifactPath)
		if err != nil {
			return err
		}
		if artifact.RunID != run.ID || artifact.PlanDigest != run.PlanDigest {
			return fmt.Errorf("%w: artifact %s describes run=%s digest=%s",
				errPluginFlattenDigestMismatch, opts.ArtifactPath, artifact.RunID, artifact.PlanDigest)
		}
	}

	if run.Mode == flattenModeRollback && !opts.Force {
		if age := time.Since(run.PlannedAt); age > pluginFlattenDefaultRollbackWindow {
			return fmt.Errorf(
				"rollback run %s was planned %s ago, past the %s compatibility window; re-plan it or pass --force",
				run.ID, age.Round(time.Hour), pluginFlattenDefaultRollbackWindow)
		}
	}

	pending := make([]pluginFlattenPlanRow, 0, len(rows))
	for _, r := range rows {
		if r.RowState == flattenRowPending && r.Action != flattenActionSkip {
			pending = append(pending, r)
		}
	}
	if !opts.Confirm {
		fmt.Fprintf(out, "DRY RUN: run %s has %d pending row(s) ready to apply. Re-run with --confirm.\n",
			run.ID, len(pending))
		reportPluginFlattenRows(out, rows, opts)
		return nil
	}

	if err := db.Exec(`UPDATE plugin_flatten_migration_runs
		SET status = ?, started_at = COALESCE(started_at, now()), updated_at = now()
		WHERE id = ?`, flattenRunApplying, run.ID).Error; err != nil {
		return fmt.Errorf("mark run applying: %w", err)
	}

	batch := run.BatchSize
	if batch <= 0 {
		batch = pluginFlattenDefaultBatchSize
	}
	applied, skipped := 0, 0
	for start := 0; start < len(pending); start += batch {
		end := start + batch
		if end > len(pending) {
			end = len(pending)
		}
		batchApplied, batchSkipped, err := applyPluginFlattenBatch(db, run.ID, pending[start:end])
		applied += batchApplied
		skipped += batchSkipped
		if err != nil {
			// The committed batches stand and their rows are marked; the run
			// stays resumable by id. Reporting `partial` here rather than
			// rolling everything back is deliberate: an interrupted cleanup
			// that lies about how far it got is worse than one that stops.
			_ = finishPluginFlattenRun(db, run.ID, flattenRunPartial)
			return fmt.Errorf("apply batch starting at %d (applied=%d skipped=%d): %w", start, applied, skipped, err)
		}
	}

	final, err := recomputePluginFlattenStatus(db, run.ID, run.Mode)
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "run %s: applied=%d skipped=%d status=%s\n", run.ID, applied, skipped, final)
	rows, err = loadPluginFlattenRows(db, run.ID)
	if err != nil {
		return err
	}
	reportPluginFlattenRows(out, rows, opts)
	return nil
}

// applyPluginFlattenBatch writes one bounded transaction.
//
// The UPDATE's WHERE clause is the entire safety argument, so it is spelled out
// rather than built: the row must still have the status, the parent link, the
// content backend and the source type the plan inventoried. `IS NOT DISTINCT
// FROM` rather than `=` because the parent link is nullable and `NULL = NULL` is
// unknown — with `=`, an already-unlinked row would silently never match and be
// reported as a conflict forever.
func applyPluginFlattenBatch(db *gorm.DB, runID string, rows []pluginFlattenPlanRow) (int, int, error) {
	applied, skipped := 0, 0
	err := db.Transaction(func(tx *gorm.DB) error {
		for _, r := range rows {
			res := tx.Exec(`UPDATE capability_items
				SET status = ?, parent_plugin_id = ?
				WHERE id = ?
				  AND status = ?
				  AND parent_plugin_id IS NOT DISTINCT FROM ?::uuid
				  AND content_backend = ?
				  AND source_type = ?`,
				r.AfterStatus, r.AfterParentPluginID,
				r.ItemID,
				r.BeforeStatus, r.BeforeParentPluginID,
				r.ContentBackend, r.SourceType)
			if res.Error != nil {
				return fmt.Errorf("row %s: %w", r.ItemID, res.Error)
			}
			if res.RowsAffected == 1 {
				if err := tx.Exec(`UPDATE plugin_flatten_migration_rows
					SET row_state = ?, conflict = '', applied_at = now()
					WHERE run_id = ? AND item_id = ?`,
					flattenRowApplied, runID, r.ItemID).Error; err != nil {
					return fmt.Errorf("mark row %s applied: %w", r.ItemID, err)
				}
				applied++
				continue
			}
			// Zero rows matched. Either the row already holds the target state
			// (a resumed run re-touching a row whose marker did not commit) or
			// somebody changed it. Distinguishing the two is what keeps a rerun
			// a no-op instead of a false conflict.
			conflict, err := describePluginFlattenConflict(tx, r)
			if err != nil {
				return err
			}
			state := flattenRowSkipped
			if conflict == "" {
				state = flattenRowApplied
				applied++
			} else {
				skipped++
			}
			if err := tx.Exec(`UPDATE plugin_flatten_migration_rows
				SET row_state = ?, conflict = ?, applied_at = now()
				WHERE run_id = ? AND item_id = ?`,
				state, conflict, runID, r.ItemID).Error; err != nil {
				return fmt.Errorf("mark row %s %s: %w", r.ItemID, state, err)
			}
		}
		return nil
	})
	if err != nil {
		return applied, skipped, err
	}
	return applied, skipped, nil
}

// describePluginFlattenConflict explains why a compare-and-set matched nothing.
// It returns "" when the live row already equals the intended end state, which
// is the idempotent-rerun case and not a conflict.
func describePluginFlattenConflict(tx *gorm.DB, r pluginFlattenPlanRow) (string, error) {
	var live struct {
		Status         string
		ParentPluginID *string
		ContentBackend string
		SourceType     string
	}
	err := tx.Table("capability_items").
		Select("COALESCE(status,'') AS status, parent_plugin_id::text AS parent_plugin_id, content_backend, source_type").
		Where("id = ?", r.ItemID).Scan(&live).Error
	if err != nil {
		return "", fmt.Errorf("row %s: read live state: %w", r.ItemID, err)
	}
	if live.Status == "" && live.ParentPluginID == nil && live.ContentBackend == "" {
		return "row no longer exists", nil
	}
	if live.Status == r.AfterStatus &&
		derefOr(live.ParentPluginID, "<nil>") == derefOr(r.AfterParentPluginID, "<nil>") {
		return "", nil
	}
	return fmt.Sprintf(
		"concurrent change: expected status=%s parent=%s backend=%s source=%s, found status=%s parent=%s backend=%s source=%s",
		r.BeforeStatus, derefOr(r.BeforeParentPluginID, "<nil>"), r.ContentBackend, r.SourceType,
		live.Status, derefOr(live.ParentPluginID, "<nil>"), live.ContentBackend, live.SourceType), nil
}

// recomputePluginFlattenStatus derives the run status from its rows rather than
// from a counter the applying loop maintained. A crash between the last batch
// commit and a status write would otherwise leave the summary disagreeing with
// the rows it summarises.
func recomputePluginFlattenStatus(db *gorm.DB, runID, mode string) (string, error) {
	rows, err := loadPluginFlattenRows(db, runID)
	if err != nil {
		return "", err
	}
	totals := pluginFlattenTotals(rows)
	status := flattenRunApplied
	if mode == flattenModeRollback {
		status = flattenRunRolledBack
	}
	if totals["state_"+flattenRowPending] > 0 || totals["state_"+flattenRowFailed] > 0 {
		status = flattenRunPartial
	}
	totalsJSON, err := json.Marshal(totals)
	if err != nil {
		return "", fmt.Errorf("encode totals: %w", err)
	}
	if err := db.Exec(`UPDATE plugin_flatten_migration_runs
		SET status = ?, totals = ?::jsonb, finished_at = now(), updated_at = now()
		WHERE id = ?`, status, string(totalsJSON), runID).Error; err != nil {
		return "", fmt.Errorf("finish run: %w", err)
	}
	return status, nil
}

func finishPluginFlattenRun(db *gorm.DB, runID, status string) error {
	return db.Exec(`UPDATE plugin_flatten_migration_runs
		SET status = ?, finished_at = now(), updated_at = now() WHERE id = ?`, status, runID).Error
}

// ---------------------------------------------------------------------------
// Rollback
// ---------------------------------------------------------------------------

// planPluginFlattenRollback turns the applied rows of a migrate run into a new
// run whose before-state is the POST-migration state and whose after-state is
// the original.
//
// Only rows the migration actually applied are eligible. A row it skipped was
// never changed, so "restoring" it would write a state the migration never
// caused — the classic rollback that damages more than the thing it undoes.
func planPluginFlattenRollback(db *gorm.DB, opts pluginFlattenOptions, out io.Writer) (string, error) {
	if err := ensurePluginFlattenTables(db); err != nil {
		return "", err
	}
	source, err := loadPluginFlattenRun(db, opts.RunID)
	if err != nil {
		return "", err
	}
	if source.Mode != flattenModeMigrate {
		return "", fmt.Errorf("run %s is a %s run; only a migrate run can be rolled back", source.ID, source.Mode)
	}
	if source.Status != flattenRunApplied && source.Status != flattenRunPartial {
		return "", fmt.Errorf("run %s is %s; only an applied or partial run has anything to roll back", source.ID, source.Status)
	}
	if !opts.Force {
		if age := time.Since(source.PlannedAt); age > pluginFlattenDefaultRollbackWindow {
			return "", fmt.Errorf(
				"run %s was planned %s ago, past the %s compatibility window; pass --force to plan a rollback anyway",
				source.ID, age.Round(time.Hour), pluginFlattenDefaultRollbackWindow)
		}
	}

	sourceRows, err := loadPluginFlattenRows(db, source.ID)
	if err != nil {
		return "", err
	}
	if got := pluginFlattenPlanDigest(source.SchemaVersion, source.Mode, sourceRows); got != source.PlanDigest {
		return "", fmt.Errorf("%w: source run %s stored=%s recomputed=%s",
			errPluginFlattenDigestMismatch, source.ID, source.PlanDigest, got)
	}

	rows := make([]pluginFlattenPlanRow, 0, len(sourceRows))
	seq := int64(0)
	for _, r := range sourceRows {
		if r.RowState != flattenRowApplied {
			continue
		}
		seq++
		inverted := r
		inverted.Seq = seq
		inverted.BeforeStatus = r.AfterStatus
		inverted.BeforeParentPluginID = r.AfterParentPluginID
		inverted.AfterStatus = r.BeforeStatus
		inverted.AfterParentPluginID = r.BeforeParentPluginID
		inverted.Action = flattenActionRestore
		inverted.Reason = "restore state inventoried by run " + source.ID
		inverted.Conflict = ""
		inverted.RowState = flattenRowPending
		rows = append(rows, inverted)
	}

	runID := uuid.NewString()
	digest := pluginFlattenPlanDigest(pluginFlattenSchemaVersion, flattenModeRollback, rows)
	totals := pluginFlattenTotals(rows)
	sourceID := source.ID
	if err := persistPluginFlattenRun(db, pluginFlattenRunRecord{
		ID:            runID,
		SchemaVersion: pluginFlattenSchemaVersion,
		Mode:          flattenModeRollback,
		Status:        flattenRunPlanned,
		SourceRunID:   &sourceID,
		BatchSize:     source.BatchSize,
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
			Mode:          flattenModeRollback,
			SourceRunID:   source.ID,
			PlanDigest:    digest,
			GeneratedAt:   time.Now().UTC().Format(time.RFC3339),
			Totals:        totals,
			Rows:          rows,
		}); err != nil {
			return "", err
		}
	}
	fmt.Fprintf(out, "rollback run %s planned for %s: %d row(s) restorable, digest %s\n",
		runID, source.ID, len(rows), digest)
	reportPluginFlattenRows(out, rows, opts)
	return runID, nil
}

// ---------------------------------------------------------------------------
// Loading, artifact IO, reporting
// ---------------------------------------------------------------------------

func loadPluginFlattenRun(db *gorm.DB, runID string) (pluginFlattenRunRecord, error) {
	if strings.TrimSpace(runID) == "" {
		return pluginFlattenRunRecord{}, errors.New("missing --run=<uuid>")
	}
	var row struct {
		ID            string
		SchemaVersion int
		Mode          string
		Status        string
		SourceRunID   *string
		BatchSize     int
		PlanDigest    string
		Totals        []byte
		CreatedBy     string
		PlannedAt     time.Time
	}
	err := db.Table("plugin_flatten_migration_runs").
		Select(`id::text AS id, schema_version, mode, status, source_run_id::text AS source_run_id,
		        batch_size, plan_digest, totals, created_by, planned_at`).
		Where("id = ?::uuid", runID).Scan(&row).Error
	if err != nil {
		return pluginFlattenRunRecord{}, fmt.Errorf("load run %s: %w", runID, err)
	}
	if row.ID == "" {
		return pluginFlattenRunRecord{}, fmt.Errorf("run %s not found", runID)
	}
	totals := map[string]int{}
	if len(row.Totals) > 0 {
		_ = json.Unmarshal(row.Totals, &totals)
	}
	return pluginFlattenRunRecord{
		ID: row.ID, SchemaVersion: row.SchemaVersion, Mode: row.Mode, Status: row.Status,
		SourceRunID: row.SourceRunID, BatchSize: row.BatchSize, PlanDigest: row.PlanDigest,
		Totals: totals, CreatedBy: row.CreatedBy, PlannedAt: row.PlannedAt,
	}, nil
}

func loadPluginFlattenRows(db *gorm.DB, runID string) ([]pluginFlattenPlanRow, error) {
	var rows []pluginFlattenPlanRow
	err := db.Table("plugin_flatten_migration_rows").
		Select(`seq, item_id::text AS item_id, item_type, item_slug, registry_id, source_type, content_backend,
		        catalog_entry_dir, bundled_in, source_path, source_manifest_sha,
		        git_server_id, git_repo_id, git_repo_path, git_entry_key,
		        forked_from_item_id::text AS forked_from_item_id,
		        parent_item_id::text AS parent_item_id, parent_exists, parent_item_type, parent_source_type,
		        favorite_count, distribution_count,
		        before_status, before_parent_plugin_id::text AS before_parent_plugin_id,
		        after_status, after_parent_plugin_id::text AS after_parent_plugin_id,
		        classification, action, reason, conflict, row_state`).
		Where("run_id = ?::uuid", runID).
		Order("seq ASC").
		Scan(&rows).Error
	if err != nil {
		return nil, fmt.Errorf("load plan rows for run %s: %w", runID, err)
	}
	return rows, nil
}

func writePluginFlattenArtifact(path string, artifact pluginFlattenArtifact) error {
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return fmt.Errorf("create artifact directory: %w", err)
		}
	}
	encoded, err := json.MarshalIndent(artifact, "", "  ")
	if err != nil {
		return fmt.Errorf("encode artifact: %w", err)
	}
	if err := os.WriteFile(path, append(encoded, '\n'), 0o644); err != nil {
		return fmt.Errorf("write artifact: %w", err)
	}
	return nil
}

func readPluginFlattenArtifact(path string) (pluginFlattenArtifact, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return pluginFlattenArtifact{}, fmt.Errorf("read artifact: %w", err)
	}
	var artifact pluginFlattenArtifact
	if err := json.Unmarshal(raw, &artifact); err != nil {
		return pluginFlattenArtifact{}, fmt.Errorf("decode artifact: %w", err)
	}
	// Recompute rather than trust the file's own header: an edited artifact whose
	// planDigest field was left alone must not pass.
	if got := pluginFlattenPlanDigest(artifact.SchemaVersion, artifact.Mode, artifact.Rows); got != artifact.PlanDigest {
		return pluginFlattenArtifact{}, fmt.Errorf(
			"%w: artifact %s declares %s but its rows hash to %s",
			errPluginFlattenDigestMismatch, path, artifact.PlanDigest, got)
	}
	return artifact, nil
}

func reportPluginFlattenPlan(out io.Writer, runID, digest string, totals map[string]int, rows []pluginFlattenPlanRow, opts pluginFlattenOptions) {
	fmt.Fprintf(out, "plugin flatten plan %s (schema v%d)\n", runID, pluginFlattenSchemaVersion)
	fmt.Fprintf(out, "  digest      %s\n", digest)
	fmt.Fprintf(out, "  candidates  %d\n", totals["candidates"])
	for _, class := range []string{
		flattenClassDerivedCatalog, flattenClassDerivedArchive, flattenClassDerivedFork,
		flattenClassIndependent, flattenClassAmbiguous,
	} {
		fmt.Fprintf(out, "    %-16s %d\n", class, totals["class_"+class])
	}
	for _, action := range []string{flattenActionArchiveAndUnlink, flattenActionUnlinkOnly, flattenActionSkip} {
		fmt.Fprintf(out, "  action %-19s %d\n", action, totals["action_"+action])
	}
	fmt.Fprintf(out, "  favorites on candidates      %d\n", totals["favorites_on_candidates"])
	fmt.Fprintf(out, "  distributions on candidates  %d\n", totals["distributions_on_candidates"])
	reportPluginFlattenRows(out, rows, opts)
}

func reportPluginFlattenRows(out io.Writer, rows []pluginFlattenPlanRow, opts pluginFlattenOptions) {
	// 0 means none and -1 means all; the default lives in the flag parser, so an
	// operator who asks for 0 lines gets 0 lines rather than the default.
	limit := opts.ReportLimit
	if limit < 0 {
		limit = len(rows)
	}
	shown := 0
	for _, r := range rows {
		if shown >= limit {
			fmt.Fprintf(out, "  ... %d more row(s); export the artifact for the complete plan\n", len(rows)-shown)
			return
		}
		fmt.Fprintf(out, "  %s %-16s %-18s %s->%s parent %s->%s fav=%d dist=%d %s%s\n",
			r.ItemID, r.Classification, r.Action,
			r.BeforeStatus, r.AfterStatus,
			derefOr(r.BeforeParentPluginID, "-"), derefOr(r.AfterParentPluginID, "-"),
			r.FavoriteCount, r.DistributionCount, r.Reason, conflictSuffix(r.Conflict))
		shown++
	}
}

func conflictSuffix(conflict string) string {
	if conflict == "" {
		return ""
	}
	return " | CONFLICT: " + conflict
}

func statusPluginFlatten(db *gorm.DB, opts pluginFlattenOptions, out io.Writer) error {
	if err := ensurePluginFlattenTables(db); err != nil {
		return err
	}
	if strings.TrimSpace(opts.RunID) == "" {
		type runRow struct {
			ID        string
			Mode      string
			Status    string
			PlannedAt time.Time
			CreatedBy string
		}
		var runs []runRow
		if err := db.Table("plugin_flatten_migration_runs").
			Select("id::text AS id, mode, status, planned_at, created_by").
			Order("planned_at DESC").Limit(20).Scan(&runs).Error; err != nil {
			return fmt.Errorf("list runs: %w", err)
		}
		if len(runs) == 0 {
			fmt.Fprintln(out, "no plugin flatten runs recorded")
			return nil
		}
		for _, r := range runs {
			fmt.Fprintf(out, "%s  %-8s %-11s %s  %s\n", r.ID, r.Mode, r.Status,
				r.PlannedAt.UTC().Format(time.RFC3339), r.CreatedBy)
		}
		return nil
	}
	run, err := loadPluginFlattenRun(db, opts.RunID)
	if err != nil {
		return err
	}
	rows, err := loadPluginFlattenRows(db, run.ID)
	if err != nil {
		return err
	}
	totals := pluginFlattenTotals(rows)
	fmt.Fprintf(out, "run %s mode=%s status=%s digest=%s planned=%s\n",
		run.ID, run.Mode, run.Status, run.PlanDigest, run.PlannedAt.UTC().Format(time.RFC3339))
	keys := make([]string, 0, len(totals))
	for k := range totals {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		fmt.Fprintf(out, "  %-32s %d\n", k, totals[k])
	}
	reportPluginFlattenRows(out, rows, opts)
	return nil
}

// runPluginFlattenCommand parses `migrate flatten-plugins <subcommand> [flags]`.
func runPluginFlattenCommand(db *gorm.DB, args []string) error {
	if len(args) == 0 {
		printPluginFlattenHelp(os.Stdout)
		return errors.New("missing subcommand")
	}
	sub := args[0]
	opts := pluginFlattenOptions{
		BatchSize:   pluginFlattenDefaultBatchSize,
		ReportLimit: pluginFlattenDefaultReportLimit,
	}
	for _, arg := range args[1:] {
		switch {
		case arg == "--confirm":
			opts.Confirm = true
		case arg == "--force":
			opts.Force = true
		case strings.HasPrefix(arg, "--run="):
			opts.RunID = strings.TrimPrefix(arg, "--run=")
		case strings.HasPrefix(arg, "--artifact="):
			opts.ArtifactPath = strings.TrimPrefix(arg, "--artifact=")
		case strings.HasPrefix(arg, "--created-by="):
			opts.CreatedBy = strings.TrimPrefix(arg, "--created-by=")
		case strings.HasPrefix(arg, "--batch-size="):
			n, err := strconvAtoi(strings.TrimPrefix(arg, "--batch-size="))
			if err != nil {
				return fmt.Errorf("invalid --batch-size: %w", err)
			}
			opts.BatchSize = n
		case strings.HasPrefix(arg, "--report-limit="):
			n, err := strconvAtoi(strings.TrimPrefix(arg, "--report-limit="))
			if err != nil {
				return fmt.Errorf("invalid --report-limit: %w", err)
			}
			opts.ReportLimit = n
		default:
			return fmt.Errorf("unknown flag %q", arg)
		}
	}

	switch sub {
	case "plan":
		_, err := planPluginFlatten(db, opts, os.Stdout)
		return err
	case "apply":
		return applyPluginFlatten(db, opts, os.Stdout)
	case "rollback-plan":
		_, err := planPluginFlattenRollback(db, opts, os.Stdout)
		return err
	case "rollback-apply":
		return applyPluginFlatten(db, opts, os.Stdout)
	case "status":
		return statusPluginFlatten(db, opts, os.Stdout)
	case "help", "-h", "--help":
		printPluginFlattenHelp(os.Stdout)
		return nil
	default:
		printPluginFlattenHelp(os.Stdout)
		return fmt.Errorf("unknown subcommand %q", sub)
	}
}

func printPluginFlattenHelp(out io.Writer) {
	fmt.Fprint(out, `migrate flatten-plugins <subcommand> [flags]

Retire package-derived Plugin child rows (capability_items.parent_plugin_id)
under the flat capability model. Nothing is ever hard-deleted, and no Git
repository is touched.

Subcommands
  plan            Inventory and classify every parent-linked row; writes no
                  capability_items rows. Persists the plan and prints a report.
  apply           Execute a planned run with compare-and-set per row. Dry-run
                  unless --confirm. Resumable by run id after a crash.
  rollback-plan   Plan the inverse of an applied run's applied rows.
  rollback-apply  Execute a rollback run (same flags as apply).
  status          Show one run, or the 20 most recent runs.

Flags
  --run=<uuid>          Run to act on (required by everything except plan).
  --artifact=<path>     Write the checksummed plan (plan/rollback-plan), or
                        verify it against the stored run (apply).
  --confirm             Actually write. Absent, apply is a dry run.
  --force               Override the rollback compatibility window. It cannot
                        override a digest mismatch or a failed compare-and-set.
  --batch-size=<n>      Rows per applying transaction (default 200).
  --report-limit=<n>    Per-row lines to print; -1 for all (default 20).
  --created-by=<who>    Recorded on the run.
`)
}

func strconvAtoi(s string) (int, error) {
	var n int
	if _, err := fmt.Sscanf(strings.TrimSpace(s), "%d", &n); err != nil {
		return 0, fmt.Errorf("%q is not a number", s)
	}
	return n, nil
}
