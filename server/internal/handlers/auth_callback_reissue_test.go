package handlers

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/costrict/costrict-web/server/internal/casdoor"
	"github.com/costrict/costrict-web/server/internal/config"
	"github.com/costrict/costrict-web/server/internal/database"
	"github.com/costrict/costrict-web/server/internal/models"
	userpkg "github.com/costrict/costrict-web/server/internal/user"
	"github.com/gin-gonic/gin"
	"github.com/golang-jwt/jwt/v4"
)

// stubReissueWriter is a UserWriter stand-in that lets the OAuth callback test
// drive ReissueToken to a chosen outcome (success or failure) and record the
// arguments. Only the two methods the callback touches are meaningfully
// implemented — the rest panic if accidentally reached, surfacing misuse
// loudly rather than silently faking behaviour.
type stubReissueWriter struct {
	reissueToken    string
	reissueErr      error
	gotAudience     []string
	gotRawCasdoorJWT string
}

func (s *stubReissueWriter) GetOrCreateUser(ctx context.Context, claims *userpkg.JWTClaims) (*models.User, bool, error) {
	// Return a non-nil user so the callback advances into the ReissueToken
	// branch. isNewUser=false keeps the reg_pending cookie path off.
	return &models.User{SubjectID: "usr_stub"}, false, nil
}
func (s *stubReissueWriter) ReissueToken(ctx context.Context, audience []string, rawCasdoorJWT string) (string, time.Time, error) {
	s.gotAudience = audience
	s.gotRawCasdoorJWT = rawCasdoorJWT
	return s.reissueToken, time.Now().Add(time.Hour), s.reissueErr
}

func (s *stubReissueWriter) SyncUser(context.Context, *userpkg.JWTClaims) (*models.User, error) {
	panic("SyncUser not expected in OAuth callback test")
}
func (s *stubReissueWriter) BindIdentityToUser(context.Context, string, *userpkg.JWTClaims, ...userpkg.BindIdentityOptions) error {
	panic("BindIdentityToUser not expected in OAuth callback test")
}
func (s *stubReissueWriter) TransferIdentityToUser(context.Context, string, string, string) error {
	panic("TransferIdentityToUser not expected")
}
func (s *stubReissueWriter) UnbindIdentityByProvider(context.Context, string, string) error {
	panic("UnbindIdentityByProvider not expected")
}
func (s *stubReissueWriter) ApplyEnterpriseMapping(context.Context, string, string) error {
	return nil
}
func (s *stubReissueWriter) CompleteRegistration(context.Context, string, string, string) (*models.User, error) {
	panic("CompleteRegistration not expected")
}
func (s *stubReissueWriter) UpdateMyProfile(context.Context, string, string) (*models.User, error) {
	panic("UpdateMyProfile not expected")
}
func (s *stubReissueWriter) IsUsernameAvailable(context.Context, string, string) (bool, error) {
	panic("IsUsernameAvailable not expected")
}
func (s *stubReissueWriter) SuggestProfile(context.Context, *userpkg.JWTClaims) (string, string, error) {
	panic("SuggestProfile not expected")
}

