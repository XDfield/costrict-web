package main

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/costrict/costrict-web/server/internal/models"
	"gorm.io/gorm"
)

// Round-6 findings, one test per finding. Same PostgreSQL-only construction as
// plugin_flatten_postgres_test.go: every assertion below is about a
// compare-and-set, a CHECK constraint or an ON CONFLICT, none of which SQLite has.

const (
	fxHolderFavorite = "user-favorite"
	fxHolderReceipt  = "user-receipt"
	fxHolderBoth     = "user-both"
)

func seedFavorite(t *testing.T, db *gorm.DB, itemID, userID string) {
	t.Helper()
	if err := db.Exec(`INSERT INTO item_favorites (item_id, user_id) VALUES (?::uuid, ?)`,
		itemID, userID).Error; err != nil {
		t.Fatalf("seed favorite %s/%s: %v", itemID, userID, err)
	}
}

// seedLiveDistribution creates an active distribution of itemID with one
// undismissed receipt per user — the other half of "who currently holds this".
func seedLiveDistribution(t *testing.T, db *gorm.DB, itemID string, userIDs ...string) {
	t.Helper()
	var distID string
	if err := db.Raw(`INSERT INTO item_distributions (item_id, status) VALUES (?::uuid, 'active')
		RETURNING id::text`, itemID).Scan(&distID).Error; err != nil {
		t.Fatalf("seed distribution for %s: %v", itemID, err)
	}
	for _, userID := range userIDs {
		if err := db.Exec(`INSERT INTO item_distribution_receipts (distribution_id, user_id, receipt_status)
			VALUES (?::uuid, ?, 'unread')`, distID, userID).Error; err != nil {
			t.Fatalf("seed receipt %s/%s: %v", itemID, userID, err)
		}
	}
}

// R6-1 (P1) — the finding this file exists for.
//
// `apply` archives rows with raw SQL, which takes it around
// adminitem.setItemStatusTx and therefore around the tombstone write that path
// added for F-27. An archived row leaves the snapshot's active set, and under
// snapshot v2 absence is explicitly not a removal instruction, so without a
// tombstone every holder keeps the capability installed forever with nothing
// reporting a fault. Today's v1 clients still infer removal from absence — which
// is the rule this task family is deleting, so the defect is invisible until the
// v2 gate opens.
//
// The test pins all four halves: archived rows tombstone every holder, the
// holder set is a union (one row for someone who both favorited and received),
// rows that were not archived tombstone nobody, and a row whose compare-and-set
// lost tombstones nobody either.
func TestPluginFlatten_ApplyTombstonesEveryHolderOfARowItArchived(t *testing.T) {
	db := newPluginFlattenPostgresDB(t)
	seedFlattenWorld(t, db)

	// Archived by the migration, with holders of every shape.
	seedFavorite(t, db, fxCatalog1, fxHolderFavorite)
	seedFavorite(t, db, fxCatalog1, fxHolderBoth)
	seedLiveDistribution(t, db, fxCatalog1, fxHolderReceipt, fxHolderBoth)
	// Unlinked but NOT archived: it stays on the shelf, so nothing was removed.
	seedFavorite(t, db, fxIndependent, fxHolderFavorite)
	// Skipped by the classifier: never touched at all.
	seedFavorite(t, db, fxBanned, fxHolderFavorite)
	// Archived by the plan, but a third party moves it between plan and apply so
	// the compare-and-set finds nothing. No transition, no tombstone.
	seedFavorite(t, db, fxArchive, fxHolderFavorite)

	runID, _ := planFor(t, db, "")
	if err := db.Exec(`UPDATE capability_items SET status = 'inactive' WHERE id = ?::uuid`, fxArchive).Error; err != nil {
		t.Fatalf("concurrent change: %v", err)
	}
	if err := applyPluginFlatten(db, pluginFlattenOptions{RunID: runID, Confirm: true, ReportLimit: 0},
		flattenModeMigrate, io.Discard); err != nil {
		t.Fatalf("apply: %v", err)
	}

	archived := tombstonesFor(t, db, fxCatalog1)
	if len(archived) != 3 {
		t.Fatalf("archived row tombstoned %d holder(s), want 3: %+v", len(archived), archived)
	}
	for _, user := range []string{fxHolderFavorite, fxHolderReceipt, fxHolderBoth} {
		got, ok := archived[user]
		if !ok {
			t.Errorf("holder %s of an archived row received no removal instruction", user)
			continue
		}
		// The cause has to be one the production CHECK accepts, one that is not
		// suppressed by the Git rollout flag, and one that is TRUE — nobody
		// moderated anything here. See recordPluginFlattenRemovalTx.
		if got[0] != models.SyncTombstoneReasonPackageFlattened ||
			got[1] != models.SyncTombstoneSourceDataMigration {
			t.Errorf("holder %s tombstoned as %s/%s, want package_flattened/data_migration",
				user, got[0], got[1])
		}
	}

	for _, tc := range []struct{ item, why string }{
		{fxIndependent, "unlink_only leaves the row active, so nothing was removed"},
		{fxBanned, "the classifier skipped it, so apply never touched it"},
		{fxArchive, "its compare-and-set matched nothing, so this run did not archive it"},
	} {
		if got := tombstonesFor(t, db, tc.item); len(got) != 0 {
			t.Errorf("%s produced %d tombstone(s): %s", tc.item, len(got), tc.why)
		}
	}

	// SD-3 again, now against the tombstone path: writing a removal record must
	// not have deleted the relationship it describes.
	var favorites, receipts int64
	db.Table("item_favorites").Where("item_id = ?::uuid", fxCatalog1).Count(&favorites)
	db.Table("item_distribution_receipts").Count(&receipts)
	if favorites != 2 || receipts != 2 {
		t.Errorf("relationships were removed: favorites=%d receipts=%d", favorites, receipts)
	}
}

