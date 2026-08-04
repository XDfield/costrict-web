package models

// Pre-flight refusal for operations that would destroy or re-home Git-backed
// capability rows.
//
// The BeforeUpdate/BeforeCreate guard in capability_item_git_guard.go answers
// "may this statement rewrite repository truth?". It cannot answer the two
// questions asked here, because neither goes through an UPDATE of a guarded
// column:
//
//   - DELETE removes the row outright. The repository stays bound, so the next
//     push finds no bound item, falls through to first-time discovery and
//     recreates the capability under a NEW uuid. Every reference to the old id
//     (item_favorites, item_distributions, csc's local sync state, bookmarks)
//     is left dangling — silently, and only visible after the next push.
//   - registry_id / repo_id moves are deliberately outside the guarded column
//     set (both writers legitimately place rows), but git_capability_repositories
//     pins a binding to one registry. Moving the item away from it makes the
//     binding and the row disagree about where the capability lives.
//
// Both are therefore refused up front, in the shape DeleteGitServer already
// uses: count the blocking rows, return a named sentinel, let the handler map
// it to 409 with a message that names the next step.

import (
	"errors"
	"fmt"

	"gorm.io/gorm"
)

// ErrGitBackedItemsPresent is the sentinel every pre-flight refusal wraps.
// Handlers map it to HTTP 409.
var ErrGitBackedItemsPresent = errors.New("operation is refused because it would orphan Git-backed capability items")

// gitBackedItemReportLimit bounds how many blocking rows are named in the
// error. A repository-wide delete can match thousands; the caller needs enough
// to act on, not the whole set.
const gitBackedItemReportLimit = 20

// GitBackedItemRef identifies one blocking row, carrying the repo coordinate so
// the 409 body can point at where the content actually lives.
type GitBackedItemRef struct {
	ID            string `json:"id"`
	Slug          string `json:"slug"`
	Name          string `json:"name"`
	ItemType      string `json:"itemType"`
	SourceRepoURL string `json:"repoUrl,omitempty"`
	SourceRepoRef string `json:"repoRef,omitempty"`
}

// GitBackedItemsError reports which rows blocked the operation. Total may
// exceed len(Items) when more rows matched than gitBackedItemReportLimit.
type GitBackedItemsError struct {
	Items []GitBackedItemRef
	Total int64
}

func (e *GitBackedItemsError) Error() string {
	return fmt.Sprintf("%v (%d git-backed item(s), e.g. %v)", ErrGitBackedItemsPresent, e.Total, e.IDs())
}

// Unwrap lets callers match with errors.Is(err, ErrGitBackedItemsPresent)
// without knowing this type.
func (e *GitBackedItemsError) Unwrap() error { return ErrGitBackedItemsPresent }

// IDs returns the reported (not necessarily all) blocking item ids.
func (e *GitBackedItemsError) IDs() []string {
	out := make([]string, 0, len(e.Items))
	for _, item := range e.Items {
		out = append(out, item.ID)
	}
	return out
}

// CapabilityItemScope narrows capability_items to the exact blast radius of the
// operation being checked. It must match the rows the operation would touch —
// scoping wider refuses legitimate work, scoping narrower leaves the hole open.
type CapabilityItemScope func(*gorm.DB) *gorm.DB

// CapabilityItemsWithIDs scopes to an explicit id set (delete paths, which
// operate on ids the caller already resolved).
func CapabilityItemsWithIDs(ids ...string) CapabilityItemScope {
	return func(query *gorm.DB) *gorm.DB {
		return query.Where("id IN ?", ids)
	}
}

// CapabilityItemsInRegistries scopes by registry membership. Registry-level
// delete/transfer act on `registry_id IN (...)`, so the check asks the same
// question the mutation does — asking by repo_id instead would both miss rows
// (a registry whose items were moved) and over-match (rows sharing the repo
// through another registry).
func CapabilityItemsInRegistries(registryIDs ...string) CapabilityItemScope {
	return func(query *gorm.DB) *gorm.DB {
		return query.Where("registry_id IN ?", registryIDs)
	}
}

// RefuseGitBackedItems returns a *GitBackedItemsError (wrapping
// ErrGitBackedItemsPresent) when scope matches at least one Git-backed row, and
// nil otherwise. db may be a transaction: the check then reads the same
// snapshot the mutation will write.
//
// An empty scope set is not an error — a delete of nothing blocks nothing.
func RefuseGitBackedItems(db *gorm.DB, scope CapabilityItemScope) error {
	blocking, total, err := FindGitBackedItems(db, scope)
	if err != nil {
		return err
	}
	if total == 0 {
		return nil
	}
	return &GitBackedItemsError{Items: blocking, Total: total}
}

// FindGitBackedItems returns up to gitBackedItemReportLimit Git-backed rows
// within scope plus the total number that matched.
//
// A schema without capability_items.content_backend cannot hold Git-backed rows
// at all (the column is NOT NULL DEFAULT 'db' and predates nothing that could
// have set 'git'), so the query is skipped there rather than erroring — the
// same reasoning the update guard uses, and what keeps partial SQLite fixtures
// working.
func FindGitBackedItems(db *gorm.DB, scope CapabilityItemScope) ([]GitBackedItemRef, int64, error) {
	if db == nil || scope == nil {
		return nil, 0, nil
	}
	if !capabilityItemsHaveContentBackend(db) {
		return nil, 0, nil
	}

	// Built twice rather than chained off one handle: Count is a finisher, and
	// reusing the same *gorm.DB afterwards carries its clauses into the next
	// statement.
	gitBacked := func() *gorm.DB {
		return scope(db.Model(&CapabilityItem{}).Where("content_backend = ?", ContentBackendGit))
	}

	var total int64
	if err := gitBacked().Count(&total).Error; err != nil {
		return nil, 0, err
	}
	if total == 0 {
		return nil, 0, nil
	}

	var refs []GitBackedItemRef
	if err := gitBacked().
		Select("id", "slug", "name", "item_type", "source_repo_url", "source_repo_ref").
		Order("id").
		Limit(gitBackedItemReportLimit).
		Find(&refs).Error; err != nil {
		return nil, 0, err
	}
	return refs, total, nil
}
