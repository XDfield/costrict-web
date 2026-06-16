package adminuser

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	appmiddleware "github.com/costrict/costrict-web/server/internal/middleware"
	"github.com/costrict/costrict-web/server/internal/models"
	userpkg "github.com/costrict/costrict-web/server/internal/user"
	"github.com/gin-gonic/gin"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

func setupTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{
		Logger: logger.Default.LogMode(logger.Silent),
	})
	if err != nil {
		t.Fatalf("open test db: %v", err)
	}
	sqlDB, _ := db.DB()
	sqlDB.SetMaxOpenConns(1)

	if err := db.AutoMigrate(&models.User{}); err != nil {
		t.Fatalf("migrate users: %v", err)
	}
	stmts := []string{
		`CREATE TABLE capability_items (id TEXT PRIMARY KEY DEFAULT (lower(hex(randomblob(16)))), created_by TEXT)`,
		`CREATE TABLE item_distributions (id TEXT PRIMARY KEY DEFAULT (lower(hex(randomblob(16)))), distributor_id TEXT)`,
		`CREATE TABLE item_distribution_receipts (id TEXT PRIMARY KEY DEFAULT (lower(hex(randomblob(16)))), user_id TEXT)`,
		`CREATE TABLE user_system_roles (id TEXT PRIMARY KEY, user_id TEXT, role TEXT, created_at DATETIME, deleted_at DATETIME)`,
	}
	for _, s := range stmts {
		if err := db.Exec(s).Error; err != nil {
			t.Fatalf("create table: %v", err)
		}
	}
	return db
}

func newTestModule(t *testing.T, db *gorm.DB) *Module {
	t.Helper()
	return New(userpkg.NewUserService(db))
}

func seed(t *testing.T, db *gorm.DB, subjectID, username, org, status string) {
	t.Helper()
	o := org
	u := models.User{SubjectID: subjectID, Username: username, Organization: &o, IsActive: true, Status: status}
	if err := db.Create(&u).Error; err != nil {
		t.Fatalf("seed: %v", err)
	}
}

