// Package handlers — provisioning a capability repository in the caller's own
// Gitea namespace.
//
// Three entry points need the same thing: "make a repository that already holds
// this capability, and only then create the DB row". Cloud's create-new form,
// a fork whose source is DB-backed (there is no upstream repository to fork),
// and the S6 migration script all go through provisionGitCapabilityRepo so the
// ordering constraint below is derived once instead of three times.
//
// The order is repository → content → verified readable → DB row, and it may
// not be reordered. A row flipped to content_backend='git' before its
// repository holds the content is a row every read path resolves into a 404 at
// the git server, and nothing repairs it: the sync worker reconciles a
// projection from a repository, it never invents content. A repository without
// a row, by contrast, is inert — the next attempt reuses it.
//
// Failure is always reported. There is no silent fallback to the DB channel:
// a DB copy of something that should have been git-backed is exactly the
// "second source of truth" this whole rollout exists to remove.

package handlers

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"regexp"
	"strings"

	"github.com/costrict/costrict-web/server/internal/gitserver"
	"github.com/costrict/costrict-web/server/internal/gitsync"
	"github.com/costrict/costrict-web/server/internal/services"
	"github.com/gin-gonic/gin"
	"gorm.io/datatypes"
)

// gitCapabilityRepoBranch is the default branch every provisioned capability
// repository is created with. Discovery reads the repository's own
// default_branch, so this only fixes what a fresh repository starts as.
const gitCapabilityRepoBranch = "main"

// gitCapabilityOwnershipMarkerPath aliases the shared constant so this file
// reads the same as before. The definition lives in services because the
// read-through asset listing has to exclude exactly the path provisioning
// writes; see services.GitCapabilityOwnershipMarkerPath.
const gitCapabilityOwnershipMarkerPath = services.GitCapabilityOwnershipMarkerPath

// gitCapabilityManifestPath returns the repo-relative path a standalone
// capability repository keeps its top-level manifest at (V4 §5.1).
//
// These names are deliberately NOT contentFilename (registry.go). That function
// names the attachment of an HTTP download and answers "<slug>.md" for
// subagent/command — and a repository file called <slug>.md is invisible to
// classifyGitCapabilityManifest, whose root table only knows skill.md /
// agent.md / command.md / mcp.json. A repository provisioned under the download
// naming would push cleanly and then produce no capability at all. The two
// functions answer different questions and must stay apart; neither may be
// changed into the other.
func gitCapabilityManifestPath(itemType string) (string, bool) {
	switch itemType {
	case "skill":
		return "skill.md", true
	case "subagent":
		return "agent.md", true
	case "command":
		return "command.md", true
	case "mcp":
		return "mcp.json", true
	}
	return "", false
}

