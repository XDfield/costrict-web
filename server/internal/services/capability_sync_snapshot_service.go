// Package services — the csc snapshot v2 authority.
//
// The invariant everything here serves: AN ABSENT ITEM IS NOT A DELETION
// SIGNAL. A client may unload or disable a managed local capability only when a
// complete, newer snapshot carries an EXPLICIT tombstone for it. Network
// failure, auth failure, partial pagination, an empty response, a missing item —
// every one of those must be a no-op on the device, which means the server must
// never be able to express removal by omission.
//
// Three properties make that true and are the reason this file is shaped the
// way it is:
//
//  1. A snapshot is FROZEN when it is built. Pages are slices of stored bytes,
//     not fresh queries, so the digest a client verifies describes exactly one
//     data state.
//  2. `complete` is a property of the artifact, not of the request. A build that
//     did not finish leaves no complete snapshot to serve.
//  3. Conflicting state is rejected at build time. The server never hands a
//     client both "you have this" and "delete this" and asks it to choose.
package services

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/costrict/costrict-web/server/internal/logger"
	"github.com/costrict/costrict-web/server/internal/models"
	"github.com/costrict/costrict-web/server/internal/syncsnapshot"
	"github.com/google/uuid"
	"gorm.io/gorm"
)

const (
	// DefaultSnapshotPageSize is the frozen page size. It is a server constant
	// rather than a request parameter because page_count participates in the
	// digest: a client that could choose its own page size could ask for a
	// pagination the stored artifact was never cut at.
	DefaultSnapshotPageSize = 200

	// DefaultSnapshotTTL bounds how long a snapshot id and its page cursors stay
	// valid. It is generous on purpose: expiring mid-pagination forces a client
	// to restart, and a client that keeps restarting never converges.
	DefaultSnapshotTTL = 30 * time.Minute

	// snapshotSupersededGrace is how long a snapshot stays servable after a
	// newer one replaces it.
	//
	// Deleting it immediately would re-introduce the livelock the frozen
	// artifact exists to remove: a client halfway through paging generation N
	// would be told the snapshot is gone, restart on N+1, and on a busy account
	// be interrupted again. A short grace lets an in-flight pass finish — the
	// data it returns is slightly stale but internally consistent and complete,
	// which is exactly what the contract requires.
	//
	// The window bounds TIME only. The COUNT bound is snapshotSupersededKeep,
	// enforced by retireSuperseded: without it, entitlement churn faster than
	// the window could stack an unbounded pile of full payloads per principal
	// inside five minutes (F-26).
	snapshotSupersededGrace = 5 * time.Minute

	// snapshotSupersededKeep caps how many snapshots one principal may hold at
	// once: the current snapshot plus a single superseded grace copy. Anything
	// older is deleted immediately, grace window or not — the moment a THIRD
	// build lands, nobody can be paging the oldest on a reference the server
	// still hands out (EnsureSnapshot only ever issues the newest id), so a
	// client that deep only loses a snapshot it had already been told twice
	// over is stale, and it restarts on the current one. That makes the cost
	// of the grace window exactly one extra stored payload per principal,
	// independent of how fast the account churns (F-26).
	snapshotSupersededKeep = 2

	// snapshotBuildMaxAttempts bounds the serialization-failure retry loop.
	snapshotBuildMaxAttempts = 5

	// snapshotSweepBatch bounds the opportunistic global expiry sweep.
	snapshotSweepBatch = 100
)

var (
	// ErrSnapshotNotFound is returned for an unknown, foreign or expired
	// snapshot id. The three are deliberately indistinguishable to the caller:
	// telling one principal that another's snapshot id exists leaks nothing
	// useful and costs an enumeration oracle.
	ErrSnapshotNotFound = errors.New("capability sync snapshot not found or expired")

	// ErrSnapshotPageOutOfRange is returned for a page index outside the
	// snapshot's page count.
	ErrSnapshotPageOutOfRange = errors.New("capability sync snapshot page out of range")

	// ErrSnapshotIsolation is returned when a build starts outside REPEATABLE
	// READ. See assertRepeatableRead.
	ErrSnapshotIsolation = errors.New("capability sync snapshot build requires REPEATABLE READ isolation")
)

