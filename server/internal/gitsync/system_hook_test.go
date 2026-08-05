package gitsync

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
)

type fakeSystemHookGitea struct {
	mu        sync.Mutex
	hooks     []giteaSystemHook
	mutations []string
}

func (f *fakeSystemHookGitea) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("Authorization") != "token admin-token" {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()

	if r.URL.Path == "/api/v1/admin/hooks" {
		switch r.Method {
		case http.MethodGet:
			visibleHooks := f.hooks
			if r.URL.Query().Get("type") == "system" {
				visibleHooks = make([]giteaSystemHook, 0, len(f.hooks))
				for _, hook := range f.hooks {
					if hook.IsSystemWebhook {
						visibleHooks = append(visibleHooks, hook)
					}
				}
			}
			w.Header().Set("X-Total-Count", strconv.Itoa(len(visibleHooks)))
			page, _ := strconv.Atoi(r.URL.Query().Get("page"))
			limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
			if page < 1 {
				page = 1
			}
			if limit < 1 {
				limit = systemHookListPageSize
			}
			start := (page - 1) * limit
			end := start + limit
			if start > len(visibleHooks) {
				start = len(visibleHooks)
			}
			if end > len(visibleHooks) {
				end = len(visibleHooks)
			}
			publicHooks := make([]giteaSystemHook, end-start)
			for i := range publicHooks {
				hook := visibleHooks[start+i]
				publicHooks[i] = hook
				publicHooks[i].Config = map[string]string{
					"url":          hook.Config["url"],
					"content_type": hook.Config["content_type"],
				}
			}
			_ = json.NewEncoder(w).Encode(publicHooks)
			return
		case http.MethodPost:
			var req giteaSystemHookRequest
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			id := int64(1)
			for _, hook := range f.hooks {
				if hook.ID >= id {
					id = hook.ID + 1
				}
			}
			f.hooks = append(f.hooks, hookFromRequest(id, req))
			f.mutations = append(f.mutations, "create")
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(f.hooks[len(f.hooks)-1])
			return
		}
	}

	const prefix = "/api/v1/admin/hooks/"
	if strings.HasPrefix(r.URL.Path, prefix) {
		id, err := strconv.ParseInt(strings.TrimPrefix(r.URL.Path, prefix), 10, 64)
		if err != nil {
			http.NotFound(w, r)
			return
		}
		for i := range f.hooks {
			if f.hooks[i].ID != id {
				continue
			}
			switch r.Method {
			case http.MethodPatch:
				var req giteaSystemHookUpdateRequest
				if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
					http.Error(w, err.Error(), http.StatusBadRequest)
					return
				}
				f.hooks[i].Config["url"] = req.Config["url"]
				f.hooks[i].Config["content_type"] = req.Config["content_type"]
				f.hooks[i].Events = req.Events
				f.hooks[i].Active = req.Active
				if req.Name != nil {
					f.hooks[i].Name = *req.Name
				}
				f.mutations = append(f.mutations, "update")
				_ = json.NewEncoder(w).Encode(f.hooks[i])
				return
			case http.MethodDelete:
				f.hooks = append(f.hooks[:i], f.hooks[i+1:]...)
				f.mutations = append(f.mutations, "delete")
				w.WriteHeader(http.StatusNoContent)
				return
			}
		}
	}
	http.NotFound(w, r)
}

func hookFromRequest(id int64, req giteaSystemHookRequest) giteaSystemHook {
	return giteaSystemHook{
		ID: id, Name: req.Name, Type: req.Type, Config: req.Config, Events: req.Events, Active: req.Active,
		IsSystemWebhook: req.Config["is_system_webhook"] == "true",
	}
}

func desiredSystemHook(id int64, gitServerID, target, secret string) giteaSystemHook {
	return giteaSystemHook{
		ID: id, Name: managedSystemHookName(gitServerID, secret), Type: systemHookTypeGitea, Active: true, Events: []string{"push"}, IsSystemWebhook: true,
		Config: map[string]string{"url": target, "content_type": "json", "secret": secret, "is_system_webhook": "true"},
	}
}

