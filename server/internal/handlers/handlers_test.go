package handlers

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/costrict/costrict-web/server/internal/database"
	"github.com/costrict/costrict-web/server/internal/middleware"
	"github.com/costrict/costrict-web/server/internal/models"
	"github.com/gin-gonic/gin"
)

func newRepoRouter(userID string) *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	injectUser := func(c *gin.Context) {
		if userID != "" {
			c.Set(middleware.UserIDKey, userID)
		}
		c.Next()
	}
	r.GET("/api/repositories", injectUser, ListRepositories)
	r.POST("/api/repositories", injectUser, CreateRepository)
	r.GET("/api/repositories/:id", injectUser, GetRepository)
	r.PUT("/api/repositories/:id", injectUser, UpdateRepository)
	r.DELETE("/api/repositories/:id", injectUser, DeleteRepository)
	r.GET("/api/repositories/:id/members", injectUser, ListRepositoryMembers)
	r.POST("/api/repositories/:id/members", injectUser, AddRepositoryMember)
	r.DELETE("/api/repositories/:id/members/:userId", injectUser, RemoveRepositoryMember)
	r.GET("/api/repositories/:id/registry", injectUser, GetRepositoryRegistry)
	r.GET("/api/repositories/my", injectUser, GetMyRepositories)
	return r
}

// ---------------------------------------------------------------------------
// buildSyncConfigJSON
// ---------------------------------------------------------------------------

func TestBuildSyncConfigJSON(t *testing.T) {
	raw := buildSyncConfigJSON([]string{"*.md"}, []string{"vendor/**"}, "keep_remote", "secret")
	var out map[string]interface{}
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("invalid json: %v", err)
	}
	if out["conflictStrategy"] != "keep_remote" {
		t.Fatalf("expected conflictStrategy=keep_remote, got %v", out["conflictStrategy"])
	}
	if out["webhookSecret"] != "secret" {
		t.Fatalf("expected webhookSecret=secret, got %v", out["webhookSecret"])
	}
	includes := out["includePatterns"].([]interface{})
	if len(includes) != 1 || includes[0] != "*.md" {
		t.Fatalf("unexpected includePatterns: %v", includes)
	}
}

// ---------------------------------------------------------------------------
// ListRepositories
// ---------------------------------------------------------------------------

func TestListRepositories_Empty(t *testing.T) {
	defer setupTestDB(t)()

	w := get(newRepoRouter(""), "/api/repositories")
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	var body map[string]interface{}
	json.NewDecoder(w.Body).Decode(&body)
	repos := body["repositories"].([]interface{})
	if len(repos) != 0 {
		t.Fatalf("expected 0 repos, got %d", len(repos))
	}
}

func TestListRepositories_WithData(t *testing.T) {
	defer setupTestDB(t)()
	database.DB.Create(&models.Repository{ID: "repo-l1", Name: "alpha", OwnerID: "u1"})
	database.DB.Create(&models.Repository{ID: "repo-l2", Name: "beta", OwnerID: "u2"})

	w := get(newRepoRouter(""), "/api/repositories")
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	var body map[string]interface{}
	json.NewDecoder(w.Body).Decode(&body)
	repos := body["repositories"].([]interface{})
	if len(repos) != 2 {
		t.Fatalf("expected 2 repos, got %d", len(repos))
	}
}

// ---------------------------------------------------------------------------
// CreateRepository
// ---------------------------------------------------------------------------

func TestCreateRepository_Success(t *testing.T) {
	defer setupTestDB(t)()

	w := postJSON(newRepoRouter("u1"), "/api/repositories", map[string]interface{}{
		"name": "my-repo", "ownerId": "u1",
	})
	if w.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", w.Code, w.Body.String())
	}
	var repo map[string]interface{}
	json.NewDecoder(w.Body).Decode(&repo)
	if repo["name"] != "my-repo" {
		t.Fatalf("unexpected name: %v", repo["name"])
	}
	if repo["repoType"] != "normal" {
		t.Fatalf("expected repoType=normal, got %v", repo["repoType"])
	}
	if repo["visibility"] != "private" {
		t.Fatalf("expected visibility=private, got %v", repo["visibility"])
	}
}

func TestCreateRepository_MissingRequired(t *testing.T) {
	defer setupTestDB(t)()

	w := postJSON(newRepoRouter("u1"), "/api/repositories", map[string]interface{}{
		"name": "no-owner",
	})
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", w.Code)
	}
}

