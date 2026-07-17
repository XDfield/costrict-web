package user

import (
	"context"
	"fmt"
	"strings"

	"github.com/costrict/costrict-web/server/internal/models"
)

// SyncUser upserts a user from JWT/Casdoor claims WITHOUT running the post-login
// hook. It is the read-only-sync counterpart of GetOrCreateUser: use it for
// background reconciliation (e.g. user-search backfill from Casdoor) where the
// caller is not the authenticated user themselves and therefore must not trigger
// login-only side effects such as bootstrap platform-admin granting.
//
// GetOrCreateUser (which DOES fire the hook) must remain reserved for genuine
// login paths (OAuth callback + JWKS auth), where the bearer has proven they own
// the identity. Granting roles during a third party's search of a user would
// violate the "granted on the user's own verified login" invariant.
func (s *UserService) SyncUser(ctx context.Context, claims *JWTClaims) (*models.User, error) {
	if s.writeMode == WriteModeReadonly {
		return nil, ErrWriteBlocked
	}
	_ = ctx // local writer ignores ctx — kept for interface compat (B3b.2b)
	return s.getOrCreateUser(claims)
}

// RoleGranter is the minimal surface the bootstrap granter needs from the
// systemrole service. Declaring it here (instead of importing systemrole) keeps
// the user package free of a systemrole dependency and avoids an import cycle —
// the same "inject a hook from main.go" pattern used by middleware's
// SetStatusChecker / SetSubjectResolver. GrantRole must be idempotent (it is:
// systemrole.SystemRoleService.GrantRole skips when the role already exists and
// the unique index backstops races).
type RoleGranter interface {
	GrantRole(userID, role, operatorID string) error
}

// bootstrapGrantedBy marks role grants created by the bootstrap mechanism so
// they are distinguishable from manual grants ('system' / an operator subject
// id) in user_system_roles.granted_by.
const bootstrapGrantedBy = "bootstrap"

// platformAdminRole mirrors systemrole.SystemRolePlatformAdmin without importing
// the systemrole package (cycle avoidance). Kept in sync by the build / tests.
const platformAdminRole = "platform_admin"

// BootstrapAdminGranter grants the platform_admin role on login to any user
// whose Casdoor universal_id is in the configured allowlist. It is wired as a
// post-login hook from main.go so the user package never imports systemrole.
//
// universal_id (not email) is the identity anchor: Casdoor issues a stable,
// globally-unique universal_id for every identity, whereas email can be empty
// (GitHub / phone logins) and is therefore unreliable for "this specific person".
//
// Behaviour:
//   - The allowlist is matched exactly (universal_id is case-sensitive; entries
//     are only whitespace-trimmed, never lowercased).
//   - An empty allowlist is a complete no-op (zero behaviour change).
//   - Granting is best-effort: failures are logged, never returned, so a grant
//     error can never block login.
//   - GrantRole is idempotent, so this can safely run on every login (config is
//     the source of truth: adding a universal_id later promotes that user on
//     their next login).
type BootstrapAdminGranter struct {
	granter RoleGranter
	// universalIDs is the set of allowlisted Casdoor universal_id values.
	universalIDs map[string]struct{}
}

// NewBootstrapAdminGranter builds a granter from the configured universal_id
// allowlist. Returns nil-safe behaviour: a granter with an empty allowlist whose
// ApplyOnLogin is a no-op. A nil RoleGranter is tolerated (ApplyOnLogin becomes a
// no-op). Entries are whitespace-trimmed (NOT lowercased — universal_id is
// case-sensitive); blank entries are skipped.
func NewBootstrapAdminGranter(granter RoleGranter, universalIDs []string) *BootstrapAdminGranter {
	set := make(map[string]struct{}, len(universalIDs))
	for _, id := range universalIDs {
		normalized := strings.TrimSpace(id)
		if normalized == "" {
			continue
		}
		set[normalized] = struct{}{}
	}
	return &BootstrapAdminGranter{granter: granter, universalIDs: set}
}

// ApplyOnLogin grants platform_admin to the given user when their Casdoor
// universal_id is in the allowlist and they do not already hold the role. It is
// safe to call on every login. Errors are swallowed (logged) so login is never
// blocked. nil-safe: a nil receiver, nil granter, empty allowlist, nil user, or
// nil/empty universal_id all short-circuit to a no-op.
func (b *BootstrapAdminGranter) ApplyOnLogin(u *models.User) {
	if b == nil || b.granter == nil || len(b.universalIDs) == 0 || u == nil {
		return
	}
	if u.CasdoorUniversalID == nil {
		return
	}
	universalID := strings.TrimSpace(*u.CasdoorUniversalID)
	if universalID == "" {
		return
	}
	if _, ok := b.universalIDs[universalID]; !ok {
		return
	}
	if u.SubjectID == "" {
		return
	}

	// GrantRole is idempotent: it returns nil without inserting when the role is
	// already present, and the unique index on (user_id, role) backstops races.
	if err := b.granter.GrantRole(u.SubjectID, platformAdminRole, bootstrapGrantedBy); err != nil {
		fmt.Printf("[WARN] bootstrap platform_admin grant failed for %s (universal_id=%s): %v\n", u.SubjectID, universalID, err)
	}
}
