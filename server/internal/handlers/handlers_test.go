package handlers

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/costrict/costrict-web/server/internal/casdoor"
	"github.com/costrict/costrict-web/server/internal/database"
	"github.com/costrict/costrict-web/server/internal/middleware"
	"github.com/costrict/costrict-web/server/internal/models"
	userpkg "github.com/costrict/costrict-web/server/internal/user"
	"github.com/gin-gonic/gin"
	"github.com/golang-jwt/jwt/v4"
)

func signHandlersTestJWT(t *testing.T, claims jwt.MapClaims) string {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate rsa key: %v", err)
	}
	token := jwt.NewWithClaims(jwt.SigningMethodRS256, claims)
	tokenString, err := token.SignedString(key)
	if err != nil {
		t.Fatalf("sign jwt: %v", err)
	}
	return tokenString
}

func newRepoRouter(userID string) *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	injectUser := func(c *gin.Context) {
		if userID != "" {
			c.Set(middleware.UserIDKey, userID)
		}
		c.Next()
	}
	r.GET("/api/repositories", injectUser, ListRepositories)
	r.POST("/api/repositories", injectUser, CreateRepository)
	r.GET("/api/repositories/:id", injectUser, GetRepository)
	r.PUT("/api/repositories/:id", injectUser, UpdateRepository)
	r.DELETE("/api/repositories/:id", injectUser, DeleteRepository)
	r.GET("/api/repositories/:id/members", injectUser, ListRepositoryMembers)
	r.POST("/api/repositories/:id/members", injectUser, AddRepositoryMember)
	r.DELETE("/api/repositories/:id/members/:userId", injectUser, RemoveRepositoryMember)
	r.GET("/api/repositories/:id/registry", injectUser, GetRepositoryRegistry)
	r.GET("/api/repositories/my", injectUser, GetMyRepositories)
	return r
}

func newAuthRouter(userID string) *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	injectUser := func(c *gin.Context) {
		if userID != "" {
			c.Set(middleware.UserIDKey, userID)
		}
		c.Set("accessToken", "fake-token")
		c.Next()
	}
	r.GET("/api/auth/me", injectUser, GetCurrentUser)
	r.GET("/api/auth/identities", injectUser, ListBoundIdentities)
	r.GET("/api/auth/resolve", injectUser, ResolveAuthUser)
	r.POST("/api/auth/bind/start", injectUser, StartBindAuth)
	r.POST("/api/auth/identities/:provider/unbind", injectUser, UnbindIdentity)
	r.GET("/api/auth/callback", injectUser, AuthCallback)
	return r
}

// ---------------------------------------------------------------------------
// buildSyncConfigJSON
// ---------------------------------------------------------------------------

func TestBuildSyncConfigJSON(t *testing.T) {
	raw := buildSyncConfigJSON([]string{"*.md"}, []string{"vendor/**"}, "keep_remote", "secret")
	var out map[string]interface{}
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("invalid json: %v", err)
	}
	if out["conflictStrategy"] != "keep_remote" {
		t.Fatalf("expected conflictStrategy=keep_remote, got %v", out["conflictStrategy"])
	}
	if out["webhookSecret"] != "secret" {
		t.Fatalf("expected webhookSecret=secret, got %v", out["webhookSecret"])
	}
	includes := out["includePatterns"].([]interface{})
	if len(includes) != 1 || includes[0] != "*.md" {
		t.Fatalf("unexpected includePatterns: %v", includes)
	}
}

