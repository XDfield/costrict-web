package worker

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/costrict/costrict-web/server/internal/gitsync"
	"github.com/costrict/costrict-web/server/internal/models"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

// --- doubles -----------------------------------------------------------

type fakeCostrictPusher struct {
	mu          sync.Mutex
	quotaCalls  [][]gitsync.QuotaRule
	jwksCalls   int
	quotaErr    error
	jwksErr     error
	quotaErrFor int // fail the first N quota pushes, then succeed
}

func (f *fakeCostrictPusher) InvalidateQuotaCache(_ context.Context, rules []gitsync.QuotaRule) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.quotaCalls = append(f.quotaCalls, append([]gitsync.QuotaRule(nil), rules...))
	if f.quotaErr != nil {
		if f.quotaErrFor == 0 || len(f.quotaCalls) <= f.quotaErrFor {
			return f.quotaErr
		}
	}
	return nil
}

func (f *fakeCostrictPusher) InvalidateJWKSCache(context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.jwksCalls++
	return f.jwksErr
}

func (f *fakeCostrictPusher) quotaCallCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.quotaCalls)
}

func (f *fakeCostrictPusher) jwksCallCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.jwksCalls
}

func (f *fakeCostrictPusher) lastQuotaSnapshot() []gitsync.QuotaRule {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.quotaCalls) == 0 {
		return nil
	}
	return f.quotaCalls[len(f.quotaCalls)-1]
}

type stubJWKSLister struct {
	mu  sync.Mutex
	ids []string
	err error
}

func (s *stubJWKSLister) ListKeyIDs(context.Context) ([]string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.err != nil {
		return nil, s.err
	}
	return append([]string(nil), s.ids...), nil
}

func (s *stubJWKSLister) set(ids []string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.ids = ids
}

// --- fixtures ----------------------------------------------------------

func setupGiteaConfigSyncDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		t.Fatalf("open SQLite: %v", err)
	}
	if err := db.Exec(`CREATE TABLE git_servers (
		server_id TEXT PRIMARY KEY, kind TEXT NOT NULL, endpoint TEXT NOT NULL,
		display_name TEXT NOT NULL, config TEXT NOT NULL DEFAULT '{}',
		is_template INTEGER NOT NULL DEFAULT 0, enabled INTEGER NOT NULL DEFAULT 1,
		created_at DATETIME, updated_at DATETIME
	)`).Error; err != nil {
		t.Fatalf("create git_servers: %v", err)
	}
	if err := db.Exec(`CREATE TABLE git_quota_rules (
		git_server_id TEXT NOT NULL, owner TEXT NOT NULL, repo TEXT NOT NULL DEFAULT '',
		max_file_size_mb INTEGER NOT NULL DEFAULT 0, repo_quota_mb INTEGER NOT NULL DEFAULT 0,
		updated_by TEXT NOT NULL DEFAULT '',
		created_at DATETIME, updated_at DATETIME,
		PRIMARY KEY (git_server_id, owner, repo)
	)`).Error; err != nil {
		t.Fatalf("create git_quota_rules: %v", err)
	}
	return db
}

func seedGiteaConfigSyncServer(t *testing.T, db *gorm.DB, id, endpoint, config string, enabled bool) {
	t.Helper()
	if err := db.Exec(`INSERT INTO git_servers
		(server_id, kind, endpoint, display_name, config, is_template, enabled, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, 0, ?, CURRENT_TIMESTAMP, CURRENT_TIMESTAMP)`,
		id, models.GitServerKindGitea, endpoint, id, config, enabled).Error; err != nil {
		t.Fatalf("seed Git server %s: %v", id, err)
	}
}

func seedQuotaRule(t *testing.T, db *gorm.DB, serverID, owner, repo string, maxFileMB, repoMB int64) {
	t.Helper()
	if err := db.Exec(`INSERT INTO git_quota_rules
		(git_server_id, owner, repo, max_file_size_mb, repo_quota_mb, updated_by, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, 'tester', CURRENT_TIMESTAMP, CURRENT_TIMESTAMP)`,
		serverID, owner, repo, maxFileMB, repoMB).Error; err != nil {
		t.Fatalf("seed quota rule %s/%s: %v", owner, repo, err)
	}
}

