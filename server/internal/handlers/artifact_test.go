package handlers

import (
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

	"github.com/costrict/costrict-web/server/internal/database"
	"github.com/costrict/costrict-web/server/internal/middleware"
	"github.com/costrict/costrict-web/server/internal/models"
	"github.com/costrict/costrict-web/server/internal/storage"
	"github.com/gin-gonic/gin"
	"gorm.io/datatypes"
)

type memBackend struct {
	mu   sync.Mutex
	data map[string][]byte
}

func newMemBackend() *memBackend {
	return &memBackend{data: make(map[string][]byte)}
}

func (m *memBackend) Put(_ context.Context, key string, r io.Reader, _ int64) error {
	b, err := io.ReadAll(r)
	if err != nil {
		return err
	}
	m.mu.Lock()
	m.data[key] = b
	m.mu.Unlock()
	return nil
}

func (m *memBackend) Get(_ context.Context, key string) (io.ReadCloser, int64, error) {
	m.mu.Lock()
	b, ok := m.data[key]
	m.mu.Unlock()
	if !ok {
		return nil, 0, fmt.Errorf("not found: %s", key)
	}
	return io.NopCloser(bytes.NewReader(b)), int64(len(b)), nil
}

func (m *memBackend) Delete(_ context.Context, key string) error {
	m.mu.Lock()
	delete(m.data, key)
	m.mu.Unlock()
	return nil
}

func (m *memBackend) PresignURL(_ context.Context, _ string, _ time.Duration) (string, error) {
	return "", nil
}

func (m *memBackend) Exists(_ context.Context, key string) (bool, error) {
	m.mu.Lock()
	_, ok := m.data[key]
	m.mu.Unlock()
	return ok, nil
}

var _ storage.Backend = (*memBackend)(nil)

func newArtifactRouter(userID string, backend storage.Backend) *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	injectUser := func(c *gin.Context) {
		if userID != "" {
			c.Set(middleware.UserIDKey, userID)
		}
		c.Next()
	}
	StorageBackend = backend
	r.POST("/api/artifacts/upload", injectUser, UploadArtifact)
	r.GET("/api/artifacts/:id/download", injectUser, DownloadArtifact)
	r.DELETE("/api/artifacts/:id", injectUser, DeleteArtifact)
	r.GET("/api/items/:id/artifacts", injectUser, ListArtifacts)
	return r
}

func setupArtifactDB(t *testing.T) func() {
	t.Helper()
	return setupTestDB(t)
}

func multipartUpload(r *gin.Engine, path string, filename, content, itemID, version string) *httptest.ResponseRecorder {
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	fw, _ := w.CreateFormFile("file", filename)
	io.Copy(fw, strings.NewReader(content))
	w.WriteField("item_id", itemID)
	w.WriteField("version", version)
	w.Close()

	rec := httptest.NewRecorder()
	req, _ := http.NewRequest(http.MethodPost, path, &buf)
	req.Header.Set("Content-Type", w.FormDataContentType())
	r.ServeHTTP(rec, req)
	return rec
}

// ---------------------------------------------------------------------------
// UploadArtifact
// ---------------------------------------------------------------------------

func TestUploadArtifact_Success(t *testing.T) {
	defer setupArtifactDB(t)()
	backend := newMemBackend()

	database.DB.Create(&models.CapabilityRegistry{
		ID: "reg-art1", Name: "art-reg", SourceType: "internal", OrgID: "org-1", OwnerID: "u1",
	})
	database.DB.Create(&models.CapabilityItem{
		ID: "item-art1", RegistryID: "reg-art1", Slug: "art-skill", ItemType: "skill",
		Name: "Art Skill", Status: "active", CreatedBy: "u1", Metadata: datatypes.JSON([]byte("{}")),
	})

	rec := multipartUpload(newArtifactRouter("u1", backend), "/api/artifacts/upload", "tool.zip", "binary content", "item-art1", "1.0.0")
	if rec.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", rec.Code, rec.Body.String())
	}
	var artifact map[string]interface{}
	json.NewDecoder(rec.Body).Decode(&artifact)
	if artifact["filename"] != "tool.zip" {
		t.Fatalf("unexpected filename: %v", artifact["filename"])
	}
	if artifact["itemId"] != "item-art1" {
		t.Fatalf("unexpected itemId: %v", artifact["itemId"])
	}
	if artifact["isLatest"] != true {
		t.Fatalf("expected isLatest=true, got %v", artifact["isLatest"])
	}
}

