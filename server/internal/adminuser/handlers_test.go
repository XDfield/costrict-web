package adminuser

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	appmiddleware "github.com/costrict/costrict-web/server/internal/middleware"
	userpkg "github.com/costrict/costrict-web/server/internal/user"
	"github.com/gin-gonic/gin"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// stubRPC is a configurable AdminUserRPC stub. Each field is a function the
// test sets; nil fields default to "service unavailable" semantics so unset
// methods fail loudly rather than silently returning zero values.
type stubRPC struct {
	configured        bool
	listUsers         func(ctx context.Context, p userpkg.AdminUserListParams) (*userpkg.AdminUserListResult, error)
	setUserStatus     func(ctx context.Context, subjectID, status, operatorID string) (*userpkg.AdminSetUserStatusResult, error)
	listOrganizations func(ctx context.Context) ([]userpkg.AdminOrganization, error)
	getUserProfile    func(ctx context.Context, subjectID string) (*userpkg.AdminUserProfile, error)
	adminUpdateProfile func(ctx context.Context, subjectID string, args userpkg.AdminUpdateProfileArgs) (*userpkg.AdminUserProfile, error)
}

func (s *stubRPC) Configured() bool { return s.configured }

func (s *stubRPC) ListUsers(ctx context.Context, p userpkg.AdminUserListParams) (*userpkg.AdminUserListResult, error) {
	if s.listUsers == nil {
		return nil, userpkg.ErrRPCUnavailable
	}
	return s.listUsers(ctx, p)
}

func (s *stubRPC) SetUserStatus(ctx context.Context, subjectID, status, operatorID string) (*userpkg.AdminSetUserStatusResult, error) {
	if s.setUserStatus == nil {
		return nil, userpkg.ErrRPCUnavailable
	}
	return s.setUserStatus(ctx, subjectID, status, operatorID)
}

func (s *stubRPC) ListOrganizations(ctx context.Context) ([]userpkg.AdminOrganization, error) {
	if s.listOrganizations == nil {
		return nil, userpkg.ErrRPCUnavailable
	}
	return s.listOrganizations(ctx)
}

func (s *stubRPC) GetUserProfile(ctx context.Context, subjectID string) (*userpkg.AdminUserProfile, error) {
	if s.getUserProfile == nil {
		return nil, userpkg.ErrRPCUnavailable
	}
	return s.getUserProfile(ctx, subjectID)
}

func (s *stubRPC) AdminUpdateProfile(ctx context.Context, subjectID string, args userpkg.AdminUpdateProfileArgs) (*userpkg.AdminUserProfile, error) {
	if s.adminUpdateProfile == nil {
		return nil, userpkg.ErrRPCUnavailable
	}
	return s.adminUpdateProfile(ctx, subjectID, args)
}

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

	stmts := []string{
		`CREATE TABLE capability_items (id TEXT PRIMARY KEY DEFAULT (lower(hex(randomblob(16)))), created_by TEXT)`,
		`CREATE TABLE item_distributions (id TEXT PRIMARY KEY DEFAULT (lower(hex(randomblob(16)))), distributor_id TEXT)`,
		`CREATE TABLE item_distribution_receipts (id TEXT PRIMARY KEY DEFAULT (lower(hex(randomblob(16)))), user_id TEXT)`,
		`CREATE TABLE user_system_roles (id TEXT, user_id TEXT, role TEXT, created_at DATETIME, deleted_at DATETIME)`,
	}
	for _, s := range stmts {
		if err := db.Exec(s).Error; err != nil {
			t.Fatalf("create table: %v", err)
		}
	}
	return db
}

func newTestModule(t *testing.T, db *gorm.DB, rpc AdminUserRPC) *Module {
	t.Helper()
	return New(rpc, userpkg.NewUserService(db))
}

func strPtr(s string) *string { return &s }

// --- ListUsersHandler ---

