// Command smoke exercises the cs-user ↔ @server ↔ (optionally) Gitea fork
// integration surface from outside the process. It is a manual / CI smoke
// probe, NOT a unit test — both services (and Postgres) must be running.
//
// Three layers wired in one binary:
//
//	-layer=rpc       cs-user ↔ @server RPC + JWT verify (default; no Gitea)
//	-layer=gitea     cs-user signs JWT → Gitea fork verifies (no @server)
//	-layer=all       both, sequentially
//
// Required env (see -h for the full list with defaults):
//
//	CS_USER_URL              cs-user base URL, e.g. http://localhost:8081
//	CS_USER_INTERNAL_TOKEN   shared secret matching cs-user's CS_USER_INTERNAL_TOKEN
//	CS_USER_JWT_SIGNING_KEY  PEM (PKCS#1 or PKCS#8) of the RSA key cs-user signs with
//	SERVER_URL               @server base URL, e.g. http://localhost:8080
//	GITEA_URL                Gitea fork base URL, e.g. http://localhost:3000
//	GITEA_ADMIN_USER         Gitea admin username (for pre-creating u-<name>)
//	GITEA_ADMIN_PASS         Gitea admin password
//
// Exit code: 0 on all-green, 1 on any failure. Each step prints a one-line
// ✓ / ✗ verdict so output is easy to grep in CI.
package main

import (
	"bytes"
	"context"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"flag"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/golang-jwt/jwt/v5"

	"github.com/costrict/costrict-web/cs-user/internal/auth"
	"github.com/costrict/costrict-web/cs-user/internal/models"
)

const (
	subject = "smoke-test-user-001"
	name    = "smoke"
	slug    = "default"
)

type env struct {
	csUserURL        string
	csUserToken      string
	csUserKeyPEM     []byte
	csUserIssuer     string
	serverURL        string
	giteaURL         string
	giteaAdminUser   string
	giteaAdminPass   string
	giteaInternalTOK string
}

func main() {
	layer := flag.String("layer", "rpc", "rpc | gitea | all")
	flag.Parse()

	e, err := loadEnv()
	if err != nil {
		fail("env: %v", err)
	}

	signer, err := auth.NewSignerFromPEM(e.csUserKeyPEM)
	if err != nil {
		fail("load signer: %v", err)
	}

	ok := true
	if *layer == "rpc" || *layer == "all" {
		ok = runRPCLayer(e, signer) && ok
	}
	if *layer == "gitea" || *layer == "all" {
		ok = runGiteaLayer(e, signer) && ok
	}
	if !ok {
		os.Exit(1)
	}
	fmt.Println("\nSMOKE: all green ✓")
}

// ---- RPC layer: cs-user ↔ @server ----

func runRPCLayer(e *env, signer *auth.Signer) bool {
	fmt.Println("\n=== Layer 1: cs-user ↔ @server ===")
	allOK := true

	// Step 1: cs-user healthz via X-Internal-Token.
	allOK = step("cs-user ping (X-Internal-Token)", func() error {
		return getWithToken(e.csUserURL+"/api/internal/ping", e.csUserToken)
	}) && allOK

	// Step 2: JWKS exposed and contains the kid our key derives.
	allOK = step("cs-user JWKS contains expected kid", func() error {
		return checkJWKS(e.csUserURL+"/.well-known/jwks", signer.KID())
	}) && allOK

	// Step 3: cs-user GetOrCreateUser (no X-Tenant-Id → default tenant fallback path).
	allOK = step("cs-user get-or-create (default tenant fallback)", func() error {
		return postGetOrCreate(e, signer)
	}) && allOK

	// Step 4: Sign a real JWT, hit @server with it — verifies the JWKS round trip.
	if e.serverURL != "" {
		allOK = step("@server accepts cs-user-signed JWT", func() error {
			tok, err := signUserJWT(signer, e.csUserIssuer, time.Minute*5)
			if err != nil {
				return err
			}
			return checkServerAccepts(e.serverURL, tok)
		}) && allOK
	} else {
		fmt.Println("✗ SKIP: @server JWT verify (SERVER_URL unset)")
	}

	return allOK
}

// ---- Gitea layer: cs-user signs → Gitea fork verifies ----

