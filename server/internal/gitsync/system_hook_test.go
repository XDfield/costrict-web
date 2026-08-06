package gitsync

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
)

type fakeSystemHookGitea struct {
	mu             sync.Mutex
	hooks          []giteaSystemHook
	mutations      []string
	nullNames      bool
	deleteFailures int
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
				if f.nullNames {
					publicHooks[i].Name = ""
				}
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
				if req.Config["secret"] != "" {
					f.hooks[i].Config["secret"] = req.Config["secret"]
				}
				f.hooks[i].Events = req.Events
				f.hooks[i].Active = req.Active
				if req.Name != nil {
					f.hooks[i].Name = *req.Name
				}
				f.mutations = append(f.mutations, "update")
				_ = json.NewEncoder(w).Encode(f.hooks[i])
				return
			case http.MethodDelete:
				if f.deleteFailures > 0 {
					f.deleteFailures--
					http.Error(w, "injected delete failure", http.StatusInternalServerError)
					return
				}
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
		ID: id, Name: managedSystemHookName(gitServerID, secret), Type: systemHookTypeGitea, Active: true, Events: systemHookEvents, IsSystemWebhook: true,
		Config: map[string]string{"url": managedSystemHookURL(target, managedSystemHookName(gitServerID, secret)), "content_type": "json", "secret": secret, "is_system_webhook": "true"},
	}
}

func TestEnsureSystemCapabilityWebhook_CreatesMissingHook(t *testing.T) {
	fake := &fakeSystemHookGitea{}
	server := httptest.NewServer(fake)
	defer server.Close()

	client := newClientWithHTTPC(server.URL, "admin-token", server.Client())
	target := "https://cloud.example/api/internal/git-sync/gs-1"
	if err := client.EnsureSystemCapabilityWebhook(context.Background(), "gs-1", target, "secret-1"); err != nil {
		t.Fatalf("ensure: %v", err)
	}
	if len(fake.hooks) != 1 || !systemHookIsDesired(fake.hooks[0], giteaSystemHookRequest{
		Type: systemHookTypeGitea, Name: managedSystemHookName("gs-1", "secret-1"), Active: true, Events: systemHookEvents,
		Config: map[string]string{"url": managedSystemHookURL(target, managedSystemHookName("gs-1", "secret-1")), "content_type": "json", "secret": "secret-1", "is_system_webhook": "true"},
	}) {
		t.Fatalf("created hooks = %+v", fake.hooks)
	}
	if !fake.hooks[0].IsSystemWebhook || fake.hooks[0].Config["is_system_webhook"] != "true" {
		t.Fatalf("created hook is not a system webhook: %+v", fake.hooks[0])
	}
	if err := client.EnsureSystemCapabilityWebhook(context.Background(), "gs-1", target, "secret-1"); err != nil {
		t.Fatalf("second ensure: %v", err)
	}
	if len(fake.hooks) != 1 || len(fake.mutations) != 1 || fake.mutations[0] != "create" {
		t.Fatalf("second ensure created a duplicate: hooks=%+v mutations=%v", fake.hooks, fake.mutations)
	}
}

func TestEnsureSystemCapabilityWebhook_UpdatesDriftedHook(t *testing.T) {
	target := "https://cloud.example/api/internal/git-sync/gs-1"
	fake := &fakeSystemHookGitea{hooks: []giteaSystemHook{{
		ID: 7, Name: managedSystemHookName("gs-1", "new-secret"), Type: systemHookTypeGitea, Active: false, Events: []string{"issues"}, IsSystemWebhook: true,
		Config: map[string]string{"url": target, "content_type": "form", "secret": "new-secret"},
	}}}
	server := httptest.NewServer(fake)
	defer server.Close()

	client := newClientWithHTTPC(server.URL, "admin-token", server.Client())
	if err := client.EnsureSystemCapabilityWebhook(context.Background(), "gs-1", target, "new-secret"); err != nil {
		t.Fatalf("ensure: %v", err)
	}
	if len(fake.mutations) != 1 || fake.mutations[0] != "update" {
		t.Fatalf("mutations = %v, want update", fake.mutations)
	}
	if got := fake.hooks[0]; !systemHookIsDesired(got, giteaSystemHookRequest{
		Type: systemHookTypeGitea, Name: managedSystemHookName("gs-1", "new-secret"), Active: true, Events: systemHookEvents,
		Config: map[string]string{"url": managedSystemHookURL(target, managedSystemHookName("gs-1", "new-secret")), "content_type": "json", "secret": "new-secret", "is_system_webhook": "true"},
	}) {
		t.Fatalf("updated hook = %+v", got)
	}
}

