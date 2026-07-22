// Package userteams resolves "which teams does this user belong to?".
//
// Team membership's source of truth is the external org-team-service. Until
// that integration lands (TODO), this package returns a sentinel
// ErrOrgTeamServiceNotIntegrated so callers (@server's kb/ensure) fail
// closed with 503 ORG_TEAM_SERVICE_UNAVAILABLE — no silent Gitea fallback
// (KB_USER_ENSURE_API.md §2.3 forbids double source-of-truth).
//
// When org-team-service lands, swap the body of ListUserTeams with a real
// RPC + cache layer; the public API surface stays stable.

package userteams

import (
	"context"
	"errors"
)

// Sentinel errors.
var (
	// ErrEmptySubjectID — caller passed empty subject_id.
	ErrEmptySubjectID = errors.New("userteams: subject_id is required")
	// ErrOrgTeamServiceNotIntegrated — current state: the upstream
	// org-team-service integration isn't wired yet. Handlers should map
	// this to HTTP 503 with code ORG_TEAM_SERVICE_UNAVAILABLE.
	ErrOrgTeamServiceNotIntegrated = errors.New("userteams: org-team-service integration not yet wired")
)

// TeamSummary is the JSON-shape contract consumed by @server's kb/ensure
// resolver. Field tags match KB_USER_ENSURE_API.md §3.3.
type TeamSummary struct {
	TeamID      string `json:"team_id"`
	DisplayName string `json:"display_name"`
	Role        string `json:"role"`
}

// Service is the package-level entry point. The zero value is a valid
// not-integrated stub; production wiring injects an org-team-service
// client once available.
type Service struct {
	// orgTeamSvc reserved for future use (interface slot, intentionally
	// empty until the integration lands).
}

// New returns a Service. Today this is a not-integrated placeholder.
func New() *Service { return &Service{} }

// ListUserTeams returns the teams the user belongs to.
//
// Current state: always returns ErrOrgTeamServiceNotIntegrated. When
// org-team-service is wired, replace this body with the real lookup;
// callers (@server's kb/ensure) will get []TeamSummary back and the
// 0/1/multi matrix will activate.
func (s *Service) ListUserTeams(ctx context.Context, subjectID string) ([]TeamSummary, error) {
	if subjectID == "" {
		return nil, ErrEmptySubjectID
	}
	return nil, ErrOrgTeamServiceNotIntegrated
}
