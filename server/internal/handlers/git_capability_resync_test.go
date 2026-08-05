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
		// An addressable row whose binding is unusable. It keeps its server id on
		// purpose: blanking that instead would make the row unreachable through
		// the server-scoped lookup and the 409 guard would never be exercised.
		repo.FullName = ""
	}
	if err := database.GetDB().Create(&repo).Error; err != nil {
		t.Fatal(err)
	}
}

// seedResyncRepoOn seeds one binding with an explicit server, so a test can put
// the same numeric repo id on two different servers — which the table allows,
// because its uniqueness is on the pair.
func seedResyncRepoOn(t *testing.T, rowID, serverID string, repoID int64, fullName string) {
	t.Helper()
	repo := models.GitCapabilityRepository{
		ID: rowID, GitServerID: serverID, GitRepoID: repoID,
		RepositoryID: "r-" + rowID, RegistryID: "reg-" + rowID, FullName: fullName,
		GitRemoteURL: "https://" + serverID + "/" + fullName, DefaultBranch: "main",
		CreatedBy: "u", CreatedAt: time.Now(), UpdatedAt: time.Now(),
	}
	if err := database.GetDB().Create(&repo).Error; err != nil {
		t.Fatal(err)
	}
}

// resyncRouter mounts the endpoint at its real shape: BOTH halves of the
// repository identity are path segments.
func resyncRouter() *gin.Engine {
	r := gin.New()
	api := NewGitCapabilityResyncAPI(database.GetDB())
	r.POST("/resync/:git_server_id/:git_repo_id", api.ResyncGitCapabilityRepository)
	return r
}

func resyncRequest(r http.Handler, path string) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, path, nil)
	r.ServeHTTP(w, req)
	return w
}

func TestResyncGitCapabilityRepositoryValidationAndIdempotency(t *testing.T) {
	defer setupResyncDB(t)()
	r := resyncRouter()
	for _, id := range []string{"0", "-1", "abc"} {
		if w := resyncRequest(r, "/resync/gs-1/"+id); w.Code != 409 {
			t.Errorf("id %s: got %d", id, w.Code)
		}
	}
	if w := resyncRequest(r, "/resync/gs-1/99"); w.Code != 404 {
		t.Fatalf("missing repo: %d", w.Code)
	}
	seedResyncRepo(t, 7, false)
	if w := resyncRequest(r, "/resync/gs-1/7"); w.Code != 409 {
		t.Fatalf("invalid binding: %d", w.Code)
	}
	// Same repo id, a server that has no such binding: 404, never a fallback to
	// the row another server owns.
	if w := resyncRequest(r, "/resync/gs-other/7"); w.Code != 404 {
		t.Fatalf("unknown server must not resolve to another server's row: %d", w.Code)
	}
	database.GetDB().Exec("DELETE FROM git_capability_repositories")
	seedResyncRepo(t, 7, true)
	first := resyncRequest(r, "/resync/gs-1/7")
	if first.Code != 202 {
		t.Fatal(first.Code)
	}
	var one map[string]interface{}
	_ = json.Unmarshal(first.Body.Bytes(), &one)
	second := resyncRequest(r, "/resync/gs-1/7")
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

// A Gitea repository id is unique only inside one server. With two servers
// carrying the same id, a lookup keyed on the id alone returns whichever row
// the database happens to yield first — and the endpoint then queues a job for
// THAT repository while answering 202 for the one the administrator asked for.
// The intended repository is never resynced and nothing reports a failure.
func TestResyncGitCapabilityRepositoryDisambiguatesByGitServer(t *testing.T) {
	defer setupResyncDB(t)()
	// Seeded in this order so a single-key query resolves to gs-a: the test has
	// to be able to fail.
	seedResyncRepoOn(t, "repo-a", "gs-a", 42, "org-a/alpha")
	seedResyncRepoOn(t, "repo-b", "gs-b", 42, "org-b/beta")
	r := resyncRouter()

	for _, tc := range []struct{ server, wantFullName string }{
		{"gs-b", "org-b/beta"},
		{"gs-a", "org-a/alpha"},
	} {
		w := resyncRequest(r, "/resync/"+tc.server+"/42")
		if w.Code != 202 {
			t.Fatalf("%s: status %d", tc.server, w.Code)
		}
		var body map[string]interface{}
		if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
			t.Fatalf("%s: decode: %v", tc.server, err)
		}
		jobID, _ := body["job_id"].(string)
		var job models.GitCapabilitySyncJob
		if err := database.GetDB().Where("id = ?", jobID).First(&job).Error; err != nil {
			t.Fatalf("%s: load queued job: %v", tc.server, err)
		}
		if job.GitServerID != tc.server || job.RepoFullName != tc.wantFullName {
			t.Fatalf("resync of %s/42 queued a job for %s/%s, want %s/%s",
				tc.server, job.GitServerID, job.RepoFullName, tc.server, tc.wantFullName)
		}
	}

	// Two distinct repositories means two distinct jobs; the per-server delivery
	// key must not collapse them into one.
	var count int64
	database.GetDB().Model(&models.GitCapabilitySyncJob{}).Count(&count)
	if count != 2 {
		t.Fatalf("jobs=%d, want 2 (one per server)", count)
	}
}

func TestResyncGitCapabilityRepositoryConcurrentIdempotent(t *testing.T) {
	defer setupResyncDB(t)()
	seedResyncRepo(t, 8, true)
	r := resyncRouter()
	var wg sync.WaitGroup
	codes := make(chan int, 2)
	wg.Add(2)
	for i := 0; i < 2; i++ {
		go func() { defer wg.Done(); codes <- resyncRequest(r, "/resync/gs-1/8").Code }()
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
	const route = "/git-capability-repositories/:git_server_id/:git_repo_id/resync"
	const path = "/api/admin/git-capability-repositories/gs-1/9/resync"
	r := gin.New()
	g := r.Group("/api/admin")
	g.Use(systemrole.RequirePlatformAdmin(database.GetDB()))
	g.POST(route, api.ResyncGitCapabilityRepository)
	if w := resyncRequest(r, path); w.Code != 401 {
		t.Errorf("unauth=%d", w.Code)
	}
	for _, uid := range []string{"user", "admin"} {
		r2 := gin.New()
		inject := func(c *gin.Context) { c.Set(middleware.UserIDKey, uid); c.Next() }
		g2 := r2.Group("/api/admin", inject, systemrole.RequirePlatformAdmin(database.GetDB()))
		g2.POST(route, api.ResyncGitCapabilityRepository)
		rec := resyncRequest(r2, path)
		want := 403
		if uid == "admin" {
			want = 202
		}
		if rec.Code != want {
			t.Errorf("%s=%d", uid, rec.Code)
		}
	}
}
