package deptsync

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/costrict/costrict-web/server/internal/config"
	"github.com/costrict/costrict-web/server/internal/models"
	"github.com/gin-gonic/gin"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	gormlogger "gorm.io/gorm/logger"
)

func setupTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{
		Logger: gormlogger.Default.LogMode(gormlogger.Silent),
	})
	if err != nil {
		t.Fatalf("open test db: %v", err)
	}
	sqlDB, _ := db.DB()
	sqlDB.SetMaxOpenConns(1)

	if err := db.AutoMigrate(&models.User{}); err != nil {
		t.Fatalf("migrate users: %v", err)
	}
	if err := db.Exec(`CREATE TABLE user_system_roles (id TEXT PRIMARY KEY, user_id TEXT, role TEXT, created_at DATETIME, deleted_at DATETIME)`).Error; err != nil {
		t.Fatalf("create user_system_roles: %v", err)
	}
	return db
}

func ptr(s string) *string { return &s }

func TestGetTreeHandler_NotConfigured(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db := setupTestDB(t)
	m := NewModule(New(config.DeptSyncConfig{}), db)

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodGet, "/admin/departments/tree", nil)
	m.GetTreeHandler()(c)

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503, got %d", rec.Code)
	}
	var body map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	if body["code"] != "dept_sync_unavailable" {
		t.Fatalf("expected dept_sync_unavailable code, got %v", body["code"])
	}
}

func TestGetTreeHandler_Success(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db := setupTestDB(t)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"code":0,"data":[{"deptId":"6560","deptName":"Costrict研发部","deptPath":"/x/Costrict研发部","children":[{"deptId":"6571","deptName":"开发组"}]}]}`))
	}))
	defer srv.Close()

	m := NewModule(New(config.DeptSyncConfig{BaseURL: srv.URL, APIKey: "k"}), db)

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodGet, "/admin/departments/tree", nil)
	m.GetTreeHandler()(c)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d (body=%s)", rec.Code, rec.Body.String())
	}
	var body struct {
		Departments []Dept `json:"departments"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(body.Departments) != 1 || body.Departments[0].DeptID != "6560" {
		t.Fatalf("unexpected departments: %+v", body.Departments)
	}
}

func TestGetDeptUsersHandler_Correlation(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db := setupTestDB(t)

	// Seed a local user that matches uid-1 by casdoor_universal_id, plus a role.
	if err := db.Create(&models.User{
		SubjectID:          "usr_hai",
		Username:           "haijun",
		DisplayName:        ptr("朱海俊"),
		Email:              ptr("hai@example.com"),
		Organization:       ptr("Costrict研发部"),
		Status:             "active",
		IsActive:           true,
		CasdoorUniversalID: ptr("uid-1"),
	}).Error; err != nil {
		t.Fatalf("seed user: %v", err)
	}
	db.Exec(`INSERT INTO user_system_roles (id, user_id, role, created_at, deleted_at) VALUES ('r1','usr_hai','platform_admin',CURRENT_TIMESTAMP,NULL)`)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// uid-1 matches a local user; uid-2 does not (unregistered).
		_, _ = w.Write([]byte(`{"code":0,"data":[
			{"userId":"u1","username":"朱海俊","universalId":"uid-1","isMain":true,"position":"实习生"},
			{"userId":"u2","username":"周凯","universalId":"uid-2","isMain":false,"position":"TMO"}
		]}`))
	}))
	defer srv.Close()

	m := NewModule(New(config.DeptSyncConfig{BaseURL: srv.URL, APIKey: "k"}), db)

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodGet, "/admin/departments/6571/users", nil)
	c.Params = gin.Params{{Key: "id", Value: "6571"}}
	m.GetDeptUsersHandler()(c)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d (body=%s)", rec.Code, rec.Body.String())
	}
	var body struct {
		Members []deptMemberResponse `json:"members"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(body.Members) != 2 {
		t.Fatalf("expected 2 members, got %d", len(body.Members))
	}

	first := body.Members[0]
	if !first.Registered || first.Linked == nil {
		t.Fatalf("expected first member linked/registered, got %+v", first)
	}
	if first.Linked.SubjectID != "usr_hai" {
		t.Fatalf("expected linked subject usr_hai, got %s", first.Linked.SubjectID)
	}
	if len(first.Linked.Roles) != 1 || first.Linked.Roles[0] != "platform_admin" {
		t.Fatalf("expected platform_admin role, got %v", first.Linked.Roles)
	}

	second := body.Members[1]
	if second.Registered || second.Linked != nil {
		t.Fatalf("expected second member unregistered, got %+v", second)
	}
	if second.Username != "周凯" {
		t.Fatalf("expected dept-sync username preserved, got %s", second.Username)
	}
}

func TestGetDeptUsersHandler_Unavailable(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db := setupTestDB(t)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer srv.Close()

	m := NewModule(New(config.DeptSyncConfig{BaseURL: srv.URL, APIKey: "k"}), db)

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodGet, "/admin/departments/1/users", nil)
	c.Params = gin.Params{{Key: "id", Value: "1"}}
	m.GetDeptUsersHandler()(c)

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503 on upstream failure, got %d", rec.Code)
	}
}
