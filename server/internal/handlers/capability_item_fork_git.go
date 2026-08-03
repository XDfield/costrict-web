// Package handlers — git-backed fork of marketplace plugins.
//
// A plugin fork used to be a pure DB copy (item row + capability_assets +
// child items). For plugins that are mirrored on the tenant's Gitea, the fork
// instead produces a REAL repository under the user's own Gitea namespace, and
// the DB row keeps metadata only (no assets, no child items) with
// content_backend='git' plus the repo coordinate.
//
// Two hard constraints shape this file:
//
//  1. Gitea's CreateForkOption carries only `name` and `organization` — a fork
//     always lands under the *authenticated identity*. There is no "fork into
//     user X" parameter, so the call MUST be signed with the user's own PAT
//     (gitsync.DecryptUserToken / EnsureUserPAT). An admin token cannot do it.
//
//  2. The target namespace is derived server-side from UserGitBinding.
//     git_username (the cs-user ShortID) and is never accepted from the
//     caller — otherwise a user could fork into someone else's namespace.
//
// Failure semantics (design §5): Gitea is contacted BEFORE any DB write, so a
// Gitea failure never leaves a half-made item. The reverse ordering would
// leave items whose repo link 404s. If the DB write fails after a successful
// fork, the orphaned Gitea repo is reused on the next attempt because
// ForkRepo treats "already exists" as success.

package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"os"
	"strings"

	"github.com/costrict/costrict-web/server/internal/gitserver"
	"github.com/costrict/costrict-web/server/internal/gitsync"
	"github.com/costrict/costrict-web/server/internal/models"
	"github.com/gin-gonic/gin"
	"gorm.io/datatypes"
	"gorm.io/gorm"
)

// defaultPluginGitMirrorOwner is the Gitea namespace holding the plugin
// mirrors. Every catalog plugin is mirrored as <owner>/<item slug> —
// metadata.install.marketplace_repo names the UPSTREAM coordinate (usually a
// GitHub org), which only coincides with the mirror for first-party plugins.
// Override with PLUGIN_GIT_MIRROR_OWNER.
const defaultPluginGitMirrorOwner = "costrict-plugins-repo"

// content_backend values. Rows default to contentBackendDB in the DB, so an
// empty value on an in-memory struct means "db" too.
const (
	contentBackendDB  = "db"
	contentBackendGit = "git"
)

// isGitBacked reports whether an item's content lives in a git repository
// instead of the DB (content column + capability_assets).
func isGitBacked(item *models.CapabilityItem) bool {
	return item != nil && item.ContentBackend == contentBackendGit
}

// errNoGiteaMirror signals "this item has no repository on the tenant's Gitea"
// — not a failure, just a plugin that stays on the DB fork path.
var errNoGiteaMirror = errors.New("handlers: no gitea mirror for item")

func pluginGitMirrorOwner() string {
	if v := strings.TrimSpace(os.Getenv("PLUGIN_GIT_MIRROR_OWNER")); v != "" {
		return v
	}
	return defaultPluginGitMirrorOwner
}

// gitForkPlan is the outcome of a successful Gitea fork: the coordinate to
// persist on the new item.
type gitForkPlan struct {
	RepoURL string // normalized <endpoint>/<owner>/<name>
	RepoRef string // branch name
}

