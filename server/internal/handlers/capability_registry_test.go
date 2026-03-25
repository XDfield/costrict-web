package handlers

import (
	"encoding/json"
	"fmt"
	"net/http"
	"testing"

	"github.com/costrict/costrict-web/server/internal/database"
	"github.com/costrict/costrict-web/server/internal/middleware"
	"github.com/costrict/costrict-web/server/internal/models"
	"github.com/gin-gonic/gin"
	"gorm.io/datatypes"
)

func newRegistryRouter(userID string) *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	injectUser := func(c *gin.Context) {
		if userID != "" {
			c.Set(middleware.UserIDKey, userID)
		}
		c.Next()
	}
	r.GET("/api/registries", injectUser, ListRegistries)
	r.POST("/api/registries", injectUser, CreateRegistry)
	r.GET("/api/registries/:id", injectUser, GetRegistry)
	r.PUT("/api/registries/:id", injectUser, UpdateRegistry)
	r.DELETE("/api/registries/:id", injectUser, DeleteRegistry)
	r.POST("/api/registries/ensure-personal", injectUser, EnsurePersonalRegistry)
	r.GET("/api/registries/my", injectUser, ListMyRegistries)
	r.GET("/api/items/my", injectUser, ListMyItems)
	r.PUT("/api/registries/:id/transfer", injectUser, TransferRegistry)
	return r
}

// ---------------------------------------------------------------------------
// ListRegistries
// ---------------------------------------------------------------------------

func TestListRegistries_Anonymous_OnlyPublic(t *testing.T) {
	defer setupTestDB(t)()
	database.DB.Create(&models.CapabilityRegistry{
		ID: "pub-r1", Name: "public-reg", SourceType: "internal", Visibility: "public", RepoID: "public", OwnerID: "system",
	})
	database.DB.Create(&models.CapabilityRegistry{
		ID: "repo-r1", Name: "repo-reg", SourceType: "internal", Visibility: "repo", RepoID: "repo-1", OwnerID: "u1",
	})

	w := get(newRegistryRouter(""), "/api/registries")
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	var body map[string]interface{}
	json.NewDecoder(w.Body).Decode(&body)
	regs := body["registries"].([]interface{})
	if len(regs) != 1 {
		t.Fatalf("expected 1 public registry for anonymous, got %d", len(regs))
	}
}

func TestListRegistries_FilterByRepoId(t *testing.T) {
	defer setupTestDB(t)()
	database.DB.Create(&models.CapabilityRegistry{
		ID: "repo-r2", Name: "repo-reg2", SourceType: "internal", Visibility: "repo", RepoID: "repo-filter", OwnerID: "u1",
	})
	database.DB.Create(&models.CapabilityRegistry{
		ID: "repo-r3", Name: "repo-reg3", SourceType: "internal", Visibility: "repo", RepoID: "repo-other", OwnerID: "u1",
	})

	w := get(newRegistryRouter(""), "/api/registries?repoId=repo-filter")
	var body map[string]interface{}
	json.NewDecoder(w.Body).Decode(&body)
	regs := body["registries"].([]interface{})
	if len(regs) != 1 {
		t.Fatalf("expected 1 registry for repo-filter, got %d", len(regs))
	}
}

func TestListRegistries_AuthUser_IncludesOrgAndPersonal(t *testing.T) {
	defer setupTestDB(t)()
	database.DB.Create(&models.CapabilityRegistry{
		ID: "pub-r2", Name: "public-r2", SourceType: "internal", Visibility: "public", RepoID: "public", OwnerID: "system",
	})
	database.DB.Create(&models.CapabilityRegistry{
		ID: "repo-r4", Name: "repo-r4", SourceType: "internal", Visibility: "repo", RepoID: "repo-2", OwnerID: "u2",
	})
	database.DB.Create(&models.CapabilityRegistry{
		ID: "priv-r1", Name: "private-r1", SourceType: "internal", Visibility: "private", RepoID: "", OwnerID: "auth-user",
	})
	database.DB.Create(&models.RepoMember{
		ID: "mem-au", RepoID: "repo-2", UserID: "auth-user", Role: "member",
	})

	w := get(newRegistryRouter("auth-user"), "/api/registries")
	var body map[string]interface{}
	json.NewDecoder(w.Body).Decode(&body)
	regs := body["registries"].([]interface{})
	if len(regs) != 3 {
		t.Fatalf("expected 3 registries (public+org+personal), got %d", len(regs))
	}
}

