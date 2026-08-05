// `migrate capability-to-git` — the manual, per-item path that moves an
// existing DB-backed capability onto Git.
//
// The rollout's rule is "new capabilities are born in Git, existing ones stay
// where they are". This command is the deliberate exception: an operator names
// the items, sees exactly what would happen, and only then asks for it. It is
// not wired into deployment, migration-on-boot, or any scheduler, and it must
// not become so — the default `migrate` path runs unattended on every release,
// and an item silently changing its content backend during a deploy is the one
// outcome nobody could debug.
//
// Everything here is arranged around a single ordering:
//
//	repository → files → verified readable → then content_backend = 'git'
//
// Fail before the flip and the item is untouched: still DB-backed, still
// readable, still installable, and the next run resumes from the repository
// that already exists. Fail after it and the sync worker converges the
// projection from a repository that already holds the content. There is no
// third state, because a row that says "git" while its repository is empty is
// unreadable to every read path and nothing repairs it — the sync worker
// projects content, it never invents it.
//
// The repository half is not implemented here. It is handlers.
// ProvisionCapabilityRepo, the same primitive Cloud's create form and the
// DB-backed fork go through, so the three cannot drift on repository naming,
// manifest naming, or what "verified" means.

package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"strconv"
	"strings"

	"github.com/costrict/costrict-web/server/internal/crypto"
	"github.com/costrict/costrict-web/server/internal/gitserver"
	"github.com/costrict/costrict-web/server/internal/gitsync"
	"github.com/costrict/costrict-web/server/internal/handlers"
	"github.com/costrict/costrict-web/server/internal/models"
	"github.com/costrict/costrict-web/server/internal/services"
	"github.com/costrict/costrict-web/server/internal/storage"
	"gorm.io/gorm"
)

// migratableCapabilityTypes are the types that have a standalone repository
// shape. plugin is absent on purpose: plugins reach Git through the mirror and
// the existing fork path, and a plugin is a tree this command does not own.
// rule has no manifest name in the V4 layout at all.
var migratableCapabilityTypes = []string{"skill", "subagent", "command", "mcp"}

// capabilityToGitDefaultLimit caps a single batch. Migration is per-item, each
// item costs several Gitea round trips and one commit per file, and a run that
// is too large to read the plan of is a run nobody actually reviewed.
const capabilityToGitDefaultLimit = 50

type capabilityToGitOptions struct {
	TenantID string
	Types    []string
	Owner    string
	IDs      []string
	Limit    int
	// Confirm inverts the default. Dry-run is what you get for free; changing
	// data takes a word.
	Confirm bool
	// IncludeCatalog opts into items mirrored from the upstream catalog
	// (catalog_entry_dir set). Their truth is the upstream repository on
	// GitHub, so republishing them into a user's namespace creates a second
	// source for content the catalog will keep re-ingesting.
	IncludeCatalog bool
	// ClearStaleContent switches to the other operation this command owns:
	// emptying the `content` column of rows that are ALREADY Git-backed but
	// still carry a pre-read-through snapshot.
	ClearStaleContent bool
}

func (o capabilityToGitOptions) scoped() bool {
	return len(o.Types) > 0 || o.Owner != "" || len(o.IDs) > 0
}

func parseCapabilityToGitArgs(args []string) (capabilityToGitOptions, error) {
	opts := capabilityToGitOptions{TenantID: "default", Limit: capabilityToGitDefaultLimit}
	for _, arg := range args {
		switch {
		case arg == "--dry-run":
			// Accepted and ignored: dry-run is the default, and rejecting the
			// flag would punish the careful spelling of what already happens.
			opts.Confirm = false
		case arg == "--confirm":
			opts.Confirm = true
		case arg == "--include-catalog":
			opts.IncludeCatalog = true
		case arg == "--clear-stale-content":
			opts.ClearStaleContent = true
		case strings.HasPrefix(arg, "--type="):
			opts.Types = splitCommaList(strings.TrimPrefix(arg, "--type="))
		case strings.HasPrefix(arg, "--owner="):
			opts.Owner = strings.TrimSpace(strings.TrimPrefix(arg, "--owner="))
		case strings.HasPrefix(arg, "--ids="):
			opts.IDs = splitCommaList(strings.TrimPrefix(arg, "--ids="))
		case strings.HasPrefix(arg, "--tenant="):
			opts.TenantID = strings.TrimSpace(strings.TrimPrefix(arg, "--tenant="))
		case strings.HasPrefix(arg, "--limit="):
			n, err := strconv.Atoi(strings.TrimPrefix(arg, "--limit="))
			if err != nil || n < 0 {
				return opts, fmt.Errorf("--limit expects a non-negative integer, got %q", strings.TrimPrefix(arg, "--limit="))
			}
			opts.Limit = n
		default:
			return opts, fmt.Errorf("unknown flag %q", arg)
		}
	}
	if opts.TenantID == "" {
		opts.TenantID = "default"
	}
	for _, t := range opts.Types {
		if !containsString(migratableCapabilityTypes, t) {
			return opts, fmt.Errorf("--type %q is not migratable; choose from %s",
				t, strings.Join(migratableCapabilityTypes, ", "))
		}
	}
	// Refused in dry-run too. "Show me everything" reads as a harmless request
	// and answers with a plan over ten thousand catalog mirrors, which is both
	// useless and the exact selection nobody should be one flag away from
	// executing.
	if !opts.scoped() {
		return opts, errors.New("refusing an unscoped selection: pass at least one of --type=, --owner=, --ids=")
	}
	return opts, nil
}

