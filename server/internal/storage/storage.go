package storage

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
)

type Backend interface {
	Put(ctx context.Context, key string, reader io.Reader, size int64) error
	Get(ctx context.Context, key string) (io.ReadCloser, int64, error)
	Delete(ctx context.Context, key string) error
	PresignURL(ctx context.Context, key string, expiry time.Duration) (string, error)
	Exists(ctx context.Context, key string) (bool, error)
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
	if !strings.HasPrefix(cleanPath, filepath.Clean(l.BasePath)) {
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

func (l *LocalBackend) Delete(ctx context.Context, key string) error {
	fullPath, err := l.resolvePath(key)
	if err != nil {
		return err
	}
	return os.Remove(fullPath)
}

func (l *LocalBackend) PresignURL(ctx context.Context, key string, expiry time.Duration) (string, error) {
	return "", nil
}

func (l *LocalBackend) Exists(ctx context.Context, key string) (bool, error) {
	fullPath, err := l.resolvePath(key)
	if err != nil {
		return false, err
	}
	_, err = os.Stat(fullPath)
	if os.IsNotExist(err) {
		return false, nil
	}
	return err == nil, err
}
