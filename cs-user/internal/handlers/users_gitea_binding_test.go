package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/costrict/costrict-web/cs-user/internal/models"
	"github.com/costrict/costrict-web/cs-user/internal/user"
	"gorm.io/gorm"
)

// TestGetGiteaBinding_HappyPath verifies the 200 path: the handler returns
// the binding JSON straight from the service.
func TestGetGiteaBinding_HappyPath(t *testing.T) {
	uid := int64(42)
	binding := &models.UserGiteaBinding{
		UserSubjectID: "usr_1",
		TenantID:      "default",
		GiteaUID:      &uid,
		GiteaUsername: "u-alice",
		SyncStatus:    models.GiteaSyncStatusSynced,
	}
	_, r := newUsersAPI(stubUserService{
		getGiteaBinding: func(_ context.Context, id string) (*models.UserGiteaBinding, error) {
			if id != "usr_1" {
				t.Errorf("subject_id: got %q, want usr_1", id)
			}
			return binding, nil
		},
	})

	w := doJSON(t, r, http.MethodGet, "/api/internal/users/usr_1/gitea-binding", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("status: got %d body=%s", w.Code, w.Body.String())
	}
	var got models.UserGiteaBinding
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.GiteaUsername != "u-alice" {
		t.Errorf("gitea_username: got %q, want u-alice", got.GiteaUsername)
	}
	if got.GiteaUID == nil || *got.GiteaUID != 42 {
		t.Errorf("gitea_uid: got %v, want 42", got.GiteaUID)
	}
	if got.SyncStatus != models.GiteaSyncStatusSynced {
		t.Errorf("sync_status: got %q, want synced", got.SyncStatus)
	}
}

// TestGetGiteaBinding_NotFoundReturns404 verifies the missing-binding case
// maps to 404 (not 500). This is the common case for users who signed up
// before E3a.1 deployed.
func TestGetGiteaBinding_NotFoundReturns404(t *testing.T) {
	_, r := newUsersAPI(stubUserService{
		getGiteaBinding: func(_ context.Context, _ string) (*models.UserGiteaBinding, error) {
			return nil, gorm.ErrRecordNotFound
		},
	})
	w := doJSON(t, r, http.MethodGet, "/api/internal/users/usr_ghost/gitea-binding", nil)
	if w.Code != http.StatusNotFound {
		t.Errorf("status: got %d, want 404; body=%s", w.Code, w.Body.String())
	}
}

// TestGetGiteaBinding_EmptySubjectReturns400 verifies the handler-level
// guard against empty :subject_id — gin cleans `/users//gitea-binding` to
// `/users/:subject_id/gitea-binding` with an empty segment value, so the
// handler still gets called. Our subject_id == "" check returns 400
// (better than letting the service layer reject it as ErrEmptySubjectID
// — we never even hit the service).
func TestGetGiteaBinding_EmptySubjectReturns400(t *testing.T) {
	_, r := newUsersAPI(stubUserService{
		getGiteaBinding: func(context.Context, string) (*models.UserGiteaBinding, error) {
			t.Error("service should not be called when subject_id is empty")
			return nil, nil
		},
	})

	w := doJSON(t, r, http.MethodGet, "/api/internal/users//gitea-binding", nil)
	if w.Code != http.StatusBadRequest {
		t.Errorf("status: got %d, want 400 (handler-level empty-subject guard)", w.Code)
	}
}

// TestGetGiteaBinding_EmptySubjectIDErrorFromServiceReturns400 verifies
// the user.ErrEmptySubjectID branch — defensive depth, since the handler
// already guards empty subject_id above.
func TestGetGiteaBinding_EmptySubjectIDErrorFromServiceReturns400(t *testing.T) {
	_, r := newUsersAPI(stubUserService{
		getGiteaBinding: func(_ context.Context, _ string) (*models.UserGiteaBinding, error) {
			return nil, user.ErrEmptySubjectID
		},
	})
	w := doJSON(t, r, http.MethodGet, "/api/internal/users/usr_x/gitea-binding", nil)
	if w.Code != http.StatusBadRequest {
		t.Errorf("status: got %d, want 400", w.Code)
	}
}

// TestGetGiteaBinding_InternalErrorReturns500 verifies an unexpected
// service failure surfaces as a generic 500 without leaking the
// underlying error message (security: DB connection strings etc.).
func TestGetGiteaBinding_InternalErrorReturns500(t *testing.T) {
	_, r := newUsersAPI(stubUserService{
		getGiteaBinding: func(_ context.Context, _ string) (*models.UserGiteaBinding, error) {
			return nil, errors.New("conn refused: postgres://user:secret@db:5432")
		},
	})
	w := doJSON(t, r, http.MethodGet, "/api/internal/users/usr_x/gitea-binding", nil)
	if w.Code != http.StatusInternalServerError {
		t.Errorf("status: got %d, want 500", w.Code)
	}
	if strings.Contains(w.Body.String(), "secret") {
		t.Errorf("body leaked internal error detail: %s", w.Body.String())
	}
}
