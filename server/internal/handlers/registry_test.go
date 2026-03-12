package handlers

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/costrict/costrict-web/server/internal/database"
	"github.com/costrict/costrict-web/server/internal/middleware"
	"github.com/costrict/costrict-web/server/internal/models"
	"github.com/gin-gonic/gin"
	"gorm.io/datatypes"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// setupTestDB initialises an in-memory SQLite database and injects it as the
// global DB used by handlers.  Returns a cleanup function.
//
// SQLite does not support PostgreSQL-specific DDL (e.g. gen_random_uuid()),
// so we create tables with hand-written CREATE TABLE statements that are
// compatible with both engines at the Go level.
func setupTestDB(t *testing.T) func() {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{
		Logger: logger.Default.LogMode(logger.Silent),
	})
	if err != nil {
		t.Fatalf("failed to open test db: %v", err)
	}

	stmts := []string{
		`CREATE TABLE IF NOT EXISTS organizations (
			id          TEXT PRIMARY KEY,
			name        TEXT NOT NULL UNIQUE,
			display_name TEXT,
			description TEXT,
			visibility  TEXT DEFAULT 'private',
			org_type    TEXT DEFAULT 'normal',
			owner_id    TEXT NOT NULL,
			created_at  DATETIME,
			updated_at  DATETIME
		)`,
		`CREATE TABLE IF NOT EXISTS org_members (
			id         TEXT PRIMARY KEY,
			org_id     TEXT NOT NULL,
			user_id    TEXT NOT NULL,
			username   TEXT,
			role       TEXT DEFAULT 'member',
			created_at DATETIME
		)`,
		`CREATE TABLE IF NOT EXISTS capability_registries (
			id               TEXT PRIMARY KEY,
			name             TEXT NOT NULL,
			description      TEXT,
			source_type      TEXT NOT NULL DEFAULT 'internal',
			external_url     TEXT,
			external_branch  TEXT DEFAULT 'main',
			sync_enabled     INTEGER DEFAULT 0,
			sync_interval    INTEGER DEFAULT 3600,
			last_synced_at   DATETIME,
			last_sync_sha    TEXT,
			sync_status      TEXT DEFAULT 'idle',
			sync_config      TEXT DEFAULT '{}',
			last_sync_log_id TEXT,
			visibility       TEXT DEFAULT 'org',
			org_id           TEXT,
			owner_id         TEXT NOT NULL,
			created_at       DATETIME,
			updated_at       DATETIME
		)`,
		`CREATE TABLE IF NOT EXISTS capability_items (
			id                   TEXT PRIMARY KEY,
			registry_id          TEXT NOT NULL,
			slug                 TEXT NOT NULL,
			item_type            TEXT NOT NULL,
			name                 TEXT NOT NULL,
			description          TEXT,
			category             TEXT,
			version              TEXT DEFAULT '1.0.0',
			content              TEXT,
			metadata             TEXT DEFAULT '{}',
			source_path          TEXT,
			source_sha           TEXT,
			install_count        INTEGER DEFAULT 0,
			status               TEXT DEFAULT 'active',
			security_status      TEXT DEFAULT 'unscanned',
			last_scan_id         TEXT,
			created_by           TEXT NOT NULL,
			updated_by           TEXT,
			embedding            TEXT,
			experience_score     REAL DEFAULT 0,
			embedding_updated_at DATETIME,
			created_at           DATETIME,
			updated_at           DATETIME,
			UNIQUE(registry_id, item_type, slug)
		)`,
		`CREATE TABLE IF NOT EXISTS capability_versions (
			id         TEXT PRIMARY KEY,
			item_id    TEXT NOT NULL,
			revision   INTEGER NOT NULL,
			content    TEXT NOT NULL,
			metadata   TEXT DEFAULT '{}',
			commit_msg TEXT,
			created_by TEXT NOT NULL,
			created_at DATETIME
		)`,
		`CREATE TABLE IF NOT EXISTS security_scans (
			id           TEXT PRIMARY KEY,
			item_id      TEXT NOT NULL,
			revision_id  TEXT NOT NULL,
			trigger_type TEXT NOT NULL,
			status       TEXT NOT NULL DEFAULT 'pending',
			risk_level   TEXT,
			verdict      TEXT,
			red_flags    TEXT DEFAULT '[]',
			permissions  TEXT DEFAULT '{}',
			report       TEXT DEFAULT '{}',
			error_list   TEXT DEFAULT '[]',
			scanned_by   TEXT,
			created_at   DATETIME,
			finished_at  DATETIME
		)`,
		`CREATE TABLE IF NOT EXISTS capability_assets (
			id              TEXT PRIMARY KEY,
			item_id         TEXT NOT NULL,
			rel_path        TEXT NOT NULL,
			text_content    TEXT,
			storage_backend TEXT DEFAULT 'local',
			storage_key     TEXT,
			mime_type       TEXT,
			file_size       INTEGER DEFAULT 0,
			content_sha     TEXT,
			created_at      DATETIME,
			updated_at      DATETIME
		)`,
		`CREATE TABLE IF NOT EXISTS capability_artifacts (
			id               TEXT PRIMARY KEY,
			item_id          TEXT NOT NULL,
			filename         TEXT NOT NULL,
			file_size        INTEGER NOT NULL,
			checksum_sha256  TEXT NOT NULL,
			mime_type        TEXT,
			storage_backend  TEXT DEFAULT 'local',
			storage_key      TEXT NOT NULL,
			artifact_version TEXT NOT NULL,
			is_latest        INTEGER DEFAULT 0,
			source_type      TEXT DEFAULT 'upload',
			download_count   INTEGER DEFAULT 0,
			uploaded_by      TEXT NOT NULL,
			created_at       DATETIME
		)`,
	}

	for _, s := range stmts {
		if err := db.Exec(s).Error; err != nil {
			t.Fatalf("failed to create table: %v\nSQL: %s", err, s)
		}
	}

	database.DB = db
	return func() { database.DB = nil }
}

