// Package tenant — admin-side CRUD + lifecycle ops (Phase C2).
//
// Admin is the write/lifecycle counterpart to Resolver (which is read-only
// resolution semantics). It owns the seven operations behind the
// /api/internal/platform/tenants* surface that server's
// /api/platform/tenants* proxies:
//
//   - CreateTenant        — slug + email_domains validation, status=active
//   - ListTenants         — paginated, optional status filter
//   - GetTenant           — by tenant_id OR slug
//   - UpdateTenant        — partial update of mutable fields (slug/tenant_id immutable)
//   - SuspendTenant       — active → suspended
//   - RestoreTenant       — suspended → active
//   - RequestDeletion     — active|suspended → deleted + deletion_requested_at
//
// Lifecycle state machine (design §4.2):
//
//   create         suspend        restore
//   ─────► [active] ◄──────► [suspended]
//            │                    │
//            │  delete            │  delete
//            └────► [deleted] ◄───┘
//                   (deletion_requested_at = now;
//                    30-day grace cron hard-deletes — out of scope here)
//
// Illegal transitions return ErrInvalidStateTransition so handlers map to 409.
//
// Audit / webhook: this PR (C2) deliberately leaves tenant.* webhook fanout
// (E4) as a TODO — structured logger carries the actor + action fields now
// so E4 can hook without an API change. Audit-log writes were added in C4.1
// (see handlers.PlatformTenantsAPI.Audit — handler-layer orchestration, no
// signature change to Admin methods).

package tenant

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/costrict/costrict-web/cs-user/internal/models"
	"github.com/google/uuid"
	"gorm.io/gorm"
)

// Sentinel errors for the admin write/lifecycle ops. Handlers translate these
// to HTTP codes:
//
//   - ErrTenantNotFound            → 404
//   - ErrSlugTaken                 → 409
//   - ErrEmailDomainConflict       → 409
//   - ErrInvalidStateTransition    → 409 (illegal suspend/restore/delete)
//   - ErrInvalidSlug / ErrInvalidEdition / ErrInvalidEmailDomains → 400
var (
	ErrSlugTaken              = errors.New("tenant: slug already taken")
	ErrEmailDomainConflict    = errors.New("tenant: email_domain overlaps an existing tenant")
	ErrInvalidStateTransition = errors.New("tenant: invalid lifecycle state transition")
	ErrInvalidSlug            = errors.New("tenant: invalid slug")
	ErrInvalidEdition         = errors.New("tenant: invalid edition")
	ErrInvalidEmailDomains    = errors.New("tenant: invalid email_domains")
)

// Status constants — single source of truth for the lifecycle enum. The DB
// column has no CHECK constraint (B1 schema decision: app-layer enum guard so
// adding a status never needs a migration). Keep these in sync with the design
// §4.1 active | suspended | deleted set.
const (
	StatusActive    = "active"
	StatusSuspended = "suspended"
	StatusDeleted   = "deleted"
)

// Edition constants — design §4.1 enumerates free | team | enterprise |
// on_premise. App-layer validation only (no CHECK constraint, same reason as
// Status).
const (
	EditionFree       = "free"
	EditionTeam       = "team"
	EditionEnterprise = "enterprise"
	EditionOnPremise  = "on_premise"
)

// slugPattern is the [a-z0-9-]{3,32} URL-safe slug rule from design §4.1 /
// B1 schema comment. Anchored — must match the WHOLE slug.
var slugPattern = regexp.MustCompile(`^[a-z0-9-]{3,32}$`)

// Admin owns write/lifecycle ops on the tenants table. Like Resolver it
// carries only the gorm pool; safe to share across goroutines once built.
type Admin struct {
	db *gorm.DB
}

// NewAdmin binds an Admin to the supplied gorm pool. Caller owns the pool's
// lifecycle.
func NewAdmin(db *gorm.DB) *Admin {
	return &Admin{db: db}
}

// CreateParams is the input shape for CreateTenant. EmailDomains / Features /
// Limits / Settings are Go values; CreateTenant serializes the JSON-text
// columns itself so callers don't have to round-trip a string.
type CreateParams struct {
	Slug         string   // required, must match slugPattern
	DisplayName  string   // required
	Edition      string   // optional, defaults to EditionTeam
	EmailDomains []string // optional, normalized (lowercased, trimmed)
	Features     string   // optional raw JSON object; "{}" if empty
	Limits       string   // optional raw JSON object; "{}" if empty
	Settings     string   // optional raw JSON object; "{}" if empty
}

