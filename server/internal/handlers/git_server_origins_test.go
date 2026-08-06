package handlers

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/costrict/costrict-web/server/internal/database"
	"github.com/gin-gonic/gin"
)

func newTrustedOriginsRouter() *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.GET("/api/git-servers/trusted-origins", ListTrustedGitOrigins)
	return r
}

// seedGitServersTable creates the table the read path needs. The other Git
// tests get it from setupGitContentFixture, which also stands up a fake Gitea;
// this endpoint never talks to one, so it gets the table alone.
func seedGitServersTable(t *testing.T) {
	t.Helper()
	if err := database.GetDB().Exec(`CREATE TABLE IF NOT EXISTS git_servers (
		server_id    TEXT PRIMARY KEY,
		kind         TEXT NOT NULL,
		endpoint     TEXT NOT NULL,
		display_name TEXT NOT NULL,
		config       TEXT NOT NULL DEFAULT '{}',
		is_template  INTEGER NOT NULL DEFAULT 0,
		enabled      INTEGER NOT NULL DEFAULT 1,
		created_at   DATETIME NOT NULL,
		updated_at   DATETIME NOT NULL
	)`).Error; err != nil {
		t.Fatalf("create git_servers: %v", err)
	}
}

func seedGitServer(t *testing.T, serverID, endpoint, config string, enabled bool) {
	t.Helper()
	flag := 0
	if enabled {
		flag = 1
	}
	if err := database.GetDB().Exec(
		`INSERT INTO git_servers (server_id, kind, endpoint, display_name, config, is_template, enabled, created_at, updated_at)
		 VALUES (?, 'gitea', ?, ?, ?, 0, ?, CURRENT_TIMESTAMP, CURRENT_TIMESTAMP)`,
		serverID, endpoint, serverID, config, flag).Error; err != nil {
		t.Fatalf("seed git server %s: %v", serverID, err)
	}
}