// gitCapabilityRepoName validates a slug as a Gitea repository name. Gitea
// accepts alphanumerics plus '-', '_' and '.'; our slugs are already
// slugified, so a rejection here means the caller built one from something
// that was never a slug.
var gitCapabilityRepoName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,99}$`)

// gitCapabilityProvisionSpec describes the repository to create and the
// capability it must contain.
type gitCapabilityProvisionSpec struct {
	ItemType    string
	Slug        string // repository name and item slug — the two stay identical
	Name        string
	Description string
	Category    string
	Version     string
	Tags        []string
	Author      string
	License     string
	// Content, when non-empty, is written to the manifest verbatim. That is the
	// DB-backed fork path: the source text is the contract the device installs
	// byte for byte, so it is never re-serialized. Empty means "generate the
	// skeleton from the fields above".
	Content string
	// WantEntryKey pins which entry of a multi-entry MCP manifest this row owns.
	// Empty asks for the only entry, and a manifest with several then fails
	// rather than picking one at random.
	WantEntryKey string
	// ExtraFiles are the non-manifest files that belong to the same capability
	// (a multi-file skill's references/, assets/, scripts/ …). They go through
	// the same write-then-read-back sequence as the manifest, so a repository is
	// never adopted as complete while part of the tree is missing. Only the S6
	// migration supplies them: the fork path refuses multi-file sources outright
	// because it does not own the whole tree.
	ExtraFiles []GitCapabilityFile
}

// provisionGitCapabilityRepo creates <short_id>/<slug> on the tenant's Gitea,
// writes the capability manifest into it, proves the manifest reads back
// byte-identical, and returns the coordinate to persist.
//
// It writes nothing to capability_items: the caller persists the row only after
// this returns successfully, which is what keeps "flipped but empty" out of
// existence.
//
// Takes a context and a tenant rather than the *gin.Context it used to, because
// the third caller is a CLI: the migration script has no request to derive them
// from, and duplicating the sequence there is exactly the second build path
// this function exists to prevent.
func provisionGitCapabilityRepo(ctx context.Context, tenantID, userID string, spec gitCapabilityProvisionSpec) (*gitForkPlan, *httpErr) {
	manifestPath, supported := gitCapabilityManifestPath(spec.ItemType)
	if !supported {
		return nil, &httpErr{
			status: http.StatusBadRequest,
			body: gin.H{
				"error":      "capabilities of type " + spec.ItemType + " cannot be stored in git yet",
				"error_code": "GIT_ITEM_TYPE_UNSUPPORTED",
			},
		}
	}
	if !gitCapabilityRepoName.MatchString(spec.Slug) {
		return nil, &httpErr{
			status: http.StatusBadRequest,
			body: gin.H{
				"error":      "slug " + spec.Slug + " cannot be used as a repository name; use letters, digits, '-', '_' and '.'",
				"error_code": "GIT_REPO_NAME_INVALID",
			},
		}
	}

	content, err := buildGitCapabilityManifest(spec)
	if err != nil {
		return nil, &httpErr{
			status: http.StatusBadRequest,
			body:   gin.H{"error": err.Error(), "error_code": "GIT_MANIFEST_INVALID"},
		}
	}
	entryKey, err := gitCapabilityEntryKey(spec.ItemType, content, spec.WantEntryKey)
	if err != nil {
		return nil, &httpErr{
			status: http.StatusConflict,
			body:   gin.H{"error": err.Error(), "error_code": "GIT_MANIFEST_AMBIGUOUS"},
		}
	}
	if herr := validateGitCapabilityExtraFiles(spec.ExtraFiles, manifestPath); herr != nil {
		return nil, herr
	}

	cfg, herr := resolveGitBackingConfig(ctx, tenantID)
	if herr != nil {
		return nil, herr
	}
	binding, herr := resolveForkUserBinding(ctx, userID, tenantID)
	if herr != nil {
		return nil, herr
	}
	token, herr := resolveForkUserToken(ctx, cfg, userID, tenantID, binding)
	if herr != nil {
		return nil, herr
	}

	// Repository creation goes through the admin path (POST
	// /admin/users/{owner}/repos), which names its owner explicitly — unlike
	// ForkRepo, which can only ever land on the token's own identity. The
	// content write then uses the user's PAT so the commit is authored by the
	// person who owns the namespace, and so a broken credential surfaces here
	// rather than on their first push.
	adminCli := gitsync.NewClient(cfg.Endpoint, cfg.AdminToken)
	userCli := gitsync.NewUserClient(cfg.Endpoint, token)
	if adminCli == nil || userCli == nil {
		return nil, &httpErr{
			status: http.StatusInternalServerError,
			body:   gin.H{"error": "failed to build git client", "error_code": "GIT_CLIENT_UNAVAILABLE"},
		}
	}

	owner, name := binding.GitUsername, spec.Slug
	repo, created, herr := ensureGitCapabilityRepo(ctx, adminCli, userCli, owner, name, spec, manifestPath)
	if herr != nil {
		return nil, herr
	}
	cleanupCreated := func(failure *httpErr) (*gitForkPlan, *httpErr) {
		if created {
			if err := adminCli.DeleteRepo(context.WithoutCancel(ctx), owner, name); err != nil {
				log.Printf("provision: failed to roll back newly created repository %s/%s: %v", owner, name, err)
			}
		}
		return nil, failure
	}
	branch := firstNonEmpty(repo.DefaultBranch, gitCapabilityRepoBranch)

	if herr := writeAndVerifyGitCapabilityFile(ctx, userCli, owner, name, branch, manifestPath, content,
		"feat("+spec.Slug+"): publish capability manifest"); herr != nil {
		return cleanupCreated(herr)
	}
	// Every other file of the capability goes through the same sequence before
	// the caller is told the repository is usable. Publishing the manifest and
	// reporting success while a reference file is still missing is the silent
	// partial the migration is forbidden to produce.
	for _, file := range spec.ExtraFiles {
		if herr := writeAndVerifyGitCapabilityFile(ctx, userCli, owner, name, branch, file.Path, file.Content,
			"feat("+spec.Slug+"): publish "+file.Path); herr != nil {
			return cleanupCreated(herr)
		}
	}

	warnMissingGitWebhook(ctx, cfg.ServerID, owner, name)

	return &gitForkPlan{
		RepoURL:     gitWebBase(cfg) + "/" + owner + "/" + name,
		RepoRef:     branch,
		RepoPath:    manifestPath,
		GitServerID: cfg.ServerID,
		GitRepoID:   repo.ID,
		EntryKey:    entryKey,
		Content:     string(content),
	}, nil
}

// writeAndVerifyGitCapabilityFile writes one repository file and proves it
// reads back byte-identical.
//
// Read-back is not paranoia. WriteFile reports a pre-existing file as success
// (its conflict branch), so without comparing the bytes a repository that
// already held different content at this path would be adopted as if we had
// just written ours — and every read path would then serve content this row
// never produced. It is also the lineage check on a retry: a second run over a
// repository somebody else's capability already occupies fails here rather than
// binding it.
func writeAndVerifyGitCapabilityFile(
	ctx context.Context, cli *gitsync.Client, owner, name, branch, filePath string,
	content []byte, message string,
) *httpErr {
	if err := cli.WriteFile(ctx, owner, name, branch, filePath, content, message); err != nil {
		return &httpErr{
			status: http.StatusBadGateway,
			body: gin.H{
				"error":      "failed to write " + filePath + " to git: " + err.Error(),
				"error_code": "GIT_REPO_WRITE_FAILED",
				"path":       filePath,
			},
		}
	}
	stored, err := cli.ReadFile(ctx, owner, name, branch, filePath)
	if err != nil {
		return &httpErr{
			status: http.StatusBadGateway,
			body: gin.H{
				"error":      "failed to verify " + filePath + " in git: " + err.Error(),
				"error_code": "GIT_REPO_VERIFY_FAILED",
				"path":       filePath,
			},
		}
	}
	if !bytes.Equal(stored, content) {
		log.Printf("provision: %s/%s file %s did not read back identical (%d bytes stored, %d written)",
			owner, name, filePath, len(stored), len(content))
		return &httpErr{
			status: http.StatusBadGateway,
			body: gin.H{
				"error": "the file " + filePath + " in git does not match what was written; " +
					"the repository already holds different content",
				"error_code": "GIT_REPO_VERIFY_MISMATCH",
				"path":       filePath,
			},
		}
	}
	return nil
}

// validateGitCapabilityExtraFiles refuses a file set that would make the
// repository describe something other than the one capability it is being
// provisioned for.
//
// Two rejections, both about identity rather than safety:
//
//   - a path discovery would classify as a manifest (commands/x.md,
//     skills/y/skill.md, .claude-plugin/plugin.json …) becomes a SECOND
//     capability the next sync pass indexes as its own row. The migration would
//     publish one item and produce several.
//   - the manifest path itself, or a duplicate, means two writers disagree
//     about the same file within a single call; the last write would win
//     silently.
func validateGitCapabilityExtraFiles(files []GitCapabilityFile, manifestPath string) *httpErr {
	if len(files) == 0 {
		return nil
	}
	reject := func(path, reason string) *httpErr {
		return &httpErr{
			status: http.StatusBadRequest,
			body: gin.H{
				"error":      "file " + path + " cannot be published with this capability: " + reason,
				"error_code": "GIT_EXTRA_FILE_REJECTED",
				"path":       path,
			},
		}
	}
	seen := make(map[string]struct{}, len(files))
	for _, file := range files {
		normalized, err := services.NormalizeArchivePath(file.Path)
		if err != nil {
			return reject(file.Path, err.Error())
		}
		if normalized != file.Path {
			return reject(file.Path, "path is not in normal form (expected "+normalized+")")
		}
		if strings.EqualFold(normalized, manifestPath) {
			return reject(file.Path, "it collides with the capability manifest")
		}
		if _, duplicate := seen[normalized]; duplicate {
			return reject(file.Path, "it appears twice in the same capability")
		}
		seen[normalized] = struct{}{}
		if services.IsGitCapabilityManifestPath(normalized) {
			return reject(file.Path, "discovery would index it as a separate capability")
		}
	}
	return nil
}

// resolveGitBackingConfig loads the tenant's git server, mapping every "this
// deployment cannot do git backing" case onto the shared 503 so callers that
// must fail closed and callers that may fall back both see one contract.
func resolveGitBackingConfig(ctx context.Context, tenantID string) (*gitserver.Config, *httpErr) {
	unavailable := func(reason string) *httpErr {
		return &httpErr{
			status: http.StatusServiceUnavailable,
			body: gin.H{
				"error":      "this capability is stored in git and cannot be provisioned right now: " + reason,
				"error_code": "GIT_BACKING_UNAVAILABLE",
			},
		}
	}
	if !gitBackingWired() {
		return nil, unavailable("git backing is not configured on this deployment")
	}
	cfg, err := gitsyncResolver.Resolve(ctx, tenantID)
	if err != nil {
		if errors.Is(err, gitserver.ErrTenantMissingGitServer) {
			return nil, unavailable("no git server is bound to this tenant")
		}
		return nil, &httpErr{
			status: http.StatusServiceUnavailable,
			body: gin.H{
				"error":      "git server unavailable: " + err.Error(),
				"error_code": "GIT_SERVER_UNAVAILABLE",
			},
		}
	}
	if strings.TrimSpace(cfg.ServerID) == "" {
		return nil, &httpErr{
			status: http.StatusServiceUnavailable,
			body: gin.H{
				"error":      "git server is missing its stable server identity; contact your platform admin",
				"error_code": "GIT_SERVER_ID_MISSING",
			},
		}
	}
	return cfg, nil
}

// gitBackingWired reports whether the personal-space dependencies are present.
// Absent them the whole git-backing feature is off for this deployment.
func gitBackingWired() bool {
	return gitsyncDB != nil && gitsyncResolver != nil && gitsyncCrypt != nil
}

// ensureGitCapabilityRepo returns the repository to write into, creating it
// when it does not exist.
//
// An existing repository of that name is the hard case. Retrying after a failed
// write must converge on the same repository, but adopting whatever happens to
// carry the name would let a capability row point at an unrelated project of
// the user's and record it as content truth — the same mistake the fork path's
// lineage check exists to prevent. So an existing repository is reused only
// when it holds no OTHER capability manifest: nothing to shadow, nothing to
// misclaim.
func ensureGitCapabilityRepo(
	ctx context.Context, adminCli, userCli *gitsync.Client, owner, name string,
	spec gitCapabilityProvisionSpec, manifestPath string,
) (*gitsync.Repo, bool, *httpErr) {
	lookupFailed := func(err error) *httpErr {
		return &httpErr{
			status: http.StatusBadGateway,
			body: gin.H{
				"error":      "failed to look up your git namespace: " + err.Error(),
				"error_code": "GIT_REPO_LOOKUP_FAILED",
			},
		}
	}

	existing, err := adminCli.GetRepo(ctx, owner, name)
	if err != nil {
		return nil, false, lookupFailed(err)
	}
	if existing != nil {
		if herr := assertRepoOwnedByCapability(ctx, adminCli, existing, owner, name, spec, manifestPath); herr != nil {
			return nil, false, herr
		}
		return existing, false, nil
	}

	created, err := adminCli.CreateRepo(ctx, owner, gitsync.CreateRepoOptions{
		Name:        name,
		Description: firstNonEmpty(spec.Description, spec.Name),
		// Public: these rows land in the public registry, and the device clones
		// the repository directly. A private repository would publish an address
		// no consumer can read.
		Private: false,
		// AutoInit is false ON PURPOSE, and this is the one call site where the
		// package default (true, see gitsync.CreateRepoOptions) is wrong.
		//
		// Auto-init commits a generated README.md. Nothing ever removes it, and
		// the read-through asset listing has no way to tell a generated README
		// from one the author wrote — so it reports it as an asset and the device
		// installs it. A user who forks a skill gets a README describing the
		// repository, delivered as part of the capability.
		//
		// Not initialising is safe here because this flow writes the first commit
		// itself. Verified against Gitea 1.24.6: creating with auto_init=false and
		// default_branch=main returns default_branch="main" immediately, and the
		// contents API accepts the first file on that not-yet-existing branch
		// (HTTP 201, parentless commit) — including a nested dotfile path like the
		// marker. Only ListTree rejects a still-empty repository (HTTP 400), and
		// nothing reads the tree before the marker write below: the adoption guard
		// reads the marker first and a missing marker already short-circuits it.
		//
		// The alternative — auto-init and then delete the README — needs a new
		// delete-file client call, two extra round trips, and a decision about
		// what to do when the delete fails; every one of those failure modes ends
		// in the leak this comment exists to prevent.
		AutoInit:      false,
		DefaultBranch: gitCapabilityRepoBranch,
	})
	if err != nil {
		// Lost a race with a concurrent create (or with the user). Re-read and
		// apply the same adoption rule rather than failing a retryable request.
		if errors.Is(err, gitsync.ErrGiteaUsernameTaken) {
			raced, lookupErr := adminCli.GetRepo(ctx, owner, name)
			if lookupErr != nil {
				return nil, false, lookupFailed(lookupErr)
			}
			if raced != nil {
				if herr := assertRepoOwnedByCapability(ctx, adminCli, raced, owner, name, spec, manifestPath); herr != nil {
					return nil, false, herr
				}
				return raced, false, nil
			}
		}
		return nil, false, &httpErr{
			status: http.StatusBadGateway,
			body: gin.H{
				"error":      "failed to create the repository on the git server: " + err.Error(),
				"error_code": "GIT_REPO_CREATE_FAILED",
			},
		}
	}
	if created == nil || created.ID <= 0 {
		return nil, false, &httpErr{
			status: http.StatusBadGateway,
			body: gin.H{
				"error":      "git server created the repository without a stable identity; contact your platform admin",
				"error_code": "GIT_REPO_ID_INVALID",
			},
		}
	}
	branch := firstNonEmpty(created.DefaultBranch, gitCapabilityRepoBranch)
	marker, markerErr := gitCapabilityOwnershipMarker(spec, manifestPath)
	if markerErr != nil {
		_ = adminCli.DeleteRepo(context.WithoutCancel(ctx), owner, name)
		return nil, false, &httpErr{status: http.StatusInternalServerError, body: gin.H{
			"error": "failed to build repository ownership marker", "error_code": "GIT_REPO_MARKER_INVALID",
		}}
	}
	if herr := writeAndVerifyGitCapabilityFile(ctx, userCli, owner, name, branch,
		gitCapabilityOwnershipMarkerPath, marker, "chore: mark CoStrict capability ownership"); herr != nil {
		_ = adminCli.DeleteRepo(context.WithoutCancel(ctx), owner, name)
		return nil, false, herr
	}
	return created, true, nil
}

// assertRepoOwnedByCapability refuses every pre-existing repository that cannot
// prove it was provisioned for this exact capability.
func assertRepoOwnedByCapability(
	ctx context.Context, cli *gitsync.Client, repo *gitsync.Repo, owner, name string,
	spec gitCapabilityProvisionSpec, manifestPath string,
) *httpErr {
	taken := func(reason string) *httpErr {
		return &httpErr{
			status: http.StatusConflict,
			body: gin.H{
				"error": "a repository named " + name + " already exists in your git namespace and " + reason +
					"; rename or remove it, then try again",
				"error_code": "GIT_REPO_NAME_TAKEN",
				"repoName":   name,
			},
		}
	}
	if repo.Private {
		return taken("is private; public registry capabilities must remain anonymously readable")
	}
	if !strings.EqualFold(repo.FullName, owner+"/"+name) || !strings.EqualFold(repo.Name, name) {
		return taken("does not match the requested repository identity")
	}
	branch := firstNonEmpty(repo.DefaultBranch, gitCapabilityRepoBranch)
	marker, err := cli.ReadFile(ctx, owner, name, branch, gitCapabilityOwnershipMarkerPath)
	if err != nil {
		return &httpErr{status: http.StatusBadGateway, body: gin.H{
			"error":      "failed to verify the existing repository ownership marker: " + err.Error(),
			"error_code": "GIT_REPO_LOOKUP_FAILED",
		}}
	}
	wantMarker, err := gitCapabilityOwnershipMarker(spec, manifestPath)
	if err != nil || len(marker) == 0 || !bytes.Equal(marker, wantMarker) {
		return taken("does not carry the ownership marker for this capability")
	}
	tree, err := cli.ListTree(ctx, owner, name, branch)
	if err != nil {
		return &httpErr{
			status: http.StatusBadGateway,
			body: gin.H{
				"error":      "failed to inspect the existing repository: " + err.Error(),
				"error_code": "GIT_REPO_LOOKUP_FAILED",
			},
		}
	}
	for _, entry := range tree {
		if entry.Type != "" && !strings.EqualFold(entry.Type, "blob") {
			continue
		}
		if entry.Path == manifestPath {
			continue
		}
		if entry.Path == gitCapabilityOwnershipMarkerPath {
			continue
		}
		if services.IsGitCapabilityManifestPath(entry.Path) {
			return taken("already describes another capability (" + entry.Path + ")")
		}
	}
	return nil
}

func gitCapabilityOwnershipMarker(spec gitCapabilityProvisionSpec, manifestPath string) ([]byte, error) {
	payload := struct {
		Schema       string `json:"schema"`
		ItemType     string `json:"itemType"`
		Slug         string `json:"slug"`
		ManifestPath string `json:"manifestPath"`
	}{
		Schema: "costrict-capability/v1", ItemType: spec.ItemType,
		Slug: spec.Slug, ManifestPath: manifestPath,
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	return append(raw, '\n'), nil
}

// warnMissingGitWebhook logs when the tenant's git server carries no webhook
// secret. The push hook is a SERVER-level system hook, reconciled by the worker
// and covering repositories created later, so provisioning registers nothing
// per repository. Its absence does not break this operation — the first index
// pass is queued explicitly by the caller — but it does mean later edits in
// Gitea will not flow back, which is worth saying out loud rather than
// discovering as a stale item.
func warnMissingGitWebhook(ctx context.Context, serverID, owner, name string) {
	if gitsyncDB == nil {
		return
	}
	var raw string
	if err := gitsyncDB.WithContext(ctx).Table("git_servers").
		Where("server_id = ?", serverID).Select("config").Scan(&raw).Error; err != nil {
		return
	}
	var cfg struct {
		WebhookSecret string `json:"webhook_secret"`
	}
	if json.Unmarshal([]byte(raw), &cfg) != nil {
		return
	}
	if strings.TrimSpace(cfg.WebhookSecret) == "" {
		log.Printf("provision: git server %s has no webhook_secret; edits pushed to %s/%s will not flow back until it is configured",
			serverID, owner, name)
	}
}

// buildGitCapabilityManifest renders the file that goes into the repository.
//
// A caller-supplied Content is written verbatim: it is the source item's text,
// the device installs it byte for byte, and content_md5 is computed over those
// exact bytes. Re-serializing it would drift key order and quoting on every
// round trip and make the sync worker see a change that never happened.
func buildGitCapabilityManifest(spec gitCapabilityProvisionSpec) ([]byte, error) {
	if strings.TrimSpace(spec.Content) != "" {
		return []byte(spec.Content), nil
	}
	if spec.ItemType == "mcp" {
		return buildMCPSkeleton(spec)
	}
	return buildMarkdownSkeleton(spec)
}

// buildMarkdownSkeleton renders the V4 §5.2 frontmatter plus a placeholder
// body. The body is a placeholder on purpose: Cloud registers and discovers,
// Gitea is where the capability is written (user decision U3), so the skeleton
// exists to make the repository self-describing, not to be the finished text.
//
// Field order is fixed so two runs of the same spec produce identical bytes —
// a Go map would not, and an unstable rendering turns every re-provision into
// a spurious diff.
func buildMarkdownSkeleton(spec gitCapabilityProvisionSpec) ([]byte, error) {
	if strings.TrimSpace(spec.Name) == "" {
		return nil, errors.New("name is required")
	}
	var b strings.Builder
	b.WriteString("---\n")
	writeYAMLScalar(&b, "slug", spec.Slug)
	writeYAMLScalar(&b, "type", spec.ItemType)
	writeYAMLScalar(&b, "name", spec.Name)
	if spec.Description != "" {
		writeYAMLScalar(&b, "description", spec.Description)
	}
	if spec.Category != "" {
		writeYAMLScalar(&b, "category", spec.Category)
	}
	writeYAMLScalar(&b, "version", firstNonEmpty(spec.Version, "1.0.0"))
	// tags sit at the TOP level, not under `metadata:` as V4 §5.2 draws them.
	// The parser projects the whole frontmatter map into CapabilityItem.Metadata
	// and reads tags from Metadata["tags"] (parser_service.go, and
	// applyExplicitGitIndexFields for the reconcile pass), so a nested list would
	// be written and then never read — the skeleton would declare tags the
	// platform ignores. The S0 fixture repositories use the top-level form for
	// the same reason. author/license have no parser meaning either way, so they
	// follow the schema.
	if len(spec.Tags) > 0 {
		b.WriteString("tags:\n")
		for _, tag := range spec.Tags {
			b.WriteString("  - ")
			b.WriteString(yamlScalar(tag))
			b.WriteString("\n")
		}
	}
	if spec.Author != "" || spec.License != "" {
		b.WriteString("metadata:\n")
		if spec.Author != "" {
			b.WriteString("  author: " + yamlScalar(spec.Author) + "\n")
		}
		if spec.License != "" {
			b.WriteString("  license: " + yamlScalar(spec.License) + "\n")
		}
	}
	b.WriteString("---\n\n")
	b.WriteString("# " + spec.Name + "\n\n")
	if spec.Description != "" {
		b.WriteString(spec.Description + "\n\n")
	}
	b.WriteString("<!-- Write the capability here, then commit. This repository is the source of truth. -->\n")
	return []byte(b.String()), nil
}

// buildMCPSkeleton renders a single-entry .mcp.json. The map key is the entry
// identity the sync worker matches on (source_git_entry_key), so it is the slug
// and nothing else.
func buildMCPSkeleton(spec gitCapabilityProvisionSpec) ([]byte, error) {
	if strings.TrimSpace(spec.Name) == "" {
		return nil, errors.New("name is required")
	}
	server := map[string]any{
		"name":    spec.Name,
		"command": "",
		"args":    []string{},
	}
	if spec.Description != "" {
		server["description"] = spec.Description
	}
	payload := map[string]any{"mcpServers": map[string]any{spec.Slug: server}}
	raw, err := json.MarshalIndent(payload, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(raw, '\n'), nil
}

func writeYAMLScalar(b *strings.Builder, key, value string) {
	b.WriteString(key)
	b.WriteString(": ")
	b.WriteString(yamlScalar(value))
	b.WriteString("\n")
}

// yamlScalar quotes a value whenever plain style would change its meaning.
// Double quotes with escaped backslashes/quotes are always valid YAML, so the
// rule is "quote unless obviously safe" rather than a full style analysis.
func yamlScalar(value string) string {
	safe := value != "" &&
		!strings.ContainsAny(value, ":#{}[]&*!|>'\"%@`,\n\r\t") &&
		!strings.HasPrefix(value, " ") && !strings.HasSuffix(value, " ") &&
		!strings.HasPrefix(value, "-")
	if safe {
		return value
	}
	escaped := strings.ReplaceAll(value, `\`, `\\`)
	escaped = strings.ReplaceAll(escaped, `"`, `\"`)
	escaped = strings.ReplaceAll(escaped, "\n", `\n`)
	escaped = strings.ReplaceAll(escaped, "\r", `\r`)
	escaped = strings.ReplaceAll(escaped, "\t", `\t`)
	return `"` + escaped + `"`
}

