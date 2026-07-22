package gitsync

import (
	"context"
	"encoding/base64"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
)

// TestClient_CreateRepo_HappyPath verifies the admin-path repo creation
// routes correctly and returns the projected Repo.
func TestClient_CreateRepo_HappyPath(t *testing.T) {
	var capturedBody string
	srv := newDispatchServer(t, dispatch{
		"POST /api/v1/admin/users/t-abc12345/repos": func(w http.ResponseWriter, r *http.Request) {
			capturedBody = readBody(t, r.Body)
			respondJSON(t, w, http.StatusCreated, Repo{
				ID: 42, Name: "wf-my-wf", FullName: "t-abc12345/wf-my-wf", Private: true,
			})
		},
	})
	c := newClientWithHTTPC(srv.URL, "tok", srv.Client())

	repo, err := c.CreateRepo(context.Background(), "t-abc12345", CreateRepoOptions{
		Name:          "wf-my-wf",
		Private:       true,
		AutoInit:      true,
		DefaultBranch: "main",
	})
	if err != nil {
		t.Fatalf("CreateRepo: %v", err)
	}
	if repo.ID != 42 || repo.Name != "wf-my-wf" || repo.FullName != "t-abc12345/wf-my-wf" {
		t.Errorf("unexpected repo: %+v", repo)
	}
	if !strings.Contains(capturedBody, `"name":"wf-my-wf"`) {
		t.Errorf("body missing name: %s", capturedBody)
	}
	if !strings.Contains(capturedBody, `"private":true`) {
		t.Errorf("body missing private flag: %s", capturedBody)
	}
	if !strings.Contains(capturedBody, `"auto_init":true`) {
		t.Errorf("body missing auto_init: %s", capturedBody)
	}
}

func TestClient_CreateRepo_AlreadyExists(t *testing.T) {
	srv := newDispatchServer(t, dispatch{
		"POST /api/v1/admin/users/t-abc12345/repos": func(w http.ResponseWriter, r *http.Request) {
			http.Error(w, `{"message":"repo already exists"}`, http.StatusConflict)
		},
	})
	c := newClientWithHTTPC(srv.URL, "tok", srv.Client())

	_, err := c.CreateRepo(context.Background(), "t-abc12345", CreateRepoOptions{Name: "dup"})
	if !errors.Is(err, ErrGiteaUsernameTaken) {
		t.Fatalf("expected ErrGiteaUsernameTaken on 409, got %v", err)
	}
}

func TestClient_CreateRepo_MissingOwner(t *testing.T) {
	c := newClientWithHTTPC("http://x", "tok", nil)
	_, err := c.CreateRepo(context.Background(), "", CreateRepoOptions{Name: "x"})
	if err == nil || !strings.Contains(err.Error(), "owner is required") {
		t.Fatalf("expected owner-required error, got %v", err)
	}
}

func TestClient_GetRepo_HappyPath(t *testing.T) {
	srv := newDispatchServer(t, dispatch{
		"GET /api/v1/repos/t-abc12345/wf-my-wf": func(w http.ResponseWriter, r *http.Request) {
			respondJSON(t, w, http.StatusOK, Repo{ID: 42, Name: "wf-my-wf", FullName: "t-abc12345/wf-my-wf"})
		},
	})
	c := newClientWithHTTPC(srv.URL, "tok", srv.Client())

	repo, err := c.GetRepo(context.Background(), "t-abc12345", "wf-my-wf")
	if err != nil {
		t.Fatalf("GetRepo: %v", err)
	}
	if repo == nil || repo.ID != 42 {
		t.Fatalf("unexpected repo: %+v", repo)
	}
}

func TestClient_GetRepo_NotFoundReturnsNil(t *testing.T) {
	srv := newDispatchServer(t, dispatch{
		"GET /api/v1/repos/t-abc12345/missing": func(w http.ResponseWriter, r *http.Request) {
			http.Error(w, `{"message":"not found"}`, http.StatusNotFound)
		},
	})
	c := newClientWithHTTPC(srv.URL, "tok", srv.Client())

	repo, err := c.GetRepo(context.Background(), "t-abc12345", "missing")
	if err != nil {
		t.Fatalf("expected nil error on 404, got %v", err)
	}
	if repo != nil {
		t.Fatalf("expected nil repo on 404, got %+v", repo)
	}
}

