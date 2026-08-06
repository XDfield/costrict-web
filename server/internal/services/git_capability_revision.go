// Package services — writing a Git-backed capability's revision history.
//
// A revision is one SUCCESSFUL projection transition of an item, not a delivery
// and not an attempt. git_capability_sync_jobs already records deliveries;
// overloading it into user-facing history would show retries, failures and
// no-op reconciles as if the capability had changed.
//
// The whole contract reduces to one comparison, made under the item's row lock:
//
//	append ⟺ this item's newly projected content digest differs from its
//	         CURRENT digest
//
// The trigger is deliberately NOT the repository's head SHA. Measured against
// production-shaped data on 2026-08-06, 66 of the repositories holding
// Git-backed capabilities hold more than one — 507 of 538 items (94%), the
// largest repository holding 55. A push edits a manifest; it moves the head of
// the repository, not of an item. Triggering on the head would therefore give
// every sibling a revision for a commit that never touched it (so an item's
// "5 most recent revisions" would mostly be other capabilities' work) and would
// write 55 rows for one push to the largest repository. The head SHA is still
// RECORDED on each row as the coordinate observed at the transition — but
// because a digest change may be observed several commits after the fact, it is
// "the head when this change was seen", not "the commit that made it".
//
// Everything else follows from the comparison. A duplicate delivery and a
// same-content reconcile append nothing. A sibling manifest's commit appends
// nothing to this item, because this item's digest did not move. A revert back
// to previously seen content DOES append, because the test is inequality
// against the CURRENT digest, never absence from the set of digests ever seen.
// An archiving commit advances git_sha without appending (the archive branch
// never reaches the append), so a restore that brings back different content
// appends exactly one row — and a restore that brings back byte-identical
// content appends none, which is the same rule, not an exception to it.
package services

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/costrict/costrict-web/server/internal/models"
	"github.com/google/uuid"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// GitRevisionSourceForDelivery classifies the job that authorized a projection.
//
// The delivery id is the only trigger evidence that reaches the sync service,
// and it is authoritative: reconcile and manual resync mint their own prefixed
// ids (models.GitCapabilitySyncDeliveryPrefix*), while a webhook carries
// Gitea's own delivery UUID.
//
// A manual resync maps to `reconcile` rather than to a source of its own. The
// enum is frozen by the contract and by the table's CHECK constraint, and an
// operator resync is definitionally a forced re-read of current Git state —
// the same act reconcile performs on a schedule. Calling it `push` would be a
// lie (no commit caused it); inventing `manual` would fail the constraint.
func GitRevisionSourceForDelivery(deliveryID string) string {
	switch {
	case strings.HasPrefix(deliveryID, models.GitCapabilitySyncDeliveryPrefixReconcile),
		strings.HasPrefix(deliveryID, models.GitCapabilitySyncDeliveryPrefixManual):
		return models.GitRevisionSourceReconcile
	default:
		return models.GitRevisionSourcePush
	}
}

// gitCapabilityProjectionDigestVersion is part of the hashed payload.
//
// Changing WHAT the digest covers changes every item's digest at once, and the
// first sync afterwards would append one revision per item for a change nobody
// made. The version makes that consequence explicit at the top of the payload
// rather than something a reviewer has to infer, and it makes an intentional
// re-baseline distinguishable from a silent drift in the serialization.
//
// Version 2 made two corrections, both of the same kind — v1 called things
// "unchanged" that a reader would have received differently:
//
//   - the capability's asset subtree entered the payload. v1 hashed only the
//     manifest, so a commit that changed nothing but a skill's bundled scripts
//     or references appended no revision, while csc downloaded those files and
//     the user's installed capability really had changed.
//   - the manifest is hashed from its raw bytes instead of through
//     ContentHashService. That service folds CRLF to LF and re-serializes
//     mcp/plugin JSON, so converting a SKILL.md to CRLF or reindenting a
//     .mcp.json left the digest untouched — while the read path streams the file
//     verbatim and the device writes those exact bytes to disk.
//
// The re-baseline that comes with the bump is one revision per item on the first
// sync after deployment; that is the price of the correction, paid once, and it
// is why this constant exists.
const gitCapabilityProjectionDigestVersion = 2

