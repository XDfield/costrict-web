// Package user — provider → username/display_name suggestion (R4 of
// REGISTRATION_PROFILE_DESIGN). Pure function — no DB, no I/O. Inputs are
// the JWT claims Casdoor minted after brokering the upstream IdP; output
// is a best-effort suggestion the registration form can pre-fill so users
// aren't staring at a blank input.
//
// Strategy per provider (see REGISTRATION_PROFILE_DESIGN §9):
//
//   - github:     preferred_username when it already satisfies our charset;
//                 otherwise "gh_<provider_user_id>" so we always produce a
//                 syntactically valid candidate.
//   - idtrust:    external_claims["username"] / ["employee_id"] / ["login"]
//                 depending on the tenant's field_map; fallback
//                 "idt_<universal_id>".
//   - phone:      E.164 → digits-only (still charset-valid; the user can
//                 edit on the form).
//   - wecom:      external_claims["UserId"] (wecom's preferred identifier)
//                 when present; fallback "wecom_<provider_user_id>".
//   - other/empty: empty suggestion (form stays blank).
//
// Output is always advisory: ValidateUsername may still reject it (e.g.
// reserved words like "admin" leaked from an upstream IdP). The caller is
// expected to validate before saving.
package user

import (
	"fmt"
	"strings"

	"github.com/costrict/costrict-web/cs-user/internal/models"
)

// ProfileSuggestion carries the provider-derived hints. Empty fields mean
// "no suggestion" — the form should show an empty input for those.
type ProfileSuggestion struct {
	Username    string `json:"username,omitempty"`
	DisplayName string `json:"display_name,omitempty"`
}

// MapProviderToProfile produces a registration-form suggestion from the
// JWT claims. Pure & deterministic; safe to call from any goroutine.
// Returns the zero value when provider is unrecognised or the claims lack
// the identifying fields — caller treats that as "user must type it in".
func MapProviderToProfile(claims *models.JWTClaims) ProfileSuggestion {
	if claims == nil {
		return ProfileSuggestion{}
	}
	provider := strings.ToLower(strings.TrimSpace(claims.Provider))
	switch provider {
	case "github":
		return mapGithub(claims)
	case "idtrust":
		return mapIdTrust(claims)
	case "phone":
		return mapPhone(claims)
	case "wecom", "wxwork", "wechat_work":
		return mapWecom(claims)
	default:
		// Unknown provider: still surface display_name (always safe to
		// suggest) but leave username blank.
		return ProfileSuggestion{DisplayName: pickDisplayName(claims)}
	}
}

// mapGithub prefers the user's GitHub login when it's charset-compatible;
// otherwise falls back to a deterministic gh_<id>. The fallback always
// matches ValidateUsername's charset because GitHub userids are numeric.
func mapGithub(claims *models.JWTClaims) ProfileSuggestion {
	s := ProfileSuggestion{DisplayName: pickDisplayName(claims)}
	if claims.PreferredUsername != "" && ValidateUsername(claims.PreferredUsername) == nil {
		s.Username = claims.PreferredUsername
	} else if id := strings.TrimSpace(claims.ProviderUserID); id != "" {
		s.Username = "gh_" + id
	}
	return s
}

// mapIdTrust extracts the enterprise-managed identifier. Tenants using
// idtrust typically populate one of username / employee_id / login in
// ExternalClaims via the field_map. We try each in order of preference;
// the username candidate is validated before being suggested.
func mapIdTrust(claims *models.JWTClaims) ProfileSuggestion {
	s := ProfileSuggestion{DisplayName: pickDisplayName(claims)}
	for _, key := range []string{"username", "login", "employee_id"} {
		if v, ok := claims.ExternalClaims[key].(string); ok {
			v = strings.TrimSpace(v)
			if v == "" {
				continue
			}
			if ValidateUsername(v) == nil {
				s.Username = v
			}
			break
		}
	}
	if s.Username == "" && claims.UniversalID != "" {
		s.Username = "idt_" + claims.UniversalID
	}
	return s
}

// mapPhone turns an E.164 phone into a digits-only username. Stripping
// the leading + keeps the result charset-valid (alphanumeric + _-).
func mapPhone(claims *models.JWTClaims) ProfileSuggestion {
	s := ProfileSuggestion{DisplayName: pickDisplayName(claims)}
	phone := strings.TrimSpace(claims.Phone)
	if phone == "" {
		return s
	}
	digits := strings.TrimLeft(phone, "+")
	digits = strings.Map(func(r rune) rune {
		if r >= '0' && r <= '9' {
			return r
		}
		return -1
	}, digits)
	if len(digits) >= UsernameMinLength {
		s.Username = "p_" + digits
	}
	return s
}

// mapWecom prefers the WeCom UserId (an admin-assigned string), falling
// back to the openid-style provider_user_id when absent.
func mapWecom(claims *models.JWTClaims) ProfileSuggestion {
	s := ProfileSuggestion{DisplayName: pickDisplayName(claims)}
	if v, ok := claims.ExternalClaims["UserId"].(string); ok && v != "" {
		if ValidateUsername(v) == nil {
			s.Username = v
		} else {
			s.Username = "wecom_" + sanitizeForPrefix(v)
		}
	} else if id := strings.TrimSpace(claims.ProviderUserID); id != "" {
		s.Username = "wecom_" + sanitizeForPrefix(id)
	}
	return s
}

// pickDisplayName returns the most human-friendly name field available.
// Useful as a non-identifying suggestion regardless of provider.
func pickDisplayName(claims *models.JWTClaims) string {
	if claims.Name != "" {
		return claims.Name
	}
	if claims.PreferredUsername != "" {
		return claims.PreferredUsername
	}
	return ""
}

// sanitizeForPrefix strips characters ValidateUsername would reject so the
// prefixed fallback (e.g. wecom_<raw>) still parses. Used as a last-resort
// candidate — the user is expected to refine it on the form.
func sanitizeForPrefix(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	var b strings.Builder
	for _, r := range raw {
		if (r >= 'A' && r <= 'Z') || (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '_' || r == '-' {
			b.WriteRune(r)
		}
	}
	out := b.String()
	if out == "" {
		// All-stripped: use a hash-free placeholder. Caller still gets a
		// non-empty suggestion they can edit.
		return fmt.Sprintf("user%d", len(raw))
	}
	return out
}
