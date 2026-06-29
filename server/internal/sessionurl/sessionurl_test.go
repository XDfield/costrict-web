package sessionurl

import (
	"testing"

	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

func TestResolveWorkspaceIDNormalizesWindowsPath(t *testing.T) {
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatalf("open test db: %v", err)
	}

	stmts := []string{
		`CREATE TABLE devices (
			id TEXT PRIMARY KEY,
			device_id TEXT NOT NULL,
			deleted_at DATETIME
		)`,
		`CREATE TABLE workspaces (
			id TEXT PRIMARY KEY,
			device_id TEXT NOT NULL,
			deleted_at DATETIME
		)`,
		`CREATE TABLE workspace_directories (
			id TEXT PRIMARY KEY,
			workspace_id TEXT NOT NULL,
			path TEXT NOT NULL,
			deleted_at DATETIME
		)`,
	}
	for _, stmt := range stmts {
		if err := db.Exec(stmt).Error; err != nil {
			t.Fatalf("migrate test db: %v", err)
		}
	}

	if err := db.Exec(`INSERT INTO devices (id, device_id) VALUES (?, ?)`, "dev-uuid-1", "device-1").Error; err != nil {
		t.Fatalf("seed device: %v", err)
	}
	if err := db.Exec(`INSERT INTO workspaces (id, device_id) VALUES (?, ?)`, "ws-1", "dev-uuid-1").Error; err != nil {
		t.Fatalf("seed workspace: %v", err)
	}
	if err := db.Exec(`INSERT INTO workspace_directories (id, workspace_id, path) VALUES (?, ?, ?)`, "wd-1", "ws-1", "D:/DEV/myclaw").Error; err != nil {
		t.Fatalf("seed workspace directory: %v", err)
	}

	workspaceID, err := ResolveWorkspaceID(db, "device-1", `D:\DEV\myclaw`)
	if err != nil {
		t.Fatalf("resolve workspace id: %v", err)
	}
	if workspaceID != "ws-1" {
		t.Fatalf("expected workspace id ws-1, got %s", workspaceID)
	}
}

func TestBuildEmptyInputs(t *testing.T) {
	if got := Build("", "ws-1", "sess-1"); got != "" {
		t.Fatalf("expected empty URL for empty base, got %s", got)
	}
	if got := Build("https://app.example.com", "", "sess-1"); got != "" {
		t.Fatalf("expected empty URL for empty workspaceID, got %s", got)
	}
	if got := Build("https://app.example.com", "ws-1", ""); got != "" {
		t.Fatalf("expected empty URL for empty sessionID, got %s", got)
	}
}

func TestBuildURL(t *testing.T) {
	got := Build("https://app.example.com", "ws-1", "sess-1")
	want := "https://app.example.com/workspace/ws-1/?session=sess-1"
	if got != want {
		t.Fatalf("expected %s, got %s", want, got)
	}
}
