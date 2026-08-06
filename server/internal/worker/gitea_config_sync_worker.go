// GiteaConfigSyncWorker pushes costrict-web-owned configuration into the
// CoStrict Gitea fork's in-memory state (Gitea fork integration FI-4 / FI-5).
//
// Two jobs, both against the fork's private surface (see
// internal/gitsync/costrict_internal.go for the wire contract):
//
//   - push the complete push-quota rule snapshot for each Git server, so the
//     fork's pre-receive gate enforces per-owner / per-repository overrides
//     instead of only its app.ini defaults;
//   - force a JWKS re-fetch when cs-user's signing key rotates, so tokens
//     signed by the new key verify immediately rather than after the fork's
//     5-minute JWKS TTL expires.
//
// Why periodic rather than purely event-driven, which would look tidier:
//
//  1. cmd/api and cmd/worker are separate processes with no bus between them,
//     so an admin edit in the API process cannot call into this one; and
//  2. decisively — the fork holds quota rules in process memory. A Gitea
//     restart drops every rule we ever pushed, without telling anyone. Only
//     something that re-pushes unprompted can recover from that. Setting
//     [costrict] QUOTA_RULES_FILE on the Gitea side covers the window before
//     the first tick after such a restart; the two together are belt and
//     braces, and neither alone is sufficient.
//
// Steady state costs nothing: a per-server digest of the last acknowledged
// snapshot is held in memory, and a tick that finds nothing changed takes no
// lock, opens no transaction and sends no request. A worker restart replays one
// redundant push per server, which is harmless because the fork's Refresh()
// is a whole-set replace.
package worker

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/costrict/costrict-web/server/internal/gitsync"
	"github.com/costrict/costrict-web/server/internal/logger"
	"github.com/costrict/costrict-web/server/internal/models"
	"gorm.io/gorm"
)

const (
	defaultGiteaConfigSyncInterval       = 5 * time.Minute
	defaultGiteaConfigSyncRequestTimeout = 15 * time.Second
)

// CostrictConfigPusher is the fork-private surface this worker depends on.
// Declared here as a narrow interface so tests inject a stub instead of
// standing up a Gitea. *gitsync.CostrictInternalClient satisfies it.
type CostrictConfigPusher interface {
	InvalidateQuotaCache(ctx context.Context, rules []gitsync.QuotaRule) error
	InvalidateJWKSCache(ctx context.Context) error
}

// JWKSKeyIDLister reports the key ids currently published by the JWT issuer
// (cs-user). Only the ids matter: the fork selects a verification key by strict
// kid match, so a changed id set is exactly the condition under which its
// cached key set has gone stale.
type JWKSKeyIDLister interface {
	ListKeyIDs(ctx context.Context) ([]string, error)
}

// giteaConfigSyncAck records what a given Git server has acknowledged, so an
// unchanged tick can be skipped entirely.
type giteaConfigSyncAck struct {
	quotaDigest string
	jwksDigest  string
}

// GiteaConfigSyncWorker converges every enabled Gitea server.
type GiteaConfigSyncWorker struct {
	DB *gorm.DB
	// Enabled is the kill switch. False leaves the worker inert (Start is a
	// no-op), which stops all pushes without touching any Gitea state: rules
	// already pushed simply go stale rather than being withdrawn.
	Enabled        bool
	Interval       time.Duration
	RequestTimeout time.Duration
	// NewClient builds a client for one server. Returning nil means "not
	// configured for this server" and the server is skipped.
	NewClient func(endpoint, internalToken string) CostrictConfigPusher
	// JWKS is optional. Nil disables rotation watching, and quota pushing
	// continues unaffected — the two are independent failures.
	JWKS   JWKSKeyIDLister
	Locker GiteaConfigSyncLocker

	lifecycleMu sync.Mutex
	cancel      context.CancelFunc
	running     bool
	wg          sync.WaitGroup

	stateMu sync.Mutex
	acks    map[string]giteaConfigSyncAck
	// lastJWKSDigest is the id set observed on the previous successful fetch.
	lastJWKSDigest string
	// pendingJWKSDigest is non-empty only after an observed CHANGE, and is
	// cleared once every server has acknowledged it.
	//
	// Cold start deliberately adopts the first observed digest WITHOUT pushing.
	// Invalidate() drops the fork's cached keys and with them its stale-key
	// fallback, so an invalidation issued while the JWKS happens to be
	// unreachable converts a degraded state into a total authentication
	// outage. Restarting this worker is not evidence that anything rotated, so
	// it must not trigger one. The cost of that choice is that a rotation
	// occurring across a worker restart is missed and falls back to the fork's
	// own 5-minute TTL — a bounded delay, not an outage.
	pendingJWKSDigest string
}

