package gitsync

import (
	"context"
	"errors"
	"fmt"
	"time"

	"go.uber.org/zap"
)

// GiteaTeamResolver maps a logical team_id (provider's namespace) to the
// numeric Gitea team ID used in API paths. Production wiring uses a
// config-backed map (ConfigTeamResolver); tests inject a stub.
type GiteaTeamResolver interface {
	ResolveGiteaTeamID(teamID string) (int64, bool)
}

// ConfigTeamResolver backs GiteaTeamResolver with a static map. Returns
// (0, false) for unknown team_ids.
type ConfigTeamResolver struct {
	mapping map[string]int64
}

// NewConfigTeamResolver returns a resolver backed by the supplied map.
// Nil map is treated as empty.
func NewConfigTeamResolver(mapping map[string]int64) *ConfigTeamResolver {
	if mapping == nil {
		mapping = make(map[string]int64)
	}
	return &ConfigTeamResolver{mapping: mapping}
}

// ResolveGiteaTeamID implements GiteaTeamResolver.
func (r *ConfigTeamResolver) ResolveGiteaTeamID(teamID string) (int64, bool) {
	if r == nil {
		return 0, false
	}
	id, ok := r.mapping[teamID]
	return id, ok
}

// MemberSyncError captures a single member-level failure during sync.
// The Service continues processing other members after recording one of
// these, so the caller (handler) can surface a partial-success result
// rather than aborting the batch on the first error.
type MemberSyncError struct {
	GiteaUsername string `json:"gitea_username"`
	Operation     string `json:"operation"` // "add" | "remove"
	Error         string `json:"error"`
}

// SyncResult is the full-reconcile outcome returned by SyncTeam. The
// handler serializes this as the HTTP 200 response body.
//
// Field semantics:
//
//   - Added:   expected members newly added to Gitea (PUT succeeded).
//   - Removed: previously-synced members purged from Gitea (DELETE succeeded).
//   - Skipped: members already in the desired state (no Gitea API call made).
//   - Errors:  per-member failures; batch continued past these.
type SyncResult struct {
	TeamID      string            `json:"team_id"`
	GiteaTeamID int64             `json:"gitea_team_id"`
	Added       []string          `json:"added"`
	Removed     []string          `json:"removed"`
	Skipped     []string          `json:"skipped"`
	Errors      []MemberSyncError `json:"errors,omitempty"`
}

// Service owns the full-reconcile diff/apply loop. Construct via NewService.
//
// The sync contract is:
//
//   - Idempotent: identical inputs produce identical Gitea state across calls.
//   - Best-effort per member: a single add/remove failure does not abort
//     the batch; the failure is recorded in SyncResult.Errors.
//   - Bounded: the caller's ctx deadline is honored across all Gitea calls.
type Service struct {
	provider TeamDataProvider
	client   GiteaTeamMemberAPI
	resolver GiteaTeamResolver
	logger   *zap.Logger
}

// NewService wires a Service. nil client returns nil (feature-disabled
// signal to cmd/api/main.go — handler layer treats nil Service as 503).
// nil resolver / nil logger are tolerated with sensible defaults so tests
// don't have to construct the full dependency graph.
func NewService(provider TeamDataProvider, client GiteaTeamMemberAPI, resolver GiteaTeamResolver, logger *zap.Logger) *Service {
	if client == nil {
		return nil
	}
	if resolver == nil {
		resolver = NewConfigTeamResolver(nil)
	}
	if logger == nil {
		logger = zap.NewNop()
	}
	return &Service{
		provider: provider,
		client:   client,
		resolver: resolver,
		logger:   logger,
	}
}

// syncTimeout caps the total wall-clock for one SyncTeam call when the
// caller's ctx has no deadline. Generous enough for a 50-member team at
// ~100ms/Gitea-call; tight enough that a wedged Gitea doesn't hang admin
// UI indefinitely.
const syncTimeout = 30 * time.Second

