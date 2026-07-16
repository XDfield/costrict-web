//go:build cgo

package user

import (
	"testing"

	"github.com/costrict/costrict-web/cs-user/internal/models"
)

// TestNormalizeJWTClaims_BackfillsNameWhenEmpty verifies the claim-name
// fallback chain matches server:1009. Without these rules, a phone-only login
// (no Name / PreferredUsername in the claim) would persist an empty display
// name and break the user's profile UI.
func TestNormalizeJWTClaims_BackfillsNameWhenEmpty(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		in   *models.JWTClaims
		want string // expected Name after normalization
	}{
		{
			name: "preferred_username backfills name",
			in:   &models.JWTClaims{PreferredUsername: "alice"},
			want: "alice",
		},
		{
			name: "name backfills preferred_username",
			in:   &models.JWTClaims{Name: "bob"},
			want: "bob",
		},
		{
			name: "phone-only falls back to phone_ prefix",
			in:   &models.JWTClaims{Phone: "+8613800000000"},
			want: "phone_+8613800000000",
		},
		{
			name: "provider_user_id fallback when no name and no phone",
			in:   &models.JWTClaims{ProviderUserID: "12345"},
			want: "12345",
		},
		{
			name: "phone wins over provider_user_id",
			in:   &models.JWTClaims{Phone: "+86123", ProviderUserID: "999"},
			want: "phone_+86123",
		},
		{
			name: "explicit name preserved",
			in:   &models.JWTClaims{Name: "explicit", Phone: "+86123"},
			want: "explicit",
		},
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			got := normalizeJWTClaims(c.in)
			if got.Name != c.want {
				t.Fatalf("Name: got %q, want %q", got.Name, c.want)
			}
		})
	}
}

func TestNormalizeJWTClaims_NilSafe(t *testing.T) {
	t.Parallel()
	if got := normalizeJWTClaims(nil); got != nil {
		t.Fatalf("nil input should return nil, got %v", got)
	}
}

// TestBuildExternalKey_Format verifies the format-priority chain
// (universal_id → sub → id) matches server:1029. The exact string is
// load-bearing — RPCWriter (P0-8b) and server's local writer must agree on
// the key for the same claim or lookups miss across DBs.
func TestBuildExternalKey_Format(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name  string
		claims *models.JWTClaims
		want  string
	}{
		{
			name: "universal_id with provider",
			claims: &models.JWTClaims{Provider: "github", UniversalID: "uuid-1"},
			want:  "casdoor:github:uuid-1",
		},
		{
			name: "universal_id without provider",
			claims: &models.JWTClaims{UniversalID: "uuid-1"},
			want:  "casdoor:uuid-1",
		},
		{
			name: "sub fallback with provider",
			claims: &models.JWTClaims{Provider: "phone", Sub: "sub-1"},
			want:  "casdoor-sub:phone:sub-1",
		},
		{
			name: "sub fallback without provider",
			claims: &models.JWTClaims{Sub: "sub-1"},
			want:  "casdoor-sub:sub-1",
		},
		{
			name: "id-only fallback",
			claims: &models.JWTClaims{ID: "id-1"},
			want:  "casdoor-id:id-1",
		},
		{
			name: "provider uppercased gets normalized",
			claims: &models.JWTClaims{Provider: "GITHUB", UniversalID: "uuid-1"},
			want:  "casdoor:github:uuid-1",
		},
		{
			name: "provider trimmed",
			claims: &models.JWTClaims{Provider: "  github  ", UniversalID: "uuid-1"},
			want:  "casdoor:github:uuid-1",
		},
		{
			name:  "empty claim returns empty",
			claims: &models.JWTClaims{},
			want:  "",
		},
		{
			name:  "nil claim returns empty",
			claims: nil,
			want:  "",
		},
		{
			name: "universal_id wins over sub and id",
			claims: &models.JWTClaims{UniversalID: "uuid-1", Sub: "sub-1", ID: "id-1"},
			want:  "casdoor:uuid-1",
		},
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			got := buildExternalKey(c.claims)
			if got != c.want {
				t.Fatalf("got %q, want %q", got, c.want)
			}
		})
	}
}