// CapabilitySyncSnapshotService builds and serves csc snapshot v2.
type CapabilitySyncSnapshotService struct {
	DB *gorm.DB
	// PageSize / TTL default to the constants above when zero.
	PageSize int
	TTL      time.Duration
	// LifecyclePropagation gates git_archived tombstones.
	//
	// This is the production kill switch. With it off, a Git-archived item is
	// neither active (it is not in the entitlement set once archived) nor
	// tombstoned — so a client sees ABSENCE, which by the core invariant is a
	// no-op. Flipping the switch is therefore safe in both directions: off can
	// never delete anything, and on can only ever add explicit instructions.
	LifecyclePropagation bool
	// Now is injectable for tests.
	Now func() time.Time
	// isolation is a test seam, unexported so nothing outside this package can
	// lower it. Its only legitimate use is proving that the REPEATABLE READ
	// assertion actually fires on the production build path rather than being
	// a comment about an invariant nobody checks.
	isolation func() *sql.TxOptions
}

func (s *CapabilitySyncSnapshotService) txOptions() *sql.TxOptions {
	if s.isolation != nil {
		return s.isolation()
	}
	return repeatableRead()
}

func (s *CapabilitySyncSnapshotService) pageSize() int {
	if s.PageSize > 0 {
		return s.PageSize
	}
	return DefaultSnapshotPageSize
}

func (s *CapabilitySyncSnapshotService) ttl() time.Duration {
	if s.TTL > 0 {
		return s.TTL
	}
	return DefaultSnapshotTTL
}

func (s *CapabilitySyncSnapshotService) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

// SnapshotPage is one page of a frozen snapshot, ready to serialize.
type SnapshotPage struct {
	ContractVersion int
	SnapshotID      string
	Generation      int64
	GeneratedAt     time.Time
	PageIndex       int
	PageCount       int
	ItemCount       int
	TombstoneCount  int
	SnapshotDigest  string
	// Complete is true only on the FINAL page of a fully materialized snapshot.
	// A client that has not seen it has not seen an authoritative snapshot and
	// may not remove anything.
	Complete   bool
	Items      []jsonRaw
	Tombstones []jsonRaw
	// Reused reports that this snapshot was served unchanged rather than newly
	// built. Diagnostic only.
	Reused bool
}

// jsonRaw is pre-canonicalized JSON that goes on the wire verbatim.
type jsonRaw = []byte

// EnsureSnapshot returns the principal's current authoritative snapshot,
// building a new generation only when the observable content actually changed.
func (s *CapabilitySyncSnapshotService) EnsureSnapshot(ctx context.Context, principalID string) (*models.CapabilitySyncSnapshot, bool, error) {
	principalID = strings.TrimSpace(principalID)
	if principalID == "" {
		return nil, false, errors.New("capability sync snapshot requires a principal")
	}

	var lastErr error
	for attempt := 1; attempt <= snapshotBuildMaxAttempts; attempt++ {
		snapshot, reused, err := s.buildOnce(ctx, principalID)
		if err == nil {
			// Sweep only after a real build, never on the reuse path. Reuse is
			// what a polling fleet does thousands of times between two content
			// changes, and running a DELETE on a shared table on each of those
			// buys nothing: the rows it would find were already reclaimed by
			// retireSuperseded during the build that created them. A build is
			// rare by construction, and any build sweeps for every principal, so
			// a system with activity anywhere still reclaims everywhere.
			if !reused {
				s.sweepExpired(ctx)
			}
			return snapshot, reused, nil
		}
		if !isSerializationFailure(err) {
			return nil, false, err
		}
		// A concurrent build for the same principal committed first. Under
		// REPEATABLE READ our allocation aborts rather than quietly proceeding,
		// so a retry re-reads a strictly newer data view and either matches the
		// winner's content digest (and re-serves it, allocating nothing) or
		// allocates a strictly newer generation over strictly newer data. Either
		// way generation order still equals data order.
		lastErr = err
		logger.Warn("[sync-snapshot] rebuild after serialization failure principal=%s attempt=%d: %v",
			principalID, attempt, err)
	}
	return nil, false, fmt.Errorf("capability sync snapshot for %s lost %d serialization races: %w",
		principalID, snapshotBuildMaxAttempts, lastErr)
}