// gitRepoInspector is the read-only Git surface the plan needs. Every call is a
// GET: the dry-run predicts what provisioning will find without creating it.
type gitRepoInspector interface {
	GetRepo(ctx context.Context, owner, name string) (*gitsync.Repo, error)
	ListTree(ctx context.Context, owner, repo, ref string) ([]gitsync.GitTreeEntry, error)
	ReadFile(ctx context.Context, owner, repo, branch, path string) ([]byte, error)
}

// capabilityRepoProvisioner is handlers.ProvisionCapabilityRepo behind an
// interface, so the ordering guarantees can be tested against an injected
// failure at each step without a Git server.
type capabilityRepoProvisioner interface {
	Provision(ctx context.Context, tenantID, userID string, req handlers.GitCapabilityProvisionRequest) (*handlers.GitCapabilityRepoCoordinate, error)
}

// blobLoader fetches an asset whose bytes live in object storage rather than in
// the row. Nil means storage is not configured for this run, which turns any
// item that needs it into a blocked item rather than a partial migration.
type blobLoader interface {
	Load(ctx context.Context, key string) ([]byte, error)
}

// gitContentReader reads a Git-backed row's manifest through the same
// read-through path the API serves. Used only by --clear-stale-content, where
// it answers the one question that makes the delete safe: does the repository
// already serve this row?
type gitContentReader interface {
	ItemContent(ctx context.Context, item *models.CapabilityItem) (string, error)
}

type capabilityToGitDeps struct {
	DB          *gorm.DB
	Inspector   gitRepoInspector
	Provisioner capabilityRepoProvisioner
	Blobs       blobLoader
	GitContent  gitContentReader
	Out         io.Writer
}

// capabilityMigrationPlan is one item's complete answer, computed without
// writing anything. Blockers are the reason it will not be attempted; notes are
// what the operator should know before saying yes.
type capabilityMigrationPlan struct {
	Item         models.CapabilityItem
	Owner        string
	ManifestPath string
	// Files are the non-manifest repository files, already materialized. They
	// are read during planning on purpose: an asset that cannot be read is a
	// blocker, and discovering that after the repository exists would leave a
	// half-published tree behind.
	Files      []handlers.GitCapabilityFile
	AssetBytes int
	Blockers   []string
	Notes      []string
}

func (p *capabilityMigrationPlan) block(format string, args ...any) {
	p.Blockers = append(p.Blockers, fmt.Sprintf(format, args...))
}

func (p *capabilityMigrationPlan) note(format string, args ...any) {
	p.Notes = append(p.Notes, fmt.Sprintf(format, args...))
}

type capabilityToGitSummary struct {
	Planned  int
	Blocked  int
	Migrated int
	Skipped  int
	Failed   int
}