// ---------------------------------------------------------------------------
// CreateRegistry
// ---------------------------------------------------------------------------

func TestCreateRegistry_Success(t *testing.T) {
	defer setupTestDB(t)()

	w := postJSON(newRegistryRouter("u1"), "/api/registries", map[string]interface{}{
		"name": "My Registry", "ownerId": "u1", "visibility": "public",
	})
	if w.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", w.Code, w.Body.String())
	}
	var reg map[string]interface{}
	json.NewDecoder(w.Body).Decode(&reg)
	if reg["name"] != "My Registry" {
		t.Fatalf("unexpected name: %v", reg["name"])
	}
}

func TestCreateRegistry_MissingRequired(t *testing.T) {
	defer setupTestDB(t)()

	// name is the only required field now; ownerId comes from auth context
	w := postJSON(newRegistryRouter("u1"), "/api/registries", map[string]interface{}{})
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", w.Code)
	}
}

// ---------------------------------------------------------------------------
// GetRegistry
// ---------------------------------------------------------------------------

func TestGetRegistry_Found(t *testing.T) {
	defer setupTestDB(t)()
	database.DB.Create(&models.CapabilityRegistry{
		ID: "reg-get1", Name: "get-reg", SourceType: "internal", Visibility: "public", RepoID: "repo-1", OwnerID: "u1",
	})

	w := get(newRegistryRouter(""), "/api/registries/reg-get1")
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	var reg map[string]interface{}
	json.NewDecoder(w.Body).Decode(&reg)
	if reg["id"] != "reg-get1" {
		t.Fatalf("unexpected id: %v", reg["id"])
	}
}

func TestGetRegistry_NotFound(t *testing.T) {
	defer setupTestDB(t)()
	w := get(newRegistryRouter(""), "/api/registries/no-such-reg")
	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", w.Code)
	}
}

// ---------------------------------------------------------------------------
// UpdateRegistry
// ---------------------------------------------------------------------------

func TestUpdateRegistry_Success(t *testing.T) {
	defer setupTestDB(t)()
	database.DB.Create(&models.CapabilityRegistry{
		ID: "reg-upd1", Name: "old-name", SourceType: "internal", Visibility: "repo", RepoID: "repo-1", OwnerID: "u1",
	})

	w := putJSON(newRegistryRouter("u1"), "/api/registries/reg-upd1", map[string]interface{}{
		"name": "new-name", "visibility": "public",
	})
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var reg map[string]interface{}
	json.NewDecoder(w.Body).Decode(&reg)
	if reg["name"] != "new-name" {
		t.Fatalf("expected name=new-name, got %v", reg["name"])
	}
	if reg["visibility"] != "public" {
		t.Fatalf("expected visibility=public, got %v", reg["visibility"])
	}
}

func TestUpdateRegistry_NotFound(t *testing.T) {
	defer setupTestDB(t)()
	w := putJSON(newRegistryRouter("u1"), "/api/registries/no-such", map[string]interface{}{
		"name": "x",
	})
	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", w.Code)
	}
}

// ---------------------------------------------------------------------------
// DeleteRegistry
// ---------------------------------------------------------------------------

func TestDeleteRegistry_Success(t *testing.T) {
	defer setupTestDB(t)()
	database.DB.Create(&models.CapabilityRegistry{
		ID: "reg-del1", Name: "del-reg", SourceType: "internal", Visibility: "public", RepoID: "repo-1", OwnerID: "u1",
	})

	w := deleteReq(newRegistryRouter("u1"), "/api/registries/reg-del1")
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}

	var count int64
	database.DB.Model(&models.CapabilityRegistry{}).Where("id = ?", "reg-del1").Count(&count)
	if count != 0 {
		t.Fatal("registry should have been deleted")
	}
}

// ---------------------------------------------------------------------------
// EnsurePersonalRegistry
// ---------------------------------------------------------------------------