func (s *CapabilitySyncSnapshotService) buildOnce(ctx context.Context, principalID string) (*models.CapabilitySyncSnapshot, bool, error) {
	var (
		built  *models.CapabilitySyncSnapshot
		reused bool
	)
	err := s.DB.WithContext(ctx).
		Transaction(func(tx *gorm.DB) error {
			// F-4: the generation protocol is only sound under REPEATABLE READ,
			// and PostgreSQL defaults to READ COMMITTED. Measured behaviour
			// under RC: the loser of the allocation upsert does NOT abort — it
			// blocks, then returns the next number and carries on, while every
			// subsequent SELECT takes a fresh statement snapshot. The build is
			// then not internally consistent: page 1's active set and page 3's
			// tombstone set can come from different data states, and the server
			// manufactures exactly the active+tombstone snapshot the contract
			// calls invalid.
			//
			// So this fails rather than degrades. A snapshot built under the
			// wrong isolation is not a slightly worse snapshot; it is one whose
			// central guarantee is absent, and it would be indistinguishable
			// from a correct one to everyone downstream.
			//
			// It runs FIRST, before the allocation. That leaves a window in
			// which the data view is established just before the number is
			// taken — which is safe for the reason the protocol works at all:
			// if a concurrent build commits in that window, the allocation
			// upsert touches a row modified after our snapshot and REPEATABLE
			// READ aborts us with 40001. We retry against a newer view. The
			// inversion the ordering was meant to prevent is prevented by the
			// abort, not by the ordering.
			if err := assertRepeatableRead(tx); err != nil {
				return err
			}

			current, err := loadCurrentSnapshot(tx, principalID)
			if err != nil {
				return err
			}

			items, tombstones, err := s.collectEntitlementState(tx, principalID)
			if err != nil {
				return err
			}
			contentDigest, err := syncsnapshot.ContentDigest(s.pageSize(), items, tombstones)
			if err != nil {
				return err
			}

			// F-1 step 3: identical content re-serves the existing snapshot.
			//
			// Generation means "version of the observable final state", not
			// "number of times a device asked". Allocating per request would
			// make a polling fleet burn numbers, and since csc rejects anything
			// not strictly greater than what it applied, every one of those
			// numbers costs a full client-side re-verification of state that did
			// not change. It also satisfies AC-LH17 idempotency directly: the
			// same content always yields the same snapshot id and generation.
			if current != nil && current.ContentDigest != nil && *current.ContentDigest == contentDigest &&
				current.PageSize == s.pageSize() {
				if err := s.extendExpiry(tx, current); err != nil {
					return err
				}
				built, reused = current, true
				return nil
			}

			generation, err := allocateSnapshotGeneration(tx, principalID, s.now())
			if err != nil {
				return err
			}
			snapshotID := uuid.NewString()
			generatedAt := s.now().UTC().Truncate(time.Second)

			materialized, err := syncsnapshot.Materialize(syncsnapshot.Manifest{
				SnapshotID:  snapshotID,
				Generation:  generation,
				GeneratedAt: formatSnapshotTime(generatedAt),
			}, s.pageSize(), items, tombstones)
			if err != nil {
				return err
			}

			snapshot := models.CapabilitySyncSnapshot{
				ID:              snapshotID,
				PrincipalID:     principalID,
				Generation:      generation,
				ContractVersion: syncsnapshot.ContractVersion,
				PageCount:       materialized.Manifest.PageCount,
				PageSize:        s.pageSize(),
				ItemCount:       materialized.Manifest.ItemCount,
				TombstoneCount:  materialized.Manifest.TombstoneCount,
				SnapshotDigest:  &materialized.Digest,
				ContentDigest:   &contentDigest,
				// Written complete in the same transaction as its payload. A
				// two-phase build would leave a window in which a manifest
				// exists without the bytes it describes; here an interrupted
				// build simply rolls back and leaves nothing to serve.
				Complete:    true,
				GeneratedAt: generatedAt,
				ExpiresAt:   s.now().Add(s.ttl()),
			}
			if err := tx.Create(&snapshot).Error; err != nil {
				return fmt.Errorf("persist snapshot manifest for %s: %w", principalID, err)
			}
			if err := tx.Create(&models.CapabilitySyncSnapshotPayload{
				SnapshotID: snapshotID,
				ByteSize:   len(materialized.Canonical),
				Payload:    materialized.Canonical,
			}).Error; err != nil {
				return fmt.Errorf("persist snapshot payload for %s: %w", principalID, err)
			}
			if err := tx.Model(&models.CapabilitySyncSnapshotGeneration{}).
				Where("principal_id = ?", principalID).
				Update("last_snapshot_id", snapshotID).Error; err != nil {
				return fmt.Errorf("record last snapshot for %s: %w", principalID, err)
			}
			if err := s.retireSuperseded(tx, principalID, snapshotID); err != nil {
				return err
			}

			built = &snapshot
			return nil
		}, s.txOptions())
	if err != nil {
		return nil, false, err
	}
	return built, reused, nil
}