// planGitBackedFork performs the Gitea side of a plugin fork and returns the
// coordinate to persist, or:
//
//   - (nil, nil)      → not a git-backed fork; caller runs the legacy DB fork
//     (non-plugin, no marketplace metadata, feature not wired
//     for this tenant, or no mirror repo on Gitea)
//   - (nil, *httpErr) → the fork IS git-backed but failed; caller must abort
//     WITHOUT writing anything to the DB
//
// The (nil, nil) cases are deliberate: they are all "this deployment/item has
// no git backing at all". Two situations are explicitly NOT among them, because
// a DB copy there would look like success while producing nothing usable:
// a user whose Gitea account isn't ready (409), and a source item that is
// already git-backed and therefore has no DB content to copy (503).
func planGitBackedFork(c *gin.Context, userID string, src models.CapabilityItem) (*gitForkPlan, *httpErr) {
	mpOwner, mpRepo, hasMarketplace := parseMarketplaceRepo(src.Metadata)
	// Forking a fork: the source already lives in git, so its own repo is the
	// fork source and the DB holds nothing worth copying.
	srcIsGit := isGitBacked(&src)
	srcRepoOwner, srcRepoName, srcRepoOK := "", "", false
	if srcIsGit {
		srcRepoOwner, srcRepoName, srcRepoOK = splitRepoURL(src.SourceRepoURL)
		if !srcRepoOK {
			return nil, &httpErr{
				status: http.StatusInternalServerError,
				body: gin.H{
					"error":      "this item's repository address is unreadable; contact your platform admin",
					"error_code": "GIT_REPO_COORDINATE_INVALID",
				},
			}
		}
	}
	if src.ItemType != "plugin" || (!hasMarketplace && !srcIsGit) {
		return nil, nil
	}

	// unavailable decides what "git backing can't be used here" means. For a
	// marketplace plugin it means "fall back to the legacy DB fork"; for a
	// source that is ALREADY git-backed a DB fork would produce an item with
	// no content at all, so it must fail instead.
	unavailable := func(reason string) (*gitForkPlan, *httpErr) {
		if !srcIsGit {
			return nil, nil
		}
		return nil, &httpErr{
			status: http.StatusServiceUnavailable,
			body: gin.H{
				"error":      "this item is stored in git and cannot be forked right now: " + reason,
				"error_code": "GIT_BACKING_UNAVAILABLE",
			},
		}
	}

	// Personal-space deps are wired in main.go via InitUserSpaceService; absent
	// them the whole git-backing feature is off.
	if gitsyncDB == nil || gitsyncResolver == nil || gitsyncCrypt == nil {
		return unavailable("git backing is not configured on this deployment")
	}

	ctx := c.Request.Context()
	tenantID := resolveTenantID(c)

	cfg, err := gitsyncResolver.Resolve(ctx, tenantID)
	if err != nil {
		if errors.Is(err, gitserver.ErrTenantMissingGitServer) {
			return unavailable("no git server is bound to this tenant")
		}
		return nil, &httpErr{
			status: http.StatusServiceUnavailable,
			body: gin.H{
				"error":      "git server unavailable: " + err.Error(),
				"error_code": "GIT_SERVER_UNAVAILABLE",
			},
		}
	}

	// Locate the source repo with the ADMIN token: this is a read-only probe
	// and must not depend on the user having a PAT yet.
	adminCli := gitsync.NewClient(cfg.Endpoint, cfg.AdminToken)
	if adminCli == nil {
		return unavailable("git server is not usable")
	}
	candidates := make([]repoCandidate, 0, 3)
	if srcRepoOK {
		// Persisted coordinate — already verified when it was written.
		candidates = append(candidates, repoCandidate{srcRepoOwner, srcRepoName, true})
	}
	if src.Slug != "" {
		candidates = append(candidates, repoCandidate{pluginGitMirrorOwner(), src.Slug, false})
	}
	if hasMarketplace {
		candidates = append(candidates, repoCandidate{mpOwner, mpRepo, false})
	}
	srcOwner, srcName, srcBranch, err := locateGiteaSourceRepo(ctx, adminCli, candidates, pluginNameOf(src.Metadata))
	if err != nil {
		if errors.Is(err, errNoGiteaMirror) {
			return unavailable("its repository was not found on the git server")
		}
		// Reachability/permission failure: fail closed rather than silently
		// producing a DB copy of something that should have been git-backed.
		return nil, &httpErr{
			status: http.StatusBadGateway,
			body: gin.H{
				"error":      "failed to locate the source repository on git server: " + err.Error(),
				"error_code": "GIT_SOURCE_LOOKUP_FAILED",
			},
		}
	}

	// --- From here on the fork IS git-backed: every failure is surfaced. ---

	binding, herr := resolveForkUserBinding(ctx, userID, tenantID)
	if herr != nil {
		return nil, herr
	}

	token, herr := resolveForkUserToken(ctx, cfg, userID, tenantID, binding)
	if herr != nil {
		return nil, herr
	}

	userCli := gitsync.NewUserClient(cfg.Endpoint, token)
	if userCli == nil {
		return nil, &httpErr{
			status: http.StatusInternalServerError,
			body:   gin.H{"error": "failed to build git client for user", "error_code": "GIT_CLIENT_UNAVAILABLE"},
		}
	}

	repo, err := userCli.ForkRepo(ctx, srcOwner, srcName, gitsync.ForkRepoOptions{
		// Target namespace is server-derived; the caller never picks it.
		TargetOwner: binding.GitUsername,
	})
	if err != nil {
		// A name clash with a repo that isn't a fork of this source is not a
		// transient failure: the user's namespace holds something we must not
		// touch, and retrying can never succeed. Give it its own code so the UI
		// can tell them to rename/remove that repo instead of retrying forever.
		if errors.Is(err, gitsync.ErrUsernameTaken) {
			return nil, &httpErr{
				status: http.StatusConflict,
				body: gin.H{
					"error":      "a repository of that name already exists in your git namespace and is not a fork of this item; rename or remove it, then try again",
					"error_code": "GIT_FORK_NAME_TAKEN",
					"repoName":   srcName,
				},
			}
		}
		return nil, &httpErr{
			status: http.StatusBadGateway,
			body: gin.H{
				"error":      "failed to fork repository on git server: " + err.Error(),
				"error_code": "GIT_FORK_FAILED",
			},
		}
	}

	owner, name, ok := splitOwnerRepoPath(repo.FullName)
	if !ok {
		// An unparsable full_name is an untrustworthy response — do NOT fall back
		// to the expected coordinate. Doing so would make the namespace check
		// below compare a value against itself (always true) and then persist a
		// repo URL that was never verified to exist.
		log.Printf("fork: gitea returned unparsable repo full_name %q (item %s)", repo.FullName, src.ID)
		return nil, &httpErr{
			status: http.StatusBadGateway,
			body: gin.H{
				"error":      "git server returned an unreadable repository identity; contact your platform admin",
				"error_code": "GIT_FORK_RESPONSE_INVALID",
			},
		}
	}
	if !strings.EqualFold(owner, binding.GitUsername) {
		// The fork landed outside the caller's namespace — the PAT identity
		// disagrees with the binding. Refuse to record it.
		log.Printf("fork: gitea fork landed in %q but user namespace is %q (item %s)", owner, binding.GitUsername, src.ID)
		return nil, &httpErr{
			status: http.StatusInternalServerError,
			body: gin.H{
				"error":      "fork landed outside your git namespace; contact your platform admin",
				"error_code": "GIT_FORK_NAMESPACE_MISMATCH",
			},
		}
	}

	ref := firstNonEmpty(repo.DefaultBranch, srcBranch, "main")
	return &gitForkPlan{
		// Normalized from the tenant's configured endpoint rather than Gitea's
		// html_url: a misconfigured Gitea ROOT_URL currently reports
		// https://gitea_upstream/... which is not resolvable by clients.
		RepoURL: strings.TrimRight(cfg.Endpoint, "/") + "/" + owner + "/" + name,
		RepoRef: ref,
	}, nil
}

