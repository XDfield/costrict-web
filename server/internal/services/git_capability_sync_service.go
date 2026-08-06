package services

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"strings"
	"time"

	"github.com/costrict/costrict-web/server/internal/gitcapability"
	"github.com/costrict/costrict-web/server/internal/gitserver"
	"github.com/costrict/costrict-web/server/internal/gitsync"
	"github.com/costrict/costrict-web/server/internal/models"
	"gorm.io/datatypes"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

const (
	gitCapabilitySyncPending = "pending"
	gitCapabilitySyncSynced  = "synced"
	gitCapabilitySyncError   = "error"
	// gitCapabilitySyncOrphaned marks a row this sync hid itself because its
	// manifest — or the whole default branch — disappeared from Git. It is the
	// discriminator the next push needs to tell "Git took this down" from "a
	// human took this down": both end up as status='archived', but only the
	// first may be raised back to 'active' when the manifest returns.
	//
	// Rows carrying it are archived, so they are already excluded from every
	// listing and from the marketplace projection (which filters
	// status='active' before it looks at git_sync_status).
	gitCapabilitySyncOrphaned = "orphaned"
)

// gitCapabilityHiddenStatuses are the statuses a human puts a row into to take
// it off the shelf: adminitem.SetStatus / BatchSetStatus write 'archived', and
// PUT /items/:id accepts any status from the author or a platform admin — it
// deliberately leaves `status` outside gitBackedUpdateTouchesContentProjection
// so those administrative actions stay possible on a Git-backed row (R1.6).
//
// That permission is only safe because a push never raises a row out of this
// set — otherwise "PUT may set archived" plus "every push sets active" combine
// into a resurrection hole: the moderator takes the capability down, the next
// commit puts it back, and nothing reports the conflict.
//
// Read from models rather than redeclared: the same list decides which statuses
// clear Git's archive claim (models.guardGitLifecycleStatusWrite) and which
// statuses this sync must not overwrite. Two copies would eventually disagree,
// and the symptom of disagreement is exactly the resurrection hole above.
var gitCapabilityHiddenStatuses = models.CapabilityHiddenStatuses()

func isGitCapabilityHiddenStatus(status string) bool {
	return models.IsCapabilityHiddenStatus(status)
}

// gitCapabilityRecoverableReasons are the git_lifecycle_reason values that
// leave Git permission to raise a row it archived back to 'active'.
// 'repository_deleted' is deliberately absent: a repository recreated with the
// same owner/name gets a new numeric id, so it is a different identity and
// cannot restore the old one.
var gitCapabilityRecoverableReasons = []string{
	models.GitLifecycleReasonManifestRemoved,
	models.GitLifecycleReasonDefaultBranchMissing,
}

// gitCapabilityRecoverablePredicate is the SQL test for "Git took this row down
// and is still allowed to put it back". Both halves are required:
//
//   - git_sync_status = 'orphaned' says THIS sync hid the row, so a human's
//     archive is not undone;
//   - git_lifecycle_reason says the cause is recoverable, and it is the half a
//     human moderation write clears (models.guardGitLifecycleStatusWrite), which
//     is how "I hid this deliberately" revokes Git's permission for good.
//
// COALESCE on both columns so a NULL never makes the predicate itself NULL: in
// SQL `NOT (x AND NULL)` is NULL, and a CASE arm that is NULL falls through to
// ELSE — which here is 'active'. Without the COALESCE, a row whose lifecycle
// reason was never written would be silently republished by the ELSE branch.
const gitCapabilityRecoverablePredicate = "(COALESCE(git_sync_status, '') = ? AND COALESCE(git_lifecycle_reason, '') IN (?))"

// gitCapabilityActivateStatus is the status assignment for a manifest that is
// present at HEAD.
//
// 'banned' is absolute. 'archived'/'inactive' are honoured too, unless this
// sync is the one that hid the row AND still holds a recoverable claim on it,
// in which case the reappearing manifest republishes it — a deleted-then-
// restored file, or a restored default branch, must not leave the capability
// dark forever.
//
// The residual case this used to carry — an admin archiving an already-orphaned
// row could not revoke the republish permission, because git_sync_status is
// Git-owned and only this writer may move it — is closed by the second half of
// the predicate. A manual hidden-status write clears git_lifecycle_reason in the
// same statement (models.guardGitLifecycleStatusWrite), and without a
// recoverable reason this CASE keeps the human's status however the orphan
// marker reads.
func gitCapabilityActivateStatus() clause.Expr {
	return gorm.Expr(
		"CASE WHEN status = ? THEN status "+
			"WHEN status IN (?) AND NOT "+gitCapabilityRecoverablePredicate+" THEN status "+
			"ELSE ? END",
		"banned", gitCapabilityHiddenStatuses,
		gitCapabilitySyncOrphaned, gitCapabilityRecoverableReasons, "active")
}

