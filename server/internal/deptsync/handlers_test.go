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
		_, _ = w.Write([]byte(`{"code":"0","success":true,"data":[{"dept_id":"6560","dept_name":"Costrict研发部","dept_path":"/x/Costrict研发部","children":[{"dept_id":"6571","dept_name":"开发组"}]}]}`))
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

	// Frontend contract guard: the tree is passed through verbatim to the admin UI,
	// so the output must stay camelCase (AdminDept). Assert on raw keys so a future
	// tag regression to snake_case is caught here instead of in the browser.
	var raw map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &raw); err != nil {
		t.Fatalf("decode raw: %v", err)
	}
	depts, _ := raw["departments"].([]any)
	if len(depts) == 0 {
		t.Fatal("expected departments array in raw output")
	}
	first, _ := depts[0].(map[string]any)
	for _, k := range []string{"deptId", "deptName", "deptPath", "parentDeptId", "deptLevel", "childDeptCount", "leaderId", "orderNum"} {
		if _, ok := first[k]; !ok {
			t.Errorf("frontend contract broken: missing camelCase key %q in %v", k, first)
		}
	}
	if _, ok := first["dept_id"]; ok {
		t.Error("frontend contract broken: snake_case dept_id leaked to admin output")
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
		_, _ = w.Write([]byte(`{"code":"0","success":true,"data":[
			{"user_id":"u1","username":"朱海俊","universal_id":"uid-1","is_main":1,"position":"实习生"},
			{"user_id":"u2","username":"周凯","universal_id":"uid-2","is_main":0,"position":"TMO"}
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

// childrenTreeServer returns a dept-sync stub serving a two-level nested tree:
// 6560 → [6571 → [6572, 6573], 6580]. All children endpoints read from this
// (GetChildren slices the cached full tree, so the path is irrelevant here).
func childrenTreeServer() *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"code":"0","success":true,"data":[
			{"dept_id":"6560","dept_name":"研发部","dept_path":"/x/研发部","child_dept_count":2,"children":[
				{"dept_id":"6571","dept_name":"开发组","parent_dept_id":"6560","child_dept_count":2,"children":[
					{"dept_id":"6572","dept_name":"前端","parent_dept_id":"6571","child_dept_count":0},
					{"dept_id":"6573","dept_name":"后端","parent_dept_id":"6571","child_dept_count":0}
				]},
				{"dept_id":"6580","dept_name":"测试组","parent_dept_id":"6560","child_dept_count":0}
			]}
		]}`))
	}))
}

func TestGetChildrenHandler_Roots(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db := setupTestDB(t)
	srv := childrenTreeServer()
	defer srv.Close()

	m := NewModule(New(config.DeptSyncConfig{BaseURL: srv.URL, APIKey: "k"}), db)

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodGet, "/admin/departments/children", nil)
	m.GetChildrenHandler()(c)

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
		t.Fatalf("expected only root 6560, got %+v", body.Departments)
	}
	// Root keeps childDeptCount so the UI shows an expand affordance...
	if body.Departments[0].ChildDeptCount != 2 {
		t.Fatalf("expected root childDeptCount=2, got %d", body.Departments[0].ChildDeptCount)
	}
	// ...but its nested children are stripped (depth-1 only).
	if len(body.Departments[0].Children) != 0 {
		t.Fatalf("expected root children stripped (depth-1), got %+v", body.Departments[0].Children)
	}

	// Depth-1 contract: the "children" key must be omitted (omitempty) from the wire
	// output so lazy-load consumers never receive a preloaded level.
	var raw map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &raw); err != nil {
		t.Fatalf("decode raw: %v", err)
	}
	depts, _ := raw["departments"].([]any)
	first, _ := depts[0].(map[string]any)
	if _, ok := first["children"]; ok {
		t.Errorf("depth-1 contract broken: children key leaked into children output")
	}
	if _, ok := first["deptId"]; !ok {
		t.Errorf("frontend contract broken: missing camelCase deptId in %v", first)
	}
}

func TestGetChildrenHandler_Children(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db := setupTestDB(t)
	srv := childrenTreeServer()
	defer srv.Close()

	m := NewModule(New(config.DeptSyncConfig{BaseURL: srv.URL, APIKey: "k"}), db)

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodGet, "/admin/departments/children?parentId=6560", nil)
	m.GetChildrenHandler()(c)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d (body=%s)", rec.Code, rec.Body.String())
	}
	var body struct {
		Departments []Dept `json:"departments"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(body.Departments) != 2 {
		t.Fatalf("expected 2 direct children of 6560, got %d (%+v)", len(body.Departments), body.Departments)
	}
	if body.Departments[0].DeptID != "6571" || body.Departments[1].DeptID != "6580" {
		t.Fatalf("unexpected children order/ids: %+v", body.Departments)
	}
	// 6571 has grandchildren upstream, but they must not be serialized (childDeptCount
	// preserved, Children stripped).
	if body.Departments[0].ChildDeptCount != 2 {
		t.Fatalf("expected 6571 childDeptCount=2, got %d", body.Departments[0].ChildDeptCount)
	}
	if len(body.Departments[0].Children) != 0 {
		t.Fatalf("expected grandchildren stripped, got %+v", body.Departments[0].Children)
	}
}

func TestGetChildrenHandler_UnknownParent(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db := setupTestDB(t)
	srv := childrenTreeServer()
	defer srv.Close()

	m := NewModule(New(config.DeptSyncConfig{BaseURL: srv.URL, APIKey: "k"}), db)

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodGet, "/admin/departments/children?parentId=9999", nil)
	m.GetChildrenHandler()(c)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 for unknown parent, got %d", rec.Code)
	}
	// Unknown parent returns an empty (but present, non-null) array.
	if got := rec.Body.String(); got != `{"departments":[]}` {
		t.Fatalf("expected empty departments array, got %s", got)
	}
}

func TestGetChildrenHandler_NotConfigured(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db := setupTestDB(t)
	m := NewModule(New(config.DeptSyncConfig{}), db)

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodGet, "/admin/departments/children", nil)
	m.GetChildrenHandler()(c)

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503, got %d", rec.Code)
	}
	var body map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &body)
	if body["code"] != "dept_sync_unavailable" {
		t.Fatalf("expected dept_sync_unavailable code, got %v", body["code"])
	}
}
