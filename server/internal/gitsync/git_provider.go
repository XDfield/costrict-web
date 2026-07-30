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
//     with an existing account (HTTP 409), or ErrGiteaValidationFailed
//     when Gitea rejects the payload on non-name grounds (HTTP 422 —
//     email collision, password policy, ...). ProvisionUser routes these
//     differently: 409 → GetUserByName recovery; 422 → straight to
//     markError (no recovery possible since no user was created).
//   - GetUserByName returns the same provider-side not-found error
//     *Client already uses (ErrGiteaTeamNotFound — kept under that name
//     for now since team / bot paths share it).
type GitProvider interface {
	CreateUser(ctx context.Context, opts CreateUserOptions) (*ProviderUser, error)
	GetUserByName(ctx context.Context, username string) (*ProviderUser, error)
	// EditUser patches an existing user. Only fields whose pointer is non-nil
	// are written. Returns ErrGiteaTeamNotFound on HTTP 404 (consistent with
	// GetUserByName). Used by display_name reconciliation during backfill.
	EditUser(ctx context.Context, username string, opts EditUserOptions) (*ProviderUser, error)
}

// EditUserOptions is the PATCH /admin/users/{username} body. Pointer fields
// mean "absent from request" (no change) vs zero value. Today only FullName
// is exercised — display_name sync from cs-user → Gitea.
type EditUserOptions struct {
	FullName *string `json:"full_name,omitempty"`
	Email    *string `json:"email,omitempty"`
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
