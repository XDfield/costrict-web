// Package gitsync resolver type definitions.
//
// Historically this file hosted RPCResolver, the @server-side counterpart
// of cs-user's gitserver.Resolver. The Git Ownership Refactor (Phase 4)
// made @server the canonical owner of git_servers data; RPC back to cs-user
// was removed along with cs-user's gitserver package. LocalResolver (see
// local_resolver.go) is the only production implementation now.
//
// What remains here are the types both LocalResolver and the gitsync
// Service / UserProvisionService consume.

package gitsync

import "context"

// GitServerConfig is the per-tenant Git server configuration the gitsync
// Service uses to construct a Client. Lives in gitsync to avoid an
// import cycle (user doesn't depend on gitsync; gitsync mustn't depend
// on user).
type GitServerConfig struct {
	ServerID   string
	Kind       string
	Endpoint   string
	AdminToken string
	// AdminUser / AdminPassword are OPTIONAL credentials used for Gitea
	// endpoints that reject admin PAT auth. Specifically, upstream Gitea's
	// POST /users/{name}/tokens is locked behind reqBasicOrRevProxyAuth,
	// which 401s any "Authorization: token ..." request. When these are
	// empty, the Client falls back to token auth (sufficient for every
	// other endpoint, including bot user creation under /admin/users).
	AdminUser     string
	AdminPassword string
}

// GitServerResolver is the abstract surface the Service consumes.
// LocalResolver is the production impl; tests inject a stub.
type GitServerResolver interface {
	Resolve(ctx context.Context, tenantID string) (*GitServerConfig, error)
}
