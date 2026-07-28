// cs-user-migrate is the standalone migration binary for cs-user's independent
// PostgreSQL database (ADR D1, ADR D7). It acquires a PostgreSQL advisory
// lock before applying migrations so multiple replicas cannot race.
//
// Usage:
//
//	cs-user-migrate              # apply pending migrations
//	cs-user-migrate up           # same as bare invocation
//	cs-user-migrate version      # print current schema version
//	cs-user-migrate help
//
// Env: same CS_USER_POSTGRES_* vars as the API binary (see README).
package main

import (
	"context"
	"database/sql"
	"fmt"
	"log"
	"os"
	"time"

	"github.com/costrict/costrict-web/cs-user/internal/config"
	"github.com/costrict/costrict-web/cs-user/internal/storage"
	"github.com/costrict/costrict-web/cs-user/migrations"
	"github.com/joho/godotenv"
	"github.com/pressly/goose/v3"
)

// advisoryLockKeys intentionally differ from server's (12345/67890) so the two
// services can run migrations on neighbouring DBs without false-positive lock
// contention if they ever share a host.
const (
	csUserLockKey1 = 24680
	csUserLockKey2 = 13579
)

func main() {
	// Dev convenience: load .env from CWD if present. In production the
	// container runtime injects env vars directly and .env is absent —
	// godotenv.Load returns nil for missing files and is a safe no-op.
	// Existing process env wins (Load does not override set vars).
	if err := godotenv.Load(); err != nil && !os.IsNotExist(err) {
		log.Printf("load .env: %v (continuing with process env)", err)
	}

	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "help", "-h", "--help":
			printHelp()
			return
		}
	}

	cfg, err := config.Load()
	if err != nil {
		log.Fatalf("load config: %v", err)
	}

	pool, err := storage.Open(cfg)
	if err != nil {
		log.Fatalf("open postgres: %v", err)
	}
	defer func() {
		if cerr := pool.Close(); cerr != nil {
			log.Printf("close pool: %v", cerr)
		}
	}()

	sqlDB, err := pool.SQLDB()
	if err != nil {
		log.Fatalf("acquire *sql.DB: %v", err)
	}

	// Block until we hold the advisory lock — prevents two migrate jobs from
	// racing during a rolling deploy. The lock auto-releases on process exit.
	if err := acquireAdvisoryLock(sqlDB); err != nil {
		log.Fatalf("acquire advisory lock: %v", err)
	}
	defer releaseAdvisoryLock(sqlDB)

	goose.SetBaseFS(migrations.FS)
	if err := goose.SetDialect("postgres"); err != nil {
		log.Fatalf("set dialect: %v", err)
	}

	cmd := "up"
	if len(os.Args) > 1 {
		cmd = os.Args[1]
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	switch cmd {
	case "up", "":
		if err := goose.UpContext(ctx, sqlDB, ".", goose.WithAllowMissing()); err != nil {
			log.Fatalf("migrate up: %v", err)
		}
		log.Println("migrations applied successfully")
	case "version":
		v, err := goose.GetDBVersion(sqlDB)
		if err != nil {
			log.Fatalf("get version: %v", err)
		}
		fmt.Printf("current schema version: %d\n", v)
	default:
		log.Fatalf("unknown command %q — run 'cs-user-migrate help'", cmd)
	}
}

// acquireAdvisoryLock wraps pg_advisory_lock(int4,int4). Blocks until the lock
// is acquired or the query fails.
func acquireAdvisoryLock(db *sql.DB) error {
	_, err := db.Exec(
		"SELECT pg_advisory_lock($1, $2)", csUserLockKey1, csUserLockKey2,
	)
	return err
}

// releaseAdvisoryLock best-effort releases the lock at shutdown. A failure
// here only matters if the process is about to exit anyway, so we log without
// crashing.
func releaseAdvisoryLock(db *sql.DB) {
	var ok bool
	if err := db.QueryRow(
		"SELECT pg_advisory_unlock($1, $2)", csUserLockKey1, csUserLockKey2,
	).Scan(&ok); err != nil {
		log.Printf("release advisory lock: %v", err)
	}
}

func printHelp() {
	fmt.Print(`cs-user-migrate — schema migration tool for cs-user

Usage:
  cs-user-migrate [command]

Commands:
  up        Apply all pending migrations (default)
  version   Print the current schema version
  help      Show this help

Env:
  CS_USER_POSTGRES_HOST          default: localhost
  CS_USER_POSTGRES_PORT          default: 5432
  CS_USER_POSTGRES_DATABASE      default: cs_user
  CS_USER_POSTGRES_USER          (required)
  CS_USER_POSTGRES_PASSWORD      (required)
  CS_USER_POSTGRES_SSLMODE       default: disable
  CS_USER_INTERNAL_TOKEN         (required — shared secret)
`)
}
