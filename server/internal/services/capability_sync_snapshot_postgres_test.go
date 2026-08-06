package services

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/costrict/costrict-web/server/internal/models"
	"github.com/costrict/costrict-web/server/internal/syncsnapshot"
	"github.com/google/uuid"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// The csc snapshot v2 tests run on a real PostgreSQL because everything they
// assert IS a PostgreSQL mechanism: REPEATABLE READ abort semantics, the
// allocator's row lock, the CHECK constraints and the three guard triggers.
// SQLite serializes every writer and enforces none of those, so a green SQLite
// run would prove nothing about the property that decides whether a user's
// files get deleted.

// snapshotFixtureTables are the tables the snapshot service reads that are NOT
// owned by the snapshot migrations. They are cut down to the columns the code
// under test touches (plus every column a SELECT * path needs), in production
// column types.
var snapshotFixtureTables = []string{
	`CREATE TABLE capability_items (
		id             UUID PRIMARY KEY,
		item_type      TEXT NOT NULL DEFAULT 'skill',
		slug           TEXT NOT NULL DEFAULT '',
		name           TEXT NOT NULL DEFAULT '',
		version        TEXT NOT NULL DEFAULT '',
		content_md5    VARCHAR(64) NOT NULL DEFAULT '',
		git_sha        VARCHAR(40) NOT NULL DEFAULT '',
		status         TEXT NOT NULL DEFAULT 'active',
		favorite_count INT NOT NULL DEFAULT 0,
		content_backend VARCHAR(16) NOT NULL DEFAULT 'db'
	)`,
	`CREATE TABLE item_favorites (
		id         UUID PRIMARY KEY DEFAULT gen_random_uuid(),
		item_id    UUID NOT NULL,
		user_id    VARCHAR(191) NOT NULL,
		created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
		UNIQUE (item_id, user_id)
	)`,
	`CREATE TABLE item_distributions (
		id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
		item_id         UUID NOT NULL,
		distributor_id  TEXT NOT NULL DEFAULT '',
		permission_mode VARCHAR(32) NOT NULL DEFAULT 'readonly',
		status          VARCHAR(32) NOT NULL DEFAULT 'active',
		scope_type      VARCHAR(32) NOT NULL DEFAULT 'user',
		target_id       TEXT NOT NULL DEFAULT '',
		message         TEXT NOT NULL DEFAULT '',
		revoked_at      TIMESTAMPTZ,
		expires_at      TIMESTAMPTZ,
		created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
		updated_at      TIMESTAMPTZ NOT NULL DEFAULT now()
	)`,
	`CREATE TABLE item_distribution_receipts (
		id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
		distribution_id UUID NOT NULL,
		user_id         TEXT NOT NULL,
		receipt_status  VARCHAR(32) NOT NULL DEFAULT 'unread',
		forked_item_id  UUID,
		created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
		updated_at      TIMESTAMPTZ NOT NULL DEFAULT now()
	)`,
}

// snapshotMigrations are applied verbatim from server/migrations, in goose
// order. Copying their DDL into the test instead would let the tests pass
// against constraints production does not have — the exact failure mode the
// review found in the first slice.
var snapshotMigrations = []string{
	"20260805000200_create_capability_sync_tombstones.sql",
	"20260805000300_create_capability_sync_snapshot_generations.sql",
	"20260805000700_constrain_capability_sync_tombstone_triples.sql",
	"20260805000800_materialize_capability_sync_snapshots.sql",
}

func newSnapshotPostgresDB(t *testing.T) *gorm.DB {
	t.Helper()
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		t.Skip("DATABASE_URL not set; skipping PostgreSQL capability sync snapshot test")
	}
	parsed, err := url.Parse(dsn)
	if err != nil {
		t.Fatalf("parse DATABASE_URL: %v", err)
	}
	schema := fmt.Sprintf("sync_snapshot_%d", time.Now().UnixNano())

	admin, err := gorm.Open(postgres.Open(dsn), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatalf("open PostgreSQL: %v", err)
	}
	quoted := `"` + strings.ReplaceAll(schema, `"`, `""`) + `"`
	if err := admin.Exec("CREATE SCHEMA " + quoted).Error; err != nil {
		t.Fatalf("create schema: %v", err)
	}
	t.Cleanup(func() {
		_ = admin.Exec("DROP SCHEMA " + quoted + " CASCADE").Error
		if sqlDB, err := admin.DB(); err == nil {
			_ = sqlDB.Close()
		}
	})

	// search_path travels in the DSN, not as a SET: the concurrency tests use
	// several connections at once and a per-session SET would leave the others
	// pointing at public.
	query := parsed.Query()
	query.Set("search_path", schema)
	parsed.RawQuery = query.Encode()

	db, err := gorm.Open(postgres.Open(parsed.String()), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatalf("open PostgreSQL in %s: %v", schema, err)
	}
	t.Cleanup(func() {
		if sqlDB, err := db.DB(); err == nil {
			_ = sqlDB.Close()
		}
	})

	for _, ddl := range snapshotFixtureTables {
		if err := db.Exec(ddl).Error; err != nil {
			t.Fatalf("create fixture table: %v\nSQL: %s", err, ddl)
		}
	}
	for _, file := range snapshotMigrations {
		if err := db.Exec(readSnapshotMigrationUp(t, file)).Error; err != nil {
			t.Fatalf("apply migration %s: %v", file, err)
		}
	}
	return db
}

// readSnapshotMigrationUp extracts the Up half of a goose migration.
func readSnapshotMigrationUp(t *testing.T, name string) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "..", "migrations", name))
	if err != nil {
		t.Fatalf("read migration %s: %v", name, err)
	}
	body := string(raw)
	start := strings.Index(body, "-- +goose Up")
	if start < 0 {
		t.Fatalf("migration %s has no Up section", name)
	}
	body = body[start+len("-- +goose Up"):]
	if end := strings.Index(body, "-- +goose Down"); end >= 0 {
		body = body[:end]
	}
	// The goose statement markers are comments to PostgreSQL; stripping them is
	// cosmetic, but it keeps a failure message readable.
	body = strings.ReplaceAll(body, "-- +goose StatementBegin", "")
	body = strings.ReplaceAll(body, "-- +goose StatementEnd", "")
	return body
}

const (
	snapshotUserA = "user-a"
	snapshotUserB = "user-b"
)

func newSnapshotService(db *gorm.DB, lifecycle bool) *CapabilitySyncSnapshotService {
	return &CapabilitySyncSnapshotService{DB: db, PageSize: 2, LifecyclePropagation: lifecycle}
}

func seedSnapshotItem(t *testing.T, db *gorm.DB, id, name, status string) {
	t.Helper()
	if err := db.Exec(`INSERT INTO capability_items (id, item_type, slug, name, version, content_md5, status)
		VALUES (?, 'skill', ?, ?, '1.0.0', 'md5-'||?, ?)`,
		id, "slug-"+name, name, name, status).Error; err != nil {
		t.Fatalf("seed item %s: %v", id, err)
	}
}

func seedSnapshotFavorite(t *testing.T, db *gorm.DB, itemID, userID string) {
	t.Helper()
	if err := db.Exec(`INSERT INTO item_favorites (item_id, user_id) VALUES (?, ?)`, itemID, userID).Error; err != nil {
		t.Fatalf("seed favorite: %v", err)
	}
}

func snapshotItemID(n int) string {
	return fmt.Sprintf("%08d-0000-4000-8000-000000000000", n)
}

