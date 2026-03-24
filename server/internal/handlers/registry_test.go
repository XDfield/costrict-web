package handlers

import (
	"bytes"
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
		`CREATE TABLE IF NOT EXISTS repositories (
			id          TEXT PRIMARY KEY,
			name        TEXT NOT NULL UNIQUE,
			display_name TEXT,
			description TEXT,
			visibility  TEXT DEFAULT 'private',
			repo_type   TEXT DEFAULT 'normal',
			owner_id    TEXT NOT NULL,
			created_at  DATETIME,
			updated_at  DATETIME
		)`,
		`CREATE TABLE IF NOT EXISTS repo_members (
			id         TEXT PRIMARY KEY,
			repo_id    TEXT NOT NULL,
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
			visibility       TEXT DEFAULT 'repo',
			repo_id          TEXT,
			owner_id         TEXT NOT NULL,
			created_at       DATETIME,
			updated_at       DATETIME
		)`,
		`CREATE TABLE IF NOT EXISTS capability_items (
			id                   TEXT PRIMARY KEY,
			registry_id          TEXT NOT NULL,
			repo_id              TEXT NOT NULL DEFAULT 'public',
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
			source_type          TEXT NOT NULL DEFAULT 'direct',
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
			UNIQUE(repo_id, item_type, slug)
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
		`CREATE TABLE IF NOT EXISTS behavior_logs (
			id           TEXT PRIMARY KEY,
			user_id      TEXT,
			item_id      TEXT,
			registry_id  TEXT,
			action_type  TEXT NOT NULL,
			context      TEXT,
			search_query TEXT,
			session_id   TEXT,
			metadata     TEXT DEFAULT '{}',
			duration_ms  INTEGER DEFAULT 0,
			rating       INTEGER DEFAULT 0,
			feedback     TEXT,
			created_at   DATETIME
		)`,
		`CREATE TABLE IF NOT EXISTS experience_candidates (
			id             TEXT PRIMARY KEY,
			item_id        TEXT,
			type           TEXT NOT NULL,
			title          TEXT NOT NULL,
			description    TEXT,
			context        TEXT,
			resolution     TEXT,
			source_type    TEXT NOT NULL,
			source_log_id  TEXT,
			frequency      INTEGER DEFAULT 1,
			impact_score   REAL DEFAULT 0,
			status         TEXT DEFAULT 'pending',
			promotion_type TEXT,
			promoted_at    DATETIME,
			promoted_by    TEXT,
			created_at     DATETIME,
			updated_at     DATETIME
		)`,
		`CREATE TABLE IF NOT EXISTS experience_promotions (
			id              TEXT PRIMARY KEY,
			candidate_id    TEXT NOT NULL,
			item_id         TEXT NOT NULL,
			promotion_type  TEXT NOT NULL,
			promoted_by     TEXT NOT NULL,
			metadata_before TEXT DEFAULT '{}',
			metadata_after  TEXT DEFAULT '{}',
			created_at      DATETIME
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
	r.GET("/api/registry/:repo/access", injectUser, RegistryAccess)
	r.GET("/api/registry/:repo/index.json", injectUser, RegistryIndex)
	r.GET("/api/registry/:repo/:itemType/:slug/*file", injectUser, DownloadRegistryFile)
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
// resolveRepoID
// ---------------------------------------------------------------------------

func TestResolveRepoID_Public(t *testing.T) {
	id, ok := resolveRepoID("public")
	if !ok || id != "public" {
		t.Fatalf("expected (\"public\", true), got (%q, %v)", id, ok)
	}
}

func TestResolveRepoID_NotFound(t *testing.T) {
	defer setupTestDB(t)()
	_, ok := resolveRepoID("nonexistent")
	if ok {
		t.Fatal("expected false for unknown repo")
	}
}

func TestResolveRepoID_Found(t *testing.T) {
	defer setupTestDB(t)()
	repo := models.Repository{ID: "repo-uuid-1", Name: "acme", OwnerID: "u1"}
	database.DB.Create(&repo)

	id, ok := resolveRepoID("acme")
	if !ok || id != "repo-uuid-1" {
		t.Fatalf("expected (\"repo-uuid-1\", true), got (%q, %v)", id, ok)
	}
}

// ---------------------------------------------------------------------------
// RegistryAccess
// ---------------------------------------------------------------------------

func TestRegistryAccess_PublicRepo(t *testing.T) {
	defer setupTestDB(t)()
	database.DB.Create(&models.CapabilityRegistry{
		ID: PublicRegistryID, Name: "public",
		SourceType: "internal", Visibility: "public", RepoID: "public", OwnerID: "system",
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

func TestRegistryAccess_NonExistentRepo(t *testing.T) {
	defer setupTestDB(t)()

	w := get(newRouter(""), "/api/registry/ghost/access")
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	var body map[string]bool
	json.NewDecoder(w.Body).Decode(&body)
	if body["public"] {
		t.Fatal("expected public=false for non-existent repo")
	}
}

func TestRegistryAccess_PrivateRepo(t *testing.T) {
	defer setupTestDB(t)()
	repo := models.Repository{ID: "repo-1", Name: "sangfor", OwnerID: "u1"}
	database.DB.Create(&repo)
	database.DB.Create(&models.CapabilityRegistry{
		ID: "reg-1", Name: "sangfor-reg",
		SourceType: "internal", Visibility: "repo", RepoID: "repo-1", OwnerID: "u1",
	})

	w := get(newRouter(""), "/api/registry/sangfor/access")
	var body map[string]bool
	json.NewDecoder(w.Body).Decode(&body)
	if body["public"] {
		t.Fatal("expected public=false for repo-visibility registry")
	}
}

// ---------------------------------------------------------------------------
// RegistryIndex
// ---------------------------------------------------------------------------

func TestRegistryIndex_PublicRepo_Anonymous(t *testing.T) {
	defer setupTestDB(t)()
	database.DB.Create(&models.CapabilityRegistry{
		ID: PublicRegistryID, Name: "public",
		SourceType: "internal", Visibility: "public", RepoID: "public", OwnerID: "system",
	})
	database.DB.Create(&models.CapabilityItem{
		ID: "item-1", RegistryID: PublicRegistryID, RepoID: "public",
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
		SourceType: "internal", Visibility: "public", RepoID: "public", OwnerID: "system",
	})
	database.DB.Create(&models.CapabilityItem{
		ID: "item-2", RegistryID: PublicRegistryID, RepoID: "public",
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

func TestRegistryIndex_PrivateRepo_Unauthenticated(t *testing.T) {
	defer setupTestDB(t)()
	repo := models.Repository{ID: "repo-2", Name: "internal", OwnerID: "u1"}
	database.DB.Create(&repo)
	database.DB.Create(&models.CapabilityRegistry{
		ID: "reg-2", Name: "internal-reg",
		SourceType: "internal", Visibility: "repo", RepoID: "repo-2", OwnerID: "u1",
	})

	w := get(newRouter(""), "/api/registry/internal/index.json")
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", w.Code)
	}
}

func TestRegistryIndex_PrivateRepo_NonMember(t *testing.T) {
	defer setupTestDB(t)()
	repo := models.Repository{ID: "repo-3", Name: "secret", OwnerID: "u1"}
	database.DB.Create(&repo)
	database.DB.Create(&models.CapabilityRegistry{
		ID: "reg-3", Name: "secret-reg",
		SourceType: "internal", Visibility: "repo", RepoID: "repo-3", OwnerID: "u1",
	})

	w := get(newRouter("stranger"), "/api/registry/secret/index.json")
	if w.Code != http.StatusForbidden {
		t.Fatalf("expected 403, got %d", w.Code)
	}
}

func TestRegistryIndex_PrivateRepo_Member(t *testing.T) {
	defer setupTestDB(t)()
	repo := models.Repository{ID: "repo-4", Name: "myrepo", OwnerID: "u1"}
	database.DB.Create(&repo)
	database.DB.Create(&models.CapabilityRegistry{
		ID: "reg-4", Name: "myrepo-reg",
		SourceType: "internal", Visibility: "repo", RepoID: "repo-4", OwnerID: "u1",
	})
	database.DB.Create(&models.RepoMember{
		ID: "mem-1", RepoID: "repo-4", UserID: "member-user", Role: "member",
	})
	database.DB.Create(&models.CapabilityItem{
		ID: "item-3", RegistryID: "reg-4", RepoID: "repo-4",
		Slug: "internal-tool", ItemType: "subagent",
		Name: "Internal Tool", Status: "active", CreatedBy: "u1",
		Content: "# Internal", Metadata: datatypes.JSON([]byte("{}")),
	})

	w := get(newRouter("member-user"), "/api/registry/myrepo/index.json")
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var body indexJSON
	json.NewDecoder(w.Body).Decode(&body)
	if len(body.Items) != 1 {
		t.Fatalf("expected 1 item, got %d", len(body.Items))
	}
}

func assertFilesContainExactly(t *testing.T, got []string, want ...string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("unexpected files: got %#v want %#v", got, want)
	}

	counts := make(map[string]int, len(want))
	for _, file := range want {
		counts[file]++
	}
	for _, file := range got {
		counts[file]--
	}
	for file, count := range counts {
		if count != 0 {
			t.Fatalf("unexpected files: got %#v want %#v", got, want)
		}
		_ = file
	}
}

func TestRegistryIndex_SkillWithAssets(t *testing.T) {
	defer setupTestDB(t)()
	database.DB.Create(&models.CapabilityRegistry{
		ID: PublicRegistryID, Name: "public",
		SourceType: "internal", Visibility: "public", RepoID: "public", OwnerID: "system",
	})
	database.DB.Create(&models.CapabilityItem{
		ID: "item-skill-assets", RegistryID: PublicRegistryID, RepoID: "public",
		Slug: "my-skill", ItemType: "skill",
		Name: "My Skill", Status: "active", CreatedBy: "system",
		Content: "# My Skill", Metadata: datatypes.JSON([]byte("{}")),
	})
	setupScript := "#!/bin/sh\necho setup\n"
	demoScript := "print('demo')\n"
	database.DB.Create(&models.CapabilityAsset{
		ID: "asset-skill-script", ItemID: "item-skill-assets",
		RelPath: "scripts/setup.sh", TextContent: &setupScript, MimeType: "text/x-shellscript",
		FileSize: int64(len(setupScript)), ContentSHA: "sha-script",
	})
	database.DB.Create(&models.CapabilityAsset{
		ID: "asset-skill-example", ItemID: "item-skill-assets",
		RelPath: "examples/demo.py", TextContent: &demoScript, MimeType: "text/x-python",
		FileSize: int64(len(demoScript)), ContentSHA: "sha-example",
	})

	w := get(newRouter(""), "/api/registry/public/index.json")
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var body indexJSON
	json.NewDecoder(w.Body).Decode(&body)
	if len(body.Items) != 1 {
		t.Fatalf("expected 1 item, got %d", len(body.Items))
	}
	assertFilesContainExactly(t, body.Items[0].Files, "SKILL.md", "scripts/setup.sh", "examples/demo.py")
}

func TestRegistryIndex_MCPWithAssets(t *testing.T) {
	defer setupTestDB(t)()
	database.DB.Create(&models.CapabilityRegistry{
		ID: PublicRegistryID, Name: "public",
		SourceType: "internal", Visibility: "public", RepoID: "public", OwnerID: "system",
	})
	meta := `{"hosting_type":"command","command":"npx"}`
	database.DB.Create(&models.CapabilityItem{
		ID: "item-mcp", RegistryID: PublicRegistryID, RepoID: "public",
		Slug: "my-tool", ItemType: "mcp",
		Name: "My Tool", Status: "active", CreatedBy: "system",
		Content: "", Metadata: datatypes.JSON([]byte(meta)),
	})
	assetText := "# MCP usage"
	database.DB.Create(&models.CapabilityAsset{
		ID: "asset-mcp-doc", ItemID: "item-mcp",
		RelPath: "docs/usage.md", TextContent: &assetText, MimeType: "text/markdown",
		FileSize: int64(len(assetText)), ContentSHA: "sha-doc",
	})

	w := get(newRouter(""), "/api/registry/public/index.json")
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var body indexJSON
	json.NewDecoder(w.Body).Decode(&body)
	if len(body.Items) != 1 {
		t.Fatalf("expected 1 item, got %d", len(body.Items))
	}
	item := body.Items[0]
	if item.MCP == nil {
		t.Fatal("expected mcp field to be set")
	}
	assertFilesContainExactly(t, item.Files, ".mcp.json", "docs/usage.md")
}

// ---------------------------------------------------------------------------
// DownloadRegistryFile
// ---------------------------------------------------------------------------

func TestDownloadRegistryFile_PublicItem(t *testing.T) {
	defer setupTestDB(t)()
	database.DB.Create(&models.CapabilityRegistry{
		ID: PublicRegistryID, Name: "public",
		SourceType: "internal", Visibility: "public", RepoID: "public", OwnerID: "system",
	})
	database.DB.Create(&models.CapabilityItem{
		ID: "item-dl", RegistryID: PublicRegistryID, RepoID: "public",
		Slug: "code-reviewer", ItemType: "skill",
		Name: "Code Reviewer", Status: "active", CreatedBy: "system",
		Content: "# Code Reviewer\nsome content", Metadata: datatypes.JSON([]byte("{}")),
	})

	w := get(newRouter(""), "/api/registry/public/skill/code-reviewer/SKILL.md")
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

func TestDownloadRegistryFile_MainFile(t *testing.T) {
	defer setupTestDB(t)()
	database.DB.Create(&models.CapabilityRegistry{
		ID: PublicRegistryID, Name: "public",
		SourceType: "internal", Visibility: "public", RepoID: "public", OwnerID: "system",
	})
	content := "# My Skill\nmain content"
	database.DB.Create(&models.CapabilityItem{
		ID: "item-dl-main", RegistryID: PublicRegistryID, RepoID: "public",
		Slug: "my-skill", ItemType: "skill",
		Name: "My Skill", Status: "active", CreatedBy: "system",
		Content: content, Metadata: datatypes.JSON([]byte("{}")),
	})

	w := get(newRouter(""), "/api/registry/public/skill/my-skill/SKILL.md")
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	if w.Body.String() != content {
		t.Fatalf("unexpected body: %s", w.Body.String())
	}
}

func TestDownloadRegistryFile_EmptyFile(t *testing.T) {
	defer setupTestDB(t)()
	database.DB.Create(&models.CapabilityRegistry{
		ID: PublicRegistryID, Name: "public",
		SourceType: "internal", Visibility: "public", RepoID: "public", OwnerID: "system",
	})
	content := "# My Skill\nmain content"
	database.DB.Create(&models.CapabilityItem{
		ID: "item-dl-empty", RegistryID: PublicRegistryID, RepoID: "public",
		Slug: "my-skill", ItemType: "skill",
		Name: "My Skill", Status: "active", CreatedBy: "system",
		Content: content, Metadata: datatypes.JSON([]byte("{}")),
	})

	w := get(newRouter(""), "/api/registry/public/skill/my-skill/")
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	if w.Body.String() != content {
		t.Fatalf("unexpected body: %s", w.Body.String())
	}
	if cd := w.Header().Get("Content-Disposition"); cd != `attachment; filename="SKILL.md"` {
		t.Fatalf("unexpected Content-Disposition: %s", cd)
	}
}

func TestDownloadRegistryFile_TextAsset(t *testing.T) {
	defer setupTestDB(t)()
	database.DB.Create(&models.CapabilityRegistry{
		ID: PublicRegistryID, Name: "public",
		SourceType: "internal", Visibility: "public", RepoID: "public", OwnerID: "system",
	})
	database.DB.Create(&models.CapabilityItem{
		ID: "item-dl-text-asset", RegistryID: PublicRegistryID, RepoID: "public",
		Slug: "my-skill", ItemType: "skill",
		Name: "My Skill", Status: "active", CreatedBy: "system",
		Content: "# My Skill\nmain content", Metadata: datatypes.JSON([]byte("{}")),
	})
	assetText := "#!/bin/bash\necho test"
	database.DB.Create(&models.CapabilityAsset{
		ID: "asset-text", ItemID: "item-dl-text-asset",
		RelPath: "scripts/setup.sh", TextContent: &assetText, MimeType: "text/x-sh",
		FileSize: int64(len(assetText)), ContentSHA: "sha-text",
	})

	w := get(newRouter(""), "/api/registry/public/skill/my-skill/scripts/setup.sh")
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	if w.Body.String() != assetText {
		t.Fatalf("unexpected body: %s", w.Body.String())
	}
	if cd := w.Header().Get("Content-Disposition"); cd != `attachment; filename="scripts/setup.sh"` {
		t.Fatalf("unexpected Content-Disposition: %s", cd)
	}
}

func TestDownloadRegistryFile_BinaryAsset(t *testing.T) {
	defer setupTestDB(t)()
	backend := newMemBackend()
	binaryData := []byte{0x89, 'P', 'N', 'G', '\r', '\n', 0x1a, '\n'}
	backend.data["test-key"] = binaryData
	oldBackend := StorageBackend
	StorageBackend = backend
	defer func() { StorageBackend = oldBackend }()

	database.DB.Create(&models.CapabilityRegistry{
		ID: PublicRegistryID, Name: "public",
		SourceType: "internal", Visibility: "public", RepoID: "public", OwnerID: "system",
	})
	database.DB.Create(&models.CapabilityItem{
		ID: "item-dl-binary-asset", RegistryID: PublicRegistryID, RepoID: "public",
		Slug: "my-skill", ItemType: "skill",
		Name: "My Skill", Status: "active", CreatedBy: "system",
		Content: "# My Skill\nmain content", Metadata: datatypes.JSON([]byte("{}")),
	})
	database.DB.Create(&models.CapabilityAsset{
		ID: "asset-binary", ItemID: "item-dl-binary-asset",
		RelPath: "image.png", StorageBackend: "local", StorageKey: "test-key",
		MimeType: "image/png", FileSize: int64(len(binaryData)), ContentSHA: "sha-binary",
	})

	w := get(newRouter(""), "/api/registry/public/skill/my-skill/image.png")
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	if ct := w.Header().Get("Content-Type"); ct != "image/png" {
		t.Fatalf("unexpected Content-Type: %s", ct)
	}
	if !bytes.Equal(w.Body.Bytes(), binaryData) {
		t.Fatalf("unexpected body: %v", w.Body.Bytes())
	}
}

func TestDownloadRegistryFile_NestedPath(t *testing.T) {
	defer setupTestDB(t)()
	database.DB.Create(&models.CapabilityRegistry{
		ID: PublicRegistryID, Name: "public",
		SourceType: "internal", Visibility: "public", RepoID: "public", OwnerID: "system",
	})
	database.DB.Create(&models.CapabilityItem{
		ID: "item-dl-nested", RegistryID: PublicRegistryID, RepoID: "public",
		Slug: "my-skill", ItemType: "skill",
		Name: "My Skill", Status: "active", CreatedBy: "system",
		Content: "# My Skill\nmain content", Metadata: datatypes.JSON([]byte("{}")),
	})
	assetText := "nested file"
	database.DB.Create(&models.CapabilityAsset{
		ID: "asset-nested", ItemID: "item-dl-nested",
		RelPath: "deep/nested/file.txt", TextContent: &assetText, MimeType: "text/plain",
		FileSize: int64(len(assetText)), ContentSHA: "sha-nested",
	})

	w := get(newRouter(""), "/api/registry/public/skill/my-skill/deep/nested/file.txt")
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
}

func TestDownloadRegistryFile_NonexistentFile(t *testing.T) {
	defer setupTestDB(t)()
	database.DB.Create(&models.CapabilityRegistry{
		ID: PublicRegistryID, Name: "public",
		SourceType: "internal", Visibility: "public", RepoID: "public", OwnerID: "system",
	})
	database.DB.Create(&models.CapabilityItem{
		ID: "item-dl-missing-file", RegistryID: PublicRegistryID, RepoID: "public",
		Slug: "my-skill", ItemType: "skill",
		Name: "My Skill", Status: "active", CreatedBy: "system",
		Content: "# My Skill\nmain content", Metadata: datatypes.JSON([]byte("{}")),
	})

	w := get(newRouter(""), "/api/registry/public/skill/my-skill/nonexistent.txt")
	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", w.Code)
	}
}

func TestDownloadRegistryFile_PathTraversal(t *testing.T) {
	defer setupTestDB(t)()
	database.DB.Create(&models.CapabilityRegistry{
		ID: PublicRegistryID, Name: "public",
		SourceType: "internal", Visibility: "public", RepoID: "public", OwnerID: "system",
	})
	database.DB.Create(&models.CapabilityItem{
		ID: "item-dl-path-traversal", RegistryID: PublicRegistryID, RepoID: "public",
		Slug: "my-skill", ItemType: "skill",
		Name: "My Skill", Status: "active", CreatedBy: "system",
		Content: "# My Skill\nmain content", Metadata: datatypes.JSON([]byte("{}")),
	})

	w := get(newRouter(""), "/api/registry/public/skill/my-skill/../../../etc/passwd")
	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", w.Code)
	}
}

func TestDownloadRegistryFile_NotFound(t *testing.T) {
	defer setupTestDB(t)()
	database.DB.Create(&models.CapabilityRegistry{
		ID: PublicRegistryID, Name: "public",
		SourceType: "internal", Visibility: "public", RepoID: "public", OwnerID: "system",
	})

	w := get(newRouter(""), "/api/registry/public/skill/nonexistent/SKILL.md")
	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", w.Code)
	}
}

func TestDownloadRegistryFile_RepoItem_Forbidden(t *testing.T) {
	defer setupTestDB(t)()
	repo := models.Repository{ID: "repo-5", Name: "corp", OwnerID: "u1"}
	database.DB.Create(&repo)
	database.DB.Create(&models.CapabilityRegistry{
		ID: "reg-5", Name: "corp-reg",
		SourceType: "internal", Visibility: "repo", RepoID: "repo-5", OwnerID: "u1",
	})
	database.DB.Create(&models.CapabilityItem{
		ID: "item-repo", RegistryID: "reg-5", RepoID: "repo-5",
		Slug: "secret-skill", ItemType: "skill",
		Name: "Secret", Status: "active", CreatedBy: "u1",
		Content: "secret", Metadata: datatypes.JSON([]byte("{}")),
	})

	w := get(newRouter("outsider"), "/api/registry/corp/skill/secret-skill/SKILL.md")
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
		SourceType: "internal", Visibility: "public", RepoID: "public", OwnerID: "system",
	})
	database.DB.Create(&models.CapabilityItem{
		ID: "item-byid", RegistryID: PublicRegistryID, RepoID: "public",
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
		RepoID: "public",
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
		RepoID: "public",
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
	si := models.CapabilityItem{RepoID: "public", Metadata: datatypes.JSON([]byte{})}
	if buildMCPConfig(si) != nil {
		t.Fatal("expected nil for empty metadata")
	}
}

func TestBuildMCPConfig_UnknownHostingType_PassThrough(t *testing.T) {
	raw := `{"hosting_type":"artifact","package_name":"my-pkg"}`
	si := models.CapabilityItem{RepoID: "public", Metadata: datatypes.JSON([]byte(raw))}
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
