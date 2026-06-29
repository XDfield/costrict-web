package deptsync

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/costrict/costrict-web/server/internal/config"
)

func TestClient_NotConfigured(t *testing.T) {
	// Missing both URL and key.
	c := New(config.DeptSyncConfig{})
	if c.Configured() {
		t.Fatal("expected client to be unconfigured with empty config")
	}
	if _, err := c.GetTree(); err != ErrNotConfigured {
		t.Fatalf("GetTree: expected ErrNotConfigured, got %v", err)
	}
	if _, err := c.GetDeptUsers("1"); err != ErrNotConfigured {
		t.Fatalf("GetDeptUsers: expected ErrNotConfigured, got %v", err)
	}
	if _, err := c.GetUserDepartments("u"); err != ErrNotConfigured {
		t.Fatalf("GetUserDepartments: expected ErrNotConfigured, got %v", err)
	}

	// URL without key is still unconfigured.
	c2 := New(config.DeptSyncConfig{BaseURL: "http://x:8080"})
	if c2.Configured() {
		t.Fatal("expected client to be unconfigured without API key")
	}
}

func TestClient_GetTree_NestedAndQueryKey(t *testing.T) {
	var gotKey, gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotKey = r.Header.Get(defaultAuthHeader)
		gotPath = r.URL.Path
		if r.URL.Path != defaultPathPrefix+"/department/tree" {
			t.Errorf("unexpected path %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"code":"0","success":true,"data":[
			{"dept_id":"49","dept_name":"深信服","dept_path":"/深信服","parent_dept_id":"","dept_level":1,"child_dept_count":1,"children":[
				{"dept_id":"6560","dept_name":"Costrict研发部","dept_path":"/深信服/Costrict研发部","parent_dept_id":"49","dept_level":2,"child_dept_count":0}
			]}
		]}`))
	}))
	defer srv.Close()

	c := New(config.DeptSyncConfig{BaseURL: srv.URL, APIKey: "secret-key"})
	tree, err := c.GetTree()
	if err != nil {
		t.Fatalf("GetTree: %v", err)
	}
	if gotKey != "secret-key" {
		t.Fatalf("expected X-Query-Key header secret-key, got %q", gotKey)
	}
	if gotPath != defaultPathPrefix+"/department/tree" {
		t.Fatalf("expected prefixed path, got %q", gotPath)
	}
	if len(tree) != 1 {
		t.Fatalf("expected 1 root, got %d", len(tree))
	}
	if tree[0].DeptID != "49" || len(tree[0].Children) != 1 {
		t.Fatalf("unexpected root node: %+v", tree[0])
	}
	if tree[0].Children[0].DeptName != "Costrict研发部" {
		t.Fatalf("unexpected child: %+v", tree[0].Children[0])
	}
}

func TestClient_GetTree_ListWrapper(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// department tree wrapped as {list,total} — exercises decodeDeptList unwrap.
		_, _ = w.Write([]byte(`{"code":"0","success":true,"data":{"total":1,"list":[
			{"dept_id":"49","dept_name":"深信服","dept_path":"/深信服","children":[
				{"dept_id":"1416","dept_name":"研发体系","dept_path":"/深信服/研发体系"}
			]}
		]}}`))
	}))
	defer srv.Close()

	c := New(config.DeptSyncConfig{BaseURL: srv.URL, APIKey: "k"})
	tree, err := c.GetTree()
	if err != nil {
		t.Fatalf("GetTree: %v", err)
	}
	if len(tree) != 1 || tree[0].DeptID != "49" || len(tree[0].Children) != 1 {
		t.Fatalf("unexpected tree from list wrapper: %+v", tree)
	}
	if tree[0].Children[0].DeptName != "研发体系" {
		t.Fatalf("unexpected nested child: %+v", tree[0].Children[0])
	}
}

func TestClient_GetDeptUsers_ListWrapper(t *testing.T) {
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		// data wrapped as {list,total} — exercises unwrapList compatibility.
		_, _ = w.Write([]byte(`{"code":"0","success":true,"data":{"total":2,"list":[
			{"user_id":"u1","username":"朱海俊","universal_id":"uid-1","is_main":1,"position":"实习生"},
			{"user_id":"u2","username":"韦体东","universal_id":"uid-2","is_main":0,"position":"研发主管"}
		]}}`))
	}))
	defer srv.Close()

	c := New(config.DeptSyncConfig{BaseURL: srv.URL, APIKey: "k"})
	users, err := c.GetDeptUsers("6571")
	if err != nil {
		t.Fatalf("GetDeptUsers: %v", err)
	}
	if gotPath != defaultPathPrefix+"/department/6571/users" {
		t.Fatalf("expected prefixed members path, got %q", gotPath)
	}
	if len(users) != 2 {
		t.Fatalf("expected 2 users, got %d", len(users))
	}
	if users[0].UniversalID != "uid-1" || users[0].IsMain != 1 {
		t.Fatalf("unexpected user[0]: %+v", users[0])
	}
}

func TestClient_GetDeptUsers_BareArray(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"code":"0","success":true,"data":[{"user_id":"u1","username":"a","universal_id":"uid-1"}]}`))
	}))
	defer srv.Close()

	c := New(config.DeptSyncConfig{BaseURL: srv.URL, APIKey: "k"})
	users, err := c.GetDeptUsers("1")
	if err != nil {
		t.Fatalf("GetDeptUsers: %v", err)
	}
	if len(users) != 1 || users[0].UserID != "u1" {
		t.Fatalf("unexpected users: %+v", users)
	}
}

