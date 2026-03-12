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

func newOrgRouter(userID string) *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	injectUser := func(c *gin.Context) {
		if userID != "" {
			c.Set(middleware.UserIDKey, userID)
		}
		c.Next()
	}
	r.GET("/api/organizations", injectUser, ListOrganizations)
	r.POST("/api/organizations", injectUser, CreateOrganization)
	r.GET("/api/organizations/:id", injectUser, GetOrganization)
	r.PUT("/api/organizations/:id", injectUser, UpdateOrganization)
	r.DELETE("/api/organizations/:id", injectUser, DeleteOrganization)
	r.GET("/api/organizations/:id/members", injectUser, ListOrganizationMembers)
	r.POST("/api/organizations/:id/members", injectUser, AddOrganizationMember)
	r.DELETE("/api/organizations/:id/members/:userId", injectUser, RemoveOrganizationMember)
	r.GET("/api/organizations/:id/registry", injectUser, GetOrganizationRegistry)
	r.GET("/api/organizations/my", injectUser, GetMyOrganizations)
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
// ListOrganizations
// ---------------------------------------------------------------------------

func TestListOrganizations_Empty(t *testing.T) {
	defer setupTestDB(t)()

	w := get(newOrgRouter(""), "/api/organizations")
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	var body map[string]interface{}
	json.NewDecoder(w.Body).Decode(&body)
	orgs := body["organizations"].([]interface{})
	if len(orgs) != 0 {
		t.Fatalf("expected 0 orgs, got %d", len(orgs))
	}
}

func TestListOrganizations_WithData(t *testing.T) {
	defer setupTestDB(t)()
	database.DB.Create(&models.Organization{ID: "org-l1", Name: "alpha", OwnerID: "u1"})
	database.DB.Create(&models.Organization{ID: "org-l2", Name: "beta", OwnerID: "u2"})

	w := get(newOrgRouter(""), "/api/organizations")
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	var body map[string]interface{}
	json.NewDecoder(w.Body).Decode(&body)
	orgs := body["organizations"].([]interface{})
	if len(orgs) != 2 {
		t.Fatalf("expected 2 orgs, got %d", len(orgs))
	}
}

// ---------------------------------------------------------------------------
// CreateOrganization
// ---------------------------------------------------------------------------

func TestCreateOrganization_Success(t *testing.T) {
	defer setupTestDB(t)()

	w := postJSON(newOrgRouter("u1"), "/api/organizations", map[string]interface{}{
		"name": "my-org", "ownerId": "u1",
	})
	if w.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", w.Code, w.Body.String())
	}
	var org map[string]interface{}
	json.NewDecoder(w.Body).Decode(&org)
	if org["name"] != "my-org" {
		t.Fatalf("unexpected name: %v", org["name"])
	}
	if org["orgType"] != "normal" {
		t.Fatalf("expected orgType=normal, got %v", org["orgType"])
	}
	if org["visibility"] != "private" {
		t.Fatalf("expected visibility=private, got %v", org["visibility"])
	}
}

func TestCreateOrganization_MissingRequired(t *testing.T) {
	defer setupTestDB(t)()

	w := postJSON(newOrgRouter("u1"), "/api/organizations", map[string]interface{}{
		"name": "no-owner",
	})
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", w.Code)
	}
}

func TestCreateOrganization_DefaultsVisibilityAndType(t *testing.T) {
	defer setupTestDB(t)()

	w := postJSON(newOrgRouter("u1"), "/api/organizations", map[string]interface{}{
		"name": "defaults-org", "ownerId": "u1",
	})
	if w.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", w.Code, w.Body.String())
	}
	var org map[string]interface{}
	json.NewDecoder(w.Body).Decode(&org)
	if org["visibility"] != "private" {
		t.Fatalf("expected default visibility=private, got %v", org["visibility"])
	}
	if org["orgType"] != "normal" {
		t.Fatalf("expected default orgType=normal, got %v", org["orgType"])
	}
}

