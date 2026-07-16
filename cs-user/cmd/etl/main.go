// Command cs-user-etl is a one-shot tool that copies users + user_auth_identities
// from costrict-web's PostgreSQL into cs-user's independent PostgreSQL.
//
// Design and usage:
//
//	--source-dsn          costrict-web DB DSN (read-only role recommended)
//	--target-dsn          cs-user DB DSN (write role)
//	--batch-size          Rows per fetch/commit cycle (default 500)
//	--dry-run             Report what would change; write nothing to target
//	--max-diff-records    Cap field-level diff records in report (default 100, -1 = unlimited)
//	--skip-users          Skip the users table
//	--skip-auth-identities  Skip user_auth_identities table
//	--sqlite              Dev-only: treat DSNs as sqlite file paths
//	--report FILE         Path to write JSON report (default: log summary only)
//
// Idempotent by design: a re-run with no source changes produces zero writes.
// All writes per batch run in one transaction; mid-batch failure leaves
// target consistent. Run cs-user-migrate first so the target schema is
// current — this command does NOT auto-migrate.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net/url"
	"os"
	"os/signal"
	"regexp"
	"syscall"
	"time"

	"github.com/costrict/costrict-web/cs-user/internal/etl"
	"github.com/costrict/costrict-web/cs-user/internal/models"
	"github.com/costrict/costrict-web/cs-user/internal/storage"
	"gorm.io/driver/postgres"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	gormlogger "gorm.io/gorm/logger"
)

const (
	defaultBatchSize       = 500
	defaultMaxDiffRecords  = 100
	defaultConnMaxIdle     = 5
	defaultConnMaxOpen     = 25
	defaultConnMaxLifetime = 60 // minutes
)

type flags struct {
	sourceDSN      string
	targetDSN      string
	batchSize      int
	dryRun         bool
	maxDiffRecords int
	skipUsers      bool
	skipAuthIDs    bool
	useSqlite      bool
	reportFile     string
}

func parseFlags() flags {
	var f flags
	flag.StringVar(&f.sourceDSN, "source-dsn", os.Getenv("ETL_SOURCE_DSN"), "costrict-web source PostgreSQL DSN (or set ETL_SOURCE_DSN)")
	flag.StringVar(&f.targetDSN, "target-dsn", os.Getenv("ETL_TARGET_DSN"), "cs-user target PostgreSQL DSN (or set ETL_TARGET_DSN)")
	flag.IntVar(&f.batchSize, "batch-size", defaultBatchSize, "rows per fetch + commit cycle")
	flag.BoolVar(&f.dryRun, "dry-run", false, "report what would change; write nothing to target")
	flag.IntVar(&f.maxDiffRecords, "max-diff-records", defaultMaxDiffRecords, "cap on field-level diff records in report (-1 = unlimited)")
	flag.BoolVar(&f.skipUsers, "skip-users", false, "skip the users table")
	flag.BoolVar(&f.skipAuthIDs, "skip-auth-identities", false, "skip the user_auth_identities table")
	flag.BoolVar(&f.useSqlite, "sqlite", false, "dev-only: treat DSNs as sqlite file paths")
	flag.StringVar(&f.reportFile, "report", "", "path to write JSON report (default: log summary only)")
	flag.Parse()
	return f
}

