package handlers

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"testing"

	userpkg "github.com/costrict/costrict-web/server/internal/user"
	"github.com/gin-gonic/gin"
)

// stubTenantResolver is a hand-rolled stand-in for *RPCClient on the
// TenantResolver interface. The closure fields let each test dial in the
// exact outcome (ok / ambiguous / not_found / error) without spinning up an
// httptest server — the suggest handler only consumes the Go-level
// TenantEmailResolution, not the wire format.
type stubTenantResolver struct {
	resolvedEmail string
	out           *userpkg.TenantEmailResolution
	err           error
}

func (s *stubTenantResolver) ResolveTenantByEmail(_ context.Context, email string) (*userpkg.TenantEmailResolution, error) {
	s.resolvedEmail = email
	return s.out, s.err
}

func TestSuggestTenant_Ok(t *testing.T) {
	stub := &stubTenantResolver{out: &userpkg.TenantEmailResolution{
		Status:   "ok",
		Slug:     "acme",
		TenantID: "t-acme",
	}}
	prev := UserModule
	defer func() { UserModule = prev }()
	UserModule = &userpkg.Module{TenantResolver: stub}

	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.GET("/api/tenants/suggest", SuggestTenant)

	w := get(r, "/api/tenants/suggest?email=alice@acme.com")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", w.Code, w.Body.String())
	}
	if stub.resolvedEmail != "alice@acme.com" {
		t.Errorf("resolver got email=%q want alice@acme.com", stub.resolvedEmail)
	}
	var body tenantSuggestResponse
	if err := json.NewDecoder(w.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.Status != "ok" || body.Slug != "acme" || body.TenantID != "t-acme" {
		t.Errorf("body = %+v, want status=ok/slug=acme/tenant_id=t-acme", body)
	}
	if len(body.Candidates) != 0 {
		t.Errorf("ok response should not include candidates, got %d", len(body.Candidates))
	}
}

func TestSuggestTenant_Ambiguous(t *testing.T) {
	stub := &stubTenantResolver{out: &userpkg.TenantEmailResolution{
		Status: "ambiguous",
		Candidates: []userpkg.TenantEmailCandidate{
			{Slug: "acme", TenantID: "t1", Name: "Acme"},
			{Slug: "acme-emea", TenantID: "t2", Name: "Acme EMEA"},
		},
	}}
	prev := UserModule
	defer func() { UserModule = prev }()
	UserModule = &userpkg.Module{TenantResolver: stub}

	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.GET("/api/tenants/suggest", SuggestTenant)

	w := get(r, "/api/tenants/suggest?email=bob@acme.com")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", w.Code, w.Body.String())
	}
	var body tenantSuggestResponse
	if err := json.NewDecoder(w.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.Status != "ambiguous" {
		t.Errorf("status = %q, want ambiguous", body.Status)
	}
	if len(body.Candidates) != 2 {
		t.Fatalf("candidates = %d, want 2", len(body.Candidates))
	}
	if body.Candidates[0].Slug != "acme" || body.Candidates[1].Name != "Acme EMEA" {
		t.Errorf("candidates = %+v", body.Candidates)
	}
}

func TestSuggestTenant_NotFound(t *testing.T) {
	stub := &stubTenantResolver{out: &userpkg.TenantEmailResolution{Status: "not_found"}}
	prev := UserModule
	defer func() { UserModule = prev }()
	UserModule = &userpkg.Module{TenantResolver: stub}

	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.GET("/api/tenants/suggest", SuggestTenant)

	w := get(r, "/api/tenants/suggest?email=carol@example.com")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", w.Code, w.Body.String())
	}
	var body tenantSuggestResponse
	if err := json.NewDecoder(w.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.Status != "not_found" {
		t.Errorf("status = %q, want not_found", body.Status)
	}
	if body.Slug != "" || body.TenantID != "" || len(body.Candidates) != 0 {
		t.Errorf("not_found should leave all fields empty, got %+v", body)
	}
}

func TestSuggestTenant_EmptyEmailReturns400(t *testing.T) {
	stub := &stubTenantResolver{}
	prev := UserModule
	defer func() { UserModule = prev }()
	UserModule = &userpkg.Module{TenantResolver: stub}

	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.GET("/api/tenants/suggest", SuggestTenant)

	w := get(r, "/api/tenants/suggest?email=%20%20")
	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400 for whitespace-only email", w.Code)
	}
	if stub.resolvedEmail != "" {
		t.Errorf("resolver should not have been called, got email=%q", stub.resolvedEmail)
	}
}

func TestSuggestTenant_LocalModeReturns503(t *testing.T) {
	prev := UserModule
	defer func() { UserModule = prev }()
	// UserModule nil OR TenantResolver nil → local mode (ADR D1).
	UserModule = &userpkg.Module{TenantResolver: nil}

	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.GET("/api/tenants/suggest", SuggestTenant)

	w := get(r, "/api/tenants/suggest?email=dave@acme.com")
	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503 in local mode", w.Code)
	}
}

func TestSuggestTenant_RPCUnavailableReturns502(t *testing.T) {
	stub := &stubTenantResolver{err: userpkg.ErrRPCUnavailable}
	prev := UserModule
	defer func() { UserModule = prev }()
	UserModule = &userpkg.Module{TenantResolver: stub}

	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.GET("/api/tenants/suggest", SuggestTenant)

	w := get(r, "/api/tenants/suggest?email=eve@acme.com")
	if w.Code != http.StatusBadGateway {
		t.Errorf("status = %d, want 502 when RPC unavailable", w.Code)
	}
}

func TestSuggestTenant_OtherErrorReturns502(t *testing.T) {
	stub := &stubTenantResolver{err: errors.New("boom")}
	prev := UserModule
	defer func() { UserModule = prev }()
	UserModule = &userpkg.Module{TenantResolver: stub}

	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.GET("/api/tenants/suggest", SuggestTenant)

	w := get(r, "/api/tenants/suggest?email=frank@acme.com")
	if w.Code != http.StatusBadGateway {
		t.Errorf("status = %d, want 502 on unexpected resolver error", w.Code)
	}
	if bytes.Contains(w.Body.Bytes(), []byte("boom")) {
		t.Errorf("body leaks internal error: %s", w.Body.String())
	}
}

func TestSuggestTenant_UnknownStatusNormalizesToNotFound(t *testing.T) {
	stub := &stubTenantResolver{out: &userpkg.TenantEmailResolution{Status: "garbage"}}
	prev := UserModule
	defer func() { UserModule = prev }()
	UserModule = &userpkg.Module{TenantResolver: stub}

	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.GET("/api/tenants/suggest", SuggestTenant)

	w := get(r, "/api/tenants/suggest?email=grace@acme.com")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", w.Code, w.Body.String())
	}
	var body tenantSuggestResponse
	if err := json.NewDecoder(w.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if body.Status != "not_found" {
		t.Errorf("status = %q, want not_found (normalized from unknown)", body.Status)
	}
}

func TestSuggestTenant_NilResponseReturns502(t *testing.T) {
	// Defensive — ResolveTenantByEmail's contract is (non-nil, nil), but a
	// future regression could return (nil, nil); the handler must not panic.
	stub := &stubTenantResolver{out: nil}
	prev := UserModule
	defer func() { UserModule = prev }()
	UserModule = &userpkg.Module{TenantResolver: stub}

	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.GET("/api/tenants/suggest", SuggestTenant)

	w := get(r, "/api/tenants/suggest?email=henry@acme.com")
	if w.Code != http.StatusBadGateway {
		t.Errorf("status = %d, want 502 on nil response", w.Code)
	}
}
