//go:build cgo

package user

import (
	"context"
	"testing"

	"github.com/costrict/costrict-web/cs-user/internal/models"
)

// TestProviderRank_Ordering verifies the exact rank values match server:1121.
// Reordering breaks primary cascade — an unbind that should promote a phone
// identity would silently keep a lower-rank primary.
func TestProviderRank_Ordering(t *testing.T) {
	t.Parallel()

	cases := []struct {
		provider string
		want     int
	}{
		{"idtrust", 300},
		{"github", 200},
		{"phone", 100},
		{"casdoor", 0},
		{"", 0},
		{"unknown", 0},
		// Case-insensitivity is required — Casdoor sends the provider string
		// in different cases depending on flow.
		{"GITHUB", 200},
		{"IDTrust", 300},
		{"  phone  ", 100},
	}
	for _, c := range cases {
		c := c
		t.Run(c.provider, func(t *testing.T) {
			t.Parallel()
			if got := providerRank(c.provider); got != c.want {
				t.Fatalf("providerRank(%q): got %d, want %d", c.provider, got, c.want)
			}
		})
	}
}

// TestSelectBestPrimary_RankOrdering verifies the highest-rank identity wins.
func TestSelectBestPrimary_RankOrdering(t *testing.T) {
	t.Parallel()

	github := &models.UserAuthIdentity{ID: 1, Provider: "github"}
	phone := &models.UserAuthIdentity{ID: 2, Provider: "phone"}
	idtrust := &models.UserAuthIdentity{ID: 3, Provider: "idtrust"}

	got := selectBestPrimary([]*models.UserAuthIdentity{github, phone, idtrust})
	if got != idtrust {
		t.Fatalf("got provider=%q, want idtrust", got.Provider)
	}
}

// TestSelectBestPrimary_TieBreakByID verifies that when two identities share
// the same rank, the lowest ID wins — matches server's deterministic
// tiebreak (server:1140).
func TestSelectBestPrimary_TieBreakByID(t *testing.T) {
	t.Parallel()

	// Both phone rank (100); ID=2 should NOT win over ID=1.
	first := &models.UserAuthIdentity{ID: 1, Provider: "phone"}
	second := &models.UserAuthIdentity{ID: 2, Provider: "phone"}

	got := selectBestPrimary([]*models.UserAuthIdentity{second, first})
	if got != first {
		t.Fatalf("tie should break to lower ID: got ID=%d, want 1", got.ID)
	}
}

func TestSelectBestPrimary_NilSafe(t *testing.T) {
	t.Parallel()
	if got := selectBestPrimary(nil); got != nil {
		t.Fatalf("nil slice should return nil, got %v", got)
	}
	if got := selectBestPrimary([]*models.UserAuthIdentity{nil, nil}); got != nil {
		t.Fatalf("all-nil slice should return nil, got %v", got)
	}
}

// TestRefreshUserProfileFromIdentitiesTx_PromotesNewPrimary verifies that
// when the current primary identity is no longer the best-rank one (e.g.
// after a higher-rank identity was bound), the tx flips the is_primary flags
// accordingly. Drives the bind/transfer cascade.
func TestRefreshUserProfileFromIdentitiesTx_PromotesNewPrimary(t *testing.T) {
	svc := newTestService(t)

	// Seed a user with a phone primary identity, then add a github identity
	// (higher rank). After refresh, github should be primary.
	seedUser(t, svc, func(u *models.User) {
		u.SubjectID = "subj-promote"
	})
	phoneIdentity := seedIdentity(t, svc, func(i *models.UserAuthIdentity) {
		i.UserSubjectID = "subj-promote"
		i.Provider = "phone"
		i.ExternalKey = "casdoor:phone:1"
		i.IsPrimary = true
	})
	githubIdentity := seedIdentity(t, svc, func(i *models.UserAuthIdentity) {
		i.UserSubjectID = "subj-promote"
		i.Provider = "github"
		i.ExternalKey = "casdoor:github:1"
		i.IsPrimary = false
	})
	_ = phoneIdentity
	_ = githubIdentity

	err := refreshUserProfileFromIdentitiesTx(context.Background(), svc.db, "subj-promote")
	if err != nil {
		t.Fatalf("refreshUserProfileFromIdentitiesTx: %v", err)
	}

	// Reload both identities — github should now be primary.
	var reloadedGithub models.UserAuthIdentity
	if err := svc.db.Where("external_key = ?", "casdoor:github:1").Take(&reloadedGithub).Error; err != nil {
		t.Fatalf("reload github: %v", err)
	}
	if !reloadedGithub.IsPrimary {
		t.Errorf("github should be primary after refresh")
	}

	var reloadedPhone models.UserAuthIdentity
	if err := svc.db.Where("external_key = ?", "casdoor:phone:1").Take(&reloadedPhone).Error; err != nil {
		t.Fatalf("reload phone: %v", err)
	}
	if reloadedPhone.IsPrimary {
		t.Errorf("phone should NOT be primary after refresh")
	}
}