func TestHandler_ListUsers(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db := setupTestDB(t)
	m := newTestModule(t, db)
	seed(t, db, "usr_a", "alice", "org_a", userpkg.UserStatusActive)
	seed(t, db, "usr_b", "bob", "org_a", userpkg.UserStatusBanned)
	db.Exec(`INSERT INTO user_system_roles (id, user_id, role, created_at, deleted_at) VALUES ('r1','usr_a','platform_admin',CURRENT_TIMESTAMP,NULL)`)

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodGet, "/?page=1&pageSize=20", nil)

	m.ListUsersHandler()(c)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Users []adminUserResponse `json:"users"`
		Total int64               `json:"total"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Total != 2 || len(resp.Users) != 2 {
		t.Fatalf("total=%d len=%d, want 2/2", resp.Total, len(resp.Users))
	}
	// roles batch-attached
	var alice *adminUserResponse
	for i := range resp.Users {
		if resp.Users[i].SubjectID == "usr_a" {
			alice = &resp.Users[i]
		}
	}
	if alice == nil || len(alice.Roles) != 1 || alice.Roles[0] != "platform_admin" {
		t.Fatalf("alice roles wrong: %+v", alice)
	}
}

func TestHandler_ListUsers_StatusFilter(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db := setupTestDB(t)
	m := newTestModule(t, db)
	seed(t, db, "usr_a", "alice", "org_a", userpkg.UserStatusActive)
	seed(t, db, "usr_b", "bob", "org_a", userpkg.UserStatusBanned)

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodGet, "/?status=banned", nil)
	m.ListUsersHandler()(c)

	var resp struct {
		Users []adminUserResponse `json:"users"`
		Total int64               `json:"total"`
	}
	json.Unmarshal(rec.Body.Bytes(), &resp)
	if resp.Total != 1 || resp.Users[0].Username != "bob" {
		t.Fatalf("status filter wrong: %+v", resp)
	}
}

func TestHandler_SetUserStatus_Success(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db := setupTestDB(t)
	m := newTestModule(t, db)
	seed(t, db, "usr_target", "target", "org_a", userpkg.UserStatusActive)

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Set(appmiddleware.UserIDKey, "usr_admin")
	c.Params = gin.Params{{Key: "id", Value: "usr_target"}}
	c.Request = httptest.NewRequest(http.MethodPut, "/", strings.NewReader(`{"status":"disabled"}`))
	c.Request.Header.Set("Content-Type", "application/json")

	m.SetUserStatusHandler()(c)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var status string
	db.Model(&models.User{}).Where("subject_id = ?", "usr_target").Pluck("status", &status)
	if status != "disabled" {
		t.Fatalf("persisted status = %q, want disabled", status)
	}
}

func TestHandler_SetUserStatus_SelfLockRejected(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db := setupTestDB(t)
	m := newTestModule(t, db)
	seed(t, db, "usr_admin", "admin", "org_a", userpkg.UserStatusActive)

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Set(appmiddleware.UserIDKey, "usr_admin")
	c.Params = gin.Params{{Key: "id", Value: "usr_admin"}}
	c.Request = httptest.NewRequest(http.MethodPut, "/", strings.NewReader(`{"status":"banned"}`))
	c.Request.Header.Set("Content-Type", "application/json")

	m.SetUserStatusHandler()(c)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("self-lock should be 400, got %d; body=%s", rec.Code, rec.Body.String())
	}
	var status string
	db.Model(&models.User{}).Where("subject_id = ?", "usr_admin").Pluck("status", &status)
	if status != userpkg.UserStatusActive {
		t.Fatalf("self-lock must not change status, got %q", status)
	}
}

func TestHandler_SetUserStatus_InvalidValue(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db := setupTestDB(t)
	m := newTestModule(t, db)
	seed(t, db, "usr_target", "target", "org_a", userpkg.UserStatusActive)

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Set(appmiddleware.UserIDKey, "usr_admin")
	c.Params = gin.Params{{Key: "id", Value: "usr_target"}}
	c.Request = httptest.NewRequest(http.MethodPut, "/", strings.NewReader(`{"status":"nope"}`))
	c.Request.Header.Set("Content-Type", "application/json")

	m.SetUserStatusHandler()(c)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("invalid status should be 400, got %d", rec.Code)
	}
}

func TestHandler_ListOrganizations(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db := setupTestDB(t)
	m := newTestModule(t, db)
	seed(t, db, "usr_1", "u1", "org_big", userpkg.UserStatusActive)
	seed(t, db, "usr_2", "u2", "org_big", userpkg.UserStatusActive)
	seed(t, db, "usr_3", "u3", "org_small", userpkg.UserStatusActive)

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodGet, "/", nil)
	m.ListOrganizationsHandler()(c)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	var resp struct {
		Organizations []userpkg.OrganizationCount `json:"organizations"`
	}
	json.Unmarshal(rec.Body.Bytes(), &resp)
	if len(resp.Organizations) != 2 || resp.Organizations[0].Organization != "org_big" || resp.Organizations[0].MemberCount != 2 {
		t.Fatalf("org rollup wrong: %+v", resp.Organizations)
	}
}

func TestHandler_GetUserProfile(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db := setupTestDB(t)
	m := newTestModule(t, db)
	seed(t, db, "usr_p", "profileuser", "org_a", userpkg.UserStatusActive)
	db.Exec(`INSERT INTO capability_items (created_by) VALUES ('usr_p'), ('usr_p')`)
	db.Exec(`INSERT INTO item_distributions (distributor_id) VALUES ('usr_p')`)
	db.Exec(`INSERT INTO item_distribution_receipts (user_id) VALUES ('usr_p'), ('usr_p'), ('usr_p')`)

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Params = gin.Params{{Key: "id", Value: "usr_p"}}
	c.Request = httptest.NewRequest(http.MethodGet, "/", nil)
	m.GetUserProfileHandler()(c)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var resp struct {
		User    adminUserResponse   `json:"user"`
		Profile userpkg.UserProfile `json:"profile"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.User.SubjectID != "usr_p" {
		t.Fatalf("user wrong: %+v", resp.User)
	}
	if resp.Profile.CreatedItemCount != 2 || resp.Profile.DistributedCount != 1 || resp.Profile.ReceivedCount != 3 {
		t.Fatalf("profile aggregation wrong: %+v", resp.Profile)
	}
}

func TestHandler_GetUserProfile_NotFound(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db := setupTestDB(t)
	m := newTestModule(t, db)

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Params = gin.Params{{Key: "id", Value: "usr_ghost"}}
	c.Request = httptest.NewRequest(http.MethodGet, "/", nil)
	m.GetUserProfileHandler()(c)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("missing user should be 404, got %d", rec.Code)
	}
}
