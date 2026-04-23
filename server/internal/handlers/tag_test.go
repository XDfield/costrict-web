package handlers

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/costrict/costrict-web/server/internal/database"
	"github.com/costrict/costrict-web/server/internal/middleware"
	"github.com/costrict/costrict-web/server/internal/models"
	"github.com/costrict/costrict-web/server/internal/services"
	"github.com/costrict/costrict-web/server/internal/systemrole"
	"github.com/gin-gonic/gin"
)

func newTagRouter(userID string) *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	injectUser := func(c *gin.Context) {
		if userID != "" {
			c.Set(middleware.UserIDKey, userID)
		}
		c.Next()
	}
	svc := &services.TagService{DB: database.GetDB()}
	r.GET("/api/tags", injectUser, ListTagsHandler(svc))
	r.POST("/api/items/:id/tags", injectUser, SetItemTagsHandler(svc))
	admin := r.Group("/api")
	admin.Use(injectUser, systemrole.RequirePlatformAdmin(database.GetDB()))
	admin.POST("/tags", CreateTagHandler(svc))
	admin.PUT("/tags/:id", UpdateTagHandler(svc))
	admin.DELETE("/tags/:id", DeleteTagHandler(svc))
	return r
}

func seedUser(t *testing.T, subjectID string) {
	t.Helper()
	if err := database.DB.Exec(`INSERT INTO users (subject_id, username, created_at, updated_at) VALUES (?, ?, ?, ?)`, subjectID, subjectID, time.Now(), time.Now()).Error; err != nil {
		t.Fatalf("seed user: %v", err)
	}
}

