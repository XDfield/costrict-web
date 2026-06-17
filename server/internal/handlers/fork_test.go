package handlers

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/costrict/costrict-web/server/internal/database"
	"github.com/costrict/costrict-web/server/internal/middleware"
	"github.com/costrict/costrict-web/server/internal/models"
	"github.com/costrict/costrict-web/server/internal/services"
	"github.com/gin-gonic/gin"
	"gorm.io/datatypes"
)

func newForkRouter(userID string) *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	ScanJobService = nil
	injectUser := func(c *gin.Context) {
		if userID != "" {
			c.Set(middleware.UserIDKey, userID)
		}
		c.Next()
	}
	db := database.GetDB()
	tagSvc := &services.TagService{DB: db}
	TagSvc = tagSvc
	itemHandler := NewItemHandler(db, &services.ParserService{}, nil, tagSvc)
	r.POST("/api/items/:id/fork", injectUser, itemHandler.ForkItem)
	r.GET("/api/items/:id", injectUser, GetItem)
	return r
}

type forkTestResp struct {
	ID                string  `json:"id"`
	RegistryID        string  `json:"registryId"`
	Slug              string  `json:"slug"`
	Content           string  `json:"content"`
	CreatedBy         string  `json:"createdBy"`
	SourceType        string  `json:"sourceType"`
	ForkedFromItemID  *string `json:"forkedFromItemId"`
	ForkedFromOwnerID *string `json:"forkedFromOwnerId"`
	ForkCount         int     `json:"forkCount"`
	MyForkItemID      *string `json:"myForkItemId"`
}

func seedForkSourceItem(id, slug, createdBy, sourceType, repoID string) {
	database.GetDB().Create(&models.CapabilityItem{
		ID:              id,
		RegistryID:      PublicRegistryID,
		RepoID:          repoID,
		Slug:            slug,
		ItemType:        "skill",
		Name:            "Source " + slug,
		Description:     "source desc",
		Descriptions:    datatypes.JSON([]byte(`{}`)),
		Category:        "utilities",
		Version:         "1.2.0",
		Content:         "original content",
		SourceType:      sourceType,
		CreatedBy:       createdBy,
		CurrentRevision: 1,
		Status:          "active",
	})
}

func forkReq(r *gin.Engine, itemID string) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	req, _ := http.NewRequest(http.MethodPost, "/api/items/"+itemID+"/fork", nil)
	r.ServeHTTP(w, req)
	return w
}