const testInternalTokenConfig = `{"admin_token":"pat","internal_token":"itok"}`

// --- tests -------------------------------------------------------------

// G3(b). Steady state must be free: after the first tick has delivered the
// snapshot, a tick that finds nothing changed sends no request at all (and, by
// construction, takes no advisory lock and opens no transaction either).
func TestGiteaConfigSyncWorker_SecondTickWithUnchangedRulesSendsNothing(t *testing.T) {
	db := setupGiteaConfigSyncDB(t)
	seedGiteaConfigSyncServer(t, db, "gs-1", "https://gitea.example", testInternalTokenConfig, true)
	seedQuotaRule(t, db, "gs-1", "acme", "", 20, 200)

	pusher := &fakeCostrictPusher{}
	jwks := &stubJWKSLister{ids: []string{"kid-1"}}
	w := &GiteaConfigSyncWorker{
		DB:        db,
		JWKS:      jwks,
		NewClient: func(string, string) CostrictConfigPusher { return pusher },
	}

	if err := w.ReconcileOnce(context.Background()); err != nil {
		t.Fatalf("first reconcile: %v", err)
	}
	if pusher.quotaCallCount() != 1 {
		t.Fatalf("first tick quota pushes = %d, want 1", pusher.quotaCallCount())
	}
	// Cold start adopts the JWKS key set without invalidating: a worker restart
	// is not evidence that anything rotated, and Invalidate() drops the fork's
	// stale-key fallback.
	if pusher.jwksCallCount() != 0 {
		t.Fatalf("first tick JWKS invalidations = %d, want 0 (cold start must not invalidate)", pusher.jwksCallCount())
	}

	if err := w.ReconcileOnce(context.Background()); err != nil {
		t.Fatalf("second reconcile: %v", err)
	}
	if got := pusher.quotaCallCount(); got != 1 {
		t.Fatalf("quota pushes after unchanged second tick = %d, want 1 (steady state must send nothing)", got)
	}
	if got := pusher.jwksCallCount(); got != 0 {
		t.Fatalf("JWKS invalidations after unchanged second tick = %d, want 0", got)
	}
}

// G3(c) at the worker layer. A 200 that says "quota disabled, no-op" leaves the
// server unacknowledged, so the next tick tries again — the deployment must not
// settle into believing the rules were delivered.
func TestGiteaConfigSyncWorker_QuotaDisabledResponseIsRetriedNotAcknowledged(t *testing.T) {
	db := setupGiteaConfigSyncDB(t)
	seedGiteaConfigSyncServer(t, db, "gs-1", "https://gitea.example", testInternalTokenConfig, true)
	seedQuotaRule(t, db, "gs-1", "acme", "", 20, 200)

	pusher := &fakeCostrictPusher{quotaErr: gitsync.ErrCostrictQuotaDisabled}
	w := &GiteaConfigSyncWorker{
		DB:        db,
		NewClient: func(string, string) CostrictConfigPusher { return pusher },
	}

	err := w.ReconcileOnce(context.Background())
	if !errors.Is(err, gitsync.ErrCostrictQuotaDisabled) {
		t.Fatalf("first reconcile error = %v, want ErrCostrictQuotaDisabled surfaced", err)
	}
	if err := w.ReconcileOnce(context.Background()); !errors.Is(err, gitsync.ErrCostrictQuotaDisabled) {
		t.Fatalf("second reconcile error = %v, want the same failure again", err)
	}
	if got := pusher.quotaCallCount(); got != 2 {
		t.Fatalf("quota pushes = %d, want 2 (a no-op acknowledgement must not be recorded as delivered)", got)
	}

	// Once the operator enables quotas on the Git server, the next tick lands
	// and the retry stops.
	pusher.mu.Lock()
	pusher.quotaErr = nil
	pusher.mu.Unlock()
	if err := w.ReconcileOnce(context.Background()); err != nil {
		t.Fatalf("third reconcile: %v", err)
	}
	if err := w.ReconcileOnce(context.Background()); err != nil {
		t.Fatalf("fourth reconcile: %v", err)
	}
	if got := pusher.quotaCallCount(); got != 3 {
		t.Fatalf("quota pushes = %d, want 3 (settled after the accepted push)", got)
	}
}

