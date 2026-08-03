package gitsync

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
)

// TestClient_ForkRepo_HappyPath verifies the 202 path: the request goes to
// POST /repos/{srcOwner}/{srcRepo}/forks with the user token, and the
// returned repo (under the user's namespace) is projected back.
func TestClient_ForkRepo_HappyPath(t *testing.T) {
	var capturedBody, capturedAuth string
	srv := newDispatchServer(t, dispatch{
		"POST /api/v1/repos/costrict-plugins-repo/cospowers-requirements/forks": func(w http.ResponseWriter, r *http.Request) {
			capturedBody = readBody(t, r.Body)
			capturedAuth = r.Header.Get("Authorization")
			respondJSON(t, w, http.StatusAccepted, Repo{
				ID: 7, Name: "cospowers-requirements",
				FullName: "10001/cospowers-requirements", DefaultBranch: "main",
			})
		},
	})
	c := newClientWithHTTPC(srv.URL, "user-pat", srv.Client())

	repo, err := c.ForkRepo(context.Background(), "costrict-plugins-repo", "cospowers-requirements",
		ForkRepoOptions{TargetOwner: "10001"})
	if err != nil {
		t.Fatalf("ForkRepo: %v", err)
	}
	if repo.FullName != "10001/cospowers-requirements" || repo.DefaultBranch != "main" {
		t.Errorf("unexpected repo: %+v", repo)
	}
	if capturedAuth != "token user-pat" {
		t.Errorf("fork must be signed with the user PAT, got %q", capturedAuth)
	}
	// No rename requested → body must not pin a name, and must never carry
	// `organization` (that would redirect the fork out of the user namespace).
	if strings.Contains(capturedBody, `"name"`) || strings.Contains(capturedBody, `"organization"`) {
		t.Errorf("unexpected fork body: %s", capturedBody)
	}
}

// TestClient_ForkRepo_AlreadyExistsIsIdempotent covers the retry path: Gitea
// answers 409 for a fork that already exists, and ForkRepo converges on the
// existing repo instead of erroring or creating a second one.
func TestClient_ForkRepo_AlreadyExistsIsIdempotent(t *testing.T) {
	forkCalls, getCalls := 0, 0
	srv := newDispatchServer(t, dispatch{
		"POST /api/v1/repos/up/plug/forks": func(w http.ResponseWriter, r *http.Request) {
			forkCalls++
			http.Error(w, `{"message":"The repository with the same name already exists."}`, http.StatusConflict)
		},
		"GET /api/v1/repos/10001/plug": func(w http.ResponseWriter, r *http.Request) {
			getCalls++
			respondJSON(t, w, http.StatusOK, Repo{
				ID: 9, Name: "plug", FullName: "10001/plug", DefaultBranch: "master",
				Fork: true, Parent: &Repo{FullName: "up/plug"},
			})
		},
	})
	c := newClientWithHTTPC(srv.URL, "user-pat", srv.Client())

	repo, err := c.ForkRepo(context.Background(), "up", "plug", ForkRepoOptions{TargetOwner: "10001"})
	if err != nil {
		t.Fatalf("ForkRepo on conflict should recover, got %v", err)
	}
	if repo == nil || repo.ID != 9 || repo.DefaultBranch != "master" {
		t.Fatalf("unexpected repo: %+v", repo)
	}
	if forkCalls != 1 || getCalls != 1 {
		t.Errorf("calls: fork=%d get=%d, want 1/1", forkCalls, getCalls)
	}
}

// Some Gitea builds report a duplicate fork as 500 + "already exists" rather
// than the documented 409; that must recover the same way.
func TestClient_ForkRepo_AlreadyExistsNon409(t *testing.T) {
	srv := newDispatchServer(t, dispatch{
		"POST /api/v1/repos/up/plug/forks": func(w http.ResponseWriter, r *http.Request) {
			http.Error(w, `{"message":"repository already exists"}`, http.StatusInternalServerError)
		},
		"GET /api/v1/repos/10001/plug": func(w http.ResponseWriter, r *http.Request) {
			respondJSON(t, w, http.StatusOK, Repo{
				ID: 9, Name: "plug", FullName: "10001/plug",
				Fork: true, Parent: &Repo{FullName: "up/plug"},
			})
		},
	})
	c := newClientWithHTTPC(srv.URL, "user-pat", srv.Client())

	repo, err := c.ForkRepo(context.Background(), "up", "plug", ForkRepoOptions{TargetOwner: "10001"})
	if err != nil || repo == nil || repo.ID != 9 {
		t.Fatalf("expected idempotent recovery, got repo=%+v err=%v", repo, err)
	}
}

// A name collision with an UNRELATED repo must NOT be mistaken for "already
// forked". Bare repo names (mcp-server, docs, …) recur across upstreams, and
// Gitea answers both cases with the same 409 — accepting the clashing repo
// would wire this item to somebody else's content and record it as truth.
func TestClient_ForkRepo_ConflictWithUnrelatedRepoIsRejected(t *testing.T) {
	srv := newDispatchServer(t, dispatch{
		"POST /api/v1/repos/up/plug/forks": func(w http.ResponseWriter, r *http.Request) {
			http.Error(w, `{"message":"The repository with the same name already exists."}`, http.StatusConflict)
		},
		"GET /api/v1/repos/10001/plug": func(w http.ResponseWriter, r *http.Request) {
			// Same bare name, but a fork of a DIFFERENT upstream.
			respondJSON(t, w, http.StatusOK, Repo{
				ID: 77, Name: "plug", FullName: "10001/plug",
				Fork: true, Parent: &Repo{FullName: "someone-else/plug"},
			})
		},
	})
	c := newClientWithHTTPC(srv.URL, "user-pat", srv.Client())

	repo, err := c.ForkRepo(context.Background(), "up", "plug", ForkRepoOptions{TargetOwner: "10001"})
	if err == nil {
		t.Fatalf("expected rejection of unrelated same-name repo, got repo=%+v", repo)
	}
	if repo != nil {
		t.Errorf("no repo should be returned on lineage mismatch, got %+v", repo)
	}
}