// TestBuildExternalKey_PublicWrapper confirms the public wrapper delegates
// to the package-private helper — keeps the API surface honest for any
// RPCWriter consumer that needs to derive a key without invoking writes.
func TestBuildExternalKey_PublicWrapper(t *testing.T) {
	t.Parallel()
	claims := &models.JWTClaims{Provider: "github", UniversalID: "uuid-x"}
	want := "casdoor:github:uuid-x"
	if got := BuildExternalKey(claims); got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

// TestLegacyExternalKey_Format verifies the pre-provider-keyed format used
// by historical rows. TransferIdentityToUser uses this to match identities
// bound before the provider prefix shipped.
func TestLegacyExternalKey_Format(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name   string
		claims *models.JWTClaims
		want   string
	}{
		{
			name:   "universal_id form",
			claims: &models.JWTClaims{UniversalID: "uuid-1", Provider: "github"},
			want:   "casdoor:uuid-1",
		},
		{
			name:   "sub form",
			claims: &models.JWTClaims{Sub: "sub-1", Provider: "phone"},
			want:   "casdoor-sub:sub-1",
		},
		{
			name:   "no usable field",
			claims: &models.JWTClaims{ID: "id-1"},
			want:   "",
		},
		{
			name:   "nil claims",
			claims: nil,
			want:   "",
		},
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			got := legacyExternalKey(c.claims)
			if got != c.want {
				t.Fatalf("got %q, want %q", got, c.want)
			}
		})
	}
}

// TestBuildUserAuthIdentity_PopulatesFields verifies the identity row built
// from a claim carries every claim field that the bind path expects to
// persist. Mismatched field mappings here would silently lose profile data
// on first login.
func TestBuildUserAuthIdentity_PopulatesFields(t *testing.T) {
	t.Parallel()

	claims := &models.JWTClaims{
		ID:                "id-1",
		Sub:               "sub-1",
		UniversalID:       "uuid-1",
		Name:              "alice",
		PreferredUsername: "alice",
		Email:             "alice@example.com",
		Picture:           "https://example.com/a.png",
		Owner:             "org-1",
		Provider:          "github",
		ProviderUserID:    "gh-123",
		Phone:             "+8613800000000",
	}

	got := buildUserAuthIdentity("usr-target", claims)

	if got.UserSubjectID != "usr-target" {
		t.Errorf("UserSubjectID: got %q, want usr-target", got.UserSubjectID)
	}
	if got.Provider != "github" {
		t.Errorf("Provider: got %q, want github", got.Provider)
	}
	if got.ExternalKey != "casdoor:github:uuid-1" {
		t.Errorf("ExternalKey: got %q, want casdoor:github:uuid-1", got.ExternalKey)
	}
	if got.ExternalSubject == nil || *got.ExternalSubject != "uuid-1" {
		t.Errorf("ExternalSubject: got %v, want uuid-1", got.ExternalSubject)
	}
	if got.ExternalUserID == nil || *got.ExternalUserID != "id-1" {
		t.Errorf("ExternalUserID: got %v, want id-1", got.ExternalUserID)
	}
	if got.ProviderUserID == nil || *got.ProviderUserID != "gh-123" {
		t.Errorf("ProviderUserID: got %v, want gh-123", got.ProviderUserID)
	}
	if got.DisplayName == nil || *got.DisplayName != "alice" {
		t.Errorf("DisplayName: got %v, want alice", got.DisplayName)
	}
	if got.Email == nil || *got.Email != "alice@example.com" {
		t.Errorf("Email: got %v, want alice@example.com", got.Email)
	}
	if got.Phone == nil || *got.Phone != "+8613800000000" {
		t.Errorf("Phone: got %v, want +8613800000000", got.Phone)
	}
	if got.AvatarURL == nil || *got.AvatarURL != "https://example.com/a.png" {
		t.Errorf("AvatarURL: got %v, want https://example.com/a.png", got.AvatarURL)
	}
	if got.Organization == nil || *got.Organization != "org-1" {
		t.Errorf("Organization: got %v, want org-1", got.Organization)
	}
	if got.LastLoginAt == nil {
		t.Error("LastLoginAt should be set on new identity")
	}
}

