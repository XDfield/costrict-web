package user

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/costrict/costrict-web/server/internal/config"
	"github.com/costrict/costrict-web/server/internal/models"
)

// stubCSUserServer returns a httptest.Server that invokes handler with the
// incoming request. The returned URL is the cs-user base URL to feed into
// NewRPCWriter. The server also exposes the last request it received via the
// returned *lastReq pointer for assertion.
func stubCSUserServer(t *testing.T, handler func(w http.ResponseWriter, r *http.Request)) (*httptest.Server, *capturedRequest) {
	t.Helper()
	cap := &capturedRequest{}
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		cap.capture(r)
		handler(w, r)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv, cap
}

type capturedRequest struct {
	Method      string
	Path        string
	AuthHeader  string
	ContentType string
	Body        []byte
}

func (c *capturedRequest) capture(r *http.Request) {
	c.Method = r.Method
	c.Path = r.URL.Path
	c.AuthHeader = r.Header.Get("X-Internal-Token")
	c.ContentType = r.Header.Get("Content-Type")
	if r.Body != nil {
		b, _ := io.ReadAll(r.Body)
		c.Body = b
		r.Body = io.NopCloser(bytes.NewReader(b))
	}
}

func newTestRPCWriter(t *testing.T, baseURL string) *RPCWriter {
	t.Helper()
	return NewRPCWriter(config.UserServiceConfig{
		Backend:       config.UserServiceBackendRPC,
		BaseURL:       baseURL,
		InternalToken: "test-internal-token",
		TimeoutSec:    2,
	})
}

// --- GetOrCreateUser ---

func TestRPCWriter_GetOrCreateUser_HappyPath(t *testing.T) {
	t.Parallel()
	srv, cap := stubCSUserServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/internal/users/get-or-create" || r.Method != http.MethodPost {
			t.Errorf("unexpected call: %s %s", r.Method, r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"user":        models.User{SubjectID: "usr_test-1", Username: "alice"},
			"is_new_user": true,
		})
	})
	w := newTestRPCWriter(t, srv.URL)

	u, _, err := w.GetOrCreateUser(context.Background(), &JWTClaims{UniversalID: "uuid-1", Name: "alice"})
	if err != nil {
		t.Fatalf("GetOrCreateUser: %v", err)
	}
	if u.SubjectID != "usr_test-1" {
		t.Fatalf("SubjectID: got %q, want usr_test-1", u.SubjectID)
	}
	if cap.AuthHeader != "test-internal-token" {
		t.Fatalf("X-Internal-Token: got %q", cap.AuthHeader)
	}
	if cap.ContentType != "application/json" {
		t.Fatalf("Content-Type: got %q", cap.ContentType)
	}
	// Body should be the bare JWTClaims object.
	var got JWTClaims
	if err := json.Unmarshal(cap.Body, &got); err != nil {
		t.Fatalf("unmarshal request body: %v", err)
	}
	if got.UniversalID != "uuid-1" || got.Name != "alice" {
		t.Fatalf("decoded claims mismatch: %+v", got)
	}
}

func TestRPCWriter_GetOrCreateUser_5xxMapsToErrRPCUnavailable(t *testing.T) {
	t.Parallel()
	srv, _ := stubCSUserServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`upstream broke`))
	})
	w := newTestRPCWriter(t, srv.URL)

	_, _, err := w.GetOrCreateUser(context.Background(), &JWTClaims{UniversalID: "uuid-1"})
	if !errors.Is(err, ErrRPCUnavailable) {
		t.Fatalf("expected ErrRPCUnavailable, got %v", err)
	}
}

func TestRPCWriter_GetOrCreateUser_4xxSurfacesServerMessage(t *testing.T) {
	t.Parallel()
	srv, _ := stubCSUserServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":"no valid user identifier"}`))
	})
	w := newTestRPCWriter(t, srv.URL)

	_, _, err := w.GetOrCreateUser(context.Background(), &JWTClaims{})
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if err.Error() != "no valid user identifier" {
		t.Fatalf("expected verbatim cs-user message, got %q", err.Error())
	}
}

func TestRPCWriter_GetOrCreateUser_NilClaimsRejected(t *testing.T) {
	t.Parallel()
	srv, _ := stubCSUserServer(t, func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("server should not be called for nil claims")
	})
	w := newTestRPCWriter(t, srv.URL)

	_, _, err := w.GetOrCreateUser(context.Background(), nil)
	if err == nil || !strings.Contains(err.Error(), "nil JWT claims") {
		t.Fatalf("expected nil JWT claims error, got %v", err)
	}
}

// --- SyncUser ---

func TestRPCWriter_SyncUser_DelegatesToGetOrCreateEndpoint(t *testing.T) {
	t.Parallel()
	var hitGetOrCreate bool
	srv, _ := stubCSUserServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/internal/users/get-or-create" {
			hitGetOrCreate = true
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(models.User{SubjectID: "usr_sync-1"})
	})
	w := newTestRPCWriter(t, srv.URL)

	if _, err := w.SyncUser(context.Background(), &JWTClaims{UniversalID: "uuid-1"}); err != nil {
		t.Fatalf("SyncUser: %v", err)
	}
	if !hitGetOrCreate {
		t.Fatal("SyncUser must hit the same endpoint as GetOrCreateUser")
	}
}

