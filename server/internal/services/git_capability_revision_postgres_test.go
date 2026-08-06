package services

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/costrict/costrict-web/server/internal/gitserver"
	"github.com/costrict/costrict-web/server/internal/gitsync"
	"github.com/costrict/costrict-web/server/internal/models"
	"github.com/google/uuid"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// These tests need a real PostgreSQL because the thing under test is a
// PostgreSQL mechanism: SELECT ... FOR UPDATE on capability_items is what makes
// revision numbering safe, and SQLite — which serializes every writer anyway —
// cannot tell a working lock from a missing one.

const (
	pgRevisionSHA1 = "1111111111111111111111111111111111111111"
	pgRevisionSHA2 = "2222222222222222222222222222222222222222"
	pgRevisionSHA3 = "3333333333333333333333333333333333333333"
	pgRevisionSHA4 = "4444444444444444444444444444444444444444"
	pgRevisionSHA5 = "5555555555555555555555555555555555555555"
	pgRevisionSHA6 = "6666666666666666666666666666666666666666"
	pgRevisionSHA7 = "7777777777777777777777777777777777777777"
)

// pgRevisionDigest builds a well-formed 64-hex digest for the tests that drive
// projectGitCapabilityRevision directly rather than through the sync service.
func pgRevisionDigest(marker string) string {
	return strings.Repeat(marker, 64)
}