func TestClient_CreateBranch_HappyPath(t *testing.T) {
	var capturedBody string
	srv := newDispatchServer(t, dispatch{
		"POST /api/v1/repos/t-abc12345/wf-my-wf/branches": func(w http.ResponseWriter, r *http.Request) {
			capturedBody = readBody(t, r.Body)
			respondJSON(t, w, http.StatusCreated, map[string]any{
				"branch": map[string]any{"name": "inst-xyz12345"},
			})
		},
	})
	c := newClientWithHTTPC(srv.URL, "tok", srv.Client())

	if err := c.CreateBranch(context.Background(), "t-abc12345", "wf-my-wf", "inst-xyz12345", "main"); err != nil {
		t.Fatalf("CreateBranch: %v", err)
	}
	if !strings.Contains(capturedBody, `"new_branch_name":"inst-xyz12345"`) {
		t.Errorf("body missing new_branch_name: %s", capturedBody)
	}
	if !strings.Contains(capturedBody, `"old_ref_name":"main"`) {
		t.Errorf("body missing old_ref_name: %s", capturedBody)
	}
}

func TestClient_CreateBranch_AlreadyExistsIsIdempotent(t *testing.T) {
	srv := newDispatchServer(t, dispatch{
		"POST /api/v1/repos/t-abc12345/wf-my-wf/branches": func(w http.ResponseWriter, r *http.Request) {
			http.Error(w, `{"message":"branch already exists"}`, http.StatusConflict)
		},
	})
	c := newClientWithHTTPC(srv.URL, "tok", srv.Client())

	if err := c.CreateBranch(context.Background(), "t-abc12345", "wf-my-wf", "inst-existing", "main"); err != nil {
		t.Fatalf("expected nil error on 409 (idempotent), got %v", err)
	}
}

func TestClient_GetBranch_HappyPath(t *testing.T) {
	srv := newDispatchServer(t, dispatch{
		"GET /api/v1/repos/t-abc12345/wf-my-wf/branches/main": func(w http.ResponseWriter, r *http.Request) {
			respondJSON(t, w, http.StatusOK, map[string]any{
				"branch": map[string]any{
					"name":   "main",
					"commit": map[string]any{"id": "abc123commitsha"},
				},
			})
		},
	})
	c := newClientWithHTTPC(srv.URL, "tok", srv.Client())

	br, err := c.GetBranch(context.Background(), "t-abc12345", "wf-my-wf", "main")
	if err != nil {
		t.Fatalf("GetBranch: %v", err)
	}
	if br == nil || br.Name != "main" || br.CommitSHA != "abc123commitsha" {
		t.Fatalf("unexpected branch: %+v", br)
	}
}

func TestClient_GetBranch_NotFoundReturnsNil(t *testing.T) {
	srv := newDispatchServer(t, dispatch{
		"GET /api/v1/repos/t-abc12345/wf-my-wf/branches/inst-missing": func(w http.ResponseWriter, r *http.Request) {
			http.Error(w, `{"message":"not found"}`, http.StatusNotFound)
		},
	})
	c := newClientWithHTTPC(srv.URL, "tok", srv.Client())

	br, err := c.GetBranch(context.Background(), "t-abc12345", "wf-my-wf", "inst-missing")
	if err != nil {
		t.Fatalf("expected nil error on 404, got %v", err)
	}
	if br != nil {
		t.Fatalf("expected nil branch on 404, got %+v", br)
	}
}

func TestClient_SetBranchProtection_HappyPath(t *testing.T) {
	var capturedBody string
	srv := newDispatchServer(t, dispatch{
		"POST /api/v1/repos/t-abc12345/wf-my-wf/branch_protections": func(w http.ResponseWriter, r *http.Request) {
			capturedBody = readBody(t, r.Body)
			respondJSON(t, w, http.StatusCreated, map[string]any{"rule_name": "main"})
		},
	})
	c := newClientWithHTTPC(srv.URL, "tok", srv.Client())

	err := c.SetBranchProtection(context.Background(), "t-abc12345", "wf-my-wf", BranchProtectionOptions{
		RuleName:        "inst-*",
		EnablePush:      false,
		EnableForcePush: false,
	})
	if err != nil {
		t.Fatalf("SetBranchProtection: %v", err)
	}
	if !strings.Contains(capturedBody, `"rule_name":"inst-*"`) {
		t.Errorf("body missing rule_name: %s", capturedBody)
	}
	if strings.Contains(capturedBody, `"enable_push":true`) {
		t.Errorf("body should have enable_push=false: %s", capturedBody)
	}
}