func TestHandler_ListUsers_HappyPath(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db := setupTestDB(t)
	db.Exec(`INSERT INTO user_system_roles (id, user_id, role, created_at, deleted_at) VALUES ('r1','usr_a','platform_admin',CURRENT_TIMESTAMP,NULL)`)

	rpc := &stubRPC{
		configured: true,
		listUsers: func(ctx context.Context, p userpkg.AdminUserListParams) (*userpkg.AdminUserListResult, error) {
			return &userpkg.AdminUserListResult{
				Users: []userpkg.AdminUser{
					{SubjectID: "usr_a", Username: "alice", Organization: strPtr("org_a"), Status: userpkg.UserStatusActive, IsActive: true, CreatedAt: "2026-01-01T00:00:00Z"},
					{SubjectID: "usr_b", Username: "bob", Organization: strPtr("org_a"), Status: userpkg.UserStatusBanned, IsActive: false, CreatedAt: "2026-01-02T00:00:00Z"},
				},
				Total: 2,
			}, nil
		},
	}
	m := newTestModule(t, db, rpc)

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

func TestHandler_ListUsers_ForwardsFilters(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db := setupTestDB(t)

	var gotParams userpkg.AdminUserListParams
	rpc := &stubRPC{
		configured: true,
		listUsers: func(ctx context.Context, p userpkg.AdminUserListParams) (*userpkg.AdminUserListResult, error) {
			gotParams = p
			return &userpkg.AdminUserListResult{Users: []userpkg.AdminUser{}, Total: 0}, nil
		},
	}
	m := newTestModule(t, db, rpc)

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodGet, "/?search=ali&organization=Eng&status=active&page=2&pageSize=10", nil)
	m.ListUsersHandler()(c)

	if gotParams.Keyword != "ali" || gotParams.Organization != "Eng" || gotParams.Status != "active" {
		t.Errorf("filters not forwarded: %+v", gotParams)
	}
	if gotParams.Page != 2 || gotParams.PageSize != 10 {
		t.Errorf("pagination not forwarded: %+v", gotParams)
	}
}

func TestHandler_ListUsers_RPCUnavailableReturns503(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db := setupTestDB(t)
	rpc := &stubRPC{configured: true} // listUsers nil → ErrRPCUnavailable
	m := newTestModule(t, db, rpc)

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodGet, "/", nil)
	m.ListUsersHandler()(c)

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", rec.Code)
	}
}

func TestHandler_ListUsers_NotConfiguredReturns503(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db := setupTestDB(t)
	rpc := &stubRPC{configured: false}
	m := newTestModule(t, db, rpc)

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodGet, "/", nil)
	m.ListUsersHandler()(c)

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", rec.Code)
	}
}

// --- SetUserStatusHandler ---

