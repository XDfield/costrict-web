package config

import (
	"os"
	"strings"
	"testing"
	"time"
)

// clearEnv unsets every CS_USER_* var Load() reads, so each test starts from
// a deterministic baseline regardless of the developer's shell environment.
// t.Setenv captures the prior value for restoration; we precede it with
// os.Unsetenv because t.Setenv("", "") sets to empty rather than unsetting,
// and Load() must distinguish "unset" from "empty" for required fields.
func clearEnv(t *testing.T) {
	t.Helper()
	for _, key := range []string{
		"CS_USER_HTTP_PORT",
		"CS_USER_HTTP_MODE",
		"CS_USER_POSTGRES_HOST",
		"CS_USER_POSTGRES_PORT",
		"CS_USER_POSTGRES_DATABASE",
		"CS_USER_POSTGRES_USER",
		"CS_USER_POSTGRES_PASSWORD",
		"CS_USER_POSTGRES_SSLMODE",
		"CS_USER_INTERNAL_TOKEN",
		"CS_USER_CONFIG_FILE",
		"CS_USER_JWT_SIGNING_KEY_PATH",
		"CS_USER_JWT_ISSUER",
		"CS_USER_JWT_TTL",
		"CS_USER_JWT_AUDIENCE",
	} {
		prev, had := os.LookupEnv(key)
		os.Unsetenv(key)
		t.Cleanup(func() {
			if had {
				os.Setenv(key, prev)
			} else {
				os.Unsetenv(key)
			}
		})
	}
}

func TestLoad_DefaultsApplied(t *testing.T) {
	clearEnv(t)
	t.Setenv("CS_USER_INTERNAL_TOKEN", "secret")
	t.Setenv("CS_USER_POSTGRES_USER", "u")
	t.Setenv("CS_USER_POSTGRES_PASSWORD", "p")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: unexpected error: %v", err)
	}
	if cfg.HTTP.Port != "8081" {
		t.Errorf("HTTP.Port default = %q, want 8081", cfg.HTTP.Port)
	}
	if cfg.HTTP.Mode != "debug" {
		t.Errorf("HTTP.Mode default = %q, want debug", cfg.HTTP.Mode)
	}
	if cfg.Postgres.Database != "cs_user" {
		t.Errorf("Postgres.Database default = %q, want cs_user", cfg.Postgres.Database)
	}
	if cfg.Postgres.SSLMode != "disable" {
		t.Errorf("Postgres.SSLMode default = %q, want disable", cfg.Postgres.SSLMode)
	}
	if cfg.Postgres.Host != "localhost" {
		t.Errorf("Postgres.Host default = %q, want localhost", cfg.Postgres.Host)
	}
	if cfg.Postgres.Port != "5432" {
		t.Errorf("Postgres.Port default = %q, want 5432", cfg.Postgres.Port)
	}
}

func TestLoad_CustomEnvValues(t *testing.T) {
	clearEnv(t)
	t.Setenv("CS_USER_HTTP_PORT", "9090")
	t.Setenv("CS_USER_HTTP_MODE", "release")
	t.Setenv("CS_USER_POSTGRES_HOST", "db.example.com")
	t.Setenv("CS_USER_POSTGRES_PORT", "6543")
	t.Setenv("CS_USER_POSTGRES_DATABASE", "prod_db")
	t.Setenv("CS_USER_POSTGRES_USER", "produser")
	t.Setenv("CS_USER_POSTGRES_PASSWORD", "prodpass")
	t.Setenv("CS_USER_POSTGRES_SSLMODE", "require")
	t.Setenv("CS_USER_INTERNAL_TOKEN", "tok-123")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: unexpected error: %v", err)
	}
	if cfg.HTTP.Port != "9090" {
		t.Errorf("HTTP.Port = %q, want 9090", cfg.HTTP.Port)
	}
	if cfg.HTTP.Mode != "release" {
		t.Errorf("HTTP.Mode = %q, want release", cfg.HTTP.Mode)
	}
	if cfg.Postgres.Host != "db.example.com" {
		t.Errorf("Postgres.Host = %q, want db.example.com", cfg.Postgres.Host)
	}
	if cfg.Postgres.Port != "6543" {
		t.Errorf("Postgres.Port = %q, want 6543", cfg.Postgres.Port)
	}
	if cfg.Postgres.Database != "prod_db" {
		t.Errorf("Postgres.Database = %q, want prod_db", cfg.Postgres.Database)
	}
	if cfg.Postgres.User != "produser" {
		t.Errorf("Postgres.User = %q, want produser", cfg.Postgres.User)
	}
	if cfg.Postgres.Password != "prodpass" {
		t.Errorf("Postgres.Password = %q, want prodpass", cfg.Postgres.Password)
	}
	if cfg.Postgres.SSLMode != "require" {
		t.Errorf("Postgres.SSLMode = %q, want require", cfg.Postgres.SSLMode)
	}
	if cfg.Internal.Token != "tok-123" {
		t.Errorf("Internal.Token = %q, want tok-123", cfg.Internal.Token)
	}
}

