package middleware

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/costrict/costrict-web/server/internal/logger"
	"github.com/gin-gonic/gin"
)

// sensitiveQueryParams is the set of URL query parameter names whose values
// are redacted from access / error logs. The values themselves are still
// honored by ExtractToken (the only legitimate consumer of ?token= —
// browser EventSource / WebSocket fallback), but they must not land in
// logs/requests.log or any other log sink. Case-insensitive match.
var sensitiveQueryParams = map[string]struct{}{
	"token": {},
}

// redactQueryToken returns uri with the value of any sensitive query
// parameter (see sensitiveQueryParams) replaced by "[REDACTED]". Used by
// Logger / ErrorLogger so JWTs passed via ?token= don't leak into access
// logs (gin's default formatter writes Path + "?" + RawQuery verbatim,
// and ErrorLogger logs c.Request.RequestURI directly).
//
// Non-sensitive params pass through unchanged. Malformed queries fall
// through to the original uri (better a noisy log line than a dropped one).
// Redaction re-encodes the query via url.Values.Encode, which sorts keys
// alphabetically — order is not preserved, but log readability is fine.
func redactQueryToken(uri string) string {
	qIdx := strings.Index(uri, "?")
	if qIdx == -1 {
		return uri
	}
	path, rawQ := uri[:qIdx], uri[qIdx+1:]
	values, err := url.ParseQuery(rawQ)
	if err != nil {
		return uri
	}
	changed := false
	for k := range values {
		if _, sens := sensitiveQueryParams[strings.ToLower(k)]; sens {
			values[k] = []string{"[REDACTED]"}
			changed = true
		}
	}
	if !changed {
		return uri
	}
	return path + "?" + values.Encode()
}

// CORSConfig holds the allowed origins for CORS.
//
// Default policy is DENY: when AllowedOrigins is empty AND DevMode is false,
// no CORS headers are emitted and credentialed cross-origin requests are
// blocked by the browser. This is the safe default for production.
//
// DevMode=true opts back into the legacy "reflect any Origin" behavior so
// local development workflows (vite dev server on a different port, etc.)
// keep working. DevMode must never be true in a production deployment —
// reflecting arbitrary Origins while sending credentials enables CSRF-style
// exfiltration from any malicious site a user visits. See secreport
// CVSS 2.3 (CORS reflection).
type CORSConfig struct {
	AllowedOrigins []string
	DevMode        bool
}

func CORS(cfg CORSConfig) gin.HandlerFunc {
	allowed := make(map[string]bool, len(cfg.AllowedOrigins))
	for _, o := range cfg.AllowedOrigins {
		allowed[strings.TrimRight(o, "/")] = true
	}

	return func(c *gin.Context) {
		origin := c.GetHeader("Origin")

		if len(allowed) == 0 {
			// No allow-list configured. Default-deny: emit no CORS headers so the
			// browser blocks credentialed cross-origin reads. Only opt into the
			// legacy reflect-Origin behavior when DevMode is explicitly set —
			// this is for local development only.
			if !cfg.DevMode {
				c.Next()
				return
			}
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

// Logger returns the access-log middleware. The default gin formatter writes
// param.Path verbatim into the access log; for requests that carry a JWT in
// the `?token=` query param (browser EventSource / WebSocket fallback — see
// ExtractToken), that path contains the raw token. We override the formatter
// to redact sensitive query params before formatting, then render the line
// using gin's default layout. The formatter is reproduced here (rather than
// wrapped) because gin keeps its defaultLogFormatter unexported.
func Logger() gin.HandlerFunc {
	return gin.LoggerWithConfig(gin.LoggerConfig{
		Formatter: func(param gin.LogFormatterParams) string {
			param.Path = redactQueryToken(param.Path)

			var statusColor, methodColor, resetColor string
			if param.IsOutputColor() {
				statusColor = param.StatusCodeColor()
				methodColor = param.MethodColor()
				resetColor = param.ResetColor()
			}

			if param.Latency > time.Minute {
				// Truncate in a well-defined way (matches gin's default).
				param.Latency = param.Latency - param.Latency%time.Second
			}

			return fmt.Sprintf("[GIN] %v |%s %3d %s| %13v | %15s |%s %-7s %s %#v\n%s",
				param.TimeStamp.Format("2006/01/02 - 15:04:05"),
				statusColor, param.StatusCode, resetColor,
				param.Latency,
				param.ClientIP,
				methodColor, param.Method, resetColor,
				param.Path,
				param.ErrorMessage,
			)
		},
	})
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
			// Mask known-sensitive fields (password/apiKey/token/secret/etc.)
			// before logging the body. Non-JSON bodies pass through unchanged
			// and remain subject to length truncation below.
			masked := maskSensitiveBody(bodyBytes)
			msg := "%s %s => %d | body: %s | errors: %s"
			args := []any{
				c.Request.Method,
				redactQueryToken(c.Request.RequestURI),
				status,
				logger.Truncate(string(masked), 2000),
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
