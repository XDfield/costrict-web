package clawagent

import (
	"context"
	"testing"
)

func TestProviderManager_CreateAndLoad(t *testing.T) {
	db := setupTestDB(t)
	mgr := NewProviderManager(db, testAgentConfig())

	ctx := context.Background()
	p := &Provider{
		UserID:          "user-1",
		Name:            "my-deepseek",
		ProviderType:    "deepseek",
		APIKeyEncrypted: "sk-test-key-not-important-here",
		BaseURL:         "https://api.deepseek.com/v1",
		ModelName:       "deepseek-chat",
		IsDefault:       true,
	}

	if err := mgr.Create(ctx, p); err != nil {
		t.Fatalf("Create failed: %v", err)
	}

	if p.ID == 0 {
		t.Fatal("Create should populate ID")
	}

	loaded, err := mgr.LoadByID(ctx, p.ID)
	if err != nil {
		t.Fatalf("LoadByID: %v", err)
	}
	if loaded.Name != "my-deepseek" {
		t.Errorf("Name = %q, want %q", loaded.Name, "my-deepseek")
	}
	if loaded.ProviderType != "deepseek" {
		t.Errorf("ProviderType = %q, want %q", loaded.ProviderType, "deepseek")
	}

	// API key should be encrypted (not plaintext)
	if loaded.APIKeyEncrypted == "sk-test-key-not-important-here" {
		t.Error("API key stored in plaintext!")
	}
	if loaded.APIKeyEncrypted == "" {
		t.Error("encrypted API key should not be empty")
	}
}

func TestProviderManager_Create_EncryptsAPIKey(t *testing.T) {
	db := setupTestDB(t)
	mgr := NewProviderManager(db, testAgentConfig())

	ctx := context.Background()
	p := &Provider{
		UserID:          "user-1",
		Name:            "test",
		ProviderType:    "openai",
		APIKeyEncrypted: "sk-raw-plaintext-key",
		ModelName:       "gpt-4",
	}

	if err := mgr.Create(ctx, p); err != nil {
		t.Fatalf("Create: %v", err)
	}

	// Verify it was encrypted
	if p.APIKeyEncrypted == "sk-raw-plaintext-key" {
		t.Fatal("API key was not encrypted on Create")
	}

	// Verify it can be decrypted
	decrypted, err := DecryptAPIKey(p.APIKeyEncrypted, testEncryptionKey())
	if err != nil {
		t.Fatalf("Failed to decrypt stored key: %v", err)
	}
	if decrypted != "sk-raw-plaintext-key" {
		t.Errorf("decrypted = %q, want %q", decrypted, "sk-raw-plaintext-key")
	}
}

func TestProviderManager_Update_AlreadyEncrypted(t *testing.T) {
	db := setupTestDB(t)
	mgr := NewProviderManager(db, testAgentConfig())
	ctx := context.Background()

	// Create a provider
	p := &Provider{
		UserID:          "user-1",
		Name:            "test",
		ProviderType:    "openai",
		APIKeyEncrypted: "sk-raw-key",
		ModelName:       "gpt-4",
	}
	if err := mgr.Create(ctx, p); err != nil {
		t.Fatalf("Create: %v", err)
	}

	encryptedKey := p.APIKeyEncrypted

	// Update without changing API key
	p.Name = "test-renamed"
	if err := mgr.Update(ctx, p); err != nil {
		t.Fatalf("Update: %v", err)
	}

	// The already-encrypted key should remain unchanged
	if p.APIKeyEncrypted != encryptedKey {
		t.Error("Update changed already-encrypted API key")
	}
}

func TestProviderManager_Update_NewPlaintext(t *testing.T) {
	db := setupTestDB(t)
	mgr := NewProviderManager(db, testAgentConfig())
	ctx := context.Background()

	// Create then update with new plaintext key
	p := &Provider{
		UserID:          "user-1",
		Name:            "test",
		ProviderType:    "openai",
		APIKeyEncrypted: "sk-old-key",
		ModelName:       "gpt-4",
	}
	if err := mgr.Create(ctx, p); err != nil {
		t.Fatalf("Create: %v", err)
	}

	// Update with a new plaintext key
	p.APIKeyEncrypted = "sk-new-plaintext-key"
	if err := mgr.Update(ctx, p); err != nil {
		t.Fatalf("Update: %v", err)
	}

	// Should be encrypted (not matching the input)
	if p.APIKeyEncrypted == "sk-new-plaintext-key" {
		t.Fatal("Updated API key was not encrypted")
	}

	// Should be decryptable
	decrypted, err := DecryptAPIKey(p.APIKeyEncrypted, testEncryptionKey())
	if err != nil {
		t.Fatalf("Failed to decrypt updated key: %v", err)
	}
	if decrypted != "sk-new-plaintext-key" {
		t.Errorf("decrypted = %q, want %q", decrypted, "sk-new-plaintext-key")
	}
}

func TestProviderManager_ListByUser(t *testing.T) {
	db := setupTestDB(t)
	mgr := NewProviderManager(db, testAgentConfig())
	ctx := context.Background()

	p1 := &Provider{UserID: "user-1", Name: "a", ProviderType: "openai", ModelName: "gpt-4"}
	p2 := &Provider{UserID: "user-1", Name: "b", ProviderType: "deepseek", ModelName: "deepseek-chat"}

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
		t.Errorf("len = %d, want 2", len(list))
	}
}

func TestProviderManager_EmptyUserFallsBack(t *testing.T) {
	db := setupTestDB(t)
	mgr := NewProviderManager(db, testAgentConfig())
	ctx := context.Background()

	providers, err := mgr.LoadByUser(ctx, "user-with-no-provider")
	if err != nil {
		t.Fatalf("LoadByUser with no providers: %v", err)
	}
	if len(providers) == 0 {
		t.Fatal("LoadByUser should return platform default when user has none")
	}
	if providers[0].Name != "platform-default" {
		t.Errorf("Name = %q, want %q", providers[0].Name, "platform-default")
	}
}

func TestProviderManager_Delete(t *testing.T) {
	db := setupTestDB(t)
	mgr := NewProviderManager(db, testAgentConfig())
	ctx := context.Background()

	p := &Provider{UserID: "user-1", Name: "test", ProviderType: "openai", ModelName: "gpt-4"}
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

func testEncryptionKey() string {
	return "test-encryption-key-32bytes!!"
}

func testAgentConfig() ClawAgentConfig {
	return ClawAgentConfig{
		EncryptionKey:   testEncryptionKey(),
		DefaultProvider: "openai",
		DefaultModelName: "gpt-4",
		DefaultBaseURL:  "https://api.openai.com/v1",
		DefaultAPIKey:   "sk-platform-default-key",
	}
}