func TestUploadArtifact_ItemNotFound(t *testing.T) {
	defer setupArtifactDB(t)()
	backend := newMemBackend()

	rec := multipartUpload(newArtifactRouter("u1", backend), "/api/artifacts/upload", "tool.zip", "content", "no-such-item", "1.0.0")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", rec.Code)
	}
}

func TestUploadArtifact_NoFile(t *testing.T) {
	defer setupArtifactDB(t)()
	backend := newMemBackend()

	rec := httptest.NewRecorder()
	req, _ := http.NewRequest(http.MethodPost, "/api/artifacts/upload", strings.NewReader(""))
	req.Header.Set("Content-Type", "multipart/form-data; boundary=xxx")
	newArtifactRouter("u1", backend).ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", rec.Code)
	}
}

func TestUploadArtifact_PreviousIsLatestCleared(t *testing.T) {
	defer setupArtifactDB(t)()
	backend := newMemBackend()

	database.DB.Create(&models.CapabilityRegistry{
		ID: "reg-art2", Name: "art-reg2", SourceType: "internal", OrgID: "org-1", OwnerID: "u1",
	})
	database.DB.Create(&models.CapabilityItem{
		ID: "item-art2", RegistryID: "reg-art2", Slug: "art-skill2", ItemType: "skill",
		Name: "Art Skill2", Status: "active", CreatedBy: "u1", Metadata: datatypes.JSON([]byte("{}")),
	})
	database.DB.Create(&models.CapabilityArtifact{
		ID: "old-art", ItemID: "item-art2", Filename: "old.zip", FileSize: 10,
		ChecksumSHA256: "abc", StorageKey: "item-art2/v0.9.0/old.zip",
		ArtifactVersion: "0.9.0", IsLatest: true, UploadedBy: "u1", CreatedAt: time.Now(),
	})

	multipartUpload(newArtifactRouter("u1", backend), "/api/artifacts/upload", "new.zip", "new content", "item-art2", "1.0.0")

	var old models.CapabilityArtifact
	database.DB.First(&old, "id = ?", "old-art")
	if old.IsLatest {
		t.Fatal("old artifact should no longer be latest")
	}
}

func TestUploadArtifact_ChecksumSet(t *testing.T) {
	defer setupArtifactDB(t)()
	backend := newMemBackend()

	database.DB.Create(&models.CapabilityRegistry{
		ID: "reg-art3", Name: "art-reg3", SourceType: "internal", OrgID: "org-1", OwnerID: "u1",
	})
	database.DB.Create(&models.CapabilityItem{
		ID: "item-art3", RegistryID: "reg-art3", Slug: "art-skill3", ItemType: "skill",
		Name: "Art Skill3", Status: "active", CreatedBy: "u1", Metadata: datatypes.JSON([]byte("{}")),
	})

	rec := multipartUpload(newArtifactRouter("u1", backend), "/api/artifacts/upload", "data.bin", "hello", "item-art3", "1.0.0")
	if rec.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d", rec.Code)
	}
	var artifact map[string]interface{}
	json.NewDecoder(rec.Body).Decode(&artifact)
	if artifact["checksumSha256"] == "" || artifact["checksumSha256"] == nil {
		t.Fatal("expected checksumSha256 to be set")
	}
}

// ---------------------------------------------------------------------------
// DownloadArtifact
// ---------------------------------------------------------------------------

func TestDownloadArtifact_Success(t *testing.T) {
	defer setupArtifactDB(t)()
	backend := newMemBackend()
	backend.data["item-dl/v1.0.0/tool.zip"] = []byte("file content")

	database.DB.Create(&models.CapabilityArtifact{
		ID: "art-dl1", ItemID: "item-dl", Filename: "tool.zip", FileSize: 12,
		ChecksumSHA256: "abc123", StorageKey: "item-dl/v1.0.0/tool.zip",
		ArtifactVersion: "1.0.0", IsLatest: true, UploadedBy: "u1", CreatedAt: time.Now(),
	})

	rec := get(newArtifactRouter("", backend), "/api/artifacts/art-dl1/download")
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	if rec.Body.String() != "file content" {
		t.Fatalf("unexpected body: %s", rec.Body.String())
	}
	cd := rec.Header().Get("Content-Disposition")
	if cd != `attachment; filename="tool.zip"` {
		t.Fatalf("unexpected Content-Disposition: %s", cd)
	}
	if rec.Header().Get("X-Checksum-SHA256") != "abc123" {
		t.Fatalf("unexpected checksum header")
	}
}