// runCapabilityToGit is the whole command: select, plan, print, and — only with
// --confirm — execute.
func runCapabilityToGit(ctx context.Context, deps capabilityToGitDeps, opts capabilityToGitOptions) (capabilityToGitSummary, error) {
	out := deps.Out
	if out == nil {
		return capabilityToGitSummary{}, errors.New("capability-to-git: no output writer")
	}
	if opts.ClearStaleContent {
		return runClearStaleGitContent(ctx, deps, opts)
	}

	items, err := selectCapabilitiesForGitMigration(deps.DB, opts)
	if err != nil {
		return capabilityToGitSummary{}, err
	}

	mode := "DRY RUN (no writes)"
	if opts.Confirm {
		mode = "EXECUTE"
	}
	fmt.Fprintf(out, "capability-to-git — %s\n", mode)
	fmt.Fprintf(out, "  tenant=%s types=%s owner=%s ids=%d limit=%d include-catalog=%v\n",
		opts.TenantID, orNone(strings.Join(opts.Types, ",")), orNone(opts.Owner), len(opts.IDs), opts.Limit, opts.IncludeCatalog)
	fmt.Fprintf(out, "  selected %d DB-backed item(s)\n\n", len(items))
	if len(items) == 0 {
		return capabilityToGitSummary{}, nil
	}

	plans := make([]*capabilityMigrationPlan, 0, len(items))
	// Two items of the same owner claiming the same slug would race for one
	// repository name; whichever ran second would adopt the first one's
	// repository or be refused by it. Detected here so the plan says so.
	claimed := map[string]string{}
	for i := range items {
		plan := planCapabilityMigration(ctx, deps, opts, items[i])
		if plan.Owner != "" {
			key := plan.Owner + "/" + plan.Item.Slug
			if other, taken := claimed[key]; taken {
				plan.block("slug conflict: item %s already claims %s in this batch", other, key)
			} else {
				claimed[key] = plan.Item.ID
			}
		}
		plans = append(plans, plan)
	}

	summary := capabilityToGitSummary{}
	for _, plan := range plans {
		printCapabilityMigrationPlan(out, plan)
		if len(plan.Blockers) > 0 {
			summary.Blocked++
			continue
		}
		summary.Planned++
	}

	if !opts.Confirm {
		fmt.Fprintf(out, "\nplan: %d migratable, %d blocked. Nothing was written.\n", summary.Planned, summary.Blocked)
		fmt.Fprintf(out, "Re-run with --confirm to execute.\n")
		return summary, nil
	}

	fmt.Fprintf(out, "\n--- executing ---\n")
	failures := make([]string, 0)
	for _, plan := range plans {
		if len(plan.Blockers) > 0 {
			continue
		}
		outcome, err := migrateCapabilityToGit(ctx, deps, opts, plan)
		switch {
		case err != nil:
			summary.Failed++
			failures = append(failures, fmt.Sprintf("%s (%s): %v", plan.Item.ID, plan.Item.Slug, err))
			fmt.Fprintf(out, "  FAILED  %s %s: %v\n", plan.Item.Slug, plan.Item.ID, err)
		case outcome == "skipped":
			summary.Skipped++
			fmt.Fprintf(out, "  SKIPPED %s %s (already git-backed)\n", plan.Item.Slug, plan.Item.ID)
		default:
			summary.Migrated++
			fmt.Fprintf(out, "  OK      %s %s -> %s\n", plan.Item.Slug, plan.Item.ID, outcome)
		}
	}

	fmt.Fprintf(out, "\nsummary: migrated=%d skipped=%d blocked=%d failed=%d\n",
		summary.Migrated, summary.Skipped, summary.Blocked, summary.Failed)
	if len(failures) > 0 {
		// A failure list, not a stop-on-first: one unreachable repository must
		// not strand the rest of a reviewed batch.
		return summary, fmt.Errorf("%d item(s) failed to migrate:\n  %s", len(failures), strings.Join(failures, "\n  "))
	}
	return summary, nil
}

// selectCapabilitiesForGitMigration resolves the flags into rows.
//
// Only DB-backed rows are candidates. An already-Git-backed row is not
// "migrated again": it is not selected at all, which is what makes a re-run of
// the same command a no-op instead of a second provisioning pass.
func selectCapabilitiesForGitMigration(db *gorm.DB, opts capabilityToGitOptions) ([]models.CapabilityItem, error) {
	query := db.Model(&models.CapabilityItem{}).
		Where("content_backend = ?", models.ContentBackendDB).
		Where("item_type IN ?", migratableCapabilityTypes)

	if len(opts.Types) > 0 {
		query = query.Where("item_type IN ?", opts.Types)
	}
	if len(opts.IDs) > 0 {
		query = query.Where("id IN ?", opts.IDs)
	}
	if opts.Owner != "" {
		subjects, err := resolveOwnerSubjects(db, opts.TenantID, opts.Owner)
		if err != nil {
			return nil, err
		}
		query = query.Where("created_by IN ?", subjects)
	}
	if !opts.IncludeCatalog {
		// catalog_entry_dir is what the upstream catalog ingest stamps on the
		// rows it owns. Those rows are re-reconciled on every ingest pass from a
		// bundle built out of GitHub, so publishing one into a user's namespace
		// creates a second writer for content that is not theirs.
		query = query.Where("COALESCE(catalog_entry_dir, '') = ?", "")
	}
	query = query.Order("item_type ASC, slug ASC")
	if opts.Limit > 0 {
		query = query.Limit(opts.Limit)
	}

	var items []models.CapabilityItem
	if err := query.Find(&items).Error; err != nil {
		return nil, fmt.Errorf("select capability items: %w", err)
	}
	return items, nil
}

// resolveOwnerSubjects turns --owner into the subject ids that created items.
//
// Accepts either spelling of the same person: the Gitea short_id the repository
// namespace is named after (what an operator reads off a repository URL) or the
// raw subject id stored in created_by. An owner with no binding still resolves —
// to itself — so a dry-run can report "no git binding" instead of "no items".
func resolveOwnerSubjects(db *gorm.DB, tenantID, owner string) ([]string, error) {
	var bindings []models.UserGitBinding
	err := db.Where("tenant_id = ? AND (git_username = ? OR user_subject_id = ?)", tenantID, owner, owner).
		Find(&bindings).Error
	if err != nil {
		return nil, fmt.Errorf("resolve owner %q: %w", owner, err)
	}
	subjects := make([]string, 0, len(bindings)+1)
	for _, binding := range bindings {
		subjects = append(subjects, binding.UserSubjectID)
	}
	if !containsString(subjects, owner) {
		subjects = append(subjects, owner)
	}
	return subjects, nil
}

