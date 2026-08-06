// Package adminitem exposes the platform-admin content-management surface
// (M6 · 内容管理): a cross-registry capability-item list with type / status /
// security-status filters, an across-author status switch (上下架), and an
// across-author delete.
//
// Unlike the public GET /items handler — which is scoped to the visible
// registries of the calling user — this list deliberately spans ALL registries
// (platform admins moderate every author's items) and defaults to showing
// every status (active + archived), not just active.
//
// The HTTP handlers live in handlers.go; this file owns the data logic. The
// platform-admin guard is applied by the caller (main.go mounts RegisterRoutes
// onto an already-guarded /admin group), matching the internal/audit and
// internal/adminuser module conventions.
package adminitem

import (
	"errors"
	"strings"
	"time"

	"github.com/costrict/costrict-web/server/internal/itemdelete"
	"github.com/costrict/costrict-web/server/internal/models"
	"github.com/costrict/costrict-web/server/internal/services"
	"gorm.io/gorm"
)

// Valid statuses an admin may set on an item. Mirrors the two-state lifecycle
// used everywhere else in the codebase (active ↔ archived).
const (
	StatusActive   = "active"
	StatusArchived = "archived"
)

// securityStatusGroups expands a coarse risk-group filter token into the
// concrete security_status values that belong to it. Mirrors the grouping used
// by the public item list (handlers.capability_item.go securityStatusGroups).
var securityStatusGroups = map[string][]string{
	"unknown": {"unscanned", "pending", "scanning", "error", "skipped"},
	"low":     {"clean", "low"},
	"medium":  {"medium"},
	"high":    {"high", "extreme"},
}

// ErrItemNotFound is returned when the target item id does not exist.
var ErrItemNotFound = errors.New("item not found")

// ErrInvalidStatus is returned when a status other than active|archived is given.
var ErrInvalidStatus = errors.New("invalid item status")

// Service owns the admin item queries/mutations against a shared DB handle.
type Service struct {
	db *gorm.DB
}

// NewService constructs the service around an existing DB handle.
func NewService(db *gorm.DB) *Service {
	return &Service{db: db}
}

// ListParams captures the supported list filters. Empty fields are ignored.
type ListParams struct {
	ItemType       string // exact item_type (skill|plugin|mcp|...)
	Status         string // exact status (active|archived); empty = all statuses
	SecurityStatus string // a coarse risk group or an exact security_status value
	Search         string // LIKE over name/description
	CreatedBy      string // exact author subject_id
	// MissingSecurityEval narrows to items that were NEVER security-evaluated
	// (security_status = 'unscanned'). Deliberately narrower than the
	// securityStatusGroups["unknown"] bucket (which also includes pending /
	// scanning / error / skipped) — "never evaluated" ≠ "evaluation in progress
	// or errored", so the two are separate filters, not the same constant.
	MissingSecurityEval bool
	// MissingScore narrows to items with no positive experience score
	// (experience_score <= 0): the upstream final_score was absent/zero.
	MissingScore bool
	Page         int
	PageSize     int
}

// ItemRow is the flat, frontend-facing shape for one content row. It carries the
// moderation-relevant fields plus the resolved repo name for display.
type ItemRow struct {
	ID              string  `json:"id"`
	Name            string  `json:"name"`
	Slug            string  `json:"slug"`
	ItemType        string  `json:"itemType"`
	Category        string  `json:"category"`
	Source          string  `json:"source"`
	Status          string  `json:"status"`
	SecurityStatus  string  `json:"securityStatus"`
	ExperienceScore float64 `json:"experienceScore"`
	CreatedBy       string  `json:"createdBy"`
	RegistryID      string  `json:"registryId"`
	RepoName        string  `json:"repoName"`
	UpdatedAt       string  `json:"updatedAt"`
	CreatedAt       string  `json:"createdAt"`
}

// ListItems returns a page of items across ALL registries, filtered by params.
// It deliberately does NOT apply the per-user visible-registry scoping used by
// the public list, because platform admins moderate every author's content.
func (s *Service) ListItems(p ListParams) ([]ItemRow, int64, error) {
	page := p.Page
	if page < 1 {
		page = 1
	}
	pageSize := p.PageSize
	if pageSize < 1 || pageSize > 200 {
		pageSize = 20
	}

	query := s.applyListFilters(p)

	var total int64
	if err := query.Count(&total).Error; err != nil {
		return nil, 0, err
	}

	var items []models.CapabilityItem
	if err := query.
		Preload("Registry").
		Order("updated_at DESC").
		Limit(pageSize).
		Offset((page - 1) * pageSize).
		Find(&items).Error; err != nil {
		return nil, 0, err
	}

	return s.toRows(items), total, nil
}