// collectSnapshot pages a snapshot exactly as a client must: from page 0, using
// the returned snapshot id, until the page that carries `complete`.
type collectedSnapshot struct {
	SnapshotID     string
	Generation     int64
	SnapshotDigest string
	PageCount      int
	ItemCount      int
	TombstoneCount int
	Items          []json.RawMessage
	Tombstones     []json.RawMessage
	Complete       bool
	GeneratedAt    time.Time
	Reused         bool
}

func collectSnapshot(t *testing.T, svc *CapabilitySyncSnapshotService, principal string) collectedSnapshot {
	t.Helper()
	ctx := context.Background()
	snapshot, reused, err := svc.EnsureSnapshot(ctx, principal)
	if err != nil {
		t.Fatalf("ensure snapshot for %s: %v", principal, err)
	}
	first, err := svc.PageOfSnapshot(ctx, snapshot, 0, reused)
	if err != nil {
		t.Fatalf("page 0: %v", err)
	}
	out := collectedSnapshot{
		SnapshotID:     first.SnapshotID,
		Generation:     first.Generation,
		SnapshotDigest: first.SnapshotDigest,
		PageCount:      first.PageCount,
		ItemCount:      first.ItemCount,
		TombstoneCount: first.TombstoneCount,
		GeneratedAt:    first.GeneratedAt,
		Reused:         reused,
	}
	appendPage := func(page *SnapshotPage) {
		for _, item := range page.Items {
			out.Items = append(out.Items, append(json.RawMessage(nil), item...))
		}
		for _, tombstone := range page.Tombstones {
			out.Tombstones = append(out.Tombstones, append(json.RawMessage(nil), tombstone...))
		}
		out.Complete = page.Complete
	}
	appendPage(first)
	for index := 1; index < first.PageCount; index++ {
		page, err := svc.GetSnapshotPage(ctx, principal, first.SnapshotID, index)
		if err != nil {
			t.Fatalf("page %d: %v", index, err)
		}
		if page.SnapshotID != out.SnapshotID || page.Generation != out.Generation ||
			page.PageCount != out.PageCount || page.ItemCount != out.ItemCount ||
			page.TombstoneCount != out.TombstoneCount || page.SnapshotDigest != out.SnapshotDigest {
			t.Fatalf("page %d does not repeat the manifest verbatim: %+v vs %+v", index, page, out)
		}
		appendPage(page)
	}
	return out
}

// verifyCollected performs the client's entire acceptance procedure and reports
// whether the snapshot may authorize removals.
func verifyCollected(t *testing.T, collected collectedSnapshot) error {
	t.Helper()
	if !collected.Complete {
		return fmt.Errorf("snapshot is not complete")
	}
	if len(collected.Items) != collected.ItemCount || len(collected.Tombstones) != collected.TombstoneCount {
		return fmt.Errorf("reassembled %d/%d entries, manifest claims %d/%d",
			len(collected.Items), len(collected.Tombstones), collected.ItemCount, collected.TombstoneCount)
	}
	items := make([]any, 0, len(collected.Items))
	for _, item := range collected.Items {
		items = append(items, syncsnapshot.Raw(item))
	}
	tombstones := make([]any, 0, len(collected.Tombstones))
	for _, tombstone := range collected.Tombstones {
		tombstones = append(tombstones, syncsnapshot.Raw(tombstone))
	}
	_, digest, err := syncsnapshot.Digest(syncsnapshot.DocumentFor(syncsnapshot.Manifest{
		SnapshotID:     collected.SnapshotID,
		Generation:     collected.Generation,
		GeneratedAt:    collected.GeneratedAt.UTC().Format(time.RFC3339),
		PageCount:      collected.PageCount,
		ItemCount:      collected.ItemCount,
		TombstoneCount: collected.TombstoneCount,
	}, items, tombstones))
	if err != nil {
		return err
	}
	if digest != collected.SnapshotDigest {
		return fmt.Errorf("digest %s != %s", digest, collected.SnapshotDigest)
	}
	return nil
}

func tombstoneItemIDs(t *testing.T, collected collectedSnapshot) map[string]map[string]any {
	t.Helper()
	out := map[string]map[string]any{}
	for _, raw := range collected.Tombstones {
		var decoded map[string]any
		if err := json.Unmarshal(raw, &decoded); err != nil {
			t.Fatalf("decode tombstone: %v", err)
		}
		out[decoded["itemId"].(string)] = decoded
	}
	return out
}

func itemIDs(t *testing.T, collected collectedSnapshot) map[string]map[string]any {
	t.Helper()
	out := map[string]map[string]any{}
	for _, raw := range collected.Items {
		var decoded map[string]any
		if err := json.Unmarshal(raw, &decoded); err != nil {
			t.Fatalf("decode item: %v", err)
		}
		out[decoded["itemId"].(string)] = decoded
	}
	return out
}

// -----------------------------------------------------------------------------
// AC-LH12: what may and may not authorize a removal
// -----------------------------------------------------------------------------

// AC-LH12 (positive half): an archived favorite appears as an EXPLICIT
// tombstone inside a complete, digest-verified snapshot. This is the only shape
// that entitles a client to delete anything.
func TestSnapshot_ArchivedFavoriteBecomesAnExplicitTombstone(t *testing.T) {
	db := newSnapshotPostgresDB(t)
	svc := newSnapshotService(db, true)

	kept, archived := snapshotItemID(1), snapshotItemID(2)
	seedSnapshotItem(t, db, kept, "kept", "active")
	seedSnapshotItem(t, db, archived, "archived", "active")
	seedSnapshotFavorite(t, db, kept, snapshotUserA)
	seedSnapshotFavorite(t, db, archived, snapshotUserA)

	// The Git lifecycle writer's entry point, called as Phase C will call it:
	// inside the transaction that archives the item.
	if err := db.Transaction(func(tx *gorm.DB) error {
		if err := tx.Exec(`UPDATE capability_items SET status='archived' WHERE id = ?`, archived).Error; err != nil {
			return err
		}
		count, err := RecordGitArchiveTombstonesTx(tx, archived, models.GitLifecycleReasonRepositoryDeleted, time.Now())
		if err != nil {
			return err
		}
		if count != 1 {
			return fmt.Errorf("tombstoned %d principals, want 1", count)
		}
		return nil
	}); err != nil {
		t.Fatalf("archive item: %v", err)
	}

	collected := collectSnapshot(t, svc, snapshotUserA)
	if err := verifyCollected(t, collected); err != nil {
		t.Fatalf("snapshot must be acceptable: %v", err)
	}
	if _, active := itemIDs(t, collected)[kept]; !active {
		t.Fatal("the surviving favorite must still be active")
	}
	tombstones := tombstoneItemIDs(t, collected)
	tombstone, ok := tombstones[archived]
	if !ok {
		t.Fatalf("archived favorite has no tombstone; got %v", tombstones)
	}
	if tombstone["reason"] != models.SyncTombstoneReasonGitArchived ||
		tombstone["source"] != models.SyncTombstoneSourceGitLifecycle ||
		tombstone["lifecycleReason"] != models.GitLifecycleReasonRepositoryDeleted {
		t.Fatalf("tombstone does not carry the Git cause: %v", tombstone)
	}
	if tombstone["eventId"] == "" || tombstone["removedAt"] == "" {
		t.Fatalf("tombstone is missing its durable event identity: %v", tombstone)
	}

	// The favorite itself is preserved: the contract removes the entitlement's
	// PROJECTION, not the user's relationship, so a Git restore reactivates the
	// same item for the same people.
	var favorites int64
	db.Table("item_favorites").Where("item_id = ? AND user_id = ?", archived, snapshotUserA).Count(&favorites)
	if favorites != 1 {
		t.Fatalf("archive deleted the favorite (%d rows); it must be preserved", favorites)
	}
}