func TestGetCurrentUserReturnsLocalSubjectUser(t *testing.T) {
	defer setupTestDB(t)()
	defer InitUserModule(nil)
	InitUserModule(userpkg.New(database.DB))

	displayName := "Alice"
	email := "alice@example.com"
	avatar := "https://example.com/a.png"
	casdoorUniversalID := "uuid-u1"
	if err := database.DB.Create(&models.User{
		SubjectID:          "usr_local_1",
		Username:           "alice",
		DisplayName:        &displayName,
		Email:              &email,
		AvatarURL:          &avatar,
		CasdoorUniversalID: &casdoorUniversalID,
		IsActive:           true,
	}).Error; err != nil {
		t.Fatalf("seed user: %v", err)
	}

	w := get(newAuthRouter("usr_local_1"), "/api/auth/me")
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	var body struct {
		User struct {
			ID                 string  `json:"id"`
			SubjectID          string  `json:"subjectId"`
			Name               string  `json:"name"`
			Username           string  `json:"username"`
			Email              *string `json:"email"`
			AvatarURL          *string `json:"avatarUrl"`
			CasdoorUniversalID *string `json:"casdoorUniversalId"`
		} `json:"user"`
	}
	if err := json.NewDecoder(w.Body).Decode(&body); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if body.User.ID != "usr_local_1" || body.User.SubjectID != "usr_local_1" {
		t.Fatalf("expected subject_id based response, got %+v", body.User)
	}
	if body.User.Name != "Alice" || body.User.Username != "alice" {
		t.Fatalf("unexpected name fields: %+v", body.User)
	}
	if body.User.Email == nil || *body.User.Email != email {
		t.Fatalf("unexpected email: %+v", body.User)
	}
	if body.User.AvatarURL == nil || *body.User.AvatarURL != avatar {
		t.Fatalf("unexpected avatar: %+v", body.User)
	}
	if body.User.CasdoorUniversalID == nil || *body.User.CasdoorUniversalID != casdoorUniversalID {
		t.Fatalf("unexpected casdoor universal id: %+v", body.User)
	}
}

func TestListBoundIdentities(t *testing.T) {
	defer setupTestDB(t)()
	defer InitUserModule(nil)
	InitUserModule(userpkg.New(database.DB))
	if err := database.DB.Create(&models.User{SubjectID: "usr_local_1", Username: "alice", IsActive: true}).Error; err != nil {
		t.Fatalf("seed user: %v", err)
	}
	if err := database.DB.Create(&models.UserAuthIdentity{UserSubjectID: "usr_local_1", Provider: "github", ExternalKey: "casdoor:uuid-1", IsPrimary: true}).Error; err != nil {
		t.Fatalf("seed identity: %v", err)
	}
	w := get(newAuthRouter("usr_local_1"), "/api/auth/identities")
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var body struct{ Identities []map[string]any `json:"identities"` }
	if err := json.NewDecoder(w.Body).Decode(&body); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if len(body.Identities) != 1 {
		t.Fatalf("expected 1 identity, got %+v", body)
	}
}

func TestUnbindIdentityRejectsLastIdentity(t *testing.T) {
	defer setupTestDB(t)()
	defer InitUserModule(nil)
	InitUserModule(userpkg.New(database.DB))
	if err := database.DB.Create(&models.User{SubjectID: "usr_local_1", Username: "alice", IsActive: true}).Error; err != nil {
		t.Fatalf("seed user: %v", err)
	}
	identity := models.UserAuthIdentity{UserSubjectID: "usr_local_1", Provider: "github", ExternalKey: "casdoor:uuid-1", IsPrimary: true}
	if err := database.DB.Create(&identity).Error; err != nil {
		t.Fatalf("seed identity: %v", err)
	}
	w := postJSON(newAuthRouter("usr_local_1"), "/api/auth/identities/github/unbind", map[string]any{})
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", w.Code, w.Body.String())
	}
}

func TestEncodeDecodeBindStateWithSignature(t *testing.T) {
	bindStateSecret = "test-secret"
	encoded := encodeBindState(bindState{Action: "bind", UserSubjectID: "usr_1", Provider: "github", ExpiresAt: time.Now().Add(time.Minute).Unix(), Nonce: "n1"})
	decoded := decodeBindState(encoded)
	if decoded.Action != "bind" || decoded.UserSubjectID != "usr_1" || decoded.Provider != "github" {
		t.Fatalf("unexpected decoded state: %+v", decoded)
	}
	if len(strings.Split(encoded, ".")) != 2 {
		t.Fatalf("expected signed state payload, got %q", encoded)
	}
}

func TestDecodeBindStateRejectsTamperedState(t *testing.T) {
	bindStateSecret = "test-secret"
	encoded := encodeBindState(bindState{Action: "bind", UserSubjectID: "usr_1", Provider: "github", ExpiresAt: time.Now().Add(time.Minute).Unix(), Nonce: "n1"})
	parts := strings.Split(encoded, ".")
	tampered := parts[0] + ".deadbeef"
	decoded := decodeBindState(tampered)
	if decoded.Action != "" {
		t.Fatalf("expected tampered state to be rejected, got %+v", decoded)
	}
}

