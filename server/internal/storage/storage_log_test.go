package storage

import (
	"bytes"
	"context"
	"io"
	"log"
	"strings"
	"testing"
)

type failingBackend struct{}

func (failingBackend) Put(ctx context.Context, key string, r io.Reader, size int64) error {
	return io.ErrUnexpectedEOF
}
func (failingBackend) Get(ctx context.Context, key string) (io.ReadCloser, int64, error) {
	return nil, 0, io.ErrUnexpectedEOF
}

func captureLog(t *testing.T, fn func()) string {
	t.Helper()
	var buf bytes.Buffer
	prev := log.Writer()
	log.SetOutput(&buf)
	defer log.SetOutput(prev)
	fn()
	return buf.String()
}

func TestConfiguredBackendLogsPutAndGetFailures(t *testing.T) {
	b := &ConfiguredBackend{Kind: KindS3, Backend: failingBackend{}}

	out := captureLog(t, func() {
		if err := b.Put(context.Background(), "a/key", strings.NewReader("x"), 1); err == nil {
			t.Fatal("expected put error")
		}
	})
	if !strings.Contains(out, "storage put failed") || !strings.Contains(out, "a/key") {
		t.Fatalf("put failure not logged with key: %q", out)
	}

	out = captureLog(t, func() {
		if _, _, err := b.Get(context.Background(), "a/key"); err == nil {
			t.Fatal("expected get error")
		}
	})
	if !strings.Contains(out, "storage get failed") || !strings.Contains(out, "backend=s3") {
		t.Fatalf("get failure not logged with backend: %q", out)
	}
}

func TestValidateRecordedBackendLogsMismatch(t *testing.T) {
	out := captureLog(t, func() {
		// failingBackend is not a *ConfiguredBackend, so KindOf reports
		// "local"; a row recorded under s3 must therefore be rejected.
		if err := ValidateRecordedBackend(KindS3, failingBackend{}); err == nil {
			t.Fatal("expected mismatch error")
		}
	})
	if !strings.Contains(out, "storage backend mismatch") {
		t.Fatalf("mismatch not logged: %q", out)
	}
}
