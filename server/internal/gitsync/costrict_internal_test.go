package gitsync

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

type capturedCostrictRequest struct {
	method      string
	path        string
	contentType string
	authHeader  string
	body        []byte
}

// newCostrictTestServer records every request and answers with the supplied
// status + body.
func newCostrictTestServer(t *testing.T, status int, body string, captured *[]capturedCostrictRequest) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		*captured = append(*captured, capturedCostrictRequest{
			method:      r.Method,
			path:        r.URL.Path,
			contentType: r.Header.Get("Content-Type"),
			authHeader:  r.Header.Get("X-Gitea-Internal-Auth"),
			body:        raw,
		})
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv
}

// G3(a). The fork's quota-invalidate handler binds its body through a helper
// that DISCARDS binding errors, so a request without a JSON content type
// decodes to zero rules, wipes the entire rule set and still answers
// 200 {"user_msg":"ok"}. The header is therefore not cosmetic and this test
// exists to make its removal fail loudly rather than silently disarm quotas.
func TestCostrictInternalClient_AlwaysSendsJSONContentType(t *testing.T) {
	var captured []capturedCostrictRequest
	srv := newCostrictTestServer(t, http.StatusOK, `{"user_msg":"ok"}`, &captured)

	client := NewCostrictInternalClient(srv.URL, "internal-token")
	if client == nil {
		t.Fatal("NewCostrictInternalClient returned nil for a valid endpoint + token")
	}
	if err := client.InvalidateQuotaCache(context.Background(), []QuotaRule{
		{Owner: "acme", Repo: "", MaxFileSizeMB: 20, RepoQuotaMB: 200},
	}); err != nil {
		t.Fatalf("InvalidateQuotaCache: %v", err)
	}
	// The JWKS endpoint takes no body; the header must still be present, so a
	// future refactor cannot make it conditional on there being one.
	if err := client.InvalidateJWKSCache(context.Background()); err != nil {
		t.Fatalf("InvalidateJWKSCache: %v", err)
	}

	if len(captured) != 2 {
		t.Fatalf("captured %d requests, want 2", len(captured))
	}
	for _, req := range captured {
		if req.contentType != "application/json" {
			t.Fatalf("%s %s Content-Type = %q, want application/json (a non-JSON content type silently wipes every quota rule)",
				req.method, req.path, req.contentType)
		}
		if req.method != http.MethodPost {
			t.Fatalf("method = %q, want POST", req.method)
		}
		// Not "Authorization": the fork reads only X-Gitea-Internal-Auth, and
		// the "Bearer " prefix is matched case-sensitively with exactly one
		// space.
		if req.authHeader != "Bearer internal-token" {
			t.Fatalf("X-Gitea-Internal-Auth = %q, want %q", req.authHeader, "Bearer internal-token")
		}
	}
	if captured[0].path != "/api/internal/costrict/quota-invalidate" {
		t.Fatalf("quota path = %q", captured[0].path)
	}
	if captured[1].path != "/api/internal/costrict/jwks-invalidate" {
		t.Fatalf("jwks path = %q", captured[1].path)
	}

	var payload struct {
		Rules []QuotaRule `json:"rules"`
	}
	if err := json.Unmarshal(captured[0].body, &payload); err != nil {
		t.Fatalf("decode pushed body %q: %v", captured[0].body, err)
	}
	if len(payload.Rules) != 1 || payload.Rules[0].Owner != "acme" || payload.Rules[0].MaxFileSizeMB != 20 {
		t.Fatalf("pushed rules = %+v", payload.Rules)
	}
}

// An empty snapshot is a legitimate instruction ("no per-owner overrides"), and
// must be encoded as [] rather than null so a capture can tell it apart from a
// malformed request.
func TestCostrictInternalClient_EmptySnapshotSendsEmptyArray(t *testing.T) {
	var captured []capturedCostrictRequest
	srv := newCostrictTestServer(t, http.StatusOK, `{"user_msg":"ok"}`, &captured)

	client := NewCostrictInternalClient(srv.URL, "tok")
	if err := client.InvalidateQuotaCache(context.Background(), nil); err != nil {
		t.Fatalf("InvalidateQuotaCache: %v", err)
	}
	if got, want := string(captured[0].body), `{"rules":[]}`; got != want {
		t.Fatalf("body = %s, want %s", got, want)
	}
}

