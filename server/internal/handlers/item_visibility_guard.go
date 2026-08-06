// Package handlers — the authorization gate for DETAIL-scoped capability reads.
//
// The problem this file solves
// ----------------------------
// canAccessItem answers from the local database alone: repositories.visibility
// plus repo_members. For a Git-backed capability that answer is structurally
// stale, because changing a repository's visibility on the deployed Gitea 1.24.6
// emits no webhook at all (measured 2026-08-06). The local row can therefore
// only be refreshed by the periodic reconcile, and between two passes a
// repository that went private still reads as public here — not as a rare race,
// but as the normal state of the row for the whole interval.
//
// Without a live check, `GET /api/items/:id/git-history` (and its siblings)
// answer 200 to an anonymous caller for a repository that is already private,
// handing out the full commit timeline, versions and repository coordinate.
//
// The rule (safe-lifecycle-contract.md, "Authorization And Visibility"; PRD
// LH-7 / AC-LH11 / AC-LH16)
// -------------------------------------------------------------------------
// Before serving Git content, history, or repository coordinates to a caller
// who relies on PUBLIC visibility, verify the repository's current visibility
// on the Git server and fail closed when the check fails or the repository is
// private.
//
// Three deliberate scope limits:
//
//  1. DB-backed rows never reach the Git server. Their behaviour is unchanged,
//     byte for byte — there is no remote visibility to verify.
//  2. Callers authorized LOCALLY — the owner, a member of a private repository,
//     a platform operator — are not gated by the remote check. Their permission
//     does not come from "the repository is public", so a repository going
//     private cannot revoke it.
//  3. Detail reads use a live check. Browse/list reads use the most recent
//     successful reconcile verification instead: applyGitBrowseVisibilityFilter
//     fails closed for public callers when that timestamp is missing or stale,
//     while preserving local owner/member/operator access without per-item
//     Gitea probes.
package handlers

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"strconv"
	"sync"
	"time"

	"github.com/costrict/costrict-web/server/internal/database"
	"github.com/costrict/costrict-web/server/internal/middleware"
	"github.com/costrict/costrict-web/server/internal/models"
	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
)

var (
	// gitVisibilityTTL is how long a VERIFIED-PUBLIC answer may be reused.
	//
	// Not zero: one item detail view fans out into several requests (detail,
	// assets, history, download), every Git-backed row in a repository shares
	// one visibility, and the largest local repository holds 55 of them. Without
	// reuse each of those requests would open its own round trip to the Git
	// server, which turns an authorization check into a load amplifier and makes
	// the Git server's latency the platform's latency.
	//
	// Not long either: the TTL is the exact window in which a repository that
	// has gone private is still served. Thirty seconds is an order of magnitude
	// under the contract's ten-minute browse freshness budget, and two orders
	// under the reconcile interval that would otherwise be the only correction.
	gitVisibilityTTL = envDuration("GIT_VISIBILITY_CACHE_TTL_SECONDS", 30*time.Second)

	// gitVisibilityFailureTTL caches a FAILED verification, briefly.
	//
	// A failure denies the request, so caching it cannot widen exposure — it only
	// stops a Git server that is down from receiving one probe per inbound
	// request per repository while it is down. It is much shorter than the
	// success TTL so recovery is visible almost immediately.
	gitVisibilityFailureTTL = envDuration("GIT_VISIBILITY_FAILURE_TTL_SECONDS", 5*time.Second)

	// gitVisibilityTimeout bounds one probe. The shared Git client's default is
	// 10s, which is a reasonable ceiling for a background sync job and far too
	// long for a gate sitting in front of a user-facing read: a hung Git server
	// would hold every request thread until the client gave up. Four seconds
	// fails closed quickly instead of accumulating requests.
	gitVisibilityTimeout = envDuration("GIT_VISIBILITY_TIMEOUT_SECONDS", 4*time.Second)

	// gitBrowseVisibilityTTL is the maximum age of a successful reconcile
	// verification that public browse/list responses may trust. It is deliberately
	// separate from the detail cache: list pages must not fan out into Gitea
	// probes, and an unavailable reconciler must hide a Git-backed row rather
	// than continue exposing its repository coordinate indefinitely.
	gitBrowseVisibilityTTL = envDuration("GIT_VISIBILITY_BROWSE_FRESHNESS_TTL_SECONDS", 10*time.Minute)
)

// gitVisibilityCacheCap bounds the cache. Entries are per repository, so the
// natural size is "repositories being browsed right now"; the cap only exists so
// a scan over thousands of dead repositories cannot grow the map without limit.
const gitVisibilityCacheCap = 4096