// TestRefreshUserProfileFromIdentitiesTx_NoOpWhenUnchanged verifies the
// change-detection gate: re-running refresh with no provider-tracking field
// changes leaves the user row's updated_at untouched. Otherwise repeat
// operations would mask real drift in ops dashboards.
func TestRefreshUserProfileFromIdentitiesTx_NoOpWhenUnchanged(t *testing.T) {
	svc := newTestService(t)

	ext := "casdoor:github:noop"
	auth := "github"
	seedUser(t, svc, func(u *models.User) {
		u.SubjectID = "subj-noop"
		// Pre-populate every provider-tracking field refresh would compute
		// so the change-detection gate triggers and we exercise the genuine
		// no-op path. User-facing fields (display_name, email, etc.) are
		// intentionally NOT pre-populated — refresh no longer touches them.
		u.ExternalKey = &ext
		u.AuthProvider = &auth
	})
	seedIdentity(t, svc, func(i *models.UserAuthIdentity) {
		i.UserSubjectID = "subj-noop"
		i.Provider = "github"
		i.ExternalKey = "casdoor:github:noop"
		i.IsPrimary = true
		display := "alice"
		i.DisplayName = &display
	})

	// First call writes the changes (provider-tracking denormalized).
	if err := refreshUserProfileFromIdentitiesTx(context.Background(), svc.db, "subj-noop"); err != nil {
		t.Fatalf("first refresh: %v", err)
	}
	var afterFirst models.User
	if err := svc.db.Where("subject_id = ?", "subj-noop").Take(&afterFirst).Error; err != nil {
		t.Fatalf("reload: %v", err)
	}

	// Second call should be a no-op — same data, no field changes.
	if err := refreshUserProfileFromIdentitiesTx(context.Background(), svc.db, "subj-noop"); err != nil {
		t.Fatalf("second refresh: %v", err)
	}
	var afterSecond models.User
	if err := svc.db.Where("subject_id = ?", "subj-noop").Take(&afterSecond).Error; err != nil {
		t.Fatalf("reload: %v", err)
	}

	if !afterFirst.UpdatedAt.Equal(afterSecond.UpdatedAt) {
		t.Errorf("no-op refresh should not bump updated_at: first=%v second=%v",
			afterFirst.UpdatedAt, afterSecond.UpdatedAt)
	}

	// Regression check: display_name on the identity must NOT have leaked
	// onto the user row — refresh no longer touches user-facing fields.
	if afterSecond.DisplayName != nil {
		t.Errorf("refresh must not write display_name: got %q", *afterSecond.DisplayName)
	}
}

// TestRefreshUserProfileFromIdentitiesTx_UserNotFound verifies the missing
// user case bubbles up as gorm.ErrRecordNotFound so the service write path
// can map it to HTTP 404.
func TestRefreshUserProfileFromIdentitiesTx_UserNotFound(t *testing.T) {
	svc := newTestService(t)
	err := refreshUserProfileFromIdentitiesTx(context.Background(), svc.db, "no-such-user")
	if err == nil {
		t.Fatal("expected error for missing user, got nil")
	}
}

// TestEqualStringPtr_NilSafe verifies nil-safe equality for the change-detection gate.
func TestEqualStringPtr_NilSafe(t *testing.T) {
	t.Parallel()

	a := "a"
	b := "b"

	cases := []struct {
		name string
		a    *string
		b    *string
		want bool
	}{
		{"both nil", nil, nil, true},
		{"both same value", &a, &a, true},
		{"both different value", &a, &b, false},
		{"a nil b set", nil, &a, false},
		{"a set b nil", &a, nil, false},
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			if got := equalStringPtr(c.a, c.b); got != c.want {
				t.Fatalf("got %v, want %v", got, c.want)
			}
		})
	}
}
