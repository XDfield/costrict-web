package handlers

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/costrict/costrict-web/server/internal/config"
	"github.com/costrict/costrict-web/server/internal/database"
	"github.com/costrict/costrict-web/server/internal/middleware"
	"github.com/costrict/costrict-web/server/internal/models"
	"github.com/costrict/costrict-web/server/internal/services"
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

	// Create ItemHandler for CreateItemDirect
	db := database.GetDB()
	embeddingSvc := services.NewEmbeddingService(&config.EmbeddingConfig{Provider: "mock", Dimensions: 1024})
	indexerSvc := services.NewIndexerService(db, embeddingSvc)
	itemHandler := NewItemHandler(db, indexerSvc, &services.ParserService{})

	r.GET("/api/registries/:id/items", injectUser, ListItems)
	r.POST("/api/registries/:id/items", injectUser, CreateItem)
	r.GET("/api/items/:id", injectUser, GetItem)
	r.PUT("/api/items/:id", injectUser, itemHandler.UpdateItem)
	r.DELETE("/api/items/:id", injectUser, DeleteItem)
	r.GET("/api/items/:id/versions", injectUser, ListItemVersions)
	r.GET("/api/items/:id/versions/:version", injectUser, GetItemVersion)
	r.GET("/api/items", injectUser, ListAllItems)
	r.POST("/api/items", injectUser, itemHandler.CreateItemDirect)
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

type memoryBackend struct {
	mu   sync.Mutex
	data map[string][]byte
}

func newMemoryBackend() *memoryBackend {
	return &memoryBackend{data: make(map[string][]byte)}
}

func (m *memoryBackend) Put(ctx context.Context, key string, reader io.Reader, size int64) error {
	data, err := io.ReadAll(reader)
	if err != nil {
		return err
	}
	m.mu.Lock()
	m.data[key] = data
	m.mu.Unlock()
	return nil
}

func (m *memoryBackend) Get(ctx context.Context, key string) (io.ReadCloser, int64, error) {
	m.mu.Lock()
	data, ok := m.data[key]
	m.mu.Unlock()
	if !ok {
		return nil, 0, fmt.Errorf("not found: %s", key)
	}
	return io.NopCloser(bytes.NewReader(data)), int64(len(data)), nil
}

func (m *memoryBackend) Delete(ctx context.Context, key string) error {
	m.mu.Lock()
	delete(m.data, key)
	m.mu.Unlock()
	return nil
}

func (m *memoryBackend) PresignURL(ctx context.Context, key string, expiry time.Duration) (string, error) {
	return "", nil
}

func (m *memoryBackend) Exists(ctx context.Context, key string) (bool, error) {
	m.mu.Lock()
	_, ok := m.data[key]
	m.mu.Unlock()
	return ok, nil
}

func (m *memoryBackend) Len() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.data)
}

func createTestZip(files map[string][]byte) []byte {
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for name, content := range files {
		w, err := zw.Create(name)
		if err != nil {
			panic(fmt.Sprintf("create zip entry %s: %v", name, err))
		}
		if _, err := w.Write(content); err != nil {
			panic(fmt.Sprintf("write zip entry %s: %v", name, err))
		}
	}
	if err := zw.Close(); err != nil {
		panic(fmt.Sprintf("close zip: %v", err))
	}
	return buf.Bytes()
}

func postMultipart(r *gin.Engine, path string, fields map[string]string, zipBytes []byte) *httptest.ResponseRecorder {
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	for key, value := range fields {
		if err := writer.WriteField(key, value); err != nil {
			panic(fmt.Sprintf("write multipart field %s: %v", key, err))
		}
	}
	fileWriter, err := writer.CreateFormFile("file", "upload.zip")
	if err != nil {
		panic(fmt.Sprintf("create multipart file: %v", err))
	}
	if _, err := fileWriter.Write(zipBytes); err != nil {
		panic(fmt.Sprintf("write multipart file: %v", err))
	}
	if err := writer.Close(); err != nil {
		panic(fmt.Sprintf("close multipart writer: %v", err))
	}

	w := httptest.NewRecorder()
	req, _ := http.NewRequest(http.MethodPost, path, &body)
	req.Header.Set("Content-Type", writer.FormDataContentType())
	r.ServeHTTP(w, req)
	return w
}