// gitCapabilityProjectionPayload is the exact, ordered set of facts a revision
// is a transition BETWEEN.
//
// The membership rule is one sentence: everything the item projects to a
// reader, and nothing its siblings share. So:
//
//   - ManifestSHA256 is in: SHA-256 over the manifest file's bytes EXACTLY as
//     the repository stores them. Content is by far the most common thing to
//     change and would otherwise be invisible here.
//
//     The bytes are deliberately raw rather than run through
//     ContentHashService, which is what this digest used to reuse. That service
//     normalizes newlines and re-serializes `mcp`/`plugin` JSON from its parsed
//     form — appropriate for content_md5, whose job is to compare a Git manifest
//     against a DB row written by a different path, and wrong here. The read
//     path (ItemContentBytes) streams the file byte for byte, so converting a
//     SKILL.md to CRLF or reindenting a .mcp.json changes what every consumer
//     receives — the device writes those exact bytes to disk — while leaving a
//     normalized hash untouched. A digest that called those "no change" would
//     contradict its own membership rule.
//
//     The counter-argument, that skipping purely cosmetic commits is a feature,
//     is rejected on the contract's own terms: the test is whether the bytes the
//     user receives moved, not whether their meaning did. content_md5 and the
//     device-facing asset SHA-256 keep their existing definitions; only this
//     consumer changed.
//
//   - Name/Description/Category/Version are in. They are the manifest-derived
//     fields item detail displays, they are Git-owned columns (only this writer
//     may move them), and a manifest that renames a capability without touching
//     its body has changed what the reader sees.
//
//   - Metadata is in — the MANIFEST's metadata, not the merged column. The
//     merged column is deliberately not Git-owned (see
//     models.gitOwnedCapabilityColumns), so Cloud can write it; hashing the
//     merged value would let a Cloud-side edit append a "revision" stamped with
//     a Git commit that did not cause it.
//
//   - Assets are in, as (capability-relative path, Git blob id) pairs. They are
//     distributed to the device and verified there, so an asset-only commit
//     really does change what the user installed; leaving them out made that
//     change invisible in history. The blob id is what makes this affordable:
//     it arrives with the tree listing the sync already performs, so covering
//     assets costs no extra Git round trip, whereas re-reading every asset of
//     every skill on every reconcile to compute SHA-256 would cost one request
//     per file per pass. The device-facing SHA-256 contract
//     (GitCapabilityAsset.ContentSHA) is untouched and unrelated — see
//     gitCapabilityTreeBlob.BlobID for why the two must not be confused.
//     The PATH is hashed alongside the id so that a rename, which in Git moves
//     no blob, is still a transition.
//
//   - The head SHA, remote URL, default branch and repository full name are
//     OUT. They are identical for every capability in the repository, which is
//     the whole reason this digest exists.
//
//   - `status`, sync health and timestamps are OUT: lifecycle is not content,
//     and an archive is explicitly not a content revision.
//
//   - Asset SIZE is out. It is a function of the bytes the blob id already
//     identifies, so it can only add ways for the digest to move without the
//     content moving.
//
// Field order is the struct's order and map keys are sorted by encoding/json,
// so the serialization is deterministic without a canonicalization pass. The
// asset list is sorted by path (gitCapabilityAssetsFromTree) for the same
// reason: the Git server's answer order is not part of the contract.
//
// One coupling survives on purpose. Several MCP entries can share ONE manifest
// file (ParseMCPJSON gives every `mcpServers` entry the whole file as its
// Content), so editing one entry moves every entry's digest and each of them
// records a revision. That is correct rather than leaked: the read-through
// serves the whole manifest file as each entry's content, so a reader of entry
// B genuinely receives different bytes after entry A is edited. It is also a
// far smaller grouping than the repository one — measured locally, 15 of 538
// Git-backed items sit in a multi-entry manifest, largest 9, against 507 in a
// shared repository.
//
// Assets add a second, narrower sharing case of the same kind. Two capabilities
// whose manifests both sit at the REPOSITORY root share one asset root, so they
// share their asset set — and again the read path agrees, because the device
// installs those same files for both. Capabilities in their own directories,
// which is the layout of every multi-capability repository measured locally, are
// isolated.
type gitCapabilityProjectionPayload struct {
	Version  int    `json:"v"`
	ItemType string `json:"itemType"`
	// The key is manifestSha256 rather than the old contentHash so that nothing
	// reads it as the content_md5 column, which is still computed the other way
	// and still means something else.
	ManifestSHA256 string                          `json:"manifestSha256"`
	Name           string                          `json:"name"`
	Description    string                          `json:"description"`
	Category       string                          `json:"category"`
	VersionText    string                          `json:"version"`
	Metadata       json.RawMessage                 `json:"metadata"`
	Assets         []gitCapabilityAssetFingerprint `json:"assets"`
}

