package database

import (
	"fmt"
	"log"
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

	db, err := gorm.Open(postgres.Open(databaseURL), &gorm.Config{
		Logger:         gormLogger.LogMode(gormlogger.Silent),
		TranslateError: true,
	})
	if err != nil {
		return nil, fmt.Errorf("failed to connect to database: %w", err)
	}

	log.Println("Database connected successfully")

	if err := enablePgVector(db); err != nil {
		log.Printf("Warning: Failed to enable pgvector extension: %v (continuing without vector support)", err)
	}

	DB = db
	return db, nil
}

// enablePgVector enables the pgvector extension in PostgreSQL.
func enablePgVector(db *gorm.DB) error {
	if err := db.Exec("CREATE EXTENSION IF NOT EXISTS vector").Error; err != nil {
		return fmt.Errorf("failed to create vector extension: %w", err)
	}
	log.Println("pgvector extension enabled successfully")
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