// AC-LH12: an EMPTY snapshot is complete and authoritative, and authorizes
// nothing. This is the case a naive "absence means removal" client would read
// as "delete everything".
func TestSnapshot_EmptySnapshotAuthorizesNoRemoval(t *testing.T) {
	db := newSnapshotPostgresDB(t)
	svc := newSnapshotService(db, true)

	collected := collectSnapshot(t, svc, snapshotUserA)
	if err := verifyCollected(t, collected); err != nil {
		t.Fatalf("an empty snapshot must still verify: %v", err)
	}
	if collected.PageCount != 1 {
		t.Fatalf("page count = %d, an empty snapshot must still have one page to carry `complete`", collected.PageCount)
	}
	if collected.ItemCount != 0 || collected.TombstoneCount != 0 {
		t.Fatalf("empty snapshot = %d items / %d tombstones", collected.ItemCount, collected.TombstoneCount)
	}
	if len(collected.Tombstones) != 0 {
		t.Fatal("an empty snapshot carries no tombstones, so it instructs no removal")
	}
}

// AC-LH12: a FAILED build leaves nothing servable — no complete manifest, no
// payload, and no half-written snapshot a client could page.
func TestSnapshot_FailedBuildLeavesNothingServable(t *testing.T) {
	db := newSnapshotPostgresDB(t)
	svc := newSnapshotService(db, true)

	item := snapshotItemID(1)
	seedSnapshotItem(t, db, item, "one", "active")
	seedSnapshotFavorite(t, db, item, snapshotUserA)

	// Break materialization from inside: a tombstone whose event id collides
	// with another item's makes the snapshot invalid by the contract's own
	// rule, so the build must abort rather than emit it.
	other := snapshotItemID(2)
	seedSnapshotItem(t, db, other, "two", "archived")
	third := snapshotItemID(3)
	seedSnapshotItem(t, db, third, "three", "archived")
	if err := db.Exec(`INSERT INTO capability_sync_tombstones (user_id, item_id, reason, source, event_id, removed_at)
		VALUES (?, ?, 'unfavorited', 'favorite', 'shared-event', now())`, snapshotUserA, other).Error; err != nil {
		t.Fatalf("seed tombstone: %v", err)
	}
	// The unique event_id constraint prevents the collision at rest, so it is
	// injected the only way it could ever occur in production: two rows that
	// each look fine but describe the same event.
	if err := db.Exec(`ALTER TABLE capability_sync_tombstones DROP CONSTRAINT uq_capability_sync_tombstones_event`).Error; err != nil {
		t.Fatalf("relax event uniqueness for the fault injection: %v", err)
	}
	if err := db.Exec(`INSERT INTO capability_sync_tombstones (user_id, item_id, reason, source, event_id, removed_at)
		VALUES (?, ?, 'unfavorited', 'favorite', 'shared-event', now())`, snapshotUserA, third).Error; err != nil {
		t.Fatalf("seed colliding tombstone: %v", err)
	}

	if _, _, err := svc.EnsureSnapshot(context.Background(), snapshotUserA); err == nil {
		t.Fatal("a snapshot the contract calls invalid must not be produced")
	}

	var manifests, payloads int64
	db.Table("capability_sync_snapshots").Count(&manifests)
	db.Table("capability_sync_snapshot_payloads").Count(&payloads)
	if manifests != 0 || payloads != 0 {
		t.Fatalf("a failed build left %d manifests and %d payloads behind", manifests, payloads)
	}
}

// AC-LH12: an INCOMPLETE snapshot is never served, even by id. A client that
// could page one would reassemble a partial set and — if it trusted absence —
// delete everything not in it.
func TestSnapshot_IncompleteSnapshotIsNeverServed(t *testing.T) {
	db := newSnapshotPostgresDB(t)
	svc := newSnapshotService(db, true)

	var snapshotID string
	if err := db.Transaction(func(tx *gorm.DB) error {
		generation, err := allocateSnapshotGeneration(tx, snapshotUserA, time.Now())
		if err != nil {
			return err
		}
		snapshotID = uuid.NewString()
		return tx.Exec(`INSERT INTO capability_sync_snapshots
			(id, principal_id, generation, page_count, page_size, item_count, tombstone_count, complete, expires_at)
			VALUES (?, ?, ?, 1, 2, 0, 0, false, now() + interval '1 hour')`,
			snapshotID, snapshotUserA, generation).Error
	}); err != nil {
		t.Fatalf("seed incomplete snapshot: %v", err)
	}

	if _, err := svc.GetSnapshotPage(context.Background(), snapshotUserA, snapshotID, 0); err != ErrSnapshotNotFound {
		t.Fatalf("err = %v, want ErrSnapshotNotFound for an incomplete snapshot", err)
	}
}

// AC-LH12: an EXPIRED snapshot id is refused rather than answered from current
// data. Answering it is how pages from different data states end up in one
// reassembly.
func TestSnapshot_ExpiredSnapshotIsRefusedNotRebuilt(t *testing.T) {
	db := newSnapshotPostgresDB(t)
	svc := newSnapshotService(db, true)
	seedSnapshotItem(t, db, snapshotItemID(1), "one", "active")
	seedSnapshotFavorite(t, db, snapshotItemID(1), snapshotUserA)

	collected := collectSnapshot(t, svc, snapshotUserA)
	if err := db.Exec(`UPDATE capability_sync_snapshots SET expires_at = now() - interval '1 minute' WHERE id = ?`,
		collected.SnapshotID).Error; err != nil {
		t.Fatalf("expire snapshot: %v", err)
	}
	if _, err := svc.GetSnapshotPage(context.Background(), snapshotUserA, collected.SnapshotID, 0); err != ErrSnapshotNotFound {
		t.Fatalf("err = %v, want ErrSnapshotNotFound for an expired snapshot", err)
	}
}

// AC-LH12: another principal's snapshot id is indistinguishable from an unknown
// one, and never leaks a page.
func TestSnapshot_ForeignSnapshotIDIsNotFound(t *testing.T) {
	db := newSnapshotPostgresDB(t)
	svc := newSnapshotService(db, true)
	seedSnapshotItem(t, db, snapshotItemID(1), "one", "active")
	seedSnapshotFavorite(t, db, snapshotItemID(1), snapshotUserA)

	collected := collectSnapshot(t, svc, snapshotUserA)
	if _, err := svc.GetSnapshotPage(context.Background(), snapshotUserB, collected.SnapshotID, 0); err != ErrSnapshotNotFound {
		t.Fatalf("err = %v, want ErrSnapshotNotFound for another principal's snapshot", err)
	}
	if _, err := svc.GetSnapshotPage(context.Background(), snapshotUserA, uuid.NewString(), 0); err != ErrSnapshotNotFound {
		t.Fatalf("err = %v, want ErrSnapshotNotFound for an unknown snapshot", err)
	}
}

// AC-LH12: a COUNT mismatch between the manifest and the stored artifact is
// refused at read time. A client checks counts before the digest, so serving a
// mismatch would cost it a full pagination pass to reach the same answer.
func TestSnapshot_CountMismatchIsRefused(t *testing.T) {
	db := newSnapshotPostgresDB(t)
	svc := newSnapshotService(db, true)
	for i := 1; i <= 3; i++ {
		seedSnapshotItem(t, db, snapshotItemID(i), fmt.Sprintf("item-%d", i), "active")
		seedSnapshotFavorite(t, db, snapshotItemID(i), snapshotUserA)
	}
	collected := collectSnapshot(t, svc, snapshotUserA)

	// The manifest guard freezes a complete snapshot, so the corruption has to
	// come from outside the guard — which is exactly the scenario the read-side
	// check exists for.
	if err := db.Exec(`ALTER TABLE capability_sync_snapshots DISABLE TRIGGER capability_sync_snapshot_manifest_guard`).Error; err != nil {
		t.Fatalf("disable manifest guard for the fault injection: %v", err)
	}
	if err := db.Exec(`UPDATE capability_sync_snapshots SET item_count = item_count + 1 WHERE id = ?`,
		collected.SnapshotID).Error; err != nil {
		t.Fatalf("corrupt counts: %v", err)
	}
	if _, err := svc.GetSnapshotPage(context.Background(), snapshotUserA, collected.SnapshotID, 0); err == nil {
		t.Fatal("a manifest whose counts disagree with its artifact must not be served")
	}
}

