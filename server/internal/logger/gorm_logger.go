package logger

// GormLogger returns a GORM-compatible logger adapter.
//
// It only emits WARN and ERROR level GORM logs, so routine SQL statements are
// suppressed from both console and log files.
//
// Usage (call after logger.Init):
//
//	db, err := gorm.Open(postgres.Open(dsn), &gorm.Config{
//	    Logger: logger.GormLogger(200 * time.Millisecond),
//	})
//
// The slowThreshold controls when a query is reported as slow on the console
// (default: 200 ms).  Pass 0 to keep the default.

import (
	"context"
	"time"

	gormlogger "gorm.io/gorm/logger"
)

// gormAdapter implements gorm/logger.Interface on top of the package-level
// zap sugar logger.
type gormAdapter struct {
	// slowThreshold: queries slower than this are printed at WARN on console.
	slowThreshold time.Duration
}

// GormLogger constructs a GORM logger adapter.
// slowThreshold: queries that exceed this duration are logged as WARN.
// Pass 0 to use the default of 200 ms.
func GormLogger(slowThreshold time.Duration) gormlogger.Interface {
	if slowThreshold == 0 {
		slowThreshold = 200 * time.Millisecond
	}
	return &gormAdapter{slowThreshold: slowThreshold}
}

func (g *gormAdapter) LogMode(level gormlogger.LogLevel) gormlogger.Interface {
	// We manage levels ourselves; ignore GORM's runtime level change.
	return g
}

func (g *gormAdapter) Info(ctx context.Context, msg string, data ...any) {
	// Suppress GORM info logs.
}

func (g *gormAdapter) Warn(ctx context.Context, msg string, data ...any) {
	// Warnings appear in both console and file via the zap pipeline.
	getSugar().Warnf("[gorm] "+msg, data...)
}

func (g *gormAdapter) Error(ctx context.Context, msg string, data ...any) {
	getSugar().Errorf("[gorm] "+msg, data...)
}

// Trace is called for every SQL statement.
//
// Only slow queries and errors are logged.
func (g *gormAdapter) Trace(ctx context.Context, begin time.Time, fc func() (sql string, rowsAffected int64), err error) {
	elapsed := time.Since(begin)
	sql, rows := fc()

	if err != nil && err.Error() != "record not found" {
		getSugar().Errorf("[gorm] err=%v elapsed=%s rows=%d sql=%s", err, elapsed, rows, sql)
	} else if elapsed >= g.slowThreshold {
		getSugar().Warnf("[gorm] SLOW elapsed=%s rows=%d sql=%s", elapsed, rows, sql)
	}
}

// GormLoggerConsoleWarn is kept for compatibility and behaves the same as
// GormLogger: only WARN and ERROR level GORM logs are emitted.
func GormLoggerConsoleWarn(slowThreshold time.Duration) gormlogger.Interface {
	if slowThreshold == 0 {
		slowThreshold = 200 * time.Millisecond
	}
	return &gormAdapterWarnConsole{slowThreshold: slowThreshold}
}

// gormAdapterWarnConsole writes all SQL to the file logger (INFO/WARN/ERROR)
// but writes to the console only for slow queries and errors, by using
// separate log calls that map to different zap levels.
type gormAdapterWarnConsole struct {
	slowThreshold time.Duration
}

func (g *gormAdapterWarnConsole) LogMode(level gormlogger.LogLevel) gormlogger.Interface {
	return g
}

func (g *gormAdapterWarnConsole) Info(ctx context.Context, msg string, data ...any) {}

func (g *gormAdapterWarnConsole) Warn(ctx context.Context, msg string, data ...any) {
	getSugar().Warnf("[gorm] "+msg, data...)
}

func (g *gormAdapterWarnConsole) Error(ctx context.Context, msg string, data ...any) {
	getSugar().Errorf("[gorm] "+msg, data...)
}

// Trace only logs slow queries (WARN) and errors (ERROR).
func (g *gormAdapterWarnConsole) Trace(ctx context.Context, begin time.Time, fc func() (sql string, rowsAffected int64), err error) {
	elapsed := time.Since(begin)
	sql, rows := fc()

	if err != nil && err.Error() != "record not found" {
		// ERROR → goes to both file and console.
		getSugar().Errorf("[gorm] err=%v elapsed=%s rows=%d sql=%s", err, elapsed, rows, sql)
		return
	}

	if elapsed >= g.slowThreshold {
		getSugar().Warnf("[gorm] SLOW elapsed=%s rows=%d sql=%s", elapsed, rows, sql)
		return
	}
}
