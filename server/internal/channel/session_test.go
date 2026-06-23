package channel

import (
	"os"
	"testing"
	"time"

	"github.com/costrict/costrict-web/server/internal/database"
	"github.com/costrict/costrict-web/server/internal/models"
	"gorm.io/gorm"
)

func setupReplyContextTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		t.Skip("DATABASE_URL not set; skipping PostgreSQL-backed reply context test")
	}
	db, err := database.Initialize(dsn)
	if err != nil {
		t.Fatalf("initialize database: %v", err)
	}
	// Clean up any leftover test rows.
	_ = db.Where("channel_config_id LIKE ?", "cfg-%").Delete(&models.ChannelReplyContext{})
	return db
}

func TestMemoryReplyContextStore(t *testing.T) {
	s := NewReplyContextStore()
	rc := ReplyContext{
		ChannelConfigID: "cfg-1",
		ChannelType:     "wecom",
		UserID:          "user-1",
		Target: ReplyTarget{
			ExternalChatID: "chat-1",
			ExternalUserID: "ext-1",
			ContextToken:   "token-1",
		},
	}

	s.Record(rc)

	got, ok := s.Lookup("cfg-1", "ext-1")
	if !ok {
		t.Fatal("expected lookup to succeed")
	}
	if got.Target.ContextToken != "token-1" {
		t.Fatalf("unexpected context token: %s", got.Target.ContextToken)
	}

	byUser := s.LookupByUser("user-1")
	if len(byUser) != 1 {
		t.Fatalf("expected 1 context for user, got %d", len(byUser))
	}
}

func TestPostgresReplyContextStore(t *testing.T) {
	db := setupReplyContextTestDB(t)
	s := NewPostgresReplyContextStore(db)

	rc := ReplyContext{
		ChannelConfigID: "cfg-1",
		ChannelType:     "wecom",
		UserID:          "user-1",
		Target: ReplyTarget{
			ExternalChatID: "chat-1",
			ExternalUserID: "ext-1",
			ContextToken:   "token-1",
		},
	}

	s.Record(rc)

	got, ok := s.Lookup("cfg-1", "ext-1")
	if !ok {
		t.Fatal("expected lookup to succeed")
	}
	if got.Target.ContextToken != "token-1" {
		t.Fatalf("unexpected context token: %s", got.Target.ContextToken)
	}

	byUser := s.LookupByUser("user-1")
	if len(byUser) != 1 {
		t.Fatalf("expected 1 context for user, got %d", len(byUser))
	}

	// Update existing context.
	rc.Target.ContextToken = "token-2"
	s.Record(rc)
	got, ok = s.Lookup("cfg-1", "ext-1")
	if !ok || got.Target.ContextToken != "token-2" {
		t.Fatalf("expected updated context token, got %v", got)
	}
}

func TestPostgresReplyContextStoreCleanup(t *testing.T) {
	db := setupReplyContextTestDB(t)
	s := NewPostgresReplyContextStore(db)

	rc := ReplyContext{
		ChannelConfigID: "cfg-cleanup",
		ChannelType:     "wecom",
		UserID:          "user-cleanup",
		Target: ReplyTarget{
			ExternalChatID: "chat-cleanup",
			ExternalUserID: "ext-cleanup",
			ContextToken:   "token-cleanup",
		},
	}
	s.Record(rc)

	// Force the record to look old by updating its updated_at directly.
	db.Model(&models.ChannelReplyContext{}).
		Where("channel_config_id = ? AND external_user_id = ?", "cfg-cleanup", "ext-cleanup").
		Update("updated_at", time.Now().Add(-8*24*time.Hour))

	s.Cleanup(7 * 24 * time.Hour)

	_, ok := s.Lookup("cfg-cleanup", "ext-cleanup")
	if ok {
		t.Fatal("expected record to be cleaned up")
	}
}