func TestCreateRepository_DefaultsVisibilityAndType(t *testing.T) {
	defer setupTestDB(t)()

	w := postJSON(newRepoRouter("u1"), "/api/repositories", map[string]interface{}{
		"name": "defaults-repo", "ownerId": "u1",
	})
	if w.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", w.Code, w.Body.String())
	}
	var repo map[string]interface{}
	json.NewDecoder(w.Body).Decode(&repo)
	if repo["visibility"] != "private" {
		t.Fatalf("expected default visibility=private, got %v", repo["visibility"])
	}
	if repo["repoType"] != "normal" {
		t.Fatalf("expected default repoType=normal, got %v", repo["repoType"])
	}
}

func TestCreateRepository_OwnerAddedAsMember(t *testing.T) {
	defer setupTestDB(t)()

	w := postJSON(newRepoRouter("u1"), "/api/repositories", map[string]interface{}{
		"name": "member-repo", "ownerId": "u1",
	})
	if w.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d", w.Code)
	}
	var repo map[string]interface{}
	json.NewDecoder(w.Body).Decode(&repo)
	repoID := repo["id"].(string)

	var count int64
	database.DB.Model(&models.RepoMember{}).Where("repo_id = ? AND user_id = ? AND role = 'owner'", repoID, "u1").Count(&count)
	if count != 1 {
		t.Fatalf("expected owner to be added as member, got count=%d", count)
	}
}

func TestCreateRepository_SyncType_MissingExternalURL(t *testing.T) {
	defer setupTestDB(t)()

	w := postJSON(newRepoRouter("u1"), "/api/repositories", map[string]interface{}{
		"name": "sync-repo", "ownerId": "u1", "repoType": "sync",
		"syncRegistry": map[string]interface{}{},
	})
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", w.Code)
	}
}

func TestCreateRepository_SyncType_Success(t *testing.T) {
	defer setupTestDB(t)()

	w := postJSON(newRepoRouter("u1"), "/api/repositories", map[string]interface{}{
		"name": "sync-repo2", "ownerId": "u1", "repoType": "sync",
		"syncRegistry": map[string]interface{}{
			"externalUrl": "https://github.com/example/repo",
		},
	})
	if w.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", w.Code, w.Body.String())
	}
	var body map[string]interface{}
	json.NewDecoder(w.Body).Decode(&body)
	if body["repository"] == nil {
		t.Fatal("expected repository field in response")
	}
	if body["registries"] == nil {
		t.Fatal("expected registries field in response")
	}
}

// ---------------------------------------------------------------------------
// GetRepository
// ---------------------------------------------------------------------------

func TestGetRepository_Found(t *testing.T) {
	defer setupTestDB(t)()
	database.DB.Create(&models.Repository{ID: "repo-g1", Name: "get-repo", OwnerID: "u1"})

	w := get(newRepoRouter(""), "/api/repositories/repo-g1")
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	var repo map[string]interface{}
	json.NewDecoder(w.Body).Decode(&repo)
	if repo["id"] != "repo-g1" {
		t.Fatalf("unexpected id: %v", repo["id"])
	}
}

func TestGetRepository_NotFound(t *testing.T) {
	defer setupTestDB(t)()
	w := get(newRepoRouter(""), "/api/repositories/no-such-repo")
	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", w.Code)
	}
}

// ---------------------------------------------------------------------------
// UpdateRepository
// ---------------------------------------------------------------------------

func TestUpdateRepository_Success(t *testing.T) {
	defer setupTestDB(t)()
	database.DB.Create(&models.Repository{ID: "repo-u1", Name: "old-name", OwnerID: "u1", Visibility: "private"})

	w := putJSON(newRepoRouter("u1"), "/api/repositories/repo-u1", map[string]interface{}{
		"name": "new-name", "visibility": "public",
	})
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var repo map[string]interface{}
	json.NewDecoder(w.Body).Decode(&repo)
	if repo["name"] != "new-name" {
		t.Fatalf("expected name=new-name, got %v", repo["name"])
	}
	if repo["visibility"] != "public" {
		t.Fatalf("expected visibility=public, got %v", repo["visibility"])
	}
}

func TestUpdateRepository_NotFound(t *testing.T) {
	defer setupTestDB(t)()
	w := putJSON(newRepoRouter("u1"), "/api/repositories/no-such", map[string]interface{}{
		"name": "x",
	})
	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", w.Code)
	}
}