func TestLoad_MissingInternalToken(t *testing.T) {
	clearEnv(t)
	t.Setenv("CS_USER_POSTGRES_USER", "u")
	t.Setenv("CS_USER_POSTGRES_PASSWORD", "p")

	_, err := Load()
	if err == nil {
		t.Fatal("Load: expected error for missing CS_USER_INTERNAL_TOKEN, got nil")
	}
	if !strings.Contains(err.Error(), "CS_USER_INTERNAL_TOKEN") {
		t.Errorf("Load error = %q, want substring CS_USER_INTERNAL_TOKEN", err.Error())
	}
}

func TestLoad_MissingPostgresCredentials(t *testing.T) {
	clearEnv(t)
	t.Setenv("CS_USER_INTERNAL_TOKEN", "secret")
	// postgres user/password intentionally unset

	_, err := Load()
	if err == nil {
		t.Fatal("Load: expected error for missing postgres creds, got nil")
	}
	if !strings.Contains(err.Error(), "CS_USER_POSTGRES_USER") {
		t.Errorf("Load error = %q, want substring CS_USER_POSTGRES_USER", err.Error())
	}
}

func TestPostgresDSN(t *testing.T) {
	p := PostgresConfig{
		Host: "h", Port: "1234", User: "u", Password: "pass",
		Database: "d", SSLMode: "disable",
	}
	got := p.DSN()
	for _, want := range []string{"host=h", "port=1234", "user=u", "password=pass", "dbname=d", "sslmode=disable"} {
		if !strings.Contains(got, want) {
			t.Errorf("DSN() = %q, missing %q", got, want)
		}
	}
}

// --- Phase A7: JWT config ---

// TestLoad_JWTDefaults verifies the safe defaults applied when no JWT env
// vars are set. A fresh deployment shouldn't need extra config to issue
// tokens; operators override only when relying parties enforce specific
// iss / aud values.
func TestLoad_JWTDefaults(t *testing.T) {
	clearEnv(t)
	t.Setenv("CS_USER_INTERNAL_TOKEN", "secret")
	t.Setenv("CS_USER_POSTGRES_USER", "u")
	t.Setenv("CS_USER_POSTGRES_PASSWORD", "p")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.JWT.Issuer != defaultJWTIssuer {
		t.Errorf("JWT.Issuer default = %q, want %q", cfg.JWT.Issuer, defaultJWTIssuer)
	}
	if cfg.JWT.TTL != defaultJWTTTL {
		t.Errorf("JWT.TTL default = %v, want %v", cfg.JWT.TTL, defaultJWTTTL)
	}
	if len(cfg.JWT.DefaultAudience) != 0 {
		t.Errorf("JWT.DefaultAudience default = %v, want empty", cfg.JWT.DefaultAudience)
	}
	if cfg.JWT.SigningKeyPath != "" {
		t.Errorf("JWT.SigningKeyPath default = %q, want empty", cfg.JWT.SigningKeyPath)
	}
}

// TestLoad_JWTIssuerOverride verifies the env override applies cleanly.
func TestLoad_JWTIssuerOverride(t *testing.T) {
	clearEnv(t)
	t.Setenv("CS_USER_INTERNAL_TOKEN", "secret")
	t.Setenv("CS_USER_POSTGRES_USER", "u")
	t.Setenv("CS_USER_POSTGRES_PASSWORD", "p")
	t.Setenv("CS_USER_JWT_ISSUER", "https://cs-user.example.com")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.JWT.Issuer != "https://cs-user.example.com" {
		t.Errorf("JWT.Issuer = %q, want https://cs-user.example.com", cfg.JWT.Issuer)
	}
}

