package gateway

import (
	"fmt"
	"net"
	"net/url"
	"strconv"
	"strings"
)

// ValidateInternalGatewayURL enforces that raw is a well-formed internal URL
// suitable for server-to-gateway traffic.
//
// Unlike safetch.ValidateURL, this does NOT require the host to resolve to a
// public IP — gateway InternalURL values are typically cluster-internal
// (e.g. "http://gateway-svc:8081", "http://10.0.0.5:8081") and must remain
// reachable. We block the highest-impact SSRF sinkhole targets that no
// legitimate gateway address would use:
//
//   - localhost / .localhost / loopback name aliases (forced DNS bypass)
//   - link-local addresses (169.254.0.0/16, incl. cloud metadata 169.254.169.254)
//   - unspecified addresses (0.0.0.0, ::)
//   - non-http(s) schemes
//
// The same check is run at gateway registration time and at every reuse of
// a stored InternalURL, so a polluted DB row cannot redirect server traffic
// to a metadata sinkhole. The registration endpoint itself is gated by
// X-Internal-Secret, and closeHTTPClient has redirects disabled, so the
// remaining attack surface (a registered gateway probing other in-cluster
// services) requires already-trusted internal credentials.
func ValidateInternalGatewayURL(raw string) error {
	if raw == "" {
		return fmt.Errorf("internal url is required")
	}
	u, err := url.Parse(raw)
	if err != nil {
		return fmt.Errorf("invalid internal url: %w", err)
	}
	if !u.IsAbs() {
		return fmt.Errorf("internal url must be absolute")
	}
	scheme := strings.ToLower(u.Scheme)
	if scheme != "http" && scheme != "https" {
		return fmt.Errorf("internal url scheme must be http or https")
	}
	host := u.Hostname()
	if host == "" {
		return fmt.Errorf("internal url must have a host")
	}
	if isBlockedInternalHost(host) {
		return fmt.Errorf("internal url host is not allowed: %s", host)
	}
	port := u.Port()
	if port != "" {
		p, err := net.LookupPort("tcp", port)
		if err != nil || p <= 0 || p > 65535 {
			return fmt.Errorf("internal url port is invalid: %s", port)
		}
	}
	return nil
}

// isBlockedInternalHost rejects localhost name aliases, link-local addresses,
// unspecified addresses, and the cloud-metadata sinkhole. Loopback IPs
// (127.0.0.0/8, ::1) and private RFC1918 ranges are intentionally ALLOWED —
// internal gateway addresses commonly use them, and the gateway register
// endpoint is already gated by X-Internal-Secret.
func isBlockedInternalHost(host string) bool {
	lower := strings.ToLower(host)
	switch lower {
	case "localhost", "ip6-localhost", "ip6-loopback":
		return true
	}
	if strings.HasSuffix(lower, ".localhost") {
		return true
	}
	if ip := net.ParseIP(host); ip != nil {
		if isBlockedInternalIP(ip) {
			return true
		}
	}
	// Defense against non-canonical IPv4 encodings (inet_aton-style: hex 0x,
	// octal 0, decimal single-int, and multi-component forms like "127.1").
	// net.ParseIP rejects these forms, so without this canonicalization step
	// a host like "0xa9fea9fe" or "0xa9.0xfe.0xa9.0xfe" (both = 169.254.169.254
	// cloud metadata) would slip past the IP-class checks and be treated as a
	// plain hostname — exploitable on deployments using a libc resolver (cgo
	// build with getaddrinfo/inet_aton) that expands these forms to metadata
	// / link-local / unspecified addresses. Note: loopback encodings like
	// "0x7f000001" (= 127.0.0.1) remain ALLOWED because loopback is
	// intentionally permitted for cluster-internal gateways. secreport
	// 20260731141243580377 (CVMS 5.1).
	if canon := canonicalizeInternalIPv4Host(host); canon != "" {
		if ip := net.ParseIP(canon); ip != nil {
			if isBlockedInternalIP(ip) {
				return true
			}
		}
	}
	return false
}

// isBlockedInternalIP classifies a parsed IP against the gateway-internal
// sinkhole set. Loopback and RFC1918 are intentionally allowed.
func isBlockedInternalIP(ip net.IP) bool {
	if ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() ||
		ip.IsInterfaceLocalMulticast() || ip.IsUnspecified() {
		return true
	}
	// Explicitly block the cloud metadata sinkhole even though
	// IsLinkLocalUnicast already covers 169.254.0.0/16 — keep the explicit
	// check so the intent is obvious to future readers.
	if ip.Equal(net.IPv4(169, 254, 169, 254)) {
		return true
	}
	return false
}

// canonicalizeInternalIPv4Host mirrors glibc inet_aton semantics for the
// gateway-internal validator. See services.canonicalizeIPv4Host for the
// full rationale. Duplicated here because Go forbids cross-package use of
// unexported helpers, and lifting both into a shared package would require
// a wider refactor than this SSRF defense warrants.
func canonicalizeInternalIPv4Host(host string) string {
	for i := 0; i < len(host); i++ {
		c := host[i]
		if !((c >= '0' && c <= '9') || c == '.' || c == 'x' || c == 'X' ||
			c == 'o' || c == 'O' || c == 'b' || c == 'B' ||
			(c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')) {
			return ""
		}
	}
	parts := strings.Split(host, ".")
	if len(parts) == 0 || len(parts) > 4 {
		return ""
	}
	vals := make([]uint32, len(parts))
	for i, p := range parts {
		if p == "" {
			return ""
		}
		v, err := strconv.ParseUint(p, 0, 32)
		if err != nil {
			return ""
		}
		vals[i] = uint32(v)
	}
	var ip uint32
	switch len(vals) {
	case 1:
		ip = vals[0]
	case 2:
		if vals[0] > 0xff || vals[1] > 0xffffff {
			return ""
		}
		ip = (vals[0] << 24) | vals[1]
	case 3:
		if vals[0] > 0xff || vals[1] > 0xff || vals[2] > 0xffff {
			return ""
		}
		ip = (vals[0] << 24) | (vals[1] << 16) | vals[2]
	case 4:
		for _, v := range vals {
			if v > 0xff {
				return ""
			}
		}
		ip = (vals[0] << 24) | (vals[1] << 16) | (vals[2] << 8) | vals[3]
	}
	return fmt.Sprintf("%d.%d.%d.%d", byte(ip>>24), byte(ip>>16), byte(ip>>8), byte(ip))
}
