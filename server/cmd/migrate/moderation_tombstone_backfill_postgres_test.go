package main

import (
	"bytes"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// PostgreSQL-only by construction: the command relies on UNION-ed subqueries,
// a correlated NOT EXISTS inside the inserting statement, gen_random_uuid() and
// ON CONFLICT. It is tested against the engine it runs on, and against the real
// tombstone CHECK constraint — a backfill that writes a row the constraint
// rejects would fail in production and nowhere else.

func newModerationBackfillPostgresDB(t *testing.T) *gorm.DB {
	t.Helper()
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		t.Skip("DATABASE_URL not set; skipping PostgreSQL moderation backfill test")
	}
	parsed, err := url.Parse(dsn)
	if err != nil {
		t.Fatalf("parse DATABASE_URL: %v", err)
	}
	schema := fmt.Sprintf("moderation_backfill_%d", time.Now().UnixNano())
	quoted := `"` + strings.ReplaceAll(schema, `"`, `""`) + `"`

	admin, err := gorm.Open(postgres.Open(dsn), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatalf("open PostgreSQL: %v", err)
	}
	if err := admin.Exec("CREATE SCHEMA " + quoted).Error; err != nil {
		t.Fatalf("create schema: %v", err)
	}
	t.Cleanup(func() {
		_ = admin.Exec("DROP SCHEMA " + quoted + " CASCADE").Error
		if sqlDB, err := admin.DB(); err == nil {
			_ = sqlDB.Close()
		}
	})

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

	for _, ddl := range []string{
		`CREATE TABLE capability_items (
			id UUID PRIMARY KEY,
			slug TEXT NOT NULL DEFAULT '', item_type TEXT NOT NULL DEFAULT 'skill',
			status TEXT NOT NULL DEFAULT 'active',
			git_lifecycle_reason VARCHAR(32),
			updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
		)`,
		`CREATE TABLE item_favorites (
			id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
			item_id UUID NOT NULL, user_id VARCHAR(191) NOT NULL,
			UNIQUE (item_id, user_id)
		)`,
		`CREATE TABLE item_distributions (
			id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
			item_id UUID NOT NULL, status VARCHAR(32) NOT NULL DEFAULT 'active'
		)`,
		`CREATE TABLE item_distribution_receipts (
			id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
			distribution_id UUID NOT NULL, user_id TEXT NOT NULL,
			receipt_status VARCHAR(32) NOT NULL DEFAULT 'unread'
		)`,
	} {
		if err := db.Exec(ddl).Error; err != nil {
			t.Fatalf("create fixture table: %v\nSQL: %s", err, ddl)
		}
	}
	// The tombstone table comes from the migrations verbatim, constraint and
	// all. Hand-copying its DDL here is how a backfill passes its own test and
	// then trips the CHECK on the first production row.
	for _, name := range []string{
		"20260805000200_create_capability_sync_tombstones.sql",
		"20260805000700_constrain_capability_sync_tombstone_triples.sql",
		"20260806000300_extend_capability_sync_tombstone_causes.sql",
	} {
		if err := db.Exec(readModerationMigrationUp(t, name)).Error; err != nil {
			t.Fatalf("apply migration %s: %v", name, err)
		}
	}
	return db
}

func readModerationMigrationUp(t *testing.T, name string) string {
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
	body = strings.ReplaceAll(body, "-- +goose StatementBegin", "")
	return strings.ReplaceAll(body, "-- +goose StatementEnd", "")
}

func moderationItemID(n int) string {
	return fmt.Sprintf("%08d-0000-4000-8000-000000000000", n)
}

func seedModerationItem(t *testing.T, db *gorm.DB, id, status string, lifecycleReason any) {
	t.Helper()
	if err := db.Exec(`INSERT INTO capability_items (id, slug, status, git_lifecycle_reason, updated_at)
		VALUES (?, ?, ?, ?, now() - interval '1 day')`, id, "slug-"+id[:8], status, lifecycleReason).Error; err != nil {
		t.Fatalf("seed item: %v", err)
	}
}

func seedModerationFavorite(t *testing.T, db *gorm.DB, itemID, userID string) {
	t.Helper()
	if err := db.Exec(`INSERT INTO item_favorites (item_id, user_id) VALUES (?, ?)`, itemID, userID).Error; err != nil {
		t.Fatalf("seed favorite: %v", err)
	}
}

func seedModerationReceipt(t *testing.T, db *gorm.DB, itemID, userID, distStatus, receiptStatus string) {
	t.Helper()
	var distributionID string
	if err := db.Raw(`INSERT INTO item_distributions (item_id, status) VALUES (?, ?) RETURNING id`,
		itemID, distStatus).Row().Scan(&distributionID); err != nil {
		t.Fatalf("seed distribution: %v", err)
	}
	if err := db.Exec(`INSERT INTO item_distribution_receipts (distribution_id, user_id, receipt_status)
		VALUES (?, ?, ?)`, distributionID, userID, receiptStatus).Error; err != nil {
		t.Fatalf("seed receipt: %v", err)
	}
}