// A plain (non-fork) repo occupying the target name is likewise not a match.
func TestClient_ForkRepo_ConflictWithNonForkRepoIsRejected(t *testing.T) {
	srv := newDispatchServer(t, dispatch{
		"POST /api/v1/repos/up/plug/forks": func(w http.ResponseWriter, r *http.Request) {
			http.Error(w, `{"message":"The repository with the same name already exists."}`, http.StatusConflict)
		},
		"GET /api/v1/repos/10001/plug": func(w http.ResponseWriter, r *http.Request) {
			respondJSON(t, w, http.StatusOK, Repo{ID: 78, Name: "plug", FullName: "10001/plug"})
		},
	})
	c := newClientWithHTTPC(srv.URL, "user-pat", srv.Client())

	if repo, err := c.ForkRepo(context.Background(), "up", "plug", ForkRepoOptions{TargetOwner: "10001"}); err == nil {
		t.Fatalf("expected rejection of non-fork repo, got %+v", repo)
	}
}

// A conflict whose target cannot be found is NOT success — the caller must
// not persist a coordinate that doesn't exist.
func TestClient_ForkRepo_ConflictButTargetMissing(t *testing.T) {
	srv := newDispatchServer(t, dispatch{
		"POST /api/v1/repos/up/plug/forks": func(w http.ResponseWriter, r *http.Request) {
			http.Error(w, `{"message":"already exists"}`, http.StatusConflict)
		},
		"GET /api/v1/repos/10001/plug": func(w http.ResponseWriter, r *http.Request) {
			http.Error(w, `{"message":"not found"}`, http.StatusNotFound)
		},
	})
	c := newClientWithHTTPC(srv.URL, "user-pat", srv.Client())

	if _, err := c.ForkRepo(context.Background(), "up", "plug", ForkRepoOptions{TargetOwner: "10001"}); err == nil {
		t.Fatal("expected an error when the conflicting fork cannot be located")
	}
}

// A 404 on the source repo means "not hosted here" — surfaced as the 404
// sentinel so callers can tell it apart from a transport failure.
func TestClient_ForkRepo_SourceNotFound(t *testing.T) {
	srv := newDispatchServer(t, dispatch{
		"POST /api/v1/repos/up/missing/forks": func(w http.ResponseWriter, r *http.Request) {
			http.Error(w, `{"message":"not found"}`, http.StatusNotFound)
		},
	})
	c := newClientWithHTTPC(srv.URL, "user-pat", srv.Client())

	_, err := c.ForkRepo(context.Background(), "up", "missing", ForkRepoOptions{TargetOwner: "10001"})
	if !errors.Is(err, ErrGiteaNotFound) {
		t.Fatalf("expected 404 sentinel, got %v", err)
	}
	if !isHTTPNotFound(err) {
		t.Errorf("isHTTPNotFound should match the source-missing error: %v", err)
	}
}

func TestClient_ForkRepo_Unreachable(t *testing.T) {
	// Point at a closed port: transport error, no HTTP status.
	c := newClientWithHTTPC("http://127.0.0.1:1", "user-pat", nil)
	_, err := c.ForkRepo(context.Background(), "up", "plug", ForkRepoOptions{TargetOwner: "10001"})
	if !errors.Is(err, ErrGiteaUnreachable) {
		t.Fatalf("expected ErrGiteaUnreachable, got %v", err)
	}
}

func TestClient_ForkRepo_RequiresTargetOwner(t *testing.T) {
	c := newClientWithHTTPC("http://x", "user-pat", nil)
	_, err := c.ForkRepo(context.Background(), "up", "plug", ForkRepoOptions{})
	if err == nil || !strings.Contains(err.Error(), "target owner is required") {
		t.Fatalf("expected target-owner-required error, got %v", err)
	}
	if _, err := c.ForkRepo(context.Background(), "", "plug", ForkRepoOptions{TargetOwner: "10001"}); err == nil {
		t.Fatal("expected source-required error")
	}
}

// A renamed fork pins `name` in the body and looks the rename up on conflict.
func TestClient_ForkRepo_WithRename(t *testing.T) {
	var capturedBody string
	srv := newDispatchServer(t, dispatch{
		"POST /api/v1/repos/up/plug/forks": func(w http.ResponseWriter, r *http.Request) {
			capturedBody = readBody(t, r.Body)
			respondJSON(t, w, http.StatusAccepted, Repo{ID: 3, Name: "plug-fork", FullName: "10001/plug-fork"})
		},
	})
	c := newClientWithHTTPC(srv.URL, "user-pat", srv.Client())

	repo, err := c.ForkRepo(context.Background(), "up", "plug",
		ForkRepoOptions{TargetOwner: "10001", Name: "plug-fork"})
	if err != nil {
		t.Fatalf("ForkRepo: %v", err)
	}
	if repo.Name != "plug-fork" {
		t.Errorf("repo name: want plug-fork, got %q", repo.Name)
	}
	if !strings.Contains(capturedBody, `"name":"plug-fork"`) {
		t.Errorf("body missing rename: %s", capturedBody)
	}
}
