// Package logger provides application-wide structured logging backed by
// go.uber.org/zap with automatic file rotation (lumberjack) and 7-day
// retention.
//
// Two log files are maintained under the configured directory (default: ./logs):
//   - app.log    – all messages (DEBUG, INFO, WARN, ERROR, ...)
//   - error.log  – ERROR and above only
//
// The package exposes printf-style convenience functions (Info, Warn, Error, ...)
// so callers do not need to manage logger instances directly.
package logger

import (
	"fmt"
	"io"
	"log"
	"os"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"gopkg.in/natefinch/lumberjack.v2"
)

var (
	// zapLogger is the underlying zap logger.
	zapLogger *zap.Logger
	// sugar is the sugared (printf-style) logger used by public helpers.
	sugar *zap.SugaredLogger
	// appWriter is exposed for GinWriter().
	appWriter io.Writer
	// errWriter is exposed for GinErrorWriter().
	errWriter io.Writer
)

// Config controls log output behaviour.
type Config struct {
	Dir          string // directory for log files, default "./logs"
	FilePrefix   string // prefix for log file names, e.g. "worker" => worker-app.log; empty => app.log
	MaxSizeMB    int    // max size in MB before rotation, default 100
	MaxAgeDays   int    // max days to keep old files, default 7
	MaxBackups   int    // max number of old files to keep, default 10
	Console      bool   // also write to stdout/stderr, default true
	ConsoleLevel string // minimum log level for console output: "debug"(default)|"info"|"warn"|"error"
}

// Init initialises the global loggers. It MUST be called once early in main().
// After Init the standard library log.Printf / log.Println also write to
// app.log (the default logger's output is redirected).
func Init(cfg Config) {
	// LOG_DIR environment variable takes highest priority, allowing
	// deployment to specify an absolute path (e.g. /var/log/costrict-server).
	if envDir := os.Getenv("LOG_DIR"); envDir != "" {
		cfg.Dir = envDir
	}
	if cfg.Dir == "" {
		cfg.Dir = "./logs"
	}
	if cfg.MaxSizeMB == 0 {
		cfg.MaxSizeMB = 100
	}
	if cfg.MaxAgeDays == 0 {
		cfg.MaxAgeDays = 7
	}
	if cfg.MaxBackups == 0 {
		cfg.MaxBackups = 10
	}

	// Ensure logs directory exists.
	if err := os.MkdirAll(cfg.Dir, 0o755); err != nil {
		log.Fatalf("logger: failed to create log dir %s: %v", cfg.Dir, err)
	}

	// Build file names: "app.log" / "error.log" or "worker-app.log" / "worker-error.log".
	appFilename := "app.log"
	errFilename := "error.log"
	if cfg.FilePrefix != "" {
		appFilename = cfg.FilePrefix + "-app.log"
		errFilename = cfg.FilePrefix + "-error.log"
	}

	// --- encoder configs ---
	fileEncoderCfg := zapcore.EncoderConfig{
		TimeKey:        "ts",
		LevelKey:       "level",
		NameKey:        "logger",
		CallerKey:      "caller",
		MessageKey:     "msg",
		StacktraceKey:  "stacktrace",
		LineEnding:     zapcore.DefaultLineEnding,
		EncodeLevel:    zapcore.CapitalLevelEncoder,
		EncodeTime:     zapcore.ISO8601TimeEncoder,
		EncodeDuration: zapcore.StringDurationEncoder,
		EncodeCaller:   zapcore.ShortCallerEncoder,
	}

	consoleEncoderCfg := zapcore.EncoderConfig{
		TimeKey:        "ts",
		LevelKey:       "level",
		NameKey:        "logger",
		CallerKey:      "caller",
		MessageKey:     "msg",
		StacktraceKey:  "stacktrace",
		LineEnding:     zapcore.DefaultLineEnding,
		EncodeLevel:    zapcore.CapitalColorLevelEncoder,
		EncodeTime:     zapcore.ISO8601TimeEncoder,
		EncodeDuration: zapcore.StringDurationEncoder,
		EncodeCaller:   zapcore.ShortCallerEncoder,
	}

	// --- lumberjack rotators ---
	appRotator := &lumberjack.Logger{
		Filename:   cfg.Dir + "/" + appFilename,
		MaxSize:    cfg.MaxSizeMB,
		MaxAge:     cfg.MaxAgeDays,
		MaxBackups: cfg.MaxBackups,
		LocalTime:  true,
		Compress:   true,
	}

	errRotator := &lumberjack.Logger{
		Filename:   cfg.Dir + "/" + errFilename,
		MaxSize:    cfg.MaxSizeMB,
		MaxAge:     cfg.MaxAgeDays,
		MaxBackups: cfg.MaxBackups,
		LocalTime:  true,
		Compress:   true,
	}

	// Keep references for GinWriter / GinErrorWriter.
	appWriter = appRotator
	errWriter = errRotator

	// --- zap cores ---
	fileEncoder := zapcore.NewConsoleEncoder(fileEncoderCfg)

	// app.log: all levels
	appFileCore := zapcore.NewCore(
		fileEncoder,
		zapcore.AddSync(appRotator),
		zap.DebugLevel,
	)

	// error.log: ERROR and above
	errFileCore := zapcore.NewCore(
		fileEncoder,
		zapcore.AddSync(errRotator),
		zap.ErrorLevel,
	)

	cores := []zapcore.Core{appFileCore, errFileCore}

	// Console output (optional)
	if cfg.Console {
		consoleMinLevel := resolveConsoleLevel(cfg.ConsoleLevel)
		consoleEncoder := zapcore.NewConsoleEncoder(consoleEncoderCfg)
		consoleCore := zapcore.NewCore(
			consoleEncoder,
			zapcore.AddSync(os.Stdout),
			consoleMinLevel,
		)
		cores = append(cores, consoleCore)

		// Also let GinWriter/GinErrorWriter tee to console.
		appWriter = io.MultiWriter(os.Stdout, appRotator)
		errWriter = io.MultiWriter(os.Stderr, errRotator)
	}

	core := zapcore.NewTee(cores...)

	// Build the logger.
	// AddCaller: log the file:line of the call site.
	// AddCallerSkip(1): skip this wrapper layer so the caller location is correct.
	zapLogger = zap.New(core,
		zap.AddCaller(),
		zap.AddCallerSkip(1),
	)
	sugar = zapLogger.Sugar()

	// Redirect the standard library logger so existing log.Printf calls
	// throughout the codebase flow into app.log.
	stdWriter, _ := zap.NewStdLogAt(zapLogger.WithOptions(zap.AddCallerSkip(1)), zap.InfoLevel)
	if stdWriter != nil {
		log.SetOutput(stdWriter.Writer())
		log.SetFlags(0) // zap handles timestamps and formatting
	}
}

