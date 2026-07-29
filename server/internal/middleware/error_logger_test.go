package middleware

import (
	"bytes"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
)

func newErrorLoggerRouter(t *testing.T, onRequest func(*gin.Context)) *gin.Engine {
	t.Helper()
	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.Use(ErrorLogger())
	router.POST("/items", func(c *gin.Context) {
		onRequest(c)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "boom"})
	})
	return router
}

// A multipart upload must reach the handler as an unread stream. Buffering it
// in the logger doubled peak memory on every 50MB archive upload.
func TestErrorLoggerLeavesMultipartBodyStreaming(t *testing.T) {
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	part, err := writer.CreateFormFile("file", "port-oracle.zip")
	if err != nil {
		t.Fatalf("CreateFormFile: %v", err)
	}
	payload := append([]byte("PK\x03\x04"), bytes.Repeat([]byte{0x00, 0xff}, 4096)...)
	if _, err := part.Write(payload); err != nil {
		t.Fatalf("write part: %v", err)
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("close writer: %v", err)
	}

	var received []byte
	router := newErrorLoggerRouter(t, func(c *gin.Context) {
		file, _, err := c.Request.FormFile("file")
		if err != nil {
			t.Errorf("handler could not read uploaded file: %v", err)
			return
		}
		defer file.Close()
		received, _ = io.ReadAll(file)
	})

	req := httptest.NewRequest(http.MethodPost, "/items", &body)
	req.Header.Set("Content-Type", writer.FormDataContentType())
	router.ServeHTTP(httptest.NewRecorder(), req)

	if !bytes.Equal(received, payload) {
		t.Fatalf("handler saw %d bytes, want %d", len(received), len(payload))
	}
}

func TestSkipBodyCaptureByContentType(t *testing.T) {
	cases := []struct {
		name        string
		contentType string
		length      int64
		wantSkip    bool
		wantIn      string
	}{
		{"multipart upload", "multipart/form-data; boundary=xyz", 5_414_497, true, "multipart/form-data"},
		{"raw zip", "application/zip", 1024, true, "application/zip"},
		{"octet stream", "application/octet-stream", 1024, true, "application/octet-stream"},
		{"small json", "application/json", 512, false, ""},
		{"oversized json", "application/json", maxLoggedBodyBytes + 1, true, "exceeds"},
		{"json at limit", "application/json", maxLoggedBodyBytes, false, ""},
		{"unknown length json", "application/json", -1, false, ""},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "/items", strings.NewReader("{}"))
			req.Header.Set("Content-Type", tc.contentType)
			req.ContentLength = tc.length

			skip, summary := skipBodyCapture(req)
			if skip != tc.wantSkip {
				t.Fatalf("skip = %v, want %v (summary %q)", skip, tc.wantSkip, summary)
			}
			if !tc.wantSkip {
				if summary != "" {
					t.Fatalf("summary must be empty when body is captured, got %q", summary)
				}
				return
			}
			if !strings.Contains(summary, tc.wantIn) {
				t.Fatalf("summary %q does not mention %q", summary, tc.wantIn)
			}
			if strings.Contains(summary, "PK") {
				t.Fatalf("summary must not contain body bytes: %q", summary)
			}
		})
	}
}

func TestSkipBodyCaptureHandlesNilBody(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/items", nil)
	req.Body = nil
	if skip, summary := skipBodyCapture(req); skip || summary != "" {
		t.Fatalf("nil body must not be skipped explicitly, got skip=%v summary=%q", skip, summary)
	}
}

// A textual body under the cap is still logged — the fix narrows what gets
// captured, it does not remove request-body logging.
func TestErrorLoggerStillCapturesSmallTextBody(t *testing.T) {
	router := newErrorLoggerRouter(t, func(c *gin.Context) {
		data, err := io.ReadAll(c.Request.Body)
		if err != nil {
			t.Errorf("read body: %v", err)
			return
		}
		if string(data) != `{"name":"x"}` {
			t.Errorf("handler saw %q, want the original JSON body", string(data))
		}
	})

	req := httptest.NewRequest(http.MethodPost, "/items", strings.NewReader(`{"name":"x"}`))
	req.Header.Set("Content-Type", "application/json")
	router.ServeHTTP(httptest.NewRecorder(), req)
}
