// Tests for the platform-admin push-quota rule CRUD (FI-4).
//
// sqlite :memory: + an in-process gin engine; no HTTP server, no Gitea.
//
// The assertions worth having here are about the fork's semantics leaking into
// the API surface, not about CRUD mechanics:
//
//   - repo "" is a value (the owner-level default), not a missing field, so it
//     must be storable and must not collide with a repo-level rule;
//   - 0 means "unlimited", so an OMITTED limit must be rejected rather than
//     defaulted to 0 — defaulting would silently remove a limit;
//   - a rule for a server that can never receive it is refused, because a
//     stored rule reads as protection that is in force.

package handlers

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/costrict/costrict-web/server/internal/models"
	"github.com/gin-gonic/gin"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

func setupQuotaRuleDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	for _, ddl := range []string{
		`CREATE TABLE git_servers (
			server_id TEXT PRIMARY KEY,
			kind TEXT NOT NULL,
			endpoint TEXT NOT NULL,
			display_name TEXT NOT NULL,
			config TEXT NOT NULL DEFAULT '{}',
			is_template INTEGER NOT NULL DEFAULT 0,
			enabled INTEGER NOT NULL DEFAULT 1,
			created_at DATETIME NOT NULL,
			updated_at DATETIME NOT NULL
		)`,
		`CREATE TABLE git_quota_rules (
			git_server_id TEXT NOT NULL,
			owner TEXT NOT NULL,
			repo TEXT NOT NULL DEFAULT '',
			max_file_size_mb INTEGER NOT NULL DEFAULT 0,
			repo_quota_mb INTEGER NOT NULL DEFAULT 0,
			updated_by TEXT NOT NULL DEFAULT '',
			created_at DATETIME NOT NULL,
			updated_at DATETIME NOT NULL,
			PRIMARY KEY (git_server_id, owner, repo)
		)`,
	} {
		if err := db.Exec(ddl).Error; err != nil {
			t.Fatalf("create schema: %v", err)
		}
	}
	now := time.Now()
	if err := db.Exec(`INSERT INTO git_servers
		(server_id, kind, endpoint, display_name, config, is_template, enabled, created_at, updated_at)
		VALUES ('gs-1', ?, 'http://127.0.0.1:3001', 'Test Gitea', '{}', 0, 1, ?, ?)`,
		models.GitServerKindGitea, now, now).Error; err != nil {
		t.Fatalf("seed git server: %v", err)
	}
	return db
}

func newQuotaRuleRouter(db *gorm.DB) *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	api := NewGitQuotaRuleAPI(db)
	r.GET("/api/admin/git-quota-rules", api.ListGitQuotaRules)
	r.PUT("/api/admin/git-quota-rules", api.UpsertGitQuotaRule)
	r.DELETE("/api/admin/git-quota-rules", api.DeleteGitQuotaRule)
	return r
}

func quotaRuleRequest(t *testing.T, router *gin.Engine, method, target string, body any) *httptest.ResponseRecorder {
	t.Helper()
	var reader *bytes.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("marshal body: %v", err)
		}
		reader = bytes.NewReader(raw)
	} else {
		reader = bytes.NewReader(nil)
	}
	req := httptest.NewRequest(method, target, reader)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	return rec
}

