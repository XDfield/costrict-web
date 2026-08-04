package safetch

import (
	"net"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestValidateURL(t *testing.T) {
	cases := []struct {
		name    string
		url     string
		wantErr bool
	}{
		{"empty", "", true},
		{"public https", "https://api.openai.com/v1", false},
		{"public http", "http://api.openai.com/v1", false},
		{"loopback literal", "http://127.0.0.1:8080", true},
		{"loopback v6", "http://[::1]:8080", true},
		{"localhost name", "http://localhost:8080", true},
		{"localhost subdomain", "http://api.localhost", true},
		{"link-local", "http://169.254.169.254/latest", true},
		{"private 10.x", "http://10.0.0.1", true},
		{"private 192.168", "http://192.168.1.1", true},
		{"private 172.16", "http://172.16.0.1", true},
		{"bad scheme", "ftp://example.com", true},
		{"non-absolute", "/v1/api", true},
		{"no host", "http://", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateURL(tc.url)
			if tc.wantErr && err == nil {
				t.Errorf("expected error for %q, got nil", tc.url)
			}
			if !tc.wantErr && err != nil {
				t.Errorf("unexpected error for %q: %v", tc.url, err)
			}
		})
	}
}

func TestIsPublicIP(t *testing.T) {
	cases := []struct {
		ip   string
		want bool
	}{
		{"8.8.8.8", true},
		{"1.1.1.1", true},
		{"127.0.0.1", false},
		{"::1", false},
		{"169.254.169.254", false},
		{"10.0.0.1", false},
		{"192.168.1.1", false},
		{"172.16.0.1", false},
		{"0.0.0.0", false},
		{"fe80::1", false},  // link-local unicast v6
		{"ff02::1", false},  // link-local multicast v6
		{"fc00::1", false},  // ULA v6 (RFC 4193) — IsPrivate
	}
	for _, tc := range cases {
		t.Run(tc.ip, func(t *testing.T) {
			ip := net.ParseIP(tc.ip)
			if ip == nil {
				t.Fatalf("ParseIP(%q) = nil", tc.ip)
			}
			if got := IsPublicIP(ip); got != tc.want {
				t.Errorf("IsPublicIP(%s) = %v, want %v", tc.ip, got, tc.want)
			}
		})
	}
}

// TestNewClientDialBlocksLoopbackLiteral verifies that the custom DialContext
// rejects a loopback IP literal before any connect attempt — the runtime
// guarantee that closes the DNS-rebinding / write-time-only-check gap.
func TestNewClientDialBlocksLoopbackLiteral(t *testing.T) {
	client := NewClient(Options{Timeout: 3 * time.Second})
	resp, err := client.Get("http://127.0.0.1:1/")
	if err == nil {
		if resp != nil {
			resp.Body.Close()
		}
		t.Fatal("expected dial error for loopback literal, got nil")
	}
	if !strings.Contains(err.Error(), "non-public IP") {
		t.Errorf("expected 'non-public IP' in error, got: %v", err)
	}
}

func TestNewClientDialBlocksPrivateLiteral(t *testing.T) {
	client := NewClient(Options{Timeout: 3 * time.Second})
	for _, u := range []string{
		"http://10.0.0.1/",
		"http://192.168.1.1/",
		"http://172.16.0.1/",
		"http://169.254.169.254/latest/meta-data/",
	} {
		resp, err := client.Get(u)
		if err == nil {
			if resp != nil {
				resp.Body.Close()
			}
			t.Errorf("expected dial error for %s, got nil", u)
			continue
		}
		if !strings.Contains(err.Error(), "non-public IP") {
			t.Errorf("expected 'non-public IP' for %s, got: %v", u, err)
		}
	}
}

// TestNewClientCheckRedirectRejectsBlockedTarget verifies CheckRedirect runs
// ValidateURL on each redirect hop. We override Transport with a fake so the
// loopback redirect URL is judged by CheckRedirect alone (not DialContext).
func TestNewClientCheckRedirectRejectsBlockedTarget(t *testing.T) {
	client := NewClient(Options{Timeout: 3 * time.Second})
	client.Transport = &fakeRoundTripper{
		respond: func(req *http.Request) (*http.Response, error) {
			if req.URL.String() == "http://example.test/start" {
				return &http.Response{
					StatusCode: 302,
					Header:     http.Header{"Location": []string{"http://127.0.0.1:9999/secret"}},
					Body:       http.NoBody,
					Request:    req,
				}, nil
			}
			return &http.Response{StatusCode: 200, Body: http.NoBody, Request: req}, nil
		},
	}

	resp, err := client.Get("http://example.test/start")
	if err == nil {
		if resp != nil {
			resp.Body.Close()
		}
		t.Fatal("expected redirect-to-loopback to be rejected, got nil error")
	}
}

// TestNewClientRedirectsDisabled verifies MaxRedirects<0 returns the redirect
// response to the caller without following.
func TestNewClientRedirectsDisabled(t *testing.T) {
	client := NewClient(Options{Timeout: 3 * time.Second, MaxRedirects: -1})
	client.Transport = &fakeRoundTripper{
		respond: func(req *http.Request) (*http.Response, error) {
			return &http.Response{
				StatusCode: 302,
				Header:     http.Header{"Location": []string{"http://example.test/elsewhere"}},
				Body:       http.NoBody,
				Request:    req,
			}, nil
		},
	}

	resp, err := client.Get("http://example.test/start")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 302 {
		t.Errorf("expected 302 (last response), got %d", resp.StatusCode)
	}
}

type fakeRoundTripper struct {
	respond func(*http.Request) (*http.Response, error)
}

func (f *fakeRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	return f.respond(req)
}

// ensure fakeRoundTripper satisfies http.RoundTripper
var _ http.RoundTripper = (*fakeRoundTripper)(nil)