func TestEnsureSystemCapabilityWebhook_ReplacesWrongHookType(t *testing.T) {
	target := "https://cloud.example/api/internal/git-sync/gs-1"
	fake := &fakeSystemHookGitea{hooks: []giteaSystemHook{{
		ID: 7, Name: managedSystemHookName("gs-1", "secret"), Type: "slack", Active: true, Events: []string{"push"}, IsSystemWebhook: true,
		Config: map[string]string{"url": target, "content_type": "json", "secret": "secret"},
	}}}
	server := httptest.NewServer(fake)
	defer server.Close()

	client := newClientWithHTTPC(server.URL, "admin-token", server.Client())
	if err := client.EnsureSystemCapabilityWebhook(context.Background(), "gs-1", target, "secret"); err != nil {
		t.Fatalf("ensure: %v", err)
	}
	if len(fake.mutations) != 2 || fake.mutations[0] != "create" || fake.mutations[1] != "delete" {
		t.Fatalf("mutations = %v, want create then delete", fake.mutations)
	}
	if len(fake.hooks) != 1 || fake.hooks[0].Type != systemHookTypeGitea {
		t.Fatalf("remaining hooks = %+v", fake.hooks)
	}
}

func TestEnsureSystemCapabilityWebhook_ReplacesHookWhenSecretFingerprintChanges(t *testing.T) {
	target := "https://cloud.example/api/internal/git-sync/gs-1"
	fake := &fakeSystemHookGitea{hooks: []giteaSystemHook{desiredSystemHook(4, "gs-1", target, "old-secret")}}
	server := httptest.NewServer(fake)
	defer server.Close()

	client := newClientWithHTTPC(server.URL, "admin-token", server.Client())
	if err := client.EnsureSystemCapabilityWebhook(context.Background(), "gs-1", target, "new-secret"); err != nil {
		t.Fatalf("ensure: %v", err)
	}
	if len(fake.mutations) != 2 || fake.mutations[0] != "create" || fake.mutations[1] != "delete" {
		t.Fatalf("mutations = %v, want create then delete", fake.mutations)
	}
	if len(fake.hooks) != 1 || fake.hooks[0].Name != managedSystemHookName("gs-1", "new-secret") || fake.hooks[0].Config["secret"] != "new-secret" {
		t.Fatalf("rotated hook = %+v", fake.hooks)
	}
}

func TestEnsureSystemCapabilityWebhook_NoopsWhenDesired(t *testing.T) {
	target := "https://cloud.example/api/internal/git-sync/gs-1"
	fake := &fakeSystemHookGitea{hooks: []giteaSystemHook{desiredSystemHook(3, "gs-1", target, "secret")}}
	server := httptest.NewServer(fake)
	defer server.Close()

	client := newClientWithHTTPC(server.URL, "admin-token", server.Client())
	if err := client.EnsureSystemCapabilityWebhook(context.Background(), "gs-1", target, "secret"); err != nil {
		t.Fatalf("ensure: %v", err)
	}
	if len(fake.mutations) != 0 {
		t.Fatalf("mutations = %v, want none", fake.mutations)
	}
}

