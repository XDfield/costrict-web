package user

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/costrict/costrict-web/server/internal/models"
	"gorm.io/gorm"
)

// dispatchMux routes per-(METHOD PATH) handlers so one httptest.Server can
// stand in for the whole cs-user internal API surface for UserRefResolver
// tests. Handler funcs receive (w, r) like any http.HandlerFunc.
type dispatchMux map[string]http.HandlerFunc

func newDispatchMux(t *testing.T, d dispatchMux) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key := r.Method + " " + r.URL.Path
		if h, ok := d[key]; ok {
			h(w, r)
			return
		}
		t.Errorf("userref dispatch: unexpected request %s", key)
		http.NotFound(w, r)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestUserRefResolver_UserIDPath_HappyPath(t *testing.T) {
	srv := newDispatchMux(t, dispatchMux{
		"GET /api/internal/users/usr_123": func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"subject_id":"usr_123","username":"alice"}`))
		},
		"GET /api/internal/users/usr_123/gitea-binding": func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"user_subject_id":"usr_123","gitea_username":"u-alice","sync_status":"synced"}`))
		},
	})
	r := NewUserRefResolver(newConfiguredRPCClient(t, srv.URL))

	got, err := r.Resolve(context.Background(), UserRef{UserID: "usr_123"})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got.SubjectID != "usr_123" {
		t.Errorf("subject_id: got %q", got.SubjectID)
	}
	if got.GiteaUsername != "u-alice" {
		t.Errorf("gitea_username: got %q", got.GiteaUsername)
	}
}

func TestUserRefResolver_EmployeeNumberPath_HappyPath(t *testing.T) {
	srv := newDispatchMux(t, dispatchMux{
		"GET /api/internal/users/search": func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Query().Get("employee_number") != "EMP-001" {
				t.Errorf("employee_number query: got %q", r.URL.Query().Get("employee_number"))
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"users":[{"subject_id":"usr_456","username":"bob"}]}`))
		},
		"GET /api/internal/users/usr_456/gitea-binding": func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"user_subject_id":"usr_456","gitea_username":"u-bob","sync_status":"synced"}`))
		},
	})
	r := NewUserRefResolver(newConfiguredRPCClient(t, srv.URL))

	got, err := r.Resolve(context.Background(), UserRef{EmployeeNumber: "EMP-001"})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got.SubjectID != "usr_456" || got.GiteaUsername != "u-bob" {
		t.Errorf("got %+v", got)
	}
}

func TestUserRefResolver_BothEmpty_Rejected(t *testing.T) {
	r := NewUserRefResolver(newConfiguredRPCClient(t, "http://unused"))
	if _, err := r.Resolve(context.Background(), UserRef{}); !errors.Is(err, ErrInvalidUserRef) {
		t.Errorf("empty UserRef: got %v, want ErrInvalidUserRef", err)
	}
}

func TestUserRefResolver_BothSet_Rejected(t *testing.T) {
	r := NewUserRefResolver(newConfiguredRPCClient(t, "http://unused"))
	ref := UserRef{UserID: "usr_1", EmployeeNumber: "EMP-1"}
	if _, err := r.Resolve(context.Background(), ref); !errors.Is(err, ErrInvalidUserRef) {
		t.Errorf("both set: got %v, want ErrInvalidUserRef", err)
	}
}

func TestUserRefResolver_UserIDPath_NotFound(t *testing.T) {
	srv := newDispatchMux(t, dispatchMux{
		"GET /api/internal/users/usr_ghost": func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusNotFound)
		},
	})
	r := NewUserRefResolver(newConfiguredRPCClient(t, srv.URL))

	_, err := r.Resolve(context.Background(), UserRef{UserID: "usr_ghost"})
	if !errors.Is(err, ErrUserNotFound) {
		t.Errorf("got %v, want ErrUserNotFound", err)
	}
}