// ExportRows returns ALL rows matching the filters (no pagination) for CSV
// export. Shares applyListFilters with ListItems so the export always reflects
// the exact same filter semantics the console list shows.
func (s *Service) ExportRows(p ListParams) ([]ItemRow, error) {
	var items []models.CapabilityItem
	if err := s.applyListFilters(p).
		Preload("Registry").
		Order("updated_at DESC").
		Find(&items).Error; err != nil {
		return nil, err
	}
	return s.toRows(items), nil
}

// applyListFilters builds the shared filtered query used by both ListItems and
// ExportRows. Empty/false fields are ignored; all present filters AND together.
func (s *Service) applyListFilters(p ListParams) *gorm.DB {
	query := s.db.Model(&models.CapabilityItem{})
	if p.ItemType != "" {
		query = query.Where("item_type = ?", p.ItemType)
	}
	if p.Status != "" {
		query = query.Where("status = ?", p.Status)
	}
	if p.SecurityStatus != "" {
		query = query.Where("security_status IN ?", expandSecurityStatus(p.SecurityStatus))
	}
	if p.MissingSecurityEval {
		query = query.Where("security_status = ?", "unscanned")
	}
	if p.MissingScore {
		query = query.Where("experience_score <= ?", 0)
	}
	if p.CreatedBy != "" {
		query = query.Where("created_by = ?", p.CreatedBy)
	}
	if search := strings.TrimSpace(p.Search); search != "" {
		like := "%" + search + "%"
		if s.db.Dialector.Name() == "postgres" {
			query = query.Where("name ILIKE ? OR description ILIKE ?", like, like)
		} else {
			query = query.Where("name LIKE ? OR description LIKE ?", like, like)
		}
	}
	return query
}

// toRows maps loaded items to frontend rows, batch-resolving repo names (no N+1).
func (s *Service) toRows(items []models.CapabilityItem) []ItemRow {
	repoNames := s.resolveRepoNames(items)
	rows := make([]ItemRow, 0, len(items))
	for _, item := range items {
		row := ItemRow{
			ID:              item.ID,
			Name:            item.Name,
			Slug:            item.Slug,
			ItemType:        item.ItemType,
			Category:        item.Category,
			Source:          item.Source,
			Status:          item.Status,
			SecurityStatus:  item.SecurityStatus,
			ExperienceScore: item.ExperienceScore,
			CreatedBy:       item.CreatedBy,
			RegistryID:      item.RegistryID,
			UpdatedAt:       item.UpdatedAt.Format("2006-01-02T15:04:05Z07:00"),
			CreatedAt:       item.CreatedAt.Format("2006-01-02T15:04:05Z07:00"),
		}
		if item.Registry != nil {
			row.RepoName = repoNames[item.Registry.RepoID]
		}
		rows = append(rows, row)
	}
	return rows
}

// resolveRepoNames batch-fetches repository display names for the registries the
// page references (avoids N+1).
func (s *Service) resolveRepoNames(items []models.CapabilityItem) map[string]string {
	repoIDSet := make(map[string]struct{})
	for _, item := range items {
		if item.Registry != nil && item.Registry.RepoID != "" && item.Registry.RepoID != "public" {
			repoIDSet[item.Registry.RepoID] = struct{}{}
		}
	}
	names := make(map[string]string, len(repoIDSet))
	if len(repoIDSet) == 0 {
		return names
	}
	ids := make([]string, 0, len(repoIDSet))
	for id := range repoIDSet {
		ids = append(ids, id)
	}
	var repos []models.Repository
	s.db.Select("id, name").Where("id IN ?", ids).Find(&repos)
	for _, repo := range repos {
		names[repo.ID] = repo.Name
	}
	return names
}

// ItemMeta is the minimal set of audit-relevant fields about an item, captured
// before a mutation so the audit payload still describes the (possibly hard-
// deleted) target rather than leaving a dangling UUID.
type ItemMeta struct {
	CreatedBy string
	ItemType  string
	Name      string
	Status    string
}

// GetItemMeta loads only the audit-relevant fields for an item without the
// heavier Preload/repo-name resolution GetItem does. Returns ErrItemNotFound
// when the id does not exist.
func (s *Service) GetItemMeta(id string) (*ItemMeta, error) {
	var item models.CapabilityItem
	if err := s.db.Select("created_by", "item_type", "name", "status").First(&item, "id = ?", id).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, ErrItemNotFound
		}
		return nil, err
	}
	return &ItemMeta{
		CreatedBy: item.CreatedBy,
		ItemType:  item.ItemType,
		Name:      item.Name,
		Status:    item.Status,
	}, nil
}

