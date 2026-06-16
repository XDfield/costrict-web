package adminitem

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// setupTestDB opens an in-memory sqlite DB with the minimal capability-item
// schema the admin content service touches. We hand-roll the tables (rather than
// AutoMigrate) because the postgres-specific jsonb / uuid / vector column types
// do not map cleanly onto sqlite; this mirrors the postgres migration closely
// enough for the list/status/delete logic under test.
func setupTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{
		Logger: logger.Default.LogMode(logger.Silent),
	})
	if err != nil {
		t.Fatalf("open test db: %v", err)
	}
	sqlDB, _ := db.DB()
	sqlDB.SetMaxOpenConns(1)

	stmts := []string{
		`CREATE TABLE repositories (
			id TEXT PRIMARY KEY,
			name TEXT NOT NULL,
			visibility TEXT DEFAULT 'private',
			owner_id TEXT NOT NULL
		)`,
		`CREATE TABLE capability_registries (
			id TEXT PRIMARY KEY,
			name TEXT NOT NULL,
			source_type TEXT NOT NULL DEFAULT 'internal',
			repo_id TEXT,
			owner_id TEXT NOT NULL
		)`,
		`CREATE TABLE capability_items (
			id TEXT PRIMARY KEY,
			registry_id TEXT NOT NULL,
			repo_id TEXT NOT NULL DEFAULT 'public',
			slug TEXT NOT NULL,
			item_type TEXT NOT NULL,
			name TEXT NOT NULL,
			description TEXT,
			descriptions TEXT NOT NULL DEFAULT '{}',
			category TEXT,
			version TEXT DEFAULT '1.0.0',
			content TEXT,
			content_md5 TEXT DEFAULT '',
			current_revision INTEGER NOT NULL DEFAULT 1,
			metadata TEXT DEFAULT '{}',
			health TEXT DEFAULT '{}',
			evaluation TEXT DEFAULT '{}',
			source_path TEXT,
			source_sha TEXT,
			source_type TEXT NOT NULL DEFAULT 'direct',
			source TEXT NOT NULL DEFAULT '',
			forked_from_item_id TEXT,
			forked_from_owner_id TEXT,
			parent_plugin_id TEXT,
			preview_count INTEGER DEFAULT 0,
			install_count INTEGER DEFAULT 0,
			favorite_count INTEGER DEFAULT 0,
			status TEXT DEFAULT 'active',
			security_status TEXT DEFAULT 'unscanned',
			last_scan_id TEXT,
			created_by TEXT NOT NULL,
			updated_by TEXT,
			is_built_in INTEGER DEFAULT 0,
			embedding TEXT,
			experience_score REAL DEFAULT 0,
			embedding_updated_at DATETIME,
			created_at DATETIME,
			updated_at DATETIME
		)`,
		`CREATE TABLE capability_versions (
			id TEXT PRIMARY KEY, item_id TEXT NOT NULL, revision INTEGER NOT NULL,
			name TEXT, description TEXT, descriptions TEXT NOT NULL DEFAULT '{}',
			category TEXT, version TEXT, content TEXT NOT NULL, content_md5 TEXT DEFAULT '',
			metadata TEXT DEFAULT '{}', source_path TEXT, commit_msg TEXT,
			created_by TEXT NOT NULL, created_at DATETIME
		)`,
		`CREATE TABLE capability_assets (
			id TEXT PRIMARY KEY, item_id TEXT NOT NULL, rel_path TEXT NOT NULL,
			text_content TEXT, storage_backend TEXT DEFAULT 'local', storage_key TEXT,
			mime_type TEXT, file_size INTEGER DEFAULT 0, content_sha TEXT,
			created_at DATETIME, updated_at DATETIME
		)`,
		`CREATE TABLE item_favorites (
			id TEXT PRIMARY KEY, item_id TEXT NOT NULL, user_id TEXT NOT NULL, created_at DATETIME
		)`,
		`CREATE TABLE behavior_logs (
			id TEXT PRIMARY KEY, user_id TEXT, item_id TEXT, registry_id TEXT,
			action_type TEXT NOT NULL, context TEXT, created_at DATETIME
		)`,
		`CREATE TABLE item_tags (
			id TEXT PRIMARY KEY, item_id TEXT NOT NULL, tag_id TEXT NOT NULL, created_at DATETIME
		)`,
		`CREATE TABLE user_system_roles (
			id TEXT PRIMARY KEY, user_id TEXT, role TEXT, created_at DATETIME, deleted_at DATETIME
		)`,
	}
	for _, s := range stmts {
		if err := db.Exec(s).Error; err != nil {
			t.Fatalf("create table: %v", err)
		}
	}
	return db
}

