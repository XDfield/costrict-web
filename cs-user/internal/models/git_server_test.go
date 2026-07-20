//go:build cgo

package models

import (
	"strings"
	"testing"

	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

func newGitServerDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	if err := db.AutoMigrate(&GitServer{}); err != nil {
		t.Fatalf("AutoMigrate: %v", err)
	}
	t.Cleanup(func() {
		if sqlDB, err := db.DB(); err == nil {
			_ = sqlDB.Close()
		}
	})
	return db
}

// TestGitServer_TableName verifies the row maps to the "git_servers" table.
func TestGitServer_TableName(t *testing.T) {
	t.Parallel()
	gs := GitServer{}
	if got := gs.TableName(); got != "git_servers" {
		t.Fatalf("TableName: got %q, want %q", got, "git_servers")
	}
}

// TestGitServer_ConfigJSONRoundTrip verifies the JSON config blob survives a
// Create → First cycle byte-for-byte. The column is JSONB in production but
// TEXT-backed under sqlite; either way the app-layer (un)marshalling owns the
// shape, so this guards against accidental escaping / normalization.
func TestGitServer_ConfigJSONRoundTrip(t *testing.T) {
	t.Parallel()
	db := newGitServerDB(t)

	const cfg = `{"admin_token":"tok-abc-123","extra":{"note":"internal"}}`
	gs := &GitServer{
		ServerID:    "gs-test-1",
		Kind:        GitServerKindGitea,
		Endpoint:    "https://gitea.example.com",
		DisplayName: "Example Gitea",
		Config:      cfg,
		IsTemplate:  false,
		Enabled:     true,
	}
	if err := db.Create(gs).Error; err != nil {
		t.Fatalf("create: %v", err)
	}

	var got GitServer
	if err := db.First(&got, "server_id = ?", "gs-test-1").Error; err != nil {
		t.Fatalf("First: %v", err)
	}
	if got.Config != cfg {
		t.Fatalf("config round-trip mismatch:\n got: %q\nwant: %q", got.Config, cfg)
	}
	if got.Kind != GitServerKindGitea {
		t.Errorf("kind: got %q, want %q", got.Kind, GitServerKindGitea)
	}
}

// TestGitServer_DefaultsOnInsert verifies that a row created with only the
// PK + required string fields picks up the documented defaults (enabled=true,
// is_template=false, config='{}', timestamps non-zero).
func TestGitServer_DefaultsOnInsert(t *testing.T) {
	t.Parallel()
	db := newGitServerDB(t)

	gs := &GitServer{
		ServerID:    "gs-min",
		Kind:        GitServerKindGitea,
		Endpoint:    "https://g.example.com",
		DisplayName: "Min",
	}
	if err := db.Create(gs).Error; err != nil {
		t.Fatalf("create: %v", err)
	}

	var got GitServer
	if err := db.First(&got, "server_id = ?", "gs-min").Error; err != nil {
		t.Fatalf("First: %v", err)
	}
	if !got.Enabled {
		t.Error("enabled default: got false, want true")
	}
	if got.IsTemplate {
		t.Error("is_template default: got true, want false")
	}
	if got.Config != "{}" {
		t.Errorf("config default: got %q, want {}", got.Config)
	}
	if got.CreatedAt.IsZero() {
		t.Error("created_at should default to a non-zero timestamp")
	}
}

// TestGitServer_KindVocabulary guards the kind vocabulary documented in the
// model doc: v1 only supports "gitea". Future kinds land here as a
// single-source change.
func TestGitServer_KindVocabulary(t *testing.T) {
	t.Parallel()
	supported := []string{GitServerKindGitea}
	for _, k := range supported {
		if strings.TrimSpace(k) == "" {
			t.Errorf("kind %q is empty", k)
		}
	}
	// sanity: prevent accidental rename of the constant
	if GitServerKindGitea != "gitea" {
		t.Fatalf("GitServerKindGitea constant drift: got %q", GitServerKindGitea)
	}
}
