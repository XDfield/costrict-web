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
)

func newItemRouter(userID string) *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	injectUser := func(c *gin.Context) {
		if userID != "" {
			c.Set(middleware.UserIDKey, userID)
		}
		c.Next()
	}
	r.GET("/api/registries/:id/items", injectUser, ListItems)
	r.POST("/api/registries/:id/items", injectUser, CreateItem)
	r.GET("/api/items/:id", injectUser, GetItem)
	r.PUT("/api/items/:id", injectUser, UpdateItem)
	r.DELETE("/api/items/:id", injectUser, DeleteItem)
	r.GET("/api/items/:id/versions", injectUser, ListItemVersions)
	r.GET("/api/items/:id/versions/:version", injectUser, GetItemVersion)
	r.GET("/api/items", injectUser, ListAllItems)
	r.POST("/api/items", injectUser, CreateItemDirect)
	r.GET("/api/registries/public", injectUser, GetPublicRegistry)
	return r
}

func postJSON(r *gin.Engine, path string, body interface{}) *httptest.ResponseRecorder {
	b, _ := json.Marshal(body)
	w := httptest.NewRecorder()
	req, _ := http.NewRequest(http.MethodPost, path, bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(w, req)
	return w
}

func putJSON(r *gin.Engine, path string, body interface{}) *httptest.ResponseRecorder {
	b, _ := json.Marshal(body)
	w := httptest.NewRecorder()
	req, _ := http.NewRequest(http.MethodPut, path, bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(w, req)
	return w
}

func deleteReq(r *gin.Engine, path string) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	req, _ := http.NewRequest(http.MethodDelete, path, nil)
	r.ServeHTTP(w, req)
	return w
}

// ---------------------------------------------------------------------------
// slugify
// ---------------------------------------------------------------------------

func TestSlugify(t *testing.T) {
	cases := []struct{ input, want string }{
		{"Hello World", "hello-world"},
		{"My Skill", "my-skill"},
		{"  leading spaces", "leading-spaces"},
		{"trailing spaces  ", "trailing-spaces"},
		{"multiple   spaces", "multiple-spaces"},
		{"CamelCase", "camelcase"},
		{"already-slug", "already-slug"},
		{"123 numbers", "123-numbers"},
		{"special!@#chars", "special-chars"},
		{"", ""},
	}
	for _, tc := range cases {
		got := slugify(tc.input)
		if got != tc.want {
			t.Errorf("slugify(%q) = %q, want %q", tc.input, got, tc.want)
		}
	}
}

// ---------------------------------------------------------------------------
// ListItems
// ---------------------------------------------------------------------------

func TestListItems_Empty(t *testing.T) {
	defer setupTestDB(t)()
	database.DB.Create(&models.CapabilityRegistry{
		ID: "reg-list", Name: "test-reg", SourceType: "internal", OrgID: "org-1", OwnerID: "u1",
	})

	w := get(newItemRouter(""), "/api/registries/reg-list/items")
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	var body map[string]interface{}
	json.NewDecoder(w.Body).Decode(&body)
	items := body["items"].([]interface{})
	if len(items) != 0 {
		t.Fatalf("expected 0 items, got %d", len(items))
	}
}

func TestListItems_WithItems(t *testing.T) {
	defer setupTestDB(t)()
	database.DB.Create(&models.CapabilityRegistry{
		ID: "reg-li2", Name: "test-reg2", SourceType: "internal", OrgID: "org-1", OwnerID: "u1",
	})
	database.DB.Create(&models.CapabilityItem{
		ID: "item-a", RegistryID: "reg-li2", Slug: "skill-a", ItemType: "skill",
		Name: "Skill A", Status: "active", CreatedBy: "u1", Metadata: datatypes.JSON([]byte("{}")),
	})
	database.DB.Create(&models.CapabilityItem{
		ID: "item-b", RegistryID: "reg-li2", Slug: "cmd-b", ItemType: "command",
		Name: "Cmd B", Status: "active", CreatedBy: "u1", Metadata: datatypes.JSON([]byte("{}")),
	})

	w := get(newItemRouter(""), "/api/registries/reg-li2/items")
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

func TestListItems_FilterByType(t *testing.T) {
	defer setupTestDB(t)()
	database.DB.Create(&models.CapabilityRegistry{
		ID: "reg-li3", Name: "test-reg3", SourceType: "internal", OrgID: "org-1", OwnerID: "u1",
	})
	database.DB.Create(&models.CapabilityItem{
		ID: "item-c", RegistryID: "reg-li3", Slug: "skill-c", ItemType: "skill",
		Name: "Skill C", Status: "active", CreatedBy: "u1", Metadata: datatypes.JSON([]byte("{}")),
	})
	database.DB.Create(&models.CapabilityItem{
		ID: "item-d", RegistryID: "reg-li3", Slug: "cmd-d", ItemType: "command",
		Name: "Cmd D", Status: "active", CreatedBy: "u1", Metadata: datatypes.JSON([]byte("{}")),
	})

	w := get(newItemRouter(""), "/api/registries/reg-li3/items?type=skill")
	var body map[string]interface{}
	json.NewDecoder(w.Body).Decode(&body)
	items := body["items"].([]interface{})
	if len(items) != 1 {
		t.Fatalf("expected 1 skill item, got %d", len(items))
	}
}

func TestListItems_FilterByStatus(t *testing.T) {
	defer setupTestDB(t)()
	database.DB.Create(&models.CapabilityRegistry{
		ID: "reg-li4", Name: "test-reg4", SourceType: "internal", OrgID: "org-1", OwnerID: "u1",
	})
	database.DB.Create(&models.CapabilityItem{
		ID: "item-e", RegistryID: "reg-li4", Slug: "skill-e", ItemType: "skill",
		Name: "Skill E", Status: "active", CreatedBy: "u1", Metadata: datatypes.JSON([]byte("{}")),
	})
	database.DB.Create(&models.CapabilityItem{
		ID: "item-f", RegistryID: "reg-li4", Slug: "skill-f", ItemType: "skill",
		Name: "Skill F", Status: "inactive", CreatedBy: "u1", Metadata: datatypes.JSON([]byte("{}")),
	})

	w := get(newItemRouter(""), "/api/registries/reg-li4/items?status=inactive")
	var body map[string]interface{}
	json.NewDecoder(w.Body).Decode(&body)
	items := body["items"].([]interface{})
	if len(items) != 1 {
		t.Fatalf("expected 1 inactive item, got %d", len(items))
	}
}

// ---------------------------------------------------------------------------
// CreateItem
// ---------------------------------------------------------------------------

func TestCreateItem_Success(t *testing.T) {
	defer setupTestDB(t)()
	database.DB.Create(&models.CapabilityRegistry{
		ID: "reg-ci1", Name: "ci-reg", SourceType: "internal", OrgID: "org-1", OwnerID: "u1",
	})

	w := postJSON(newItemRouter("u1"), "/api/registries/reg-ci1/items", map[string]interface{}{
		"slug": "new-skill", "itemType": "skill", "name": "New Skill", "createdBy": "u1",
	})
	if w.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", w.Code, w.Body.String())
	}
	var item map[string]interface{}
	json.NewDecoder(w.Body).Decode(&item)
	if item["slug"] != "new-skill" {
		t.Fatalf("unexpected slug: %v", item["slug"])
	}
}

func TestCreateItem_MissingRequired(t *testing.T) {
	defer setupTestDB(t)()

	w := postJSON(newItemRouter("u1"), "/api/registries/reg-ci2/items", map[string]interface{}{
		"name": "No Slug",
	})
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", w.Code)
	}
}

