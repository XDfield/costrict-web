package services

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/costrict/costrict-web/server/internal/config"
	"github.com/costrict/costrict-web/server/internal/llm"
	"github.com/costrict/costrict-web/server/internal/models"
	"gorm.io/datatypes"
)

// W22: the scan worker owns security_status/last_scan_id, but `category` is
// projected from the repository manifest and rewritten on every push. Sending
// it for a Git-backed row would make the whole Updates() fail under the
// Git-owned field guard, and the worker would retry that item forever.
func TestScanItem_DoesNotOverwriteCategoryOnGitBackedRow(t *testing.T) {
	db := newIngestTestDB(t)

	item := &models.CapabilityItem{
		ID: "git-scan-1", RegistryID: "registry-1", RepoID: "public",
		Slug: "git-scan-skill", ItemType: "skill", Name: "Git Scan Skill",
		Description: "Git-backed", Category: "from-manifest",
		Content:  "This skill analyzes backend APIs and service contracts.",
		Metadata: datatypes.JSON([]byte(`{}`)), Status: "active", CreatedBy: "tester",
		ContentBackend: models.ContentBackendGit, SourceRepoURL: "https://git.example.test/o/r",
		SourceRepoRef: "main", SourceRepoPath: "skill.md",
		SourceGitServerID: "srv-1", SourceGitRepoID: 7, GitSyncStatus: "synced",
	}
	if err := db.Create(item).Error; err != nil {
		t.Fatalf("create git-backed item: %v", err)
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id": "chatcmpl-test", "object": "chat.completion", "created": 1, "model": "test-model",
			"choices": []map[string]any{{
				"index": 0,
				"message": map[string]any{"role": "assistant", "content": `{
					"category":"backend-development",
					"risk_level":"low",
					"verdict":"safe",
					"red_flags":[],
					"permissions":{"files":[],"network":[],"commands":[]},
					"summary":"低风险。",
					"recommendations":[]
				}`},
				"finish_reason": "stop",
			}},
			"usage": map[string]any{"prompt_tokens": 10, "completion_tokens": 10, "total_tokens": 20},
		})
	}))
	defer server.Close()

	scanSvc := &ScanService{
		DB: db,
		LLMClient: llm.NewClient(&config.LLMConfig{
			BaseURL: server.URL, APIKey: "test-key", Model: "test-model",
		}),
		ModelName:   "test-model",
		CategorySvc: &CategoryService{DB: db},
		TagSvc:      &TagService{DB: db},
	}

	result, err := scanSvc.ScanItem(context.Background(), item.ID, 1, "manual")
	if err != nil {
		t.Fatalf("scan failed: %v", err)
	}
	if result.Category != "backend-development" {
		t.Fatalf("scan record should still carry the suggested category, got %q", result.Category)
	}

	var updated models.CapabilityItem
	if err := db.First(&updated, "id = ?", item.ID).Error; err != nil {
		t.Fatalf("reload item: %v", err)
	}
	if updated.Category != "from-manifest" {
		t.Fatalf("git-owned category was overwritten by the scan: %q", updated.Category)
	}
	if updated.SecurityStatus != "low" || updated.LastScanID == nil || *updated.LastScanID != result.ID {
		t.Fatalf("scan runtime state did not land: status=%q lastScanID=%v", updated.SecurityStatus, updated.LastScanID)
	}
}