func TestEnsureSystemCapabilityWebhook_UpdatesURLForSameManagedIdentity(t *testing.T) {
	oldTarget := "https://old.example/cloud-api/api/internal/git-sync/gs-1"
	newTarget := "https://new.example/api/internal/git-sync/gs-1"
	fake := &fakeSystemHookGitea{hooks: []giteaSystemHook{desiredSystemHook(3, "gs-1", oldTarget, "secret")}}
	server := httptest.NewServer(fake)
	defer server.Close()

	client := newClientWithHTTPC(server.URL, "admin-token", server.Client())
	if err := client.EnsureSystemCapabilityWebhook(context.Background(), "gs-1", newTarget, "secret"); err != nil {
		t.Fatalf("ensure: %v", err)
	}
	if len(fake.mutations) != 1 || fake.mutations[0] != "update" {
		t.Fatalf("mutations = %v, want update", fake.mutations)
	}
	if len(fake.hooks) != 1 || fake.hooks[0].ID != 3 || fake.hooks[0].Config["url"] != managedSystemHookURL(newTarget, managedSystemHookName("gs-1", "secret")) {
		t.Fatalf("migrated hook = %+v", fake.hooks)
	}
}

func TestEnsureSystemCapabilityWebhook_PreservesOtherServerAndUnmanagedHooks(t *testing.T) {
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
	if err := client.EnsureSystemCapabilityWebhook(context.Background(), "gs-1", target, "secret"); err != nil {
		t.Fatalf("ensure: %v", err)
	}
	if len(fake.mutations) != 1 || fake.mutations[0] != "create" {
		t.Fatalf("mutations = %v, want create only", fake.mutations)
	}
	if len(fake.hooks) != 3 || fake.hooks[0].ID != 8 || fake.hooks[1].ID != 9 {
		t.Fatalf("preserved hooks = %+v", fake.hooks)
	}
}

func TestEnsureSystemCapabilityWebhook_RemovesDuplicateManagedHooksOnly(t *testing.T) {
	target := "https://cloud.example/api/internal/git-sync/gs-1"
	fake := &fakeSystemHookGitea{nullNames: true, hooks: []giteaSystemHook{
		desiredSystemHook(2, "gs-1", target, "secret"),
		desiredSystemHook(9, "gs-1", target, "secret"),
		desiredSystemHook(5, "gs-2", "https://other.example/hook", "other"),
	}}
	server := httptest.NewServer(fake)
	defer server.Close()

	client := newClientWithHTTPC(server.URL, "admin-token", server.Client())
	if err := client.EnsureSystemCapabilityWebhook(context.Background(), "gs-1", target, "secret"); err != nil {
		t.Fatalf("ensure: %v", err)
	}
	if len(fake.mutations) != 2 || fake.mutations[0] != "update" || fake.mutations[1] != "delete" {
		t.Fatalf("mutations = %v, want update then delete", fake.mutations)
	}
	if len(fake.hooks) != 2 || fake.hooks[0].ID != 2 || fake.hooks[1].ID != 5 {
		t.Fatalf("remaining hooks = %+v", fake.hooks)
	}
}

func TestEnsureSystemCapabilityWebhook_AdoptsGitea124NamelessDuplicates(t *testing.T) {
	target := "https://cloud.example/api/internal/git-sync/gs-1"
	markedTarget := managedSystemHookURL(target, managedSystemHookName("gs-1", "new"))
	fake := &fakeSystemHookGitea{nullNames: true, hooks: []giteaSystemHook{
		{ID: 2, Name: "", Type: systemHookTypeGitea, Active: true, Events: []string{"push"}, IsSystemWebhook: true, Config: map[string]string{"url": markedTarget, "content_type": "json", "secret": "old"}},
		{ID: 3, Name: "", Type: systemHookTypeGitea, Active: true, Events: []string{"push"}, IsSystemWebhook: true, Config: map[string]string{"url": markedTarget, "content_type": "json", "secret": "old"}},
		{ID: 8, Name: "", Type: systemHookTypeGitea, Active: true, Events: []string{"issues"}, IsSystemWebhook: true, Config: map[string]string{"url": target + "/other", "content_type": "json"}},
	}}
	server := httptest.NewServer(fake)
	defer server.Close()
	client := newClientWithHTTPC(server.URL, "admin-token", server.Client())
	if err := client.EnsureSystemCapabilityWebhook(context.Background(), "gs-1", target, "new"); err != nil {
		t.Fatal(err)
	}
	if len(fake.hooks) != 2 || fake.hooks[0].Name != managedSystemHookName("gs-1", "new") {
		t.Fatalf("hooks=%+v", fake.hooks)
	}
	if len(fake.mutations) != 2 || fake.mutations[0] != "update" || fake.mutations[1] != "delete" {
		t.Fatalf("mutations=%v", fake.mutations)
	}
	fake.mutations = nil
	if err := client.EnsureSystemCapabilityWebhook(context.Background(), "gs-1", target, "new"); err != nil {
		t.Fatal(err)
	}
	if len(fake.mutations) != 1 || fake.mutations[0] != "update" {
		t.Fatalf("second ensure mutations=%v, want one update", fake.mutations)
	}
}

