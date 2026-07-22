// Package gitsync — bot account provisioning client surface (Phase E3c).
//
// This file extends Client with the user / org / token CRUD ops ProvisionBot
// / RevokeBot / RotateBot need. Lives in a sibling file (not appended to
// client.go) so the team-member slice can be reviewed / shipped independently.
//
// Gitea API reference (v1): https://docs.gitea.com/api/ — endpoints used:
//
//   POST   /api/v1/admin/users                — CreateUser
//   GET    /api/v1/users/{username}           — GetUserByName
//   POST   /api/v1/users/{username}/tokens    — CreateUserToken
//   DELETE /api/v1/users/{username}/tokens/{id} — DeleteUserToken
//   POST   /api/v1/orgs                       — CreateOrg
//   GET    /api/v1/orgs/{org}                 — GetOrgByName
//   GET    /api/v1/orgs/{org}/teams           — ListOrgTeams
//   PUT    /api/v1/teams/{id}/members/{user}  — AddTeamMember (already in client.go)
//   DELETE /api/v1/teams/{id}/members/{user}  — RemoveTeamMember (already in client.go)

package gitsync

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
)

// Additional sentinel errors surfaced by the bot-provisioning path. The
// Service layer switches on these to decide retry vs operator-intervention.
var (
	// ErrGiteaUsernameTaken — HTTP 409 / 422 from POST /admin/users or POST
	// /orgs. Username / org name is globally unique inside one Gitea instance.
	// Caller can't retry; needs operator rename or out-of-band cleanup.
	ErrGiteaUsernameTaken = errors.New("gitsync: gitea username/org name already taken")

	// ErrGiteaNotFound (alias of ErrGiteaTeamNotFound) — HTTP 404 on user / org
	// / token lookups. Already declared in client.go as ErrGiteaTeamNotFound;
	// we re-use it but expose a generic alias for the bot-provisioning path
	// to keep call sites readable.
)

// GiteaUser is the minimal slice of Gitea's user payload ProvisionBot
// consumes. Only ID + Login are load-bearing (Login echoes the input,
// ID is the stable handle persisted to team_bot_credentials.gitea_user_id).
type GiteaUser struct {
	ID       int64  `json:"id"`
	Login    string `json:"login"`
	Email    string `json:"email"`
	FullName string `json:"full_name"`
}

// CreateUserOptions is the body shape POST /admin/users expects. Email is
// synthetic — bot+<short>@costrict.internal — to avoid clashing with any
// real employee email. Password is a random 32-byte string the bot never
// sees (login is via PAT only); we generate it locally so the account
// isn't effectively passwordless.
type CreateUserOptions struct {
	Login    string `json:"username"`
	Email    string `json:"email"`
	FullName string `json:"full_name"`
	Password string `json:"password"`
	// SourceID 0 = local account. Gitea also supports external auth sources
	// (LDAP/OAuth); bot accounts always use local.
	SourceID int64 `json:"source_id"`
	// SendNotify false — bot has no inbox.
	SendNotify bool `json:"send_notify"`
	// MustChangePassword false — bot never logs in via password; must be
	// explicitly set because Gitea's admin-create-user default is true,
	// which would 403 subsequent PAT ops until a password change happens.
	MustChangePassword bool `json:"must_change_password"`
}

// GiteaToken is the response from POST /users/{name}/tokens. TokenPlaintext
// is the clear-text PAT — Gitea returns it exactly once, we return it to
// the caller, and we never persist it (only the SHA256 fingerprint).
type GiteaToken struct {
	ID             int64  `json:"id"`
	Name           string `json:"name"`
	TokenPlaintext string `json:"sha1"` // Gitea field is named sha1 but holds the clear-text token at creation time
	// TokenSHA1 — populated by Gitea on subsequent GETs with the SHA1 of the
	// token. Not load-bearing for our flow (we compute our own SHA256).
}

// CreateUserTokenOptions is the body for POST /users/{name}/tokens. Scopes
// is the list of permission scopes the new PAT grants. For team bot we
// use ["write:repository", "read:user"].
type CreateUserTokenOptions struct {
	Name   string   `json:"name"`
	Scopes []string `json:"scopes"`
}

