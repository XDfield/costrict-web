package services

import (
	"testing"

	"github.com/costrict/costrict-web/server/internal/models"
	"github.com/google/uuid"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

func setupWorkspaceServiceDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	// Register a callback to generate UUIDs before create,
	// since SQLite does not support gen_random_uuid().
	if err := db.Callback().Create().Before("gorm:create").Register("workspace_uuid", func(d *gorm.DB) {
		if d.Statement.Schema != nil {
			for _, field := range d.Statement.Schema.Fields {
				if field.Name == "ID" && field.DataType == "uuid" {
					if _, zero := field.ValueOf(d.Statement.Context, d.Statement.ReflectValue); zero {
						d.Statement.SetColumn("ID", uuid.New().String())
					}
				}
			}
		}
	}); err != nil {
		t.Fatalf("register uuid callback: %v", err)
	}
	stmts := []string{
		`CREATE TABLE workspaces (
			id TEXT PRIMARY KEY,
			name TEXT NOT NULL,
			description TEXT,
			user_id TEXT NOT NULL,
			device_id TEXT,
			is_default BOOLEAN NOT NULL DEFAULT 0,
			status TEXT NOT NULL DEFAULT 'active',
			settings TEXT DEFAULT '{}',
			created_at DATETIME,
			updated_at DATETIME,
			deleted_at DATETIME
		)`,
		`CREATE TABLE workspace_directories (
			id TEXT PRIMARY KEY,
			workspace_id TEXT NOT NULL,
			name TEXT NOT NULL,
			path TEXT NOT NULL,
			is_default BOOLEAN NOT NULL DEFAULT 0,
			order_index INTEGER NOT NULL DEFAULT 0,
			settings TEXT DEFAULT '{}',
			created_at DATETIME,
			updated_at DATETIME,
			deleted_at DATETIME
		)`,
	}
	for _, stmt := range stmts {
		if err := db.Exec(stmt).Error; err != nil {
			t.Fatalf("create table: %v", err)
		}
	}
	return db
}

func setupWorkspaceService(t *testing.T) (*WorkspaceService, *gorm.DB) {
	t.Helper()
	db := setupWorkspaceServiceDB(t)
	svc := &WorkspaceService{DB: db, DeviceService: nil}
	return svc, db
}

// TestRecreateWorkspaceAfterDelete verifies that creating a workspace with the
// same name as a previously soft-deleted workspace succeeds.
//
// Bug scenario: Delete a workspace → soft-delete sets deleted_at on the
// workspace row, but the associated WorkspaceDirectory rows remain active
// (deleted_at IS NULL). Creating a new workspace with the same name should
// succeed and return the new workspace with its directories.
func TestRecreateWorkspaceAfterDelete(t *testing.T) {
	svc, db := setupWorkspaceService(t)
	userID := "user-1"

	// Step 1: Create workspace "my-workspace"
	ws1, err := svc.CreateWorkspace(userID, CreateWorkspaceRequest{
		Name: "my-workspace",
		Directories: []CreateDirectoryRequest{
			{Name: "root", Path: "/home/user/project"},
		},
	})
	if err != nil {
		t.Fatalf("first create: %v", err)
	}
	if ws1.Name != "my-workspace" {
		t.Fatalf("expected name 'my-workspace', got %q", ws1.Name)
	}

	// Step 2: Delete the workspace (soft-delete)
	if err := svc.DeleteWorkspace(ws1.ID, userID); err != nil {
		t.Fatalf("delete: %v", err)
	}

	// Verify the workspace is soft-deleted
	var softDeleted models.Workspace
	if err := db.Unscoped().Where("id = ?", ws1.ID).First(&softDeleted).Error; err != nil {
		t.Fatalf("should find soft-deleted workspace: %v", err)
	}
	if softDeleted.DeletedAt.Valid == false {
		t.Fatal("workspace should have deleted_at set after soft-delete")
	}

	// Step 3: Create workspace with the same name — this should succeed
	ws2, err := svc.CreateWorkspace(userID, CreateWorkspaceRequest{
		Name: "my-workspace",
		Directories: []CreateDirectoryRequest{
			{Name: "root", Path: "/home/user/new-project"},
		},
	})
	if err != nil {
		t.Fatalf("recreate after delete: %v", err)
	}
	if ws2.Name != "my-workspace" {
		t.Fatalf("expected name 'my-workspace', got %q", ws2.Name)
	}
	if ws2.ID == ws1.ID {
		t.Fatal("new workspace should have a different ID from the soft-deleted one")
	}
	if len(ws2.Directories) != 1 {
		t.Fatalf("expected 1 directory, got %d", len(ws2.Directories))
	}
	if ws2.Directories[0].Path != "/home/user/new-project" {
		t.Fatalf("expected directory path '/home/user/new-project', got %q", ws2.Directories[0].Path)
	}
}