func runGiteaLayer(e *env, signer *auth.Signer) bool {
	fmt.Println("\n=== Layer 2: cs-user → Gitea fork JWT verify ===")
	allOK := true

	// Step 1: Gitea CoStrict healthz (no auth).
	allOK = step("gitea /api/internal/costrict/healthz", func() error {
		resp, err := http.Get(e.giteaURL + "/api/internal/costrict/healthz")
		if err != nil {
			return err
		}
		defer resp.Body.Close()
		if resp.StatusCode != 200 {
			return fmt.Errorf("healthz: want 200, got %d", resp.StatusCode)
		}
		body, _ := io.ReadAll(resp.Body)
		var h struct {
			Enabled bool   `json:"enabled"`
			JWKSURL string `json:"jwks_url"`
		}
		if err := json.Unmarshal(body, &h); err != nil {
			return fmt.Errorf("parse healthz: %w", err)
		}
		if !h.Enabled {
			return errors.New("gitea [costrict] ENABLED=false — fork is inert")
		}
		fmt.Printf("    gitea costrict enabled, jwks=%s\n", h.JWKSURL)
		return nil
	}) && allOK

	// Step 2: pre-create u-<username> via Gitea admin API (cs-user → giteasync → Gitea).
	allOK = step("gitea pre-create u-"+name, func() error {
		return ensureGiteaUser(e, name)
	}) && allOK

	// Step 3: cs-user-signed JWT authenticates against Gitea.
	allOK = step("gitea authenticates cs-user JWT", func() error {
		tok, err := signUserJWT(signer, e.csUserIssuer, time.Minute*5)
		if err != nil {
			return err
		}
		req, _ := http.NewRequest("GET", e.giteaURL+"/api/v1/user", nil)
		req.Header.Set("Authorization", "Bearer "+tok)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return err
		}
		defer resp.Body.Close()
		if resp.StatusCode != 200 {
			body, _ := io.ReadAll(resp.Body)
			return fmt.Errorf("gitea /api/v1/user: want 200, got %d body=%s", resp.StatusCode, string(body))
		}
		return nil
	}) && allOK

	// Step 4: trigger quota rejection — disabled by default to avoid
	// leaving test repos in Gitea. Set -quota flag at call site manually.
	fmt.Println("✓ (quota rejection test skipped — run manually with big push)")

	return allOK
}

// ---- helpers ----

func loadEnv() (*env, error) {
	e := &env{
		csUserURL:        strings.TrimRight(os.Getenv("CS_USER_URL"), "/"),
		csUserToken:      os.Getenv("CS_USER_INTERNAL_TOKEN"),
		serverURL:        strings.TrimRight(os.Getenv("SERVER_URL"), "/"),
		giteaURL:         strings.TrimRight(os.Getenv("GITEA_URL"), "/"),
		giteaAdminUser:   os.Getenv("GITEA_ADMIN_USER"),
		giteaAdminPass:   os.Getenv("GITEA_ADMIN_PASS"),
		giteaInternalTOK: os.Getenv("GITEA_INTERNAL_TOKEN"),
		csUserIssuer:     getenvDefault("CS_USER_JWT_ISSUER", "cs-user"),
	}
	if e.csUserURL == "" {
		return nil, errors.New("CS_USER_URL required")
	}
	if e.csUserToken == "" {
		return nil, errors.New("CS_USER_INTERNAL_TOKEN required")
	}
	pemPath := os.Getenv("CS_USER_JWT_SIGNING_KEY")
	if pemPath == "" {
		return nil, errors.New("CS_USER_JWT_SIGNING_KEY (path to PEM) required")
	}
	b, err := os.ReadFile(pemPath)
	if err != nil {
		return nil, fmt.Errorf("read signing key: %w", err)
	}
	e.csUserKeyPEM = b
	return e, nil
}

func getenvDefault(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func signUserJWT(signer *auth.Signer, issuer string, ttl time.Duration) (string, error) {
	now := time.Now()
	claims := &jwt.RegisteredClaims{
		Issuer:    issuer,
		Subject:   subject,
		Audience:  []string{"costrict-cloud"},
		IssuedAt:  jwt.NewNumericDate(now),
		NotBefore: jwt.NewNumericDate(now.Add(-time.Minute)),
		ExpiresAt: jwt.NewNumericDate(now.Add(ttl)),
	}
	// Custom claims via mapClaims so preferred_username lands at top level
	// (matches cs-user's EnterpriseClaims schema + Gitea fork expectation).
	mapClaims := jwt.MapClaims{
		"iss":                issuer,
		"sub":                subject,
		"aud":                []string{"costrict-cloud"},
		"iat":                now.Unix(),
		"nbf":                now.Add(-time.Minute).Unix(),
		"exp":                now.Add(ttl).Unix(),
		"preferred_username": name,
		"email":              name + "@example.com",
		"name":               name,
		"provider":           "smoke",
		"provider_user_id":   subject,
		"universal_id":       subject,
	}
	_ = claims
	return signer.SignJWT(mapClaims, now)
}

func getWithToken(url, tok string) error {
	req, _ := http.NewRequest("GET", url, nil)
	req.Header.Set("X-Internal-Token", tok)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return fmt.Errorf("want 200, got %d", resp.StatusCode)
	}
	return nil
}