// locateGiteaSourceRepo returns the first candidate coordinate that actually
// exists on the git server, along with its default branch.
//
// Callers order candidates by confidence:
//
//  1. the source item's own repo (forking a fork),
//  2. <mirror owner>/<item slug> — the mirror convention: every catalog plugin
//     is published to the mirror namespace under its own slug,
//  3. metadata.install.marketplace_repo verbatim — correct for first-party
//     plugins whose marketplace repo IS the Gitea repo.
//
// The upstream coordinate alone is not sufficient: for a marketplace hosting
// many plugins (e.g. anthropics/claude-plugins-official) it names a repo
// shared by dozens of items, so the per-item mirror wins when both exist.
//
// Returns errNoGiteaMirror when no candidate exists; any other error is a real
// lookup failure and must not be mistaken for "not mirrored".
func locateGiteaSourceRepo(
	ctx context.Context, cli *gitsync.Client, candidates []repoCandidate, wantPlugin string,
) (owner, name, defaultBranch string, err error) {
	seen := map[string]bool{}
	for _, cand := range candidates {
		if cand.owner == "" || cand.name == "" {
			continue
		}
		key := cand.owner + "/" + cand.name
		if seen[key] {
			continue
		}
		seen[key] = true

		repo, lookupErr := cli.GetRepo(ctx, cand.owner, cand.name)
		if lookupErr != nil {
			return "", "", "", lookupErr
		}
		if repo == nil {
			continue // 404 — try the next candidate
		}
		// A guessed coordinate that exists is not proof it belongs to THIS item:
		// the namespace may hold an unrelated repo of the same name. Confirm via
		// the plugin manifest before forking somebody else's content.
		if !cand.trusted && !repoManifestMatches(ctx, cli, cand.owner, cand.name, repo.DefaultBranch, wantPlugin) {
			continue
		}
		return cand.owner, cand.name, repo.DefaultBranch, nil
	}
	return "", "", "", errNoGiteaMirror
}

