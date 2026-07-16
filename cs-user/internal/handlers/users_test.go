package handlers

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/costrict/costrict-web/cs-user/internal/models"
	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
)

func init() { gin.SetMode(gin.TestMode) }

// stubUserService lets handlers tests pin responses without a DB.
type stubUserService struct {
	getByID     func(string) (*models.User, error)
	getByIDs    func([]string) (map[string]*models.User, error)
	searchUsers func(string, int) ([]*models.User, error)
}

func (s stubUserService) GetUserByID(id string) (*models.User, error) {
	return s.getByID(id)
}
func (s stubUserService) GetUsersByIDs(ids []string) (map[string]*models.User, error) {
	return s.getByIDs(ids)
}
func (s stubUserService) SearchUsers(kw string, lim int) ([]*models.User, error) {
	return s.searchUsers(kw, lim)
}

func newUsersAPI(svc UserService) (*UsersAPI, *gin.Engine) {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	api := &UsersAPI{Svc: svc}
	r.GET("/api/internal/users/:subject_id", api.GetUser)
	r.POST("/api/internal/users/by-ids", api.GetUsersByIDs)
	r.GET("/api/internal/users/search", api.SearchUsers)
	return api, r
}

func doJSON(t *testing.T, r *gin.Engine, method, path string, body any) *httptest.ResponseRecorder {
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

func TestGetUser_HappyPath(t *testing.T) {
	want := &models.User{SubjectID: "subj-1", Username: "alice"}
	api, r := newUsersAPI(stubUserService{
		getByID: func(id string) (*models.User, error) {
			if id != "subj-1" {
				t.Errorf("handler passed id=%q, want subj-1", id)
			}
			return want, nil
		},
	})
	_ = api

	w := doJSON(t, r, http.MethodGet, "/api/internal/users/subj-1", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", w.Code, w.Body.String())
	}
	var got models.User
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal: %v (body=%s)", err, w.Body.String())
	}
	if got.SubjectID != "subj-1" || got.Username != "alice" {
		t.Errorf("got %+v, want subj-1/alice", got)
	}
}

func TestGetUser_NotFound(t *testing.T) {
	_, r := newUsersAPI(stubUserService{
		getByID: func(string) (*models.User, error) { return nil, gorm.ErrRecordNotFound },
	})

	w := doJSON(t, r, http.MethodGet, "/api/internal/users/missing", nil)
	if w.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", w.Code)
	}
}

func TestGetUser_ServiceError(t *testing.T) {
	_, r := newUsersAPI(stubUserService{
		getByID: func(string) (*models.User, error) { return nil, errors.New("db dead") },
	})

	w := doJSON(t, r, http.MethodGet, "/api/internal/users/x", nil)
	if w.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500", w.Code)
	}
	// Body must NOT echo the internal error string — avoid leaking.
	if bytes.Contains(w.Body.Bytes(), []byte("db dead")) {
		t.Errorf("body leaks internal error: %s", w.Body.String())
	}
}

func TestGetUsersByIDs_HappyPath(t *testing.T) {
	_, r := newUsersAPI(stubUserService{
		getByIDs: func(ids []string) (map[string]*models.User, error) {
			if len(ids) != 2 || ids[0] != "a" || ids[1] != "b" {
				t.Errorf("handler passed ids=%v, want [a b]", ids)
			}
			return map[string]*models.User{
				"a": {SubjectID: "a", Username: "alice"},
			}, nil
		},
	})

	w := doJSON(t, r, http.MethodPost, "/api/internal/users/by-ids", map[string]any{
		"ids": []string{"a", "b"},
	})
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", w.Code, w.Body.String())
	}
	var resp struct {
		Users map[string]*models.User `json:"users"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v (body=%s)", err, w.Body.String())
	}
	if len(resp.Users) != 1 || resp.Users["a"].Username != "alice" {
		t.Errorf("got %+v", resp.Users)
	}
}

func TestGetUsersByIDs_RejectsEmptyBody(t *testing.T) {
	_, r := newUsersAPI(stubUserService{})

	w := doJSON(t, r, http.MethodPost, "/api/internal/users/by-ids", map[string]any{})
	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", w.Code)
	}
}

func TestGetUsersByIDs_RejectsOversizedBatch(t *testing.T) {
	_, r := newUsersAPI(stubUserService{})

	big := make([]string, 501)
	for i := range big {
		big[i] = "x"
	}
	w := doJSON(t, r, http.MethodPost, "/api/internal/users/by-ids", map[string]any{"ids": big})
	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400 (max=500)", w.Code)
	}
}

func TestSearchUsers_HappyPath(t *testing.T) {
	called := false
	_, r := newUsersAPI(stubUserService{
		searchUsers: func(kw string, lim int) ([]*models.User, error) {
			called = true
			if kw != "ali" {
				t.Errorf("keyword: got %q want ali", kw)
			}
			// Handler passes 0 when the limit query is unset; the service is
			// responsible for substituting defaultSearchLimit. Asserting here
			// would couple the handler to the service's default.
			if lim != 0 {
				t.Errorf("limit: got %d want 0 (handler passes raw, service applies default)", lim)
			}
			return []*models.User{{SubjectID: "a", Username: "alice"}}, nil
		},
	})

	w := doJSON(t, r, http.MethodGet, "/api/internal/users/search?keyword=ali", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", w.Code, w.Body.String())
	}
	if !called {
		t.Error("SearchUsers not invoked")
	}
}

func TestSearchUsers_ClampsLimit(t *testing.T) {
	_, r := newUsersAPI(stubUserService{
		searchUsers: func(_ string, lim int) ([]*models.User, error) {
			if lim != 200 {
				t.Errorf("limit not clamped: got %d want 200", lim)
			}
			return nil, nil
		},
	})

	w := doJSON(t, r, http.MethodGet, "/api/internal/users/search?limit=99999", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", w.Code, w.Body.String())
	}
}

func TestSearchUsers_RejectsNegativeLimit(t *testing.T) {
	_, r := newUsersAPI(stubUserService{})

	w := doJSON(t, r, http.MethodGet, "/api/internal/users/search?limit=-1", nil)
	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", w.Code)
	}
}

func TestSearchUsers_RejectsGarbageLimit(t *testing.T) {
	_, r := newUsersAPI(stubUserService{})

	w := doJSON(t, r, http.MethodGet, "/api/internal/users/search?limit=abc", nil)
	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", w.Code)
	}
}
