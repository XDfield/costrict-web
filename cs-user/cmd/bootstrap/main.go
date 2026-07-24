// cs-user-bootstrap is a single-purpose orchestration binary that mirrors
// scripts/bootstrap-dev-env.sh but ships inside the cs-user Docker image
// so ops can run it via `kubectl exec` without needing bash + sibling
// scripts + jq + curl on the host.
//
// Usage:
//
//	cs-user-bootstrap tenant-stack [flags]   # full 4-step bootstrap of one tenant
//	cs-user-bootstrap help
//
// The `tenant-stack` subcommand orchestrates four steps over HTTP against
// the cs-user and server internal APIs:
//
//  1. cs-user tenant upsert (POST /api/internal/platform/tenants[/{slug}])
//  2. server git_server upsert + tenant binding (POST/PUT /api/internal/git-servers,
//     PUT /api/internal/tenants/{tenant}/git-server)
//  3. cs-user tenant config upload (PUT /api/internal/tenant/config)
//  4. server Casdoor env sanity check (local file scan, no API call)
//
// All flags have environment-variable equivalents with the same names as
// the legacy bash script (CS_USER_INTERNAL_TOKEN, INTERNAL_SECRET,
// DEFAULT_TENANT_SLUG, etc.). Flag value wins; env wins over defaults.
//
// In production the binary runs inside the cs-user container. Cross-service
// HTTP calls target SERVER_BASE_URL (typically the in-cluster server Service
// DNS). The employment mapping YAML is mounted via ConfigMap and supplied
// through --employment-yaml.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/joho/godotenv"
)

const helpText = `cs-user-bootstrap — orchestrate tenant-stack bootstrap from inside the cs-user image

Subcommands:
  tenant-stack   Run the 4-step bootstrap (tenant / git-server / employment config / Casdoor check)
  help           Print this message

Run "cs-user-bootstrap tenant-stack -h" for the full flag set.
`

func main() {
	// Dev convenience: load .env from CWD if present. No-op in production
	// (godotenv.Load returns nil for missing files, existing env wins).
	if err := godotenv.Load(); err != nil && !os.IsNotExist(err) {
		log.Printf("[cs-user-bootstrap] load .env: %v (continuing with process env)", err)
	}

	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, helpText)
		os.Exit(2)
	}

	switch os.Args[1] {
	case "tenant-stack":
		runTenantStack(os.Args[2:])
	case "help", "-h", "--help":
		fmt.Print(helpText)
	default:
		fmt.Fprintf(os.Stderr, "unknown subcommand: %s\n\n%s", os.Args[1], helpText)
		os.Exit(2)
	}
}

// tenantStackFlags holds all knobs for the `tenant-stack` subcommand. Each
// field has an env-var fallback; flag value wins.
type tenantStackFlags struct {
	TenantSlug      string
	TenantDisplay   string
	TenantEdition   string
	CsUserURL       string
	ServerURL       string
	GiteaEndpoint   string
	GiteaDisplay    string
	EmploymentYAML  string
	GiteaAdminToken string

	SkipGitServer  bool
	SkipEmployment bool
	UpdateIfExists bool
	DryRun         bool
}

