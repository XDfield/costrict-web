package main

import (
	"testing"
	"time"

	"github.com/costrict/costrict-web/server/internal/config"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

func TestNewGitSystemHookReconcilerWiresWorkerConfig(t *testing.T) {
	t.Setenv("GIT_SYSTEM_HOOK_RECONCILE_INTERVAL_SECONDS", "37")
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		t.Fatalf("open SQLite: %v", err)
	}
	reconciler := newGitSystemHookReconciler(db, &config.Config{
		GitSystemWebhookBaseURL: "https://cloud.example/cloud-api",
	})
	if reconciler.DB != db {
		t.Fatal("worker DB was not wired into Git system webhook reconciler")
	}
	if reconciler.WebhookBaseURL != "https://cloud.example/cloud-api" {
		t.Fatalf("WebhookBaseURL = %q", reconciler.WebhookBaseURL)
	}
	if reconciler.Interval != 37*time.Second {
		t.Fatalf("Interval = %s, want 37s", reconciler.Interval)
	}
	if reconciler.RequestTimeout != 15*time.Second {
		t.Fatalf("RequestTimeout = %s, want 15s", reconciler.RequestTimeout)
	}
}

func TestNewGitSystemHookReconcilerDoesNotUseLegacyWebhookBaseURL(t *testing.T) {
	reconciler := newGitSystemHookReconciler(nil, &config.Config{
		WebhookBaseURL: "https://legacy-callback.example/cloud",
		CloudBaseURL:   "https://frontend.example/cloud",
	})
	if reconciler.WebhookBaseURL != "" {
		t.Fatalf("WebhookBaseURL = %q, want disabled", reconciler.WebhookBaseURL)
	}
}

func TestNewGitSystemHookReconcilerRejectsInvalidIntervalOverride(t *testing.T) {
	t.Setenv("GIT_SYSTEM_HOOK_RECONCILE_INTERVAL_SECONDS", "0")
	reconciler := newGitSystemHookReconciler(nil, &config.Config{})
	if reconciler.Interval != 5*time.Minute {
		t.Fatalf("Interval = %s, want default 5m", reconciler.Interval)
	}
}