// GetSnapshotPage serves one page of a stored snapshot.
//
// It never falls back to live data. An unknown, foreign or expired id is an
// error, because answering it from current tables is how a client ends up
// reassembling pages from several data states and computing a digest that
// matches nothing.
func (s *CapabilitySyncSnapshotService) GetSnapshotPage(ctx context.Context, principalID, snapshotID string, pageIndex int) (*SnapshotPage, error) {
	if pageIndex < 0 {
		return nil, ErrSnapshotPageOutOfRange
	}
	var snapshot models.CapabilitySyncSnapshot
	err := s.DB.WithContext(ctx).
		Where("id = ? AND principal_id = ? AND complete = ? AND expires_at > ?",
			snapshotID, principalID, true, s.now()).
		First(&snapshot).Error
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, ErrSnapshotNotFound
		}
		return nil, fmt.Errorf("load snapshot %s: %w", snapshotID, err)
	}
	return s.pageOf(ctx, &snapshot, pageIndex, false)
}

// PageOfSnapshot serves a page of a snapshot the caller already loaded (the
// page-0 path, immediately after EnsureSnapshot).
func (s *CapabilitySyncSnapshotService) PageOfSnapshot(ctx context.Context, snapshot *models.CapabilitySyncSnapshot, pageIndex int, reused bool) (*SnapshotPage, error) {
	return s.pageOf(ctx, snapshot, pageIndex, reused)
}