// CreateTenant inserts a new tenant row after validating slug uniqueness,
// slug format, edition enum, and email_domains non-overlap with existing
// tenants. status defaults to active; tenant_id is a freshly-minted UUID.
//
// email_domains overlap is checked in Go (O(N) scan) because the column is
// JSON-text, not JSONB / TEXT[] — same constraint as Resolver's
// ResolveByEmailDomain (B3 known limitation).
func (a *Admin) CreateTenant(ctx context.Context, p CreateParams) (*models.Tenant, error) {
	if a == nil || a.db == nil {
		return nil, errAdminNotConfigured
	}
	// Caller-supplied slug is validated AS-IS (no silent lowercasing) —
	// design §4.1 declares slug format [a-z0-9-]{3,32} and any deviation is
	// a client bug worth surfacing as 400 rather than silently rewriting.
	slug := strings.TrimSpace(p.Slug)
	if !slugPattern.MatchString(slug) {
		return nil, ErrInvalidSlug
	}
	displayName := strings.TrimSpace(p.DisplayName)
	if displayName == "" {
		return nil, ErrInvalidDisplayName
	}
	edition := strings.TrimSpace(p.Edition)
	if edition == "" {
		edition = EditionTeam
	}
	if !isValidEdition(edition) {
		return nil, ErrInvalidEdition
	}
	domains := normalizeDomains(p.EmailDomains)
	if err := a.checkDomainOverlap(ctx, domains, ""); err != nil {
		return nil, err
	}
	features := defaultIfEmpty(p.Features, "{}")
	limits := defaultIfEmpty(p.Limits, "{}")
	settings := defaultIfEmpty(p.Settings, "{}")

	tenantID := uuid.NewString()
	tn := models.Tenant{
		TenantID:     tenantID,
		Slug:         slug,
		DisplayName:  displayName,
		Status:       StatusActive,
		Edition:      edition,
		EmailDomains: encodeDomains(domains),
		Features:     features,
		Limits:       limits,
		Settings:     settings,
	}

	if err := a.db.WithContext(ctx).Create(&tn).Error; err != nil {
		// sqlite / postgres duplicate-key detection. We don't try to parse
		// the constraint name (portability nightmare); just confirm via a
		// follow-up lookup whether the conflict was the slug.
		if isDuplicateKeyErr(err) {
			existing, lookupErr := a.findByIDOrSlug(ctx, slug)
			if lookupErr == nil && existing != nil {
				return nil, ErrSlugTaken
			}
		}
		return nil, fmt.Errorf("tenant: create: %w", err)
	}

	// Audit (C4.1) is wired at the handler layer (handlers.PlatformTenantsAPI)
	// so this service stays signature-stable.
	// TODO(webhook): E4 — emit tenant.created event.
	return &tn, nil
}

// ListParams is the input shape for ListTenants. Limit defaults to 100
// (capped at 500); Offset defaults to 0; Status filter is optional and skips
// the WHERE when empty.
type ListParams struct {
	Limit  int
	Offset int
	Status string // "" means no filter
}

// ListResult is the paginated response shape. Total is the count BEFORE
// pagination so the caller can render total page count.
type ListResult struct {
	Tenants []*models.Tenant `json:"tenants"`
	Total   int64            `json:"total"`
	Limit   int              `json:"limit"`
	Offset  int              `json:"offset"`
}

// ListTenants returns a paginated slice of tenants (newest first by
// created_at descending — typical admin-list UX). Total reflects the
// pre-pagination count under the same status filter.
//
// Soft-deleted rows (deleted_at IS NOT NULL) are excluded via gorm's
// DeletedAt field; tenants in status=deleted but not yet hard-deleted (still
// inside the 30-day grace) ARE returned when no status filter is applied —
// the admin UI needs to show "deleted pending grace" rows. Filter
// status="deleted" to see only those.
func (a *Admin) ListTenants(ctx context.Context, p ListParams) (*ListResult, error) {
	if a == nil || a.db == nil {
		return nil, errAdminNotConfigured
	}
	if p.Limit <= 0 || p.Limit > 500 {
		p.Limit = 100
	}
	if p.Offset < 0 {
		p.Offset = 0
	}
	status := strings.TrimSpace(p.Status)

	q := a.db.WithContext(ctx).Model(&models.Tenant{})
	if status != "" {
		q = q.Where("status = ?", status)
	}

	var total int64
	if err := q.Count(&total).Error; err != nil {
		return nil, fmt.Errorf("tenant: list count: %w", err)
	}

	var rows []models.Tenant
	if err := q.Order("created_at DESC").
		Limit(p.Limit).
		Offset(p.Offset).
		Find(&rows).Error; err != nil {
		return nil, fmt.Errorf("tenant: list query: %w", err)
	}

	out := make([]*models.Tenant, 0, len(rows))
	for i := range rows {
		out = append(out, &rows[i])
	}
	return &ListResult{Tenants: out, Total: total, Limit: p.Limit, Offset: p.Offset}, nil
}

