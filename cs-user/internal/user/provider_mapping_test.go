package user

import (
	"testing"

	"github.com/costrict/costrict-web/cs-user/internal/models"
)

func TestMapProviderToProfile_Github(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name   string
		claims *models.JWTClaims
		wantUser string
		wantDisplay string
	}{
		{
			name:        "valid_login",
			claims:      &models.JWTClaims{Provider: "github", PreferredUsername: "alice", Name: "Alice", ProviderUserID: "12345"},
			wantUser:    "alice",
			wantDisplay: "Alice",
		},
		{
			name:        "invalid_login_falls_back_to_gh_id",
			claims:      &models.JWTClaims{Provider: "github", PreferredUsername: "alice.bob", ProviderUserID: "12345"},
			wantUser:    "gh_12345",
			wantDisplay: "alice.bob", // PreferredUsername still surfaces as DisplayName
		},
		{
			name:        "missing_login_uses_gh_id",
			claims:      &models.JWTClaims{Provider: "github", ProviderUserID: "99999", Name: "Bob"},
			wantUser:    "gh_99999",
			wantDisplay: "Bob",
		},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := MapProviderToProfile(tc.claims)
			if got.Username != tc.wantUser {
				t.Errorf("username: want %q, got %q", tc.wantUser, got.Username)
			}
			if got.DisplayName != tc.wantDisplay {
				t.Errorf("display: want %q, got %q", tc.wantDisplay, got.DisplayName)
			}
		})
	}
}

func TestMapProviderToProfile_IdTrust(t *testing.T) {
	t.Parallel()
	// Happy path: username in external_claims passes validation.
	got := MapProviderToProfile(&models.JWTClaims{
		Provider:        "idtrust",
		Name:            "Carol",
		UniversalID:     "uni-001",
		ExternalClaims:  map[string]any{"username": "carol"},
	})
	if got.Username != "carol" {
		t.Errorf("username: want carol, got %q", got.Username)
	}
	if got.DisplayName != "Carol" {
		t.Errorf("display: want Carol, got %q", got.DisplayName)
	}

	// Reserved word in claims → fall back to idt_<universal_id>.
	got = MapProviderToProfile(&models.JWTClaims{
		Provider:       "idtrust",
		UniversalID:    "uni-002",
		ExternalClaims: map[string]any{"username": "admin"},
	})
	if got.Username != "idt_uni-002" {
		t.Errorf("expected fallback idt_uni-002, got %q", got.Username)
	}

	// No matching key → universal_id fallback.
	got = MapProviderToProfile(&models.JWTClaims{
		Provider:    "idtrust",
		UniversalID: "uni-003",
	})
	if got.Username != "idt_uni-003" {
		t.Errorf("expected idt_uni-003, got %q", got.Username)
	}
}

func TestMapProviderToProfile_Phone(t *testing.T) {
	t.Parallel()
	// E.164 with + and spaces → digits-only with p_ prefix.
	got := MapProviderToProfile(&models.JWTClaims{Provider: "phone", Phone: "+86 138 0000 1234", Name: "Dave"})
	if got.Username != "p_8613800001234" {
		t.Errorf("username: want p_8613800001234, got %q", got.Username)
	}
	if got.DisplayName != "Dave" {
		t.Errorf("display: want Dave, got %q", got.DisplayName)
	}

	// Too-short phone → no username suggestion.
	got = MapProviderToProfile(&models.JWTClaims{Provider: "phone", Phone: "12"})
	if got.Username != "" {
		t.Errorf("expected empty username for short phone, got %q", got.Username)
	}
}

func TestMapProviderToProfile_Wecom(t *testing.T) {
	t.Parallel()
	// WeCom UserId that's already charset-valid → used verbatim.
	got := MapProviderToProfile(&models.JWTClaims{
		Provider:       "wecom",
		Name:           "Eve",
		ExternalClaims: map[string]any{"UserId": "eve_corp"},
	})
	if got.Username != "eve_corp" {
		t.Errorf("username: want eve_corp, got %q", got.Username)
	}
	if got.DisplayName != "Eve" {
		t.Errorf("display: want Eve, got %q", got.DisplayName)
	}

	// No UserId → fall back to provider_user_id (sanitized).
	got = MapProviderToProfile(&models.JWTClaims{
		Provider:       "wecom",
		ProviderUserID: "OpenID_123",
	})
	if got.Username != "wecom_OpenID_123" {
		t.Errorf("expected wecom_OpenID_123, got %q", got.Username)
	}
}

func TestMapProviderToProfile_UnknownProvider(t *testing.T) {
	t.Parallel()
	got := MapProviderToProfile(&models.JWTClaims{Provider: "azure_ad", Name: "Frank"})
	if got.Username != "" {
		t.Errorf("expected empty username for unknown provider, got %q", got.Username)
	}
	if got.DisplayName != "Frank" {
		t.Errorf("expected display name passthrough, got %q", got.DisplayName)
	}
}

func TestMapProviderToProfile_Nil(t *testing.T) {
	t.Parallel()
	got := MapProviderToProfile(nil)
	if got != (ProfileSuggestion{}) {
		t.Errorf("expected zero value for nil claims, got %+v", got)
	}
}