// GetItem loads a single item with its registry for the detail drawer.
func (s *Service) GetItem(id string) (*models.CapabilityItem, string, error) {
	var item models.CapabilityItem
	if err := s.db.Preload("Registry").First(&item, "id = ?", id).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, "", ErrItemNotFound
		}
		return nil, "", err
	}
	repoName := ""
	if item.Registry != nil {
		repoName = s.resolveRepoNames([]models.CapabilityItem{item})[item.Registry.RepoID]
	}
	return &item, repoName, nil
}

// SetStatus flips an item's lifecycle status (上下架). status must be one of
// active|archived. No author check — platform admins moderate any author.
//
// Runs in a transaction because taking an item off the shelf is now two writes
// that must land together: the status flip and the moderation tombstones that
// tell every holder's device to unload it (F-27). Committing the flip without
// the tombstones is the exact defect this fixes; committing tombstones without
// the flip would unload a capability that is still on sale.
func (s *Service) SetStatus(id, status string) error {
	if status != StatusActive && status != StatusArchived {
		return ErrInvalidStatus
	}
	return s.db.Transaction(func(tx *gorm.DB) error {
		return setItemStatusTx(tx, id, status)
	})
}

// setItemStatusTx applies one status write inside tx and, when it is a genuine
// active -> archived transition, records the moderation tombstones.
//
// # Why the transition is claimed by a predicate rather than read first
//
// The tombstone's EventID is the client's dedup key and MUST rotate on a new
// removal, and must NOT rotate otherwise (RecordEntitlementRemovalTx explains
// what each mistake costs). "Was this a removal?" therefore has to be answered
// by the write itself: a read-then-write, which is what this function replaced,
// lets two concurrent archives both observe `active` and both mint a fresh
// event id for one transition, and lets an archive that lost the race mint one
// for no transition at all.
//
// The predicate is `status = 'active'` specifically, not `status <> 'archived'`.
// The snapshot's active set is exactly `status = 'active'`, so a row in any
// other status is ALREADY absent from every holder's entitlements — moving it
// to archived ends nothing and must not be reported as a removal.
//
// A row that the claim does not match still gets its requested status through
// the unconditional write below, which is byte-for-byte the statement this
// endpoint always issued. That matters beyond preserving behaviour: writing a
// hidden status is what releases a Git lifecycle claim (models'
// guardGitLifecycleStatusWrite), so skipping the write on an already-archived
// row would quietly change who owns that row's recovery.
func setItemStatusTx(tx *gorm.DB, id, status string) error {
	var exists int64
	if err := tx.Model(&models.CapabilityItem{}).Where("id = ?", id).Count(&exists).Error; err != nil {
		return err
	}
	if exists == 0 {
		return ErrItemNotFound
	}

	transitioned := false
	// models.IsCapabilityHiddenStatus rather than `== StatusArchived`: it is the
	// single definition of "off the shelf" (archived | banned | inactive), and
	// the same predicate gates the PUT /items/:id path, which accepts all three.
	// Two entry points asking the same question two ways is how one of them ends
	// up not asking it.
	if models.IsCapabilityHiddenStatus(status) {
		claim := tx.Model(&models.CapabilityItem{}).
			Where("id = ? AND status = ?", id, StatusActive).
			Update("status", status)
		if claim.Error != nil {
			return claim.Error
		}
		transitioned = claim.RowsAffected == 1
	}
	if !transitioned {
		if err := tx.Model(&models.CapabilityItem{}).Where("id = ?", id).Update("status", status).Error; err != nil {
			return err
		}
		// Restoring to active deliberately deletes NO tombstone: an active item
		// supersedes its own older tombstone when the snapshot is built, so the
		// record an offline device still needs survives while the item comes
		// back under the same id.
		return nil
	}

	_, err := services.RecordAdminArchiveTombstonesTx(tx, id, time.Now())
	return err
}

// GetItemStatuses returns the current status keyed by id for the given ids,
// skipping any that don't exist. Used to capture the prior status for batch
// audit (from→to), mirroring the single-item status path.
func (s *Service) GetItemStatuses(ids []string) map[string]string {
	out := make(map[string]string, len(ids))
	if len(ids) == 0 {
		return out
	}
	var rows []struct {
		ID     string
		Status string
	}
	s.db.Model(&models.CapabilityItem{}).Select("id, status").Where("id IN ?", ids).Find(&rows)
	for _, r := range rows {
		out[r.ID] = r.Status
	}
	return out
}

