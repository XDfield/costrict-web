// Tests for the admin backfill handler (POST /api/admin/users/provision/backfill).
//
// Exercises the RPC pagination loop, param clamping, error mapping, and the
// happy path that drives BackfillMissingBindings end-to-end against a real
// UserProvisionService backed by sqlite + a recording fake Gitea.

package handlers

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/costrict/costrict-web/server/internal/gitserver"
	"github.com/costrict/costrict-web/server/internal/gitsync"
	userpkg "github.com/costrict/costrict-web/server/internal/user"
	"github.com/gin-gonic/gin"
	"go.uber.org/zap"
	"gorm.io/gorm"
)

// stubListUsersRPC is a canned AdminUserListRPC for backfill handler tests.
type stubListUsersRPC struct {
	configured bool
	pages      [][]userpkg.AdminUser // pages[i] is returned on the (i+1)-th call
	pageSize   int
	calls      int
	err        error // if non-nil, every ListUsers call returns this
}

func (s *stubListUsersRPC) Configured() bool { return s.configured }

func (s *stubListUsersRPC) ListUsers(ctx context.Context, p userpkg.AdminUserListParams) (*userpkg.AdminUserListResult, error) {
	s.calls++
	if s.err != nil {
		return nil, s.err
	}
	if p.Page < 1 || p.Page > len(s.pages) {
		return &userpkg.AdminUserListResult{Users: []userpkg.AdminUser{}, Total: 0, Page: p.Page, Size: p.PageSize}, nil
	}
	return &userpkg.AdminUserListResult{
		Users: s.pages[p.Page-1],
		Total: int64(len(s.pages) * s.pageSize),
		Page:  p.Page,
		Size:  p.PageSize,
	}, nil
}

func setupBackfillRouter(t *testing.T, rpc AdminUserListRPC) *gin.Engine {
	t.Helper()
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.POST("/api/admin/users/provision/backfill", BackfillGitBindingsHandler(rpc))
	return r
}

func doBackfill(t *testing.T, r *gin.Engine, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/api/admin/users/provision/backfill", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

func TestBackfillHandler_ServiceUnavailableWhenNotWired(t *testing.T) {
	// Ensure no provision service is registered for this test.
	InitUserProvisionService(nil)
	rpc := &stubListUsersRPC{configured: true}

	w := doBackfill(t, setupBackfillRouter(t, rpc), `{}`)

	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503 (body=%s)", w.Code, w.Body.String())
	}
}

func TestBackfillHandler_RPCUnavailableReturns503(t *testing.T) {
	// Register a provision service so we get past the first 503 gate.
	db := setupUserEventLogDB(t)
	provSvc := newRealProvisionService(t, db)
	InitUserProvisionService(provSvc)
	t.Cleanup(func() { InitUserProvisionService(nil) })

	rpc := &stubListUsersRPC{configured: false}
	w := doBackfill(t, setupBackfillRouter(t, rpc), `{}`)
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503 (RPC not configured)", w.Code)
	}
}

func TestBackfillHandler_PaginatesAndProvisions(t *testing.T) {
	db := setupUserEventLogDB(t)
	seedTenantForProvision(t, db, "t1", "https://g.example", "tok")
	gitea := newRecordingGitea("tok")
	giteaSrv := httptest.NewServer(gitea)
	t.Cleanup(giteaSrv.Close)

	// Re-seed tenant pointing at the fake server's address.
	if err := db.Exec(`UPDATE git_servers SET endpoint = ? WHERE server_id = 'gs-1'`, giteaSrv.URL).Error; err != nil {
		t.Fatalf("update endpoint: %v", err)
	}

	provSvc := newRealProvisionService(t, db)
	InitUserProvisionService(provSvc)
	t.Cleanup(func() { InitUserProvisionService(nil) })

	// Two pages of 2 users each.
	rpc := &stubListUsersRPC{
		configured: true,
		pageSize:   2,
		pages: [][]userpkg.AdminUser{
			{
				{SubjectID: "usr-1", ShortID: "u-alice01", Username: "alice"},
				{SubjectID: "usr-2", ShortID: "u-bob02", Username: "bob"},
			},
			{
				{SubjectID: "usr-3", ShortID: "u-carol03", Username: "carol"},
				{SubjectID: "usr-4", ShortID: "", Username: "stale"}, // skipped
			},
		},
	}

	w := doBackfill(t, setupBackfillRouter(t, rpc), `{"tenant_id":"t1","page_size":2,"max_users":4}`)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body=%s)", w.Code, w.Body.String())
	}
	if rpc.calls != 2 {
		t.Errorf("ListUsers calls = %d, want 2 (pagination)", rpc.calls)
	}

	var resp struct {
		Total        int `json:"total"`
		AlreadyBound int `json:"already_bound"`
		Provisioned  int `json:"provisioned"`
		Skipped      int `json:"skipped"`
		Failed       int `json:"failed"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v (body=%s)", err, w.Body.String())
	}
	if resp.Total != 4 || resp.Provisioned != 3 || resp.Skipped != 1 || resp.Failed != 0 {
		t.Errorf("response = %+v, want Total=4 Provisioned=3 Skipped=1 Failed=0", resp)
	}
}

func TestBackfillHandler_ClampsMaxUsers(t *testing.T) {
	db := setupUserEventLogDB(t)
	seedTenantForProvision(t, db, "t1", "https://g.example", "tok")
	gitea := newRecordingGitea("tok")
	giteaSrv := httptest.NewServer(gitea)
	t.Cleanup(giteaSrv.Close)
	if err := db.Exec(`UPDATE git_servers SET endpoint = ? WHERE server_id = 'gs-1'`, giteaSrv.URL).Error; err != nil {
		t.Fatalf("update endpoint: %v", err)
	}

	provSvc := newRealProvisionService(t, db)
	InitUserProvisionService(provSvc)
	t.Cleanup(func() { InitUserProvisionService(nil) })

	// Page of 5 users; max_users=2 should slice to 2.
	users := make([]userpkg.AdminUser, 5)
	for i := range users {
		users[i] = userpkg.AdminUser{
			SubjectID: "usr-" + string(rune('a'+i)),
			ShortID:   "u-short" + string(rune('a'+i)),
			Username:  string(rune('a' + i)),
		}
	}
	rpc := &stubListUsersRPC{configured: true, pageSize: 5, pages: [][]userpkg.AdminUser{users}}

	w := doBackfill(t, setupBackfillRouter(t, rpc), `{"tenant_id":"t1","page_size":5,"max_users":2}`)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body=%s)", w.Code, w.Body.String())
	}
	var resp struct {
		Total       int `json:"total"`
		Provisioned int `json:"provisioned"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Total != 2 || resp.Provisioned != 2 {
		t.Errorf("response = %+v, want Total=2 Provisioned=2 (clamped)", resp)
	}
}

// newRealProvisionService builds a real UserProvisionService against the
// provided DB (mirrors the phase3 e2e setup).
func newRealProvisionService(t *testing.T, db *gorm.DB) *gitsync.UserProvisionService {
	t.Helper()
	resolver := gitserver.NewDBResolver(db)
	return gitsync.NewUserProvisionService(db, resolver, zap.NewNop())
}
