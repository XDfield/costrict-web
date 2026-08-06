package gitserver

import (
	"errors"
	"testing"

	"github.com/costrict/costrict-web/server/internal/models"
)

func TestBrowserBaseURL_PrecedenceAndTrimming(t *testing.T) {
	cases := []struct {
		name     string
		webURL   string
		endpoint string
		want     string
	}{
		{"web url wins", "https://gitea.example.test", "http://internal:3000", "https://gitea.example.test"},
		{"endpoint is the single-address fallback", "", "http://localhost:3001", "http://localhost:3001"},
		{"blank web url is not a web url", "   ", "http://localhost:3001", "http://localhost:3001"},
		{"trailing slashes never survive", "https://gitea.example.test///", "", "https://gitea.example.test"},
		{"endpoint trailing slash too", "", "http://localhost:3001/", "http://localhost:3001"},
		{"neither configured", "", "", ""},
		// A subpath install is preserved here; only BrowserOrigin drops it.
		{"subpath install", "https://example.test/gitea/", "", "https://example.test/gitea"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := BrowserBaseURL(tc.webURL, tc.endpoint); got != tc.want {
				t.Fatalf("BrowserBaseURL(%q, %q) = %q, want %q", tc.webURL, tc.endpoint, got, tc.want)
			}
		})
	}
}

// The Config method and the free function must not drift: the coordinate
// writers use the former and the allowlist uses the latter, and a disagreement
// between them is invisible until every repository link on some deployment
// shape goes dead.
func TestConfigBrowserBaseURL_MatchesTheFreeFunction(t *testing.T) {
	cfg := &Config{WebURL: "https://web.example.test/", Endpoint: "http://api.internal:3000"}
	if got, want := cfg.BrowserBaseURL(), BrowserBaseURL(cfg.WebURL, cfg.Endpoint); got != want {
		t.Fatalf("Config method = %q, free function = %q", got, want)
	}
	var nilCfg *Config
	if got := nilCfg.BrowserBaseURL(); got != "" {
		t.Fatalf("nil Config should yield no coordinate, got %q", got)
	}
}

func TestServerBrowserBaseURL_ReadsWebURLWithoutRequiringAnAdminToken(t *testing.T) {
	// No admin_token: ResolveByServerID would refuse this row, but "may I mint
	// a token against it" is a different question from "is this address ours".
	base, err := ServerBrowserBaseURL(models.GitServer{
		ServerID: "gs-1", Endpoint: "http://api.internal:3000",
		Config: `{"web_url":"https://web.example.test"}`,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if base != "https://web.example.test" {
		t.Fatalf("got %q", base)
	}
}

func TestServerBrowserBaseURL_EmptyConfigFallsBackToEndpoint(t *testing.T) {
	for _, config := range []string{"", "{}", `{"admin_token":"tok"}`} {
		base, err := ServerBrowserBaseURL(models.GitServer{
			ServerID: "gs-1", Endpoint: "http://localhost:3001", Config: config,
		})
		if err != nil {
			t.Fatalf("config %q: unexpected error: %v", config, err)
		}
		if base != "http://localhost:3001" {
			t.Fatalf("config %q: got %q", config, base)
		}
	}
}

// An unreadable config hides web_url. Falling back to Endpoint would assert the
// API address is browser-facing without ever having checked, so the row is
// refused instead.
func TestServerBrowserBaseURL_MalformedConfigIsRefusedNotGuessed(t *testing.T) {
	_, err := ServerBrowserBaseURL(models.GitServer{
		ServerID: "gs-1", Endpoint: "http://api.internal:3000", Config: `{not json`,
	})
	if !errors.Is(err, ErrConfigMalformed) {
		t.Fatalf("expected ErrConfigMalformed, got %v", err)
	}
}

func TestBrowserOrigin(t *testing.T) {
	cases := []struct {
		in   string
		want string
		ok   bool
	}{
		{"https://gitea.example.test", "https://gitea.example.test", true},
		{"http://localhost:3001", "http://localhost:3001", true},
		{"https://gitea.example.test:8443/sub/path?q=1#f", "https://gitea.example.test:8443", true},
		// Userinfo has no business in a rendered link and is dropped with the
		// rest of the URL's non-origin parts.
		{"https://user:pw@gitea.example.test/x", "https://gitea.example.test", true},
		// Case-folded the way every browser's URL parser folds it, so the set
		// cannot hold one host twice and a raw string comparison still matches.
		{"HTTPS://Gitea.Example.Test", "https://gitea.example.test", true},
		{"https://[2001:DB8::1]:3000/x", "https://[2001:db8::1]:3000", true},
		{"", "", false},
		{"   ", "", false},
		// url.Parse reads a schemeless string as a path, so this has no host.
		{"gitea.example.test", "", false},
		{"ssh://git@gitea.example.test", "", false},
		{"git://gitea.example.test/x", "", false},
		{"file:///etc/passwd", "", false},
		{"javascript:alert(1)//gitea.example.test/x", "", false},
		{"//gitea.example.test/x", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			got, ok := BrowserOrigin(tc.in)
			if ok != tc.ok || got != tc.want {
				t.Fatalf("BrowserOrigin(%q) = (%q, %v), want (%q, %v)", tc.in, got, ok, tc.want, tc.ok)
			}
		})
	}
}