// The tombstone cause migration is a HARD precondition of apply, and it was
// enforced only by two paragraphs of runbook prose. Without it, apply discovers
// the problem in the worst available way: the tombstone is written in the same
// transaction that archives the row, so the first archived row with a holder
// fails the CHECK, its batch rolls back, and the run is left `partial` with
// earlier batches committed — an outcome that needs a rollback run to clean up
// and is strictly worse than never having started.
//
// This pins both halves of the gate on ONE database, so it also proves the gate
// is a precondition and not a permanent refusal: refused while the migration is
// missing, and the identical command succeeds once it has run.
func TestPluginFlatten_ApplyRefusesADatabaseMissingTheTombstoneCauseMigration(t *testing.T) {
	stale := pluginFlattenTombstoneMigrations[:len(pluginFlattenTombstoneMigrations)-1]
	db := newPluginFlattenPostgresDBAt(t, stale)
	seedFlattenWorld(t, db)
	seedFavorite(t, db, fxCatalog1, fxHolderFavorite)
	seedLiveDistribution(t, db, fxCatalog2, fxHolderReceipt)

	// `plan` is deliberately NOT gated: it writes no capability_items row and so
	// no tombstone, and an operator may legitimately build a plan before the
	// schema catches up. If this ever starts failing, the gate has been put in
	// the wrong place.
	runID, _ := planFor(t, db, "")

	before := flattenItemStates(t, db)

	// The dry run is refused too. Its whole job is to say what --confirm will do.
	dryRunErr := applyPluginFlatten(db, pluginFlattenOptions{RunID: runID, ReportLimit: 0},
		flattenModeMigrate, io.Discard)
	assertFlattenCausePreconditionError(t, "dry run", dryRunErr)

	confirmErr := applyPluginFlatten(db, pluginFlattenOptions{RunID: runID, Confirm: true, ReportLimit: 0},
		flattenModeMigrate, io.Discard)
	assertFlattenCausePreconditionError(t, "confirmed apply", confirmErr)

	// Zero data written: not one capability_items row moved, and not one
	// tombstone exists. A gate that refuses after the first batch would be no
	// better than the CHECK it is standing in front of.
	if after := flattenItemStates(t, db); !reflect.DeepEqual(before, after) {
		t.Fatalf("a refused apply changed capability_items:\n before %v\n after  %v", before, after)
	}
	var tombstones int64
	if err := db.Table("capability_sync_tombstones").Count(&tombstones).Error; err != nil {
		t.Fatalf("count tombstones: %v", err)
	}
	if tombstones != 0 {
		t.Fatalf("a refused apply wrote %d tombstone(s)", tombstones)
	}
	// And the run is untouched, so it is not left looking half-executed.
	run, err := loadPluginFlattenRun(db, runID)
	if err != nil {
		t.Fatalf("load run: %v", err)
	}
	if run.Status != flattenRunPlanned {
		t.Fatalf("refused run status = %s, want %s", run.Status, flattenRunPlanned)
	}
	for _, r := range mustLoadFlattenRows(t, db, runID) {
		if r.Action != flattenActionSkip && r.RowState != flattenRowPending {
			t.Errorf("row %s is %s after a refused apply, want pending", r.ItemID, r.RowState)
		}
	}

	// Now run the missing migration on the same database — the fix the error
	// message tells the operator to apply — and re-run the identical command.
	missing := pluginFlattenTombstoneMigrations[len(pluginFlattenTombstoneMigrations)-1]
	if err := applyMigrationUpBlock(db, missing); err != nil {
		t.Fatalf("apply %s: %v", missing, err)
	}
	if err := applyPluginFlatten(db, pluginFlattenOptions{RunID: runID, Confirm: true, ReportLimit: 0},
		flattenModeMigrate, io.Discard); err != nil {
		t.Fatalf("apply after the migration landed: %v", err)
	}
	for _, id := range []string{fxCatalog1, fxCatalog2, fxArchive, fxForkSource, fxFork} {
		if status, parent := liveItem(t, db, id); status != "archived" || parent != nil {
			t.Errorf("row %s did not converge: status=%s parent=%v", id, status, parent)
		}
	}
	for _, tc := range []struct{ item, holder string }{
		{fxCatalog1, fxHolderFavorite},
		{fxCatalog2, fxHolderReceipt},
	} {
		got, ok := tombstonesFor(t, db, tc.item)[tc.holder]
		if !ok {
			t.Fatalf("holder %s of %s received no removal instruction", tc.holder, tc.item)
		}
		if got[0] != models.SyncTombstoneReasonPackageFlattened ||
			got[1] != models.SyncTombstoneSourceDataMigration {
			t.Errorf("holder %s tombstoned as %s/%s", tc.holder, got[0], got[1])
		}
	}
}