// gitCapabilityArchiveStatus is the status assignment for a manifest that is
// gone from HEAD. A row a human already hid keeps that human's status, so
// 'inactive' is not silently rewritten to 'archived'.
func gitCapabilityArchiveStatus() clause.Expr {
	return gorm.Expr("CASE WHEN status IN (?) THEN status ELSE ? END",
		gitCapabilityHiddenStatuses, "archived")
}

// gitCapabilityArchiveLifecycleReason records WHY Git took a row down.
//
// Written on every bound row the archive pass touches, including rows a human
// had already hidden. That is deliberate and is the one place this column's
// rule differs from the orphan marker's: the marker answers "who hid this row"
// (and must stay with the human), while the reason answers "what does Git say
// about this capability right now". A row whose repository is gone has no
// reachable content whoever hid it, and recording that is what stops a later
// Cloud-side activation from publishing an item that 404s on every read.
//
// Recovery still requires BOTH halves, so writing the reason onto a
// human-hidden row grants Git nothing: gitCapabilityRecoverablePredicate also
// demands the orphan marker, which that row does not carry.
func gitCapabilityArchiveLifecycleReason(reason string) clause.Expr {
	return gorm.Expr("?", reason)
}

// gitCapabilityArchiveLifecycleChangedAt stamps the transition, but only when
// the reason actually moves. Re-archiving a row that is already archived for
// the same cause is not a new transition, and rewriting the timestamp on every
// reconcile would erase when the capability really disappeared.
func gitCapabilityArchiveLifecycleChangedAt(reason string, now time.Time) clause.Expr {
	return gorm.Expr("CASE WHEN COALESCE(git_lifecycle_reason, '') = ? THEN git_lifecycle_changed_at ELSE ? END",
		reason, now)
}

// gitCapabilityClearedLifecycleChangedAt stamps the moment Git dropped its
// archive claim, and leaves an already-clear row's timestamp alone so a healthy
// repository does not rewrite the column on every reconcile.
func gitCapabilityClearedLifecycleChangedAt(now time.Time) clause.Expr {
	return gorm.Expr("CASE WHEN git_lifecycle_reason IS NULL THEN git_lifecycle_changed_at ELSE ? END", now)
}

// gitCapabilityArchiveSyncStatus claims the orphan marker only for rows this
// sync is actually taking down. A row a human had already hidden keeps
// 'synced' — claiming it there would hand the next push permission to undo the
// human's decision. A row that is already orphaned stays orphaned, so repeated
// pushes with the manifest still missing do not lose the marker.
func gitCapabilityArchiveSyncStatus() clause.Expr {
	return gorm.Expr("CASE WHEN status IN (?) AND COALESCE(git_sync_status, '') <> ? THEN ? ELSE ? END",
		gitCapabilityHiddenStatuses, gitCapabilitySyncOrphaned,
		gitCapabilitySyncSynced, gitCapabilitySyncOrphaned)
}

type GitCapabilityReader interface {
	GetRepoByID(ctx context.Context, repoID int64) (*gitsync.Repo, error)
	GetBranch(ctx context.Context, owner, repo, branch string) (*gitsync.Branch, error)
	ListTree(ctx context.Context, owner, repo, ref string) ([]gitsync.GitTreeEntry, error)
	ReadFile(ctx context.Context, owner, repo, ref, filePath string) ([]byte, error)
}

type GitCapabilitySyncService struct {
	DB        *gorm.DB
	Parser    *ParserService
	NewReader func(*gitserver.Config) GitCapabilityReader
}

type GitCapabilitySyncResult struct {
	CommitSHA string
	Created   int
	Updated   int
	Archived  int
	Skipped   int
}

// GitCapabilitySyncLease fences a worker claim. A worker must still own this
// token immediately before it commits index updates; reclaimed jobs receive a
// new token, so a timed-out worker cannot write after its lease has been lost.
type GitCapabilitySyncLease struct {
	JobID string
	Token string
}

var ErrGitCapabilityLeaseLost = errors.New("git capability sync lease lost")

type preparedGitCapability struct {
	item     models.CapabilityItem
	parsed   *ParsedItem
	metadata datatypes.JSON
	// contentDigest is this item's own projected digest — the revision writer's
	// trigger. Computed outside the transaction, from the manifest and asset
	// listing this pass read, so the projection and the digest describe the same
	// bytes.
	contentDigest string
	updateTags    bool
	removed       bool
}