// ---------------------------------------------------------------------------
// GetItem
// ---------------------------------------------------------------------------

func TestGetItem_Found(t *testing.T) {
	defer setupTestDB(t)()
	database.DB.Create(&models.CapabilityRegistry{
		ID: "reg-gi1", Name: "gi-reg", SourceType: "internal", OrgID: "org-1", OwnerID: "u1",
	})
	database.DB.Create(&models.CapabilityItem{
		ID: "item-gi1", RegistryID: "reg-gi1", Slug: "get-me", ItemType: "skill",
		Name: "Get Me", Status: "active", CreatedBy: "u1", Metadata: datatypes.JSON([]byte("{}")),
	})

	w := get(newItemRouter(""), "/api/items/item-gi1")
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	var item map[string]interface{}
	json.NewDecoder(w.Body).Decode(&item)
	if item["id"] != "item-gi1" {
		t.Fatalf("unexpected id: %v", item["id"])
	}
}

func TestGetItem_NotFound(t *testing.T) {
	defer setupTestDB(t)()
	w := get(newItemRouter(""), "/api/items/no-such-item")
	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", w.Code)
	}
}

// ---------------------------------------------------------------------------
// UpdateItem
// ---------------------------------------------------------------------------

func TestUpdateItem_Success(t *testing.T) {
	defer setupTestDB(t)()
	database.DB.Create(&models.CapabilityRegistry{
		ID: "reg-ui1", Name: "ui-reg", SourceType: "internal", OrgID: "org-1", OwnerID: "u1",
	})
	database.DB.Create(&models.CapabilityItem{
		ID: "item-ui1", RegistryID: "reg-ui1", Slug: "update-me", ItemType: "skill",
		Name: "Old Name", Status: "active", CreatedBy: "u1", Metadata: datatypes.JSON([]byte("{}")),
	})

	w := putJSON(newItemRouter("u1"), "/api/items/item-ui1", map[string]interface{}{
		"name": "New Name", "updatedBy": "u1",
	})
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var item map[string]interface{}
	json.NewDecoder(w.Body).Decode(&item)
	if item["name"] != "New Name" {
		t.Fatalf("expected name=New Name, got %v", item["name"])
	}
}

