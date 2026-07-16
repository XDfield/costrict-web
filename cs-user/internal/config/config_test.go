package config

import (
	"os"
	"strings"
	"testing"
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