func runTenantStack(args []string) {
	fs := flag.NewFlagSet("tenant-stack", flag.ExitOnError)
	f := tenantStackFlags{
		TenantSlug:     envOr("DEFAULT_TENANT_SLUG", "default"),
		TenantDisplay:  envOr("DEFAULT_TENANT_DISPLAY", "Default Tenant"),
		TenantEdition:  envOr("DEFAULT_TENANT_EDITION", "enterprise"),
		CsUserURL:      envOr("CS_USER_BASE_URL", "http://localhost:8082"),
		ServerURL:      envOr("SERVER_BASE_URL", "http://localhost:8080"),
		GiteaEndpoint:  envOr("DEFAULT_GITEA_ENDPOINT", "http://127.0.0.1:3001"),
		GiteaDisplay:   "Local Gitea (dev)",
		EmploymentYAML: "",
	}
	fs.StringVar(&f.TenantSlug, "tenant", f.TenantSlug, "default tenant slug")
	fs.StringVar(&f.TenantDisplay, "tenant-display", f.TenantDisplay, "tenant display name")
	fs.StringVar(&f.TenantEdition, "tenant-edition", f.TenantEdition, "free | team | enterprise | on_premise (enterprise needed for IdP mapping)")
	fs.StringVar(&f.CsUserURL, "cs-user-url", f.CsUserURL, "cs-user base URL")
	fs.StringVar(&f.ServerURL, "server-url", f.ServerURL, "server base URL")
	fs.StringVar(&f.GiteaEndpoint, "gitea-endpoint", f.GiteaEndpoint, "Gitea base URL")
	fs.StringVar(&f.GiteaDisplay, "gitea-display", f.GiteaDisplay, "git_server display name")
	fs.StringVar(&f.EmploymentYAML, "employment-yaml", f.EmploymentYAML, "path to employment mapping YAML (required unless --skip-employment)")
	fs.StringVar(&f.GiteaAdminToken, "admin-token", envOr("DEFAULT_GITEA_ADMIN_TOKEN", ""), "Gitea admin token")
	fs.BoolVar(&f.SkipGitServer, "skip-git-server", false, "skip the server-side git_server step")
	fs.BoolVar(&f.SkipEmployment, "skip-employment", false, "skip the employment provider config upload step")
	fs.BoolVar(&f.UpdateIfExists, "update-if-exists", false, "PATCH mutable fields if tenant already exists (default: skip)")
	fs.BoolVar(&f.DryRun, "dry-run", false, "print what would run without invoking HTTP")
	fs.Usage = func() {
		fmt.Fprintf(os.Stderr, "usage: cs-user-bootstrap tenant-stack [flags]\n\n")
		fs.PrintDefaults()
	}
	fs.Parse(args)

	csUserToken := os.Getenv("CS_USER_INTERNAL_TOKEN")
	serverToken := os.Getenv("INTERNAL_SECRET")

	if csUserToken == "" {
		die("CS_USER_INTERNAL_TOKEN not set — source cs-user/.env or export it")
	}
	if !f.SkipGitServer {
		if serverToken == "" {
			die("INTERNAL_SECRET not set — source server/.env or pass --skip-git-server")
		}
		if f.GiteaAdminToken == "" {
			die("DEFAULT_GITEA_ADMIN_TOKEN not set — pass --admin-token or export DEFAULT_GITEA_ADMIN_TOKEN")
		}
	}
	if !f.SkipEmployment {
		if f.EmploymentYAML == "" {
			die("--employment-yaml required (or pass --skip-employment)")
		}
	}

	ctx := &stepCtx{
		f:           &f,
		csUserToken: csUserToken,
		serverToken: serverToken,
		client:      &http.Client{Timeout: 15 * time.Second},
	}

	logStepf := func(num int, total int, format string, a ...any) {
		logf("step %d/%d: %s", num, total, fmt.Sprintf(format, a...))
	}

	const total = 4
	logStepf(1, total, "create tenant slug=%s edition=%s", f.TenantSlug, f.TenantEdition)
	bootstrapTenant(ctx)

	if f.SkipGitServer {
		logStepf(2, total, "SKIPPED (--skip-git-server)")
	} else {
		logStepf(2, total, "upsert git_server endpoint=%s + bind tenant=%s", f.GiteaEndpoint, f.TenantSlug)
		bootstrapGitServer(ctx)
	}

	if f.SkipEmployment {
		logStepf(3, total, "SKIPPED (--skip-employment)")
	} else {
		logStepf(3, total, "upload employment mapping yaml=%s", f.EmploymentYAML)
		uploadEmploymentConfig(ctx)
	}

	logStepf(4, total, "verify Casdoor env in server/.env")
	checkCasdoorEnv(ctx)

	logf("DONE — dev environment bootstrap complete")
	if f.DryRun {
		logf("(dry-run: no API calls were made)")
	}
}

