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
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

func TestScanItemUpdatesCategoryFromScanResult(t *testing.T) {
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{
		Logger: logger.Default.LogMode(logger.Silent),
	})
	if err != nil {
		t.Fatalf("failed to open test db: %v", err)
	}

	stmts := []string{
		`CREATE TABLE capability_items (
			id TEXT PRIMARY KEY,
			registry_id TEXT NOT NULL,
			repo_id TEXT NOT NULL,
			slug TEXT NOT NULL,
			item_type TEXT NOT NULL,
			name TEXT NOT NULL,
			description TEXT,
			category TEXT,
			version TEXT DEFAULT '1.0.0',
			content TEXT,
			content_md5 TEXT DEFAULT '',
			current_revision INTEGER NOT NULL DEFAULT 1,
			metadata TEXT DEFAULT '{}',
			source_path TEXT,
			source_sha TEXT,
			source_type TEXT DEFAULT 'direct',
			preview_count INTEGER DEFAULT 0,
			install_count INTEGER DEFAULT 0,
			favorite_count INTEGER DEFAULT 0,
			status TEXT DEFAULT 'active',
			security_status TEXT DEFAULT 'unscanned',
			last_scan_id TEXT,
			created_by TEXT NOT NULL,
			updated_by TEXT,
			embedding TEXT,
			experience_score REAL DEFAULT 0,
			embedding_updated_at DATETIME,
			created_at DATETIME,
			updated_at DATETIME
		)`,
		`CREATE TABLE security_scans (
			id TEXT PRIMARY KEY,
			item_id TEXT NOT NULL,
			item_revision INTEGER NOT NULL DEFAULT 0,
			trigger_type TEXT NOT NULL,
			scan_model TEXT,
			category TEXT DEFAULT '',
			risk_level TEXT DEFAULT '',
			verdict TEXT DEFAULT '',
			red_flags TEXT DEFAULT '[]',
			permissions TEXT DEFAULT '{}',
			summary TEXT,
			recommendations TEXT DEFAULT '[]',
			raw_output TEXT,
			duration_ms INTEGER DEFAULT 0,
			created_at DATETIME,
			finished_at DATETIME
		)`,
		`CREATE TABLE item_categories (
			id TEXT PRIMARY KEY,
			slug TEXT NOT NULL UNIQUE,
			icon TEXT,
			sort_order INTEGER DEFAULT 0,
			names TEXT NOT NULL DEFAULT '{}',
			descriptions TEXT DEFAULT '{}',
			created_by TEXT NOT NULL,
			created_at DATETIME,
			updated_at DATETIME
		)`,
	}
	for _, stmt := range stmts {
		if err := db.Exec(stmt).Error; err != nil {
			t.Fatalf("failed to create test table: %v", err)
		}
	}

	item := &models.CapabilityItem{
		ID:          "item-1",
		RegistryID:  "registry-1",
		RepoID:      "public",
		Slug:        "demo-skill",
		ItemType:    "skill",
		Name:        "Demo Skill",
		Description: "Demo item",
		Category:    "tool-invocation",
		Content:     "This skill analyzes backend APIs and service contracts.",
		Metadata:    datatypes.JSON([]byte(`{"language":"go"}`)),
		Status:      "active",
		CreatedBy:   "tester",
	}
	if err := db.Create(item).Error; err != nil {
		t.Fatalf("failed to create item: %v", err)
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id":      "chatcmpl-test",
			"object":  "chat.completion",
			"created": 1,
			"model":   "test-model",
			"choices": []map[string]any{
				{
					"index": 0,
					"message": map[string]any{
						"role": "assistant",
						"content": `{
							"category":"backend-development",
							"risk_level":"low",
							"verdict":"safe",
							"red_flags":[],
							"permissions":{"files":[],"network":[],"commands":[]},
							"summary":"后端 API 分析能力，风险较低。",
							"recommendations":["补充适用场景说明"]
						}`,
					},
					"finish_reason": "stop",
				},
			},
			"usage": map[string]any{
				"prompt_tokens":     10,
				"completion_tokens": 10,
				"total_tokens":      20,
			},
		})
	}))
	defer server.Close()

	scanSvc := &ScanService{
		DB: db,
		LLMClient: llm.NewClient(&config.LLMConfig{
			BaseURL: server.URL,
			APIKey:  "test-key",
			Model:   "test-model",
		}),
		ModelName:   "test-model",
		CategorySvc: &CategoryService{DB: db},
	}

	result, err := scanSvc.ScanItem(context.Background(), item.ID, 1, "manual")
	if err != nil {
		t.Fatalf("scan failed: %v", err)
	}

	if result.Category != "backend-development" {
		t.Fatalf("expected scan category to be backend-development, got %q", result.Category)
	}

	var updated models.CapabilityItem
	if err := db.First(&updated, "id = ?", item.ID).Error; err != nil {
		t.Fatalf("failed to reload item: %v", err)
	}
	if updated.Category != "backend-development" {
		t.Fatalf("expected item category to be updated, got %q", updated.Category)
	}

	var category models.ItemCategory
	if err := db.Where("slug = ?", "backend-development").First(&category).Error; err != nil {
		t.Fatalf("expected backend-development category to exist: %v", err)
	}
}
