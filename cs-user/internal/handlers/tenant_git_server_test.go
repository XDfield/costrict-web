package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/costrict/costrict-web/cs-user/internal/gitserver"
	"github.com/gin-gonic/gin"
)

// stubGitServerResolver implements TenantGitServerResolver for tests.
type stubGitServerResolver struct {
	cfg  *gitserver.Config
	err  error
	last string
}

func (s *stubGitServerResolver) Resolve(ctx context.Context, tenantID string) (*gitserver.Config, error) {
	s.last = tenantID
	if s.err != nil {
		return nil, s.err
	}
	if s.cfg != nil {
		return s.cfg, nil
	}
	return &gitserver.Config{
		ServerID:   "gs-test",
		Kind:       "gitea",
		Endpoint:   "https://gitea.test.local",
		AdminToken: "tok-test",
	}, nil
}

func newTenantGitServerRouter(resolver TenantGitServerResolver) *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	api := &TenantGitServerAPI{Svc: resolver}
	r.GET("/api/internal/tenants/:tenant_id/git-server", api.GetTenantGitServer)
	return r
}

func doTenantGitServer(t *testing.T, router *gin.Engine, tenantID string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/api/internal/tenants/"+tenantID+"/git-server", nil)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)
	return w
}

// TestGetTenantGitServer_HappyPath verifies the 200 + JSON body shape on
// a successful resolve.
func TestGetTenantGitServer_HappyPath(t *testing.T) {
	t.Parallel()
	resolver := &stubGitServerResolver{cfg: &gitserver.Config{
		ServerID:   "gs-acme",
		Kind:       "gitea",
		Endpoint:   "https://gitea.acme.com",
		AdminToken: "tok-acme-XYZ",
	}}
	router := newTenantGitServerRouter(resolver)

	w := doTenantGitServer(t, router, "t-acme")
	if w.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200; body=%s", w.Code, w.Body.String())
	}
	var resp tenantGitServerResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v; body=%s", err, w.Body.String())
	}
	if resp.ServerID != "gs-acme" {
		t.Errorf("server_id: got %q", resp.ServerID)
	}
	if resp.Endpoint != "https://gitea.acme.com" {
		t.Errorf("endpoint: got %q", resp.Endpoint)
	}
	if resp.AdminToken != "tok-acme-XYZ" {
		t.Errorf("admin_token: got %q", resp.AdminToken)
	}
	if resp.Kind != "gitea" {
		t.Errorf("kind: got %q", resp.Kind)
	}
	if resolver.last != "t-acme" {
		t.Errorf("resolver called with %q, want t-acme", resolver.last)
	}
}

// TestGetTenantGitServer_NotFound verifies 404 mapping for unknown tenants.
func TestGetTenantGitServer_NotFound(t *testing.T) {
	t.Parallel()
	resolver := &stubGitServerResolver{err: gitserver.ErrTenantNotFound}
	router := newTenantGitServerRouter(resolver)

	w := doTenantGitServer(t, router, "t-ghost")
	if w.Code != http.StatusNotFound {
		t.Fatalf("status: got %d, want 404; body=%s", w.Code, w.Body.String())
	}
}

// TestGetTenantGitServer_MissingGitServer verifies 500 mapping for the
// migration-window "tenant has no git_server_id" case.
func TestGetTenantGitServer_MissingGitServer(t *testing.T) {
	t.Parallel()
	resolver := &stubGitServerResolver{err: gitserver.ErrTenantMissingGitServer}
	router := newTenantGitServerRouter(resolver)

	w := doTenantGitServer(t, router, "t-orphan")
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status: got %d, want 500; body=%s", w.Code, w.Body.String())
	}
}

// TestGetTenantGitServer_Disabled verifies 503 mapping for the soft-disable
// drain path.
func TestGetTenantGitServer_Disabled(t *testing.T) {
	t.Parallel()
	resolver := &stubGitServerResolver{err: gitserver.ErrGitServerDisabled}
	router := newTenantGitServerRouter(resolver)

	w := doTenantGitServer(t, router, "t-drained")
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status: got %d, want 503; body=%s", w.Code, w.Body.String())
	}
}

// TestGetTenantGitServer_ConfigMalformed verifies 500 mapping for
// operator-bug malformed config.
func TestGetTenantGitServer_ConfigMalformed(t *testing.T) {
	t.Parallel()
	resolver := &stubGitServerResolver{err: gitserver.ErrConfigMalformed}
	router := newTenantGitServerRouter(resolver)

	w := doTenantGitServer(t, router, "t-bad")
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status: got %d, want 500; body=%s", w.Code, w.Body.String())
	}
}

// TestGetTenantGitServer_EmptyTenantID covers the request-validation
// guard directly. We can't easily go through the gin router (an empty
// path param is unreachable — gin would 404 before hitting the handler),
// so we drive the handler with a hand-rolled gin.Context.
func TestGetTenantGitServer_EmptyTenantID(t *testing.T) {
	t.Parallel()
	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodGet, "/api/internal/tenants//git-server", nil)
	c.Params = gin.Params{{Key: "tenant_id", Value: ""}}

	api := &TenantGitServerAPI{Svc: &stubGitServerResolver{}}
	api.GetTenantGitServer(c)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status: got %d, want 400; body=%s", w.Code, w.Body.String())
	}
}

// TestGetTenantGitServer_NilSvc verifies the service-down path returns 503
// rather than panicking.
func TestGetTenantGitServer_NilSvc(t *testing.T) {
	t.Parallel()
	api := &TenantGitServerAPI{Svc: nil}
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.GET("/api/internal/tenants/:tenant_id/git-server", api.GetTenantGitServer)

	req := httptest.NewRequest(http.MethodGet, "/api/internal/tenants/t-x/git-server", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status: got %d, want 503; body=%s", w.Code, w.Body.String())
	}
}

// TestMapTenantGitServerError_UnclassifiedErrorsDefault500 verifies the
// default branch on the error-mapping helper.
func TestMapTenantGitServerError_UnclassifiedErrorsDefault500(t *testing.T) {
	t.Parallel()
	if got := mapTenantGitServerError(errors.New("random")); got != http.StatusInternalServerError {
		t.Errorf("random err: got %d, want 500", got)
	}
}