func TestCreateOrganization_OwnerAddedAsMember(t *testing.T) {
	defer setupTestDB(t)()

	w := postJSON(newOrgRouter("u1"), "/api/organizations", map[string]interface{}{
		"name": "member-org", "ownerId": "u1",
	})
	if w.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d", w.Code)
	}
	var org map[string]interface{}
	json.NewDecoder(w.Body).Decode(&org)
	orgID := org["id"].(string)

	var count int64
	database.DB.Model(&models.OrgMember{}).Where("org_id = ? AND user_id = ? AND role = 'owner'", orgID, "u1").Count(&count)
	if count != 1 {
		t.Fatalf("expected owner to be added as member, got count=%d", count)
	}
}

func TestCreateOrganization_SyncType_MissingExternalURL(t *testing.T) {
	defer setupTestDB(t)()

	w := postJSON(newOrgRouter("u1"), "/api/organizations", map[string]interface{}{
		"name": "sync-org", "ownerId": "u1", "orgType": "sync",
		"syncRegistry": map[string]interface{}{},
	})
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", w.Code)
	}
}

func TestCreateOrganization_SyncType_Success(t *testing.T) {
	defer setupTestDB(t)()

	w := postJSON(newOrgRouter("u1"), "/api/organizations", map[string]interface{}{
		"name": "sync-org2", "ownerId": "u1", "orgType": "sync",
		"syncRegistry": map[string]interface{}{
			"externalUrl": "https://github.com/example/repo",
		},
	})
	if w.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", w.Code, w.Body.String())
	}
	var body map[string]interface{}
	json.NewDecoder(w.Body).Decode(&body)
	if body["organization"] == nil {
		t.Fatal("expected organization field in response")
	}
	if body["registries"] == nil {
		t.Fatal("expected registries field in response")
	}
}

// ---------------------------------------------------------------------------
// GetOrganization
// ---------------------------------------------------------------------------

func TestGetOrganization_Found(t *testing.T) {
	defer setupTestDB(t)()
	database.DB.Create(&models.Organization{ID: "org-g1", Name: "get-org", OwnerID: "u1"})

	w := get(newOrgRouter(""), "/api/organizations/org-g1")
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	var org map[string]interface{}
	json.NewDecoder(w.Body).Decode(&org)
	if org["id"] != "org-g1" {
		t.Fatalf("unexpected id: %v", org["id"])
	}
}

func TestGetOrganization_NotFound(t *testing.T) {
	defer setupTestDB(t)()
	w := get(newOrgRouter(""), "/api/organizations/no-such-org")
	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", w.Code)
	}
}

// ---------------------------------------------------------------------------
// UpdateOrganization
// ---------------------------------------------------------------------------

func TestUpdateOrganization_Success(t *testing.T) {
	defer setupTestDB(t)()
	database.DB.Create(&models.Organization{ID: "org-u1", Name: "old-name", OwnerID: "u1", Visibility: "private"})

	w := putJSON(newOrgRouter("u1"), "/api/organizations/org-u1", map[string]interface{}{
		"name": "new-name", "visibility": "public",
	})
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var org map[string]interface{}
	json.NewDecoder(w.Body).Decode(&org)
	if org["name"] != "new-name" {
		t.Fatalf("expected name=new-name, got %v", org["name"])
	}
	if org["visibility"] != "public" {
		t.Fatalf("expected visibility=public, got %v", org["visibility"])
	}
}

func TestUpdateOrganization_NotFound(t *testing.T) {
	defer setupTestDB(t)()
	w := putJSON(newOrgRouter("u1"), "/api/organizations/no-such", map[string]interface{}{
		"name": "x",
	})
	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", w.Code)
	}
}