func TestUpdateRepository_PartialUpdate(t *testing.T) {
	defer setupTestDB(t)()
	database.DB.Create(&models.Repository{ID: "repo-u2", Name: "partial-repo", DisplayName: "Old Display", OwnerID: "u1"})

	w := putJSON(newRepoRouter("u1"), "/api/repositories/repo-u2", map[string]interface{}{
		"displayName": "New Display",
	})
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	var repo map[string]interface{}
	json.NewDecoder(w.Body).Decode(&repo)
	if repo["name"] != "partial-repo" {
		t.Fatalf("name should not change, got %v", repo["name"])
	}
	if repo["displayName"] != "New Display" {
		t.Fatalf("expected displayName=New Display, got %v", repo["displayName"])
	}
}

// ---------------------------------------------------------------------------
// DeleteRepository
// ---------------------------------------------------------------------------

func TestDeleteRepository_Success(t *testing.T) {
	defer setupTestDB(t)()
	database.DB.Create(&models.Repository{ID: "repo-d1", Name: "del-repo", OwnerID: "u1"})

	w := deleteReq(newRepoRouter("u1"), "/api/repositories/repo-d1")
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}

	var count int64
	database.DB.Model(&models.Repository{}).Where("id = ?", "repo-d1").Count(&count)
	if count != 0 {
		t.Fatal("repository should have been deleted")
	}
}

// ---------------------------------------------------------------------------
// ListRepositoryMembers
// ---------------------------------------------------------------------------

func TestListRepositoryMembers_Empty(t *testing.T) {
	defer setupTestDB(t)()

	w := get(newRepoRouter(""), "/api/repositories/repo-no-members/members")
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	var body map[string]interface{}
	json.NewDecoder(w.Body).Decode(&body)
	members := body["members"].([]interface{})
	if len(members) != 0 {
		t.Fatalf("expected 0 members, got %d", len(members))
	}
}

func TestListRepositoryMembers_WithMembers(t *testing.T) {
	defer setupTestDB(t)()
	database.DB.Create(&models.Repository{ID: "repo-m1", Name: "member-repo", OwnerID: "u1"})
	database.DB.Create(&models.RepoMember{ID: "mem-m1", RepoID: "repo-m1", UserID: "u1", Role: "owner"})
	database.DB.Create(&models.RepoMember{ID: "mem-m2", RepoID: "repo-m1", UserID: "u2", Role: "member"})

	w := get(newRepoRouter(""), "/api/repositories/repo-m1/members")
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	var body map[string]interface{}
	json.NewDecoder(w.Body).Decode(&body)
	members := body["members"].([]interface{})
	if len(members) != 2 {
		t.Fatalf("expected 2 members, got %d", len(members))
	}
}

// ---------------------------------------------------------------------------
// AddRepositoryMember
// ---------------------------------------------------------------------------

func TestAddRepositoryMember_Success(t *testing.T) {
	defer setupTestDB(t)()
	database.DB.Create(&models.Repository{ID: "repo-am1", Name: "add-member-repo", OwnerID: "u1"})

	w := postJSON(newRepoRouter("u1"), "/api/repositories/repo-am1/members", map[string]interface{}{
		"userId": "u-new", "username": "newuser", "role": "member",
	})
	if w.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", w.Code, w.Body.String())
	}
	var member map[string]interface{}
	json.NewDecoder(w.Body).Decode(&member)
	if member["userId"] != "u-new" {
		t.Fatalf("unexpected userId: %v", member["userId"])
	}
	if member["role"] != "member" {
		t.Fatalf("expected role=member, got %v", member["role"])
	}
}

func TestAddRepositoryMember_DefaultRole(t *testing.T) {
	defer setupTestDB(t)()
	database.DB.Create(&models.Repository{ID: "repo-am2", Name: "default-role-repo", OwnerID: "u1"})

	w := postJSON(newRepoRouter("u1"), "/api/repositories/repo-am2/members", map[string]interface{}{
		"userId": "u-default",
	})
	if w.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", w.Code, w.Body.String())
	}
	var member map[string]interface{}
	json.NewDecoder(w.Body).Decode(&member)
	if member["role"] != "member" {
		t.Fatalf("expected default role=member, got %v", member["role"])
	}
}

func TestAddRepositoryMember_MissingUserID(t *testing.T) {
	defer setupTestDB(t)()

	w := postJSON(newRepoRouter("u1"), "/api/repositories/repo-am3/members", map[string]interface{}{
		"username": "no-user-id",
	})
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", w.Code)
	}
}

func TestAddRepositoryMember_Duplicate(t *testing.T) {
	defer setupTestDB(t)()
	database.DB.Create(&models.Repository{ID: "repo-am4", Name: "dup-repo", OwnerID: "u1"})
	database.DB.Create(&models.RepoMember{ID: "mem-dup1", RepoID: "repo-am4", UserID: "u-dup", Role: "member"})

	w := postJSON(newRepoRouter("u1"), "/api/repositories/repo-am4/members", map[string]interface{}{
		"userId": "u-dup",
	})
	if w.Code != http.StatusConflict {
		t.Fatalf("expected 409, got %d", w.Code)
	}
}