func TestUpdateItem_NotFound(t *testing.T) {
	defer setupTestDB(t)()
	w := putJSON(newItemRouter("u1"), "/api/items/no-such", map[string]interface{}{
		"name": "X",
	})
	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", w.Code)
	}
}

func TestUpdateItem_ContentCreatesVersion(t *testing.T) {
	defer setupTestDB(t)()
	database.DB.Create(&models.CapabilityRegistry{
		ID: "reg-ui2", Name: "ui-reg2", SourceType: "internal", OrgID: "org-1", OwnerID: "u1",
	})
	database.DB.Create(&models.CapabilityItem{
		ID: "item-ui2", RegistryID: "reg-ui2", Slug: "versioned", ItemType: "skill",
		Name: "Versioned", Status: "active", CreatedBy: "u1", Metadata: datatypes.JSON([]byte("{}")),
	})
	database.DB.Create(&models.CapabilityVersion{
		ID: "ver-1", ItemID: "item-ui2", Version: 1, Content: "v1", CreatedBy: "u1",
		Metadata: datatypes.JSON([]byte("{}")),
	})

	w := putJSON(newItemRouter("u1"), "/api/items/item-ui2", map[string]interface{}{
		"content": "updated content", "commitMsg": "update v2", "updatedBy": "u1",
	})
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}

	var versions []models.CapabilityVersion
	database.DB.Where("item_id = ?", "item-ui2").Find(&versions)
	if len(versions) != 2 {
		t.Fatalf("expected 2 versions after content update, got %d", len(versions))
	}
}

// ---------------------------------------------------------------------------
// DeleteItem
// ---------------------------------------------------------------------------

func TestDeleteItem_Success(t *testing.T) {
	defer setupTestDB(t)()
	database.DB.Create(&models.CapabilityRegistry{
		ID: "reg-di1", Name: "di-reg", SourceType: "internal", OrgID: "org-1", OwnerID: "u1",
	})
	database.DB.Create(&models.CapabilityItem{
		ID: "item-di1", RegistryID: "reg-di1", Slug: "delete-me", ItemType: "skill",
		Name: "Delete Me", Status: "active", CreatedBy: "u1", Metadata: datatypes.JSON([]byte("{}")),
	})

	w := deleteReq(newItemRouter("u1"), "/api/items/item-di1")
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}

	var count int64
	database.DB.Model(&models.CapabilityItem{}).Where("id = ?", "item-di1").Count(&count)
	if count != 0 {
		t.Fatal("item should have been deleted")
	}
}

// ---------------------------------------------------------------------------
// ListItemVersions
// ---------------------------------------------------------------------------

func TestListItemVersions(t *testing.T) {
	defer setupTestDB(t)()
	database.DB.Create(&models.CapabilityRegistry{
		ID: "reg-lv1", Name: "lv-reg", SourceType: "internal", OrgID: "org-1", OwnerID: "u1",
	})
	database.DB.Create(&models.CapabilityItem{
		ID: "item-lv1", RegistryID: "reg-lv1", Slug: "versioned", ItemType: "skill",
		Name: "Versioned", Status: "active", CreatedBy: "u1", Metadata: datatypes.JSON([]byte("{}")),
	})
	database.DB.Create(&models.CapabilityVersion{
		ID: "ver-lv1", ItemID: "item-lv1", Version: 1, Content: "v1", CreatedBy: "u1",
		Metadata: datatypes.JSON([]byte("{}")),
	})
	database.DB.Create(&models.CapabilityVersion{
		ID: "ver-lv2", ItemID: "item-lv1", Version: 2, Content: "v2", CreatedBy: "u1",
		Metadata: datatypes.JSON([]byte("{}")),
	})

	w := get(newItemRouter(""), "/api/items/item-lv1/versions")
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	var body map[string]interface{}
	json.NewDecoder(w.Body).Decode(&body)
	versions := body["versions"].([]interface{})
	if len(versions) != 2 {
		t.Fatalf("expected 2 versions, got %d", len(versions))
	}
}