func TestGitQuotaRuleAPI_OwnerAndRepoRulesCoexist(t *testing.T) {
	db := setupQuotaRuleDB(t)
	router := newQuotaRuleRouter(db)

	// Owner-level default: repo omitted entirely.
	rec := quotaRuleRequest(t, router, http.MethodPut, "/api/admin/git-quota-rules", map[string]any{
		"git_server_id": "gs-1", "owner": "acme",
		"max_file_size_mb": 20, "repo_quota_mb": 200,
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("owner-level upsert = %d body=%s", rec.Code, rec.Body.String())
	}
	// Repo-level override for the same owner.
	rec = quotaRuleRequest(t, router, http.MethodPut, "/api/admin/git-quota-rules", map[string]any{
		"git_server_id": "gs-1", "owner": "acme", "repo": "widgets",
		"max_file_size_mb": 5, "repo_quota_mb": 50,
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("repo-level upsert = %d body=%s", rec.Code, rec.Body.String())
	}

	rec = quotaRuleRequest(t, router, http.MethodGet, "/api/admin/git-quota-rules?git_server_id=gs-1", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("list = %d", rec.Code)
	}
	var listed struct {
		Rules []gitQuotaRuleResponse `json:"rules"`
		Total int                    `json:"total"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &listed); err != nil {
		t.Fatalf("decode list: %v", err)
	}
	if listed.Total != 2 {
		t.Fatalf("total = %d, want 2 (the owner default must not be overwritten by the repo rule)", listed.Total)
	}
	if listed.Rules[0].Repo != "" || listed.Rules[0].MaxFileSizeMB != 20 {
		t.Fatalf("owner rule = %+v", listed.Rules[0])
	}
	if listed.Rules[1].Repo != "widgets" || listed.Rules[1].MaxFileSizeMB != 5 {
		t.Fatalf("repo rule = %+v", listed.Rules[1])
	}
}

func TestGitQuotaRuleAPI_UpsertReplacesExistingRule(t *testing.T) {
	db := setupQuotaRuleDB(t)
	router := newQuotaRuleRouter(db)

	for _, limit := range []int64{20, 40} {
		rec := quotaRuleRequest(t, router, http.MethodPut, "/api/admin/git-quota-rules", map[string]any{
			"git_server_id": "gs-1", "owner": "acme",
			"max_file_size_mb": limit, "repo_quota_mb": 200,
		})
		if rec.Code != http.StatusOK {
			t.Fatalf("upsert(%d) = %d body=%s", limit, rec.Code, rec.Body.String())
		}
	}
	var rules []models.GitQuotaRule
	if err := db.Find(&rules).Error; err != nil {
		t.Fatalf("read rules: %v", err)
	}
	if len(rules) != 1 || rules[0].MaxFileSizeMB != 40 {
		t.Fatalf("rules = %+v, want a single row updated in place", rules)
	}
}

// 0 is the fork's encoding of "unlimited", so an omitted limit must not be
// defaulted to it — that would quietly remove a limit nobody asked to remove.
func TestGitQuotaRuleAPI_OmittedLimitIsRejected(t *testing.T) {
	db := setupQuotaRuleDB(t)
	router := newQuotaRuleRouter(db)

	rec := quotaRuleRequest(t, router, http.MethodPut, "/api/admin/git-quota-rules", map[string]any{
		"git_server_id": "gs-1", "owner": "acme", "max_file_size_mb": 20,
	})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("omitted repo_quota_mb = %d, want 400 (body=%s)", rec.Code, rec.Body.String())
	}

	// Explicit 0 is legitimate and means unlimited.
	rec = quotaRuleRequest(t, router, http.MethodPut, "/api/admin/git-quota-rules", map[string]any{
		"git_server_id": "gs-1", "owner": "acme",
		"max_file_size_mb": 20, "repo_quota_mb": 0,
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("explicit zero = %d, want 200 (body=%s)", rec.Code, rec.Body.String())
	}
}

func TestGitQuotaRuleAPI_RejectsInvalidInput(t *testing.T) {
	db := setupQuotaRuleDB(t)
	router := newQuotaRuleRouter(db)

	tests := []struct {
		name string
		body map[string]any
		want int
	}{
		{"missing owner", map[string]any{"git_server_id": "gs-1", "max_file_size_mb": 1, "repo_quota_mb": 1}, http.StatusBadRequest},
		{"blank owner", map[string]any{"git_server_id": "gs-1", "owner": "   ", "max_file_size_mb": 1, "repo_quota_mb": 1}, http.StatusBadRequest},
		{"negative limit", map[string]any{"git_server_id": "gs-1", "owner": "acme", "max_file_size_mb": -1, "repo_quota_mb": 1}, http.StatusBadRequest},
		{"unknown server", map[string]any{"git_server_id": "nope", "owner": "acme", "max_file_size_mb": 1, "repo_quota_mb": 1}, http.StatusNotFound},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rec := quotaRuleRequest(t, router, http.MethodPut, "/api/admin/git-quota-rules", tc.body)
			if rec.Code != tc.want {
				t.Fatalf("status = %d, want %d (body=%s)", rec.Code, tc.want, rec.Body.String())
			}
		})
	}
}

// Push quotas exist only in the CoStrict Gitea fork; storing a rule for another
// kind of server would present as enforcement that cannot happen.
func TestGitQuotaRuleAPI_RejectsNonGiteaServer(t *testing.T) {
	db := setupQuotaRuleDB(t)
	now := time.Now()
	if err := db.Exec(`INSERT INTO git_servers
		(server_id, kind, endpoint, display_name, config, is_template, enabled, created_at, updated_at)
		VALUES ('gs-other', 'gitlab', 'http://gitlab.example', 'GitLab', '{}', 0, 1, ?, ?)`,
		now, now).Error; err != nil {
		t.Fatalf("seed server: %v", err)
	}
	router := newQuotaRuleRouter(db)

	rec := quotaRuleRequest(t, router, http.MethodPut, "/api/admin/git-quota-rules", map[string]any{
		"git_server_id": "gs-other", "owner": "acme",
		"max_file_size_mb": 1, "repo_quota_mb": 1,
	})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (body=%s)", rec.Code, rec.Body.String())
	}
}

func TestGitQuotaRuleAPI_Delete(t *testing.T) {
	db := setupQuotaRuleDB(t)
	router := newQuotaRuleRouter(db)

	rec := quotaRuleRequest(t, router, http.MethodPut, "/api/admin/git-quota-rules", map[string]any{
		"git_server_id": "gs-1", "owner": "acme", "repo": "widgets",
		"max_file_size_mb": 5, "repo_quota_mb": 50,
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("seed upsert = %d", rec.Code)
	}

	// Omitting repo addresses the OWNER rule, which does not exist here — the
	// two must not be confusable.
	rec = quotaRuleRequest(t, router, http.MethodDelete,
		"/api/admin/git-quota-rules?git_server_id=gs-1&owner=acme", nil)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("delete owner rule = %d, want 404", rec.Code)
	}

	rec = quotaRuleRequest(t, router, http.MethodDelete,
		"/api/admin/git-quota-rules?git_server_id=gs-1&owner=acme&repo=widgets", nil)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("delete repo rule = %d, want 204 (body=%s)", rec.Code, rec.Body.String())
	}

	var remaining int64
	if err := db.Model(&models.GitQuotaRule{}).Count(&remaining).Error; err != nil {
		t.Fatalf("count: %v", err)
	}
	if remaining != 0 {
		t.Fatalf("remaining rules = %d, want 0", remaining)
	}

	rec = quotaRuleRequest(t, router, http.MethodDelete, "/api/admin/git-quota-rules?owner=acme", nil)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("delete without git_server_id = %d, want 400", rec.Code)
	}
}