// assertFlattenCausePreconditionError pins what the refusal has to TELL the
// operator, not merely that it refused. The message is the whole point of the
// gate: it has to name the migration by id and say how to run it, or the
// operator is back to reading a handbook to decode an error.
func assertFlattenCausePreconditionError(t *testing.T, what string, err error) {
	t.Helper()
	if err == nil {
		t.Fatalf("%s ran against a database missing %s", what, pluginFlattenTombstoneCauseMigration)
	}
	for _, want := range []string{
		pluginFlattenTombstoneCauseMigration,
		pluginFlattenTombstoneCauseCheck,
		models.SyncTombstoneReasonPackageFlattened,
		models.SyncTombstoneSourceDataMigration,
		"go run ./cmd/migrate",
		"nothing has been written",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("%s refusal does not mention %q:\n%v", what, want, err)
		}
	}
}

// flattenItemStates is every capability_items row's mutable state, for
// before/after comparison.
func flattenItemStates(t *testing.T, db *gorm.DB) map[string]string {
	t.Helper()
	var rows []struct {
		ID             string
		Status         string
		ParentPluginID *string
	}
	if err := db.Table("capability_items").
		Select("id::text AS id, status, parent_plugin_id::text AS parent_plugin_id").
		Scan(&rows).Error; err != nil {
		t.Fatalf("snapshot capability_items: %v", err)
	}
	out := make(map[string]string, len(rows))
	for _, r := range rows {
		out[r.ID] = r.Status + "/" + derefOr(r.ParentPluginID, "<nil>")
	}
	return out
}

