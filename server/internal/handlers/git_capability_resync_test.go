package handlers

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/costrict/costrict-web/server/internal/database"
	"github.com/costrict/costrict-web/server/internal/middleware"
	"github.com/costrict/costrict-web/server/internal/models"
	"github.com/costrict/costrict-web/server/internal/systemrole"
	"github.com/gin-gonic/gin"
)

func setupResyncDB(t *testing.T) func() {
	cleanup := setupTestDB(t)
	db := database.GetDB()
	if err := db.Exec(`CREATE TABLE IF NOT EXISTS git_capability_repositories (
	 id TEXT PRIMARY KEY, git_server_id TEXT NOT NULL, git_repo_id INTEGER NOT NULL,
	 repository_id TEXT NOT NULL, registry_id TEXT NOT NULL, full_name TEXT NOT NULL,
	 repo_kind TEXT NOT NULL DEFAULT 'standalone', identification_status TEXT NOT NULL DEFAULT 'unknown',
	 visibility TEXT NOT NULL DEFAULT 'public', git_remote_url TEXT NOT NULL, default_branch TEXT NOT NULL,
	 last_synced_commit TEXT NOT NULL DEFAULT '', last_synced_at DATETIME, last_error TEXT NOT NULL DEFAULT '',
	 created_by TEXT NOT NULL, created_at DATETIME NOT NULL, updated_at DATETIME NOT NULL,
	 UNIQUE(git_server_id, git_repo_id), UNIQUE(repository_id), UNIQUE(registry_id))`).Error; err != nil {
		t.Fatal(err)
	}
	createGitCapabilitySyncJobTable(t)
	return cleanup
}

func seedResyncRepo(t *testing.T, id int64, valid bool) {
	t.Helper()
	repo := models.GitCapabilityRepository{ID: "repo-1", GitServerID: "gs-1", GitRepoID: id, RepositoryID: "r-1", RegistryID: "reg-1", FullName: "org/repo", GitRemoteURL: "https://git/repo", DefaultBranch: "main", CreatedBy: "u", CreatedAt: time.Now(), UpdatedAt: time.Now()}
	if !valid {
		repo.GitServerID = ""
	}
	if err := database.GetDB().Create(&repo).Error; err != nil {
		t.Fatal(err)
	}
}

func resyncRequest(r http.Handler, path string) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, path, nil)
	r.ServeHTTP(w, req)
	return w
}

func TestResyncGitCapabilityRepositoryValidationAndIdempotency(t *testing.T) {
	defer setupResyncDB(t)()
	api := NewGitCapabilityResyncAPI(database.GetDB())
	r := gin.New()
	r.POST("/resync/:git_repo_id", api.ResyncGitCapabilityRepository)
	for _, id := range []string{"0", "-1", "abc"} {
		if w := resyncRequest(r, "/resync/"+id); w.Code != 409 {
			t.Errorf("id %s: got %d", id, w.Code)
		}
	}
	if w := resyncRequest(r, "/resync/99"); w.Code != 404 {
		t.Fatalf("missing repo: %d", w.Code)
	}
	seedResyncRepo(t, 7, false)
	if w := resyncRequest(r, "/resync/7"); w.Code != 409 {
		t.Fatalf("invalid binding: %d", w.Code)
	}
	database.GetDB().Exec("DELETE FROM git_capability_repositories")
	seedResyncRepo(t, 7, true)
	first := resyncRequest(r, "/resync/7")
	if first.Code != 202 {
		t.Fatal(first.Code)
	}
	var one map[string]interface{}
	_ = json.Unmarshal(first.Body.Bytes(), &one)
	second := resyncRequest(r, "/resync/7")
	if second.Code != 202 {
		t.Fatal(second.Code)
	}
	var two map[string]interface{}
	_ = json.Unmarshal(second.Body.Bytes(), &two)
	if one["duplicate"] != false || two["duplicate"] != true || one["job_id"] != two["job_id"] || one["status"] != "queued" {
		t.Fatalf("responses: %#v %#v", one, two)
	}
	var count int64
	database.GetDB().Model(&models.GitCapabilitySyncJob{}).Count(&count)
	if count != 1 {
		t.Fatalf("jobs=%d", count)
	}
}

func TestResyncGitCapabilityRepositoryConcurrentIdempotent(t *testing.T) {
	defer setupResyncDB(t)()
	seedResyncRepo(t, 8, true)
	api := NewGitCapabilityResyncAPI(database.GetDB())
	r := gin.New()
	r.POST("/resync/:git_repo_id", api.ResyncGitCapabilityRepository)
	var wg sync.WaitGroup
	codes := make(chan int, 2)
	wg.Add(2)
	for i := 0; i < 2; i++ {
		go func() { defer wg.Done(); codes <- resyncRequest(r, "/resync/8").Code }()
	}
	wg.Wait()
	close(codes)
	for code := range codes {
		if code != 202 {
			t.Fatalf("concurrent status %d", code)
		}
	}
	var count int64
	database.GetDB().Model(&models.GitCapabilitySyncJob{}).Count(&count)
	if count != 1 {
		t.Fatalf("jobs=%d", count)
	}
}

func TestResyncGitCapabilityRepositoryMiddleware(t *testing.T) {
	defer setupResyncDB(t)()
	seedResyncRepo(t, 9, true)
	seedUser(t, "admin")
	seedUser(t, "user")
	if err := systemrole.NewSystemRoleService(database.GetDB()).GrantRole("admin", systemrole.SystemRolePlatformAdmin, "admin"); err != nil {
		t.Fatal(err)
	}
	api := NewGitCapabilityResyncAPI(database.GetDB())
	r := gin.New()
	g := r.Group("/api/admin")
	g.Use(systemrole.RequirePlatformAdmin(database.GetDB()))
	g.POST("/git-capability-repositories/:git_repo_id/resync", api.ResyncGitCapabilityRepository)
	if w := resyncRequest(r, "/api/admin/git-capability-repositories/9/resync"); w.Code != 401 {
		t.Errorf("unauth=%d", w.Code)
	}
	for _, uid := range []string{"user", "admin"} {
		r2 := gin.New()
		inject := func(c *gin.Context) { c.Set(middleware.UserIDKey, uid); c.Next() }
		g2 := r2.Group("/api/admin", inject, systemrole.RequirePlatformAdmin(database.GetDB()))
		g2.POST("/git-capability-repositories/:git_repo_id/resync", api.ResyncGitCapabilityRepository)
		rec := resyncRequest(r2, "/api/admin/git-capability-repositories/9/resync")
		want := 403
		if uid == "admin" {
			want = 202
		}
		if rec.Code != want {
			t.Errorf("%s=%d", uid, rec.Code)
		}
	}
}
