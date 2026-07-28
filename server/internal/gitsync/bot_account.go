// Package gitsync — bot account provisioning (Phase E3c).
//
// This file extends Service with the ProvisionBot / RevokeBot / RotateBot
// surface used by teamns.Service. Each method is per-tenant — it resolves
// the tenant's Git server via s.gitResolver (same path SyncTeam uses), then
// builds a transient *Client scoped to that server.
//
// ProvisionBot contract:
//
//   - Idempotent on the Gitea user (HTTP 409 → re-use the existing user).
//     NOT idempotent on the token — each call mints a new PAT. Caller-side
//     idempotency via team_bot_credentials row is the contract.
//   - The plaintext token is returned to the caller exactly once; the
//     caller persists AES-GCM(plaintext) + SHA256(plaintext), never the
//     plaintext itself.
//
// RevokeBot contract:
//
//   - Idempotent: 404 on user/token lookup is a no-op.
//   - Does NOT delete the Gitea user — that's reserved for a future
//     retention-window expiry flow. Revoke just kills the PAT.
//
// RotateBot contract:
//
//   - Mints a new PAT, returns it. Caller is responsible for revoking the
//     previous PAT (via RevokeBot, or by overwriting the row). This file's
//     RotateBot does the new-mint + old-revoke in one shot.

package gitsync

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"

	"github.com/costrict/costrict-web/server/internal/crypto"
	"go.uber.org/zap"
)

// BotCredentials is what ProvisionBot / RotateBot return. TokenPlaintext
// is the clear-text PAT — the caller MUST persist SHA256(TokenPlaintext)
// and AES-GCM(TokenPlaintext), not TokenPlaintext itself.
type BotCredentials struct {
	GiteaUsername  string
	GiteaUserID    int64
	GiteaTokenID   int64
	TokenPlaintext string
	TokenSHA256    string
}

// ErrBotAccountMissing is returned when ProvisionBot's caller forgot one
// of the required arguments (team_id / team_short / tenant_id). Maps to
// HTTP 400 at the handler layer.
var ErrBotAccountMissing = errors.New("gitsync: team_id / team_short / tenant_id required for bot provisioning")

// ProvisionBot creates the bot Gitea user + adds it to the team's org
// Owners team + mints a write-scoped PAT. Steps are NOT rolled back on
// partial failure — operator must inspect via the response and decide
// whether to retry (idempotent on user creation) or clean up out-of-band.
//
// orgName is the team's Gitea org_name (t-<team_short>); ownersTeamName is
// typically "Owners" but exposed as a param to keep ProvisionBot agnostic
// to Gitea's locale-named default teams.
func (s *Service) ProvisionBot(ctx context.Context, tenantID, teamID, teamShort, orgName string) (*BotCredentials, error) {
	if s == nil {
		return nil, ErrGiteaUnreachable
	}
	if tenantID == "" || teamID == "" || teamShort == "" || orgName == "" {
		return nil, ErrBotAccountMissing
	}

	_, client, err := s.botClientFor(ctx, tenantID)
	if err != nil {
		return nil, err
	}

	botUsername := "bot-t-" + teamShort
	botEmail := "bot+" + teamShort + "@costrict.internal"

	// 1. Get-or-create the bot user. 404 → create; 200 → reuse; anything
	//    else → propagate.
	var giteaUserID int64
	if existing, err := client.GetUserByName(ctx, botUsername); err == nil {
		giteaUserID = existing.ID
	} else if errors.Is(err, ErrGiteaTeamNotFound) {
		// Not present — create.
		password, perr := randomBotPassword()
		if perr != nil {
			return nil, fmt.Errorf("gitsync: generate bot password: %w", perr)
		}
		created, cerr := client.CreateUser(ctx, CreateUserOptions{
			Login:              botUsername,
			Email:              botEmail,
			FullName:           "Bot for team " + teamShort,
			Password:           password,
			MustChangePassword: false,
		})
		if cerr != nil {
			return nil, cerr
		}
		giteaUserID = created.ID
	} else {
		return nil, err
	}

	// 2. Add the bot to the org's "Owners" team so it inherits full repo
	//    permissions inside the namespace. We tolerate "already member"
	//    (no Gitea error code; PUT is idempotent by spec).
	ownersTeamID, err := s.findOwnersTeamID(ctx, client, orgName)
	if err != nil {
		return nil, fmt.Errorf("gitsync: locate Owners team in org %q: %w", orgName, err)
	}
	if err := client.AddTeamMember(ctx, ownersTeamID, botUsername); err != nil {
		return nil, fmt.Errorf("gitsync: add bot to Owners team: %w", err)
	}

	// 3. Mint the PAT. Scopes match the doc v1.1 §6.4 spec.
	tok, err := client.CreateUserToken(ctx, botUsername, CreateUserTokenOptions{
		Name:   "team-bot-" + teamShort,
		Scopes: []string{"write:repository", "read:user"},
	})
	if err != nil {
		return nil, err
	}

	return &BotCredentials{
		GiteaUsername:  botUsername,
		GiteaUserID:    giteaUserID,
		GiteaTokenID:   tok.ID,
		TokenPlaintext: tok.TokenPlaintext,
		TokenSHA256:    crypto.SHA256Hex([]byte(tok.TokenPlaintext)),
	}, nil
}

