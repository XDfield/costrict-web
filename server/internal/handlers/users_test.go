package handlers

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/costrict/costrict-web/server/internal/database"
	"github.com/costrict/costrict-web/server/internal/models"
	userpkg "github.com/costrict/costrict-web/server/internal/user"
	"github.com/gin-gonic/gin"
)

func newUserRouter() *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.GET("/api/users/info", GetUserBasicInfo)
	r.GET("/api/users/names", GetUserNames)
	return r
}

func newSearchUsersRouter(accessToken string) *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	injectAuth := func(c *gin.Context) {
		if accessToken != "" {
			c.Set("accessToken", accessToken)
		}
		c.Next()
	}
	r.GET("/api/users/search", injectAuth, SearchUsers)
	return r
}

func TestGetUserBasicInfo_Success(t *testing.T) {
	defer setupTestDB(t)()
	defer InitUserModule(nil)
	InitUserModule(userpkg.New(database.DB))

	avatarURL := "https://example.com/avatar.png"
	displayName := "Alice"
	if err := database.DB.Create(&models.User{
		SubjectID:   "u1",
		Username:    "alice",
		DisplayName: &displayName,
		AvatarURL:   &avatarURL,
		IsActive:    true,
	}).Error; err != nil {
		t.Fatalf("failed to seed user: %v", err)
	}

	w := get(newUserRouter(), "/api/users/info?id=u1")
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	var body struct {
		User struct {
			ID        string  `json:"id"`
			Name      string  `json:"name"`
			AvatarURL *string `json:"avatarUrl"`
		} `json:"user"`
	}
	if err := json.NewDecoder(w.Body).Decode(&body); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}
	if body.User.ID != "u1" {
		t.Fatalf("expected id=u1, got %q", body.User.ID)
	}
	if body.User.Name != "Alice" {
		t.Fatalf("expected name=Alice, got %q", body.User.Name)
	}
	if body.User.AvatarURL == nil || *body.User.AvatarURL != avatarURL {
		t.Fatalf("expected avatarUrl=%q, got %#v", avatarURL, body.User.AvatarURL)
	}
}

func TestGetUserBasicInfo_FallbackToUsername(t *testing.T) {
	defer setupTestDB(t)()
	defer InitUserModule(nil)
	InitUserModule(userpkg.New(database.DB))

	if err := database.DB.Create(&models.User{
		SubjectID: "u2",
		Username: "bob",
		IsActive: true,
	}).Error; err != nil {
		t.Fatalf("failed to seed user: %v", err)
	}

	w := get(newUserRouter(), "/api/users/info?id=u2")
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	var body struct {
		User struct {
			Name string `json:"name"`
		} `json:"user"`
	}
	if err := json.NewDecoder(w.Body).Decode(&body); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}
	if body.User.Name != "bob" {
		t.Fatalf("expected name=bob, got %q", body.User.Name)
	}
}

func TestGetUserBasicInfo_MissingID(t *testing.T) {
	defer setupTestDB(t)()
	defer InitUserModule(nil)
	InitUserModule(userpkg.New(database.DB))

	w := get(newUserRouter(), "/api/users/info")
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", w.Code)
	}
}

func TestGetUserBasicInfo_NotFound(t *testing.T) {
	defer setupTestDB(t)()
	defer InitUserModule(nil)
	InitUserModule(userpkg.New(database.DB))

	w := get(newUserRouter(), "/api/users/info?id=missing")
	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d: %s", w.Code, w.Body.String())
	}
}

// ---------------------------------------------------------------------------
// SearchUsers
// ---------------------------------------------------------------------------

func TestSearchUsers_Unauthenticated(t *testing.T) {
	defer setupTestDB(t)()

	r := newSearchUsersRouter("")
	w := get(r, "/api/users/search?q=alice")
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("expected 401, got %d: %s", w.Code, w.Body.String())
	}
}

func TestSearchUsers_EmptyKeyword(t *testing.T) {
	defer setupTestDB(t)()

	r := newSearchUsersRouter("fake-token")
	w := get(r, "/api/users/search")
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for empty keyword, got %d: %s", w.Code, w.Body.String())
	}
}

