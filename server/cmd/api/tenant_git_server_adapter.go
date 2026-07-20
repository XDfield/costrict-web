// tenant_git_server_adapter.go — Phase E3b.1.1 wiring shim.
//
// Adapts *user.RPCClient (which returns *user.TenantGitServerConfig) to
// gitsync.GitServerClient (which expects *gitsync.GitServerConfig). The
// two types are field-for-field identical but live in different packages
// to avoid an import cycle: gitsync must not depend on user. main.go is
// the natural place for the type translation.

package main

import (
	"context"

	"github.com/costrict/costrict-web/server/internal/gitsync"
	userpkg "github.com/costrict/costrict-web/server/internal/user"
)

// tenantGitServerAdapter wraps an *userpkg.RPCClient and re-types its
// TenantGitServerConfig into gitsync.GitServerConfig.
type tenantGitServerAdapter struct {
	rpc *userpkg.RPCClient
}

// GetTenantGitServer implements gitsync.GitServerClient.
func (a *tenantGitServerAdapter) GetTenantGitServer(ctx context.Context, tenantID string) (*gitsync.GitServerConfig, error) {
	cfg, err := a.rpc.GetTenantGitServer(ctx, tenantID)
	if err != nil {
		return nil, err
	}
	if cfg == nil {
		return nil, nil
	}
	return &gitsync.GitServerConfig{
		ServerID:   cfg.ServerID,
		Kind:       cfg.Kind,
		Endpoint:   cfg.Endpoint,
		AdminToken: cfg.AdminToken,
	}, nil
}