// gitCapabilityEntryKey returns the manifest entry identity to persist in
// source_git_entry_key. Only MCP has one: a .mcp.json describes N servers and
// the sync worker matches a row by (source_repo_path, source_git_entry_key), so
// a wrong or missing key makes the very next push treat the row as removed and
// archive it.
//
// wantKey is the key the source row already carried, if any. It is preferred
// over guessing so a fork of one entry out of a multi-server manifest stays
// bound to that entry.
func gitCapabilityEntryKey(itemType string, content []byte, wantKey string) (string, error) {
	if itemType != "mcp" {
		return "", nil
	}
	var doc struct {
		MCPServers map[string]json.RawMessage `json:"mcpServers"`
	}
	if err := json.Unmarshal(content, &doc); err != nil {
		return "", fmt.Errorf("mcp manifest is not valid JSON: %w", err)
	}
	// No mcpServers block: ParseMCPJSON yields the single "mcp-config" item,
	// whose entry key is empty.
	if len(doc.MCPServers) == 0 {
		return "", nil
	}
	if wantKey != "" {
		if _, ok := doc.MCPServers[wantKey]; ok {
			return wantKey, nil
		}
		return "", fmt.Errorf("mcp manifest does not contain server %q", wantKey)
	}
	if len(doc.MCPServers) > 1 {
		return "", errors.New("mcp manifest describes multiple servers but this item names none; " +
			"split it into one server per capability first")
	}
	for key := range doc.MCPServers {
		return key, nil
	}
	return "", nil
}

// mcpEntryKeyOf reads metadata.key — the entry identity ParseMCPJSON stamps on
// every server it projects, and therefore what a DB-backed MCP row carries.
func mcpEntryKeyOf(metadata datatypes.JSON) string {
	if len(metadata) == 0 {
		return ""
	}
	var meta struct {
		Key string `json:"key"`
	}
	if json.Unmarshal(metadata, &meta) != nil {
		return ""
	}
	return strings.TrimSpace(meta.Key)
}

// parsedCapabilityName reads the capability name a markdown manifest declares.
// Uses the same parser the sync worker uses, so "what the repository says this
// is" cannot drift between the fork guard and the projection.
func parsedCapabilityName(content []byte, sourcePath string) (string, error) {
	parsed, err := (&services.ParserService{}).ParseSKILLMD(content, sourcePath)
	if err != nil {
		return "", err
	}
	if parsed == nil {
		return "", errors.New("manifest produced no capability")
	}
	return strings.TrimSpace(parsed.Name), nil
}