// envDuration reads a whole-second override, falling back to the default for a
// missing, unparseable or non-positive value. A misconfigured deployment gets
// the safe default rather than a zero TTL (which would remove the protection
// against probe amplification) or a zero timeout (which would fail everything).
//
// Read straight from the process environment, like
// services.scan_job_service's own switch: these are operational escape hatches
// for one behaviour, not deployment configuration, and keeping them out of
// internal/config avoids growing the Config struct for a knob nobody sets. The
// consequence is that they must be REAL environment variables — viper's .env
// loading does not populate os.Environ, so a value written only in .env is not
// seen here.
func envDuration(name string, fallback time.Duration) time.Duration {
	seconds, err := strconv.Atoi(os.Getenv(name))
	if err != nil || seconds <= 0 {
		return fallback
	}
	return time.Duration(seconds) * time.Second
}

// applyGitBrowseVisibilityFilter keeps publicly-visible list queries from
// returning a Git-backed item whose remote visibility has not been verified
// recently. It is intentionally a SQL predicate, so callers use the exact same
// filtered relation for Count, pagination, and Find.
//
// A creator, repository member, or platform admin does not rely on public
// visibility. Those callers remain able to browse their own locally-authorized
// rows even while reconcile is delayed. DB-backed rows have no Git visibility
// to verify and keep their existing behaviour.
func applyGitBrowseVisibilityFilter(query *gorm.DB, c *gin.Context, db *gorm.DB) *gorm.DB {
	if query == nil || c == nil {
		return query
	}
	if callerIsPlatformAdmin(c, db) {
		return query
	}

	cutoff := time.Now().Add(-gitBrowseVisibilityTTL)
	userID := c.GetString(middleware.UserIDKey)
	if userID == "" {
		return query.Where(
			"content_backend <> ? OR (git_visibility_verified_at IS NOT NULL AND git_visibility_verified_at >= ?)",
			contentBackendGit, cutoff,
		)
	}

	return query.Where(
		"content_backend <> ? OR (git_visibility_verified_at IS NOT NULL AND git_visibility_verified_at >= ?) OR created_by = ? OR EXISTS (SELECT 1 FROM repo_members WHERE repo_members.repo_id = capability_items.repo_id AND repo_members.user_id = ?)",
		contentBackendGit, cutoff, userID, userID,
	)
}

// gitVisibilityEntry is one repository's verification result. ready is closed
// once public/err are final, so concurrent callers can join an in-flight probe
// instead of starting their own.
type gitVisibilityEntry struct {
	ready   chan struct{}
	public  bool
	err     error
	expires time.Time
}

var (
	gitVisibilityMu    sync.Mutex
	gitVisibilityCache = map[string]*gitVisibilityEntry{}
)

// resetGitVisibilityCache drops every memoized verification. Tests call it
// between cases: the cache key is (git server, numeric repository id), which is
// stable across tests, so a warm entry from one case would otherwise silently
// answer the next one.
func resetGitVisibilityCache() {
	gitVisibilityMu.Lock()
	defer gitVisibilityMu.Unlock()
	gitVisibilityCache = map[string]*gitVisibilityEntry{}
}

