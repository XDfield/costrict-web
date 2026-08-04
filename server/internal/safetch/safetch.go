// Package safetch provides SSRF-safe HTTP client construction.
//
// ValidateURL performs write-time validation: scheme allow-list, host-literal
// blocking, and DNS resolution check against all A records. Use it at config
// ingest time to reject internal/loopback/private destinations early.
//
// NewClient returns an *http.Client whose Transport.DialContext re-resolves
// and re-checks the resolved IP at the moment of dialing — closing the
// DNS-rebinding window that a pure write-time check leaves open — and whose
// CheckRedirect re-runs ValidateURL on every redirect hop.
//
// These guards are defense-in-depth; deployment-level egress policy is still
// recommended for full coverage.
package safetch

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Options controls NewClient behavior. Zero-value fields are replaced with
// sensible defaults (Timeout=30s, MaxRedirects=3).
type Options struct {
	// Timeout is the end-to-end client timeout. Defaults to 30s.
	Timeout time.Duration
	// MaxRedirects caps the number of followed redirects. 0 → 3. Negative →
	// redirects disabled (first response returned as-is).
	MaxRedirects int
}

// ValidateURL enforces that raw is safe for the server to fetch.
//
//   - must be non-empty and parse as an absolute URL
//   - scheme must be http or https
//   - host must be present, must not be a blocked literal (localhost / ::1 /
//     .localhost / any non-public IP literal)
//   - host must resolve (via net.LookupIP) only to public IPs
//
// Returns nil for URLs that pass all checks. DNS resolution is performed at
// validation time; for runtime DNS-rebinding protection use NewClient, whose
// DialContext re-checks the resolved IP at dial time.
func ValidateURL(raw string) error {
	if raw == "" {
		return fmt.Errorf("url is required")
	}
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("invalid url: %w", err)
	}
	if !u.IsAbs() {
		return fmt.Errorf("url must be absolute")
	}
	scheme := strings.ToLower(u.Scheme)
	if scheme != "http" && scheme != "https" {
		return fmt.Errorf("url scheme must be http or https")
	}
	host := u.Hostname()
	if host == "" {
		return fmt.Errorf("url must have a host")
	}
	if isBlockedHostLiteral(host) {
		return fmt.Errorf("url host is not allowed")
	}
	ips, err := net.LookupIP(host)
	if err != nil {
		return fmt.Errorf("cannot resolve url host: %w", err)
	}
	for _, ip := range ips {
		if !IsPublicIP(ip) {
			return fmt.Errorf("url host resolves to non-public IP %s", ip.String())
		}
	}
	return nil
}

// IsPublicIP reports whether ip is publicly routable. Returns false for nil,
// loopback, link-local (unicast/multicast), interface-local multicast,
// private (RFC 1918 / RFC 4193), and unspecified addresses.
func IsPublicIP(ip net.IP) bool {
	if ip == nil {
		return false
	}
	if ip.IsLoopback() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() ||
		ip.IsInterfaceLocalMulticast() || ip.IsPrivate() || ip.IsUnspecified() {
		return false
	}
	return true
}

// isBlockedHostLiteral catches names that bypass DNS or are local aliases
// before the (potentially slow) DNS lookup.
func isBlockedHostLiteral(host string) bool {
	lower := strings.ToLower(host)
	switch lower {
	case "localhost", "ip6-localhost", "ip6-loopback":
		return true
	}
	if strings.HasSuffix(lower, ".localhost") {
		return true
	}
	if ip := net.ParseIP(host); ip != nil {
		return !IsPublicIP(ip)
	}
	return false
}

// NewClient returns an *http.Client hardened against SSRF at every stage:
//
//   - CheckRedirect re-validates the target URL of each redirect hop and
//     caps the hop count (default 3).
//   - The custom Transport.DialContext resolves the host once, validates every
//     returned IP, and then dials one of the already-validated IPs directly —
//     so a domain whose A records rotate between validation and dial cannot
//     smuggle in a private address (defeats DNS rebinding / TOCTOU).
//
// Validation failures inside DialContext are returned as errors; clients that
// surface these errors to end users should treat them as opaque.
func NewClient(opts Options) *http.Client {
	timeout := opts.Timeout
	if timeout == 0 {
		timeout = 30 * time.Second
	}
	maxRedirects := opts.MaxRedirects
	if maxRedirects == 0 {
		maxRedirects = 3
	}

	dialer := &net.Dialer{
		Timeout:   10 * time.Second,
		KeepAlive: 30 * time.Second,
	}
	transport := &http.Transport{
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			host, port, err := net.SplitHostPort(addr)
			if err != nil {
				return nil, fmt.Errorf("invalid address %q: %w", addr, err)
			}
			ips, err := net.DefaultResolver.LookupIPAddr(ctx, host)
			if err != nil {
				return nil, fmt.Errorf("cannot resolve %s: %w", host, err)
			}
			for _, resolved := range ips {
				if !IsPublicIP(resolved.IP) {
					return nil, fmt.Errorf("dial blocked: %s resolves to non-public IP %s", host, resolved.IP)
				}
			}
			// Dial only the validated IPs directly so the OS resolver cannot
			// return a different (private) address between our check and the
			// actual connect.
			var lastErr error
			for _, resolved := range ips {
				conn, dErr := dialer.DialContext(ctx, network, net.JoinHostPort(resolved.IP.String(), port))
				if dErr == nil {
					return conn, nil
				}
				lastErr = dErr
			}
			if lastErr == nil {
				lastErr = fmt.Errorf("no reachable public IP for %s", host)
			}
			return nil, lastErr
		},
	}

	client := &http.Client{
		Timeout:   timeout,
		Transport: transport,
	}
	if maxRedirects < 0 {
		client.CheckRedirect = func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		}
	} else {
		client.CheckRedirect = func(req *http.Request, via []*http.Request) error {
			if len(via) >= maxRedirects {
				return fmt.Errorf("too many redirects (max %d)", maxRedirects)
			}
			return ValidateURL(req.URL.String())
		}
	}
	return client
}