func fetchTrustedOrigins(t *testing.T) []string {
	t.Helper()
	w := httptest.NewRecorder()
	req, _ := http.NewRequest(http.MethodGet, "/api/git-servers/trusted-origins", nil)
	newTrustedOriginsRouter().ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}
	var body struct {
		Origins *[]string `json:"origins"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v (%s)", err, w.Body.String())
	}
	if body.Origins == nil {
		// A missing field reads as "no allowlist configured" on the client and
		// silently restores the protocol-only behaviour this endpoint exists to
		// replace, so the difference from an empty array is load-bearing.
		t.Fatalf("origins was absent or null, which the client reads as 'no allowlist': %s", w.Body.String())
	}
	return *body.Origins
}

// The browser-facing address wins over the cluster-internal API endpoint, and
// only the origin is served. This is the whole point: `endpoint` on a split
// deployment names a host no browser can resolve.
func TestTrustedGitOrigins_PrefersWebURLAndReturnsOriginOnly(t *testing.T) {
	defer setupTestDB(t)()
	seedGitServersTable(t)
	seedGitServer(t, "gs-split", "http://gitea.costrict.svc.cluster.local:3000",
		`{"admin_token":"tok","web_url":"https://gitea.costrict.ai/some/path?x=1#frag"}`, true)

	origins := fetchTrustedOrigins(t)
	if len(origins) != 1 || origins[0] != "https://gitea.costrict.ai" {
		t.Fatalf("expected the web origin only, got %v", origins)
	}
	for _, origin := range origins {
		if origin == "http://gitea.costrict.svc.cluster.local:3000" {
			t.Fatal("the cluster-internal API endpoint reached the browser")
		}
	}
}

// Single-address deployments (local dev, and any install where the API host is
// the browser host) have no web_url, and source_repo_url is then built from the
// endpoint. Refusing to list it would make the allowlist narrower than the set
// of origins this server itself wrote — every repository link would go dead.
func TestTrustedGitOrigins_FallsBackToEndpointWhenNoWebURL(t *testing.T) {
	defer setupTestDB(t)()
	seedGitServersTable(t)
	seedGitServer(t, "gs-single", "http://localhost:3001/", `{"admin_token":"tok"}`, true)

	origins := fetchTrustedOrigins(t)
	if len(origins) != 1 || origins[0] != "http://localhost:3001" {
		t.Fatalf("expected the endpoint origin, got %v", origins)
	}
}

// Disabled is the operator's "stop using this server"; gitserver.ResolveByServerID
// already refuses it, so items bound to it cannot have their content read
// either. Trusting its origin would also mean trusting a decommissioned host
// somebody else may have since registered.
func TestTrustedGitOrigins_ExcludesDisabledServers(t *testing.T) {
	defer setupTestDB(t)()
	seedGitServersTable(t)
	seedGitServer(t, "gs-live", "https://live.example.test", `{"admin_token":"tok"}`, true)
	seedGitServer(t, "gs-drained", "https://drained.example.test", `{"admin_token":"tok"}`, false)

	origins := fetchTrustedOrigins(t)
	if len(origins) != 1 || origins[0] != "https://live.example.test" {
		t.Fatalf("expected the enabled server only, got %v", origins)
	}
}

// A server with no admin token is unusable for writes but its origin is still
// ours, and items already point at it. Trust and write-capability are different
// questions; conflating them would blank the links on a mis-seeded row.
func TestTrustedGitOrigins_IncludesServerWithoutAdminToken(t *testing.T) {
	defer setupTestDB(t)()
	seedGitServersTable(t)
	seedGitServer(t, "gs-tokenless", "https://tokenless.example.test", `{}`, true)

	origins := fetchTrustedOrigins(t)
	if len(origins) != 1 || origins[0] != "https://tokenless.example.test" {
		t.Fatalf("expected the tokenless server's origin, got %v", origins)
	}
}

// Rows we cannot make a truthful claim about are skipped rather than guessed
// at: an unparseable config hides web_url, and a non-http(s) address names no
// place a browser may be sent. Both fail closed, and neither takes the healthy
// server down with it.
func TestTrustedGitOrigins_SkipsUnreadableAndUnusableServers(t *testing.T) {
	defer setupTestDB(t)()
	seedGitServersTable(t)
	seedGitServer(t, "gs-a-broken-config", "https://broken.example.test", `{not json`, true)
	seedGitServer(t, "gs-b-ssh", "ssh://git@ssh.example.test", `{"admin_token":"tok"}`, true)
	seedGitServer(t, "gs-c-schemeless", "gitea.example.test", `{"admin_token":"tok"}`, true)
	seedGitServer(t, "gs-d-empty", "", `{"admin_token":"tok"}`, true)
	seedGitServer(t, "gs-e-javascript", "javascript:alert(1)", `{"admin_token":"tok"}`, true)
	seedGitServer(t, "gs-f-good", "https://good.example.test", `{"admin_token":"tok"}`, true)

	origins := fetchTrustedOrigins(t)
	if len(origins) != 1 || origins[0] != "https://good.example.test" {
		t.Fatalf("expected only the usable server, got %v", origins)
	}
}

// Two rows may point at one Gitea (different admin tokens, one host). The
// allowlist is a set.
func TestTrustedGitOrigins_DeduplicatesSharedHosts(t *testing.T) {
	defer setupTestDB(t)()
	seedGitServersTable(t)
	seedGitServer(t, "gs-one", "https://shared.example.test", `{"admin_token":"a"}`, true)
	seedGitServer(t, "gs-two", "https://ignored.example.test",
		`{"admin_token":"b","web_url":"https://shared.example.test/"}`, true)

	origins := fetchTrustedOrigins(t)
	if len(origins) != 1 || origins[0] != "https://shared.example.test" {
		t.Fatalf("expected one deduplicated origin, got %v", origins)
	}
}

// No Git servers is a real answer, and it must arrive as [] rather than null:
// the client treats a configured empty list as "trust nothing" and a missing
// list as "no allowlist", which are opposite stances.
func TestTrustedGitOrigins_EmptyDeploymentReturnsEmptyArray(t *testing.T) {
	defer setupTestDB(t)()
	seedGitServersTable(t)

	if origins := fetchTrustedOrigins(t); len(origins) != 0 {
		t.Fatalf("expected no origins, got %v", origins)
	}
}

// The response carries nothing but origins. Secrets live in the same row, one
// column away, so this asserts on the serialized bytes rather than the struct.
func TestTrustedGitOrigins_LeaksNoCredentialsOrServerIdentity(t *testing.T) {
	defer setupTestDB(t)()
	seedGitServersTable(t)
	seedGitServer(t, "gs-secretive", "http://gitea.internal.svc:3000",
		`{"admin_token":"SECRET-ADMIN-TOKEN","webhook_secret":"SECRET-HOOK",`+
			`"internal_token":"SECRET-INTERNAL","admin_password":"SECRET-PASSWORD",`+
			`"web_url":"https://public.example.test"}`, true)

	w := httptest.NewRecorder()
	req, _ := http.NewRequest(http.MethodGet, "/api/git-servers/trusted-origins", nil)
	newTrustedOriginsRouter().ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	raw := w.Body.String()
	for _, forbidden := range []string{
		"SECRET-ADMIN-TOKEN", "SECRET-HOOK", "SECRET-INTERNAL", "SECRET-PASSWORD",
		"admin_token", "webhook_secret", "internal_token", "admin_password",
		"gs-secretive",                // server identity
		"gitea.internal.svc",          // cluster-internal API host
		"display_name", "is_template", // row shape
	} {
		if strings.Contains(raw, forbidden) {
			t.Fatalf("response leaked %q: %s", forbidden, raw)
		}
	}
	if !strings.Contains(raw, "https://public.example.test") {
		t.Fatalf("response lost the origin it exists to serve: %s", raw)
	}
}
