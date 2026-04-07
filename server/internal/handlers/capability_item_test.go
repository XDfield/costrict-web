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
	itemHandler := NewItemHandler(db, indexerSvc, &services.ParserService{}, nil)

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
	r.PUT("/api/items/:id/move", injectUser, MoveItem)
	r.PUT("/api/items/:id/transfer", injectUser, TransferItemToRepo)
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
		ID: PublicRegistryID, Name: "public", SourceType: "internal", RepoID: "public", OwnerID: "system",
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

func TestListItems_SortByFavoriteCountDesc(t *testing.T) {
	defer setupTestDB(t)()
	database.DB.Create(&models.CapabilityRegistry{
		ID: "reg-li-sort-fav", Name: "sort-reg", SourceType: "internal", RepoID: "repo-1", OwnerID: "u1",
	})
	database.DB.Create(&models.CapabilityItem{
		ID: "item-li-fav-low", RegistryID: "reg-li-sort-fav", RepoID: "repo-1", Slug: "fav-low", ItemType: "skill",
		Name: "Fav Low", Status: "active", CreatedBy: "u1", Metadata: datatypes.JSON([]byte("{}")), FavoriteCount: 2,
	})
	database.DB.Create(&models.CapabilityItem{
		ID: "item-li-fav-high", RegistryID: "reg-li-sort-fav", RepoID: "repo-1", Slug: "fav-high", ItemType: "skill",
		Name: "Fav High", Status: "active", CreatedBy: "u1", Metadata: datatypes.JSON([]byte("{}")), FavoriteCount: 8,
	})

	w := get(newItemRouter(""), "/api/registries/reg-li-sort-fav/items?sortBy=favoriteCount&sortOrder=desc")
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}

	var body map[string]interface{}
	json.NewDecoder(w.Body).Decode(&body)
	items := body["items"].([]interface{})
	if len(items) != 2 {
		t.Fatalf("expected 2 items, got %d", len(items))
	}
	first := items[0].(map[string]interface{})
	if first["id"] != "item-li-fav-high" {
		t.Fatalf("expected highest favoriteCount first, got %v", first["id"])
	}
}

func TestListAllItems_SortByPreviewCountDesc(t *testing.T) {
	defer setupTestDB(t)()
	database.DB.Create(&models.CapabilityRegistry{
		ID: "reg-all-sort-preview", Name: "public", SourceType: "internal", RepoID: "public", OwnerID: "system",
	})
	database.DB.Create(&models.CapabilityItem{
		ID: "item-all-preview-low", RegistryID: "reg-all-sort-preview", RepoID: "public", Slug: "preview-low", ItemType: "skill",
		Name: "Preview Low", Status: "active", CreatedBy: "u1", Metadata: datatypes.JSON([]byte("{}")), PreviewCount: 3,
	})
	database.DB.Create(&models.CapabilityItem{
		ID: "item-all-preview-high", RegistryID: "reg-all-sort-preview", RepoID: "public", Slug: "preview-high", ItemType: "skill",
		Name: "Preview High", Status: "active", CreatedBy: "u1", Metadata: datatypes.JSON([]byte("{}")), PreviewCount: 11,
	})

	w := get(newItemRouter(""), "/api/items?sortBy=previewCount&sortOrder=desc")
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}

	var body map[string]interface{}
	json.NewDecoder(w.Body).Decode(&body)
	items := body["items"].([]interface{})
	if len(items) != 2 {
		t.Fatalf("expected 2 items, got %d", len(items))
	}
	first := items[0].(map[string]interface{})
	if first["id"] != "item-all-preview-high" {
		t.Fatalf("expected highest previewCount first, got %v", first["id"])
	}
}

func TestListAllItems_SortByInstallCountAsc(t *testing.T) {
	defer setupTestDB(t)()
	database.DB.Create(&models.CapabilityRegistry{
		ID: "reg-all-sort-install", Name: "public", SourceType: "internal", RepoID: "public", OwnerID: "system",
	})
	database.DB.Create(&models.CapabilityItem{
		ID: "item-all-install-high", RegistryID: "reg-all-sort-install", RepoID: "public", Slug: "install-high", ItemType: "skill",
		Name: "Install High", Status: "active", CreatedBy: "u1", Metadata: datatypes.JSON([]byte("{}")), InstallCount: 10,
	})
	database.DB.Create(&models.CapabilityItem{
		ID: "item-all-install-low", RegistryID: "reg-all-sort-install", RepoID: "public", Slug: "install-low", ItemType: "skill",
		Name: "Install Low", Status: "active", CreatedBy: "u1", Metadata: datatypes.JSON([]byte("{}")), InstallCount: 1,
	})

	w := get(newItemRouter(""), "/api/items?sortBy=installCount&sortOrder=asc")
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}

	var body map[string]interface{}
	json.NewDecoder(w.Body).Decode(&body)
	items := body["items"].([]interface{})
	if len(items) != 2 {
		t.Fatalf("expected 2 items, got %d", len(items))
	}
	first := items[0].(map[string]interface{})
	if first["id"] != "item-all-install-low" {
		t.Fatalf("expected lowest installCount first, got %v", first["id"])
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
		PreviewCount: 12, InstallCount: 3, FavoriteCount: 5,
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
	if item["previewCount"] != float64(12) || item["installCount"] != float64(3) || item["favoriteCount"] != float64(5) {
		t.Fatalf("unexpected metric fields: preview=%v install=%v favorite=%v", item["previewCount"], item["installCount"], item["favoriteCount"])
	}
}