func TestEnsureSystemPushWebhook_CreatesMissingHook(t *testing.T) {
	fake := &fakeSystemHookGitea{}
	server := httptest.NewServer(fake)
	defer server.Close()

	client := newClientWithHTTPC(server.URL, "admin-token", server.Client())
	target := "https://cloud.example/api/internal/git-sync/gs-1"
	if err := client.EnsureSystemPushWebhook(context.Background(), "gs-1", target, "secret-1"); err != nil {
		t.Fatalf("ensure: %v", err)
	}
	if len(fake.hooks) != 1 || !systemHookIsDesired(fake.hooks[0], giteaSystemHookRequest{
		Type: systemHookTypeGitea, Name: managedSystemHookName("gs-1", "secret-1"), Active: true, Events: []string{"push"},
		Config: map[string]string{"url": target, "content_type": "json", "secret": "secret-1", "is_system_webhook": "true"},
	}) {
		t.Fatalf("created hooks = %+v", fake.hooks)
	}
	if !fake.hooks[0].IsSystemWebhook || fake.hooks[0].Config["is_system_webhook"] != "true" {
		t.Fatalf("created hook is not a system webhook: %+v", fake.hooks[0])
	}
	if err := client.EnsureSystemPushWebhook(context.Background(), "gs-1", target, "secret-1"); err != nil {
		t.Fatalf("second ensure: %v", err)
	}
	if len(fake.hooks) != 1 || len(fake.mutations) != 1 || fake.mutations[0] != "create" {
		t.Fatalf("second ensure created a duplicate: hooks=%+v mutations=%v", fake.hooks, fake.mutations)
	}
}

func TestEnsureSystemPushWebhook_UpdatesDriftedHook(t *testing.T) {
	target := "https://cloud.example/api/internal/git-sync/gs-1"
	fake := &fakeSystemHookGitea{hooks: []giteaSystemHook{{
		ID: 7, Name: managedSystemHookName("gs-1", "new-secret"), Type: systemHookTypeGitea, Active: false, Events: []string{"issues"}, IsSystemWebhook: true,
		Config: map[string]string{"url": target, "content_type": "form", "secret": "new-secret"},
	}}}
	server := httptest.NewServer(fake)
	defer server.Close()

	client := newClientWithHTTPC(server.URL, "admin-token", server.Client())
	if err := client.EnsureSystemPushWebhook(context.Background(), "gs-1", target, "new-secret"); err != nil {
		t.Fatalf("ensure: %v", err)
	}
	if len(fake.mutations) != 1 || fake.mutations[0] != "update" {
		t.Fatalf("mutations = %v, want update", fake.mutations)
	}
	if got := fake.hooks[0]; !systemHookIsDesired(got, giteaSystemHookRequest{
		Type: systemHookTypeGitea, Name: managedSystemHookName("gs-1", "new-secret"), Active: true, Events: []string{"push"},
		Config: map[string]string{"url": target, "content_type": "json", "secret": "new-secret", "is_system_webhook": "true"},
	}) {
		t.Fatalf("updated hook = %+v", got)
	}
}

func TestEnsureSystemPushWebhook_ReplacesWrongHookType(t *testing.T) {
	target := "https://cloud.example/api/internal/git-sync/gs-1"
	fake := &fakeSystemHookGitea{hooks: []giteaSystemHook{{
		ID: 7, Name: managedSystemHookName("gs-1", "secret"), Type: "slack", Active: true, Events: []string{"push"}, IsSystemWebhook: true,
		Config: map[string]string{"url": target, "content_type": "json", "secret": "secret"},
	}}}
	server := httptest.NewServer(fake)
	defer server.Close()

	client := newClientWithHTTPC(server.URL, "admin-token", server.Client())
	if err := client.EnsureSystemPushWebhook(context.Background(), "gs-1", target, "secret"); err != nil {
		t.Fatalf("ensure: %v", err)
	}
	if len(fake.mutations) != 2 || fake.mutations[0] != "create" || fake.mutations[1] != "delete" {
		t.Fatalf("mutations = %v, want create then delete", fake.mutations)
	}
	if len(fake.hooks) != 1 || fake.hooks[0].Type != systemHookTypeGitea {
		t.Fatalf("remaining hooks = %+v", fake.hooks)
	}
}

func TestEnsureSystemPushWebhook_ReplacesHookWhenSecretFingerprintChanges(t *testing.T) {
	target := "https://cloud.example/api/internal/git-sync/gs-1"
	fake := &fakeSystemHookGitea{hooks: []giteaSystemHook{desiredSystemHook(4, "gs-1", target, "old-secret")}}
	server := httptest.NewServer(fake)
	defer server.Close()

	client := newClientWithHTTPC(server.URL, "admin-token", server.Client())
	if err := client.EnsureSystemPushWebhook(context.Background(), "gs-1", target, "new-secret"); err != nil {
		t.Fatalf("ensure: %v", err)
	}
	if len(fake.mutations) != 2 || fake.mutations[0] != "create" || fake.mutations[1] != "delete" {
		t.Fatalf("mutations = %v, want create then delete", fake.mutations)
	}
	if len(fake.hooks) != 1 || fake.hooks[0].Name != managedSystemHookName("gs-1", "new-secret") || fake.hooks[0].Config["secret"] != "new-secret" {
		t.Fatalf("rotated hook = %+v", fake.hooks)
	}
}

