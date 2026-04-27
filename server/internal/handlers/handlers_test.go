package handlers

import (
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
	r.POST("/api/auth/bind/start", injectUser, StartBindAuth)
	r.POST("/api/auth/identities/:id/unbind", injectUser, UnbindIdentity)
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
	w := postJSON(newAuthRouter("usr_local_1"), "/api/auth/identities/1/unbind", map[string]any{})
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
	currentUser, err := UserModule.Service.GetOrCreateUser(&userpkg.JWTClaims{ID: "current-id", Sub: "current-sub", UniversalID: "current-uuid", Name: "acct_alpha", PreferredUsername: "Account Alpha", Provider: "phone", Phone: "15500000001"})
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
	if w.Code != http.StatusConflict {
		t.Fatalf("expected 409, got %d: %s", w.Code, w.Body.String())
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
	currentUser, err := UserModule.Service.GetOrCreateUser(&userpkg.JWTClaims{ID: "current-id", Sub: "current-sub", UniversalID: "current-uuid", Name: "acct_alpha", PreferredUsername: "Account Alpha", Provider: "phone", Phone: "15500000001"})
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
	if location := w.Header().Get("Location"); location != "https://example.test/account" {
		t.Fatalf("expected redirect to account page, got %q", location)
	}
	identities, err := UserModule.Service.ListUserIdentities(currentUser.SubjectID)
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
	currentUser, err := UserModule.Service.GetOrCreateUser(&userpkg.JWTClaims{ID: "current-id", Sub: "current-sub", UniversalID: "current-uuid", Name: "acct_alpha", PreferredUsername: "Account Alpha", Provider: "phone", Phone: "15500000001"})
	if err != nil {
		t.Fatalf("seed current user: %v", err)
	}
	otherUser, err := UserModule.Service.GetOrCreateUser(&userpkg.JWTClaims{ID: "other-id", Sub: "other-sub", UniversalID: "other-uuid", Name: "acct_beta", PreferredUsername: "Account Beta", Provider: "github", ProviderUserID: "provider-gh-occupied"})
	if err != nil {
		t.Fatalf("seed other user: %v", err)
	}
	if err := UserModule.Service.BindIdentityToUser(otherUser.SubjectID, &userpkg.JWTClaims{ID: "bound-gh-id", Sub: "bound-gh-sub", UniversalID: "bound-gh-uuid", Name: "acct_github_user", PreferredUsername: "Display Github User", Provider: "github", ProviderUserID: "provider-gh-001"}); err != nil {
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
	if w.Code != http.StatusConflict {
		t.Fatalf("expected 409, got %d: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "identity_already_bound") {
		t.Fatalf("expected identity_already_bound error, got %s", w.Body.String())
	}
}

// ---------------------------------------------------------------------------
// ListRepositories
// ---------------------------------------------------------------------------

func TestListRepositories_Empty(t *testing.T) {
	defer setupTestDB(t)()

	w := get(newRepoRouter(""), "/api/repositories")
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	var body map[string]interface{}
	json.NewDecoder(w.Body).Decode(&body)
	repos := body["repositories"].([]interface{})
	if len(repos) != 0 {
		t.Fatalf("expected 0 repos, got %d", len(repos))
	}
}

func TestListRepositories_WithData(t *testing.T) {
	defer setupTestDB(t)()
	database.DB.Create(&models.Repository{ID: "repo-l1", Name: "alpha", OwnerID: "u1"})
	database.DB.Create(&models.Repository{ID: "repo-l2", Name: "beta", OwnerID: "u2"})

	w := get(newRepoRouter(""), "/api/repositories")
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	var body map[string]interface{}
	json.NewDecoder(w.Body).Decode(&body)
	repos := body["repositories"].([]interface{})
	if len(repos) != 2 {
		t.Fatalf("expected 2 repos, got %d", len(repos))
	}
}

// ---------------------------------------------------------------------------
// CreateRepository
// ---------------------------------------------------------------------------

func TestCreateRepository_Success(t *testing.T) {
	defer setupTestDB(t)()

	w := postJSON(newRepoRouter("u1"), "/api/repositories", map[string]interface{}{
		"name": "my-repo", "ownerId": "u1",
	})
	if w.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", w.Code, w.Body.String())
	}
	var repo map[string]interface{}
	json.NewDecoder(w.Body).Decode(&repo)
	if repo["name"] != "my-repo" {
		t.Fatalf("unexpected name: %v", repo["name"])
	}
	if repo["repoType"] != "normal" {
		t.Fatalf("expected repoType=normal, got %v", repo["repoType"])
	}
	if repo["visibility"] != "private" {
		t.Fatalf("expected visibility=private, got %v", repo["visibility"])
	}
}

func TestCreateRepository_MissingRequired(t *testing.T) {
	defer setupTestDB(t)()

	// name is the only required field now; ownerId comes from auth context
	w := postJSON(newRepoRouter("u1"), "/api/repositories", map[string]interface{}{})
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", w.Code)
	}
}

func TestCreateRepository_DefaultsVisibilityAndType(t *testing.T) {
	defer setupTestDB(t)()

	w := postJSON(newRepoRouter("u1"), "/api/repositories", map[string]interface{}{
		"name": "defaults-repo", "ownerId": "u1",
	})
	if w.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", w.Code, w.Body.String())
	}
	var repo map[string]interface{}
	json.NewDecoder(w.Body).Decode(&repo)
	if repo["visibility"] != "private" {
		t.Fatalf("expected default visibility=private, got %v", repo["visibility"])
	}
	if repo["repoType"] != "normal" {
		t.Fatalf("expected default repoType=normal, got %v", repo["repoType"])
	}
}