// AC-LH12: TRUNCATION and PAGE OMISSION break verification. Proven against real
// served pages, not only against the fixture.
func TestSnapshot_TruncatedOrOmittedPagesFailVerification(t *testing.T) {
	db := newSnapshotPostgresDB(t)
	svc := newSnapshotService(db, true)
	for i := 1; i <= 5; i++ {
		seedSnapshotItem(t, db, snapshotItemID(i), fmt.Sprintf("item-%d", i), "active")
		seedSnapshotFavorite(t, db, snapshotItemID(i), snapshotUserA)
	}
	collected := collectSnapshot(t, svc, snapshotUserA)
	if collected.PageCount < 3 {
		t.Fatalf("this test needs a multi-page snapshot, got %d pages", collected.PageCount)
	}
	if err := verifyCollected(t, collected); err != nil {
		t.Fatalf("the intact snapshot must verify: %v", err)
	}

	t.Run("a dropped element", func(t *testing.T) {
		damaged := collected
		damaged.Items = append([]json.RawMessage(nil), collected.Items[1:]...)
		if err := verifyCollected(t, damaged); err == nil {
			t.Fatal("dropping an item still verified")
		}
	})
	t.Run("a stop before the final page", func(t *testing.T) {
		ctx := context.Background()
		partial := collectedSnapshot{
			SnapshotID: collected.SnapshotID, Generation: collected.Generation,
			SnapshotDigest: collected.SnapshotDigest, PageCount: collected.PageCount,
			ItemCount: collected.ItemCount, TombstoneCount: collected.TombstoneCount,
			GeneratedAt: collected.GeneratedAt,
		}
		for index := 0; index < collected.PageCount-1; index++ {
			page, err := svc.GetSnapshotPage(ctx, snapshotUserA, collected.SnapshotID, index)
			if err != nil {
				t.Fatalf("page %d: %v", index, err)
			}
			if page.Complete {
				t.Fatalf("page %d claims completeness before the final page", index)
			}
			for _, item := range page.Items {
				partial.Items = append(partial.Items, append(json.RawMessage(nil), item...))
			}
			partial.Complete = page.Complete
		}
		if err := verifyCollected(t, partial); err == nil {
			t.Fatal("a client that stopped early must not hold an acceptable snapshot")
		}
		// Two independent gates catch this, and both are checked: the missing
		// completeness marker AND — for a client that ignored it — the count and
		// digest of what it actually assembled.
		optimistic := partial
		optimistic.Complete = true
		if err := verifyCollected(t, optimistic); err == nil {
			t.Fatal("a client that assumed completeness must still fail on counts/digest")
		}
	})
	t.Run("a reordered element", func(t *testing.T) {
		damaged := collected
		damaged.Items = append([]json.RawMessage(nil), collected.Items...)
		damaged.Items[0], damaged.Items[1] = damaged.Items[1], damaged.Items[0]
		if err := verifyCollected(t, damaged); err == nil {
			t.Fatal("reordering items still verified; element order is part of the contract")
		}
	})
}

// AC-LH12: a DIGEST mismatch — one byte of one element altered — is detected.
func TestSnapshot_TamperedElementFailsTheDigest(t *testing.T) {
	db := newSnapshotPostgresDB(t)
	svc := newSnapshotService(db, true)
	seedSnapshotItem(t, db, snapshotItemID(1), "one", "active")
	seedSnapshotFavorite(t, db, snapshotItemID(1), snapshotUserA)

	collected := collectSnapshot(t, svc, snapshotUserA)
	tampered := collected
	tampered.Items = append([]json.RawMessage(nil), collected.Items...)
	tampered.Items[0] = json.RawMessage(strings.Replace(string(collected.Items[0]), `"1.0.0"`, `"9.9.9"`, 1))
	if string(tampered.Items[0]) == string(collected.Items[0]) {
		t.Fatal("the fault injection did not change anything")
	}
	if err := verifyCollected(t, tampered); err == nil {
		t.Fatal("a tampered element still verified")
	}
}

// -----------------------------------------------------------------------------
// AC-LH13: tombstones from the two live removal paths, and supersession
// -----------------------------------------------------------------------------

// AC-LH13: unfavorite produces an explicit tombstone through the real service
// path, and a later refavorite supersedes it under the SAME item id.
func TestSnapshot_UnfavoriteTombstoneAndSupersession(t *testing.T) {
	db := newSnapshotPostgresDB(t)
	svc := newSnapshotService(db, true)
	behavior := NewBehaviorService(db)

	item := snapshotItemID(1)
	seedSnapshotItem(t, db, item, "one", "active")
	seedSnapshotFavorite(t, db, item, snapshotUserA)

	first := collectSnapshot(t, svc, snapshotUserA)
	if _, active := itemIDs(t, first)[item]; !active {
		t.Fatal("the favorite must start active")
	}

	if _, removed, err := behavior.UnfavoriteItem(context.Background(), item, snapshotUserA); err != nil || !removed {
		t.Fatalf("unfavorite: removed=%v err=%v", removed, err)
	}
	afterRemoval := collectSnapshot(t, svc, snapshotUserA)
	if err := verifyCollected(t, afterRemoval); err != nil {
		t.Fatalf("verify: %v", err)
	}
	tombstone, ok := tombstoneItemIDs(t, afterRemoval)[item]
	if !ok {
		t.Fatal("unfavorite produced no tombstone")
	}
	if tombstone["reason"] != models.SyncTombstoneReasonUnfavorited ||
		tombstone["source"] != models.SyncTombstoneSourceFavorite ||
		tombstone["lifecycleReason"] != nil {
		t.Fatalf("unfavorite tombstone has the wrong cause: %v", tombstone)
	}
	if afterRemoval.Generation <= first.Generation {
		t.Fatalf("generation did not advance: %d -> %d", first.Generation, afterRemoval.Generation)
	}

	// Refavorite: the item returns as ACTIVE under the same id, and its older
	// tombstone is superseded rather than deleted.
	if _, created, err := behavior.FavoriteItem(context.Background(), item, snapshotUserA); err != nil || !created {
		t.Fatalf("refavorite: created=%v err=%v", created, err)
	}
	restored := collectSnapshot(t, svc, snapshotUserA)
	if err := verifyCollected(t, restored); err != nil {
		t.Fatalf("verify: %v", err)
	}
	if _, active := itemIDs(t, restored)[item]; !active {
		t.Fatal("the refavorited item must be active again under the same id")
	}
	if _, stillTombstoned := tombstoneItemIDs(t, restored)[item]; stillTombstoned {
		t.Fatal("an active item must not also be tombstoned")
	}
	var stored int64
	db.Table("capability_sync_tombstones").Where("user_id = ? AND item_id = ?", snapshotUserA, item).Count(&stored)
	if stored != 1 {
		t.Fatalf("supersession must be computed, not stored: %d tombstone rows remain", stored)
	}
	if restored.Generation <= afterRemoval.Generation {
		t.Fatalf("generation did not advance on restore: %d -> %d", afterRemoval.Generation, restored.Generation)
	}
}