// gitCapabilityAssetFingerprint is one asset as the digest sees it.
//
// It is a type of its own rather than a reuse of gitCapabilityAssetEntry so
// that the hashed payload cannot silently gain a field: adding one to the read
// path's struct would otherwise re-baseline every item's history.
type gitCapabilityAssetFingerprint struct {
	Path string `json:"path"`
	Blob string `json:"blob"`
}

// gitCapabilityProjectionSource is everything one capability's digest is
// computed from, gathered by the pass that read the repository.
//
// It is a struct rather than an argument list because three of the fields are
// strings that a caller could transpose without the compiler noticing, and the
// consequence of transposing them is a wrong digest — which reads as a content
// change nobody made.
type gitCapabilityProjectionSource struct {
	ItemType     string
	ManifestPath string
	// ManifestSHA256 is SHA-256 over the manifest file's bytes as the repository
	// stores them — the same bytes ItemContentBytes streams to a reader. It is
	// NOT hashDiscoveredCapabilityContent / content_md5, which normalizes first;
	// see the payload's field comment for why the difference matters here.
	ManifestSHA256 string
	Parsed         *ParsedItem
	// Assets must come from gitCapabilityAssetsFromTree over the SAME tree
	// listing the manifest was read from.
	Assets []gitCapabilityAssetEntry
}

// gitCapabilityProjectionDigest hashes one capability's projection: its
// manifest and the assets that ship with it.
//
// It is the single definition, called by both writers — the reconcile path in
// SyncRepository and the provisioning path in createDiscoveredCapability. Two
// implementations that agree today would diverge the first time one of them is
// edited, and the symptom would be a spurious revision on the next sync of
// every row the other one created.
//
// Three guards keep an unobserved input from masquerading as an observed one.
// Each of them protects the same property: the digest must move only when the
// bytes a reader receives moved.
//
//   - a nil asset slice is rejected. gitCapabilityAssetsFromTree returns a
//     non-nil empty slice for a capability that genuinely has no assets, so nil
//     can only mean "never resolved" — and digesting it as "no assets" is
//     indistinguishable from "the author deleted every asset".
//   - an asset without a Git object id is rejected. Fingerprinting it as the
//     empty string would make every future change to that file invisible, which
//     is silent and permanent; failing is neither.
//   - an empty manifest digest is rejected, for the same reason in the other
//     half of the payload.
//
// A tree listing or manifest read that failed outright never reaches here at
// all: the sync aborts first, so the item keeps its current digest and its
// history is untouched.
func gitCapabilityProjectionDigest(source gitCapabilityProjectionSource) (string, error) {
	if source.Parsed == nil {
		return "", fmt.Errorf("cannot digest a nil projection for %s", source.ManifestPath)
	}
	manifestSHA := strings.ToLower(strings.TrimSpace(source.ManifestSHA256))
	if manifestSHA == "" {
		return "", fmt.Errorf("cannot digest %s without the manifest's own bytes", source.ManifestPath)
	}
	if source.Assets == nil {
		return "", fmt.Errorf("cannot digest %s without a resolved asset set", source.ManifestPath)
	}
	fingerprints := make([]gitCapabilityAssetFingerprint, 0, len(source.Assets))
	for _, asset := range source.Assets {
		blob := strings.ToLower(strings.TrimSpace(asset.BlobID))
		if blob == "" {
			return "", fmt.Errorf("asset %q of %s carries no Git object id to fingerprint",
				asset.RelPath, source.ManifestPath)
		}
		fingerprints = append(fingerprints, gitCapabilityAssetFingerprint{Path: asset.RelPath, Blob: blob})
	}
	metadata := []byte("{}")
	if len(source.Parsed.Metadata) > 0 {
		encoded, err := json.Marshal(source.Parsed.Metadata)
		if err != nil {
			// Unreachable in practice: the same map is marshalled by
			// mergeGitCapabilityMetadata a few lines away, so a map that cannot
			// be encoded fails the sync before it reaches here. Reported rather
			// than defaulted to "{}", because silently hashing an empty object
			// would make two different manifests digest identically.
			return "", fmt.Errorf("encode manifest metadata for %s: %w", source.ManifestPath, err)
		}
		metadata = encoded
	}
	payload := gitCapabilityProjectionPayload{
		Version:        gitCapabilityProjectionDigestVersion,
		ItemType:       source.ItemType,
		ManifestSHA256: manifestSHA,
		Name:           source.Parsed.Name,
		Description:    source.Parsed.Description,
		Category:       source.Parsed.Category,
		VersionText:    source.Parsed.Version,
		Metadata:       metadata,
		Assets:         fingerprints,
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		return "", fmt.Errorf("encode projection digest payload for %s: %w", source.ManifestPath, err)
	}
	return sha256Hex(encoded), nil
}