func (s *CapabilitySyncSnapshotService) pageOf(ctx context.Context, snapshot *models.CapabilitySyncSnapshot, pageIndex int, reused bool) (*SnapshotPage, error) {
	if pageIndex < 0 || pageIndex >= snapshot.PageCount {
		return nil, ErrSnapshotPageOutOfRange
	}
	var payload models.CapabilitySyncSnapshotPayload
	if err := s.DB.WithContext(ctx).
		Where("snapshot_id = ?", snapshot.ID).First(&payload).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			// A complete manifest without its payload cannot happen through the
			// build path (both are written in one transaction), so this means
			// external interference. Refusing is the only safe answer: serving a
			// header with no elements would look like "you are entitled to
			// nothing" on a snapshot marked complete.
			logger.Error("[sync-snapshot] complete snapshot %s has no payload; refusing to serve", snapshot.ID)
			return nil, ErrSnapshotNotFound
		}
		return nil, fmt.Errorf("load snapshot payload %s: %w", snapshot.ID, err)
	}

	items, tombstones, err := syncsnapshot.SplitStoredPayload(payload.Payload)
	if err != nil {
		return nil, fmt.Errorf("decode snapshot payload %s: %w", snapshot.ID, err)
	}
	if len(items) != snapshot.ItemCount || len(tombstones) != snapshot.TombstoneCount {
		// The manifest and the artifact must agree; a client verifies counts
		// before the digest, so serving a mismatch would waste a whole
		// pagination pass to reach the same conclusion.
		return nil, fmt.Errorf("snapshot %s payload holds %d/%d entries, manifest claims %d/%d",
			snapshot.ID, len(items), len(tombstones), snapshot.ItemCount, snapshot.TombstoneCount)
	}

	pageSize := snapshot.PageSize
	if pageSize < 1 {
		return nil, fmt.Errorf("snapshot %s has no page size", snapshot.ID)
	}
	pageItems, pageTombstones, err := syncsnapshot.SlicePage(items, tombstones, pageSize, pageIndex)
	if err != nil {
		return nil, ErrSnapshotPageOutOfRange
	}

	digest := ""
	if snapshot.SnapshotDigest != nil {
		digest = *snapshot.SnapshotDigest
	}
	page := &SnapshotPage{
		ContractVersion: snapshot.ContractVersion,
		SnapshotID:      snapshot.ID,
		Generation:      snapshot.Generation,
		GeneratedAt:     snapshot.GeneratedAt,
		PageIndex:       pageIndex,
		PageCount:       snapshot.PageCount,
		ItemCount:       snapshot.ItemCount,
		TombstoneCount:  snapshot.TombstoneCount,
		SnapshotDigest:  digest,
		// The completeness marker rides ONLY on the last page. It is what
		// authorizes removal, so it must not be reachable by a client that
		// stopped early or by a response that was truncated in transit.
		Complete:   snapshot.Complete && pageIndex == snapshot.PageCount-1,
		Items:      make([]jsonRaw, 0, len(pageItems)),
		Tombstones: make([]jsonRaw, 0, len(pageTombstones)),
		Reused:     reused,
	}
	for _, item := range pageItems {
		page.Items = append(page.Items, item)
	}
	for _, tombstone := range pageTombstones {
		page.Tombstones = append(page.Tombstones, tombstone)
	}
	return page, nil
}