// stepCtx carries everything the per-step helpers need.
type stepCtx struct {
	f           *tenantStackFlags
	csUserToken string
	serverToken string
	client      *http.Client
}

// ---------- step 1: tenant ----------

func bootstrapTenant(ctx *stepCtx) {
	slug := ctx.f.TenantSlug
	url := ctx.f.CsUserURL + "/api/internal/platform/tenants/" + slug

	status, body, err := ctx.do("GET", url, ctx.csUserHeader(), nil)
	if err != nil {
		die("tenant lookup: %v", err)
	}

	switch status {
	case 200:
		tenantID := jsonPath(body, "tenant_id")
		if ctx.f.UpdateIfExists {
			logf("tenant slug=%s exists (id=%s); PATCHing mutable fields (--update-if-exists)", slug, tenantID)
			patchBody := map[string]any{
				"display_name":  ctx.f.TenantDisplay,
				"edition":       ctx.f.TenantEdition,
				"email_domains": []string{},
			}
			s, b, err := ctx.do("PATCH", url, ctx.csUserHeader(), patchBody)
			if err != nil {
				die("tenant PATCH: %v", err)
			}
			if s != 200 {
				die("tenant PATCH failed (HTTP %d): %s", s, string(b))
			}
		} else {
			logf("tenant slug=%s already exists (id=%s); skipping (use --update-if-exists to sync)", slug, tenantID)
		}
		return
	case 404:
		// proceed to create
	default:
		die("tenant lookup failed (HTTP %d): %s", status, string(body))
	}

	createBody := map[string]any{
		"slug":          slug,
		"display_name":  ctx.f.TenantDisplay,
		"edition":       ctx.f.TenantEdition,
		"email_domains": []string{},
	}
	logf("creating tenant slug=%s edition=%s", slug, ctx.f.TenantEdition)
	s, b, err := ctx.do("POST", ctx.f.CsUserURL+"/api/internal/platform/tenants", ctx.csUserHeader(), createBody)
	if err != nil {
		die("tenant POST: %v", err)
	}
	if s != 200 && s != 201 {
		die("tenant POST failed (HTTP %d): %s", s, string(b))
	}
}

// ---------- step 2: git_server + bind ----------

func bootstrapGitServer(ctx *stepCtx) {
	endpoint := strings.TrimRight(ctx.f.GiteaEndpoint, "/")

	// Look up existing git_server by endpoint.
	s, b, err := ctx.do("GET", ctx.f.ServerURL+"/api/internal/git-servers", ctx.serverHeader(), nil)
	if err != nil {
		die("git_server list: %v", err)
	}
	if s != 200 {
		die("git_server list failed (HTTP %d): %s", s, string(b))
	}

	serverID := findGitServerIDByEndpoint(b, endpoint)

	cfg := map[string]any{"admin_token": ctx.f.GiteaAdminToken}

	if serverID == "" {
		createBody := map[string]any{
			"kind":         "gitea",
			"endpoint":     endpoint,
			"display_name": ctx.f.GiteaDisplay,
			"config":       cfg,
		}
		logf("POSTing new git_server kind=gitea endpoint=%s", endpoint)
		s, b, err = ctx.do("POST", ctx.f.ServerURL+"/api/internal/git-servers", ctx.serverHeader(), createBody)
		if err != nil {
			die("git_server POST: %v", err)
		}
		if s != 200 && s != 201 {
			die("git_server POST failed (HTTP %d): %s", s, string(b))
		}
		serverID = jsonPath(b, "server_id")
		if serverID == "" && ctx.f.DryRun {
			serverID = "dry-run"
		}
	} else {
		updateBody := map[string]any{
			"endpoint":     endpoint,
			"display_name": ctx.f.GiteaDisplay,
			"config":       cfg,
		}
		logf("PUTting update to git_server server_id=%s", serverID)
		s, b, err = ctx.do("PUT", ctx.f.ServerURL+"/api/internal/git-servers/"+serverID, ctx.serverHeader(), updateBody)
		if err != nil {
			die("git_server PUT: %v", err)
		}
		if s != 200 {
			die("git_server PUT failed (HTTP %d): %s", s, string(b))
		}
	}

	if serverID == "" {
		die("could not resolve server_id from git_server response")
	}
	fmt.Fprintf(os.Stderr, "server_id=%s\n", serverID)

	// Bind tenant. The dev flow uses slug as the tenant identifier (mirrors
	// the legacy bash script — server's tenant_git_server_binding.tenant_id
	// stores whatever string is supplied; cs-user's Resolver resolves slug
	// → tenant_id at read time).
	bindBody := map[string]any{"git_server_id": serverID}
	bindURL := ctx.f.ServerURL + "/api/internal/tenants/" + ctx.f.TenantSlug + "/git-server"
	logf("binding tenant=%s → git_server=%s", ctx.f.TenantSlug, serverID)
	s, b, err = ctx.do("PUT", bindURL, ctx.serverHeader(), bindBody)
	if err != nil {
		die("tenant bind: %v", err)
	}
	if s != 200 {
		die("tenant bind failed (HTTP %d): %s", s, string(b))
	}
}