func createPublicRegistry(t *testing.T) {
	t.Helper()
	if err := database.DB.Create(&models.CapabilityRegistry{
		ID: PublicRegistryID, Name: "public", SourceType: "internal", Visibility: "public", RepoID: "public", OwnerID: "system",
	}).Error; err != nil {
		t.Fatalf("failed to create public registry: %v", err)
	}
}

func setMemoryStorageBackend(t *testing.T) *memoryBackend {
	t.Helper()
	oldBackend := StorageBackend
	backend := newMemoryBackend()
	StorageBackend = backend
	t.Cleanup(func() { StorageBackend = oldBackend })
	return backend
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
		ID: "reg-list", Name: "test-reg", SourceType: "internal", RepoID: "repo-1", OwnerID: "u1",
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
		ID: "reg-li2", Name: "test-reg2", SourceType: "internal", RepoID: "repo-1", OwnerID: "u1",
	})
	database.DB.Create(&models.CapabilityItem{
		ID: "item-a", RegistryID: "reg-li2", RepoID: "repo-1", Slug: "skill-a", ItemType: "skill",
		Name: "Skill A", Status: "active", CreatedBy: "u1", Metadata: datatypes.JSON([]byte("{}")),
	})
	database.DB.Create(&models.CapabilityItem{
		ID: "item-b", RegistryID: "reg-li2", RepoID: "repo-1", Slug: "cmd-b", ItemType: "command",
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
		ID: "reg-li3", Name: "test-reg3", SourceType: "internal", RepoID: "repo-1", OwnerID: "u1",
	})
	database.DB.Create(&models.CapabilityItem{
		ID: "item-c", RegistryID: "reg-li3", RepoID: "repo-1", Slug: "skill-c", ItemType: "skill",
		Name: "Skill C", Status: "active", CreatedBy: "u1", Metadata: datatypes.JSON([]byte("{}")),
	})
	database.DB.Create(&models.CapabilityItem{
		ID: "item-d", RegistryID: "reg-li3", RepoID: "repo-1", Slug: "cmd-d", ItemType: "command",
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
		ID: "reg-li4", Name: "test-reg4", SourceType: "internal", RepoID: "repo-1", OwnerID: "u1",
	})
	database.DB.Create(&models.CapabilityItem{
		ID: "item-e", RegistryID: "reg-li4", RepoID: "repo-1", Slug: "skill-e", ItemType: "skill",
		Name: "Skill E", Status: "active", CreatedBy: "u1", Metadata: datatypes.JSON([]byte("{}")),
	})
	database.DB.Create(&models.CapabilityItem{
		ID: "item-f", RegistryID: "reg-li4", RepoID: "repo-1", Slug: "skill-f", ItemType: "skill",
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
		ID: "reg-ci1", Name: "ci-reg", SourceType: "internal", RepoID: "repo-1", OwnerID: "u1",
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
		ID: "reg-gi1", Name: "gi-reg", SourceType: "internal", RepoID: "repo-1", OwnerID: "u1",
	})
	database.DB.Create(&models.CapabilityItem{
		ID: "item-gi1", RegistryID: "reg-gi1", RepoID: "repo-1", Slug: "get-me", ItemType: "skill",
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
		ID: "reg-ui1", Name: "ui-reg", SourceType: "internal", RepoID: "repo-1", OwnerID: "u1",
	})
	database.DB.Create(&models.CapabilityItem{
		ID: "item-ui1", RegistryID: "reg-ui1", RepoID: "repo-1", Slug: "update-me", ItemType: "skill",
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
		ID: "reg-ui2", Name: "ui-reg2", SourceType: "internal", RepoID: "repo-1", OwnerID: "u1",
	})
	database.DB.Create(&models.CapabilityItem{
		ID: "item-ui2", RegistryID: "reg-ui2", RepoID: "repo-1", Slug: "versioned", ItemType: "skill",
		Name: "Versioned", Status: "active", CreatedBy: "u1", Metadata: datatypes.JSON([]byte("{}")),
	})
	database.DB.Create(&models.CapabilityVersion{
		ID: "ver-1", ItemID: "item-ui2", Revision: 1, Content: "v1", CreatedBy: "u1",
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
		ID: "reg-di1", Name: "di-reg", SourceType: "internal", RepoID: "repo-1", OwnerID: "u1",
	})
	database.DB.Create(&models.CapabilityItem{
		ID: "item-di1", RegistryID: "reg-di1", RepoID: "repo-1", Slug: "delete-me", ItemType: "skill",
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
		ID: "reg-lv1", Name: "lv-reg", SourceType: "internal", RepoID: "repo-1", OwnerID: "u1",
	})
	database.DB.Create(&models.CapabilityItem{
		ID: "item-lv1", RegistryID: "reg-lv1", RepoID: "repo-1", Slug: "versioned", ItemType: "skill",
		Name: "Versioned", Status: "active", CreatedBy: "u1", Metadata: datatypes.JSON([]byte("{}")),
	})
	database.DB.Create(&models.CapabilityVersion{
		ID: "ver-lv1", ItemID: "item-lv1", Revision: 1, Content: "v1", CreatedBy: "u1",
		Metadata: datatypes.JSON([]byte("{}")),
	})
	database.DB.Create(&models.CapabilityVersion{
		ID: "ver-lv2", ItemID: "item-lv1", Revision: 2, Content: "v2", CreatedBy: "u1",
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
		ID: "reg-gv1", Name: "gv-reg", SourceType: "internal", RepoID: "repo-1", OwnerID: "u1",
	})
	database.DB.Create(&models.CapabilityItem{
		ID: "item-gv1", RegistryID: "reg-gv1", RepoID: "repo-1", Slug: "gv-item", ItemType: "skill",
		Name: "GV Item", Status: "active", CreatedBy: "u1", Metadata: datatypes.JSON([]byte("{}")),
	})
	database.DB.Create(&models.CapabilityVersion{
		ID: "ver-gv1", ItemID: "item-gv1", Revision: 1, Content: "v1 content", CreatedBy: "u1",
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
		ID: "pub-reg", Name: "public-r", SourceType: "internal", Visibility: "public", RepoID: "public", OwnerID: "system",
	})
	database.DB.Create(&models.CapabilityRegistry{
		ID: "priv-reg", Name: "private-r", SourceType: "internal", Visibility: "repo", RepoID: "repo-1", OwnerID: "u1",
	})

	ids := buildVisibleRegistryIDs(database.DB, "")
	if len(ids) != 1 || ids[0] != "pub-reg" {
		t.Fatalf("expected only public registry, got %v", ids)
	}
}