func mustLoadFlattenRows(t *testing.T, db *gorm.DB, runID string) []pluginFlattenPlanRow {
	t.Helper()
	rows, err := loadPluginFlattenRows(db, runID)
	if err != nil {
		t.Fatalf("load rows: %v", err)
	}
	return rows
}

// The event id is the client's dedup key: it must rotate on a real removal and
// never otherwise. A rerun of an applied run performs no transition, so nothing
// may rotate — otherwise every rerun makes each device re-run removal work.
func TestPluginFlatten_RerunDoesNotRotateTombstoneEventIDs(t *testing.T) {
	db := newPluginFlattenPostgresDB(t)
	seedFlattenWorld(t, db)
	seedFavorite(t, db, fxCatalog1, fxHolderFavorite)
	runID, _ := planFor(t, db, "")

	apply := func(what string) {
		t.Helper()
		if err := applyPluginFlatten(db, pluginFlattenOptions{RunID: runID, Confirm: true, ReportLimit: 0},
			flattenModeMigrate, io.Discard); err != nil {
			t.Fatalf("%s: %v", what, err)
		}
	}
	eventID := func() string {
		t.Helper()
		var id string
		if err := db.Table("capability_sync_tombstones").
			Where("item_id = ?::uuid AND user_id = ?", fxCatalog1, fxHolderFavorite).
			Pluck("event_id", &id).Error; err != nil {
			t.Fatalf("read event id: %v", err)
		}
		return id
	}

	apply("first apply")
	first := eventID()
	if first == "" {
		t.Fatal("no tombstone was written")
	}
	apply("rerun")
	if got := eventID(); got != first {
		t.Fatalf("rerun rotated the dedup key: %s -> %s", first, got)
	}
}

// Rollback restores the item, and an active item supersedes its own older
// tombstone when the snapshot is built. Deleting the tombstone instead would
// erase the instruction a device that has not polled yet still needs, and it
// would do so on the strength of a decision that may itself be reverted.
func TestPluginFlatten_RollbackRestoresTheItemAndKeepsItsTombstone(t *testing.T) {
	db := newPluginFlattenPostgresDB(t)
	seedFlattenWorld(t, db)
	seedFavorite(t, db, fxCatalog1, fxHolderFavorite)
	runID, _ := planFor(t, db, "")
	if err := applyPluginFlatten(db, pluginFlattenOptions{RunID: runID, Confirm: true, ReportLimit: 0},
		flattenModeMigrate, io.Discard); err != nil {
		t.Fatalf("apply: %v", err)
	}
	rollbackID, err := planPluginFlattenRollback(db, pluginFlattenOptions{
		RunID: runID, ReportLimit: 0, CreatedBy: "test",
	}, io.Discard)
	if err != nil {
		t.Fatalf("rollback-plan: %v", err)
	}
	if err := applyPluginFlatten(db, pluginFlattenOptions{RunID: rollbackID, Confirm: true, ReportLimit: 0},
		flattenModeRollback, io.Discard); err != nil {
		t.Fatalf("rollback-apply: %v", err)
	}

	if status, parent := liveItem(t, db, fxCatalog1); status != "active" || derefOr(parent, "") != fxPlugin {
		t.Fatalf("rollback did not restore the row: status=%s parent=%v", status, parent)
	}
	if got := tombstonesFor(t, db, fxCatalog1); len(got) != 1 {
		t.Fatalf("rollback deleted the tombstone (%d left); an active item supersedes it instead", len(got))
	}
	// Restoring to active is not a removal, so it must not have minted new ones.
	var total int64
	db.Table("capability_sync_tombstones").Count(&total)
	if total != 1 {
		t.Fatalf("rollback wrote %d tombstones; restoring an item removes nothing", total)
	}
}

