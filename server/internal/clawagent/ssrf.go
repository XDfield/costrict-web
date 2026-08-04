package clawagent

import (
	"github.com/costrict/costrict-web/server/internal/safetch"
)

// ValidateProviderBaseURL enforces that an LLM provider BaseURL is safe to fetch.
//
// Wraps safetch.ValidateURL, which performs scheme/host-literal/A-record checks
// at write time. The empty-string case returns nil so callers can omit BaseURL
// and fall back to the platform default elsewhere. See safetch package docs for
// the runtime DNS-rebinding protection applied at fetch time when callers use
// safetch.NewClient.
func ValidateProviderBaseURL(raw string) error {
	if raw == "" {
		return nil // empty BaseURL falls back to platform default elsewhere
	}
	return safetch.ValidateURL(raw)
}