func TestEnsureSystemCapabilityWebhook_HandlesServerSidePageCapBelowRequestedLimit(t *testing.T) {
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
	if err := client.EnsureSystemCapabilityWebhook(context.Background(), "gs-1", target, secret); err != nil {
		t.Fatalf("ensure: %v", err)
	}
	if getCount != 2 || mutationCount != 0 {
		t.Fatalf("requests: GET=%d mutations=%d, want GET=2 mutations=0", getCount, mutationCount)
	}
}

func TestEnsureSystemCapabilityWebhook_RefusesIncompleteListAtPageLimit(t *testing.T) {
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
	err := client.EnsureSystemCapabilityWebhook(context.Background(), "gs-1", "https://target.example/hook", "secret")
	if err == nil || !strings.Contains(err.Error(), "incomplete") {
		t.Fatalf("ensure error = %v, want explicit pagination safety error", err)
	}
	if getCount != systemHookListMaxPages || mutationCount != 0 {
		t.Fatalf("requests: GET=%d mutations=%d, want GET=%d mutations=0", getCount, mutationCount, systemHookListMaxPages)
	}
}

func TestEnsureSystemCapabilityWebhook_StopsWhenServerIgnoresPagination(t *testing.T) {
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
	if err := client.EnsureSystemCapabilityWebhook(context.Background(), "gs-1", target, "secret"); err != nil {
		t.Fatalf("ensure: %v", err)
	}
	if getCount != 1 || deleteCount != 0 {
		t.Fatalf("requests: GET=%d DELETE=%d, want GET=1 DELETE=0", getCount, deleteCount)
	}
}

func TestEnsureSystemCapabilityWebhook_ContinuesAfterOverlapOnlyPage(t *testing.T) {
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
	if err := client.EnsureSystemCapabilityWebhook(context.Background(), "gs-1", target, secret); err != nil {
		t.Fatalf("ensure: %v", err)
	}
	if getCount != 3 {
		t.Fatalf("GET requests = %d, want 3", getCount)
	}
}

func TestEnsureSystemCapabilityWebhook_RefusesHooksWhenTotalCountIsZero(t *testing.T) {
	getCount := 0
	mutationCount := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			mutationCount++
			http.Error(w, "unexpected mutation", http.StatusInternalServerError)
			return
		}
		getCount++
		w.Header().Set("X-Total-Count", "0")
		_ = json.NewEncoder(w).Encode([]giteaSystemHook{
			desiredSystemHook(1, "gs-2", "https://other.example/hook", "other"),
		})
	}))
	defer server.Close()

	client := newClientWithHTTPC(server.URL, "admin-token", server.Client())
	err := client.EnsureSystemCapabilityWebhook(context.Background(), "gs-1", "https://target.example/hook", "secret")
	if err == nil || !strings.Contains(err.Error(), "inconsistent") {
		t.Fatalf("ensure error = %v, want explicit inconsistent listing error", err)
	}
	if getCount != 1 || mutationCount != 0 {
		t.Fatalf("requests: GET=%d mutations=%d, want GET=1 mutations=0", getCount, mutationCount)
	}
}