func TestClient_GetDeptUsersTree(t *testing.T) {
	// Not configured → ErrNotConfigured (no fan-out to a nonexistent endpoint).
	if _, err := New(config.DeptSyncConfig{}).GetDeptUsersTree("6560"); err != ErrNotConfigured {
		t.Fatalf("unconfigured GetDeptUsersTree: expected ErrNotConfigured, got %v", err)
	}

	var gotPath, gotInclude string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotInclude = r.URL.Query().Get("include_children")
		// Subtree members across two sub-departments, snake_case payload.
		_, _ = w.Write([]byte(`{"code":"0","success":true,"data":[
			{"user_id":"u1","username":"朱海俊","universal_id":"uid-1","dept_id":"6571","dept_name":"开发组","is_main":1},
			{"user_id":"u2","username":"韦体东","universal_id":"uid-2","dept_id":"6572","dept_name":"AI Native组","is_main":1}
		]}`))
	}))
	defer srv.Close()

	c := New(config.DeptSyncConfig{BaseURL: srv.URL, APIKey: "qk"})
	users, err := c.GetDeptUsersTree("6560")
	if err != nil {
		t.Fatalf("GetDeptUsersTree: %v", err)
	}
	if gotPath != defaultPathPrefix+"/department/6560/users" {
		t.Fatalf("expected subtree members path, got %q", gotPath)
	}
	if gotInclude != "true" {
		t.Fatalf("expected include_children=true, got %q", gotInclude)
	}
	if len(users) != 2 {
		t.Fatalf("expected 2 subtree users, got %d", len(users))
	}
	if users[0].UniversalID != "uid-1" || users[0].DeptID != "6571" {
		t.Fatalf("unexpected user[0]: %+v", users[0])
	}
	if users[1].UniversalID != "uid-2" {
		t.Fatalf("unexpected user[1]: %+v", users[1])
	}
}

func TestClient_NullData(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"code":"0","success":true,"message":"empty","data":null}`))
	}))
	defer srv.Close()

	c := New(config.DeptSyncConfig{BaseURL: srv.URL, APIKey: "k"})
	tree, err := c.GetTree()
	if err != nil {
		t.Fatalf("GetTree null data: %v", err)
	}
	if len(tree) != 0 {
		t.Fatalf("expected empty tree, got %d", len(tree))
	}
}

func TestClient_UpstreamError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"code":"500","success":false,"message":"boom"}`))
	}))
	defer srv.Close()

	c := New(config.DeptSyncConfig{BaseURL: srv.URL, APIKey: "k"})
	if _, err := c.GetTree(); err == nil {
		t.Fatal("expected error on upstream 500, got nil")
	}
}

func TestClient_AppFailureFlag(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// HTTP 200 but explicit success:false (e.g. invalid query key surfaced in body).
		_, _ = w.Write([]byte(`{"code":"40001","success":false,"message":"invalid query key","data":null}`))
	}))
	defer srv.Close()

	c := New(config.DeptSyncConfig{BaseURL: srv.URL, APIKey: "k"})
	if _, err := c.GetTree(); err == nil {
		t.Fatal("expected error on success:false, got nil")
	}
}

