// Package crypto hosts symmetric encryption helpers for at-rest secrets.
//
// The first resident use case is team_bot_credentials.token_encrypted (Phase
// E3c). The plaintext Gitea PAT is held only transiently — at Provision /
// Rotate time it is returned to the caller exactly once and then dropped.
// Persistence uses AES-256-GCM with a key supplied by the operator via env
// `CS_BOT_TOKEN_KEY` (base64-stdenc of 32 raw bytes).
//
// GCM was chosen over AES-CBC because GCM gives authenticated encryption:
// tampered ciphertext fails to decrypt with crypto/cipher.ErrDecrypt, which
// lets us distinguish "wrong key" (operator misconfig) from "corrupted row"
// (DB bug) without a separate MAC column. AES-256 key length matches the
// 32-byte requirement; AES-128 / AES-192 are intentionally not supported to
// keep the surface narrow.
//
// Nonce handling: each Seal call generates a fresh random 12-byte nonce and
// prepends it to the ciphertext (Standard pattern — crypto/cipher docs §GCM).
// Decrypt reads it back from the prefix. Nonce reuse is the only GCM
// catastrophe; we trust crypto/rand to not collide within 2^32 calls per key.
package crypto

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
)

// KeyLenBytes is the only accepted AES key length. AES-256 — 32 raw bytes.
const KeyLenBytes = 32

// NonceLenBytes is GCM's standard nonce length. Hardcoded by crypto/cipher.
const NonceLenBytes = 12

// ErrInvalidKey signals the supplied key isn't 32 bytes after base64 decode.
// Surfaced as a sentinel so main.go's boot-time check can fail loudly with
// an actionable message.
var ErrInvalidKey = errors.New("crypto: key must be 32 raw bytes (base64-stdenc)")

// ErrCiphertextTooShort covers Decrypt given a value shorter than one nonce.
// Almost always indicates a corrupt row, not a wrong key.
var ErrCiphertextTooShort = errors.New("crypto: ciphertext shorter than nonce")

// AESGCM is the small façade over crypto/aes + crypto/cipher.GCM. Construct
// once with NewAESGCM and reuse; methods are goroutine-safe (no per-call
// state mutated).
type AESGCM struct {
	gcm cipher.AEAD
}

// NewAESGCM builds an AES-256-GCM façade from a 32-byte key. Use
// DecodeBase64Key first if the key arrives as base64 (the typical env shape).
func NewAESGCM(key []byte) (*AESGCM, error) {
	if len(key) != KeyLenBytes {
		return nil, ErrInvalidKey
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("crypto: aes.NewCipher: %w", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("crypto: cipher.NewGCM: %w", err)
	}
	return &AESGCM{gcm: gcm}, nil
}

// DecodeBase64Key decodes a base64-stdenc string of 32 raw bytes. Standard
// encoding only — urlenc / raw variants rejected so operator errors surface
// deterministically. Use hex if the key arrives as a hex string instead.
func DecodeBase64Key(s string) ([]byte, error) {
	raw, err := base64.StdEncoding.DecodeString(s)
	if err != nil {
		return nil, fmt.Errorf("crypto: base64 decode: %w", err)
	}
	if len(raw) != KeyLenBytes {
		return nil, ErrInvalidKey
	}
	return raw, nil
}

// Seal encrypts plaintext and returns base64-stdenc(nonce || ciphertext || tag).
// The output is safe to store in a TEXT column.
func (a *AESGCM) Seal(plaintext []byte) (string, error) {
	if a == nil || a.gcm == nil {
		return "", errors.New("crypto: nil AESGCM")
	}
	nonce := make([]byte, NonceLenBytes)
	if _, err := rand.Read(nonce); err != nil {
		return "", fmt.Errorf("crypto: rand.Read: %w", err)
	}
	// Seal appends ciphertext+tag to dst and returns the slice. We seed dst
	// with the nonce so the output is self-describing.
	out := a.gcm.Seal(nonce, nonce, plaintext, nil)
	return base64.StdEncoding.EncodeToString(out), nil
}

// Open is the inverse of Seal: takes base64-stdenc(nonce||ct||tag) and
// returns the plaintext. Returns cipher.ErrAuth (re-wrapped) on tamper or
// wrong key — callers must NOT treat these as recoverable.
func (a *AESGCM) Open(b64 string) ([]byte, error) {
	if a == nil || a.gcm == nil {
		return nil, errors.New("crypto: nil AESGCM")
	}
	raw, err := base64.StdEncoding.DecodeString(b64)
	if err != nil {
		return nil, fmt.Errorf("crypto: base64 decode: %w", err)
	}
	if len(raw) < NonceLenBytes+a.gcm.Overhead() {
		return nil, ErrCiphertextTooShort
	}
	nonce, ct := raw[:NonceLenBytes], raw[NonceLenBytes:]
	pt, err := a.gcm.Open(nil, nonce, ct, nil)
	if err != nil {
		return nil, fmt.Errorf("crypto: gcm.Open: %w", err)
	}
	return pt, nil
}

// SHA256Hex is a tiny convenience used to compute the
// team_bot_credentials.token_sha256 fingerprint column. Hex (lowercase) keeps
// the column type CHAR(64) and matches how GitHub / Gitea surface token SHAs.
//
// Not in crypto/sha256 directly because the call site is repeated and the
// hex encoding is the same every time.
func SHA256Hex(b []byte) string {
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}