func TestEnsureSystemCapabilityWebhook_UnmarkedExactURLIsPreserved(t *testing.T) {
	target := "https://cloud.example/api/internal/git-sync/gs-1"
	fake := &fakeSystemHookGitea{hooks: []giteaSystemHook{{
		ID: 11, Name: managedSystemHookServerPrefix("gs-1") + "operator-owned", Type: systemHookTypeGitea,
		Active: true, Events: []string{"push"}, IsSystemWebhook: true,
		Config: map[string]string{"url": target, "content_type": "json"},
	}}}
	server := httptest.NewServer(fake)
	defer server.Close()
	client := newClientWithHTTPC(server.URL, "admin-token", server.Client())
	if err := client.EnsureSystemCapabilityWebhook(context.Background(), "gs-1", target, "secret"); err != nil {
		t.Fatal(err)
	}
	if len(fake.hooks) != 2 || fake.hooks[0].ID != 11 {
		t.Fatalf("operator hook changed: %+v", fake.hooks)
	}
}

func TestEnsureSystemCapabilityWebhook_RetryAfterDeleteFailure(t *testing.T) {
	target := "https://cloud.example/api/internal/git-sync/gs-1"
	fake := &fakeSystemHookGitea{deleteFailures: 1, hooks: []giteaSystemHook{desiredSystemHook(2, "gs-1", target, "old")}}
	server := httptest.NewServer(fake)
	defer server.Close()
	client := newClientWithHTTPC(server.URL, "admin-token", server.Client())
	if err := client.EnsureSystemCapabilityWebhook(context.Background(), "gs-1", target, "new"); err == nil {
		t.Fatal("expected delete failure")
	}
	if err := client.EnsureSystemCapabilityWebhook(context.Background(), "gs-1", target, "new"); err != nil {
		t.Fatal(err)
	}
	if len(fake.hooks) != 1 || fake.hooks[0].Name != managedSystemHookName("gs-1", "new") {
		t.Fatalf("hooks=%+v", fake.hooks)
	}
}

func TestManagedSystemHookURLPreservesExistingQuery(t *testing.T) {
	base := "https://cloud.example/hook?tenant=t1&x=1"
	got := managedSystemHookURL(base, "marker value")
	u, err := url.Parse(got)
	if err != nil {
		t.Fatal(err)
	}
	if u.Query().Get("tenant") != "t1" || u.Query().Get("x") != "1" || u.Query().Get(systemHookMarkerQuery) != "marker value" {
		t.Fatalf("query=%v", u.Query())
	}
}

func TestManagedSystemHookMarkerFailClosed(t *testing.T) {
	if got := managedSystemHookMarker("://bad%zz"); got != "" {
		t.Fatalf("marker=%q", got)
	}
	if got := managedSystemHookMarker("/relative?costrict_hook=secret"); got != "" {
		t.Fatalf("marker=%q", got)
	}
}