// A transport failure is likewise not an acknowledgement.
func TestGiteaConfigSyncWorker_TransportFailureIsRetried(t *testing.T) {
	db := setupGiteaConfigSyncDB(t)
	seedGiteaConfigSyncServer(t, db, "gs-1", "https://gitea.example", testInternalTokenConfig, true)

	pusher := &fakeCostrictPusher{quotaErr: gitsync.ErrGiteaUnreachable, quotaErrFor: 1}
	w := &GiteaConfigSyncWorker{
		DB:        db,
		NewClient: func(string, string) CostrictConfigPusher { return pusher },
	}
	if err := w.ReconcileOnce(context.Background()); !errors.Is(err, gitsync.ErrGiteaUnreachable) {
		t.Fatalf("first reconcile error = %v, want unreachable", err)
	}
	if err := w.ReconcileOnce(context.Background()); err != nil {
		t.Fatalf("second reconcile: %v", err)
	}
	if got := pusher.quotaCallCount(); got != 2 {
		t.Fatalf("quota pushes = %d, want 2", got)
	}
	if err := w.ReconcileOnce(context.Background()); err != nil {
		t.Fatalf("third reconcile: %v", err)
	}
	if got := pusher.quotaCallCount(); got != 2 {
		t.Fatalf("quota pushes = %d, want 2 (settled after the successful retry)", got)
	}
}

// The fork's Refresh() replaces the whole rule set, so every push must carry
// the complete snapshot for that server and nothing belonging to another one.
func TestGiteaConfigSyncWorker_PushesFullPerServerSnapshot(t *testing.T) {
	db := setupGiteaConfigSyncDB(t)
	seedGiteaConfigSyncServer(t, db, "gs-1", "https://one.example", testInternalTokenConfig, true)
	seedGiteaConfigSyncServer(t, db, "gs-2", "https://two.example", testInternalTokenConfig, true)
	seedQuotaRule(t, db, "gs-1", "acme", "", 20, 200)
	seedQuotaRule(t, db, "gs-1", "acme", "widgets", 5, 50)
	seedQuotaRule(t, db, "gs-2", "other", "", 1, 10)

	pushers := map[string]*fakeCostrictPusher{
		"https://one.example": {},
		"https://two.example": {},
	}
	w := &GiteaConfigSyncWorker{
		DB: db,
		NewClient: func(endpoint, internalToken string) CostrictConfigPusher {
			if internalToken != "itok" {
				t.Errorf("internal token = %q, want the value from git_servers.config", internalToken)
			}
			return pushers[endpoint]
		},
	}
	if err := w.ReconcileOnce(context.Background()); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	one := pushers["https://one.example"].lastQuotaSnapshot()
	if len(one) != 2 {
		t.Fatalf("gs-1 snapshot = %+v, want both rules", one)
	}
	// Owner-level default sorts before the repo-level override, and the empty
	// Repo is carried through verbatim: it is the fork's own sentinel.
	if one[0].Repo != "" || one[0].MaxFileSizeMB != 20 || one[1].Repo != "widgets" || one[1].MaxFileSizeMB != 5 {
		t.Fatalf("gs-1 snapshot = %+v", one)
	}
	two := pushers["https://two.example"].lastQuotaSnapshot()
	if len(two) != 1 || two[0].Owner != "other" {
		t.Fatalf("gs-2 snapshot = %+v, want only its own rule", two)
	}
}

