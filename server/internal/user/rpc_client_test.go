package user

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/costrict/costrict-web/server/internal/config"
	"github.com/costrict/costrict-web/server/internal/models"
	"gorm.io/gorm"
)

func newConfiguredRPCClient(t *testing.T, baseURL string) *RPCClient {
	t.Helper()
	return NewRPCClient(config.UserServiceConfig{
		Backend:       "rpc",
		BaseURL:       baseURL,
		InternalToken: "test-token",
		TimeoutSec:    2,
	})
}

func strPtr(s string) *string { return &s }

func TestRPCClient_NotConfigured(t *testing.T) {
	c := NewRPCClient(config.UserServiceConfig{Backend: "rpc"}) // empty URL/token
	if c.Configured() {
		t.Fatal("expected Configured()=false with empty url/token")
	}
	ctx := context.Background()
	if _, err := c.GetUserByID(ctx, "u1"); !errors.Is(err, ErrNotConfigured) {
		t.Fatalf("GetUserByID: want ErrNotConfigured, got %v", err)
	}
	if _, err := c.GetUsersByIDs(ctx, []string{"u1"}); !errors.Is(err, ErrNotConfigured) {
		t.Fatalf("GetUsersByIDs: want ErrNotConfigured, got %v", err)
	}
	if _, err := c.SearchUsers(ctx, "alice", 5); !errors.Is(err, ErrNotConfigured) {
		t.Fatalf("SearchUsers: want ErrNotConfigured, got %v", err)
	}
	if _, err := c.ListUserIdentities(ctx, "u1"); !errors.Is(err, ErrNotConfigured) {
		t.Fatalf("ListUserIdentities: want ErrNotConfigured, got %v", err)
	}
}

func TestRPCClient_GetUserByID_HappyPath(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Internal-Token") != "test-token" {
			t.Errorf("missing/incorrect internal token header: %q", r.Header.Get("X-Internal-Token"))
		}
		if r.URL.Path != "/api/internal/users/usr_123" {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(models.User{
			SubjectID: "usr_123", Username: "alice", DisplayName: strPtr("Alice"),
		})
	}))
	defer srv.Close()

	c := newConfiguredRPCClient(t, srv.URL)
	got, err := c.GetUserByID(context.Background(), "usr_123")
	if err != nil {
		t.Fatalf("GetUserByID: %v", err)
	}
	if got.SubjectID != "usr_123" || got.Username != "alice" {
		t.Fatalf("unexpected user: %+v", got)
	}
	if got.DisplayName == nil || *got.DisplayName != "Alice" {
		t.Fatalf("unexpected display name: %v", got.DisplayName)
	}
}

func TestRPCClient_GetUserByID_NotFound(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = io.WriteString(w, `{"error":"user not found"}`)
	}))
	defer srv.Close()

	c := newConfiguredRPCClient(t, srv.URL)
	_, err := c.GetUserByID(context.Background(), "missing")
	if !errors.Is(err, gorm.ErrRecordNotFound) {
		t.Fatalf("want gorm.ErrRecordNotFound, got %v", err)
	}
}

func TestRPCClient_GetUserByID_ServerError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	c := newConfiguredRPCClient(t, srv.URL)
	_, err := c.GetUserByID(context.Background(), "u1")
	if !errors.Is(err, ErrRPCUnavailable) {
		t.Fatalf("want ErrRPCUnavailable, got %v", err)
	}
}

func TestRPCClient_GetUserByID_Timeout(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(500 * time.Millisecond)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	c := NewRPCClient(config.UserServiceConfig{
		Backend:       "rpc",
		BaseURL:       srv.URL,
		InternalToken: "test-token",
		TimeoutSec:    1, // 1s > request deadline exercised via context
	})
	// Override context with very short deadline to force timeout path.
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	_, err := c.GetUserByID(ctx, "u1")
	if !errors.Is(err, ErrRPCUnavailable) {
		t.Fatalf("want ErrRPCUnavailable on timeout, got %v", err)
	}
}