func TestClient_Cache(t *testing.T) {
	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		_, _ = w.Write([]byte(`{"code":"0","success":true,"data":[{"dept_id":"1","dept_name":"a"}]}`))
	}))
	defer srv.Close()

	c := New(config.DeptSyncConfig{BaseURL: srv.URL, APIKey: "k", CacheTTLSec: 60})
	if _, err := c.GetTree(); err != nil {
		t.Fatalf("first GetTree: %v", err)
	}
	if _, err := c.GetTree(); err != nil {
		t.Fatalf("second GetTree: %v", err)
	}
	if calls != 1 {
		t.Fatalf("expected 1 upstream call (cached), got %d", calls)
	}

	c.InvalidateCache()
	if _, err := c.GetTree(); err != nil {
		t.Fatalf("third GetTree: %v", err)
	}
	if calls != 2 {
		t.Fatalf("expected 2 upstream calls after invalidate, got %d", calls)
	}
}

func TestClient_CustomPrefixAndHeader(t *testing.T) {
	var gotKey, gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotKey = r.Header.Get("X-Custom-Key")
		gotPath = r.URL.Path
		_, _ = w.Write([]byte(`{"code":"0","success":true,"data":[{"dept_id":"1","dept_name":"a"}]}`))
	}))
	defer srv.Close()

	c := New(config.DeptSyncConfig{
		BaseURL: srv.URL,
		APIKey:  "ck",
		// No leading slash on purpose: New() must normalize it to /custom/api.
		PathPrefix: "custom/api",
		AuthHeader: "X-Custom-Key",
	})
	if _, err := c.GetTree(); err != nil {
		t.Fatalf("GetTree: %v", err)
	}
	if gotKey != "ck" {
		t.Fatalf("expected custom header value ck, got %q", gotKey)
	}
	if gotPath != "/custom/api/department/tree" {
		t.Fatalf("expected custom-prefixed path, got %q", gotPath)
	}
}

func TestClient_GetUserDepartments(t *testing.T) {
	var gotKey, gotPath, gotType string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotKey = r.Header.Get(defaultAuthHeader)
		gotPath = r.URL.Path
		gotType = r.URL.Query().Get("type")
		_, _ = w.Write([]byte(`{"code":"0","success":true,"data":[
			{"dept_id":"6560","dept_name":"Costrict研发部","dept_path":"/研发体系/Costrict研发部","parent_dept_id":"1416","dept_level":2}
		]}`))
	}))
	defer srv.Close()

	c := New(config.DeptSyncConfig{BaseURL: srv.URL, APIKey: "qk"})
	depts, err := c.GetUserDepartments("uid-25163")
	if err != nil {
		t.Fatalf("GetUserDepartments: %v", err)
	}
	if gotPath != defaultPathPrefix+"/user/uid-25163/departments" {
		t.Fatalf("expected prefixed user-departments path, got %q", gotPath)
	}
	if gotType != "universal" {
		t.Fatalf("expected type=universal (authz passes universal_id), got %q", gotType)
	}
	if gotKey != "qk" {
		t.Fatalf("expected X-Query-Key qk, got %q", gotKey)
	}
	if len(depts) != 1 || depts[0].DeptPath != "/研发体系/Costrict研发部" {
		t.Fatalf("unexpected departments: %+v", depts)
	}
}

func TestClient_GetDepartmentPath(t *testing.T) {
	// Not configured → ErrNotConfigured.
	if _, err := New(config.DeptSyncConfig{}).GetDepartmentPath("6560"); err != ErrNotConfigured {
		t.Fatalf("unconfigured GetDepartmentPath: expected ErrNotConfigured, got %v", err)
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"code":"0","success":true,"data":[
			{"dept_id":"1416","dept_name":"研发体系","dept_path":"/研发体系","children":[
				{"dept_id":"6560","dept_name":"Costrict研发部","dept_path":"/研发体系/Costrict研发部","children":[
					{"dept_id":"6571","dept_name":"开发组","dept_path":"/研发体系/Costrict研发部/开发组"}
				]}
			]}
		]}`))
	}))
	defer srv.Close()

	c := New(config.DeptSyncConfig{BaseURL: srv.URL, APIKey: "k"})

	// Nested child resolves to its materialized path.
	path, err := c.GetDepartmentPath("6571")
	if err != nil {
		t.Fatalf("GetDepartmentPath(6571): %v", err)
	}
	if path != "/研发体系/Costrict研发部/开发组" {
		t.Fatalf("unexpected path for 6571: %q", path)
	}

	// Mid-tree node resolves too.
	path, err = c.GetDepartmentPath("6560")
	if err != nil {
		t.Fatalf("GetDepartmentPath(6560): %v", err)
	}
	if path != "/研发体系/Costrict研发部" {
		t.Fatalf("unexpected path for 6560: %q", path)
	}

	// Unknown dept id → error.
	if _, err := c.GetDepartmentPath("9999"); err == nil {
		t.Fatal("expected error for unknown department id")
	}
}