// planCapabilityMigration answers everything about one item without writing.
//
// It deliberately collects every blocker rather than returning the first: the
// operator is reading this to decide whether to run, and a plan that reveals
// its problems one run at a time is worse than no plan.
func planCapabilityMigration(
	ctx context.Context, deps capabilityToGitDeps, opts capabilityToGitOptions, item models.CapabilityItem,
) *capabilityMigrationPlan {
	plan := &capabilityMigrationPlan{Item: item}

	manifestPath, supported := handlers.GitCapabilityManifestPath(item.ItemType)
	if !supported {
		plan.block("item type %q has no repository manifest name", item.ItemType)
		return plan
	}
	plan.ManifestPath = manifestPath

	if strings.TrimSpace(item.Content) == "" {
		plan.block("row has no content to publish")
	}

	binding, err := loadReadyGitBinding(deps.DB, item.CreatedBy, opts.TenantID)
	switch {
	case err != nil:
		plan.block("%v", err)
	default:
		plan.Owner = binding.GitUsername
	}

	if strings.TrimSpace(item.CatalogEntryDir) != "" {
		plan.note("catalog-mirrored item (catalog_entry_dir=%s); the upstream catalog remains its other writer",
			item.CatalogEntryDir)
	}

	files, assetBlockers, assetNotes := loadCapabilityAssetFiles(ctx, deps, item, manifestPath)
	plan.Files = files
	for _, blocker := range assetBlockers {
		plan.Blockers = append(plan.Blockers, blocker)
	}
	plan.Notes = append(plan.Notes, assetNotes...)
	for _, file := range files {
		plan.AssetBytes += len(file.Content)
	}

	if plan.Owner != "" {
		inspectTargetRepo(ctx, deps, plan)
	}
	return plan
}

// loadReadyGitBinding returns the Gitea namespace an item's creator owns.
//
// Account creation is driven by the cs-user provisioning reconciler, never by
// this command: a short_id invented here would name a namespace that does not
// exist and cannot be reconciled with the one that eventually does.
func loadReadyGitBinding(db *gorm.DB, subjectID, tenantID string) (*models.UserGitBinding, error) {
	if strings.TrimSpace(subjectID) == "" {
		return nil, errors.New("row has no created_by, so it has no git namespace to publish into")
	}
	var binding models.UserGitBinding
	err := db.Where("user_subject_id = ? AND tenant_id = ?", subjectID, tenantID).First(&binding).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, fmt.Errorf("owner %s has no git binding in tenant %s", subjectID, tenantID)
	}
	if err != nil {
		return nil, fmt.Errorf("load git binding for %s: %w", subjectID, err)
	}
	if binding.GitUsername == "" || binding.SyncStatus != models.GitSyncStatusSynced {
		return nil, fmt.Errorf("owner %s has a git binding that is not ready (status=%s)", subjectID, orNone(binding.SyncStatus))
	}
	return &binding, nil
}

// loadCapabilityAssetFiles materializes the item's non-manifest files.
//
// This is where the multi-file promise is kept or explicitly broken. An asset
// whose bytes cannot be produced — no inline text, no reachable storage — is a
// blocker for the whole item. Publishing the rest and moving on would be the
// silent partial migration this command exists to make impossible: the row
// would flip, the API would stop serving capability_assets (a Git-backed row
// answers assetsBackend="git" with an empty list), and the missing file would
// have no remaining copy anywhere a reader looks.
func loadCapabilityAssetFiles(
	ctx context.Context, deps capabilityToGitDeps, item models.CapabilityItem, manifestPath string,
) (files []handlers.GitCapabilityFile, blockers []string, notes []string) {
	var assets []models.CapabilityAsset
	if err := deps.DB.Where("item_id = ?", item.ID).Order("rel_path ASC").Find(&assets).Error; err != nil {
		return nil, []string{fmt.Sprintf("load assets: %v", err)}, nil
	}
	if len(assets) == 0 {
		return nil, nil, nil
	}

	notes = append(notes, fmt.Sprintf("multi-file item: %d asset file(s) will be committed to the repository. "+
		"After the flip, GET /items/{id}/assets answers assetsBackend=\"git\" with an empty list and the "+
		"download endpoint serves only %s — the files live in Git and are reached by cloning it",
		len(assets), manifestPath))

	files = make([]handlers.GitCapabilityFile, 0, len(assets))
	for _, asset := range assets {
		relPath, err := services.NormalizeArchivePath(asset.RelPath)
		if err != nil {
			blockers = append(blockers, fmt.Sprintf("asset %q has an unusable path: %v", asset.RelPath, err))
			continue
		}
		content, err := readCapabilityAssetBytes(ctx, deps, asset)
		if err != nil {
			blockers = append(blockers, fmt.Sprintf("asset %q: %v", relPath, err))
			continue
		}
		files = append(files, handlers.GitCapabilityFile{Path: relPath, Content: content})
	}
	// The provisioning primitive rejects paths that would make the repository
	// describe a second capability, or collide with the manifest. Running the
	// same check here means the plan says so before anything is created.
	if herr := handlers.ValidateGitCapabilityFiles(files, manifestPath); herr != nil {
		blockers = append(blockers, herr.Error())
	}
	return files, blockers, notes
}

