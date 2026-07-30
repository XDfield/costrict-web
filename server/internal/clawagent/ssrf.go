package clawagent

import (
	"fmt"
	"net"
	"net/url"
	"strings"
)

// ValidateProviderBaseURL enforces that an LLM provider BaseURL is safe to fetch.
// Rules:
//   - must be a valid absolute URL
//   - scheme must be http or https
//   - host must resolve to a public IP (no loopback / link-local / private ranges)
//
// DNS resolution is performed at validation time; this is a defense-in-depth
// measure against user-supplied provider URLs reaching internal services.
// It is not a complete SSRF mitigation (DNS rebinding windows remain) — for
// full coverage, egress network policy should also be enforced at the
// deployment level.
func ValidateProviderBaseURL(raw string) error {
	if raw == "" {
		return nil // empty BaseURL falls back to platform default elsewhere
	}
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("invalid base URL: %w", err)
	}
	if !u.IsAbs() {
		return fmt.Errorf("base URL must be absolute")
	}
	scheme := strings.ToLower(u.Scheme)
	if scheme != "http" && scheme != "https" {
		return fmt.Errorf("base URL scheme must be http or https")
	}
	host := u.Hostname()
	if host == "" {
		return fmt.Errorf("base URL must have a host")
	}
	// Reject obvious literals before DNS lookup.
	if isBlockedHostLiteral(host) {
		return fmt.Errorf("base URL host is not allowed")
	}
	// Resolve and check all A records.
	ips, err := net.LookupIP(host)
	if err != nil {
		return fmt.Errorf("cannot resolve base URL host: %w", err)
	}
	for _, ip := range ips {
		if !isPublicIP(ip) {
			return fmt.Errorf("base URL host resolves to non-public IP %s", ip.String())
		}
	}
	return nil
}

// isBlockedHostLiteral catches names that won't pass DNS but should still be
// rejected explicitly (and faster).
func isBlockedHostLiteral(host string) bool {
	lower := strings.ToLower(host)
	switch lower {
	case "localhost", "ip6-localhost", "ip6-loopback":
		return true
	}
	if strings.HasSuffix(lower, ".localhost") {
		return true
	}
	// Bare IPv4 / IPv6 literals: defer to isPublicIP via parsing.
	if net.ParseIP(host) != nil {
		ip := net.ParseIP(host)
		return !isPublicIP(ip)
	}
	return false
}

func isPublicIP(ip net.IP) bool {
	if ip == nil {
		return false
	}
	if ip.IsLoopback() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() ||
		ip.IsInterfaceLocalMulticast() || ip.IsPrivate() || ip.IsUnspecified() {
		return false
	}
	return true
}