// newRouter builds a minimal Gin router wired to the three registry handlers.
// If userID is non-empty it is injected directly into the context (bypassing
// real Casdoor auth).
func newRouter(userID string) *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	injectUser := func(c *gin.Context) {
		if userID != "" {
			c.Set(middleware.UserIDKey, userID)
		}
		c.Next()
	}
	r.GET("/api/registry/:org/access", injectUser, RegistryAccess)
	r.GET("/api/registry/:org/index.json", injectUser, RegistryIndex)
	r.GET("/api/registry/:org/:slug/:file", injectUser, DownloadRegistryFile)
	r.GET("/api/items/:id/download", injectUser, DownloadItem)
	return r
}

func get(r *gin.Engine, path string) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	req, _ := http.NewRequest(http.MethodGet, path, nil)
	r.ServeHTTP(w, req)
	return w
}

// ---------------------------------------------------------------------------
// resolveOrgID
// ---------------------------------------------------------------------------

func TestResolveOrgID_Public(t *testing.T) {
	id, ok := resolveOrgID("public")
	if !ok || id != "public" {
		t.Fatalf("expected (\"public\", true), got (%q, %v)", id, ok)
	}
}

func TestResolveOrgID_NotFound(t *testing.T) {
	defer setupTestDB(t)()
	_, ok := resolveOrgID("nonexistent")
	if ok {
		t.Fatal("expected false for unknown org")
	}
}

func TestResolveOrgID_Found(t *testing.T) {
	defer setupTestDB(t)()
	org := models.Organization{ID: "org-uuid-1", Name: "acme", OwnerID: "u1"}
	database.DB.Create(&org)

	id, ok := resolveOrgID("acme")
	if !ok || id != "org-uuid-1" {
		t.Fatalf("expected (\"org-uuid-1\", true), got (%q, %v)", id, ok)
	}
}

// ---------------------------------------------------------------------------
// RegistryAccess
// ---------------------------------------------------------------------------

func TestRegistryAccess_PublicOrg(t *testing.T) {
	defer setupTestDB(t)()
	database.DB.Create(&models.CapabilityRegistry{
		ID: PublicRegistryID, Name: "public",
		SourceType: "internal", Visibility: "public", OrgID: "public", OwnerID: "system",
	})

	w := get(newRouter(""), "/api/registry/public/access")
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	var body map[string]bool
	json.NewDecoder(w.Body).Decode(&body)
	if !body["public"] {
		t.Fatal("expected public=true")
	}
}

func TestRegistryAccess_NonExistentOrg(t *testing.T) {
	defer setupTestDB(t)()

	w := get(newRouter(""), "/api/registry/ghost/access")
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	var body map[string]bool
	json.NewDecoder(w.Body).Decode(&body)
	if body["public"] {
		t.Fatal("expected public=false for non-existent org")
	}
}

func TestRegistryAccess_PrivateOrg(t *testing.T) {
	defer setupTestDB(t)()
	org := models.Organization{ID: "org-1", Name: "sangfor", OwnerID: "u1"}
	database.DB.Create(&org)
	database.DB.Create(&models.CapabilityRegistry{
		ID: "reg-1", Name: "sangfor-reg",
		SourceType: "internal", Visibility: "org", OrgID: "org-1", OwnerID: "u1",
	})

	w := get(newRouter(""), "/api/registry/sangfor/access")
	var body map[string]bool
	json.NewDecoder(w.Body).Decode(&body)
	if body["public"] {
		t.Fatal("expected public=false for org-visibility registry")
	}
}