func TestDownloadArtifact_NotFound(t *testing.T) {
	defer setupArtifactDB(t)()
	backend := newMemBackend()

	rec := get(newArtifactRouter("", backend), "/api/artifacts/no-such-art/download")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", rec.Code)
	}
}

// ---------------------------------------------------------------------------
// ListArtifacts
// ---------------------------------------------------------------------------

func TestListArtifacts_Empty(t *testing.T) {
	defer setupArtifactDB(t)()
	backend := newMemBackend()

	rec := get(newArtifactRouter("", backend), "/api/items/no-item/artifacts")
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	var body map[string]interface{}
	json.NewDecoder(rec.Body).Decode(&body)
	artifacts := body["artifacts"].([]interface{})
	if len(artifacts) != 0 {
		t.Fatalf("expected 0 artifacts, got %d", len(artifacts))
	}
}

func TestListArtifacts_WithData(t *testing.T) {
	defer setupArtifactDB(t)()
	backend := newMemBackend()

	database.DB.Create(&models.CapabilityArtifact{
		ID: "art-la1", ItemID: "item-la", Filename: "v1.zip", FileSize: 10,
		ChecksumSHA256: "sha1", StorageKey: "item-la/v1.0.0/v1.zip",
		ArtifactVersion: "1.0.0", IsLatest: false, UploadedBy: "u1", CreatedAt: time.Now(),
	})
	database.DB.Create(&models.CapabilityArtifact{
		ID: "art-la2", ItemID: "item-la", Filename: "v2.zip", FileSize: 20,
		ChecksumSHA256: "sha2", StorageKey: "item-la/v2.0.0/v2.zip",
		ArtifactVersion: "2.0.0", IsLatest: true, UploadedBy: "u1", CreatedAt: time.Now(),
	})

	rec := get(newArtifactRouter("", backend), "/api/items/item-la/artifacts")
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}
	var body map[string]interface{}
	json.NewDecoder(rec.Body).Decode(&body)
	artifacts := body["artifacts"].([]interface{})
	if len(artifacts) != 2 {
		t.Fatalf("expected 2 artifacts, got %d", len(artifacts))
	}
}

// ---------------------------------------------------------------------------
// DeleteArtifact
// ---------------------------------------------------------------------------

func TestDeleteArtifact_Success(t *testing.T) {
	defer setupArtifactDB(t)()
	backend := newMemBackend()
	backend.data["item-del/v1.0.0/del.zip"] = []byte("data")

	database.DB.Create(&models.CapabilityArtifact{
		ID: "art-del1", ItemID: "item-del", Filename: "del.zip", FileSize: 4,
		ChecksumSHA256: "abc", StorageKey: "item-del/v1.0.0/del.zip",
		ArtifactVersion: "1.0.0", IsLatest: true, UploadedBy: "u1", CreatedAt: time.Now(),
	})

	rec := deleteReq(newArtifactRouter("u1", backend), "/api/artifacts/art-del1")
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rec.Code)
	}

	var count int64
	database.DB.Model(&models.CapabilityArtifact{}).Where("id = ?", "art-del1").Count(&count)
	if count != 0 {
		t.Fatal("artifact should have been deleted from DB")
	}

	exists, _ := backend.Exists(context.Background(), "item-del/v1.0.0/del.zip")
	if exists {
		t.Fatal("artifact file should have been deleted from storage")
	}
}

func TestDeleteArtifact_NotFound(t *testing.T) {
	defer setupArtifactDB(t)()
	backend := newMemBackend()

	rec := deleteReq(newArtifactRouter("u1", backend), "/api/artifacts/no-such-art")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", rec.Code)
	}
}