// The production hook (Gitea id 4) was created subscribed to `push` alone, so
// repository/deleted — the only lifecycle event 1.24.6 emits — never arrived.
// Widening the surface must be an in-place PATCH: the hook keeps its id, its
// secret and its delivery history. A delete-and-recreate would drop deliveries
// in the gap and change the id operators have in their runbooks.
func TestEnsureSystemCapabilityWebhook_WidensPushOnlySubscriptionInPlace(t *testing.T) {
	target := "https://cloud.example/api/internal/git-sync/gs-1"
	const secret = "secret"
	pushOnly := desiredSystemHook(4, "gs-1", target, secret)
	pushOnly.Events = []string{"push"}
	fake := &fakeSystemHookGitea{hooks: []giteaSystemHook{pushOnly}}
	server := httptest.NewServer(fake)
	defer server.Close()

	client := newClientWithHTTPC(server.URL, "admin-token", server.Client())
	if err := client.EnsureSystemCapabilityWebhook(context.Background(), "gs-1", target, secret); err != nil {
		t.Fatalf("ensure: %v", err)
	}
	if len(fake.mutations) != 1 || fake.mutations[0] != "update" {
		t.Fatalf("mutations = %v, want exactly one update (never delete+create)", fake.mutations)
	}
	if len(fake.hooks) != 1 || fake.hooks[0].ID != 4 {
		t.Fatalf("hook identity changed: %+v", fake.hooks)
	}
	if !sameEventSet(fake.hooks[0].Events, systemHookEvents) {
		t.Fatalf("events = %v, want %v", fake.hooks[0].Events, systemHookEvents)
	}
	if fake.hooks[0].Config["secret"] != secret || fake.hooks[0].Config["is_system_webhook"] != "true" {
		t.Fatalf("PATCH lost the hook's identity: %+v", fake.hooks[0].Config)
	}

	// And a second pass writes nothing: the steady state is zero mutations, so
	// the five-minute repair task does not PATCH the hook forever.
	if err := client.EnsureSystemCapabilityWebhook(context.Background(), "gs-1", target, secret); err != nil {
		t.Fatalf("second ensure: %v", err)
	}
	if len(fake.mutations) != 1 {
		t.Fatalf("mutations = %v after a converged second pass, want still one", fake.mutations)
	}
}

// repository/deleted is the whole reason the surface widened; create/delete are
// what make an otherwise silent default-branch change observable at push time.
func TestSystemHookEventsCoverTheFixtureProvenLifecycleSurface(t *testing.T) {
	for _, required := range []string{"push", "repository", "create", "delete"} {
		if !sameEventSet(append([]string(nil), systemHookEvents...), systemHookEvents) {
			t.Fatal("event set comparison is not reflexive")
		}
		found := false
		for _, event := range systemHookEvents {
			if event == required {
				found = true
			}
		}
		if !found {
			t.Errorf("system hook does not subscribe to %q", required)
		}
	}
}

// Gitea returns the subscription in its own order, so a set that matches must
// not be rewritten just because the order differs.
func TestSystemHookIsDesiredComparesEventsAsASet(t *testing.T) {
	target := "https://cloud.example/api/internal/git-sync/gs-1"
	current := desiredSystemHook(1, "gs-1", target, "secret")
	current.Events = []string{"delete", "push", "create", "repository"}
	desired := giteaSystemHookRequest{
		Type: systemHookTypeGitea, Name: current.Name, Active: true, Events: systemHookEvents,
		Config: current.Config,
	}
	if !systemHookIsDesired(current, desired) {
		t.Fatal("reordered but identical subscription was treated as drift")
	}
	current.Events = []string{"push", "repository", "create"}
	if systemHookIsDesired(current, desired) {
		t.Fatal("a missing event was treated as converged")
	}
}

