// `migrate backfill-git-revisions` — seed revision 1 for Git-backed
// capabilities that were already synced before the revision writer existed.
//
// Without it those rows have a current git_sha and no history at all, and their
// first revision would be dated to whenever their next successful projection
// happened to run rather than to the projection that is actually current.
//
// Three rules, all from the contract and none of them negotiable:
//
//   - revision 1 is allocated ONLY where the item has no history. This command
//     never renumbers, never inserts "the missing earlier rows", and cannot
//     collide with a revision the sync writer allocated a moment earlier —
//     the emptiness test is part of the inserting statement, not a separate
//     read.
//   - observed_at is the item's EXISTING git_last_synced_at, not now(). The
//     seeded row states when that projection actually succeeded; stamping it
//     with the migration time would date every capability's history to whenever
//     an operator happened to run this.
//   - only currently SYNCED rows are eligible. An orphaned row's git_sha is the
//     commit that took it down, and the contract is explicit that an
//     archive-triggering commit does not become a content-history row; an
//     errored row's SHA is a stale last-success that a retry may supersede.
//     Both simply get their first revision from their next successful
//     projection, which is the more accurate answer.
//
// content_digest is deliberately left NULL
// ----------------------------------------
// The append trigger is the item's own projected content digest, and a
// Git-backed row does not store its content — the content lives in the
// repository. So the digest of the projection this command is describing is not
// computable from the database, and this command does not talk to Gitea.
//
// NULL is therefore recorded as what it is: "synthesized baseline, digest never
// observed". The schema permits it for `source='backfill'` and for nothing
// else. The first successful projection ADOPTS the digest it observes into the
// seeded row (services.adoptGitCapabilityBaselineDigest) and appends nothing,
// so a backfilled fleet does not produce one spurious revision per item on the
// first sync after deployment. Rows seeded by an earlier run of this command,
// before the column existed, are in exactly that state and need no repair —
// they are adopted on their next projection like any other baseline.
//
// Order matters for one reason only: a content change between this run and the
// first post-deployment projection is folded into the baseline instead of being
// recorded. Run this immediately before enabling revision writes and the window
// is minutes.
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

// gitRevisionBackfillBatchSize bounds each applying transaction. The whole set
// is small today, but a single transaction over every Git-backed row is a lock
// footprint nobody needs.
const gitRevisionBackfillBatchSize = 200

type gitRevisionBackfillOptions struct {
	// Confirm inverts the default: dry-run is free, changing data takes a word.
	Confirm bool
	// Limit caps how many eligible rows are seeded in one run (0 = all).
	Limit int
	// ReportLimit caps the per-row lines printed by the dry-run. The totals are
	// always complete.
	ReportLimit int
}

const gitRevisionBackfillDefaultReportLimit = 50

