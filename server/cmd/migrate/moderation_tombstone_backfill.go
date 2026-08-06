// `migrate backfill-moderation-tombstones` — issue the removal instruction for
// items an operator took off the shelf BEFORE the moderation paths learned to
// write one (review finding F-27).
//
// Under snapshot v2 an item's absence never authorizes a client to unload it;
// only an explicit tombstone does. Until this slice, adminitem.SetStatus and
// PUT /items/:id archived an item without writing one, so every device that had
// favorited or been distributed that capability keeps it installed forever —
// silently, because the server has nothing left to notice and the client was
// never told anything. Fixing the writers stops the bleeding; the rows already
// archived need this.
//
// What it covers, and what it deliberately does not
// -------------------------------------------------
//   - COVERED: an item currently in a hidden status (archived | banned |
//     inactive) whose git_lifecycle_reason is NULL, for each principal who
//     still holds an entitlement (a favorite, or a live distribution receipt)
//     and has no tombstone for that item yet.
//
//   - NOT COVERED — historical HARD deletes. The holder set lives in
//     item_favorites and the distribution receipts, and the cascade deleted
//     both along with the item. There is no row anywhere from which the
//     database can name the people who held a capability that no longer
//     exists, so those devices cannot be reached at all: the instruction can
//     never be issued, not merely "not yet". This command reports the gap
//     rather than pretending to cover it; the fixed cascade prevents new ones.
//
//   - NOT COVERED — items Git archived (git_lifecycle_reason IS NOT NULL). The
//     truthful tombstone for those is `git_archived` carrying that reason, not
//     `admin_archived`, and stamping them as a moderation take-down would be
//     exactly the false-but-legal row the separate reasons exist to prevent.
//     They are counted and reported so the number is visible, and they belong
//     to the Git lifecycle slice, which owns that reason and its rollout flag.
//
// Why removed_at is the item's updated_at and not now()
// -----------------------------------------------------
// The database does not record when the archive happened. updated_at is the
// closest evidence it retains — the last write to the row, which for most
// archived rows IS the archive. It is an approximation and can be LATER than
// the true archive if anything touched the row afterwards (a re-scan, a tag
// rebuild), never earlier. Stamping now() instead would date every removal in
// the fleet to whenever an operator happened to run this command, which is the
// same objection backfill-git-revisions records against now() for observed_at.
// removedAt is display-only in the contract — csc orders by generation — so the
// approximation costs nothing beyond the display.
//
// Event ids are freshly minted per row, as they must be: they are the client's
// dedup key, and a device that has never seen a removal for this item has
// nothing to dedup against.
//
// An existing tombstone for a (user, item) pair is never overwritten, whatever
// its reason. The pair already carries a removal instruction; replacing it
// would rotate the event id for a transition that did not happen, and a device
// that already applied the removal would re-run removal work.
//
// Dry-run is the default and prints a row-level report; --confirm applies.

package main

import (
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"time"

	"gorm.io/gorm"
)

// moderationTombstoneBackfillBatchSize bounds each applying transaction.
const moderationTombstoneBackfillBatchSize = 500

type moderationTombstoneBackfillOptions struct {
	// Confirm inverts the default: dry-run is free, changing data takes a word.
	Confirm bool
	// Limit caps how many (user, item) pairs are written in one run (0 = all).
	Limit int
	// ReportLimit caps the per-row lines printed. Totals are always complete.
	ReportLimit int
}

const moderationTombstoneBackfillDefaultReportLimit = 50

func parseModerationTombstoneBackfillArgs(args []string) (moderationTombstoneBackfillOptions, error) {
	opts := moderationTombstoneBackfillOptions{ReportLimit: moderationTombstoneBackfillDefaultReportLimit}
	for _, arg := range args {
		switch {
		case arg == "--dry-run":
			// Accepted and ignored: dry-run is already the default.
			opts.Confirm = false
		case arg == "--confirm":
			opts.Confirm = true
		case strings.HasPrefix(arg, "--limit="):
			n, err := strconv.Atoi(strings.TrimPrefix(arg, "--limit="))
			if err != nil || n < 0 {
				return opts, fmt.Errorf("--limit expects a non-negative integer, got %q", strings.TrimPrefix(arg, "--limit="))
			}
			opts.Limit = n
		case strings.HasPrefix(arg, "--report-limit="):
			n, err := strconv.Atoi(strings.TrimPrefix(arg, "--report-limit="))
			if err != nil || n < 0 {
				return opts, fmt.Errorf("--report-limit expects a non-negative integer, got %q", strings.TrimPrefix(arg, "--report-limit="))
			}
			opts.ReportLimit = n
		default:
			return opts, fmt.Errorf("unknown flag %q", arg)
		}
	}
	return opts, nil
}