func readCapabilityAssetBytes(ctx context.Context, deps capabilityToGitDeps, asset models.CapabilityAsset) ([]byte, error) {
	if asset.TextContent != nil {
		return []byte(*asset.TextContent), nil
	}
	key := strings.TrimSpace(asset.StorageKey)
	if key == "" {
		return nil, fmt.Errorf("has neither inline text nor a storage key (backend=%s)", orNone(asset.StorageBackend))
	}
	if deps.Blobs == nil {
		return nil, fmt.Errorf("lives in object storage (key=%s) but no storage backend is configured for this run", key)
	}
	content, err := deps.Blobs.Load(ctx, key)
	if err != nil {
		return nil, fmt.Errorf("could not be read from storage (key=%s): %w", key, err)
	}
	return content, nil
}

// inspectTargetRepo reports what provisioning will find at <owner>/<slug>.
//
// An existing repository is not automatically a problem — a previous run that
// died between the write and the flip leaves exactly that, and resuming onto it
// is the intended behaviour. It IS a problem when it belongs to something else,
// which is the distinction that keeps one user's repository from being recorded
// as another item's content truth.
func inspectTargetRepo(ctx context.Context, deps capabilityToGitDeps, plan *capabilityMigrationPlan) {
	if deps.Inspector == nil {
		plan.note("target repository not inspected (no git server configured for this run)")
		return
	}
	owner, name := plan.Owner, plan.Item.Slug
	repo, err := deps.Inspector.GetRepo(ctx, owner, name)
	if err != nil {
		plan.block("cannot inspect %s/%s: %v", owner, name, err)
		return
	}
	if repo == nil {
		plan.note("target %s/%s does not exist yet and will be created", owner, name)
		return
	}
	branch := repo.DefaultBranch
	if branch == "" {
		branch = "main"
	}
	tree, err := deps.Inspector.ListTree(ctx, owner, name, branch)
	if err != nil {
		// An empty repository has no tree. Anything else is unknown territory
		// and must not be read as "free".
		if strings.Contains(err.Error(), "status=404") || strings.Contains(err.Error(), "status=409") {
			plan.note("target %s/%s exists and is empty; it will be reused", owner, name)
			return
		}
		plan.block("cannot list %s/%s: %v", owner, name, err)
		return
	}
	for _, entry := range tree {
		if entry.Type != "" && !strings.EqualFold(entry.Type, "blob") {
			continue
		}
		if entry.Path == plan.ManifestPath {
			continue
		}
		if services.IsGitCapabilityManifestPath(entry.Path) {
			plan.block("target %s/%s already describes another capability (%s)", owner, name, entry.Path)
			return
		}
	}
	stored, err := deps.Inspector.ReadFile(ctx, owner, name, branch, plan.ManifestPath)
	if err != nil {
		plan.block("cannot read %s/%s:%s: %v", owner, name, plan.ManifestPath, err)
		return
	}
	switch {
	case stored == nil:
		plan.note("target %s/%s exists without %s; it will be reused", owner, name, plan.ManifestPath)
	case string(stored) == plan.Item.Content:
		plan.note("target %s/%s already holds identical content; this run only completes the flip", owner, name)
	default:
		plan.block("target %s/%s already holds DIFFERENT content at %s (%d bytes there, %d here); "+
			"rename or remove it, or migrate under a different slug",
			owner, name, plan.ManifestPath, len(stored), len(plan.Item.Content))
	}
}

func printCapabilityMigrationPlan(out io.Writer, plan *capabilityMigrationPlan) {
	item := plan.Item
	target := "?/" + item.Slug
	if plan.Owner != "" {
		target = plan.Owner + "/" + item.Slug
	}
	status := "MIGRATE"
	if len(plan.Blockers) > 0 {
		status = "BLOCKED"
	}
	fmt.Fprintf(out, "[%s] %s %s\n", status, item.ItemType, item.ID)
	fmt.Fprintf(out, "    slug=%s owner=%s target=%s/%s\n", item.Slug, orNone(item.CreatedBy), target, plan.ManifestPath)
	fmt.Fprintf(out, "    content=%d bytes, assets=%d file(s)/%d bytes\n",
		len([]byte(item.Content)), len(plan.Files), plan.AssetBytes)
	for _, note := range plan.Notes {
		fmt.Fprintf(out, "    note: %s\n", note)
	}
	for _, blocker := range plan.Blockers {
		fmt.Fprintf(out, "    BLOCKER: %s\n", blocker)
	}
}