// BatchSetStatus flips the lifecycle status (上下架) of many items in a single
// transaction. status must be active|archived. ids that no longer exist are
// reported in skipped rather than updated. All-or-nothing: on any hard error
// nothing is committed and (nil, nil, err) is returned.
func (s *Service) BatchSetStatus(ids []string, status string) (updated, skipped []string, err error) {
	if status != StatusActive && status != StatusArchived {
		return nil, nil, ErrInvalidStatus
	}
	txErr := s.db.Transaction(func(tx *gorm.DB) error {
		// Reset per attempt: the closure may be re-entered on retry, and
		// appending to the previous attempt's slices would report ids twice.
		updated, skipped = nil, nil
		for _, id := range ids {
			// Same per-id path as SetStatus, so a batch take-down produces the
			// same moderation tombstones a single one does. Nothing about a
			// batch changes what a removal is.
			e := setItemStatusTx(tx, id, status)
			if errors.Is(e, ErrItemNotFound) {
				skipped = append(skipped, id)
				continue
			}
			if e != nil {
				return e
			}
			updated = append(updated, id)
		}
		return nil
	})
	if txErr != nil {
		return nil, nil, txErr
	}
	if updated == nil {
		updated = []string{}
	}
	if skipped == nil {
		skipped = []string{}
	}
	return updated, skipped, nil
}

// DeleteItem removes an item and ALL of its associated data across any author.
// The cascade (shared with handlers.DeleteItem and BatchDeleteItems) lives in
// internal/itemdelete: bundled sub-skills are hard-deleted recursively,
// dependent rows + distribution/mcp-config orphans are cleared, then the item
// itself. Forks owned by other users are intentionally left intact.
func (s *Service) DeleteItem(id string) error {
	var item models.CapabilityItem
	if err := s.db.Select("id").First(&item, "id = ?", id).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return ErrItemNotFound
		}
		return err
	}

	// W14 — admin moderation does not get to bypass the Git-backed refusal: the
	// cascade is a physical DELETE and the bound repository would recreate the
	// capability under a new uuid on its next push. Pre-flighted here so the
	// caller gets the reason before any row is touched; the cascade repeats the
	// check for rows only the recursion reaches.
	if err := models.RefuseGitBackedItems(s.db, models.CapabilityItemsWithIDs(id)); err != nil {
		return err
	}

	return s.db.Transaction(func(tx *gorm.DB) error {
		return itemdelete.CascadeDelete(tx, id)
	})
}

// BatchDeleteItems hard-deletes every id in one transaction so a single failure
// rolls the entire batch back (no partial deletes). ids that no longer exist
// when their turn comes — never existed, or were already removed as a sub-skill
// of an earlier id in the same batch — are returned in skipped rather than
// deleted. On any hard error nothing is committed and (nil, nil, err) is
// returned.
func (s *Service) BatchDeleteItems(ids []string) (deleted, skipped []string, err error) {
	// W14 (batch) — a Git-backed id is skipped, not refused. The all-or-nothing
	// contract above is about hard errors: an id that simply cannot be deleted
	// (gone, or already removed as a sub-skill) already lands in skipped, and a
	// Git-backed id is the same kind of outcome. Refusing the batch would cost
	// the admin every other deletion because of one row.
	gitBacked := make([]string, 0)
	if len(ids) > 0 {
		blocked, _, findErr := models.FindGitBackedItems(s.db, models.CapabilityItemsWithIDs(ids...))
		if findErr != nil {
			return nil, nil, findErr
		}
		if len(blocked) > 0 {
			blockedIDs := make(map[string]struct{}, len(blocked))
			for _, ref := range blocked {
				blockedIDs[ref.ID] = struct{}{}
			}
			remaining := make([]string, 0, len(ids))
			for _, id := range ids {
				if _, isGit := blockedIDs[id]; isGit {
					gitBacked = append(gitBacked, id)
					continue
				}
				remaining = append(remaining, id)
			}
			ids = remaining
		}
	}

	txErr := s.db.Transaction(func(tx *gorm.DB) error {
		deleted, skipped, err = itemdelete.CascadeDeleteMany(tx, ids)
		return err
	})
	if txErr != nil {
		return nil, nil, txErr
	}
	// Normalize nil → empty so the JSON response is [] not null (matches the
	// public batch endpoint and keeps frontend .length/.map safe).
	if deleted == nil {
		deleted = []string{}
	}
	if skipped == nil {
		skipped = []string{}
	}
	skipped = append(skipped, gitBacked...)
	return deleted, skipped, nil
}

// expandSecurityStatus turns a coarse risk-group token (unknown|low|medium|high)
// into its concrete security_status values; an unknown token is treated as an
// exact value so callers can also pass e.g. "clean" directly.
func expandSecurityStatus(value string) []string {
	if group, ok := securityStatusGroups[value]; ok {
		return group
	}
	return []string{value}
}
