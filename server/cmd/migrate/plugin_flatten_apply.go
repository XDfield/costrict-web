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
	"strconv"
	"strings"
	"time"

	"github.com/costrict/costrict-web/server/internal/models"
	"github.com/costrict/costrict-web/server/internal/services"
	migrations "github.com/costrict/costrict-web/server/migrations"
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

// pluginFlattenPlanDigest is the artifact's integrity contract.
//
// # What it covers, and why the rule is stated as an exclusion
//
// Everything except `conflict` and `rowState`. Those two are the run's OUTCOME:
// they change as apply progresses, and a digest that moved during apply could
// not be verified on resume — the moment verification matters most.
//
// The v1 digest instead listed the fields it covered, and the list was wrong.
// `contentBackend` and `sourceType` are part of the compare-and-set predicate in
// applyPluginFlattenBatch, so editing them in the stored plan (or in the
// artifact) changed which live rows apply would agree to write, while the digest
// still verified. The provenance columns — registry, catalog entry dir,
// bundled_in, the fork/parent identity — are the evidence the classification
// rests on and the evidence an operator signs off on in runbook §4, and they
// were not covered either. An enumerate-what-is-covered rule invites exactly
// that omission every time a column is added; enumerate-what-is-excluded does
// not.
//
// The counts are covered too: `favoriteCount`/`distributionCount` are the
// consumer-impact numbers the sign-off gate is built on, so understating them
// must not verify.
func pluginFlattenPlanDigest(schemaVersion int, mode string, rows []pluginFlattenPlanRow) string {
	h := sha256.New()
	fmt.Fprintf(h, "v%d\n%s\n%d\n", schemaVersion, mode, len(rows))
	ordered := make([]pluginFlattenPlanRow, len(rows))
	copy(ordered, rows)
	sort.Slice(ordered, func(i, j int) bool { return ordered[i].ItemID < ordered[j].ItemID })
	for _, r := range ordered {
		fmt.Fprintf(h,
			"%d\x1f%s\x1f%s\x1f%s\x1f%s\x1f%s\x1f%s\x1f%s\x1f%s\x1f%s\x1f%s\x1f"+
				"%s\x1f%d\x1f%s\x1f%s\x1f%s\x1f%s\x1f%t\x1f%s\x1f%s\x1f%d\x1f%d\x1f"+
				"%s\x1f%s\x1f%s\x1f%s\x1f%s\x1f%s\x1f%s\x1e",
			// Identity and batching order.
			r.Seq, r.ItemID, r.ItemType, r.ItemSlug, r.RegistryID,
			// Compare-and-set predicate columns that are NOT before_* (these two
			// are the ones v1 missed).
			r.SourceType, r.ContentBackend,
			// Provenance evidence.
			r.CatalogEntryDir, r.BundledIn, r.SourcePath, r.SourceManifestSHA,
			r.GitServerID, r.GitRepoID, r.GitRepoPath, r.GitEntryKey,
			derefOr(r.ForkedFromItemID, "<nil>"),
			derefOr(r.ParentItemID, "<nil>"), r.ParentExists, r.ParentItemType, r.ParentSourceType,
			// Consumer impact, i.e. the sign-off numbers.
			r.FavoriteCount, r.DistributionCount,
			// Before-state predicate and intended end state.
			r.BeforeStatus, derefOr(r.BeforeParentPluginID, "<nil>"),
			r.AfterStatus, derefOr(r.AfterParentPluginID, "<nil>"),
			// Verdict and its justification.
			r.Classification, r.Action, r.Reason,
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
		"state_" + flattenRowAlreadyAtTarget:      0,
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

// pluginFlattenMigrationFile is the sole definition of the two tool tables.
const pluginFlattenMigrationFile = "20260806000400_create_plugin_flatten_migration_runs.sql"

// ensurePluginFlattenTables creates the run/row tables when the goose migration
// has not run yet. Subcommands return before prepareSchema, matching how the
// existing backfills guard their own dependencies.
//
// It runs the Up block of the real migration file rather than a copy of it. The
// copy this replaces had already drifted — two indexes and every column comment
// were missing from it — which is the failure mode
// .trellis/spec/server/backend/database-guidelines.md's "one source of truth for
// schema" rule exists to prevent, and it is self-inflicted: nothing forces two
// hand-maintained texts to stay equal, so they do not. Reading the migration
// also means the PostgreSQL tests below exercise the DDL production actually
// gets, instead of certifying a second one that only tests ever see.
func ensurePluginFlattenTables(db *gorm.DB) error {
	return applyMigrationUpBlock(db, pluginFlattenMigrationFile)
}

// applyMigrationUpBlock executes one migration file's Up block statement by
// statement.
func applyMigrationUpBlock(db *gorm.DB, file string) error {
	statements, err := migrationUpStatements(file)
	if err != nil {
		return err
	}
	for _, ddl := range statements {
		if err := db.Exec(ddl).Error; err != nil {
			return fmt.Errorf("apply %s (%.60s...): %w", file, ddl, err)
		}
	}
	return nil
}

// pluginFlattenMigrationStatements extracts the Up block of the flatten
// migration. Named separately from the generic helper because the test that
// asserts the splitter's behaviour is about THIS file's contents.
func pluginFlattenMigrationStatements() ([]string, error) {
	return migrationUpStatements(pluginFlattenMigrationFile)
}

// migrationUpStatements extracts the Up block of a migration and splits it into
// individual statements.
//
// Splitting is done here rather than handing the whole block to one Exec because
// multi-statement execution depends on which wire protocol the driver picks, and
// a DDL bootstrap must not depend on that. The splitter understands the two
// things these files actually contain that hold a semicolon: `--` line comments
// and single-quoted literals (the COMMENT ON strings). It is deliberately not a
// general SQL parser — it is asserted against real files by test.
func migrationUpStatements(file string) ([]string, error) {
	raw, err := migrations.FS.ReadFile(file)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", file, err)
	}
	body := string(raw)
	start := strings.Index(body, "-- +goose Up")
	if start < 0 {
		return nil, fmt.Errorf("%s has no Up block", file)
	}
	body = body[start+len("-- +goose Up"):]
	if end := strings.Index(body, "-- +goose Down"); end >= 0 {
		body = body[:end]
	}

	var clean strings.Builder
	for _, line := range strings.Split(body, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "-- +goose") {
			continue
		}
		clean.WriteString(line)
		clean.WriteByte('\n')
	}

	statements := splitSQLStatements(clean.String())
	if len(statements) == 0 {
		return nil, fmt.Errorf("%s Up block contains no statements", file)
	}
	return statements, nil
}

// splitSQLStatements cuts on top-level semicolons, ignoring those inside `--`
// comments and single-quoted literals. Blank/comment-only fragments are dropped.
func splitSQLStatements(sql string) []string {
	var out []string
	var current strings.Builder
	inString, inComment := false, false
	runes := []rune(sql)
	for i := 0; i < len(runes); i++ {
		c := runes[i]
		switch {
		case inComment:
			if c == '\n' {
				inComment = false
				current.WriteRune(c)
			}
			continue
		case inString:
			current.WriteRune(c)
			if c == '\'' {
				// '' is an escaped quote, not the end of the literal.
				if i+1 < len(runes) && runes[i+1] == '\'' {
					current.WriteRune(runes[i+1])
					i++
					continue
				}
				inString = false
			}
			continue
		case c == '-' && i+1 < len(runes) && runes[i+1] == '-':
			inComment = true
			i++
			continue
		case c == '\'':
			inString = true
			current.WriteRune(c)
			continue
		case c == ';':
			if stmt := strings.TrimSpace(current.String()); stmt != "" {
				out = append(out, stmt)
			}
			current.Reset()
			continue
		}
		current.WriteRune(c)
	}
	if stmt := strings.TrimSpace(current.String()); stmt != "" {
		out = append(out, stmt)
	}
	return out
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
func applyPluginFlatten(db *gorm.DB, opts pluginFlattenOptions, expectedMode string, out io.Writer) error {
	if err := ensurePluginFlattenTables(db); err != nil {
		return err
	}
	run, err := loadPluginFlattenRun(db, opts.RunID)
	if err != nil {
		return err
	}
	// `apply` and `rollback-apply` share this function, and a run carries its own
	// direction, so without this check pasting a migrate run id after
	// `rollback-apply` (both ids are on the operator's screen during the same
	// operation) silently runs the migration forwards under a command that says
	// it undoes it.
	if run.Mode != expectedMode {
		return fmt.Errorf("run %s is a %s run; use %s instead",
			run.ID, run.Mode, pluginFlattenApplySubcommand(run.Mode))
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

	// The compatibility window is a property of the MIGRATION being undone, not
	// of the paperwork that undoes it. Measuring the rollback run's own age let
	// the window be walked around in two steps — rollback-plan on day 29,
	// rollback-apply on day 58 — and neither step asked for --force, while the
	// thing actually being reverted was two months old and the deprecated parent
	// link had been absent from responses for two releases.
	if run.Mode == flattenModeRollback && !opts.Force {
		windowStart := run.PlannedAt
		if run.SourceRunID != nil {
			source, err := loadPluginFlattenRun(db, *run.SourceRunID)
			if err != nil {
				return fmt.Errorf("load source run of rollback %s: %w", run.ID, err)
			}
			windowStart = source.PlannedAt
		}
		if age := time.Since(windowStart); age > pluginFlattenDefaultRollbackWindow {
			return fmt.Errorf(
				"the migration rollback run %s reverses was planned %s ago, past the %s compatibility window; pass --force to roll it back anyway",
				run.ID, age.Round(time.Hour), pluginFlattenDefaultRollbackWindow)
		}
	}

	pending := make([]pluginFlattenPlanRow, 0, len(rows))
	for _, r := range rows {
		if r.RowState == flattenRowPending && r.Action != flattenActionSkip {
			pending = append(pending, r)
		}
	}
	// An explicit --batch-size wins over the size frozen into the run at plan
	// time. The flag used to be accepted and ignored here, which is worse than
	// rejecting it: the operator reads the runbook's batch arithmetic, passes the
	// number, and gets a different one. Resolved before the dry run so the dry
	// run can report the batching that --confirm will actually use.
	batch := run.BatchSize
	if opts.BatchSizeSet && opts.BatchSize > 0 {
		batch = opts.BatchSize
	}
	if batch <= 0 {
		batch = pluginFlattenDefaultBatchSize
	}
	batchCount := (len(pending) + batch - 1) / batch

	if !opts.Confirm {
		fmt.Fprintf(out, "DRY RUN: run %s has %d pending row(s) ready to apply in %d batch(es) of %d. Re-run with --confirm.\n",
			run.ID, len(pending), batchCount, batch)
		reportPluginFlattenRows(out, rows, opts)
		return nil
	}

	if err := db.Exec(`UPDATE plugin_flatten_migration_runs
		SET status = ?, started_at = COALESCE(started_at, now()), updated_at = now()
		WHERE id = ?`, flattenRunApplying, run.ID).Error; err != nil {
		return fmt.Errorf("mark run applying: %w", err)
	}

	applied, alreadyAtTarget, skipped := 0, 0, 0
	for start := 0; start < len(pending); start += batch {
		end := start + batch
		if end > len(pending) {
			end = len(pending)
		}
		batchApplied, batchAlready, batchSkipped, err := applyPluginFlattenBatch(db, run.ID, pending[start:end])
		applied += batchApplied
		alreadyAtTarget += batchAlready
		skipped += batchSkipped
		if err != nil {
			// The committed batches stand and their rows are marked; the run
			// stays resumable by id. Reporting `partial` here rather than
			// rolling everything back is deliberate: an interrupted cleanup
			// that lies about how far it got is worse than one that stops.
			_ = finishPluginFlattenRun(db, run.ID, flattenRunPartial)
			return fmt.Errorf("apply batch starting at %d (applied=%d alreadyAtTarget=%d skipped=%d): %w",
				start, applied, alreadyAtTarget, skipped, err)
		}
	}

	final, err := recomputePluginFlattenStatus(db, run.ID, run.Mode)
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "run %s: applied=%d alreadyAtTarget=%d skipped=%d status=%s\n",
		run.ID, applied, alreadyAtTarget, skipped, final)
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
//
// # Why the Git lifecycle guard is not bypassed here
//
// This is raw SQL, so models.guardGitLifecycleStatusWrite (the only enforcement
// point for "Git may not silently reclaim a row a human hid") does not fire. It
// does not need to: a plan that archives a row also pins `content_backend` and
// `source_type` in the predicate above, and the classifier only ever produces
// `archive_and_unlink` for a row with NO Git identity at all — any Git identity,
// complete or partial, is either unlink-only or skipped. A row that acquired one
// between plan and apply fails the predicate. The reachability argument is what
// makes the missing hook safe; if the classification rules ever change, this
// comment is the thing that has to be re-checked.
func applyPluginFlattenBatch(db *gorm.DB, runID string, rows []pluginFlattenPlanRow) (int, int, int, error) {
	applied, alreadyAtTarget, skipped := 0, 0, 0
	err := db.Transaction(func(tx *gorm.DB) error {
		removedAt := time.Now()
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
				if err := recordPluginFlattenRemovalTx(tx, r, removedAt); err != nil {
					return err
				}
				if err := tx.Exec(`UPDATE plugin_flatten_migration_rows
					SET row_state = ?, conflict = '', applied_at = now()
					WHERE run_id = ? AND item_id = ?`,
					flattenRowApplied, runID, r.ItemID).Error; err != nil {
					return fmt.Errorf("mark row %s applied: %w", r.ItemID, err)
				}
				applied++
				continue
			}
			// Zero rows matched. Either the row already holds the target state or
			// somebody changed it into something else. Distinguishing the two is
			// what keeps a rerun a no-op instead of a false conflict — but it is
			// NOT the same as "this run wrote it": the data write and the marker
			// below commit in one transaction, so a row this run changed can never
			// come back here. Already-at-target is therefore always somebody
			// else's write, gets its own state, and is not rollback-eligible.
			conflict, err := describePluginFlattenConflict(tx, r)
			if err != nil {
				return err
			}
			state := flattenRowSkipped
			if conflict == "" {
				state = flattenRowAlreadyAtTarget
				alreadyAtTarget++
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
		return applied, alreadyAtTarget, skipped, err
	}
	return applied, alreadyAtTarget, skipped, nil
}

// recordPluginFlattenRemovalTx writes the per-holder tombstones for a row this
// batch just took off the shelf.
//
// # Why this exists
//
// Archiving a row removes it from the snapshot's active set, and under the
// snapshot-v2 contract absence is explicitly NOT a removal instruction: csc
// treats a missing item as a no-op, by design, because a truncated page or a
// failed request looks exactly like one. So an archive with no tombstone leaves
// the capability installed on every holder's machine forever, with nothing
// anywhere reporting a fault. That is review finding F-27, and this command was
// a second archiving writer that reintroduced it — invisibly, because today's v1
// clients still infer removal from absence, and that inference is precisely what
// this task family is removing. The bomb's fuse is the v2 gate.
//
// # Why the call is gated on the compare-and-set, not on the plan
//
// EventID rotation is the client's dedup key and must rotate on a real removal
// and never otherwise (services.RecordEntitlementRemovalTx explains what each
// mistake costs). `res.RowsAffected == 1` is the proof that this statement, in
// this transaction, moved the row — the same proof adminitem.setItemStatusTx
// gets from its own `WHERE status = 'active'`. A row that was already archived,
// or that a third party archived, does not come through here.
//
// # Why the transition test is `active -> hidden` and not `-> archived`
//
// `active` is exactly the snapshot's active set, so a row in any other status
// was already absent from every holder's entitlements and moving it ends
// nothing. models.IsCapabilityHiddenStatus on the destination is the single
// definition of "off the shelf" used by the moderation paths; asking the same
// question a different way here is how one of the two ends up not asking it.
//
// # Why the reason is package_flattened and not admin_archived
//
// This first shipped as `admin_archived`/`moderation`, because that was the only
// existing triple whose every OTHER claim was true. One claim was not: no
// moderator looked at anything. The reason travels to the device, csc logs it
// verbatim and derives the user's wording from it, so "an administrator archived
// this" points a user asking why their capability vanished at a decision nobody
// made about content nobody read. The contract's rule is that reason and effect
// must be truthful, and a legal-but-false row is the same disease as an
// internally inconsistent one, one layer up.
//
// Minting a reason is cheap precisely because the set is open by contract: the
// tombstone's presence IS the instruction and `reason` only explains it, so a
// client that does not recognise this value still removes and reports the string
// verbatim. No csc release, drain window or minimum-version gate is involved.
// The alternatives remain what they were — `git_archived` requires a Git event
// that did not happen AND is suppressed while the Git rollout flag is off, so
// the removal would be silently swallowed; `unfavorited` tells the user they did
// this themselves; `distribution_revoked` points them at a distribution that
// never existed; `item_deleted` claims a hard delete of a row that is still
// there and is rollback-restorable.
//
// # Why no rollout flag gates it
//
// `data_migration` is not a Git source, so the snapshot service's
// LifecyclePropagation kill switch — which suppresses `git_archived` only —
// leaves it alone. That is load-bearing, not incidental: the flag is OFF by
// default, so a cause it suppressed would make every archive this command
// performs invisible on every deployment that has not enabled Git, which is
// F-27 again. services.TestPackageFlattenTombstone_SurvivesTheGitLifecycleKillSwitch
// pins it.
func recordPluginFlattenRemovalTx(tx *gorm.DB, r pluginFlattenPlanRow, removedAt time.Time) error {
	if r.BeforeStatus != "active" || !models.IsCapabilityHiddenStatus(r.AfterStatus) {
		return nil
	}
	if _, err := services.RecordPackageFlattenTombstonesTx(tx, r.ItemID, removedAt); err != nil {
		return fmt.Errorf("record removal tombstones for %s: %w", r.ItemID, err)
	}
	return nil
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
	// RowsAffected, not a zero-value probe. "No row came back" and "the row came
	// back with empty columns" are different facts, and conflating them is the
	// shape .trellis/spec/server/backend/error-handling.md names as conflating
	// not-found with everything else: a row whose status somehow read empty would
	// be reported to the operator as deleted.
	res := tx.Table("capability_items").
		Select("COALESCE(status,'') AS status, parent_plugin_id::text AS parent_plugin_id, content_backend, source_type").
		Where("id = ?", r.ItemID).Scan(&live)
	if res.Error != nil {
		return "", fmt.Errorf("row %s: read live state: %w", r.ItemID, res.Error)
	}
	if res.RowsAffected == 0 {
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
// Only rows the migration actually applied are eligible — `flattenRowApplied`
// specifically, which by construction means this run's compare-and-set claimed
// the row. A row it skipped, and a row that was `already_at_target` because
// somebody else had made the same change, were never changed BY THE MIGRATION,
// so "restoring" them would write a state the migration never caused — the
// classic rollback that damages more than the thing it undoes.
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
	res := db.Table("plugin_flatten_migration_runs").
		Select(`id::text AS id, schema_version, mode, status, source_run_id::text AS source_run_id,
		        batch_size, plan_digest, totals, created_by, planned_at`).
		Where("id = ?::uuid", runID).Scan(&row)
	if res.Error != nil {
		return pluginFlattenRunRecord{}, fmt.Errorf("load run %s: %w", runID, res.Error)
	}
	if res.RowsAffected == 0 {
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
	if artifact.SchemaVersion != pluginFlattenSchemaVersion {
		return pluginFlattenArtifact{}, fmt.Errorf(
			"artifact %s has schema version %d; this binary understands %d",
			path, artifact.SchemaVersion, pluginFlattenSchemaVersion)
	}
	// Recompute rather than trust the file's own header: an edited artifact whose
	// planDigest field was left alone must not pass.
	if got := pluginFlattenPlanDigest(artifact.SchemaVersion, artifact.Mode, artifact.Rows); got != artifact.PlanDigest {
		return pluginFlattenArtifact{}, fmt.Errorf(
			"%w: artifact %s declares %s but its rows hash to %s",
			errPluginFlattenDigestMismatch, path, artifact.PlanDigest, got)
	}
	// The totals are derived, so they are checked by recomputation rather than by
	// the digest. They are also the ONLY thing runbook §4 asks a human to read
	// before approving: classification mix, action mix, and the favorite /
	// distribution impact. A file whose rows verify but whose summary understates
	// how many people are affected would pass a digest check and fail the actual
	// purpose of having one.
	if err := comparePluginFlattenTotals(artifact.Totals, pluginFlattenTotals(artifact.Rows)); err != nil {
		return pluginFlattenArtifact{}, fmt.Errorf("artifact %s: %w", path, err)
	}
	return artifact, nil
}

// comparePluginFlattenTotals reports the first counter that disagrees, naming it.
func comparePluginFlattenTotals(declared, recomputed map[string]int) error {
	keys := make([]string, 0, len(recomputed))
	for k := range recomputed {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		if declared[k] != recomputed[k] {
			return fmt.Errorf("totals disagree with its own rows: %s declared %d, rows say %d",
				k, declared[k], recomputed[k])
		}
	}
	for k, v := range declared {
		if _, known := recomputed[k]; !known {
			return fmt.Errorf("totals carry an unknown counter %s=%d", k, v)
		}
	}
	return nil
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
			n, err := parseFlattenInt(strings.TrimPrefix(arg, "--batch-size="))
			if err != nil {
				return fmt.Errorf("invalid --batch-size: %w", err)
			}
			opts.BatchSize = n
			opts.BatchSizeSet = true
		case strings.HasPrefix(arg, "--report-limit="):
			n, err := parseFlattenInt(strings.TrimPrefix(arg, "--report-limit="))
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
		return applyPluginFlatten(db, opts, flattenModeMigrate, os.Stdout)
	case "rollback-plan":
		_, err := planPluginFlattenRollback(db, opts, os.Stdout)
		return err
	case "rollback-apply":
		return applyPluginFlatten(db, opts, flattenModeRollback, os.Stdout)
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
  --force               Override the rollback compatibility window, which is
                        measured from when the MIGRATION being reverted was
                        planned. It cannot override a digest mismatch or a
                        failed compare-and-set.
  --batch-size=<n>      Rows per applying transaction (default 200). Frozen into
                        the run at plan time; passing it to apply overrides it
                        for that invocation.
  --report-limit=<n>    Per-row lines to print; -1 for all (default 20).
  --created-by=<who>    Recorded on the run.
`)
}

// parseFlattenInt is strict on purpose. fmt.Sscanf, which this replaces, stops
// at the first non-digit and reports success, so `--batch-size=200x` and
// `--report-limit=5abc` were silently accepted as 200 and 5. On a tool where one
// flag decides how much data moves per transaction, "I clearly meant something
// else" must be an error, not a truncation.
func parseFlattenInt(s string) (int, error) {
	n, err := strconv.Atoi(strings.TrimSpace(s))
	if err != nil {
		return 0, fmt.Errorf("%q is not a number", s)
	}
	return n, nil
}

// pluginFlattenApplySubcommand names the command that executes a run of the
// given mode, so a mode mismatch tells the operator what to type instead.
func pluginFlattenApplySubcommand(mode string) string {
	if mode == flattenModeRollback {
		return "rollback-apply"
	}
	return "apply"
}