// ---------- step 3: employment config ----------

func uploadEmploymentConfig(ctx *stepCtx) {
	yamlBytes, err := os.ReadFile(ctx.f.EmploymentYAML)
	if err != nil {
		die("read employment yaml: %v", err)
	}
	body := map[string]any{
		"config_yaml": string(yamlBytes),
	}
	header := ctx.csUserHeader()
	// X-Tenant-Id carries the tenant scope (cs-user middleware resolves it).
	// Slug works because the Resolver matches tenant_id OR slug.
	header["X-Tenant-Id"] = ctx.f.TenantSlug

	url := ctx.f.CsUserURL + "/api/internal/tenant/config"
	logf("uploading tenant config_yaml tenant=%s (bytes=%d)", ctx.f.TenantSlug, len(yamlBytes))
	s, b, err := ctx.do("PUT", url, header, body)
	if err != nil {
		die("config PUT: %v", err)
	}
	if s != 200 {
		die("config PUT failed (HTTP %d): %s", s, string(b))
	}
}

// ---------- step 4: Casdoor env sanity ----------

func checkCasdoorEnv(ctx *stepCtx) {
	// In-container: server/.env is in a different image, so this is a
	// best-effort scan. When absent we warn and continue (matches bash).
	for _, candidate := range []string{"server/.env", "/etc/cs-user/server.env"} {
		if _, err := os.Stat(candidate); err != nil {
			continue
		}
		scanCasdoorEnv(candidate)
		return
	}
	warnf("server/.env not found at server/.env or /etc/cs-user/server.env — skipping Casdoor check")
	warnf("Casdoor cannot be configured via runtime API; populate server/.env before starting @server:")
	warnf("  CASDOOR_ENDPOINT, CASDOOR_CLIENT_ID, CASDOOR_CLIENT_SECRET, CASDOOR_CALLBACK_URL")
}

func scanCasdoorEnv(path string) {
	data, err := os.ReadFile(path)
	if err != nil {
		warnf("read %s failed: %v — skipping Casdoor check", path, err)
		return
	}
	required := []string{"CASDOOR_ENDPOINT", "CASDOOR_CLIENT_ID", "CASDOOR_CLIENT_SECRET", "CASDOOR_CALLBACK_URL"}
	placeholder := map[string]bool{
		"your-client-id":     true,
		"your-client-secret": true,
	}
	missing := []string{}
	for _, key := range required {
		val := envValueFrom(string(data), key)
		if val == "" || placeholder[val] {
			missing = append(missing, key)
		}
	}
	if len(missing) > 0 {
		warnf("%s has missing/placeholder Casdoor vars: %s", path, strings.Join(missing, " "))
		warnf("Login via Casdoor will fail until these are populated.")
		return
	}
	logf("  %s Casdoor vars look populated", path)
}

