//go:build e2e

// Package teamns e2e tests — opt-in via `-tags=e2e`.
//
// These tests stand up a real gitsync.Service pointed at a locally-running
// Gitea fork (config supplied via env vars) and exercise the team-namespace
// API v1.1 surface end-to-end: CreateTeam → SyncMembers → RotateBot →
// DissolveTeam → EnsureWorkflowRepo. Every test owns its own team_id (UUID)
// and cleans up its Gitea-side org / bot user / repos on exit.
//
// Skipped automatically when env vars are missing. See
// docs/repo-management/E2E_TESTING.md for the runbook.

package teamns

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/costrict/costrict-web/server/internal/crypto"
	"github.com/costrict/costrict-web/server/internal/gitsync"
	"github.com/costrict/costrict-web/server/internal/models"
	"github.com/costrict/costrict-web/server/internal/tenant"
	"github.com/costrict/costrict-web/server/internal/user"
	"github.com/google/uuid"
	"go.uber.org/zap"
	"gorm.io/gorm"
)

// e2eEnv holds the resolved env-derived config. Tests call setupE2E once at
// the top; if any required var is missing, the test skips with a clear reason.
type e2eEnv struct {
	giteaURL   string
	adminToken string
	aesKey     *crypto.AESGCM
	tenantID   string
	userRPCURL string // optional — empty skips UserRef-resolution tests
	userRPCTok string
	db         *gorm.DB
	gs         *gitsync.Service
	httpClient *http.Client
}

func mustEnv(t *testing.T, key string) string {
	t.Helper()
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		t.Skipf("e2e: env var %s not set", key)
	}
	return v
}

// setupE2E returns the env-derived config. The DB is in-memory sqlite per
// test (the team_ns / team_bot_credentials mirror of state; Gitea itself is
// the source of truth for the git-side artifacts we assert against).
func setupE2E(t *testing.T) *e2eEnv {
	t.Helper()
	url := mustEnv(t, "E2E_GITEA_URL")
	tok := mustEnv(t, "E2E_GITEA_TOKEN")
	keyB64 := mustEnv(t, "E2E_BOT_TOKEN_KEY")
	tenantID := os.Getenv("E2E_TENANT_ID")
	if tenantID == "" {
		tenantID = "tenant-e2e"
	}

	key, err := crypto.DecodeBase64Key(keyB64)
	if err != nil {
		t.Fatalf("decode E2E_BOT_TOKEN_KEY: %v", err)
	}
	aes, err := crypto.NewAESGCM(key)
	if err != nil {
		t.Fatalf("aes: %v", err)
	}

	db := setupDB(t)
	gs := newGiteaBackedGitsync(t, url, tok)

	env := &e2eEnv{
		giteaURL:   strings.TrimRight(url, "/"),
		adminToken: tok,
		aesKey:     aes,
		tenantID:   tenantID,
		db:         db,
		gs:         gs,
		httpClient: &http.Client{Timeout: 15 * time.Second},
	}
	if rpcURL := strings.TrimSpace(os.Getenv("E2E_USER_RPC_URL")); rpcURL != "" {
		env.userRPCURL = rpcURL
		env.userRPCTok = os.Getenv("E2E_USER_RPC_TOKEN")
	}
	return env
}

// newGiteaBackedGitsync constructs a real *gitsync.Service whose resolver
// returns env-derived cfg for any tenantID. This bypasses the cs-user RPC
// (which is independently unit-tested) so the e2e suite stays focused on
// server ↔ Gitea integration.
//
// adminUser/adminPassword are required because Gitea's POST /users/{name}/tokens
// endpoint rejects admin PAT auth (reqBasicOrRevProxyAuth in upstream Gitea);
// the test supplies the same admin account the fork was booted with.
func newGiteaBackedGitsync(t *testing.T, giteaURL, adminToken string) *gitsync.Service {
	t.Helper()
	adminUser := strings.TrimSpace(os.Getenv("E2E_GITEA_ADMIN_USER"))
	adminPass := os.Getenv("E2E_GITEA_ADMIN_PASSWORD")
	if adminUser == "" || adminPass == "" {
		t.Skip("e2e: E2E_GITEA_ADMIN_USER / E2E_GITEA_ADMIN_PASSWORD not set (required for token-mint endpoints)")
	}
	cfg := &gitsync.GitServerConfig{
		ServerID:      "gitea-local",
		Kind:          gitsync.GitServerKindGitea,
		Endpoint:      giteaURL,
		AdminToken:    adminToken,
		AdminUser:     adminUser,
		AdminPassword: adminPass,
	}
	resolver := &staticResolver{cfg: cfg}
	return gitsync.NewService(nil, resolver, nil, zap.NewNop())
}