// SyncTeam runs a full reconcile for one team_id.
//
// Flow: provider → expected list; resolver → gitea_team_id; client → current
// list; diff → toAdd/toRemove; apply per member.
//
// Returned errors are sentinel (ErrTeamNotFound / ErrGiteaTeamNotFound /
// ErrGiteaUnauthorized / ErrGiteaUnreachable) so the handler can map to
// the right HTTP status. Per-member failures are NOT returned as the
// top-level error — they go into SyncResult.Errors and the call returns
// successfully with whatever subset of operations succeeded.
func (s *Service) SyncTeam(ctx context.Context, teamID string) (*SyncResult, error) {
	if s == nil {
		return nil, ErrGiteaUnreachable
	}
	if teamID == "" {
		return nil, fmt.Errorf("gitsync: team_id is required")
	}
	if s.provider == nil {
		return nil, ErrTeamNotFound
	}

	// Honor a sane upper bound on the sync if the caller's ctx is unbounded.
	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, syncTimeout)
		defer cancel()
	}

	expected, err := s.provider.ListTeamMembers(ctx, teamID)
	if err != nil {
		if errors.Is(err, ErrTeamNotFound) {
			return nil, ErrTeamNotFound
		}
		return nil, fmt.Errorf("gitsync: provider error: %w", err)
	}

	giteaTeamID, ok := s.resolver.ResolveGiteaTeamID(teamID)
	if !ok {
		s.logger.Warn("gitsync.SyncTeam: no gitea_team_id mapping for team_id",
			zap.String("team_id", teamID))
		return nil, ErrTeamNotFound
	}

	current, err := s.client.ListTeamMembers(ctx, giteaTeamID)
	if err != nil {
		// Surface the sentinel directly so handler maps status correctly.
		return nil, err
	}

	result := &SyncResult{
		TeamID:      teamID,
		GiteaTeamID: giteaTeamID,
		Added:       []string{},
		Removed:     []string{},
		Skipped:     []string{},
	}

	expectedSet := make(map[string]struct{}, len(expected))
	for _, m := range expected {
		if m.GiteaUsername == "" {
			// Skip malformed entries but don't fail the whole sync.
			result.Errors = append(result.Errors, MemberSyncError{
				Operation: "add",
				Error:     "empty gitea_username in expected list",
			})
			continue
		}
		expectedSet[m.GiteaUsername] = struct{}{}
	}

	currentSet := make(map[string]struct{}, len(current))
	for _, m := range current {
		currentSet[m.Login] = struct{}{}
	}

	// Add: in expected but not in current.
	for username := range expectedSet {
		if _, present := currentSet[username]; present {
			result.Skipped = append(result.Skipped, username)
			continue
		}
		if err := s.client.AddTeamMember(ctx, giteaTeamID, username); err != nil {
			result.Errors = append(result.Errors, MemberSyncError{
				GiteaUsername: username,
				Operation:     "add",
				Error:         err.Error(),
			})
			s.logger.Warn("gitsync.SyncTeam: AddTeamMember failed",
				zap.String("team_id", teamID),
				zap.Int64("gitea_team_id", giteaTeamID),
				zap.String("username", username),
				zap.Error(err))
			continue
		}
		result.Added = append(result.Added, username)
	}

	// Remove: in current but not in expected.
	for username := range currentSet {
		if _, present := expectedSet[username]; present {
			continue
		}
		if err := s.client.RemoveTeamMember(ctx, giteaTeamID, username); err != nil {
			result.Errors = append(result.Errors, MemberSyncError{
				GiteaUsername: username,
				Operation:     "remove",
				Error:         err.Error(),
			})
			s.logger.Warn("gitsync.SyncTeam: RemoveTeamMember failed",
				zap.String("team_id", teamID),
				zap.Int64("gitea_team_id", giteaTeamID),
				zap.String("username", username),
				zap.Error(err))
			continue
		}
		result.Removed = append(result.Removed, username)
	}

	s.logger.Info("gitsync.SyncTeam: completed",
		zap.String("team_id", teamID),
		zap.Int64("gitea_team_id", giteaTeamID),
		zap.Int("added", len(result.Added)),
		zap.Int("removed", len(result.Removed)),
		zap.Int("skipped", len(result.Skipped)),
		zap.Int("errors", len(result.Errors)))

	return result, nil
}