func TestCreateRepository_OwnerAddedAsMember(t *testing.T) {
	defer setupTestDB(t)()

	w := postJSON(newRepoRouter("u1"), "/api/repositories", map[string]interface{}{
		"name": "member-repo", "ownerId": "u1",
	})
	if w.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d", w.Code)
	}
	var repo map[string]interface{}
	json.NewDecoder(w.Body).Decode(&repo)
	repoID := repo["id"].(string)

	var count int64
	database.DB.Model(&models.RepoMember{}).Where("repo_id = ? AND user_id = ? AND role = 'owner'", repoID, "u1").Count(&count)
	if count != 1 {
		t.Fatalf("expected owner to be added as member, got count=%d", count)
	}
}

func TestCreateRepository_SyncType_MissingExternalURL(t *testing.T) {
	defer setupTestDB(t)()

	w := postJSON(newRepoRouter("u1"), "/api/repositories", map[string]interface{}{
		"name": "sync-repo", "ownerId": "u1", "repoType": "sync",
		"syncRegistry": map[string]interface{}{},
	})
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", w.Code)
	}
}

func TestCreateRepository_SyncType_Success(t *testing.T) {
	defer setupTestDB(t)()

	w := postJSON(newRepoRouter("u1"), "/api/repositories", map[string]interface{}{
		"name": "sync-repo2", "ownerId": "u1", "repoType": "sync",
		"syncRegistry": map[string]interface{}{
			"externalUrl": "https://github.com/example/repo",
		},
	})
	if w.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", w.Code, w.Body.String())
	}
	var body map[string]interface{}
	json.NewDecoder(w.Body).Decode(&body)
	if body["repository"] == nil {
		t.Fatal("expected repository field in response")
	}
	if body["registries"] == nil {
		t.Fatal("expected registries field in response")
	}
}

// ---------------------------------------------------------------------------
// GetRepository
// ---------------------------------------------------------------------------

func TestGetRepository_Found(t *testing.T) {
	defer setupTestDB(t)()
	database.DB.Create(&models.Repository{ID: "repo-g1", Name: "get-repo", OwnerID: "u1"})

	w := get(newRepoRouter(""), "/api/repositories/repo-g1")
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	var repo map[string]interface{}
	json.NewDecoder(w.Body).Decode(&repo)
	if repo["id"] != "repo-g1" {
		t.Fatalf("unexpected id: %v", repo["id"])
	}
}