func TestStartBindAuthReturnsSignedURL(t *testing.T) {
	defer setupTestDB(t)()
	bindStateSecret = "test-secret"
	getLoginURLWithCallbackFunc = func(state, callbackURL string) string {
		return "https://casdoor.example/login?state=" + state
	}
	defer func() { getLoginURLWithCallbackFunc = func(state, callbackURL string) string { return CasdoorClient.GetLoginURLWithCallback(state, callbackURL) } }()
	w := postJSON(newAuthRouter("usr_local_1"), "/api/auth/bind/start", map[string]any{"provider": "github", "redirectTo": "https://zgsm.sangfor.com/cloud/settings/account"})
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var body struct{ AuthURL string `json:"authUrl"` }
	if err := json.NewDecoder(w.Body).Decode(&body); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if !strings.Contains(body.AuthURL, "state=") {
		t.Fatalf("expected authUrl to contain state, got %q", body.AuthURL)
	}
}

func TestBindCallbackRejectsExpiredState(t *testing.T) {
	defer setupTestDB(t)()
	defer InitUserModule(nil)
	InitUserModule(userpkg.New(database.DB))
	bindStateSecret = "test-secret"
	state := encodeBindState(bindState{Action: "bind", UserSubjectID: "usr_local_1", Provider: "github", ExpiresAt: time.Now().Add(-time.Minute).Unix(), Nonce: "n1"})
	w := httptest.NewRecorder()
	req, _ := http.NewRequest(http.MethodGet, "/api/auth/callback?code=abc&state="+state, nil)
	newAuthRouter("usr_local_1").ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", w.Code, w.Body.String())
	}
}

func TestBindCallbackRejectsProviderMismatch(t *testing.T) {
	defer setupTestDB(t)()
	defer InitUserModule(nil)
	InitUserModule(userpkg.New(database.DB))
	bindStateSecret = "test-secret"
	currentToken := signHandlersTestJWT(t, jwt.MapClaims{"id": "current-id", "sub": "current-sub", "universal_id": "current-uuid", "name": "acct_alpha", "provider": "phone", "phone_number": "15500000001"})
	currentUser, err := UserModule.Service.GetOrCreateUser(context.Background(), &userpkg.JWTClaims{ID: "current-id", Sub: "current-sub", UniversalID: "current-uuid", Name: "acct_alpha", PreferredUsername: "Account Alpha", Provider: "phone", Phone: "15500000001"})
	if err != nil {
		t.Fatalf("seed current user: %v", err)
	}
	r := gin.New()
	r.GET("/api/auth/callback", func(c *gin.Context) {
		c.Set(middleware.UserIDKey, currentUser.SubjectID)
		c.Request.Header.Set("Authorization", "Bearer "+currentToken)
		c.Set("accessToken", currentToken)
		AuthCallback(c)
	})
	defer func() {
		exchangeCodeForTokenFunc = func(code, callbackURL string) (*casdoor.CasdoorTokenResponse, error) { return CasdoorClient.ExchangeCodeForToken(code, callbackURL) }
		getUserInfoFunc = func(accessToken string) (*casdoor.CasdoorUserInfoResponse, error) { return CasdoorClient.GetUserInfo(accessToken) }
	}()
	exchangeCodeForTokenFunc = func(code, callbackURL string) (*casdoor.CasdoorTokenResponse, error) {
		boundToken := signHandlersTestJWT(t, jwt.MapClaims{"id": "bound-id", "sub": "bound-sub", "universal_id": "bound-uuid", "name": "bound_user", "provider": "idtrust", "properties": map[string]any{"oauth_Custom_id": "custom-user-001", "oauth_Custom_username": "custom_user", "oauth_Custom_displayName": "Display Custom User"}})
		return &casdoor.CasdoorTokenResponse{AccessToken: boundToken}, nil
	}
	getUserInfoFunc = func(accessToken string) (*casdoor.CasdoorUserInfoResponse, error) {
		return &casdoor.CasdoorUserInfoResponse{User: &casdoor.CasdoorUser{Id: "bound-id", Sub: "bound-sub", UniversalID: "bound-uuid", Name: "bound"}}, nil
	}
	state := encodeBindState(bindState{Action: "bind", UserSubjectID: currentUser.SubjectID, Provider: "github", ExpiresAt: time.Now().Add(time.Minute).Unix(), Nonce: "n1"})
	w := httptest.NewRecorder()
	req, _ := http.NewRequest(http.MethodGet, "/api/auth/callback?code=abc&state="+state, nil)
	r.ServeHTTP(w, req)
	if w.Code != http.StatusFound {
		t.Fatalf("expected 302, got %d: %s", w.Code, w.Body.String())
	}
	loc := w.Header().Get("Location")
	if !strings.Contains(loc, "bind=provider_mismatch") {
		t.Fatalf("expected redirect to contain bind=provider_mismatch, got %q", loc)
	}
	if !strings.Contains(loc, "expected_provider=github") {
		t.Fatalf("expected redirect to contain expected_provider=github, got %q", loc)
	}
	if !strings.Contains(loc, "actual_provider=idtrust") {
		t.Fatalf("expected redirect to contain actual_provider=idtrust, got %q", loc)
	}
}