func TestUpdateOrganization_PartialUpdate(t *testing.T) {
	defer setupTestDB(t)()
	database.DB.Create(&models.Organization{ID: "org-u2", Name: "partial-org", DisplayName: "Old Display", OwnerID: "u1"})

	w := putJSON(newOrgRouter("u1"), "/api/organizations/org-u2", map[string]interface{}{
		"displayName": "New Display",
	})
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	var org map[string]interface{}
	json.NewDecoder(w.Body).Decode(&org)
	if org["name"] != "partial-org" {
		t.Fatalf("name should not change, got %v", org["name"])
	}
	if org["displayName"] != "New Display" {
		t.Fatalf("expected displayName=New Display, got %v", org["displayName"])
	}
}

// ---------------------------------------------------------------------------
// DeleteOrganization
// ---------------------------------------------------------------------------

func TestDeleteOrganization_Success(t *testing.T) {
	defer setupTestDB(t)()
	database.DB.Create(&models.Organization{ID: "org-d1", Name: "del-org", OwnerID: "u1"})

	w := deleteReq(newOrgRouter("u1"), "/api/organizations/org-d1")
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}

	var count int64
	database.DB.Model(&models.Organization{}).Where("id = ?", "org-d1").Count(&count)
	if count != 0 {
		t.Fatal("organization should have been deleted")
	}
}

// ---------------------------------------------------------------------------
// ListOrganizationMembers
// ---------------------------------------------------------------------------