// collectEntitlementState materializes the principal's active items and the
// tombstones that survive supersession.
//
// "Active" uses the same definition the legacy favorites endpoint uses — a
// favorite, or a live distribution receipt — so snapshot v2 is additive rather
// than a redefinition of what a user has.
func (s *CapabilitySyncSnapshotService) collectEntitlementState(tx *gorm.DB, principalID string) ([]syncsnapshot.Item, []syncsnapshot.Tombstone, error) {
	sources := map[string]map[string]struct{}{}
	addSource := func(itemID, source string) {
		if _, ok := sources[itemID]; !ok {
			sources[itemID] = map[string]struct{}{}
		}
		sources[itemID][source] = struct{}{}
	}

	var favoriteItemIDs []string
	if err := tx.Model(&models.ItemFavorite{}).
		Where("user_id = ?", principalID).
		Pluck("item_id", &favoriteItemIDs).Error; err != nil {
		return nil, nil, fmt.Errorf("load favorites for %s: %w", principalID, err)
	}
	for _, itemID := range favoriteItemIDs {
		addSource(itemID, syncsnapshot.EntitlementFavorite)
	}

	var distributedItemIDs []string
	if err := tx.Model(&models.ItemDistributionReceipt{}).
		Joins("JOIN item_distributions ON item_distributions.id = item_distribution_receipts.distribution_id").
		Where("item_distribution_receipts.user_id = ? AND item_distribution_receipts.receipt_status != ? AND item_distributions.status = ?",
			principalID, "dismissed", "active").
		Pluck("item_distributions.item_id", &distributedItemIDs).Error; err != nil {
		return nil, nil, fmt.Errorf("load distributions for %s: %w", principalID, err)
	}
	for _, itemID := range distributedItemIDs {
		addSource(itemID, syncsnapshot.EntitlementDistribution)
	}

	candidateIDs := make([]string, 0, len(sources))
	for itemID := range sources {
		candidateIDs = append(candidateIDs, itemID)
	}

	items := make([]syncsnapshot.Item, 0, len(candidateIDs))
	activeIDs := make(map[string]struct{}, len(candidateIDs))
	if len(candidateIDs) > 0 {
		var rows []models.CapabilityItem
		// One batched read for the page, per the list-endpoint rule: never a
		// query per entitlement.
		if err := tx.Select("id, item_type, slug, name, version, content_md5, git_sha").
			Where("id IN ? AND status = ?", candidateIDs, "active").
			Find(&rows).Error; err != nil {
			return nil, nil, fmt.Errorf("load entitled items for %s: %w", principalID, err)
		}
		for _, row := range rows {
			entitlements := make([]string, 0, 2)
			for source := range sources[row.ID] {
				entitlements = append(entitlements, source)
			}
			items = append(items, syncsnapshot.Item{
				ItemID:     row.ID,
				ItemType:   row.ItemType,
				Slug:       row.Slug,
				Name:       row.Name,
				Version:    row.Version,
				ContentMD5: row.ContentMD5,
				GitSHA:     row.GitSHA,
				Sources:    entitlements,
			})
			activeIDs[row.ID] = struct{}{}
		}
	}

	var stored []models.CapabilitySyncTombstone
	if err := tx.Where("user_id = ?", principalID).Find(&stored).Error; err != nil {
		return nil, nil, fmt.Errorf("load tombstones for %s: %w", principalID, err)
	}
	tombstones := make([]syncsnapshot.Tombstone, 0, len(stored))
	for _, row := range stored {
		// Supersession, computed here rather than stored: a currently active
		// item wins over its own older tombstone. Refavoriting therefore
		// restores the item under the SAME id in a newer snapshot instead of
		// requiring the tombstone to be deleted — which matters because V4 does
		// not hard-delete tombstones, and because deleting one would erase the
		// record a device that has been offline still needs.
		if _, active := activeIDs[row.ItemID]; active {
			continue
		}
		if row.Reason == models.SyncTombstoneReasonGitArchived && !s.LifecyclePropagation {
			// Kill switch. Suppressing the tombstone leaves the item merely
			// absent, and absence is a no-op by the core invariant — so the
			// switch cannot cause a removal in either position.
			continue
		}
		lifecycleReason := ""
		if row.LifecycleReason != nil {
			lifecycleReason = *row.LifecycleReason
		}
		tombstones = append(tombstones, syncsnapshot.Tombstone{
			ItemID:          row.ItemID,
			Reason:          row.Reason,
			LifecycleReason: lifecycleReason,
			Source:          row.Source,
			EventID:         row.EventID,
			RemovedAt:       formatSnapshotTime(row.RemovedAt),
		})
	}

	return items, tombstones, nil
}

