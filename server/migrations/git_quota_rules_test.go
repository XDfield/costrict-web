package migrations_test

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/costrict/costrict-web/server/internal/database"
)

// The quota rule table is only meaningful next to the Git server it is pushed
// to, and its primary key encodes the fork's own lookup shape (repo "" is the
// owner-level default, not a missing value). Both properties are constraints
// rather than conventions, so they are asserted against a real PostgreSQL.
func TestGitQuotaRulesMigration_EnforcesIdentityAndCascades(t *testing.T) {
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

	schemaName := fmt.Sprintf("git_quota_migration_test_%d", time.Now().UnixNano())
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

	migrationSQL, err := readGooseUp(filepath.Join("20260806000200_create_git_quota_rules.sql"))
	if err != nil {
		t.Fatalf("read migration: %v", err)
	}
	if err := tx.Exec(migrationSQL).Error; err != nil {
		t.Fatalf("apply Git quota rules migration: %v", err)
	}
	if err := tx.Exec(`INSERT INTO git_servers (server_id) VALUES ('gs-quota')`).Error; err != nil {
		t.Fatalf("insert git server: %v", err)
	}

	// repo defaults to '' — the fork's owner-level sentinel — so an owner rule
	// can be written without naming a repository.
	if err := tx.Exec(`INSERT INTO git_quota_rules (git_server_id, owner, max_file_size_mb, repo_quota_mb)
		VALUES ('gs-quota', 'acme', 20, 200)`).Error; err != nil {
		t.Fatalf("insert owner-level rule: %v", err)
	}
	var ownerRepo string
	if err := tx.Raw(`SELECT repo FROM git_quota_rules WHERE git_server_id = 'gs-quota' AND owner = 'acme'`).
		Scan(&ownerRepo).Error; err != nil {
		t.Fatalf("read owner-level rule: %v", err)
	}
	if ownerRepo != "" {
		t.Fatalf("owner-level repo = %q, want empty string", ownerRepo)
	}

	// The repo-level override coexists with the owner default rather than
	// replacing it: the fork consults repo first, then owner.
	if err := tx.Exec(`INSERT INTO git_quota_rules (git_server_id, owner, repo, max_file_size_mb, repo_quota_mb)
		VALUES ('gs-quota', 'acme', 'widgets', 5, 50)`).Error; err != nil {
		t.Fatalf("insert repo-level rule: %v", err)
	}

	if err := tx.Exec(`SAVEPOINT dup`).Error; err != nil {
		t.Fatalf("savepoint: %v", err)
	}
	if err := tx.Exec(`INSERT INTO git_quota_rules (git_server_id, owner, repo, max_file_size_mb, repo_quota_mb)
		VALUES ('gs-quota', 'acme', 'widgets', 9, 90)`).Error; err == nil {
		t.Fatal("duplicate (git_server_id, owner, repo) was accepted")
	}
	if err := tx.Exec(`ROLLBACK TO SAVEPOINT dup`).Error; err != nil {
		t.Fatalf("rollback to savepoint: %v", err)
	}

	for _, invalid := range []struct {
		name string
		stmt string
	}{
		{"blank owner", `INSERT INTO git_quota_rules (git_server_id, owner) VALUES ('gs-quota', '')`},
		{"negative file size", `INSERT INTO git_quota_rules (git_server_id, owner, max_file_size_mb) VALUES ('gs-quota', 'neg', -1)`},
		{"negative repo quota", `INSERT INTO git_quota_rules (git_server_id, owner, repo_quota_mb) VALUES ('gs-quota', 'neg', -1)`},
	} {
		if err := tx.Exec(`SAVEPOINT invalid`).Error; err != nil {
			t.Fatalf("savepoint: %v", err)
		}
		if err := tx.Exec(invalid.stmt).Error; err == nil {
			t.Fatalf("%s was accepted", invalid.name)
		}
		if err := tx.Exec(`ROLLBACK TO SAVEPOINT invalid`).Error; err != nil {
			t.Fatalf("rollback to savepoint: %v", err)
		}
	}

	// Rules describe one server's enforcement; they have no meaning once that
	// server row is gone.
	if err := tx.Exec(`DELETE FROM git_servers WHERE server_id = 'gs-quota'`).Error; err != nil {
		t.Fatalf("delete git server: %v", err)
	}
	var remaining int64
	if err := tx.Raw(`SELECT COUNT(*) FROM git_quota_rules WHERE git_server_id = 'gs-quota'`).
		Scan(&remaining).Error; err != nil {
		t.Fatalf("count quota rules: %v", err)
	}
	if remaining != 0 {
		t.Fatalf("quota rules after git server deletion = %d, want 0", remaining)
	}
}