func TestBuildVisibleRegistryIDs_MemberUser(t *testing.T) {
	defer setupTestDB(t)()
	database.DB.Create(&models.CapabilityRegistry{
		ID: "pub-reg2", Name: "public-r2", SourceType: "internal", Visibility: "public", RepoID: "public", OwnerID: "system",
	})
	database.DB.Create(&models.CapabilityRegistry{
		ID: "repo-reg2", Name: "repo-r2", SourceType: "internal", Visibility: "repo", RepoID: "repo-x", OwnerID: "u2",
	})
	database.DB.Create(&models.RepoMember{
		ID: "mem-x", RepoID: "repo-x", UserID: "u-member", Role: "member",
	})

	ids := buildVisibleRegistryIDs(database.DB, "u-member")
	found := map[string]bool{}
	for _, id := range ids {
		found[id] = true
	}
	if !found["pub-reg2"] || !found["repo-reg2"] {
		t.Fatalf("expected both public and repo registry, got %v", ids)
	}
}

func TestBuildVisibleRegistryIDs_PersonalOwner(t *testing.T) {
	defer setupTestDB(t)()
	database.DB.Create(&models.CapabilityRegistry{
		ID: "personal-reg", Name: "my-skills", SourceType: "internal", Visibility: "private", RepoID: "", OwnerID: "u-owner",
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
		ID: PublicRegistryID, Name: "public", SourceType: "internal", Visibility: "public", RepoID: "public", OwnerID: "system",
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
		ID: PublicRegistryID, Name: "public", SourceType: "internal", Visibility: "public", RepoID: "public", OwnerID: "system",
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
		ID: PublicRegistryID, Name: "public", SourceType: "internal", Visibility: "public", RepoID: "public", OwnerID: "system",
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
		ID: PublicRegistryID, Name: "public", SourceType: "internal", Visibility: "public", RepoID: "public", OwnerID: "system",
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
		ID: PublicRegistryID, Name: "public", SourceType: "internal", Visibility: "public", RepoID: "public", OwnerID: "system",
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

func TestCreateItemDirect_ZipSkill_Success(t *testing.T) {
	defer setupTestDB(t)()
	createPublicRegistry(t)
	backend := setMemoryStorageBackend(t)

	skillContent := "---\nname: My Skill\ndescription: A test skill\nversion: 2.0.0\n---\n# My Skill\nContent here"
	zipBytes := createTestZip(map[string][]byte{
		"SKILL.md":         []byte(skillContent),
		"scripts/setup.sh": []byte("#!/bin/bash\necho hello"),
	})

	w := postMultipart(newItemRouter("u1"), "/api/items", map[string]string{
		"itemType": "skill",
		"name":     "My Skill",
	}, zipBytes)
	if w.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", w.Code, w.Body.String())
	}

	var item map[string]interface{}
	if err := json.NewDecoder(w.Body).Decode(&item); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if item["slug"] != "my-skill" {
		t.Fatalf("expected slug=my-skill, got %v", item["slug"])
	}
	if item["content"] != skillContent {
		t.Fatalf("unexpected content: %v", item["content"])
	}
	if item["description"] != "A test skill" {
		t.Fatalf("unexpected description: %v", item["description"])
	}

	itemID, _ := item["id"].(string)
	if itemID == "" {
		t.Fatal("expected item id in response")
	}

	var assets []models.CapabilityAsset
	if err := database.DB.Where("item_id = ?", itemID).Find(&assets).Error; err != nil {
		t.Fatalf("load assets: %v", err)
	}
	if len(assets) != 1 {
		t.Fatalf("expected 1 asset, got %d", len(assets))
	}
	if assets[0].RelPath != "scripts/setup.sh" {
		t.Fatalf("unexpected asset path: %s", assets[0].RelPath)
	}
	if assets[0].TextContent == nil || *assets[0].TextContent != "#!/bin/bash\necho hello" {
		t.Fatalf("unexpected text asset content: %#v", assets[0].TextContent)
	}

	var artifact models.CapabilityArtifact
	if err := database.DB.Where("item_id = ? AND filename = ?", itemID, "upload.zip").First(&artifact).Error; err != nil {
		t.Fatalf("load artifact: %v", err)
	}
	if artifact.StorageKey == "" {
		t.Fatal("expected artifact storage key")
	}
	backend.mu.Lock()
	_, ok := backend.data[artifact.StorageKey]
	backend.mu.Unlock()
	if !ok {
		t.Fatalf("expected stored artifact for key %s", artifact.StorageKey)
	}
}

func TestCreateItemDirect_ZipSkill_MainFileOnly(t *testing.T) {
	defer setupTestDB(t)()
	createPublicRegistry(t)
	setMemoryStorageBackend(t)

	zipBytes := createTestZip(map[string][]byte{
		"SKILL.md": []byte("# Skill\n"),
	})

	w := postMultipart(newItemRouter("u1"), "/api/items", map[string]string{
		"itemType": "skill",
		"name":     "Main File Only",
	}, zipBytes)
	if w.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", w.Code, w.Body.String())
	}

	var item map[string]interface{}
	if err := json.NewDecoder(w.Body).Decode(&item); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	itemID, _ := item["id"].(string)
	if itemID == "" {
		t.Fatal("expected item id in response")
	}

	var assetCount int64
	if err := database.DB.Model(&models.CapabilityAsset{}).Where("item_id = ?", itemID).Count(&assetCount).Error; err != nil {
		t.Fatalf("count assets: %v", err)
	}
	if assetCount != 0 {
		t.Fatalf("expected 0 assets, got %d", assetCount)
	}

	var artifactCount int64
	if err := database.DB.Model(&models.CapabilityArtifact{}).Where("item_id = ? AND filename = ?", itemID, "upload.zip").Count(&artifactCount).Error; err != nil {
		t.Fatalf("count artifacts: %v", err)
	}
	if artifactCount != 1 {
		t.Fatalf("expected 1 artifact, got %d", artifactCount)
	}
}

func TestCreateItemDirect_ZipMCP_Success(t *testing.T) {
	defer setupTestDB(t)()
	createPublicRegistry(t)
	setMemoryStorageBackend(t)

	zipBytes := createTestZip(map[string][]byte{
		".mcp.json": []byte(`{"mcpServers":{"test":{"command":"npx","args":["-y","@test/mcp"]}}}`),
	})

	w := postMultipart(newItemRouter("u1"), "/api/items", map[string]string{
		"itemType": "mcp",
		"name":     "Test MCP",
	}, zipBytes)
	if w.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", w.Code, w.Body.String())
	}

	var item map[string]interface{}
	if err := json.NewDecoder(w.Body).Decode(&item); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	metadata, ok := item["metadata"].(map[string]interface{})
	if !ok {
		t.Fatalf("expected metadata object, got %#v", item["metadata"])
	}
	if metadata["hosting_type"] != "command" {
		t.Fatalf("expected hosting_type=command, got %#v", metadata["hosting_type"])
	}
}