// Start launches the reconcile loop. Idempotent.
func (w *GiteaConfigSyncWorker) Start() {
	if w == nil || w.DB == nil {
		return
	}
	w.lifecycleMu.Lock()
	defer w.lifecycleMu.Unlock()
	if w.running {
		return
	}
	if !w.Enabled {
		logger.Info("Gitea config sync worker disabled")
		return
	}
	if w.Interval <= 0 {
		w.Interval = defaultGiteaConfigSyncInterval
	}
	if w.RequestTimeout <= 0 {
		w.RequestTimeout = defaultGiteaConfigSyncRequestTimeout
	}
	if w.NewClient == nil {
		w.NewClient = defaultCostrictConfigPusherFactory
	}
	ctx, cancel := context.WithCancel(context.Background())
	w.cancel = cancel
	w.running = true
	w.wg.Add(1)
	go w.run(ctx)
}

// Stop cancels the loop and waits for the in-flight round to unwind.
func (w *GiteaConfigSyncWorker) Stop() {
	if w == nil {
		return
	}
	w.lifecycleMu.Lock()
	defer w.lifecycleMu.Unlock()
	if w.cancel == nil {
		return
	}
	w.cancel()
	w.wg.Wait()
	w.cancel = nil
	w.running = false
}

func defaultCostrictConfigPusherFactory(endpoint, internalToken string) CostrictConfigPusher {
	client := gitsync.NewCostrictInternalClient(endpoint, internalToken)
	if client == nil {
		return nil
	}
	return client
}

func (w *GiteaConfigSyncWorker) run(ctx context.Context) {
	defer w.wg.Done()
	w.reconcileAndLog(ctx)
	ticker := time.NewTicker(w.Interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			w.reconcileAndLog(ctx)
		}
	}
}

func (w *GiteaConfigSyncWorker) reconcileAndLog(ctx context.Context) {
	if err := w.ReconcileOnce(ctx); err != nil && !errors.Is(err, context.Canceled) {
		logger.Error("Gitea config sync completed with errors: %v", err)
	}
}

// ReconcileOnce converges every enabled Gitea server once.
//
// Each server is independent: a missing internal token is a gray-rollout skip,
// and a failing server neither blocks nor is blocked by the others. Errors are
// joined and returned only after every server has had its turn.
func (w *GiteaConfigSyncWorker) ReconcileOnce(ctx context.Context) error {
	if w == nil || w.DB == nil {
		return errors.New("Gitea config sync worker is not configured")
	}

	// JWKS first, and non-fatally: a rotation check that cannot run must not
	// prevent quota rules from being delivered.
	jwksDigest := w.refreshJWKSDigest(ctx)

	var servers []models.GitServer
	if err := w.DB.WithContext(ctx).
		Where("enabled = ? AND kind = ?", true, models.GitServerKindGitea).
		Order("server_id ASC").Find(&servers).Error; err != nil {
		return fmt.Errorf("query enabled Git servers: %w", err)
	}
	if len(servers) == 0 {
		return nil
	}

	serverIDs := make([]string, 0, len(servers))
	for i := range servers {
		serverIDs = append(serverIDs, servers[i].ServerID)
	}
	rulesByServer, err := w.loadQuotaRules(ctx, serverIDs)
	if err != nil {
		return err
	}

	locker := w.Locker
	if locker == nil {
		locker = newGiteaConfigSyncLocker(w.DB)
	}
	factory := w.NewClient
	if factory == nil {
		factory = defaultCostrictConfigPusherFactory
	}
	timeout := w.RequestTimeout
	if timeout <= 0 {
		timeout = defaultGiteaConfigSyncRequestTimeout
	}

	var syncErrors []error
	acknowledgedJWKS := 0
	for i := range servers {
		if err := ctx.Err(); err != nil {
			return errors.Join(append(syncErrors, err)...)
		}
		server := &servers[i]
		rules := rulesByServer[server.ServerID]
		quotaDigest := giteaQuotaRulesDigest(rules)

		ack := w.ackFor(server.ServerID)
		needQuota := ack.quotaDigest != quotaDigest
		needJWKS := jwksDigest != "" && ack.jwksDigest != jwksDigest
		if !needQuota && !needJWKS {
			// Steady state: no lock, no transaction, no request.
			if jwksDigest != "" {
				acknowledgedJWKS++
			}
			continue
		}

		internalToken, cfgErr := parseGiteaInternalToken(server.Config)
		if cfgErr != nil {
			logger.Warn("Gitea config sync skipped serverID=%s reason=invalid-config err=%v", server.ServerID, cfgErr)
			continue
		}
		if strings.TrimSpace(server.Endpoint) == "" || internalToken == "" {
			// Gray rollout: a server without git_servers.config.internal_token
			// has simply not been onboarded to the fork surface yet.
			logger.Warn("Gitea config sync skipped serverID=%s reason=missing-config fields=%s",
				server.ServerID, strings.Join(missingGiteaConfigSyncFields(server.Endpoint, internalToken), ","))
			continue
		}
		client := factory(server.Endpoint, internalToken)
		if client == nil {
			logger.Warn("Gitea config sync skipped serverID=%s reason=client-unavailable", server.ServerID)
			continue
		}

		lock, acquired, lockErr := locker.TryLock(ctx, server.ServerID)
		if lockErr != nil {
			logger.Error("Gitea config sync lock failed serverID=%s err=%v", server.ServerID, lockErr)
			syncErrors = append(syncErrors, fmt.Errorf("server %s advisory lock: %w", server.ServerID, lockErr))
			continue
		}
		if !acquired {
			logger.Info("Gitea config sync skipped serverID=%s reason=lock-held", server.ServerID)
			continue
		}

		quotaOK, jwksOK, opErr := w.pushToServer(ctx, client, timeout, lock, pushRequest{
			serverID:   server.ServerID,
			rules:      rules,
			pushQuota:  needQuota,
			pushJWKS:   needJWKS,
			ruleDigest: quotaDigest,
		})
		if quotaOK {
			w.recordQuotaAck(server.ServerID, quotaDigest)
		}
		if jwksOK {
			w.recordJWKSAck(server.ServerID, jwksDigest)
		}
		if jwksDigest != "" && (jwksOK || !needJWKS) {
			acknowledgedJWKS++
		}
		if opErr != nil {
			syncErrors = append(syncErrors, fmt.Errorf("server %s: %w", server.ServerID, opErr))
		}
	}

	// The pending rotation is cleared only once every server has taken it;
	// anything else would drop the retry for whichever server was unreachable.
	if jwksDigest != "" && acknowledgedJWKS == len(servers) {
		w.clearPendingJWKS(jwksDigest)
	}
	return errors.Join(syncErrors...)
}