// ---------- HTTP plumbing ----------

func (ctx *stepCtx) csUserHeader() map[string]string {
	return map[string]string{
		"X-Internal-Token": ctx.csUserToken,
		"Accept":           "application/json",
		"Content-Type":     "application/json",
	}
}

func (ctx *stepCtx) serverHeader() map[string]string {
	return map[string]string{
		"X-Internal-Secret": ctx.serverToken,
		"Accept":            "application/json",
		"Content-Type":      "application/json",
	}
}

// do executes a single HTTP call. body may be nil for GET. The response
// body is fully buffered (these endpoints return small JSON).
func (ctx *stepCtx) do(method, url string, headers map[string]string, body any) (int, []byte, error) {
	var (
		bodyReader io.Reader
		bodyDesc   = "(no body)"
	)
	if body != nil {
		buf, err := json.Marshal(body)
		if err != nil {
			return 0, nil, fmt.Errorf("marshal body: %w", err)
		}
		// Strip whitespace for the dry-run log line.
		bodyDesc = string(bytes.TrimSpace(buf))
		bodyReader = bytes.NewReader(buf)
	}

	if ctx.f.DryRun {
		logf("[dry-run] %s %s headers=%v body=%s", method, redactURL(url), headers, truncate(bodyDesc, 200))
		return 200, []byte(`{}`), nil
	}

	req, err := http.NewRequestWithContext(context.Background(), method, url, bodyReader)
	if err != nil {
		return 0, nil, err
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := ctx.client.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, respBody, nil
}

// ---------- helpers ----------

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func logf(format string, a ...any) {
	fmt.Fprintf(os.Stderr, "[cs-user-bootstrap] "+format+"\n", a...)
}

func warnf(format string, a ...any) {
	fmt.Fprintf(os.Stderr, "[cs-user-bootstrap] WARN: "+format+"\n", a...)
}

func die(format string, a ...any) {
	fmt.Fprintf(os.Stderr, "[cs-user-bootstrap] ERROR: "+format+"\n", a...)
	os.Exit(1)
}

// jsonPath returns the string value at the given top-level key, or "" if
// missing / not a string. Sufficient for the responses we deal with here
// ({tenant_id: "..."}, {server_id: "..."}); avoids a dep on gjson.
func jsonPath(data []byte, key string) string {
	var m map[string]any
	if err := json.Unmarshal(data, &m); err != nil {
		return ""
	}
	v, ok := m[key].(string)
	if !ok {
		return ""
	}
	return v
}

// findGitServerIDByEndpoint scans the array response from GET
// /api/internal/git-servers for an entry whose endpoint matches.
func findGitServerIDByEndpoint(data []byte, endpoint string) string {
	var arr []map[string]any
	if err := json.Unmarshal(data, &arr); err != nil {
		return ""
	}
	for _, item := range arr {
		if ep, _ := item["endpoint"].(string); ep == endpoint {
			if id, _ := item["server_id"].(string); id != "" {
				return id
			}
		}
	}
	return ""
}

// envValueFrom parses "KEY=VALUE" lines from a .env-style file body and
// returns the trimmed value for key, ignoring comments / blank lines.
func envValueFrom(body, key string) string {
	prefix := key + "="
	for _, line := range strings.Split(body, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if !strings.HasPrefix(line, prefix) {
			continue
		}
		val := strings.TrimPrefix(line, prefix)
		val = strings.Trim(val, `"'`)
		return strings.TrimSpace(val)
	}
	return ""
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// redactURL strips query strings so secrets don't leak into dry-run logs.
func redactURL(s string) string {
	if i := strings.Index(s, "?"); i >= 0 {
		return s[:i] + "?…"
	}
	return s
}
