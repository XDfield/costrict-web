// Package logger provides application-wide structured logging for cs-user,
// backed by go.uber.org/zap with automatic file rotation (lumberjack) and
// 7-day retention.
//
// This is a port of costrict-web/server/internal/logger so both services
// share the same log layout (app.log / error.log / requests.log) and the
// same env knobs (LOG_DIR, LOG_FILE_PREFIX).
//
// Three log files are maintained under the configured directory (default: ./logs):
//   - <prefix>-app.log      – application logs (DEBUG … ERROR), excluding gin access logs
//   - <prefix>-error.log    – ERROR and above only
//   - requests.log          – gin HTTP access logs (shared layout, no prefix)
//
// The package exposes printf-style convenience functions (Info, Warn, Error, ...)
// so callers do not need to manage logger instances directly.
package logger

import (
	"fmt"
	"io"
	"log"
	"log/slog"
	"os"

	"go.uber.org/zap"
	"go.uber.org/zap/exp/zapslog"
	"go.uber.org/zap/zapcore"
	"gopkg.in/natefinch/lumberjack.v2"
)

var (
	zapLogger     *zap.Logger
	sugar         *zap.SugaredLogger
	appWriter     io.Writer
	errWriter     io.Writer
	requestWriter io.Writer
)

// Config controls log output behaviour.
type Config struct {
	Dir          string // directory for log files, default "./logs"
	FilePrefix   string // prefix for log file names, e.g. "cs-user" => cs-user-app.log
	MaxSizeMB    int    // max size in MB before rotation, default 100
	MaxAgeDays   int    // max days to keep old files, default 7
	MaxBackups   int    // max number of old files to keep, default 10
	Console      bool   // also write to stdout/stderr, default true
	ConsoleLevel string // minimum log level for console output: "debug"(default)|"info"|"warn"|"error"
}

// Init initialises the global loggers. It MUST be called once early in main().
// After Init the standard library log.Printf / log.Println also write to
// app.log (the default logger's output is redirected).
//
// LOG_DIR and LOG_FILE_PREFIX env vars override Config.Dir and
// Config.FilePrefix respectively — set in cs-user/.env for deployment.
func Init(cfg Config) {
	if envDir := os.Getenv("LOG_DIR"); envDir != "" {
		cfg.Dir = envDir
	}
	if envPrefix := os.Getenv("LOG_FILE_PREFIX"); envPrefix != "" {
		cfg.FilePrefix = envPrefix
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

	if err := os.MkdirAll(cfg.Dir, 0o755); err != nil {
		log.Fatalf("logger: failed to create log dir %s: %v", cfg.Dir, err)
	}

	appFilename := "app.log"
	errFilename := "error.log"
	reqFilename := "requests.log"
	if cfg.FilePrefix != "" {
		appFilename = cfg.FilePrefix + "-app.log"
		errFilename = cfg.FilePrefix + "-error.log"
	}

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

	consoleEncoderCfg := fileEncoderCfg
	consoleEncoderCfg.EncodeLevel = zapcore.CapitalColorLevelEncoder

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
	reqRotator := &lumberjack.Logger{
		Filename:   cfg.Dir + "/" + reqFilename,
		MaxSize:    cfg.MaxSizeMB,
		MaxAge:     cfg.MaxAgeDays,
		MaxBackups: cfg.MaxBackups,
		LocalTime:  true,
		Compress:   true,
	}

	appWriter = appRotator
	errWriter = errRotator
	requestWriter = reqRotator

	fileEncoder := zapcore.NewConsoleEncoder(fileEncoderCfg)

	appFileCore := zapcore.NewCore(
		fileEncoder,
		zapcore.AddSync(appRotator),
		zap.DebugLevel,
	)
	errFileCore := zapcore.NewCore(
		fileEncoder,
		zapcore.AddSync(errRotator),
		zap.ErrorLevel,
	)

	cores := []zapcore.Core{appFileCore, errFileCore}

	if cfg.Console {
		consoleMinLevel := resolveConsoleLevel(cfg.ConsoleLevel)
		consoleEncoder := zapcore.NewConsoleEncoder(consoleEncoderCfg)
		consoleCore := zapcore.NewCore(
			consoleEncoder,
			zapcore.AddSync(os.Stdout),
			consoleMinLevel,
		)
		cores = append(cores, consoleCore)

		appWriter = io.MultiWriter(os.Stdout, appRotator)
		errWriter = io.MultiWriter(os.Stderr, errRotator)
		requestWriter = io.MultiWriter(os.Stdout, reqRotator)
	}

	core := zapcore.NewTee(cores...)

	// AddCaller without skip on the base logger — L() returns this raw
	// logger, so its caller resolution must NOT over-count wrapper frames.
	// The sugar helpers (Info/Warn/Error/Fatal) wrap one extra frame and
	// get their own AddCallerSkip(1) below.
	zapLogger = zap.New(core, zap.AddCaller())
	sugar = zapLogger.Sugar().WithOptions(zap.AddCallerSkip(1))

	// Replace globals so any library code using zap.L() / zap.S() (GORM,
	// gin middleware, etc.) also flows through these file-backed loggers.
	zap.ReplaceGlobals(zapLogger)

	// stdlib log.Printf writes through stdWriter's underlying writer, which
	// adds one frame (the stdlib log machinery) on top of this wrapper.
	// Skip 2 so callers see their own file:line rather than log/output.go.
	stdWriter, _ := zap.NewStdLogAt(zapLogger.WithOptions(zap.AddCallerSkip(2)), zap.InfoLevel)
	if stdWriter != nil {
		log.SetOutput(stdWriter.Writer())
		log.SetFlags(0)
	}

	// Route slog output through zap so libraries using slog (e.g. goose)
	// produce the same console-encoder format as the rest of the app
	// instead of mismatched JSON.
	slog.SetDefault(slog.New(zapslog.NewHandler(zapLogger.Core())))
}

// Sync flushes any buffered log entries. Call before application exit.
func Sync() {
	if zapLogger != nil {
		_ = zapLogger.Sync()
	}
}

// ---------- public helpers (printf-style) ----------

// L returns the package-level *zap.Logger initialized by Init. Returns a
// no-op logger if Init has not been called yet (defensive).
func L() *zap.Logger {
	if zapLogger == nil {
		return zap.NewNop()
	}
	return zapLogger
}

func Info(format string, args ...any)  { getSugar().Infof(format, args...) }
func Warn(format string, args ...any)  { getSugar().Warnf(format, args...) }
func Error(format string, args ...any) { getSugar().Errorf(format, args...) }

// Errorf is an alias for Error (for call-site readability).
func Errorf(format string, args ...any) { getSugar().Errorf(format, args...) }

// Fatal logs an error and exits the process.
func Fatal(format string, args ...any) { getSugar().Fatalf(format, args...) }

// GinWriter returns an io.Writer suitable for gin.DefaultWriter so that Gin
// access logs flow into requests.log (separate from application logs).
func GinWriter() io.Writer {
	if requestWriter != nil {
		return requestWriter
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
		l, _ := zap.NewDevelopment()
		return l.Sugar().WithOptions(zap.AddCallerSkip(1))
	}
	return sugar
}

func init() {
	// Provide a usable default so that calls before Init() don't panic.
	l, _ := zap.NewDevelopment()
	zapLogger = l
	sugar = l.Sugar()
	appWriter = os.Stdout
	errWriter = os.Stderr
	requestWriter = os.Stdout
}

// FormatError formats an error with a message prefix, useful for structured
// error wrapping in log calls.
func FormatError(msg string, err error) string {
	return fmt.Sprintf("%s: %v", msg, err)
}