// A server with no internal_token has not been onboarded to the fork surface;
// it is skipped rather than failing the round, and no client is built for it.
func TestGiteaConfigSyncWorker_SkipsServersWithoutInternalToken(t *testing.T) {
	db := setupGiteaConfigSyncDB(t)
	seedGiteaConfigSyncServer(t, db, "gs-nocfg", "https://nocfg.example", `{"admin_token":"pat"}`, true)
	seedGiteaConfigSyncServer(t, db, "gs-bad", "https://bad.example", `not json`, true)
	seedGiteaConfigSyncServer(t, db, "gs-off", "https://off.example", testInternalTokenConfig, false)
	seedGiteaConfigSyncServer(t, db, "gs-ok", "https://ok.example", testInternalTokenConfig, true)

	built := map[string]int{}
	pusher := &fakeCostrictPusher{}
	w := &GiteaConfigSyncWorker{
		DB: db,
		NewClient: func(endpoint, _ string) CostrictConfigPusher {
			built[endpoint]++
			return pusher
		},
	}
	if err := w.ReconcileOnce(context.Background()); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if built["https://nocfg.example"] != 0 || built["https://bad.example"] != 0 || built["https://off.example"] != 0 {
		t.Fatalf("skipped servers built clients: %v", built)
	}
	if built["https://ok.example"] != 1 {
		t.Fatalf("configured server client builds = %d, want 1", built["https://ok.example"])
	}
	if pusher.quotaCallCount() != 1 {
		t.Fatalf("quota pushes = %d, want 1", pusher.quotaCallCount())
	}
}

// An edit to the rules propagates on the next tick without a Gitea restart.
func TestGiteaConfigSyncWorker_RuleChangeTriggersRepush(t *testing.T) {
	db := setupGiteaConfigSyncDB(t)
	seedGiteaConfigSyncServer(t, db, "gs-1", "https://gitea.example", testInternalTokenConfig, true)
	seedQuotaRule(t, db, "gs-1", "acme", "", 20, 200)

	pusher := &fakeCostrictPusher{}
	w := &GiteaConfigSyncWorker{
		DB:        db,
		NewClient: func(string, string) CostrictConfigPusher { return pusher },
	}
	if err := w.ReconcileOnce(context.Background()); err != nil {
		t.Fatalf("first reconcile: %v", err)
	}

	if err := db.Exec(`UPDATE git_quota_rules SET max_file_size_mb = 40 WHERE git_server_id = 'gs-1'`).Error; err != nil {
		t.Fatalf("update rule: %v", err)
	}
	if err := w.ReconcileOnce(context.Background()); err != nil {
		t.Fatalf("second reconcile: %v", err)
	}
	if got := pusher.quotaCallCount(); got != 2 {
		t.Fatalf("quota pushes = %d, want 2", got)
	}
	if snapshot := pusher.lastQuotaSnapshot(); len(snapshot) != 1 || snapshot[0].MaxFileSizeMB != 40 {
		t.Fatalf("re-pushed snapshot = %+v", snapshot)
	}

	// Deleting every rule is a meaningful instruction, not a no-op: the fork
	// must be told to drop the overrides and fall back to its app.ini defaults.
	if err := db.Exec(`DELETE FROM git_quota_rules`).Error; err != nil {
		t.Fatalf("delete rules: %v", err)
	}
	if err := w.ReconcileOnce(context.Background()); err != nil {
		t.Fatalf("third reconcile: %v", err)
	}
	if got := pusher.quotaCallCount(); got != 3 {
		t.Fatalf("quota pushes = %d, want 3 (clearing rules must be delivered)", got)
	}
	if snapshot := pusher.lastQuotaSnapshot(); len(snapshot) != 0 {
		t.Fatalf("cleared snapshot = %+v, want empty", snapshot)
	}
}

