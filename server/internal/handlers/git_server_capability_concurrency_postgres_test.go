package handlers

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/costrict/costrict-web/server/internal/database"
	"gorm.io/datatypes"
	"gorm.io/gorm"
)

func TestGitServerDeleteSerializesWithGitBackedCapabilityPersist(t *testing.T) {
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		t.Skip("DATABASE_URL not set; skipping PostgreSQL lock regression test")
	}
	db, err := database.Initialize(dsn)
	if err != nil {
		t.Fatalf("initialize PostgreSQL: %v", err)
	}

	schema := fmt.Sprintf("git_server_capability_lock_%d", time.Now().UnixNano())
	quotedSchema := quotePostgresIdentifier(schema)
	if err := db.Exec("CREATE SCHEMA " + quotedSchema).Error; err != nil {
		t.Fatalf("create schema: %v", err)
	}
	t.Cleanup(func() { _ = db.Exec("DROP SCHEMA " + quotedSchema + " CASCADE").Error })

	fixtureErr := db.Connection(func(conn *gorm.DB) error {
		if err := conn.Exec("SET search_path TO " + quotedSchema).Error; err != nil {
			return err
		}
		for _, ddl := range []string{
			`CREATE TABLE git_servers (
			server_id TEXT PRIMARY KEY, kind TEXT NOT NULL, endpoint TEXT NOT NULL,
			display_name TEXT NOT NULL, config JSONB NOT NULL DEFAULT '{}',
			is_template BOOLEAN NOT NULL DEFAULT false, enabled BOOLEAN NOT NULL DEFAULT true,
			created_at TIMESTAMPTZ NOT NULL, updated_at TIMESTAMPTZ NOT NULL
		)`,
			`CREATE TABLE tenant_git_server_binding (git_server_id TEXT NOT NULL)`,
			`CREATE TABLE capability_items (
			id TEXT PRIMARY KEY, content_backend TEXT NOT NULL,
			source_git_server_id TEXT NOT NULL DEFAULT ''
		)`,
			`CREATE TABLE git_capability_sync_jobs (git_server_id TEXT NOT NULL)`,
		} {
			if err := conn.Exec(ddl).Error; err != nil {
				return err
			}
		}
		return conn.Exec(`INSERT INTO git_servers
			(server_id, kind, endpoint, display_name, config, is_template, enabled, created_at, updated_at)
			VALUES ('gs-race', 'gitea', 'https://git.example', 'race', '{}', false, true, now(), now())`).Error
	})
	if fixtureErr != nil {
		t.Fatalf("create PostgreSQL fixture: %v", fixtureErr)
	}

	blocker := db.Begin()
	if blocker.Error != nil {
		t.Fatalf("begin blocker: %v", blocker.Error)
	}
	defer blocker.Rollback()
	if err := blocker.Exec("SET LOCAL search_path TO " + quotedSchema).Error; err != nil {
		t.Fatalf("set blocker search path: %v", err)
	}
	if err := blocker.Exec("LOCK TABLE capability_items IN ACCESS EXCLUSIVE MODE").Error; err != nil {
		t.Fatalf("lock capability_items: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	deleteDone := make(chan error, 1)
	go func() {
		deleteDone <- db.WithContext(ctx).Connection(func(conn *gorm.DB) error {
			if err := conn.Exec("SET search_path TO " + quotedSchema).Error; err != nil {
				return err
			}
			return NewGormGitServerStore(conn).DeleteGitServer(ctx, "gs-race")
		})
	}()

	waitForPostgresTableLock(t, db, schema+".git_servers", "RowShareLock")

	persistDone := make(chan error, 1)
	go func() {
		persistDone <- db.WithContext(ctx).Connection(func(conn *gorm.DB) error {
			if err := conn.Exec("SET search_path TO " + quotedSchema).Error; err != nil {
				return err
			}
			_, err := persistNewItem(conn, createItemRequest{
				ID: "git-item-race", RegistryID: PublicRegistryID, RepoID: "public",
				Slug: "git-item-race", ItemType: "plugin", Name: "race",
				Version: "1.0.0", Metadata: datatypes.JSON([]byte(`{}`)),
				SourceType: "fork", CreatedBy: "user-1", ContentBackend: contentBackendGit,
				SourceGitServerID: "gs-race", SourceGitRepoID: 42,
			}, createItemAssets{})
			return err
		})
	}()

	select {
	case err := <-persistDone:
		t.Fatalf("Git-backed persist passed deletion lock early: %v", err)
	case <-time.After(100 * time.Millisecond):
	}

	if err := blocker.Commit().Error; err != nil {
		t.Fatalf("release blocker: %v", err)
	}
	if err := <-deleteDone; err != nil {
		t.Fatalf("delete Git server: %v", err)
	}
	if err := <-persistDone; !errors.Is(err, errGitServerUnavailable) {
		t.Fatalf("persist error = %v, want errGitServerUnavailable", err)
	}

	var items int64
	if err := db.Raw("SELECT COUNT(*) FROM "+quotedSchema+".capability_items WHERE id = ?", "git-item-race").Scan(&items).Error; err != nil {
		t.Fatalf("count capability items: %v", err)
	}
	if items != 0 {
		t.Fatalf("orphan Git-backed items = %d, want 0", items)
	}
}

func waitForPostgresTableLock(t *testing.T, db *gorm.DB, relation, mode string) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		var count int64
		if err := db.Raw(`SELECT COUNT(*) FROM pg_locks
			WHERE relation = to_regclass(?) AND mode = ? AND granted`, relation, mode).Scan(&count).Error; err != nil {
			t.Fatalf("query PostgreSQL locks: %v", err)
		}
		if count > 0 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s on %s", mode, relation)
}

func quotePostgresIdentifier(identifier string) string {
	return `"` + strings.ReplaceAll(identifier, `"`, `""`) + `"`
}