// TestModerationTombstoneBackfill_PlanThenApply walks the whole command: what
// the dry run promises, that it writes nothing, that --confirm writes exactly
// what was promised, and that a second run is a no-op.
//
// The last part is not decoration. Re-running must not touch existing rows,
// because rewriting a tombstone rotates its event id and a device that already
// applied the removal would re-run removal work on every poll afterwards.
func TestModerationTombstoneBackfill_PlanThenApply(t *testing.T) {
	db := newModerationBackfillPostgresDB(t)

	archived := moderationItemID(1)
	banned := moderationItemID(2)
	active := moderationItemID(3)
	gitArchived := moderationItemID(4)
	alreadyDone := moderationItemID(5)
	noHolders := moderationItemID(6)
	dismissed := moderationItemID(7)

	seedModerationItem(t, db, archived, "archived", nil)
	seedModerationFavorite(t, db, archived, "u-fav")
	seedModerationReceipt(t, db, archived, "u-dist", "active", "unread")
	// One principal holding BOTH relationships must still yield one row; the
	// UNIQUE (user_id, item_id) constraint would otherwise fail the insert.
	seedModerationFavorite(t, db, archived, "u-both")
	seedModerationReceipt(t, db, archived, "u-both", "active", "read")

	seedModerationItem(t, db, banned, "banned", nil)
	seedModerationFavorite(t, db, banned, "u-fav")

	// Still on the shelf: nothing ended.
	seedModerationItem(t, db, active, "active", nil)
	seedModerationFavorite(t, db, active, "u-fav")

	// Git's take-down, whose truthful reason is git_archived — out of scope.
	seedModerationItem(t, db, gitArchived, "archived", "manifest_removed")
	seedModerationFavorite(t, db, gitArchived, "u-fav")

	// Already carries an instruction; re-stating it would rotate the event id.
	seedModerationItem(t, db, alreadyDone, "archived", nil)
	seedModerationFavorite(t, db, alreadyDone, "u-fav")
	if err := db.Exec(`INSERT INTO capability_sync_tombstones
		(user_id, item_id, reason, lifecycle_reason, source, event_id, removed_at)
		VALUES ('u-fav', ?, 'unfavorited', NULL, 'favorite', 'pre-existing-event', now())`,
		alreadyDone).Error; err != nil {
		t.Fatalf("seed existing tombstone: %v", err)
	}

	seedModerationItem(t, db, noHolders, "archived", nil)

	// Neither a dismissed receipt nor a revoked distribution is a live holder.
	seedModerationItem(t, db, dismissed, "archived", nil)
	seedModerationReceipt(t, db, dismissed, "u-gone", "active", "dismissed")
	seedModerationReceipt(t, db, dismissed, "u-revoked", "revoked", "unread")

	var plan bytes.Buffer
	report, err := runModerationTombstoneBackfill(db, moderationTombstoneBackfillOptions{ReportLimit: 50}, &plan)
	if err != nil {
		t.Fatalf("dry run: %v", err)
	}
	if report.HiddenItems != 6 {
		t.Errorf("hidden items = %d, want 6", report.HiddenItems)
	}
	// archived: u-fav, u-dist, u-both  +  banned: u-fav
	if report.EligiblePairs != 4 {
		t.Errorf("eligible pairs = %d, want 4", report.EligiblePairs)
	}
	if report.EligibleItems != 2 {
		t.Errorf("eligible items = %d, want 2 (archived + banned)", report.EligibleItems)
	}
	if report.EligibleUsers != 3 {
		t.Errorf("eligible users = %d, want 3 (u-fav, u-dist, u-both)", report.EligibleUsers)
	}
	if report.GitClaimedPairs != 1 {
		t.Errorf("git-claimed pairs = %d, want 1 reported-but-not-covered", report.GitClaimedPairs)
	}
	if report.AlreadyTombstoned != 1 {
		t.Errorf("already tombstoned = %d, want 1", report.AlreadyTombstoned)
	}
	if report.Inserted != 0 {
		t.Errorf("a dry run inserted %d rows", report.Inserted)
	}
	var rows int64
	db.Table("capability_sync_tombstones").Count(&rows)
	if rows != 1 {
		t.Fatalf("a dry run changed the table: %d rows, want the one seeded", rows)
	}
	if !strings.Contains(plan.String(), "DRY RUN") || !strings.Contains(plan.String(), "unknowable (not 0)") {
		t.Errorf("the plan must name its mode and disclose the unrecoverable gap:\n%s", plan.String())
	}

	var apply bytes.Buffer
	applied, err := runModerationTombstoneBackfill(db, moderationTombstoneBackfillOptions{Confirm: true, ReportLimit: 50}, &apply)
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if applied.Inserted != 4 {
		t.Fatalf("inserted = %d, want the 4 pairs the plan promised", applied.Inserted)
	}

	type row struct {
		UserID    string    `gorm:"column:user_id"`
		ItemID    string    `gorm:"column:item_id"`
		Reason    string    `gorm:"column:reason"`
		Source    string    `gorm:"column:source"`
		EventID   string    `gorm:"column:event_id"`
		RemovedAt time.Time `gorm:"column:removed_at"`
	}
	var written []row
	if err := db.Raw(`SELECT user_id, item_id, reason, source, event_id, removed_at
		FROM capability_sync_tombstones ORDER BY item_id, user_id`).Scan(&written).Error; err != nil {
		t.Fatalf("read tombstones: %v", err)
	}
	if len(written) != 5 {
		t.Fatalf("rows = %d, want 4 new + 1 pre-existing", len(written))
	}
	events := map[string]struct{}{}
	for _, r := range written {
		if r.ItemID == alreadyDone {
			if r.Reason != "unfavorited" || r.EventID != "pre-existing-event" {
				t.Error("the backfill overwrote an existing tombstone; that rotates the event id for a transition that did not happen")
			}
			continue
		}
		if r.Reason != "admin_archived" || r.Source != "moderation" {
			t.Errorf("%s/%s: reason %q source %q, want admin_archived/moderation", r.ItemID, r.UserID, r.Reason, r.Source)
		}
		if _, dup := events[r.EventID]; dup {
			t.Errorf("event id %s reused; csc would dedup one removal away", r.EventID)
		}
		events[r.EventID] = struct{}{}
		// removed_at is the item's updated_at (seeded a day ago), not now().
		if time.Since(r.RemovedAt) < 12*time.Hour {
			t.Errorf("%s/%s: removed_at %s looks like now(); the backfill must date the removal from the row, not from when an operator ran it",
				r.ItemID, r.UserID, r.RemovedAt)
		}
	}
	for _, absent := range []string{active, gitArchived, noHolders, dismissed} {
		var count int64
		db.Table("capability_sync_tombstones").Where("item_id = ?", absent).Count(&count)
		if count != 0 {
			t.Errorf("item %s received %d tombstones but is out of scope", absent, count)
		}
	}

	// Idempotence: the eligibility predicate now excludes everything it wrote.
	var second bytes.Buffer
	rerun, err := runModerationTombstoneBackfill(db, moderationTombstoneBackfillOptions{Confirm: true, ReportLimit: 50}, &second)
	if err != nil {
		t.Fatalf("re-run: %v", err)
	}
	if rerun.EligiblePairs != 0 || rerun.Inserted != 0 {
		t.Fatalf("re-running found %d eligible / wrote %d rows; a second run must be a no-op",
			rerun.EligiblePairs, rerun.Inserted)
	}
}