// extendExpiry keeps a re-served snapshot alive without touching anything the
// digest covers (the manifest trigger enforces that the rest is frozen).
//
// The guard is not cosmetic: without it, every poll from every device of the
// same principal would UPDATE the same row, and under REPEATABLE READ each
// collision is a 40001 and a full rebuild. Refreshing only in the last half of
// the TTL makes that rare.
func (s *CapabilitySyncSnapshotService) extendExpiry(tx *gorm.DB, snapshot *models.CapabilitySyncSnapshot) error {
	now := s.now()
	threshold := now.Add(s.ttl() / 2)
	if snapshot.ExpiresAt.After(threshold) {
		return nil
	}
	expiresAt := now.Add(s.ttl())
	if err := tx.Model(&models.CapabilitySyncSnapshot{}).
		Where("id = ?", snapshot.ID).
		Update("expires_at", expiresAt).Error; err != nil {
		return fmt.Errorf("extend snapshot %s: %w", snapshot.ID, err)
	}
	snapshot.ExpiresAt = expiresAt
	return nil
}

// retireSuperseded bounds what a principal's older snapshots may cost: the
// newest superseded snapshot keeps a shortened lease (the grace window), and
// everything beyond the snapshotSupersededKeep cap — or past its lease — is
// deleted outright.
//
// See snapshotSupersededGrace: deleting the IMMEDIATE predecessor would
// interrupt a client mid-pagination and, on an account whose content changes
// often, could keep interrupting it. Older generations than that are a
// different case: the server has since issued two newer ids, so deleting them
// cannot strand anyone the grace window was designed to protect (F-26).
func (s *CapabilitySyncSnapshotService) retireSuperseded(tx *gorm.DB, principalID, keepID string) error {
	now := s.now()
	graceUntil := now.Add(snapshotSupersededGrace)
	// F-26: count bound first. Keep the new snapshot plus the newest
	// (snapshotSupersededKeep - 1) superseded ones; delete the rest NOW, not at
	// lease expiry — under churn faster than the grace window, lease expiry
	// alone lets full payloads accumulate without limit, and the global sweep
	// is capped at snapshotSweepBatch rows per build. The payload row goes with
	// the manifest via ON DELETE CASCADE.
	if err := tx.Exec(`
		DELETE FROM capability_sync_snapshots
		WHERE principal_id = ? AND id <> ?
		  AND id NOT IN (
			SELECT id FROM capability_sync_snapshots
			WHERE principal_id = ? AND id <> ?
			ORDER BY generation DESC
			LIMIT ?
		  )`, principalID, keepID, principalID, keepID, snapshotSupersededKeep-1).Error; err != nil {
		return fmt.Errorf("cap superseded snapshots for %s: %w", principalID, err)
	}
	if err := tx.Model(&models.CapabilitySyncSnapshot{}).
		Where("principal_id = ? AND id <> ? AND expires_at > ?", principalID, keepID, graceUntil).
		Update("expires_at", graceUntil).Error; err != nil {
		return fmt.Errorf("retire superseded snapshots for %s: %w", principalID, err)
	}
	if err := tx.Where("principal_id = ? AND id <> ? AND expires_at <= ?", principalID, keepID, now).
		Delete(&models.CapabilitySyncSnapshot{}).Error; err != nil {
		return fmt.Errorf("sweep expired snapshots for %s: %w", principalID, err)
	}
	return nil
}

// sweepExpired removes a bounded batch of expired snapshots for ANY principal.
//
// It runs outside the build transaction on purpose: deleting other principals'
// rows inside a REPEATABLE READ build would turn unrelated concurrent sync
// traffic into serialization failures for this one. Best effort — a missed
// sweep costs storage, never correctness.
func (s *CapabilitySyncSnapshotService) sweepExpired(ctx context.Context) {
	if err := s.DB.WithContext(ctx).Exec(`
		DELETE FROM capability_sync_snapshots
		WHERE id IN (
			SELECT id FROM capability_sync_snapshots
			WHERE expires_at <= ?
			LIMIT ?
		)`, s.now(), snapshotSweepBatch).Error; err != nil {
		logger.Warn("[sync-snapshot] expiry sweep failed: %v", err)
	}
}