// Converging the subscription of the LIVE system webhook, against a real Gitea.
//
// This is the one change in Phase C that touches shared, long-lived state
// outside the database: the production hook was created subscribed to `push`
// alone, so `repository`/`deleted` — the only lifecycle event Gitea 1.24.6 emits
// — never arrived. Widening it has to be an in-place PATCH; a delete-and-create
// would drop every delivery in the gap and change the hook id operators refer to.
//
// Opt-in, because it mutates a shared instance:
//
//	GITEA_HOOK_E2E_ENDPOINT=http://127.0.0.1:3001 \
//	GITEA_HOOK_E2E_TOKEN=<admin token> \
//	GITEA_HOOK_E2E_SERVER_ID=e2e-gitea \
//	GITEA_HOOK_E2E_SECRET=<webhook secret> \
//	GITEA_HOOK_E2E_TARGET=http://127.0.0.1:8080/api/internal/git-sync/e2e-gitea \
//	go test ./internal/gitsync -run TestEnsureSystemCapabilityWebhook_LiveGitea -v
//
// It asserts the properties an operator cares about and nothing else: the id is
// stable, the delivery URL is untouched, the subscription is the desired set,
// and a second pass writes nothing.
func TestEnsureSystemCapabilityWebhook_LiveGiteaConvergesInPlace(t *testing.T) {
	endpoint := strings.TrimRight(strings.TrimSpace(os.Getenv("GITEA_HOOK_E2E_ENDPOINT")), "/")
	token := strings.TrimSpace(os.Getenv("GITEA_HOOK_E2E_TOKEN"))
	serverID := strings.TrimSpace(os.Getenv("GITEA_HOOK_E2E_SERVER_ID"))
	secret := strings.TrimSpace(os.Getenv("GITEA_HOOK_E2E_SECRET"))
	target := strings.TrimSpace(os.Getenv("GITEA_HOOK_E2E_TARGET"))
	if endpoint == "" || token == "" || serverID == "" || secret == "" || target == "" {
		t.Skip("GITEA_HOOK_E2E_* not fully set; skipping live system webhook convergence")
	}

	client := NewClient(endpoint, token)
	before, err := client.listSystemHooks(context.Background())
	if err != nil {
		t.Fatalf("list system hooks: %v", err)
	}
	t.Logf("before: %d system hook(s)", len(before))
	for _, hook := range before {
		t.Logf("  id=%d events=%v url=%s", hook.ID, hook.Events, hook.Config["url"])
	}

	if err := client.EnsureSystemCapabilityWebhook(context.Background(), serverID, target, secret); err != nil {
		t.Fatalf("ensure: %v", err)
	}
	after, err := client.listSystemHooks(context.Background())
	if err != nil {
		t.Fatalf("list system hooks after ensure: %v", err)
	}
	if len(after) != len(before) {
		t.Fatalf("system hook count changed %d -> %d; convergence must not create or delete hooks here",
			len(before), len(after))
	}

	managedPrefix := managedSystemHookServerPrefix(serverID)
	var managed *giteaSystemHook
	for i := range after {
		if isManagedSystemHookMarkerForServer(after[i].Config["url"], managedPrefix) ||
			isManagedSystemHookForServer(after[i].Name, managedPrefix) {
			managed = &after[i]
		}
	}
	if managed == nil {
		t.Fatal("the managed hook disappeared")
	}
	t.Logf("after: id=%d events=%v url=%s", managed.ID, managed.Events, managed.Config["url"])

	// Identity, by id: this is what proves it was a PATCH and not a recreate.
	for _, hook := range before {
		if isManagedSystemHookMarkerForServer(hook.Config["url"], managedPrefix) {
			if hook.ID != managed.ID {
				t.Fatalf("hook id changed %d -> %d; deliveries in the gap were lost", hook.ID, managed.ID)
			}
			if hook.Config["url"] != managed.Config["url"] {
				t.Fatalf("delivery URL changed %q -> %q", hook.Config["url"], managed.Config["url"])
			}
		}
	}
	if !sameEventSet(managed.Events, systemHookEvents) {
		t.Fatalf("events = %v, want %v", managed.Events, systemHookEvents)
	}
	if !managed.Active {
		t.Fatal("the hook was left inactive")
	}

	// Second pass: converged state means zero writes, so the five-minute repair
	// task does not PATCH the hook forever (which would also reset its
	// last-delivery bookkeeping on every tick).
	if err := client.EnsureSystemCapabilityWebhook(context.Background(), serverID, target, secret); err != nil {
		t.Fatalf("second ensure: %v", err)
	}
	settled, err := client.listSystemHooks(context.Background())
	if err != nil {
		t.Fatalf("list system hooks after second ensure: %v", err)
	}
	if len(settled) != len(after) {
		t.Fatalf("second ensure changed the hook count %d -> %d", len(after), len(settled))
	}
	for i := range settled {
		if settled[i].ID == managed.ID && !sameEventSet(settled[i].Events, systemHookEvents) {
			t.Fatalf("second ensure disturbed the subscription: %v", settled[i].Events)
		}
	}
}