func TestBindCallbackSuccess(t *testing.T) {
	defer setupTestDB(t)()
	defer InitUserModule(nil)
	InitUserModule(userpkg.New(database.DB))
	bindStateSecret = "test-secret"
	defer func() {
		exchangeCodeForTokenFunc = func(code, callbackURL string) (*casdoor.CasdoorTokenResponse, error) { return CasdoorClient.ExchangeCodeForToken(code, callbackURL) }
		getUserInfoFunc = func(accessToken string) (*casdoor.CasdoorUserInfoResponse, error) { return CasdoorClient.GetUserInfo(accessToken) }
	}()

	currentToken := signHandlersTestJWT(t, jwt.MapClaims{"id": "current-id", "sub": "current-sub", "universal_id": "current-uuid", "name": "acct_alpha", "provider": "phone", "phone_number": "15500000001"})
	currentUser, err := UserModule.Service.GetOrCreateUser(context.Background(), &userpkg.JWTClaims{ID: "current-id", Sub: "current-sub", UniversalID: "current-uuid", Name: "acct_alpha", PreferredUsername: "Account Alpha", Provider: "phone", Phone: "15500000001"})
	if err != nil {
		t.Fatalf("seed current user: %v", err)
	}
	// Explicitly bind phone identity since GetOrCreateUser might not auto-bind
	if err := UserModule.Service.BindIdentityToUser(context.Background(), currentUser.SubjectID, &userpkg.JWTClaims{ID: "current-id", Sub: "current-sub", UniversalID: "current-uuid", Name: "acct_alpha", PreferredUsername: "Account Alpha", Provider: "phone", Phone: "15500000001"}); err != nil {
		t.Fatalf("bind phone identity: %v", err)
	}

	r := gin.New()
	r.GET("/api/auth/callback", func(c *gin.Context) {
		c.Set(middleware.UserIDKey, currentUser.SubjectID)
		c.Request.Header.Set("Authorization", "Bearer "+currentToken)
		c.Set("accessToken", currentToken)
		AuthCallback(c)
	})

	exchangeCodeForTokenFunc = func(code, callbackURL string) (*casdoor.CasdoorTokenResponse, error) {
		boundToken := signHandlersTestJWT(t, jwt.MapClaims{"id": "bound-gh-id", "sub": "bound-gh-sub", "universal_id": "bound-gh-uuid", "name": "acct_github_user", "provider": "github", "properties": map[string]any{"oauth_GitHub_id": "provider-gh-001", "oauth_GitHub_username": "acct_github_user", "oauth_GitHub_displayName": "Display Github User", "oauth_GitHub_email": "user_github@example.com"}})
		return &casdoor.CasdoorTokenResponse{AccessToken: boundToken}, nil
	}
	getUserInfoFunc = func(accessToken string) (*casdoor.CasdoorUserInfoResponse, error) {
		return &casdoor.CasdoorUserInfoResponse{User: &casdoor.CasdoorUser{Id: "bound-gh-id", Sub: "bound-gh-sub", UniversalID: "bound-gh-uuid", Name: "acct_github_user", PreferredUsername: "Display Github User", Email: "user_github@example.com"}}, nil
	}

	state := encodeBindState(bindState{Action: "bind", UserSubjectID: currentUser.SubjectID, Provider: "github", ExpiresAt: time.Now().Add(time.Minute).Unix(), Nonce: "n-success", RedirectTo: "https://example.test/account"})
	w := httptest.NewRecorder()
	req, _ := http.NewRequest(http.MethodGet, "/api/auth/callback?code=ok&state="+state, nil)
	r.ServeHTTP(w, req)
	if w.Code != http.StatusFound {
		t.Fatalf("expected 302, got %d: %s", w.Code, w.Body.String())
	}
	if location := w.Header().Get("Location"); location != "https://example.test/account?bind=success" {
		t.Fatalf("expected redirect to account page with bind=success, got %q", location)
	}
	identities, err := UserModule.Service.ListUserIdentities(context.Background(), currentUser.SubjectID)
	if err != nil {
		t.Fatalf("list identities: %v", err)
	}
	if len(identities) != 2 {
		t.Fatalf("expected 2 identities after successful bind, got %d", len(identities))
	}
}