func TestEnsurePersonalRegistry_Creates(t *testing.T) {
	defer setupTestDB(t)()

	w := postJSON(newRegistryRouter("user-new"), "/api/registries/ensure-personal", map[string]interface{}{
		"username": "alice",
	})
	if w.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", w.Code, w.Body.String())
	}
	var reg map[string]interface{}
	json.NewDecoder(w.Body).Decode(&reg)
	if reg["name"] != "alice-skills" {
		t.Fatalf("expected name=alice-skills, got %v", reg["name"])
	}
}

func TestEnsurePersonalRegistry_ReturnsExisting(t *testing.T) {
	defer setupTestDB(t)()
	database.DB.Create(&models.CapabilityRegistry{
		ID: "existing-personal", Name: "bob-skills", SourceType: "internal",
		Visibility: "public", RepoID: "", OwnerID: "user-bob",
	})

	w := postJSON(newRegistryRouter("user-bob"), "/api/registries/ensure-personal", map[string]interface{}{
		"username": "bob",
	})
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 for existing registry, got %d", w.Code)
	}
	var reg map[string]interface{}
	json.NewDecoder(w.Body).Decode(&reg)
	if reg["id"] != "existing-personal" {
		t.Fatalf("expected existing registry id, got %v", reg["id"])
	}
}

func TestEnsurePersonalRegistry_DefaultName(t *testing.T) {
	defer setupTestDB(t)()

	w := postJSON(newRegistryRouter("user-noname"), "/api/registries/ensure-personal", map[string]interface{}{})
	if w.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", w.Code, w.Body.String())
	}
	var reg map[string]interface{}
	json.NewDecoder(w.Body).Decode(&reg)
	if reg["name"] != "personal" {
		t.Fatalf("expected name=personal, got %v", reg["name"])
	}
}

func TestEnsurePersonalRegistry_MissingOwnerID(t *testing.T) {
	defer setupTestDB(t)()

	// No authenticated user → should return 401
	w := postJSON(newRegistryRouter(""), "/api/registries/ensure-personal", map[string]interface{}{
		"username": "nobody",
	})
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", w.Code)
	}
}

// ---------------------------------------------------------------------------
// ListMyRegistries
// ---------------------------------------------------------------------------

func TestListMyRegistries_Success(t *testing.T) {
	defer setupTestDB(t)()
	database.DB.Create(&models.CapabilityRegistry{
		ID: "my-r1", Name: "my-reg1", SourceType: "internal", Visibility: "public", RepoID: "", OwnerID: "owner-a",
	})
	database.DB.Create(&models.CapabilityRegistry{
		ID: "my-r2", Name: "my-reg2", SourceType: "internal", Visibility: "public", RepoID: "", OwnerID: "owner-a",
	})
	database.DB.Create(&models.CapabilityRegistry{
		ID: "other-r", Name: "other-reg", SourceType: "internal", Visibility: "public", RepoID: "", OwnerID: "owner-b",
	})

	w := get(newRegistryRouter("owner-a"), "/api/registries/my")
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	var body map[string]interface{}
	json.NewDecoder(w.Body).Decode(&body)
	regs := body["registries"].([]interface{})
	if len(regs) != 2 {
		t.Fatalf("expected 2 registries for owner-a, got %d", len(regs))
	}
}

func TestListMyRegistries_MissingOwnerID(t *testing.T) {
	defer setupTestDB(t)()
	// No authenticated user → should return 401
	w := get(newRegistryRouter(""), "/api/registries/my")
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", w.Code)
	}
}

// ---------------------------------------------------------------------------
// ListMyItems
// ---------------------------------------------------------------------------

