// Package storage owns cs-user's connection to its independent PostgreSQL
// database (ADR D1: independent DB per service). Phase 1 P0-2 ships the
// minimum: open a gorm-backed pool, expose a readiness checker for /readyz,
// and surface the underlying *sql.DB for the goose migration runner.
//
// We deliberately avoid a package-level singleton (unlike server's database.DB)
// so tests can spin up isolated pools and main.go can pass a single instance
// down through explicit wiring.
package storage

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"strconv"
	"time"

	"github.com/costrict/costrict-web/cs-user/internal/config"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	gormlogger "gorm.io/gorm/logger"
)

// Default pool sizing — overridable via env for prod tuning. Values mirror
// server/internal/database defaults; cs-user's load profile is far lighter
// (internal-only RPC), so these are conservative starting points.
const (
	defaultMaxOpenConns        = 25
	defaultMaxIdleConns        = 5
	defaultConnMaxLifetimeMins = 60
	pingTimeout                = 3 * time.Second
)

// Pool is a thin wrapper around *gorm.DB plus the underlying *sql.DB so
// callers can run raw SQL (goose migrations) without going through gorm.
type Pool struct {
	Gorm *gorm.DB
	sql  *sql.DB
}

// Open establishes a PostgreSQL connection pool using cfg.Postgres and applies
// env-tunable pool limits. Returns an error if the initial connection fails.
func Open(cfg *config.Config) (*Pool, error) {
	if cfg == nil {
		return nil, errors.New("storage.Open: nil config")
	}
	dsn := cfg.Postgres.DSN()

	db, err := gorm.Open(postgres.Open(dsn), &gorm.Config{
		Logger:         gormlogger.Default.LogMode(gormlogger.Silent),
		TranslateError: true,
	})
	if err != nil {
		return nil, fmt.Errorf("storage.Open: connect postgres: %w", err)
	}

	sqlDB, err := db.DB()
	if err != nil {
		return nil, fmt.Errorf("storage.Open: acquire *sql.DB: %w", err)
	}

	if err := configurePool(sqlDB); err != nil {
		// Pool misconfiguration is fatal at boot — better to fail loudly than
		// silently run with unexpected limits in prod.
		_ = sqlDB.Close()
		return nil, fmt.Errorf("storage.Open: configure pool: %w", err)
	}

	return &Pool{Gorm: db, sql: sqlDB}, nil
}

// Close releases the underlying pool. Safe to call multiple times.
func (p *Pool) Close() error {
	if p == nil || p.sql == nil {
		return nil
	}
	return p.sql.Close()
}

// SQLDB returns the underlying *sql.DB. Used by the migration runner.
// Callers must NOT close it — Pool.Close owns the lifecycle.
func (p *Pool) SQLDB() (*sql.DB, error) {
	if p == nil || p.sql == nil {
		return nil, errors.New("storage: pool not initialised")
	}
	return p.sql, nil
}

// Ping verifies the pool can reach the database within pingTimeout.
// Implements app.ReadyChecker — main.go wires it directly into /readyz.
func (p *Pool) Ping(ctx context.Context) error {
	if p == nil || p.sql == nil {
		return errors.New("storage: pool not initialised")
	}

	ctx, cancel := context.WithTimeout(ctx, pingTimeout)
	defer cancel()
	return p.sql.PingContext(ctx)
}

// Ready implements app.ReadyChecker. We accept the ctx-less contract so
// callers don't need to thread a context through /readyz; Ping enforces its
// own bounded timeout internally.
func (p *Pool) Ready() error {
	return p.Ping(context.Background())
}

// configurePool reads DB_MAX_OPEN_CONNS / DB_MAX_IDLE_CONNS /
// DB_CONN_MAX_LIFETIME_MINUTES from the environment and applies them. Invalid
// values fall through to the conservative defaults rather than panicking —
// operators can fix the env without restarting in error.
func configurePool(sqlDB interface {
	SetMaxOpenConns(int)
	SetMaxIdleConns(int)
	SetConnMaxLifetime(time.Duration)
}) error {
	maxOpen, err := envInt("DB_MAX_OPEN_CONNS", defaultMaxOpenConns)
	if err != nil {
		return err
	}
	maxIdle, err := envInt("DB_MAX_IDLE_CONNS", defaultMaxIdleConns)
	if err != nil {
		return err
	}
	maxLifetimeMins, err := envInt("DB_CONN_MAX_LIFETIME_MINUTES", defaultConnMaxLifetimeMins)
	if err != nil {
		return err
	}

	sqlDB.SetMaxOpenConns(maxOpen)
	sqlDB.SetMaxIdleConns(maxIdle)
	sqlDB.SetConnMaxLifetime(time.Duration(maxLifetimeMins) * time.Minute)
	return nil
}

// envInt parses an env var as int, falling back to def when unset/empty.
// Returns an error if the value is present but malformed — calling this out
// loudly beats silently swallowing a config typo.
func envInt(key string, def int) (int, error) {
	v := os.Getenv(key)
	if v == "" {
		return def, nil
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 0 {
		return 0, fmt.Errorf("invalid %s: %s (must be non-negative integer)", key, v)
	}
	return n, nil
}