// SyncRepository converges Git-backed capability rows using the stable
// (git server, numeric repo id) identity. Repositories without bound rows run
// first-discovery; DB-backed rows remain outside this sync path.
func (s *GitCapabilitySyncService) SyncRepository(
	ctx context.Context,
	cfg *gitserver.Config,
	repoID int64,
	repoFullName string,
	defaultBranch string,
	defaultBranchDeleted bool,
	lease GitCapabilitySyncLease,
) (result *GitCapabilitySyncResult, retErr error) {
	if s == nil || s.DB == nil || s.Parser == nil || cfg == nil {
		return nil, errors.New("git capability sync is not configured")
	}
	if cfg.ServerID == "" || repoID <= 0 {
		return nil, errors.New("git capability sync requires stable server and repository identities")
	}
	if strings.TrimSpace(lease.JobID) == "" || strings.TrimSpace(lease.Token) == "" {
		return nil, errors.New("git capability sync requires an active worker lease")
	}

	reader := s.reader(cfg)
	if reader == nil {
		return nil, errors.New("git capability sync reader is unavailable")
	}

	var boundItems []models.CapabilityItem
	if err := s.DB.WithContext(ctx).
		Where("content_backend = ? AND source_git_server_id = ? AND source_git_repo_id = ?",
			"git", cfg.ServerID, repoID).
		Order("id ASC").
		Find(&boundItems).Error; err != nil {
		return nil, fmt.Errorf("load Git-backed index rows: %w", err)
	}
	result = &GitCapabilitySyncResult{}

	defer func() {
		if retErr == nil || errors.Is(retErr, ErrGitCapabilityLeaseLost) {
			return
		}
		// A reclaimed worker must never overwrite a newer successful projection
		// with its late read/parse error. The lease is checked and locked in the
		// same transaction as this status update.
		_ = s.markGitCapabilitySyncFailure(context.WithoutCancel(ctx), cfg.ServerID, repoID, lease, retErr)
	}()

	// Webhook owner/name are mutable hints. Numeric repository ID is the only
	// identity used to locate current name and default-branch state.
	repo, err := reader.GetRepoByID(ctx, repoID)
	if err != nil {
		return nil, fmt.Errorf("load repository %d: %w", repoID, err)
	}
	if repo == nil {
		return s.archiveGitCapabilitiesForMissingRepository(ctx, cfg.ServerID, repoID, lease, boundItems)
	}
	if repo.ID != repoID {
		return nil, fmt.Errorf("repository identity mismatch: requested=%d api=%d", repoID, repo.ID)
	}
	owner, repoName, err := splitGitRepoFullName(repo.FullName)
	if err != nil {
		return nil, fmt.Errorf("repository API returned invalid full_name: %w", err)
	}
	branchName := strings.TrimSpace(repo.DefaultBranch)
	if branchName == "" {
		if defaultBranchDeleted {
			return s.archiveGitCapabilitiesForMissingDefaultBranch(ctx, cfg.ServerID, repoID, lease, boundItems)
		}
		return nil, fmt.Errorf("repository %d has no default branch (delivery reported %q)", repoID, defaultBranch)
	}

	branch, err := reader.GetBranch(ctx, owner, repoName, branchName)
	if err != nil {
		return nil, fmt.Errorf("load default branch: %w", err)
	}
	if branch == nil {
		if defaultBranchDeleted {
			return s.archiveGitCapabilitiesForMissingDefaultBranch(ctx, cfg.ServerID, repoID, lease, boundItems)
		}
		return nil, fmt.Errorf("default branch %q does not exist", branchName)
	}
	if !validGitSHA(branch.CommitSHA) {
		return nil, fmt.Errorf("default branch %q has no valid HEAD commit", branchName)
	}
	headSHA := strings.ToLower(branch.CommitSHA)
	result.CommitSHA = headSHA
	if len(boundItems) == 0 {
		if gitcapability.DiscoveryOwnerExcluded(owner) {
			return result, nil
		}
		return s.discoverGitCapabilities(ctx, cfg, reader, repo, owner, repoName, branchName, headSHA, lease)
	}

	items := make([]models.CapabilityItem, 0, len(boundItems))
	for _, item := range boundItems {
		if item.SourceRepoPath != "" {
			items = append(items, item)
		}
	}
	discovered, skipped, err := s.scanGitCapabilityManifestSet(ctx, reader, owner, repoName, headSHA, items)
	if err != nil {
		return nil, err
	}
	result.Skipped += skipped

	discoveredByIdentity := make(map[string]discoveredGitCapability, len(discovered))
	for _, entry := range discovered {
		discoveredByIdentity[gitCapabilityManifestIdentity(entry.Path, entry.EntryKey)] = entry
	}

	prepared := make([]preparedGitCapability, 0, len(items))
	for _, item := range items {
		if err := validateGitManifestPath(item.SourceRepoPath); err != nil {
			return nil, fmt.Errorf("item %s: %w", item.ID, err)
		}
		entry := preparedGitCapability{item: item}
		discoveredEntry, exists := discoveredByIdentity[gitCapabilityManifestIdentity(item.SourceRepoPath, item.SourceGitEntryKey)]
		if !exists {
			entry.removed = true
			prepared = append(prepared, entry)
			continue
		}
		delete(discoveredByIdentity, gitCapabilityManifestIdentity(item.SourceRepoPath, item.SourceGitEntryKey))
		parsed := discoveredEntry.Parsed
		parsed.ItemType = item.ItemType
		parsed.Slug = item.Slug
		// A real plugin.json often omits marketplace-only display fields. Missing
		// keys preserve the existing projection; explicit empty values still clear
		// the field, so Git can intentionally remove a description or category.
		if item.ItemType == "plugin" {
			if _, present := parsed.Metadata["description"]; !present {
				parsed.Description = item.Description
			}
			if _, present := parsed.Metadata["category"]; !present {
				parsed.Category = item.Category
			}
			if _, present := parsed.Metadata["version"]; !present {
				parsed.Version = item.Version
			}
		}
		merged, updateTags, err := mergeGitCapabilityMetadata(item.Metadata, parsed.Metadata)
		if err != nil {
			return nil, fmt.Errorf("merge metadata for item %s: %w", item.ID, err)
		}
		// discoveredEntry.Path is this item's SourceRepoPath — the two were matched
		// on it — so its manifest bytes and asset set are this item's, both read in
		// the same pass from the same commit.
		digest, err := gitCapabilityProjectionDigest(gitCapabilityProjectionSource{
			ItemType:       item.ItemType,
			ManifestPath:   item.SourceRepoPath,
			ManifestSHA256: discoveredEntry.ManifestSHA256,
			Parsed:         parsed,
			Assets:         discoveredEntry.Assets,
		})
		if err != nil {
			return nil, fmt.Errorf("digest projection for item %s: %w", item.ID, err)
		}
		entry.parsed = parsed
		entry.metadata = merged
		entry.contentDigest = digest
		entry.updateTags = updateTags
		prepared = append(prepared, entry)
	}
	newEntries := remainingDiscoveredGitCapabilities(discovered, discoveredByIdentity)

	now := time.Now()
	repoURL := strings.TrimRight(firstGitURL(cfg.WebURL, cfg.Endpoint), "/") + "/" + owner + "/" + repoName
	err = s.DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		job, err := assertGitCapabilityLease(tx, lease)
		if err != nil {
			return err
		}
		// The delivery this projection is running under decides the `source` of
		// any revision it appends. It is read from the leased job rather than
		// passed down, so the label can never disagree with the job that actually
		// authorized the write.
		triggerSource := GitRevisionSourceForDelivery(job.DeliveryID)
		// Resolve lazily on the transaction handle. This keeps owner projection in
		// the same snapshot and avoids self-deadlock in single-connection tests.
		ownerResolver := newGitCapabilityOwnerResolver(tx, cfg.ServerID, gitRepositoryOwnerID(repo), owner)
		var binding *models.GitCapabilityRepository
		var ownerID string
		if len(newEntries) > 0 {
			binding, err = ensureGitCapabilityReconciliationBinding(
				tx, cfg.ServerID, repo, repoURL, branchName, headSHA,
				inferGitCapabilityRepoKind(repo, discoverGitCapabilityCandidatesFromDiscovered(discovered)), ownerResolver, boundItems, now,
			)
			if err != nil {
				return err
			}
			// Newly discovered manifests become owned rows, so this repository
			// does consume an owner. Memoised, so it costs no extra query.
			if ownerID, err = ownerResolver.OwnerID(); err != nil {
				return fmt.Errorf("resolve repository owner: %w", err)
			}
		}
		if err := updateGitCapabilityRepositoryProjection(tx, cfg.ServerID, repoID, repo.FullName, repoURL, branchName, headSHA, repo.Private, ownerResolver, owner, now); err != nil {
			return err
		}
		for _, entry := range prepared {
			// Take the item's row lock and read its authoritative pre-update state
			// BEFORE anything below decides what this pass is doing. The lock is
			// what serializes revision numbering; the state answers whether this
			// projection is a restore (this sync had orphaned the row), a provision
			// (never projected), or — on the archiving side — whether the row was
			// still on the shelf a moment ago, which is the compare-and-set the
			// tombstone rotation contract requires. None of that can be read from
			// `entry.item`: it was loaded outside this transaction and may already
			// be stale.
			//
			// `prepared` preserves the boundItems `id ASC` order, so every writer of
			// this repository acquires the item locks in the same order and two
			// concurrent syncs queue rather than deadlock.
			state, err := lockGitCapabilityItemForProjection(tx, entry.item.ID, cfg.ServerID, repoID)
			if err != nil {
				return err
			}

			// content / content_md5 are absent from this map by design, not by
			// oversight — do not "fix" it by adding them.
			//
			// A Git-backed row does not store its content: every read path fetches
			// the file from the repository at request time. Writing it here would
			// re-create the copy this design removed, and would do it with the
			// worst possible refresh policy — updated only when a push happens to
			// be delivered, silently stale whenever the webhook is late or lost.
			// The hash stays as discovery computed it, describing the manifest
			// rather than standing in for it.
			//
			// What this map does own is the projection: the fields the marketplace
			// and the listings read without touching Git.
			updates := map[string]any{
				"source_repo_url":    repoURL,
				"source_repo_ref":    branchName,
				"source_sha":         headSHA,
				"git_sha":            headSHA,
				"git_last_synced_at": now,
				"git_sync_status":    gitCapabilitySyncSynced,
				"git_sync_error":     "",
				// This pass read the repository through the Git server, so its
				// current visibility is verified as of `now` for every row bound to
				// it. Public browse/search requires that verification to be fresh;
				// see the column comment in 20260805000000.
				"git_visibility_verified_at": now,
			}
			archivedNow := false
			if entry.removed {
				updates["status"] = gitCapabilityArchiveStatus()
				updates["git_sync_status"] = gitCapabilityArchiveSyncStatus()
				updates["git_lifecycle_reason"] = gitCapabilityArchiveLifecycleReason(models.GitLifecycleReasonManifestRemoved)
				updates["git_lifecycle_changed_at"] = gitCapabilityArchiveLifecycleChangedAt(models.GitLifecycleReasonManifestRemoved, now)
				archivedNow = !isGitCapabilityHiddenStatus(state.Status)
				if archivedNow {
					result.Archived++
				}
			} else {
				updates["status"] = gitCapabilityActivateStatus()
				// A manifest that is present at HEAD means Git makes no archive
				// claim on this row, whatever status it ends up in. Clearing the
				// reason unconditionally is what lets a moderator re-activate a row
				// they had hidden while it was ALSO missing from Git: once the file
				// is back, the refusal in models.guardGitLifecycleStatusWrite has to
				// stop applying, or their own archive would become permanent.
				updates["git_lifecycle_reason"] = nil
				updates["git_lifecycle_changed_at"] = gitCapabilityClearedLifecycleChangedAt(now)
				updates["name"] = entry.parsed.Name
				updates["description"] = entry.parsed.Description
				updates["category"] = entry.parsed.Category
				updates["version"] = entry.parsed.Version
				updates["metadata"] = entry.metadata
				if state.Status != "banned" {
					result.Updated++
				}
			}

			// Explicit, statement-local opt-out from the Git-owned field guard:
			// this is the authoritative Git writer, so it is the one caller
			// allowed to move the projection columns.
			updated := tx.Set(models.GitSyncBypassSetting, true).
				Model(&models.CapabilityItem{}).
				Where("id = ? AND content_backend = ? AND source_git_server_id = ? AND source_git_repo_id = ?",
					entry.item.ID, "git", cfg.ServerID, repoID).
				Updates(updates)
			if updated.Error != nil {
				return updated.Error
			}
			if updated.RowsAffected != 1 {
				return fmt.Errorf("Git-backed item %s changed identity during sync", entry.item.ID)
			}

			// The entitlement side of the archive, and the ONLY place a Git
			// lifecycle tombstone is minted for a manifest that disappeared.
			// Gated on the locked pre-update status, so a row that was already off
			// the shelf does not rotate event ids for a transition that did not
			// happen — see RecordEntitlementRemovalTx on why an unnecessary
			// rotation is as harmful as a missing one.
			if archivedNow {
				if _, err := RecordGitArchiveTombstonesTx(tx, entry.item.ID, models.GitLifecycleReasonManifestRemoved, now); err != nil {
					return err
				}
			}

			// History records ACTIVE projections only, and only when THIS item's
			// projected content digest moved. The head SHA above moved for every
			// item in the repository — that is exactly why it cannot be the
			// trigger — so a commit that only touched a sibling's manifest reaches
			// here with an unchanged digest and appends nothing.
			//
			// An archiving pass still advances git_sha, deliberately, but records
			// no content revision: an archive is a lifecycle event, not a version.
			if !entry.removed {
				if err := projectGitCapabilityRevision(tx, gitCapabilityRevisionInput{
					ItemID:        entry.item.ID,
					GitServerID:   cfg.ServerID,
					GitRepoID:     repoID,
					GitRef:        branchName,
					ManifestPath:  entry.item.SourceRepoPath,
					EntryKey:      entry.item.SourceGitEntryKey,
					GitSHA:        headSHA,
					VersionLabel:  entry.parsed.Version,
					ContentDigest: entry.contentDigest,
					Source:        gitRevisionSourceForProjection(*state, triggerSource),
					ObservedAt:    now,
				}); err != nil {
					return err
				}
			}

			if !entry.removed && entry.updateTags {
				tagSvc := &TagService{DB: tx}
				tags, err := tagSvc.ResolveOrCreateForAssignment(entry.parsed.Tags, entry.item.CreatedBy)
				if err != nil {
					return err
				}
				tagIDs := make([]string, 0, len(tags))
				for _, tag := range tags {
					tagIDs = append(tagIDs, tag.ID)
				}
				// Only the `git` domain is rebuilt: tags a user set in Cloud and
				// tags the scanner/catalog produced live in their own domains and
				// must survive every push.
				if err := tagSvc.SetItemTags(entry.item.ID, tagIDs, TagSourceGit); err != nil {
					return err
				}
			}
		}

		usedSlugs := make(map[string]struct{}, len(boundItems)+len(newEntries))
		for _, item := range boundItems {
			usedSlugs[item.ItemType+"\x00"+item.Slug] = struct{}{}
		}
		for _, discoveredEntry := range newEntries {
			discoveredEntry.Parsed.Slug = uniqueDiscoveredCapabilitySlug(discoveredEntry, usedSlugs)
			item, err := buildDiscoveredCapability(
				binding, cfg.ServerID, repo, repoURL, branchName, headSHA, binding.RepoKind, ownerID, discoveredEntry, now,
			)
			if err != nil {
				return err
			}
			if err := createDiscoveredCapability(tx, item, discoveredEntry, ownerID); err != nil {
				return err
			}
			result.Created++
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("commit Git capability index: %w", err)
	}
	return result, nil
}