func TestListMyItems_Success(t *testing.T) {
	defer setupTestDB(t)()
	database.DB.Create(&models.Repository{
		ID: "repo-my-items", Name: "my-repo", OwnerID: "item-owner",
	})
	database.DB.Create(&models.CapabilityRegistry{
		ID: "my-items-reg", Name: "my-items-r", SourceType: "internal", Visibility: "public", RepoID: "repo-my-items", OwnerID: "item-owner",
	})
	database.DB.Create(&models.CapabilityItem{
		ID: "my-item-1", RegistryID: "my-items-reg", Slug: "my-skill", ItemType: "skill",
		Name: "My Skill", Status: "active", CreatedBy: "item-owner", Metadata: datatypes.JSON([]byte("{}")),
	})
	database.DB.Create(&models.CapabilityItem{
		ID: "my-item-2", RegistryID: "my-items-reg", Slug: "my-cmd", ItemType: "command",
		Name: "My Cmd", Status: "active", CreatedBy: "item-owner", Metadata: datatypes.JSON([]byte("{}")),
	})

	w := get(newRegistryRouter("item-owner"), "/api/items/my")
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	var body map[string]interface{}
	json.NewDecoder(w.Body).Decode(&body)
	items := body["items"].([]interface{})
	if len(items) != 2 {
		t.Fatalf("expected 2 items, got %d", len(items))
	}
	// Verify repo info is present
	first := items[0].(map[string]interface{})
	if first["repoId"] != "repo-my-items" {
		t.Fatalf("expected repoId 'repo-my-items', got %v", first["repoId"])
	}
	if first["repoName"] != "my-repo" {
		t.Fatalf("expected repoName 'my-repo', got %v", first["repoName"])
	}
	// Verify pagination metadata
	if body["total"].(float64) != 2 {
		t.Fatalf("expected total=2, got %v", body["total"])
	}
	if body["page"].(float64) != 1 {
		t.Fatalf("expected page=1, got %v", body["page"])
	}
	if body["hasMore"].(bool) != false {
		t.Fatalf("expected hasMore=false, got %v", body["hasMore"])
	}
}

func TestListMyItems_FilterByType(t *testing.T) {
	defer setupTestDB(t)()
	database.DB.Create(&models.CapabilityRegistry{
		ID: "my-items-reg2", Name: "my-items-r2", SourceType: "internal", Visibility: "public", RepoID: "", OwnerID: "item-owner2",
	})
	database.DB.Create(&models.CapabilityItem{
		ID: "my-item-3", RegistryID: "my-items-reg2", Slug: "my-skill2", ItemType: "skill",
		Name: "My Skill2", Status: "active", CreatedBy: "item-owner2", Metadata: datatypes.JSON([]byte("{}")),
	})
	database.DB.Create(&models.CapabilityItem{
		ID: "my-item-4", RegistryID: "my-items-reg2", Slug: "my-cmd2", ItemType: "command",
		Name: "My Cmd2", Status: "active", CreatedBy: "item-owner2", Metadata: datatypes.JSON([]byte("{}")),
	})

	w := get(newRegistryRouter("item-owner2"), "/api/items/my?type=skill")
	var body map[string]interface{}
	json.NewDecoder(w.Body).Decode(&body)
	items := body["items"].([]interface{})
	if len(items) != 1 {
		t.Fatalf("expected 1 skill item, got %d", len(items))
	}
	if body["total"].(float64) != 1 {
		t.Fatalf("expected total=1, got %v", body["total"])
	}
}

func TestListMyItems_Pagination(t *testing.T) {
	defer setupTestDB(t)()
	database.DB.Create(&models.CapabilityRegistry{
		ID: "my-items-reg3", Name: "my-items-r3", SourceType: "internal", Visibility: "public", RepoID: "", OwnerID: "item-owner3",
	})
	for i := 0; i < 5; i++ {
		database.DB.Create(&models.CapabilityItem{
			ID: fmt.Sprintf("page-item-%d", i), RegistryID: "my-items-reg3",
			Slug: fmt.Sprintf("slug-%d", i), ItemType: "skill",
			Name: fmt.Sprintf("Skill %d", i), Status: "active", CreatedBy: "item-owner3",
			Metadata: datatypes.JSON([]byte("{}")),
		})
	}

	// Page 1: pageSize=2
	w := get(newRegistryRouter("item-owner3"), "/api/items/my?page=1&pageSize=2")
	var body map[string]interface{}
	json.NewDecoder(w.Body).Decode(&body)
	items := body["items"].([]interface{})
	if len(items) != 2 {
		t.Fatalf("page1: expected 2 items, got %d", len(items))
	}
	if body["total"].(float64) != 5 {
		t.Fatalf("page1: expected total=5, got %v", body["total"])
	}
	if body["hasMore"].(bool) != true {
		t.Fatalf("page1: expected hasMore=true")
	}

	// Page 3: last page with 1 item
	w = get(newRegistryRouter("item-owner3"), "/api/items/my?page=3&pageSize=2")
	json.NewDecoder(w.Body).Decode(&body)
	items = body["items"].([]interface{})
	if len(items) != 1 {
		t.Fatalf("page3: expected 1 item, got %d", len(items))
	}
	if body["hasMore"].(bool) != false {
		t.Fatalf("page3: expected hasMore=false")
	}
}

