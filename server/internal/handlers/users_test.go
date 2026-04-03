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
	return r
}

func TestGetUserBasicInfo_Success(t *testing.T) {
	defer setupTestDB(t)()
	InitUserModule(userpkg.New(database.DB))

	avatarURL := "https://example.com/avatar.png"
	displayName := "Alice"
	if err := database.DB.Create(&models.User{
		ID:          "u1",
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
	InitUserModule(userpkg.New(database.DB))

	if err := database.DB.Create(&models.User{
		ID:       "u2",
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
	InitUserModule(userpkg.New(database.DB))

	w := get(newUserRouter(), "/api/users/info")
	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", w.Code)
	}
}

func TestGetUserBasicInfo_NotFound(t *testing.T) {
	defer setupTestDB(t)()
	InitUserModule(userpkg.New(database.DB))

	w := get(newUserRouter(), "/api/users/info?id=missing")
	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d: %s", w.Code, w.Body.String())
	}
}