// GiteaOrg is the minimal slice of Gitea's org payload. Only Name + ID are
// load-bearing; Username field mirrors Name for orgs.
type GiteaOrg struct {
	ID       int64  `json:"id"`
	Name     string `json:"name"`
	Username string `json:"username"`
	FullName string `json:"full_name"`
}

// CreateOrgOptions is the body for POST /orgs. Names must match [a-zA-Z0-9-_]+;
// we use t-<team_short> to keep namespaces distinct from human orgs.
type CreateOrgOptions struct {
	Username    string `json:"username"`
	FullName    string `json:"full_name"`
	Description string `json:"description"`
	// Visibility "private" — keeps the team ns invisible to non-members on
	// the Gitea instance.
	Visibility string `json:"visibility"`
}

// GiteaTeam is the minimal team payload. ListOrgTeams returns []GiteaTeam
// so we can locate the auto-created "Owners" team and add the bot to it.
type GiteaTeam struct {
	ID         int64  `json:"id"`
	Name       string `json:"name"`
	Org        string `json:"organization"`
	Permission string `json:"permission"`
}

// BotAccountAPI is the user / org / token CRUD surface ProvisionBot and
// friends consume. Declared as an interface so the Service stays testable
// via a stub; Client implements it in production.
type BotAccountAPI interface {
	CreateUser(ctx context.Context, opts CreateUserOptions) (*GiteaUser, error)
	GetUserByName(ctx context.Context, username string) (*GiteaUser, error)
	CreateUserToken(ctx context.Context, username string, opts CreateUserTokenOptions) (*GiteaToken, error)
	DeleteUserToken(ctx context.Context, username string, tokenID int64) error
	CreateOrg(ctx context.Context, opts CreateOrgOptions) (*GiteaOrg, error)
	GetOrgByName(ctx context.Context, name string) (*GiteaOrg, error)
	ListOrgTeams(ctx context.Context, org string) ([]GiteaTeam, error)
}

// CreateUser implements BotAccountAPI.
func (c *Client) CreateUser(ctx context.Context, opts CreateUserOptions) (*GiteaUser, error) {
	if c == nil {
		return nil, ErrGiteaUnreachable
	}
	if opts.Login == "" || opts.Password == "" {
		return nil, fmt.Errorf("gitsync: login and password are required")
	}
	resp, err := c.doJSON(ctx, http.MethodPost, "/api/v1/admin/users", opts, http.StatusCreated)
	if err != nil {
		// doJSON wraps 409/422 as ErrGiteaUnreachable by default; sniff the
		// status out of the message to surface ErrGiteaUsernameTaken instead.
		if isConflictError(err) {
			return nil, fmt.Errorf("%w: %v", ErrGiteaUsernameTaken, err)
		}
		return nil, err
	}
	defer resp.Body.Close()

	var u GiteaUser
	if err := json.NewDecoder(resp.Body).Decode(&u); err != nil {
		return nil, fmt.Errorf("%w: decode response: %v", ErrGiteaUnreachable, err)
	}
	return &u, nil
}

// GetUserByName implements BotAccountAPI. Returns ErrGiteaTeamNotFound on
// HTTP 404 (re-uses the same sentinel for "Gitea resource not found").
func (c *Client) GetUserByName(ctx context.Context, username string) (*GiteaUser, error) {
	if c == nil {
		return nil, ErrGiteaUnreachable
	}
	if username == "" {
		return nil, fmt.Errorf("gitsync: username is required")
	}
	resp, err := c.doJSON(ctx, http.MethodGet, "/api/v1/users/"+url.PathEscape(username), nil, http.StatusOK)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	var u GiteaUser
	if err := json.NewDecoder(resp.Body).Decode(&u); err != nil {
		return nil, fmt.Errorf("%w: decode response: %v", ErrGiteaUnreachable, err)
	}
	return &u, nil
}

