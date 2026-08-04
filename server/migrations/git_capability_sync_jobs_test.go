package migrations_test

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/costrict/costrict-web/server/internal/database"
)

func TestGitCapabilitySyncJobsMigration_CascadesGitServerDeletion(t *testing.T) {
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		t.Skip("DATABASE_URL not set; skipping PostgreSQL migration constraint test")
	}
	db, err := database.Initialize(dsn)
	if err != nil {
		t.Fatalf("initialize PostgreSQL: %v", err)
	}

	tx := db.Begin()
	if tx.Error != nil {
		t.Fatalf("begin transaction: %v", tx.Error)
	}
	defer func() { _ = tx.Rollback().Error }()

	schemaName := fmt.Sprintf("git_sync_migration_test_%d", time.Now().UnixNano())
	quotedSchema := quoteIdentifier(schemaName)
	if err := tx.Exec("CREATE SCHEMA " + quotedSchema).Error; err != nil {
		t.Fatalf("create isolated schema: %v", err)
	}
	if err := tx.Exec("SET LOCAL search_path TO " + quotedSchema).Error; err != nil {
		t.Fatalf("set isolated search path: %v", err)
	}
	if err := tx.Exec(`CREATE TABLE git_servers (server_id VARCHAR(64) PRIMARY KEY)`).Error; err != nil {
		t.Fatalf("create git_servers fixture: %v", err)
	}

	migrationSQL, err := readGooseUp(filepath.Join("20260803020000_create_git_capability_sync_jobs.sql"))
	if err != nil {
		t.Fatalf("read migration: %v", err)
	}
	if err := tx.Exec(migrationSQL).Error; err != nil {
		t.Fatalf("apply git sync jobs migration: %v", err)
	}
	if err := tx.Exec(`INSERT INTO git_servers (server_id) VALUES ('gs-cascade')`).Error; err != nil {
		t.Fatalf("insert git server: %v", err)
	}
	if err := tx.Exec(`
		INSERT INTO git_capability_sync_jobs
			(git_server_id, delivery_id, repo_id, repo_full_name, default_branch, ref, after_sha, scheduled_at)
		VALUES ('gs-cascade', 'delivery-cascade', 42, 'owner/repository', 'main', 'refs/heads/main', 'aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa', now())
	`).Error; err != nil {
		t.Fatalf("insert sync job: %v", err)
	}
	if err := tx.Exec(`DELETE FROM git_servers WHERE server_id = 'gs-cascade'`).Error; err != nil {
		t.Fatalf("delete git server: %v", err)
	}

	var jobs int64
	if err := tx.Raw(`SELECT COUNT(*) FROM git_capability_sync_jobs WHERE git_server_id = 'gs-cascade'`).Scan(&jobs).Error; err != nil {
		t.Fatalf("count sync jobs: %v", err)
	}
	if jobs != 0 {
		t.Fatalf("sync jobs after git server deletion = %d, want 0", jobs)
	}
}

func readGooseUp(path string) (string, error) {
	contents, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	const upMarker = "-- +goose Up"
	const downMarker = "-- +goose Down"
	upStart := strings.Index(string(contents), upMarker)
	downStart := strings.Index(string(contents), downMarker)
	if upStart < 0 || downStart < 0 || downStart <= upStart {
		return "", fmt.Errorf("invalid Goose migration %s", path)
	}
	up := string(contents[upStart+len(upMarker) : downStart])
	up = strings.ReplaceAll(up, "-- +goose StatementBegin", "")
	up = strings.ReplaceAll(up, "-- +goose StatementEnd", "")
	return up, nil
}

func quoteIdentifier(identifier string) string {
	return `"` + strings.ReplaceAll(identifier, `"`, `""`) + `"`
}