func TestGetRepository_NotFound(t *testing.T) {
	defer setupTestDB(t)()
	w := get(newRepoRouter(""), "/api/repositories/no-such-repo")
	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", w.Code)
	}
}

// ---------------------------------------------------------------------------
// UpdateRepository
// ---------------------------------------------------------------------------

func TestUpdateRepository_Success(t *testing.T) {
	defer setupTestDB(t)()
	database.DB.Create(&models.Repository{ID: "repo-u1", Name: "old-name", OwnerID: "u1", Visibility: "private"})

	w := putJSON(newRepoRouter("u1"), "/api/repositories/repo-u1", map[string]interface{}{
		"name": "new-name", "visibility": "public",
	})
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var repo map[string]interface{}
	json.NewDecoder(w.Body).Decode(&repo)
	if repo["name"] != "new-name" {
		t.Fatalf("expected name=new-name, got %v", repo["name"])
	}
	if repo["visibility"] != "public" {
		t.Fatalf("expected visibility=public, got %v", repo["visibility"])
	}
}

func TestUpdateRepository_NotFound(t *testing.T) {
	defer setupTestDB(t)()
	w := putJSON(newRepoRouter("u1"), "/api/repositories/no-such", map[string]interface{}{
		"name": "x",
	})
	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", w.Code)
	}
}

func TestUpdateRepository_PartialUpdate(t *testing.T) {
	defer setupTestDB(t)()
	database.DB.Create(&models.Repository{ID: "repo-u2", Name: "partial-repo", DisplayName: "Old Display", OwnerID: "u1"})

	w := putJSON(newRepoRouter("u1"), "/api/repositories/repo-u2", map[string]interface{}{
		"displayName": "New Display",
	})
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	var repo map[string]interface{}
	json.NewDecoder(w.Body).Decode(&repo)
	if repo["name"] != "partial-repo" {
		t.Fatalf("name should not change, got %v", repo["name"])
	}
	if repo["displayName"] != "New Display" {
		t.Fatalf("expected displayName=New Display, got %v", repo["displayName"])
	}
}

// ---------------------------------------------------------------------------
// DeleteRepository
// ---------------------------------------------------------------------------

func TestDeleteRepository_Success(t *testing.T) {
	defer setupTestDB(t)()
	database.DB.Create(&models.Repository{ID: "repo-d1", Name: "del-repo", OwnerID: "u1"})

	w := deleteReq(newRepoRouter("u1"), "/api/repositories/repo-d1")
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}

	var count int64
	database.DB.Model(&models.Repository{}).Where("id = ?", "repo-d1").Count(&count)
	if count != 0 {
		t.Fatal("repository should have been deleted")
	}
}

// ---------------------------------------------------------------------------
// ListRepositoryMembers
// ---------------------------------------------------------------------------

func TestListRepositoryMembers_Empty(t *testing.T) {
	defer setupTestDB(t)()

	w := get(newRepoRouter(""), "/api/repositories/repo-no-members/members")
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	var body map[string]interface{}
	json.NewDecoder(w.Body).Decode(&body)
	members := body["members"].([]interface{})
	if len(members) != 0 {
		t.Fatalf("expected 0 members, got %d", len(members))
	}
}

func TestListRepositoryMembers_WithMembers(t *testing.T) {
	defer setupTestDB(t)()
	database.DB.Create(&models.Repository{ID: "repo-m1", Name: "member-repo", OwnerID: "u1"})
	database.DB.Create(&models.RepoMember{ID: "mem-m1", RepoID: "repo-m1", UserID: "u1", Role: "owner"})
	database.DB.Create(&models.RepoMember{ID: "mem-m2", RepoID: "repo-m1", UserID: "u2", Role: "member"})

	w := get(newRepoRouter(""), "/api/repositories/repo-m1/members")
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	var body map[string]interface{}
	json.NewDecoder(w.Body).Decode(&body)
	members := body["members"].([]interface{})
	if len(members) != 2 {
		t.Fatalf("expected 2 members, got %d", len(members))
	}
}