func TestRPCClient_GetUsersByIDs(t *testing.T) {
	t.Run("happy", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/api/internal/users/by-ids" || r.Method != http.MethodPost {
				t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
			}
			var req struct {
				IDs []string `json:"ids"`
			}
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				t.Fatalf("decode body: %v", err)
			}
			if len(req.IDs) != 2 || req.IDs[0] != "u1" || req.IDs[1] != "u2" {
				t.Fatalf("unexpected ids: %v", req.IDs)
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"users":{"u1":{"subject_id":"u1","username":"alice"},"u2":{"subject_id":"u2","username":"bob"}}}`)
		}))
		defer srv.Close()

		c := newConfiguredRPCClient(t, srv.URL)
		got, err := c.GetUsersByIDs(context.Background(), []string{"u1", "u2"})
		if err != nil {
			t.Fatalf("GetUsersByIDs: %v", err)
		}
		if len(got) != 2 || got["u1"] == nil || got["u2"] == nil {
			t.Fatalf("unexpected result: %+v", got)
		}
	})

	t.Run("empty_response", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, _ = io.WriteString(w, `{"users":{}}`)
		}))
		defer srv.Close()

		c := newConfiguredRPCClient(t, srv.URL)
		got, err := c.GetUsersByIDs(context.Background(), []string{"u1"})
		if err != nil {
			t.Fatalf("GetUsersByIDs: %v", err)
		}
		if len(got) != 0 {
			t.Fatalf("expected empty map, got %+v", got)
		}
	})

	t.Run("malformed_body", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, _ = io.WriteString(w, `{not json`)
		}))
		defer srv.Close()

		c := newConfiguredRPCClient(t, srv.URL)
		_, err := c.GetUsersByIDs(context.Background(), []string{"u1"})
		if err == nil {
			t.Fatal("expected error on malformed body, got nil")
		}
	})
}

func TestRPCClient_SearchUsers(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/internal/users/search" {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		if r.URL.Query().Get("keyword") != "alice" {
			t.Errorf("unexpected keyword: %s", r.URL.Query().Get("keyword"))
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"users":[{"subject_id":"u1","username":"alice"}]}`)
	}))
	defer srv.Close()

	c := newConfiguredRPCClient(t, srv.URL)
	got, err := c.SearchUsers(context.Background(), "alice", 5)
	if err != nil {
		t.Fatalf("SearchUsers: %v", err)
	}
	if len(got) != 1 || got[0].SubjectID != "u1" {
		t.Fatalf("unexpected: %+v", got)
	}
}

func TestRPCClient_SearchUsers_Empty(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"users":[]}`)
	}))
	defer srv.Close()

	c := newConfiguredRPCClient(t, srv.URL)
	got, err := c.SearchUsers(context.Background(), "nobody", 5)
	if err != nil {
		t.Fatalf("SearchUsers: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("expected empty slice, got %+v", got)
	}
}

func TestRPCClient_ListUserIdentities(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/internal/users/usr_123/auth-identities" {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		_, _ = io.WriteString(w, `{"identities":[{"user_subject_id":"usr_123","provider":"github","external_key":"gh|1"}]}`)
	}))
	defer srv.Close()

	c := newConfiguredRPCClient(t, srv.URL)
	got, err := c.ListUserIdentities(context.Background(), "usr_123")
	if err != nil {
		t.Fatalf("ListUserIdentities: %v", err)
	}
	if len(got) != 1 || got[0].Provider != "github" {
		t.Fatalf("unexpected: %+v", got)
	}
}

func TestRPCClient_ListUserIdentities_NotFound(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	c := newConfiguredRPCClient(t, srv.URL)
	_, err := c.ListUserIdentities(context.Background(), "missing")
	if !errors.Is(err, gorm.ErrRecordNotFound) {
		t.Fatalf("want gorm.ErrRecordNotFound, got %v", err)
	}
}