// RevokeBot deletes the bot's PAT. Idempotent — 404 → nil. Does NOT delete
// the bot user (preserved for audit; future retention job will GC).
func (s *Service) RevokeBot(ctx context.Context, tenantID, botUsername string, giteaTokenID int64) error {
	if s == nil {
		return ErrGiteaUnreachable
	}
	if tenantID == "" || botUsername == "" || giteaTokenID <= 0 {
		return ErrBotAccountMissing
	}

	_, client, err := s.botClientFor(ctx, tenantID)
	if err != nil {
		return err
	}
	return client.DeleteUserToken(ctx, botUsername, giteaTokenID)
}

// RotateBot revokes the previous PAT then mints a fresh one. Returns the
// new credentials; caller overwrites the persisted row.
//
// If revoke fails but mint succeeds, we return the new credentials and
// surface the revoke error in the log — caller still gets working creds
// and the leaked PAT is operator-action-required. This trades a strict
// all-or-nothing for not stranding the team without a working bot.
func (s *Service) RotateBot(ctx context.Context, tenantID, botUsername string, prevTokenID int64) (*BotCredentials, error) {
	if s == nil {
		return nil, ErrGiteaUnreachable
	}
	if tenantID == "" || botUsername == "" || prevTokenID <= 0 {
		return nil, ErrBotAccountMissing
	}

	serverCfg, client, err := s.botClientFor(ctx, tenantID)
	if err != nil {
		return nil, err
	}
	_ = serverCfg

	// Mint new PAT first so we minimize the window without valid creds.
	tok, err := client.CreateUserToken(ctx, botUsername, CreateUserTokenOptions{
		Name:   "team-bot-rotated",
		Scopes: []string{"write:repository", "read:user"},
	})
	if err != nil {
		return nil, err
	}

	// Best-effort revoke of previous PAT.
	if rerr := client.DeleteUserToken(ctx, botUsername, prevTokenID); rerr != nil {
		s.logger.Warn("gitsync.RotateBot: revoke previous token failed (new token issued regardless)",
			zap.String("bot", botUsername),
			zap.Int64("prev_token_id", prevTokenID),
			zap.Error(rerr))
	}

	// Look up the gitea user id so the caller has a complete credentials row.
	var giteaUserID int64
	if u, err := client.GetUserByName(ctx, botUsername); err == nil {
		giteaUserID = u.ID
	} else {
		s.logger.Warn("gitsync.RotateBot: GetUserByName failed after rotation",
			zap.String("bot", botUsername),
			zap.Error(err))
	}

	return &BotCredentials{
		GiteaUsername:  botUsername,
		GiteaUserID:    giteaUserID,
		GiteaTokenID:   tok.ID,
		TokenPlaintext: tok.TokenPlaintext,
		TokenSHA256:    crypto.SHA256Hex([]byte(tok.TokenPlaintext)),
	}, nil
}

// findOwnersTeamID returns the numeric ID of the "Owners" team inside the
// supplied org. Gitea auto-creates an Owners team with the org; we look it
// up by name (case-insensitive) rather than assuming ID=1.
func (s *Service) findOwnersTeamID(ctx context.Context, client *Client, orgName string) (int64, error) {
	teams, err := client.ListOrgTeams(ctx, orgName)
	if err != nil {
		return 0, err
	}
	for _, t := range teams {
		if t.Name == "Owners" {
			return t.ID, nil
		}
	}
	return 0, fmt.Errorf("gitsync: org %q has no Owners team", orgName)
}

// botClientFor resolves the tenant's Git server and builds a *Client. The
// factory is overridable via Service.botAccountClientFactory for tests.
func (s *Service) botClientFor(ctx context.Context, tenantID string) (*GitServerConfig, *Client, error) {
	serverCfg, err := s.gitResolver.Resolve(ctx, tenantID)
	if err != nil {
		return nil, nil, fmt.Errorf("gitsync: resolve git server for tenant %q: %w", tenantID, err)
	}
	client := s.botAccountClientFactory(*serverCfg)
	if client == nil {
		return nil, nil, ErrGiteaUnreachable
	}
	return serverCfg, client, nil
}

// randomBotPassword returns a 32-byte hex string the bot never uses (login
// is PAT-only). Required by Gitea's user-creation endpoint which has a
// minimum length; we go well past it.
func randomBotPassword() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}
