// Package gitsync git-server abstraction — Phase 2.1.
//
// GitServer is the backend git-platform protocol consumed by tenantns
// orchestration (team-namespace + workflow repo provisioning). It abstracts
// away Gitea-specifics so a second backend (GitLab / GLBit / Gitea fork
// with CoStrict JWT) can be added behind the same surface via a Kind-aware
// factory.
//
// Layering:
//
//   - gitsync.Client (existing) — concrete Gitea HTTP impl; satisfies
//     GitServer once the Phase 2.2 repo methods land.
//
//   - DefaultGitServerFactory (this file) — Kind dispatcher. Reads
//     GitServerConfig.Kind (populated by cs-user's git_servers table via
//     the RPC resolver) and returns the matching impl.
//
//   - gitsync.Service (existing) — tenant-aware orchestration; gains a
//     gitServerFactory field in Phase 2.2 so workflow_repo.go can request
//     a backend without knowing the Kind.
//
// Why compose rather than re-declare? GiteaTeamMemberAPI and BotAccountAPI
// already exist as narrow interfaces used by SyncTeam / ProvisionBot
// respectively. Embedding them here means narrow tests keep their small
// stub surface, while consumers needing the full surface take GitServer.

package gitsync

import "context"

// GitServerKind constants mirror cs-user's models.GitServerKind* enum.
// Server reads Kind from the RPC response (GitServerConfig.Kind) and never
// parses the cs-user enum directly, so these strings are the contract.
const (
	GitServerKindGitea = "gitea"
)

// Repo is the minimal slice of Gitea's repository payload consumed by the
// workflow_repo provisioning path. ID is the numeric primary key used in
// subsequent API calls; FullName is "owner/name" for diagnostics.
type Repo struct {
	ID       int64  `json:"id"`
	Name     string `json:"name"`
	FullName string `json:"full_name"`
	Private  bool   `json:"private"`
}

// CreateRepoOptions is the input shape for CreateRepo. Mirrors Gitea's
// CreateRepoOption (modules/structs/repo.go) but only the fields we set.
//
//   - Name: required, repo basename (e.g. "wf-my-workflow")
//   - Private: true for all team-namespace repos
//   - AutoInit: true so default_branch exists immediately (needed before
//     we can write files / create branches off it)
//   - DefaultBranch: "main" by convention; empty falls back to Gitea default
type CreateRepoOptions struct {
	Name          string `json:"name"`
	Description   string `json:"description,omitempty"`
	Private       bool   `json:"private"`
	AutoInit      bool   `json:"auto_init"`
	DefaultBranch string `json:"default_branch,omitempty"`
}

// Branch is the minimal payload returned by GetBranch. CommitSHA is the
// HEAD commit of the branch — used to detect drift between two calls.
type Branch struct {
	Name      string `json:"name"`
	CommitSHA string `json:"commit_sha"`
}

// BranchProtectionOptions mirrors Gitea's CreateBranchProtectionOption
// (modules/structs/repo_branch.go). Only the fields team-namespace policy
// needs are exposed; defaults of Gitea's other fields are accepted.
//
// RuleName supports glob (e.g. "inst-*"); Gitea's Match() uses gitea.dev/
// modules/glob to evaluate. EnablePush=false blocks all direct pushes
// (bot uses PR flow). EnableForcePush=false is belt-and-suspenders.
type BranchProtectionOptions struct {
	RuleName           string   `json:"rule_name"`
	EnablePush         bool     `json:"enable_push"`
	EnableForcePush    bool     `json:"enable_force_push"`
	RequiredApprovals  int64    `json:"required_approvals,omitempty"`
	WhitelistUsernames []string `json:"push_whitelist_usernames,omitempty"`
}

// GitServer is the full backend surface. One instance corresponds to a
// tenant-resolved (endpoint, admin_token) pair; methods are NOT tenant-
// scoped — the caller resolves the tenant and instantiates the right
// backend via the factory.
//
// Idempotency contract:
//
//   - RemoveOrgMember / DeleteUserToken: HTTP 404 is absorbed (already
//     gone is success).
//   - CreateRepo / CreateBranch: caller checks existence first via GetRepo
//     / GetBranch; implementations MAY also tolerate 409 but callers
//     should not rely on it.
//   - SetBranchProtection: idempotent overwrite is implementation-defined;
//     callers may invoke once per repo boot.
type GitServer interface {
	// ---- Team member surface (existing) ----
	GiteaTeamMemberAPI

	// ---- Bot account surface (existing) ----
	BotAccountAPI

	// ---- Org-level ops (currently loose on *Client) ----
	UpdateOrg(ctx context.Context, org string, opts UpdateOrgOptions) error
	ListOrgMembers(ctx context.Context, org string) ([]string, error)
	AddOrgMember(ctx context.Context, org, username string) error
	RemoveOrgMember(ctx context.Context, org, username string) error

	// ---- Repo / branch / file (P0 #2/#3/#4) ----
	CreateRepo(ctx context.Context, owner string, opts CreateRepoOptions) (*Repo, error)
	GetRepo(ctx context.Context, owner, name string) (*Repo, error)
	CreateBranch(ctx context.Context, owner, repo, newBranch, fromRef string) error
	GetBranch(ctx context.Context, owner, repo, branch string) (*Branch, error)
	SetBranchProtection(ctx context.Context, owner, repo string, opts BranchProtectionOptions) error
	WriteFile(ctx context.Context, owner, repo, branch, path string, content []byte, message string) error
	ReadFile(ctx context.Context, owner, repo, branch, path string) ([]byte, error)
}

// DefaultGitServerFactory dispatches on cfg.Kind and returns the matching
// backend impl. Unknown / empty Kind falls back to Gitea for backward
// compatibility with pre-Kind-configured tenants (their git_servers row
// predates the Kind column, or RPC did not populate it).
//
// Returns nil if baseURL or adminToken is empty — the caller (Service)
// treats nil as ErrTenantGitServerUnresolved at use site.
func DefaultGitServerFactory(cfg GitServerConfig) GitServer {
	switch cfg.Kind {
	case GitServerKindGitea, "": // "" = backward compat
		// NewClient returns nil for empty baseURL/token. Unwrap so the
		// returned interface is truly nil — returning the nil *Client
		// directly would wrap it in a non-nil interface (Go gotcha) and
		// callers checking `if gs == nil` would miss the config error.
		c := NewClient(cfg.Endpoint, cfg.AdminToken)
		if c == nil {
			return nil
		}
		return c
	default:
		// Unknown Kind: no backend available. Returning nil lets the
		// caller surface a configuration error rather than panic; the
		// Service layer logs and returns ErrTenantGitServerUnresolved.
		return nil
	}
}

// Compile-time assertion that *Client satisfies GitServer. Lives at the
// bottom so all method definitions (across client.go / client_extensions.go
// / repo_ops.go) are in scope when the type-checker evaluates it.
var _ GitServer = (*Client)(nil)

// GitServerFor resolves the tenant's git backend and returns it as the
// platform-agnostic GitServer interface. Consumers (teamns workflow repo
// provisioning) depend on the interface, not the concrete *Client, so
// future GitLab / GLBit impls slot in transparently.
//
// Returns ErrGiteaUnreachable when the tenant has no resolvable git server
// or the resolved Kind has no registered backend (caller maps to 503).
func (s *Service) GitServerFor(ctx context.Context, tenantID string) (GitServer, error) {
	if s == nil {
		return nil, ErrGiteaUnreachable
	}
	_, cli, err := s.botClientFor(ctx, tenantID)
	if err != nil {
		return nil, err
	}
	return cli, nil
}
