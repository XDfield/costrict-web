package handlers

import (
	"errors"
	"net/http"
	"testing"

	"github.com/costrict/costrict-web/cs-user/internal/models"
	"github.com/gin-gonic/gin"
)

type stubAuthIdentityService struct {
	listIdentities func(string) ([]*models.UserAuthIdentity, error)
}

func (s stubAuthIdentityService) ListIdentities(id string) ([]*models.UserAuthIdentity, error) {
	return s.listIdentities(id)
}

func newAuthIdentitiesAPI(svc AuthIdentityService) (*AuthIdentitiesAPI, *gin.Engine) {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	api := &AuthIdentitiesAPI{Svc: svc}
	r.GET("/api/internal/users/:subject_id/auth-identities", api.ListIdentities)
	return api, r
}

func TestListIdentities_HappyPath(t *testing.T) {
	want := []*models.UserAuthIdentity{
		{UserSubjectID: "subj-1", Provider: "casdoor", ExternalKey: "casdoor:1"},
	}
	_, r := newAuthIdentitiesAPI(stubAuthIdentityService{
		listIdentities: func(id string) ([]*models.UserAuthIdentity, error) {
			if id != "subj-1" {
				t.Errorf("handler passed id=%q want subj-1", id)
			}
			return want, nil
		},
	})

	w := doJSON(t, r, http.MethodGet, "/api/internal/users/subj-1/auth-identities", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", w.Code, w.Body.String())
	}
}

func TestListIdentities_EmptyResult(t *testing.T) {
	_, r := newAuthIdentitiesAPI(stubAuthIdentityService{
		listIdentities: func(string) ([]*models.UserAuthIdentity, error) { return nil, nil },
	})

	w := doJSON(t, r, http.MethodGet, "/api/internal/users/none/auth-identities", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 even on empty list", w.Code)
	}
}

func TestListIdentities_ServiceError(t *testing.T) {
	_, r := newAuthIdentitiesAPI(stubAuthIdentityService{
		listIdentities: func(string) ([]*models.UserAuthIdentity, error) {
			return nil, errors.New("db down")
		},
	})

	w := doJSON(t, r, http.MethodGet, "/api/internal/users/x/auth-identities", nil)
	if w.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500", w.Code)
	}
	if bytesContains(w.Body.Bytes(), []byte("db down")) {
		t.Errorf("body leaks internal error: %s", w.Body.String())
	}
}

// bytesContains is a tiny wrapper so this file doesn't pull bytes just for
// the leak check.
func bytesContains(haystack, needle []byte) bool {
	if len(needle) == 0 {
		return true
	}
	for i := 0; i+len(needle) <= len(haystack); i++ {
		match := true
		for j := range needle {
			if haystack[i+j] != needle[j] {
				match = false
				break
			}
		}
		if match {
			return true
		}
	}
	return false
}