// migrateCapabilityToGit performs one item's migration.
//
// Returns the repository URL on success, or "skipped" when the row turned out
// to be Git-backed already. The ordering is the whole point and reads top to
// bottom: provisioning either returns a repository that demonstrably holds
// every byte of this capability, or it returns an error and the row is not
// touched.
func migrateCapabilityToGit(
	ctx context.Context, deps capabilityToGitDeps, opts capabilityToGitOptions, plan *capabilityMigrationPlan,
) (string, error) {
	item := plan.Item

	// Re-read under the current state rather than trusting the planning
	// snapshot: between planning and here, another operator (or a fork) may
	// have moved this row.
	var current models.CapabilityItem
	if err := deps.DB.First(&current, "id = ?", item.ID).Error; err != nil {
		return "", fmt.Errorf("reload item: %w", err)
	}
	if current.ContentBackend != models.ContentBackendDB {
		return "skipped", nil
	}
	if current.Content != item.Content {
		return "", errors.New("content changed since the plan was computed; re-run the dry-run")
	}

	wantEntryKey := ""
	if item.ItemType == "mcp" {
		wantEntryKey = current.SourceGitEntryKey
	}

	coord, provErr := deps.Provisioner.Provision(ctx, opts.TenantID, current.CreatedBy, handlers.GitCapabilityProvisionRequest{
		ItemType:    current.ItemType,
		Slug:        current.Slug,
		Name:        current.Name,
		Description: current.Description,
		Category:    current.Category,
		Version:     current.Version,
		// Verbatim. The bytes in the row are what the device installs and what
		// content_md5 was computed over; re-rendering them from the projected
		// columns would drop every frontmatter field that has no column
		// (allowed-tools, hooks, disable-model-invocation) and make each later
		// sync see a change that never happened.
		Content:      current.Content,
		WantEntryKey: wantEntryKey,
		ExtraFiles:   plan.Files,
	})
	if provErr != nil {
		return "", provErr
	}

	if err := flipCapabilityToGit(deps.DB, current, coord); err != nil {
		return "", err
	}

	// The repository and the row now agree. Queueing the first index pass is
	// best-effort from here: provisioning produced commits but no webhook
	// delivery, and without a job the row would sit at git_sync_status='pending'
	// with an empty git_sha, which the Marketplace projection omits.
	handlers.EnqueueInitialGitCapabilitySync(deps.DB, current.ID, coord)
	return coord.RepoURL, nil
}

// flipCapabilityToGit is the commit point: one UPDATE, an explicit column list,
// and `content_backend = 'db'` in the predicate.
//
// The predicate is a compare-and-set, not a filter. It makes a concurrent
// second runner (or a retry racing itself) affect zero rows instead of
// re-pointing a row that already found its repository. And the column list is
// explicit because db.Save on this struct would write every column, including
// the Git coordinate zero values on any row it did not mean to touch.
//
// No capability_versions row is created. A Git-backed row's version anchor is
// its commit; revisions belong to the DB-backed editing flow this row is
// leaving.
func flipCapabilityToGit(db *gorm.DB, item models.CapabilityItem, coord *handlers.GitCapabilityRepoCoordinate) error {
	result := db.Model(&models.CapabilityItem{}).
		Where("id = ? AND content_backend = ?", item.ID, models.ContentBackendDB).
		Updates(map[string]any{
			// The column is emptied, not left behind. A Git-backed row whose
			// `content` still holds a snapshot is a second source of truth that
			// nothing keeps current, and every reader that falls back to it
			// serves arbitrarily old bytes while looking like success.
			"content": "",
			// SHA-256 over the bytes the repository actually returned, derived
			// the same way discovery derives it, so a later consistency check
			// compares like with like.
			"content_md5":          services.HashGitCapabilityContent(item.ItemType, coord.RepoPath, coord.Content),
			"content_backend":      models.ContentBackendGit,
			"source_path":          coord.RepoPath,
			"source_repo_url":      coord.RepoURL,
			"source_repo_ref":      coord.RepoRef,
			"source_repo_path":     coord.RepoPath,
			"source_git_server_id": coord.GitServerID,
			"source_git_repo_id":   coord.GitRepoID,
			"source_git_entry_key": coord.EntryKey,
			"git_sync_status":      "pending",
			"git_sync_error":       "",
		})
	if result.Error != nil {
		return fmt.Errorf("flip content_backend: %w", result.Error)
	}
	if result.RowsAffected == 0 {
		return errors.New("flip content_backend: row is no longer DB-backed; the repository is provisioned and a re-run will reconcile")
	}
	return nil
}