func TestListMyItems_Unauthenticated(t *testing.T) {
	defer setupTestDB(t)()
	w := get(newRegistryRouter(""), "/api/items/my")
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", w.Code)
	}
}


func TestListMyItems_IncludesCreatedByInOtherRegistries(t *testing.T) {
	defer setupTestDB(t)()
	// Registry owned by "system", not by "user-abc".
	database.DB.Create(&models.CapabilityRegistry{
		ID: "public-reg", Name: "public", SourceType: "internal", Visibility: "public", RepoID: "public", OwnerID: "system",
	})
	// Item created by "user-abc" in the public registry.
	database.DB.Create(&models.CapabilityItem{
		ID: "pub-item-1", RegistryID: "public-reg", RepoID: "public", Slug: "pub-skill", ItemType: "skill",
		Name: "Public Skill", Status: "active", CreatedBy: "user-abc", Metadata: datatypes.JSON([]byte("{}")),
	})
	// Item created by someone else — should NOT appear.
	database.DB.Create(&models.CapabilityItem{
		ID: "pub-item-2", RegistryID: "public-reg", RepoID: "public", Slug: "other-skill", ItemType: "skill",
		Name: "Other Skill", Status: "active", CreatedBy: "other-user", Metadata: datatypes.JSON([]byte("{}")),
	})

	w := get(newRegistryRouter("user-abc"), "/api/items/my")
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	var body map[string]interface{}
	json.NewDecoder(w.Body).Decode(&body)
	items := body["items"].([]interface{})
	if len(items) != 1 {
		t.Fatalf("expected 1 item (created_by match), got %d", len(items))
	}
	first := items[0].(map[string]interface{})
	if first["id"] != "pub-item-1" {
		t.Fatalf("expected pub-item-1, got %s", first["id"])
	}
}

func TestListMyItems_IncludesCommandsFromPublicAndOwnedRepo(t *testing.T) {
	defer setupTestDB(t)()

	const userID = "user-cmd-owner"

	// 1. Public registry (owned by "system", not by the user).
	database.DB.Create(&models.CapabilityRegistry{
		ID: "pub-reg-cmd", Name: "public", SourceType: "internal", Visibility: "public", RepoID: "public", OwnerID: "system",
	})
	// 2. User's own repo + registry.
	database.DB.Create(&models.Repository{
		ID: "repo-cmd-user", Name: "user-cmd-repo", OwnerID: userID,
	})
	database.DB.Create(&models.CapabilityRegistry{
		ID: "reg-cmd-user", Name: "user-reg", SourceType: "internal", Visibility: "repo", RepoID: "repo-cmd-user", OwnerID: userID,
	})

	// Command created by the user in the public registry.
	database.DB.Create(&models.CapabilityItem{
		ID: "cmd-in-public", RegistryID: "pub-reg-cmd", RepoID: "public",
		Slug: "pub-cmd", ItemType: "command",
		Name: "Public Command", Status: "active", CreatedBy: userID,
		Metadata: datatypes.JSON([]byte("{}")),
	})
	// Command created by the user in their own repo registry.
	database.DB.Create(&models.CapabilityItem{
		ID: "cmd-in-own-repo", RegistryID: "reg-cmd-user", RepoID: "repo-cmd-user",
		Slug: "own-cmd", ItemType: "command",
		Name: "Own Repo Command", Status: "active", CreatedBy: userID,
		Metadata: datatypes.JSON([]byte("{}")),
	})
	// Command created by another user in the public registry — should NOT appear.
	database.DB.Create(&models.CapabilityItem{
		ID: "cmd-other-user", RegistryID: "pub-reg-cmd", RepoID: "public",
		Slug: "other-cmd", ItemType: "command",
		Name: "Other User Command", Status: "active", CreatedBy: "someone-else",
		Metadata: datatypes.JSON([]byte("{}")),
	})

	w := get(newRegistryRouter(userID), "/api/items/my?type=command")
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	var body map[string]interface{}
	json.NewDecoder(w.Body).Decode(&body)
	items := body["items"].([]interface{})
	if len(items) != 2 {
		t.Fatalf("expected 2 command items (public + own repo), got %d", len(items))
	}
	if body["total"].(float64) != 2 {
		t.Fatalf("expected total=2, got %v", body["total"])
	}

	// Collect returned item IDs and verify both commands are present.
	ids := map[string]bool{}
	for _, raw := range items {
		item := raw.(map[string]interface{})
		ids[item["id"].(string)] = true
	}
	if !ids["cmd-in-public"] {
		t.Fatal("expected cmd-in-public to be returned")
	}
	if !ids["cmd-in-own-repo"] {
		t.Fatal("expected cmd-in-own-repo to be returned")
	}
	if ids["cmd-other-user"] {
		t.Fatal("cmd-other-user should NOT be returned")
	}
}