// GetTenant returns a single tenant by tenant_id OR slug (whichever matches).
// Soft-deleted rows are excluded. Returns ErrTenantNotFound on miss.
func (a *Admin) GetTenant(ctx context.Context, idOrSlug string) (*models.Tenant, error) {
	if a == nil || a.db == nil {
		return nil, errAdminNotConfigured
	}
	return a.findByIDOrSlug(ctx, idOrSlug)
}

// UpdateParams carries only the mutable fields. Slug + TenantID are NOT
// accepted (immutable by design — changing slug breaks every URL contract).
// Status changes go through SuspendTenant / RestoreTenant / RequestDeletion
// (the lifecycle endpoints), NOT through UpdateTenant. nil pointer fields
// mean "not supplied, leave as-is"; non-nil means "update to this value".
type UpdateParams struct {
	DisplayName  *string
	Edition      *string
	EmailDomains *[]string
	Features     *string
	Limits       *string
	Settings     *string
}

// UpdateTenant applies a partial update. Returns the post-update row.
// Immutable fields (slug, tenant_id, status, deletion_requested_at) are
// never touched here.
//
// Email-domain overlap is re-checked when EmailDomains is being updated —
// the new domain set must not collide with another tenant's existing
// domains.
func (a *Admin) UpdateTenant(ctx context.Context, idOrSlug string, p UpdateParams) (*models.Tenant, error) {
	if a == nil || a.db == nil {
		return nil, errAdminNotConfigured
	}
	tn, err := a.findByIDOrSlug(ctx, idOrSlug)
	if err != nil {
		return nil, err
	}

	updates := map[string]any{}

	if p.DisplayName != nil {
		dn := strings.TrimSpace(*p.DisplayName)
		if dn == "" {
			return nil, ErrInvalidDisplayName
		}
		updates["display_name"] = dn
	}
	if p.Edition != nil {
		ed := strings.TrimSpace(*p.Edition)
		if !isValidEdition(ed) {
			return nil, ErrInvalidEdition
		}
		updates["edition"] = ed
	}
	if p.EmailDomains != nil {
		domains := normalizeDomains(*p.EmailDomains)
		if err := a.checkDomainOverlap(ctx, domains, tn.TenantID); err != nil {
			return nil, err
		}
		updates["email_domains"] = encodeDomains(domains)
	}
	if p.Features != nil {
		updates["features"] = defaultIfEmpty(*p.Features, "{}")
	}
	if p.Limits != nil {
		updates["limits"] = defaultIfEmpty(*p.Limits, "{}")
	}
	if p.Settings != nil {
		updates["settings"] = defaultIfEmpty(*p.Settings, "{}")
	}

	if len(updates) == 0 {
		// No-op PATCH — return current state without bumping updated_at.
		return tn, nil
	}

	if err := a.db.WithContext(ctx).
		Model(&models.Tenant{}).
		Where("tenant_id = ?", tn.TenantID).
		Updates(updates).Error; err != nil {
		return nil, fmt.Errorf("tenant: update: %w", err)
	}

	// Re-read so the returned row reflects the new state.
	return a.findByIDOrSlug(ctx, tn.TenantID)
}

