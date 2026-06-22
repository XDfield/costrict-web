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

func TestClient_GetTree_NestedAndAPIKey(t *testing.T) {
	var gotKey string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotKey = r.Header.Get(apiKeyHeader)
		if r.URL.Path != "/api/department/tree" {
			t.Errorf("unexpected path %s", r.URL.Path)
		}
		if r.URL.Query().Get("include_children") != "true" {
			t.Errorf("expected include_children=true, got %q", r.URL.RawQuery)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"code":0,"message":"ok","data":[
			{"deptId":"49","deptName":"深信服","deptPath":"/深信服","parentDeptId":"","deptLevel":1,"childDeptCount":1,"children":[
				{"deptId":"6560","deptName":"Costrict研发部","deptPath":"/深信服/Costrict研发部","parentDeptId":"49","deptLevel":2,"childDeptCount":0}
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
		t.Fatalf("expected X-API-Key header secret-key, got %q", gotKey)
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

func TestClient_GetDeptUsers_ListWrapper(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// data wrapped as {list,total}
		_, _ = w.Write([]byte(`{"code":0,"data":{"total":2,"list":[
			{"userId":"u1","username":"朱海俊","universalId":"uid-1","isMain":true,"position":"实习生"},
			{"userId":"u2","username":"韦体东","universalId":"uid-2","isMain":false,"position":"研发主管"}
		]}}`))
	}))
	defer srv.Close()

	c := New(config.DeptSyncConfig{BaseURL: srv.URL, APIKey: "k"})
	users, err := c.GetDeptUsers("6571")
	if err != nil {
		t.Fatalf("GetDeptUsers: %v", err)
	}
	if len(users) != 2 {
		t.Fatalf("expected 2 users, got %d", len(users))
	}
	if users[0].UniversalID != "uid-1" || !users[0].IsMain {
		t.Fatalf("unexpected user[0]: %+v", users[0])
	}
}

func TestClient_GetDeptUsers_BareArray(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"code":0,"data":[{"userId":"u1","username":"a","universalId":"uid-1"}]}`))
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

func TestClient_NullData(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"code":0,"message":"empty","data":null}`))
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
		_, _ = w.Write([]byte(`{"code":500,"message":"boom"}`))
	}))
	defer srv.Close()

	c := New(config.DeptSyncConfig{BaseURL: srv.URL, APIKey: "k"})
	if _, err := c.GetTree(); err == nil {
		t.Fatal("expected error on upstream 500, got nil")
	}
}

func TestClient_AppCodeError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// HTTP 200 but non-zero application code.
		_, _ = w.Write([]byte(`{"code":401,"message":"invalid api key","data":null}`))
	}))
	defer srv.Close()

	c := New(config.DeptSyncConfig{BaseURL: srv.URL, APIKey: "k"})
	if _, err := c.GetTree(); err == nil {
		t.Fatal("expected error on app code 401, got nil")
	}
}

func TestClient_Cache(t *testing.T) {
	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		_, _ = w.Write([]byte(`{"code":0,"data":[{"deptId":"1","deptName":"a"}]}`))
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

func TestClient_GetDepartmentPath(t *testing.T) {
	// Not configured → ErrNotConfigured.
	if _, err := New(config.DeptSyncConfig{}).GetDepartmentPath("6560"); err != ErrNotConfigured {
		t.Fatalf("unconfigured GetDepartmentPath: expected ErrNotConfigured, got %v", err)
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"code":0,"data":[
			{"deptId":"1416","deptName":"研发体系","deptPath":"/研发体系","children":[
				{"deptId":"6560","deptName":"Costrict研发部","deptPath":"/研发体系/Costrict研发部","children":[
					{"deptId":"6571","deptName":"开发组","deptPath":"/研发体系/Costrict研发部/开发组"}
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