// moderationTombstoneHolders is the one definition of "who still holds an
// entitlement to this item", shared by the counters, the listing and the
// inserting statement so the plan an operator reviews cannot describe a
// different set than the one that gets written.
//
// It mirrors services.entitledUserIDsTx exactly — favorites UNION live
// distribution receipts — because the runtime writers and this backfill must
// agree on what a holder is. UNION (not UNION ALL) collapses a principal who
// holds both into the single row the table's one-per-(user,item) shape wants.
const moderationTombstoneHolders = `
	SELECT item_id, user_id FROM item_favorites
	UNION
	SELECT d.item_id, r.user_id
	  FROM item_distribution_receipts r
	  JOIN item_distributions d ON d.id = r.distribution_id
	 WHERE d.status = 'active' AND r.receipt_status <> 'dismissed'`

// moderationTombstoneEligibility is the WHERE clause identifying pairs that
// need a moderation tombstone. `i` is capability_items, `h` the holder set.
const moderationTombstoneEligibility = `
	i.status IN ('archived', 'banned', 'inactive')
	AND i.git_lifecycle_reason IS NULL
	AND NOT EXISTS (
		SELECT 1 FROM capability_sync_tombstones t
		 WHERE t.user_id = h.user_id AND t.item_id = i.id
	)`

// moderationTombstoneBackfillCandidate is one (user, item) pair to be written.
type moderationTombstoneBackfillCandidate struct {
	ItemID    string    `gorm:"column:item_id"`
	UserID    string    `gorm:"column:user_id"`
	Slug      string    `gorm:"column:slug"`
	ItemType  string    `gorm:"column:item_type"`
	Status    string    `gorm:"column:status"`
	RemovedAt time.Time `gorm:"column:removed_at"`
}

// moderationTombstoneBackfillReport is what an operator reads before saying yes.
type moderationTombstoneBackfillReport struct {
	HiddenItems int64
	// GitClaimedPairs are holder pairs on Git-archived rows: real gaps, but not
	// this command's to fill (see the header).
	GitClaimedPairs int64
	// AlreadyTombstoned are holder pairs on hidden rows that already carry a
	// tombstone — the coverage that exists before this run.
	AlreadyTombstoned int64
	EligiblePairs     int64
	EligibleItems     int64
	EligibleUsers     int64
	Selected          int
	Inserted          int64
	// OrphanTombstones counts item_deleted tombstones whose item is gone. It is
	// reported only as evidence that the delete path is now writing them; the
	// historical deletes that produced no rows cannot be counted at all.
	OrphanTombstones int64
}