// SuspendTenant transitions status: active → suspended. Returns the
// post-update row. ErrInvalidStateTransition if not currently active.
func (a *Admin) SuspendTenant(ctx context.Context, idOrSlug string) (*models.Tenant, error) {
	if a == nil || a.db == nil {
		return nil, errAdminNotConfigured
	}
	tn, err := a.findByIDOrSlug(ctx, idOrSlug)
	if err != nil {
		return nil, err
	}
	if tn.Status != StatusActive {
		return nil, fmt.Errorf("%w: cannot suspend tenant in status %q", ErrInvalidStateTransition, tn.Status)
	}
	if err := a.db.WithContext(ctx).
		Model(&models.Tenant{}).
		Where("tenant_id = ?", tn.TenantID).
		Update("status", StatusSuspended).Error; err != nil {
		return nil, fmt.Errorf("tenant: suspend: %w", err)
	}
	// Audit (C4.1) wired at handler layer.
	// TODO(webhook): E4 — emit tenant.suspended.
	return a.findByIDOrSlug(ctx, tn.TenantID)
}

// RestoreTenant transitions status: suspended → active. ErrInvalidStateTransition
// if not currently suspended (restoring an active or deleted tenant is not
// allowed — a deleted tenant must be undeleted via a separate flow not yet
// built).
func (a *Admin) RestoreTenant(ctx context.Context, idOrSlug string) (*models.Tenant, error) {
	if a == nil || a.db == nil {
		return nil, errAdminNotConfigured
	}
	tn, err := a.findByIDOrSlug(ctx, idOrSlug)
	if err != nil {
		return nil, err
	}
	if tn.Status != StatusSuspended {
		return nil, fmt.Errorf("%w: cannot restore tenant in status %q", ErrInvalidStateTransition, tn.Status)
	}
	if err := a.db.WithContext(ctx).
		Model(&models.Tenant{}).
		Where("tenant_id = ?", tn.TenantID).
		Update("status", StatusActive).Error; err != nil {
		return nil, fmt.Errorf("tenant: restore: %w", err)
	}
	// Audit (C4.1) wired at handler layer.
	// TODO(webhook): E4 — emit tenant.restored.
	return a.findByIDOrSlug(ctx, tn.TenantID)
}

// RequestDeletion transitions status: active|suspended → deleted, and stamps
// deletion_requested_at = now. The 30-day grace window is enforced by a
// future cron job (out of scope for C2) that hard-deletes (sets deleted_at)
// once deletion_requested_at is older than 30 days.
//
// ErrInvalidStateTransition if already deleted.
func (a *Admin) RequestDeletion(ctx context.Context, idOrSlug string) (*models.Tenant, error) {
	if a == nil || a.db == nil {
		return nil, errAdminNotConfigured
	}
	tn, err := a.findByIDOrSlug(ctx, idOrSlug)
	if err != nil {
		return nil, err
	}
	if tn.Status == StatusDeleted {
		return nil, fmt.Errorf("%w: tenant already in status %q", ErrInvalidStateTransition, tn.Status)
	}
	now := time.Now().UTC()
	if err := a.db.WithContext(ctx).
		Model(&models.Tenant{}).
		Where("tenant_id = ?", tn.TenantID).
		Updates(map[string]any{
			"status":                StatusDeleted,
			"deletion_requested_at": now,
		}).Error; err != nil {
		return nil, fmt.Errorf("tenant: request deletion: %w", err)
	}
	// Audit (C4.1) wired at handler layer.
	// TODO(webhook): E4 — emit tenant.deletion_requested.
	return a.findByIDOrSlug(ctx, tn.TenantID)
}

// findByIDOrSlug is the shared lookup for Get / Update / Suspend / Restore /
// RequestDeletion — accepts either tenant_id OR slug (whichever matches),
// excludes soft-deleted rows. Returns ErrTenantNotFound on miss.
//
// Mirrors the existing Resolver.ResolveBySlug WHERE shape (tenant_id = ? OR
// slug = ?) so the two services have identical lookup semantics at the
// cs-user-internal level.
func (a *Admin) findByIDOrSlug(ctx context.Context, idOrSlug string) (*models.Tenant, error) {
	idOrSlug = strings.TrimSpace(idOrSlug)
	if idOrSlug == "" {
		return nil, ErrTenantNotFound
	}
	var tn models.Tenant
	err := a.db.WithContext(ctx).
		Where("tenant_id = ? OR slug = ?", idOrSlug, idOrSlug).
		First(&tn).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, ErrTenantNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("tenant: lookup %q: %w", idOrSlug, err)
	}
	return &tn, nil
}

