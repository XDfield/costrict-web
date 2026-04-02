package project

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/costrict/costrict-web/server/internal/middleware"
	"github.com/gin-gonic/gin"
)

func newProjectTestRouter(t *testing.T) *gin.Engine {
	t.Helper()
	gin.SetMode(gin.TestMode)
	r := gin.New()
	db := setupProjectTestDB(t)
	module := NewWithDependencies(db, nil, nil, nil)
	api := r.Group("/api")
	api.Use(func(c *gin.Context) {
		if userID := c.GetHeader("X-User-ID"); userID != "" {
			c.Set(middleware.UserIDKey, userID)
		}
		c.Next()
	})
	module.RegisterRoutes(api)
	return r
}

func performJSON(r *gin.Engine, method, path, userID string, body any) *httptest.ResponseRecorder {
	var reqBody []byte
	if body != nil {
		reqBody, _ = json.Marshal(body)
	}
	req := httptest.NewRequest(method, path, bytes.NewReader(reqBody))
	req.Header.Set("Content-Type", "application/json")
	if userID != "" {
		req.Header.Set("X-User-ID", userID)
	}
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

func decodeBody[T any](t *testing.T, w *httptest.ResponseRecorder) T {
	t.Helper()
	var resp T
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v, body=%s", err, w.Body.String())
	}
	return resp
}

func TestCreateAndListProjectsHandler(t *testing.T) {
	r := newProjectTestRouter(t)
	enabledAt := time.Now().UTC().Format(time.RFC3339)
	w := performJSON(r, http.MethodPost, "/api/projects", "u1", map[string]any{
		"name":        "Project A",
		"description": "demo",
		"enabledAt":   enabledAt,
	})
	if w.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d, body=%s", w.Code, w.Body.String())
	}
	created := decodeBody[ProjectResponse](t, w)
	if created.Project == nil || created.Project.Name != "Project A" {
		t.Fatalf("unexpected project response: %+v", created)
	}

	w = performJSON(r, http.MethodGet, "/api/projects", "u1", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d, body=%s", w.Code, w.Body.String())
	}
	list := decodeBody[ProjectsResponse](t, w)
	if len(list.Projects) != 1 || list.Projects[0].ID != created.Project.ID {
		t.Fatalf("unexpected projects list: %+v", list)
	}
	if list.Projects[0].IsPinned {
		t.Fatalf("expected new project not pinned: %+v", list.Projects[0])
	}
}

func TestPinProjectHandlerAndListFilter(t *testing.T) {
	r := newProjectTestRouter(t)
	projectA := decodeBody[ProjectResponse](t, performJSON(r, http.MethodPost, "/api/projects", "u1", map[string]any{"name": "Project A"})).Project
	projectB := decodeBody[ProjectResponse](t, performJSON(r, http.MethodPost, "/api/projects", "u1", map[string]any{"name": "Project B"})).Project

	w := performJSON(r, http.MethodPut, "/api/projects/"+projectA.ID+"/pin", "u1", map[string]any{"pinned": true})
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d, body=%s", w.Code, w.Body.String())
	}
	pinned := decodeBody[ProjectResponse](t, w).Project
	if pinned == nil || !pinned.IsPinned {
		t.Fatalf("expected pinned project response, got %+v", pinned)
	}

	w = performJSON(r, http.MethodGet, "/api/projects", "u1", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d, body=%s", w.Code, w.Body.String())
	}
	list := decodeBody[ProjectsResponse](t, w)
	if len(list.Projects) != 2 {
		t.Fatalf("expected 2 projects, got %+v", list)
	}
	if list.Projects[0].ID != projectB.ID || list.Projects[0].IsPinned {
		t.Fatalf("expected latest created project first, got %+v", list.Projects)
	}
	if list.Projects[1].ID != projectA.ID || !list.Projects[1].IsPinned {
		t.Fatalf("expected pinned flag retained on older project, got %+v", list.Projects)
	}

	w = performJSON(r, http.MethodGet, "/api/projects?pinned=true", "u1", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d, body=%s", w.Code, w.Body.String())
	}
	filtered := decodeBody[ProjectsResponse](t, w)
	if len(filtered.Projects) != 1 || filtered.Projects[0].ID != projectA.ID || !filtered.Projects[0].IsPinned {
		t.Fatalf("expected only pinned project in filtered list, got %+v", filtered)
	}

	w = performJSON(r, http.MethodGet, "/api/projects/"+projectA.ID, "u1", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d, body=%s", w.Code, w.Body.String())
	}
	detail := decodeBody[ProjectResponse](t, w)
	if detail.Project == nil || !detail.Project.IsPinned {
		t.Fatalf("expected detail response pinned, got %+v", detail)
	}
}