// ---------------------------------------------------------------------------
// AddRepositoryMember
// ---------------------------------------------------------------------------

func TestAddRepositoryMember_Success(t *testing.T) {
	defer setupTestDB(t)()
	database.DB.Create(&models.Repository{ID: "repo-am1", Name: "add-member-repo", OwnerID: "u1"})
	database.DB.Create(&models.RepoMember{ID: "mem-am1-owner", RepoID: "repo-am1", UserID: "u1", Role: "owner"})

	w := postJSON(newRepoRouter("u1"), "/api/repositories/repo-am1/members", map[string]interface{}{
		"userId": "u-new", "username": "newuser", "role": "member",
	})
	if w.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", w.Code, w.Body.String())
	}
	var member map[string]interface{}
	json.NewDecoder(w.Body).Decode(&member)
	if member["userId"] != "u-new" {
		t.Fatalf("unexpected userId: %v", member["userId"])
	}
	if member["role"] != "member" {
		t.Fatalf("expected role=member, got %v", member["role"])
	}
}

func TestAddRepositoryMember_DefaultRole(t *testing.T) {
	defer setupTestDB(t)()
	database.DB.Create(&models.Repository{ID: "repo-am2", Name: "default-role-repo", OwnerID: "u1"})
	database.DB.Create(&models.RepoMember{ID: "mem-am2-owner", RepoID: "repo-am2", UserID: "u1", Role: "owner"})

	w := postJSON(newRepoRouter("u1"), "/api/repositories/repo-am2/members", map[string]interface{}{
		"userId": "u-default",
	})
	if w.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", w.Code, w.Body.String())
	}
	var member map[string]interface{}
	json.NewDecoder(w.Body).Decode(&member)
	if member["role"] != "member" {
		t.Fatalf("expected default role=member, got %v", member["role"])
	}
}

func TestAddRepositoryMember_MissingUserID(t *testing.T) {
	defer setupTestDB(t)()
	database.DB.Create(&models.Repository{ID: "repo-am3", Name: "missing-uid-repo", OwnerID: "u1"})
	database.DB.Create(&models.RepoMember{ID: "mem-am3-owner", RepoID: "repo-am3", UserID: "u1", Role: "owner"})

	w := postJSON(newRepoRouter("u1"), "/api/repositories/repo-am3/members", map[string]interface{}{
		"username": "no-user-id",
	})
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", w.Code)
	}
}

func TestAddRepositoryMember_Duplicate(t *testing.T) {
	defer setupTestDB(t)()
	database.DB.Create(&models.Repository{ID: "repo-am4", Name: "dup-repo", OwnerID: "u1"})
	database.DB.Create(&models.RepoMember{ID: "mem-am4-owner", RepoID: "repo-am4", UserID: "u1", Role: "owner"})
	database.DB.Create(&models.RepoMember{ID: "mem-dup1", RepoID: "repo-am4", UserID: "u-dup", Role: "member"})

	w := postJSON(newRepoRouter("u1"), "/api/repositories/repo-am4/members", map[string]interface{}{
		"userId": "u-dup",
	})
	if w.Code != http.StatusConflict {
		t.Fatalf("expected 409, got %d", w.Code)
	}
}

// ---------------------------------------------------------------------------
// RemoveRepositoryMember
// ---------------------------------------------------------------------------

