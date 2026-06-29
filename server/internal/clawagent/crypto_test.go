package clawagent

import (
	"strings"
	"testing"
)

func TestEncryptDecryptAPIKey_Roundtrip(t *testing.T) {
	key := "test-master-key-32-bytes-long-for-testing"
	plaintext := "sk-my-secret-api-key-12345"

	encrypted, err := EncryptAPIKey(plaintext, key)
	if err != nil {
		t.Fatalf("EncryptAPIKey failed: %v", err)
	}

	if encrypted == "" {
		t.Fatal("EncryptAPIKey returned empty string")
	}

	if encrypted == plaintext {
		t.Fatal("EncryptAPIKey returned plaintext (no encryption)")
	}

	decrypted, err := DecryptAPIKey(encrypted, key)
	if err != nil {
		t.Fatalf("DecryptAPIKey failed: %v", err)
	}

	if decrypted != plaintext {
		t.Fatalf("DecryptAPIKey = %q, want %q", decrypted, plaintext)
	}
}

func TestEncryptAPIKey_DeterministicDifferentNonce(t *testing.T) {
	key := "test-master-key-32-bytes-long-for-testing"
	plaintext := "sk-my-secret-api-key-12345"

	enc1, _ := EncryptAPIKey(plaintext, key)
	enc2, _ := EncryptAPIKey(plaintext, key)

	if enc1 == enc2 {
		t.Fatal("Two encryptions of same plaintext produced same ciphertext (nonce not random)")
	}
}

func TestDecryptAPIKey_WrongKey(t *testing.T) {
	encrypted, err := EncryptAPIKey("sk-secret", "correct-master-key-for-test-12345")
	if err != nil {
		t.Fatalf("EncryptAPIKey failed: %v", err)
	}

	_, err = DecryptAPIKey(encrypted, "wrong-master-key-for-test-1234567")
	if err == nil {
		t.Fatal("DecryptAPIKey with wrong key should fail")
	}
}

func TestDecryptAPIKey_InvalidData(t *testing.T) {
	_, err := DecryptAPIKey("not-valid-base64!!!", "some-key")
	if err == nil {
		t.Fatal("DecryptAPIKey with invalid base64 should fail")
	}

	_, err = DecryptAPIKey("aGVsbG8=", "some-key")
	if err == nil {
		t.Fatal("DecryptAPIKey with short data should fail")
	}
}

func TestDecryptAPIKey_EmptyString(t *testing.T) {
	_, err := DecryptAPIKey("", "some-key")
	if err == nil {
		t.Fatal("DecryptAPIKey with empty string should fail")
	}
}

func TestEncryptDecryptAPIKey_EmptyPlaintext(t *testing.T) {
	key := "test-master-key-32-bytes-long-for-testing"

	encrypted, err := EncryptAPIKey("", key)
	if err != nil {
		t.Fatalf("EncryptAPIKey empty plaintext failed: %v", err)
	}

	decrypted, err := DecryptAPIKey(encrypted, key)
	if err != nil {
		t.Fatalf("DecryptAPIKey after empty encrypt failed: %v", err)
	}
	if decrypted != "" {
		t.Fatalf("Decrypted empty = %q, want empty", decrypted)
	}
}

func TestEncryptAPIKey_DifferentKeys(t *testing.T) {
	plaintext := "sk-secret"
	key1 := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	key2 := "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"

	enc1, _ := EncryptAPIKey(plaintext, key1)
	enc2, _ := EncryptAPIKey(plaintext, key2)

	if enc1 == enc2 {
		t.Fatal("Different keys produced same ciphertext")
	}
}

func TestEncryptAPIKey_KeyDerivation(t *testing.T) {
	// PBKDF2 derivation should produce different results for different keys
	key1 := deriveKey("short")
	key2 := deriveKey("different")

	if len(key1) != 32 {
		t.Fatalf("deriveKey produced %d bytes, want 32", len(key1))
	}

	if string(key1) == string(key2) {
		t.Fatal("deriveKey: different inputs produced same key")
	}
}

func TestEstimateTokens(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		min      int
	}{
		{"empty", "", 0},
		{"short text", "hello", 1},
		{"100 chars", strings.Repeat("a", 100), 20},
		{"1000 chars", strings.Repeat("b", 1000), 240},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := estimateTokens(tt.input)
			if got < tt.min {
				t.Errorf("estimateTokens(%q) = %d, want >= %d", tt.input[:min(len(tt.input), 20)], got, tt.min)
			}
			if got < 0 {
				t.Errorf("estimateTokens returned negative: %d", got)
			}
		})
	}
}

func TestResetTypeOf(t *testing.T) {
	tests := []struct {
		baseKey string
		want    string
	}{
		{"agent:clawagent:wecom-bot:chat123:user456", "direct"},
		{"agent:clawagent:wecom-bot:chat123:group", "group"},
		{"agent:clawagent:wecom-bot:chat123:group:thread:t1", "thread"},
		{"agent:clawagent:event:permission:evt1", "direct"},
		{"single", "direct"},
	}

	for _, tt := range tests {
		t.Run(tt.baseKey, func(t *testing.T) {
			got := resetTypeOf(tt.baseKey)
			if got != tt.want {
				t.Errorf("resetTypeOf(%q) = %q, want %q", tt.baseKey, got, tt.want)
			}
		})
	}
}

func TestNewSessionID(t *testing.T) {
	id := NewSessionID("agent:clawagent:test:u1", 1)
	want := "agent:clawagent:test:u1:v1"
	if id != want {
		t.Errorf("NewSessionID = %q, want %q", id, want)
	}

	id = NewSessionID("base", 5)
	want = "base:v5"
	if id != want {
		t.Errorf("NewSessionID = %q, want %q", id, want)
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