// archiveGitCapabilitiesForMissingRepository converges a deleted upstream
// repository to a terminal state. Item archival and binding removal share the
// lease-fenced transaction so a reclaimed worker cannot partially apply this
// transition.
func (s *GitCapabilitySyncService) archiveGitCapabilitiesForMissingRepository(
	ctx context.Context,
	serverID string,
	repoID int64,
	lease GitCapabilitySyncLease,
	items []models.CapabilityItem,
) (*GitCapabilitySyncResult, error) {
	result := &GitCapabilitySyncResult{}
	now := time.Now()
	err := s.DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if _, err := assertGitCapabilityLease(tx, lease); err != nil {
			return err
		}
		for _, item := range items {
			state, err := lockGitCapabilityItemForProjection(tx, item.ID, serverID, repoID)
			if err != nil {
				return err
			}
			updated := tx.Set(models.GitSyncBypassSetting, true).
				Model(&models.CapabilityItem{}).
				Where("id = ? AND content_backend = ? AND source_git_server_id = ? AND source_git_repo_id = ?",
					item.ID, "git", serverID, repoID).
				Updates(map[string]any{
					"status":             gitCapabilityArchiveStatus(),
					"git_last_synced_at": now,
					"git_sync_status":    gitCapabilityArchiveSyncStatus(),
					"git_sync_error":     "",
					// Terminal for automatic recovery. A repository recreated with the
					// same owner/name receives a NEW numeric id, so it is a different
					// identity and can never satisfy the recovery predicate for these
					// rows; adopting a replacement is an explicit operator action.
					"git_lifecycle_reason":     gitCapabilityArchiveLifecycleReason(models.GitLifecycleReasonRepositoryDeleted),
					"git_lifecycle_changed_at": gitCapabilityArchiveLifecycleChangedAt(models.GitLifecycleReasonRepositoryDeleted, now),
					// Deliberately NOT refreshed: the repository could not be read, so
					// nothing about its visibility was verified. Letting the existing
					// value go stale is the fail-closed direction.
				})
			if updated.Error != nil {
				return updated.Error
			}
			if updated.RowsAffected != 1 {
				return fmt.Errorf("Git-backed item %s changed identity during repository archival", item.ID)
			}
			if !isGitCapabilityHiddenStatus(state.Status) {
				result.Archived++
				if _, err := RecordGitArchiveTombstonesTx(tx, item.ID, models.GitLifecycleReasonRepositoryDeleted, now); err != nil {
					return err
				}
			}
		}
		return tx.Where("git_server_id = ? AND git_repo_id = ?", serverID, repoID).
			Delete(&models.GitCapabilityRepository{}).Error
	})
	if err != nil {
		return nil, fmt.Errorf("archive Git capabilities for missing repository: %w", err)
	}
	return result, nil
}