// --limit bounds a run without changing what is eligible, so an operator can
// apply a large backfill in pieces and watch the remaining count fall.
func TestModerationTombstoneBackfill_LimitAppliesInPieces(t *testing.T) {
	db := newModerationBackfillPostgresDB(t)

	for n := 1; n <= 3; n++ {
		id := moderationItemID(n)
		seedModerationItem(t, db, id, "archived", nil)
		seedModerationFavorite(t, db, id, fmt.Sprintf("u-%d", n))
	}

	var buf bytes.Buffer
	first, err := runModerationTombstoneBackfill(db,
		moderationTombstoneBackfillOptions{Confirm: true, Limit: 2, ReportLimit: 50}, &buf)
	if err != nil {
		t.Fatalf("first pass: %v", err)
	}
	if first.EligiblePairs != 3 {
		t.Errorf("eligible = %d, want the full 3 regardless of --limit", first.EligiblePairs)
	}
	if first.Inserted != 2 {
		t.Fatalf("inserted = %d, want 2", first.Inserted)
	}

	buf.Reset()
	second, err := runModerationTombstoneBackfill(db,
		moderationTombstoneBackfillOptions{Confirm: true, ReportLimit: 50}, &buf)
	if err != nil {
		t.Fatalf("second pass: %v", err)
	}
	if second.EligiblePairs != 1 || second.Inserted != 1 {
		t.Fatalf("second pass eligible = %d inserted = %d, want 1/1", second.EligiblePairs, second.Inserted)
	}
}

func TestModerationTombstoneBackfill_ArgParsing(t *testing.T) {
	cases := []struct {
		args    []string
		want    moderationTombstoneBackfillOptions
		wantErr bool
	}{
		{args: nil, want: moderationTombstoneBackfillOptions{ReportLimit: 50}},
		{args: []string{"--dry-run"}, want: moderationTombstoneBackfillOptions{ReportLimit: 50}},
		{args: []string{"--confirm"}, want: moderationTombstoneBackfillOptions{Confirm: true, ReportLimit: 50}},
		{args: []string{"--limit=10", "--report-limit=0"}, want: moderationTombstoneBackfillOptions{Limit: 10}},
		{args: []string{"--limit=-1"}, wantErr: true},
		{args: []string{"--nope"}, wantErr: true},
	}
	for _, tc := range cases {
		got, err := parseModerationTombstoneBackfillArgs(tc.args)
		if (err != nil) != tc.wantErr {
			t.Fatalf("%v: err = %v, wantErr = %v", tc.args, err, tc.wantErr)
		}
		if err == nil && got != tc.want {
			t.Errorf("%v: got %+v, want %+v", tc.args, got, tc.want)
		}
	}
}