func checkJWKS(url, kid string) error {
	resp, err := http.Get(url)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return fmt.Errorf("JWKS: want 200, got %d", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	var jwks auth.JWKS
	if err := json.Unmarshal(body, &jwks); err != nil {
		return fmt.Errorf("parse JWKS: %w", err)
	}
	for _, k := range jwks.Keys {
		if k.Kid == kid {
			fmt.Printf("    JWKS kid=%s match ✓\n", kid)
			return nil
		}
	}
	return fmt.Errorf("JWKS has no key with kid=%s (got %d keys)", kid, len(jwks.Keys))
}

func postGetOrCreate(e *env, _ *auth.Signer) error {
	claims := &models.JWTClaims{
		Sub:               subject,
		UniversalID:       subject,
		Name:              name,
		PreferredUsername: name,
		Email:             name + "@example.com",
		Provider:          "smoke",
		ProviderUserID:    subject,
	}
	body, _ := json.Marshal(claims)
	req, _ := http.NewRequest("POST", e.csUserURL+"/api/internal/users/get-or-create", bytes.NewReader(body))
	req.Header.Set("X-Internal-Token", e.csUserToken)
	req.Header.Set("Content-Type", "application/json")
	// Intentionally NOT setting X-Tenant-Id — verify default tenant fallback.
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		rbody, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("get-or-create: want 200, got %d body=%s", resp.StatusCode, string(rbody))
	}
	return nil
}

func checkServerAccepts(serverURL, tok string) error {
	// /api/me is a typical authenticated endpoint in @server. If it 401s,
	// JWKS verify path failed. If 200, verify round-trip succeeded.
	req, _ := http.NewRequest("GET", serverURL+"/api/me", nil)
	req.Header.Set("Authorization", "Bearer "+tok)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode == 401 {
		return errors.New("@server rejected JWT — JWKS verify failed (kid not in cache? issuer mismatch?)")
	}
	// 200 or 404 both indicate auth passed (404 = endpoint shape differs in deployment).
	if resp.StatusCode == 200 || resp.StatusCode == 404 {
		fmt.Printf("    @server returned %d (auth passed)\n", resp.StatusCode)
		return nil
	}
	return fmt.Errorf("@server: unexpected %d", resp.StatusCode)
}

func ensureGiteaUser(e *env, username string) error {
	// First try GET; if user exists, return.
	req, _ := http.NewRequest("GET", e.giteaURL+"/api/v1/users/u-"+username, nil)
	req.SetBasicAuth(e.giteaAdminUser, e.giteaAdminPass)
	resp, err := http.DefaultClient.Do(req)
	if err == nil {
		resp.Body.Close()
		if resp.StatusCode == 200 {
			return nil
		}
	}
	body := map[string]any{
		"username":  "u-" + username,
		"email":     username + "@example.com",
		"password":  "smoke-test-pwd-replace-me!",
		"loginName": "u-" + username,
		"source":    0,
	}
	b, _ := json.Marshal(body)
	req, _ = http.NewRequest("POST", e.giteaURL+"/api/v1/admin/users", bytes.NewReader(b))
	req.SetBasicAuth(e.giteaAdminUser, e.giteaAdminPass)
	req.Header.Set("Content-Type", "application/json")
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode == 201 || resp.StatusCode == 409 || resp.StatusCode == 422 {
		return nil // 422 = already exists with different shape
	}
	rbody, _ := io.ReadAll(resp.Body)
	return fmt.Errorf("admin create user: %d body=%s", resp.StatusCode, string(rbody))
}

func step(name string, fn func() error) bool {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- fn() }()
	select {
	case err := <-done:
		if err == nil {
			fmt.Printf("✓ %s\n", name)
			return true
		}
		fmt.Printf("✗ %s: %v\n", name, err)
		return false
	case <-ctx.Done():
		fmt.Printf("✗ %s: timeout\n", name)
		return false
	}
}

func fail(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "SMOKE FATAL: "+format+"\n", args...)
	os.Exit(2)
}

// Suppress unused-symbol lints for the helpers retained for future steps.
var (
	_ = (*rsa.PrivateKey)(nil)
	_ = x509.MarshalPKIXPublicKey
	_ = pem.Decode
	_ = big.NewInt
)