func TestListMyItems_NoDuplicateWhenBothMatch(t *testing.T) {
	defer setupTestDB(t)()
	// Registry owned by the user.
	database.DB.Create(&models.CapabilityRegistry{
		ID: "user-reg", Name: "user-r", SourceType: "internal", Visibility: "public", RepoID: "user-repo", OwnerID: "user-dedup",
	})
	// Item in user's own registry AND created_by = user -> matches both sides of OR.
	database.DB.Create(&models.CapabilityItem{
		ID: "dedup-item", RegistryID: "user-reg", RepoID: "user-repo", Slug: "dedup-skill", ItemType: "skill",
		Name: "Dedup Skill", Status: "active", CreatedBy: "user-dedup", Metadata: datatypes.JSON([]byte("{}")),
	})

	w := get(newRegistryRouter("user-dedup"), "/api/items/my")
	var body map[string]interface{}
	json.NewDecoder(w.Body).Decode(&body)
	items := body["items"].([]interface{})
	if len(items) != 1 {
		t.Fatalf("expected exactly 1 item (no duplicates), got %d", len(items))
	}
}

// ---------------------------------------------------------------------------
// TransferRegistry
// ---------------------------------------------------------------------------

func TestTransferRegistry_Success_UpdatesItemsRepoID(t *testing.T) {
	defer setupTestDB(t)()
	database.DB.Create(&models.Repository{ID: "repo-rt-src", Name: "rt-src", OwnerID: "u1"})
	database.DB.Create(&models.Repository{ID: "repo-rt-tgt", Name: "rt-tgt", OwnerID: "u1"})
	database.DB.Create(&models.CapabilityRegistry{
		ID: "reg-rt", Name: "rt-reg", SourceType: "internal", Visibility: "repo", RepoID: "repo-rt-src", OwnerID: "u1",
	})
	// Caller must be member of target repo
	database.DB.Create(&models.RepoMember{ID: "mem-rt", RepoID: "repo-rt-tgt", UserID: "u1", Role: "member"})
	// Create several items under this registry
	database.DB.Create(&models.CapabilityItem{
		ID: "item-rt1", RegistryID: "reg-rt", RepoID: "repo-rt-src", Slug: "rt-skill1", ItemType: "skill",
		Name: "RT Skill 1", Status: "active", CreatedBy: "u1", Metadata: datatypes.JSON([]byte("{}")),
	})
	database.DB.Create(&models.CapabilityItem{
		ID: "item-rt2", RegistryID: "reg-rt", RepoID: "repo-rt-src", Slug: "rt-cmd1", ItemType: "command",
		Name: "RT Cmd 1", Status: "active", CreatedBy: "u1", Metadata: datatypes.JSON([]byte("{}")),
	})
	database.DB.Create(&models.CapabilityItem{
		ID: "item-rt3", RegistryID: "reg-rt", RepoID: "repo-rt-src", Slug: "rt-skill2", ItemType: "skill",
		Name: "RT Skill 2", Status: "active", CreatedBy: "u1", Metadata: datatypes.JSON([]byte("{}")),
	})

	w := putJSON(newRegistryRouter("u1"), "/api/registries/reg-rt/transfer", map[string]interface{}{
		"targetRepoId": "repo-rt-tgt",
	})
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	// Verify registry repo_id updated
	var reg models.CapabilityRegistry
	database.DB.First(&reg, "id = ?", "reg-rt")
	if reg.RepoID != "repo-rt-tgt" {
		t.Fatalf("DB: expected registry repoId=repo-rt-tgt, got %s", reg.RepoID)
	}

	// Key assertion for Bug 2 fix: ALL items under this registry must have repo_id updated
	var items []models.CapabilityItem
	database.DB.Where("registry_id = ?", "reg-rt").Find(&items)
	if len(items) != 3 {
		t.Fatalf("expected 3 items, got %d", len(items))
	}
	for _, item := range items {
		if item.RepoID != "repo-rt-tgt" {
			t.Fatalf("DB: item %s expected repoId=repo-rt-tgt, got %s", item.ID, item.RepoID)
		}
	}
}