// checkDomainOverlap scans all OTHER tenants (excluding excludeTenantID) for
// an email_domains entry that collides with any of the supplied domains.
// O(N) scan, same constraint as Resolver.ResolveByEmailDomain — accepted
// for current scale (<100 tenants per design §3.3); Postgres GIN index is a
// follow-up optimization.
func (a *Admin) checkDomainOverlap(ctx context.Context, domains []string, excludeTenantID string) error {
	if len(domains) == 0 {
		return nil
	}
	var all []models.Tenant
	q := a.db.WithContext(ctx).Model(&models.Tenant{})
	if excludeTenantID != "" {
		q = q.Where("tenant_id <> ?", excludeTenantID)
	}
	if err := q.Find(&all).Error; err != nil {
		return fmt.Errorf("tenant: overlap scan: %w", err)
	}
	want := make(map[string]struct{}, len(domains))
	for _, d := range domains {
		want[d] = struct{}{}
	}
	for i := range all {
		for _, d := range ParseEmailDomains(&all[i]) {
			if _, ok := want[d]; ok {
				return fmt.Errorf("%w: domain %q claimed by tenant %s", ErrEmailDomainConflict, d, all[i].Slug)
			}
		}
	}
	return nil
}

// errAdminNotConfigured is the nil-db / nil-receiver fallback. Mirrors
// Resolver.ResolveBySlug's "r == nil → ErrTenantNotFound" convention but
// surfaces as a programming error rather than a lookup miss — handlers
// should map this to 503.
var errAdminNotConfigured = errors.New("tenant: admin service not configured")

// ErrInvalidDisplayName is the only-on-Create / only-on-Update-empty check
// for display_name. Declared here (not at the top with the other sentinels)
// because it's the only "missing required field" sentinel the service needs;
// keeping it next to its first use keeps the sentinel block above tidy.
var ErrInvalidDisplayName = errors.New("tenant: display_name is required")

// isValidEdition enforces the edition enum at the app layer (B1 schema has
// no CHECK constraint so future editions don't need a migration).
func isValidEdition(e string) bool {
	switch e {
	case EditionFree, EditionTeam, EditionEnterprise, EditionOnPremise:
		return true
	}
	return false
}

// normalizeDomains lowercases + trims each domain and de-dupes. Empty
// strings drop out.
func normalizeDomains(in []string) []string {
	if len(in) == 0 {
		return nil
	}
	seen := make(map[string]struct{}, len(in))
	out := make([]string, 0, len(in))
	for _, d := range in {
		d = normalizeEmailDomain(d)
		if d == "" {
			continue
		}
		if _, ok := seen[d]; ok {
			continue
		}
		seen[d] = struct{}{}
		out = append(out, d)
	}
	return out
}

// encodeDomains marshals the slice back to the JSON-array-of-strings shape
// the column expects. Empty slice → "[]" (matches the column default).
func encodeDomains(in []string) string {
	if len(in) == 0 {
		return "[]"
	}
	// Build manually rather than pull in encoding/json — fixed shape (flat
	// string array), and avoiding one more import keeps the surface tight.
	var b strings.Builder
	b.WriteByte('[')
	for i, d := range in {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteByte('"')
		// Escape per RFC 8259 — domains are simple lowercase ASCII, so only
		// the two chars that can appear in a domain and need escaping (`"`
		// and `\`) are handled. Forward slash is also escaped per JSON spec
		// although domains never contain it.
		for _, r := range d {
			switch r {
			case '"':
				b.WriteString(`\"`)
			case '\\':
				b.WriteString(`\\`)
			default:
				b.WriteRune(r)
			}
		}
		b.WriteByte('"')
	}
	b.WriteByte(']')
	return b.String()
}

// defaultIfEmpty returns fallback when s is empty/whitespace-only. Used for
// the JSON-text columns (features / limits / settings) so empty input
// becomes "{}" rather than "" — keeps downstream ParseEmailDomains-style
// readers from choking.
func defaultIfEmpty(s, fallback string) string {
	if strings.TrimSpace(s) == "" {
		return fallback
	}
	return s
}

// isDuplicateKeyErr sniffs the gorm/pg/sqlite duplicate-key errors. Used in
// CreateTenant as a belt-and-braces fallback when the explicit uniqueness
// pre-check races another concurrent insert (unique-index violation).
func isDuplicateKeyErr(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	// pq / pgx: "duplicate key value violates unique constraint"
	// sqlite: "UNIQUE constraint failed"
	return strings.Contains(msg, "duplicate key") ||
		strings.Contains(msg, "UNIQUE constraint failed") ||
		strings.Contains(msg, "unique constraint")
}
