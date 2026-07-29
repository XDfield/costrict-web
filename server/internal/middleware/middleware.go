package middleware

import (
	"bytes"
	"fmt"
	"io"
	"mime"
	"net/http"
	"strings"

	"github.com/costrict/costrict-web/server/internal/logger"
	"github.com/gin-gonic/gin"
)

// CORSConfig holds the allowed origins for CORS.
// If AllowedOrigins is empty, all origins are allowed (insecure, for development only).
type CORSConfig struct {
	AllowedOrigins []string
}

func CORS(cfg CORSConfig) gin.HandlerFunc {
	allowed := make(map[string]bool, len(cfg.AllowedOrigins))
	for _, o := range cfg.AllowedOrigins {
		allowed[strings.TrimRight(o, "/")] = true
	}

	return func(c *gin.Context) {
		origin := c.GetHeader("Origin")

		if len(allowed) == 0 {
			// Development mode: allow all origins (but still echo the actual origin
			// so that credentials work correctly).
			if origin != "" {
				c.Writer.Header().Set("Access-Control-Allow-Origin", origin)
				c.Writer.Header().Set("Vary", "Origin")
			} else {
				c.Writer.Header().Set("Access-Control-Allow-Origin", "*")
			}
		} else {
			if allowed[origin] {
				c.Writer.Header().Set("Access-Control-Allow-Origin", origin)
				c.Writer.Header().Set("Vary", "Origin")
			} else {
				// Origin not allowed — skip setting CORS headers.
				if c.Request.Method == "OPTIONS" {
					c.AbortWithStatus(http.StatusForbidden)
					return
				}
				c.Next()
				return
			}
		}

		c.Writer.Header().Set("Access-Control-Allow-Credentials", "true")
		c.Writer.Header().Set("Access-Control-Allow-Headers", "Content-Type, Content-Length, Accept-Encoding, X-CSRF-Token, Authorization, accept, origin, Cache-Control, X-Requested-With, X-Workspace-Directory, X-Opencode-Directory, X-Opencode-Workspace")
		c.Writer.Header().Set("Access-Control-Allow-Methods", "POST, OPTIONS, GET, PUT, DELETE, PATCH")

		if c.Request.Method == "OPTIONS" {
			c.AbortWithStatus(204)
			return
		}

		c.Next()
	}
}

func Logger() gin.HandlerFunc {
	return gin.Logger()
}

func Recovery() gin.HandlerFunc {
	return gin.Recovery()
}

// maxLoggedBodyBytes caps how much of a request body reaches the log on a
// failure. Bodies larger than this are never buffered at all.
const maxLoggedBodyBytes = 64 << 10

func ErrorLogger() gin.HandlerFunc {
	return func(c *gin.Context) {
		// Buffering the body is only worth it for small textual payloads.
		// A multipart archive upload is neither: dumping 2000 bytes of zip
		// into the log produced ~40 lines of mojibake that buried the actual
		// error, and reading the whole body just to throw it away doubled
		// peak memory on every 50MB upload.
		var bodyBytes []byte
		bodySummary := ""
		if skip, reason := skipBodyCapture(c.Request); skip {
			bodySummary = reason
		} else if c.Request.Body != nil {
			bodyBytes, _ = io.ReadAll(c.Request.Body)
			c.Request.Body = io.NopCloser(bytes.NewBuffer(bodyBytes))
		}

		c.Next()

		status := c.Writer.Status()
		if status >= http.StatusBadRequest {
			body := bodySummary
			if body == "" {
				body = logger.Truncate(string(bodyBytes), 2000)
			}
			msg := "%s %s => %d | body: %s | errors: %s"
			args := []any{
				c.Request.Method,
				c.Request.RequestURI,
				status,
				body,
				c.Errors.String(),
			}

			// 5xx = server fault → Error.
			// 4xx = client fault → Warn only.
			if status >= http.StatusInternalServerError {
				logger.Error(msg, args...)
			} else {
				logger.Warn(msg, args...)
			}
		}
	}
}

// skipBodyCapture reports whether a request body should be left unbuffered,
// along with the placeholder to log in its place. Skipping keeps the upload
// streaming straight through to the handler instead of being materialised
// twice in memory.
func skipBodyCapture(r *http.Request) (bool, string) {
	if r == nil || r.Body == nil {
		return false, ""
	}

	contentType := r.Header.Get("Content-Type")
	mediaType, _, err := mime.ParseMediaType(contentType)
	if err != nil {
		mediaType = contentType
	}
	switch {
	case strings.HasPrefix(mediaType, "multipart/"),
		mediaType == "application/octet-stream",
		mediaType == "application/zip",
		mediaType == "application/gzip",
		mediaType == "application/x-zip-compressed":
		return true, fmt.Sprintf("<not logged: %s, %s>", describeMediaType(mediaType), describeLength(r.ContentLength))
	}

	// An oversized body is not worth buffering even when it is textual.
	if r.ContentLength > maxLoggedBodyBytes {
		return true, fmt.Sprintf("<not logged: %s, %s exceeds %d byte log limit>",
			describeMediaType(mediaType), describeLength(r.ContentLength), maxLoggedBodyBytes)
	}

	return false, ""
}

func describeMediaType(mediaType string) string {
	if mediaType == "" {
		return "unknown content type"
	}
	return mediaType
}

func describeLength(contentLength int64) string {
	if contentLength < 0 {
		return "unknown length"
	}
	return fmt.Sprintf("%d bytes", contentLength)
}

// isDeviceProxyPath checks whether the URI is a device proxy request
// (pattern: /cloud/device/<deviceID>/proxy/...).
func isDeviceProxyPath(uri string) bool {
	// Strip query string before matching.
	path := uri
	if idx := strings.Index(uri, "?"); idx != -1 {
		path = uri[:idx]
	}
	return strings.HasPrefix(path, "/cloud/device/") && strings.Contains(path, "/proxy/")
}