// R6-4 — "the live row already holds the target state" is not the same fact as
// "this run wrote it". The data write and the row marker commit in ONE
// transaction, so a row this run changed can never come back as already-at-
// target; it is always somebody else's write. Recording it as `applied` made
// rollback revert a change the migration never made.
func TestPluginFlatten_AlreadyAtTargetIsNotRollbackEligible(t *testing.T) {
	db := newPluginFlattenPostgresDB(t)
	seedFlattenWorld(t, db)
	runID, _ := planFor(t, db, "")

	// A third party performs exactly the change the plan intended.
	if err := db.Exec(`UPDATE capability_items SET status = 'archived', parent_plugin_id = NULL
		WHERE id = ?::uuid`, fxCatalog1).Error; err != nil {
		t.Fatalf("third-party change: %v", err)
	}
	if err := applyPluginFlatten(db, pluginFlattenOptions{RunID: runID, Confirm: true, ReportLimit: 0},
		flattenModeMigrate, io.Discard); err != nil {
		t.Fatalf("apply: %v", err)
	}

	rows, err := loadPluginFlattenRows(db, runID)
	if err != nil {
		t.Fatalf("load rows: %v", err)
	}
	byItem := rowsByItem(rows)
	if got := byItem[fxCatalog1]; got.RowState != flattenRowAlreadyAtTarget {
		t.Fatalf("row state = %s, want %s", got.RowState, flattenRowAlreadyAtTarget)
	}
	if got := byItem[fxCatalog2]; got.RowState != flattenRowApplied {
		t.Fatalf("a row this run really wrote is %s, want applied", got.RowState)
	}
	// It is a completed outcome, not outstanding work.
	run, err := loadPluginFlattenRun(db, runID)
	if err != nil {
		t.Fatalf("load run: %v", err)
	}
	if run.Status != flattenRunApplied {
		t.Fatalf("run status = %s, want applied", run.Status)
	}
	// And no tombstone: this run archived nothing for that item.
	if got := tombstonesFor(t, db, fxCatalog1); len(got) != 0 {
		t.Errorf("already-at-target row produced %d tombstone(s)", len(got))
	}

	rollbackID, err := planPluginFlattenRollback(db, pluginFlattenOptions{
		RunID: runID, ReportLimit: 0, CreatedBy: "test",
	}, io.Discard)
	if err != nil {
		t.Fatalf("rollback-plan: %v", err)
	}
	rollbackRows, err := loadPluginFlattenRows(db, rollbackID)
	if err != nil {
		t.Fatalf("load rollback rows: %v", err)
	}
	if _, present := rowsByItem(rollbackRows)[fxCatalog1]; present {
		t.Fatal("rollback plans to revert a change the migration never made")
	}
}

// R6-6 — apply and rollback-apply are the same function and a run carries its
// own direction. Both ids are on the operator's screen during one operation, so
// pasting the wrong one must fail rather than run the migration forwards under
// a command whose name says it undoes it.
func TestPluginFlatten_ApplyRefusesARunOfTheOtherMode(t *testing.T) {
	db := newPluginFlattenPostgresDB(t)
	seedFlattenWorld(t, db)
	runID, _ := planFor(t, db, "")

	err := applyPluginFlatten(db, pluginFlattenOptions{RunID: runID, Confirm: true, ReportLimit: 0},
		flattenModeRollback, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "is a migrate run") {
		t.Fatalf("rollback-apply accepted a migrate run: %v", err)
	}
	if status, parent := liveItem(t, db, fxCatalog1); status != "active" || parent == nil {
		t.Fatalf("a refused apply still wrote: status=%s parent=%v", status, parent)
	}

	if err := applyPluginFlatten(db, pluginFlattenOptions{RunID: runID, Confirm: true, ReportLimit: 0},
		flattenModeMigrate, io.Discard); err != nil {
		t.Fatalf("apply: %v", err)
	}
	rollbackID, err := planPluginFlattenRollback(db, pluginFlattenOptions{
		RunID: runID, ReportLimit: 0, CreatedBy: "test",
	}, io.Discard)
	if err != nil {
		t.Fatalf("rollback-plan: %v", err)
	}
	err = applyPluginFlatten(db, pluginFlattenOptions{RunID: rollbackID, Confirm: true, ReportLimit: 0},
		flattenModeMigrate, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "is a rollback run") {
		t.Fatalf("apply accepted a rollback run: %v", err)
	}
}