func TestEnsureSystemPushWebhook_NoopsWhenDesired(t *testing.T) {
	target := "https://cloud.example/api/internal/git-sync/gs-1"
	fake := &fakeSystemHookGitea{hooks: []giteaSystemHook{desiredSystemHook(3, "gs-1", target, "secret")}}
	server := httptest.NewServer(fake)
	defer server.Close()

	client := newClientWithHTTPC(server.URL, "admin-token", server.Client())
	if err := client.EnsureSystemPushWebhook(context.Background(), "gs-1", target, "secret"); err != nil {
		t.Fatalf("ensure: %v", err)
	}
	if len(fake.mutations) != 0 {
		t.Fatalf("mutations = %v, want none", fake.mutations)
	}
}

func TestEnsureSystemPushWebhook_UpdatesURLForSameManagedIdentity(t *testing.T) {
	oldTarget := "https://old.example/cloud-api/api/internal/git-sync/gs-1"
	newTarget := "https://new.example/api/internal/git-sync/gs-1"
	fake := &fakeSystemHookGitea{hooks: []giteaSystemHook{desiredSystemHook(3, "gs-1", oldTarget, "secret")}}
	server := httptest.NewServer(fake)
	defer server.Close()

	client := newClientWithHTTPC(server.URL, "admin-token", server.Client())
	if err := client.EnsureSystemPushWebhook(context.Background(), "gs-1", newTarget, "secret"); err != nil {
		t.Fatalf("ensure: %v", err)
	}
	if len(fake.mutations) != 1 || fake.mutations[0] != "update" {
		t.Fatalf("mutations = %v, want update", fake.mutations)
	}
	if len(fake.hooks) != 1 || fake.hooks[0].ID != 3 || fake.hooks[0].Config["url"] != newTarget {
		t.Fatalf("migrated hook = %+v", fake.hooks)
	}
}

func TestEnsureSystemPushWebhook_PreservesOtherServerAndUnmanagedHooks(t *testing.T) {
	target := "https://cloud.example/api/internal/git-sync/gs-1"
	unmanaged := giteaSystemHook{
		ID: 8, Name: managedSystemHookServerPrefix("gs-1") + "operator-owned", Type: systemHookTypeGitea, Active: true, Events: []string{"push"}, IsSystemWebhook: true,
		Config: map[string]string{"url": target, "content_type": "json"},
	}
	otherServer := desiredSystemHook(9, "gs-1-secondary", target, "other-secret")
	fake := &fakeSystemHookGitea{hooks: []giteaSystemHook{unmanaged, otherServer}}
	server := httptest.NewServer(fake)
	defer server.Close()

	client := newClientWithHTTPC(server.URL, "admin-token", server.Client())
	if err := client.EnsureSystemPushWebhook(context.Background(), "gs-1", target, "secret"); err != nil {
		t.Fatalf("ensure: %v", err)
	}
	if len(fake.mutations) != 1 || fake.mutations[0] != "create" {
		t.Fatalf("mutations = %v, want create only", fake.mutations)
	}
	if len(fake.hooks) != 3 || fake.hooks[0].ID != 8 || fake.hooks[1].ID != 9 {
		t.Fatalf("preserved hooks = %+v", fake.hooks)
	}
}

func TestEnsureSystemPushWebhook_RemovesDuplicateManagedHooksOnly(t *testing.T) {
	target := "https://cloud.example/api/internal/git-sync/gs-1"
	fake := &fakeSystemHookGitea{hooks: []giteaSystemHook{
		desiredSystemHook(2, "gs-1", target, "secret"),
		desiredSystemHook(9, "gs-1", target, "secret"),
		desiredSystemHook(5, "gs-2", "https://other.example/hook", "other"),
	}}
	server := httptest.NewServer(fake)
	defer server.Close()

	client := newClientWithHTTPC(server.URL, "admin-token", server.Client())
	if err := client.EnsureSystemPushWebhook(context.Background(), "gs-1", target, "secret"); err != nil {
		t.Fatalf("ensure: %v", err)
	}
	if len(fake.mutations) != 1 || fake.mutations[0] != "delete" {
		t.Fatalf("mutations = %v, want one delete", fake.mutations)
	}
	if len(fake.hooks) != 2 || fake.hooks[0].ID != 2 || fake.hooks[1].ID != 5 {
		t.Fatalf("remaining hooks = %+v", fake.hooks)
	}
}