// repoCandidate is a coordinate to probe. trusted marks a coordinate we already
// verified and persisted (the item's own source_repo_url); untrusted ones are
// guesses from naming conventions and must be confirmed against the manifest.
type repoCandidate struct {
	owner   string
	name    string
	trusted bool
}

// pluginManifestPaths are the marketplace layouts a mirrored plugin may use.
var pluginManifestPaths = []string{".claude-plugin/plugin.json", ".plugin.json", "plugin.json"}

// repoManifestMatches reports whether the repo's plugin manifest names the
// plugin we're looking for.
//
// Verdict when no manifest can be read: ACCEPT, with a warning. Being strict
// here would break mirrors that use a layout we don't know, turning a working
// fork into a silent DB-copy fallback. The check exists to catch the realistic
// failure — a name collision with a repo that IS a plugin but a different one —
// and that case does yield a readable, mismatching manifest.
func repoManifestMatches(ctx context.Context, cli *gitsync.Client, owner, name, branch, wantPlugin string) bool {
	if wantPlugin == "" {
		return true // nothing to compare against
	}
	if branch == "" {
		branch = "main"
	}
	for _, path := range pluginManifestPaths {
		raw, err := cli.ReadFile(ctx, owner, name, branch, path)
		if err != nil || len(raw) == 0 {
			continue
		}
		var manifest struct {
			Name string `json:"name"`
		}
		if json.Unmarshal(raw, &manifest) != nil || manifest.Name == "" {
			continue
		}
		if strings.EqualFold(strings.TrimSpace(manifest.Name), wantPlugin) {
			return true
		}
		log.Printf("fork: repo %s/%s holds plugin %q, not %q — skipping candidate",
			owner, name, manifest.Name, wantPlugin)
		return false
	}
	log.Printf("fork: repo %s/%s has no readable plugin manifest; accepting %q on name match alone",
		owner, name, wantPlugin)
	return true
}

// pluginNameOf reads metadata.install.plugin_name — the marketplace identifier
// a mirrored repo's manifest should carry.
func pluginNameOf(metadata datatypes.JSON) string {
	if len(metadata) == 0 {
		return ""
	}
	var meta struct {
		Install struct {
			PluginName string `json:"plugin_name"`
		} `json:"install"`
	}
	if json.Unmarshal(metadata, &meta) != nil {
		return ""
	}
	return strings.TrimSpace(meta.Install.PluginName)
}

// splitRepoURL extracts the owner/name pair from a stored repo URL
// (<endpoint>/<owner>/<name>) by taking its last two path segments.
func splitRepoURL(raw string) (owner, name string, ok bool) {
	trimmed := strings.TrimRight(strings.TrimSpace(raw), "/")
	if trimmed == "" {
		return "", "", false
	}
	parts := strings.Split(trimmed, "/")
	if len(parts) < 2 {
		return "", "", false
	}
	return splitOwnerRepoPath(parts[len(parts)-2] + "/" + parts[len(parts)-1])
}

// resolveForkUserBinding loads the caller's Gitea identity. A missing or
// unsynced binding is a 409 with an actionable hint — never a silent fallback
// to the DB fork, which would look like success while producing no repo.
func resolveForkUserBinding(ctx context.Context, userID, tenantID string) (*models.UserGitBinding, *httpErr) {
	var binding models.UserGitBinding
	err := gitsyncDB.WithContext(ctx).
		Where("user_subject_id = ? AND tenant_id = ?", userID, tenantID).
		First(&binding).Error
	if err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, &httpErr{
			status: http.StatusInternalServerError,
			body:   gin.H{"error": "failed to load your git account: " + err.Error(), "error_code": "GIT_BINDING_LOOKUP_FAILED"},
		}
	}
	if errors.Is(err, gorm.ErrRecordNotFound) || binding.GitUsername == "" || binding.SyncStatus != models.GitSyncStatusSynced {
		status := "missing"
		if binding.SyncStatus != "" {
			status = binding.SyncStatus
		}
		return nil, &httpErr{
			status: http.StatusConflict,
			body: gin.H{
				"error":      "your git account is not ready yet (status: " + status + "); sign out and sign in again to trigger provisioning, or contact your platform admin",
				"error_code": "GIT_ACCOUNT_NOT_READY",
				"status":     status,
			},
		}
	}
	return &binding, nil
}