// verifyGitRepoIsPublic answers "is this item's repository public right now?",
// memoized per repository and de-duplicated across concurrent callers.
func verifyGitRepoIsPublic(ctx context.Context, item *models.CapabilityItem) (bool, error) {
	key := fmt.Sprintf("%s#%d", item.SourceGitServerID, item.SourceGitRepoID)

	gitVisibilityMu.Lock()
	if entry, ok := gitVisibilityCache[key]; ok {
		select {
		case <-entry.ready:
			if time.Now().Before(entry.expires) {
				gitVisibilityMu.Unlock()
				return entry.public, entry.err
			}
			// Expired: fall through and replace it below.
		default:
			// A probe for this repository is already in flight. Wait for it
			// rather than opening a second one; the whole point of the cache is
			// that 55 capabilities in one repository ask one question.
			gitVisibilityMu.Unlock()
			select {
			case <-entry.ready:
				return entry.public, entry.err
			case <-ctx.Done():
				return false, ctx.Err()
			}
		}
	}
	entry := &gitVisibilityEntry{ready: make(chan struct{})}
	gitVisibilityCache[key] = entry
	pruneGitVisibilityCacheLocked()
	gitVisibilityMu.Unlock()

	// The probe does NOT inherit the caller's cancellation. Other requests are
	// waiting on this one result, so a client that walks away must not cancel
	// the answer they are blocked on. It gets its own bounded deadline instead.
	probeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), gitVisibilityTimeout)
	defer cancel()
	public, err := gitContentService().ItemRepositoryIsPublic(probeCtx, item)

	gitVisibilityMu.Lock()
	entry.public, entry.err = public, err
	if err != nil {
		entry.expires = time.Now().Add(gitVisibilityFailureTTL)
	} else {
		entry.expires = time.Now().Add(gitVisibilityTTL)
	}
	close(entry.ready)
	gitVisibilityMu.Unlock()

	// Logged once per cache miss rather than once per request: an operator needs
	// to see "this repository stopped being public" and "the git server is not
	// answering", and neither is worth a log line on every read. The item's
	// repository coordinate is already operator-visible; nothing secret is
	// logged (no tokens, no webhook secrets, no content).
	switch {
	case err != nil:
		log.Printf("git visibility verification failed for item %s (server=%s repo=%d): %v",
			item.ID, item.SourceGitServerID, item.SourceGitRepoID, err)
	case !public:
		log.Printf("git-backed item %s is hidden from public callers: repository %d on %s is no longer public",
			item.ID, item.SourceGitRepoID, item.SourceGitServerID)
	}
	return public, err
}

// pruneGitVisibilityCacheLocked keeps the map bounded. Expired entries go
// first; if that is not enough the cache is dropped wholesale, which costs one
// extra probe per repository and never serves anything stale.
func pruneGitVisibilityCacheLocked() {
	if len(gitVisibilityCache) <= gitVisibilityCacheCap {
		return
	}
	now := time.Now()
	for key, entry := range gitVisibilityCache {
		select {
		case <-entry.ready:
			if now.After(entry.expires) {
				delete(gitVisibilityCache, key)
			}
		default:
		}
	}
	if len(gitVisibilityCache) > gitVisibilityCacheCap {
		gitVisibilityCache = map[string]*gitVisibilityEntry{}
	}
}

// authorizeItemRead is the single gate for every DETAIL-scoped read of a
// capability: item detail, content download, asset listing/download, version
// listing, Git revision history, and any response carrying the repository
// coordinate.
//
// It returns nil when the caller may be served, and the response to write
// otherwise. Callers must have loaded item with Registry preloaded — the local
// decision needs the registry's repo.
func authorizeItemRead(c *gin.Context, item *models.CapabilityItem) *httpErr {
	userID := c.GetString(middleware.UserIDKey)
	allowed, viaPublicVisibility := itemAccessDecision(item, userID)
	if !allowed {
		// F-24: itemAccessDecision knows only repositories.visibility and
		// repo_members, but two local permissions live elsewhere — the item's
		// creator (capability_items.created_by) and the platform-admin role.
		// Refusing here without consulting them locked owners without a member
		// row, and operators, out of their own locally-private items (LH-7:
		// archived private items are visible to their owner and platform
		// operators). None of these callers rely on the repository being
		// public, so admitting them needs no live probe. A caller none of the
		// checks admit still gets itemAccessRefusal's shape: not-found for a
		// hidden Git-backed row, so existence is not confirmed to strangers.
		if itemCallerIsPrivileged(c, database.GetDB(), item, userID) {
			return nil
		}
		return itemAccessRefusal(item)
	}
	// Locally-private repository: permission came from membership, not from the
	// repository being public, so the remote visibility cannot revoke it.
	// DB-backed rows have no remote visibility at all.
	if !viaPublicVisibility || !isGitBacked(item) {
		return nil
	}

	return requireLivePublicGitRepository(c, item,
		"this item's git server could not confirm who may read it; try again later")
}