// archiveGitCapabilitiesForMissingDefaultBranch is deliberately available only
// after the numeric repository lookup has confirmed the same repository and a
// default-branch deletion delivery has observed no current HEAD. All bound
// rows are archived, including legacy rows without a manifest path; otherwise
// those rows would remain active forever with no recoverable Git source.
func (s *GitCapabilitySyncService) archiveGitCapabilitiesForMissingDefaultBranch(
	ctx context.Context,
	serverID string,
	repoID int64,
	lease GitCapabilitySyncLease,
	items []models.CapabilityItem,
) (*GitCapabilitySyncResult, error) {
	result := &GitCapabilitySyncResult{}
	now := time.Now()
	err := s.DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if _, err := assertGitCapabilityLease(tx, lease); err != nil {
			return err
		}
		for _, item := range items {
			state, err := lockGitCapabilityItemForProjection(tx, item.ID, serverID, repoID)
			if err != nil {
				return err
			}
			// Authoritative Git writer — see the marker in SyncRepository.
			updated := tx.Set(models.GitSyncBypassSetting, true).
				Model(&models.CapabilityItem{}).
				Where("id = ? AND content_backend = ? AND source_git_server_id = ? AND source_git_repo_id = ?",
					item.ID, "git", serverID, repoID).
				Updates(map[string]any{
					"status":             gitCapabilityArchiveStatus(),
					"git_last_synced_at": now,
					"git_sync_status":    gitCapabilityArchiveSyncStatus(),
					"git_sync_error":     "",
					// Recoverable: the SAME numeric repository regaining a valid
					// default branch restores these rows.
					"git_lifecycle_reason":     gitCapabilityArchiveLifecycleReason(models.GitLifecycleReasonDefaultBranchMissing),
					"git_lifecycle_changed_at": gitCapabilityArchiveLifecycleChangedAt(models.GitLifecycleReasonDefaultBranchMissing, now),
					// The repository itself WAS read successfully to get here, so its
					// visibility is verified as of now.
					"git_visibility_verified_at": now,
				})
			if updated.Error != nil {
				return updated.Error
			}
			if updated.RowsAffected != 1 {
				return fmt.Errorf("Git-backed item %s changed identity during default-branch archival", item.ID)
			}
			if !isGitCapabilityHiddenStatus(state.Status) {
				result.Archived++
				if _, err := RecordGitArchiveTombstonesTx(tx, item.ID, models.GitLifecycleReasonDefaultBranchMissing, now); err != nil {
					return err
				}
			}
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("archive Git capabilities for missing default branch: %w", err)
	}
	return result, nil
}