func runModerationTombstoneBackfill(db *gorm.DB, opts moderationTombstoneBackfillOptions, out io.Writer) (*moderationTombstoneBackfillReport, error) {
	report := &moderationTombstoneBackfillReport{}

	if err := db.Raw(`SELECT count(*) FROM capability_items
		WHERE status IN ('archived', 'banned', 'inactive')`).
		Scan(&report.HiddenItems).Error; err != nil {
		return nil, fmt.Errorf("count hidden items: %w", err)
	}
	if err := db.Raw(`SELECT count(*) FROM capability_items i
		JOIN (` + moderationTombstoneHolders + `) h ON h.item_id = i.id
		WHERE i.status IN ('archived', 'banned', 'inactive')
		  AND i.git_lifecycle_reason IS NOT NULL
		  AND NOT EXISTS (
			SELECT 1 FROM capability_sync_tombstones t
			 WHERE t.user_id = h.user_id AND t.item_id = i.id)`).
		Scan(&report.GitClaimedPairs).Error; err != nil {
		return nil, fmt.Errorf("count Git-claimed holder pairs: %w", err)
	}
	if err := db.Raw(`SELECT count(*) FROM capability_items i
		JOIN (` + moderationTombstoneHolders + `) h ON h.item_id = i.id
		WHERE i.status IN ('archived', 'banned', 'inactive')
		  AND EXISTS (
			SELECT 1 FROM capability_sync_tombstones t
			 WHERE t.user_id = h.user_id AND t.item_id = i.id)`).
		Scan(&report.AlreadyTombstoned).Error; err != nil {
		return nil, fmt.Errorf("count already-tombstoned holder pairs: %w", err)
	}
	if err := db.Raw(`SELECT count(*), count(DISTINCT i.id), count(DISTINCT h.user_id)
		FROM capability_items i
		JOIN (`+moderationTombstoneHolders+`) h ON h.item_id = i.id
		WHERE `+moderationTombstoneEligibility).
		Row().Scan(&report.EligiblePairs, &report.EligibleItems, &report.EligibleUsers); err != nil {
		return nil, fmt.Errorf("count eligible pairs: %w", err)
	}
	if err := db.Raw(`SELECT count(*) FROM capability_sync_tombstones t
		WHERE t.reason = 'item_deleted'
		  AND NOT EXISTS (SELECT 1 FROM capability_items i WHERE i.id = t.item_id)`).
		Scan(&report.OrphanTombstones).Error; err != nil {
		return nil, fmt.Errorf("count deletion tombstones: %w", err)
	}

	listing := `SELECT i.id AS item_id, h.user_id AS user_id, i.slug, i.item_type, i.status,
			i.updated_at AS removed_at
		FROM capability_items i
		JOIN (` + moderationTombstoneHolders + `) h ON h.item_id = i.id
		WHERE ` + moderationTombstoneEligibility + `
		ORDER BY i.updated_at ASC, i.id ASC, h.user_id ASC`
	if opts.Limit > 0 {
		listing += fmt.Sprintf(" LIMIT %d", opts.Limit)
	}
	var candidates []moderationTombstoneBackfillCandidate
	if err := db.Raw(listing).Scan(&candidates).Error; err != nil {
		return nil, fmt.Errorf("load backfill candidates: %w", err)
	}
	report.Selected = len(candidates)

	mode := "DRY RUN"
	if opts.Confirm {
		mode = "APPLY"
	}
	fmt.Fprintf(out, "backfill-moderation-tombstones (%s)\n", mode)
	fmt.Fprintf(out, "  items currently off the shelf            : %d\n", report.HiddenItems)
	fmt.Fprintf(out, "  holder pairs already tombstoned          : %d\n", report.AlreadyTombstoned)
	fmt.Fprintf(out, "  holder pairs needing admin_archived      : %d (over %d item(s), %d user(s))\n",
		report.EligiblePairs, report.EligibleItems, report.EligibleUsers)
	fmt.Fprintf(out, "  selected by this run                     : %d\n", report.Selected)
	fmt.Fprintln(out, "")
	fmt.Fprintf(out, "  NOT covered — Git-archived holder pairs  : %d\n", report.GitClaimedPairs)
	fmt.Fprintln(out, "    Those rows carry a git_lifecycle_reason, so their truthful tombstone is")
	fmt.Fprintln(out, "    'git_archived' with that reason. Stamping them 'admin_archived' would be a")
	fmt.Fprintln(out, "    false statement about why the capability went away. They belong to the Git")
	fmt.Fprintln(out, "    lifecycle slice, which owns that reason and its rollout flag.")
	fmt.Fprintln(out, "")
	fmt.Fprintln(out, "  NOT covered — historical HARD deletes    : unknowable (not 0)")
	fmt.Fprintln(out, "    A delete cascaded away item_favorites and the distribution receipts, so no")
	fmt.Fprintln(out, "    row remains from which to name the people who held the capability. Those")
	fmt.Fprintln(out, "    devices cannot be reached by any backfill; the instruction can never be")
	fmt.Fprintln(out, "    issued. The fixed cascade tombstones holders BEFORE deleting them, so this")
	fmt.Fprintln(out, "    set stops growing from now on.")
	fmt.Fprintf(out, "    (deletion tombstones already recorded for now-absent items: %d)\n", report.OrphanTombstones)
	fmt.Fprintln(out, "")

	shown := 0
	for _, candidate := range candidates {
		if opts.ReportLimit > 0 && shown >= opts.ReportLimit {
			fmt.Fprintf(out, "  ... %d more not shown (raise --report-limit to see them)\n",
				len(candidates)-shown)
			break
		}
		shown++
		fmt.Fprintf(out, "  %s  user=%-28s  %-8s  %-40s  status=%s  removed_at=%s\n",
			candidate.ItemID, truncateForReport(candidate.UserID, 28), candidate.ItemType,
			truncateForReport(candidate.Slug, 40), candidate.Status,
			candidate.RemovedAt.UTC().Format(time.RFC3339))
	}
	if len(candidates) > 0 {
		fmt.Fprintln(out, "")
	}

	if !opts.Confirm {
		fmt.Fprintln(out, "no rows written (dry run). Re-run with --confirm to apply.")
		return report, nil
	}

	for start := 0; start < len(candidates); start += moderationTombstoneBackfillBatchSize {
		end := start + moderationTombstoneBackfillBatchSize
		if end > len(candidates) {
			end = len(candidates)
		}
		inserted, err := insertModerationTombstoneBatch(db, candidates[start:end])
		if err != nil {
			return report, err
		}
		report.Inserted += inserted
	}
	fmt.Fprintf(out, "inserted %d moderation tombstone(s).\n", report.Inserted)
	return report, nil
}

