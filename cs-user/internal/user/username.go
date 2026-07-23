// Package user provides username validation for the registration gate
// (REGISTRATION_PROFILE_DESIGN §6.1). Centralised here so server-side
// handlers (via RPC) and cs-user-internal callers share one rule set:
// charset, length, and reserved-words blacklist. Tenant-scoped uniqueness
// is enforced at the DB layer (idx_users_tenant_username) and queried
// separately by Service.IsUsernameAvailable.
package user

import (
	"errors"
	"regexp"
	"strings"
)

// Username constraints — exposed as vars so tests can pin them.
const (
	UsernameMinLength = 3
	UsernameMaxLength = 32
)

// usernamePattern allows [a-zA-Z0-9_-], must start with a letter or digit
// (reject leading dash/underscore so generated usernames don't collide with
// CLI flags or shell variables).
var usernamePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]*$`)

// reservedUsernames blocks route-conflict / system words. Lowercased compare;
// the list is intentionally conservative — additions are safe, removals are
// breaking. Sourced from REGISTRATION_PROFILE_DESIGN §6.1.
var reservedUsernames = map[string]bool{
	"admin": true, "root": true, "system": true, "me": true,
	"api": true, "auth": true, "register": true, "login": true,
	"logout": true, "settings": true, "help": true, "support": true,
	"sysop": true, "operator": true, "staff": true, "moderator": true,
	"official": true, "costrict": true, "casdoor": true,
	"self": true, "null": true, "undefined": true, "none": true,
	"new": true, "edit": true, "delete": true, "create": true,
	"superuser": true, "superadmin": true, "everyone": true,
}

// Username validation error tokens — handlers surface these as the
// {"error": "<token>"} body so callers can branch on the exact reason.
// Kept as values (not types) to match the existing cs-user convention
// (see identity.go's ErrExplicitlyUnbound).
var (
	ErrUsernameInvalid = errors.New("invalid_username")
	ErrUsernameTaken   = errors.New("username_taken")
	ErrUsernameReserved = errors.New("username_reserved")
)

// ValidateUsername checks charset + length + reserved-words. Returns nil on
// success, ErrUsernameInvalid for format violations, ErrUsernameReserved for
// blacklist matches. Uniqueness is NOT checked here — call IsUsernameAvailable
// separately because availability depends on tenant context which this pure
// helper intentionally does not consult.
func ValidateUsername(username string) error {
	u := strings.TrimSpace(username)
	if len(u) < UsernameMinLength || len(u) > UsernameMaxLength {
		return ErrUsernameInvalid
	}
	if !usernamePattern.MatchString(u) {
		return ErrUsernameInvalid
	}
	if reservedUsernames[strings.ToLower(u)] {
		return ErrUsernameReserved
	}
	return nil
}