// R6-7 / codex P1-B — the compatibility window belongs to the MIGRATION being
// undone. Measuring the rollback run's own age let it be walked around in two
// steps: rollback-plan on day 29, rollback-apply on day 58, neither asking for
// --force, while the thing being reverted was two months old.
func TestPluginFlatten_RollbackWindowMeasuresTheMigrationBeingReverted(t *testing.T) {
	db := newPluginFlattenPostgresDB(t)
	seedFlattenWorld(t, db)
	runID, _ := planFor(t, db, "")
	if err := applyPluginFlatten(db, pluginFlattenOptions{RunID: runID, Confirm: true, ReportLimit: 0},
		flattenModeMigrate, io.Discard); err != nil {
		t.Fatalf("apply: %v", err)
	}
	// Day 29: inside the window, so the rollback plans without --force.
	if err := db.Exec(`UPDATE plugin_flatten_migration_runs SET planned_at = now() - interval '29 days'
		WHERE id = ?::uuid`, runID).Error; err != nil {
		t.Fatalf("age the migrate run: %v", err)
	}
	rollbackID, err := planPluginFlattenRollback(db, pluginFlattenOptions{
		RunID: runID, ReportLimit: 0, CreatedBy: "test",
	}, io.Discard)
	if err != nil {
		t.Fatalf("rollback-plan at day 29: %v", err)
	}

	// Day 58 for the migration; the rollback plan itself is still brand new.
	if err := db.Exec(`UPDATE plugin_flatten_migration_runs SET planned_at = now() - interval '58 days'
		WHERE id = ?::uuid`, runID).Error; err != nil {
		t.Fatalf("re-age the migrate run: %v", err)
	}
	err = applyPluginFlatten(db, pluginFlattenOptions{RunID: rollbackID, Confirm: true, ReportLimit: 0},
		flattenModeRollback, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "compatibility window") {
		t.Fatalf("a 58-day-old migration was rolled back without --force: %v", err)
	}
	if status, _ := liveItem(t, db, fxCatalog1); status != "archived" {
		t.Fatalf("the refused rollback still wrote: %s", status)
	}

	if err := applyPluginFlatten(db, pluginFlattenOptions{
		RunID: rollbackID, Confirm: true, Force: true, ReportLimit: 0,
	}, flattenModeRollback, io.Discard); err != nil {
		t.Fatalf("--force did not override the window: %v", err)
	}
	if status, _ := liveItem(t, db, fxCatalog1); status != "active" {
		t.Fatalf("forced rollback did not restore the row: %s", status)
	}
}

// R6-5 — the fork rule asks whether the fork's source is a package child, and
// apply unlinks package children. Re-planning after an apply therefore used to
// reclassify every remaining fork as ambiguous and strand it permanently — while
// runbook §6.4 tells the operator to re-plan a new run to finish up. The
// migration's own durable row table is the evidence that survives its own writes.
func TestPluginFlatten_ForkOfAnAlreadyRetiredPackageChildIsStillDerived(t *testing.T) {
	db := newPluginFlattenPostgresDB(t)
	seedFlattenWorld(t, db)

	firstRun, _ := planFor(t, db, "")
	if err := applyPluginFlatten(db, pluginFlattenOptions{RunID: firstRun, Confirm: true, ReportLimit: 0},
		flattenModeMigrate, io.Discard); err != nil {
		t.Fatalf("first apply: %v", err)
	}

	// A fork of an already-retired package child turns up in a later round: the
	// conflict from the first run is resolved, or a straggler was created before
	// the writers were shut off.
	const lateFork = "11111111-1111-4111-8111-00000000000c"
	seedFlattenItem(t, db, flattenFixture{
		ID: lateFork, SourceType: "fork", Parent: ptr(fxPlugin), ForkedFrom: ptr(fxForkSource)})

	_, rows := planFor(t, db, "")
	got, ok := rowsByItem(rows)[lateFork]
	if !ok {
		t.Fatal("the second plan did not inventory the late fork")
	}
	if got.Classification != flattenClassDerivedFork || got.Action != flattenActionArchiveAndUnlink {
		t.Fatalf("second plan classified the fork %s/%s, want %s/%s (%s)",
			got.Classification, got.Action, flattenClassDerivedFork, flattenActionArchiveAndUnlink, got.Reason)
	}
	if !strings.Contains(got.Reason, "earlier flatten run") {
		t.Errorf("reason does not say where the evidence came from: %q", got.Reason)
	}

	// The rule must not have been widened: a fork of a row that was never a
	// package child is still ambiguous, and an `unlink_only` row's unlinking is
	// still no evidence at all.
	const forkOfIndependent = "11111111-1111-4111-8111-00000000000d"
	seedFlattenItem(t, db, flattenFixture{
		ID: forkOfIndependent, SourceType: "fork", Parent: ptr(fxPlugin), ForkedFrom: ptr(fxIndependent)})
	_, rows = planFor(t, db, "")
	if got := rowsByItem(rows)[forkOfIndependent]; got.Classification != flattenClassAmbiguous {
		t.Fatalf("a fork of an unlink_only row classified as %s (%s)", got.Classification, got.Reason)
	}
}