type pushRequest struct {
	serverID   string
	rules      []gitsync.QuotaRule
	pushQuota  bool
	pushJWKS   bool
	ruleDigest string
}

// pushToServer performs the needed operations under the held advisory lock and
// reports which of them succeeded.
//
// The lock is finished here (rather than by the caller) so the deferred
// rollback survives a panic in the HTTP path.
func (w *GiteaConfigSyncWorker) pushToServer(
	ctx context.Context,
	client CostrictConfigPusher,
	timeout time.Duration,
	lock GiteaConfigSyncLock,
	req pushRequest,
) (quotaOK bool, jwksOK bool, retErr error) {
	defer func() {
		panicValue := recover()
		if finishErr := lock.Finish(panicValue == nil && retErr == nil); finishErr != nil {
			retErr = errors.Join(retErr, fmt.Errorf("finish advisory lock transaction: %w", finishErr))
		}
		if panicValue != nil {
			panic(panicValue)
		}
	}()

	if req.pushQuota {
		requestCtx, cancel := context.WithTimeout(ctx, timeout)
		err := client.InvalidateQuotaCache(requestCtx, req.rules)
		cancel()
		switch {
		case err == nil:
			quotaOK = true
			logger.Info("Gitea quota rules pushed serverID=%s rules=%d digest=%s",
				req.serverID, len(req.rules), req.ruleDigest[:8])
		case errors.Is(err, gitsync.ErrCostrictQuotaDisabled):
			// HTTP 200, but the fork threw the rules away. Recording this as
			// success would leave the deployment believing quotas are enforced
			// when nothing is enforcing them, so it stays unacknowledged and is
			// retried — the repetition in the log is the point.
			logger.Warn("Gitea quota rules rejected serverID=%s reason=quota-disabled-on-server", req.serverID)
			retErr = errors.Join(retErr, err)
		default:
			logger.Error("Gitea quota rules push failed serverID=%s err=%v", req.serverID, err)
			retErr = errors.Join(retErr, err)
		}
	}

	if req.pushJWKS {
		requestCtx, cancel := context.WithTimeout(ctx, timeout)
		err := client.InvalidateJWKSCache(requestCtx)
		cancel()
		switch {
		case err == nil:
			jwksOK = true
			logger.Info("Gitea JWKS cache invalidated serverID=%s", req.serverID)
		case errors.Is(err, gitsync.ErrCostrictDisabled):
			logger.Warn("Gitea JWKS invalidation rejected serverID=%s reason=costrict-disabled-on-server", req.serverID)
			retErr = errors.Join(retErr, err)
		default:
			logger.Error("Gitea JWKS invalidation failed serverID=%s err=%v", req.serverID, err)
			retErr = errors.Join(retErr, err)
		}
	}
	return quotaOK, jwksOK, retErr
}

