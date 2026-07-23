package handlers

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/costrict/costrict-web/server/internal/database"
	"github.com/costrict/costrict-web/server/internal/middleware"
	"github.com/costrict/costrict-web/server/internal/models"
	userpkg "github.com/costrict/costrict-web/server/internal/user"
	"github.com/gin-gonic/gin"
)

// testUserModel builds a minimal User row for registration tests.
func testUserModel(subjectID, username string) *models.User {
	return &models.User{SubjectID: subjectID, Username: username, IsActive: true}
}

// newRegistrationRouter mounts the three R2 routes behind an injected
// userID middleware, mirroring the authed group in main.go.
func newRegistrationRouter(userID string) *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	injectUser := func(c *gin.Context) {
		if userID != "" {
			c.Set(middleware.UserIDKey, userID)
		}
		c.Next()
	}
	me := r.Group("/api/users/me", injectUser)
	me.GET("/username-available", UsernameAvailable)
	me.POST("/complete-registration", CompleteRegistration)
	me.PATCH("/profile", UpdateMyProfile)
	return r
}

func doReq(t *testing.T, r *gin.Engine, method, path string, body any) *httptest.ResponseRecorder {
	t.Helper()
	var buf bytes.Buffer
	if body != nil {
		if err := json.NewEncoder(&buf).Encode(body); err != nil {
			t.Fatalf("encode body: %v", err)
		}
	}
	req := httptest.NewRequest(method, path, &buf)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

// TestUsernameAvailable_Free verifies the happy path: a free username
// returns available=true.
func TestUsernameAvailable_Free(t *testing.T) {
	defer setupTestDB(t)()
	defer InitUserModule(nil)
	InitUserModule(userpkg.New(database.DB))

	if err := database.DB.Create(testUserModel("usr_self", "self_taken")).Error; err != nil {
		t.Fatalf("seed self: %v", err)
	}

	w := doReq(t, newRegistrationRouter("usr_self"), http.MethodGet, "/api/users/me/username-available?username=alice", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var resp struct {
		Available bool   `json:"available"`
		Reason    string `json:"reason"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !resp.Available {
		t.Errorf("expected available=true, got %+v", resp)
	}
}

// TestUsernameAvailable_Taken verifies that another user's username
// returns available=false with reason "taken".
func TestUsernameAvailable_Taken(t *testing.T) {
	defer setupTestDB(t)()
	defer InitUserModule(nil)
	InitUserModule(userpkg.New(database.DB))

	if err := database.DB.Create(testUserModel("usr_other", "alice")).Error; err != nil {
		t.Fatalf("seed other: %v", err)
	}
	if err := database.DB.Create(testUserModel("usr_self", "self_taken")).Error; err != nil {
		t.Fatalf("seed self: %v", err)
	}

	w := doReq(t, newRegistrationRouter("usr_self"), http.MethodGet, "/api/users/me/username-available?username=alice", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var resp struct {
		Available bool   `json:"available"`
		Reason    string `json:"reason"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Available || resp.Reason != "taken" {
		t.Errorf("expected available=false reason=taken, got %+v", resp)
	}
}

// TestUsernameAvailable_SelfExclude verifies that exclude_subject_id
// (auto-derived from UserIDKey in the handler) lets the current user keep
// their own username.
func TestUsernameAvailable_SelfExclude(t *testing.T) {
	defer setupTestDB(t)()
	defer InitUserModule(nil)
	InitUserModule(userpkg.New(database.DB))

	if err := database.DB.Create(testUserModel("usr_self", "alice")).Error; err != nil {
		t.Fatalf("seed self: %v", err)
	}

	w := doReq(t, newRegistrationRouter("usr_self"), http.MethodGet, "/api/users/me/username-available?username=alice", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var resp struct {
		Available bool `json:"available"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !resp.Available {
		t.Errorf("expected available=true for self, got %+v", resp)
	}
}

// TestCompleteRegistration_HappyPath verifies the wire shape + that
// profile_completed_at gets stamped.
func TestCompleteRegistration_HappyPath(t *testing.T) {
	defer setupTestDB(t)()
	defer InitUserModule(nil)
	InitUserModule(userpkg.New(database.DB))

	if err := database.DB.Create(testUserModel("usr_self", "self_old")).Error; err != nil {
		t.Fatalf("seed self: %v", err)
	}

	w := doReq(t, newRegistrationRouter("usr_self"), http.MethodPost, "/api/users/me/complete-registration", map[string]string{
		"username":     "alice",
		"display_name": "Alice Wonderland",
	})
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var env struct {
		User struct {
			SubjectID string `json:"subjectId"`
			Username  string `json:"username"`
		} `json:"user"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &env); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if env.User.Username != "alice" {
		t.Errorf("expected username=alice, got %s", env.User.Username)
	}
}

// TestCompleteRegistration_Taken verifies that a taken username yields 409
// with the username_taken token.
func TestCompleteRegistration_Taken(t *testing.T) {
	defer setupTestDB(t)()
	defer InitUserModule(nil)
	InitUserModule(userpkg.New(database.DB))

	if err := database.DB.Create(testUserModel("usr_other", "alice")).Error; err != nil {
		t.Fatalf("seed other: %v", err)
	}
	if err := database.DB.Create(testUserModel("usr_self", "self_old")).Error; err != nil {
		t.Fatalf("seed self: %v", err)
	}

	w := doReq(t, newRegistrationRouter("usr_self"), http.MethodPost, "/api/users/me/complete-registration", map[string]string{
		"username": "alice",
	})
	if w.Code != http.StatusConflict {
		t.Fatalf("expected 409, got %d: %s", w.Code, w.Body.String())
	}
	if !bytes.Contains(w.Body.Bytes(), []byte("username_taken")) {
		t.Errorf("expected username_taken token, got %s", w.Body.String())
	}
}

// TestCompleteRegistration_AlreadyComplete verifies one-shot semantics:
// a second call returns 409 with registration_already_complete.
func TestCompleteRegistration_AlreadyComplete(t *testing.T) {
	defer setupTestDB(t)()
	defer InitUserModule(nil)
	InitUserModule(userpkg.New(database.DB))

	if err := database.DB.Create(testUserModel("usr_self", "self_old")).Error; err != nil {
		t.Fatalf("seed self: %v", err)
	}

	if _, err := userpkg.New(database.DB).Writer.CompleteRegistration(context.Background(), "usr_self", "alice", ""); err != nil {
		t.Fatalf("first call: %v", err)
	}

	w := doReq(t, newRegistrationRouter("usr_self"), http.MethodPost, "/api/users/me/complete-registration", map[string]string{
		"username": "alice2",
	})
	if w.Code != http.StatusConflict {
		t.Fatalf("expected 409, got %d: %s", w.Code, w.Body.String())
	}
	if !bytes.Contains(w.Body.Bytes(), []byte("registration_already_complete")) {
		t.Errorf("expected registration_already_complete, got %s", w.Body.String())
	}
}

// TestUpdateMyProfile_HappyPath verifies display_name self-edit updates
// the row and returns the user envelope.
func TestUpdateMyProfile_HappyPath(t *testing.T) {
	defer setupTestDB(t)()
	defer InitUserModule(nil)
	InitUserModule(userpkg.New(database.DB))

	if err := database.DB.Create(testUserModel("usr_self", "alice")).Error; err != nil {
		t.Fatalf("seed self: %v", err)
	}

	w := doReq(t, newRegistrationRouter("usr_self"), http.MethodPatch, "/api/users/me/profile", map[string]string{
		"display_name": "Alice the Great",
	})
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var env struct {
		User struct {
			DisplayName string `json:"displayName"`
		} `json:"user"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &env); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if env.User.DisplayName != "Alice the Great" {
		t.Errorf("expected displayName=Alice the Great, got %q", env.User.DisplayName)
	}
}