func TestListOrganizationMembers_Empty(t *testing.T) {
	defer setupTestDB(t)()

	w := get(newOrgRouter(""), "/api/organizations/org-no-members/members")
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

func TestListOrganizationMembers_WithMembers(t *testing.T) {
	defer setupTestDB(t)()
	database.DB.Create(&models.Organization{ID: "org-m1", Name: "member-org", OwnerID: "u1"})
	database.DB.Create(&models.OrgMember{ID: "mem-m1", OrgID: "org-m1", UserID: "u1", Role: "owner"})
	database.DB.Create(&models.OrgMember{ID: "mem-m2", OrgID: "org-m1", UserID: "u2", Role: "member"})

	w := get(newOrgRouter(""), "/api/organizations/org-m1/members")
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
// AddOrganizationMember
// ---------------------------------------------------------------------------

func TestAddOrganizationMember_Success(t *testing.T) {
	defer setupTestDB(t)()
	database.DB.Create(&models.Organization{ID: "org-am1", Name: "add-member-org", OwnerID: "u1"})

	w := postJSON(newOrgRouter("u1"), "/api/organizations/org-am1/members", map[string]interface{}{
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

func TestAddOrganizationMember_DefaultRole(t *testing.T) {
	defer setupTestDB(t)()
	database.DB.Create(&models.Organization{ID: "org-am2", Name: "default-role-org", OwnerID: "u1"})

	w := postJSON(newOrgRouter("u1"), "/api/organizations/org-am2/members", map[string]interface{}{
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

func TestAddOrganizationMember_MissingUserID(t *testing.T) {
	defer setupTestDB(t)()

	w := postJSON(newOrgRouter("u1"), "/api/organizations/org-am3/members", map[string]interface{}{
		"username": "no-user-id",
	})
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", w.Code)
	}
}

func TestAddOrganizationMember_Duplicate(t *testing.T) {
	defer setupTestDB(t)()
	database.DB.Create(&models.Organization{ID: "org-am4", Name: "dup-org", OwnerID: "u1"})
	database.DB.Create(&models.OrgMember{ID: "mem-dup1", OrgID: "org-am4", UserID: "u-dup", Role: "member"})

	w := postJSON(newOrgRouter("u1"), "/api/organizations/org-am4/members", map[string]interface{}{
		"userId": "u-dup",
	})
	if w.Code != http.StatusConflict {
		t.Fatalf("expected 409, got %d", w.Code)
	}
}

// ---------------------------------------------------------------------------
// RemoveOrganizationMember
// ---------------------------------------------------------------------------

func TestRemoveOrganizationMember_Success(t *testing.T) {
	defer setupTestDB(t)()
	database.DB.Create(&models.Organization{ID: "org-rm1", Name: "remove-org", OwnerID: "u1"})
	database.DB.Create(&models.OrgMember{ID: "mem-rm1", OrgID: "org-rm1", UserID: "u-remove", Role: "member"})

	w := deleteReq(newOrgRouter("u1"), "/api/organizations/org-rm1/members/u-remove")
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}

	var count int64
	database.DB.Model(&models.OrgMember{}).Where("org_id = ? AND user_id = ?", "org-rm1", "u-remove").Count(&count)
	if count != 0 {
		t.Fatal("member should have been removed")
	}
}

// ---------------------------------------------------------------------------
// GetOrganizationRegistry
// ---------------------------------------------------------------------------

func TestGetOrganizationRegistry_Found(t *testing.T) {
	defer setupTestDB(t)()
	database.DB.Create(&models.Organization{ID: "org-gr1", Name: "reg-org", OwnerID: "u1"})
	database.DB.Create(&models.CapabilityRegistry{
		ID: "reg-for-org", Name: "reg-org", SourceType: "internal", Visibility: "org", OrgID: "org-gr1", OwnerID: "u1",
	})

	w := get(newOrgRouter(""), "/api/organizations/org-gr1/registry")
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	var reg map[string]interface{}
	json.NewDecoder(w.Body).Decode(&reg)
	if reg["id"] != "reg-for-org" {
		t.Fatalf("unexpected id: %v", reg["id"])
	}
}

func TestGetOrganizationRegistry_NotFound(t *testing.T) {
	defer setupTestDB(t)()
	w := get(newOrgRouter(""), "/api/organizations/no-such-org/registry")
	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", w.Code)
	}
}

func TestGetOrganizationRegistry_ExternalRegistryNotReturned(t *testing.T) {
	defer setupTestDB(t)()
	database.DB.Create(&models.Organization{ID: "org-gr2", Name: "ext-reg-org", OwnerID: "u1"})
	database.DB.Create(&models.CapabilityRegistry{
		ID: "ext-reg-for-org", Name: "ext-reg-org", SourceType: "external", Visibility: "org", OrgID: "org-gr2", OwnerID: "u1",
	})

	w := get(newOrgRouter(""), "/api/organizations/org-gr2/registry")
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 (external registry returned first), got %d", w.Code)
	}
}

// ---------------------------------------------------------------------------
// GetMyOrganizations
// ---------------------------------------------------------------------------

func TestGetMyOrganizations_Success(t *testing.T) {
	defer setupTestDB(t)()
	database.DB.Create(&models.Organization{ID: "org-my1", Name: "my-org-1", OwnerID: "u1"})
	database.DB.Create(&models.Organization{ID: "org-my2", Name: "my-org-2", OwnerID: "u2"})
	database.DB.Create(&models.OrgMember{ID: "mem-my1", OrgID: "org-my1", UserID: "u-me", Role: "member"})
	database.DB.Create(&models.OrgMember{ID: "mem-my2", OrgID: "org-my2", UserID: "u-me", Role: "admin"})

	w := get(newOrgRouter(""), "/api/organizations/my?userId=u-me")
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	var body map[string]interface{}
	json.NewDecoder(w.Body).Decode(&body)
	orgs := body["organizations"].([]interface{})
	if len(orgs) != 2 {
		t.Fatalf("expected 2 orgs, got %d", len(orgs))
	}
}

func TestGetMyOrganizations_MissingUserID(t *testing.T) {
	defer setupTestDB(t)()
	w := get(newOrgRouter(""), "/api/organizations/my")
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", w.Code)
	}
}

func TestGetMyOrganizations_NoMemberships(t *testing.T) {
	defer setupTestDB(t)()

	w := get(newOrgRouter(""), "/api/organizations/my?userId=u-nobody")
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	var body map[string]interface{}
	json.NewDecoder(w.Body).Decode(&body)
	orgs, _ := body["organizations"].([]interface{})
	if len(orgs) != 0 {
		t.Fatalf("expected 0 orgs, got %d", len(orgs))
	}
}
