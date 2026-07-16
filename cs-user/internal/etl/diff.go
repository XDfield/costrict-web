// Package etl implements the one-shot data migration tool that copies
// users + user_auth_identities from costrict-web's PostgreSQL into cs-user's
// independent PostgreSQL.
//
// Design choices:
//
//  1. Schema parity — cs-user's models were extracted verbatim from server's,
//     so source/target row shape is identical. We reuse models.User /
//     models.UserAuthIdentity for both sides and don't need a transform layer.
//
//  2. Compare-then-write idempotency — instead of relying on ON CONFLICT
//     DO UPDATE WHERE (which has subtle semantics around auto-increment IDs
//     and timestamp columns), ImportUsers loads target rows by subject_id,
//     diffs field-by-field, and only writes when something actually changed.
//     This makes the second run produce zero writes by construction and gives
//     us explicit inserted/updated/unchanged counts for the report.
//
//  3. Unscoped read/write — soft-deleted rows in source must be preserved
//     in target for schema parity. All gorm calls go through Unscoped() and
//     deleted_at is treated as a regular field in the diff.
package etl

import (
	"time"

	"github.com/costrict/costrict-web/cs-user/internal/models"
	"gorm.io/gorm"
)

// FieldDiff describes a single field-level difference between source and
// target rows. Used both for the dry-run report and to decide whether an
// UPDATE is needed.
type FieldDiff struct {
	Field       string `json:"field"`
	SourceValue any    `json:"source_value"`
	TargetValue any    `json:"target_value"`
}

// UserDiff compares a source row against its target counterpart (matched by
// subject_id). Returns the list of differing fields and a changed flag.
//
// ID / CreatedAt are intentionally excluded: target's auto-increment ID and
// original creation timestamp must be preserved across updates. SubjectID is
// the join key and is always equal by definition. DeletedAt is included so
// soft-deletes propagate.
func UserDiff(src, tgt *models.User) ([]FieldDiff, bool) {
	if src == nil || tgt == nil {
		return nil, false
	}
	var diffs []FieldDiff

	diffs = appendStringDiff(diffs, "username", src.Username, tgt.Username)
	diffs = appendStringDiff(diffs, "status", src.Status, tgt.Status)

	diffs = appendPtrStringDiff(diffs, "display_name", src.DisplayName, tgt.DisplayName)
	diffs = appendPtrStringDiff(diffs, "email", src.Email, tgt.Email)
	diffs = appendPtrStringDiff(diffs, "phone", src.Phone, tgt.Phone)
	diffs = appendPtrStringDiff(diffs, "avatar_url", src.AvatarURL, tgt.AvatarURL)
	diffs = appendPtrStringDiff(diffs, "auth_provider", src.AuthProvider, tgt.AuthProvider)
	diffs = appendPtrStringDiff(diffs, "external_key", src.ExternalKey, tgt.ExternalKey)
	diffs = appendPtrStringDiff(diffs, "provider_user_id", src.ProviderUserID, tgt.ProviderUserID)
	diffs = appendPtrStringDiff(diffs, "casdoor_id", src.CasdoorID, tgt.CasdoorID)
	diffs = appendPtrStringDiff(diffs, "casdoor_universal_id", src.CasdoorUniversalID, tgt.CasdoorUniversalID)
	diffs = appendPtrStringDiff(diffs, "casdoor_sub", src.CasdoorSub, tgt.CasdoorSub)
	diffs = appendPtrStringDiff(diffs, "organization", src.Organization, tgt.Organization)

	if src.IsActive != tgt.IsActive {
		diffs = append(diffs, FieldDiff{Field: "is_active", SourceValue: src.IsActive, TargetValue: tgt.IsActive})
	}

	diffs = appendPtrTimeDiff(diffs, "last_login_at", src.LastLoginAt, tgt.LastLoginAt)
	diffs = appendPtrTimeDiff(diffs, "last_sync_at", src.LastSyncAt, tgt.LastSyncAt)

	diffs = appendDeletedAtDiff(diffs, src.DeletedAt, tgt.DeletedAt)

	return diffs, len(diffs) > 0
}