func TestClient_SetBranchProtection_AlreadyExists(t *testing.T) {
	srv := newDispatchServer(t, dispatch{
		"POST /api/v1/repos/t-abc12345/wf-my-wf/branch_protections": func(w http.ResponseWriter, r *http.Request) {
			http.Error(w, `{"message":"rule exists"}`, http.StatusConflict)
		},
	})
	c := newClientWithHTTPC(srv.URL, "tok", srv.Client())

	err := c.SetBranchProtection(context.Background(), "t-abc12345", "wf-my-wf", BranchProtectionOptions{
		RuleName: "main",
	})
	if !errors.Is(err, ErrGiteaUsernameTaken) {
		t.Fatalf("expected ErrGiteaUsernameTaken on 409, got %v", err)
	}
}

func TestClient_WriteFile_HappyPath_CreateStatus(t *testing.T) {
	var capturedBody string
	srv := newDispatchServer(t, dispatch{
		"POST /api/v1/repos/t-abc12345/wf-my-wf/contents/definition_snapshot.json": func(w http.ResponseWriter, r *http.Request) {
			capturedBody = readBody(t, r.Body)
			respondJSON(t, w, http.StatusCreated, map[string]any{"content": map[string]any{"sha": "abc"}})
		},
	})
	c := newClientWithHTTPC(srv.URL, "tok", srv.Client())

	content := []byte(`{"workflow":"my-wf","version":1}`)
	if err := c.WriteFile(context.Background(), "t-abc12345", "wf-my-wf", "main",
		"definition_snapshot.json", content, "init snapshot"); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	// Body must contain base64-encoded content + branch + message.
	expectedB64 := base64.StdEncoding.EncodeToString(content)
	if !strings.Contains(capturedBody, `"content":"`+expectedB64) {
		t.Errorf("body missing base64 content: %s", capturedBody)
	}
	if !strings.Contains(capturedBody, `"branch":"main"`) {
		t.Errorf("body missing branch: %s", capturedBody)
	}
	if !strings.Contains(capturedBody, `"message":"init snapshot"`) {
		t.Errorf("body missing message: %s", capturedBody)
	}
}

func TestClient_WriteFile_AcceptsUpdateStatus(t *testing.T) {
	srv := newDispatchServer(t, dispatch{
		"POST /api/v1/repos/t-abc12345/wf-my-wf/contents/definition_snapshot.json": func(w http.ResponseWriter, r *http.Request) {
			respondJSON(t, w, http.StatusOK, map[string]any{"content": map[string]any{"sha": "def"}})
		},
	})
	c := newClientWithHTTPC(srv.URL, "tok", srv.Client())

	// 200 OK on update — must not be treated as error.
	if err := c.WriteFile(context.Background(), "t-abc12345", "wf-my-wf", "main",
		"definition_snapshot.json", []byte("v2"), ""); err != nil {
		t.Fatalf("WriteFile with 200 update: %v", err)
	}
}

func TestClient_ReadFile_HappyPath(t *testing.T) {
	original := []byte(`{"workflow":"my-wf","version":1}`)
	srv := newDispatchServer(t, dispatch{
		"GET /api/v1/repos/t-abc12345/wf-my-wf/contents/definition_snapshot.json": func(w http.ResponseWriter, r *http.Request) {
			ref := r.URL.Query().Get("ref")
			if ref != "main" {
				t.Errorf("expected ref=main, got %q", ref)
			}
			respondJSON(t, w, http.StatusOK, fileResponse{
				Content:  base64.StdEncoding.EncodeToString(original),
				Encoding: "base64",
			})
		},
	})
	c := newClientWithHTTPC(srv.URL, "tok", srv.Client())

	got, err := c.ReadFile(context.Background(), "t-abc12345", "wf-my-wf", "main", "definition_snapshot.json")
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if string(got) != string(original) {
		t.Fatalf("content mismatch: got %q want %q", got, original)
	}
}

func TestClient_ReadFile_NotFoundReturnsNil(t *testing.T) {
	srv := newDispatchServer(t, dispatch{
		"GET /api/v1/repos/t-abc12345/wf-my-wf/contents/definition_snapshot.json": func(w http.ResponseWriter, r *http.Request) {
			http.Error(w, `{"message":"not found"}`, http.StatusNotFound)
		},
	})
	c := newClientWithHTTPC(srv.URL, "tok", srv.Client())

	got, err := c.ReadFile(context.Background(), "t-abc12345", "wf-my-wf", "main", "definition_snapshot.json")
	if err != nil {
		t.Fatalf("expected nil error on 404, got %v", err)
	}
	if got != nil {
		t.Fatalf("expected nil content on 404, got %q", got)
	}
}

// Ensure imports used by all tests are visible to the compiler even if a
// future test trims the body. io is referenced by readBody in this file's
// sister tests.
var _ = io.Discard