// ---------------------------------------------------------------------------
// RegistryIndex
// ---------------------------------------------------------------------------

func TestRegistryIndex_PublicOrg_Anonymous(t *testing.T) {
	defer setupTestDB(t)()
	database.DB.Create(&models.CapabilityRegistry{
		ID: PublicRegistryID, Name: "public",
		SourceType: "internal", Visibility: "public", OrgID: "public", OwnerID: "system",
	})
	database.DB.Create(&models.CapabilityItem{
		ID: "item-1", RegistryID: PublicRegistryID,
		Slug: "code-reviewer", ItemType: "skill",
		Name: "Code Reviewer", Status: "active", CreatedBy: "system",
		Content: "# Code Reviewer", Metadata: datatypes.JSON([]byte("{}")),
	})

	w := get(newRouter(""), "/api/registry/public/index.json")
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var body indexJSON
	json.NewDecoder(w.Body).Decode(&body)
	if body.Version != 1 {
		t.Fatalf("expected version=1, got %d", body.Version)
	}
	if len(body.Items) != 1 {
		t.Fatalf("expected 1 item, got %d", len(body.Items))
	}
	if body.Items[0].Slug != "code-reviewer" {
		t.Fatalf("unexpected slug: %s", body.Items[0].Slug)
	}
	if len(body.Items[0].Files) == 0 || body.Items[0].Files[0] != "SKILL.md" {
		t.Fatalf("expected Files=[SKILL.md], got %v", body.Items[0].Files)
	}
}

func TestRegistryIndex_CommandFilename_UsesSlug(t *testing.T) {
	defer setupTestDB(t)()
	database.DB.Create(&models.CapabilityRegistry{
		ID: PublicRegistryID, Name: "public",
		SourceType: "internal", Visibility: "public", OrgID: "public", OwnerID: "system",
	})
	database.DB.Create(&models.CapabilityItem{
		ID: "item-2", RegistryID: PublicRegistryID,
		Slug: "git-review", ItemType: "command",
		Name: "Git Review", Status: "active", CreatedBy: "system",
		Content: "# Git Review", Metadata: datatypes.JSON([]byte("{}")),
	})

	w := get(newRouter(""), "/api/registry/public/index.json")
	var body indexJSON
	json.NewDecoder(w.Body).Decode(&body)
	if len(body.Items) != 1 {
		t.Fatalf("expected 1 item, got %d", len(body.Items))
	}
	if body.Items[0].Files[0] != "git-review.md" {
		t.Fatalf("expected git-review.md, got %s", body.Items[0].Files[0])
	}
}

func TestRegistryIndex_PrivateOrg_Unauthenticated(t *testing.T) {
	defer setupTestDB(t)()
	org := models.Organization{ID: "org-2", Name: "internal", OwnerID: "u1"}
	database.DB.Create(&org)
	database.DB.Create(&models.CapabilityRegistry{
		ID: "reg-2", Name: "internal-reg",
		SourceType: "internal", Visibility: "org", OrgID: "org-2", OwnerID: "u1",
	})

	w := get(newRouter(""), "/api/registry/internal/index.json")
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", w.Code)
	}
}

func TestRegistryIndex_PrivateOrg_NonMember(t *testing.T) {
	defer setupTestDB(t)()
	org := models.Organization{ID: "org-3", Name: "secret", OwnerID: "u1"}
	database.DB.Create(&org)
	database.DB.Create(&models.CapabilityRegistry{
		ID: "reg-3", Name: "secret-reg",
		SourceType: "internal", Visibility: "org", OrgID: "org-3", OwnerID: "u1",
	})

	w := get(newRouter("stranger"), "/api/registry/secret/index.json")
	if w.Code != http.StatusForbidden {
		t.Fatalf("expected 403, got %d", w.Code)
	}
}