// runAuthCallbackReissueTest performs the common OAuth-callback setup for the
// ReissueToken cookie-takeover tests: inits a fresh in-memory DB, overrides
// exchange/getUserInfo to deterministic stubs, swaps jwtSignMode to dual so
// the ReissueToken branch fires, and points UserModule.Writer at the stub.
func runAuthCallbackReissueTest(t *testing.T, stub *stubReissueWriter) (cookieValue, rawCasdoorJWT string) {
	t.Helper()
	defer setupTestDB(t)()
	defer InitUserModule(nil)

	InitUserModule(userpkg.New(database.DB))
	// Swap the writer for our stub AFTER InitUserModule (which set the default
	// local UserService). Reader / TenantResolver stay nil — the callback only
	// needs Writer for this path.
	UserModule.Writer = stub

	// Save & restore the package-level knobs the test mutates.
	prevSignMode := jwtSignMode
	defer func() { jwtSignMode = prevSignMode }()
	jwtSignMode = config.JWTSignModeDual

	defer func() {
		exchangeCodeForTokenFunc = func(code, callbackURL string) (*casdoor.CasdoorTokenResponse, error) {
			return CasdoorClient.ExchangeCodeForToken(code, callbackURL)
		}
		getUserInfoFunc = func(accessToken string) (*casdoor.CasdoorUserInfoResponse, error) {
			return CasdoorClient.GetUserInfo(accessToken)
		}
	}()

	// A real-looking raw Casdoor JWT — content doesn't matter because the
	// stub writer ignores it; we only assert it's forwarded verbatim.
	rawCasdoorJWT = signHandlersTestJWT(t, jwt.MapClaims{
		"id":           "casdoor-id-1",
		"sub":          "casdoor-sub-1",
		"universal_id": "uni-1",
		"name":         "alice",
		"exp":          time.Now().Add(time.Hour).Unix(),
	})

	exchangeCodeForTokenFunc = func(code, callbackURL string) (*casdoor.CasdoorTokenResponse, error) {
		return &casdoor.CasdoorTokenResponse{AccessToken: rawCasdoorJWT}, nil
	}
	getUserInfoFunc = func(accessToken string) (*casdoor.CasdoorUserInfoResponse, error) {
		return &casdoor.CasdoorUserInfoResponse{User: &casdoor.CasdoorUser{Id: "casdoor-id-1", Sub: "casdoor-sub-1", UniversalID: "uni-1", Name: "alice"}}, nil
	}

	r := gin.New()
	r.GET("/api/auth/callback", AuthCallback)

	w := httptest.NewRecorder()
	req, _ := http.NewRequest(http.MethodGet, "/api/auth/callback?code=abc", nil)
	r.ServeHTTP(w, req)

	if w.Code != http.StatusFound {
		t.Fatalf("expected 302 redirect, got %d: body=%s", w.Code, w.Body.String())
	}

	// Pull the zgsmAdminToken cookie out of the Set-Cookie header. net/http
	// lets us read all cookies via the Response.
	resp := &http.Response{Header: w.Result().Header}
	for _, c := range resp.Cookies() {
		if c.Name == "zgsmAdminToken" {
			cookieValue = c.Value
			return
		}
	}
	t.Fatalf("zgsmAdminToken cookie not set; headers=%v", w.Result().Header)
	return "", rawCasdoorJWT
}

// TestAuthCallback_ReissueTokenSuccess_UsesCsUserToken pins the Phase A8
// happy path: when jwtSignMode != off and the writer returns a fresh JWT,
// the zgsmAdminToken cookie carries the cs-user-signed value — NOT the
// upstream Casdoor access token. This is the moment the platform-layer
// identity authority switches over.
func TestAuthCallback_ReissueTokenSuccess_UsesCsUserToken(t *testing.T) {
	stub := &stubReissueWriter{reissueToken: "CS-USER-MINTED-TOKEN"}
	cookieValue, rawCasdoorJWT := runAuthCallbackReissueTest(t, stub)

	if cookieValue != "CS-USER-MINTED-TOKEN" {
		t.Fatalf("cookie: got %q, want cs-user minted token", cookieValue)
	}
	if cookieValue == rawCasdoorJWT {
		t.Fatalf("cookie still equals the raw Casdoor JWT — takeover did not happen")
	}
	if stub.gotRawCasdoorJWT != rawCasdoorJWT {
		t.Errorf("ReissueToken rawCasdoorJWT arg: got %q, want %q", stub.gotRawCasdoorJWT, rawCasdoorJWT)
	}
	if stub.gotAudience != nil {
		t.Errorf("ReissueToken audience arg: got %v, want nil", stub.gotAudience)
	}
}