func TestHandler_SetUserStatus_Success(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db := setupTestDB(t)

	var gotSubject, gotStatus, gotOperator string
	rpc := &stubRPC{
		configured: true,
		setUserStatus: func(ctx context.Context, subjectID, status, operatorID string) (*userpkg.AdminSetUserStatusResult, error) {
			gotSubject, gotStatus, gotOperator = subjectID, status, operatorID
			return &userpkg.AdminSetUserStatusResult{FromStatus: "active", ToStatus: "disabled"}, nil
		},
	}
	m := newTestModule(t, db, rpc)

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
	if gotSubject != "usr_target" || gotStatus != "disabled" || gotOperator != "usr_admin" {
		t.Errorf("RPC args wrong: subj=%q status=%q op=%q", gotSubject, gotStatus, gotOperator)
	}
	var resp struct {
		Success    bool   `json:"success"`
		FromStatus string `json:"from_status"`
		ToStatus   string `json:"to_status"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !resp.Success || resp.FromStatus != "active" || resp.ToStatus != "disabled" {
		t.Errorf("response wrong: %+v", resp)
	}
}

func TestHandler_SetUserStatus_SelfLockRejectedAs400(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db := setupTestDB(t)
	rpc := &stubRPC{
		configured: true,
		setUserStatus: func(ctx context.Context, subjectID, status, operatorID string) (*userpkg.AdminSetUserStatusResult, error) {
			return nil, userpkg.ErrAdminUserRPCCannotChangeOwn
		},
	}
	m := newTestModule(t, db, rpc)

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
}

func TestHandler_SetUserStatus_NotFoundReturns404(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db := setupTestDB(t)
	rpc := &stubRPC{
		configured: true,
		setUserStatus: func(ctx context.Context, subjectID, status, operatorID string) (*userpkg.AdminSetUserStatusResult, error) {
			return nil, userpkg.ErrAdminUserRPCNotFound
		},
	}
	m := newTestModule(t, db, rpc)

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Set(appmiddleware.UserIDKey, "usr_admin")
	c.Params = gin.Params{{Key: "id", Value: "usr_ghost"}}
	c.Request = httptest.NewRequest(http.MethodPut, "/", strings.NewReader(`{"status":"disabled"}`))
	c.Request.Header.Set("Content-Type", "application/json")

	m.SetUserStatusHandler()(c)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("not found should be 404, got %d", rec.Code)
	}
}

func TestHandler_SetUserStatus_InvalidValueReturns400(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db := setupTestDB(t)
	rpc := &stubRPC{
		configured: true,
		setUserStatus: func(ctx context.Context, subjectID, status, operatorID string) (*userpkg.AdminSetUserStatusResult, error) {
			return nil, userpkg.ErrAdminUserRPCInvalidStatus
		},
	}
	m := newTestModule(t, db, rpc)

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

func TestHandler_SetUserStatus_RPCUnavailableReturns503(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db := setupTestDB(t)
	rpc := &stubRPC{
		configured: true,
		setUserStatus: func(ctx context.Context, subjectID, status, operatorID string) (*userpkg.AdminSetUserStatusResult, error) {
			return nil, errors.New("transport")
		},
	}
	m := newTestModule(t, db, rpc)

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Set(appmiddleware.UserIDKey, "usr_admin")
	c.Params = gin.Params{{Key: "id", Value: "usr_target"}}
	c.Request = httptest.NewRequest(http.MethodPut, "/", strings.NewReader(`{"status":"disabled"}`))
	c.Request.Header.Set("Content-Type", "application/json")

	m.SetUserStatusHandler()(c)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("non-sentinel error should be 500, got %d", rec.Code)
	}
}

func TestHandler_SetUserStatus_RequiresOperator(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db := setupTestDB(t)
	rpc := &stubRPC{configured: true}
	m := newTestModule(t, db, rpc)

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	// No UserIDKey set
	c.Params = gin.Params{{Key: "id", Value: "usr_target"}}
	c.Request = httptest.NewRequest(http.MethodPut, "/", strings.NewReader(`{"status":"disabled"}`))
	c.Request.Header.Set("Content-Type", "application/json")

	m.SetUserStatusHandler()(c)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("missing operator should be 401, got %d", rec.Code)
	}
}

// --- ListOrganizationsHandler ---

func TestHandler_ListOrganizations_HappyPath(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db := setupTestDB(t)
	rpc := &stubRPC{
		configured: true,
		listOrganizations: func(ctx context.Context) ([]userpkg.AdminOrganization, error) {
			return []userpkg.AdminOrganization{
				{Organization: "org_big", MemberCount: 2},
				{Organization: "org_small", MemberCount: 1},
			}, nil
		},
	}
	m := newTestModule(t, db, rpc)

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodGet, "/", nil)
	m.ListOrganizationsHandler()(c)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	var resp struct {
		Organizations []userpkg.AdminOrganization `json:"organizations"`
	}
	json.Unmarshal(rec.Body.Bytes(), &resp)
	if len(resp.Organizations) != 2 || resp.Organizations[0].Organization != "org_big" || resp.Organizations[0].MemberCount != 2 {
		t.Fatalf("org rollup wrong: %+v", resp.Organizations)
	}
}

func TestHandler_ListOrganizations_EmptySerializesAsArray(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db := setupTestDB(t)
	rpc := &stubRPC{
		configured: true,
		listOrganizations: func(ctx context.Context) ([]userpkg.AdminOrganization, error) {
			return nil, nil
		},
	}
	m := newTestModule(t, db, rpc)

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodGet, "/", nil)
	m.ListOrganizationsHandler()(c)

	if !strings.Contains(rec.Body.String(), `"organizations":[]`) {
		t.Fatalf("nil slice should serialize as [], got: %s", rec.Body.String())
	}
}

// --- GetUserProfileHandler ---

func TestHandler_GetUserProfile_CombinesIdentityAndActivity(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db := setupTestDB(t)
	db.Exec(`INSERT INTO capability_items (created_by) VALUES ('usr_p'), ('usr_p')`)
	db.Exec(`INSERT INTO item_distributions (distributor_id) VALUES ('usr_p')`)
	db.Exec(`INSERT INTO item_distribution_receipts (user_id) VALUES ('usr_p'), ('usr_p'), ('usr_p')`)
	db.Exec(`INSERT INTO user_system_roles (id, user_id, role, created_at, deleted_at) VALUES ('r1','usr_p','platform_admin',CURRENT_TIMESTAMP,NULL)`)

	rpc := &stubRPC{
		configured: true,
		getUserProfile: func(ctx context.Context, subjectID string) (*userpkg.AdminUserProfile, error) {
			return &userpkg.AdminUserProfile{
				SubjectID:    "usr_p",
				Username:     "profileuser",
				Organization: strPtr("org_a"),
				Status:       userpkg.UserStatusActive,
				IsActive:     true,
				CreatedAt:    "2026-01-01T00:00:00Z",
			}, nil
		},
	}
	m := newTestModule(t, db, rpc)

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
	if resp.User.SubjectID != "usr_p" || resp.User.Username != "profileuser" {
		t.Fatalf("identity slice wrong: %+v", resp.User)
	}
	if len(resp.User.Roles) != 1 || resp.User.Roles[0] != "platform_admin" {
		t.Fatalf("roles wrong: %+v", resp.User.Roles)
	}
	if resp.Profile.CreatedItemCount != 2 || resp.Profile.DistributedCount != 1 || resp.Profile.ReceivedCount != 3 {
		t.Fatalf("activity aggregation wrong: %+v", resp.Profile)
	}
}

func TestHandler_GetUserProfile_NotFoundReturns404(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db := setupTestDB(t)
	rpc := &stubRPC{
		configured: true,
		getUserProfile: func(ctx context.Context, subjectID string) (*userpkg.AdminUserProfile, error) {
			return nil, userpkg.ErrAdminUserRPCNotFound
		},
	}
	m := newTestModule(t, db, rpc)

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Params = gin.Params{{Key: "id", Value: "usr_ghost"}}
	c.Request = httptest.NewRequest(http.MethodGet, "/", nil)
	m.GetUserProfileHandler()(c)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("missing user should be 404, got %d", rec.Code)
	}
}

func TestHandler_GetUserProfile_RPCUnavailableReturns503(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db := setupTestDB(t)
	rpc := &stubRPC{
		configured: true,
		getUserProfile: func(ctx context.Context, subjectID string) (*userpkg.AdminUserProfile, error) {
			return nil, userpkg.ErrRPCUnavailable
		},
	}
	m := newTestModule(t, db, rpc)

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Params = gin.Params{{Key: "id", Value: "usr_p"}}
	c.Request = httptest.NewRequest(http.MethodGet, "/", nil)
	m.GetUserProfileHandler()(c)

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("RPC unavailable should be 503, got %d", rec.Code)
	}
}

// Sanity: compile-time assertion that *userpkg.RPCClient satisfies our
// AdminUserRPC interface. If Commit 6's RPCClient ever drifts, this fails the
// build rather than a runtime.
var _ AdminUserRPC = (*userpkg.RPCClient)(nil)