// runClearStaleGitContent empties the `content` column of rows that are already
// Git-backed but still carry a snapshot from before read-through existed.
//
// Same ordering discipline as the migration, for the same reason: prove the
// replacement is readable before removing what it replaces. Each row's manifest
// is fetched through the read-through service — the exact path the API serves —
// and only a row that answers is cleared. A row whose repository cannot be read
// keeps its stale copy and is reported, because a stale copy is recoverable and
// a deleted one is not.
func runClearStaleGitContent(ctx context.Context, deps capabilityToGitDeps, opts capabilityToGitOptions) (capabilityToGitSummary, error) {
	out := deps.Out
	mode := "DRY RUN (no writes)"
	if opts.Confirm {
		mode = "EXECUTE"
	}
	fmt.Fprintf(out, "capability-to-git --clear-stale-content — %s\n", mode)

	query := deps.DB.Model(&models.CapabilityItem{}).
		Where("content_backend = ?", models.ContentBackendGit).
		Where("COALESCE(content, '') <> ?", "")
	if len(opts.Types) > 0 {
		query = query.Where("item_type IN ?", opts.Types)
	}
	if len(opts.IDs) > 0 {
		query = query.Where("id IN ?", opts.IDs)
	}
	if opts.Owner != "" {
		subjects, err := resolveOwnerSubjects(deps.DB, opts.TenantID, opts.Owner)
		if err != nil {
			return capabilityToGitSummary{}, err
		}
		query = query.Where("created_by IN ?", subjects)
	}
	query = query.Order("item_type ASC, slug ASC")
	if opts.Limit > 0 {
		query = query.Limit(opts.Limit)
	}

	var items []models.CapabilityItem
	if err := query.Find(&items).Error; err != nil {
		return capabilityToGitSummary{}, fmt.Errorf("select git-backed items with stale content: %w", err)
	}
	fmt.Fprintf(out, "  %d git-backed row(s) still carry a content snapshot\n\n", len(items))

	summary := capabilityToGitSummary{}
	failures := make([]string, 0)
	for i := range items {
		item := items[i]
		fmt.Fprintf(out, "[%s] %s %s (%d stale bytes)\n", item.ItemType, item.Slug, item.ID, len([]byte(item.Content)))
		if deps.GitContent == nil {
			summary.Blocked++
			fmt.Fprintf(out, "    BLOCKER: read-through is not configured for this run\n")
			continue
		}
		live, err := deps.GitContent.ItemContent(ctx, &item)
		if err != nil {
			summary.Blocked++
			fmt.Fprintf(out, "    BLOCKER: repository does not serve this row: %v\n", err)
			continue
		}
		if live != item.Content {
			// Expected, and worth saying: the repository has moved on since the
			// snapshot was taken. It is still the truth — that is the point —
			// but an operator should see that the bytes being discarded are not
			// the bytes being served.
			fmt.Fprintf(out, "    note: repository content differs from the stale snapshot (%d vs %d bytes)\n",
				len([]byte(live)), len([]byte(item.Content)))
		}
		summary.Planned++
		if !opts.Confirm {
			continue
		}
		if err := clearStaleGitContent(deps.DB, item, live); err != nil {
			summary.Planned--
			summary.Failed++
			failures = append(failures, fmt.Sprintf("%s (%s): %v", item.ID, item.Slug, err))
			fmt.Fprintf(out, "    FAILED: %v\n", err)
			continue
		}
		summary.Migrated++
		fmt.Fprintf(out, "    OK: content column cleared\n")
	}

	if !opts.Confirm {
		fmt.Fprintf(out, "\nplan: %d clearable, %d blocked. Nothing was written.\n", summary.Planned, summary.Blocked)
		fmt.Fprintf(out, "Re-run with --confirm to execute.\n")
		return summary, nil
	}
	fmt.Fprintf(out, "\nsummary: cleared=%d blocked=%d failed=%d\n", summary.Migrated, summary.Blocked, summary.Failed)
	if len(failures) > 0 {
		return summary, fmt.Errorf("%d row(s) failed:\n  %s", len(failures), strings.Join(failures, "\n  "))
	}
	return summary, nil
}

// clearStaleGitContent writes the two columns that describe "the DB no longer
// holds this content".
//
// content_md5 is recomputed from the bytes just read out of the repository. It
// is not made live by this — nothing recomputes it on push, by design — but
// leaving a fingerprint of the copy we are deleting would describe bytes that
// no longer exist anywhere.
//
// Needs the bypass marker: the target row IS Git-backed, so the guard would
// otherwise refuse both columns. This is the authoritative Git writer acting on
// its own columns, which is exactly what the marker is for.
//
// Set() goes on a fresh session rather than straight onto the handle. Settings
// live on the Statement, and on an already-chained *gorm.DB the statement is
// shared — the marker would outlive this one UPDATE and disarm the guard for
// every later write on that handle. A session makes it statement-local
// regardless of what the caller passed in.
func clearStaleGitContent(db *gorm.DB, item models.CapabilityItem, live string) error {
	result := db.Session(&gorm.Session{}).
		Set(models.GitSyncBypassSetting, true).
		Model(&models.CapabilityItem{}).
		Where("id = ? AND content_backend = ?", item.ID, models.ContentBackendGit).
		Updates(map[string]any{
			"content":     "",
			"content_md5": services.HashGitCapabilityContent(item.ItemType, item.SourceRepoPath, live),
		})
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected == 0 {
		return errors.New("row is no longer git-backed")
	}
	return nil
}