// staticResolver returns the same cfg regardless of tenantID. Production
// wiring uses gitsync.LocalResolver (server.git_servers local DB); e2e uses
// this stub to skip the DB.
type staticResolver struct{ cfg *gitsync.GitServerConfig }

func (s *staticResolver) Resolve(ctx context.Context, tenantID string) (*gitsync.GitServerConfig, error) {
	return s.cfg, nil
}

// newE2EService wires a teamns.Service against the e2e env.
func (e *e2eEnv) newService(t *testing.T) *Service {
	t.Helper()
	return NewService(e.db, e.gs, nil, e.aesKey, zap.NewNop())
}

// withTenant returns a ctx carrying the e2e tenantID.
func (e *e2eEnv) withTenant(ctx context.Context) context.Context {
	return tenant.WithTenantID(ctx, e.tenantID)
}

// freshTeamID returns a fresh UUID for team_id. team_short = first 8 hex chars.
func freshTeamID() string { return uuid.NewString() }

// giteaDelete is a best-effort DELETE used for cleanup. 404 is tolerated
// (already gone). Errors are logged via t.Log so a cleanup failure doesn't
// mask the real assertion failure.
func (e *e2eEnv) giteaDelete(t *testing.T, path string) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodDelete, e.giteaURL+"/api/v1"+path, nil)
	req.Header.Set("Authorization", "token "+e.adminToken)
	resp, err := e.httpClient.Do(req)
	if err != nil {
		t.Logf("cleanup DELETE %s: %v", path, err)
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent && resp.StatusCode != http.StatusNotFound {
		t.Logf("cleanup DELETE %s: status=%d", path, resp.StatusCode)
	}
}

// cleanupTeamGitea deletes the org, the bot user, and any repos under the
// org. Called via t.Cleanup so it runs even on test failure.
//
// Order matters: Gitea refuses to DELETE an org that still has repos, so we
// list and purge repos first. The org DELETE then succeeds, and finally the
// bot user is purged.
func (e *e2eEnv) cleanupTeamGitea(t *testing.T, orgName, botUsername string) {
	t.Helper()
	// 1. List and delete every repo under the org.
	if repos, err := e.giteaListOrgRepos(t, orgName); err == nil {
		for _, name := range repos {
			e.giteaDelete(t, "/repos/"+orgName+"/"+name)
		}
	} else {
		t.Logf("cleanup list repos %s: %v", orgName, err)
	}
	// 2. Delete the org (now empty).
	e.giteaDelete(t, "/orgs/"+orgName)
	// 3. Delete the bot user (must come after org because the user is an
	// owner of the org — Gitea refuses to delete a user owning an org).
	if botUsername != "" {
		e.giteaDelete(t, "/admin/users/"+botUsername)
	}
}