// --- BindIdentityToUser ---

func TestRPCWriter_BindIdentityToUser_HappyPath(t *testing.T) {
	t.Parallel()
	srv, cap := stubCSUserServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/internal/users/usr_target/bind-identity" || r.Method != http.MethodPost {
			t.Errorf("unexpected call: %s %s", r.Method, r.URL.Path)
		}
		w.WriteHeader(http.StatusNoContent)
	})
	w := newTestRPCWriter(t, srv.URL)

	if err := w.BindIdentityToUser(context.Background(), "usr_target", &JWTClaims{UniversalID: "uuid-1", Provider: "github"}, BindIdentityOptions{ForceRebind: true}); err != nil {
		t.Fatalf("BindIdentityToUser: %v", err)
	}
	// Body should be the bind envelope with claims + options.
	var body struct {
		Claims  *JWTClaims           `json:"claims"`
		Options *BindIdentityOptions `json:"options"`
	}
	if err := json.Unmarshal(cap.Body, &body); err != nil {
		t.Fatalf("unmarshal body: %v", err)
	}
	if body.Claims == nil || body.Claims.UniversalID != "uuid-1" {
		t.Fatalf("claims mismatch: %+v", body.Claims)
	}
	if body.Options == nil || !body.Options.ForceRebind {
		t.Fatalf("ForceRebind not propagated: %+v", body.Options)
	}
}

func TestRPCWriter_BindIdentityToUser_OmitsOptionsWhenDefault(t *testing.T) {
	t.Parallel()
	srv, cap := stubCSUserServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	w := newTestRPCWriter(t, srv.URL)

	if err := w.BindIdentityToUser(context.Background(), "usr_target", &JWTClaims{UniversalID: "uuid-1"}); err != nil {
		t.Fatalf("BindIdentityToUser: %v", err)
	}
	var body struct {
		Options *BindIdentityOptions `json:"options"`
	}
	_ = json.Unmarshal(cap.Body, &body)
	if body.Options != nil {
		t.Fatalf("expected options to be omitted on default BindIdentityOptions, got %+v", body.Options)
	}
}

func TestRPCWriter_BindIdentityToUser_ExplicitlyUnboundReturnsNil(t *testing.T) {
	t.Parallel()
	srv, _ := stubCSUserServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusConflict)
		_, _ = w.Write([]byte(`{"error":"identity explicitly unbound; requires force_rebind"}`))
	})
	w := newTestRPCWriter(t, srv.URL)

	// Must return nil (no-op) — matches server's local writer at service.go:290.
	if err := w.BindIdentityToUser(context.Background(), "usr_target", &JWTClaims{UniversalID: "uuid-1"}); err != nil {
		t.Fatalf("explicitly_unbound should map to nil, got %v", err)
	}
}

func TestRPCWriter_BindIdentityToUser_AlreadyBoundReturnsServerToken(t *testing.T) {
	t.Parallel()
	srv, _ := stubCSUserServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusConflict)
		_, _ = w.Write([]byte(`{"error":"identity already bound to another user"}`))
	})
	w := newTestRPCWriter(t, srv.URL)

	err := w.BindIdentityToUser(context.Background(), "usr_target", &JWTClaims{UniversalID: "uuid-1"})
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	// handlers.go:566 matches on this exact string — byte-for-byte required.
	if err.Error() != serverIdentityAlreadyBoundToken {
		t.Fatalf("expected %q, got %q", serverIdentityAlreadyBoundToken, err.Error())
	}
}

func TestRPCWriter_BindIdentityToUser_Other4xxSurfacesServerMessage(t *testing.T) {
	t.Parallel()
	srv, _ := stubCSUserServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":"external key is required"}`))
	})
	w := newTestRPCWriter(t, srv.URL)

	err := w.BindIdentityToUser(context.Background(), "usr_target", &JWTClaims{})
	if err == nil || err.Error() != "external key is required" {
		t.Fatalf("expected verbatim server message, got %v", err)
	}
}

// --- TransferIdentityToUser ---

func TestRPCWriter_TransferIdentityToUser_HappyPath(t *testing.T) {
	t.Parallel()
	srv, cap := stubCSUserServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/internal/users/transfer-identity" || r.Method != http.MethodPost {
			t.Errorf("unexpected call: %s %s", r.Method, r.URL.Path)
		}
		w.WriteHeader(http.StatusNoContent)
	})
	w := newTestRPCWriter(t, srv.URL)

	if err := w.TransferIdentityToUser(context.Background(), "usr_target", "casdoor:github:uuid-1", "usr_source"); err != nil {
		t.Fatalf("TransferIdentityToUser: %v", err)
	}
	var body struct {
		TargetUserSubjectID string `json:"target_user_subject_id"`
		ExternalKey         string `json:"external_key"`
		SourceUserSubjectID string `json:"source_user_subject_id"`
	}
	if err := json.Unmarshal(cap.Body, &body); err != nil {
		t.Fatalf("unmarshal body: %v", err)
	}
	if body.TargetUserSubjectID != "usr_target" || body.ExternalKey != "casdoor:github:uuid-1" || body.SourceUserSubjectID != "usr_source" {
		t.Fatalf("transfer body mismatch: %+v", body)
	}
}