// G3(c). 200 + "costrict quota disabled, no-op" means the fork threw the rules
// away because quota enforcement is off on its side. Treating that as success
// would leave a deployment believing quotas are enforced when nothing enforces
// them.
func TestCostrictInternalClient_QuotaDisabledIsNotSuccess(t *testing.T) {
	var captured []capturedCostrictRequest
	srv := newCostrictTestServer(t, http.StatusOK, `{"user_msg":"costrict quota disabled, no-op"}`, &captured)

	client := NewCostrictInternalClient(srv.URL, "tok")
	err := client.InvalidateQuotaCache(context.Background(), []QuotaRule{{Owner: "acme"}})
	if err == nil {
		t.Fatal("InvalidateQuotaCache returned nil for a quota-disabled acknowledgement")
	}
	if !errors.Is(err, ErrCostrictQuotaDisabled) {
		t.Fatalf("error = %v, want ErrCostrictQuotaDisabled", err)
	}
}

func TestCostrictInternalClient_ForkDisabledJWKSIsNotSuccess(t *testing.T) {
	var captured []capturedCostrictRequest
	srv := newCostrictTestServer(t, http.StatusOK, `{"user_msg":"costrict disabled, no-op"}`, &captured)

	client := NewCostrictInternalClient(srv.URL, "tok")
	err := client.InvalidateJWKSCache(context.Background())
	if !errors.Is(err, ErrCostrictDisabled) {
		t.Fatalf("error = %v, want ErrCostrictDisabled", err)
	}
}

// An acknowledgement we do not recognise is not evidence of acceptance.
func TestCostrictInternalClient_UnknownUserMsgIsError(t *testing.T) {
	var captured []capturedCostrictRequest
	srv := newCostrictTestServer(t, http.StatusOK, `{"user_msg":"something new"}`, &captured)

	client := NewCostrictInternalClient(srv.URL, "tok")
	err := client.InvalidateQuotaCache(context.Background(), nil)
	if !errors.Is(err, ErrCostrictUnexpectedResponse) {
		t.Fatalf("error = %v, want ErrCostrictUnexpectedResponse", err)
	}
}

func TestCostrictInternalClient_StatusMapping(t *testing.T) {
	tests := []struct {
		name   string
		status int
		body   string
		want   error
	}{
		// Wrong or missing internal token.
		{"forbidden", http.StatusForbidden, "Forbidden\n", ErrGiteaUnauthorized},
		// Upstream Gitea answers 404 here even with a valid internal-auth
		// header — the cleanest runtime signal that the fork is not deployed.
		{"not found means not the fork", http.StatusNotFound, "404 page not found", ErrGiteaTeamNotFound},
		{"server error", http.StatusInternalServerError, `{"err":"boom"}`, ErrGiteaUnreachable},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var captured []capturedCostrictRequest
			srv := newCostrictTestServer(t, tc.status, tc.body, &captured)
			client := NewCostrictInternalClient(srv.URL, "tok")
			err := client.InvalidateQuotaCache(context.Background(), nil)
			if !errors.Is(err, tc.want) {
				t.Fatalf("error = %v, want %v", err, tc.want)
			}
		})
	}
}

func TestNewCostrictInternalClient_RequiresEndpointAndToken(t *testing.T) {
	if NewCostrictInternalClient("", "tok") != nil {
		t.Fatal("empty endpoint should yield nil (not configured)")
	}
	if NewCostrictInternalClient("http://gitea.example", "  ") != nil {
		t.Fatal("blank internal token should yield nil (not configured)")
	}
	client := NewCostrictInternalClient("http://gitea.example/", "tok")
	if client == nil || client.baseURL != "http://gitea.example" {
		t.Fatalf("trailing slash not stripped: %+v", client)
	}
}

func TestCostrictInternalClient_Healthz(t *testing.T) {
	var captured []capturedCostrictRequest
	srv := newCostrictTestServer(t, http.StatusOK,
		`{"enabled":true,"quota_enabled":true,"jwks_url":"http://cs-user/.well-known/jwks","version":"poc-1"}`, &captured)

	client := newCostrictInternalClientWithHTTPC(srv.URL, "tok", &http.Client{Timeout: 5 * time.Second})
	health, err := client.Healthz(context.Background())
	if err != nil {
		t.Fatalf("Healthz: %v", err)
	}
	if !health.Enabled || !health.QuotaEnabled {
		t.Fatalf("health = %+v", health)
	}
	if captured[0].method != http.MethodGet || captured[0].path != "/api/internal/costrict/healthz" {
		t.Fatalf("request = %s %s", captured[0].method, captured[0].path)
	}
}