// insertModerationTombstoneBatch writes one bounded batch.
//
// INSERT ... SELECT, not a read-then-write loop: the eligibility predicate —
// including "has no tombstone yet" — is re-evaluated by the database inside the
// inserting statement, so a pair that a live archive or unfavorite tombstoned
// between the plan and the apply drops out of the set instead of being
// overwritten with a rotated event id. ON CONFLICT DO NOTHING covers the same
// race at the constraint.
func insertModerationTombstoneBatch(db *gorm.DB, batch []moderationTombstoneBackfillCandidate) (int64, error) {
	if len(batch) == 0 {
		return 0, nil
	}
	itemIDs := make([]string, 0, len(batch))
	userIDs := make([]string, 0, len(batch))
	seenItem := make(map[string]struct{}, len(batch))
	seenUser := make(map[string]struct{}, len(batch))
	for _, candidate := range batch {
		if _, dup := seenItem[candidate.ItemID]; !dup {
			seenItem[candidate.ItemID] = struct{}{}
			itemIDs = append(itemIDs, candidate.ItemID)
		}
		if _, dup := seenUser[candidate.UserID]; !dup {
			seenUser[candidate.UserID] = struct{}{}
			userIDs = append(userIDs, candidate.UserID)
		}
	}

	// The id/user lists narrow the batch; the eligibility predicate re-decides
	// membership. A pair present in one list but not the other is simply not
	// produced by the join, so the cross product cannot widen the write.
	result := db.Exec(`
		INSERT INTO capability_sync_tombstones
			(user_id, item_id, reason, lifecycle_reason, source, event_id, removed_at)
		SELECT h.user_id, i.id, 'admin_archived', NULL, 'moderation',
			gen_random_uuid()::text, i.updated_at
		FROM capability_items i
		JOIN (`+moderationTombstoneHolders+`) h ON h.item_id = i.id
		WHERE i.id IN ? AND h.user_id IN ?
			AND `+moderationTombstoneEligibility+`
		ON CONFLICT (user_id, item_id) DO NOTHING`, itemIDs, userIDs)
	if result.Error != nil {
		return 0, fmt.Errorf("insert moderation tombstones: %w", result.Error)
	}
	return result.RowsAffected, nil
}

func runModerationTombstoneBackfillCommand(db *gorm.DB, args []string) error {
	opts, err := parseModerationTombstoneBackfillArgs(args)
	if err != nil {
		return err
	}
	_, err = runModerationTombstoneBackfill(db, opts, os.Stdout)
	return err
}