// requireLivePublicGitRepository is the live half of the gate: it confirms
// against the Git server that the item's repository is still public, and shapes
// the refusal when it is not.
//
// unverifiedMessage is the human-readable text for "we could not ask" — the
// only part that differs between the read and the fork gate. The error_code and
// the not-public answer are identical everywhere on purpose, so a client cannot
// tell the two call sites apart.
func requireLivePublicGitRepository(c *gin.Context, item *models.CapabilityItem, unverifiedMessage string) *httpErr {
	public, err := verifyGitRepoIsPublic(c.Request.Context(), item)
	if err == nil && public {
		return nil
	}

	// From here the caller cannot be served on the strength of public
	// visibility. Before refusing, resolve whether they hold a permission that
	// does not depend on it — the item's creator, a member of its repository, a
	// platform operator. That resolution costs two queries, which is why it sits
	// here and not at the top: the common path (still public) pays nothing.
	if itemCallerIsPrivileged(c, database.GetDB(), item, c.GetString(middleware.UserIDKey)) {
		return nil
	}

	if err != nil {
		// Fail closed. An unanswered visibility question is not permission to
		// serve: answering from the stale local row would make a Git server
		// outage the most reliable way to read a repository that was just taken
		// private. The body carries no repository coordinate — the caller is
		// unauthorized until proven otherwise, and the coordinate is one of the
		// things being protected.
		status := http.StatusServiceUnavailable
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			status = http.StatusGatewayTimeout
		}
		return &httpErr{
			status: status,
			body:   gin.H{"error": unverifiedMessage, "error_code": "GIT_VISIBILITY_UNVERIFIED"},
		}
	}
	// Not "forbidden": a caller whose only claim was public visibility has no
	// standing to learn that the item exists at all. Same body as a genuine
	// miss, so the two are indistinguishable.
	return &httpErr{status: http.StatusNotFound, body: gin.H{"error": "Item not found"}}
}

// itemAccessRefusal shapes the refusal for a caller the local check rejected.
//
// A Git-backed row in a hidden state (archived by Git lifecycle convergence,
// or by manual moderation) answers not-found instead of forbidden: the contract
// makes archived private items visible to their owner and platform operators
// only, and a 403 would confirm to everyone else that the capability exists.
// Reaching this function already means the caller failed the local check, so
// this only ever converts a 403 into a 404 — it never hides a row from someone
// who could otherwise read it.
//
// DB-backed rows keep the existing 403 verbatim. Their lifecycle is not owned
// by Git and changing their refusal is not part of this contract.
func itemAccessRefusal(item *models.CapabilityItem) *httpErr {
	if isGitBacked(item) && item.Status != "active" {
		return &httpErr{status: http.StatusNotFound, body: gin.H{"error": "Item not found"}}
	}
	return &httpErr{status: http.StatusForbidden, body: gin.H{"error": "You don't have access to this item"}}
}

// authorizeGitBackedFork gates copying a source capability into the caller's
// own namespace.
//
// Forking a Git-backed source forks its REPOSITORY, so a source whose
// repository has gone private would be copied — content and all — into a
// namespace its owner never granted. The local "only public items can be
// forked" rule reads the same stale column every other path does, so it is
// backed by the live check here.
//
// An UNANSWERED probe deliberately does NOT refuse here, unlike the read gate.
// A fork cannot happen without the Git server: the same server has to fork the
// repository, and it does so with the CALLER's personal access token, so Gitea
// applies its own per-user authorization to the copy. When the probe fails, the
// fork therefore fails on its own — with the specific error the fork path
// already reports (GIT_BACKING_UNAVAILABLE and friends), which is more useful
// than replacing it with a generic "could not verify". This gate exists for the
// one case Gitea would NOT catch on its own: a definite "no longer public"
// answer, where the stale local column is the only thing still saying yes.
func authorizeGitBackedFork(c *gin.Context, src *models.CapabilityItem) *httpErr {
	if !isGitBacked(src) {
		return nil
	}
	public, err := verifyGitRepoIsPublic(c.Request.Context(), src)
	if err != nil || public {
		return nil
	}
	if itemCallerIsPrivileged(c, database.GetDB(), src, c.GetString(middleware.UserIDKey)) {
		return nil
	}
	return &httpErr{status: http.StatusNotFound, body: gin.H{"error": "Item not found"}}
}

// itemCallerIsPrivileged reports whether the caller's permission survives the
// repository becoming private: the item's creator, a member of its repository,
// or a platform operator.
//
// Only consulted on the refusal path, so the two extra queries are paid when a
// request is about to be denied — never on the common "still public" read.
func itemCallerIsPrivileged(c *gin.Context, db *gorm.DB, item *models.CapabilityItem, userID string) bool {
	if userID == "" || db == nil || item == nil {
		return false
	}
	if item.CreatedBy == userID {
		return true
	}
	repoID := item.RepoID
	if item.Registry != nil && item.Registry.RepoID != "" {
		repoID = item.Registry.RepoID
	}
	if repoID != "" && repoID != "public" {
		var count int64
		db.Model(&models.RepoMember{}).
			Where("repo_id = ? AND user_id = ?", repoID, userID).
			Count(&count)
		if count > 0 {
			return true
		}
	}
	return callerIsPlatformAdmin(c, db)
}