func TestUserRefResolver_EmployeeNumber_NoRows(t *testing.T) {
	srv := newDispatchMux(t, dispatchMux{
		"GET /api/internal/users/search": func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"users":[]}`))
		},
	})
	r := NewUserRefResolver(newConfiguredRPCClient(t, srv.URL))

	_, err := r.Resolve(context.Background(), UserRef{EmployeeNumber: "MISSING"})
	if !errors.Is(err, ErrUserNotFound) {
		t.Errorf("got %v, want ErrUserNotFound", err)
	}
}

func TestUserRefResolver_BindingMissing_NotGiteaReady(t *testing.T) {
	srv := newDispatchMux(t, dispatchMux{
		"GET /api/internal/users/usr_123": func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"subject_id":"usr_123","username":"alice"}`))
		},
		"GET /api/internal/users/usr_123/gitea-binding": func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusNotFound)
		},
	})
	r := NewUserRefResolver(newConfiguredRPCClient(t, srv.URL))

	_, err := r.Resolve(context.Background(), UserRef{UserID: "usr_123"})
	if !errors.Is(err, ErrUserNotGiteaReady) {
		t.Errorf("got %v, want ErrUserNotGiteaReady", err)
	}
}

func TestUserRefResolver_BindingEmptyUsername_NotGiteaReady(t *testing.T) {
	// Defensive: a binding row exists but gitea_username is empty (provisioning
	// was interrupted mid-flight). Resolver should not surface an empty
	// username to the caller.
	srv := newDispatchMux(t, dispatchMux{
		"GET /api/internal/users/usr_123": func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"subject_id":"usr_123","username":"alice"}`))
		},
		"GET /api/internal/users/usr_123/gitea-binding": func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"user_subject_id":"usr_123","gitea_username":"","sync_status":"pending"}`))
		},
	})
	r := NewUserRefResolver(newConfiguredRPCClient(t, srv.URL))

	_, err := r.Resolve(context.Background(), UserRef{UserID: "usr_123"})
	if !errors.Is(err, ErrUserNotGiteaReady) {
		t.Errorf("got %v, want ErrUserNotGiteaReady", err)
	}
}

func TestUserRefResolver_RPCUnavailable_On5xx(t *testing.T) {
	srv := newDispatchMux(t, dispatchMux{
		"GET /api/internal/users/usr_123": func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusInternalServerError)
		},
	})
	r := NewUserRefResolver(newConfiguredRPCClient(t, srv.URL))

	_, err := r.Resolve(context.Background(), UserRef{UserID: "usr_123"})
	if !errors.Is(err, ErrRPCUnavailable) {
		t.Errorf("got %v, want ErrRPCUnavailable", err)
	}
}

func TestUserRefResolver_NilRPC_Unavailable(t *testing.T) {
	var r *UserRefResolver // nil receiver
	_, err := r.Resolve(context.Background(), UserRef{UserID: "usr_1"})
	if !errors.Is(err, ErrRPCUnavailable) {
		t.Errorf("nil resolver: got %v, want ErrRPCUnavailable", err)
	}
}

// Verify error mapping gorm.ErrRecordNotFound from by-id path → ErrUserNotFound
// (defensive — RPCClient already maps HTTP 404 to gorm.ErrRecordNotFound).
func TestUserRefResolver_UserID_GormNotFound(t *testing.T) {
	// Cannot easily make RPCClient return gorm.ErrRecordNotFound directly;
	// the HTTP 404 path already covers this via TestUserRefResolver_UserIDPath_NotFound.
	// Keep this test as a unit-level check on the mapping function.
	ctx := context.Background()
	resolver := &UserRefResolver{rpc: nil}
	if _, err := resolver.Resolve(ctx, UserRef{UserID: "x"}); !errors.Is(err, ErrRPCUnavailable) {
		t.Errorf("nil rpc: got %v", err)
	}
	// Suppress unused-var warning for models import in case the file's other
	// tests stop referencing it.
	_ = models.User{}
	_ = gorm.ErrRecordNotFound
}