func TestCreateItemDirect_ZipMCP_MultiServer(t *testing.T) {
	defer setupTestDB(t)()
	createPublicRegistry(t)
	setMemoryStorageBackend(t)

	zipBytes := createTestZip(map[string][]byte{
		".mcp.json": []byte(`{"mcpServers":{"one":{"command":"one"},"two":{"command":"two"}}}`),
	})

	w := postMultipart(newItemRouter("u1"), "/api/items", map[string]string{
		"itemType": "mcp",
		"name":     "Multi MCP",
	}, zipBytes)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", w.Code, w.Body.String())
	}
}

func TestCreateItemDirect_Zip_MissingMainFile(t *testing.T) {
	defer setupTestDB(t)()
	createPublicRegistry(t)
	setMemoryStorageBackend(t)

	zipBytes := createTestZip(map[string][]byte{
		"README.md": []byte("# missing main\n"),
	})

	w := postMultipart(newItemRouter("u1"), "/api/items", map[string]string{
		"itemType": "skill",
		"name":     "Missing Main",
	}, zipBytes)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "SKILL.md") {
		t.Fatalf("expected error to mention SKILL.md, got %s", w.Body.String())
	}
}

func TestCreateItemDirect_Zip_UnsupportedItemType(t *testing.T) {
	defer setupTestDB(t)()
	createPublicRegistry(t)
	setMemoryStorageBackend(t)

	zipBytes := createTestZip(map[string][]byte{
		"SKILL.md": []byte("# Skill\n"),
	})

	w := postMultipart(newItemRouter("u1"), "/api/items", map[string]string{
		"itemType": "hook",
		"name":     "Unsupported",
	}, zipBytes)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", w.Code, w.Body.String())
	}
}

