package handlers

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/costrict/costrict-web/server/internal/casdoor"
	"github.com/costrict/costrict-web/server/internal/database"
	"github.com/costrict/costrict-web/server/internal/models"
	userpkg "github.com/costrict/costrict-web/server/internal/user"
	"github.com/gin-gonic/gin"
	"github.com/golang-jwt/jwt/v5"
)

// stubReissueWriter is a UserWriter stand-in that lets the OAuth callback test
// drive ReissueToken to a chosen outcome (success or failure) and record the
// arguments. Only the two methods the callback touches are meaningfully
// implemented — the rest panic if accidentally reached, surfacing misuse
// loudly rather than silently faking behaviour.
//
// ReissueToken returns *ReissueResult. The stub populates Profile + IsNew
// so the callback's happy path exercises the "drive local mirror
// GetOrCreateUser from response Profile" branch.
type stubReissueWriter struct {
	reissueToken     string
	reissueErr       error
	gotAudience      []string
	gotRawCasdoorJWT string
}

func (s *stubReissueWriter) GetOrCreateUser(ctx context.Context, claims *userpkg.JWTClaims) (*models.User, bool, error) {
	// Return a non-nil user so the callback advances into the ReissueToken
	// branch. isNewUser=false keeps the reg_pending cookie path off.
	return &models.User{SubjectID: "usr_stub"}, false, nil
}
func (s *stubReissueWriter) ReissueToken(ctx context.Context, audience []string, rawCasdoorJWT string) (*userpkg.ReissueResult, error) {
	s.gotAudience = audience
	s.gotRawCasdoorJWT = rawCasdoorJWT
	if s.reissueErr != nil {
		return nil, s.reissueErr
	}
	return &userpkg.ReissueResult{
		Token:     s.reissueToken,
		ExpiresAt: time.Now().Add(time.Hour),
		Profile: &userpkg.ReissueProfile{
			ID:          "casdoor-id-1",
			Sub:         "casdoor-sub-1",
			UniversalID: "uni-1",
			Name:        "alice",
		},
		IsNew: true,
	}, nil
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
func (s *stubReissueWriter) ParseIdentity(context.Context, string) (*userpkg.ParseIdentityResult, error) {
	// OAuth callback fallback path (ReissueToken failure) calls ParseIdentity
	// for JWT enrichment. Return an error so the fallback proceeds with
	// userinfo-only claims — the test exercises the cookie-fallback contract,
	// not the enrichment detail.
	return nil, userpkg.ErrSelfSignUnavailable
}

// runAuthCallbackReissueTest performs the common OAuth-callback setup for the
// ReissueToken cookie-takeover tests: inits a fresh in-memory DB, overrides
// exchange/getUserInfo to deterministic stubs, and points UserModule.Writer
// at the stub. The callback always asks cs-user to reissue a self-signed JWT
// (the old JWTSignMode gating was removed when local unverified JWT decoding
// was deleted — every cookie now flows through cs-user).
func runAuthCallbackReissueTest(t *testing.T, stub *stubReissueWriter) (cookieValue, rawCasdoorJWT string) {
	t.Helper()
	defer setupTestDB(t)()
	defer InitUserModule(nil)

	InitUserModule(userpkg.New(database.DB))
	// Swap the writer for our stub AFTER InitUserModule (which set the default
	// local UserService). Reader / TenantResolver stay nil — the callback only
	// needs Writer for this path.
	UserModule.Writer = stub

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

// TestAuthCallback_ReissueTokenSuccess_UsesCsUserToken pins the happy path:
// the OAuth callback always calls ReissueToken, and when the writer returns
// a fresh JWT the zgsmAdminToken cookie carries the cs-user-signed value —
// NOT the upstream Casdoor access token. This is the moment the
// platform-layer identity authority takes over the cookie.
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
// non-blocking contract: any ReissueToken failure (transport error,
// ErrSelfSignUnavailable from local-mode misconfig, cs-user 4xx/5xx) MUST
// NOT block login — the cookie falls back to the Casdoor access token so a
// cs-user outage can't take login down with it.
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

// TestAuthCallback_SSOShortcut_ProvisionsUser pins the regression: when the
// browser already holds a Casdoor access token that /api/userinfo honours,
// the OAuth callback MUST still run provisionCsUser (GetOrCreateUser +
// ReissueToken) before redirecting. Skipping provisioning — the old
// behaviour — left cs-user without a row, so middleware's by-identity lookup
// 404'd on the next request and the browser bounced back to /login, where
// the still-valid Casdoor cookie short-circuited the callback again. Loop.
//
// This test drives the exact shape: request arrives with a Cookie header
// carrying a Casdoor access token that getUserInfoFunc validates. We assert
// that ReissueToken fires (provisioning ran) and that the cookie the
// response sets carries the cs-user-signed JWT (not the input Casdoor
// value), proving the shortcut now goes through provisionCsUser.
func TestAuthCallback_SSOShortcut_ProvisionsUser(t *testing.T) {
	defer setupTestDB(t)()
	defer InitUserModule(nil)
	InitUserModule(userpkg.New(database.DB))
	stub := &stubReissueWriter{reissueToken: "CS-USER-MINTED-VIA-SSO"}
	UserModule.Writer = stub

	// A pre-existing Casdoor cookie — content is irrelevant because
	// getUserInfoFunc is stubbed; this is the value the browser would
	// carry when an already-logged-in Casdoor session returns to /login.
	const existingCasdoorToken = "EXISTING-CASDOOR-TOKEN"

	defer func() {
		exchangeCodeForTokenFunc = func(code, callbackURL string) (*casdoor.CasdoorTokenResponse, error) {
			return CasdoorClient.ExchangeCodeForToken(code, callbackURL)
		}
		getUserInfoFunc = func(accessToken string) (*casdoor.CasdoorUserInfoResponse, error) {
			return CasdoorClient.GetUserInfo(accessToken)
		}
	}()
	// Code-exchange must NOT fire on the SSO path — if it does, the test
	// would silently pass via the wrong branch. Make it loud.
	exchangeCodeForTokenFunc = func(code, callbackURL string) (*casdoor.CasdoorTokenResponse, error) {
		t.Fatalf("SSO shortcut must not exchange code; got code=%q", code)
		return nil, nil
	}
	getUserInfoFunc = func(accessToken string) (*casdoor.CasdoorUserInfoResponse, error) {
		if accessToken != existingCasdoorToken {
			t.Errorf("getUserInfoFunc accessToken: got %q, want %q", accessToken, existingCasdoorToken)
		}
		return &casdoor.CasdoorUserInfoResponse{
			User: &casdoor.CasdoorUser{Id: "sso-id", Sub: "sso-sub", UniversalID: "sso-uni", Name: "sso-user"},
		}, nil
	}

	r := gin.New()
	r.GET("/api/auth/callback", AuthCallback)

	w := httptest.NewRecorder()
	req, _ := http.NewRequest(http.MethodGet, "/api/auth/callback?code=abc", nil)
	req.AddCookie(&http.Cookie{Name: "zgsmAdminToken", Value: existingCasdoorToken})
	r.ServeHTTP(w, req)

	if w.Code != http.StatusFound {
		t.Fatalf("expected 302 redirect, got %d: body=%s", w.Code, w.Body.String())
	}

	// Provisioning MUST have run — ReissueToken got called with the
	// existing Casdoor token, and the cookie now carries the cs-user JWT.
	if stub.gotRawCasdoorJWT != existingCasdoorToken {
		t.Errorf("ReissueToken arg: got %q, want %q (provisioning skipped on SSO path)",
			stub.gotRawCasdoorJWT, existingCasdoorToken)
	}

	var cookieValue string
	resp := &http.Response{Header: w.Result().Header}
	for _, c := range resp.Cookies() {
		if c.Name == "zgsmAdminToken" {
			cookieValue = c.Value
		}
	}
	if cookieValue != "CS-USER-MINTED-VIA-SSO" {
		t.Fatalf("cookie: got %q, want cs-user minted token (SSO shortcut must overwrite the input Casdoor cookie)", cookieValue)
	}
	if cookieValue == existingCasdoorToken {
		t.Fatalf("cookie still equals the input Casdoor token — provisioning did not take over")
	}
}
