//go:build integration

// Integration test against a REAL dept-sync service (e.g. reached via an SSH
// tunnel). It is excluded from the normal `go test` run by the `integration`
// build tag, so it never runs in CI without explicit opt-in.
//
// Usage (after `ssh -L 127.0.0.1:18080:<dept-sync-ip>:8080 user@host -p <port>`):
//
//	DEPT_SYNC_URL=http://127.0.0.1:18080 \
//	DEPT_SYNC_API_KEY=<query_key> \
//	DEPT_SYNC_TEST_UNIVERSAL_ID=<a real universal_id> \
//	go test -tags=integration -run TestIntegration -v ./internal/deptsync/
//
// Optional: DEPT_SYNC_TEST_DEPT_ID to pin the department probed for members;
// DEPT_SYNC_PATH_PREFIX / DEPT_SYNC_AUTH_HEADER to override the defaults.
package deptsync

import (
	"os"
	"testing"

	"github.com/costrict/costrict-web/server/internal/config"
)

func integrationClient(t *testing.T) *Client {
	t.Helper()
	url := os.Getenv("DEPT_SYNC_URL")
	key := os.Getenv("DEPT_SYNC_API_KEY")
	if url == "" || key == "" {
		t.Skip("set DEPT_SYNC_URL and DEPT_SYNC_API_KEY to run the integration test")
	}
	return New(config.DeptSyncConfig{
		BaseURL:    url,
		APIKey:     key,
		PathPrefix: os.Getenv("DEPT_SYNC_PATH_PREFIX"), // empty → default /costrict-dept-info/api/v1
		AuthHeader: os.Getenv("DEPT_SYNC_AUTH_HEADER"), // empty → default X-Query-Key
		TimeoutSec: 30,
	})
}

// firstLeafDeptID returns the id of the first leaf department (no children),
// walking the tree depth-first, to probe a department likely to have direct
// members; it falls back to the first node when no leaf is found.
func firstLeafDeptID(nodes []Dept) string {
	for _, n := range nodes {
		if len(n.Children) == 0 {
			return n.DeptID
		}
		if id := firstLeafDeptID(n.Children); id != "" {
			return id
		}
	}
	if len(nodes) > 0 {
		return nodes[0].DeptID
	}
	return ""
}

func TestIntegration_GetTree(t *testing.T) {
	c := integrationClient(t)
	tree, err := c.GetTree()
	if err != nil {
		t.Fatalf("GetTree: %v", err)
	}
	if len(tree) == 0 {
		t.Fatal("expected non-empty department tree from real dept-sync")
	}
	root := tree[0]
	t.Logf("root: deptId=%q deptName=%q deptPath=%q leaderId=%q childDeptCount=%d children=%d",
		root.DeptID, root.DeptName, root.DeptPath, root.LeaderID, root.ChildDeptCount, len(root.Children))
	if root.DeptID == "" || root.DeptName == "" {
		t.Errorf("snake_case decode looks wrong (empty deptId/deptName): %+v", root)
	}
	// At least one node should carry a materialized dept_path (authz relies on it).
	var sawPath bool
	var walk func([]Dept)
	walk = func(ns []Dept) {
		for _, n := range ns {
			if n.DeptPath != "" {
				sawPath = true
			}
			walk(n.Children)
		}
	}
	walk(tree)
	if !sawPath {
		t.Error("no node had a non-empty deptPath; dept_path decode likely broken")
	}
}

func TestIntegration_GetDeptUsers(t *testing.T) {
	c := integrationClient(t)
	deptID := os.Getenv("DEPT_SYNC_TEST_DEPT_ID")
	if deptID == "" {
		tree, err := c.GetTree()
		if err != nil {
			t.Fatalf("GetTree (to pick a dept): %v", err)
		}
		deptID = firstLeafDeptID(tree)
	}
	if deptID == "" {
		t.Skip("could not determine a department id to probe")
	}
	users, err := c.GetDeptUsers(deptID)
	if err != nil {
		t.Fatalf("GetDeptUsers(%s): %v", deptID, err)
	}
	t.Logf("dept %s direct members = %d", deptID, len(users))
	if len(users) > 0 {
		u := users[0]
		t.Logf("member[0]: userId=%q username=%q universalId=%q isMain=%d position=%q status=%d",
			u.UserID, u.Username, u.UniversalID, u.IsMain, u.Position, u.Status)
		if u.UserID == "" && u.Username == "" {
			t.Errorf("member decode looks wrong (empty userId/username): %+v", u)
		}
	}
}

func TestIntegration_GetUserDepartments(t *testing.T) {
	c := integrationClient(t)
	uid := os.Getenv("DEPT_SYNC_TEST_UNIVERSAL_ID")
	if uid == "" {
		t.Skip("set DEPT_SYNC_TEST_UNIVERSAL_ID to verify user→departments (type=universal)")
	}
	depts, err := c.GetUserDepartments(uid)
	if err != nil {
		t.Fatalf("GetUserDepartments(%s): %v", uid, err)
	}
	t.Logf("universal_id %s → %d departments", uid, len(depts))
	for _, d := range depts {
		t.Logf("  deptId=%q deptName=%q deptPath=%q", d.DeptID, d.DeptName, d.DeptPath)
	}
	if len(depts) == 0 {
		t.Error("got 0 departments for the given universal_id — check that ?type=universal matched (else it queried by 工号)")
	} else if depts[0].DeptPath == "" {
		t.Error("departments returned but deptPath empty — authz dept-prefix resolution would fail")
	}
}
