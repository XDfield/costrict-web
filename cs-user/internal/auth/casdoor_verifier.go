// Casdoor JWT verifier. Used by the reissue-token handler to drop the
// legacy "trust server-supplied parsed claims" contract: when configured,
// cs-user fetches Casdoor's JWKS itself and re-validates any raw Casdoor
// JWT forwarded by server. Failure → 401 (handler translates).
//
// Design notes:
//   - JWKS key set is cached (RefreshTTL, default 15m) — see Plan §设计原则.
//     "No cache" in the user's intent refers to the verification RESULT,
//     not the JWKS keys themselves; fetching JWKS per request would DDOS
//     Casdoor.
//   - Unknown `kid` triggers a single synchronous refresh per request. If
//     refresh still doesn't yield the kid, verification fails (rotation
//     race beyond the TTL window is the operator's responsibility).
//   - Only RS256 accepted. Matches cs-user's own signer (Phase A scope).

package auth

import (
	"context"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/costrict/costrict-web/cs-user/internal/models"
	"github.com/golang-jwt/jwt/v5"
)

// ErrCasdoorVerifierDisabled signals the verifier has no JWKSURL configured.
// The reissue-token handler treats this distinctly from a verification
// failure — disabled means "fall through to legacy trust path" (warn log),
// not "reject".
var ErrCasdoorVerifierDisabled = errors.New("casdoor verifier disabled: JWKSURL not configured")

// ErrCasdoorJWTInvalid is the umbrella error for any verification failure
// (bad signature, expired, wrong iss/aud, malformed). The handler maps it
// to 401 — callers MUST NOT distinguish sub-cases when surfacing to
// clients, to avoid leaking which check failed.
var ErrCasdoorJWTInvalid = errors.New("invalid casdoor jwt")

// CasdoorVerifier fetches Casdoor's JWKS and verifies raw Casdoor JWTs.
// Construct once at startup; share across requests. Safe for concurrent use.
type CasdoorVerifier struct {
	jwksURL    string
	issuer     string
	audience   []string
	client     *http.Client
	refreshTTL time.Duration

	mu     sync.RWMutex
	cached *cachedKeySet
}

type cachedKeySet struct {
	keys      map[string]*rsa.PublicKey
	fetchedAt time.Time
}

// NewCasdoorVerifier constructs a verifier from config. Returns nil when
// JWKSURL is empty — handlers treat nil as "verification disabled" and
// preserve the legacy trust path.
func NewCasdoorVerifier(jwksURL, issuer string, audience []string, httpTimeout, refreshTTL time.Duration) *CasdoorVerifier {
	jwksURL = strings.TrimSpace(jwksURL)
	if jwksURL == "" {
		return nil
	}
	if httpTimeout <= 0 {
		httpTimeout = 5 * time.Second
	}
	if refreshTTL <= 0 {
		refreshTTL = 15 * time.Minute
	}
	return &CasdoorVerifier{
		jwksURL:    jwksURL,
		issuer:     strings.TrimSpace(issuer),
		audience:   audience,
		client:     &http.Client{Timeout: httpTimeout},
		refreshTTL: refreshTTL,
	}
}

// Verify validates a raw Casdoor JWT and returns the parsed claims. Every
// well-known failure path returns a wrapped ErrCasdoorJWTInvalid so callers
// can `errors.Is(err, ErrCasdoorJWTInvalid)` without caring which check
// tripped. A nil receiver returns ErrCasdoorVerifierDisabled.
func (v *CasdoorVerifier) Verify(ctx context.Context, rawJWT string) (*models.JWTClaims, error) {
	if v == nil {
		return nil, ErrCasdoorVerifierDisabled
	}
	if strings.TrimSpace(rawJWT) == "" {
		return nil, fmt.Errorf("%w: empty token", ErrCasdoorJWTInvalid)
	}

	// Use MapClaims so jwt/v5 enforces exp/nbf/iat for us; we then copy the
	// well-known fields into models.JWTClaims. models.JWTClaims does not
	// implement jwt.Claims (no GetExpirationTime etc.) so direct use would
	// skip registered-claim enforcement.
	mapClaims := jwt.MapClaims{}
	_, err := jwt.ParseWithClaims(rawJWT, mapClaims, func(t *jwt.Token) (interface{}, error) {
		// Pin RS256 — Phase A scope deliberately rejects other algs to
		// keep the audit surface small. An attacker advertising HS256
		// with the public key as HMAC secret would otherwise bypass.
		if _, ok := t.Method.(*jwt.SigningMethodRSA); !ok {
			return nil, fmt.Errorf("%w: unexpected alg %v", ErrCasdoorJWTInvalid, t.Header["alg"])
		}
		kid, _ := t.Header["kid"].(string)
		key, keyErr := v.lookupKey(ctx, kid)
		if keyErr != nil {
			return nil, keyErr
		}
		return key, nil
	},
		jwt.WithValidMethods([]string{"RS256"}),
		jwt.WithExpirationRequired(),
	)
	if err != nil {
		return nil, fmt.Errorf("%w: %s", ErrCasdoorJWTInvalid, err.Error())
	}

	if err := v.validateRegistered(mapClaims); err != nil {
		return nil, err
	}
	return mapClaimsToModel(mapClaims), nil
}