// AC-LH13: revoking a distribution produces a distribution_revoked tombstone,
// driven through the real DistributionService path rather than by calling the
// tombstone writer directly — the attribution is the thing under test.
func TestSnapshot_DistributionRevokeProducesTombstone(t *testing.T) {
	db := newSnapshotPostgresDB(t)
	svc := newSnapshotService(db, true)
	behavior := NewBehaviorService(db)
	distributions := NewDistributionService(db, behavior, nil)

	item := snapshotItemID(1)
	seedSnapshotItem(t, db, item, "one", "active")

	distID := uuid.NewString()
	if err := db.Exec(`INSERT INTO item_distributions (id, item_id, distributor_id, permission_mode, status, scope_type, target_id)
		VALUES (?, ?, 'distributor', 'readonly', 'active', 'user', ?)`, distID, item, snapshotUserA).Error; err != nil {
		t.Fatalf("seed distribution: %v", err)
	}
	if err := db.Exec(`INSERT INTO item_distribution_receipts (distribution_id, user_id, receipt_status)
		VALUES (?, ?, 'unread')`, distID, snapshotUserA).Error; err != nil {
		t.Fatalf("seed receipt: %v", err)
	}
	seedSnapshotFavorite(t, db, item, snapshotUserA)

	before := collectSnapshot(t, svc, snapshotUserA)
	sources := before.Items
	if len(sources) != 1 {
		t.Fatalf("expected the distributed item to be active, got %d items", len(sources))
	}

	if err := distributions.RevokeDistribution(context.Background(), distID, "distributor", false); err != nil {
		t.Fatalf("revoke: %v", err)
	}

	after := collectSnapshot(t, svc, snapshotUserA)
	if err := verifyCollected(t, after); err != nil {
		t.Fatalf("verify: %v", err)
	}
	tombstone, ok := tombstoneItemIDs(t, after)[item]
	if !ok {
		t.Fatalf("revoke produced no tombstone; snapshot = %+v", after)
	}
	if tombstone["reason"] != models.SyncTombstoneReasonDistributionRevoked ||
		tombstone["source"] != models.SyncTombstoneSourceDistribution {
		t.Fatalf("revoke tombstone has the wrong cause: %v", tombstone)
	}
	if len(after.Items) != 0 {
		t.Fatalf("the revoked item must leave the active set, got %d items", len(after.Items))
	}
}

// -----------------------------------------------------------------------------
// F-6: event id rotation
// -----------------------------------------------------------------------------

// F-6 regression. unfavorite -> refavorite -> unfavorite must produce a DIFFERENT
// event id the second time.
//
// With a stable id the device recognizes an event it already applied, dedupes
// the second removal to a no-op, and the capability stays installed forever —
// no error, no symptom, nothing to page an operator about. The review could
// only describe this risk because no row predicate can express it; this test is
// what actually holds the line.
func TestTombstone_EventIDRotatesOnEachNewRemovalTransition(t *testing.T) {
	db := newSnapshotPostgresDB(t)
	svc := newSnapshotService(db, true)
	behavior := NewBehaviorService(db)
	ctx := context.Background()

	item := snapshotItemID(1)
	seedSnapshotItem(t, db, item, "one", "active")
	seedSnapshotFavorite(t, db, item, snapshotUserA)

	eventIDOf := func(t *testing.T) string {
		t.Helper()
		collected := collectSnapshot(t, svc, snapshotUserA)
		tombstone, ok := tombstoneItemIDs(t, collected)[item]
		if !ok {
			t.Fatalf("expected a tombstone, snapshot = %+v", collected)
		}
		return tombstone["eventId"].(string)
	}

	if _, removed, err := behavior.UnfavoriteItem(ctx, item, snapshotUserA); err != nil || !removed {
		t.Fatalf("first unfavorite: removed=%v err=%v", removed, err)
	}
	firstEvent := eventIDOf(t)

	// A repeated removal that changes NOTHING must not rotate: the device would
	// otherwise re-run removal work on every poll forever.
	if _, removed, err := behavior.UnfavoriteItem(ctx, item, snapshotUserA); err != nil || removed {
		t.Fatalf("repeat unfavorite: removed=%v err=%v (nothing existed to remove)", removed, err)
	}
	if again := eventIDOf(t); again != firstEvent {
		t.Fatalf("a no-op unfavorite rotated the event id: %s -> %s", firstEvent, again)
	}

	if _, created, err := behavior.FavoriteItem(ctx, item, snapshotUserA); err != nil || !created {
		t.Fatalf("refavorite: created=%v err=%v", created, err)
	}
	if _, removed, err := behavior.UnfavoriteItem(ctx, item, snapshotUserA); err != nil || !removed {
		t.Fatalf("second unfavorite: removed=%v err=%v", removed, err)
	}
	secondEvent := eventIDOf(t)
	if secondEvent == firstEvent {
		t.Fatalf("the second removal reused event id %s; a client that already applied it "+
			"would dedupe the removal away and keep the capability installed forever", firstEvent)
	}
}

// -----------------------------------------------------------------------------
// AC-LH17 / F-1 / F-4 / F-5: generation and snapshot identity
// -----------------------------------------------------------------------------

// AC-LH17 idempotency, and F-1 step 3: unchanged content re-serves the SAME
// snapshot id and generation instead of allocating a new one. Without this a
// polling fleet burns generations, and every burned generation costs each
// client a full re-verification of state that did not change.
func TestSnapshot_UnchangedContentReusesTheSameSnapshot(t *testing.T) {
	db := newSnapshotPostgresDB(t)
	svc := newSnapshotService(db, true)
	seedSnapshotItem(t, db, snapshotItemID(1), "one", "active")
	seedSnapshotFavorite(t, db, snapshotItemID(1), snapshotUserA)

	first := collectSnapshot(t, svc, snapshotUserA)
	if first.Reused {
		t.Fatal("the first build cannot be a reuse")
	}
	for attempt := 0; attempt < 5; attempt++ {
		again := collectSnapshot(t, svc, snapshotUserA)
		if again.SnapshotID != first.SnapshotID || again.Generation != first.Generation ||
			again.SnapshotDigest != first.SnapshotDigest {
			t.Fatalf("poll %d minted a new snapshot: %s/%d -> %s/%d",
				attempt, first.SnapshotID, first.Generation, again.SnapshotID, again.Generation)
		}
		if !again.Reused {
			t.Fatalf("poll %d rebuilt instead of re-serving", attempt)
		}
	}
	var generation int64
	db.Table("capability_sync_snapshot_generations").Where("principal_id = ?", snapshotUserA).
		Select("generation").Scan(&generation)
	if generation != 1 {
		t.Fatalf("six identical polls advanced the counter to %d; generation is a version, not a request count", generation)
	}
}

// AC-LH17: a wall-clock rollback cannot reorder snapshots. Generation is the
// only ordering signal precisely because generatedAt is not trustworthy.
func TestSnapshot_WallClockRollbackDoesNotReorderGenerations(t *testing.T) {
	db := newSnapshotPostgresDB(t)
	now := time.Now()
	svc := newSnapshotService(db, true)
	svc.Now = func() time.Time { return now }

	seedSnapshotItem(t, db, snapshotItemID(1), "one", "active")
	seedSnapshotFavorite(t, db, snapshotItemID(1), snapshotUserA)
	first := collectSnapshot(t, svc, snapshotUserA)

	// The clock jumps an hour BACKWARDS, then the content changes.
	now = now.Add(-time.Hour)
	seedSnapshotItem(t, db, snapshotItemID(2), "two", "active")
	seedSnapshotFavorite(t, db, snapshotItemID(2), snapshotUserA)
	second := collectSnapshot(t, svc, snapshotUserA)

	if second.Generation <= first.Generation {
		t.Fatalf("generation went backwards with the clock: %d -> %d", first.Generation, second.Generation)
	}
	if !second.GeneratedAt.Before(first.GeneratedAt) {
		t.Fatal("this test requires generatedAt to actually move backwards")
	}
	// The point: ordering by generatedAt would invert these two, ordering by
	// generation does not.
}