func TestCreateItemDirect_Zip_MissingRequiredFields(t *testing.T) {
	defer setupTestDB(t)()
	createPublicRegistry(t)
	setMemoryStorageBackend(t)

	zipBytes := createTestZip(map[string][]byte{
		"SKILL.md": []byte("# Skill\n"),
	})

	w := postMultipart(newItemRouter("u1"), "/api/items", map[string]string{
		"itemType": "skill",
	}, zipBytes)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", w.Code, w.Body.String())
	}
}

func TestCreateItemDirect_Zip_SlugConflict(t *testing.T) {
	defer setupTestDB(t)()
	createPublicRegistry(t)
	backend := setMemoryStorageBackend(t)

	if err := database.DB.Create(&models.CapabilityItem{
		ID: "existing-item", RegistryID: PublicRegistryID, RepoID: "public", Slug: "my-skill", ItemType: "skill", Name: "Existing Skill", Version: "1.0.0", Status: "active", CreatedBy: "u1", Metadata: datatypes.JSON([]byte("{}")),
	}).Error; err != nil {
		t.Fatalf("create existing item: %v", err)
	}

	zipBytes := createTestZip(map[string][]byte{
		"SKILL.md": []byte("# Skill\n"),
	})

	w := postMultipart(newItemRouter("u1"), "/api/items", map[string]string{
		"itemType": "skill",
		"name":     "My Skill",
	}, zipBytes)
	if w.Code != http.StatusConflict {
		t.Fatalf("expected 409, got %d: %s", w.Code, w.Body.String())
	}

	// Storage must be fully cleaned up — no orphaned zip or asset files.
	if n := backend.Len(); n != 0 {
		t.Errorf("expected 0 storage keys after slug conflict, got %d", n)
	}
}

