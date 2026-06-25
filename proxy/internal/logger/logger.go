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
	zapLogger    *zap.Logger
	sugar        *zap.SugaredLogger
	appWriter    io.Writer
	errWriter    io.Writer
	requestWriter io.Writer
)

type Config struct {
	Dir        string
	MaxSizeMB  int
	MaxAgeDays int
	MaxBackups int
	Console    bool
}

func Init(cfg Config) {
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

	if err := os.MkdirAll(cfg.Dir, 0o755); err != nil {
		log.Fatalf("logger: failed to create log dir %s: %v", cfg.Dir, err)
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

	appRotator := &lumberjack.Logger{
		Filename:   cfg.Dir + "/app.log",
		MaxSize:    cfg.MaxSizeMB,
		MaxAge:     cfg.MaxAgeDays,
		MaxBackups: cfg.MaxBackups,
		LocalTime:  true,
		Compress:   true,
	}

	errRotator := &lumberjack.Logger{
		Filename:   cfg.Dir + "/error.log",
		MaxSize:    cfg.MaxSizeMB,
		MaxAge:     cfg.MaxAgeDays,
		MaxBackups: cfg.MaxBackups,
		LocalTime:  true,
		Compress:   true,
	}

	reqRotator := &lumberjack.Logger{
		Filename:   cfg.Dir + "/requests.log",
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
		consoleEncoder := zapcore.NewConsoleEncoder(consoleEncoderCfg)
		consoleCore := zapcore.NewCore(
			consoleEncoder,
			zapcore.AddSync(os.Stdout),
			zap.DebugLevel,
		)
		cores = append(cores, consoleCore)

		appWriter = io.MultiWriter(os.Stdout, appRotator)
		errWriter = io.MultiWriter(os.Stderr, errRotator)
		requestWriter = io.MultiWriter(os.Stdout, reqRotator)
	}

	core := zapcore.NewTee(cores...)

	zapLogger = zap.New(core,
		zap.AddCaller(),
		zap.AddCallerSkip(1),
		zap.AddStacktrace(zap.ErrorLevel),
	)
	sugar = zapLogger.Sugar()

	stdWriter, _ := zap.NewStdLogAt(zapLogger.WithOptions(zap.AddCallerSkip(1)), zap.InfoLevel)
	if stdWriter != nil {
		log.SetOutput(stdWriter.Writer())
		log.SetFlags(0)
	}
}

func Sync() {
	if zapLogger != nil {
		_ = zapLogger.Sync()
	}
}

func Info(format string, args ...any) {
	getSugar().Infof(format, args...)
}

func Warn(format string, args ...any) {
	getSugar().Warnf(format, args...)
}

func Error(format string, args ...any) {
	getSugar().Errorf(format, args...)
}

func Errorf(format string, args ...any) {
	getSugar().Errorf(format, args...)
}

func Fatal(format string, args ...any) {
	getSugar().Fatalf(format, args...)
}

func GinWriter() io.Writer {
	if requestWriter != nil {
		return requestWriter
	}
	return os.Stdout
}

func GinErrorWriter() io.Writer {
	if errWriter != nil {
		return errWriter
	}
	return os.Stderr
}

func getSugar() *zap.SugaredLogger {
	if sugar == nil {
		l, _ := zap.NewDevelopment()
		return l.Sugar().WithOptions(zap.AddCallerSkip(1))
	}
	return sugar
}

func init() {
	l, _ := zap.NewDevelopment()
	zapLogger = l
	sugar = l.Sugar()
	appWriter = os.Stdout
	errWriter = os.Stderr
	requestWriter = os.Stdout
}

func FormatError(msg string, err error) string {
	return fmt.Sprintf("%s: %v", msg, err)
}