// gitCapabilityProjectionState is an item's authoritative pre-update state,
// read under its row lock inside the projection transaction.
type gitCapabilityProjectionState struct {
	GitSHA        string `gorm:"column:git_sha"`
	Status        string `gorm:"column:status"`
	GitSyncStatus string `gorm:"column:git_sync_status"`
}

// lockGitCapabilityItemForProjection reads and locks the item this transaction
// is about to project.
//
// The lock is the serialization point for revision numbering. Every writer of a
// revision is also a writer of the item's current git_sha, so holding this row
// lock before reading the item's current revision makes two concurrent
// allocations for one item impossible: the second transaction blocks here, and
// when it proceeds it reads the revision the first one committed — so it either
// allocates the next number or correctly decides the transition already
// happened and appends nothing.
//
// The predicate repeats the identity the caller's UPDATE uses, so a row that
// changed identity (unbound, re-pointed at another repository) is reported here
// rather than silently skipped.
//
// SQLite omits FOR UPDATE: it has no row locks, and its write transaction
// already serializes the test path.
func lockGitCapabilityItemForProjection(
	tx *gorm.DB, itemID, serverID string, repoID int64,
) (*gitCapabilityProjectionState, error) {
	query := tx.Model(&models.CapabilityItem{}).
		Select("git_sha", "status", "git_sync_status").
		Where("id = ? AND content_backend = ? AND source_git_server_id = ? AND source_git_repo_id = ?",
			itemID, models.ContentBackendGit, serverID, repoID)
	if tx.Dialector.Name() == "postgres" {
		query = query.Clauses(clause.Locking{Strength: "UPDATE"})
	}
	var state gitCapabilityProjectionState
	result := query.Take(&state)
	if result.Error != nil {
		return nil, fmt.Errorf("lock Git-backed item %s for projection: %w", itemID, result.Error)
	}
	return &state, nil
}

// gitRevisionSourceForProjection resolves the enum value for one item's
// transition, given its locked pre-update state.
//
//   - an item bound but never projected (empty git_sha) is being provisioned,
//     whatever triggered the pass;
//   - an item this sync itself had archived (orphaned + a hidden status that is
//     not the absolute 'banned') is being restored — mirrors exactly the
//     condition under which gitCapabilityActivateStatus raises it back to
//     'active';
//   - otherwise the trigger decides.
func gitRevisionSourceForProjection(state gitCapabilityProjectionState, triggerSource string) string {
	if strings.TrimSpace(state.GitSHA) == "" {
		return models.GitRevisionSourceProvision
	}
	if state.Status != "banned" && isGitCapabilityHiddenStatus(state.Status) &&
		state.GitSyncStatus == gitCapabilitySyncOrphaned {
		return models.GitRevisionSourceRestore
	}
	return triggerSource
}

// gitCapabilityRevisionInput is one candidate transition.
type gitCapabilityRevisionInput struct {
	ItemID        string
	GitServerID   string
	GitRepoID     int64
	GitRef        string
	ManifestPath  string
	EntryKey      string
	GitSHA        string
	VersionLabel  string
	ContentDigest string
	Source        string
	ObservedAt    time.Time
}

// gitCapabilityRevisionHead is the item's newest recorded revision: the row the
// candidate is compared against.
type gitCapabilityRevisionHead struct {
	RevisionNo int64 `gorm:"column:revision_no"`
	// ContentDigest is empty when the column is SQL NULL, which the schema
	// permits only on a `backfill` baseline. Empty therefore means "unknown
	// baseline", never "digest of nothing".
	ContentDigest string `gorm:"column:content_digest"`
	Source        string `gorm:"column:source"`
}

