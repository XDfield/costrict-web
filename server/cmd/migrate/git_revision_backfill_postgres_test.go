package main

import (
	"bytes"
	"fmt"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// The backfill is PostgreSQL-only by construction: it relies on a regex match,
// a correlated NOT EXISTS inside the inserting statement and ON CONFLICT. There
// is no honest SQLite version of it, so it is tested against the engine it runs
// on.

func newBackfillPostgresDB(t *testing.T) *gorm.DB {
	t.Helper()
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		t.Skip("DATABASE_URL not set; skipping PostgreSQL backfill test")
	}
	parsed, err := url.Parse(dsn)
	if err != nil {
		t.Fatalf("parse DATABASE_URL: %v", err)
	}
	schema := fmt.Sprintf("git_backfill_%d", time.Now().UnixNano())
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
			slug TEXT NOT NULL, item_type TEXT NOT NULL, status TEXT NOT NULL DEFAULT 'active',
			version TEXT, content_backend VARCHAR(16) NOT NULL DEFAULT 'db',
			source_git_server_id VARCHAR(64) NOT NULL DEFAULT '', source_git_repo_id BIGINT NOT NULL DEFAULT 0,
			source_repo_ref VARCHAR(64), source_repo_path TEXT, source_git_entry_key TEXT NOT NULL DEFAULT '',
			git_sha VARCHAR(40) NOT NULL DEFAULT '', git_last_synced_at TIMESTAMPTZ,
			git_sync_status VARCHAR(16) NOT NULL DEFAULT ''
		)`,
		`CREATE TABLE capability_item_git_revisions (
			id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
			item_id UUID NOT NULL REFERENCES capability_items(id) ON DELETE CASCADE,
			revision_no BIGINT NOT NULL,
			git_server_id VARCHAR(64) NOT NULL, git_repo_id BIGINT NOT NULL,
			git_ref TEXT NOT NULL DEFAULT '', manifest_path TEXT NOT NULL DEFAULT '',
			entry_key TEXT NOT NULL DEFAULT '', git_sha VARCHAR(40) NOT NULL,
			version_label TEXT NOT NULL DEFAULT '', source VARCHAR(16) NOT NULL,
			content_digest VARCHAR(64),
			observed_at TIMESTAMPTZ NOT NULL, created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
			CONSTRAINT uq_capability_item_git_revisions_no UNIQUE (item_id, revision_no),
			CONSTRAINT chk_capability_item_git_revisions_no CHECK (revision_no > 0),
			CONSTRAINT chk_capability_item_git_revisions_sha CHECK (git_sha ~ '^[0-9a-f]{40}$'),
			CONSTRAINT chk_capability_item_git_revisions_source
				CHECK (source IN ('backfill', 'provision', 'push', 'reconcile', 'restore')),
			CONSTRAINT chk_capability_item_git_revisions_digest_format
				CHECK (content_digest IS NULL OR content_digest ~ '^[0-9a-f]{64}$'),
			CONSTRAINT chk_capability_item_git_revisions_digest_source
				CHECK (content_digest IS NOT NULL OR source = 'backfill')
		)`,
	} {
		if err := db.Exec(ddl).Error; err != nil {
			t.Fatalf("create fixture: %v", err)
		}
	}
	return db
}

func uuidN(n int) string { return fmt.Sprintf("00000000-0000-0000-0000-%012d", n) }

func seedBackfillItem(t *testing.T, db *gorm.DB, n int, backend, syncStatus, sha, version string, synced *time.Time) string {
	t.Helper()
	id := uuidN(n)
	if err := db.Exec(`INSERT INTO capability_items
		(id, slug, item_type, version, content_backend, source_git_server_id, source_git_repo_id,
		 source_repo_ref, source_repo_path, source_git_entry_key, git_sha, git_last_synced_at, git_sync_status)
		VALUES (?, ?, 'skill', ?, ?, 'gs-1', 77, 'main', 'SKILL.md', '', ?, ?, ?)`,
		id, fmt.Sprintf("item-%d", n), version, backend, sha, synced, syncStatus).Error; err != nil {
		t.Fatalf("seed item %d: %v", n, err)
	}
	return id
}

func TestGitRevisionBackfill_SeedsOnlySyncedRowsWithNoHistory(t *testing.T) {
	db := newBackfillPostgresDB(t)
	syncedAt := time.Date(2026, 3, 4, 5, 6, 7, 0, time.UTC)
	later := syncedAt.Add(time.Hour)

	eligible := seedBackfillItem(t, db, 1, "git", "synced", strings.Repeat("a", 40), "1.2.3", &syncedAt)
	noVersion := seedBackfillItem(t, db, 2, "git", "synced", strings.Repeat("b", 40), "", &later)
	// Deliberately ineligible, each for its own contractual reason.
	orphaned := seedBackfillItem(t, db, 3, "git", "orphaned", strings.Repeat("c", 40), "1.0.0", &syncedAt)
	errored := seedBackfillItem(t, db, 4, "git", "error", strings.Repeat("d", 40), "1.0.0", &syncedAt)
	dbBacked := seedBackfillItem(t, db, 5, "db", "", "", "1.0.0", nil)
	noSHA := seedBackfillItem(t, db, 6, "git", "synced", "", "1.0.0", &syncedAt)
	neverSynced := seedBackfillItem(t, db, 7, "git", "synced", strings.Repeat("e", 40), "1.0.0", nil)
	// Already has history: the sync writer got there first.
	hasHistory := seedBackfillItem(t, db, 8, "git", "synced", strings.Repeat("f", 40), "1.0.0", &syncedAt)
	if err := db.Exec(`INSERT INTO capability_item_git_revisions
		(item_id, revision_no, git_server_id, git_repo_id, git_sha, source, content_digest, observed_at)
		VALUES (?, 1, 'gs-1', 77, ?, 'push', ?, now())`,
		hasHistory, strings.Repeat("f", 40), strings.Repeat("9", 64)).Error; err != nil {
		t.Fatalf("seed existing history: %v", err)
	}

	// Dry run first: it must report the plan and write nothing.
	var out bytes.Buffer
	report, err := runGitRevisionBackfill(db, gitRevisionBackfillOptions{ReportLimit: 100}, &out)
	if err != nil {
		t.Fatalf("dry run: %v", err)
	}
	if report.Eligible != 2 || report.Selected != 2 {
		t.Fatalf("dry run eligible/selected = %d/%d, want 2/2\n%s", report.Eligible, report.Selected, out.String())
	}
	if report.GitBacked != 7 || report.AlreadyHaveRows != 1 {
		t.Fatalf("dry run counters = git:%d history:%d, want 7/1", report.GitBacked, report.AlreadyHaveRows)
	}
	if report.Inserted != 0 {
		t.Fatalf("dry run inserted %d rows", report.Inserted)
	}
	if !strings.Contains(out.String(), eligible) || !strings.Contains(out.String(), noVersion) {
		t.Fatalf("dry run report omitted a candidate:\n%s", out.String())
	}
	var written int64
	if err := db.Raw(`SELECT COUNT(*) FROM capability_item_git_revisions`).Scan(&written).Error; err != nil {
		t.Fatalf("count: %v", err)
	}
	if written != 1 {
		t.Fatalf("dry run changed the database: %d revision rows, want 1", written)
	}

	// Apply.
	out.Reset()
	report, err = runGitRevisionBackfill(db, gitRevisionBackfillOptions{Confirm: true, ReportLimit: 100}, &out)
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if report.Inserted != 2 {
		t.Fatalf("inserted = %d, want 2", report.Inserted)
	}

	type row struct {
		ItemID       string    `gorm:"column:item_id"`
		RevisionNo   int64     `gorm:"column:revision_no"`
		GitSHA       string    `gorm:"column:git_sha"`
		VersionLabel string    `gorm:"column:version_label"`
		Source       string    `gorm:"column:source"`
		ObservedAt   time.Time `gorm:"column:observed_at"`
		GitRef       string    `gorm:"column:git_ref"`
		ManifestPath string    `gorm:"column:manifest_path"`
	}
	var seeded row
	if err := db.Raw(`SELECT * FROM capability_item_git_revisions WHERE item_id = ?`, eligible).Scan(&seeded).Error; err != nil {
		t.Fatalf("load seeded revision: %v", err)
	}
	if seeded.RevisionNo != 1 || seeded.Source != "backfill" {
		t.Fatalf("seeded revision = %+v, want revision 1 with source=backfill", seeded)
	}
	if seeded.GitSHA != strings.Repeat("a", 40) || seeded.VersionLabel != "1.2.3" {
		t.Fatalf("seeded revision = %+v, want the item's own SHA/version", seeded)
	}
	// The row states when the projection actually succeeded, not when the
	// migration ran.
	if !seeded.ObservedAt.UTC().Equal(syncedAt) {
		t.Fatalf("observed_at = %s, want the item's git_last_synced_at %s", seeded.ObservedAt.UTC(), syncedAt)
	}
	if seeded.GitRef != "main" || seeded.ManifestPath != "SKILL.md" {
		t.Fatalf("seeded coordinate = %+v, want main/SKILL.md", seeded)
	}

	// A seeded baseline carries NO content digest, and says so as SQL NULL
	// rather than as an empty string: the append trigger is the item's projected
	// content digest, a Git-backed row does not store its content, and this
	// command never talks to Gitea. NULL is what the first successful projection
	// looks for before adopting the digest it observes (and appending nothing);
	// an invented value would instead make that projection look like a change.
	var digestIsNull bool
	if err := db.Raw(`SELECT content_digest IS NULL FROM capability_item_git_revisions WHERE item_id = ?`, eligible).
		Row().Scan(&digestIsNull); err != nil {
		t.Fatalf("read seeded digest: %v", err)
	}
	if !digestIsNull {
		t.Fatal("a backfilled baseline must record an unobserved digest as NULL")
	}
	if report.AwaitingDigest != 2 {
		t.Fatalf("awaiting-digest counter = %d, want 2", report.AwaitingDigest)
	}

	// A manifest with no declared version stores the empty label; presenting a
	// non-empty value is the read API's job, not the backfill's.
	var emptyLabel string
	if err := db.Raw(`SELECT version_label FROM capability_item_git_revisions WHERE item_id = ?`, noVersion).
		Row().Scan(&emptyLabel); err != nil {
		t.Fatalf("load version label: %v", err)
	}
	if emptyLabel != "" {
		t.Fatalf("version_label = %q, want the empty label preserved", emptyLabel)
	}

	for _, id := range []string{orphaned, errored, dbBacked, noSHA, neverSynced} {
		var count int64
		if err := db.Raw(`SELECT COUNT(*) FROM capability_item_git_revisions WHERE item_id = ?`, id).
			Scan(&count).Error; err != nil {
			t.Fatalf("count for %s: %v", id, err)
		}
		if count != 0 {
			t.Fatalf("ineligible item %s received %d revision(s)", id, count)
		}
	}

	// The pre-existing history is untouched and un-renumbered.
	var existing int64
	if err := db.Raw(`SELECT COUNT(*) FROM capability_item_git_revisions WHERE item_id = ? AND source = 'push'`, hasHistory).
		Scan(&existing).Error; err != nil {
		t.Fatalf("count existing: %v", err)
	}
	if existing != 1 {
		t.Fatalf("existing history rows = %d, want 1", existing)
	}

	// Re-running is a no-op: every candidate now has history.
	out.Reset()
	report, err = runGitRevisionBackfill(db, gitRevisionBackfillOptions{Confirm: true, ReportLimit: 100}, &out)
	if err != nil {
		t.Fatalf("second apply: %v", err)
	}
	if report.Eligible != 0 || report.Inserted != 0 {
		t.Fatalf("second apply eligible/inserted = %d/%d, want 0/0", report.Eligible, report.Inserted)
	}
}

func TestParseGitRevisionBackfillArgs(t *testing.T) {
	if opts, err := parseGitRevisionBackfillArgs(nil); err != nil || opts.Confirm {
		t.Fatalf("default = %+v, %v; want a dry run", opts, err)
	}
	if opts, err := parseGitRevisionBackfillArgs([]string{"--dry-run"}); err != nil || opts.Confirm {
		t.Fatalf("--dry-run = %+v, %v; want a dry run", opts, err)
	}
	opts, err := parseGitRevisionBackfillArgs([]string{"--confirm", "--limit=10", "--report-limit=0"})
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if !opts.Confirm || opts.Limit != 10 || opts.ReportLimit != 0 {
		t.Fatalf("parsed = %+v", opts)
	}
	for _, bad := range [][]string{{"--limit=-1"}, {"--limit=x"}, {"--nope"}} {
		if _, err := parseGitRevisionBackfillArgs(bad); err == nil {
			t.Fatalf("%v should have been rejected", bad)
		}
	}
}