func TestRPCWriter_TransferIdentityToUser_NotFoundSurfacesServerMessage(t *testing.T) {
	t.Parallel()
	srv, _ := stubCSUserServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"error":"identity_not_found"}`))
	})
	w := newTestRPCWriter(t, srv.URL)

	err := w.TransferIdentityToUser(context.Background(), "usr_target", "casdoor:github:uuid-x", "")
	// handlers.go:833 matches on this exact string — byte-for-byte required.
	if err == nil || err.Error() != "identity_not_found" {
		t.Fatalf("expected identity_not_found, got %v", err)
	}
}

// --- UnbindIdentityByProvider ---

func TestRPCWriter_UnbindIdentityByProvider_HappyPath(t *testing.T) {
	t.Parallel()
	srv, cap := stubCSUserServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/internal/users/usr_target/identities/github" || r.Method != http.MethodDelete {
			t.Errorf("unexpected call: %s %s", r.Method, r.URL.Path)
		}
		w.WriteHeader(http.StatusNoContent)
	})
	w := newTestRPCWriter(t, srv.URL)

	if err := w.UnbindIdentityByProvider(context.Background(), "usr_target", "github"); err != nil {
		t.Fatalf("UnbindIdentityByProvider: %v", err)
	}
	if cap.Body != nil && len(cap.Body) != 0 {
		t.Fatalf("DELETE should have no body, got %q", string(cap.Body))
	}
}

func TestRPCWriter_UnbindIdentityByProvider_LastIdentityConflictSurfacesServerMessage(t *testing.T) {
	t.Parallel()
	srv, _ := stubCSUserServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusConflict)
		_, _ = w.Write([]byte(`{"error":"cannot unbind last identity"}`))
	})
	w := newTestRPCWriter(t, srv.URL)

	err := w.UnbindIdentityByProvider(context.Background(), "usr_target", "github")
	// handlers.go:766 matches on this exact string — byte-for-byte required.
	if err == nil || err.Error() != "cannot unbind last identity" {
		t.Fatalf("expected cannot unbind last identity, got %v", err)
	}
}

// --- Configured / NotConfigured ---

func TestRPCWriter_NotConfiguredReturnsErrNotConfigured(t *testing.T) {
	t.Parallel()
	w := NewRPCWriter(config.UserServiceConfig{Backend: config.UserServiceBackendRPC}) // no URL/token
	for name, fn := range map[string]func() error{
		"get-or-create": func() error {
			_, _, err := w.GetOrCreateUser(context.Background(), &JWTClaims{UniversalID: "x"})
			return err
		},
		"sync":     func() error { _, err := w.SyncUser(context.Background(), &JWTClaims{UniversalID: "x"}); return err },
		"bind":     func() error { return w.BindIdentityToUser(context.Background(), "usr", &JWTClaims{UniversalID: "x"}) },
		"transfer": func() error { return w.TransferIdentityToUser(context.Background(), "usr", "key", "") },
		"unbind":   func() error { return w.UnbindIdentityByProvider(context.Background(), "usr", "github") },
	} {
		name, fn := name, fn
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if err := fn(); !errors.Is(err, ErrNotConfigured) {
				t.Fatalf("expected ErrNotConfigured, got %v", err)
			}
		})
	}
}

// --- DualWriter ---

// recordingWriter is a UserWriter fake that records every call. Used to
// exercise DualWriter's primary/secondary fan-out semantics without spinning
// up an httptest server.
type recordingWriter struct {
	getOrCreate            int
	sync                   int
	bind                   int
	transfer               int
	unbind                 int
	applyEnterpriseMapping int
	reissueToken           int
	reissueTokenFn         func(userSubjectID string, claims *JWTClaims, audience []string) (string, time.Time, error)
	primaryError           error // forces a non-nil return from all methods when set
}

func (r *recordingWriter) GetOrCreateUser(_ context.Context, _ *JWTClaims) (*models.User, bool, error) {
	r.getOrCreate++
	if r.primaryError != nil {
		return nil, false, r.primaryError
	}
	return &models.User{SubjectID: "usr_recording"}, r.getOrCreate == 1, nil
}
func (r *recordingWriter) SyncUser(_ context.Context, _ *JWTClaims) (*models.User, error) {
	r.sync++
	if r.primaryError != nil {
		return nil, r.primaryError
	}
	return &models.User{SubjectID: "usr_recording"}, nil
}
func (r *recordingWriter) BindIdentityToUser(_ context.Context, _ string, _ *JWTClaims, _ ...BindIdentityOptions) error {
	r.bind++
	return r.primaryError
}
func (r *recordingWriter) TransferIdentityToUser(_ context.Context, _, _, _ string) error {
	r.transfer++
	return r.primaryError
}
func (r *recordingWriter) UnbindIdentityByProvider(_ context.Context, _, _ string) error {
	r.unbind++
	return r.primaryError
}
func (r *recordingWriter) ApplyEnterpriseMapping(_ context.Context, _, _ string) error {
	r.applyEnterpriseMapping++
	return r.primaryError
}
func (r *recordingWriter) ReissueToken(_ context.Context, userSubjectID string, claims *JWTClaims, audience []string) (string, time.Time, error) {
	r.reissueToken++
	if r.reissueTokenFn != nil {
		return r.reissueTokenFn(userSubjectID, claims, audience)
	}
	if r.primaryError != nil {
		return "", time.Time{}, r.primaryError
	}
	return "token-from-recording", time.Now().Add(time.Hour), nil
}

func TestDualWriter_PrimarySuccessFansOutToSecondary(t *testing.T) {
	t.Parallel()
	primary := &recordingWriter{}
	secondary := &recordingWriter{}
	dw := &DualWriter{Primary: primary, Secondary: secondary}

	if _, _, err := dw.GetOrCreateUser(context.Background(), &JWTClaims{UniversalID: "uuid-1"}); err != nil {
		t.Fatalf("GetOrCreateUser: %v", err)
	}
	if primary.getOrCreate != 1 || secondary.getOrCreate != 1 {
		t.Fatalf("expected primary=1 secondary=1, got primary=%d secondary=%d", primary.getOrCreate, secondary.getOrCreate)
	}
}

func TestDualWriter_PrimaryFailureSkipsSecondary(t *testing.T) {
	t.Parallel()
	primaryErr := fmt.Errorf("primary db down")
	primary := &recordingWriter{primaryError: primaryErr}
	secondary := &recordingWriter{}
	dw := &DualWriter{Primary: primary, Secondary: secondary}

	if _, _, err := dw.GetOrCreateUser(context.Background(), &JWTClaims{UniversalID: "uuid-1"}); err == nil || !errors.Is(err, primaryErr) {
		t.Fatalf("expected primary error to propagate, got %v", err)
	}
	if secondary.getOrCreate != 0 {
		t.Fatalf("secondary must not be called when primary fails, got %d calls", secondary.getOrCreate)
	}
}

func TestDualWriter_SecondaryFailureDoesNotFailRequest(t *testing.T) {
	t.Parallel()
	primary := &recordingWriter{}
	secondary := &recordingWriter{primaryError: fmt.Errorf("cs-user down")}
	dw := &DualWriter{Primary: primary, Secondary: secondary}

	// Primary succeeds → request succeeds even though secondary fails.
	// Secondary failure is logged but not surfaced.
	if _, _, err := dw.GetOrCreateUser(context.Background(), &JWTClaims{UniversalID: "uuid-1"}); err != nil {
		t.Fatalf("secondary failure must not fail the request, got %v", err)
	}
	if secondary.getOrCreate != 1 {
		t.Fatalf("secondary should have been called, got %d", secondary.getOrCreate)
	}
}

func TestDualWriter_NilSecondarySkipsFanOut(t *testing.T) {
	t.Parallel()
	primary := &recordingWriter{}
	dw := &DualWriter{Primary: primary, Secondary: nil}

	if err := dw.BindIdentityToUser(context.Background(), "usr", &JWTClaims{UniversalID: "uuid-1"}); err != nil {
		t.Fatalf("BindIdentityToUser: %v", err)
	}
	if primary.bind != 1 {
		t.Fatalf("primary call count: got %d, want 1", primary.bind)
	}
}

// --- parseErrorBody helper ---

func TestParseErrorBody(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		body []byte
		want string
		ok   bool
	}{
		{"simple envelope", []byte(`{"error":"identity_not_found"}`), "identity_not_found", true},
		{"empty body", nil, "", false},
		{"non-json", []byte(`upstream broke`), "", false},
		{"empty error field", []byte(`{"error":""}`), "", false},
		{"missing error field", []byte(`{"detail":"some other shape"}`), "", false},
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			got, ok := parseErrorBody(c.body)
			if ok != c.ok {
				t.Fatalf("ok: got %v, want %v", ok, c.ok)
			}
			if got != c.want {
				t.Fatalf("msg: got %q, want %q", got, c.want)
			}
		})
	}
}

// --- Phase A4b: ApplyEnterpriseMapping ---

func TestRPCWriter_ApplyEnterpriseMapping_HappyPath(t *testing.T) {
	t.Parallel()
	srv, cap := stubCSUserServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/internal/users/apply-enterprise-mapping" || r.Method != http.MethodPost {
			t.Errorf("unexpected call: %s %s", r.Method, r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"applied": true}`))
	})
	w := newTestRPCWriter(t, srv.URL)

	if err := w.ApplyEnterpriseMapping(context.Background(), "usr_alice", "idtrust"); err != nil {
		t.Fatalf("ApplyEnterpriseMapping: %v", err)
	}
	if cap.AuthHeader != "test-internal-token" {
		t.Errorf("X-Internal-Token: got %q", cap.AuthHeader)
	}
	// Body shape — empty tenant_id is omitted.
	var got struct {
		TenantID      string `json:"tenant_id"`
		UserSubjectID string `json:"user_subject_id"`
		Provider      string `json:"provider"`
	}
	if err := json.Unmarshal(cap.Body, &got); err != nil {
		t.Fatalf("unmarshal request body: %v", err)
	}
	if got.UserSubjectID != "usr_alice" || got.Provider != "idtrust" {
		t.Errorf("decoded body mismatch: %+v", got)
	}
	if got.TenantID != "" {
		t.Errorf("TenantID should be omitted when empty, got %q", got.TenantID)
	}
}