// F-5: the generation counter cannot be bypassed. Both halves are enforced in
// the database, so no future writer — application, migration or psql session —
// can reintroduce the inversion.
func TestSnapshot_GenerationCannotBeInvented(t *testing.T) {
	db := newSnapshotPostgresDB(t)
	svc := newSnapshotService(db, true)
	seedSnapshotItem(t, db, snapshotItemID(1), "one", "active")
	seedSnapshotFavorite(t, db, snapshotItemID(1), snapshotUserA)
	collectSnapshot(t, svc, snapshotUserA)

	t.Run("the counter may not jump", func(t *testing.T) {
		err := db.Exec(`UPDATE capability_sync_snapshot_generations SET generation = 42 WHERE principal_id = ?`,
			snapshotUserA).Error
		if err == nil {
			t.Fatal("the counter accepted a jump from 1 to 42; csc would then reject 2..41 forever")
		}
	})
	t.Run("the counter may not start high", func(t *testing.T) {
		err := db.Exec(`INSERT INTO capability_sync_snapshot_generations (principal_id, generation) VALUES (?, 42)`,
			snapshotUserB).Error
		if err == nil {
			t.Fatal("a principal was seeded at generation 42")
		}
	})
	t.Run("a manifest may not carry an unallocated generation", func(t *testing.T) {
		err := db.Exec(`INSERT INTO capability_sync_snapshots
			(id, principal_id, generation, page_count, page_size, item_count, tombstone_count,
			 snapshot_digest, content_digest, complete, expires_at)
			VALUES (?, ?, 42, 1, 2, 0, 0, repeat('a', 64), repeat('b', 64), true, now() + interval '1 hour')`,
			uuid.NewString(), snapshotUserA).Error
		if err == nil {
			t.Fatal("a manifest carrying generation 42 was accepted while the allocator held 1")
		}
	})
	t.Run("a complete snapshot is frozen", func(t *testing.T) {
		err := db.Exec(`UPDATE capability_sync_snapshots SET item_count = item_count + 1`).Error
		if err == nil {
			t.Fatal("a complete snapshot's counts were mutable")
		}
	})
	t.Run("a stored payload is immutable", func(t *testing.T) {
		err := db.Exec(`UPDATE capability_sync_snapshot_payloads SET payload = payload`).Error
		if err == nil {
			t.Fatal("a stored snapshot payload was mutable; its digest would then describe content that is gone")
		}
	})
}

// F-4: the build refuses to run outside REPEATABLE READ.
//
// Under READ COMMITTED — PostgreSQL's default — the loser of the allocation
// upsert does not abort; it blocks, takes the next number, and continues, while
// each later statement reads a fresh snapshot. The build is then not internally
// consistent, and the server can emit an item as active on one page and
// tombstoned on another: the exact state the contract calls invalid. Degrading
// is therefore not an option, because the result is indistinguishable from a
// correct snapshot to everything downstream.
func TestSnapshot_BuildRefusesOutsideRepeatableRead(t *testing.T) {
	db := newSnapshotPostgresDB(t)
	svc := newSnapshotService(db, true)
	seedSnapshotItem(t, db, snapshotItemID(1), "one", "active")
	seedSnapshotFavorite(t, db, snapshotItemID(1), snapshotUserA)

	svc.isolation = func() *sql.TxOptions { return &sql.TxOptions{Isolation: sql.LevelReadCommitted} }
	_, _, err := svc.EnsureSnapshot(context.Background(), snapshotUserA)
	if err == nil {
		t.Fatal("a build under READ COMMITTED must fail, not degrade")
	}
	if !strings.Contains(err.Error(), "REPEATABLE READ") {
		t.Fatalf("err = %v, want it to name the isolation requirement", err)
	}
	var manifests int64
	db.Table("capability_sync_snapshots").Count(&manifests)
	if manifests != 0 {
		t.Fatalf("the refused build still wrote %d manifests", manifests)
	}

	// And the default path does satisfy it.
	svc.isolation = nil
	if _, _, err := svc.EnsureSnapshot(context.Background(), snapshotUserA); err != nil {
		t.Fatalf("the production isolation must satisfy its own assertion: %v", err)
	}
}

// F-1: a snapshot is frozen. Data changing MID-PAGINATION does not move the
// pages a client is already collecting, and its digest still verifies.
//
// This is the defect the whole slice exists for: recomputing page N from live
// tables made every concurrent write invalidate an in-flight pagination, so a
// busy account could fail to converge indefinitely.
func TestSnapshot_PagesAreFrozenWhileDataChanges(t *testing.T) {
	db := newSnapshotPostgresDB(t)
	svc := newSnapshotService(db, true)
	behavior := NewBehaviorService(db)
	ctx := context.Background()

	for i := 1; i <= 5; i++ {
		seedSnapshotItem(t, db, snapshotItemID(i), fmt.Sprintf("item-%d", i), "active")
		seedSnapshotFavorite(t, db, snapshotItemID(i), snapshotUserA)
	}

	snapshot, reused, err := svc.EnsureSnapshot(ctx, snapshotUserA)
	if err != nil {
		t.Fatalf("ensure: %v", err)
	}
	first, err := svc.PageOfSnapshot(ctx, snapshot, 0, reused)
	if err != nil {
		t.Fatalf("page 0: %v", err)
	}
	if first.PageCount < 3 {
		t.Fatalf("this test needs a multi-page snapshot, got %d", first.PageCount)
	}

	collected := collectedSnapshot{
		SnapshotID: first.SnapshotID, Generation: first.Generation, SnapshotDigest: first.SnapshotDigest,
		PageCount: first.PageCount, ItemCount: first.ItemCount, TombstoneCount: first.TombstoneCount,
		GeneratedAt: first.GeneratedAt,
	}
	for _, item := range first.Items {
		collected.Items = append(collected.Items, append(json.RawMessage(nil), item...))
	}
	collected.Complete = first.Complete

	// Between page 0 and the rest: one entitlement disappears and a new one
	// appears. Under the old design this is the moment the client's reassembly
	// stopped matching the digest.
	if _, removed, err := behavior.UnfavoriteItem(ctx, snapshotItemID(3), snapshotUserA); err != nil || !removed {
		t.Fatalf("mid-pagination unfavorite: removed=%v err=%v", removed, err)
	}
	seedSnapshotItem(t, db, snapshotItemID(9), "late", "active")
	seedSnapshotFavorite(t, db, snapshotItemID(9), snapshotUserA)

	for index := 1; index < first.PageCount; index++ {
		page, err := svc.GetSnapshotPage(ctx, snapshotUserA, first.SnapshotID, index)
		if err != nil {
			t.Fatalf("page %d after the data moved: %v", index, err)
		}
		if page.SnapshotDigest != collected.SnapshotDigest || page.ItemCount != collected.ItemCount ||
			page.PageCount != collected.PageCount || page.Generation != collected.Generation {
			t.Fatalf("page %d's manifest drifted mid-pagination: %+v", index, page)
		}
		for _, item := range page.Items {
			collected.Items = append(collected.Items, append(json.RawMessage(nil), item...))
		}
		for _, tombstone := range page.Tombstones {
			collected.Tombstones = append(collected.Tombstones, append(json.RawMessage(nil), tombstone...))
		}
		collected.Complete = page.Complete
	}
	if err := verifyCollected(t, collected); err != nil {
		t.Fatalf("a snapshot collected across concurrent writes must still verify: %v", err)
	}

	// The next build sees the new world and gets a strictly newer generation.
	next := collectSnapshot(t, svc, snapshotUserA)
	if next.Generation <= collected.Generation {
		t.Fatalf("generation did not advance after the data changed: %d -> %d", collected.Generation, next.Generation)
	}
}