// codex P2-1 — the totals are the only thing runbook §4 asks a human to read
// before approving, and they are derived, so they are verified by recomputation
// rather than by the digest. A file whose rows verify but whose summary
// understates the consumer impact passes a checksum and fails the purpose of
// having one.
func TestPluginFlatten_ArtifactTotalsMustAgreeWithItsRows(t *testing.T) {
	db := newPluginFlattenPostgresDB(t)
	seedFlattenWorld(t, db)
	seedFavorite(t, db, fxCatalog1, fxHolderFavorite)
	artifact := filepath.Join(t.TempDir(), "plan.json")
	planFor(t, db, artifact)

	raw, err := os.ReadFile(artifact)
	if err != nil {
		t.Fatalf("read artifact: %v", err)
	}
	var decoded pluginFlattenArtifact
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if decoded.Totals["favorites_on_candidates"] != 1 {
		t.Fatalf("fixture did not produce the impact number under test: %+v", decoded.Totals)
	}
	decoded.Totals["favorites_on_candidates"] = 0
	edited, _ := json.MarshalIndent(decoded, "", "  ")
	if err := os.WriteFile(artifact, edited, 0o644); err != nil {
		t.Fatalf("write edited artifact: %v", err)
	}

	_, err = readPluginFlattenArtifact(artifact)
	if err == nil || !strings.Contains(err.Error(), "favorites_on_candidates") {
		t.Fatalf("an artifact that understates its own impact verified: %v", err)
	}
}

// codex P2-2 — the batch size was frozen at plan time and `--batch-size` was
// accepted and ignored at apply time, so the runbook's own arithmetic
// (`--batch-size=500` over 6738 rows = 14 batches) described something that did
// not happen: 34 batches of 200. The dry run now states the batching it will
// use, which is where an operator can still act on it.
func TestPluginFlatten_ApplyHonoursAnExplicitBatchSize(t *testing.T) {
	db := newPluginFlattenPostgresDB(t)
	seedFlattenWorld(t, db)
	runID, _ := planFor(t, db, "")

	dryRun := func(opts pluginFlattenOptions) string {
		t.Helper()
		var out bytes.Buffer
		opts.RunID = runID
		opts.ReportLimit = 0
		if err := applyPluginFlatten(db, opts, flattenModeMigrate, &out); err != nil {
			t.Fatalf("dry run: %v", err)
		}
		return out.String()
	}

	// The fixture has 6 actionable rows.
	if got := dryRun(pluginFlattenOptions{}); !strings.Contains(got, "6 pending row(s) ready to apply in 1 batch(es) of 200") {
		t.Errorf("default batching not reported: %q", got)
	}
	if got := dryRun(pluginFlattenOptions{BatchSize: 2, BatchSizeSet: true}); !strings.Contains(got, "in 3 batch(es) of 2") {
		t.Errorf("--batch-size was ignored: %q", got)
	}

	// And it is honoured for real, not only in the report.
	if err := applyPluginFlatten(db, pluginFlattenOptions{
		RunID: runID, Confirm: true, ReportLimit: 0, BatchSize: 2, BatchSizeSet: true,
	}, flattenModeMigrate, io.Discard); err != nil {
		t.Fatalf("apply: %v", err)
	}
	for _, id := range []string{fxCatalog1, fxCatalog2, fxArchive, fxForkSource, fxFork} {
		if status, parent := liveItem(t, db, id); status != "archived" || parent != nil {
			t.Errorf("row %s did not converge under a small batch size: status=%s parent=%v", id, status, parent)
		}
	}
}

