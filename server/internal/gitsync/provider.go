package gitsync

import (
	"context"
	"errors"
)

// TeamMember is the source-of-truth expected membership for a team.
//
// GiteaUsername is the load-bearing field — it's the key used in
// PUT/DELETE /teams/:id/members/:username. SubjectID is carried through
// for traceability in logs / audit (future); GiteaEmail is informational
// only.
type TeamMember struct {
	SubjectID     string `json:"subject_id"`
	GiteaUsername string `json:"gitea_username"`
	GiteaEmail    string `json:"gitea_email,omitempty"`
}

// ErrTeamNotFound is returned by TeamDataProvider when the requested
// team_id is unknown to the provider. The Service surfaces this as HTTP
// 404 to the admin caller.
var ErrTeamNotFound = errors.New("gitsync: team not found in provider")

// TeamDataProvider is the source-of-truth for expected team membership.
//
// This is the swap point for future real providers:
//
//   - MVP: StubTeamProvider returns hardcoded data (this file).
//   - Future slice A: cs-user RPC client implementing this interface
//     against /api/internal/teams/:id/members (requires cs-user team
//     table — waits for org-team-service integration per ADR-10).
//   - Future slice B: org-team-service webhook payload adapter
//     (E4 webhook system) — translates incoming events into a snapshot.
type TeamDataProvider interface {
	// ListTeamMembers returns the expected membership for the given
	// team_id. Returns ErrTeamNotFound if the team is unknown.
	//
	// Callers must NOT treat an empty slice + nil error as "unknown team"
	// — a known team legitimately can have zero expected members (which
	// would trigger a full purge of Gitea-side members during sync).
	ListTeamMembers(ctx context.Context, teamID string) ([]TeamMember, error)
}

// StubTeamProvider is the MVP TeamDataProvider. Returns hardcoded
// membership from a map keyed by team_id. Construct via NewStubProvider
// or NewStubProviderFromMap.
//
// Tests use this directly; production wiring (cmd/api/main.go) uses it
// temporarily until a real provider lands behind the same interface.
type StubTeamProvider struct {
	teams map[string][]TeamMember
}

// NewStubProvider returns a StubTeamProvider seeded with no teams. Callers
// add teams via WithTeam before use.
func NewStubProvider() *StubTeamProvider {
	return &StubTeamProvider{teams: make(map[string][]TeamMember)}
}

// NewStubProviderFromMap returns a StubTeamProvider seeded with the
// supplied map. Nil map is treated as empty (safe default).
func NewStubProviderFromMap(teams map[string][]TeamMember) *StubTeamProvider {
	if teams == nil {
		teams = make(map[string][]TeamMember)
	}
	return &StubTeamProvider{teams: teams}
}

// WithTeam adds (or replaces) the membership for a team_id. Builder-style;
// returns the receiver for chaining in test setup.
func (p *StubTeamProvider) WithTeam(teamID string, members []TeamMember) *StubTeamProvider {
	if p == nil {
		return p
	}
	if p.teams == nil {
		p.teams = make(map[string][]TeamMember)
	}
	p.teams[teamID] = members
	return p
}

// ListTeamMembers implements TeamDataProvider.
func (p *StubTeamProvider) ListTeamMembers(ctx context.Context, teamID string) ([]TeamMember, error) {
	if p == nil {
		return nil, ErrTeamNotFound
	}
	members, ok := p.teams[teamID]
	if !ok {
		return nil, ErrTeamNotFound
	}
	// Return a defensive copy so callers can mutate without corrupting
	// the provider's state.
	out := make([]TeamMember, len(members))
	copy(out, members)
	return out, nil
}