// F-9: two concurrent builds for one principal. The review's only evidence for
// this path was an ad-hoc psql session; this is the repository's copy.
//
// Whatever the interleaving, generations must be distinct and strictly ordered,
// each snapshot must verify, and the newer generation must not describe older
// data.
func TestSnapshot_ConcurrentBuildsDoNotInvert(t *testing.T) {
	db := newSnapshotPostgresDB(t)
	seedSnapshotItem(t, db, snapshotItemID(1), "one", "active")
	seedSnapshotFavorite(t, db, snapshotItemID(1), snapshotUserA)

	// Give each racer its own service so nothing is shared but the database.
	const racers = 4
	results := make([]collectedSnapshot, racers)
	errs := make([]error, racers)
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < racers; i++ {
		wg.Add(1)
		go func(index int) {
			defer wg.Done()
			svc := newSnapshotService(db, true)
			<-start
			snapshot, reused, err := svc.EnsureSnapshot(context.Background(), snapshotUserA)
			if err != nil {
				errs[index] = err
				return
			}
			page, err := svc.PageOfSnapshot(context.Background(), snapshot, 0, reused)
			if err != nil {
				errs[index] = err
				return
			}
			results[index] = collectedSnapshot{SnapshotID: page.SnapshotID, Generation: page.Generation}
		}(i)
	}
	close(start)
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("racer %d: %v", i, err)
		}
	}
	// Identical content: every racer must converge on ONE snapshot. Allocating
	// per racer would burn generations for a state that never changed.
	for i := 1; i < racers; i++ {
		if results[i].SnapshotID != results[0].SnapshotID || results[i].Generation != results[0].Generation {
			t.Fatalf("racers disagreed on the current snapshot: %+v vs %+v", results[0], results[i])
		}
	}
	var counter int64
	db.Table("capability_sync_snapshot_generations").Where("principal_id = ?", snapshotUserA).
		Select("generation").Scan(&counter)
	if counter != 1 {
		t.Fatalf("%d concurrent builds of identical content advanced the counter to %d", racers, counter)
	}

	// Now race builds that each observe a DIFFERENT world.
	var mu sync.Mutex
	observed := map[int64]string{}
	wg = sync.WaitGroup{}
	for i := 0; i < racers; i++ {
		wg.Add(1)
		go func(index int) {
			defer wg.Done()
			svc := newSnapshotService(db, true)
			itemID := snapshotItemID(100 + index)
			if err := db.Exec(`INSERT INTO capability_items (id, item_type, slug, name, version, status)
				VALUES (?, 'skill', ?, ?, '1.0.0', 'active')`, itemID, itemID, itemID).Error; err != nil {
				return
			}
			if err := db.Exec(`INSERT INTO item_favorites (item_id, user_id) VALUES (?, ?)`,
				itemID, snapshotUserA).Error; err != nil {
				return
			}
			snapshot, _, err := svc.EnsureSnapshot(context.Background(), snapshotUserA)
			if err != nil {
				mu.Lock()
				errs[index] = err
				mu.Unlock()
				return
			}
			mu.Lock()
			observed[snapshot.Generation] = snapshot.ID
			mu.Unlock()
		}(i)
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("racer %d (changing content): %v", i, err)
		}
	}
	// One generation may never name two snapshots.
	seen := map[string]int64{}
	for generation, snapshotID := range observed {
		if previous, duplicate := seen[snapshotID]; duplicate {
			t.Fatalf("snapshot %s served under generations %d and %d", snapshotID, previous, generation)
		}
		seen[snapshotID] = generation
	}

	// And the final state is coherent: exactly one complete snapshot survives at
	// the counter's generation, and it verifies.
	final := collectSnapshot(t, newSnapshotService(db, true), snapshotUserA)
	if err := verifyCollected(t, final); err != nil {
		t.Fatalf("the converged snapshot must verify: %v", err)
	}
	if final.ItemCount != 1+racers {
		t.Fatalf("converged item count = %d, want %d", final.ItemCount, 1+racers)
	}
}

// F-9, deterministically. The review's claim that REPEATABLE READ turns a lost
// allocation race into an abort (rather than READ COMMITTED's "block, then take
// the next number and carry on with a stale view") rested on an ad-hoc psql
// session. Two real transactions, driven in a fixed order, so the property is
// asserted rather than remembered.
//
// This is the mechanism that makes "generation order == data order" true: the
// loser cannot commit a newer number over an older view, because it does not
// commit at all.
func TestSnapshot_AllocationAbortsTheLoserUnderRepeatableRead(t *testing.T) {
	db := newSnapshotPostgresDB(t)

	// Establish the counter row first, so both racers take the UPDATE branch —
	// the branch a repair script or a second worker would actually hit.
	if err := db.Transaction(func(tx *gorm.DB) error {
		_, err := allocateSnapshotGeneration(tx, snapshotUserA, time.Now())
		return err
	}, repeatableRead()); err != nil {
		t.Fatalf("seed counter: %v", err)
	}

	sqlDB, err := db.DB()
	if err != nil {
		t.Fatalf("raw handle: %v", err)
	}
	ctx := context.Background()
	txA, err := sqlDB.BeginTx(ctx, repeatableRead())
	if err != nil {
		t.Fatalf("begin A: %v", err)
	}
	defer func() { _ = txA.Rollback() }()
	txB, err := sqlDB.BeginTx(ctx, repeatableRead())
	if err != nil {
		t.Fatalf("begin B: %v", err)
	}
	defer func() { _ = txB.Rollback() }()

	const allocate = `
		INSERT INTO capability_sync_snapshot_generations (principal_id, generation, last_allocated_at)
		VALUES ($1, 1, now())
		ON CONFLICT (principal_id) DO UPDATE
			SET generation = capability_sync_snapshot_generations.generation + 1,
			    last_allocated_at = now(), updated_at = now()
		RETURNING generation`

	// Both transactions establish their snapshot before either allocates, which
	// is the interleaving that could otherwise produce a newer number over older
	// data.
	var probe int
	if err := txA.QueryRowContext(ctx, "SELECT 1").Scan(&probe); err != nil {
		t.Fatalf("A probe: %v", err)
	}
	if err := txB.QueryRowContext(ctx, "SELECT 1").Scan(&probe); err != nil {
		t.Fatalf("B probe: %v", err)
	}

	var generationA int64
	if err := txA.QueryRowContext(ctx, allocate, snapshotUserA).Scan(&generationA); err != nil {
		t.Fatalf("A allocate: %v", err)
	}

	blocked := make(chan error, 1)
	go func() {
		var generationB int64
		blocked <- txB.QueryRowContext(ctx, allocate, snapshotUserA).Scan(&generationB)
	}()

	// B must be waiting on A's row lock, not racing ahead.
	select {
	case err := <-blocked:
		t.Fatalf("B completed before A committed (err=%v); the allocator is not serializing one principal's builds", err)
	case <-time.After(200 * time.Millisecond):
	}

	if err := txA.Commit(); err != nil {
		t.Fatalf("A commit: %v", err)
	}
	errB := <-blocked
	if errB == nil {
		t.Fatal("B survived losing the race; under READ COMMITTED it would take the next number " +
			"and continue over a data view that predates A's commit, which is exactly the inversion " +
			"REPEATABLE READ is here to prevent")
	}
	if !isSerializationFailure(errB) {
		t.Fatalf("B failed with %v, want a serialization failure the retry loop can recognize", errB)
	}
}

