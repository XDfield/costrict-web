package user

import (
	"fmt"
	"strings"

	"github.com/costrict/costrict-web/server/internal/models"
)

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
// whose email is in the configured allowlist. It is wired as a post-login hook
// from main.go so the user package never imports systemrole.
//
// Behaviour:
//   - The allowlist is matched case-insensitively (emails are lowercased here;
//     config also lowercases them, so this is belt-and-suspenders).
//   - An empty allowlist is a complete no-op (zero behaviour change).
//   - Granting is best-effort: failures are logged, never returned, so a grant
//     error can never block login.
//   - GrantRole is idempotent, so this can safely run on every login (config is
//     the source of truth: adding an email later promotes that user on their
//     next login).
type BootstrapAdminGranter struct {
	granter RoleGranter
	// emails is the set of lowercased allowlisted emails.
	emails map[string]struct{}
}

// NewBootstrapAdminGranter builds a granter from the configured emails. Returns
// nil-safe behaviour: a granter with an empty allowlist whose ApplyOnLogin is a
// no-op. A nil RoleGranter is tolerated (ApplyOnLogin becomes a no-op).
func NewBootstrapAdminGranter(granter RoleGranter, emails []string) *BootstrapAdminGranter {
	set := make(map[string]struct{}, len(emails))
	for _, e := range emails {
		normalized := strings.ToLower(strings.TrimSpace(e))
		if normalized == "" {
			continue
		}
		set[normalized] = struct{}{}
	}
	return &BootstrapAdminGranter{granter: granter, emails: set}
}

// ApplyOnLogin grants platform_admin to the given user when their email is in
// the allowlist and they do not already hold the role. It is safe to call on
// every login. Errors are swallowed (logged) so login is never blocked.
func (b *BootstrapAdminGranter) ApplyOnLogin(u *models.User) {
	if b == nil || b.granter == nil || len(b.emails) == 0 || u == nil {
		return
	}
	if u.Email == nil {
		return
	}
	email := strings.ToLower(strings.TrimSpace(*u.Email))
	if email == "" {
		return
	}
	if _, ok := b.emails[email]; !ok {
		return
	}
	if u.SubjectID == "" {
		return
	}

	// GrantRole is idempotent: it returns nil without inserting when the role is
	// already present, and the unique index on (user_id, role) backstops races.
	if err := b.granter.GrantRole(u.SubjectID, platformAdminRole, bootstrapGrantedBy); err != nil {
		fmt.Printf("[WARN] bootstrap platform_admin grant failed for %s (%s): %v\n", u.SubjectID, email, err)
	}
}
