package clawagent

import (
	"context"
	"strings"
	"testing"
)

func TestMemoryManager_Load_NotFound(t *testing.T) {
	db := setupTestDB(t)
	mgr := NewMemoryManager(db)

	content, err := mgr.Load(context.Background(), "user-nonexistent")
	if err != nil {
		t.Fatalf("Load non-existent: %v", err)
	}
	if content != "" {
		t.Errorf("Load non-existent = %q, want empty", content)
	}
}

func TestMemoryManager_SaveAndLoad(t *testing.T) {
	db := setupTestDB(t)
	mgr := NewMemoryManager(db)
	ctx := context.Background()

	memContent := "用户偏好 Go 语言。常用 workspace 是 ws-001。"
	if err := mgr.Save(ctx, "user-1", memContent); err != nil {
		t.Fatalf("Save failed: %v", err)
	}

	loaded, err := mgr.Load(ctx, "user-1")
	if err != nil {
		t.Fatalf("Load after save: %v", err)
	}
	if loaded != memContent {
		t.Errorf("Load = %q, want %q", loaded, memContent)
	}
}

func TestMemoryManager_Overwrite(t *testing.T) {
	db := setupTestDB(t)
	mgr := NewMemoryManager(db)
	ctx := context.Background()

	_ = mgr.Save(ctx, "user-1", "旧内容")
	_ = mgr.Save(ctx, "user-1", "新内容")

	loaded, _ := mgr.Load(ctx, "user-1")
	if loaded != "新内容" {
		t.Errorf("Load after overwrite = %q, want %q", loaded, "新内容")
	}
}

func TestMemoryManager_Truncation(t *testing.T) {
	db := setupTestDB(t)
	mgr := NewMemoryManager(db)
	ctx := context.Background()

	// Create content exceeding MaxMemoryBytes
	overLimit := strings.Repeat("a", MaxMemoryBytes+1000)
	if err := mgr.Save(ctx, "user-1", overLimit); err != nil {
		t.Fatalf("Save over limit: %v", err)
	}

	loaded, _ := mgr.Load(ctx, "user-1")
	if len(loaded) > MaxMemoryBytes {
		t.Errorf("loaded content length = %d, want <= %d", len(loaded), MaxMemoryBytes)
	}
}

func TestMemoryManager_MultipleUsers(t *testing.T) {
	db := setupTestDB(t)
	mgr := NewMemoryManager(db)
	ctx := context.Background()

	_ = mgr.Save(ctx, "user-a", "Alice 的记忆")
	_ = mgr.Save(ctx, "user-b", "Bob 的记忆")

	alice, _ := mgr.Load(ctx, "user-a")
	bob, _ := mgr.Load(ctx, "user-b")

	if alice != "Alice 的记忆" {
		t.Errorf("alice = %q", alice)
	}
	if bob != "Bob 的记忆" {
		t.Errorf("bob = %q", bob)
	}
}

func TestMemoryManager_EmptyContent(t *testing.T) {
	db := setupTestDB(t)
	mgr := NewMemoryManager(db)
	ctx := context.Background()

	if err := mgr.Save(ctx, "user-1", ""); err != nil {
		t.Fatalf("Save empty: %v", err)
	}

	loaded, _ := mgr.Load(ctx, "user-1")
	if loaded != "" {
		t.Errorf("Load after empty save = %q, want empty", loaded)
	}
}