// R6-9 — the command used to carry a hand-copied second definition of the two
// tool tables, and it had already drifted by two indexes and every column
// comment. It now runs the migration file itself; this pins that the bootstrap
// path really produces the production schema.
func TestEnsurePluginFlattenTables_ProducesTheMigrationsSchema(t *testing.T) {
	db := newPluginFlattenPostgresDB(t) // calls ensurePluginFlattenTables

	var indexes []string
	if err := db.Raw(`SELECT indexname FROM pg_indexes
		WHERE schemaname = current_schema() AND tablename LIKE 'plugin_flatten_migration_%'
		ORDER BY indexname`).Scan(&indexes).Error; err != nil {
		t.Fatalf("list indexes: %v", err)
	}
	have := make(map[string]bool, len(indexes))
	for _, name := range indexes {
		have[name] = true
	}
	for _, want := range []string{
		"plugin_flatten_migration_runs_pkey",
		"idx_plugin_flatten_runs_status",
		"idx_plugin_flatten_runs_source",
		"plugin_flatten_migration_rows_pkey",
		"uq_plugin_flatten_rows_seq",
		"idx_plugin_flatten_rows_pending",
		"idx_plugin_flatten_rows_item",
	} {
		if !have[want] {
			t.Errorf("index %s is missing; the bootstrap DDL has drifted from the migration", want)
		}
	}

	// The comments the migration carries must be there too — they are how the
	// next person learns what `already_at_target` means from the database itself.
	var comment string
	if err := db.Raw(`SELECT COALESCE(obj_description(
		(current_schema() || '.plugin_flatten_migration_rows')::regclass, 'pg_class'), '')`).
		Scan(&comment).Error; err != nil {
		t.Fatalf("read table comment: %v", err)
	}
	if !strings.Contains(comment, "compare-and-set predicate") {
		t.Errorf("table comment missing: %q", comment)
	}

	// And the CHECK must accept the new state while still rejecting nonsense.
	runID := seedBareFlattenRun(t, db)
	if err := db.Exec(`UPDATE plugin_flatten_migration_rows SET row_state = 'already_at_target'
		WHERE run_id = ?::uuid`, runID).Error; err != nil {
		t.Fatalf("CHECK rejects already_at_target: %v", err)
	}
	if err := db.Exec(`UPDATE plugin_flatten_migration_rows SET row_state = 'whatever'
		WHERE run_id = ?::uuid`, runID).Error; err == nil {
		t.Error("CHECK accepted an unknown row_state")
	}
}

// seedBareFlattenRun writes one run with one row, for constraint probing.
func seedBareFlattenRun(t *testing.T, db *gorm.DB) string {
	t.Helper()
	rows := []pluginFlattenPlanRow{{
		Seq: 1, ItemID: fxCatalog1, BeforeStatus: "active", AfterStatus: "archived",
		Classification: flattenClassDerivedCatalog, Action: flattenActionArchiveAndUnlink,
		Reason: "probe", RowState: flattenRowPending,
	}}
	runID := "44444444-4444-4444-8444-000000000001"
	if err := persistPluginFlattenRun(db, pluginFlattenRunRecord{
		ID: runID, SchemaVersion: pluginFlattenSchemaVersion, Mode: flattenModeMigrate,
		Status: flattenRunPlanned, BatchSize: 200,
		PlanDigest: pluginFlattenPlanDigest(pluginFlattenSchemaVersion, flattenModeMigrate, rows),
		Totals:     pluginFlattenTotals(rows), CreatedBy: "test",
	}, rows); err != nil {
		t.Fatalf("seed run: %v", err)
	}
	return runID
}