// CreateUserToken implements BotAccountAPI. TokenPlaintext is in
// GiteaToken.TokenPlaintext (json:"sha1" per Gitea API). Caller must
// persist a SHA256 fingerprint, not the plaintext.
//
// Auth note: Gitea locks POST /users/{name}/tokens behind reqBasicOrRevProxyAuth
// (upstream policy — admin PATs are not allowed to mint other users'
// tokens). This method therefore switches to HTTP Basic auth using the
// Client's admin credentials; if those aren't configured it returns
// ErrGiteaBasicAuthRequired.
func (c *Client) CreateUserToken(ctx context.Context, username string, opts CreateUserTokenOptions) (*GiteaToken, error) {
	if c == nil {
		return nil, ErrGiteaUnreachable
	}
	if username == "" {
		return nil, fmt.Errorf("gitsync: username is required")
	}
	if opts.Name == "" {
		return nil, fmt.Errorf("gitsync: token name is required")
	}
	path := "/api/v1/users/" + url.PathEscape(username) + "/tokens"
	resp, err := c.doJSONBasicAuth(ctx, http.MethodPost, path, opts, http.StatusCreated)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	var t GiteaToken
	if err := json.NewDecoder(resp.Body).Decode(&t); err != nil {
		return nil, fmt.Errorf("%w: decode response: %v", ErrGiteaUnreachable, err)
	}
	return &t, nil
}

// DeleteUserToken implements BotAccountAPI. Idempotent — a 404 means the
// token was already revoked, which we treat as success.
//
// Same Basic-auth requirement as CreateUserToken (DELETE on the same
// endpoint group).
func (c *Client) DeleteUserToken(ctx context.Context, username string, tokenID int64) error {
	if c == nil {
		return ErrGiteaUnreachable
	}
	if username == "" {
		return fmt.Errorf("gitsync: username is required")
	}
	if tokenID <= 0 {
		return fmt.Errorf("gitsync: token_id must be positive")
	}
	path := fmt.Sprintf("/api/v1/users/%s/tokens/%d", url.PathEscape(username), tokenID)
	resp, err := c.doJSONBasicAuth(ctx, http.MethodDelete, path, nil, http.StatusNoContent)
	if err != nil {
		// 404 → idempotent no-op.
		if errors.Is(err, ErrGiteaTeamNotFound) {
			return nil
		}
		return err
	}
	_ = resp.Body.Close()
	return nil
}

// CreateOrg implements BotAccountAPI.
func (c *Client) CreateOrg(ctx context.Context, opts CreateOrgOptions) (*GiteaOrg, error) {
	if c == nil {
		return nil, ErrGiteaUnreachable
	}
	if opts.Username == "" {
		return nil, fmt.Errorf("gitsync: org username (name) is required")
	}
	resp, err := c.doJSON(ctx, http.MethodPost, "/api/v1/orgs", opts, http.StatusCreated)
	if err != nil {
		if isConflictError(err) {
			return nil, fmt.Errorf("%w: %v", ErrGiteaUsernameTaken, err)
		}
		return nil, err
	}
	defer resp.Body.Close()

	var o GiteaOrg
	if err := json.NewDecoder(resp.Body).Decode(&o); err != nil {
		return nil, fmt.Errorf("%w: decode response: %v", ErrGiteaUnreachable, err)
	}
	return &o, nil
}

// GetOrgByName implements BotAccountAPI.
func (c *Client) GetOrgByName(ctx context.Context, name string) (*GiteaOrg, error) {
	if c == nil {
		return nil, ErrGiteaUnreachable
	}
	if name == "" {
		return nil, fmt.Errorf("gitsync: org name is required")
	}
	resp, err := c.doJSON(ctx, http.MethodGet, "/api/v1/orgs/"+url.PathEscape(name), nil, http.StatusOK)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	var o GiteaOrg
	if err := json.NewDecoder(resp.Body).Decode(&o); err != nil {
		return nil, fmt.Errorf("%w: decode response: %v", ErrGiteaUnreachable, err)
	}
	return &o, nil
}

// ListOrgTeams implements BotAccountAPI. Used by ProvisionBot to locate
// the auto-created "Owners" team so the bot user can be added with full
// permissions on the team's repos.
func (c *Client) ListOrgTeams(ctx context.Context, org string) ([]GiteaTeam, error) {
	if c == nil {
		return nil, ErrGiteaUnreachable
	}
	if org == "" {
		return nil, fmt.Errorf("gitsync: org is required")
	}
	resp, err := c.doJSON(ctx, http.MethodGet, "/api/v1/orgs/"+url.PathEscape(org)+"/teams", nil, http.StatusOK)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	var teams []GiteaTeam
	if err := json.NewDecoder(resp.Body).Decode(&teams); err != nil {
		return nil, fmt.Errorf("%w: decode response: %v", ErrGiteaUnreachable, err)
	}
	return teams, nil
}