// TestRPCWriter_ApplyEnterpriseMapping_DisabledIsSuccess verifies the 200
// response with `{"applied": false}` is treated as success. cs-user returns
// this when the provider isn't in the tenant's enabled list — login callers
// treat the whole call as best-effort.
func TestRPCWriter_ApplyEnterpriseMapping_DisabledIsSuccess(t *testing.T) {
	t.Parallel()
	srv, _ := stubCSUserServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"applied": false}`))
	})
	w := newTestRPCWriter(t, srv.URL)

	if err := w.ApplyEnterpriseMapping(context.Background(), "usr_alice", "github"); err != nil {
		t.Fatalf("disabled response must be nil error, got %v", err)
	}
}

// TestRPCWriter_ApplyEnterpriseMapping_5xxMapsToErrRPCUnavailable verifies
// 5xx responses from cs-user map to ErrRPCUnavailable so the OAuth callback
// can log + continue without surfacing a low-level transport error.
func TestRPCWriter_ApplyEnterpriseMapping_5xxMapsToErrRPCUnavailable(t *testing.T) {
	t.Parallel()
	srv, _ := stubCSUserServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`cs-user internal error`))
	})
	w := newTestRPCWriter(t, srv.URL)

	err := w.ApplyEnterpriseMapping(context.Background(), "usr_alice", "idtrust")
	if !errors.Is(err, ErrRPCUnavailable) {
		t.Fatalf("expected ErrRPCUnavailable, got %v", err)
	}
}

// TestRPCWriter_ApplyEnterpriseMapping_4xxSurfacesServerMessage verifies 4xx
// responses surface the JSON error body verbatim — matches the existing
// write-method contract.
func TestRPCWriter_ApplyEnterpriseMapping_4xxSurfacesServerMessage(t *testing.T) {
	t.Parallel()
	srv, _ := stubCSUserServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":"invalid tenant config"}`))
	})
	w := newTestRPCWriter(t, srv.URL)

	err := w.ApplyEnterpriseMapping(context.Background(), "usr_alice", "idtrust")
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "invalid tenant config") {
		t.Fatalf("error should surface server message, got: %v", err)
	}
}