// readGitCapabilityRevisionHead returns the item's newest revision, or nil when
// it has no history yet.
//
// Callers must already hold the item's row lock; without it two transactions
// could read the same head and allocate the same number. One query answers both
// questions the writer has — what number comes next, and what digest to compare
// against — so the comparison can never be made against a different row than
// the one being extended.
func readGitCapabilityRevisionHead(tx *gorm.DB, itemID string) (*gitCapabilityRevisionHead, error) {
	var heads []gitCapabilityRevisionHead
	if err := tx.Model(&models.CapabilityItemGitRevision{}).
		Select("revision_no", "content_digest", "source").
		Where("item_id = ?", itemID).
		Order("revision_no DESC").
		Limit(1).
		Find(&heads).Error; err != nil {
		return nil, fmt.Errorf("read current revision for item %s: %w", itemID, err)
	}
	if len(heads) == 0 {
		return nil, nil
	}
	return &heads[0], nil
}

// projectGitCapabilityRevision applies the append rule for one successful
// active projection. It is the only place that decides.
//
// Three outcomes, and the middle one is the reason the backfill is safe:
//
//  1. no history yet → append revision 1. A row bound before this writer
//     existed and never backfilled gets its baseline from the first projection
//     that actually observes it, which is more honest than leaving it with an
//     empty history until its repository happens to change.
//  2. the head is a backfilled baseline with no observed digest → ADOPT the
//     observed digest into it and append nothing. Treating "unknown" as
//     "different" would write one spurious revision for every backfilled item
//     on the first sync after deployment — the exact noise this change removes.
//  3. otherwise → append only when the digest actually moved.
//
// Callers must already hold the item's row lock (see
// lockGitCapabilityItemForProjection).
func projectGitCapabilityRevision(tx *gorm.DB, input gitCapabilityRevisionInput) error {
	if strings.TrimSpace(input.ContentDigest) == "" {
		// A digest-less append would be unconstrainable: the next projection
		// would have nothing to compare against and would append again forever.
		// The schema rejects it too; failing here names the item.
		return fmt.Errorf("refusing to record a revision for item %s without a content digest", input.ItemID)
	}
	head, err := readGitCapabilityRevisionHead(tx, input.ItemID)
	if err != nil {
		return err
	}
	if head == nil {
		return appendGitCapabilityRevision(tx, 1, input)
	}
	if head.ContentDigest == "" {
		return adoptGitCapabilityBaselineDigest(tx, input.ItemID, head.RevisionNo, input.ContentDigest)
	}
	if strings.EqualFold(head.ContentDigest, input.ContentDigest) {
		return nil
	}
	return appendGitCapabilityRevision(tx, head.RevisionNo+1, input)
}

// adoptGitCapabilityBaselineDigest completes a synthesized baseline with the
// first digest that could actually be observed for it.
//
// This is the one write this package makes to an existing history row, and it
// is narrowly what its name says: `migrate backfill-git-revisions` seeds
// revision 1 from the database alone, and a Git-backed row does not store its
// content, so the baseline's digest is not computable at backfill time. The
// row is completed here, not rewritten — revision number, SHA, version,
// observed time and `source='backfill'` are all untouched, so it keeps
// declaring itself synthesized.
//
// The predicate is a compare-and-set on `content_digest IS NULL` (plus the
// backfill source), so it can never overwrite an observed digest, and a
// concurrent adopter simply finds nothing to update.
//
// Zero rows affected is therefore ALMOST always success — but "almost" is not
// something this function is willing to assume, because the failure it would
// hide is silent and permanent. Every caller today holds the item's row lock,
// so the only way to match nothing is a concurrent adopter that already
// finished. A future caller without that lock, or a predicate that stopped
// matching for some other reason, would make adoption a no-op forever: every
// projection would take this branch, match nothing, report success, and that
// item's history would stop growing with nothing to notice it. So the zero-row
// path re-reads the head and requires that a digest is now present, whoever
// wrote it. One indexed read, only on a path that should not be taken.
//
// Stated rather than hidden: if the item's content changed between the backfill
// and this first observation, that one change is folded into the baseline
// instead of being recorded as revision 2. For a healthy item the rollout order
// (backfill, then enable revision writes) bounds that window to about one
// reconcile interval. It is NOT bounded for an item that is archived between the
// two: an archived row is never projected, so its baseline waits for its
// manifest to return, which may be weeks away or never come. Either way no
// later projection is affected — from here on the digest is observed and every
// change appends.
func adoptGitCapabilityBaselineDigest(tx *gorm.DB, itemID string, revisionNo int64, digest string) error {
	result := tx.Model(&models.CapabilityItemGitRevision{}).
		Where("item_id = ? AND revision_no = ? AND source = ? AND content_digest IS NULL",
			itemID, revisionNo, models.GitRevisionSourceBackfill).
		Update("content_digest", strings.ToLower(strings.TrimSpace(digest)))
	if result.Error != nil {
		return fmt.Errorf("adopt baseline digest for item %s revision %d: %w", itemID, revisionNo, result.Error)
	}
	if result.RowsAffected > 0 {
		return nil
	}
	// The head, not the row this statement targeted: a concurrent adopter may
	// have completed the baseline AND appended past it, and that is a healthy
	// outcome. What must not be true afterwards is that the item's newest
	// revision still has no digest, because that is the state in which the next
	// projection would come straight back here and no-op again.
	head, err := readGitCapabilityRevisionHead(tx, itemID)
	if err != nil {
		return err
	}
	if head == nil || strings.TrimSpace(head.ContentDigest) == "" {
		return fmt.Errorf(
			"baseline adoption for item %s revision %d matched no row and left the current revision without a digest",
			itemID, revisionNo)
	}
	return nil
}