// ---------------------------------------------------------------------------
// GetItemVersion
// ---------------------------------------------------------------------------

func TestGetItemVersion_Found(t *testing.T) {
	defer setupTestDB(t)()
	database.DB.Create(&models.CapabilityRegistry{
		ID: "reg-gv1", Name: "gv-reg", SourceType: "internal", OrgID: "org-1", OwnerID: "u1",
	})
	database.DB.Create(&models.CapabilityItem{
		ID: "item-gv1", RegistryID: "reg-gv1", Slug: "gv-item", ItemType: "skill",
		Name: "GV Item", Status: "active", CreatedBy: "u1", Metadata: datatypes.JSON([]byte("{}")),
	})
	database.DB.Create(&models.CapabilityVersion{
		ID: "ver-gv1", ItemID: "item-gv1", Version: 1, Content: "v1 content", CreatedBy: "u1",
		Metadata: datatypes.JSON([]byte("{}")),
	})

	w := get(newItemRouter(""), "/api/items/item-gv1/versions/1")
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	var ver map[string]interface{}
	json.NewDecoder(w.Body).Decode(&ver)
	if ver["content"] != "v1 content" {
		t.Fatalf("unexpected content: %v", ver["content"])
	}
}

func TestGetItemVersion_NotFound(t *testing.T) {
	defer setupTestDB(t)()
	w := get(newItemRouter(""), "/api/items/no-item/versions/99")
	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", w.Code)
	}
}

func TestGetItemVersion_InvalidVersion(t *testing.T) {
	defer setupTestDB(t)()
	w := get(newItemRouter(""), "/api/items/some-item/versions/abc")
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", w.Code)
	}
}

// ---------------------------------------------------------------------------
// buildVisibleRegistryIDs
// ---------------------------------------------------------------------------

func TestBuildVisibleRegistryIDs_Anonymous(t *testing.T) {
	defer setupTestDB(t)()
	database.DB.Create(&models.CapabilityRegistry{
		ID: "pub-reg", Name: "public-r", SourceType: "internal", Visibility: "public", OrgID: "public", OwnerID: "system",
	})
	database.DB.Create(&models.CapabilityRegistry{
		ID: "priv-reg", Name: "private-r", SourceType: "internal", Visibility: "org", OrgID: "org-1", OwnerID: "u1",
	})

	ids := buildVisibleRegistryIDs(database.DB, "")
	if len(ids) != 1 || ids[0] != "pub-reg" {
		t.Fatalf("expected only public registry, got %v", ids)
	}
}

func TestBuildVisibleRegistryIDs_MemberUser(t *testing.T) {
	defer setupTestDB(t)()
	database.DB.Create(&models.CapabilityRegistry{
		ID: "pub-reg2", Name: "public-r2", SourceType: "internal", Visibility: "public", OrgID: "public", OwnerID: "system",
	})
	database.DB.Create(&models.CapabilityRegistry{
		ID: "org-reg2", Name: "org-r2", SourceType: "internal", Visibility: "org", OrgID: "org-x", OwnerID: "u2",
	})
	database.DB.Create(&models.OrgMember{
		ID: "mem-x", OrgID: "org-x", UserID: "u-member", Role: "member",
	})

	ids := buildVisibleRegistryIDs(database.DB, "u-member")
	found := map[string]bool{}
	for _, id := range ids {
		found[id] = true
	}
	if !found["pub-reg2"] || !found["org-reg2"] {
		t.Fatalf("expected both public and org registry, got %v", ids)
	}
}

