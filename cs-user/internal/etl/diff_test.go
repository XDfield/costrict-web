package etl

import (
	"testing"
	"time"

	"github.com/costrict/costrict-web/cs-user/internal/models"
	"gorm.io/gorm"
)

func strPtr(s string) *string        { return &s }
func timePtr(t time.Time) *time.Time { return &t }

func TestUserDiff_IdenticalRows(t *testing.T) {
	now := time.Now().UTC()
	src := &models.User{
		SubjectID:   "subj-1",
		Username:    "alice",
		DisplayName: strPtr("Alice"),
		Email:       strPtr("alice@example.com"),
		IsActive:    true,
		Status:      "active",
		LastLoginAt: timePtr(now),
	}
	tgt := *src
	diffs, changed := UserDiff(src, &tgt)
	if changed {
		t.Errorf("expected no diff for identical rows, got %d: %+v", len(diffs), diffs)
	}
}

func TestUserDiff_DetectsStringFieldChange(t *testing.T) {
	src := &models.User{SubjectID: "subj-1", Username: "alice2"}
	tgt := &models.User{SubjectID: "subj-1", Username: "alice"}
	diffs, changed := UserDiff(src, tgt)
	if !changed {
		t.Fatalf("expected changed=true")
	}
	found := false
	for _, d := range diffs {
		if d.Field == "username" && d.SourceValue == "alice2" && d.TargetValue == "alice" {
			found = true
		}
	}
	if !found {
		t.Errorf("username diff not recorded: %+v", diffs)
	}
}

func TestUserDiff_DetectsPtrStringChange(t *testing.T) {
	src := &models.User{SubjectID: "subj-1", Email: strPtr("a@b.c")}
	tgt := &models.User{SubjectID: "subj-1", Email: strPtr("old@b.c")}
	_, changed := UserDiff(src, tgt)
	if !changed {
		t.Errorf("expected changed for email pointer diff")
	}
}

func TestUserDiff_NilVsEmptyAreDistinct(t *testing.T) {
	// Source has empty string, target has nil — these should differ
	// because we preserve null-ness rather than collapsing them.
	src := &models.User{SubjectID: "subj-1", Email: strPtr("")}
	tgt := &models.User{SubjectID: "subj-1", Email: nil}
	diffs, changed := UserDiff(src, tgt)
	if !changed {
		t.Fatalf("expected changed=true for empty-string vs nil")
	}
	var sawDiff bool
	for _, d := range diffs {
		if d.Field == "email" {
			sawDiff = true
			if d.SourceValue != "" {
				t.Errorf("source value = %v, want empty string", d.SourceValue)
			}
			if d.TargetValue != nil {
				t.Errorf("target value = %v, want nil", d.TargetValue)
			}
		}
	}
	if !sawDiff {
		t.Errorf("email diff not found: %+v", diffs)
	}
}

func TestUserDiff_DetectsBoolChange(t *testing.T) {
	src := &models.User{SubjectID: "subj-1", IsActive: false}
	tgt := &models.User{SubjectID: "subj-1", IsActive: true}
	diffs, changed := UserDiff(src, tgt)
	if !changed {
		t.Fatalf("expected changed for is_active diff")
	}
	var saw bool
	for _, d := range diffs {
		if d.Field == "is_active" {
			saw = true
			if d.SourceValue != false || d.TargetValue != true {
				t.Errorf("is_active values wrong: %+v", d)
			}
		}
	}
	if !saw {
		t.Errorf("is_active diff not found: %+v", diffs)
	}
}

func TestUserDiff_DetectsTimeChange(t *testing.T) {
	t1 := time.Now().UTC()
	t2 := t1.Add(time.Hour)
	src := &models.User{SubjectID: "subj-1", LastLoginAt: timePtr(t1)}
	tgt := &models.User{SubjectID: "subj-1", LastLoginAt: timePtr(t2)}
	_, changed := UserDiff(src, tgt)
	if !changed {
		t.Errorf("expected changed for last_login_at diff")
	}
}

func TestUserDiff_NilVsTimeIsChange(t *testing.T) {
	src := &models.User{SubjectID: "subj-1", LastLoginAt: nil}
	tgt := &models.User{SubjectID: "subj-1", LastLoginAt: timePtr(time.Now())}
	_, changed := UserDiff(src, tgt)
	if !changed {
		t.Errorf("expected changed for nil vs time diff")
	}
}

func TestUserDiff_DeletedAtChange(t *testing.T) {
	now := time.Now().UTC()
	src := &models.User{SubjectID: "subj-1", DeletedAt: gorm.DeletedAt{Time: now, Valid: true}}
	tgt := &models.User{SubjectID: "subj-1", DeletedAt: gorm.DeletedAt{Valid: false}}
	_, changed := UserDiff(src, tgt)
	if !changed {
		t.Errorf("expected changed for deleted_at diff")
	}
}

func TestUserDiff_ExcludesIDAndCreatedAt(t *testing.T) {
	// ID and CreatedAt must NOT be in the diff even when different — target
	// preserves its own auto-increment ID and original creation timestamp.
	src := &models.User{ID: 999, SubjectID: "subj-1"}
	tgt := &models.User{ID: 1, SubjectID: "subj-1"}
	diffs, changed := UserDiff(src, tgt)
	if changed {
		t.Errorf("expected no diff when only ID differs, got %+v", diffs)
	}
}

func TestAuthIdentityDiff_IdenticalRows(t *testing.T) {
	src := &models.UserAuthIdentity{
		ExternalKey:   "k1",
		UserSubjectID: "subj-1",
		Provider:      "casdoor",
	}
	tgt := *src
	_, changed := AuthIdentityDiff(src, &tgt)
	if changed {
		t.Errorf("expected no diff for identical auth identity rows")
	}
}

func TestAuthIdentityDiff_DetectsProviderChange(t *testing.T) {
	src := &models.UserAuthIdentity{ExternalKey: "k1", Provider: "oauth2"}
	tgt := &models.UserAuthIdentity{ExternalKey: "k1", Provider: "casdoor"}
	diffs, changed := AuthIdentityDiff(src, tgt)
	if !changed {
		t.Fatalf("expected changed for provider diff")
	}
	var saw bool
	for _, d := range diffs {
		if d.Field == "provider" && d.SourceValue == "oauth2" && d.TargetValue == "casdoor" {
			saw = true
		}
	}
	if !saw {
		t.Errorf("provider diff missing: %+v", diffs)
	}
}

func TestAuthIdentityDiff_DetectsBoolFlags(t *testing.T) {
	src := &models.UserAuthIdentity{ExternalKey: "k1", IsPrimary: true, ExplicitlyUnbound: true}
	tgt := &models.UserAuthIdentity{ExternalKey: "k1", IsPrimary: false, ExplicitlyUnbound: false}
	diffs, _ := AuthIdentityDiff(src, tgt)
	fields := map[string]bool{}
	for _, d := range diffs {
		fields[d.Field] = true
	}
	if !fields["is_primary"] {
		t.Errorf("is_primary diff missing: %+v", diffs)
	}
	if !fields["explicitly_unbound"] {
		t.Errorf("explicitly_unbound diff missing: %+v", diffs)
	}
}

func TestUserDiff_NilInputsReturnNoDiff(t *testing.T) {
	if _, ok := UserDiff(nil, &models.User{}); ok {
		t.Errorf("nil src should produce no diff")
	}
	if _, ok := UserDiff(&models.User{}, nil); ok {
		t.Errorf("nil tgt should produce no diff")
	}
}