// TestRPCWriter_ApplyEnterpriseMapping_NotConfigured verifies the unconfigured
// writer returns ErrNotConfigured without making an HTTP call.
func TestRPCWriter_ApplyEnterpriseMapping_NotConfigured(t *testing.T) {
	t.Parallel()
	w := NewRPCWriter(config.UserServiceConfig{Backend: config.UserServiceBackendRPC})
	if w.Configured() {
		t.Fatal("writer should be unconfigured with empty URL+token")
	}
	if err := w.ApplyEnterpriseMapping(context.Background(), "usr_alice", "idtrust"); !errors.Is(err, ErrNotConfigured) {
		t.Fatalf("expected ErrNotConfigured, got %v", err)
	}
}

// --- DualWriter.ApplyEnterpriseMapping ---

// TestDualWriter_ApplyEnterpriseMapping_FansOut verifies both Primary and
// Secondary receive the call. Primary (local UserService stub) is a no-op;
// Secondary (RPCWriter) forwards to cs-user.
func TestDualWriter_ApplyEnterpriseMapping_FansOut(t *testing.T) {
	t.Parallel()
	primary := &recordingWriter{}
	secondary := &recordingWriter{}
	dw := &DualWriter{Primary: primary, Secondary: secondary}

	if err := dw.ApplyEnterpriseMapping(context.Background(), "usr_alice", "idtrust"); err != nil {
		t.Fatalf("ApplyEnterpriseMapping: %v", err)
	}
	if primary.applyEnterpriseMapping != 1 {
		t.Fatalf("primary call count: got %d, want 1", primary.applyEnterpriseMapping)
	}
	if secondary.applyEnterpriseMapping != 1 {
		t.Fatalf("secondary call count: got %d, want 1", secondary.applyEnterpriseMapping)
	}
}