func TestForkItem_SuccessAndDuplicate(t *testing.T) {
	defer setupTestDB(t)()
	seedForkSourceItem("src-1", "my-skill", "alice", "direct", "public")

	bobRouter := newForkRouter("bob")

	// First fork by bob → 201 with provenance + copied content.
	w := forkReq(bobRouter, "src-1")
	if w.Code != http.StatusCreated {
		t.Fatalf("fork: expected 201, got %d (%s)", w.Code, w.Body.String())
	}
	var resp forkTestResp
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.ID == "src-1" || resp.ID == "" {
		t.Fatalf("expected a new item id, got %q", resp.ID)
	}
	if resp.CreatedBy != "bob" {
		t.Errorf("createdBy: want bob, got %q", resp.CreatedBy)
	}
	if resp.RegistryID != PublicRegistryID {
		t.Errorf("registryId: want public registry, got %q", resp.RegistryID)
	}
	if resp.SourceType != "fork" {
		t.Errorf("sourceType: want fork, got %q", resp.SourceType)
	}
	if resp.Content != "original content" {
		t.Errorf("content not copied, got %q", resp.Content)
	}
	if resp.ForkedFromItemID == nil || *resp.ForkedFromItemID != "src-1" {
		t.Errorf("forkedFromItemId: want src-1, got %v", resp.ForkedFromItemID)
	}
	if resp.ForkedFromOwnerID == nil || *resp.ForkedFromOwnerID != "alice" {
		t.Errorf("forkedFromOwnerId: want alice, got %v", resp.ForkedFromOwnerID)
	}
	forkID := resp.ID

	// Second fork by bob → 200, returns same existing fork (no duplicate).
	w2 := forkReq(bobRouter, "src-1")
	if w2.Code != http.StatusOK {
		t.Fatalf("re-fork: expected 200, got %d (%s)", w2.Code, w2.Body.String())
	}
	var resp2 forkTestResp
	_ = json.Unmarshal(w2.Body.Bytes(), &resp2)
	if resp2.ID != forkID {
		t.Errorf("re-fork should return existing fork %q, got %q", forkID, resp2.ID)
	}
	var count int64
	database.GetDB().Model(&models.CapabilityItem{}).
		Where("forked_from_item_id = ? AND created_by = ?", "src-1", "bob").Count(&count)
	if count != 1 {
		t.Errorf("expected exactly 1 fork for bob, got %d", count)
	}

	// GetItem on source as alice → forkCount=1, myForkItemId nil (alice didn't fork).
	wa := httptest.NewRecorder()
	reqA, _ := http.NewRequest(http.MethodGet, "/api/items/src-1", nil)
	newForkRouter("alice").ServeHTTP(wa, reqA)
	var srcAsAlice forkTestResp
	_ = json.Unmarshal(wa.Body.Bytes(), &srcAsAlice)
	if srcAsAlice.ForkCount != 1 {
		t.Errorf("source forkCount: want 1, got %d", srcAsAlice.ForkCount)
	}
	if srcAsAlice.MyForkItemID != nil {
		t.Errorf("alice should have no fork, got %v", srcAsAlice.MyForkItemID)
	}

	// GetItem on source as bob → myForkItemId = bob's fork.
	wb := httptest.NewRecorder()
	reqB, _ := http.NewRequest(http.MethodGet, "/api/items/src-1", nil)
	newForkRouter("bob").ServeHTTP(wb, reqB)
	var srcAsBob forkTestResp
	_ = json.Unmarshal(wb.Body.Bytes(), &srcAsBob)
	if srcAsBob.MyForkItemID == nil || *srcAsBob.MyForkItemID != forkID {
		t.Errorf("bob myForkItemId: want %q, got %v", forkID, srcAsBob.MyForkItemID)
	}
}

func TestForkItem_Rejections(t *testing.T) {
	defer setupTestDB(t)()
	seedForkSourceItem("pub-1", "pub-skill", "alice", "direct", "public")
	seedForkSourceItem("arc-1", "arc-skill", "alice", "archive", "public")

	// Private repo + item.
	database.GetDB().Create(&models.Repository{
		ID: "repo-priv", Name: "priv-repo", Visibility: "private", RepoType: "normal", OwnerID: "alice",
	})
	seedForkSourceItem("prv-1", "prv-skill", "alice", "direct", "repo-priv")

	// Inactive (archived) public item — must not be forkable into an active copy.
	seedForkSourceItem("arch-1", "archived-skill", "alice", "direct", "public")
	database.GetDB().Model(&models.CapabilityItem{}).Where("id = ?", "arch-1").Update("status", "archived")

	// Unauthenticated → 401.
	if w := forkReq(newForkRouter(""), "pub-1"); w.Code != http.StatusUnauthorized {
		t.Errorf("unauth fork: want 401, got %d (%s)", w.Code, w.Body.String())
	}
	// Fork own item → 400.
	if w := forkReq(newForkRouter("alice"), "pub-1"); w.Code != http.StatusBadRequest {
		t.Errorf("fork-self: want 400, got %d (%s)", w.Code, w.Body.String())
	}
	// Fork archive item → 400.
	if w := forkReq(newForkRouter("bob"), "arc-1"); w.Code != http.StatusBadRequest {
		t.Errorf("fork-archive: want 400, got %d (%s)", w.Code, w.Body.String())
	}
	// Fork private item → 403.
	if w := forkReq(newForkRouter("bob"), "prv-1"); w.Code != http.StatusForbidden {
		t.Errorf("fork-private: want 403, got %d (%s)", w.Code, w.Body.String())
	}
	// Fork missing item → 404.
	if w := forkReq(newForkRouter("bob"), "nope"); w.Code != http.StatusNotFound {
		t.Errorf("fork-missing: want 404, got %d (%s)", w.Code, w.Body.String())
	}
	// Fork inactive (archived) item → 404 (hidden, must not be republished).
	if w := forkReq(newForkRouter("bob"), "arch-1"); w.Code != http.StatusNotFound {
		t.Errorf("fork-inactive: want 404, got %d (%s)", w.Code, w.Body.String())
	}
}
