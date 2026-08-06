package handlers

import (
	"bytes"
	"encoding/json"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/costrict/costrict-web/server/internal/database"
	"github.com/costrict/costrict-web/server/internal/middleware"
	"github.com/costrict/costrict-web/server/internal/models"
	"github.com/gin-gonic/gin"
)

func registryValidationContext(uid string) *gin.Context {
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Set(middleware.UserIDKey, uid)
	return c
}

func TestValidateCreateRegistryMatrix(t *testing.T) {
	defer setupTestDB(t)()
	db := database.GetDB()
	db.Create(&models.Repository{ID: "repo-private", Name: "repo-private", OwnerID: "owner", RepoType: "normal"})
	db.Create(&models.Repository{ID: "repo-sync", Name: "repo-sync", OwnerID: "owner", RepoType: "sync"})
	db.Create(&models.Repository{ID: "repo-external", Name: "repo-external", OwnerID: "owner", RepoType: "normal"})
	db.Create(&models.CapabilityRegistry{ID: "reg-private", Name: "private", RepoID: "repo-private", OwnerID: "owner", SourceType: "internal"})
	db.Create(&models.CapabilityRegistry{ID: "reg-sync", Name: "sync", RepoID: "repo-sync", OwnerID: "owner", SourceType: "internal"})
	db.Create(&models.CapabilityRegistry{ID: "reg-external", Name: "external", RepoID: "repo-external", OwnerID: "owner", SourceType: "external"})
	db.Create(&models.RepoMember{ID: "member-1", RepoID: "repo-private", UserID: "member"})
	cases := []struct {
		name, uid, reg string
		wantStatus     int
		wantOK         bool
	}{
		{"public", "stranger", PublicRegistryID, http.StatusCreated, true},
		{"member", "member", "reg-private", http.StatusCreated, true},
		{"non-member", "stranger", "reg-private", http.StatusForbidden, false},
		{"owner", "owner", "reg-private", http.StatusCreated, true},
		{"platform-admin", "admin", "reg-private", http.StatusCreated, true},
		{"unknown", "owner", "missing", http.StatusNotFound, false},
		{"external", "owner", "reg-external", http.StatusBadRequest, false},
		{"sync", "owner", "reg-sync", http.StatusBadRequest, false},
	}
	grantPlatformAdmin(t, "admin")
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			status, _, ok := validateCreateRegistry(registryValidationContext(tc.uid), db, tc.reg)
			if status != tc.wantStatus || ok != tc.wantOK {
				t.Fatalf("got (%d,%v), want (%d,%v)", status, ok, tc.wantStatus, tc.wantOK)
			}
		})
	}
}

func TestCreateItemFromJSONRejectsRegistryBeforePersistence(t *testing.T) {
	defer setupTestDB(t)()
	db := database.GetDB()
	db.Create(&models.Repository{ID: "repo-private", Name: "repo-private", OwnerID: "owner", RepoType: "normal"})
	db.Create(&models.CapabilityRegistry{ID: "reg-private", Name: "private", RepoID: "repo-private", OwnerID: "owner", SourceType: "internal"})
	h := NewItemHandler(db, nil, nil, nil)
	r := gin.New()
	r.POST("/", func(c *gin.Context) { c.Set(middleware.UserIDKey, "stranger"); h.createItemFromJSON(c) })
	body, _ := json.Marshal(map[string]any{"registryId": "reg-private", "itemType": "skill", "name": "x", "content": "x"})
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(w, req)
	if w.Code != http.StatusForbidden {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
}

func TestCreateItemFromArchiveRejectsRegistryBeforeParse(t *testing.T) {
	defer setupTestDB(t)()
	db := database.GetDB()
	db.Create(&models.Repository{ID: "repo-private", Name: "repo-private", OwnerID: "owner", RepoType: "normal"})
	db.Create(&models.CapabilityRegistry{ID: "reg-private", Name: "private", RepoID: "repo-private", OwnerID: "owner", SourceType: "internal"})
	h := NewItemHandler(db, nil, nil, nil)
	r := gin.New()
	r.POST("/", func(c *gin.Context) { c.Set(middleware.UserIDKey, "stranger"); h.createItemFromArchive(c) })
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	part, _ := mw.CreateFormFile("file", "x.zip")
	part.Write([]byte("not parsed"))
	mw.WriteField("itemType", "skill")
	mw.WriteField("registryId", "reg-private")
	mw.Close()
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/", &buf)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	r.ServeHTTP(w, req)
	if w.Code != http.StatusForbidden {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
}
