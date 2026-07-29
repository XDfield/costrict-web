package storage

import (
	"context"
	"fmt"
	"io"
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

// captureLog swaps the package's error sink so assertions do not depend on the
// global logger being initialised.
func captureLog(t *testing.T, fn func()) string {
	t.Helper()
	var out strings.Builder
	prev := logErrorf
	logErrorf = func(format string, args ...any) {
		out.WriteString(fmt.Sprintf(format, args...))
		out.WriteString("\n")
	}
	defer func() { logErrorf = prev }()
	fn()
	return out.String()
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

// The cause must precede the key. Log viewers truncate long lines, and a
// storage key is 100+ characters of UUID and content SHA — putting the error
// last is what hid a DNS failure through three rounds of investigation.
func TestPutFailureLogsCauseBeforeKey(t *testing.T) {
	b := &ConfiguredBackend{Kind: KindS3, Backend: failingBackend{}}
	key := "d0b3cc1d-902a-4bab-b580-1aa23b3ba3c6/assets/r1/" +
		"222c087915cd7271a28db552bb125da8fab0ec980cb85eb021cb58449a99a773/assets/fingerprints.bin"

	out := captureLog(t, func() {
		if err := b.Put(context.Background(), key, strings.NewReader("x"), 1); err == nil {
			t.Fatal("expected put error")
		}
	})

	causeAt := strings.Index(out, io.ErrUnexpectedEOF.Error())
	keyAt := strings.Index(out, key)
	if causeAt < 0 {
		t.Fatalf("cause missing from log line: %q", out)
	}
	if keyAt < 0 {
		t.Fatalf("key missing from log line: %q", out)
	}
	if causeAt > keyAt {
		t.Fatalf("cause must appear before the key, got: %q", out)
	}
	if prefix := "storage put failed: "; !strings.HasPrefix(out, prefix) {
		t.Fatalf("line must lead with %q, got: %q", prefix, out)
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
