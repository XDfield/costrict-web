// Package middleware holds cs-user HTTP middlewares.
//
// internal_auth.go implements ADR D8: costrict-web → cs-user calls carry a
// shared secret in the X-Internal-Token header. Endpoints under /api/internal/*
// require this header; mismatched / missing token → 401.
package middleware

import (
	"crypto/subtle"
	"net/http"

	"github.com/gin-gonic/gin"
)

const InternalTokenHeader = "X-Internal-Token"

// RequireInternalToken returns a gin middleware that rejects requests without
// a valid X-Internal-Token header. Use it on /api/internal/* route groups only.
//
// Comparison uses subtle.ConstantTimeCompare to avoid timing leaks. As a
// defense-in-depth against operator misconfiguration, an empty expected token
// fails every request — config.Load() should already prevent this at startup,
// but the middleware guards direct construction too.
func RequireInternalToken(expected string) gin.HandlerFunc {
	return func(c *gin.Context) {
		if expected == "" {
			// Misconfiguration: refuse to serve rather than risk auth bypass
			// (ConstantTimeCompare("","") would return 1).
			c.AbortWithStatusJSON(http.StatusInternalServerError, gin.H{
				"error": "internal token not configured",
			})
			return
		}
		got := c.GetHeader(InternalTokenHeader)
		if got == "" {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "missing " + InternalTokenHeader})
			return
		}
		if subtle.ConstantTimeCompare([]byte(got), []byte(expected)) != 1 {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": "invalid internal token"})
			return
		}
		c.Next()
	}
}