// TestAuthCallback_ReissueTokenFails_FallsBackToCasdoorToken pins the
// non-blocking 灰度 contract: any ReissueToken failure (transport error,
// ErrSelfSignUnavailable, cs-user 4xx/5xx) MUST NOT block login — the
// cookie falls back to the Casdoor access token. The Phase A8 dual-sign
// cutover is intentionally permissive so a cs-user outage cannot take
// login down with it.
func TestAuthCallback_ReissueTokenFails_FallsBackToCasdoorToken(t *testing.T) {
	stub := &stubReissueWriter{reissueErr: errors.New("cs-user unreachable")}
	cookieValue, rawCasdoorJWT := runAuthCallbackReissueTest(t, stub)

	if cookieValue != rawCasdoorJWT {
		t.Fatalf("cookie: got %q, want raw Casdoor JWT (fallback)", cookieValue)
	}
	if stub.gotRawCasdoorJWT != rawCasdoorJWT {
		t.Errorf("ReissueToken still called with wrong arg: got %q, want %q", stub.gotRawCasdoorJWT, rawCasdoorJWT)
	}
}

// TestAuthCallback_JWTSignModeOff_SkipsReissueToken locks the gate: when
// jwtSignMode == off the callback MUST NOT call ReissueToken at all. This
// is the legacy / pre-Phase-A8 posture and must remain inert so operators
// can roll back by flipping one config knob.
func TestAuthCallback_JWTSignModeOff_SkipsReissueToken(t *testing.T) {
	defer setupTestDB(t)()
	defer InitUserModule(nil)
	InitUserModule(userpkg.New(database.DB))
	stub := &stubReissueWriter{reissueToken: "SHOULD-NOT-APPEAR"}
	UserModule.Writer = stub

	prevSignMode := jwtSignMode
	defer func() { jwtSignMode = prevSignMode }()
	jwtSignMode = config.JWTSignModeOff

	defer func() {
		exchangeCodeForTokenFunc = func(code, callbackURL string) (*casdoor.CasdoorTokenResponse, error) {
			return CasdoorClient.ExchangeCodeForToken(code, callbackURL)
		}
		getUserInfoFunc = func(accessToken string) (*casdoor.CasdoorUserInfoResponse, error) {
			return CasdoorClient.GetUserInfo(accessToken)
		}
	}()

	rawCasdoorJWT := signHandlersTestJWT(t, jwt.MapClaims{
		"id":  "casdoor-id-2",
		"sub": "casdoor-sub-2",
		"exp": time.Now().Add(time.Hour).Unix(),
	})
	exchangeCodeForTokenFunc = func(code, callbackURL string) (*casdoor.CasdoorTokenResponse, error) {
		return &casdoor.CasdoorTokenResponse{AccessToken: rawCasdoorJWT}, nil
	}
	getUserInfoFunc = func(accessToken string) (*casdoor.CasdoorUserInfoResponse, error) {
		return &casdoor.CasdoorUserInfoResponse{User: &casdoor.CasdoorUser{Id: "casdoor-id-2", Sub: "casdoor-sub-2", Name: "bob"}}, nil
	}

	r := gin.New()
	r.GET("/api/auth/callback", AuthCallback)
	w := httptest.NewRecorder()
	req, _ := http.NewRequest(http.MethodGet, "/api/auth/callback?code=abc", nil)
	r.ServeHTTP(w, req)
	if w.Code != http.StatusFound {
		t.Fatalf("expected 302, got %d: %s", w.Code, w.Body.String())
	}

	var cookieValue string
	resp := &http.Response{Header: w.Result().Header}
	for _, c := range resp.Cookies() {
		if c.Name == "zgsmAdminToken" {
			cookieValue = c.Value
		}
	}
	if cookieValue != rawCasdoorJWT {
		t.Fatalf("cookie: got %q, want raw Casdoor JWT (sign-mode off = no takeover)", cookieValue)
	}
	if stub.gotRawCasdoorJWT != "" {
		t.Errorf("ReissueToken was called even though jwtSignMode=off; arg=%q", stub.gotRawCasdoorJWT)
	}
}