func TestGetItem_RepoVisibilityField(t *testing.T) {
	defer setupTestDB(t)()
	database.DB.Create(&models.Repository{
		ID: "repo-gi-vis", Name: "gi-vis-repo", OwnerID: "u1", Visibility: "public",
	})
	database.DB.Create(&models.CapabilityRegistry{
		ID: "reg-gi-vis", Name: "gi-vis-reg", SourceType: "internal", RepoID: "repo-gi-vis", OwnerID: "u1",
	})
	database.DB.Create(&models.CapabilityItem{
		ID: "item-gi-vis", RegistryID: "reg-gi-vis", RepoID: "repo-gi-vis", Slug: "vis-item", ItemType: "skill",
		Name: "Vis Item", Status: "active", CreatedBy: "u1", Metadata: datatypes.JSON([]byte("{}")),
	})

	w := get(newItemRouter(""), "/api/items/item-gi-vis")
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	var item map[string]interface{}
	json.NewDecoder(w.Body).Decode(&item)
	if item["repoVisibility"] != "public" {
		t.Fatalf("expected repoVisibility=public, got %v", item["repoVisibility"])
	}
}

func TestGetItem_PrivateRepoVisibilityField(t *testing.T) {
	defer setupTestDB(t)()
	database.DB.Create(&models.Repository{
		ID: "repo-gi-priv", Name: "gi-priv-repo", OwnerID: "u1", Visibility: "private",
	})
	database.DB.Create(&models.CapabilityRegistry{
		ID: "reg-gi-priv", Name: "gi-priv-reg", SourceType: "internal", RepoID: "repo-gi-priv", OwnerID: "u1",
	})
	database.DB.Create(&models.CapabilityItem{
		ID: "item-gi-priv", RegistryID: "reg-gi-priv", RepoID: "repo-gi-priv", Slug: "priv-item", ItemType: "skill",
		Name: "Priv Item", Status: "active", CreatedBy: "u1", Metadata: datatypes.JSON([]byte("{}")),
	})

	w := get(newItemRouter(""), "/api/items/item-gi-priv")
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	var item map[string]interface{}
	json.NewDecoder(w.Body).Decode(&item)
	if item["repoVisibility"] != "private" {
		t.Fatalf("expected repoVisibility=private, got %v", item["repoVisibility"])
	}
}

func TestGetItem_FavoritedForCurrentUser(t *testing.T) {
	defer setupTestDB(t)()
	database.DB.Create(&models.CapabilityRegistry{
		ID: "reg-gi-fav", Name: "gi-fav-reg", SourceType: "internal", RepoID: "repo-1", OwnerID: "u1",
	})
	database.DB.Create(&models.CapabilityItem{
		ID: "item-gi-fav", RegistryID: "reg-gi-fav", RepoID: "repo-1", Slug: "fav-item", ItemType: "skill",
		Name: "Fav Item", Status: "active", CreatedBy: "u1", Metadata: datatypes.JSON([]byte("{}")),
		FavoriteCount: 1,
	})
	database.DB.Create(&models.ItemFavorite{
		ID:     "fav-gi-1",
		ItemID: "item-gi-fav",
		UserID: "viewer-1",
	})

	w := get(newItemRouter("viewer-1"), "/api/items/item-gi-fav")
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	var item map[string]interface{}
	json.NewDecoder(w.Body).Decode(&item)
	if item["favorited"] != true {
		t.Fatalf("expected favorited=true, got %v", item["favorited"])
	}
}

func TestGetItem_NotFavoritedForAnonymousUser(t *testing.T) {
	defer setupTestDB(t)()
	database.DB.Create(&models.CapabilityRegistry{
		ID: "reg-gi-anon", Name: "gi-anon-reg", SourceType: "internal", RepoID: "repo-1", OwnerID: "u1",
	})
	database.DB.Create(&models.CapabilityItem{
		ID: "item-gi-anon", RegistryID: "reg-gi-anon", RepoID: "repo-1", Slug: "anon-item", ItemType: "skill",
		Name: "Anon Item", Status: "active", CreatedBy: "u1", Metadata: datatypes.JSON([]byte("{}")),
		FavoriteCount: 1,
	})
	database.DB.Create(&models.ItemFavorite{
		ID:     "fav-gi-2",
		ItemID: "item-gi-anon",
		UserID: "viewer-1",
	})

	w := get(newItemRouter(""), "/api/items/item-gi-anon")
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	var item map[string]interface{}
	json.NewDecoder(w.Body).Decode(&item)
	if item["favorited"] != false {
		t.Fatalf("expected favorited=false, got %v", item["favorited"])
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
		ID: "pub-reg", Name: "public-r", SourceType: "internal", RepoID: "public", OwnerID: "system",
	})
	database.DB.Create(&models.CapabilityRegistry{
		ID: "priv-reg", Name: "private-r", SourceType: "internal", RepoID: "repo-1", OwnerID: "u1",
	})

	ids := buildVisibleRegistryIDs(database.DB, "")
	if len(ids) != 1 || ids[0] != "pub-reg" {
		t.Fatalf("expected only public registry, got %v", ids)
	}
}

