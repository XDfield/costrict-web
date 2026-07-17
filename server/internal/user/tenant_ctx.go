// Package user — tenant slug forwarding helper for RPC outbound calls.
//
// Small wrapper around tenant.SlugFromContext kept local to the user package
// so rpc_client.go / rpc_writer.go share one import + one lookup site. If
// the slug is empty (no signal), RPC calls omit X-Tenant-Id and cs-user
// falls back to its default tenant.
package user

import (
	"context"

	"github.com/costrict/costrict-web/server/internal/tenant"
)

func tenantSlugFromContext(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	return tenant.SlugFromContext(ctx)
}