func parseGitRevisionBackfillArgs(args []string) (gitRevisionBackfillOptions, error) {
	opts := gitRevisionBackfillOptions{ReportLimit: gitRevisionBackfillDefaultReportLimit}
	for _, arg := range args {
		switch {
		case arg == "--dry-run":
			// Accepted and ignored: dry-run is already the default, and rejecting
			// the explicit spelling would punish the careful caller.
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

// gitRevisionBackfillCandidate is one item that would receive revision 1.
type gitRevisionBackfillCandidate struct {
	ItemID          string     `gorm:"column:id"`
	Slug            string     `gorm:"column:slug"`
	ItemType        string     `gorm:"column:item_type"`
	Status          string     `gorm:"column:status"`
	GitServerID     string     `gorm:"column:source_git_server_id"`
	GitRepoID       int64      `gorm:"column:source_git_repo_id"`
	GitRef          string     `gorm:"column:source_repo_ref"`
	ManifestPath    string     `gorm:"column:source_repo_path"`
	EntryKey        string     `gorm:"column:source_git_entry_key"`
	GitSHA          string     `gorm:"column:git_sha"`
	VersionLabel    string     `gorm:"column:version"`
	GitLastSyncedAt *time.Time `gorm:"column:git_last_synced_at"`
}

// gitRevisionBackfillReport is what an operator reads before saying yes.
type gitRevisionBackfillReport struct {
	GitBacked       int64
	Synced          int64
	AlreadyHaveRows int64
	Eligible        int64
	Ineligible      int64
	Selected        int
	Inserted        int64
	// AwaitingDigest counts baselines already seeded (by this run or an earlier
	// one) whose digest has not been observed yet. It is reported so an operator
	// can watch it drain to zero as reconcile visits each binding, instead of
	// having to trust that the adoption path ran.
	AwaitingDigest int64
}

// gitRevisionBackfillEligibility is the single definition of "may be seeded",
// shared by the counters, the candidate listing and the inserting statement, so
// the plan an operator reviews cannot describe a different set than the one
// that gets written.
const gitRevisionBackfillEligibility = `
	content_backend = 'git'
	AND git_sync_status = 'synced'
	AND git_sha ~ '^[0-9a-fA-F]{40}$'
	AND git_last_synced_at IS NOT NULL
	AND source_git_server_id <> ''
	AND source_git_repo_id > 0
	AND NOT EXISTS (
		SELECT 1 FROM capability_item_git_revisions r WHERE r.item_id = capability_items.id
	)`

func runGitRevisionBackfill(db *gorm.DB, opts gitRevisionBackfillOptions, out io.Writer) (*gitRevisionBackfillReport, error) {
	report := &gitRevisionBackfillReport{}

	if err := db.Table("capability_items").
		Where("content_backend = 'git'").Count(&report.GitBacked).Error; err != nil {
		return nil, fmt.Errorf("count Git-backed items: %w", err)
	}
	if err := db.Table("capability_items").
		Where("content_backend = 'git' AND git_sync_status = 'synced'").Count(&report.Synced).Error; err != nil {
		return nil, fmt.Errorf("count synced Git-backed items: %w", err)
	}
	if err := db.Table("capability_items").
		Where(`content_backend = 'git' AND EXISTS (
			SELECT 1 FROM capability_item_git_revisions r WHERE r.item_id = capability_items.id)`).
		Count(&report.AlreadyHaveRows).Error; err != nil {
		return nil, fmt.Errorf("count Git-backed items that already have history: %w", err)
	}
	if err := db.Table("capability_items").
		Where(gitRevisionBackfillEligibility).Count(&report.Eligible).Error; err != nil {
		return nil, fmt.Errorf("count backfill candidates: %w", err)
	}
	report.Ineligible = report.GitBacked - report.AlreadyHaveRows - report.Eligible
	if err := db.Table("capability_item_git_revisions").
		Where("source = 'backfill' AND content_digest IS NULL").Count(&report.AwaitingDigest).Error; err != nil {
		return nil, fmt.Errorf("count baselines awaiting a digest: %w", err)
	}

	query := db.Table("capability_items").
		Select("id, slug, item_type, status, source_git_server_id, source_git_repo_id, " +
			"source_repo_ref, source_repo_path, source_git_entry_key, git_sha, version, git_last_synced_at").
		Where(gitRevisionBackfillEligibility).
		Order("git_last_synced_at ASC, id ASC")
	if opts.Limit > 0 {
		query = query.Limit(opts.Limit)
	}
	var candidates []gitRevisionBackfillCandidate
	if err := query.Find(&candidates).Error; err != nil {
		return nil, fmt.Errorf("load backfill candidates: %w", err)
	}
	report.Selected = len(candidates)

	mode := "DRY RUN"
	if opts.Confirm {
		mode = "APPLY"
	}
	fmt.Fprintf(out, "backfill-git-revisions (%s)\n", mode)
	fmt.Fprintf(out, "  git-backed items              : %d\n", report.GitBacked)
	fmt.Fprintf(out, "  ... currently synced          : %d\n", report.Synced)
	fmt.Fprintf(out, "  ... already have history      : %d\n", report.AlreadyHaveRows)
	fmt.Fprintf(out, "  ... not eligible (see header) : %d\n", report.Ineligible)
	fmt.Fprintf(out, "  eligible for revision 1       : %d\n", report.Eligible)
	fmt.Fprintf(out, "  selected by this run          : %d\n", report.Selected)
	fmt.Fprintf(out, "  baselines awaiting a digest   : %d (before this run)\n", report.AwaitingDigest)
	fmt.Fprintln(out, "")
	fmt.Fprintln(out, "  Seeded rows carry no content digest: it is not computable from the")
	fmt.Fprintln(out, "  database. Each one adopts the digest observed by its next successful")
	fmt.Fprintln(out, "  projection, which appends no revision. Watch this counter drain to")
	fmt.Fprintln(out, "  zero as reconcile visits every binding.")
	fmt.Fprintln(out, "")

	shown := 0
	for _, candidate := range candidates {
		if opts.ReportLimit > 0 && shown >= opts.ReportLimit {
			fmt.Fprintf(out, "  ... %d more not shown (raise --report-limit to see them)\n",
				len(candidates)-shown)
			break
		}
		shown++
		version := strings.TrimSpace(candidate.VersionLabel)
		if version == "" {
			version = "(none)"
		}
		observed := ""
		if candidate.GitLastSyncedAt != nil {
			observed = candidate.GitLastSyncedAt.UTC().Format(time.RFC3339)
		}
		fmt.Fprintf(out, "  %s  %-8s  %-40s  rev=1  sha=%s  version=%s  observed=%s  repo=%s#%d  path=%s\n",
			candidate.ItemID, candidate.ItemType, truncateForReport(candidate.Slug, 40),
			shortSHAForReport(candidate.GitSHA), version, observed,
			candidate.GitServerID, candidate.GitRepoID, candidate.ManifestPath)
	}
	if len(candidates) > 0 {
		fmt.Fprintln(out, "")
	}

	if !opts.Confirm {
		fmt.Fprintln(out, "no rows written (dry run). Re-run with --confirm to apply.")
		return report, nil
	}

	ids := make([]string, 0, len(candidates))
	for _, candidate := range candidates {
		ids = append(ids, candidate.ItemID)
	}
	for start := 0; start < len(ids); start += gitRevisionBackfillBatchSize {
		end := start + gitRevisionBackfillBatchSize
		if end > len(ids) {
			end = len(ids)
		}
		inserted, err := insertGitRevisionBackfillBatch(db, ids[start:end])
		if err != nil {
			return report, err
		}
		report.Inserted += inserted
	}
	if err := db.Table("capability_item_git_revisions").
		Where("source = 'backfill' AND content_digest IS NULL").Count(&report.AwaitingDigest).Error; err != nil {
		return report, fmt.Errorf("count baselines awaiting a digest: %w", err)
	}
	fmt.Fprintf(out, "inserted %d revision-1 row(s); %d baseline(s) now awaiting a digest.\n",
		report.Inserted, report.AwaitingDigest)
	return report, nil
}

// insertGitRevisionBackfillBatch writes one bounded batch.
//
// INSERT ... SELECT, not a read-then-write loop: the eligibility predicate —
// including "has no history yet" — is re-evaluated by the database inside the
// inserting statement, so a sync worker that appended revision 1 between the
// plan and the apply loses this row from the set instead of colliding with it.
// ON CONFLICT DO NOTHING covers the same race at the constraint.
func insertGitRevisionBackfillBatch(db *gorm.DB, ids []string) (int64, error) {
	if len(ids) == 0 {
		return 0, nil
	}
	result := db.Exec(`
		INSERT INTO capability_item_git_revisions
			(item_id, revision_no, git_server_id, git_repo_id, git_ref, manifest_path,
			 entry_key, git_sha, version_label, source, observed_at)
		SELECT
			capability_items.id, 1, source_git_server_id, source_git_repo_id,
			COALESCE(source_repo_ref, ''), COALESCE(source_repo_path, ''),
			COALESCE(source_git_entry_key, ''), lower(git_sha),
			COALESCE(version, ''), 'backfill', git_last_synced_at
		FROM capability_items
		WHERE capability_items.id IN ?
			AND `+gitRevisionBackfillEligibility+`
		ON CONFLICT (item_id, revision_no) DO NOTHING`, ids)
	if result.Error != nil {
		return 0, fmt.Errorf("insert backfill revisions: %w", result.Error)
	}
	return result.RowsAffected, nil
}

func runGitRevisionBackfillCommand(db *gorm.DB, args []string) error {
	opts, err := parseGitRevisionBackfillArgs(args)
	if err != nil {
		return err
	}
	_, err = runGitRevisionBackfill(db, opts, os.Stdout)
	return err
}

func shortSHAForReport(sha string) string {
	trimmed := strings.TrimSpace(sha)
	if len(trimmed) <= 7 {
		return trimmed
	}
	return trimmed[:7]
}

func truncateForReport(value string, max int) string {
	if len(value) <= max {
		return value
	}
	if max <= 3 {
		return value[:max]
	}
	return value[:max-3] + "..."
}
