package clawagent

import (
	"context"
	"strings"
	"testing"

)


func TestPersonaManager_Load_CreatesDefault(t *testing.T) {
	db := setupTestDB(t)
	mgr := NewPersonaManager(db, ClawAgentConfig{})

	p, err := mgr.Load(context.Background(), "user-1")
	if err != nil {
		t.Fatalf("Load failed: %v", err)
	}
	if p == nil {
		t.Fatal("Load returned nil persona")
	}
	if p.Name != "default" {
		t.Errorf("persona.Name = %q, want %q", p.Name, "default")
	}
	if !p.IsDefault {
		t.Error("default persona should be IsDefault")
	}
	if p.SoulContent == "" {
		t.Error("default persona should have SoulContent")
	}
	if strings.Contains(p.SoulContent, "Behavioral Rules") {
		// Good: default prompt is loaded
	} else {
		t.Error("default persona missing Behavioral Rules")
	}
}

func TestPersonaManager_CreateAndLoad(t *testing.T) {
	db := setupTestDB(t)
	mgr := NewPersonaManager(db, ClawAgentConfig{})

	ctx := context.Background()

	p := &Persona{
		UserID:          "user-1",
		Name:            "tech-advisor",
		SoulContent:     "你是一位资深技术顾问。",
		IdentityContent: "Name: TechBot\nEmoji: 🔧",
		UserContext:     "用户是 Go 后端开发者",
		IsDefault:       true,
	}

	if err := mgr.Create(ctx, p); err != nil {
		t.Fatalf("Create failed: %v", err)
	}

	if p.ID == "" {
		t.Fatal("Create should populate ID")
	}

	loaded, err := mgr.Load(ctx, "user-1")
	if err != nil {
		t.Fatalf("Load after create failed: %v", err)
	}
	if loaded.Name != "tech-advisor" {
		t.Errorf("loaded.Name = %q, want %q", loaded.Name, "tech-advisor")
	}
	if loaded.SoulContent != "你是一位资深技术顾问。" {
		t.Errorf("loaded.SoulContent = %q, want %q", loaded.SoulContent, "你是一位资深技术顾问。")
	}
}

func TestPersonaManager_MultiplePersonas(t *testing.T) {
	db := setupTestDB(t)
	mgr := NewPersonaManager(db, ClawAgentConfig{})
	ctx := context.Background()

	p1 := &Persona{UserID: "user-1", Name: "default", SoulContent: "default", IsDefault: true}
	p2 := &Persona{UserID: "user-1", Name: "helper", SoulContent: "helper"}

	if err := mgr.Create(ctx, p1); err != nil {
		t.Fatalf("Create p1: %v", err)
	}
	if err := mgr.Create(ctx, p2); err != nil {
		t.Fatalf("Create p2: %v", err)
	}

	list, err := mgr.ListByUser(ctx, "user-1")
	if err != nil {
		t.Fatalf("ListByUser: %v", err)
	}
	if len(list) != 2 {
		t.Errorf("len(list) = %d, want 2", len(list))
	}

	// Load should return the default persona
	loaded, err := mgr.Load(ctx, "user-1")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if loaded.Name != "default" {
		t.Errorf("Load returned %q, want %q", loaded.Name, "default")
	}
}

func TestPersonaManager_SetDefault(t *testing.T) {
	db := setupTestDB(t)
	mgr := NewPersonaManager(db, ClawAgentConfig{})
	ctx := context.Background()

	p1 := &Persona{UserID: "user-1", Name: "a", SoulContent: "a", IsDefault: true}
	p2 := &Persona{UserID: "user-1", Name: "b", SoulContent: "b"}

	if err := mgr.Create(ctx, p1); err != nil {
		t.Fatalf("Create p1: %v", err)
	}
	if err := mgr.Create(ctx, p2); err != nil {
		t.Fatalf("Create p2: %v", err)
	}

	if err := mgr.SetDefault(ctx, "user-1", p2.ID); err != nil {
		t.Fatalf("SetDefault: %v", err)
	}

	loaded, _ := mgr.Load(ctx, "user-1")
	if loaded.Name != "b" {
		t.Errorf("after SetDefault, Load returned %q, want %q", loaded.Name, "b")
	}

	// Original default should no longer be default
	var p1Reloaded Persona
	db.First(&p1Reloaded, "id = ?", p1.ID)
	if p1Reloaded.IsDefault {
		t.Error("original default persona should no longer be IsDefault")
	}
}

func TestPersonaManager_Delete(t *testing.T) {
	db := setupTestDB(t)
	mgr := NewPersonaManager(db, ClawAgentConfig{})
	ctx := context.Background()

	p := &Persona{UserID: "user-1", Name: "test", SoulContent: "test"}
	if err := mgr.Create(ctx, p); err != nil {
		t.Fatalf("Create: %v", err)
	}

	if err := mgr.Delete(ctx, p.ID); err != nil {
		t.Fatalf("Delete: %v", err)
	}

	_, err := mgr.LoadByID(ctx, p.ID)
	if err == nil {
		t.Fatal("LoadByID after Delete should fail")
	}
}

func TestPersonaManager_BuildInstruction(t *testing.T) {
	mgr := NewPersonaManager(nil, ClawAgentConfig{})

	p := &Persona{
		IdentityContent: "你是 costrict 助手。",
		SoulContent:     "用中文回复。",
		UserContext:     "用户是开发者。",
	}

	instruction := mgr.BuildInstruction(p, "用户偏好 Go 语言。")

	if !strings.Contains(instruction, "你是 costrict 助手。") {
		t.Error("instruction missing IdentityContent")
	}
	if !strings.Contains(instruction, "用中文回复。") {
		t.Error("instruction missing SoulContent")
	}
	if !strings.Contains(instruction, "# User Context") {
		t.Error("instruction missing User Context header")
	}
	if !strings.Contains(instruction, "用户是开发者。") {
		t.Error("instruction missing UserContext")
	}
	if !strings.Contains(instruction, "# Memory") {
		t.Error("instruction missing Memory header")
	}
	if !strings.Contains(instruction, "用户偏好 Go 语言。") {
		t.Error("instruction missing memory content")
	}
}

func TestPersonaManager_BuildInstruction_EmptyMemory(t *testing.T) {
	mgr := NewPersonaManager(nil, ClawAgentConfig{})

	p := &Persona{
		IdentityContent: "你是助手。",
		SoulContent:     "用中文回复。",
	}

	instruction := mgr.BuildInstruction(p, "")
	if strings.Contains(instruction, "# Memory") {
		t.Error("instruction should not contain Memory section when memory is empty")
	}
}

func TestPersonaManager_BuildInstruction_EmptyIdentity(t *testing.T) {
	mgr := NewPersonaManager(nil, ClawAgentConfig{})

	p := &Persona{
		IdentityContent: "",
		SoulContent:     "用中文回复。",
		UserContext:     "用户是开发者。",
	}

	instruction := mgr.BuildInstruction(p, "")
	if strings.Contains(instruction, "# Identity") {
		t.Error("instruction should not contain Identity section when IdentityContent is empty")
	}
}