func main() {
	f := parseFlags()
	if f.batchSize <= 0 {
		log.Fatalf("--batch-size must be > 0, got %d", f.batchSize)
	}
	if f.sourceDSN == "" || f.targetDSN == "" {
		log.Fatal("--source-dsn and --target-dsn are required (or set ETL_SOURCE_DSN / ETL_TARGET_DSN)")
	}
	if f.sourceDSN == f.targetDSN {
		log.Fatal("--source-dsn and --target-dsn must differ (refusing to copy a DB onto itself)")
	}

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	source, err := openDB(f.useSqlite, f.sourceDSN, "source")
	if err != nil {
		log.Fatalf("open source: %v", err)
	}
	defer source.Close()
	target, err := openDB(f.useSqlite, f.targetDSN, "target")
	if err != nil {
		log.Fatalf("open target: %v", err)
	}
	defer target.Close()

	log.Printf("etl: source=%s target=%s batch_size=%d dry_run=%v",
		maskDSN(f.sourceDSN), maskDSN(f.targetDSN), f.batchSize, f.dryRun)

	started := time.Now()
	report := etl.Stats{DryRun: f.dryRun}

	// Pre-flight: source-level uniqueness validation (cheap, catches a
	// duplicate casdoor_universal_id that would otherwise abort mid-batch).
	if !f.skipUsers {
		dups, err := etl.ValidateSource(ctx, source.Gorm)
		if err != nil {
			log.Printf("WARN: validate source skipped: %v", err)
		} else if len(dups) > 0 {
			log.Printf("WARN: %d duplicate casdoor_universal_id value(s) in source:", len(dups))
			for _, d := range dups {
				log.Printf("  - %q appears %d times", d.Value, d.Count)
			}
			log.Printf("continuing — duplicates will surface as INSERT errors at the offending rows")
		}
	}

	if !f.skipUsers {
		usersStats, err := runUsers(ctx, source.Gorm, target.Gorm, f)
		if err != nil {
			log.Fatalf("users pass failed: %v", err)
		}
		report.Add(usersStats)
		report.FieldDiffs = append(report.FieldDiffs, usersStats.FieldDiffs...)
	}
	if !f.skipAuthIDs {
		aiStats, err := runAuthIdentities(ctx, source.Gorm, target.Gorm, f)
		if err != nil {
			log.Fatalf("auth-identities pass failed: %v", err)
		}
		report.Add(aiStats)
		report.FieldDiffs = append(report.FieldDiffs, aiStats.FieldDiffs...)
	}

	logAndReport(report, time.Since(started), f)
}

func runUsers(ctx context.Context, source, target *gorm.DB, f flags) (etl.Stats, error) {
	var acc etl.Stats
	acc.DryRun = f.dryRun

	srcCount, err := etl.CountUsers(ctx, source)
	if err != nil {
		return acc, fmt.Errorf("count source users: %w", err)
	}
	log.Printf("users: source has %d rows", srcCount)

	batchCount := 0
	if err := etl.ExportUsers(ctx, source, f.batchSize, func(batch []*models.User) error {
		batchCount++
		log.Printf("users: batch %d (%d rows)", batchCount, len(batch))
		return etl.ImportUsers(ctx, target, batch, f.dryRun, f.maxDiffRecords, &acc)
	}); err != nil {
		return acc, err
	}

	tgtCount, err := etl.CountUsers(ctx, target)
	if err != nil {
		log.Printf("WARN: count target users: %v", err)
	} else {
		log.Printf("users: target now has %d rows (Δ=%d from source)",
			tgtCount, tgtCount-srcCount)
	}
	log.Printf("users: inserted=%d updated=%d unchanged=%d failed=%d",
		acc.Inserted, acc.Updated, acc.Unchanged, acc.Failed)
	return acc, nil
}

func runAuthIdentities(ctx context.Context, source, target *gorm.DB, f flags) (etl.Stats, error) {
	var acc etl.Stats
	acc.DryRun = f.dryRun

	srcCount, err := etl.CountAuthIdentities(ctx, source)
	if err != nil {
		return acc, fmt.Errorf("count source auth-identities: %w", err)
	}
	log.Printf("auth-identities: source has %d rows", srcCount)

	batchCount := 0
	if err := etl.ExportAuthIdentities(ctx, source, f.batchSize, func(batch []*models.UserAuthIdentity) error {
		batchCount++
		log.Printf("auth-identities: batch %d (%d rows)", batchCount, len(batch))
		return etl.ImportAuthIdentities(ctx, target, batch, f.dryRun, f.maxDiffRecords, &acc)
	}); err != nil {
		return acc, err
	}

	tgtCount, err := etl.CountAuthIdentities(ctx, target)
	if err != nil {
		log.Printf("WARN: count target auth-identities: %v", err)
	} else {
		log.Printf("auth-identities: target now has %d rows (Δ=%d from source)",
			tgtCount, tgtCount-srcCount)
	}
	log.Printf("auth-identities: inserted=%d updated=%d unchanged=%d failed=%d",
		acc.Inserted, acc.Updated, acc.Unchanged, acc.Failed)
	return acc, nil
}