func TestSearchUsers_WhitespaceOnlyKeyword(t *testing.T) {
	defer setupTestDB(t)()

	r := newSearchUsersRouter("fake-token")
	w := get(r, "/api/users/search?q=%20%20")
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for whitespace-only keyword, got %d: %s", w.Code, w.Body.String())
	}
}

func TestSearchUsers_FieldConvergence(t *testing.T) {
	defer setupTestDB(t)()
	defer InitUserModule(nil)
	InitUserModule(userpkg.New(database.DB))

	email := "alice@example.com"
	phone := "15500000001"
	displayName := "Alice Smith"
	avatarURL := "https://example.com/avatar.png"
	externalKey := "casdoor:uuid-u1"
	providerUserID := "provider-gh-001"
	if err := database.DB.Create(&models.User{
		SubjectID:      "u1",
		Username:       "alice",
		DisplayName:    &displayName,
		Email:          &email,
		Phone:          &phone,
		AvatarURL:      &avatarURL,
		ExternalKey:    &externalKey,
		ProviderUserID: &providerUserID,
		IsActive:       true,
	}).Error; err != nil {
		t.Fatalf("failed to seed user: %v", err)
	}

	r := newSearchUsersRouter("fake-token")
	w := get(r, "/api/users/search?q=alice")
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	var body struct {
		Users []struct {
			ID          string  `json:"id"`
			Name        string  `json:"name"`
			DisplayName *string `json:"displayName"`
			AvatarURL   *string `json:"avatarUrl"`
			Email       *string `json:"email,omitempty"`
			Phone       *string `json:"phone,omitempty"`
			ExternalKey *string `json:"external_key,omitempty"`
		} `json:"users"`
	}
	if err := json.NewDecoder(w.Body).Decode(&body); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}
	if len(body.Users) != 1 {
		t.Fatalf("expected 1 user, got %d", len(body.Users))
	}

	u := body.Users[0]
	if u.ID != "u1" {
		t.Fatalf("expected id=u1, got %q", u.ID)
	}
	if u.Name != "alice" {
		t.Fatalf("expected name=alice, got %q", u.Name)
	}
	if u.DisplayName == nil || *u.DisplayName != "Alice Smith" {
		t.Fatalf("expected displayName=Alice Smith, got %#v", u.DisplayName)
	}
	if u.AvatarURL == nil || *u.AvatarURL != avatarURL {
		t.Fatalf("expected avatarUrl=%q, got %#v", avatarURL, u.AvatarURL)
	}

	// PII fields must NOT be present in the response
	if u.Email != nil {
		t.Fatalf("email must not be exposed in search results, got %q", *u.Email)
	}
	if u.Phone != nil {
		t.Fatalf("phone must not be exposed in search results, got %q", *u.Phone)
	}
	if u.ExternalKey != nil {
		t.Fatalf("external_key must not be exposed in search results, got %q", *u.ExternalKey)
	}
}

func TestSearchUsers_NoPIIInRawJSON(t *testing.T) {
	defer setupTestDB(t)()
	defer InitUserModule(nil)
	InitUserModule(userpkg.New(database.DB))

	email := "sensitive@example.com"
	phone := "13900001111"
	displayName := "Sensitive User"
	if err := database.DB.Create(&models.User{
		SubjectID:   "u-sec",
		Username:    "sensitive",
		DisplayName: &displayName,
		Email:       &email,
		Phone:       &phone,
		IsActive:    true,
	}).Error; err != nil {
		t.Fatalf("failed to seed user: %v", err)
	}

	r := newSearchUsersRouter("fake-token")
	w := get(r, "/api/users/search?q=sensitive")
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	raw := w.Body.String()
	for _, forbidden := range []string{email, phone, "external_key", "provider_user_id", "casdoor_id"} {
		if contains := jsonContains(raw, forbidden); contains {
			t.Fatalf("response must not contain %q, but it did: %s", forbidden, raw)
		}
	}
}

// jsonContains checks if a JSON string contains a given value as a string literal.
func jsonContains(jsonStr, value string) bool {
	return json.Unmarshal([]byte(jsonStr), &struct{}{}) == nil &&
		len(jsonStr) > 0 &&
		containsString(jsonStr, `"`+value+`"`)
}

func containsString(s, substr string) bool {
	for i := 0; i+len(substr) <= len(s); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}
