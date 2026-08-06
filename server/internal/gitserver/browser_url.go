package gitserver

import (
	"fmt"
	"net/url"
	"strings"

	"github.com/costrict/costrict-web/server/internal/models"
)

// A Git server has two addresses and they are not interchangeable.
//
//	Endpoint       — the API address. On a split internal/external deployment
//	                 this is a cluster-internal host (http://gitea.costrict:3000)
//	                 that no browser can resolve.
//	config.web_url — the browser-facing address, when the deployment has one.
//
// Every repository coordinate this system hands a user — capability_items.
// source_repo_url, the fork response's RepoURL, the provision response's — is
// built by prefixing one of them onto "<owner>/<repo>", with web_url winning
// and endpoint as the single-address fallback (local dev, and any deployment
// where the two are the same host).
//
// That precedence used to be written out three separate times: firstGitURL in
// two services call sites and gitWebBase in the handlers. Three copies of a
// rule is survivable while nothing else depends on it, but the browser-side
// trusted-origin allowlist does: it answers "is this URL one of our Git
// servers?" by comparing against exactly this derivation. If the allowlist
// computed the base one way and the coordinate writer another, the two would
// disagree for whichever deployment shape the copies drifted on, and the
// symptom would be every legitimate repository link on that server silently
// degrading to "not a link we can open". So the rule gets one definition and
// every caller — writer and allowlist alike — goes through it.

// BrowserBaseURL resolves the base URL a browser must use to reach a Git
// server's repositories: config.web_url when set, otherwise the API endpoint.
// Trailing slashes are stripped so callers can concatenate "/owner/repo"
// without producing a "//" that Gitea resolves elsewhere.
//
// Returns "" when neither address is set, which callers must treat as "no
// usable coordinate" rather than as a relative URL.
func BrowserBaseURL(webURL, endpoint string) string {
	base := strings.TrimSpace(webURL)
	if base == "" {
		base = strings.TrimSpace(endpoint)
	}
	return strings.TrimRight(base, "/")
}

// BrowserBaseURL is the resolved-config form, for callers that already hold a
// Config (fork, provision, discovery, sync).
func (c *Config) BrowserBaseURL() string {
	if c == nil {
		return ""
	}
	return BrowserBaseURL(c.WebURL, c.Endpoint)
}

// ServerBrowserBaseURL is the raw-row form, for callers that must not require
// the server to be usable for writes.
//
// It deliberately does NOT go through Resolve/ResolveByServerID: those reject
// a server whose admin_token is empty, which is the right answer for "may I
// mint a token against this server?" and the wrong one for "is this address
// one of ours?". A row with no admin token still has coordinates pointing at
// it in capability_items, and refusing to recognise its own origin would break
// exactly those links.
//
// A config blob that will not parse is a different matter and is refused: we
// cannot read web_url out of it, so falling back to Endpoint would claim the
// API address is browser-facing without ever having checked. ErrConfigMalformed
// is returned so the caller can skip the row and say why.
//
// In production that branch is unreachable and is kept only as a backstop:
// git_servers.config is `jsonb`, and PostgreSQL rejects invalid JSON at the
// INSERT (asserted in TestTrustedGitOrigins_ProjectsRealPostgresJSONBConfig).
// It does fire on any engine that stores the column as text, which includes
// the SQLite test suites.
func ServerBrowserBaseURL(gs models.GitServer) (string, error) {
	cfg, err := parseConfig(gs.Config)
	if err != nil {
		return "", fmt.Errorf("%w: server=%s", ErrConfigMalformed, gs.ServerID)
	}
	return BrowserBaseURL(cfg.WebURL, gs.Endpoint), nil
}

// BrowserOrigin reduces a base URL to the origin a browser compares against:
// scheme + host + port, with path, query, fragment and any user:password@
// userinfo dropped.
//
// The second return is false for anything that is not an http(s) URL with a
// host. web_url is operator-written free text inside a JSONB blob, so "" ,
// "gitea.example.com" (no scheme, which url.Parse happily reads as a *path*),
// and "ssh://git@host" all reach here, and none of them names a place a
// browser may be sent.
func BrowserOrigin(baseURL string) (string, bool) {
	trimmed := strings.TrimSpace(baseURL)
	if trimmed == "" {
		return "", false
	}
	parsed, err := url.Parse(trimmed)
	if err != nil {
		return "", false
	}
	scheme := strings.ToLower(parsed.Scheme)
	if scheme != "http" && scheme != "https" {
		return "", false
	}
	if parsed.Host == "" {
		return "", false
	}
	// Lowercase scheme AND host, because that is what the WHATWG URL parser
	// every browser client compares with does. Without it "HTTPS://Gitea.X"
	// and "https://gitea.x" are two entries in a set that should hold one, and
	// a consumer comparing raw strings instead of re-parsing would miss the
	// match outright. Safe for ports and for bracketed IPv6 literals, both of
	// which are case-insensitive.
	return scheme + "://" + strings.ToLower(parsed.Host), true
}