// logAndReport emits a human-readable summary to stderr and, if --report is
// set, dumps the full Stats as JSON to that path.
func logAndReport(s etl.Stats, elapsed time.Duration, f flags) {
	log.Printf("done in %s: inserted=%d updated=%d unchanged=%d failed=%d (dry_run=%v)",
		elapsed.Round(time.Millisecond),
		s.Inserted, s.Updated, s.Unchanged, s.Failed, s.DryRun)
	if f.reportFile == "" {
		if s.DryRun && len(s.FieldDiffs) > 0 {
			log.Printf("dry-run sample of changed rows (capped at %d):", f.maxDiffRecords)
			for _, fd := range s.FieldDiffs {
				log.Printf("  [%s] %s: %d field(s) changed", fd.Kind, fd.Key, len(fd.Diffs))
			}
		}
		return
	}
	fp, err := os.Create(f.reportFile)
	if err != nil {
		log.Printf("WARN: open report file: %v", err)
		return
	}
	defer fp.Close()
	enc := json.NewEncoder(fp)
	enc.SetIndent("", "  ")
	if err := enc.Encode(s); err != nil {
		log.Printf("WARN: encode report: %v", err)
	}
}

// openDB opens either a postgres or sqlite gorm.DB based on useSqlite. Both
// pool sizes follow the same env-tuned knobs as the cs-user API binary so a
// single set of operational docs covers all entry points.
func openDB(useSqlite bool, dsn, label string) (*storage.Pool, error) {
	if useSqlite {
		gdb, err := gorm.Open(sqlite.Open(dsn), &gorm.Config{
			Logger: gormlogger.Default.LogMode(gormlogger.Warn),
		})
		if err != nil {
			return nil, fmt.Errorf("open sqlite %s: %w", label, err)
		}
		return &storage.Pool{Gorm: gdb}, nil
	}
	gdb, err := gorm.Open(postgres.Open(dsn), &gorm.Config{
		Logger: gormlogger.Default.LogMode(gormlogger.Warn),
	})
	if err != nil {
		return nil, fmt.Errorf("open postgres %s: %w", label, err)
	}
	sqlDB, err := gdb.DB()
	if err != nil {
		return nil, fmt.Errorf("raw db %s: %w", label, err)
	}
	sqlDB.SetMaxOpenConns(defaultConnMaxOpen)
	sqlDB.SetMaxIdleConns(defaultConnMaxIdle)
	sqlDB.SetConnMaxLifetime(time.Duration(defaultConnMaxLifetime) * time.Minute)
	return &storage.Pool{Gorm: gdb}, nil
}

// pgPasswordRe matches the password segment of a libpq URL DSN
// (postgres://user:PASSWORD@host/db) so we can mask it for logs.
var pgPasswordRe = regexp.MustCompile(`(postgres(?:ql)?://[^:@/]*:[^@/]*@)`)

// maskDSN returns the DSN with the password redacted for safe logging.
// Handles two libpq DSN formats: URL form (postgres://user:pass@host/db) and
// keyword=value form (host=localhost password=secret). When neither pattern
// matches (e.g. an already-redacted DSN), returns the input unchanged.
func maskDSN(dsn string) string {
	if dsn == "" {
		return ""
	}
	if pgPasswordRe.MatchString(dsn) {
		return pgPasswordRe.ReplaceAllStringFunc(dsn, func(m string) string {
			// strip the password by rejoining user@protocol
			at := lastIndexByte(m, '@')
			colon := lastIndexByte(m, ':')
			if at < 0 || colon < 0 || colon > at {
				return m
			}
			schemeUser := m[:colon]
			return schemeUser + ":***" + m[at:]
		})
	}
	// Keyword form: scan for password=VALUE
	if u, err := url.Parse(dsn); err == nil && u.Host != "" && u.User != nil {
		if _, has := u.User.Password(); has {
			u.User = url.UserPassword(u.User.Username(), "***")
			return u.String()
		}
	}
	return maskKeywordPassword(dsn)
}

func lastIndexByte(s string, b byte) int {
	for i := len(s) - 1; i >= 0; i-- {
		if s[i] == b {
			return i
		}
	}
	return -1
}

// maskKeywordPassword replaces the value of any "password=" (case-insensitive)
// keyword with ***. Naive but sufficient for log masking — libpq DSNs use
// spaces as separators.
var keywordPasswordRe = regexp.MustCompile(`(?i)(password\s*=\s*)[^\s]+`)

func maskKeywordPassword(dsn string) string {
	if !keywordPasswordRe.MatchString(dsn) {
		return dsn
	}
	return keywordPasswordRe.ReplaceAllString(dsn, "${1}***")
}
