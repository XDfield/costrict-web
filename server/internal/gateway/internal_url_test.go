package gateway

import "testing"

func TestValidateInternalGatewayURL(t *testing.T) {
	cases := []struct {
		name    string
		raw     string
		wantErr bool
	}{
		{"empty", "", true},
		{"not absolute", "gateway-svc:8081", true},
		{"ftp scheme", "ftp://gateway-svc:8081", true},
		{"javascript scheme", "javascript:alert(1)", true},
		{"missing host", "http://", true},
		{"localhost literal", "http://localhost:8081", true},
		{"localhost suffix", "http://attacker.localhost:8081", true},
		{"ip6 loopback name", "http://ip6-loopback:8081", true},
		// Loopback IPs are intentionally ALLOWED: internal gateway addresses
		// commonly use them (httptest.NewServer defaults to 127.0.0.1), and
		// the gateway register endpoint is already gated by X-Internal-Secret.
		{"loopback ipv4", "http://127.0.0.1:8081", false},
		{"loopback ipv6", "http://[::1]:8081", false},
		{"link-local metadata", "http://169.254.169.254/latest/meta-data/", true},
		{"link-local range", "http://169.254.1.1:8081", true},
		{"unspecified ipv4", "http://0.0.0.0:8081", true},
		{"invalid port", "http://gateway-svc:99999", true},
		{"valid cluster internal hostname", "http://gateway-svc:8081", false},
		{"valid internal rfc1918", "http://10.0.0.5:8081", false},
		{"valid https internal", "https://gateway.svc.cluster.local:8443", false},
		{"valid default port", "http://gateway-svc", false},
		// Non-canonical IPv4 encodings of BLOCKED addresses (inet_aton-style).
		// Loopback encodings are intentionally ALLOWED (see isBlockedInternalHost
		// doc) so we don't assert on them here; we focus on the metadata and
		// unspecified encodings that MUST be rejected.
		{"hex metadata single int", "http://0xa9fea9fe:8081", true},
		{"decimal metadata single int", "http://2852039166:8081", true},
		{"hex metadata dotted", "http://0xa9.0xfe.0xa9.0xfe:8081", true},
		{"octal metadata dotted", "http://0251.0376.0251.0376:8081", true},
		{"short metadata 2-comp", "http://169.16689662:8081", true},
		// Non-canonical loopback encoding remains ALLOWED (per documented stance
		// that loopback is permitted for cluster-internal gateways).
		{"hex loopback single int allowed", "http://0x7f000001:8081", false},
		// Negative control: hostname that starts with digits but contains
		// non-IP-literal characters must NOT be flagged.
		{"digit-prefixed hostname allowed", "http://gw01.internal.svc:8081", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateInternalGatewayURL(tc.raw)
			if tc.wantErr && err == nil {
				t.Fatalf("expected error for %q, got nil", tc.raw)
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("unexpected error for %q: %v", tc.raw, err)
			}
		})
	}
}
