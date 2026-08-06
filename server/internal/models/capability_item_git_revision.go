package models

import "time"

// Git lifecycle archive causes recorded in capability_items.git_lifecycle_reason.
//
// The split that matters is recoverability, not wording: a manifest that was
// deleted from HEAD and a default branch that disappeared can both come back at
// the SAME bound identity, so Git is allowed to reactivate those rows. A
// deleted repository cannot — a repository recreated with the same owner/name
// gets a new numeric id and is therefore a different identity, so
// GitLifecycleReasonRepositoryDeleted is terminal for automatic recovery and
// replacement requires an explicit operator adoption flow.
const (
	GitLifecycleReasonManifestRemoved      = "manifest_removed"
	GitLifecycleReasonDefaultBranchMissing = "default_branch_missing"
	GitLifecycleReasonRepositoryDeleted    = "repository_deleted"
)

// IsRecoverableGitLifecycleReason reports whether Git may auto-reactivate a row
// archived for this reason once the exact bound identity is restored. An
// unknown value returns false so a future/typo'd reason fails closed.
func IsRecoverableGitLifecycleReason(reason string) bool {
	switch reason {
	case GitLifecycleReasonManifestRemoved, GitLifecycleReasonDefaultBranchMissing:
		return true
	default:
		return false
	}
}

// Sources of a CapabilityItemGitRevision row.
const (
	GitRevisionSourceBackfill  = "backfill"  // one-off seed of existing synced rows
	GitRevisionSourceProvision = "provision" // first successful projection of a newly bound item
	GitRevisionSourcePush      = "push"      // webhook-triggered projection
	GitRevisionSourceReconcile = "reconcile" // periodic next-due reconcile
	GitRevisionSourceRestore   = "restore"   // projection that reactivated a Git-archived row
)

// CapabilityItemGitRevision is one successful Git projection transition of a
// capability item — the user-facing "recent revisions" history.
//
// It is deliberately not derived from GitCapabilitySyncJob: jobs record
// deliveries and attempts (including retries, failures and no-op reconciles),
// while a revision exists only when a successful projection actually changed
// what THIS item projects.
//
// ContentDigest is the trigger: a row is appended only when the newly projected
// digest differs from the item's current one. GitSHA is the repository
// default-branch commit observed at that transition — a recorded coordinate,
// not the trigger. The distinction is load-bearing rather than pedantic: 94% of
// Git-backed items share their repository with other bound capabilities (the
// largest holds 55), so triggering on the head SHA would give every sibling a
// revision for a commit that never touched it.
//
// RevisionNo is strictly increasing within ItemID; it is allocated inside the
// same transaction that updates the item's current SHA, under that item's row
// lock, with the unique index as the backstop.
//
// Mirrors migrations 20260805000100_create_capability_item_git_revisions.sql,
// 20260805000500_add_capability_item_git_revision_content_digest.sql and
// 20260805000600_constrain_capability_item_git_revision_sha.sql. This model is
// intentionally NOT part of autoMigrateAll — like GitCapabilityRepository and
// GitCapabilitySyncJob, the goose migration is the single owner of its DDL, so
// the model and the schema cannot drift.
type CapabilityItemGitRevision struct {
	ID           string `gorm:"column:id;primaryKey;type:uuid;default:gen_random_uuid()" json:"id"`
	ItemID       string `gorm:"column:item_id;type:uuid;not null;uniqueIndex:uq_capability_item_git_revisions_no,priority:1" json:"itemId"`
	RevisionNo   int64  `gorm:"column:revision_no;not null;uniqueIndex:uq_capability_item_git_revisions_no,priority:2" json:"revisionNo"`
	GitServerID  string `gorm:"column:git_server_id;type:varchar(64);not null" json:"-"`
	GitRepoID    int64  `gorm:"column:git_repo_id;not null" json:"-"`
	GitRef       string `gorm:"column:git_ref;type:text;not null;default:''" json:"gitRef,omitempty"`
	ManifestPath string `gorm:"column:manifest_path;type:text;not null;default:''" json:"manifestPath,omitempty"`
	EntryKey     string `gorm:"column:entry_key;type:text;not null;default:''" json:"-"`
	GitSHA       string `gorm:"column:git_sha;type:varchar(40);not null" json:"gitSha"`
	// VersionLabel is the item-visible version at this revision. It may be empty
	// when the manifest declares none; the read API is responsible for
	// presenting a non-empty display value.
	VersionLabel string    `gorm:"column:version_label;type:text;not null;default:''" json:"versionLabel"`
	Source       string    `gorm:"column:source;type:varchar(16);not null" json:"source"`
	ObservedAt   time.Time `gorm:"column:observed_at;type:timestamptz;not null" json:"observedAt"`
	CreatedAt    time.Time `gorm:"column:created_at;type:timestamptz;not null;default:now()" json:"-"`

	// ContentDigest is the lowercase SHA-256 of this item's own projected
	// content and manifest-derived display fields (see
	// services.gitCapabilityProjectionDigest). The empty string represents SQL
	// NULL, which is legal only on a `backfill` baseline whose digest was never
	// observed; every other writer must supply one, and the schema rejects a row
	// that does not.
	//
	// Not serialized: it is the writer's comparison key, not a fact a history
	// reader can act on, and publishing it would invite a client to treat it as
	// a content identifier it can fetch by.
	ContentDigest string `gorm:"column:content_digest;type:varchar(64)" json:"-"`
}

// TableName pins the production table name.
func (CapabilityItemGitRevision) TableName() string { return "capability_item_git_revisions" }
