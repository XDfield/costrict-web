package crypto

import (
	"encoding/base64"
	"strings"
	"testing"
)

func testKey(t *testing.T) []byte {
	t.Helper()
	// 32 raw bytes — anything deterministic for tests; production reads env.
	raw := make([]byte, KeyLenBytes)
	for i := range raw {
		raw[i] = byte(i)
	}
	return raw
}

func TestNewAESGCM_RejectsWrongKeyLengths(t *testing.T) {
	cases := []int{0, 16, 24, 31, 33}
	for _, n := range cases {
		k := make([]byte, n)
		if _, err := NewAESGCM(k); err == nil {
			t.Errorf("key len %d: want error, got nil", n)
		}
	}
}

func TestSealOpen_RoundTrip(t *testing.T) {
	a, err := NewAESGCM(testKey(t))
	if err != nil {
		t.Fatalf("NewAESGCM: %v", err)
	}
	plaintext := []byte("bot-token-12345")
	ct, err := a.Seal(plaintext)
	if err != nil {
		t.Fatalf("Seal: %v", err)
	}
	if ct == "" {
		t.Fatal("ciphertext empty")
	}
	// Two Seals of the same plaintext produce different ciphertexts (random nonce).
	ct2, _ := a.Seal(plaintext)
	if ct == ct2 {
		t.Error("nonce reuse: two Seal calls produced identical ciphertext")
	}
	got, err := a.Open(ct)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if string(got) != string(plaintext) {
		t.Errorf("got %q, want %q", got, plaintext)
	}
}

func TestOpen_WrongKeyFails(t *testing.T) {
	encryptor, _ := NewAESGCM(testKey(t))
	ct, _ := encryptor.Seal([]byte("secret"))

	other := make([]byte, KeyLenBytes)
	for i := range other {
		other[i] = byte(i + 1)
	}
	decryptor, err := NewAESGCM(other)
	if err != nil {
		t.Fatalf("NewAESGCM(other): %v", err)
	}
	if _, err := decryptor.Open(ct); err == nil {
		t.Error("Open with wrong key: want error, got nil")
	}
}

func TestOpen_TamperedCiphertextFails(t *testing.T) {
	a, _ := NewAESGCM(testKey(t))
	ct, _ := a.Seal([]byte("secret"))

	raw, _ := base64.StdEncoding.DecodeString(ct)
	raw[len(raw)-1] ^= 0xFF // flip a bit in the GCM tag
	tampered := base64.StdEncoding.EncodeToString(raw)

	_, err := a.Open(tampered)
	if err == nil {
		t.Fatal("Open tampered: want error, got nil")
	}
	if !strings.Contains(err.Error(), "gcm") {
		t.Errorf("got %v, want error mentioning gcm authentication failure", err)
	}
}

func TestOpen_ShortCiphertext(t *testing.T) {
	a, _ := NewAESGCM(testKey(t))
	short := base64.StdEncoding.EncodeToString([]byte("tooshort"))
	if _, err := a.Open(short); err == nil {
		t.Error("Open short ciphertext: want error, got nil")
	}
}

func TestDecodeBase64Key(t *testing.T) {
	raw := testKey(t)
	b64 := base64.StdEncoding.EncodeToString(raw)
	got, err := DecodeBase64Key(b64)
	if err != nil {
		t.Fatalf("DecodeBase64Key: %v", err)
	}
	if len(got) != KeyLenBytes {
		t.Errorf("got %d bytes, want %d", len(got), KeyLenBytes)
	}
}

func TestDecodeBase64Key_RejectsShort(t *testing.T) {
	short := base64.StdEncoding.EncodeToString([]byte("only-12-bytes"))
	if _, err := DecodeBase64Key(short); err == nil {
		t.Error("short base64 key: want error, got nil")
	}
}

func TestSHA256Hex(t *testing.T) {
	got := SHA256Hex(nil)
	const want = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}
