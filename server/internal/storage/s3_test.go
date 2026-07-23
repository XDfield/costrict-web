package storage

import (
	"bytes"
	"context"
	"encoding/pem"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

type observedRequest struct {
	method        string
	path          string
	contentLength int64
	header        http.Header
	trailer       http.Header
	transfer      []string
	body          []byte
}

func TestS3BackendUsesOnlyExactPutAndGetWithoutCRC32(t *testing.T) {
	var (
		mu       sync.Mutex
		requests []observedRequest
	)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read request body: %v", err)
		}
		mu.Lock()
		requests = append(requests, observedRequest{
			method:        r.Method,
			path:          r.URL.Path,
			contentLength: r.ContentLength,
			header:        r.Header.Clone(),
			trailer:       r.Trailer.Clone(),
			transfer:      append([]string(nil), r.TransferEncoding...),
			body:          body,
		})
		mu.Unlock()

		switch r.Method {
		case http.MethodPut:
			w.WriteHeader(http.StatusOK)
		case http.MethodGet:
			w.Header().Set("Content-Length", "12")
			_, _ = w.Write([]byte("downloaded!!"))
		default:
			http.Error(w, "unsupported method", http.StatusMethodNotAllowed)
		}
	}))
	defer server.Close()

	tempDir := t.TempDir()
	t.Setenv("TMPDIR", tempDir)
	t.Setenv("AWS_ACCESS_KEY_ID", "test-access-key")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "test-secret-key")
	t.Setenv("AWS_SESSION_TOKEN", "")
	t.Setenv("AWS_EC2_METADATA_DISABLED", "true")

	backend, err := NewS3Backend(context.Background(), S3Config{
		Endpoint:       server.URL,
		Bucket:         "artifacts",
		Region:         "internal",
		ForcePathStyle: true,
	})
	if err != nil {
		t.Fatal(err)
	}

	payload := []byte("upload data")
	nonSeekable := io.TeeReader(bytes.NewReader(payload), io.Discard)
	if err := backend.Put(context.Background(), "items/item-1/file.bin", nonSeekable, int64(len(payload))); err != nil {
		t.Fatal(err)
	}
	reader, size, err := backend.Get(context.Background(), "items/item-1/file.bin")
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	downloaded, err := io.ReadAll(reader)
	if err != nil {
		t.Fatal(err)
	}
	if size != int64(len(downloaded)) || string(downloaded) != "downloaded!!" {
		t.Fatalf("unexpected get result size=%d body=%q", size, downloaded)
	}

	mu.Lock()
	got := append([]observedRequest(nil), requests...)
	mu.Unlock()
	if len(got) != 2 {
		t.Fatalf("expected exactly PUT and GET, got %d requests: %#v", len(got), got)
	}
	if got[0].method != http.MethodPut || got[1].method != http.MethodGet {
		t.Fatalf("unexpected methods: %s, %s", got[0].method, got[1].method)
	}
	for _, request := range got {
		if request.path != "/artifacts/items/item-1/file.bin" {
			t.Errorf("%s path = %q", request.method, request.path)
		}
		for name, values := range request.header {
			lowerName := strings.ToLower(name)
			lowerValues := strings.ToLower(strings.Join(values, ","))
			if strings.Contains(lowerName, "crc32") || strings.Contains(lowerName, "checksum") ||
				strings.Contains(lowerValues, "crc32") || strings.Contains(lowerValues, "checksum") {
				t.Errorf("%s sent unsupported header %s=%q", request.method, name, values)
			}
		}
		for name, values := range request.trailer {
			lower := strings.ToLower(name + strings.Join(values, ","))
			if strings.Contains(lower, "crc32") || strings.Contains(lower, "checksum") {
				t.Errorf("%s sent unsupported trailer %s=%q", request.method, name, values)
			}
		}
		if len(request.trailer) != 0 {
			t.Errorf("%s sent unexpected trailers: %q", request.method, request.trailer)
		}
		if len(request.transfer) != 0 {
			t.Errorf("%s used transfer encoding %q", request.method, request.transfer)
		}
	}
	if got[0].contentLength != int64(len(payload)) {
		t.Errorf("PUT Content-Length = %d, want %d", got[0].contentLength, len(payload))
	}
	if !bytes.Equal(got[0].body, payload) {
		t.Errorf("PUT body = %q, want %q", got[0].body, payload)
	}
	entries, err := os.ReadDir(tempDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Errorf("temporary upload files were not removed: %v", entries)
	}
}

func TestHTTPClientWithCA(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()

	caFile := filepath.Join(t.TempDir(), "ca.pem")
	caPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw})
	if err := os.WriteFile(caFile, caPEM, 0o600); err != nil {
		t.Fatal(err)
	}
	client, err := httpClientWithCA(caFile)
	if err != nil {
		t.Fatal(err)
	}
	response, err := client.Get(server.URL)
	if err != nil {
		t.Fatalf("custom CA client request failed: %v", err)
	}
	response.Body.Close()
}

func TestHTTPClientWithCARejectsInvalidPEM(t *testing.T) {
	caFile := filepath.Join(t.TempDir(), "invalid.pem")
	if err := os.WriteFile(caFile, []byte("not a certificate"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := httpClientWithCA(caFile); err == nil || !strings.Contains(err.Error(), "no valid certificates") {
		t.Fatalf("expected invalid CA error, got %v", err)
	}
}

func TestObjectStorageHTTPClientHasResponseHeaderTimeout(t *testing.T) {
	client, err := objectStorageHTTPClient("")
	if err != nil {
		t.Fatal(err)
	}
	transport, ok := client.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("unexpected transport type %T", client.Transport)
	}
	if transport.ResponseHeaderTimeout <= 0 {
		t.Fatal("object storage transport must bound response header waits")
	}
}