// Sync flushes any buffered log entries. Call before application exit.
func Sync() {
	if zapLogger != nil {
		_ = zapLogger.Sync()
	}
}

// ---------- public helpers (printf-style) ----------

// Info logs an informational message to app.log.
func Info(format string, args ...any) {
	getSugar().Infof(format, args...)
}

// Warn logs a warning message to app.log.
func Warn(format string, args ...any) {
	getSugar().Warnf(format, args...)
}

// Error logs an error message to both app.log and error.log.
func Error(format string, args ...any) {
	getSugar().Errorf(format, args...)
}

// Errorf is an alias for Error (for call-site readability).
func Errorf(format string, args ...any) {
	getSugar().Errorf(format, args...)
}

// Fatal logs an error and exits the process.
func Fatal(format string, args ...any) {
	getSugar().Fatalf(format, args...)
}

// GinWriter returns an io.Writer suitable for gin.DefaultWriter so that Gin
// access logs also flow into app.log.
func GinWriter() io.Writer {
	if appWriter != nil {
		return appWriter
	}
	return os.Stdout
}

// GinErrorWriter returns an io.Writer suitable for gin.DefaultErrorWriter.
func GinErrorWriter() io.Writer {
	if errWriter != nil {
		return errWriter
	}
	return os.Stderr
}

// ---------- internal ----------

// resolveConsoleLevel converts a string level name to a zapcore.Level.
// Accepted values (case-insensitive): "debug", "info", "warn", "error".
// Defaults to DEBUG so that existing callers that omit ConsoleLevel keep the
// previous behaviour.
func resolveConsoleLevel(s string) zapcore.Level {
	switch s {
	case "info", "INFO":
		return zap.InfoLevel
	case "warn", "WARN":
		return zap.WarnLevel
	case "error", "ERROR":
		return zap.ErrorLevel
	default:
		return zap.DebugLevel
	}
}

func getSugar() *zap.SugaredLogger {
	if sugar == nil {
		// Fallback before Init() is called.
		l, _ := zap.NewDevelopment()
		return l.Sugar().WithOptions(zap.AddCallerSkip(1))
	}
	return sugar
}

// Deprecated: kept only for reference; zap handles stack traces natively.
func init() {
	// Provide a usable default so that calls before Init() don't panic.
	l, _ := zap.NewDevelopment()
	zapLogger = l
	sugar = l.Sugar()
	appWriter = os.Stdout
	errWriter = os.Stderr
}

// FormatError formats an error with a message prefix, useful for structured
// error wrapping in log calls.
func FormatError(msg string, err error) string {
	return fmt.Sprintf("%s: %v", msg, err)
}