func TestTransferRegistry_NotFound(t *testing.T) {
	defer setupTestDB(t)()
	database.DB.Create(&models.RepoMember{ID: "mem-rnf", RepoID: "repo-x", UserID: "u1", Role: "member"})

	w := putJSON(newRegistryRouter("u1"), "/api/registries/no-such-reg/transfer", map[string]interface{}{
		"targetRepoId": "repo-x",
	})
	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", w.Code)
	}
}

func TestTransferRegistry_Unauthenticated(t *testing.T) {
	defer setupTestDB(t)()
	database.DB.Create(&models.CapabilityRegistry{
		ID: "reg-rt-noauth", Name: "rt-noauth", SourceType: "internal", Visibility: "repo", RepoID: "repo-1", OwnerID: "u1",
	})

	w := putJSON(newRegistryRouter(""), "/api/registries/reg-rt-noauth/transfer", map[string]interface{}{
		"targetRepoId": "repo-2",
	})
	if w.Code != http.StatusForbidden {
		t.Fatalf("expected 403, got %d", w.Code)
	}
}

func TestTransferRegistry_ForbiddenNotOwnerNorAdmin(t *testing.T) {
	defer setupTestDB(t)()
	database.DB.Create(&models.Repository{ID: "repo-rtf", Name: "rtf-repo", OwnerID: "u1"})
	database.DB.Create(&models.CapabilityRegistry{
		ID: "reg-rtf", Name: "rtf-reg", SourceType: "internal", Visibility: "repo", RepoID: "repo-rtf", OwnerID: "u1",
	})
	// u-other is only a member (not admin/owner) of the source repo, and NOT the registry owner
	database.DB.Create(&models.RepoMember{ID: "mem-rtf", RepoID: "repo-rtf", UserID: "u-other", Role: "member"})

	w := putJSON(newRegistryRouter("u-other"), "/api/registries/reg-rtf/transfer", map[string]interface{}{
		"targetRepoId": "repo-2",
	})
	if w.Code != http.StatusForbidden {
		t.Fatalf("expected 403, got %d", w.Code)
	}
}

func TestTransferRegistry_SameRepo(t *testing.T) {
	defer setupTestDB(t)()
	database.DB.Create(&models.Repository{ID: "repo-rts", Name: "rts-repo", OwnerID: "u1"})
	database.DB.Create(&models.CapabilityRegistry{
		ID: "reg-rts", Name: "rts-reg", SourceType: "internal", Visibility: "repo", RepoID: "repo-rts", OwnerID: "u1",
	})
	database.DB.Create(&models.RepoMember{ID: "mem-rts", RepoID: "repo-rts", UserID: "u1", Role: "owner"})

	w := putJSON(newRegistryRouter("u1"), "/api/registries/reg-rts/transfer", map[string]interface{}{
		"targetRepoId": "repo-rts",
	})
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", w.Code)
	}
}