// isConflictError sniffs doJSON's wrapped error string for the 409/422
// status markers. doJSON packs the raw status code into the error text
// (status=NNN body=...), so a substring match is the most robust way to
// distinguish username-taken from generic unreachable without changing
// doJSON's signature.
func isConflictError(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "status=409") || strings.Contains(msg, "status=422")
}

// UpdateOrgOptions is the body for PATCH /orgs/{org}. Only fields set in
// the JSON tag are sent — Gitea treats omitted fields as no-op.
type UpdateOrgOptions struct {
	Description *string `json:"description,omitempty"`
	Visibility  *string `json:"visibility,omitempty"`
	Website     *string `json:"website,omitempty"`
}

// UpdateOrg implements BotAccountAPI-adjacent org mutation. PATCH /orgs/{org}.
// Used by teamns.PatchTeam to mirror display_name → Gitea org description.
func (c *Client) UpdateOrg(ctx context.Context, org string, opts UpdateOrgOptions) error {
	if c == nil {
		return ErrGiteaUnreachable
	}
	if org == "" {
		return fmt.Errorf("gitsync: org is required")
	}
	path := "/api/v1/orgs/" + url.PathEscape(org)
	resp, err := c.doJSON(ctx, http.MethodPatch, path, opts, http.StatusOK)
	if err != nil {
		return err
	}
	_ = resp.Body.Close()
	return nil
}

// ListOrgMembers returns the usernames of all members in the org. Used by
// teamns.DissolveTeam (purge all members) and teamns.SyncTeamMembers
// (full_sync diff). Path: GET /orgs/{org}/members.
func (c *Client) ListOrgMembers(ctx context.Context, org string) ([]string, error) {
	if c == nil {
		return nil, ErrGiteaUnreachable
	}
	if org == "" {
		return nil, fmt.Errorf("gitsync: org is required")
	}
	path := "/api/v1/orgs/" + url.PathEscape(org) + "/members"
	resp, err := c.doJSON(ctx, http.MethodGet, path, nil, http.StatusOK)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	// Gitea returns [{login: ...}, ...]. We only need the login field.
	var members []struct {
		Login string `json:"login"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&members); err != nil {
		return nil, fmt.Errorf("%w: decode response: %v", ErrGiteaUnreachable, err)
	}
	out := make([]string, 0, len(members))
	for _, m := range members {
		out = append(out, m.Login)
	}
	return out, nil
}

// AddOrgMember adds a user to an org by putting them on a team. For
// team-namespace purposes the user joins the auto-created "Owners" team —
// this matches ProvisionBot's setup. Path: PUT /teams/{id}/members/{user}.
// Caller must resolve the Owners team id first (use ListOrgTeams).
func (c *Client) AddOrgMember(ctx context.Context, org, username string) error {
	if c == nil {
		return ErrGiteaUnreachable
	}
	if org == "" || username == "" {
		return fmt.Errorf("gitsync: org and username required")
	}
	teamID, err := c.lookupOwnersTeamID(ctx, org)
	if err != nil {
		return err
	}
	return c.AddTeamMember(ctx, teamID, username)
}

// RemoveOrgMember removes a user from an org's Owners team.
// Path: DELETE /teams/{id}/members/{user}.
func (c *Client) RemoveOrgMember(ctx context.Context, org, username string) error {
	if c == nil {
		return ErrGiteaUnreachable
	}
	if org == "" || username == "" {
		return fmt.Errorf("gitsync: org and username required")
	}
	teamID, err := c.lookupOwnersTeamID(ctx, org)
	if err != nil {
		return err
	}
	return c.RemoveTeamMember(ctx, teamID, username)
}

// lookupOwnersTeamID finds the "Owners" team id for an org. Tolerates the
// team being absent as a 404 → ErrGiteaTeamNotFound.
func (c *Client) lookupOwnersTeamID(ctx context.Context, org string) (int64, error) {
	teams, err := c.ListOrgTeams(ctx, org)
	if err != nil {
		return 0, err
	}
	for _, t := range teams {
		if t.Name == "Owners" {
			return t.ID, nil
		}
	}
	return 0, fmt.Errorf("gitsync: org %q has no Owners team", org)
}
