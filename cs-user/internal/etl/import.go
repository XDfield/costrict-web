package etl

import (
	"context"
	"errors"
	"fmt"

	"github.com/costrict/costrict-web/cs-user/internal/models"
	"github.com/costrict/costrict-web/cs-user/internal/user"
	"gorm.io/gorm"
)

// ErrNilDB is returned by Import* / Export* when a nil *gorm.DB is passed.
var ErrNilDB = errors.New("etl: nil *gorm.DB")

// ErrAbort is the sentinel a callback may return to stop iteration early
// without surfacing as an error.
var ErrAbort = errors.New("etl: abort iteration")

// Stats captures write counts for one Import* invocation.
type Stats struct {
	Inserted   int               `json:"inserted"`
	Updated    int               `json:"updated"`
	Unchanged  int               `json:"unchanged"`
	Failed     int               `json:"failed"`
	DryRun     bool              `json:"dry_run"`
	FieldDiffs []FieldDiffRecord `json:"field_diffs,omitempty"`
}

// FieldDiffRecord captures one changed row's diffs for the dry-run report.
// Bounded by maxDiffRecords so a large drift doesn't blow up memory.
type FieldDiffRecord struct {
	Kind  string      `json:"kind"` // "user" or "auth_identity"
	Key   string      `json:"key"`  // subject_id or external_key
	Diffs []FieldDiff `json:"diffs"`
}

// Add merges one batch's counts into the receiver. FieldDiffs are not
// merged (they're per-batch and bounded independently by callers).
func (s *Stats) Add(other Stats) {
	s.Inserted += other.Inserted
	s.Updated += other.Updated
	s.Unchanged += other.Unchanged
	s.Failed += other.Failed
}

// ImportUsers upserts one batch of source users into the target db. For each
// row:
//
//   - target not found → INSERT (Unscoped to preserve soft-delete flag)
//   - target found + diff present → UPDATE only the differing columns
//   - target found + no diff → count as unchanged, skip write
//
// All writes in a batch go through a single transaction so a mid-batch crash
// leaves target in a consistent state. Returns aggregate Stats for the batch
// via acc (caller-allocated so the report can accumulate across batches).
//
// When dryRun is true, no writes are issued — Stats still reflects what
// *would* have happened, and FieldDiffs is populated (bounded by
// maxDiffRecords) for the report.
//
// Updates use map[string]interface{} (not struct) so that nil pointer values
// (e.g. clearing email) are written correctly. gorm struct Updates silently
// drops zero/nil fields, which would prevent nullification.
func ImportUsers(ctx context.Context, db *gorm.DB, batch []*models.User, dryRun bool, maxDiffRecords int, acc *Stats) error {
	if db == nil {
		return ErrNilDB
	}
	if len(batch) == 0 {
		return nil
	}
	// Reflect the run mode on the accumulator so the report's DryRun flag is
	// trustworthy without requiring every caller to set it manually.
	acc.DryRun = dryRun

	subjectIDs := make([]string, 0, len(batch))
	for _, u := range batch {
		if u == nil || u.SubjectID == "" {
			acc.Failed++
			continue
		}
		subjectIDs = append(subjectIDs, u.SubjectID)
	}
	if len(subjectIDs) == 0 {
		return nil
	}

	var targets []*models.User
	if err := db.WithContext(ctx).Unscoped().
		Where("subject_id IN ?", subjectIDs).
		Find(&targets).Error; err != nil {
		return fmt.Errorf("etl.ImportUsers: load targets: %w", err)
	}
	bySubject := make(map[string]*models.User, len(targets))
	for _, t := range targets {
		bySubject[t.SubjectID] = t
	}

	type plan struct {
		src     *models.User
		tgt     *models.User
		diffs   []FieldDiff
		changed bool
	}
	plans := make([]plan, 0, len(batch))
	for _, src := range batch {
		if src == nil || src.SubjectID == "" {
			continue
		}
		tgt := bySubject[src.SubjectID]
		if tgt == nil {
			plans = append(plans, plan{src: src, tgt: nil, changed: true})
			continue
		}
		diffs, changed := UserDiff(src, tgt)
		plans = append(plans, plan{src: src, tgt: tgt, diffs: diffs, changed: changed})
	}

	if dryRun {
		for _, p := range plans {
			switch {
			case p.tgt == nil:
				acc.Inserted++
			case p.changed:
				acc.Updated++
				if maxDiffRecords < 0 || len(acc.FieldDiffs) < maxDiffRecords {
					acc.FieldDiffs = append(acc.FieldDiffs, FieldDiffRecord{
						Kind:  "user",
						Key:   p.src.SubjectID,
						Diffs: p.diffs,
					})
				}
			default:
				acc.Unchanged++
			}
		}
		return nil
	}

	return db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		for _, p := range plans {
			if p.tgt == nil {
				// INSERT — zero out source ID so target picks its own
				// auto-increment (avoiding collision with source's PK).
				clone := *p.src
				clone.ID = 0
				// short_id is a cs-user-only derived column. Source (server's
				// users) doesn't carry it, so ETL's read SELECT omits it
				// (see runUsers in cmd/etl/main.go); we materialize it here
				// from the deterministic BuildShortID(subject_id) so the
				// inserted row satisfies NOT NULL on target.
				if clone.ShortID == "" && clone.SubjectID != "" {
					clone.ShortID = user.BuildShortID(clone.SubjectID)
				}
				// tenant_id is NOT NULL on target but absent on source.
				// GORM would write the zero value ("") rather than letting
				// the column DEFAULT 'default' apply, so set it explicitly.
				// Future multi-tenant migrations will need to revisit this.
				if clone.TenantID == "" {
					clone.TenantID = "default"
				}
				if err := tx.Unscoped().Create(&clone).Error; err != nil {
					acc.Failed++
					return fmt.Errorf("etl.ImportUsers: insert subject_id=%s: %w", p.src.SubjectID, err)
				}
				acc.Inserted++
				continue
			}
			if !p.changed {
				acc.Unchanged++
				continue
			}
			updates := buildUserUpdateMap(p.src, p.diffs)
			if len(updates) == 0 {
				acc.Unchanged++
				continue
			}
			if err := tx.Unscoped().Model(&models.User{}).
				Where("id = ?", p.tgt.ID).
				Updates(updates).Error; err != nil {
				acc.Failed++
				return fmt.Errorf("etl.ImportUsers: update subject_id=%s: %w", p.src.SubjectID, err)
			}
			acc.Updated++
		}
		return nil
	})
}