// TestBuildUserAuthIdentity_DefaultsProviderWhenMissing verifies the
// provider-less case falls back to "casdoor" rather than persisting "" —
// the ExternalKey unique index would otherwise collide on multiple
// provider-less rows.
func TestBuildUserAuthIdentity_DefaultsProviderWhenMissing(t *testing.T) {
	t.Parallel()

	claims := &models.JWTClaims{UniversalID: "uuid-1"}
	got := buildUserAuthIdentity("usr-x", claims)

	if got.Provider != "casdoor" {
		t.Fatalf("Provider: got %q, want casdoor (default)", got.Provider)
	}
	if got.ExternalKey != "casdoor:uuid-1" {
		t.Fatalf("ExternalKey: got %q, want casdoor:uuid-1", got.ExternalKey)
	}
}

// TestBuildIdentityUpdates_OmitsUnchanged verifies that repeat-logins don't
// produce spurious UPDATEs — the change-detection gate must return an empty
// map when nothing differs.
func TestBuildIdentityUpdates_OmitsUnchanged(t *testing.T) {
	t.Parallel()

	display := "alice"
	email := "alice@example.com"
	phone := "+86138"
	existing := &models.UserAuthIdentity{
		DisplayName: &display,
		Email:       &email,
		Phone:       &phone,
	}
	claims := &models.JWTClaims{
		PreferredUsername: "alice",
		Email:             "alice@example.com",
		Phone:             "+86138",
	}
	got := buildIdentityUpdates(existing, claims)
	if len(got) != 0 {
		t.Fatalf("expected empty updates, got %v", got)
	}
}

// TestBuildIdentityUpdates_DetectsChangedFields verifies each diverging field
// surfaces as an update key.
func TestBuildIdentityUpdates_DetectsChangedFields(t *testing.T) {
	t.Parallel()

	oldDisplay := "old"
	existing := &models.UserAuthIdentity{
		DisplayName: &oldDisplay,
	}
	claims := &models.JWTClaims{
		PreferredUsername: "new",
		Email:             "alice@example.com",
		Phone:             "+86138",
		Picture:           "https://example.com/a.png",
		Owner:             "org-1",
		ProviderUserID:    "gh-1",
		UniversalID:       "uuid-1",
	}
	got := buildIdentityUpdates(existing, claims)

	if got["display_name"] != "new" {
		t.Errorf("display_name: got %v, want new", got["display_name"])
	}
	if got["email"] != "alice@example.com" {
		t.Errorf("email: got %v, want alice@example.com", got["email"])
	}
	if got["phone"] != "+86138" {
		t.Errorf("phone: got %v, want +86138", got["phone"])
	}
	if got["avatar_url"] != "https://example.com/a.png" {
		t.Errorf("avatar_url: got %v", got["avatar_url"])
	}
	if got["organization"] != "org-1" {
		t.Errorf("organization: got %v", got["organization"])
	}
	if got["provider_user_id"] != "gh-1" {
		t.Errorf("provider_user_id: got %v", got["provider_user_id"])
	}
	if got["external_subject"] != "uuid-1" {
		t.Errorf("external_subject: got %v", got["external_subject"])
	}
}

// TestStringPtr_ReturnsNilForEmpty verifies the nil-on-empty contract —
// load-bearing for unique indexes that must permit multiple empty rows.
func TestStringPtr_ReturnsNilForEmpty(t *testing.T) {
	t.Parallel()
	if got := stringPtr(""); got != nil {
		t.Fatalf("got %v, want nil", got)
	}
	v := "x"
	if got := stringPtr(v); got == nil || *got != "x" {
		t.Fatalf("got %v, want *x", got)
	}
}

// TestFirstNonEmptyString_Fallbacks verifies the fallback chain collapses
// whitespace-only and empty values.
func TestFirstNonEmptyString_Fallbacks(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		in   []string
		want string
	}{
		{"first non-empty wins", []string{"", "  ", "x", "y"}, "x"},
		{"all empty returns empty", []string{"", "  "}, ""},
		{"trim preserves inner spaces", []string{"  a b  "}, "a b"},
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			if got := firstNonEmptyString(c.in...); got != c.want {
				t.Fatalf("got %q, want %q", got, c.want)
			}
		})
	}
}