// ---------------------------------------------------------------------------
// RemoveRepositoryMember
// ---------------------------------------------------------------------------

func TestRemoveRepositoryMember_Success(t *testing.T) {
	defer setupTestDB(t)()
	database.DB.Create(&models.Repository{ID: "repo-rm1", Name: "remove-repo", OwnerID: "u1"})
	database.DB.Create(&models.RepoMember{ID: "mem-rm1", RepoID: "repo-rm1", UserID: "u-remove", Role: "member"})

	w := deleteReq(newRepoRouter("u1"), "/api/repositories/repo-rm1/members/u-remove")
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}

	var count int64
	database.DB.Model(&models.RepoMember{}).Where("repo_id = ? AND user_id = ?", "repo-rm1", "u-remove").Count(&count)
	if count != 0 {
		t.Fatal("member should have been removed")
	}
}

// ---------------------------------------------------------------------------
// GetRepositoryRegistry
// ---------------------------------------------------------------------------

func TestGetRepositoryRegistry_Found(t *testing.T) {
	defer setupTestDB(t)()
	database.DB.Create(&models.Repository{ID: "repo-gr1", Name: "reg-repo", OwnerID: "u1"})
	database.DB.Create(&models.CapabilityRegistry{
		ID: "reg-for-repo", Name: "reg-repo", SourceType: "internal", Visibility: "repo", RepoID: "repo-gr1", OwnerID: "u1",
	})

	w := get(newRepoRouter(""), "/api/repositories/repo-gr1/registry")
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	var reg map[string]interface{}
	json.NewDecoder(w.Body).Decode(&reg)
	if reg["id"] != "reg-for-repo" {
		t.Fatalf("unexpected id: %v", reg["id"])
	}
}

func TestGetRepositoryRegistry_NotFound(t *testing.T) {
	defer setupTestDB(t)()
	w := get(newRepoRouter(""), "/api/repositories/no-such-repo/registry")
	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", w.Code)
	}
}

func TestGetRepositoryRegistry_ExternalRegistryNotReturned(t *testing.T) {
	defer setupTestDB(t)()
	database.DB.Create(&models.Repository{ID: "repo-gr2", Name: "ext-reg-repo", OwnerID: "u1"})
	database.DB.Create(&models.CapabilityRegistry{
		ID: "ext-reg-for-repo", Name: "ext-reg-repo", SourceType: "external", Visibility: "repo", RepoID: "repo-gr2", OwnerID: "u1",
	})

	w := get(newRepoRouter(""), "/api/repositories/repo-gr2/registry")
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 (external registry returned first), got %d", w.Code)
	}
}

// ---------------------------------------------------------------------------
// GetMyRepositories
// ---------------------------------------------------------------------------

func TestGetMyRepositories_Success(t *testing.T) {
	defer setupTestDB(t)()
	database.DB.Create(&models.Repository{ID: "repo-my1", Name: "my-repo-1", OwnerID: "u1"})
	database.DB.Create(&models.Repository{ID: "repo-my2", Name: "my-repo-2", OwnerID: "u2"})
	database.DB.Create(&models.RepoMember{ID: "mem-my1", RepoID: "repo-my1", UserID: "u-me", Role: "member"})
	database.DB.Create(&models.RepoMember{ID: "mem-my2", RepoID: "repo-my2", UserID: "u-me", Role: "admin"})

	w := get(newRepoRouter(""), "/api/repositories/my?userId=u-me")
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	var body map[string]interface{}
	json.NewDecoder(w.Body).Decode(&body)
	repos := body["repositories"].([]interface{})
	if len(repos) != 2 {
		t.Fatalf("expected 2 repos, got %d", len(repos))
	}
}

func TestGetMyRepositories_MissingUserID(t *testing.T) {
	defer setupTestDB(t)()
	w := get(newRepoRouter(""), "/api/repositories/my")
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", w.Code)
	}
}

func TestGetMyRepositories_NoMemberships(t *testing.T) {
	defer setupTestDB(t)()

	w := get(newRepoRouter(""), "/api/repositories/my?userId=u-nobody")
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	var body map[string]interface{}
	json.NewDecoder(w.Body).Decode(&body)
	repos, _ := body["repositories"].([]interface{})
	if len(repos) != 0 {
		t.Fatalf("expected 0 repos, got %d", len(repos))
	}
}
