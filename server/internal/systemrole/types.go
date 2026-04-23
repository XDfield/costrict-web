package systemrole

const (
	SystemRolePlatformAdmin = "platform_admin"
	SystemRoleBusinessAdmin = "business_admin"
)

const (
	CanManageSystemRoles      = "CanManageSystemRoles"
	CanManageSystemSettings   = "CanManageSystemSettings"
	CanManageSystemChannels   = "CanManageSystemChannels"
	CanViewGlobalBusinessData = "CanViewGlobalBusinessData"
	CanAccessBusinessDashboard = "CanAccessBusinessDashboard"
)

func IsValidRole(role string) bool {
	switch role {
	case SystemRolePlatformAdmin, SystemRoleBusinessAdmin:
		return true
	default:
		return false
	}
}

func ExpandRoles(roles []string) []string {
	set := make(map[string]struct{}, len(roles)+1)
	for _, role := range roles {
		if role == "" {
			continue
		}
		set[role] = struct{}{}
		if role == SystemRolePlatformAdmin {
			set[SystemRoleBusinessAdmin] = struct{}{}
		}
	}
	result := make([]string, 0, len(set))
	for _, role := range []string{SystemRolePlatformAdmin, SystemRoleBusinessAdmin} {
		if _, ok := set[role]; ok {
			result = append(result, role)
		}
	}
	return result
}

// CapabilityProvider wraps the package-level CapabilitiesForRoles for injection.
type CapabilityProvider struct{}

func (CapabilityProvider) CapabilitiesForRoles(roles []string) []string {
	return CapabilitiesForRoles(roles)
}

func CapabilitiesForRoles(roles []string) []string {
	roleSet := make(map[string]struct{}, len(roles))
	for _, role := range ExpandRoles(roles) {
		roleSet[role] = struct{}{}
	}

	capSet := map[string]struct{}{}
	if _, ok := roleSet[SystemRolePlatformAdmin]; ok {
		capSet[CanManageSystemRoles] = struct{}{}
		capSet[CanManageSystemSettings] = struct{}{}
		capSet[CanManageSystemChannels] = struct{}{}
		capSet[CanViewGlobalBusinessData] = struct{}{}
		capSet[CanAccessBusinessDashboard] = struct{}{}
	}
	if _, ok := roleSet[SystemRoleBusinessAdmin]; ok {
		capSet[CanViewGlobalBusinessData] = struct{}{}
		capSet[CanAccessBusinessDashboard] = struct{}{}
	}

	ordered := []string{
		CanManageSystemRoles,
		CanManageSystemSettings,
		CanManageSystemChannels,
		CanViewGlobalBusinessData,
		CanAccessBusinessDashboard,
	}
	result := make([]string, 0, len(capSet))
	for _, capability := range ordered {
		if _, ok := capSet[capability]; ok {
			result = append(result, capability)
		}
	}
	return result
}
