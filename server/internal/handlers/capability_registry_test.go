package handlers

import (
	"encoding/json"
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
	return r
}

// ---------------------------------------------------------------------------
// ListRegistries
// ---------------------------------------------------------------------------

func TestListRegistries_Anonymous_OnlyPublic(t *testing.T) {
	defer setupTestDB(t)()
	database.DB.Create(&models.CapabilityRegistry{
		ID: "pub-r1", Name: "public-reg", SourceType: "internal", Visibility: "public", OrgID: "public", OwnerID: "system",
	})
	database.DB.Create(&models.CapabilityRegistry{
		ID: "org-r1", Name: "org-reg", SourceType: "internal", Visibility: "org", OrgID: "org-1", OwnerID: "u1",
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

func TestListRegistries_FilterByOrgId(t *testing.T) {
	defer setupTestDB(t)()
	database.DB.Create(&models.CapabilityRegistry{
		ID: "org-r2", Name: "org-reg2", SourceType: "internal", Visibility: "org", OrgID: "org-filter", OwnerID: "u1",
	})
	database.DB.Create(&models.CapabilityRegistry{
		ID: "org-r3", Name: "org-reg3", SourceType: "internal", Visibility: "org", OrgID: "org-other", OwnerID: "u1",
	})

	w := get(newRegistryRouter(""), "/api/registries?orgId=org-filter")
	var body map[string]interface{}
	json.NewDecoder(w.Body).Decode(&body)
	regs := body["registries"].([]interface{})
	if len(regs) != 1 {
		t.Fatalf("expected 1 registry for org-filter, got %d", len(regs))
	}
}

func TestListRegistries_AuthUser_IncludesOrgAndPersonal(t *testing.T) {
	defer setupTestDB(t)()
	database.DB.Create(&models.CapabilityRegistry{
		ID: "pub-r2", Name: "public-r2", SourceType: "internal", Visibility: "public", OrgID: "public", OwnerID: "system",
	})
	database.DB.Create(&models.CapabilityRegistry{
		ID: "org-r4", Name: "org-r4", SourceType: "internal", Visibility: "org", OrgID: "org-2", OwnerID: "u2",
	})
	database.DB.Create(&models.CapabilityRegistry{
		ID: "priv-r1", Name: "private-r1", SourceType: "internal", Visibility: "private", OrgID: "", OwnerID: "auth-user",
	})
	database.DB.Create(&models.OrgMember{
		ID: "mem-au", OrgID: "org-2", UserID: "auth-user", Role: "member",
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

	w := postJSON(newRegistryRouter("u1"), "/api/registries", map[string]interface{}{
		"name": "No Owner",
	})
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
		ID: "reg-get1", Name: "get-reg", SourceType: "internal", Visibility: "public", OrgID: "org-1", OwnerID: "u1",
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
		ID: "reg-upd1", Name: "old-name", SourceType: "internal", Visibility: "org", OrgID: "org-1", OwnerID: "u1",
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
		ID: "reg-del1", Name: "del-reg", SourceType: "internal", Visibility: "public", OrgID: "org-1", OwnerID: "u1",
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

	w := postJSON(newRegistryRouter(""), "/api/registries/ensure-personal", map[string]interface{}{
		"ownerId": "user-new", "username": "alice",
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
		Visibility: "public", OrgID: "", OwnerID: "user-bob",
	})

	w := postJSON(newRegistryRouter(""), "/api/registries/ensure-personal", map[string]interface{}{
		"ownerId": "user-bob", "username": "bob",
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

	w := postJSON(newRegistryRouter(""), "/api/registries/ensure-personal", map[string]interface{}{
		"ownerId": "user-noname",
	})
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

	w := postJSON(newRegistryRouter(""), "/api/registries/ensure-personal", map[string]interface{}{
		"username": "nobody",
	})
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", w.Code)
	}
}

// ---------------------------------------------------------------------------
// ListMyRegistries
// ---------------------------------------------------------------------------

func TestListMyRegistries_Success(t *testing.T) {
	defer setupTestDB(t)()
	database.DB.Create(&models.CapabilityRegistry{
		ID: "my-r1", Name: "my-reg1", SourceType: "internal", Visibility: "public", OrgID: "", OwnerID: "owner-a",
	})
	database.DB.Create(&models.CapabilityRegistry{
		ID: "my-r2", Name: "my-reg2", SourceType: "internal", Visibility: "public", OrgID: "", OwnerID: "owner-a",
	})
	database.DB.Create(&models.CapabilityRegistry{
		ID: "other-r", Name: "other-reg", SourceType: "internal", Visibility: "public", OrgID: "", OwnerID: "owner-b",
	})

	w := get(newRegistryRouter(""), "/api/registries/my?ownerId=owner-a")
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
	w := get(newRegistryRouter(""), "/api/registries/my")
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", w.Code)
	}
}

// ---------------------------------------------------------------------------
// ListMyItems
// ---------------------------------------------------------------------------

func TestListMyItems_Success(t *testing.T) {
	defer setupTestDB(t)()
	database.DB.Create(&models.CapabilityRegistry{
		ID: "my-items-reg", Name: "my-items-r", SourceType: "internal", Visibility: "public", OrgID: "", OwnerID: "item-owner",
	})
	database.DB.Create(&models.CapabilityItem{
		ID: "my-item-1", RegistryID: "my-items-reg", Slug: "my-skill", ItemType: "skill",
		Name: "My Skill", Status: "active", CreatedBy: "item-owner", Metadata: datatypes.JSON([]byte("{}")),
	})
	database.DB.Create(&models.CapabilityItem{
		ID: "my-item-2", RegistryID: "my-items-reg", Slug: "my-cmd", ItemType: "command",
		Name: "My Cmd", Status: "active", CreatedBy: "item-owner", Metadata: datatypes.JSON([]byte("{}")),
	})

	w := get(newRegistryRouter(""), "/api/items/my?ownerId=item-owner")
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	var body map[string]interface{}
	json.NewDecoder(w.Body).Decode(&body)
	items := body["items"].([]interface{})
	if len(items) != 2 {
		t.Fatalf("expected 2 items, got %d", len(items))
	}
}

func TestListMyItems_FilterByType(t *testing.T) {
	defer setupTestDB(t)()
	database.DB.Create(&models.CapabilityRegistry{
		ID: "my-items-reg2", Name: "my-items-r2", SourceType: "internal", Visibility: "public", OrgID: "", OwnerID: "item-owner2",
	})
	database.DB.Create(&models.CapabilityItem{
		ID: "my-item-3", RegistryID: "my-items-reg2", Slug: "my-skill2", ItemType: "skill",
		Name: "My Skill2", Status: "active", CreatedBy: "item-owner2", Metadata: datatypes.JSON([]byte("{}")),
	})
	database.DB.Create(&models.CapabilityItem{
		ID: "my-item-4", RegistryID: "my-items-reg2", Slug: "my-cmd2", ItemType: "command",
		Name: "My Cmd2", Status: "active", CreatedBy: "item-owner2", Metadata: datatypes.JSON([]byte("{}")),
	})

	w := get(newRegistryRouter(""), "/api/items/my?ownerId=item-owner2&type=skill")
	var body map[string]interface{}
	json.NewDecoder(w.Body).Decode(&body)
	items := body["items"].([]interface{})
	if len(items) != 1 {
		t.Fatalf("expected 1 skill item, got %d", len(items))
	}
}

func TestListMyItems_MissingOwnerID(t *testing.T) {
	defer setupTestDB(t)()
	w := get(newRegistryRouter(""), "/api/items/my")
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", w.Code)
	}
}
