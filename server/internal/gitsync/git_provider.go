// Package gitsync — provider-agnostic Git user provisioning surface.
//
// This file declares the GitProvider interface — the swap point for future
// git server backends (gitea today; gitlab, gitea-enterprise, ... future).
// Each provider implements this interface and is registered in the
// provider factory keyed on GitServerConfig.Kind.
//
// Today *Client (the Gitea HTTP client) satisfies GitProvider via its
// CreateUser / GetUserByName methods declared in client_extensions.go.
// Future gitlab / enterprise providers will live as sibling files
// (e.g. gitlab_provider.go) and the factory dispatches by Kind.

package gitsync

import "context"

// GitProvider is the per-user provisioning surface that UserProvisionService
// depends on. Implementations are constructed by a factory keyed on
// GitServerConfig.Kind.
//
// Method semantics mirror the existing *Client surface:
//
//   - CreateUser returns ErrUsernameTaken when the chosen login collides
//     with an existing account (HTTP 409 / 422 from Gitea today).
//   - GetUserByName returns the same provider-side not-found error
//     *Client already uses (ErrGiteaTeamNotFound — kept under that name
//     for now since team / bot paths share it).
type GitProvider interface {
	CreateUser(ctx context.Context, opts CreateUserOptions) (*ProviderUser, error)
	GetUserByName(ctx context.Context, username string) (*ProviderUser, error)
}

// ProviderUser is the provider-agnostic slice of a remote user record.
// Provider implementations map their native response into this shape so
// UserProvisionService never sees Gitea-specific / GitLab-specific fields.
//
// Source carries the provider kind string ("gitea", "gitlab", ...) for
// audit / debugging — it is informational, not load-bearing.
type ProviderUser struct {
	ID       int64  `json:"id"`
	Login    string `json:"login"`
	Email    string `json:"email"`
	FullName string `json:"full_name"`
	// Source identifies which provider populated this row; providers set
	// it to their kind constant (e.g. GitServerKindGitea).
	Source string `json:"source,omitempty"`
}