// allocateSnapshotGeneration is the ONLY writer of the generation counter.
//
// F-5: the unique constraint stops a number being reused, not invented. A
// caller that could supply its own generation could write 42 while the counter
// held 5, and csc — which accepts only strictly greater generations — would
// then permanently reject 6..41. Thirty-six generations of silent paralysis,
// from one bad insert.
//
// Two things make that unreachable. There is no generation parameter here, so
// no Go caller can express the idea; and the database rejects both a counter
// that moves by anything other than +1 and a manifest row whose generation is
// not the counter's current value (see
// 20260805000800_materialize_capability_sync_snapshots.sql).
//
// The upsert is also the point where one principal's builds serialize: it takes
// the row lock, so a concurrent build blocks and — under REPEATABLE READ —
// aborts with 40001 rather than proceeding over a data view that predates the
// winner's commit.
func allocateSnapshotGeneration(tx *gorm.DB, principalID string, now time.Time) (int64, error) {
	var allocated int64
	row := tx.Raw(`
		INSERT INTO capability_sync_snapshot_generations
			(principal_id, generation, last_allocated_at, created_at, updated_at)
		VALUES (?, 1, ?, ?, ?)
		ON CONFLICT (principal_id) DO UPDATE
			SET generation        = capability_sync_snapshot_generations.generation + 1,
			    last_allocated_at = EXCLUDED.last_allocated_at,
			    updated_at        = EXCLUDED.updated_at
		RETURNING generation`, principalID, now, now, now).Row()
	if err := row.Scan(&allocated); err != nil {
		return 0, fmt.Errorf("allocate snapshot generation for %s: %w", principalID, err)
	}
	if allocated <= 0 {
		return 0, fmt.Errorf("allocate snapshot generation for %s: got %d", principalID, allocated)
	}
	return allocated, nil
}

func loadCurrentSnapshot(tx *gorm.DB, principalID string) (*models.CapabilitySyncSnapshot, error) {
	var snapshot models.CapabilitySyncSnapshot
	err := tx.Where("principal_id = ? AND complete = ?", principalID, true).
		Order("generation DESC").
		First(&snapshot).Error
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, nil
		}
		return nil, fmt.Errorf("load current snapshot for %s: %w", principalID, err)
	}
	return &snapshot, nil
}

// assertRepeatableRead fails the build when the transaction is not REPEATABLE
// READ. See the call site for why this is fatal rather than a warning.
func assertRepeatableRead(tx *gorm.DB) error {
	var isolation string
	if err := tx.Raw("SELECT current_setting('transaction_isolation')").Row().Scan(&isolation); err != nil {
		return fmt.Errorf("%w: could not read transaction isolation: %v", ErrSnapshotIsolation, err)
	}
	if strings.ToLower(strings.TrimSpace(isolation)) != "repeatable read" {
		return fmt.Errorf("%w: transaction is %q", ErrSnapshotIsolation, isolation)
	}
	return nil
}

func formatSnapshotTime(t time.Time) string {
	return t.UTC().Format(time.RFC3339)
}

// isSerializationFailure matches PostgreSQL 40001/40P01 without depending on
// the driver's error type, which differs between lib/pq and pgx.
func isSerializationFailure(err error) bool {
	if err == nil {
		return false
	}
	message := strings.ToLower(err.Error())
	return strings.Contains(message, "could not serialize access") ||
		strings.Contains(message, "deadlock detected") ||
		strings.Contains(message, "sqlstate 40001") ||
		strings.Contains(message, "sqlstate 40p01")
}

// repeatableRead is the isolation every snapshot build runs at. It is spelled
// out here rather than inlined so there is exactly one place to look when
// asking "is the generation protocol's precondition actually set?" — the
// assertion inside the build then proves the answer at runtime.
func repeatableRead() *sql.TxOptions {
	return &sql.TxOptions{Isolation: sql.LevelRepeatableRead}
}