func TestTransferRegistry_SyncingNotAllowed(t *testing.T) {
	defer setupTestDB(t)()
	database.DB.Create(&models.Repository{ID: "repo-rtsn", Name: "rtsn-repo", OwnerID: "u1"})
	database.DB.Create(&models.Repository{ID: "repo-rtsn-tgt", Name: "rtsn-tgt", OwnerID: "u1"})
	database.DB.Create(&models.CapabilityRegistry{
		ID: "reg-rtsn", Name: "rtsn-reg", SourceType: "external", Visibility: "repo",
		RepoID: "repo-rtsn", OwnerID: "u1", SyncStatus: "syncing",
	})
	database.DB.Create(&models.RepoMember{ID: "mem-rtsn", RepoID: "repo-rtsn-tgt", UserID: "u1", Role: "member"})

	w := putJSON(newRegistryRouter("u1"), "/api/registries/reg-rtsn/transfer", map[string]interface{}{
		"targetRepoId": "repo-rtsn-tgt",
	})
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", w.Code)
	}
}

func TestTransferRegistry_NotTargetRepoMember(t *testing.T) {
	defer setupTestDB(t)()
	database.DB.Create(&models.Repository{ID: "repo-rtnm", Name: "rtnm-repo", OwnerID: "u1"})
	database.DB.Create(&models.Repository{ID: "repo-rtnm-tgt", Name: "rtnm-tgt", OwnerID: "u2"})
	database.DB.Create(&models.CapabilityRegistry{
		ID: "reg-rtnm", Name: "rtnm-reg", SourceType: "internal", Visibility: "repo", RepoID: "repo-rtnm", OwnerID: "u1",
	})
	// u1 is NOT a member of repo-rtnm-tgt

	w := putJSON(newRegistryRouter("u1"), "/api/registries/reg-rtnm/transfer", map[string]interface{}{
		"targetRepoId": "repo-rtnm-tgt",
	})
	if w.Code != http.StatusForbidden {
		t.Fatalf("expected 403, got %d", w.Code)
	}
}

func TestTransferRegistry_ItemsInOtherRegistryUnchanged(t *testing.T) {
	defer setupTestDB(t)()
	database.DB.Create(&models.Repository{ID: "repo-rio-src", Name: "rio-src", OwnerID: "u1"})
	database.DB.Create(&models.Repository{ID: "repo-rio-tgt", Name: "rio-tgt", OwnerID: "u1"})
	database.DB.Create(&models.CapabilityRegistry{
		ID: "reg-rio-move", Name: "rio-move", SourceType: "internal", Visibility: "repo", RepoID: "repo-rio-src", OwnerID: "u1",
	})
	database.DB.Create(&models.CapabilityRegistry{
		ID: "reg-rio-stay", Name: "rio-stay", SourceType: "internal", Visibility: "repo", RepoID: "repo-rio-src", OwnerID: "u1",
	})
	database.DB.Create(&models.RepoMember{ID: "mem-rio", RepoID: "repo-rio-tgt", UserID: "u1", Role: "member"})

	// Item in the registry being transferred
	database.DB.Create(&models.CapabilityItem{
		ID: "item-rio-moved", RegistryID: "reg-rio-move", RepoID: "repo-rio-src", Slug: "rio-moved", ItemType: "skill",
		Name: "Moved", Status: "active", CreatedBy: "u1", Metadata: datatypes.JSON([]byte("{}")),
	})
	// Item in a DIFFERENT registry in the same source repo — should NOT be affected
	database.DB.Create(&models.CapabilityItem{
		ID: "item-rio-stay", RegistryID: "reg-rio-stay", RepoID: "repo-rio-src", Slug: "rio-stay", ItemType: "skill",
		Name: "Stay", Status: "active", CreatedBy: "u1", Metadata: datatypes.JSON([]byte("{}")),
	})

	w := putJSON(newRegistryRouter("u1"), "/api/registries/reg-rio-move/transfer", map[string]interface{}{
		"targetRepoId": "repo-rio-tgt",
	})
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	// The moved item should have new repo_id
	var movedItem models.CapabilityItem
	database.DB.First(&movedItem, "id = ?", "item-rio-moved")
	if movedItem.RepoID != "repo-rio-tgt" {
		t.Fatalf("moved item: expected repoId=repo-rio-tgt, got %s", movedItem.RepoID)
	}

	// The other item should keep its original repo_id
	var stayItem models.CapabilityItem
	database.DB.First(&stayItem, "id = ?", "item-rio-stay")
	if stayItem.RepoID != "repo-rio-src" {
		t.Fatalf("unrelated item: expected repoId=repo-rio-src (unchanged), got %s", stayItem.RepoID)
	}
}