func TestRemoveRepositoryMember_Success(t *testing.T) {
	defer setupTestDB(t)()
	database.DB.Create(&models.Repository{ID: "repo-rm1", Name: "remove-repo", OwnerID: "u1"})
	database.DB.Create(&models.RepoMember{ID: "mem-rm1-owner", RepoID: "repo-rm1", UserID: "u1", Role: "owner"})
	database.DB.Create(&models.RepoMember{ID: "mem-rm1", RepoID: "repo-rm1", UserID: "u-remove", Role: "member"})

	w := deleteReq(newRepoRouter("u1"), "/api/repositories/repo-rm1/members/u-remove")
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}

	var count int64
	database.DB.Model(&models.RepoMember{}).Where("repo_id = ? AND user_id = ?", "repo-rm1", "u-remove").Count(&count)
	if count != 0 {
		t.Fatal("member should have been removed")
	}
}

// ---------------------------------------------------------------------------
// GetRepositoryRegistry
// ---------------------------------------------------------------------------

func TestGetRepositoryRegistry_Found(t *testing.T) {
	defer setupTestDB(t)()
	database.DB.Create(&models.Repository{ID: "repo-gr1", Name: "reg-repo", OwnerID: "u1"})
	database.DB.Create(&models.CapabilityRegistry{
		ID: "reg-for-repo", Name: "reg-repo", SourceType: "internal", RepoID: "repo-gr1", OwnerID: "u1",
	})

	w := get(newRepoRouter(""), "/api/repositories/repo-gr1/registry")
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	var reg map[string]interface{}
	json.NewDecoder(w.Body).Decode(&reg)
	if reg["id"] != "reg-for-repo" {
		t.Fatalf("unexpected id: %v", reg["id"])
	}
}

func TestGetRepositoryRegistry_NotFound(t *testing.T) {
	defer setupTestDB(t)()
	w := get(newRepoRouter(""), "/api/repositories/no-such-repo/registry")
	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", w.Code)
	}
}

func TestGetRepositoryRegistry_ExternalRegistryNotReturned(t *testing.T) {
	defer setupTestDB(t)()
	database.DB.Create(&models.Repository{ID: "repo-gr2", Name: "ext-reg-repo", OwnerID: "u1"})
	database.DB.Create(&models.CapabilityRegistry{
		ID: "ext-reg-for-repo", Name: "ext-reg-repo", SourceType: "external", RepoID: "repo-gr2", OwnerID: "u1",
	})

	w := get(newRepoRouter(""), "/api/repositories/repo-gr2/registry")
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 (external registry returned first), got %d", w.Code)
	}
}

// ---------------------------------------------------------------------------
// GetMyRepositories
// ---------------------------------------------------------------------------

func TestGetMyRepositories_Success(t *testing.T) {
	defer setupTestDB(t)()
	database.DB.Create(&models.Repository{ID: "repo-my1", Name: "my-repo-1", OwnerID: "u1"})
	database.DB.Create(&models.Repository{ID: "repo-my2", Name: "my-repo-2", OwnerID: "u2"})
	database.DB.Create(&models.RepoMember{ID: "mem-my1", RepoID: "repo-my1", UserID: "u-me", Role: "member"})
	database.DB.Create(&models.RepoMember{ID: "mem-my2", RepoID: "repo-my2", UserID: "u-me", Role: "admin"})

	w := get(newRepoRouter("u-me"), "/api/repositories/my")
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	var body map[string]interface{}
	json.NewDecoder(w.Body).Decode(&body)
	repos := body["repositories"].([]interface{})
	if len(repos) != 2 {
		t.Fatalf("expected 2 repos, got %d", len(repos))
	}
}

func TestGetMyRepositories_Unauthenticated(t *testing.T) {
	defer setupTestDB(t)()
	w := get(newRepoRouter(""), "/api/repositories/my")
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d", w.Code)
	}
}

func TestGetMyRepositories_NoMemberships(t *testing.T) {
	defer setupTestDB(t)()

	w := get(newRepoRouter("u-nobody"), "/api/repositories/my")
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", w.Code)
	}
	var body map[string]interface{}
	json.NewDecoder(w.Body).Decode(&body)
	repos, _ := body["repositories"].([]interface{})
	if len(repos) != 0 {
		t.Fatalf("expected 0 repos, got %d", len(repos))
	}
}
