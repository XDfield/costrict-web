package middleware

import (
	"bytes"
	"io"
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
		c.Writer.Header().Set("Access-Control-Allow-Headers", "Content-Type, Content-Length, Accept-Encoding, X-CSRF-Token, Authorization, accept, origin, Cache-Control, X-Requested-With")
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

func ErrorLogger() gin.HandlerFunc {
	return func(c *gin.Context) {
		var bodyBytes []byte
		if c.Request.Body != nil {
			bodyBytes, _ = io.ReadAll(c.Request.Body)
			c.Request.Body = io.NopCloser(bytes.NewBuffer(bodyBytes))
		}

		c.Next()

		status := c.Writer.Status()
		if status >= http.StatusBadRequest {
			msg := "%s %s => %d | body: %s | errors: %s"
			args := []any{
				c.Request.Method,
				c.Request.RequestURI,
				status,
				logger.Truncate(string(bodyBytes), 2000),
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
