// Package gitsync — org-level operations used by teamns.Service.
//
// This file extends Service with the org CRUD surface the team-namespace
// orchestration layer needs (get-or-create org, update description, list /
// add / remove org members, bulk-purge for dissolve). Each method is
// per-tenant: it resolves the tenant's Git server via s.gitResolver and
// builds a transient *Client scoped to it.

package gitsync

import (
	"context"
	"errors"
	"strings"
)

// ResolveGitServer exposes the tenant's resolved Git server config to
// callers (teamns.CreateTeam needs the ServerID + Endpoint to persist on
// the team_ns row at create time). Idempotent — caches via the resolver.
func (s *Service) ResolveGitServer(ctx context.Context, tenantID string) (*GitServerConfig, error) {
	if s == nil {
		return nil, ErrGiteaUnreachable
	}
	return s.gitResolver.Resolve(ctx, tenantID)
}

// EnsureOrg gets-or-creates a Gitea org. Tolerates 409 (already exists) by
// treating it as success — ProvisionBot relies on this to be idempotent
// across team create retries.
//
// displayName is mirrored into the org's full_name at creation time. Gitea
// doesn't expose a description field on POST /orgs; the teamns layer sets
// description via UpdateOrgDescription separately.
func (s *Service) EnsureOrg(ctx context.Context, tenantID, orgName, displayName string) error {
	if s == nil {
		return ErrGiteaUnreachable
	}
	_, client, err := s.botClientFor(ctx, tenantID)
	if err != nil {
		return err
	}

	// Try get first.
	if _, err := client.GetOrgByName(ctx, orgName); err == nil {
		return nil
	} else if !errors.Is(err, ErrGiteaTeamNotFound) {
		return err
	}

	_, err = client.CreateOrg(ctx, CreateOrgOptions{
		Username:   orgName,
		FullName:   displayName,
		Visibility: "private",
	})
	if err != nil {
		// 409 — already exists, race with another request. Treat as success.
		if errors.Is(err, ErrGiteaUsernameTaken) {
			return nil
		}
		return err
	}
	return nil
}

// UpdateOrgDescription mirrors text into the Gitea org's description.
// Best-effort; teamns.PatchTeam tolerates failure with a log line.
func (s *Service) UpdateOrgDescription(ctx context.Context, tenantID, orgName, description string) error {
	if s == nil {
		return ErrGiteaUnreachable
	}
	_, client, err := s.botClientFor(ctx, tenantID)
	if err != nil {
		return err
	}
	desc := description
	return client.UpdateOrg(ctx, orgName, UpdateOrgOptions{Description: &desc})
}

// ListOrgMembers returns the gitea usernames of all members in the org.
func (s *Service) ListOrgMembers(ctx context.Context, tenantID, orgName string) ([]string, error) {
	if s == nil {
		return nil, ErrGiteaUnreachable
	}
	_, client, err := s.botClientFor(ctx, tenantID)
	if err != nil {
		return nil, err
	}
	return client.ListOrgMembers(ctx, orgName)
}

// AddOrgMember resolves the org's Owners team and adds the user to it.
func (s *Service) AddOrgMember(ctx context.Context, tenantID, orgName, username string) error {
	if s == nil {
		return ErrGiteaUnreachable
	}
	_, client, err := s.botClientFor(ctx, tenantID)
	if err != nil {
		return err
	}
	return client.AddOrgMember(ctx, orgName, username)
}

// RemoveOrgMember resolves the org's Owners team and removes the user from it.
// Idempotent: a 404 on either the team or the membership is a no-op.
func (s *Service) RemoveOrgMember(ctx context.Context, tenantID, orgName, username string) error {
	if s == nil {
		return ErrGiteaUnreachable
	}
	_, client, err := s.botClientFor(ctx, tenantID)
	if err != nil {
		return err
	}
	if err := client.RemoveOrgMember(ctx, orgName, username); err != nil {
		if errors.Is(err, ErrGiteaTeamNotFound) {
			return nil
		}
		// doJSON also wraps 404 member-not-found as ErrGiteaTeamNotFound via
		// the generic 404 path — same recovery.
		if strings.Contains(err.Error(), "status=404") {
			return nil
		}
		return err
	}
	return nil
}

// RemoveAllMembers purges all non-bot members from the org. Used by
// teamns.DissolveTeam. Returns the number of members actually removed.
// Failures are logged but do not abort the loop — dissolve proceeds to
// archive the org regardless.
func (s *Service) RemoveAllMembers(ctx context.Context, tenantID, orgName string) (int, error) {
	if s == nil {
		return 0, ErrGiteaUnreachable
	}
	members, err := s.ListOrgMembers(ctx, tenantID, orgName)
	if err != nil {
		return 0, err
	}
	removed := 0
	for _, m := range members {
		// Always keep the bot account; it gets revoked via RevokeBot in a
		// separate step so the credentials row stays consistent.
		if strings.HasPrefix(m, "bot-t-") {
			continue
		}
		if err := s.RemoveOrgMember(ctx, tenantID, orgName, m); err != nil {
			continue
		}
		removed++
	}
	return removed, nil
}