// buildUserUpdateMap translates a diff list into a gorm map update payload.
// Each field name maps to its new (source) value. Pointer fields preserve
// nil-ness; DeletedAt is rendered as sql.NullTime-compatible gorm.DeletedAt.
func buildUserUpdateMap(src *models.User, diffs []FieldDiff) map[string]any {
	upd := make(map[string]any, len(diffs))
	for _, d := range diffs {
		switch d.Field {
		case "username":
			upd["username"] = src.Username
		case "status":
			upd["status"] = src.Status
		case "display_name":
			upd["display_name"] = src.DisplayName
		case "email":
			upd["email"] = src.Email
		case "phone":
			upd["phone"] = src.Phone
		case "avatar_url":
			upd["avatar_url"] = src.AvatarURL
		case "auth_provider":
			upd["auth_provider"] = src.AuthProvider
		case "external_key":
			upd["external_key"] = src.ExternalKey
		case "provider_user_id":
			upd["provider_user_id"] = src.ProviderUserID
		case "is_active":
			upd["is_active"] = src.IsActive
		case "last_login_at":
			upd["last_login_at"] = src.LastLoginAt
		case "last_sync_at":
			upd["last_sync_at"] = src.LastSyncAt
		case "deleted_at":
			upd["deleted_at"] = deletedAtMapValue(src.DeletedAt)
		}
	}
	return upd
}

// deletedAtMapValue renders a gorm.DeletedAt as either nil (for "not
// soft-deleted") or time.Time (for "soft-deleted at T"). gorm translates
// these correctly into NULL / timestamp writes via the soft-delete column.
func deletedAtMapValue(d gorm.DeletedAt) any {
	if !d.Valid {
		return nil
	}
	return d.Time
}

