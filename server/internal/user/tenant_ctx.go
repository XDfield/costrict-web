// Package user — tenant slug forwarding helper for RPC outbound calls.
//
// Small wrapper around tenant.SlugFromContext kept local to the user package
// so rpc_client.go / rpc_writer.go share one import + one lookup site. If
// the slug is empty (no signal), RPC calls omit X-Tenant-Id and cs-user
// falls back to its default tenant.
package user

import (
	"context"
	"net/http"

	"github.com/costrict/costrict-web/server/internal/tenant"
)

func tenantSlugFromContext(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	return tenant.SlugFromContext(ctx)
}

// ActorRoleHeader / ActorPlatformScopeHeader are the header names cs-user's
// handlers/audit.go reads to populate audit-log actor_tenant_role /
// actor_platform_scope columns (Phase C4.1). Mirror of the constants in
// cs-user/internal/handlers/audit.go — kept duplicated rather than shared
// because cs-user and server are separate Go modules.
const (
	ActorRoleHeader          = "X-Actor-Tenant-Role"
	ActorPlatformScopeHeader = "X-Actor-Platform-Scope"
)

// applyActorMetaHeaders forwards the JWT-derived actor role + platform scope
// (Phase C4.1) from the request ctx to the outbound cs-user request as
// X-Actor-Tenant-Role / X-Actor-Platform-Scope headers. Empty fields are
// omitted so cs-user writes NULL columns.
//
// Centralised here so the 3 RPC client call sites (platform_tenant /
// tenant_config / tenant_provider_mapping) share one implementation. req
// must be non-nil — callers build it before invoking.
func applyActorMetaHeaders(req *http.Request, ctx context.Context) {
	if req == nil || ctx == nil {
		return
	}
	m := tenant.ActorMetaFromContext(ctx)
	if m.Role != "" {
		req.Header.Set(ActorRoleHeader, m.Role)
	}
	if m.Scope != "" {
		req.Header.Set(ActorPlatformScopeHeader, m.Scope)
	}
}