// validateRegistered performs the iss/aud checks that jwt/v5 cannot do
// without parser options tied to specific values (we want them configurable
// at verifier level, not per-parse).
func (v *CasdoorVerifier) validateRegistered(claims jwt.MapClaims) error {
	if v.issuer != "" {
		iss, _ := claims["iss"].(string)
		if iss != v.issuer {
			return fmt.Errorf("%w: iss mismatch", ErrCasdoorJWTInvalid)
		}
	}
	if len(v.audience) > 0 {
		var auds []string
		switch a := claims["aud"].(type) {
		case string:
			auds = []string{a}
		case []any:
			for _, x := range a {
				if s, ok := x.(string); ok {
					auds = append(auds, s)
				}
			}
		}
		matched := false
		for _, want := range v.audience {
			for _, got := range auds {
				if want == got {
					matched = true
					break
				}
			}
			if matched {
				break
			}
		}
		if !matched {
			return fmt.Errorf("%w: aud mismatch", ErrCasdoorJWTInvalid)
		}
	}
	return nil
}

// mapClaimsToModel copies Casdoor's standard claim fields into the
// models.JWTClaims wire struct. Non-string fields are coerced defensively;
// any unknown claim is dropped (forward-compat: future Casdoor fields don't
// need verifier changes).
func mapClaimsToModel(claims jwt.MapClaims) *models.JWTClaims {
	out := &models.JWTClaims{}
	out.ID, _ = claims["id"].(string)
	out.Sub, _ = claims["sub"].(string)
	out.UniversalID, _ = claims["universal_id"].(string)
	out.Name, _ = claims["name"].(string)
	out.PreferredUsername, _ = claims["preferred_username"].(string)
	out.Email, _ = claims["email"].(string)
	out.Picture, _ = claims["picture"].(string)
	out.Owner, _ = claims["owner"].(string)
	out.Provider, _ = claims["provider"].(string)
	out.ProviderUserID, _ = claims["provider_user_id"].(string)
	out.Phone, _ = claims["phone"].(string)
	if ext, ok := claims["external_claims"].(map[string]any); ok {
		out.ExternalClaims = ext
	}
	return out
}

// lookupKey returns the RSA public key for kid, refreshing the cached JWKS
// when missing or stale.
func (v *CasdoorVerifier) lookupKey(ctx context.Context, kid string) (*rsa.PublicKey, error) {
	if key, ok := v.cacheGet(kid); ok {
		return key, nil
	}
	if err := v.refresh(ctx); err != nil {
		return nil, fmt.Errorf("%w: jwks fetch: %s", ErrCasdoorJWTInvalid, err.Error())
	}
	if key, ok := v.cacheGet(kid); ok {
		return key, nil
	}
	return nil, fmt.Errorf("%w: kid %q not in JWKS", ErrCasdoorJWTInvalid, kid)
}

func (v *CasdoorVerifier) cacheGet(kid string) (*rsa.PublicKey, bool) {
	v.mu.RLock()
	defer v.mu.RUnlock()
	if v.cached == nil {
		return nil, false
	}
	if time.Since(v.cached.fetchedAt) > v.refreshTTL {
		return nil, false
	}
	key, ok := v.cached.keys[kid]
	return key, ok
}

func (v *CasdoorVerifier) refresh(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, v.jwksURL, nil)
	if err != nil {
		return err
	}
	resp, err := v.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("jwks endpoint %s: status %d body=%q", v.jwksURL, resp.StatusCode, strings.TrimSpace(string(body)))
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20)) // 1 MiB cap — Casdoor JWKS is tiny
	if err != nil {
		return err
	}
	var wire JWKS
	if err := json.Unmarshal(body, &wire); err != nil {
		return fmt.Errorf("decode jwks: %w", err)
	}
	keys := make(map[string]*rsa.PublicKey, len(wire.Keys))
	for i, k := range wire.Keys {
		if k.Kty != "RSA" {
			continue
		}
		pub, err := jwkToRSAPublicKey(k)
		if err != nil {
			return fmt.Errorf("jwks key[%d] kid=%s: %w", i, k.Kid, err)
		}
		if k.Kid == "" {
			continue
		}
		keys[k.Kid] = pub
	}
	if len(keys) == 0 {
		return errors.New("jwks has no usable RSA keys")
	}

	v.mu.Lock()
	v.cached = &cachedKeySet{keys: keys, fetchedAt: time.Now()}
	v.mu.Unlock()
	return nil
}

// jwkToRSAPublicKey reconstructs an *rsa.PublicKey from a JWK's base64url
// n/e. Reused for verification; the signer package has its own thumbprint
// helper going the other direction.
func jwkToRSAPublicKey(k JWK) (*rsa.PublicKey, error) {
	nBytes, err := base64.RawURLEncoding.DecodeString(k.N)
	if err != nil {
		return nil, fmt.Errorf("decode n: %w", err)
	}
	eBytes, err := base64.RawURLEncoding.DecodeString(k.E)
	if err != nil {
		return nil, fmt.Errorf("decode e: %w", err)
	}
	e := 0
	for _, b := range eBytes {
		e = e<<8 + int(b)
	}
	if e < 2 {
		return nil, errors.New("exponent too small")
	}
	mod := new(big.Int).SetBytes(nBytes)
	if mod.Sign() <= 0 {
		return nil, errors.New("modulus must be positive")
	}
	return &rsa.PublicKey{N: mod, E: e}, nil
}
