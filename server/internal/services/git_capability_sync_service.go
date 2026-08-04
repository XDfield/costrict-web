package services

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"strings"
	"time"

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
// it off the shelf: admin moderation (adminitem.SetStatus writes 'archived',
// user_status handlers write 'banned') and PUT /items/:id, which deliberately
// leaves `status` outside gitBackedUpdateTouchesContentProjection so those
// administrative actions stay possible on a Git-backed row (R1.6).
//
// That permission is only safe because a push never raises a row out of this
// set — otherwise "PUT may set archived" plus "every push sets active" combine
// into a resurrection hole: the moderator takes the capability down, the next
// commit puts it back, and nothing reports the conflict.
var gitCapabilityHiddenStatuses = []string{"banned", "archived", "inactive"}

func isGitCapabilityHiddenStatus(status string) bool {
	for _, hidden := range gitCapabilityHiddenStatuses {
		if status == hidden {
			return true
		}
	}
	return false
}

// gitCapabilityActivateStatus is the status assignment for a manifest that is
// present at HEAD.
//
// 'banned' is absolute. 'archived'/'inactive' are honoured too, unless this
// sync is the one that hid the row (git_sync_status='orphaned'), in which case
// the reappearing manifest republishes it — a deleted-then-restored file, or a
// restored default branch, must not leave the capability dark forever.
//
// Residual case, stated rather than hidden: an admin who archives an
// already-orphaned row does not clear the marker (git_sync_status is Git-owned,
// so only this writer can write it), so a returning manifest still republishes
// it. 'banned' is the moderation state that survives unconditionally.
func gitCapabilityActivateStatus() clause.Expr {
	return gorm.Expr(
		"CASE WHEN status = ? THEN status "+
			"WHEN status IN (?) AND git_sync_status <> ? THEN status "+
			"ELSE ? END",
		"banned", gitCapabilityHiddenStatuses, gitCapabilitySyncOrphaned, "active")
}

// gitCapabilityArchiveStatus is the status assignment for a manifest that is
// gone from HEAD. A row a human already hid keeps that human's status, so
// 'inactive' is not silently rewritten to 'archived'.
func gitCapabilityArchiveStatus() clause.Expr {
	return gorm.Expr("CASE WHEN status IN (?) THEN status ELSE ? END",
		gitCapabilityHiddenStatuses, "archived")
}

// gitCapabilityArchiveSyncStatus claims the orphan marker only for rows this
// sync is actually taking down. A row a human had already hidden keeps
// 'synced' — claiming it there would hand the next push permission to undo the
// human's decision. A row that is already orphaned stays orphaned, so repeated
// pushes with the manifest still missing do not lose the marker.
func gitCapabilityArchiveSyncStatus() clause.Expr {
	return gorm.Expr("CASE WHEN status IN (?) AND git_sync_status <> ? THEN ? ELSE ? END",
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
	item       models.CapabilityItem
	parsed     *ParsedItem
	metadata   datatypes.JSON
	updateTags bool
	removed    bool
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
		return nil, fmt.Errorf("repository %d no longer exists (delivery reported %q)", repoID, repoFullName)
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
		entry.parsed = parsed
		entry.metadata = merged
		entry.updateTags = updateTags
		prepared = append(prepared, entry)
	}
	newEntries := remainingDiscoveredGitCapabilities(discovered, discoveredByIdentity)

	now := time.Now()
	repoURL := strings.TrimRight(firstGitURL(cfg.WebURL, cfg.Endpoint), "/") + "/" + owner + "/" + repoName
	err = s.DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := assertGitCapabilityLease(tx, lease); err != nil {
			return err
		}
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
			updates := map[string]any{
				"source_repo_url":    repoURL,
				"source_repo_ref":    branchName,
				"source_sha":         headSHA,
				"git_sha":            headSHA,
				"git_last_synced_at": now,
				"git_sync_status":    gitCapabilitySyncSynced,
				"git_sync_error":     "",
			}
			if entry.removed {
				updates["status"] = gitCapabilityArchiveStatus()
				updates["git_sync_status"] = gitCapabilityArchiveSyncStatus()
				if !isGitCapabilityHiddenStatus(entry.item.Status) {
					result.Archived++
				}
			} else {
				updates["status"] = gitCapabilityActivateStatus()
				updates["name"] = entry.parsed.Name
				updates["description"] = entry.parsed.Description
				updates["category"] = entry.parsed.Category
				updates["version"] = entry.parsed.Version
				updates["metadata"] = entry.metadata
				if entry.item.Status != "banned" {
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
				if err := tagSvc.SetItemTags(entry.item.ID, tagIDs); err != nil {
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
			item, version, err := buildDiscoveredCapability(
				binding, cfg.ServerID, repo, repoURL, branchName, headSHA, binding.RepoKind, ownerID, discoveredEntry, now,
			)
			if err != nil {
				return err
			}
			if err := createDiscoveredCapability(tx, item, version, discoveredEntry.Parsed.Tags, ownerID); err != nil {
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
		if err := assertGitCapabilityLease(tx, lease); err != nil {
			return err
		}
		for _, item := range items {
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
				})
			if updated.Error != nil {
				return updated.Error
			}
			if updated.RowsAffected != 1 {
				return fmt.Errorf("Git-backed item %s changed identity during default-branch archival", item.ID)
			}
			if !isGitCapabilityHiddenStatus(item.Status) {
				result.Archived++
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
		if err := assertGitCapabilityLease(tx, lease); err != nil {
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
func assertGitCapabilityLease(tx *gorm.DB, lease GitCapabilitySyncLease) error {
	if tx == nil || strings.TrimSpace(lease.JobID) == "" || strings.TrimSpace(lease.Token) == "" {
		return ErrGitCapabilityLeaseLost
	}
	query := tx.Where("id = ? AND status = ? AND lease_token = ?", lease.JobID, models.GitCapabilitySyncJobStatusRunning, lease.Token)
	if tx.Dialector.Name() == "postgres" {
		query = query.Clauses(clause.Locking{Strength: "UPDATE"})
	}
	var job models.GitCapabilitySyncJob
	if err := query.First(&job).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return ErrGitCapabilityLeaseLost
		}
		return fmt.Errorf("validate Git capability lease: %w", err)
	}
	return nil
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