// TestDualWriter_ApplyEnterpriseMapping_SecondaryErrorDoesNotFail verifies
// that a Secondary failure is logged but never returned — employment mapping
// is best-effort and must never block login.
func TestDualWriter_ApplyEnterpriseMapping_SecondaryErrorDoesNotFail(t *testing.T) {
	t.Parallel()
	primary := &recordingWriter{}
	secondary := &recordingWriter{primaryError: fmt.Errorf("cs-user unreachable")}
	dw := &DualWriter{Primary: primary, Secondary: secondary}

	if err := dw.ApplyEnterpriseMapping(context.Background(), "usr_alice", "idtrust"); err != nil {
		t.Fatalf("secondary failure must not bubble up, got %v", err)
	}
	if secondary.applyEnterpriseMapping != 1 {
		t.Fatalf("secondary should still have been invoked, got %d", secondary.applyEnterpriseMapping)
	}
}

// TestDualWriter_ApplyEnterpriseMapping_PrimaryErrorFails verifies a Primary
// error surfaces — DualWriter delegates to Primary's contract first. The
// local stub never errors in practice, but the contract must hold for
// symmetry with the other write methods.
func TestDualWriter_ApplyEnterpriseMapping_PrimaryErrorFails(t *testing.T) {
	t.Parallel()
	primary := &recordingWriter{primaryError: fmt.Errorf("local primary exploded")}
	secondary := &recordingWriter{}
	dw := &DualWriter{Primary: primary, Secondary: secondary}

	err := dw.ApplyEnterpriseMapping(context.Background(), "usr_alice", "idtrust")
	if err == nil || !strings.Contains(err.Error(), "local primary exploded") {
		t.Fatalf("expected primary error to surface, got %v", err)
	}
	if secondary.applyEnterpriseMapping != 0 {
		t.Fatalf("secondary must not be called when primary failed, got %d", secondary.applyEnterpriseMapping)
	}
}

// TestDualWriter_ApplyEnterpriseMapping_NilSecondarySkipsFanOut verifies the
// nil-Secondary path is safe (no method call on nil receiver).
func TestDualWriter_ApplyEnterpriseMapping_NilSecondarySkipsFanOut(t *testing.T) {
	t.Parallel()
	primary := &recordingWriter{}
	dw := &DualWriter{Primary: primary, Secondary: nil}

	if err := dw.ApplyEnterpriseMapping(context.Background(), "usr_alice", "idtrust"); err != nil {
		t.Fatalf("ApplyEnterpriseMapping with nil Secondary: %v", err)
	}
	if primary.applyEnterpriseMapping != 1 {
		t.Fatalf("primary call count: got %d, want 1", primary.applyEnterpriseMapping)
	}
}

// --- RPCWriter.ReissueToken (Phase A7b) ---

// TestRPCWriter_ReissueToken_HappyPath verifies the wire format + response
// decode. The handler's Identity field is a *JWTClaims — server-side
// JWTClaims has no JSON tags so the wire is PascalCase; cs-user decodes via
// encoding/json's case-insensitive fallback (same mechanism as GetOrCreateUser).
func TestRPCWriter_ReissueToken_HappyPath(t *testing.T) {
	t.Parallel()
	srv, cap := stubCSUserServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/internal/users/reissue-token" || r.Method != http.MethodPost {
			t.Errorf("unexpected call: %s %s", r.Method, r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"token":"signed-jwt-xyz","expires_at":"2026-07-17T13:00:00Z"}`))
	})
	w := newTestRPCWriter(t, srv.URL)

	token, exp, err := w.ReissueToken(context.Background(), "usr_alice", &JWTClaims{
		UniversalID: "uuid-alice",
		Name:        "Alice",
	}, nil)
	if err != nil {
		t.Fatalf("ReissueToken: %v", err)
	}
	if token != "signed-jwt-xyz" {
		t.Errorf("token: got %q, want signed-jwt-xyz", token)
	}
	wantExp := time.Date(2026, 7, 17, 13, 0, 0, 0, time.UTC)
	if !exp.Equal(wantExp) {
		t.Errorf("expires_at: got %v, want %v", exp, wantExp)
	}

	// Verify the request body shape: user_subject_id forwarded; identity
	// carries OIDC fields with explicit snake_case json tags (the prior
	// reliance on encoding/json's case-insensitive fallback silently dropped
	// snake_case-only fields like universal_id on the cs-user side).
	var body struct {
		UserSubjectID string `json:"user_subject_id"`
		Identity      struct {
			UniversalID string `json:"universal_id"`
			Name        string `json:"name"`
		} `json:"identity"`
	}
	if err := json.Unmarshal(cap.Body, &body); err != nil {
		t.Fatalf("unmarshal request body: %v", err)
	}
	if body.UserSubjectID != "usr_alice" {
		t.Errorf("user_subject_id: got %q", body.UserSubjectID)
	}
	if body.Identity.UniversalID != "uuid-alice" {
		t.Errorf("Identity.UniversalID: got %q", body.Identity.UniversalID)
	}
	if body.Identity.Name != "Alice" {
		t.Errorf("Identity.Name: got %q", body.Identity.Name)
	}
}

// TestRPCWriter_ReissueToken_AudienceForwarded verifies the audience override
// reaches the wire. nil audience = no audience key on wire (omitempty).
func TestRPCWriter_ReissueToken_AudienceForwarded(t *testing.T) {
	t.Parallel()
	srv, cap := stubCSUserServer(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"token":"t","expires_at":"2026-07-17T13:00:00Z"}`))
	})
	w := newTestRPCWriter(t, srv.URL)

	if _, _, err := w.ReissueToken(context.Background(), "usr_alice", nil, []string{"csc-cli", "ops-portal"}); err != nil {
		t.Fatalf("ReissueToken: %v", err)
	}

	var body struct {
		Audience []string `json:"audience"`
	}
	_ = json.Unmarshal(cap.Body, &body)
	if len(body.Audience) != 2 || body.Audience[0] != "csc-cli" || body.Audience[1] != "ops-portal" {
		t.Errorf("audience: got %v, want [csc-cli ops-portal]", body.Audience)
	}
}