// TestLoad_JWTTTLParsing verifies Go-duration-string parsing. We accept the
// full time.ParseDuration vocabulary so operators can write "1h", "30m",
// "3600s" or even "1h30m" — matching the conventions used by other Go
// services.
func TestLoad_JWTTTLParsing(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		want time.Duration
	}{
		{"hours", "2h", 2 * time.Hour},
		{"minutes", "45m", 45 * time.Minute},
		{"seconds", "900s", 900 * time.Second},
		{"composite", "1h30m", 90 * time.Minute},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			clearEnv(t)
			t.Setenv("CS_USER_INTERNAL_TOKEN", "secret")
			t.Setenv("CS_USER_POSTGRES_USER", "u")
			t.Setenv("CS_USER_POSTGRES_PASSWORD", "p")
			t.Setenv("CS_USER_JWT_TTL", tc.raw)

			cfg, err := Load()
			if err != nil {
				t.Fatalf("Load: %v", err)
			}
			if cfg.JWT.TTL != tc.want {
				t.Errorf("JWT.TTL = %v, want %v", cfg.JWT.TTL, tc.want)
			}
		})
	}
}

// TestLoad_JWTTTLInvalid verifies a malformed TTL surfaces as a descriptive
// error rather than silently falling back. Operators notice config bugs at
// boot instead of after a token is mis-issued.
func TestLoad_JWTTTLInvalid(t *testing.T) {
	clearEnv(t)
	t.Setenv("CS_USER_INTERNAL_TOKEN", "secret")
	t.Setenv("CS_USER_POSTGRES_USER", "u")
	t.Setenv("CS_USER_POSTGRES_PASSWORD", "p")
	t.Setenv("CS_USER_JWT_TTL", "not-a-duration")

	_, err := Load()
	if err == nil {
		t.Fatal("expected error for invalid TTL, got nil")
	}
	if !strings.Contains(err.Error(), "CS_USER_JWT_TTL") {
		t.Errorf("error should mention CS_USER_JWT_TTL, got: %v", err)
	}
}

// TestLoad_JWTTTLZeroRejects verifies a zero / negative TTL surfaces as an
// error. The signer wouldn't issue forever-tokens; we surface the contract
// breach at config load rather than at sign time.
func TestLoad_JWTTTLZeroRejects(t *testing.T) {
	clearEnv(t)
	t.Setenv("CS_USER_INTERNAL_TOKEN", "secret")
	t.Setenv("CS_USER_POSTGRES_USER", "u")
	t.Setenv("CS_USER_POSTGRES_PASSWORD", "p")
	t.Setenv("CS_USER_JWT_TTL", "0s")

	_, err := Load()
	if err == nil {
		t.Fatal("expected error for zero TTL, got nil")
	}
	if !strings.Contains(err.Error(), "positive") {
		t.Errorf("error should mention positivity constraint, got: %v", err)
	}
}

// TestLoad_JWTAudienceCSV verifies the comma-separated parsing. Whitespace
// around entries is trimmed so the config can be human-readable without
// trailing-space bugs.
func TestLoad_JWTAudienceCSV(t *testing.T) {
	clearEnv(t)
	t.Setenv("CS_USER_INTERNAL_TOKEN", "secret")
	t.Setenv("CS_USER_POSTGRES_USER", "u")
	t.Setenv("CS_USER_POSTGRES_PASSWORD", "p")
	t.Setenv("CS_USER_JWT_AUDIENCE", "costrict-web, csc,  portal")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	want := []string{"costrict-web", "csc", "portal"}
	if len(cfg.JWT.DefaultAudience) != len(want) {
		t.Fatalf("audience len = %d, want %d (%v)", len(cfg.JWT.DefaultAudience), len(want), cfg.JWT.DefaultAudience)
	}
	for i, w := range want {
		if cfg.JWT.DefaultAudience[i] != w {
			t.Errorf("audience[%d] = %q, want %q", i, cfg.JWT.DefaultAudience[i], w)
		}
	}
}

// TestLoad_JWTAudienceEmptyOmitted verifies that a whitespace-only audience
// env var doesn't inject empty-string entries — aud="" is technically a
// valid claim but practically always a config mistake.
func TestLoad_JWTAudienceEmptyOmitted(t *testing.T) {
	clearEnv(t)
	t.Setenv("CS_USER_INTERNAL_TOKEN", "secret")
	t.Setenv("CS_USER_POSTGRES_USER", "u")
	t.Setenv("CS_USER_POSTGRES_PASSWORD", "p")
	t.Setenv("CS_USER_JWT_AUDIENCE", "  ,  , ")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(cfg.JWT.DefaultAudience) != 0 {
		t.Errorf("expected empty audience, got %v", cfg.JWT.DefaultAudience)
	}
}