func TestInvitationRespondAndListHandlers(t *testing.T) {
	r := newProjectTestRouter(t)
	w := performJSON(r, http.MethodPost, "/api/projects", "admin", map[string]any{"name": "Project A"})
	project := decodeBody[ProjectResponse](t, w).Project

	w = performJSON(r, http.MethodPost, "/api/projects/"+project.ID+"/invitations", "admin", map[string]any{
		"inviteeId": "member1",
		"role":      "member",
		"message":   "join",
	})
	if w.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d, body=%s", w.Code, w.Body.String())
	}
	inv := decodeBody[InvitationResponse](t, w).Invitation

	w = performJSON(r, http.MethodGet, "/api/invitations", "member1", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d, body=%s", w.Code, w.Body.String())
	}
	myInv := decodeBody[InvitationsResponse](t, w)
	if len(myInv.Invitations) != 1 || myInv.Invitations[0].ID != inv.ID {
		t.Fatalf("unexpected my invitations: %+v", myInv)
	}

	w = performJSON(r, http.MethodPost, "/api/invitations/"+inv.ID+"/respond", "member1", map[string]any{"accept": true})
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d, body=%s", w.Code, w.Body.String())
	}

	w = performJSON(r, http.MethodGet, "/api/projects/"+project.ID+"/members", "member1", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d, body=%s", w.Code, w.Body.String())
	}
	members := decodeBody[MembersResponse](t, w)
	if len(members.Members) != 2 {
		t.Fatalf("expected 2 members, got %+v", members)
	}
}

func TestUpdateMemberRoleHandler(t *testing.T) {
	r := newProjectTestRouter(t)
	project := decodeBody[ProjectResponse](t, performJSON(r, http.MethodPost, "/api/projects", "admin", map[string]any{"name": "Project A"})).Project
	inv := decodeBody[InvitationResponse](t, performJSON(r, http.MethodPost, "/api/projects/"+project.ID+"/invitations", "admin", map[string]any{"inviteeId": "member1", "role": "member"})).Invitation
	performJSON(r, http.MethodPost, "/api/invitations/"+inv.ID+"/respond", "member1", map[string]any{"accept": true})

	w := performJSON(r, http.MethodPut, "/api/projects/"+project.ID+"/members/member1/role", "admin", map[string]any{"role": "admin"})
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d, body=%s", w.Code, w.Body.String())
	}
	member := decodeBody[MemberResponse](t, w).Member
	if member == nil || member.Role != RoleAdmin {
		t.Fatalf("unexpected member response: %+v", member)
	}
}

func TestArchiveAndUnarchiveHandlers(t *testing.T) {
	r := newProjectTestRouter(t)
	project := decodeBody[ProjectResponse](t, performJSON(r, http.MethodPost, "/api/projects", "admin", map[string]any{"name": "Project A"})).Project

	w := performJSON(r, http.MethodPost, "/api/projects/"+project.ID+"/archive", "admin", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d, body=%s", w.Code, w.Body.String())
	}
	archived := decodeBody[ProjectResponse](t, w).Project
	if archived == nil || archived.ArchivedAt == nil {
		t.Fatalf("unexpected archived response: %+v", archived)
	}

	w = performJSON(r, http.MethodPost, "/api/projects/"+project.ID+"/unarchive", "admin", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d, body=%s", w.Code, w.Body.String())
	}
	unarchived := decodeBody[ProjectResponse](t, w).Project
	if unarchived == nil || unarchived.ArchivedAt != nil {
		t.Fatalf("unexpected unarchived response: %+v", unarchived)
	}
}

func TestProjectInvitationRecordHandlers(t *testing.T) {
	r := newProjectTestRouter(t)
	project := decodeBody[ProjectResponse](t, performJSON(r, http.MethodPost, "/api/projects", "admin", map[string]any{"name": "Project A"})).Project
	performJSON(r, http.MethodPost, "/api/projects/"+project.ID+"/invitations", "admin", map[string]any{"inviteeId": "member1", "role": "member"})

	w := performJSON(r, http.MethodGet, "/api/projects/"+project.ID+"/invitations", "admin", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d, body=%s", w.Code, w.Body.String())
	}
	invs := decodeBody[InvitationsResponse](t, w)
	if len(invs.Invitations) != 1 {
		t.Fatalf("expected 1 invitation, got %+v", invs)
	}
}

func TestProjectRepositoryHandlers(t *testing.T) {
	r := newProjectTestRouter(t)
	project := decodeBody[ProjectResponse](t, performJSON(r, http.MethodPost, "/api/projects", "admin", map[string]any{"name": "Project A"})).Project

	w := performJSON(r, http.MethodPost, "/api/projects/"+project.ID+"/repositories", "admin", map[string]any{
		"gitRepoUrl": "git@github.com:zgsm-ai/opencode.git",
		"displayName": "opencode",
	})
	if w.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d, body=%s", w.Code, w.Body.String())
	}

	w = performJSON(r, http.MethodGet, "/api/projects/"+project.ID+"/repositories", "admin", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d, body=%s", w.Code, w.Body.String())
	}
	repos := decodeBody[ListProjectRepositoriesResponse](t, w)
	if len(repos.Repositories) != 1 {
		t.Fatalf("expected 1 repository, got %+v", repos)
	}

	w = performJSON(r, http.MethodDelete, "/api/projects/"+project.ID+"/repositories/"+repos.Repositories[0].ID, "admin", nil)
	if w.Code != http.StatusNoContent {
		t.Fatalf("expected 204, got %d, body=%s", w.Code, w.Body.String())
	}
}