// TestRPCWriter_ReissueToken_NilAudienceOmitted verifies nil audience emits
// no `audience` key on wire (omitempty) so cs-user falls back to its default.
func TestRPCWriter_ReissueToken_NilAudienceOmitted(t *testing.T) {
	t.Parallel()
	srv, cap := stubCSUserServer(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"token":"t","expires_at":"2026-07-17T13:00:00Z"}`))
	})
	w := newTestRPCWriter(t, srv.URL)

	if _, _, err := w.ReissueToken(context.Background(), "usr_alice", nil, nil); err != nil {
		t.Fatalf("ReissueToken: %v", err)
	}

	if strings.Contains(string(cap.Body), "audience") {
		t.Errorf("audience key leaked onto wire: %s", string(cap.Body))
	}
}

// TestRPCWriter_ReissueToken_EmptySubjectIDRejected verifies the local
// guard — caller bug, surface before making the HTTP call.
func TestRPCWriter_ReissueToken_EmptySubjectIDRejected(t *testing.T) {
	t.Parallel()
	srv, _ := stubCSUserServer(t, func(http.ResponseWriter, *http.Request) {
		t.Fatal("server should not be called when subject_id is empty")
	})
	defer srv.Close()
	w := newTestRPCWriter(t, srv.URL)

	_, _, err := w.ReissueToken(context.Background(), "", &JWTClaims{}, nil)
	if err == nil || !strings.Contains(err.Error(), "empty user_subject_id") {
		t.Fatalf("expected empty-subject error, got %v", err)
	}
}

// TestRPCWriter_ReissueToken_NotConfigured verifies the unconfigured writer
// returns ErrNotConfigured without making an HTTP call.
func TestRPCWriter_ReissueToken_NotConfigured(t *testing.T) {
	t.Parallel()
	w := NewRPCWriter(config.UserServiceConfig{Backend: config.UserServiceBackendRPC})
	if w.Configured() {
		t.Fatal("writer should be unconfigured with empty URL+token")
	}
	_, _, err := w.ReissueToken(context.Background(), "usr_alice", &JWTClaims{}, nil)
	if !errors.Is(err, ErrNotConfigured) {
		t.Fatalf("expected ErrNotConfigured, got %v", err)
	}
}

// TestRPCWriter_ReissueToken_5xxMapsToErrRPCUnavailable verifies 5xx
// responses surface as ErrRPCUnavailable (same mapping as other write methods).
func TestRPCWriter_ReissueToken_5xxMapsToErrRPCUnavailable(t *testing.T) {
	t.Parallel()
	srv, _ := stubCSUserServer(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
	})
	w := newTestRPCWriter(t, srv.URL)

	_, _, err := w.ReissueToken(context.Background(), "usr_alice", &JWTClaims{}, nil)
	if !errors.Is(err, ErrRPCUnavailable) {
		t.Fatalf("expected ErrRPCUnavailable, got %v", err)
	}
}

// TestRPCWriter_ReissueToken_4xxSurfacesServerMessage verifies 4xx
// responses surface the cs-user error message verbatim (same as other methods).
func TestRPCWriter_ReissueToken_4xxSurfacesServerMessage(t *testing.T) {
	t.Parallel()
	srv, _ := stubCSUserServer(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":"user_subject_id is required"}`))
	})
	w := newTestRPCWriter(t, srv.URL)

	_, _, err := w.ReissueToken(context.Background(), "usr_alice", &JWTClaims{}, nil)
	if err == nil || err.Error() != "user_subject_id is required" {
		t.Fatalf("expected server message, got %v", err)
	}
}