func (s *GitCapabilitySyncService) markGitCapabilitySyncFailure(
	ctx context.Context,
	serverID string,
	repoID int64,
	lease GitCapabilitySyncLease,
	syncErr error,
) error {
	return s.DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if _, err := assertGitCapabilityLease(tx, lease); err != nil {
			return err
		}
		// Authoritative Git writer — see the marker in SyncRepository.
		if err := tx.Set(models.GitSyncBypassSetting, true).
			Model(&models.CapabilityItem{}).
			Where("content_backend = ? AND source_git_server_id = ? AND source_git_repo_id = ?", "git", serverID, repoID).
			Updates(map[string]any{
				// A transient read failure must not erase the record that Git,
				// not a human, took these rows down: losing the marker would
				// leave them archived for good once the manifest returns. The
				// failure is still reported through git_sync_error.
				"git_sync_status": gorm.Expr("CASE WHEN git_sync_status = ? THEN git_sync_status ELSE ? END",
					gitCapabilitySyncOrphaned, gitCapabilitySyncError),
				"git_sync_error": syncErr.Error(),
			}).Error; err != nil {
			return err
		}
		return tx.Model(&models.GitCapabilityRepository{}).
			Where("git_server_id = ? AND git_repo_id = ?", serverID, repoID).
			Updates(map[string]any{"last_error": syncErr.Error(), "updated_at": time.Now().UTC()}).Error
	})
}

