package database

import (
	"fmt"
	"log"
	"os"
	"runtime"
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

	db, err := gorm.Open(postgres.Open(databaseURL), &gorm.Config{
		Logger:         gormLogger.LogMode(gormlogger.Silent),
		TranslateError: true,
	})
	if err != nil {
		return nil, fmt.Errorf("failed to connect to database: %w", err)
	}

	log.Println("Database connected successfully")

	if err := configureConnectionPool(db); err != nil {
		log.Printf("Warning: Failed to configure connection pool: %v (continuing with defaults)", err)
	}

	registerDeviceUpdateTrace(db)

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

// registerDeviceUpdateTrace 注册排查用 GORM callback：拦截所有 devices 表的 UPDATE，
// 打印最终 SQL（含 SET 子句和占位符）+ 业务调用栈。用于定位任何绕过 device_service
// 已知方法的 device 字段修改（如 Save / struct Updates / 未知路径 / 后台任务）。
// 排查专用，定位后可移除。
func registerDeviceUpdateTrace(db *gorm.DB) {
	err := db.Callback().Update().After("gorm:update").Register("costrict_trace_devices_update", func(tx *gorm.DB) {
		table := ""
		if tx.Statement != nil && tx.Statement.Schema != nil {
			table = tx.Statement.Schema.Table
		}
		// 无条件打一条：确认 callback 是否被触发 + 实际表名（排查注册是否生效）。
		logger.Warn("[DEVICE-SQL-TRACE] UPDATE callback fired: table=%s", table)
		if table != "devices" {
			return
		}
		logger.Warn("[DEVICE-SQL-TRACE] devices UPDATE  sql=%q  vars=%v\n%s",
			tx.Statement.SQL.String(), tx.Statement.Vars, deviceUpdateTraceStack())
	})
	if err != nil {
		logger.Warn("[DEVICE-SQL-TRACE] callback register FAILED: %v", err)
	} else {
		logger.Info("[DEVICE-SQL-TRACE] callback registered OK on update:after(gorm:update)")
	}
}

// deviceUpdateTraceStack 返回过滤掉 gorm / runtime 之后的调用栈，定位 UPDATE 来源代码。
func deviceUpdateTraceStack() string {
	pcs := make([]uintptr, 32)
	n := runtime.Callers(3, pcs)
	if n == 0 {
		return ""
	}
	frames := runtime.CallersFrames(pcs[:n])
	var b strings.Builder
	for {
		f, more := frames.Next()
		if f.File != "" && !strings.Contains(f.File, "gorm.io/") && !strings.Contains(f.File, "/runtime/") {
			fmt.Fprintf(&b, "    %s:%d  %s\n", f.File, f.Line, f.Function)
		}
		if !more {
			break
		}
	}
	return b.String()
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