// A superseded snapshot stays servable for a grace window, then goes away.
//
// Deleting it the instant a newer one exists would re-introduce the livelock in
// a second form: a client halfway through paging generation N is told the
// snapshot is gone, restarts on N+1, and on an account whose content changes
// often is interrupted again. Finishing a slightly stale but internally
// consistent and complete pass is exactly what the contract wants; what it
// forbids is a pass assembled from several data states.
//
// The cost is bounded — at most one extra stored payload per principal — and
// the storage is reclaimed by the same expiry sweep that already existed.
func TestSnapshot_SupersededSnapshotSurvivesItsGraceWindow(t *testing.T) {
	db := newSnapshotPostgresDB(t)
	now := time.Now()
	svc := newSnapshotService(db, true)
	svc.Now = func() time.Time { return now }
	ctx := context.Background()

	for i := 1; i <= 5; i++ {
		seedSnapshotItem(t, db, snapshotItemID(i), fmt.Sprintf("item-%d", i), "active")
		seedSnapshotFavorite(t, db, snapshotItemID(i), snapshotUserA)
	}
	first := collectSnapshot(t, svc, snapshotUserA)
	if first.PageCount < 3 {
		t.Fatalf("this test needs a multi-page snapshot, got %d", first.PageCount)
	}

	// A new generation supersedes it.
	seedSnapshotItem(t, db, snapshotItemID(9), "late", "active")
	seedSnapshotFavorite(t, db, snapshotItemID(9), snapshotUserA)
	second := collectSnapshot(t, svc, snapshotUserA)
	if second.SnapshotID == first.SnapshotID {
		t.Fatal("the content changed; a new snapshot must have been built")
	}

	// Inside the grace window the older snapshot still pages.
	if _, err := svc.GetSnapshotPage(ctx, snapshotUserA, first.SnapshotID, 1); err != nil {
		t.Fatalf("a superseded snapshot must remain servable during its grace window: %v", err)
	}

	// Past it, the id is refused rather than answered from newer data.
	now = now.Add(snapshotSupersededGrace + time.Minute)
	if _, err := svc.GetSnapshotPage(ctx, snapshotUserA, first.SnapshotID, 1); err != ErrSnapshotNotFound {
		t.Fatalf("err = %v, want ErrSnapshotNotFound once the grace window has passed", err)
	}

	// Reclamation is best effort and lags refusal, deliberately: a poll that
	// changes nothing re-serves the current snapshot and does NOT sweep, so an
	// expired row can outlive the moment it stopped being servable. That is
	// safe — it is unreachable either way — and it keeps a polling fleet from
	// running a DELETE on a shared table thousands of times between two content
	// changes.
	if _, reused, err := svc.EnsureSnapshot(ctx, snapshotUserA); err != nil {
		t.Fatalf("poll after expiry: %v", err)
	} else if !reused {
		t.Fatal("nothing changed; this poll should have re-served the current snapshot")
	}

	// The next real build sweeps, payload and all.
	seedSnapshotItem(t, db, snapshotItemID(11), "later", "active")
	seedSnapshotFavorite(t, db, snapshotItemID(11), snapshotUserA)
	if _, reused, err := svc.EnsureSnapshot(ctx, snapshotUserA); err != nil {
		t.Fatalf("build after expiry: %v", err)
	} else if reused {
		t.Fatal("the content changed; this must have been a real build")
	}
	var manifests, payloads int64
	db.Table("capability_sync_snapshots").Where("id = ?", first.SnapshotID).Count(&manifests)
	db.Table("capability_sync_snapshot_payloads").Where("snapshot_id = ?", first.SnapshotID).Count(&payloads)
	if manifests != 0 || payloads != 0 {
		t.Fatalf("expired snapshot left %d manifests and %d payloads behind", manifests, payloads)
	}
}

// -----------------------------------------------------------------------------
// Lifecycle propagation kill switch
// -----------------------------------------------------------------------------

// With propagation OFF a Git-archived item is neither active nor tombstoned, so
// the client sees absence — which by the core invariant is a no-op. Turning the
// switch off can therefore never delete anything, which is what makes it usable
// as an emergency control.
func TestSnapshot_LifecyclePropagationKillSwitch(t *testing.T) {
	db := newSnapshotPostgresDB(t)
	item := snapshotItemID(1)
	seedSnapshotItem(t, db, item, "one", "active")
	seedSnapshotFavorite(t, db, item, snapshotUserA)

	if err := db.Transaction(func(tx *gorm.DB) error {
		if err := tx.Exec(`UPDATE capability_items SET status='archived' WHERE id = ?`, item).Error; err != nil {
			return err
		}
		_, err := RecordGitArchiveTombstonesTx(tx, item, models.GitLifecycleReasonManifestRemoved, time.Now())
		return err
	}); err != nil {
		t.Fatalf("archive: %v", err)
	}

	off := collectSnapshot(t, newSnapshotService(db, false), snapshotUserA)
	if err := verifyCollected(t, off); err != nil {
		t.Fatalf("verify with propagation off: %v", err)
	}
	if len(off.Tombstones) != 0 {
		t.Fatalf("propagation is off but the snapshot carried %d tombstones", len(off.Tombstones))
	}
	if len(off.Items) != 0 {
		t.Fatal("an archived item must not be reported as active either")
	}

	on := collectSnapshot(t, newSnapshotService(db, true), snapshotUserA)
	if err := verifyCollected(t, on); err != nil {
		t.Fatalf("verify with propagation on: %v", err)
	}
	if len(on.Tombstones) != 1 {
		t.Fatalf("propagation is on but the snapshot carried %d tombstones", len(on.Tombstones))
	}
	if on.Generation <= off.Generation {
		t.Fatalf("flipping the switch changed what a client observes, so it must allocate a new generation: %d -> %d",
			off.Generation, on.Generation)
	}
	// The stored tombstone was never deleted by the switch — only hidden.
	var stored int64
	db.Table("capability_sync_tombstones").Where("user_id = ?", snapshotUserA).Count(&stored)
	if stored != 1 {
		t.Fatalf("the kill switch must not delete tombstones; %d rows remain", stored)
	}
}

// -----------------------------------------------------------------------------
// Isolation between principals
// -----------------------------------------------------------------------------

func TestSnapshot_PrincipalsDoNotSeeEachOther(t *testing.T) {
	db := newSnapshotPostgresDB(t)
	svc := newSnapshotService(db, true)

	mine, theirs := snapshotItemID(1), snapshotItemID(2)
	seedSnapshotItem(t, db, mine, "mine", "active")
	seedSnapshotItem(t, db, theirs, "theirs", "active")
	seedSnapshotFavorite(t, db, mine, snapshotUserA)
	seedSnapshotFavorite(t, db, theirs, snapshotUserB)

	a := collectSnapshot(t, svc, snapshotUserA)
	b := collectSnapshot(t, svc, snapshotUserB)
	if len(a.Items) != 1 || len(b.Items) != 1 {
		t.Fatalf("A saw %d items, B saw %d", len(a.Items), len(b.Items))
	}
	if _, leaked := itemIDs(t, a)[theirs]; leaked {
		t.Fatal("principal A received principal B's entitlement")
	}
	// Generations are per principal, so both legitimately start at 1.
	if a.Generation != 1 || b.Generation != 1 {
		t.Fatalf("generations are not per principal: A=%d B=%d", a.Generation, b.Generation)
	}
}