func TestBuildVisibleRegistryIDs_MemberUser(t *testing.T) {
	defer setupTestDB(t)()
	database.DB.Create(&models.CapabilityRegistry{
		ID: "pub-reg2", Name: "public-r2", SourceType: "internal", RepoID: "public", OwnerID: "system",
	})
	database.DB.Create(&models.CapabilityRegistry{
		ID: "repo-reg2", Name: "repo-r2", SourceType: "internal", RepoID: "repo-x", OwnerID: "u2",
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

func TestBuildVisibleRegistryIDs_ExplicitPublicRepo(t *testing.T) {
	defer setupTestDB(t)()
	database.DB.Create(&models.Repository{ID: "repo-vis-pub", Name: "vis-pub", OwnerID: "u1", Visibility: "public"})
	database.DB.Create(&models.CapabilityRegistry{
		ID: "reg-vis-pub", Name: "vis-pub-reg", SourceType: "internal", RepoID: "repo-vis-pub", OwnerID: "u1",
	})

	// Anonymous should see registries under public repos
	ids := buildVisibleRegistryIDs(database.DB, "")
	found := false
	for _, id := range ids {
		if id == "reg-vis-pub" {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected public repo registry visible to anonymous, got %v", ids)
	}
}

func TestBuildVisibleRegistryIDs_ExplicitPrivateRepo_AnonymousExcluded(t *testing.T) {
	defer setupTestDB(t)()
	database.DB.Create(&models.Repository{ID: "repo-vis-priv", Name: "vis-priv", OwnerID: "u1", Visibility: "private"})
	database.DB.Create(&models.CapabilityRegistry{
		ID: "reg-vis-priv", Name: "vis-priv-reg", SourceType: "internal", RepoID: "repo-vis-priv", OwnerID: "u1",
	})

	// Anonymous should NOT see registries under private repos
	ids := buildVisibleRegistryIDs(database.DB, "")
	for _, id := range ids {
		if id == "reg-vis-priv" {
			t.Fatalf("private repo registry should NOT be visible to anonymous, got %v", ids)
		}
	}
}

func TestBuildVisibleRegistryIDs_ExplicitPrivateRepo_MemberIncluded(t *testing.T) {
	defer setupTestDB(t)()
	database.DB.Create(&models.Repository{ID: "repo-vis-pm", Name: "vis-pm", OwnerID: "u1", Visibility: "private"})
	database.DB.Create(&models.CapabilityRegistry{
		ID: "reg-vis-pm", Name: "vis-pm-reg", SourceType: "internal", RepoID: "repo-vis-pm", OwnerID: "u1",
	})
	database.DB.Create(&models.RepoMember{
		ID: "mem-vis-pm", RepoID: "repo-vis-pm", UserID: "u-vis-member", Role: "member",
	})

	ids := buildVisibleRegistryIDs(database.DB, "u-vis-member")
	found := false
	for _, id := range ids {
		if id == "reg-vis-pm" {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected private repo registry visible to member, got %v", ids)
	}
}

func TestBuildVisibleRegistryIDs_ExplicitPrivateRepo_NonMemberExcluded(t *testing.T) {
	defer setupTestDB(t)()
	database.DB.Create(&models.Repository{ID: "repo-vis-nm", Name: "vis-nm", OwnerID: "u1", Visibility: "private"})
	database.DB.Create(&models.CapabilityRegistry{
		ID: "reg-vis-nm", Name: "vis-nm-reg", SourceType: "internal", RepoID: "repo-vis-nm", OwnerID: "u1",
	})

	ids := buildVisibleRegistryIDs(database.DB, "non-member-user")
	for _, id := range ids {
		if id == "reg-vis-nm" {
			t.Fatalf("private repo registry should NOT be visible to non-member, got %v", ids)
		}
	}
}

// ---------------------------------------------------------------------------
// GetPublicRegistry
// ---------------------------------------------------------------------------

func TestGetPublicRegistry_Found(t *testing.T) {
	defer setupTestDB(t)()
	database.DB.Create(&models.CapabilityRegistry{
		ID: PublicRegistryID, Name: "public", SourceType: "internal", RepoID: "public", OwnerID: "system",
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
		ID: PublicRegistryID, Name: "public", SourceType: "internal", RepoID: "public", OwnerID: "system",
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
		ID: PublicRegistryID, Name: "public", SourceType: "internal", RepoID: "public", OwnerID: "system",
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
		ID: PublicRegistryID, Name: "public", SourceType: "internal", RepoID: "public", OwnerID: "system",
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
		ID: PublicRegistryID, Name: "public", SourceType: "internal", RepoID: "public", OwnerID: "system",
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
	if metadata["command"] == nil {
		t.Fatalf("expected command in metadata, got %#v", metadata)
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
		ID: "reg-x", Name: "other-reg", SourceType: "internal", RepoID: "repo-x", OwnerID: "u1",
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
		"SKILL.md":         []byte(initContent),
		"scripts/setup.sh": []byte("#!/bin/bash\necho init"),
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
		"SKILL.md":          []byte(updatedContent),
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

// ---------------------------------------------------------------------------
// MoveItem
// ---------------------------------------------------------------------------

func TestMoveItem_Success(t *testing.T) {
	defer setupTestDB(t)()
	// Source registry + repo
	database.DB.Create(&models.Repository{ID: "repo-src", Name: "src-repo", OwnerID: "u1"})
	database.DB.Create(&models.CapabilityRegistry{
		ID: "reg-src", Name: "src-reg", SourceType: "internal", RepoID: "repo-src", OwnerID: "u1",
	})
	// Target registry + repo
	database.DB.Create(&models.Repository{ID: "repo-tgt", Name: "tgt-repo", OwnerID: "u1"})
	database.DB.Create(&models.CapabilityRegistry{
		ID: "reg-tgt", Name: "tgt-reg", SourceType: "internal", RepoID: "repo-tgt", OwnerID: "u1",
	})
	// Caller is member of target repo
	database.DB.Create(&models.RepoMember{ID: "mem-move1", RepoID: "repo-tgt", UserID: "u1", Role: "member"})
	// Item to move
	database.DB.Create(&models.CapabilityItem{
		ID: "item-move1", RegistryID: "reg-src", RepoID: "repo-src", Slug: "move-me", ItemType: "skill",
		Name: "Move Me", Status: "active", CreatedBy: "u1", Metadata: datatypes.JSON([]byte("{}")),
	})

	w := putJSON(newItemRouter("u1"), "/api/items/item-move1/move", map[string]interface{}{
		"targetRegistryId": "reg-tgt",
	})
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	var item map[string]interface{}
	json.NewDecoder(w.Body).Decode(&item)
	if item["registryId"] != "reg-tgt" {
		t.Fatalf("expected registryId=reg-tgt, got %v", item["registryId"])
	}
	if item["repoId"] != "repo-tgt" {
		t.Fatalf("expected repoId=repo-tgt, got %v", item["repoId"])
	}

	// Verify in DB
	var dbItem models.CapabilityItem
	database.DB.First(&dbItem, "id = ?", "item-move1")
	if dbItem.RegistryID != "reg-tgt" {
		t.Fatalf("DB: expected registryId=reg-tgt, got %s", dbItem.RegistryID)
	}
	if dbItem.RepoID != "repo-tgt" {
		t.Fatalf("DB: expected repoId=repo-tgt, got %s", dbItem.RepoID)
	}
}

func TestMoveItem_NotFound(t *testing.T) {
	defer setupTestDB(t)()
	w := putJSON(newItemRouter("u1"), "/api/items/no-such/move", map[string]interface{}{
		"targetRegistryId": "reg-x",
	})
	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", w.Code)
	}
}

func TestMoveItem_Unauthenticated(t *testing.T) {
	defer setupTestDB(t)()
	database.DB.Create(&models.CapabilityRegistry{
		ID: "reg-noauth", Name: "noauth-reg", SourceType: "internal", RepoID: "repo-1", OwnerID: "u1",
	})
	database.DB.Create(&models.CapabilityItem{
		ID: "item-noauth", RegistryID: "reg-noauth", RepoID: "repo-1", Slug: "noauth-item", ItemType: "skill",
		Name: "No Auth", Status: "active", CreatedBy: "u1", Metadata: datatypes.JSON([]byte("{}")),
	})

	w := putJSON(newItemRouter(""), "/api/items/item-noauth/move", map[string]interface{}{
		"targetRegistryId": "reg-x",
	})
	if w.Code != http.StatusForbidden {
		t.Fatalf("expected 403, got %d", w.Code)
	}
}

func TestMoveItem_ForbiddenNotCreatorNorAdmin(t *testing.T) {
	defer setupTestDB(t)()
	database.DB.Create(&models.Repository{ID: "repo-fb", Name: "fb-repo", OwnerID: "u1"})
	database.DB.Create(&models.CapabilityRegistry{
		ID: "reg-fb", Name: "fb-reg", SourceType: "internal", RepoID: "repo-fb", OwnerID: "u1",
	})
	database.DB.Create(&models.CapabilityItem{
		ID: "item-fb", RegistryID: "reg-fb", RepoID: "repo-fb", Slug: "fb-item", ItemType: "skill",
		Name: "Forbidden", Status: "active", CreatedBy: "u1", Metadata: datatypes.JSON([]byte("{}")),
	})
	// u-other is NOT the creator and NOT an admin of repo-fb
	database.DB.Create(&models.RepoMember{ID: "mem-fb", RepoID: "repo-fb", UserID: "u-other", Role: "member"})

	w := putJSON(newItemRouter("u-other"), "/api/items/item-fb/move", map[string]interface{}{
		"targetRegistryId": "reg-fb",
	})
	if w.Code != http.StatusForbidden {
		t.Fatalf("expected 403, got %d", w.Code)
	}
}

func TestMoveItem_SameRegistry(t *testing.T) {
	defer setupTestDB(t)()
	database.DB.Create(&models.CapabilityRegistry{
		ID: "reg-same", Name: "same-reg", SourceType: "internal", RepoID: "repo-1", OwnerID: "u1",
	})
	database.DB.Create(&models.CapabilityItem{
		ID: "item-same", RegistryID: "reg-same", RepoID: "repo-1", Slug: "same-item", ItemType: "skill",
		Name: "Same", Status: "active", CreatedBy: "u1", Metadata: datatypes.JSON([]byte("{}")),
	})

	w := putJSON(newItemRouter("u1"), "/api/items/item-same/move", map[string]interface{}{
		"targetRegistryId": "reg-same",
	})
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", w.Code)
	}
}

func TestMoveItem_TargetExternalRegistry(t *testing.T) {
	defer setupTestDB(t)()
	database.DB.Create(&models.CapabilityRegistry{
		ID: "reg-src-ext", Name: "src-ext", SourceType: "internal", RepoID: "repo-1", OwnerID: "u1",
	})
	database.DB.Create(&models.CapabilityRegistry{
		ID: "reg-ext", Name: "ext-reg", SourceType: "external", RepoID: "repo-1", OwnerID: "u1",
	})
	database.DB.Create(&models.CapabilityItem{
		ID: "item-ext", RegistryID: "reg-src-ext", RepoID: "repo-1", Slug: "ext-item", ItemType: "skill",
		Name: "Ext Item", Status: "active", CreatedBy: "u1", Metadata: datatypes.JSON([]byte("{}")),
	})

	w := putJSON(newItemRouter("u1"), "/api/items/item-ext/move", map[string]interface{}{
		"targetRegistryId": "reg-ext",
	})
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", w.Code)
	}
}

func TestMoveItem_SlugConflict(t *testing.T) {
	defer setupTestDB(t)()
	database.DB.Create(&models.Repository{ID: "repo-sc", Name: "sc-repo", OwnerID: "u1"})
	database.DB.Create(&models.CapabilityRegistry{
		ID: "reg-sc-src", Name: "sc-src", SourceType: "internal", RepoID: "repo-sc", OwnerID: "u1",
	})
	database.DB.Create(&models.CapabilityRegistry{
		ID: "reg-sc-tgt", Name: "sc-tgt", SourceType: "internal", RepoID: "repo-sc", OwnerID: "u1",
	})
	// Existing item in target registry with same slug+type
	database.DB.Create(&models.CapabilityItem{
		ID: "item-sc-existing", RegistryID: "reg-sc-tgt", RepoID: "repo-sc", Slug: "dup-slug", ItemType: "skill",
		Name: "Existing", Status: "active", CreatedBy: "u1", Metadata: datatypes.JSON([]byte("{}")),
	})
	// Item to move with same slug+type
	database.DB.Create(&models.CapabilityItem{
		ID: "item-sc-move", RegistryID: "reg-sc-src", RepoID: "repo-sc", Slug: "dup-slug", ItemType: "command",
		Name: "To Move", Status: "active", CreatedBy: "u1", Metadata: datatypes.JSON([]byte("{}")),
	})

	// Same slug but different type — should succeed (uniqueness is repo+type+slug)
	database.DB.Create(&models.RepoMember{ID: "mem-sc", RepoID: "repo-sc", UserID: "u1", Role: "owner"})
	w := putJSON(newItemRouter("u1"), "/api/items/item-sc-move/move", map[string]interface{}{
		"targetRegistryId": "reg-sc-tgt",
	})
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 (different type, no conflict), got %d: %s", w.Code, w.Body.String())
	}
}

func TestMoveItem_SlugConflict_SameType(t *testing.T) {
	defer setupTestDB(t)()
	// Two repos: source and target both under the same target repo for conflict
	database.DB.Create(&models.Repository{ID: "repo-sc2-src", Name: "sc2-src-repo", OwnerID: "u1"})
	database.DB.Create(&models.Repository{ID: "repo-sc2-tgt", Name: "sc2-tgt-repo", OwnerID: "u1"})
	database.DB.Create(&models.CapabilityRegistry{
		ID: "reg-sc2-src", Name: "sc2-src", SourceType: "internal", RepoID: "repo-sc2-src", OwnerID: "u1",
	})
	database.DB.Create(&models.CapabilityRegistry{
		ID: "reg-sc2-tgt", Name: "sc2-tgt", SourceType: "internal", RepoID: "repo-sc2-tgt", OwnerID: "u1",
	})
	// Existing item in target repo with slug "dup-slug" and type "skill"
	database.DB.Create(&models.CapabilityItem{
		ID: "item-sc2-existing", RegistryID: "reg-sc2-tgt", RepoID: "repo-sc2-tgt", Slug: "dup-slug", ItemType: "skill",
		Name: "Existing", Status: "active", CreatedBy: "u1", Metadata: datatypes.JSON([]byte("{}")),
	})
	// Item to move: in source repo with same slug+type (different repo, so no DB constraint violation)
	database.DB.Create(&models.CapabilityItem{
		ID: "item-sc2-move", RegistryID: "reg-sc2-src", RepoID: "repo-sc2-src", Slug: "dup-slug", ItemType: "skill",
		Name: "To Move", Status: "active", CreatedBy: "u1", Metadata: datatypes.JSON([]byte("{}")),
	})
	database.DB.Create(&models.RepoMember{ID: "mem-sc2", RepoID: "repo-sc2-tgt", UserID: "u1", Role: "owner"})

	w := putJSON(newItemRouter("u1"), "/api/items/item-sc2-move/move", map[string]interface{}{
		"targetRegistryId": "reg-sc2-tgt",
	})
	if w.Code != http.StatusConflict {
		t.Fatalf("expected 409, got %d: %s", w.Code, w.Body.String())
	}
}

func TestMoveItem_NotTargetRepoMember(t *testing.T) {
	defer setupTestDB(t)()
	database.DB.Create(&models.Repository{ID: "repo-nm-src", Name: "nm-src", OwnerID: "u1"})
	database.DB.Create(&models.Repository{ID: "repo-nm-tgt", Name: "nm-tgt", OwnerID: "u2"})
	database.DB.Create(&models.CapabilityRegistry{
		ID: "reg-nm-src", Name: "nm-src-reg", SourceType: "internal", RepoID: "repo-nm-src", OwnerID: "u1",
	})
	database.DB.Create(&models.CapabilityRegistry{
		ID: "reg-nm-tgt", Name: "nm-tgt-reg", SourceType: "internal", RepoID: "repo-nm-tgt", OwnerID: "u2",
	})
	database.DB.Create(&models.CapabilityItem{
		ID: "item-nm", RegistryID: "reg-nm-src", RepoID: "repo-nm-src", Slug: "nm-item", ItemType: "skill",
		Name: "NM Item", Status: "active", CreatedBy: "u1", Metadata: datatypes.JSON([]byte("{}")),
	})
	// u1 is NOT a member of repo-nm-tgt

	w := putJSON(newItemRouter("u1"), "/api/items/item-nm/move", map[string]interface{}{
		"targetRegistryId": "reg-nm-tgt",
	})
	if w.Code != http.StatusForbidden {
		t.Fatalf("expected 403, got %d", w.Code)
	}
}

// ---------------------------------------------------------------------------
// TransferItemToRepo
// ---------------------------------------------------------------------------

func TestTransferItemToRepo_Success(t *testing.T) {
	defer setupTestDB(t)()
	database.DB.Create(&models.Repository{ID: "repo-tr-src", Name: "tr-src", OwnerID: "u1"})
	database.DB.Create(&models.Repository{ID: "repo-tr-tgt", Name: "tr-tgt", OwnerID: "u1"})
	database.DB.Create(&models.CapabilityRegistry{
		ID: "reg-tr-src", Name: "tr-src-reg", SourceType: "internal", RepoID: "repo-tr-src", OwnerID: "u1",
	})
	database.DB.Create(&models.CapabilityRegistry{
		ID: "reg-tr-tgt", Name: "tr-tgt-reg", SourceType: "internal", RepoID: "repo-tr-tgt", OwnerID: "u1",
	})
	database.DB.Create(&models.RepoMember{ID: "mem-tr1", RepoID: "repo-tr-tgt", UserID: "u1", Role: "member"})
	database.DB.Create(&models.CapabilityItem{
		ID: "item-tr1", RegistryID: "reg-tr-src", RepoID: "repo-tr-src", Slug: "tr-skill", ItemType: "skill",
		Name: "Transfer Skill", Status: "active", CreatedBy: "u1", Metadata: datatypes.JSON([]byte("{}")),
	})

	w := putJSON(newItemRouter("u1"), "/api/items/item-tr1/transfer", map[string]interface{}{
		"targetRepoId": "repo-tr-tgt",
	})
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	var item map[string]interface{}
	json.NewDecoder(w.Body).Decode(&item)
	if item["registryId"] != "reg-tr-tgt" {
		t.Fatalf("expected registryId=reg-tr-tgt, got %v", item["registryId"])
	}
	if item["repoId"] != "repo-tr-tgt" {
		t.Fatalf("expected repoId=repo-tr-tgt, got %v", item["repoId"])
	}

	// Verify in DB
	var dbItem models.CapabilityItem
	database.DB.First(&dbItem, "id = ?", "item-tr1")
	if dbItem.RegistryID != "reg-tr-tgt" {
		t.Fatalf("DB: expected registryId=reg-tr-tgt, got %s", dbItem.RegistryID)
	}
	if dbItem.RepoID != "repo-tr-tgt" {
		t.Fatalf("DB: expected repoId=repo-tr-tgt, got %s", dbItem.RepoID)
	}
}

func TestTransferItemToRepo_ToPublic_UpdatesRepoID(t *testing.T) {
	defer setupTestDB(t)()
	createPublicRegistry(t)
	database.DB.Create(&models.Repository{ID: "repo-tp", Name: "tp-repo", OwnerID: "u1"})
	database.DB.Create(&models.CapabilityRegistry{
		ID: "reg-tp", Name: "tp-reg", SourceType: "internal", RepoID: "repo-tp", OwnerID: "u1",
	})
	database.DB.Create(&models.CapabilityItem{
		ID: "item-tp", RegistryID: "reg-tp", RepoID: "repo-tp", Slug: "tp-skill", ItemType: "skill",
		Name: "To Public", Status: "active", CreatedBy: "u1", Metadata: datatypes.JSON([]byte("{}")),
	})

	w := putJSON(newItemRouter("u1"), "/api/items/item-tp/transfer", map[string]interface{}{
		"targetRepoId": "public",
	})
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	var item map[string]interface{}
	json.NewDecoder(w.Body).Decode(&item)
	if item["registryId"] != PublicRegistryID {
		t.Fatalf("expected registryId=%s, got %v", PublicRegistryID, item["registryId"])
	}
	// This is the key assertion for Bug 1 fix: repo_id must be updated to "public"
	if item["repoId"] != "public" {
		t.Fatalf("expected repoId=public in response, got %v", item["repoId"])
	}

	// Verify in DB that repo_id is actually "public"
	var dbItem models.CapabilityItem
	database.DB.First(&dbItem, "id = ?", "item-tp")
	if dbItem.RepoID != "public" {
		t.Fatalf("DB: expected repoId=public, got %s", dbItem.RepoID)
	}
	if dbItem.RegistryID != PublicRegistryID {
		t.Fatalf("DB: expected registryId=%s, got %s", PublicRegistryID, dbItem.RegistryID)
	}
}

func TestTransferItemToRepo_ToPublic_AlreadyPublic(t *testing.T) {
	defer setupTestDB(t)()
	createPublicRegistry(t)
	database.DB.Create(&models.CapabilityItem{
		ID: "item-already-pub", RegistryID: PublicRegistryID, RepoID: "public", Slug: "already-pub", ItemType: "skill",
		Name: "Already Public", Status: "active", CreatedBy: "u1", Metadata: datatypes.JSON([]byte("{}")),
	})

	w := putJSON(newItemRouter("u1"), "/api/items/item-already-pub/transfer", map[string]interface{}{
		"targetRepoId": "public",
	})
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", w.Code)
	}
}

func TestTransferItemToRepo_SameRepo(t *testing.T) {
	defer setupTestDB(t)()
	database.DB.Create(&models.Repository{ID: "repo-same-tr", Name: "same-tr", OwnerID: "u1"})
	database.DB.Create(&models.CapabilityRegistry{
		ID: "reg-same-tr", Name: "same-tr-reg", SourceType: "internal", RepoID: "repo-same-tr", OwnerID: "u1",
	})
	database.DB.Create(&models.CapabilityItem{
		ID: "item-same-tr", RegistryID: "reg-same-tr", RepoID: "repo-same-tr", Slug: "same-tr", ItemType: "skill",
		Name: "Same Repo", Status: "active", CreatedBy: "u1", Metadata: datatypes.JSON([]byte("{}")),
	})

	w := putJSON(newItemRouter("u1"), "/api/items/item-same-tr/transfer", map[string]interface{}{
		"targetRepoId": "repo-same-tr",
	})
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", w.Code)
	}
}

func TestTransferItemToRepo_SyncTypeRepo(t *testing.T) {
	defer setupTestDB(t)()
	database.DB.Create(&models.Repository{ID: "repo-sync-src", Name: "sync-src", OwnerID: "u1"})
	database.DB.Create(&models.Repository{ID: "repo-sync-tgt", Name: "sync-tgt", OwnerID: "u1", RepoType: "sync"})
	database.DB.Create(&models.CapabilityRegistry{
		ID: "reg-sync-src", Name: "sync-src-reg", SourceType: "internal", RepoID: "repo-sync-src", OwnerID: "u1",
	})
	database.DB.Create(&models.RepoMember{ID: "mem-sync", RepoID: "repo-sync-tgt", UserID: "u1", Role: "member"})
	database.DB.Create(&models.CapabilityItem{
		ID: "item-sync", RegistryID: "reg-sync-src", RepoID: "repo-sync-src", Slug: "sync-item", ItemType: "skill",
		Name: "Sync Item", Status: "active", CreatedBy: "u1", Metadata: datatypes.JSON([]byte("{}")),
	})

	w := putJSON(newItemRouter("u1"), "/api/items/item-sync/transfer", map[string]interface{}{
		"targetRepoId": "repo-sync-tgt",
	})
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", w.Code)
	}
}

func TestTransferItemToRepo_NotTargetRepoMember(t *testing.T) {
	defer setupTestDB(t)()
	database.DB.Create(&models.Repository{ID: "repo-tnm-src", Name: "tnm-src", OwnerID: "u1"})
	database.DB.Create(&models.Repository{ID: "repo-tnm-tgt", Name: "tnm-tgt", OwnerID: "u2"})
	database.DB.Create(&models.CapabilityRegistry{
		ID: "reg-tnm-src", Name: "tnm-src-reg", SourceType: "internal", RepoID: "repo-tnm-src", OwnerID: "u1",
	})
	database.DB.Create(&models.CapabilityItem{
		ID: "item-tnm", RegistryID: "reg-tnm-src", RepoID: "repo-tnm-src", Slug: "tnm-item", ItemType: "skill",
		Name: "TNM Item", Status: "active", CreatedBy: "u1", Metadata: datatypes.JSON([]byte("{}")),
	})
	// u1 is NOT a member of repo-tnm-tgt

	w := putJSON(newItemRouter("u1"), "/api/items/item-tnm/transfer", map[string]interface{}{
		"targetRepoId": "repo-tnm-tgt",
	})
	if w.Code != http.StatusForbidden {
		t.Fatalf("expected 403, got %d", w.Code)
	}
}

func TestTransferItemToRepo_NoInternalRegistry(t *testing.T) {
	defer setupTestDB(t)()
	database.DB.Create(&models.Repository{ID: "repo-nir-src", Name: "nir-src", OwnerID: "u1"})
	database.DB.Create(&models.Repository{ID: "repo-nir-tgt", Name: "nir-tgt", OwnerID: "u1"})
	database.DB.Create(&models.CapabilityRegistry{
		ID: "reg-nir-src", Name: "nir-src-reg", SourceType: "internal", RepoID: "repo-nir-src", OwnerID: "u1",
	})
	// Target repo has NO internal registry (only external)
	database.DB.Create(&models.CapabilityRegistry{
		ID: "reg-nir-ext", Name: "nir-ext-reg", SourceType: "external", RepoID: "repo-nir-tgt", OwnerID: "u1",
	})
	database.DB.Create(&models.RepoMember{ID: "mem-nir", RepoID: "repo-nir-tgt", UserID: "u1", Role: "member"})
	database.DB.Create(&models.CapabilityItem{
		ID: "item-nir", RegistryID: "reg-nir-src", RepoID: "repo-nir-src", Slug: "nir-item", ItemType: "skill",
		Name: "NIR Item", Status: "active", CreatedBy: "u1", Metadata: datatypes.JSON([]byte("{}")),
	})

	w := putJSON(newItemRouter("u1"), "/api/items/item-nir/transfer", map[string]interface{}{
		"targetRepoId": "repo-nir-tgt",
	})
	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", w.Code)
	}
}

func TestTransferItemToRepo_SlugConflict(t *testing.T) {
	defer setupTestDB(t)()
	database.DB.Create(&models.Repository{ID: "repo-tsc-src", Name: "tsc-src", OwnerID: "u1"})
	database.DB.Create(&models.Repository{ID: "repo-tsc-tgt", Name: "tsc-tgt", OwnerID: "u1"})
	database.DB.Create(&models.CapabilityRegistry{
		ID: "reg-tsc-src", Name: "tsc-src-reg", SourceType: "internal", RepoID: "repo-tsc-src", OwnerID: "u1",
	})
	database.DB.Create(&models.CapabilityRegistry{
		ID: "reg-tsc-tgt", Name: "tsc-tgt-reg", SourceType: "internal", RepoID: "repo-tsc-tgt", OwnerID: "u1",
	})
	database.DB.Create(&models.RepoMember{ID: "mem-tsc", RepoID: "repo-tsc-tgt", UserID: "u1", Role: "member"})
	// Existing item in target repo
	database.DB.Create(&models.CapabilityItem{
		ID: "item-tsc-existing", RegistryID: "reg-tsc-tgt", RepoID: "repo-tsc-tgt", Slug: "conflict-slug", ItemType: "skill",
		Name: "Existing", Status: "active", CreatedBy: "u1", Metadata: datatypes.JSON([]byte("{}")),
	})
	// Item to transfer with same slug+type
	database.DB.Create(&models.CapabilityItem{
		ID: "item-tsc-move", RegistryID: "reg-tsc-src", RepoID: "repo-tsc-src", Slug: "conflict-slug", ItemType: "skill",
		Name: "To Transfer", Status: "active", CreatedBy: "u1", Metadata: datatypes.JSON([]byte("{}")),
	})

	w := putJSON(newItemRouter("u1"), "/api/items/item-tsc-move/transfer", map[string]interface{}{
		"targetRepoId": "repo-tsc-tgt",
	})
	if w.Code != http.StatusConflict {
		t.Fatalf("expected 409, got %d", w.Code)
	}
}

// ---------------------------------------------------------------------------
// Transfer + ListMyItems visibility
// ---------------------------------------------------------------------------

func TestTransferItemToPublic_ThenVisibleInMyItems(t *testing.T) {
	defer setupTestDB(t)()
	createPublicRegistry(t)

	const userID = "u-tr-pub"

	// User's own repo and registry.
	database.DB.Create(&models.Repository{ID: "repo-trp", Name: "trp-repo", OwnerID: userID})
	database.DB.Create(&models.CapabilityRegistry{
		ID: "reg-trp", Name: "trp-reg", SourceType: "internal", RepoID: "repo-trp", OwnerID: userID,
	})
	// Command in user's own repo.
	database.DB.Create(&models.CapabilityItem{
		ID: "item-trp-cmd", RegistryID: "reg-trp", RepoID: "repo-trp", Slug: "trp-cmd", ItemType: "command",
		Name: "My Command", Status: "active", CreatedBy: userID, Metadata: datatypes.JSON([]byte("{}")),
	})

	// Step 1: Transfer the item to public.
	w := putJSON(newItemRouter(userID), "/api/items/item-trp-cmd/transfer", map[string]interface{}{
		"targetRepoId": "public",
	})
	if w.Code != http.StatusOK {
		t.Fatalf("transfer expected 200, got %d: %s", w.Code, w.Body.String())
	}

	// Verify DB state after transfer.
	var dbItem models.CapabilityItem
	database.DB.First(&dbItem, "id = ?", "item-trp-cmd")
	if dbItem.RegistryID != PublicRegistryID {
		t.Fatalf("DB: expected registryId=%s, got %s", PublicRegistryID, dbItem.RegistryID)
	}
	if dbItem.RepoID != "public" {
		t.Fatalf("DB: expected repoId=public, got %s", dbItem.RepoID)
	}
	if dbItem.CreatedBy != userID {
		t.Fatalf("DB: created_by should remain %s, got %s", userID, dbItem.CreatedBy)
	}

	// Step 2: /items/my should still return this item (via created_by match).
	w = get(newRegistryRouter(userID), "/api/items/my?type=command")
	if w.Code != http.StatusOK {
		t.Fatalf("/items/my expected 200, got %d", w.Code)
	}
	var body map[string]interface{}
	json.NewDecoder(w.Body).Decode(&body)
	items := body["items"].([]interface{})
	if len(items) != 1 {
		t.Fatalf("expected 1 item in /items/my after transfer to public, got %d", len(items))
	}
	first := items[0].(map[string]interface{})
	if first["id"] != "item-trp-cmd" {
		t.Fatalf("expected item-trp-cmd, got %v", first["id"])
	}
}

func TestTransferItemFromPublicToRepo_ThenVisibleInMyItems(t *testing.T) {
	defer setupTestDB(t)()
	createPublicRegistry(t)

	const userID = "u-tr-frompub"

	// User's target repo and registry.
	database.DB.Create(&models.Repository{ID: "repo-tfp", Name: "tfp-repo", OwnerID: userID})
	database.DB.Create(&models.CapabilityRegistry{
		ID: "reg-tfp", Name: "tfp-reg", SourceType: "internal", RepoID: "repo-tfp", OwnerID: userID,
	})
	database.DB.Create(&models.RepoMember{ID: "mem-tfp", RepoID: "repo-tfp", UserID: userID, Role: "owner"})
	// Command originally in the public registry, created by this user.
	database.DB.Create(&models.CapabilityItem{
		ID: "item-tfp-cmd", RegistryID: PublicRegistryID, RepoID: "public", Slug: "tfp-cmd", ItemType: "command",
		Name: "Public Command", Status: "active", CreatedBy: userID, Metadata: datatypes.JSON([]byte("{}")),
	})

	// Step 1: Transfer the item from public to user's repo.
	w := putJSON(newItemRouter(userID), "/api/items/item-tfp-cmd/transfer", map[string]interface{}{
		"targetRepoId": "repo-tfp",
	})
	if w.Code != http.StatusOK {
		t.Fatalf("transfer expected 200, got %d: %s", w.Code, w.Body.String())
	}

	// Verify DB state after transfer.
	var dbItem models.CapabilityItem
	database.DB.First(&dbItem, "id = ?", "item-tfp-cmd")
	if dbItem.RegistryID != "reg-tfp" {
		t.Fatalf("DB: expected registryId=reg-tfp, got %s", dbItem.RegistryID)
	}
	if dbItem.RepoID != "repo-tfp" {
		t.Fatalf("DB: expected repoId=repo-tfp, got %s", dbItem.RepoID)
	}
	if dbItem.CreatedBy != userID {
		t.Fatalf("DB: created_by should remain %s, got %s", userID, dbItem.CreatedBy)
	}

	// Step 2: /items/my should return this item (via both registry_id and created_by match).
	w = get(newRegistryRouter(userID), "/api/items/my?type=command")
	if w.Code != http.StatusOK {
		t.Fatalf("/items/my expected 200, got %d", w.Code)
	}
	var body map[string]interface{}
	json.NewDecoder(w.Body).Decode(&body)
	items := body["items"].([]interface{})
	if len(items) != 1 {
		t.Fatalf("expected 1 item in /items/my after transfer from public, got %d", len(items))
	}
	first := items[0].(map[string]interface{})
	if first["id"] != "item-tfp-cmd" {
		t.Fatalf("expected item-tfp-cmd, got %v", first["id"])
	}
	if first["repoId"] != "repo-tfp" {
		t.Fatalf("expected repoId=repo-tfp, got %v", first["repoId"])
	}
	if first["repoName"] != "tfp-repo" {
		t.Fatalf("expected repoName=tfp-repo, got %v", first["repoName"])
	}
}

// ensure fmt is used
var _ = fmt.Sprintf
