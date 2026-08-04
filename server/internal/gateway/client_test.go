package gateway

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestProxyRequest_RejectsPoisonedInternalURL covers the defense-in-depth
// chokepoint added to Client.ProxyRequest for CVMS 5.1 (PARTIAL → PASS). The
// four proxy sinks (DeviceProxyHandler, SessionProxyHandler,
// ProxyDeviceSessionRequest, ProxyDeviceSessionRequestRaw) all funnel through
// ProxyRequest, so a single revalidation here defends every externally-
// reachable proxy path against a DB row poisoned after the registration-time
// gate (SQL injection, direct DB write, legacy pre-fix row).
//
// Each subtest MUST fail without ever dialing the network — we assert that
// ProxyRequest returns an error AND that no HTTP request was made to the
// (would-be) target. The targets are intentionally non-routable sinkholes;
// if ProxyRequest tried to dial them the test would hang or error at the
// network layer instead of returning our "invalid gateway internal url"
// sentinel.
func TestProxyRequest_RejectsPoisonedInternalURL(t *testing.T) {
	// Per ValidateInternalGatewayURL's documented stance: loopback IPs
	// (127.0.0.0/8, ::1) and RFC1918 ranges are intentionally ALLOWED because
	// cluster-internal gateways commonly use them and registration is gated
	// by X-Internal-Secret. The blocked set is metadata + link-local +
	// unspecified + localhost name aliases + non-http(s) schemes.
	poisoned := []string{
		"http://169.254.169.254",                  // cloud metadata
		"http://169.254.0.1",                      // link-local
		"http://localhost:8081",                   // localhost literal
		"http://0.0.0.0:8081",                     // unspecified
		"ftp://169.254.169.254/latest/meta-data/", // non-http scheme
		"file:///etc/passwd",                      // file scheme
		"javascript:alert(1)",                     // javascript scheme
	}

	for _, url := range poisoned {
		t.Run(url, func(t *testing.T) {
			client := NewClient("test-secret")

			req := httptest.NewRequest(http.MethodGet, "/foo", nil)
			req.URL.Path = "/foo"
			w := httptest.NewRecorder()

			err := client.ProxyRequest(url, "devX", req, w)
			if err == nil {
				t.Fatalf("expected error for poisoned InternalURL %s, got nil", url)
			}
			if err.Error() != "invalid gateway internal url" {
				t.Fatalf("expected sentinel error %q, got %q", "invalid gateway internal url", err.Error())
			}
			// Critically, no dial should have happened — recorder stays empty
			// because we short-circuit before http.Client.Do.
			if w.Code != http.StatusOK {
				// httptest recorder defaults to 200; any other status means a
				// proxy attempt leaked through (it would have written 502 etc).
				t.Fatalf("expected no response written (recorder stays at default 200), got status %d body %q", w.Code, w.Body.String())
			}
			if w.Body.Len() != 0 {
				t.Fatalf("expected empty response body, got %q", w.Body.String())
			}
		})
	}
}

// TestProxyRequest_AllowedInternalURL_DialsOut confirms the happy path still
// works: a valid (loopback) InternalURL reaches the gateway and the response
// is proxied back. Loopback is intentionally allowed by ValidateInternalGatewayURL
// because cluster-internal gateways commonly bind 127.0.0.1.
func TestProxyRequest_AllowedInternalURL_DialsOut(t *testing.T) {
	called := false
	fakeGw := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.Header().Set("Content-Type", "text/plain")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("ok"))
	}))
	defer fakeGw.Close()

	client := NewClient("test-secret")
	req := httptest.NewRequest(http.MethodGet, "/ping", nil)
	req.URL.Path = "/ping"
	w := httptest.NewRecorder()

	if err := client.ProxyRequest(fakeGw.URL, "devX", req, w); err != nil {
		t.Fatalf("expected proxy success for %s, got error: %v", fakeGw.URL, err)
	}
	if !called {
		t.Fatal("fake gateway should have received the proxied request")
	}
	if w.Code != http.StatusOK || w.Body.String() != "ok" {
		t.Fatalf("expected 200 ok response, got %d %q", w.Code, w.Body.String())
	}
}