// giteaListOrgRepos returns the repo names owned by the org. Best-effort —
// any error is logged by the caller.
func (e *e2eEnv) giteaListOrgRepos(t *testing.T, orgName string) ([]string, error) {
	t.Helper()
	u := fmt.Sprintf("%s/api/v1/orgs/%s/repos", e.giteaURL, orgName)
	req, _ := http.NewRequest(http.MethodGet, u, nil)
	req.Header.Set("Authorization", "token "+e.adminToken)
	resp, err := e.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("status=%d body=%s", resp.StatusCode, string(body))
	}
	var out []struct {
		Name string `json:"name"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, err
	}
	names := make([]string, 0, len(out))
	for _, r := range out {
		names = append(names, r.Name)
	}
	return names, nil
}

// patWorksAgainstGitea clones a probe repo using the bot PAT. Returns true
// if Gitea accepts the token; false on 401/403.
func (e *e2eEnv) patWorksAgainstGitea(t *testing.T, pat string) bool {
	t.Helper()
	// Hit any authenticated endpoint; /user is cheapest.
	req, _ := http.NewRequest(http.MethodGet, e.giteaURL+"/api/v1/user", nil)
	req.Header.Set("Authorization", "token "+pat)
	resp, err := e.httpClient.Do(req)
	if err != nil {
		t.Logf("pat check: %v", err)
		return false
	}
	defer resp.Body.Close()
	return resp.StatusCode == http.StatusOK
}

// ------------------------------------------------------------------
// Tests
// ------------------------------------------------------------------

// TestE2E_CreateTeam_FullProvisioning drives POST /teams through the real
// Gitea: org t-<short> + bot user bot-t-<short> + PAT. The PAT must
// authenticate against Gitea.
func TestE2E_CreateTeam_FullProvisioning(t *testing.T) {
	env := setupE2E(t)
	teamID := freshTeamID()
	short := teamID[:8]
	orgName := "t-" + short
	botUser := "bot-t-" + short
	env.cleanupTeamGitea(t, orgName, botUser)
	t.Cleanup(func() { env.cleanupTeamGitea(t, orgName, botUser) })

	svc := env.newService(t)
	res, err := svc.CreateTeam(env.withTenant(context.Background()), CreateTeamRequest{
		TeamID:          teamID,
		TeamDisplayName: "E2E Platform Team",
		Creator:         user.UserRef{EmployeeNumber: "E001"},
	})
	if err != nil {
		t.Fatalf("CreateTeam: %v", err)
	}
	if !res.Created.TeamNS || !res.Created.BotAccount || !res.Created.BotToken {
		t.Fatalf("expected all Created flags true: %+v", res.Created)
	}
	if res.Bot == nil || res.Bot.TokenPlaintext == "" {
		t.Fatalf("expected bot plaintext token in response: %+v", res.Bot)
	}
	if !env.patWorksAgainstGitea(t, res.Bot.TokenPlaintext) {
		t.Errorf("bot PAT did not authenticate against Gitea")
	}
}

// TestE2E_RotateBotToken_Gap rotates a bot PAT; the new one must work and
// the old one must 401.
func TestE2E_RotateBotToken_Gap(t *testing.T) {
	env := setupE2E(t)
	teamID := freshTeamID()
	short := teamID[:8]
	orgName := "t-" + short
	botUser := "bot-t-" + short
	env.cleanupTeamGitea(t, orgName, botUser)
	t.Cleanup(func() { env.cleanupTeamGitea(t, orgName, botUser) })

	svc := env.newService(t)
	createRes, err := svc.CreateTeam(env.withTenant(context.Background()), CreateTeamRequest{
		TeamID: teamID, TeamDisplayName: "E2E Rotate Team",
		Creator: user.UserRef{EmployeeNumber: "E001"},
	})
	if err != nil {
		t.Fatalf("CreateTeam: %v", err)
	}
	oldPAT := createRes.Bot.TokenPlaintext

	rotRes, err := svc.RotateBotToken(env.withTenant(context.Background()), teamID, RotateBotTokenRequest{
		Reason: "e2e rotation",
	})
	if err != nil {
		t.Fatalf("RotateBotToken: %v", err)
	}
	newPAT := rotRes.Bot.TokenPlaintext
	if newPAT == "" || newPAT == oldPAT {
		t.Fatalf("expected new distinct PAT, got new=%q old=%q", newPAT, oldPAT)
	}
	if !env.patWorksAgainstGitea(t, newPAT) {
		t.Errorf("new bot PAT did not authenticate against Gitea")
	}
	if env.patWorksAgainstGitea(t, oldPAT) {
		t.Errorf("old PAT still works after rotate (expected revoked)")
	}
}

// TestE2E_DissolveTeam_RevokesBot dissolves a team; bot PAT must 401 after.
func TestE2E_DissolveTeam_RevokesBot(t *testing.T) {
	env := setupE2E(t)
	teamID := freshTeamID()
	short := teamID[:8]
	orgName := "t-" + short
	botUser := "bot-t-" + short
	env.cleanupTeamGitea(t, orgName, botUser)
	t.Cleanup(func() { env.cleanupTeamGitea(t, orgName, botUser) })

	svc := env.newService(t)
	createRes, err := svc.CreateTeam(env.withTenant(context.Background()), CreateTeamRequest{
		TeamID: teamID, TeamDisplayName: "E2E Dissolve Team",
		Creator: user.UserRef{EmployeeNumber: "E001"},
	})
	if err != nil {
		t.Fatalf("CreateTeam: %v", err)
	}
	pat := createRes.Bot.TokenPlaintext

	if _, err := svc.DissolveTeam(env.withTenant(context.Background()), teamID, DissolveTeamRequest{
		Reason: "e2e cleanup",
	}); err != nil {
		t.Fatalf("DissolveTeam: %v", err)
	}
	if env.patWorksAgainstGitea(t, pat) {
		t.Errorf("bot PAT still works after dissolve (expected revoked)")
	}
}

// TestE2E_SyncTeamMembers_DeltaApply adds a real Gitea user (created via
// the admin API for the test) to the team org. Skips — UserRef resolution
// requires the cs-user giteasync path which is out of scope for this
// server-side e2e. Covered by cs-user's own cmd/smoke integration probe.
func TestE2E_SyncTeamMembers_DeltaApply(t *testing.T) {
	t.Skip("e2e: UserRef resolution is covered by cs-user/cmd/smoke; server-side e2e focuses on git provisioning")
}

// TestE2E_EnsureWorkflowRepo_FullPath is the keystone Phase 2.2 test. It
// drives the full workflow/init pipeline against real Gitea:
//
//   - type repo `wf-<slug>` is created (private, default_branch=main)
//   - `definition_snapshot.json` lands on main with caller's content
//   - branch protection: main (no direct push) + inst-* glob
//   - instance branch `inst-<short>` is cut from main HEAD
//   - second call is idempotent (Created flags all false)
//   - drift between caller snapshot and main HEAD → 409
func TestE2E_EnsureWorkflowRepo_FullPath(t *testing.T) {
	env := setupE2E(t)
	teamID := freshTeamID()
	short := teamID[:8]
	orgName := "t-" + short
	botUser := "bot-t-" + short
	env.cleanupTeamGitea(t, orgName, botUser)
	t.Cleanup(func() { env.cleanupTeamGitea(t, orgName, botUser) })

	// Seed team_ns + bot creds directly so EnsureWorkflowRepo has a context.
	// (CreateTeam would also work, but we want to isolate the workflow path.)
	now := time.Now().UTC()
	ns := &models.TeamNamespace{
		TeamID: teamID, TenantID: env.tenantID,
		TeamDisplayName: "E2E Workflow Team",
		TeamNSOrg:       orgName, TeamShort: short, GitServerID: "gitea-local",
		Status: "active", CreatedAt: now, UpdatedAt: now,
	}
	if err := env.db.Create(ns).Error; err != nil {
		t.Fatalf("seed team_ns: %v", err)
	}
	// Bot creds row — required by LookupBotMeta but plaintext is unused by
	// EnsureWorkflowRepo; we set a placeholder encrypted blob.
	enc, _ := env.aesKey.Seal([]byte("placeholder"))
	creds := &models.TeamBotCredentials{
		TeamID: teamID, TenantID: env.tenantID, GitServerID: "gitea-local",
		GiteaUsername: botUser, GiteaUserID: 1, GiteaTokenID: 1,
		TokenEncrypted: enc, TokenSHA256: "sha", CreatedAt: now,
	}
	if err := env.db.Create(creds).Error; err != nil {
		t.Fatalf("seed team_bot_credentials: %v", err)
	}

	// CreateTeam was skipped, so the org doesn't exist yet. EnsureWorkflowRepo
	// creates the repo under org t-<short>, which Gitea requires the org to
	// exist first. Provision the org via gitsync directly.
	if err := env.gs.EnsureOrg(context.Background(), env.tenantID, orgName, "E2E Workflow Team"); err != nil {
		t.Fatalf("EnsureOrg: %v", err)
	}

	svc := env.newService(t)
	defSlug := "bug-fix-flow"
	snapshot := `{"version":1,"name":"bug-fix-flow","steps":[]}`
	instanceID := uuid.NewString()

	// First call — everything should be created.
	res1, err := svc.EnsureWorkflowRepo(env.withTenant(context.Background()),
		teamID, defSlug, snapshot, instanceID)
	if err != nil {
		t.Fatalf("EnsureWorkflowRepo (first): %v", err)
	}
	if !res1.TypeRepoCreated || !res1.InstanceBranchCreated || !res1.BranchProtectionSet {
		t.Fatalf("expected all Created flags true on first call: %+v", res1)
	}
	if res1.SnapshotHash == "" {
		t.Errorf("expected non-empty SnapshotHash")
	}

	// Verify the snapshot file landed on main via raw Gitea API.
	gotSnapshot, err := env.giteaReadFile(t, orgName, "wf-"+defSlug, "main", DefinitionSnapshotPath)
	if err != nil {
		t.Fatalf("read back snapshot: %v", err)
	}
	if string(gotSnapshot) != snapshot {
		t.Errorf("snapshot on main drifted: got=%q want=%q", string(gotSnapshot), snapshot)
	}

	// Second call — idempotent; Created flags all false.
	res2, err := svc.EnsureWorkflowRepo(env.withTenant(context.Background()),
		teamID, defSlug, snapshot, instanceID)
	if err != nil {
		t.Fatalf("EnsureWorkflowRepo (second): %v", err)
	}
	if res2.TypeRepoCreated || res2.InstanceBranchCreated {
		t.Errorf("expected Created flags false on idempotent re-call: %+v", res2)
	}

	// Drift case — caller sends a different snapshot.
	_, err = svc.EnsureWorkflowRepo(env.withTenant(context.Background()),
		teamID, defSlug, `{"version":2}`, uuid.NewString())
	if err != ErrDefinitionDrift {
		t.Fatalf("expected ErrDefinitionDrift on third call, got %v", err)
	}
}

// giteaReadFile GETs /repos/{owner}/{repo}/contents/{path}?ref={branch} and
// decodes the base64 content Gitea wraps it in.
func (e *e2eEnv) giteaReadFile(t *testing.T, owner, repo, branch, path string) ([]byte, error) {
	t.Helper()
	u := fmt.Sprintf("%s/api/v1/repos/%s/%s/contents/%s?ref=%s",
		e.giteaURL, owner, repo, path, branch)
	req, _ := http.NewRequest(http.MethodGet, u, nil)
	req.Header.Set("Authorization", "token "+e.adminToken)
	resp, err := e.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("status=%d body=%s", resp.StatusCode, string(body))
	}
	var out struct {
		Content  string `json:"content"`
		Encoding string `json:"encoding"`
	}
	var buf bytes.Buffer
	if _, err := io.Copy(&buf, resp.Body); err != nil {
		return nil, err
	}
	if err := json.Unmarshal(buf.Bytes(), &out); err != nil {
		return nil, fmt.Errorf("decode contents response: %w", err)
	}
	if out.Encoding != "base64" {
		return nil, fmt.Errorf("unexpected encoding %q", out.Encoding)
	}
	cleaned := strings.ReplaceAll(out.Content, "\n", "")
	return base64.StdEncoding.DecodeString(cleaned)
}

// giteaListBranches returns the branch names of owner/repo.
func (e *e2eEnv) giteaListBranches(t *testing.T, owner, repo string) ([]string, error) {
	t.Helper()
	u := fmt.Sprintf("%s/api/v1/repos/%s/%s/branches", e.giteaURL, owner, repo)
	req, _ := http.NewRequest(http.MethodGet, u, nil)
	req.Header.Set("Authorization", "token "+e.adminToken)
	resp, err := e.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("status=%d body=%s", resp.StatusCode, string(body))
	}
	var rows []struct {
		Name string `json:"name"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&rows); err != nil {
		return nil, err
	}
	out := make([]string, 0, len(rows))
	for _, r := range rows {
		out = append(out, r.Name)
	}
	return out, nil
}