// gitRevisionPostgresFixture is the subset of the schema SyncRepository writes
// through, in production column types. It deliberately mirrors the real DDL
// (including the unique constraint and the CHECKs) rather than a permissive
// approximation: a test schema that accepts what production rejects proves
// nothing about production.
var gitRevisionPostgresFixture = []string{
	`CREATE TABLE capability_items (
		id                   UUID PRIMARY KEY,
		registry_id          TEXT NOT NULL,
		repo_id              TEXT NOT NULL,
		slug                 TEXT NOT NULL,
		item_type            TEXT NOT NULL,
		name                 TEXT NOT NULL,
		description          TEXT NOT NULL DEFAULT '',
		category             TEXT NOT NULL DEFAULT '',
		version              TEXT NOT NULL DEFAULT '',
		metadata             JSONB NOT NULL DEFAULT '{}',
		source_repo_url      TEXT NOT NULL DEFAULT '',
		source_repo_ref      VARCHAR(64) NOT NULL DEFAULT 'main',
		source_repo_path     TEXT NOT NULL DEFAULT '',
		source_sha           TEXT NOT NULL DEFAULT '',
		content_backend      VARCHAR(16) NOT NULL DEFAULT 'db',
		source_git_server_id VARCHAR(64) NOT NULL DEFAULT '',
		source_git_repo_id   BIGINT NOT NULL DEFAULT 0,
		source_git_entry_key TEXT NOT NULL DEFAULT '',
		git_sha              VARCHAR(40) NOT NULL DEFAULT '',
		git_last_synced_at   TIMESTAMPTZ,
		git_sync_status      VARCHAR(16) NOT NULL DEFAULT '',
		git_sync_error       TEXT NOT NULL DEFAULT '',
		status               TEXT NOT NULL DEFAULT 'active',
		created_by           TEXT NOT NULL DEFAULT '',
		created_at           TIMESTAMPTZ NOT NULL DEFAULT now(),
		updated_at           TIMESTAMPTZ NOT NULL DEFAULT now()
	)`,
	`CREATE TABLE git_capability_repositories (
		id                 UUID PRIMARY KEY,
		git_server_id      VARCHAR(64) NOT NULL,
		git_repo_id        BIGINT NOT NULL,
		last_error         TEXT NOT NULL DEFAULT '',
		updated_at         TIMESTAMPTZ NOT NULL DEFAULT now()
	)`,
	`CREATE TABLE git_capability_sync_jobs (
		id            UUID PRIMARY KEY,
		git_server_id VARCHAR(64) NOT NULL,
		delivery_id   VARCHAR(128) NOT NULL,
		repo_id       BIGINT NOT NULL,
		repo_full_name TEXT NOT NULL,
		default_branch TEXT NOT NULL,
		ref           TEXT NOT NULL,
		before_sha    TEXT NOT NULL DEFAULT '',
		after_sha     TEXT NOT NULL,
		status        VARCHAR(32) NOT NULL,
		retry_count   INT NOT NULL DEFAULT 0,
		max_attempts  INT NOT NULL DEFAULT 3,
		last_error    TEXT,
		scheduled_at  TIMESTAMPTZ NOT NULL,
		started_at    TIMESTAMPTZ,
		lease_token   VARCHAR(36) NOT NULL DEFAULT '',
		finished_at   TIMESTAMPTZ,
		created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
		UNIQUE (git_server_id, delivery_id)
	)`,
	`CREATE TABLE capability_item_git_revisions (
		id            UUID PRIMARY KEY DEFAULT gen_random_uuid(),
		item_id       UUID NOT NULL REFERENCES capability_items(id) ON DELETE CASCADE,
		revision_no   BIGINT NOT NULL,
		git_server_id VARCHAR(64) NOT NULL,
		git_repo_id   BIGINT NOT NULL,
		git_ref       TEXT NOT NULL DEFAULT '',
		manifest_path TEXT NOT NULL DEFAULT '',
		entry_key     TEXT NOT NULL DEFAULT '',
		git_sha       VARCHAR(40) NOT NULL,
		version_label TEXT NOT NULL DEFAULT '',
		source        VARCHAR(16) NOT NULL,
		content_digest VARCHAR(64),
		observed_at   TIMESTAMPTZ NOT NULL,
		created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
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
}

// newGitRevisionPostgresDB creates a throwaway schema and returns a handle
// whose every pooled connection starts in it.
//
// search_path travels in the DSN rather than as a `SET` statement because the
// concurrency test needs SEVERAL connections at once; a per-session SET would
// apply to whichever connection happened to run it and silently leave the
// others pointing at `public`.
func newGitRevisionPostgresDB(t *testing.T) (*gorm.DB, string) {
	t.Helper()
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		t.Skip("DATABASE_URL not set; skipping PostgreSQL Git revision test")
	}
	parsed, err := url.Parse(dsn)
	if err != nil {
		t.Fatalf("parse DATABASE_URL: %v", err)
	}
	schema := fmt.Sprintf("git_revision_%d", time.Now().UnixNano())

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
	for _, ddl := range gitRevisionPostgresFixture {
		if err := db.Exec(ddl).Error; err != nil {
			t.Fatalf("create fixture: %v\nSQL: %s", err, ddl)
		}
	}
	return db, schema
}

const (
	pgRevisionItemID   = "11111111-1111-1111-1111-111111111111"
	pgRevisionServerID = gitCapabilityTestServerID
)

func seedPostgresGitItem(t *testing.T, db *gorm.DB, gitSHA, status, syncStatus string) {
	t.Helper()
	if err := db.Exec(`INSERT INTO capability_items
		(id, registry_id, repo_id, slug, item_type, name, version, source_repo_ref, source_repo_path,
		 content_backend, source_git_server_id, source_git_repo_id, git_sha, git_sync_status, status, created_by)
		VALUES (?, 'reg', 'repo', 'skill', 'skill', 'Skill', '1.0.0', 'main', 'SKILL.md',
		        'git', ?, ?, ?, ?, ?, 'user-1')`,
		pgRevisionItemID, pgRevisionServerID, gitCapabilityTestRepoID, gitSHA, syncStatus, status).Error; err != nil {
		t.Fatalf("seed item: %v", err)
	}
}

func pgRevisionRows(t *testing.T, db *gorm.DB) []models.CapabilityItemGitRevision {
	t.Helper()
	var revisions []models.CapabilityItemGitRevision
	if err := db.Where("item_id = ?", pgRevisionItemID).Order("revision_no ASC").Find(&revisions).Error; err != nil {
		t.Fatalf("load revisions: %v", err)
	}
	return revisions
}

func newPostgresSyncService(db *gorm.DB, reader *fakeGitCapabilityReader) (*GitCapabilitySyncService, *gitserver.Config) {
	return &GitCapabilitySyncService{
			DB: db, Parser: &ParserService{},
			NewReader: func(*gitserver.Config) GitCapabilityReader { return reader },
		}, &gitserver.Config{
			ServerID: pgRevisionServerID,
			Endpoint: "https://gitea.internal.example",
			WebURL:   "https://git.example",
		}
}

func seedPostgresLease(t *testing.T, db *gorm.DB, id, token, delivery string) GitCapabilitySyncLease {
	t.Helper()
	if err := db.Exec(`INSERT INTO git_capability_sync_jobs
		(id, git_server_id, delivery_id, repo_id, repo_full_name, default_branch, ref, after_sha,
		 status, scheduled_at, started_at, lease_token)
		VALUES (?, ?, ?, ?, 'alice/capabilities', 'main', 'refs/heads/main', ?, 'running', now(), now(), ?)`,
		id, pgRevisionServerID, delivery, gitCapabilityTestRepoID, pgRevisionSHA1, token).Error; err != nil {
		t.Fatalf("seed lease: %v", err)
	}
	return GitCapabilitySyncLease{JobID: id, Token: token}
}

func pgSkillManifest(version string) []byte {
	return []byte("---\nname: Skill\ndescription: d\nversion: " + version + "\n---\nbody")
}

// TestGitCapabilityRevision_PostgresProjectionLifecycle drives the production
// SyncRepository through the whole contract on a real PostgreSQL: baseline,
// replay, a head that moves without this item's content moving, a real content
// change, archive, restore, revert to previously seen content, and a failed
// projection.
//
// The step that carries the most weight is the second one. The repository head
// advances while this manifest is byte-identical — which is what every commit
// to a sibling capability in the same repository looks like from this item's
// side — and nothing is appended. Under the head-SHA trigger this step wrote a
// row.
func TestGitCapabilityRevision_PostgresProjectionLifecycle(t *testing.T) {
	db, _ := newGitRevisionPostgresDB(t)
	seedPostgresGitItem(t, db, pgRevisionSHA1, "active", gitCapabilitySyncSynced)

	reader := &fakeGitCapabilityReader{
		repo: &gitsync.Repo{ID: gitCapabilityTestRepoID, FullName: "alice/capabilities",
			DefaultBranch: "main", Owner: &gitsync.RepoOwner{ID: 1001, Login: "alice"}},
		branch: &gitsync.Branch{Name: "main", CommitSHA: pgRevisionSHA1},
		files:  map[string][]byte{"SKILL.md": pgSkillManifest("1.0.0")},
	}
	svc, cfg := newPostgresSyncService(db, reader)
	sync := func(t *testing.T, name, delivery string) error {
		t.Helper()
		lease := seedPostgresLease(t, db, uuid.NewString(), name, delivery)
		_, err := svc.SyncRepository(context.Background(), cfg, gitCapabilityTestRepoID,
			"alice/capabilities", "main", false, lease)
		return err
	}

	// An item bound before the revision writer existed and never backfilled has
	// no history, so its first successful projection records the baseline. It is
	// not a "change" — it is the first state anything ever observed.
	if err := sync(t, "lease-baseline", "delivery-baseline"); err != nil {
		t.Fatalf("baseline sync: %v", err)
	}
	revisions := pgRevisionRows(t, db)
	if len(revisions) != 1 || revisions[0].RevisionNo != 1 || revisions[0].GitSHA != pgRevisionSHA1 {
		t.Fatalf("revisions after the baseline projection = %+v, want revision 1 at %s", revisions, pgRevisionSHA1)
	}
	baselineDigest := revisions[0].ContentDigest
	if len(baselineDigest) != 64 {
		t.Fatalf("baseline digest = %q, want 64 hex characters", baselineDigest)
	}

	// A duplicate delivery at the same head: nothing.
	if err := sync(t, "lease-replay", "delivery-replay"); err != nil {
		t.Fatalf("same-head sync: %v", err)
	}
	if got := len(pgRevisionRows(t, db)); got != 1 {
		t.Fatalf("revisions after a replay = %d, want 1", got)
	}

	// THE case this change exists for: the repository head moves — someone
	// committed to some other capability in it — and this manifest is untouched.
	reader.branch = &gitsync.Branch{Name: "main", CommitSHA: pgRevisionSHA2}
	if err := sync(t, "lease-sibling", "delivery-sibling"); err != nil {
		t.Fatalf("sibling-commit sync: %v", err)
	}
	if got := len(pgRevisionRows(t, db)); got != 1 {
		t.Fatalf("a commit that did not touch this manifest appended %d revision(s), want 0 extra", got-1)
	}
	// The item still tracks the current head; only its history stayed put.
	var trackedSHA string
	if err := db.Raw(`SELECT git_sha FROM capability_items WHERE id = ?`, pgRevisionItemID).
		Row().Scan(&trackedSHA); err != nil {
		t.Fatalf("read item: %v", err)
	}
	if trackedSHA != pgRevisionSHA2 {
		t.Fatalf("git_sha = %q, want the current head %s", trackedSHA, pgRevisionSHA2)
	}

	// This item's own content changes.
	reader.branch = &gitsync.Branch{Name: "main", CommitSHA: pgRevisionSHA3}
	reader.files["SKILL.md"] = pgSkillManifest("2.0.0")
	if err := sync(t, "lease-push", "delivery-push"); err != nil {
		t.Fatalf("changed-content sync: %v", err)
	}
	revisions = pgRevisionRows(t, db)
	if len(revisions) != 2 || revisions[1].RevisionNo != 2 || revisions[1].GitSHA != pgRevisionSHA3 ||
		revisions[1].Source != models.GitRevisionSourcePush || revisions[1].VersionLabel != "2.0.0" {
		t.Fatalf("revisions = %+v, want a push revision 2 at %s", revisions, pgRevisionSHA3)
	}
	changedDigest := revisions[1].ContentDigest
	if changedDigest == baselineDigest {
		t.Fatal("a content change produced the same digest")
	}

	// The manifest is deleted: the archiving commit advances git_sha and appends
	// nothing.
	reader.branch = &gitsync.Branch{Name: "main", CommitSHA: pgRevisionSHA4}
	delete(reader.files, "SKILL.md")
	if err := sync(t, "lease-archive", "delivery-archive"); err != nil {
		t.Fatalf("archiving sync: %v", err)
	}
	if got := len(pgRevisionRows(t, db)); got != 2 {
		t.Fatalf("revisions after archive = %d, want 2", got)
	}
	var archivedSHA, archivedStatus string
	if err := db.Raw(`SELECT git_sha, status FROM capability_items WHERE id = ?`, pgRevisionItemID).
		Row().Scan(&archivedSHA, &archivedStatus); err != nil {
		t.Fatalf("read archived item: %v", err)
	}
	if archivedSHA != pgRevisionSHA4 || archivedStatus != "archived" {
		t.Fatalf("archived item = %s/%s, want %s/archived", archivedSHA, archivedStatus, pgRevisionSHA4)
	}

	// The manifest comes back with different content: exactly one restore
	// revision.
	reader.branch = &gitsync.Branch{Name: "main", CommitSHA: pgRevisionSHA5}
	reader.files["SKILL.md"] = pgSkillManifest("2.1.0")
	if err := sync(t, "lease-restore", "delivery-restore"); err != nil {
		t.Fatalf("restoring sync: %v", err)
	}
	revisions = pgRevisionRows(t, db)
	if len(revisions) != 3 || revisions[2].RevisionNo != 3 ||
		revisions[2].Source != models.GitRevisionSourceRestore || revisions[2].GitSHA != pgRevisionSHA5 {
		t.Fatalf("revisions = %+v, want a restore revision 3 at %s", revisions, pgRevisionSHA5)
	}

	// A revert back to content that was already recorded is a NEW transition:
	// the test is inequality against the CURRENT digest, not absence from the
	// set of digests ever seen.
	reader.branch = &gitsync.Branch{Name: "main", CommitSHA: pgRevisionSHA6}
	reader.files["SKILL.md"] = pgSkillManifest("2.0.0")
	if err := sync(t, "lease-revert", models.GitCapabilitySyncDeliveryPrefixReconcile+"101:1"); err != nil {
		t.Fatalf("revert sync: %v", err)
	}
	revisions = pgRevisionRows(t, db)
	if len(revisions) != 4 || revisions[3].RevisionNo != 4 || revisions[3].GitSHA != pgRevisionSHA6 ||
		revisions[3].Source != models.GitRevisionSourceReconcile {
		t.Fatalf("revisions = %+v, want a reconcile revision 4 at %s", revisions, pgRevisionSHA6)
	}
	if revisions[3].ContentDigest != changedDigest {
		t.Fatalf("the reverted revision digest = %q, want the previously recorded %q",
			revisions[3].ContentDigest, changedDigest)
	}

	// A failed read rolls the whole projection back: no revision, no SHA move.
	reader.branch = &gitsync.Branch{Name: "main", CommitSHA: pgRevisionSHA7}
	reader.readErrs = map[string]error{"SKILL.md": errors.New("git server unreachable")}
	if err := sync(t, "lease-fail", "delivery-fail"); err == nil {
		t.Fatal("a failed read must fail the sync")
	}
	if got := len(pgRevisionRows(t, db)); got != 4 {
		t.Fatalf("revisions after a failed sync = %d, want 4", got)
	}
	var currentSHA string
	if err := db.Raw(`SELECT git_sha FROM capability_items WHERE id = ?`, pgRevisionItemID).
		Row().Scan(&currentSHA); err != nil {
		t.Fatalf("read item: %v", err)
	}
	if currentSHA != pgRevisionSHA6 {
		t.Fatalf("failed sync moved git_sha to %q", currentSHA)
	}
}

// TestGitCapabilityRevision_PostgresSiblingCommitLeavesTheOtherItemAlone is the
// direct, multi-item proof of the new trigger, on the schema and engine
// production uses.
//
// The construction is the production shape rather than a simulation of it: ONE
// repository, ONE numeric repo id, ONE default branch, TWO bound capabilities
// at two manifest paths. A single push moves the shared head and rewrites
// exactly one of the two manifests, and both items are projected in the same
// transaction by the same pass — so there is no way for the untouched item to
// "not be visited". Its digest is simply unchanged, so it appends nothing.
//
// 94% of Git-backed items live in this shape (66 shared repositories, 507
// items, largest 55), so an assertion that only holds for a lone capability in
// its own repository would cover the 6% case.
func TestGitCapabilityRevision_PostgresSiblingCommitLeavesTheOtherItemAlone(t *testing.T) {
	db, _ := newGitRevisionPostgresDB(t)

	const (
		itemAlpha = "aaaaaaaa-1111-1111-1111-111111111111"
		itemBeta  = "bbbbbbbb-2222-2222-2222-222222222222"
	)
	seed := func(id, slug, path string) {
		t.Helper()
		if err := db.Exec(`INSERT INTO capability_items
			(id, registry_id, repo_id, slug, item_type, name, version, source_repo_ref, source_repo_path,
			 content_backend, source_git_server_id, source_git_repo_id, git_sha, git_sync_status, status, created_by)
			VALUES (?, 'reg', 'repo', ?, 'skill', 'Skill', '1.0.0', 'main', ?,
			        'git', ?, ?, ?, ?, 'active', 'user-1')`,
			id, slug, path, pgRevisionServerID, gitCapabilityTestRepoID, pgRevisionSHA1,
			gitCapabilitySyncSynced).Error; err != nil {
			t.Fatalf("seed %s: %v", slug, err)
		}
	}
	seed(itemAlpha, "alpha", "skills/alpha/SKILL.md")
	seed(itemBeta, "beta", "skills/beta/SKILL.md")

	reader := &fakeGitCapabilityReader{
		repo: &gitsync.Repo{ID: gitCapabilityTestRepoID, FullName: "alice/capabilities",
			DefaultBranch: "main", Owner: &gitsync.RepoOwner{ID: 1001, Login: "alice"}},
		branch: &gitsync.Branch{Name: "main", CommitSHA: pgRevisionSHA1},
		files: map[string][]byte{
			"skills/alpha/SKILL.md": pgSkillManifest("1.0.0"),
			"skills/beta/SKILL.md":  pgSkillManifest("1.0.0"),
		},
	}
	svc, cfg := newPostgresSyncService(db, reader)
	sync := func(name, delivery string) {
		t.Helper()
		lease := seedPostgresLease(t, db, uuid.NewString(), name, delivery)
		if _, err := svc.SyncRepository(context.Background(), cfg, gitCapabilityTestRepoID,
			"alice/capabilities", "main", false, lease); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
	}
	count := func(itemID string) int64 {
		t.Helper()
		var n int64
		if err := db.Raw(`SELECT COUNT(*) FROM capability_item_git_revisions WHERE item_id = ?`, itemID).
			Scan(&n).Error; err != nil {
			t.Fatalf("count revisions for %s: %v", itemID, err)
		}
		return n
	}

	// Baselines for both.
	sync("lease-baseline", "delivery-baseline")
	if count(itemAlpha) != 1 || count(itemBeta) != 1 {
		t.Fatalf("baselines = alpha:%d beta:%d, want 1/1", count(itemAlpha), count(itemBeta))
	}

	// One push. The head moves for the whole repository; only alpha's manifest
	// changed.
	reader.branch = &gitsync.Branch{Name: "main", CommitSHA: pgRevisionSHA2}
	reader.files["skills/alpha/SKILL.md"] = pgSkillManifest("2.0.0")
	sync("lease-alpha-push", "delivery-alpha-push")

	if got := count(itemAlpha); got != 2 {
		t.Fatalf("the edited capability has %d revisions, want 2", got)
	}
	if got := count(itemBeta); got != 1 {
		t.Fatalf("a commit that never touched beta's manifest gave it %d revisions, want 1", got)
	}

	// beta's own row still tracks the new head — it was projected, it just did
	// not change. Without this assertion the test would also pass if beta had
	// been skipped entirely, which would prove nothing about the trigger.
	var betaSHA string
	if err := db.Raw(`SELECT git_sha FROM capability_items WHERE id = ?`, itemBeta).
		Row().Scan(&betaSHA); err != nil {
		t.Fatalf("read beta: %v", err)
	}
	if betaSHA != pgRevisionSHA2 {
		t.Fatalf("beta git_sha = %q, want the projected head %s", betaSHA, pgRevisionSHA2)
	}

	// And the reverse, so the test cannot pass by never appending anything:
	// touching beta alone gives beta a revision and leaves alpha at two.
	reader.branch = &gitsync.Branch{Name: "main", CommitSHA: pgRevisionSHA3}
	reader.files["skills/beta/SKILL.md"] = pgSkillManifest("3.0.0")
	sync("lease-beta-push", "delivery-beta-push")
	if got := count(itemBeta); got != 2 {
		t.Fatalf("beta revisions after its own change = %d, want 2", got)
	}
	if got := count(itemAlpha); got != 2 {
		t.Fatalf("alpha revisions after beta's change = %d, want 2", got)
	}
}

// TestGitCapabilityRevision_PostgresBackfilledBaselineAdoptsItsDigest covers the
// rows that already exist in a deployed index: seeded by
// `migrate backfill-git-revisions` from the database alone, with no digest,
// because a Git-backed row does not store its content.
//
// The first successful projection must COMPLETE that row rather than append to
// it. Appending would write one spurious revision for every backfilled item on
// the first sync after deployment — 531 of them locally — which is precisely
// the noise this change removes.
func TestGitCapabilityRevision_PostgresBackfilledBaselineAdoptsItsDigest(t *testing.T) {
	db, _ := newGitRevisionPostgresDB(t)
	seedPostgresGitItem(t, db, pgRevisionSHA1, "active", gitCapabilitySyncSynced)

	// Exactly what the backfill writes: no content_digest column in the INSERT.
	if err := db.Exec(`INSERT INTO capability_item_git_revisions
		(item_id, revision_no, git_server_id, git_repo_id, git_ref, manifest_path, git_sha,
		 version_label, source, observed_at)
		VALUES (?, 1, ?, ?, 'main', 'SKILL.md', ?, '1.0.0', 'backfill', now() - interval '3 days')`,
		pgRevisionItemID, pgRevisionServerID, gitCapabilityTestRepoID, pgRevisionSHA1).Error; err != nil {
		t.Fatalf("seed backfilled baseline: %v", err)
	}

	reader := &fakeGitCapabilityReader{
		repo: &gitsync.Repo{ID: gitCapabilityTestRepoID, FullName: "alice/capabilities",
			DefaultBranch: "main", Owner: &gitsync.RepoOwner{ID: 1001, Login: "alice"}},
		branch: &gitsync.Branch{Name: "main", CommitSHA: pgRevisionSHA2},
		files:  map[string][]byte{"SKILL.md": pgSkillManifest("1.0.0")},
	}
	svc, cfg := newPostgresSyncService(db, reader)
	sync := func(name, delivery string) {
		t.Helper()
		lease := seedPostgresLease(t, db, uuid.NewString(), name, delivery)
		if _, err := svc.SyncRepository(context.Background(), cfg, gitCapabilityTestRepoID,
			"alice/capabilities", "main", false, lease); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
	}

	sync("lease-adopt", "delivery-adopt")
	revisions := pgRevisionRows(t, db)
	if len(revisions) != 1 {
		t.Fatalf("revisions after adoption = %d, want 1 (the baseline, completed)", len(revisions))
	}
	adopted := revisions[0]
	if adopted.Source != models.GitRevisionSourceBackfill || adopted.RevisionNo != 1 {
		t.Fatalf("adoption rewrote the baseline: %+v", adopted)
	}
	// The synthesized coordinate is preserved: adoption fills in the one fact
	// the backfill could not know, it does not restate the row.
	if adopted.GitSHA != pgRevisionSHA1 || adopted.VersionLabel != "1.0.0" {
		t.Fatalf("adoption moved the baseline coordinate: %+v", adopted)
	}
	if len(adopted.ContentDigest) != 64 {
		t.Fatalf("baseline digest after adoption = %q, want an observed 64-hex digest", adopted.ContentDigest)
	}

	// Adoption happens once. A second pass at the same content is an ordinary
	// no-op, not a second adoption and not an append.
	sync("lease-adopt-replay", "delivery-adopt-replay")
	if got := pgRevisionRows(t, db); len(got) != 1 || got[0].ContentDigest != adopted.ContentDigest {
		t.Fatalf("replay after adoption = %+v, want the same single baseline", got)
	}

	// From here the baseline behaves like any observed revision: a real content
	// change appends.
	reader.branch = &gitsync.Branch{Name: "main", CommitSHA: pgRevisionSHA3}
	reader.files["SKILL.md"] = pgSkillManifest("2.0.0")
	sync("lease-after-adopt", "delivery-after-adopt")
	revisions = pgRevisionRows(t, db)
	if len(revisions) != 2 || revisions[1].RevisionNo != 2 || revisions[1].ContentDigest == adopted.ContentDigest {
		t.Fatalf("revisions after the first real change = %+v, want a distinct revision 2", revisions)
	}
}

// TestGitCapabilityRevision_PostgresRollbackTakesTheRevisionWithIt asserts the
// atomicity claim directly: the revision insert and the item's SHA update are
// one transaction, so aborting after the append leaves neither behind.
func TestGitCapabilityRevision_PostgresRollbackTakesTheRevisionWithIt(t *testing.T) {
	db, _ := newGitRevisionPostgresDB(t)
	seedPostgresGitItem(t, db, pgRevisionSHA1, "active", gitCapabilitySyncSynced)

	sentinel := errors.New("projection aborted after the revision was appended")
	err := db.Transaction(func(tx *gorm.DB) error {
		if _, err := lockGitCapabilityItemForProjection(tx, pgRevisionItemID, pgRevisionServerID, gitCapabilityTestRepoID); err != nil {
			return err
		}
		if err := tx.Exec(`UPDATE capability_items SET git_sha = ? WHERE id = ?`, pgRevisionSHA2, pgRevisionItemID).Error; err != nil {
			return err
		}
		if err := projectGitCapabilityRevision(tx, gitCapabilityRevisionInput{
			ItemID: pgRevisionItemID, GitServerID: pgRevisionServerID, GitRepoID: gitCapabilityTestRepoID,
			GitRef: "main", ManifestPath: "SKILL.md", GitSHA: pgRevisionSHA2, VersionLabel: "2.0.0",
			ContentDigest: pgRevisionDigest("a"),
			Source:        models.GitRevisionSourcePush, ObservedAt: time.Now(),
		}); err != nil {
			return err
		}
		return sentinel
	})
	if !errors.Is(err, sentinel) {
		t.Fatalf("transaction error = %v, want the sentinel", err)
	}
	if got := len(pgRevisionRows(t, db)); got != 0 {
		t.Fatalf("revisions after rollback = %d, want 0", got)
	}
	var sha string
	if err := db.Raw(`SELECT git_sha FROM capability_items WHERE id = ?`, pgRevisionItemID).Row().Scan(&sha); err != nil {
		t.Fatalf("read item: %v", err)
	}
	if sha != pgRevisionSHA1 {
		t.Fatalf("git_sha after rollback = %q, want %q", sha, pgRevisionSHA1)
	}
}

// TestGitCapabilityRevision_PostgresConcurrentProjections builds the actual
// race the row lock exists for.
//
// It is not "start two goroutines and hope": the first transaction is held open
// on the lock while the second is confirmed — through pg_stat_activity — to be
// BLOCKED on it, so the hand-off is the real, contended one. It then asserts
// the two outcomes that matter and that differ:
//
//   - two DIFFERENT digests produce two revisions with distinct, monotonic
//     numbers. Nothing is lost to the unique constraint.
//   - the SAME digest produces exactly one. The loser is a no-op because, after
//     the lock is granted, it re-reads the revision the winner committed and
//     finds its own digest already recorded — which is the only reason dropping
//     it is safe.
func TestGitCapabilityRevision_PostgresConcurrentProjections(t *testing.T) {
	db, _ := newGitRevisionPostgresDB(t)
	seedPostgresGitItem(t, db, pgRevisionSHA1, "active", gitCapabilitySyncSynced)

	// project runs exactly the sequence SyncRepository runs per item: lock, read
	// the authoritative state, update the tracked head, then let the revision
	// writer decide. The decision function itself is under test, not a
	// re-implementation of it.
	project := func(head, digest string, holdUntil <-chan struct{}, locked chan<- struct{}) error {
		return db.Transaction(func(tx *gorm.DB) error {
			state, err := lockGitCapabilityItemForProjection(tx, pgRevisionItemID, pgRevisionServerID, gitCapabilityTestRepoID)
			if err != nil {
				return err
			}
			if locked != nil {
				close(locked)
			}
			if holdUntil != nil {
				<-holdUntil
			}
			if err := tx.Exec(`UPDATE capability_items SET git_sha = ? WHERE id = ?`, head, pgRevisionItemID).Error; err != nil {
				return err
			}
			return projectGitCapabilityRevision(tx, gitCapabilityRevisionInput{
				ItemID: pgRevisionItemID, GitServerID: pgRevisionServerID, GitRepoID: gitCapabilityTestRepoID,
				GitRef: "main", ManifestPath: "SKILL.md", GitSHA: head, VersionLabel: "1.0.0",
				ContentDigest: digest,
				Source:        gitRevisionSourceForProjection(*state, models.GitRevisionSourcePush),
				ObservedAt:    time.Now(),
			})
		})
	}

	// Nothing between opening the barrier and closing it may call t.Fatal:
	// Fatal exits the test goroutine, the barrier would never be released, and
	// the held transaction would then block the schema teardown forever — a hang
	// instead of a failure. Every problem is therefore recorded and reported
	// after wg.Wait().
	race := func(t *testing.T, headA, digestA, headB, digestB string) {
		t.Helper()
		holdA := make(chan struct{})
		lockedA := make(chan struct{})
		var wg sync.WaitGroup
		errs := make([]error, 2)

		wg.Add(1)
		go func() {
			defer wg.Done()
			errs[0] = project(headA, digestA, holdA, lockedA)
		}()

		var setupErr error
		select {
		case <-lockedA:
		case <-time.After(5 * time.Second):
			setupErr = errors.New("the first projection never took the row lock")
		}

		if setupErr == nil {
			wg.Add(1)
			go func() {
				defer wg.Done()
				errs[1] = project(headB, digestB, nil, nil)
			}()
			setupErr = waitForPostgresRowLockWaiter(db)
		}

		close(holdA)
		wg.Wait()
		if setupErr != nil {
			t.Fatalf("the contended hand-off was not constructed, so this test would prove nothing: %v", setupErr)
		}
		for i, err := range errs {
			if err != nil {
				t.Fatalf("projection %d failed: %v", i, err)
			}
		}
	}

	// Different digests: both transitions must survive.
	race(t, pgRevisionSHA2, pgRevisionDigest("a"), pgRevisionSHA3, pgRevisionDigest("b"))
	revisions := pgRevisionRows(t, db)
	if len(revisions) != 2 {
		t.Fatalf("revisions after two contended distinct digests = %d, want 2", len(revisions))
	}
	if revisions[0].RevisionNo != 1 || revisions[1].RevisionNo != 2 {
		t.Fatalf("revision numbers = %d,%d, want 1,2", revisions[0].RevisionNo, revisions[1].RevisionNo)
	}
	if revisions[0].GitSHA != pgRevisionSHA2 || revisions[1].GitSHA != pgRevisionSHA3 {
		t.Fatalf("revision SHAs = %s,%s, want %s,%s",
			revisions[0].GitSHA, revisions[1].GitSHA, pgRevisionSHA2, pgRevisionSHA3)
	}

	// The same digest twice — two workers projecting the same manifest state,
	// at two different heads, which is what a webhook racing a reconcile looks
	// like. The loser is correctly a no-op, not a lost row, and the head it
	// carried is deliberately NOT recorded: no content transition happened.
	race(t, pgRevisionSHA4, pgRevisionDigest("c"), pgRevisionSHA5, pgRevisionDigest("c"))
	revisions = pgRevisionRows(t, db)
	if len(revisions) != 3 {
		t.Fatalf("revisions after a contended identical digest = %d, want 3", len(revisions))
	}
	if revisions[2].RevisionNo != 3 || revisions[2].ContentDigest != pgRevisionDigest("c") {
		t.Fatalf("last revision = %+v, want revision 3 at the shared digest", revisions[2])
	}
}

// TestGitCapabilityRevision_PostgresRejectsADuplicateRevisionNumber pins the
// backstop the row lock is supposed to make unreachable. It has to keep
// rejecting: a silently accepted duplicate would let two transitions share one
// number and break the paging cursor.
func TestGitCapabilityRevision_PostgresRejectsADuplicateRevisionNumber(t *testing.T) {
	db, _ := newGitRevisionPostgresDB(t)
	seedPostgresGitItem(t, db, pgRevisionSHA1, "active", gitCapabilitySyncSynced)

	insert := func(revision int64, sha string) error {
		return db.Exec(`INSERT INTO capability_item_git_revisions
			(item_id, revision_no, git_server_id, git_repo_id, git_sha, content_digest, source, observed_at)
			VALUES (?, ?, ?, ?, ?, ?, 'push', now())`,
			pgRevisionItemID, revision, pgRevisionServerID, gitCapabilityTestRepoID, sha,
			pgRevisionDigest("d")).Error
	}
	if err := insert(1, pgRevisionSHA1); err != nil {
		t.Fatalf("first revision: %v", err)
	}
	if err := insert(1, pgRevisionSHA2); err == nil {
		t.Fatal("a duplicate revision number within one item must be rejected")
	}
	// The same SHA under a NEW number is legal: history is transitions, not a set.
	if err := insert(2, pgRevisionSHA1); err != nil {
		t.Fatalf("re-observing an earlier SHA under a new revision number: %v", err)
	}
}

// waitForPostgresRowLockWaiter blocks until a backend is waiting on another
// transaction's row lock. Without it the "concurrent" test would usually run
// the two transactions one after the other and prove nothing.
//
// It returns an error rather than failing the test, because its caller is
// holding a transaction open and has to release it before reporting anything.
func waitForPostgresRowLockWaiter(db *gorm.DB) error {
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		var waiting int64
		if err := db.Raw(`SELECT COUNT(*) FROM pg_stat_activity
			WHERE datname = current_database()
			  AND wait_event_type = 'Lock'
			  AND wait_event IN ('transactionid', 'tuple')`).Scan(&waiting).Error; err != nil {
			return fmt.Errorf("query PostgreSQL waiters: %w", err)
		}
		if waiting > 0 {
			return nil
		}
		time.Sleep(10 * time.Millisecond)
	}
	return errors.New("timed out waiting for a projection to block on the item row lock")
}
