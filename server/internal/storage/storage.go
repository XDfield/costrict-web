package storage

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/costrict/costrict-web/server/internal/logger"
)

// logErrorf routes storage failures to the application error log. It is a
// variable so tests can capture the formatted output without standing up the
// global logger.
//
// Deliberately NOT the standard library log.Printf: logger.Init redirects that
// to zap at InfoLevel, so every storage failure was being recorded as INFO and
// missed by level-based filters and error.log entirely.
var logErrorf = logger.Error

type Backend interface {
	Put(ctx context.Context, key string, reader io.Reader, size int64) error
	Get(ctx context.Context, key string) (io.ReadCloser, int64, error)
}

const (
	KindLocal = "local"
	KindS3    = "s3"
)

// ConfiguredBackend keeps the persisted backend discriminator together with
// the minimal object IO implementation used by the application.
type ConfiguredBackend struct {
	Kind    string
	Backend Backend
}

// Put/Get log failures here — the single choke point every handler and
// service goes through — because the HTTP layer replies with generic
// messages and would otherwise swallow the root cause (bad credentials,
// unreachable endpoint, missing object) entirely.
//
// The error comes FIRST in the format string. Keys carry a 36-char item UUID
// plus a 64-char content SHA, so a key-first line runs past 150 characters and
// log viewers truncate exactly the part worth reading — that cost three rounds
// of "please paste the full line" on a DNS failure that was in the log all along.
func (b *ConfiguredBackend) Put(ctx context.Context, key string, reader io.Reader, size int64) error {
	if err := b.Backend.Put(ctx, key, reader, size); err != nil {
		logErrorf("storage put failed: %v (backend=%s, size=%d, key=%s)", err, b.Kind, size, key)
		return err
	}
	return nil
}

func (b *ConfiguredBackend) Get(ctx context.Context, key string) (io.ReadCloser, int64, error) {
	reader, size, err := b.Backend.Get(ctx, key)
	if err != nil {
		logErrorf("storage get failed: %v (backend=%s, key=%s)", err, b.Kind, key)
		return nil, 0, err
	}
	return reader, size, nil
}

// KindOf returns the value stored in storage_backend. Direct test backends and
// legacy callers are treated as local; production construction always returns
// a ConfiguredBackend.
func KindOf(backend Backend) string {
	if configured, ok := backend.(*ConfiguredBackend); ok {
		return configured.Kind
	}
	return KindLocal
}

// ValidateRecordedBackend rejects a DB mapping that belongs to a different
// deployment mode. Empty values are legacy local rows.
func ValidateRecordedBackend(recorded string, backend Backend) error {
	if backend == nil {
		return fmt.Errorf("storage backend is not configured")
	}
	if recorded == "" {
		recorded = KindLocal
	}
	configured := KindOf(backend)
	if recorded != configured {
		// This fires for every read of an object written under the other
		// backend — the exact situation after a local->s3 switch without
		// data migration. Callers reply with a generic 5xx, so log the
		// actionable detail here.
		logErrorf("storage backend mismatch: object recorded under %q, configured backend is %q — unreadable until data is migrated or config reverted", recorded, configured)
		return fmt.Errorf("stored object uses backend %q, configured backend is %q", recorded, configured)
	}
	return nil
}

type LocalBackend struct {
	BasePath string
}

func NewLocalBackend(basePath string) (*LocalBackend, error) {
	if err := os.MkdirAll(basePath, 0755); err != nil {
		return nil, fmt.Errorf("failed to create storage directory: %w", err)
	}
	return &LocalBackend{BasePath: basePath}, nil
}

func (l *LocalBackend) resolvePath(key string) (string, error) {
	fullPath := filepath.Join(l.BasePath, filepath.FromSlash(key))
	cleanPath := filepath.Clean(fullPath)
	rel, err := filepath.Rel(filepath.Clean(l.BasePath), cleanPath)
	if err != nil || rel == ".." || filepath.IsAbs(rel) || len(rel) >= 3 && rel[:3] == ".."+string(filepath.Separator) {
		return "", fmt.Errorf("invalid path: path traversal detected")
	}
	return cleanPath, nil
}

func (l *LocalBackend) Put(ctx context.Context, key string, reader io.Reader, size int64) error {
	fullPath, err := l.resolvePath(key)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(fullPath), 0755); err != nil {
		return fmt.Errorf("failed to create directory: %w", err)
	}
	f, err := os.Create(fullPath)
	if err != nil {
		return fmt.Errorf("failed to create file: %w", err)
	}
	defer f.Close()
	if _, err := io.Copy(f, reader); err != nil {
		os.Remove(fullPath)
		return fmt.Errorf("failed to write file: %w", err)
	}
	return nil
}

func (l *LocalBackend) Get(ctx context.Context, key string) (io.ReadCloser, int64, error) {
	fullPath, err := l.resolvePath(key)
	if err != nil {
		return nil, 0, err
	}
	f, err := os.Open(fullPath)
	if err != nil {
		return nil, 0, fmt.Errorf("file not found: %w", err)
	}
	info, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, 0, err
	}
	return f, info.Size(), nil
}
