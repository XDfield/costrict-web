package user

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

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
		_ = json.NewEncoder(w).Encode(models.User{SubjectID: "usr_test-1", Username: "alice"})
	})
	w := newTestRPCWriter(t, srv.URL)

	u, err := w.GetOrCreateUser(&JWTClaims{UniversalID: "uuid-1", Name: "alice"})
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

	_, err := w.GetOrCreateUser(&JWTClaims{UniversalID: "uuid-1"})
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

	_, err := w.GetOrCreateUser(&JWTClaims{})
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

	_, err := w.GetOrCreateUser(nil)
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

	if _, err := w.SyncUser(&JWTClaims{UniversalID: "uuid-1"}); err != nil {
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

	if err := w.BindIdentityToUser("usr_target", &JWTClaims{UniversalID: "uuid-1", Provider: "github"}, BindIdentityOptions{ForceRebind: true}); err != nil {
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

	if err := w.BindIdentityToUser("usr_target", &JWTClaims{UniversalID: "uuid-1"}); err != nil {
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
	if err := w.BindIdentityToUser("usr_target", &JWTClaims{UniversalID: "uuid-1"}); err != nil {
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

	err := w.BindIdentityToUser("usr_target", &JWTClaims{UniversalID: "uuid-1"})
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

	err := w.BindIdentityToUser("usr_target", &JWTClaims{})
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

	if err := w.TransferIdentityToUser("usr_target", "casdoor:github:uuid-1", "usr_source"); err != nil {
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

	err := w.TransferIdentityToUser("usr_target", "casdoor:github:uuid-x", "")
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

	if err := w.UnbindIdentityByProvider("usr_target", "github"); err != nil {
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

	err := w.UnbindIdentityByProvider("usr_target", "github")
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
		"get-or-create": func() error { _, err := w.GetOrCreateUser(&JWTClaims{UniversalID: "x"}); return err },
		"sync":          func() error { _, err := w.SyncUser(&JWTClaims{UniversalID: "x"}); return err },
		"bind":          func() error { return w.BindIdentityToUser("usr", &JWTClaims{UniversalID: "x"}) },
		"transfer":      func() error { return w.TransferIdentityToUser("usr", "key", "") },
		"unbind":        func() error { return w.UnbindIdentityByProvider("usr", "github") },
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
	getOrCreate  int
	sync         int
	bind         int
	transfer     int
	unbind       int
	primaryError error // forces a non-nil return from all methods when set
}

func (r *recordingWriter) GetOrCreateUser(_ *JWTClaims) (*models.User, error) {
	r.getOrCreate++
	if r.primaryError != nil {
		return nil, r.primaryError
	}
	return &models.User{SubjectID: "usr_recording"}, nil
}
func (r *recordingWriter) SyncUser(_ *JWTClaims) (*models.User, error) {
	r.sync++
	if r.primaryError != nil {
		return nil, r.primaryError
	}
	return &models.User{SubjectID: "usr_recording"}, nil
}
func (r *recordingWriter) BindIdentityToUser(_ string, _ *JWTClaims, _ ...BindIdentityOptions) error {
	r.bind++
	return r.primaryError
}
func (r *recordingWriter) TransferIdentityToUser(_, _, _ string) error {
	r.transfer++
	return r.primaryError
}
func (r *recordingWriter) UnbindIdentityByProvider(_, _ string) error {
	r.unbind++
	return r.primaryError
}

func TestDualWriter_PrimarySuccessFansOutToSecondary(t *testing.T) {
	t.Parallel()
	primary := &recordingWriter{}
	secondary := &recordingWriter{}
	dw := &DualWriter{Primary: primary, Secondary: secondary}

	if _, err := dw.GetOrCreateUser(&JWTClaims{UniversalID: "uuid-1"}); err != nil {
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

	if _, err := dw.GetOrCreateUser(&JWTClaims{UniversalID: "uuid-1"}); err == nil || !errors.Is(err, primaryErr) {
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
	if _, err := dw.GetOrCreateUser(&JWTClaims{UniversalID: "uuid-1"}); err != nil {
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

	if err := dw.BindIdentityToUser("usr", &JWTClaims{UniversalID: "uuid-1"}); err != nil {
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