func TestCreateItemDirect_JSON_CrossRepoSlugAllowed(t *testing.T) {
	defer setupTestDB(t)()
	createPublicRegistry(t)

	// Create a separate repo + registry
	database.DB.Create(&models.Repository{ID: "repo-x", Name: "other", OwnerID: "u1"})
	database.DB.Create(&models.CapabilityRegistry{
		ID: "reg-x", Name: "other-reg", SourceType: "internal", Visibility: "repo", RepoID: "repo-x", OwnerID: "u1",
	})
	database.DB.Create(&models.RepoMember{ID: "rm-x", RepoID: "repo-x", UserID: "u1", Role: "owner"})

	// Create an item in repo-x with slug "dup-slug"
	if err := database.DB.Create(&models.CapabilityItem{
		ID: "item-other-repo", RegistryID: "reg-x", RepoID: "repo-x",
		Slug: "dup-slug", ItemType: "skill", Name: "Other Repo Skill",
		Version: "1.0.0", Status: "active", CreatedBy: "u1",
		Metadata: datatypes.JSON([]byte("{}")),
	}).Error; err != nil {
		t.Fatalf("create item in other repo: %v", err)
	}

	// Creating an item with the same slug+type in public registry should succeed
	// because it's a different repo.
	w := postJSON(newItemRouter("u1"), "/api/items", map[string]interface{}{
		"itemType": "skill",
		"name":     "dup-slug",
		"slug":     "dup-slug",
		"content":  "# Dup Slug",
	})
	if w.Code != http.StatusCreated {
		t.Fatalf("expected 201 (cross-repo same slug allowed), got %d: %s", w.Code, w.Body.String())
	}
}


func TestCreateItemDirect_JSON_StillWorks(t *testing.T) {
	defer setupTestDB(t)()
	createPublicRegistry(t)

	w := postJSON(newItemRouter("u1"), "/api/items", map[string]interface{}{
		"itemType": "skill",
		"name":     "json-test",
		"content":  "# JSON Test",
	})
	if w.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", w.Code, w.Body.String())
	}
}

func putMultipart(r *gin.Engine, path string, fields map[string]string, zipBytes []byte) *httptest.ResponseRecorder {
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	for key, value := range fields {
		if err := writer.WriteField(key, value); err != nil {
			panic(fmt.Sprintf("write multipart field %s: %v", key, err))
		}
	}
	fileWriter, err := writer.CreateFormFile("file", "upload.zip")
	if err != nil {
		panic(fmt.Sprintf("create multipart file: %v", err))
	}
	if _, err := fileWriter.Write(zipBytes); err != nil {
		panic(fmt.Sprintf("write multipart file: %v", err))
	}
	if err := writer.Close(); err != nil {
		panic(fmt.Sprintf("close multipart writer: %v", err))
	}

	w := httptest.NewRecorder()
	req, _ := http.NewRequest(http.MethodPut, path, &body)
	req.Header.Set("Content-Type", writer.FormDataContentType())
	r.ServeHTTP(w, req)
	return w
}

// ---------------------------------------------------------------------------
// UpdateItem (archive)
// ---------------------------------------------------------------------------

