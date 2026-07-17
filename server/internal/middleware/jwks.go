package middleware

import (
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/costrict/costrict-web/server/internal/logger"
)

// JWKSProvider fetches and caches JSON Web Key Sets from one or more OIDC
// issuers (Casdoor in Phase 0; cs-user joins in Phase A8 dual-sign mode). It
// supports automatic refresh and on-demand re-fetch when an unknown key ID is
// encountered.
//
// Multi-source semantics (Phase A8): when configured with multiple URLs, all
// sources are fetched on each refresh and their keys are merged into a single
// kid→key map. A single source failing (network / 4xx / no keys) is logged
// but does NOT fail the refresh as long as at least one source yields keys —
// this lets dual-sign mode tolerate a transient cs-user outage without
// locking out users holding valid Casdoor tokens (and vice versa).
type JWKSProvider struct {
	sources    []string
	mu         sync.RWMutex
	keys       map[string]*rsa.PublicKey // kid -> public key
	lastFetch  time.Time
	minRefresh time.Duration // minimum interval between remote fetches
	httpClient *http.Client
}

type jwksResponse struct {
	Keys []jwkKey `json:"keys"`
}

type jwkKey struct {
	Kty string `json:"kty"`
	Use string `json:"use"`
	Kid string `json:"kid"`
	Alg string `json:"alg"`
	N   string `json:"n"`
	E   string `json:"e"`
}

// NewJWKSProvider creates a provider that fetches keys from the given base
// URL (the /.well-known/jwks suffix is appended internally). casdoorEndpoint
// should be the base URL, e.g. "http://localhost:8000".
//
// Phase A8 also uses this constructor for cs-user-only configurations (single
// mode) by passing the cs-user base URL; the suffix is the same RFC 8615 path
// both Casdoor and cs-user expose.
func NewJWKSProvider(baseURL string) *JWKSProvider {
	return &JWKSProvider{
		sources:    []string{baseURL + "/.well-known/jwks"},
		keys:       make(map[string]*rsa.PublicKey),
		minRefresh: 5 * time.Minute,
		httpClient: &http.Client{Timeout: 10 * time.Second},
	}
}

// NewMultiJWKSProvider creates a provider that fetches keys from multiple base
// URLs and merges them. Used in Phase A8 dual-sign mode where the verifier
// must accept tokens signed by either cs-user (new sessions) or Casdoor (old
// sessions during the 30-day crossover window). The order of endpoints does
// not matter — keys are merged into a single kid→key map and looked up by kid.
//
// Empty / duplicate entries are dropped silently. If the resulting source list
// is empty, the provider returns no keys (Preload logs a warning; GetKey
// returns an error).
func NewMultiJWKSProvider(baseURLs []string) *JWKSProvider {
	seen := make(map[string]bool)
	sources := make([]string, 0, len(baseURLs))
	for _, u := range baseURLs {
		u = strings.TrimSpace(u)
		if u == "" {
			continue
		}
		full := u + "/.well-known/jwks"
		if seen[full] {
			continue
		}
		seen[full] = true
		sources = append(sources, full)
	}
	return &JWKSProvider{
		sources:    sources,
		keys:       make(map[string]*rsa.PublicKey),
		minRefresh: 5 * time.Minute,
		httpClient: &http.Client{Timeout: 10 * time.Second},
	}
}

// GetKey returns the RSA public key for the given key ID.
// If the key is not cached, it attempts a remote fetch (rate-limited).
// If kid is empty, returns the first available key (single-key issuers like
// Casdoor rely on this; multi-key issuers like cs-user always populate kid).
func (p *JWKSProvider) GetKey(kid string) (*rsa.PublicKey, error) {
	// Try cached key first
	p.mu.RLock()
	if kid == "" {
		// Return first available key
		for _, key := range p.keys {
			p.mu.RUnlock()
			return key, nil
		}
		p.mu.RUnlock()
	} else {
		key, ok := p.keys[kid]
		p.mu.RUnlock()
		if ok {
			return key, nil
		}
	}

	// Cache miss — try to refresh from remote
	if err := p.refresh(); err != nil {
		return nil, fmt.Errorf("JWKS fetch failed: %w", err)
	}

	// Retry from cache
	p.mu.RLock()
	defer p.mu.RUnlock()
	if kid == "" {
		for _, key := range p.keys {
			return key, nil
		}
		return nil, fmt.Errorf("no keys available in JWKS")
	}
	key, ok := p.keys[kid]
	if !ok {
		return nil, fmt.Errorf("key %q not found in JWKS", kid)
	}
	return key, nil
}