func TestRegistryIndex_PrivateOrg_Member(t *testing.T) {
	defer setupTestDB(t)()
	org := models.Organization{ID: "org-4", Name: "myorg", OwnerID: "u1"}
	database.DB.Create(&org)
	database.DB.Create(&models.CapabilityRegistry{
		ID: "reg-4", Name: "myorg-reg",
		SourceType: "internal", Visibility: "org", OrgID: "org-4", OwnerID: "u1",
	})
	database.DB.Create(&models.OrgMember{
		ID: "mem-1", OrgID: "org-4", UserID: "member-user", Role: "member",
	})
	database.DB.Create(&models.CapabilityItem{
		ID: "item-3", RegistryID: "reg-4",
		Slug: "internal-tool", ItemType: "subagent",
		Name: "Internal Tool", Status: "active", CreatedBy: "u1",
		Content: "# Internal", Metadata: datatypes.JSON([]byte("{}")),
	})

	w := get(newRouter("member-user"), "/api/registry/myorg/index.json")
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var body indexJSON
	json.NewDecoder(w.Body).Decode(&body)
	if len(body.Items) != 1 {
		t.Fatalf("expected 1 item, got %d", len(body.Items))
	}
}

func TestRegistryIndex_MCPItem_HasMCPField(t *testing.T) {
	defer setupTestDB(t)()
	database.DB.Create(&models.CapabilityRegistry{
		ID: PublicRegistryID, Name: "public",
		SourceType: "internal", Visibility: "public", OrgID: "public", OwnerID: "system",
	})
	meta := `{"hosting_type":"command","command":"npx","args":["-y","@internal/tool"]}`
	database.DB.Create(&models.CapabilityItem{
		ID: "item-mcp", RegistryID: PublicRegistryID,
		Slug: "my-tool", ItemType: "mcp",
		Name: "My Tool", Status: "active", CreatedBy: "system",
		Content: "", Metadata: datatypes.JSON([]byte(meta)),
	})

	w := get(newRouter(""), "/api/registry/public/index.json")
	var body indexJSON
	json.NewDecoder(w.Body).Decode(&body)
	if len(body.Items) != 1 {
		t.Fatalf("expected 1 item, got %d", len(body.Items))
	}
	item := body.Items[0]
	if item.MCP == nil {
		t.Fatal("expected mcp field to be set")
	}
	var mcp map[string]interface{}
	json.Unmarshal(item.MCP, &mcp)
	if mcp["type"] != "local" {
		t.Fatalf("expected type=local, got %v", mcp["type"])
	}
}

// ---------------------------------------------------------------------------
// DownloadRegistryFile
// ---------------------------------------------------------------------------

func TestDownloadRegistryFile_PublicItem(t *testing.T) {
	defer setupTestDB(t)()
	database.DB.Create(&models.CapabilityRegistry{
		ID: PublicRegistryID, Name: "public",
		SourceType: "internal", Visibility: "public", OrgID: "public", OwnerID: "system",
	})
	database.DB.Create(&models.CapabilityItem{
		ID: "item-dl", RegistryID: PublicRegistryID,
		Slug: "code-reviewer", ItemType: "skill",
		Name: "Code Reviewer", Status: "active", CreatedBy: "system",
		Content: "# Code Reviewer\nsome content", Metadata: datatypes.JSON([]byte("{}")),
	})

	w := get(newRouter(""), "/api/registry/public/code-reviewer/SKILL.md")
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	if w.Body.String() != "# Code Reviewer\nsome content" {
		t.Fatalf("unexpected body: %s", w.Body.String())
	}
	cd := w.Header().Get("Content-Disposition")
	if cd != `attachment; filename="SKILL.md"` {
		t.Fatalf("unexpected Content-Disposition: %s", cd)
	}
}

func TestDownloadRegistryFile_NotFound(t *testing.T) {
	defer setupTestDB(t)()
	database.DB.Create(&models.CapabilityRegistry{
		ID: PublicRegistryID, Name: "public",
		SourceType: "internal", Visibility: "public", OrgID: "public", OwnerID: "system",
	})

	w := get(newRouter(""), "/api/registry/public/nonexistent/SKILL.md")
	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", w.Code)
	}
}

func TestDownloadRegistryFile_OrgItem_Forbidden(t *testing.T) {
	defer setupTestDB(t)()
	org := models.Organization{ID: "org-5", Name: "corp", OwnerID: "u1"}
	database.DB.Create(&org)
	database.DB.Create(&models.CapabilityRegistry{
		ID: "reg-5", Name: "corp-reg",
		SourceType: "internal", Visibility: "org", OrgID: "org-5", OwnerID: "u1",
	})
	database.DB.Create(&models.CapabilityItem{
		ID: "item-org", RegistryID: "reg-5",
		Slug: "secret-skill", ItemType: "skill",
		Name: "Secret", Status: "active", CreatedBy: "u1",
		Content: "secret", Metadata: datatypes.JSON([]byte("{}")),
	})

	w := get(newRouter("outsider"), "/api/registry/corp/secret-skill/SKILL.md")
	if w.Code != http.StatusForbidden {
		t.Fatalf("expected 403, got %d", w.Code)
	}
}