func TestEnsureSystemPushWebhook_HandlesServerSidePageCapBelowRequestedLimit(t *testing.T) {
	target := "https://cloud.example/cloud-api/api/internal/git-sync/gs-1"
	secret := "secret"
	getCount := 0
	mutationCount := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			mutationCount++
			http.Error(w, "unexpected mutation", http.StatusInternalServerError)
			return
		}
		getCount++
		w.Header().Set("X-Total-Count", "2")
		page, _ := strconv.Atoi(r.URL.Query().Get("page"))
		switch page {
		case 1:
			_ = json.NewEncoder(w).Encode([]giteaSystemHook{
				desiredSystemHook(1, "gs-2", "https://other.example/hook", "other"),
			})
		case 2:
			_ = json.NewEncoder(w).Encode([]giteaSystemHook{
				desiredSystemHook(99, "gs-1", target, secret),
			})
		default:
			_ = json.NewEncoder(w).Encode([]giteaSystemHook{})
		}
	}))
	defer server.Close()

	client := newClientWithHTTPC(server.URL, "admin-token", server.Client())
	if err := client.EnsureSystemPushWebhook(context.Background(), "gs-1", target, secret); err != nil {
		t.Fatalf("ensure: %v", err)
	}
	if getCount != 2 || mutationCount != 0 {
		t.Fatalf("requests: GET=%d mutations=%d, want GET=2 mutations=0", getCount, mutationCount)
	}
}

func TestEnsureSystemPushWebhook_RefusesIncompleteListAtPageLimit(t *testing.T) {
	getCount := 0
	mutationCount := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			mutationCount++
			http.Error(w, "unexpected mutation", http.StatusInternalServerError)
			return
		}
		getCount++
		w.Header().Set("X-Total-Count", strconv.Itoa(systemHookListMaxPages+1))
		page, _ := strconv.Atoi(r.URL.Query().Get("page"))
		_ = json.NewEncoder(w).Encode([]giteaSystemHook{
			desiredSystemHook(int64(page), "gs-2", "https://other.example/hook/"+strconv.Itoa(page), "other"),
		})
	}))
	defer server.Close()

	client := newClientWithHTTPC(server.URL, "admin-token", server.Client())
	err := client.EnsureSystemPushWebhook(context.Background(), "gs-1", "https://target.example/hook", "secret")
	if err == nil || !strings.Contains(err.Error(), "incomplete") {
		t.Fatalf("ensure error = %v, want explicit pagination safety error", err)
	}
	if getCount != systemHookListMaxPages || mutationCount != 0 {
		t.Fatalf("requests: GET=%d mutations=%d, want GET=%d mutations=0", getCount, mutationCount, systemHookListMaxPages)
	}
}

func TestEnsureSystemPushWebhook_StopsWhenServerIgnoresPagination(t *testing.T) {
	target := "https://cloud.example/api/internal/git-sync/gs-1"
	hook := desiredSystemHook(2, "gs-1", target, "secret")
	getCount := 0
	deleteCount := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet:
			getCount++
			// No X-Total-Count models Gitea 1.24.6's full-list response.
			_ = json.NewEncoder(w).Encode([]giteaSystemHook{hook})
		case http.MethodDelete:
			deleteCount++
			http.Error(w, "unexpected duplicate delete", http.StatusNotFound)
		default:
			http.Error(w, "unexpected mutation", http.StatusInternalServerError)
		}
	}))
	defer server.Close()

	client := newClientWithHTTPC(server.URL, "admin-token", server.Client())
	if err := client.EnsureSystemPushWebhook(context.Background(), "gs-1", target, "secret"); err != nil {
		t.Fatalf("ensure: %v", err)
	}
	if getCount != 1 || deleteCount != 0 {
		t.Fatalf("requests: GET=%d DELETE=%d, want GET=1 DELETE=0", getCount, deleteCount)
	}
}

func TestEnsureSystemPushWebhook_ContinuesAfterOverlapOnlyPage(t *testing.T) {
	target := "https://cloud.example/api/internal/git-sync/gs-1"
	secret := "secret"
	getCount := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "unexpected mutation", http.StatusInternalServerError)
			return
		}
		getCount++
		w.Header().Set("X-Total-Count", "3")
		page, _ := strconv.Atoi(r.URL.Query().Get("page"))
		switch page {
		case 1:
			_ = json.NewEncoder(w).Encode([]giteaSystemHook{desiredSystemHook(1, "gs-2", "https://other.example/1", "other"), desiredSystemHook(2, "gs-2", "https://other.example/2", "other")})
		case 2:
			_ = json.NewEncoder(w).Encode([]giteaSystemHook{desiredSystemHook(2, "gs-2", "https://other.example/2", "other")})
		case 3:
			_ = json.NewEncoder(w).Encode([]giteaSystemHook{desiredSystemHook(3, "gs-1", target, secret)})
		default:
			_ = json.NewEncoder(w).Encode([]giteaSystemHook{})
		}
	}))
	defer server.Close()

	client := newClientWithHTTPC(server.URL, "admin-token", server.Client())
	if err := client.EnsureSystemPushWebhook(context.Background(), "gs-1", target, secret); err != nil {
		t.Fatalf("ensure: %v", err)
	}
	if getCount != 3 {
		t.Fatalf("GET requests = %d, want 3", getCount)
	}
}