func TestListTagsHandler_QueryAndPagination(t *testing.T) {
	defer setupTestDB(t)()
	for _, tag := range []models.ItemTagDict{
		{ID: "t1", Slug: "auth", TagClass: services.TagClassCustom, CreatedBy: "u1", CreatedAt: time.Now()},
		{ID: "t2", Slug: "auth-client", TagClass: services.TagClassCustom, CreatedBy: "u1", CreatedAt: time.Now()},
		{ID: "t3", Slug: "official", TagClass: services.TagClassSystem, CreatedBy: "system", CreatedAt: time.Now()},
		{ID: "t4", Slug: "planning", TagClass: services.TagClassBuiltin, CreatedBy: "system", CreatedAt: time.Now()},
	} {
		if err := database.DB.Create(&tag).Error; err != nil {
			t.Fatalf("seed tag: %v", err)
		}
	}
	w := get(newTagRouter(""), "/api/tags?q=auth&page=1&pageSize=1")
	if w.Code != 200 {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var resp map[string]interface{}
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp["total"].(float64) != 2 {
		t.Fatalf("expected total=2, got %v", resp["total"])
	}
	if resp["hasMore"] != true {
		t.Fatalf("expected hasMore=true, got %v", resp["hasMore"])
	}
	tags := resp["tags"].([]interface{})
	if len(tags) != 1 {
		t.Fatalf("expected 1 tag, got %d", len(tags))
	}
}

func TestSetItemTagsHandler_NonAdminSystemTagIsIgnored(t *testing.T) {
	defer setupTestDB(t)()
	seedUser(t, "u1")
	createPublicRegistry(t)
	if err := database.DB.Create(&models.CapabilityItem{
		ID: "item-tag-1", RegistryID: PublicRegistryID, RepoID: "public", Slug: "tag-item", ItemType: "skill", Name: "Tag Item", CreatedBy: "u1", Status: "active",
	}).Error; err != nil {
		t.Fatalf("seed item: %v", err)
	}
	if err := database.DB.Create(&models.ItemTagDict{ID: "sys1", Slug: "official", TagClass: services.TagClassSystem, CreatedBy: "system", CreatedAt: time.Now()}).Error; err != nil {
		t.Fatalf("seed system tag: %v", err)
	}
	w := postJSON(newTagRouter("u1"), "/api/items/item-tag-1/tags", map[string]interface{}{"tags": []string{"official"}})
	if w.Code != 200 {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var resp map[string]interface{}
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	tags := resp["tags"].([]interface{})
	if len(tags) != 0 {
		t.Fatalf("expected system tag to be ignored for non-admin, got %#v", tags)
	}
}

func TestCreateItemDirect_JSON_NonAdminSystemTagIsIgnored(t *testing.T) {
	defer setupTestDB(t)()
	seedUser(t, "u1")
	createPublicRegistry(t)
	if err := database.DB.Create(&models.ItemTagDict{ID: "sys1", Slug: "official", TagClass: services.TagClassSystem, CreatedBy: "system", CreatedAt: time.Now()}).Error; err != nil {
		t.Fatalf("seed system tag: %v", err)
	}
	w := postJSON(newItemRouter("u1"), "/api/items", map[string]interface{}{
		"itemType": "skill",
		"name":     "Skill With System Tag",
		"content":  "# test",
		"tags":     []string{"official"},
	})
	if w.Code != 201 {
		t.Fatalf("expected 201, got %d: %s", w.Code, w.Body.String())
	}
	var count int64
	if err := database.DB.Model(&models.CapabilityItem{}).Where("name = ?", "Skill With System Tag").Count(&count).Error; err != nil {
		t.Fatalf("count items: %v", err)
	}
	if count != 1 {
		t.Fatalf("expected created item count=1, got count=%d", count)
	}
	var item models.CapabilityItem
	if err := database.DB.Where("name = ?", "Skill With System Tag").First(&item).Error; err != nil {
		t.Fatalf("load item: %v", err)
	}
	var itemTags []models.ItemTag
	if err := database.DB.Where("item_id = ?", item.ID).Find(&itemTags).Error; err != nil {
		t.Fatalf("load item tags: %v", err)
	}
	if len(itemTags) != 0 {
		t.Fatalf("expected no tags to remain after filtering user-supplied system tag, got %d", len(itemTags))
	}
}

func TestSetItemTagsHandler_AdminCanAssignSystemTag(t *testing.T) {
	defer setupTestDB(t)()
	seedUser(t, "admin1")
	createPublicRegistry(t)
	if err := systemrole.NewSystemRoleService(database.DB).GrantRole("admin1", systemrole.SystemRolePlatformAdmin, "admin1"); err != nil {
		t.Fatalf("grant admin role: %v", err)
	}
	if err := database.DB.Create(&models.CapabilityItem{
		ID: "item-tag-2", RegistryID: PublicRegistryID, RepoID: "public", Slug: "tag-item-admin", ItemType: "skill", Name: "Tag Item Admin", CreatedBy: "admin1", Status: "active",
	}).Error; err != nil {
		t.Fatalf("seed item: %v", err)
	}
	if err := database.DB.Create(&models.ItemTagDict{ID: "sys1", Slug: "official", TagClass: services.TagClassSystem, CreatedBy: "system", CreatedAt: time.Now()}).Error; err != nil {
		t.Fatalf("seed system tag: %v", err)
	}
	w := postJSON(newTagRouter("admin1"), "/api/items/item-tag-2/tags", map[string]interface{}{"tags": []string{"official"}})
	if w.Code != 200 {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
}

func TestCreateTagHandler_RequiresPlatformAdmin(t *testing.T) {
	defer setupTestDB(t)()
	seedUser(t, "u1")
	w := postJSON(newTagRouter("u1"), "/api/tags", map[string]interface{}{"slug": "auth", "tagClass": "custom"})
	if w.Code != 403 {
		t.Fatalf("expected 403, got %d: %s", w.Code, w.Body.String())
	}
}

func TestCreateTagHandler_PlatformAdminSuccess(t *testing.T) {
	defer setupTestDB(t)()
	seedUser(t, "admin1")
	if err := systemrole.NewSystemRoleService(database.DB).GrantRole("admin1", systemrole.SystemRolePlatformAdmin, "admin1"); err != nil {
		t.Fatalf("grant admin role: %v", err)
	}
	w := postJSON(newTagRouter("admin1"), "/api/tags", map[string]interface{}{"slug": "auth", "tagClass": "custom"})
	if w.Code != 201 {
		t.Fatalf("expected 201, got %d: %s", w.Code, w.Body.String())
	}
	var count int64
	if err := database.DB.Model(&models.ItemTagDict{}).Where("slug = ?", "auth").Count(&count).Error; err != nil {
		t.Fatalf("count tags: %v", err)
	}
	if count != 1 {
		t.Fatalf("expected created tag count=1, got %d", count)
	}
}

func TestUpdateTagHandler_RequiresPlatformAdmin(t *testing.T) {
	defer setupTestDB(t)()
	seedUser(t, "u1")
	if err := database.DB.Create(&models.ItemTagDict{ID: "tag-u1", Slug: "auth", TagClass: services.TagClassCustom, CreatedBy: "system", CreatedAt: time.Now()}).Error; err != nil {
		t.Fatalf("seed tag: %v", err)
	}
	w := putJSON(newTagRouter("u1"), "/api/tags/tag-u1", map[string]interface{}{"tagClass": "system"})
	if w.Code != 403 {
		t.Fatalf("expected 403, got %d: %s", w.Code, w.Body.String())
	}
}

func TestUpdateTagHandler_PlatformAdminSuccess(t *testing.T) {
	defer setupTestDB(t)()
	seedUser(t, "admin1")
	if err := systemrole.NewSystemRoleService(database.DB).GrantRole("admin1", systemrole.SystemRolePlatformAdmin, "admin1"); err != nil {
		t.Fatalf("grant admin role: %v", err)
	}
	if err := database.DB.Create(&models.ItemTagDict{ID: "tag-u2", Slug: "auth", TagClass: services.TagClassCustom, CreatedBy: "system", CreatedAt: time.Now()}).Error; err != nil {
		t.Fatalf("seed tag: %v", err)
	}
	w := putJSON(newTagRouter("admin1"), "/api/tags/tag-u2", map[string]interface{}{"tagClass": "builtin"})
	if w.Code != 200 {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var tag models.ItemTagDict
	if err := database.DB.First(&tag, "id = ?", "tag-u2").Error; err != nil {
		t.Fatalf("load tag: %v", err)
	}
	if tag.TagClass != services.TagClassBuiltin {
		t.Fatalf("expected tagClass=builtin, got %s", tag.TagClass)
	}
}

func TestDeleteTagHandler_RequiresPlatformAdmin(t *testing.T) {
	defer setupTestDB(t)()
	seedUser(t, "u1")
	if err := database.DB.Create(&models.ItemTagDict{ID: "tag-d1", Slug: "auth", TagClass: services.TagClassCustom, CreatedBy: "system", CreatedAt: time.Now()}).Error; err != nil {
		t.Fatalf("seed tag: %v", err)
	}
	w := deleteReq(newTagRouter("u1"), "/api/tags/tag-d1")
	if w.Code != 403 {
		t.Fatalf("expected 403, got %d: %s", w.Code, w.Body.String())
	}
}

func TestDeleteTagHandler_PlatformAdminSuccess(t *testing.T) {
	defer setupTestDB(t)()
	seedUser(t, "admin1")
	if err := systemrole.NewSystemRoleService(database.DB).GrantRole("admin1", systemrole.SystemRolePlatformAdmin, "admin1"); err != nil {
		t.Fatalf("grant admin role: %v", err)
	}
	if err := database.DB.Create(&models.ItemTagDict{ID: "tag-d2", Slug: "auth", TagClass: services.TagClassCustom, CreatedBy: "system", CreatedAt: time.Now()}).Error; err != nil {
		t.Fatalf("seed tag: %v", err)
	}
	w := deleteReq(newTagRouter("admin1"), "/api/tags/tag-d2")
	if w.Code != 204 {
		t.Fatalf("expected 204, got %d: %s", w.Code, w.Body.String())
	}
	var count int64
	if err := database.DB.Model(&models.ItemTagDict{}).Where("id = ?", "tag-d2").Count(&count).Error; err != nil {
		t.Fatalf("count tags: %v", err)
	}
	if count != 0 {
		t.Fatalf("expected deleted tag count=0, got %d", count)
	}
}

func TestSetItemTagsHandler_NonAdminCanAssignBuiltinTag(t *testing.T) {
	defer setupTestDB(t)()
	seedUser(t, "u1")
	createPublicRegistry(t)
	if err := database.DB.Create(&models.CapabilityItem{
		ID: "item-tag-b1", RegistryID: PublicRegistryID, RepoID: "public", Slug: "tag-item-builtin", ItemType: "skill", Name: "Tag Item Builtin", CreatedBy: "u1", Status: "active",
	}).Error; err != nil {
		t.Fatalf("seed item: %v", err)
	}
	if err := database.DB.Create(&models.ItemTagDict{ID: "builtin1", Slug: "planning", TagClass: services.TagClassBuiltin, CreatedBy: "system", CreatedAt: time.Now()}).Error; err != nil {
		t.Fatalf("seed builtin tag: %v", err)
	}
	w := postJSON(newTagRouter("u1"), "/api/items/item-tag-b1/tags", map[string]interface{}{"tags": []string{"planning"}})
	if w.Code != 200 {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var resp map[string]interface{}
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	tags := resp["tags"].([]interface{})
	if len(tags) != 1 {
		t.Fatalf("expected builtin tag to be retained for non-admin, got %#v", tags)
	}
}
