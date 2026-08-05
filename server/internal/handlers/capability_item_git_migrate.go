// Package handlers — the surface the S6 migration script drives.
//
// `migrate capability-to-git` moves an existing DB-backed capability onto Git.
// The repository side of that operation is exactly what the create and fork
// paths already do (capability_item_git_provision.go), and its ordering —
// repository → content → verified readable → only then the DB row — is the one
// property that keeps a half-migrated item from existing. So the migration does
// not get its own build path; it gets these three exports onto the same one.
//
// Everything here is a translation layer: gin.H bodies become a typed error,
// unexported specs become exported requests. No provisioning decision is made
// in this file, deliberately, so there is nothing here that can drift from what
// the HTTP callers do.
//
// The DB write stays with the caller. The migration flips content_backend
// itself, under its own `content_backend = 'db'` predicate, because that flip
// is the operation's commit point and belongs where its failure can be
// reported per item.

package handlers

import (
	"context"
	"fmt"

	"gorm.io/gorm"
)

// GitCapabilityFile is one repository file published alongside the manifest.
type GitCapabilityFile struct {
	Path    string
	Content []byte
}

// GitCapabilityRepoCoordinate is where a provisioned capability now lives, and
// the exact bytes proven to be readable there. Mirrors the internal fork plan.
type GitCapabilityRepoCoordinate struct {
	RepoURL     string
	RepoRef     string
	RepoPath    string
	GitServerID string
	GitRepoID   int64
	EntryKey    string
	// Content is the manifest as it now reads back from the repository. The
	// caller hashes these bytes rather than the row's stored copy: they are what
	// every reader will get.
	Content string
}

// GitProvisionError carries the status and error_code the HTTP paths would have
// returned, so an operator reading CLI output and an operator reading an API
// response are diagnosing the same failure with the same vocabulary.
type GitProvisionError struct {
	Status  int
	Code    string
	Message string
}

func (e *GitProvisionError) Error() string {
	if e == nil {
		return ""
	}
	if e.Code == "" {
		return e.Message
	}
	return fmt.Sprintf("%s (%s, http %d)", e.Message, e.Code, e.Status)
}

// GitCapabilityProvisionRequest describes the capability to publish. Content is
// mandatory for the migration: it is the source row's text, written verbatim,
// never regenerated from the projected columns.
type GitCapabilityProvisionRequest struct {
	ItemType     string
	Slug         string
	Name         string
	Description  string
	Category     string
	Version      string
	Content      string
	WantEntryKey string
	ExtraFiles   []GitCapabilityFile
}

// ProvisionCapabilityRepo creates <short_id>/<slug> under userID's namespace,
// writes the capability into it, and proves every file reads back
// byte-identical. It touches no capability_items row.
//
// Returns the coordinate to persist. A non-nil error means nothing about the
// item may change: the repository may exist and may even hold part of the
// tree, which is inert and reusable, but the row must stay DB-backed.
func ProvisionCapabilityRepo(
	ctx context.Context, tenantID, userID string, req GitCapabilityProvisionRequest,
) (*GitCapabilityRepoCoordinate, *GitProvisionError) {
	plan, herr := provisionGitCapabilityRepo(ctx, tenantID, userID, gitCapabilityProvisionSpec{
		ItemType:     req.ItemType,
		Slug:         req.Slug,
		Name:         req.Name,
		Description:  req.Description,
		Category:     req.Category,
		Version:      req.Version,
		Content:      req.Content,
		WantEntryKey: req.WantEntryKey,
		ExtraFiles:   req.ExtraFiles,
	})
	if herr != nil {
		return nil, toGitProvisionError(herr)
	}
	return &GitCapabilityRepoCoordinate{
		RepoURL:     plan.RepoURL,
		RepoRef:     plan.RepoRef,
		RepoPath:    plan.RepoPath,
		GitServerID: plan.GitServerID,
		GitRepoID:   plan.GitRepoID,
		EntryKey:    plan.EntryKey,
		Content:     plan.Content,
	}, nil
}

// GitCapabilityManifestPath returns the repo-relative path a standalone
// capability of this type keeps its manifest at, and whether the type can be
// stored in Git at all. Exported so the migration names the file the same way
// provisioning does — a second table would produce repositories that discovery
// cannot see.
func GitCapabilityManifestPath(itemType string) (string, bool) {
	return gitCapabilityManifestPath(itemType)
}

// ValidateGitCapabilityFiles reports why a file set may not be published
// alongside a capability, or nil when it may.
//
// Provisioning applies this itself and refuses; the migration also calls it
// while planning, so a dry-run names the offending file before any repository
// exists rather than after the operator has confirmed.
func ValidateGitCapabilityFiles(files []GitCapabilityFile, manifestPath string) *GitProvisionError {
	if herr := validateGitCapabilityExtraFiles(files, manifestPath); herr != nil {
		return toGitProvisionError(herr)
	}
	return nil
}

// GitBackingConfigured reports whether this process has the dependencies git
// backing needs (InitUserSpaceService was called with a usable AES key,
// resolver and DB). The migration checks it up front so an unconfigured run
// fails once, before selecting anything, rather than once per item.
func GitBackingConfigured() bool {
	return gitBackingWired()
}

// EnqueueInitialGitCapabilitySync queues the first index sync for a row that
// has just been flipped to Git backing.
//
// Provisioning creates commits but produces no webhook delivery the row can
// wait for, and the webhook ingress is the only other producer of sync jobs.
// Without this the row stays git_sync_status='pending' with an empty git_sha,
// and the Marketplace projection — which publishes only synced rows carrying a
// 40-char SHA — omits it, so subscribing installs nothing.
//
// Best-effort, like the fork path it shares: the repository and the row both
// already exist and are consistent, so a queueing failure must not turn a
// completed migration into a reported failure.
func EnqueueInitialGitCapabilitySync(db *gorm.DB, itemID string, coord *GitCapabilityRepoCoordinate) {
	if coord == nil {
		return
	}
	enqueueInitialGitSync(db, "migrate:"+itemID, itemID, &gitForkPlan{
		RepoURL:     coord.RepoURL,
		RepoRef:     coord.RepoRef,
		RepoPath:    coord.RepoPath,
		GitServerID: coord.GitServerID,
		GitRepoID:   coord.GitRepoID,
		EntryKey:    coord.EntryKey,
		Content:     coord.Content,
	})
}

func toGitProvisionError(herr *httpErr) *GitProvisionError {
	out := &GitProvisionError{Status: herr.status}
	if msg, ok := herr.body["error"].(string); ok {
		out.Message = msg
	}
	if code, ok := herr.body["error_code"].(string); ok {
		out.Code = code
	}
	return out
}
