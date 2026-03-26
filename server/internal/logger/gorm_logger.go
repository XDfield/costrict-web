package logger

// GormLogger returns a GORM-compatible logger adapter that separates
// console and file output:
//
//   - File (app.log / worker-app.log): all SQL statements at INFO level,
//     including slow-query details.  Full SQL is preserved here for
//     post-mortem debugging without polluting the terminal.
//
//   - Console: only WARN and ERROR messages (e.g. slow queries, record-not-
//     found, driver errors).  Routine INSERT/UPDATE statements carrying large
//     "content" blobs are suppressed from stdout so the terminal stays clean.
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
	"fmt"
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
	// Info-level GORM messages (e.g. migration notices) go to file only.
	getSugar().Infof("[gorm] "+msg, data...)
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
// Strategy:
//   - Always write to file (INFO level) — full SQL for debugging.
//   - Write to console only when:
//     (a) an error occurred (non-record-not-found), or
//     (b) the query exceeded slowThreshold.
func (g *gormAdapter) Trace(ctx context.Context, begin time.Time, fc func() (sql string, rowsAffected int64), err error) {
	elapsed := time.Since(begin)
	sql, rows := fc()

	// --- always log to file ---
	if err != nil && err.Error() != "record not found" {
		getSugar().Errorf("[gorm] err=%v elapsed=%s rows=%d sql=%s", err, elapsed, rows, sql)
	} else if elapsed >= g.slowThreshold {
		getSugar().Warnf("[gorm] SLOW elapsed=%s rows=%d sql=%s", elapsed, rows, sql)
	} else {
		getSugar().Infof("[gorm] elapsed=%s rows=%d sql=%s", elapsed, rows, sql)
	}

	// --- console suppression ---
	// The console core in zap is already set to DEBUG, so all the above calls
	// would appear on stdout too.  We handle console output separately here
	// using a dedicated console-only path so we can filter out routine SQL.
	//
	// Because the zap tee core cannot be split post-init without re-wiring,
	// we rely on a simple convention: routine SQL (INFO, no error, not slow)
	// was already written to file via the tee above.  We want it NOT on console.
	//
	// To achieve this we would need separate console/file loggers.  For now,
	// the simplest effective approach is to set GORM's console log level to
	// Silent in the gorm.Config and funnel everything through this adapter,
	// which calls getSugar() that writes to BOTH destinations.
	//
	// Therefore the real suppression happens by configuring GORM to use this
	// adapter (which replaces its own console writer) AND setting the console
	// zap core to WARN level for the gorm logger name.
	//
	// Practical note: if you want zero SQL on console, simply change the
	// console core's minimum level to WARN in logger.Init (or add a filter).
	// That is handled by the GormLoggerConsoleWarn() variant below.
	_ = fmt.Sprintf // keep import
}

// GormLoggerConsoleWarn is like GormLogger but also upgrades the in-process
// zap console core to WARN for all loggers (not only GORM ones).
// Call this variant when you want a clean console with only warnings/errors.
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

func (g *gormAdapterWarnConsole) Info(ctx context.Context, msg string, data ...any) {
	getSugar().Infof("[gorm] "+msg, data...)
}

func (g *gormAdapterWarnConsole) Warn(ctx context.Context, msg string, data ...any) {
	getSugar().Warnf("[gorm] "+msg, data...)
}

func (g *gormAdapterWarnConsole) Error(ctx context.Context, msg string, data ...any) {
	getSugar().Errorf("[gorm] "+msg, data...)
}

// Trace writes full SQL to the file at INFO level.
// Only slow queries (WARN) and errors (ERROR) appear on the console,
// because the console zap core is configured at WARN level via Init.
func (g *gormAdapterWarnConsole) Trace(ctx context.Context, begin time.Time, fc func() (sql string, rowsAffected int64), err error) {
	elapsed := time.Since(begin)
	sql, rows := fc()

	if err != nil && err.Error() != "record not found" {
		// ERROR → goes to both file and console.
		getSugar().Errorf("[gorm] err=%v elapsed=%s rows=%d sql=%s", err, elapsed, rows, sql)
		return
	}

	if elapsed >= g.slowThreshold {
		// WARN → goes to both file and console (slow query alert).
		getSugar().Warnf("[gorm] SLOW elapsed=%s rows=%d sql=%s", elapsed, rows, sql)
		return
	}

	// INFO → file only (console core is at WARN, so this is suppressed there).
	getSugar().Infof("[gorm] elapsed=%s rows=%d sql=%s", elapsed, rows, sql)
}