// TestRPCWriter_ReissueToken_EmptyTokenInResponseErrors verifies the
// defensive decode guard: a 200 with empty token is a server-side bug
// and must surface as an error rather than silently returning "".
func TestRPCWriter_ReissueToken_EmptyTokenInResponseErrors(t *testing.T) {
	t.Parallel()
	srv, _ := stubCSUserServer(t, func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"token":"","expires_at":"2026-07-17T13:00:00Z"}`))
	})
	w := newTestRPCWriter(t, srv.URL)

	_, _, err := w.ReissueToken(context.Background(), "usr_alice", &JWTClaims{}, nil)
	if err == nil || !strings.Contains(err.Error(), "empty token") {
		t.Fatalf("expected empty-token error, got %v", err)
	}
}

// --- DualWriter.ReissueToken (Phase A7b) ---

// TestDualWriter_ReissueToken_BypassesPrimary verifies Secondary is the
// authoritative path — Primary is never called. Unlike ApplyEnterpriseMapping,
// the local UserService has no RSA signing key and cannot fulfill this; the
// DualWriter intentionally routes around Primary.
func TestDualWriter_ReissueToken_BypassesPrimary(t *testing.T) {
	t.Parallel()
	// Primary stubs ReissueToken via primaryError to prove it would fail
	// if invoked — but the DualWriter must skip Primary entirely.
	primary := &recordingWriter{primaryError: fmt.Errorf("primary should not be called")}
	secondary := &recordingWriter{}
	dw := &DualWriter{Primary: primary, Secondary: secondary}

	token, _, err := dw.ReissueToken(context.Background(), "usr_alice", &JWTClaims{}, nil)
	if err != nil {
		t.Fatalf("ReissueToken: %v", err)
	}
	if token != "token-from-recording" {
		t.Errorf("token: got %q, want token-from-recording", token)
	}
	if primary.reissueToken != 0 {
		t.Fatalf("primary must not be called, got %d calls", primary.reissueToken)
	}
	if secondary.reissueToken != 1 {
		t.Fatalf("secondary call count: got %d, want 1", secondary.reissueToken)
	}
}

// TestDualWriter_ReissueToken_NilSecondaryReturnsErrSelfSignUnavailable
// verifies the nil-Secondary guard. Without Secondary there is no path to
// cs-user; surfacing ErrSelfSignUnavailable lets the OAuth callback detect
// the misconfiguration (JWT_SELF_SIGN_ENABLED=true but no RPC backend).
func TestDualWriter_ReissueToken_NilSecondaryReturnsErrSelfSignUnavailable(t *testing.T) {
	t.Parallel()
	primary := &recordingWriter{}
	dw := &DualWriter{Primary: primary, Secondary: nil}

	_, _, err := dw.ReissueToken(context.Background(), "usr_alice", &JWTClaims{}, nil)
	if !errors.Is(err, ErrSelfSignUnavailable) {
		t.Fatalf("expected ErrSelfSignUnavailable, got %v", err)
	}
	if primary.reissueToken != 0 {
		t.Fatalf("primary must not be called, got %d calls", primary.reissueToken)
	}
}

// TestDualWriter_ReissueToken_SecondaryErrorPropagates verifies Secondary
// errors are returned to the caller (NOT swallowed like ApplyEnterpriseMapping).
// This is intentional — the OAuth callback needs to know ReissueToken failed
// so it can fall back to the Casdoor token.
func TestDualWriter_ReissueToken_SecondaryErrorPropagates(t *testing.T) {
	t.Parallel()
	primary := &recordingWriter{}
	secondary := &recordingWriter{primaryError: fmt.Errorf("cs-user unreachable")}
	dw := &DualWriter{Primary: primary, Secondary: secondary}

	_, _, err := dw.ReissueToken(context.Background(), "usr_alice", &JWTClaims{}, nil)
	if err == nil || !strings.Contains(err.Error(), "cs-user unreachable") {
		t.Fatalf("expected secondary error to propagate, got %v", err)
	}
}

// --- UserService.ReissueToken local stub (Phase A7b) ---

// TestUserService_ReissueToken_LocalStubReturnsErrSelfSignUnavailable
// verifies the local backend can never satisfy this call. Server has no RSA
// signing key — only RPCWriter (via cs-user) can mint tokens.
func TestUserService_ReissueToken_LocalStubReturnsErrSelfSignUnavailable(t *testing.T) {
	t.Parallel()
	var s *UserService // method receiver is nil-safe — no db needed
	token, _, err := s.ReissueToken(context.Background(), "usr_alice", &JWTClaims{}, nil)
	if !errors.Is(err, ErrSelfSignUnavailable) {
		t.Fatalf("expected ErrSelfSignUnavailable, got %v", err)
	}
	if token != "" {
		t.Errorf("token: got %q, want empty", token)
	}
}

// --- UserService.ApplyEnterpriseMapping (local stub) ---

// TestUserService_ApplyEnterpriseMapping_LocalStubIsNoOp verifies the local
// stub returns nil unconditionally. Server has no employment_identities
// table; the call must not error in local mode.
func TestUserService_ApplyEnterpriseMapping_LocalStubIsNoOp(t *testing.T) {
	t.Parallel()
	svc := &UserService{}

	if err := svc.ApplyEnterpriseMapping(context.Background(), "usr_alice", "idtrust"); err != nil {
		t.Fatalf("local stub should be no-op nil, got %v", err)
	}
	// Empty args must also be safe — the OAuth callback may fire this hook
	// before validation runs on the cs-user side.
	if err := svc.ApplyEnterpriseMapping(context.Background(), "", ""); err != nil {
		t.Fatalf("local stub with empty args should still be no-op nil, got %v", err)
	}
}