func TestBindCallbackRejectsIdentityAlreadyBound(t *testing.T) {
	defer setupTestDB(t)()
	defer InitUserModule(nil)
	InitUserModule(userpkg.New(database.DB))
	bindStateSecret = "test-secret"
	defer func() {
		exchangeCodeForTokenFunc = func(code, callbackURL string) (*casdoor.CasdoorTokenResponse, error) { return CasdoorClient.ExchangeCodeForToken(code, callbackURL) }
		getUserInfoFunc = func(accessToken string) (*casdoor.CasdoorUserInfoResponse, error) { return CasdoorClient.GetUserInfo(accessToken) }
	}()

	currentToken := signHandlersTestJWT(t, jwt.MapClaims{"id": "current-id", "sub": "current-sub", "universal_id": "current-uuid", "name": "acct_alpha", "provider": "phone", "phone_number": "15500000001"})
	currentUser, err := UserModule.Service.GetOrCreateUser(context.Background(), &userpkg.JWTClaims{ID: "current-id", Sub: "current-sub", UniversalID: "current-uuid", Name: "acct_alpha", PreferredUsername: "Account Beta", Provider: "github", ProviderUserID: "provider-gh-occupied"})
	if err != nil {
		t.Fatalf("seed current user: %v", err)
	}
	otherUser, err := UserModule.Service.GetOrCreateUser(context.Background(), &userpkg.JWTClaims{ID: "other-id", Sub: "other-sub", UniversalID: "other-uuid", Name: "acct_beta", PreferredUsername: "Account Beta", Provider: "github", ProviderUserID: "provider-gh-occupied"})
	if err != nil {
		t.Fatalf("seed other user: %v", err)
	}
	if err := UserModule.Service.BindIdentityToUser(context.Background(), otherUser.SubjectID, &userpkg.JWTClaims{ID: "bound-gh-id", Sub: "bound-gh-sub", UniversalID: "bound-gh-uuid", Name: "acct_github_user", PreferredUsername: "Display Github User", Provider: "github", ProviderUserID: "provider-gh-001"}); err != nil {
		t.Fatalf("seed occupied identity: %v", err)
	}

	r := gin.New()
	r.GET("/api/auth/callback", func(c *gin.Context) {
		c.Set(middleware.UserIDKey, currentUser.SubjectID)
		c.Request.Header.Set("Authorization", "Bearer "+currentToken)
		c.Set("accessToken", currentToken)
		AuthCallback(c)
	})

	exchangeCodeForTokenFunc = func(code, callbackURL string) (*casdoor.CasdoorTokenResponse, error) {
		boundToken := signHandlersTestJWT(t, jwt.MapClaims{"id": "bound-gh-id", "sub": "bound-gh-sub", "universal_id": "bound-gh-uuid", "name": "acct_github_user", "provider": "github", "properties": map[string]any{"oauth_GitHub_id": "provider-gh-001", "oauth_GitHub_username": "acct_github_user", "oauth_GitHub_displayName": "Display Github User"}})
		return &casdoor.CasdoorTokenResponse{AccessToken: boundToken}, nil
	}
	getUserInfoFunc = func(accessToken string) (*casdoor.CasdoorUserInfoResponse, error) {
		return &casdoor.CasdoorUserInfoResponse{User: &casdoor.CasdoorUser{Id: "bound-gh-id", Sub: "bound-gh-sub", UniversalID: "bound-gh-uuid", Name: "acct_github_user", PreferredUsername: "Display Github User"}}, nil
	}

	state := encodeBindState(bindState{Action: "bind", UserSubjectID: currentUser.SubjectID, Provider: "github", ExpiresAt: time.Now().Add(time.Minute).Unix(), Nonce: "n-conflict"})
	w := httptest.NewRecorder()
	req, _ := http.NewRequest(http.MethodGet, "/api/auth/callback?code=conflict&state="+state, nil)
	r.ServeHTTP(w, req)
	if w.Code != http.StatusFound {
		t.Fatalf("expected 302 redirect, got %d: %s", w.Code, w.Body.String())
	}
	location := w.Header().Get("Location")
	if !strings.Contains(location, "bind=conflict") {
		t.Fatalf("expected redirect with bind=conflict, got %q", location)
	}
	if !strings.Contains(location, "merge_token=") {
		t.Fatalf("expected redirect with merge_token, got %q", location)
	}
}