func TestBuildVisibleRegistryIDs_PersonalOwner(t *testing.T) {
	defer setupTestDB(t)()
	database.DB.Create(&models.CapabilityRegistry{
		ID: "personal-reg", Name: "my-skills", SourceType: "internal", Visibility: "private", OrgID: "", OwnerID: "u-owner",
	})

	ids := buildVisibleRegistryIDs(database.DB, "u-owner")
	found := false
	for _, id := range ids {
		if id == "personal-reg" {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected personal registry to be visible to owner, got %v", ids)
	}
}

// ---------------------------------------------------------------------------
// GetPublicRegistry
// ---------------------------------------------------------------------------

func TestGetPublicRegistry_Found(t *testing.T) {
	defer setupTestDB(t)()
	database.DB.Create(&models.CapabilityRegistry{
		ID: PublicRegistryID, Name: "public", SourceType: "internal", Visibility: "public", OrgID: "public", OwnerID: "system",
	})

	w := get(newItemRouter(""), "/api/registries/public")
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	var reg map[string]interface{}
	json.NewDecoder(w.Body).Decode(&reg)
	if reg["id"] != PublicRegistryID {
		t.Fatalf("unexpected id: %v", reg["id"])
	}
}

func TestGetPublicRegistry_NotFound(t *testing.T) {
	defer setupTestDB(t)()
	w := get(newItemRouter(""), "/api/registries/public")
	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", w.Code)
	}
}

// ---------------------------------------------------------------------------
// CreateItemDirect
// ---------------------------------------------------------------------------

func TestCreateItemDirect_Success(t *testing.T) {
	defer setupTestDB(t)()
	database.DB.Create(&models.CapabilityRegistry{
		ID: PublicRegistryID, Name: "public", SourceType: "internal", Visibility: "public", OrgID: "public", OwnerID: "system",
	})

	w := postJSON(newItemRouter("u1"), "/api/items", map[string]interface{}{
		"itemType": "skill", "name": "Direct Skill", "content": "# Direct",
	})
	if w.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", w.Code, w.Body.String())
	}
	var item map[string]interface{}
	json.NewDecoder(w.Body).Decode(&item)
	if item["name"] != "Direct Skill" {
		t.Fatalf("unexpected name: %v", item["name"])
	}
	if item["registryId"] != PublicRegistryID {
		t.Fatalf("expected public registry, got %v", item["registryId"])
	}
}

func TestCreateItemDirect_AutoSlugify(t *testing.T) {
	defer setupTestDB(t)()
	database.DB.Create(&models.CapabilityRegistry{
		ID: PublicRegistryID, Name: "public", SourceType: "internal", Visibility: "public", OrgID: "public", OwnerID: "system",
	})

	w := postJSON(newItemRouter("u1"), "/api/items", map[string]interface{}{
		"itemType": "skill", "name": "My Auto Slug Skill",
	})
	if w.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", w.Code, w.Body.String())
	}
	var item map[string]interface{}
	json.NewDecoder(w.Body).Decode(&item)
	if item["slug"] != "my-auto-slug-skill" {
		t.Fatalf("expected slug=my-auto-slug-skill, got %v", item["slug"])
	}
}

func TestCreateItemDirect_DefaultVersion(t *testing.T) {
	defer setupTestDB(t)()
	database.DB.Create(&models.CapabilityRegistry{
		ID: PublicRegistryID, Name: "public", SourceType: "internal", Visibility: "public", OrgID: "public", OwnerID: "system",
	})

	w := postJSON(newItemRouter("u1"), "/api/items", map[string]interface{}{
		"itemType": "command", "name": "No Version",
	})
	if w.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d", w.Code)
	}
	var item map[string]interface{}
	json.NewDecoder(w.Body).Decode(&item)
	if item["version"] != "1.0.0" {
		t.Fatalf("expected version=1.0.0, got %v", item["version"])
	}
}

func TestCreateItemDirect_MissingRequired(t *testing.T) {
	defer setupTestDB(t)()

	w := postJSON(newItemRouter("u1"), "/api/items", map[string]interface{}{
		"name": "Missing Type",
	})
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", w.Code)
	}
}

func TestCreateItemDirect_AnonymousCreatedBy(t *testing.T) {
	defer setupTestDB(t)()
	database.DB.Create(&models.CapabilityRegistry{
		ID: PublicRegistryID, Name: "public", SourceType: "internal", Visibility: "public", OrgID: "public", OwnerID: "system",
	})

	w := postJSON(newItemRouter(""), "/api/items", map[string]interface{}{
		"itemType": "skill", "name": "Anon Skill",
	})
	if w.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", w.Code, w.Body.String())
	}
	var item map[string]interface{}
	json.NewDecoder(w.Body).Decode(&item)
	if item["createdBy"] != "anonymous" {
		t.Fatalf("expected createdBy=anonymous, got %v", item["createdBy"])
	}
}

// ensure fmt is used
var _ = fmt.Sprintf