func seedRepoRegistry(t *testing.T, db *gorm.DB) {
	t.Helper()
	if err := db.Exec(`INSERT INTO repositories (id, name, visibility, owner_id) VALUES ('repo-1','Acme Repo','public','u1')`).Error; err != nil {
		t.Fatalf("seed repo: %v", err)
	}
	if err := db.Exec(`INSERT INTO capability_registries (id, name, source_type, repo_id, owner_id) VALUES ('reg-1','Acme Reg','internal','repo-1','u1')`).Error; err != nil {
		t.Fatalf("seed registry: %v", err)
	}
}

func seedItem(t *testing.T, db *gorm.DB, id, name, itemType, status, security, createdBy string, score float64) {
	t.Helper()
	now := time.Now()
	if err := db.Exec(
		`INSERT INTO capability_items
			(id, registry_id, repo_id, slug, item_type, name, status, security_status, experience_score, created_by, created_at, updated_at)
		 VALUES (?, 'reg-1', 'repo-1', ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		id, id+"-slug", itemType, name, status, security, score, createdBy, now, now,
	).Error; err != nil {
		t.Fatalf("seed item %s: %v", id, err)
	}
}

func TestService_ListItems_FilterAndPaginate(t *testing.T) {
	db := setupTestDB(t)
	seedRepoRegistry(t, db)
	seedItem(t, db, "i1", "Alpha Skill", "skill", "active", "clean", "u1", 4.5)
	seedItem(t, db, "i2", "Beta Plugin", "plugin", "archived", "high", "u2", 3.1)
	seedItem(t, db, "i3", "Gamma Skill", "skill", "active", "medium", "u1", 4.0)
	seedItem(t, db, "i4", "Delta MCP", "mcp", "active", "extreme", "u3", 2.2)

	svc := NewService(db)

	// No filters → all four, repo name resolved.
	rows, total, err := svc.ListItems(ListParams{})
	if err != nil {
		t.Fatalf("list all: %v", err)
	}
	if total != 4 || len(rows) != 4 {
		t.Fatalf("expected 4 items, got total=%d len=%d", total, len(rows))
	}
	if rows[0].RepoName != "Acme Repo" {
		t.Fatalf("expected repo name resolved, got %q", rows[0].RepoName)
	}

	// Type filter.
	rows, total, _ = svc.ListItems(ListParams{ItemType: "skill"})
	if total != 2 || len(rows) != 2 {
		t.Fatalf("type=skill expected 2, got total=%d len=%d", total, len(rows))
	}

	// Status filter includes archived (admin sees all by default, can narrow).
	rows, total, _ = svc.ListItems(ListParams{Status: "archived"})
	if total != 1 || rows[0].ID != "i2" {
		t.Fatalf("status=archived expected i2, got total=%d rows=%+v", total, rows)
	}

	// Security group filter: high → {high, extreme} → i2 + i4.
	_, total, _ = svc.ListItems(ListParams{SecurityStatus: "high"})
	if total != 2 {
		t.Fatalf("securityStatus=high group expected 2 (high+extreme), got %d", total)
	}

	// Exact security status value.
	_, total, _ = svc.ListItems(ListParams{SecurityStatus: "medium"})
	if total != 1 {
		t.Fatalf("securityStatus=medium expected 1, got %d", total)
	}

	// createdBy filter.
	_, total, _ = svc.ListItems(ListParams{CreatedBy: "u1"})
	if total != 2 {
		t.Fatalf("createdBy=u1 expected 2, got %d", total)
	}

	// Search filter (name LIKE).
	_, total, _ = svc.ListItems(ListParams{Search: "Plugin"})
	if total != 1 {
		t.Fatalf("search=Plugin expected 1, got %d", total)
	}

	// Pagination.
	rows, total, _ = svc.ListItems(ListParams{Page: 1, PageSize: 2})
	if total != 4 || len(rows) != 2 {
		t.Fatalf("page1 size2 expected total=4 len=2, got total=%d len=%d", total, len(rows))
	}
}

func TestService_SetStatus(t *testing.T) {
	db := setupTestDB(t)
	seedRepoRegistry(t, db)
	seedItem(t, db, "i1", "Alpha", "skill", "active", "clean", "u1", 4.5)

	svc := NewService(db)

	if err := svc.SetStatus("i1", "archived"); err != nil {
		t.Fatalf("archive: %v", err)
	}
	_, total, _ := svc.ListItems(ListParams{Status: "archived"})
	if total != 1 {
		t.Fatalf("expected item archived, got %d archived", total)
	}

	if err := svc.SetStatus("i1", "active"); err != nil {
		t.Fatalf("reactivate: %v", err)
	}

	if err := svc.SetStatus("i1", "bogus"); err != ErrInvalidStatus {
		t.Fatalf("expected ErrInvalidStatus, got %v", err)
	}
	if err := svc.SetStatus("missing", "active"); err != ErrItemNotFound {
		t.Fatalf("expected ErrItemNotFound, got %v", err)
	}
}

func TestService_DeleteItem(t *testing.T) {
	db := setupTestDB(t)
	seedRepoRegistry(t, db)
	seedItem(t, db, "i1", "Alpha", "plugin", "active", "clean", "u2", 4.5)
	// dependent rows + a bundled sub-skill
	db.Exec(`INSERT INTO capability_versions (id, item_id, revision, content, created_by) VALUES ('v1','i1',1,'x','u2')`)
	db.Exec(`INSERT INTO item_favorites (id, item_id, user_id) VALUES ('f1','i1','u9')`)
	db.Exec(`INSERT INTO behavior_logs (id, item_id, action_type) VALUES ('b1','i1','view')`)
	db.Exec(`INSERT INTO capability_items
		(id, registry_id, repo_id, slug, item_type, name, status, security_status, created_by, parent_plugin_id, created_at, updated_at)
		VALUES ('sub1','reg-1','repo-1','sub-slug','skill','Sub','active','clean','u2','i1',CURRENT_TIMESTAMP,CURRENT_TIMESTAMP)`)

	svc := NewService(db)
	if err := svc.DeleteItem("i1"); err != nil {
		t.Fatalf("delete: %v", err)
	}

	var n int64
	db.Raw(`SELECT COUNT(*) FROM capability_items WHERE id = 'i1'`).Scan(&n)
	if n != 0 {
		t.Fatalf("item not deleted, count=%d", n)
	}
	db.Raw(`SELECT COUNT(*) FROM capability_versions WHERE item_id = 'i1'`).Scan(&n)
	if n != 0 {
		t.Fatalf("versions not cleaned, count=%d", n)
	}
	db.Raw(`SELECT COUNT(*) FROM item_favorites WHERE item_id = 'i1'`).Scan(&n)
	if n != 0 {
		t.Fatalf("favorites not cleaned, count=%d", n)
	}
	// Sub-skill soft-archived (still present, status archived).
	var subStatus string
	db.Raw(`SELECT status FROM capability_items WHERE id = 'sub1'`).Scan(&subStatus)
	if subStatus != "archived" {
		t.Fatalf("expected sub-skill archived, got %q", subStatus)
	}

	if err := svc.DeleteItem("missing"); err != ErrItemNotFound {
		t.Fatalf("expected ErrItemNotFound, got %v", err)
	}
}

func newCtx(t *testing.T, method, target, userID, body string) (*gin.Context, *httptest.ResponseRecorder) {
	t.Helper()
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	var r *http.Request
	if body != "" {
		r = httptest.NewRequest(method, target, strings.NewReader(body))
		r.Header.Set("Content-Type", "application/json")
	} else {
		r = httptest.NewRequest(method, target, nil)
	}
	c.Request = r
	if userID != "" {
		c.Set("userId", userID)
	}
	return c, rec
}

func TestHandler_ListItems(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db := setupTestDB(t)
	seedRepoRegistry(t, db)
	seedItem(t, db, "i1", "Alpha", "skill", "active", "clean", "u1", 4.5)
	seedItem(t, db, "i2", "Beta", "plugin", "archived", "high", "u2", 3.0)
	m := New(db)

	c, rec := newCtx(t, http.MethodGet, "/admin/items?type=skill", "admin1", "")
	m.ListItemsHandler()(c)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Items []ItemRow `json:"items"`
		Total int64     `json:"total"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Total != 1 || len(resp.Items) != 1 || resp.Items[0].ID != "i1" {
		t.Fatalf("expected only skill i1, got %+v", resp)
	}
}

func TestHandler_SetItemStatus(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db := setupTestDB(t)
	seedRepoRegistry(t, db)
	seedItem(t, db, "i1", "Alpha", "skill", "active", "clean", "u1", 4.5)
	m := New(db)

	c, rec := newCtx(t, http.MethodPut, "/admin/items/i1/status", "admin1", `{"status":"archived"}`)
	c.Params = gin.Params{{Key: "id", Value: "i1"}}
	m.SetItemStatusHandler()(c)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	var status string
	db.Raw(`SELECT status FROM capability_items WHERE id = 'i1'`).Scan(&status)
	if status != "archived" {
		t.Fatalf("expected archived, got %q", status)
	}

	// invalid status → 400
	c, rec = newCtx(t, http.MethodPut, "/admin/items/i1/status", "admin1", `{"status":"weird"}`)
	c.Params = gin.Params{{Key: "id", Value: "i1"}}
	m.SetItemStatusHandler()(c)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for bad status, got %d", rec.Code)
	}

	// unauthenticated → 401
	c, rec = newCtx(t, http.MethodPut, "/admin/items/i1/status", "", `{"status":"active"}`)
	c.Params = gin.Params{{Key: "id", Value: "i1"}}
	m.SetItemStatusHandler()(c)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401 for unauthenticated, got %d", rec.Code)
	}
}

func TestHandler_DeleteItem(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db := setupTestDB(t)
	seedRepoRegistry(t, db)
	seedItem(t, db, "i1", "Alpha", "skill", "active", "clean", "u2", 4.5)
	m := New(db)

	c, rec := newCtx(t, http.MethodDelete, "/admin/items/i1", "admin1", "")
	c.Params = gin.Params{{Key: "id", Value: "i1"}}
	m.DeleteItemHandler()(c)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	var n int64
	db.Raw(`SELECT COUNT(*) FROM capability_items WHERE id = 'i1'`).Scan(&n)
	if n != 0 {
		t.Fatalf("item not deleted")
	}

	// not found → 404
	c, rec = newCtx(t, http.MethodDelete, "/admin/items/missing", "admin1", "")
	c.Params = gin.Params{{Key: "id", Value: "missing"}}
	m.DeleteItemHandler()(c)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404 for missing item, got %d", rec.Code)
	}
}