// ---------------------------------------------------------------------------
// TestResolveAuthUser — /api/auth/resolve endpoint for gateway
// ---------------------------------------------------------------------------

// TestResolveAuthUser_ReturnsUserHeaders tests the /api/auth/resolve endpoint
// used by gateway auth_request for Gitea reverse proxy authentication.
func TestResolveAuthUser_ReturnsUserHeaders(t *testing.T) {
	defer setupTestDB(t)()
	defer InitUserModule(nil)
	InitUserModule(userpkg.New(database.DB))

	email := "alice@example.com"
	provider := "github"
	externalKey := "casdoor:uuid-1"
	if err := database.DB.Create(&models.User{
		SubjectID:    "usr_resolve_1",
		Username:     "alice",
		Email:        &email,
		AuthProvider: &provider,
		ExternalKey:  &externalKey,
		IsActive:     true,
	}).Error; err != nil {
		t.Fatalf("seed user: %v", err)
	}

	w := get(newAuthRouter("usr_resolve_1"), "/api/auth/resolve")
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	// Check response headers (gateway reads these via $upstream_http_x_cs_user)
	csUser := w.Header().Get("X-CS-User")
	csEmail := w.Header().Get("X-CS-Email")
	if csUser != "usr_resolve_1" {
		t.Errorf("X-CS-User header: got %q, want usr_resolve_1", csUser)
	}
	if csEmail != email {
		t.Errorf("X-CS-Email header: got %q, want %s", csEmail, email)
	}

	// Check response body
	var body struct {
		UserID          string `json:"user_id"`
		Email           string `json:"email"`
		PrimaryIdentity string `json:"primary_identity"`
	}
	if err := json.NewDecoder(w.Body).Decode(&body); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if body.UserID != "usr_resolve_1" {
		t.Errorf("user_id: got %q, want usr_resolve_1", body.UserID)
	}
	if body.Email != email {
		t.Errorf("email: got %q, want %s", body.Email, email)
	}
	if body.PrimaryIdentity != "github|casdoor:uuid-1" {
		t.Errorf("primary_identity: got %q, want github|casdoor:uuid-1", body.PrimaryIdentity)
	}
}

// TestResolveAuthUser_UnauthenticatedReturns401 tests that /api/auth/resolve
// returns 401 when no user is authenticated (gateway expects this for auth_request).
func TestResolveAuthUser_UnauthenticatedReturns401(t *testing.T) {
	w := get(newAuthRouter(""), "/api/auth/resolve")
	if w.Code != http.StatusUnauthorized {
		t.Errorf("expected 401, got %d: %s", w.Code, w.Body.String())
	}
}