// assertGitCapabilityLease locks the claimed job row with the same
// transaction that updates capability_items. On PostgreSQL this serializes a
// concurrent lease-reclaimer behind the index commit; the token is still
// checked so a lease reclaimed before this point fails closed. SQLite omits
// FOR UPDATE because its write transaction already serializes the test path.
//
// It returns the leased job so a caller can read the delivery that authorized
// the write — the revision writer classifies a projection's `source` from it.
// Deriving the trigger from the row this function already had to load keeps the
// label and the authorization inseparable: there is no second parameter that
// could describe a different job than the one the lease was checked against.
func assertGitCapabilityLease(tx *gorm.DB, lease GitCapabilitySyncLease) (*models.GitCapabilitySyncJob, error) {
	if tx == nil || strings.TrimSpace(lease.JobID) == "" || strings.TrimSpace(lease.Token) == "" {
		return nil, ErrGitCapabilityLeaseLost
	}
	query := tx.Where("id = ? AND status = ? AND lease_token = ?", lease.JobID, models.GitCapabilitySyncJobStatusRunning, lease.Token)
	if tx.Dialector.Name() == "postgres" {
		query = query.Clauses(clause.Locking{Strength: "UPDATE"})
	}
	var job models.GitCapabilitySyncJob
	if err := query.First(&job).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, ErrGitCapabilityLeaseLost
		}
		return nil, fmt.Errorf("validate Git capability lease: %w", err)
	}
	return &job, nil
}