// ---------------------------------------------------------------------------
// DownloadItem (by ID)
// ---------------------------------------------------------------------------

func TestDownloadItem_PublicItem(t *testing.T) {
	defer setupTestDB(t)()
	database.DB.Create(&models.CapabilityRegistry{
		ID: PublicRegistryID, Name: "public",
		SourceType: "internal", Visibility: "public", OrgID: "public", OwnerID: "system",
	})
	database.DB.Create(&models.CapabilityItem{
		ID: "item-byid", RegistryID: PublicRegistryID,
		Slug: "my-skill", ItemType: "skill",
		Name: "My Skill", Status: "active", CreatedBy: "system",
		Content: "# My Skill", Metadata: datatypes.JSON([]byte("{}")),
	})

	w := get(newRouter(""), "/api/items/item-byid/download")
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	if w.Body.String() != "# My Skill" {
		t.Fatalf("unexpected body: %s", w.Body.String())
	}
}

func TestDownloadItem_NotFound(t *testing.T) {
	defer setupTestDB(t)()
	w := get(newRouter(""), "/api/items/no-such-id/download")
	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", w.Code)
	}
}

// ---------------------------------------------------------------------------
// buildMCPConfig
// ---------------------------------------------------------------------------

func TestBuildMCPConfig_Command(t *testing.T) {
	si := models.CapabilityItem{
		Metadata: datatypes.JSON([]byte(`{"hosting_type":"command","command":"npx","args":["-y","@foo/bar"]}`)),
	}
	raw := buildMCPConfig(si)
	var out map[string]interface{}
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("invalid json: %v", err)
	}
	if out["type"] != "local" {
		t.Fatalf("expected type=local, got %v", out["type"])
	}
	cmds, _ := out["command"].([]interface{})
	if len(cmds) != 3 || cmds[0] != "npx" {
		t.Fatalf("unexpected command: %v", cmds)
	}
}

func TestBuildMCPConfig_Remote(t *testing.T) {
	si := models.CapabilityItem{
		Metadata: datatypes.JSON([]byte(`{"hosting_type":"remote","server_type":"sse","url":"https://mcp.example.com"}`)),
	}
	raw := buildMCPConfig(si)
	var out map[string]interface{}
	json.Unmarshal(raw, &out)
	if out["type"] != "sse" {
		t.Fatalf("expected type=sse, got %v", out["type"])
	}
	if out["url"] != "https://mcp.example.com" {
		t.Fatalf("unexpected url: %v", out["url"])
	}
}

func TestBuildMCPConfig_EmptyMetadata(t *testing.T) {
	si := models.CapabilityItem{Metadata: datatypes.JSON([]byte{})}
	if buildMCPConfig(si) != nil {
		t.Fatal("expected nil for empty metadata")
	}
}

func TestBuildMCPConfig_UnknownHostingType_PassThrough(t *testing.T) {
	raw := `{"hosting_type":"artifact","package_name":"my-pkg"}`
	si := models.CapabilityItem{Metadata: datatypes.JSON([]byte(raw))}
	out := buildMCPConfig(si)
	if string(out) != raw {
		t.Fatalf("expected passthrough, got %s", string(out))
	}
}

// ---------------------------------------------------------------------------
// contentFilename
// ---------------------------------------------------------------------------

func TestContentFilename(t *testing.T) {
	cases := []struct {
		itemType string
		slug     string
		want     string
	}{
		{"skill", "anything", "SKILL.md"},
		{"subagent", "my-agent", "my-agent.md"},
		{"command", "git-review", "git-review.md"},
		{"mcp", "my-tool", "my-tool.md"},
	}
	for _, tc := range cases {
		got := contentFilename(tc.itemType, tc.slug)
		if got != tc.want {
			t.Errorf("contentFilename(%q,%q) = %q, want %q", tc.itemType, tc.slug, got, tc.want)
		}
	}
}

// ---------------------------------------------------------------------------
// mergeUnique
// ---------------------------------------------------------------------------

func TestMergeUnique(t *testing.T) {
	result := mergeUnique([]string{"a", "b"}, []string{"b", "c"})
	if len(result) != 3 {
		t.Fatalf("expected 3 unique elements, got %d: %v", len(result), result)
	}
	seen := map[string]bool{}
	for _, v := range result {
		if seen[v] {
			t.Fatalf("duplicate value: %s", v)
		}
		seen[v] = true
	}
}

// ensure test file compiles even if fmt is only used indirectly
var _ = fmt.Sprintf