// runCapabilityToGitCommand is the CLI entry point: parse, wire, run.
//
// The personal-space dependencies are wired here rather than in main() so no
// other migrate subcommand acquires a Git client it never asked for, and so a
// deployment that runs plain `migrate` without CS_BOT_TOKEN_KEY is unaffected.
func runCapabilityToGitCommand(db *gorm.DB, args []string) error {
	opts, err := parseCapabilityToGitArgs(args)
	if err != nil {
		return err
	}
	out := os.Stdout

	key, keyErr := decodeBotTokenKey()
	if keyErr != nil {
		return fmt.Errorf("CS_BOT_TOKEN_KEY is required to reach the git server: %w", keyErr)
	}
	aes, err := crypto.NewAESGCM(key)
	if err != nil {
		return fmt.Errorf("CS_BOT_TOKEN_KEY is unusable: %w", err)
	}
	resolver := gitserver.NewDBResolver(db)
	handlers.InitUserSpaceService(db, aes, resolver, nil)

	deps, err := newCapabilityToGitDeps(db, opts, out)
	if err != nil {
		return err
	}
	_, err = runCapabilityToGit(context.Background(), deps, opts)
	return err
}

func decodeBotTokenKey() ([]byte, error) {
	raw := os.Getenv("CS_BOT_TOKEN_KEY")
	if raw == "" {
		return nil, errors.New("CS_BOT_TOKEN_KEY not set (it must be exported as a real process environment variable)")
	}
	return crypto.DecodeBase64Key(raw)
}

// newCapabilityToGitDeps wires the real implementations.
//
// Every dependency that can be absent is absent as nil rather than as a stub:
// the plan reports "not configured" and blocks, instead of a stub quietly
// producing a plausible-looking answer.
func newCapabilityToGitDeps(db *gorm.DB, opts capabilityToGitOptions, out io.Writer) (capabilityToGitDeps, error) {
	deps := capabilityToGitDeps{DB: db, Out: out}

	if !handlers.GitBackingConfigured() {
		return deps, errors.New("git backing is not configured in this process " +
			"(CS_BOT_TOKEN_KEY must be exported and a git server bound to the tenant)")
	}

	resolver := gitserver.NewDBResolver(db)
	cfg, err := resolver.Resolve(context.Background(), opts.TenantID)
	if err != nil {
		return deps, fmt.Errorf("resolve git server for tenant %s: %w", opts.TenantID, err)
	}
	if client := gitsync.NewClient(cfg.Endpoint, cfg.AdminToken); client != nil {
		deps.Inspector = client
	}
	deps.Provisioner = provisionerFunc(handlers.ProvisionCapabilityRepo)
	deps.GitContent = services.NewGitCapabilityContentService(db)

	// Object storage is only needed by items with binary assets. A run without
	// it is fine until such an item appears, and then it blocks that item.
	if backend, storageErr := storage.NewFromEnv(context.Background()); storageErr == nil {
		deps.Blobs = storageBlobLoader{backend: backend}
	} else {
		fmt.Fprintf(out, "note: object storage unavailable (%v); items with binary assets will be blocked\n", storageErr)
	}
	return deps, nil
}

// provisionerFunc adapts handlers.ProvisionCapabilityRepo, whose typed
// *GitProvisionError would otherwise become a non-nil error interface holding a
// nil pointer on success.
type provisionerFunc func(ctx context.Context, tenantID, userID string, req handlers.GitCapabilityProvisionRequest) (*handlers.GitCapabilityRepoCoordinate, *handlers.GitProvisionError)

func (f provisionerFunc) Provision(
	ctx context.Context, tenantID, userID string, req handlers.GitCapabilityProvisionRequest,
) (*handlers.GitCapabilityRepoCoordinate, error) {
	coord, err := f(ctx, tenantID, userID, req)
	if err != nil {
		return nil, err
	}
	return coord, nil
}

type storageBlobLoader struct{ backend storage.Backend }

func (l storageBlobLoader) Load(ctx context.Context, key string) ([]byte, error) {
	reader, _, err := l.backend.Get(ctx, key)
	if err != nil {
		return nil, err
	}
	defer reader.Close()
	return io.ReadAll(reader)
}

func splitCommaList(raw string) []string {
	parts := strings.Split(raw, ",")
	out := make([]string, 0, len(parts))
	for _, part := range parts {
		if trimmed := strings.TrimSpace(part); trimmed != "" {
			out = append(out, trimmed)
		}
	}
	sort.Strings(out)
	return out
}

func containsString(haystack []string, needle string) bool {
	for _, candidate := range haystack {
		if candidate == needle {
			return true
		}
	}
	return false
}

func orNone(value string) string {
	if strings.TrimSpace(value) == "" {
		return "-"
	}
	return value
}