func (s *GitCapabilitySyncService) reader(cfg *gitserver.Config) GitCapabilityReader {
	if s.NewReader != nil {
		return s.NewReader(cfg)
	}
	return gitsync.NewClient(cfg.Endpoint, cfg.AdminToken)
}

func mergeGitCapabilityMetadata(existing datatypes.JSON, incoming map[string]any) (datatypes.JSON, bool, error) {
	merged := map[string]any{}
	if len(existing) > 0 && string(existing) != "null" {
		if err := json.Unmarshal(existing, &merged); err != nil {
			return nil, false, err
		}
	}
	if merged == nil {
		merged = map[string]any{}
	}
	mergeGitMap(merged, incoming)
	encoded, err := json.Marshal(merged)
	if err != nil {
		return nil, false, err
	}
	_, updateTags := incoming["tags"]
	return datatypes.JSON(encoded), updateTags, nil
}

// applyExplicitGitIndexFields restores the distinction between an omitted
// metadata field and one deliberately set to its empty value. Several legacy
// parsers provide presentation fallbacks (plugin descriptions/tags and
// SKILL.md body summaries), which are useful for DB ingestion but would make
// a Git author unable to intentionally clear an indexed description or tag
// set. Metadata itself remains the authority for this distinction.
func applyExplicitGitIndexFields(parsed *ParsedItem) error {
	if parsed == nil {
		return errors.New("parsed item is nil")
	}
	if value, present := parsed.Metadata["description"]; present {
		description, ok := value.(string)
		if !ok {
			return errors.New("description must be a string")
		}
		parsed.Description = description
	}
	if value, present := parsed.Metadata["tags"]; present {
		tags, err := explicitGitTags(value)
		if err != nil {
			return err
		}
		parsed.Tags = tags
	}
	return nil
}

func explicitGitTags(value any) ([]string, error) {
	if value == nil {
		return nil, errors.New("tags must be an array of strings")
	}
	values, ok := value.([]any)
	if !ok {
		return nil, errors.New("tags must be an array of strings")
	}
	tags := make([]string, 0, len(values))
	for _, raw := range values {
		tag, ok := raw.(string)
		if !ok {
			return nil, errors.New("tags must be an array of strings")
		}
		tags = append(tags, tag)
	}
	return tags, nil
}

func mergeGitMap(dst, src map[string]any) {
	for key, value := range src {
		srcMap, srcOK := value.(map[string]any)
		dstMap, dstOK := dst[key].(map[string]any)
		if srcOK && dstOK {
			mergeGitMap(dstMap, srcMap)
			continue
		}
		dst[key] = value
	}
}

func splitGitRepoFullName(fullName string) (string, string, error) {
	parts := strings.Split(strings.TrimSpace(fullName), "/")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" || parts[0] == "." || parts[1] == "." ||
		parts[0] == ".." || parts[1] == ".." {
		return "", "", fmt.Errorf("invalid repository full_name %q", fullName)
	}
	return parts[0], parts[1], nil
}

func validateGitManifestPath(filePath string) error {
	trimmed := strings.TrimSpace(filePath)
	if trimmed == "" || strings.Contains(trimmed, "\\") || strings.HasPrefix(trimmed, "/") || path.Clean(trimmed) != trimmed ||
		trimmed == "." || trimmed == ".." || strings.HasPrefix(trimmed, "../") {
		return fmt.Errorf("invalid Git manifest path %q", filePath)
	}
	return nil
}

func validGitSHA(value string) bool {
	if len(value) != 40 {
		return false
	}
	for _, r := range value {
		if !((r >= '0' && r <= '9') || (r >= 'a' && r <= 'f') || (r >= 'A' && r <= 'F')) {
			return false
		}
	}
	return true
}

func firstGitURL(values ...string) string {
	for _, value := range values {
		if value = strings.TrimSpace(value); value != "" {
			return value
		}
	}
	return ""
}