// loadQuotaRules reads the full rule set for the supplied servers in one query
// and groups it by server, in the deterministic order the digest depends on.
func (w *GiteaConfigSyncWorker) loadQuotaRules(ctx context.Context, serverIDs []string) (map[string][]gitsync.QuotaRule, error) {
	grouped := make(map[string][]gitsync.QuotaRule, len(serverIDs))
	if len(serverIDs) == 0 {
		return grouped, nil
	}
	var rows []models.GitQuotaRule
	if err := w.DB.WithContext(ctx).
		Where("git_server_id IN ?", serverIDs).
		Order("git_server_id ASC, owner ASC, repo ASC").
		Find(&rows).Error; err != nil {
		return nil, fmt.Errorf("query Git quota rules: %w", err)
	}
	for i := range rows {
		row := &rows[i]
		grouped[row.GitServerID] = append(grouped[row.GitServerID], gitsync.QuotaRule{
			Owner:         row.Owner,
			Repo:          row.Repo,
			MaxFileSizeMB: row.MaxFileSizeMB,
			RepoQuotaMB:   row.RepoQuotaMB,
		})
	}
	return grouped, nil
}

// refreshJWKSDigest fetches the issuer's key ids and returns the digest that
// still needs pushing, or "" when there is nothing to push.
func (w *GiteaConfigSyncWorker) refreshJWKSDigest(ctx context.Context) string {
	if w.JWKS == nil {
		return w.pendingJWKS()
	}
	ids, err := w.JWKS.ListKeyIDs(ctx)
	if err != nil {
		// Not fatal, and not a reason to invalidate anything: the fork keeps
		// serving its cached keys, which is the correct behaviour while the
		// issuer is unavailable.
		logger.Warn("Gitea config sync could not read issuer JWKS: %v", err)
		return w.pendingJWKS()
	}
	return w.observeJWKSKeyIDs(ids)
}

func (w *GiteaConfigSyncWorker) observeJWKSKeyIDs(ids []string) string {
	digest := jwksKeyIDDigest(ids)
	if digest == "" {
		logger.Warn("Gitea config sync ignored an empty issuer JWKS key set")
		return w.pendingJWKS()
	}
	w.stateMu.Lock()
	defer w.stateMu.Unlock()
	switch {
	case w.lastJWKSDigest == "":
		// Cold start — adopt without pushing. See pendingJWKSDigest's comment.
		w.lastJWKSDigest = digest
	case w.lastJWKSDigest != digest:
		logger.Info("Issuer JWKS key set changed; scheduling Gitea JWKS invalidation")
		w.lastJWKSDigest = digest
		w.pendingJWKSDigest = digest
	}
	return w.pendingJWKSDigest
}

func (w *GiteaConfigSyncWorker) pendingJWKS() string {
	w.stateMu.Lock()
	defer w.stateMu.Unlock()
	return w.pendingJWKSDigest
}

func (w *GiteaConfigSyncWorker) clearPendingJWKS(digest string) {
	w.stateMu.Lock()
	defer w.stateMu.Unlock()
	if w.pendingJWKSDigest == digest {
		w.pendingJWKSDigest = ""
	}
}

func (w *GiteaConfigSyncWorker) ackFor(serverID string) giteaConfigSyncAck {
	w.stateMu.Lock()
	defer w.stateMu.Unlock()
	return w.acks[serverID]
}

func (w *GiteaConfigSyncWorker) recordQuotaAck(serverID, digest string) {
	w.stateMu.Lock()
	defer w.stateMu.Unlock()
	if w.acks == nil {
		w.acks = make(map[string]giteaConfigSyncAck)
	}
	ack := w.acks[serverID]
	ack.quotaDigest = digest
	w.acks[serverID] = ack
}

func (w *GiteaConfigSyncWorker) recordJWKSAck(serverID, digest string) {
	w.stateMu.Lock()
	defer w.stateMu.Unlock()
	if w.acks == nil {
		w.acks = make(map[string]giteaConfigSyncAck)
	}
	ack := w.acks[serverID]
	ack.jwksDigest = digest
	w.acks[serverID] = ack
}