// ImportAuthIdentities mirrors ImportUsers for user_auth_identities, keyed
// by external_key.
func ImportAuthIdentities(ctx context.Context, db *gorm.DB, batch []*models.UserAuthIdentity, dryRun bool, maxDiffRecords int, acc *Stats) error {
	if db == nil {
		return ErrNilDB
	}
	if len(batch) == 0 {
		return nil
	}
	acc.DryRun = dryRun

	keys := make([]string, 0, len(batch))
	for _, ai := range batch {
		if ai == nil || ai.ExternalKey == "" {
			acc.Failed++
			continue
		}
		keys = append(keys, ai.ExternalKey)
	}
	if len(keys) == 0 {
		return nil
	}

	var targets []*models.UserAuthIdentity
	if err := db.WithContext(ctx).Unscoped().
		Where("external_key IN ?", keys).
		Find(&targets).Error; err != nil {
		return fmt.Errorf("etl.ImportAuthIdentities: load targets: %w", err)
	}
	byKey := make(map[string]*models.UserAuthIdentity, len(targets))
	for _, t := range targets {
		byKey[t.ExternalKey] = t
	}

	type plan struct {
		src     *models.UserAuthIdentity
		tgt     *models.UserAuthIdentity
		diffs   []FieldDiff
		changed bool
	}
	plans := make([]plan, 0, len(batch))
	for _, src := range batch {
		if src == nil || src.ExternalKey == "" {
			continue
		}
		tgt := byKey[src.ExternalKey]
		if tgt == nil {
			plans = append(plans, plan{src: src, changed: true})
			continue
		}
		diffs, changed := AuthIdentityDiff(src, tgt)
		plans = append(plans, plan{src: src, tgt: tgt, diffs: diffs, changed: changed})
	}

	if dryRun {
		for _, p := range plans {
			switch {
			case p.tgt == nil:
				acc.Inserted++
			case p.changed:
				acc.Updated++
				if maxDiffRecords < 0 || len(acc.FieldDiffs) < maxDiffRecords {
					acc.FieldDiffs = append(acc.FieldDiffs, FieldDiffRecord{
						Kind:  "auth_identity",
						Key:   p.src.ExternalKey,
						Diffs: p.diffs,
					})
				}
			default:
				acc.Unchanged++
			}
		}
		return nil
	}

	return db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		for _, p := range plans {
			if p.tgt == nil {
				clone := *p.src
				clone.ID = 0
				if err := tx.Unscoped().Create(&clone).Error; err != nil {
					acc.Failed++
					return fmt.Errorf("etl.ImportAuthIdentities: insert external_key=%s: %w", p.src.ExternalKey, err)
				}
				acc.Inserted++
				continue
			}
			if !p.changed {
				acc.Unchanged++
				continue
			}
			updates := buildAuthIdentityUpdateMap(p.src, p.diffs)
			if len(updates) == 0 {
				acc.Unchanged++
				continue
			}
			if err := tx.Unscoped().Model(&models.UserAuthIdentity{}).
				Where("id = ?", p.tgt.ID).
				Updates(updates).Error; err != nil {
				acc.Failed++
				return fmt.Errorf("etl.ImportAuthIdentities: update external_key=%s: %w", p.src.ExternalKey, err)
			}
			acc.Updated++
		}
		return nil
	})
}

func buildAuthIdentityUpdateMap(src *models.UserAuthIdentity, diffs []FieldDiff) map[string]any {
	upd := make(map[string]any, len(diffs))
	for _, d := range diffs {
		switch d.Field {
		case "user_subject_id":
			upd["user_subject_id"] = src.UserSubjectID
		case "provider":
			upd["provider"] = src.Provider
		case "issuer":
			upd["issuer"] = src.Issuer
		case "external_subject":
			upd["external_subject"] = src.ExternalSubject
		case "external_user_id":
			upd["external_user_id"] = src.ExternalUserID
		case "provider_user_id":
			upd["provider_user_id"] = src.ProviderUserID
		case "display_name":
			upd["display_name"] = src.DisplayName
		case "email":
			upd["email"] = src.Email
		case "phone":
			upd["phone"] = src.Phone
		case "avatar_url":
			upd["avatar_url"] = src.AvatarURL
		case "organization":
			upd["organization"] = src.Organization
		case "is_primary":
			upd["is_primary"] = src.IsPrimary
		case "explicitly_unbound":
			upd["explicitly_unbound"] = src.ExplicitlyUnbound
		case "last_login_at":
			upd["last_login_at"] = src.LastLoginAt
		case "deleted_at":
			upd["deleted_at"] = deletedAtMapValue(src.DeletedAt)
		}
	}
	return upd
}

// (ValidateSource / DuplicateFinding removed: the only check guarded
// users.casdoor_universal_id uniqueness against target's unique index, but
// that column was dropped from cs-user. external_key uniqueness is enforced
// at write-time by the unique index on user_auth_identities and surfaced as
// a normal INSERT error.)
