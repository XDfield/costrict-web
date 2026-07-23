// LocalResolver adapter — the only production GitServerResolver.
//
// Translates a gitserver.Resolver (which returns *gitserver.Config) into
// gitsync.GitServerConfig. The Git Ownership Refactor (Phase 4) removed
// the legacy RPCResolver that used to call back into cs-user; @server is
// now the canonical owner of git_servers data.

package gitsync

import (
	"context"
	"errors"

	"github.com/costrict/costrict-web/server/internal/gitserver"
)

// LocalResolver adapts a gitserver.Resolver (P1, returns *gitserver.Config)
// to gitsync.GitServerResolver (returns *GitServerConfig). The two types
// carry the same information under different names — this adapter is the
// single place that translates.
type LocalResolver struct {
	upstream gitserver.Resolver
}

// NewLocalResolver wraps a gitserver.Resolver. upstream MUST be non-nil —
// callers (main.go) supply *gitserver.DBResolver.
func NewLocalResolver(upstream gitserver.Resolver) *LocalResolver {
	return &LocalResolver{upstream: upstream}
}

// Resolve translates gitserver.Config → gitsync.GitServerConfig.
//
// Sentinel errors are passed through unchanged so callers comparing with
// gitserver.ErrTenantMissingGitServer / ErrGitServerDisabled still work.
func (r *LocalResolver) Resolve(ctx context.Context, tenantID string) (*GitServerConfig, error) {
	if r == nil || r.upstream == nil {
		return nil, errors.New("gitsync: nil LocalResolver or upstream")
	}
	cfg, err := r.upstream.Resolve(ctx, tenantID)
	if err != nil {
		return nil, err
	}
	if cfg == nil {
		return nil, errors.New("gitsync: upstream returned nil config without error")
	}
	return &GitServerConfig{
		ServerID:      cfg.ServerID,
		Kind:          cfg.Kind,
		Endpoint:      cfg.Endpoint,
		AdminToken:    cfg.AdminToken,
		AdminUser:     cfg.AdminUser,
		AdminPassword: cfg.AdminPassword,
	}, nil
}
