package clawagent

import (
	"fmt"
	"testing"

	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// openTestSQLite opens an in-memory SQLite database for testing.
func openTestSQLite() (*gorm.DB, error) {
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{
		Logger: logger.Default.LogMode(logger.Silent),
	})
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}
	return db, nil
}

// createTestTables creates all clawagent tables using SQLite-compatible DDL.
func createTestTables(db *gorm.DB) error {
	statements := []string{
		`CREATE TABLE agent_personas (
			id TEXT PRIMARY KEY,
			user_id TEXT NOT NULL DEFAULT '',
			name TEXT NOT NULL DEFAULT '',
			soul_content TEXT NOT NULL DEFAULT '',
			identity_content TEXT DEFAULT '',
			user_context TEXT DEFAULT '',
			is_default INTEGER DEFAULT 0,
			created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
			updated_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
		)`,
		`CREATE TABLE agent_providers (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			user_id TEXT NOT NULL DEFAULT '',
			name TEXT NOT NULL DEFAULT '',
			provider_type TEXT NOT NULL DEFAULT '',
			api_key_encrypted TEXT DEFAULT '',
			base_url TEXT DEFAULT '',
			model_name TEXT NOT NULL DEFAULT '',
			models TEXT DEFAULT '',
			is_default INTEGER DEFAULT 0,
			created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
			updated_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
		)`,
		`CREATE TABLE agent_memories (
			user_id TEXT PRIMARY KEY,
			content TEXT NOT NULL DEFAULT '',
			updated_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
		)`,
		`CREATE TABLE agent_workspace_tasks (
			id TEXT PRIMARY KEY,
			task_id TEXT NOT NULL UNIQUE,
			user_id TEXT NOT NULL DEFAULT '',
			workspace_id TEXT NOT NULL DEFAULT '',
			device_id TEXT NOT NULL DEFAULT '',
			directory_path TEXT DEFAULT '',
			task TEXT NOT NULL DEFAULT '',
			skill TEXT DEFAULT '',
			agent_session_base_key TEXT DEFAULT '',
			conversation_id TEXT DEFAULT '',
			status TEXT NOT NULL DEFAULT 'queued',
			delivery_status TEXT NOT NULL DEFAULT 'pending',
			progress_summary TEXT DEFAULT '',
			output TEXT DEFAULT '',
			error TEXT DEFAULT '',
			announce_retry_count INTEGER DEFAULT 0,
			started_at DATETIME,
			completed_at DATETIME,
			last_event_at DATETIME,
			created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
		)`,
		`CREATE TABLE agent_session_meta (
			session_id TEXT PRIMARY KEY,
			user_id TEXT NOT NULL DEFAULT '',
			base_key TEXT NOT NULL DEFAULT '',
			version INTEGER NOT NULL DEFAULT 1,
			reset_type TEXT NOT NULL DEFAULT '',
			last_message_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
			message_count INTEGER NOT NULL DEFAULT 0,
			token_estimate INTEGER NOT NULL DEFAULT 0,
			event_data TEXT DEFAULT '',
			is_archived INTEGER NOT NULL DEFAULT 0,
			archived_at DATETIME,
			created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
		)`,
		`CREATE TABLE agent_session_messages (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			session_id TEXT NOT NULL DEFAULT '',
			role TEXT NOT NULL DEFAULT '',
			content TEXT DEFAULT '',
			tool_call_id TEXT DEFAULT '',
			tool_calls TEXT DEFAULT '',
			kind TEXT NOT NULL DEFAULT '',
			metadata TEXT DEFAULT '',
			created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
		)`,
		`CREATE INDEX idx_sm_session ON agent_session_messages (session_id)`,
		`CREATE INDEX idx_sm_session_created ON agent_session_messages (session_id, created_at)`,
		`CREATE INDEX idx_sm_kind ON agent_session_messages (session_id, kind)`,
	}

	for _, stmt := range statements {
		if err := db.Exec(stmt).Error; err != nil {
			return fmt.Errorf("create table: %w\nSQL: %s", err, stmt)
		}
	}
	return nil
}

// setupTestDB creates a test database with all required tables.
// This is shared across all test files in the package.
func setupTestDB(t *testing.T) *gorm.DB {
	t.Helper()

	db, err := openTestSQLite()
	if err != nil {
		t.Fatalf("failed to open test db: %v", err)
	}

	if err := createTestTables(db); err != nil {
		t.Fatalf("failed to create tables: %v", err)
	}

	return db
}