// TestRecreateWorkspaceAfterDelete_DirectoriesCascaded verifies that when
// a workspace is soft-deleted, its directories are also soft-deleted,
// preventing orphaned directory rows.
func TestRecreateWorkspaceAfterDelete_DirectoriesCascaded(t *testing.T) {
	svc, db := setupWorkspaceService(t)
	userID := "user-1"

	// Create and delete a workspace
	ws1, err := svc.CreateWorkspace(userID, CreateWorkspaceRequest{
		Name: "test-ws",
		Directories: []CreateDirectoryRequest{
			{Name: "dir1", Path: "/path/one"},
			{Name: "dir2", Path: "/path/two"},
		},
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := svc.DeleteWorkspace(ws1.ID, userID); err != nil {
		t.Fatalf("delete: %v", err)
	}

	// After deletion, directories belonging to the deleted workspace
	// should also be soft-deleted (no orphaned active directories).
	var orphanedDirs []models.WorkspaceDirectory
	err = db.Where("workspace_id = ?", ws1.ID).Find(&orphanedDirs).Error
	if err != nil {
		t.Fatalf("query directories: %v", err)
	}
	// GORM's default scope filters out soft-deleted records,
	// so Find should return 0 if directories were properly cascade-soft-deleted.
	if len(orphanedDirs) != 0 {
		t.Fatalf("expected 0 active directories for soft-deleted workspace, got %d (orphaned directories not cascaded)", len(orphanedDirs))
	}
}

// TestListWorkspacesAfterRecreate verifies that listing workspaces
// after delete+recreate does not show the deleted workspace.
func TestListWorkspacesAfterRecreate(t *testing.T) {
	svc, _ := setupWorkspaceService(t)
	userID := "user-1"

	ws1, _ := svc.CreateWorkspace(userID, CreateWorkspaceRequest{
		Name: "list-test",
		Directories: []CreateDirectoryRequest{
			{Name: "root", Path: "/root"},
		},
	})
	svc.DeleteWorkspace(ws1.ID, userID)

	ws2, err := svc.CreateWorkspace(userID, CreateWorkspaceRequest{
		Name: "list-test",
		Directories: []CreateDirectoryRequest{
			{Name: "root", Path: "/root2"},
		},
	})
	if err != nil {
		t.Fatalf("recreate: %v", err)
	}

	workspaces, err := svc.ListWorkspaces(userID)
	if err != nil {
		t.Fatalf("list: %v", err)
	}

	// Should have exactly 1 workspace (the new one)
	if len(workspaces) != 1 {
		t.Fatalf("expected 1 workspace, got %d", len(workspaces))
	}
	if workspaces[0].ID != ws2.ID {
		t.Fatalf("expected workspace ID %s, got %s", ws2.ID, workspaces[0].ID)
	}
}

// TestCreateWorkspaceNameExistsActive verifies that creating a workspace
// with a name that matches an ACTIVE (non-deleted) workspace returns
// ErrWorkspaceNameExists.
func TestCreateWorkspaceNameExistsActive(t *testing.T) {
	svc, _ := setupWorkspaceService(t)
	userID := "user-1"

	_, err := svc.CreateWorkspace(userID, CreateWorkspaceRequest{
		Name: "existing-name",
		Directories: []CreateDirectoryRequest{
			{Name: "root", Path: "/root"},
		},
	})
	if err != nil {
		t.Fatalf("first create: %v", err)
	}

	// Second create with same name should fail
	_, err = svc.CreateWorkspace(userID, CreateWorkspaceRequest{
		Name: "existing-name",
		Directories: []CreateDirectoryRequest{
			{Name: "root", Path: "/root2"},
		},
	})
	if err == nil {
		t.Fatal("expected ErrWorkspaceNameExists, got nil")
	}
	if err != ErrWorkspaceNameExists {
		t.Fatalf("expected ErrWorkspaceNameExists, got %v", err)
	}
}

// TestDeleteWorkspaceOrphanedDirectories verifies that deleting a workspace
// does not leave orphaned active directories in the database.
func TestDeleteWorkspaceOrphanedDirectories(t *testing.T) {
	svc, db := setupWorkspaceService(t)
	userID := "user-1"

	ws1, err := svc.CreateWorkspace(userID, CreateWorkspaceRequest{
		Name: "orphan-test",
		Directories: []CreateDirectoryRequest{
			{Name: "dir1", Path: "/path/one"},
			{Name: "dir2", Path: "/path/two"},
		},
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	// Delete workspace
	if err := svc.DeleteWorkspace(ws1.ID, userID); err != nil {
		t.Fatalf("delete: %v", err)
	}

	// Check that workspace directories are also soft-deleted
	var activeDirs []models.WorkspaceDirectory
	db.Where("workspace_id = ?", ws1.ID).Find(&activeDirs)
	if len(activeDirs) != 0 {
		t.Fatalf("expected 0 active directories after workspace delete, got %d", len(activeDirs))
	}

	// Verify directories still exist in unscoped query (soft-deleted, not hard-deleted)
	var allDirs []models.WorkspaceDirectory
	db.Unscoped().Where("workspace_id = ?", ws1.ID).Find(&allDirs)
	if len(allDirs) != 2 {
		t.Fatalf("expected 2 soft-deleted directories (unscoped), got %d", len(allDirs))
	}
}

// TestRecreateWorkspaceSameDirectoryPaths verifies that recreating a workspace
// with the same directory paths as a deleted workspace does not cause conflicts.
func TestRecreateWorkspaceSameDirectoryPaths(t *testing.T) {
	svc, _ := setupWorkspaceService(t)
	userID := "user-1"

	ws1, err := svc.CreateWorkspace(userID, CreateWorkspaceRequest{
		Name: "path-test",
		Directories: []CreateDirectoryRequest{
			{Name: "root", Path: "/same/path"},
		},
	})
	if err != nil {
		t.Fatalf("first create: %v", err)
	}
	svc.DeleteWorkspace(ws1.ID, userID)

	// Recreate with same directory path — should succeed
	ws2, err := svc.CreateWorkspace(userID, CreateWorkspaceRequest{
		Name: "path-test",
		Directories: []CreateDirectoryRequest{
			{Name: "root", Path: "/same/path"},
		},
	})
	if err != nil {
		t.Fatalf("recreate with same paths: %v", err)
	}
	if len(ws2.Directories) != 1 {
		t.Fatalf("expected 1 directory, got %d", len(ws2.Directories))
	}
	if ws2.Directories[0].Path != "/same/path" {
		t.Fatalf("expected path '/same/path', got %q", ws2.Directories[0].Path)
	}
}