// giteaQuotaRulesDigest fingerprints a rule snapshot. Never empty, so an
// unacknowledged server (zero-value ack) always differs from it — including
// when the snapshot itself is empty, which is a meaningful instruction ("clear
// every override") and must still be delivered once after a restart.
func giteaQuotaRulesDigest(rules []gitsync.QuotaRule) string {
	if rules == nil {
		rules = []gitsync.QuotaRule{}
	}
	encoded, err := json.Marshal(rules)
	if err != nil {
		// Cannot happen for this struct; a fresh random-ish digest is still the
		// safe direction because it forces a push rather than suppressing one.
		return fmt.Sprintf("unmarshalable-%d", time.Now().UnixNano())
	}
	sum := sha256.Sum256(encoded)
	return hex.EncodeToString(sum[:])
}

// jwksKeyIDDigest fingerprints a key-id set, order-insensitively — key order in
// a JWKS document carries no meaning and must not look like a rotation.
func jwksKeyIDDigest(ids []string) string {
	cleaned := make([]string, 0, len(ids))
	for _, id := range ids {
		if trimmed := strings.TrimSpace(id); trimmed != "" {
			cleaned = append(cleaned, trimmed)
		}
	}
	if len(cleaned) == 0 {
		return ""
	}
	sort.Strings(cleaned)
	sum := sha256.Sum256([]byte(strings.Join(cleaned, "\n")))
	return hex.EncodeToString(sum[:])
}

// parseGiteaInternalToken extracts git_servers.config.internal_token.
func parseGiteaInternalToken(raw string) (string, error) {
	if strings.TrimSpace(raw) == "" {
		return "", nil
	}
	var cfg struct {
		InternalToken string `json:"internal_token"`
	}
	if err := json.Unmarshal([]byte(raw), &cfg); err != nil {
		return "", err
	}
	return strings.TrimSpace(cfg.InternalToken), nil
}

func missingGiteaConfigSyncFields(endpoint, internalToken string) []string {
	missing := make([]string, 0, 2)
	if strings.TrimSpace(endpoint) == "" {
		missing = append(missing, "endpoint")
	}
	if internalToken == "" {
		missing = append(missing, "internal_token")
	}
	return missing
}

// HTTPJWKSKeyIDLister reads a JWKS document over HTTP and reports its key ids.
//
// Only ids are extracted on purpose: this worker's question is "has the key set
// changed", and answering it must not require agreeing with the fork about
// which keys are usable — that judgement is the fork's, and duplicating it here
// would create a second, silently diverging implementation.
type HTTPJWKSKeyIDLister struct {
	URL    string
	Client *http.Client
}

// NewHTTPJWKSKeyIDLister returns nil for an empty URL, which the worker reads
// as "rotation watching not configured".
func NewHTTPJWKSKeyIDLister(rawURL string, timeout time.Duration) *HTTPJWKSKeyIDLister {
	rawURL = strings.TrimSpace(rawURL)
	if rawURL == "" {
		return nil
	}
	if timeout <= 0 {
		timeout = defaultGiteaConfigSyncRequestTimeout
	}
	return &HTTPJWKSKeyIDLister{URL: rawURL, Client: &http.Client{Timeout: timeout}}
}

// ListKeyIDs implements JWKSKeyIDLister.
func (l *HTTPJWKSKeyIDLister) ListKeyIDs(ctx context.Context) ([]string, error) {
	if l == nil || strings.TrimSpace(l.URL) == "" {
		return nil, errors.New("JWKS URL is not configured")
	}
	client := l.Client
	if client == nil {
		client = &http.Client{Timeout: defaultGiteaConfigSyncRequestTimeout}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, l.URL, nil)
	if err != nil {
		return nil, fmt.Errorf("build JWKS request: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetch JWKS: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		// 503 is the issuer's documented "no signing key configured" answer;
		// it is a state, not a crash, and is reported as an ordinary error so
		// the caller keeps whatever it already knows.
		return nil, fmt.Errorf("JWKS endpoint returned %d", resp.StatusCode)
	}
	var doc struct {
		Keys []struct {
			KID string `json:"kid"`
		} `json:"keys"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&doc); err != nil {
		return nil, fmt.Errorf("decode JWKS: %w", err)
	}
	ids := make([]string, 0, len(doc.Keys))
	for _, key := range doc.Keys {
		if key.KID != "" {
			ids = append(ids, key.KID)
		}
	}
	return ids, nil
}