func TestUpdateItem_Archive_Success(t *testing.T) {
	defer setupTestDB(t)()
	createPublicRegistry(t)
	backend := setMemoryStorageBackend(t)

	// Create an item via zip first.
	initContent := "---\nname: Init Skill\ndescription: Original\nversion: 1.0.0\n---\n# Init"
	initZip := createTestZip(map[string][]byte{
		"SKILL.md":          []byte(initContent),
		"scripts/setup.sh":  []byte("#!/bin/bash\necho init"),
	})
	w := postMultipart(newItemRouter("u1"), "/api/items", map[string]string{
		"itemType": "skill",
		"name":     "Init Skill",
	}, initZip)
	if w.Code != http.StatusCreated {
		t.Fatalf("create: expected 201, got %d: %s", w.Code, w.Body.String())
	}
	var created map[string]interface{}
	json.NewDecoder(w.Body).Decode(&created)
	itemID := created["id"].(string)

	if created["sourceType"] != "archive" {
		t.Fatalf("expected sourceType=archive after zip create, got %v", created["sourceType"])
	}

	// Now update via archive.
	updatedContent := "---\nname: Updated Skill\ndescription: Updated\nversion: 2.0.0\n---\n# Updated"
	updatedZip := createTestZip(map[string][]byte{
		"SKILL.md":         []byte(updatedContent),
		"scripts/deploy.sh": []byte("#!/bin/bash\necho deploy"),
	})
	w = putMultipart(newItemRouter("u1"), "/api/items/"+itemID, map[string]string{
		"commitMsg": "update to v2",
	}, updatedZip)
	if w.Code != http.StatusOK {
		t.Fatalf("update: expected 200, got %d: %s", w.Code, w.Body.String())
	}

	var updated map[string]interface{}
	json.NewDecoder(w.Body).Decode(&updated)

	if updated["content"] != updatedContent {
		t.Fatalf("expected updated content, got %v", updated["content"])
	}
	if updated["sourceType"] != "archive" {
		t.Fatalf("expected sourceType=archive, got %v", updated["sourceType"])
	}
	if updated["description"] != "Updated" {
		t.Fatalf("expected description=Updated, got %v", updated["description"])
	}

	// Assets should be replaced — old setup.sh gone, new deploy.sh present.
	var assets []models.CapabilityAsset
	database.DB.Where("item_id = ?", itemID).Find(&assets)
	if len(assets) != 1 {
		t.Fatalf("expected 1 asset after update, got %d", len(assets))
	}
	if assets[0].RelPath != "scripts/deploy.sh" {
		t.Fatalf("expected asset scripts/deploy.sh, got %s", assets[0].RelPath)
	}

	// Should have 2 versions now.
	var versions []models.CapabilityVersion
	database.DB.Where("item_id = ?", itemID).Find(&versions)
	if len(versions) != 2 {
		t.Fatalf("expected 2 versions, got %d", len(versions))
	}

	// Should have 2 artifacts, only the new one is latest.
	var artifacts []models.CapabilityArtifact
	database.DB.Where("item_id = ?", itemID).Find(&artifacts)
	if len(artifacts) != 2 {
		t.Fatalf("expected 2 artifacts, got %d", len(artifacts))
	}
	latestCount := 0
	for _, a := range artifacts {
		if a.IsLatest {
			latestCount++
		}
	}
	if latestCount != 1 {
		t.Fatalf("expected exactly 1 latest artifact, got %d", latestCount)
	}

	// Storage should have new archive and asset files.
	if backend.Len() == 0 {
		t.Fatal("expected non-empty storage after archive update")
	}
}

func TestUpdateItem_Archive_NotFound(t *testing.T) {
	defer setupTestDB(t)()
	createPublicRegistry(t)
	setMemoryStorageBackend(t)

	zipBytes := createTestZip(map[string][]byte{
		"SKILL.md": []byte("# Skill\n"),
	})
	w := putMultipart(newItemRouter("u1"), "/api/items/no-such-id", map[string]string{}, zipBytes)
	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d: %s", w.Code, w.Body.String())
	}
}

func TestUpdateItem_JSON_SourceTypeRemainsDirect(t *testing.T) {
	defer setupTestDB(t)()
	database.DB.Create(&models.CapabilityRegistry{
		ID: "reg-st1", Name: "st-reg", SourceType: "internal", RepoID: "repo-1", OwnerID: "u1",
	})
	database.DB.Create(&models.CapabilityItem{
		ID: "item-st1", RegistryID: "reg-st1", RepoID: "repo-1", Slug: "direct-item", ItemType: "skill",
		Name: "Direct", Status: "active", CreatedBy: "u1", SourceType: "direct",
		Metadata: datatypes.JSON([]byte("{}")),
	})

	w := putJSON(newItemRouter("u1"), "/api/items/item-st1", map[string]interface{}{
		"name": "Renamed", "updatedBy": "u1",
	})
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var item map[string]interface{}
	json.NewDecoder(w.Body).Decode(&item)
	if item["sourceType"] != "direct" {
		t.Fatalf("expected sourceType=direct after JSON update, got %v", item["sourceType"])
	}
}

// ensure fmt is used
var _ = fmt.Sprintf
