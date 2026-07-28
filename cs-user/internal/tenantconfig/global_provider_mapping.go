// global_provider_mapping.go — Default provider_mapping for all tenants.
//
// E1 implements global provider_mapping as a code-defined default that gets
// merged with tenant-specific overrides from tenant_configs. This matches
// MULTI_TENANCY_DESIGN.md §17.2:
//   - global default is loaded from code (this file)
//   - tenant config is loaded from tenant_configs.provider_mapping subsection
//   - LoadProviderMapping performs deep merge (tenant overrides global)
//
// Future evolution:
//   - Platform admin may manage global defaults via a special tenant_configs
//     row (tenant_id='global') instead of code; this only changes the
//     loadGlobalProviderMapping implementation, not the merge contract.
//   - Provider names defined here are referenced by employment_providers
//     (claim-mapping rules) and Casdoor JWT Plan B detection.

package tenantconfig

// GlobalProviderMapping returns the default provider_mapping applied to all
// tenants before tenant-specific overrides are merged. This is the code-defined
// baseline from MULTI_TENANCY_DESIGN.md §17.2.
//
// Current version includes common providers with conservative defaults:
//   - rank: tiebreak ordering for primary provider selection
//   - field_map: claim → system attribute mappings
//   - enterprise_sync: sync cadence (when used as employment provider)
//
// This baseline can be overridden per-tenant via the PUT /api/tenant/provider-mapping
// endpoint (C3.3). Overriding a provider entirely replaces its config; omitting
// a provider keeps the global default.
func GlobalProviderMapping() *ProviderMapping {
	return &ProviderMapping{
		Version: CurrentSupportedVersion,
		Providers: map[string]Provider{
			// GitHub (social login, not employment by default)
			"github": {
				Enabled: boolPtr(true),
				Rank:    intPtr(100),
				FieldMap: map[string]string{
					"login":      "username",
					"name":       "display_name",
					"email":      "email",
					"avatar_url": "picture",
				},
			},

			// Google (social login)
			"google": {
				Enabled: boolPtr(true),
				Rank:    intPtr(100),
				FieldMap: map[string]string{
					"email":         "email",
					"given_name":    "first_name",
					"family_name":   "last_name",
					"picture":       "picture",
					"email_verified": "email_verified",
				},
			},

			// Password (built-in, lowest rank)
			"password": {
				Enabled: boolPtr(true),
				Rank:    intPtr(0),
			},

			// Enterprise IdPs (defaults, typically overridden per-tenant)
			// These are common SaaS IdP names; tenant_configs can add more.
			"idtrust": {
				Enabled: boolPtr(true),
				Rank:    intPtr(300),
				EnterpriseSync: &EnterpriseSync{
					Interval: "24h",
				},
				FieldMap: map[string]string{
					"employee_number": "employee_number",
					"cost_center":     "cost_center",
					"department":      "department",
					"title":           "job_title",
					"manager":         "manager",
				},
			},

			"azure_ad": {
				Enabled: boolPtr(true),
				Rank:    intPtr(300),
				EnterpriseSync: &EnterpriseSync{
					Interval: "24h",
				},
				FieldMap: map[string]string{
					"employeeId":   "employee_number",
					"department":   "department",
					"jobTitle":     "job_title",
					"mail":         "email",
					"userPrincipalName": "username",
				},
			},

			"ldap": {
				Enabled: boolPtr(false), // default off due to configuration variability
				Rank:    intPtr(300),
				EnterpriseSync: &EnterpriseSync{
					Interval: "24h",
				},
				FieldMap: map[string]string{
					"uid":           "username",
					"cn":            "display_name",
					"mail":          "email",
					"employeeNumber": "employee_number",
					"departmentNumber": "department",
				},
			},

			// Chinese enterprise IdPs (common in China market)
			"feishu": {
				Enabled: boolPtr(false),
				Rank:    intPtr(300),
				EnterpriseSync: &EnterpriseSync{
					Interval: "24h",
				},
				FieldMap: map[string]string{
					"user_id":     "provider_user_id",
					"name":        "display_name",
					"email":       "email",
					"mobile":      "phone",
					"employee_no": "employee_number",
					"department":  "department",
				},
			},

			"dingtalk": {
				Enabled: boolPtr(false),
				Rank:    intPtr(300),
				EnterpriseSync: &EnterpriseSync{
					Interval: "24h",
				},
				FieldMap: map[string]string{
					"userid":      "provider_user_id",
					"nickname":    "display_name",
					"email":       "email",
					"mobile":      "phone",
					"job_number":  "employee_number",
					"dept_name":   "department",
				},
			},

			"wecom": {
				Enabled: boolPtr(false),
				Rank:    intPtr(300),
				EnterpriseSync: &EnterpriseSync{
					Interval: "24h",
				},
				FieldMap: map[string]string{
					"userid":      "provider_user_id",
					"name":        "display_name",
					"email":       "email",
					"mobile":      "phone",
					"gender":      "gender",
					"department":  "department",
				},
			},
		},
	}
}

// boolPtr returns a pointer to a bool. Used for constructing Enabled pointers.
func boolPtr(b bool) *bool {
	return &b
}

// intPtr returns a pointer to an int. Used for constructing Rank pointers.
func intPtr(i int) *int {
	return &i
}