// resolveForkUserToken returns the caller's PAT plaintext, minting one when
// the account was provisioned without credentials. The plaintext is returned
// to the caller for a single API call and MUST NOT be logged or persisted.
func resolveForkUserToken(ctx context.Context, cfg *gitserver.Config, userID, tenantID string, binding *models.UserGitBinding) (string, *httpErr) {
	serverCfg := gitsync.GitServerConfig{
		ServerID:      cfg.ServerID,
		Kind:          cfg.Kind,
		Endpoint:      cfg.Endpoint,
		AdminToken:    cfg.AdminToken,
		AdminUser:     cfg.AdminUser,
		AdminPassword: cfg.AdminPassword,
	}

	token, err := gitsync.DecryptUserToken(ctx, gitsyncDB, gitsyncCrypt, userID)
	if errors.Is(err, gitsync.ErrUserCredentialsMissing) {
		gitUID := int64(0)
		if binding.GitUID != nil {
			gitUID = *binding.GitUID
		}
		if perr := gitsync.EnsureUserPAT(ctx, gitsyncDB, gitsyncCrypt, nil, serverCfg,
			userID, tenantID, binding.GitUsername, gitUID); perr != nil {
			return "", &httpErr{
				status: http.StatusInternalServerError,
				body: gin.H{
					"error":      "failed to issue git credentials: " + perr.Error(),
					"error_code": "GIT_CREDENTIALS_UNAVAILABLE",
				},
			}
		}
		token, err = gitsync.DecryptUserToken(ctx, gitsyncDB, gitsyncCrypt, userID)
	}
	if err != nil || token == "" {
		msg := "git credentials unavailable"
		if err != nil {
			msg += ": " + err.Error()
		}
		return "", &httpErr{
			status: http.StatusInternalServerError,
			body:   gin.H{"error": msg, "error_code": "GIT_CREDENTIALS_UNAVAILABLE"},
		}
	}

	// The PAT must belong to the namespace we're about to fork into; Gitea
	// forks to whoever owns the token, so a mismatched credential row would
	// silently create the repo in another user's namespace.
	meta, metaErr := gitsync.LookupUserMeta(ctx, gitsyncDB, userID)
	if metaErr != nil || meta == nil || !strings.EqualFold(meta.GitUsername, binding.GitUsername) {
		return "", &httpErr{
			status: http.StatusInternalServerError,
			body: gin.H{
				"error":      "git credentials do not match your git namespace; contact your platform admin",
				"error_code": "GIT_CREDENTIALS_MISMATCH",
			},
		}
	}
	return token, nil
}

// parseMarketplaceRepo pulls metadata.install.marketplace_repo and splits it
// into owner / repo. Anything that isn't a clean "<owner>/<repo>" pair (empty,
// full URL, nested path) yields ok=false, which keeps the item on the legacy
// DB fork path.
func parseMarketplaceRepo(metadata datatypes.JSON) (owner, repo string, ok bool) {
	if len(metadata) == 0 {
		return "", "", false
	}
	var meta struct {
		Install struct {
			MarketplaceRepo string `json:"marketplace_repo"`
		} `json:"install"`
	}
	if err := json.Unmarshal(metadata, &meta); err != nil {
		return "", "", false
	}
	return splitOwnerRepoPath(meta.Install.MarketplaceRepo)
}

// splitOwnerRepoPath validates and splits "owner/repo". Rejects path
// traversal, whitespace and anything with a different number of segments.
func splitOwnerRepoPath(raw string) (owner, repo string, ok bool) {
	parts := strings.Split(strings.Trim(strings.TrimSpace(raw), "/"), "/")
	if len(parts) != 2 {
		return "", "", false
	}
	owner, repo = strings.TrimSpace(parts[0]), strings.TrimSpace(strings.TrimSuffix(parts[1], ".git"))
	if owner == "" || repo == "" || owner == "." || owner == ".." || repo == "." || repo == ".." {
		return "", "", false
	}
	if strings.ContainsAny(owner+repo, " \t\r\n\\?#") {
		return "", "", false
	}
	return owner, repo, true
}

// gitArchiveURL builds the provider archive link for a git-backed item
// (<repo>/archive/<ref>.zip — Gitea's convention). Empty repo URL yields an
// empty string so callers can omit the hint.
func gitArchiveURL(repoURL, ref string) string {
	if repoURL == "" {
		return ""
	}
	if ref == "" {
		ref = "main"
	}
	return strings.TrimRight(repoURL, "/") + "/archive/" + ref + ".zip"
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}