// FI-5: a changed key-id set invalidates the fork's JWKS cache exactly once per
// server, and an unreadable JWKS neither invalidates anything nor blocks the
// quota push.
func TestGiteaConfigSyncWorker_JWKSRotationInvalidatesOncePerServer(t *testing.T) {
	db := setupGiteaConfigSyncDB(t)
	seedGiteaConfigSyncServer(t, db, "gs-1", "https://one.example", testInternalTokenConfig, true)
	seedGiteaConfigSyncServer(t, db, "gs-2", "https://two.example", testInternalTokenConfig, true)

	pushers := map[string]*fakeCostrictPusher{
		"https://one.example": {},
		"https://two.example": {},
	}
	jwks := &stubJWKSLister{ids: []string{"kid-old"}}
	w := &GiteaConfigSyncWorker{
		DB:        db,
		JWKS:      jwks,
		NewClient: func(endpoint, _ string) CostrictConfigPusher { return pushers[endpoint] },
	}

	if err := w.ReconcileOnce(context.Background()); err != nil { // cold start
		t.Fatalf("first reconcile: %v", err)
	}
	for endpoint, p := range pushers {
		if p.jwksCallCount() != 0 {
			t.Fatalf("%s invalidated on cold start", endpoint)
		}
	}

	jwks.set([]string{"kid-new"})
	if err := w.ReconcileOnce(context.Background()); err != nil {
		t.Fatalf("rotation reconcile: %v", err)
	}
	for endpoint, p := range pushers {
		if p.jwksCallCount() != 1 {
			t.Fatalf("%s invalidations = %d, want 1 after rotation", endpoint, p.jwksCallCount())
		}
	}

	if err := w.ReconcileOnce(context.Background()); err != nil {
		t.Fatalf("post-rotation reconcile: %v", err)
	}
	for endpoint, p := range pushers {
		if p.jwksCallCount() != 1 {
			t.Fatalf("%s invalidations = %d, want no repeat once acknowledged", endpoint, p.jwksCallCount())
		}
	}

	// Key order carries no meaning in a JWKS document and must not look like a
	// rotation.
	jwks.set([]string{"kid-new", "kid-second"})
	if err := w.ReconcileOnce(context.Background()); err != nil {
		t.Fatalf("added-key reconcile: %v", err)
	}
	jwks.set([]string{"kid-second", "kid-new"})
	if err := w.ReconcileOnce(context.Background()); err != nil {
		t.Fatalf("reordered reconcile: %v", err)
	}
	for endpoint, p := range pushers {
		if p.jwksCallCount() != 2 {
			t.Fatalf("%s invalidations = %d, want 2 (reordering is not a rotation)", endpoint, p.jwksCallCount())
		}
	}
}

func TestGiteaConfigSyncWorker_UnreadableJWKSDoesNotBlockQuotaPush(t *testing.T) {
	db := setupGiteaConfigSyncDB(t)
	seedGiteaConfigSyncServer(t, db, "gs-1", "https://gitea.example", testInternalTokenConfig, true)
	seedQuotaRule(t, db, "gs-1", "acme", "", 20, 200)

	pusher := &fakeCostrictPusher{}
	w := &GiteaConfigSyncWorker{
		DB:        db,
		JWKS:      &stubJWKSLister{err: errors.New("JWKS endpoint returned 503")},
		NewClient: func(string, string) CostrictConfigPusher { return pusher },
	}
	if err := w.ReconcileOnce(context.Background()); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if pusher.quotaCallCount() != 1 {
		t.Fatalf("quota pushes = %d, want 1", pusher.quotaCallCount())
	}
	if pusher.jwksCallCount() != 0 {
		t.Fatalf("JWKS invalidations = %d, want 0 while the issuer is unreadable", pusher.jwksCallCount())
	}
}

func TestGiteaConfigSyncWorker_DisabledStartIsInert(t *testing.T) {
	db := setupGiteaConfigSyncDB(t)
	seedGiteaConfigSyncServer(t, db, "gs-1", "https://gitea.example", testInternalTokenConfig, true)

	pusher := &fakeCostrictPusher{}
	w := &GiteaConfigSyncWorker{
		DB:        db,
		Enabled:   false,
		Interval:  time.Millisecond,
		NewClient: func(string, string) CostrictConfigPusher { return pusher },
	}
	w.Start()
	time.Sleep(20 * time.Millisecond)
	w.Stop()
	if pusher.quotaCallCount() != 0 {
		t.Fatalf("disabled worker pushed %d times", pusher.quotaCallCount())
	}
}