// AuthIdentityDiff mirrors UserDiff for user_auth_identities, keyed by
// external_key instead of subject_id.
func AuthIdentityDiff(src, tgt *models.UserAuthIdentity) ([]FieldDiff, bool) {
	if src == nil || tgt == nil {
		return nil, false
	}
	var diffs []FieldDiff

	diffs = appendStringDiff(diffs, "user_subject_id", src.UserSubjectID, tgt.UserSubjectID)
	diffs = appendStringDiff(diffs, "provider", src.Provider, tgt.Provider)

	diffs = appendPtrStringDiff(diffs, "issuer", src.Issuer, tgt.Issuer)
	diffs = appendPtrStringDiff(diffs, "external_subject", src.ExternalSubject, tgt.ExternalSubject)
	diffs = appendPtrStringDiff(diffs, "external_user_id", src.ExternalUserID, tgt.ExternalUserID)
	diffs = appendPtrStringDiff(diffs, "provider_user_id", src.ProviderUserID, tgt.ProviderUserID)
	diffs = appendPtrStringDiff(diffs, "display_name", src.DisplayName, tgt.DisplayName)
	diffs = appendPtrStringDiff(diffs, "email", src.Email, tgt.Email)
	diffs = appendPtrStringDiff(diffs, "phone", src.Phone, tgt.Phone)
	diffs = appendPtrStringDiff(diffs, "avatar_url", src.AvatarURL, tgt.AvatarURL)
	diffs = appendPtrStringDiff(diffs, "organization", src.Organization, tgt.Organization)

	if src.IsPrimary != tgt.IsPrimary {
		diffs = append(diffs, FieldDiff{Field: "is_primary", SourceValue: src.IsPrimary, TargetValue: tgt.IsPrimary})
	}
	if src.ExplicitlyUnbound != tgt.ExplicitlyUnbound {
		diffs = append(diffs, FieldDiff{Field: "explicitly_unbound", SourceValue: src.ExplicitlyUnbound, TargetValue: tgt.ExplicitlyUnbound})
	}

	diffs = appendPtrTimeDiff(diffs, "last_login_at", src.LastLoginAt, tgt.LastLoginAt)
	diffs = appendDeletedAtDiff(diffs, src.DeletedAt, tgt.DeletedAt)

	return diffs, len(diffs) > 0
}

func appendStringDiff(out []FieldDiff, field, src, tgt string) []FieldDiff {
	if src != tgt {
		return append(out, FieldDiff{Field: field, SourceValue: src, TargetValue: tgt})
	}
	return out
}

// appendPtrStringDiff treats nil and "" as distinct — we want to preserve
// source's exact null-ness rather than collapsing them.
func appendPtrStringDiff(out []FieldDiff, field string, src, tgt *string) []FieldDiff {
	if ptrStringEqual(src, tgt) {
		return out
	}
	return append(out, FieldDiff{Field: field, SourceValue: ptrStringValue(src), TargetValue: ptrStringValue(tgt)})
}

func appendPtrTimeDiff(out []FieldDiff, field string, src, tgt *time.Time) []FieldDiff {
	if (src == nil) != (tgt == nil) {
		return append(out, FieldDiff{Field: field, SourceValue: ptrTimeValue(src), TargetValue: ptrTimeValue(tgt)})
	}
	if src != nil && tgt != nil && !src.Equal(*tgt) {
		return append(out, FieldDiff{
			Field:       field,
			SourceValue: src.Format(time.RFC3339Nano),
			TargetValue: tgt.Format(time.RFC3339Nano),
		})
	}
	return out
}

func appendDeletedAtDiff(out []FieldDiff, src, tgt gorm.DeletedAt) []FieldDiff {
	if src.Valid != tgt.Valid {
		return append(out, FieldDiff{
			Field:       "deleted_at",
			SourceValue: deletedAtRaw(src),
			TargetValue: deletedAtRaw(tgt),
		})
	}
	if src.Valid && !src.Time.Equal(tgt.Time) {
		return append(out, FieldDiff{
			Field:       "deleted_at",
			SourceValue: src.Time.Format(time.RFC3339Nano),
			TargetValue: tgt.Time.Format(time.RFC3339Nano),
		})
	}
	return out
}

func ptrStringEqual(a, b *string) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}

func ptrStringValue(p *string) any {
	if p == nil {
		return nil
	}
	return *p
}

func ptrTimeValue(p *time.Time) any {
	if p == nil {
		return nil
	}
	return p.Format(time.RFC3339Nano)
}

func deletedAtRaw(d gorm.DeletedAt) any {
	if !d.Valid {
		return nil
	}
	return d.Time.Format(time.RFC3339Nano)
}
