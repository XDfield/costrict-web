package authz

// ResourceRegistry maps resource codes to the list of system roles that are allowed.
// An empty role list means any authenticated user can access the resource.
type ResourceRegistry map[string][]string

const (
	RolePlatformAdmin = "platform_admin"
	RoleBusinessAdmin = "business_admin"
)

var (
	// MenuResources defines console navigation menu permissions.
	MenuResources = ResourceRegistry{
		"console.repositories":    {},                         // all authenticated users
		"console.projects":        {},                         // all authenticated users
		"console.capabilities":    {RolePlatformAdmin},
		"console.devices":         {},                         // all authenticated users
		"console.notifications":   {RolePlatformAdmin},
		"console.kanban":          {RoleBusinessAdmin, RolePlatformAdmin},
	}

	// APIResources defines API endpoint permissions.
	APIResources = ResourceRegistry{
		"admin.system-roles":          {RolePlatformAdmin},
		"admin.notification-channels": {RolePlatformAdmin},
		"api.kanban.overview":         {RoleBusinessAdmin, RolePlatformAdmin},
	}
)

// IsOpenToAll returns true when the allowed role list is empty (any authenticated user).
func (r ResourceRegistry) IsOpenToAll(code string) bool {
	roles, ok := r[code]
	return ok && len(roles) == 0
}

// AllowedRoles returns the roles allowed for a given resource code.
func (r ResourceRegistry) AllowedRoles(code string) ([]string, bool) {
	roles, ok := r[code]
	return roles, ok
}