func TestGiteaConfigSyncWorker_StartStopRunsReconcile(t *testing.T) {
	db := setupGiteaConfigSyncDB(t)
	seedGiteaConfigSyncServer(t, db, "gs-1", "https://gitea.example", testInternalTokenConfig, true)

	pusher := &fakeCostrictPusher{}
	w := &GiteaConfigSyncWorker{
		DB:        db,
		Enabled:   true,
		Interval:  time.Hour,
		NewClient: func(string, string) CostrictConfigPusher { return pusher },
	}
	w.Start()
	deadline := time.Now().Add(2 * time.Second)
	for pusher.quotaCallCount() == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	w.Stop()
	if pusher.quotaCallCount() != 1 {
		t.Fatalf("quota pushes after start = %d, want 1", pusher.quotaCallCount())
	}
}

func TestGiteaConfigSyncWorker_NilDatabaseIsAnError(t *testing.T) {
	var w *GiteaConfigSyncWorker
	if err := w.ReconcileOnce(context.Background()); err == nil {
		t.Fatal("nil worker reconciled without error")
	}
	if err := (&GiteaConfigSyncWorker{}).ReconcileOnce(context.Background()); err == nil {
		t.Fatal("worker without DB reconciled without error")
	}
}

// --- JWKS reader -------------------------------------------------------

func TestHTTPJWKSKeyIDLister(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/.well-known/jwks" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"keys":[{"kty":"RSA","use":"sig","kid":"abc","alg":"RS256","n":"nn","e":"AQAB"},{"kty":"RSA","kid":""}]}`)
	}))
	defer srv.Close()

	lister := NewHTTPJWKSKeyIDLister(srv.URL+"/.well-known/jwks", time.Second)
	ids, err := lister.ListKeyIDs(context.Background())
	if err != nil {
		t.Fatalf("ListKeyIDs: %v", err)
	}
	if len(ids) != 1 || ids[0] != "abc" {
		t.Fatalf("ids = %v, want [abc]", ids)
	}

	// cs-user answers 503 when no signing key is configured. That is a state,
	// not a crash, and must surface as an ordinary error so the caller keeps
	// whatever it already knew.
	missing := NewHTTPJWKSKeyIDLister(srv.URL+"/nope", time.Second)
	if _, err := missing.ListKeyIDs(context.Background()); err == nil {
		t.Fatal("non-200 JWKS response returned no error")
	}

	if NewHTTPJWKSKeyIDLister("  ", time.Second) != nil {
		t.Fatal("empty URL should yield nil (rotation watching not configured)")
	}
}

func TestJWKSKeyIDDigest_OrderInsensitiveAndBlankAware(t *testing.T) {
	if jwksKeyIDDigest([]string{"a", "b"}) != jwksKeyIDDigest([]string{"b", "a"}) {
		t.Fatal("digest is order sensitive")
	}
	if jwksKeyIDDigest([]string{"a"}) == jwksKeyIDDigest([]string{"a", "b"}) {
		t.Fatal("digest collides across different key sets")
	}
	if jwksKeyIDDigest([]string{"", "  "}) != "" {
		t.Fatal("a blank key set must be reported as unknown, not as a rotation")
	}
}

func TestParseGiteaInternalToken(t *testing.T) {
	tok, err := parseGiteaInternalToken(`{"admin_token":"pat","internal_token":"  itok  "}`)
	if err != nil || tok != "itok" {
		t.Fatalf("token = %q err = %v", tok, err)
	}
	if tok, err := parseGiteaInternalToken(""); err != nil || tok != "" {
		t.Fatalf("empty config: token = %q err = %v", tok, err)
	}
	if _, err := parseGiteaInternalToken("nope"); err == nil {
		t.Fatal("malformed config parsed without error")
	}
}

func TestGiteaQuotaRulesDigest_EmptyIsStableAndNonEmpty(t *testing.T) {
	empty := giteaQuotaRulesDigest(nil)
	if empty == "" {
		t.Fatal("empty snapshot digest must not be empty, or it can never be distinguished from an unacknowledged server")
	}
	if empty != giteaQuotaRulesDigest([]gitsync.QuotaRule{}) {
		t.Fatal("nil and empty snapshots must digest identically")
	}
	if empty == giteaQuotaRulesDigest([]gitsync.QuotaRule{{Owner: "acme"}}) {
		t.Fatal("digest collision between empty and non-empty snapshots")
	}
}
