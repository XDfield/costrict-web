package clawagent

import (
	"context"
	"testing"
	"time"
)

func TestSessionMetaManager_CreateAndActive(t *testing.T) {
	db := setupTestDB(t)
	mgr := NewSessionMetaManager(db)
	ctx := context.Background()

	meta, err := mgr.Create(ctx, "user-1", "base-key", 1, "direct")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	if meta.SessionID != "base-key:v1" {
		t.Errorf("SessionID = %q, want %q", meta.SessionID, "base-key:v1")
	}
	if meta.Version != 1 {
		t.Errorf("Version = %d, want 1", meta.Version)
	}
	if meta.UserID != "user-1" {
		t.Errorf("UserID = %q", meta.UserID)
	}
	if meta.IsArchived {
		t.Error("new session should not be archived")
	}

	active, err := mgr.Active(ctx, "user-1", "base-key")
	if err != nil {
		t.Fatalf("Active: %v", err)
	}
	if active.SessionID != meta.SessionID {
		t.Errorf("Active returned %q, want %q", active.SessionID, meta.SessionID)
	}
}

func TestSessionMetaManager_Active_NotFound(t *testing.T) {
	db := setupTestDB(t)
	mgr := NewSessionMetaManager(db)
	ctx := context.Background()

	_, err := mgr.Active(ctx, "no-user", "no-key")
	if err != ErrSessionNotFound {
		t.Errorf("Active should return ErrSessionNotFound, got %v", err)
	}
}

func TestSessionMetaManager_Archive(t *testing.T) {
	db := setupTestDB(t)
	mgr := NewSessionMetaManager(db)
	ctx := context.Background()

	_, _ = mgr.Create(ctx, "user-1", "base-key", 1, "direct")

	if err := mgr.Archive(ctx, "base-key:v1"); err != nil {
		t.Fatalf("Archive: %v", err)
	}

	_, err := mgr.Active(ctx, "user-1", "base-key")
	if err != ErrSessionNotFound {
		t.Errorf("After archive, Active should return ErrSessionNotFound, got %v", err)
	}
}

func TestSessionMetaManager_MultipleVersions(t *testing.T) {
	db := setupTestDB(t)
	mgr := NewSessionMetaManager(db)
	ctx := context.Background()

	_, _ = mgr.Create(ctx, "user-1", "base-key", 1, "direct")
	_ = mgr.Archive(ctx, "base-key:v1")
	_, _ = mgr.Create(ctx, "user-1", "base-key", 2, "direct")

	active, err := mgr.Active(ctx, "user-1", "base-key")
	if err != nil {
		t.Fatalf("Active after v2: %v", err)
	}
	if active.Version != 2 {
		t.Errorf("Active version = %d, want 2", active.Version)
	}
}

func TestSessionMetaManager_IncrementMessageCount(t *testing.T) {
	db := setupTestDB(t)
	mgr := NewSessionMetaManager(db)
	ctx := context.Background()

	meta, _ := mgr.Create(ctx, "user-1", "base-key", 1, "direct")
	time.Sleep(2 * time.Millisecond) // ensure time advances

	mgr.IncrementMessageCount(meta.SessionID)

	updated, _ := mgr.Get(ctx, meta.SessionID)
	if updated.MessageCount != 1 {
		t.Errorf("MessageCount = %d, want 1", updated.MessageCount)
	}
	if !updated.LastMessageAt.After(meta.LastMessageAt) {
		t.Error("LastMessageAt was not updated after IncrementMessageCount")
	}
}

func TestSessionMetaManager_IncrementMessageCount_Multiple(t *testing.T) {
	db := setupTestDB(t)
	mgr := NewSessionMetaManager(db)
	ctx := context.Background()

	meta, _ := mgr.Create(ctx, "user-1", "base-key", 1, "direct")

	for i := 0; i < 5; i++ {
		mgr.IncrementMessageCount(meta.SessionID)
	}

	updated, _ := mgr.Get(ctx, meta.SessionID)
	if updated.MessageCount != 5 {
		t.Errorf("MessageCount = %d, want 5", updated.MessageCount)
	}
}

func TestSessionMetaManager_ResolveActive(t *testing.T) {
	db := setupTestDB(t)
	mgr := NewSessionMetaManager(db)
	ctx := context.Background()

	_, _ = mgr.Create(ctx, "user-1", "base-key", 1, "direct")

	sid, err := mgr.ResolveActive(ctx, "user-1", "base-key")
	if err != nil {
		t.Fatalf("ResolveActive: %v", err)
	}
	if sid != "base-key:v1" {
		t.Errorf("ResolveActive = %q, want %q", sid, "base-key:v1")
	}
}

func TestSessionMetaManager_ResolveActive_Pruned(t *testing.T) {
	db := setupTestDB(t)
	mgr := NewSessionMetaManager(db)
	ctx := context.Background()

	_, err := mgr.ResolveActive(ctx, "no-user", "no-key")
	if err != ErrSessionPruned {
		t.Errorf("ResolveActive should return ErrSessionPruned, got %v", err)
	}
}

func TestSessionMetaManager_UpdateTokenEstimate(t *testing.T) {
	db := setupTestDB(t)
	mgr := NewSessionMetaManager(db)
	ctx := context.Background()

	meta, _ := mgr.Create(ctx, "user-1", "base-key", 1, "direct")

	if err := mgr.UpdateTokenEstimate(ctx, meta.SessionID, 500); err != nil {
		t.Fatalf("UpdateTokenEstimate: %v", err)
	}

	updated, _ := mgr.Get(ctx, meta.SessionID)
	if updated.TokenEstimate != 500 {
		t.Errorf("TokenEstimate = %d, want 500", updated.TokenEstimate)
	}
}

func TestSessionMetaManager_ListByUser(t *testing.T) {
	db := setupTestDB(t)
	mgr := NewSessionMetaManager(db)
	ctx := context.Background()

	_, _ = mgr.Create(ctx, "user-1", "k1", 1, "direct")
	_, _ = mgr.Create(ctx, "user-1", "k2", 1, "group")

	list, err := mgr.ListByUser(ctx, "user-1")
	if err != nil {
		t.Fatalf("ListByUser: %v", err)
	}
	if len(list) != 2 {
		t.Errorf("len = %d, want 2", len(list))
	}
}