// appendGitCapabilityRevision inserts the row at the already-allocated number.
//
// Callers must hold the item's row lock and must have established that the
// projection is a transition; this function re-checks neither, because both
// answers belong to the caller's transaction.
//
// A unique violation on (item_id, revision_no) is returned rather than
// swallowed. Under the row lock it cannot happen; if it ever does, the whole
// projection transaction rolls back — including the item's git_sha update — so
// the job simply retries and the transition is re-observed. Nothing is lost.
// Silently ignoring the conflict is what would lose it: the item would keep the
// new SHA while its history skipped the transition, and no later pass would
// notice, because the digest the next comparison reads is the one already
// recorded.
func appendGitCapabilityRevision(tx *gorm.DB, revisionNo int64, input gitCapabilityRevisionInput) error {
	revision := models.CapabilityItemGitRevision{
		ItemID:        input.ItemID,
		RevisionNo:    revisionNo,
		GitServerID:   input.GitServerID,
		GitRepoID:     input.GitRepoID,
		GitRef:        input.GitRef,
		ManifestPath:  input.ManifestPath,
		EntryKey:      input.EntryKey,
		GitSHA:        strings.ToLower(strings.TrimSpace(input.GitSHA)),
		VersionLabel:  input.VersionLabel,
		ContentDigest: strings.ToLower(strings.TrimSpace(input.ContentDigest)),
		Source:        input.Source,
		ObservedAt:    input.ObservedAt,
	}
	// ID/CreatedAt are left to the column defaults on PostgreSQL, but SQLite has
	// neither gen_random_uuid() nor a default for them in the test schema, so the
	// values are supplied here rather than depending on the dialect.
	if revision.ID == "" {
		revision.ID = uuid.NewString()
	}
	if revision.CreatedAt.IsZero() {
		revision.CreatedAt = input.ObservedAt
	}
	if err := tx.Create(&revision).Error; err != nil {
		return fmt.Errorf("append revision %d for item %s: %w", revision.RevisionNo, input.ItemID, err)
	}
	return nil
}

// appendGitCapabilityProvisionRevision records revision 1 of a row this pass
// just created. It still goes through projectGitCapabilityRevision — the row is
// new, so the head lookup is guaranteed to be empty, and routing every writer
// through one decision point is worth one indexed read.
func appendGitCapabilityProvisionRevision(
	tx *gorm.DB, item *models.CapabilityItem, contentDigest string, observedAt time.Time,
) error {
	if item == nil || strings.TrimSpace(item.GitSHA) == "" {
		return nil
	}
	return projectGitCapabilityRevision(tx, gitCapabilityRevisionInput{
		ItemID:        item.ID,
		GitServerID:   item.SourceGitServerID,
		GitRepoID:     item.SourceGitRepoID,
		GitRef:        item.SourceRepoRef,
		ManifestPath:  item.SourceRepoPath,
		EntryKey:      item.SourceGitEntryKey,
		GitSHA:        item.GitSHA,
		VersionLabel:  item.Version,
		ContentDigest: contentDigest,
		Source:        models.GitRevisionSourceProvision,
		ObservedAt:    observedAt,
	})
}