// giteaListBranchProtectionRules returns the rule_name set for owner/repo.
func (e *e2eEnv) giteaListBranchProtectionRules(t *testing.T, owner, repo string) ([]string, error) {
	t.Helper()
	u := fmt.Sprintf("%s/api/v1/repos/%s/%s/branch_protections", e.giteaURL, owner, repo)
	req, _ := http.NewRequest(http.MethodGet, u, nil)
	req.Header.Set("Authorization", "token "+e.adminToken)
	resp, err := e.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("status=%d body=%s", resp.StatusCode, string(body))
	}
	var rows []struct {
		RuleName string `json:"rule_name"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&rows); err != nil {
		return nil, err
	}
	out := make([]string, 0, len(rows))
	for _, r := range rows {
		out = append(out, r.RuleName)
	}
	return out, nil
}

// TestE2E_EnsureKBRepo_FullPath drives the user-facing kb provisioning
// pipeline against real Gitea. Compared to the workflow variant:
//
//   - kb repo `kb-<host>__<segs>` is created (private, default_branch=main)
//   - ONLY main branch protection is set (no inst-* glob)
//   - NO definition_snapshot file is written
//   - NO instance branch is created
//   - second call is idempotent (Created.KbRepo=false)
func TestE2E_EnsureKBRepo_FullPath(t *testing.T) {
	env := setupE2E(t)
	teamID := freshTeamID()
	short := teamID[:8]
	orgName := "t-" + short
	botUser := "bot-t-" + short
	env.cleanupTeamGitea(t, orgName, botUser)
	t.Cleanup(func() { env.cleanupTeamGitea(t, orgName, botUser) })

	// Seed team_ns + bot creds so EnsureKBRepo has a context.
	now := time.Now().UTC()
	ns := &models.TeamNamespace{
		TeamID: teamID, TenantID: env.tenantID,
		TeamDisplayName: "E2E KB Team",
		TeamNSOrg:       orgName, TeamShort: short, GitServerID: "gitea-local",
		Status: "active", CreatedAt: now, UpdatedAt: now,
	}
	if err := env.db.Create(ns).Error; err != nil {
		t.Fatalf("seed team_ns: %v", err)
	}
	enc, _ := env.aesKey.Seal([]byte("placeholder"))
	creds := &models.TeamBotCredentials{
		TeamID: teamID, TenantID: env.tenantID, GitServerID: "gitea-local",
		GiteaUsername: botUser, GiteaUserID: 1, GiteaTokenID: 1,
		TokenEncrypted: enc, TokenSHA256: "sha", CreatedAt: now,
	}
	if err := env.db.Create(creds).Error; err != nil {
		t.Fatalf("seed team_bot_credentials: %v", err)
	}

	// EnsureKBRepo doesn't create the org; pre-provision it via gitsync.
	if err := env.gs.EnsureOrg(context.Background(), env.tenantID, orgName, "E2E KB Team"); err != nil {
		t.Fatalf("EnsureOrg: %v", err)
	}

	svc := env.newService(t)
	codeRepoURL := "https://github.com/costrict/somerepo.git"

	// First call — kb repo should be newly created.
	res1, err := svc.EnsureKBRepo(env.withTenant(context.Background()), teamID, codeRepoURL)
	if err != nil {
		t.Fatalf("EnsureKBRepo (first): %v", err)
	}
	if !res1.KbRepoCreated || !res1.BranchProtectionSet {
		t.Fatalf("expected Created + ProtectionSet on first call: %+v", res1)
	}
	wantPath := "t-" + short + "/kb-github.com__costrict__somerepo"
	if res1.KbRepoPath != wantPath {
		t.Errorf("kb_repo_path: got %q want %q", res1.KbRepoPath, wantPath)
	}

	// Verify repo exists on Gitea.
	repoName := "kb-github.com__costrict__somerepo"
	branches, err := env.giteaListBranches(t, orgName, repoName)
	if err != nil {
		t.Fatalf("list branches: %v", err)
	}
	// Only main should exist (auto-init); no inst-* branch.
	for _, b := range branches {
		if strings.HasPrefix(b, "inst-") {
			t.Errorf("kb repo must NOT have instance branches, found %q", b)
		}
	}
	hasMain := false
	for _, b := range branches {
		if b == "main" {
			hasMain = true
		}
	}
	if !hasMain {
		t.Errorf("expected main branch in kb repo, got %v", branches)
	}

	// Only ONE branch protection rule (main); no inst-* glob.
	rules, err := env.giteaListBranchProtectionRules(t, orgName, repoName)
	if err != nil {
		t.Fatalf("list protections: %v", err)
	}
	if len(rules) != 1 || rules[0] != "main" {
		t.Errorf("expected exactly 1 protection rule (main), got %v", rules)
	}

	// KB must not write a definition_snapshot file. ReadFile should 404.
	if _, err := env.giteaReadFile(t, orgName, repoName, "main", "definition_snapshot.json"); err == nil {
		t.Errorf("kb repo must NOT contain definition_snapshot.json")
	}

	// Second call — idempotent.
	res2, err := svc.EnsureKBRepo(env.withTenant(context.Background()), teamID, codeRepoURL)
	if err != nil {
		t.Fatalf("EnsureKBRepo (second): %v", err)
	}
	if res2.KbRepoCreated {
		t.Errorf("expected KbRepoCreated=false on idempotent re-call: %+v", res2)
	}
	if !res2.BranchProtectionSet {
		t.Errorf("expected BranchProtectionSet=true (idempotent re-apply permitted): %+v", res2)
	}

	// Verify a different code_repo_url produces a separate kb repo (path
	// determinism). The two repos coexist under the same org.
	res3, err := svc.EnsureKBRepo(env.withTenant(context.Background()), teamID,
		"https://github.com/costrict/another.git")
	if err != nil {
		t.Fatalf("EnsureKBRepo (third, different url): %v", err)
	}
	if !res3.KbRepoCreated {
		t.Errorf("expected KbRepoCreated=true for new url: %+v", res3)
	}
	wantPath3 := "t-" + short + "/kb-github.com__costrict__another"
	if res3.KbRepoPath != wantPath3 {
		t.Errorf("kb_repo_path 3: got %q want %q", res3.KbRepoPath, wantPath3)
	}
}