// Preload fetches keys eagerly at startup. Non-fatal: logs a warning on failure.
func (p *JWKSProvider) Preload() {
	if err := p.refresh(); err != nil {
		logger.Warn("[JWKS] initial key fetch failed (JWT verification will fall back to Casdoor API): %v", err)
	} else {
		p.mu.RLock()
		count := len(p.keys)
		p.mu.RUnlock()
		logger.Info("[JWKS] loaded %d signing key(s) from %d source(s): %s", count, len(p.sources), strings.Join(p.sources, ", "))
	}
}

// refresh fetches the JWKS from all configured sources, rate-limited by
// minRefresh. A source-level failure (network / non-200 / decode error / no
// RSA keys) is logged but does not fail the refresh UNLESS every source fails
// — that way a transient cs-user outage during dual-sign 灰度 does not evict
// working Casdoor keys. The merged key map replaces the cached one only when
// at least one source contributed keys.
func (p *JWKSProvider) refresh() error {
	p.mu.Lock()
	if time.Since(p.lastFetch) < p.minRefresh {
		p.mu.Unlock()
		return nil // too soon, skip
	}
	p.lastFetch = time.Now()
	sources := append([]string(nil), p.sources...)
	p.mu.Unlock()

	if len(sources) == 0 {
		return fmt.Errorf("no JWKS sources configured")
	}

	merged := make(map[string]*rsa.PublicKey)
	var failures []string
	for _, url := range sources {
		keys, err := fetchJWKS(p.httpClient, url)
		if err != nil {
			failures = append(failures, fmt.Sprintf("%s: %v", url, err))
			continue
		}
		for kid, pub := range keys {
			merged[kid] = pub
		}
	}

	if len(merged) == 0 {
		return fmt.Errorf("all JWKS sources failed (%d): %s", len(failures), strings.Join(failures, "; "))
	}

	if len(failures) > 0 {
		logger.Warn("[JWKS] %d/%d source(s) failed but proceeding with %d key(s): %s",
			len(failures), len(sources), len(merged), strings.Join(failures, "; "))
	}

	p.mu.Lock()
	p.keys = merged
	p.mu.Unlock()
	return nil
}

// fetchJWKS fetches and parses a single JWKS endpoint. Returns the kid→key
// map (with empty-kid keys normalized to "_default" to match the lookup
// convention in GetKey). An empty RSA key set is treated as an error so the
// caller can record the source as failed rather than silently merging nothing.
func fetchJWKS(httpClient *http.Client, url string) (map[string]*rsa.PublicKey, error) {
	resp, err := httpClient.Get(url)
	if err != nil {
		return nil, fmt.Errorf("GET %s: %w", url, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("GET %s returned %d: %s", url, resp.StatusCode, string(body))
	}

	var jwks jwksResponse
	if err := json.NewDecoder(resp.Body).Decode(&jwks); err != nil {
		return nil, fmt.Errorf("decode JWKS: %w", err)
	}

	keys := make(map[string]*rsa.PublicKey)
	for _, k := range jwks.Keys {
		if k.Kty != "RSA" {
			continue
		}
		pub, err := parseRSAPublicKey(k)
		if err != nil {
			logger.Warn("[JWKS] skip key %s: %v", k.Kid, err)
			continue
		}
		kid := k.Kid
		if kid == "" {
			kid = "_default"
		}
		keys[kid] = pub
	}

	if len(keys) == 0 {
		return nil, fmt.Errorf("no valid RSA keys in JWKS response from %s", url)
	}
	return keys, nil
}

// parseRSAPublicKey constructs an *rsa.PublicKey from JWK parameters (n, e).
func parseRSAPublicKey(k jwkKey) (*rsa.PublicKey, error) {
	nBytes, err := base64.RawURLEncoding.DecodeString(k.N)
	if err != nil {
		return nil, fmt.Errorf("decode n: %w", err)
	}
	eBytes, err := base64.RawURLEncoding.DecodeString(k.E)
	if err != nil {
		return nil, fmt.Errorf("decode e: %w", err)
	}

	n := new(big.Int).SetBytes(nBytes)
	e := 0
	for _, b := range eBytes {
		e = e<<8 | int(b)
	}

	return &rsa.PublicKey{N: n, E: e}, nil
}
