package database

import (
	"fmt"
	"log"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/costrict/costrict-web/server/internal/logger"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	gormlogger "gorm.io/gorm/logger"
)

var DB *gorm.DB

// Initialize opens a PostgreSQL connection with a custom GORM logger that:
//   - writes all SQL statements (including full content blobs) to the log file
//     for post-mortem debugging, and
//   - only prints WARN / ERROR messages to the console, keeping the terminal
//     clean.
//
// slowThreshold controls when a query is flagged as slow on the console.
// Pass 0 to use the default (200 ms).
func Initialize(databaseURL string) (*gorm.DB, error) {
	return InitializeWithOptions(databaseURL, 0)
}

// InitializeWithOptions is like Initialize but lets the caller tune the slow-
// query threshold shown on the console.
func InitializeWithOptions(databaseURL string, slowThreshold time.Duration) (*gorm.DB, error) {
	gormLogger := logger.GormLoggerConsoleWarn(slowThreshold)

	logMode := gormlogger.Warn
	if os.Getenv("GORM_SQL_LOG") == "all" {
		logMode = gormlogger.Info
	}

	db, err := gorm.Open(postgres.Open(databaseURL), &gorm.Config{
		Logger:         gormLogger.LogMode(logMode),
		TranslateError: true,
	})
	if err != nil {
		return nil, fmt.Errorf("failed to connect to database: %w", err)
	}

	log.Println("Database connected successfully")

	if err := configureConnectionPool(db); err != nil {
		log.Printf("Warning: Failed to configure connection pool: %v (continuing with defaults)", err)
	}

	DB = db
	return db, nil
}

// configureConnectionPool reads optional env vars and applies connection pool
// limits to the underlying *sql.DB.  This prevents unbounded connection growth
// under load, which was observed to exhaust PostgreSQL max_connections when
// the memory-reporting endpoint was hit concurrently.
func configureConnectionPool(db *gorm.DB) error {
	sqlDB, err := db.DB()
	if err != nil {
		return err
	}

	maxOpen := 25
	if v := os.Getenv("DB_MAX_OPEN_CONNS"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 {
			return fmt.Errorf("invalid DB_MAX_OPEN_CONNS: %s", v)
		}
		maxOpen = n
	}
	sqlDB.SetMaxOpenConns(maxOpen)

	maxIdle := 5
	if v := os.Getenv("DB_MAX_IDLE_CONNS"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 {
			return fmt.Errorf("invalid DB_MAX_IDLE_CONNS: %s", v)
		}
		maxIdle = n
	}
	sqlDB.SetMaxIdleConns(maxIdle)

	maxLifetime := 60 * time.Minute
	if v := os.Getenv("DB_CONN_MAX_LIFETIME_MINUTES"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 {
			return fmt.Errorf("invalid DB_CONN_MAX_LIFETIME_MINUTES: %s", v)
		}
		maxLifetime = time.Duration(n) * time.Minute
	}
	sqlDB.SetConnMaxLifetime(maxLifetime)

	log.Printf("Database pool configured: max_open=%d max_idle=%d max_lifetime=%s", maxOpen, maxIdle, maxLifetime)
	return nil
}

func GetDB() *gorm.DB {
	return DB
}

// ILike returns the case-insensitive LIKE operator appropriate for the
// current database dialect: "ILIKE" for PostgreSQL, "LIKE" for SQLite
// (SQLite LIKE is already case-insensitive for ASCII by default).
func ILike(db *gorm.DB) string {
	if db.Dialector.Name() == "postgres" {
		return "ILIKE"
	}
	return "LIKE"
}

// SplitSearchKeywords splits a search query into individual keywords by whitespace.
func SplitSearchKeywords(query string) []string {
	parts := strings.Split(query, " ")
	keywords := make([]string, 0, len(parts))
	for _, p := range parts {
		if trimmed := strings.TrimSpace(p); trimmed != "" {
			keywords = append(keywords, trimmed)
		}
	}
	return keywords
}
