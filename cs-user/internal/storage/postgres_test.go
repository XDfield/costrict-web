package storage

import (
	"strings"
	"testing"
	"time"

	"github.com/costrict/costrict-web/cs-user/internal/config"
)

// fakePoolConf records what configurePool applied — verifies env→method
// plumbing without spinning up a real driver.
type fakePoolConf struct {
	open     int
	idle     int
	lifetime time.Duration
}

func (f *fakePoolConf) SetMaxOpenConns(n int)              { f.open = n }
func (f *fakePoolConf) SetMaxIdleConns(n int)              { f.idle = n }
func (f *fakePoolConf) SetConnMaxLifetime(d time.Duration) { f.lifetime = d }

func TestEnvInt_Defaults(t *testing.T) {
	t.Setenv("DB_MAX_OPEN_CONNS", "")
	t.Setenv("DB_MAX_IDLE_CONNS", "")
	t.Setenv("DB_CONN_MAX_LIFETIME_MINUTES", "")

	for _, c := range []struct {
		key string
		def int
	}{
		{"DB_MAX_OPEN_CONNS", defaultMaxOpenConns},
		{"DB_MAX_IDLE_CONNS", defaultMaxIdleConns},
		{"DB_CONN_MAX_LIFETIME_MINUTES", defaultConnMaxLifetimeMins},
	} {
		got, err := envInt(c.key, c.def)
		if err != nil {
			t.Errorf("%s: unexpected err %v", c.key, err)
		}
		if got != c.def {
			t.Errorf("%s default: got %d want %d", c.key, got, c.def)
		}
	}
}

func TestEnvInt_Valid(t *testing.T) {
	t.Setenv("DB_MAX_OPEN_CONNS", "42")
	got, err := envInt("DB_MAX_OPEN_CONNS", 1)
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if got != 42 {
		t.Errorf("got %d want 42", got)
	}
}

func TestEnvInt_RejectsGarbage(t *testing.T) {
	t.Setenv("DB_MAX_OPEN_CONNS", "not-a-number")
	if _, err := envInt("DB_MAX_OPEN_CONNS", 1); err == nil {
		t.Error("expected error for non-integer value")
	}
}

func TestEnvInt_RejectsNegative(t *testing.T) {
	t.Setenv("DB_MAX_OPEN_CONNS", "-5")
	if _, err := envInt("DB_MAX_OPEN_CONNS", 1); err == nil {
		t.Error("expected error for negative value")
	}
}

func TestConfigurePool_AppliesDefaultsWhenEnvUnset(t *testing.T) {
	t.Setenv("DB_MAX_OPEN_CONNS", "")
	t.Setenv("DB_MAX_IDLE_CONNS", "")
	t.Setenv("DB_CONN_MAX_LIFETIME_MINUTES", "")

	var f fakePoolConf
	if err := configurePool(&f); err != nil {
		t.Fatalf("configurePool: %v", err)
	}
	if f.open != defaultMaxOpenConns {
		t.Errorf("max open: got %d want %d", f.open, defaultMaxOpenConns)
	}
	if f.idle != defaultMaxIdleConns {
		t.Errorf("max idle: got %d want %d", f.idle, defaultMaxIdleConns)
	}
	wantLifetime := time.Duration(defaultConnMaxLifetimeMins) * time.Minute
	if f.lifetime != wantLifetime {
		t.Errorf("max lifetime: got %v want %v", f.lifetime, wantLifetime)
	}
}

func TestConfigurePool_AppliesEnvOverrides(t *testing.T) {
	t.Setenv("DB_MAX_OPEN_CONNS", "13")
	t.Setenv("DB_MAX_IDLE_CONNS", "2")
	t.Setenv("DB_CONN_MAX_LIFETIME_MINUTES", "15")

	var f fakePoolConf
	if err := configurePool(&f); err != nil {
		t.Fatalf("configurePool: %v", err)
	}
	if f.open != 13 {
		t.Errorf("max open: got %d want 13", f.open)
	}
	if f.idle != 2 {
		t.Errorf("max idle: got %d want 2", f.idle)
	}
	if f.lifetime != 15*time.Minute {
		t.Errorf("max lifetime: got %v want 15m", f.lifetime)
	}
}

func TestConfigurePool_InvalidEnvErrors(t *testing.T) {
	t.Setenv("DB_MAX_OPEN_CONNS", "garbage")
	var f fakePoolConf
	if err := configurePool(&f); err == nil {
		t.Error("expected error for invalid env var")
	}
}

func TestOpen_NilConfigErrors(t *testing.T) {
	if _, err := Open(nil); err == nil {
		t.Error("Open(nil) should error")
	}
}

func TestPool_Close_NilSafe(t *testing.T) {
	// Close on a nil/uninitialised Pool must be a no-op — main.go's deferred
	// cleanup runs it unconditionally, even on early-fail paths.
	var p *Pool
	if err := p.Close(); err != nil {
		t.Errorf("nil Pool.Close: got %v want nil", err)
	}

	p = &Pool{}
	if err := p.Close(); err != nil {
		t.Errorf("empty Pool.Close: got %v want nil", err)
	}
}

// TestDSNFormat guards against silent breakage of the connection-string shape
// consumed by gorm's postgres driver — a missing field would yield a
// confusing "failed to connect" error at boot.
func TestDSNFormat(t *testing.T) {
	cfg := &config.Config{
		Postgres: config.PostgresConfig{
			Host: "h", Port: "5433", User: "u", Password: "p",
			Database: "d", SSLMode: "require",
		},
	}
	got := cfg.Postgres.DSN()
	for _, want := range []string{"host=h", "port=5433", "user=u", "password=p", "dbname=d", "sslmode=require", "TimeZone=UTC"} {
		if !strings.Contains(got, want) {
			t.Errorf("DSN missing %q; got %q", want, got)
		}
	}
}
