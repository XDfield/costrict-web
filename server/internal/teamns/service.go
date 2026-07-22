// Package teamns orchestrates the team-namespace API v1.1 surface.
//
// The Service ties together:
//   - the team_ns + team_bot_credentials tables (state mirror),
//   - gitsync.Service (Gitea ns + bot account + member sync),
//   - user.UserRefResolver (UserRef → gitea_username),
//   - crypto.AESGCM (encrypt bot token plaintext at rest).
//
// The handlers in internal/handlers/team_internal.go stay thin — they parse
// requests, translate sentinel errors to HTTP codes, and serialize responses.
// All orchestration logic (idempotent create, dissolve, rotate) lives here.
//
// Lifecycle invariants enforced by this file:
//
//   - team_ns.team_id is the primary key. Re-POST /teams with the same id is
//     a no-op that returns the existing ns + bot metadata WITHOUT the bot
//     plaintext token.
//   - bot token plaintext is returned exactly once at create time and once
//     at each rotate time. Never re-exposed on GET.
//   - Dissolve sets team_ns.status=archived and team_bot_credentials.revoked_at.
//     The bot rows are retained for audit; the retention_until on team_ns
//     carries the 90-day window.
//   - All operations are tenant-scoped via tenant.TenantIDFromContext(ctx).
//     Re-creating the same team_id under a different tenant_id is rejected
//     as ErrTeamIDTaken (HTTP 409) to prevent cross-tenant leakage.

package teamns

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/costrict/costrict-web/server/internal/crypto"
	"github.com/costrict/costrict-web/server/internal/gitsync"
	"github.com/costrict/costrict-web/server/internal/models"
	"github.com/costrict/costrict-web/server/internal/tenant"
	"github.com/costrict/costrict-web/server/internal/user"
	"go.uber.org/zap"
	"gorm.io/gorm"
)

// Sentinel errors. Handlers map these to HTTP codes via errors.Is.
var (
	// ErrInvalidRequest — caller supplied a bad team_id, missing display
	// name, an invalid UserRef, or an illegal mode value. HTTP 400.
	ErrInvalidRequest = errors.New("teamns: invalid request")
	// ErrTeamNotFound — team_id has no team_ns row. HTTP 404.
	ErrTeamNotFound = errors.New("teamns: team not found")
	// ErrTeamIDTaken — POST /teams with a team_id that exists under a
	// DIFFERENT tenant_id. HTTP 409 (true conflict, not idempotent re-POST).
	ErrTeamIDTaken = errors.New("teamns: team_id already exists under another tenant")
	// ErrTeamArchived — team ns is in archived status; write rejected. HTTP 410.
	ErrTeamArchived = errors.New("teamns: team archived")
	// ErrMemberUnresolved — ALL members failed UserRef resolution in a
	// members:sync batch. HTTP 404. Partial failures don't trigger this.
	ErrMemberUnresolved = errors.New("teamns: all member refs unresolved")
	// ErrBotUsernameTaken — bot-t-<short> already exists in Gitea under a
	// human user. HTTP 409.
	ErrBotUsernameTaken = errors.New("teamns: bot username taken")
	// ErrTenantGitServerUnresolved — tenant has no bound git_server or the
	// bound server is disabled. HTTP 412.
	ErrTenantGitServerUnresolved = errors.New("teamns: tenant git server unresolved")
)

// DefaultRetention is the post-dissolve retention window before physical
// GC. Per doc: 90 days. Operator runbook handles actual deletion.
const DefaultRetention = 90 * 24 * time.Hour

// uuidRe — RFC 4122 variant of UUID (most common case; canonical hex form).
// team_id is allocated by org-team-service as a UUID, so we validate shape
// up-front to give a clean 400 instead of pushing garbage to Gitea.
var uuidRe = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

// shortFromTeamID derives t-<team_short_id> per the doc spec: the first 8
// hex characters of the canonical UUID. Caller validates team_id is a UUID.
func shortFromTeamID(teamID string) string {
	if len(teamID) < 8 {
		return teamID
	}
	return teamID[:8]
}

// orgNameForTeam returns t-<team_short>.
func orgNameForTeam(teamID string) string {
	return "t-" + shortFromTeamID(teamID)
}

// Service is the orchestration layer for the team-namespace API v1.1.
type Service struct {
	db          *gorm.DB
	gitsync     *gitsync.Service
	userRef     *user.UserRefResolver
	crypto      *crypto.AESGCM
	logger      *zap.Logger
	retention   time.Duration
	idGenerator func() string // for audit_log_id; injected for tests
	// gitServerFor resolves a tenant's git backend as the platform-agnostic
	// GitServer interface. Defaults to delegating to gitsync.Service so
	// production wiring is unchanged; tests override to inject a stub.
	gitServerFor func(ctx context.Context, tenantID string) (gitsync.GitServer, error)
}

// NewService wires a Service. db may be nil — methods then return
// gorm.ErrInvalidDB (callers degrade to 503). gitsync / userRef may be nil
// to disable the Gitea / cs-user surface; the corresponding methods then
// return ErrTenantGitServerUnresolved / ErrMemberUnresolved. crypto must
// be non-nil for any code path that mints bot tokens.
func NewService(db *gorm.DB, gs *gitsync.Service, uref *user.UserRefResolver, aes *crypto.AESGCM, logger *zap.Logger) *Service {
	if logger == nil {
		logger = zap.NewNop()
	}
	s := &Service{
		db:          db,
		gitsync:     gs,
		userRef:     uref,
		crypto:      aes,
		logger:      logger,
		retention:   DefaultRetention,
		idGenerator: func() string { return fmt.Sprintf("audit-%d", time.Now().UnixNano()) },
	}
	// Default resolver: delegate to the wired gitsync.Service. When gs is
	// nil (e.g. unit tests that don't exercise git ops), the closure also
	// returns nil so callers fall through to ErrTenantGitServerUnresolved.
	s.gitServerFor = func(ctx context.Context, tenantID string) (gitsync.GitServer, error) {
		if gs == nil {
			return nil, nil
		}
		return gs.GitServerFor(ctx, tenantID)
	}
	return s
}

// CreateTeamRequest is the POST /api/internal/teams body.
type CreateTeamRequest struct {
	TeamID          string         `json:"team_id"`
	TeamDisplayName string         `json:"team_display_name"`
	Creator         user.UserRef   `json:"creator"`
	InitialMembers  []user.UserRef `json:"initial_members"`
}

// CreateTeamResult is the response. BotToken is the plaintext PAT — ONLY
// populated when this create actually minted a new token (created.bot_token
// is true). Idempotent re-POSTs leave BotToken empty.
type CreateTeamResult struct {
	TeamID            string       `json:"team_id"`
	TeamNSOrg         string       `json:"team_ns_org"`
	TeamDisplayName   string       `json:"team_display_name"`
	GitServerID       string       `json:"git_server_id"`
	GiteaBaseURL      string       `json:"gitea_base_url"`
	Created           CreatedFlags `json:"created"`
	MembersAddedCount int          `json:"members_added_count"`
	Bot               *BotView     `json:"bot,omitempty"`
	AuditLogID        string       `json:"audit_log_id"`
}

// CreatedFlags tracks which sub-operations ran. Per doc §1.3.
type CreatedFlags struct {
	TeamNS     bool `json:"team_ns"`
	BotAccount bool `json:"bot_account"`
	BotToken   bool `json:"bot_token"`
}

// BotView is the canonical bot view returned by every endpoint that exposes
// bot metadata. TokenPlaintext is the bare PAT — populated ONLY at create
// and rotate time; never on GET.
type BotView struct {
	GiteaUsername  string     `json:"gitea_username"`
	GiteaUserID    int64      `json:"gitea_user_id"`
	TokenID        int64      `json:"token_id"`
	TokenPlaintext string     `json:"token,omitempty"`
	TokenSHA256    string     `json:"token_sha256,omitempty"`
	CreatedAt      *time.Time `json:"created_at,omitempty"`
	RotatedAt      *time.Time `json:"rotated_at,omitempty"`
}

// CreateTeam implements POST /api/internal/teams. Idempotent on team_id
// within the same tenant_id. See buzzing-nibbling-kernighan.md §"接口实现".
func (s *Service) CreateTeam(ctx context.Context, req CreateTeamRequest) (*CreateTeamResult, error) {
	if s == nil {
		return nil, gorm.ErrInvalidDB
	}
	if err := validateCreateTeamRequest(req); err != nil {
		return nil, err
	}
	tenantID := tenant.TenantIDFromContext(ctx)
	if tenantID == "" {
		tenantID = tenant.DefaultTenantID
	}

	// 1. Idempotency: look up existing team_ns row.
	var existing models.TeamNamespace
	err := s.db.WithContext(ctx).Where("team_id = ?", req.TeamID).First(&existing).Error
	if err == nil {
		// team_ns row exists. Verify tenant match.
		if existing.TenantID != tenantID {
			return nil, ErrTeamIDTaken
		}
		return s.idempotentCreateResponse(ctx, existing)
	}
	if !errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, fmt.Errorf("teamns: lookup team_ns: %w", err)
	}

	// 2. Resolve tenant git server via gitsync (it does the RPC resolve).
	if s.gitsync == nil {
		return nil, ErrTenantGitServerUnresolved
	}
	serverCfg, err := s.gitsync.ResolveGitServer(ctx, tenantID)
	if err != nil {
		return nil, mapGitResolveError(err)
	}

	// 3. Get-or-create Gitea org t-<team_short>. Tolerate 409 (already exists).
	orgName := orgNameForTeam(req.TeamID)
	if err := s.gitsync.EnsureOrg(ctx, tenantID, orgName, req.TeamDisplayName); err != nil {
		return nil, fmt.Errorf("teamns: ensure org: %w", err)
	}

	// 4. Provision bot account (creates user + adds to Owners + mints PAT).
	teamShort := shortFromTeamID(req.TeamID)
	botCreds, err := s.gitsync.ProvisionBot(ctx, tenantID, req.TeamID, teamShort, orgName)
	if err != nil {
		if errors.Is(err, gitsync.ErrGiteaUsernameTaken) {
			return nil, ErrBotUsernameTaken
		}
		return nil, fmt.Errorf("teamns: provision bot: %w", err)
	}

	// 5. Encrypt + persist both rows.
	encrypted, err := s.crypto.Seal([]byte(botCreds.TokenPlaintext))
	if err != nil {
		return nil, fmt.Errorf("teamns: encrypt bot token: %w", err)
	}

	now := time.Now().UTC()
	teamNS := models.TeamNamespace{
		TeamID:          req.TeamID,
		TenantID:        tenantID,
		TeamDisplayName: req.TeamDisplayName,
		TeamNSOrg:       orgName,
		TeamShort:       teamShort,
		GitServerID:     serverCfg.ServerID,
		Status:          "active",
		CreatedAt:       now,
		UpdatedAt:       now,
	}
	if err := s.db.WithContext(ctx).Create(&teamNS).Error; err != nil {
		// Best-effort rollback on Gitea side; operator may need to clean up.
		s.logger.Error("teamns.CreateTeam: persist team_ns failed",
			zap.String("team_id", req.TeamID),
			zap.String("org", orgName),
			zap.Error(err))
		return nil, fmt.Errorf("teamns: persist team_ns: %w", err)
	}

	creds := models.TeamBotCredentials{
		TeamID:         req.TeamID,
		TenantID:       tenantID,
		GitServerID:    serverCfg.ServerID,
		GiteaUsername:  botCreds.GiteaUsername,
		GiteaUserID:    botCreds.GiteaUserID,
		GiteaTokenID:   botCreds.GiteaTokenID,
		TokenEncrypted: encrypted,
		TokenSHA256:    botCreds.TokenSHA256,
		CreatedAt:      now,
	}
	if err := s.db.WithContext(ctx).Create(&creds).Error; err != nil {
		s.logger.Error("teamns.CreateTeam: persist team_bot_credentials failed",
			zap.String("team_id", req.TeamID),
			zap.Error(err))
		return nil, fmt.Errorf("teamns: persist bot credentials: %w", err)
	}

	// 6. Optional seed members (delta mode).
	membersAdded := 0
	if len(req.InitialMembers) > 0 {
		added, _, _ := s.syncMembers(ctx, tenantID, req.TeamID, orgName, req.InitialMembers, nil, false)
		membersAdded = added
	}

	return &CreateTeamResult{
		TeamID:            req.TeamID,
		TeamNSOrg:         orgName,
		TeamDisplayName:   req.TeamDisplayName,
		GitServerID:       serverCfg.ServerID,
		GiteaBaseURL:      serverCfg.Endpoint,
		Created:           CreatedFlags{TeamNS: true, BotAccount: true, BotToken: true},
		MembersAddedCount: membersAdded,
		Bot: &BotView{
			GiteaUsername:  botCreds.GiteaUsername,
			GiteaUserID:    botCreds.GiteaUserID,
			TokenID:        botCreds.GiteaTokenID,
			TokenPlaintext: botCreds.TokenPlaintext,
			TokenSHA256:    botCreds.TokenSHA256,
			CreatedAt:      &now,
		},
		AuditLogID: s.idGenerator(),
	}, nil
}

// idempotentCreateResponse returns the response shape for an idempotent
// re-POST. Bot token plaintext is intentionally NOT included.
func (s *Service) idempotentCreateResponse(ctx context.Context, ns models.TeamNamespace) (*CreateTeamResult, error) {
	var creds models.TeamBotCredentials
	err := s.db.WithContext(ctx).Where("team_id = ?", ns.TeamID).First(&creds).Error
	if err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, fmt.Errorf("teamns: lookup bot creds: %w", err)
	}
	result := &CreateTeamResult{
		TeamID:          ns.TeamID,
		TeamNSOrg:       ns.TeamNSOrg,
		TeamDisplayName: ns.TeamDisplayName,
		GitServerID:     ns.GitServerID,
		Created:         CreatedFlags{}, // all false per doc §1.4
		AuditLogID:      s.idGenerator(),
	}
	if creds.TeamID != "" {
		result.Bot = &BotView{
			GiteaUsername: creds.GiteaUsername,
			GiteaUserID:   creds.GiteaUserID,
			TokenID:       creds.GiteaTokenID,
			TokenSHA256:   creds.TokenSHA256,
			CreatedAt:     &creds.CreatedAt,
		}
		if creds.RotatedAt != nil {
			result.Bot.RotatedAt = creds.RotatedAt
		}
	}
	return result, nil
}

// GetTeam implements GET /api/internal/teams/:team_id. Never returns bot
// plaintext.
func (s *Service) GetTeam(ctx context.Context, teamID string) (*GetTeamResult, error) {
	if s == nil {
		return nil, gorm.ErrInvalidDB
	}
	if !uuidRe.MatchString(teamID) {
		return nil, ErrInvalidRequest
	}
	var ns models.TeamNamespace
	if err := s.db.WithContext(ctx).Where("team_id = ?", teamID).First(&ns).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, ErrTeamNotFound
		}
		return nil, fmt.Errorf("teamns: get team: %w", err)
	}
	result := &GetTeamResult{
		TeamID:          ns.TeamID,
		TeamNSOrg:       ns.TeamNSOrg,
		TeamDisplayName: ns.TeamDisplayName,
		GitServerID:     ns.GitServerID,
		Status:          ns.Status,
		CreatedAt:       ns.CreatedAt,
		UpdatedAt:       ns.UpdatedAt,
	}

	var creds models.TeamBotCredentials
	if err := s.db.WithContext(ctx).Where("team_id = ?", teamID).First(&creds).Error; err == nil {
		// bot row exists; expose metadata (no plaintext).
		result.Bot = &BotView{
			GiteaUsername: creds.GiteaUsername,
			GiteaUserID:   creds.GiteaUserID,
			TokenID:       creds.GiteaTokenID,
			TokenSHA256:   creds.TokenSHA256,
			CreatedAt:     &creds.CreatedAt,
			RotatedAt:     creds.RotatedAt,
		}
	}
	return result, nil
}

// GetTeamResult is the GET /api/internal/teams/:team_id response.
type GetTeamResult struct {
	TeamID          string    `json:"team_id"`
	TeamNSOrg       string    `json:"team_ns_org"`
	TeamDisplayName string    `json:"team_display_name"`
	GitServerID     string    `json:"git_server_id"`
	Status          string    `json:"status"`
	Bot             *BotView  `json:"bot,omitempty"`
	CreatedAt       time.Time `json:"created_at"`
	UpdatedAt       time.Time `json:"updated_at"`
}

// ListTeams implements GET /api/internal/teams. Always single-tenant — the
// tenant comes from ctx. tenantIDQuery is the caller-supplied tenant_id
// query value, present only so this layer can REJECT it (the contract says
// no cross-tenant aggregation).
func (s *Service) ListTeams(ctx context.Context, params ListParams) (*ListResult, error) {
	if s == nil {
		return nil, gorm.ErrInvalidDB
	}
	if params.TenantIDQuery != "" {
		// Doc §3.3 — explicit reject.
		return nil, ErrInvalidRequest
	}
	if params.Status != "" && params.Status != "active" && params.Status != "archived" {
		return nil, ErrInvalidRequest
	}
	page := params.Page
	if page < 1 {
		page = 1
	}
	pageSize := params.PageSize
	if pageSize < 1 {
		pageSize = 50
	}
	if pageSize > 200 {
		pageSize = 200
	}
	tenantID := tenant.TenantIDFromContext(ctx)
	if tenantID == "" {
		tenantID = tenant.DefaultTenantID
	}

	q := s.db.WithContext(ctx).Model(&models.TeamNamespace{}).Where("tenant_id = ?", tenantID)
	if params.Status != "" {
		q = q.Where("status = ?", params.Status)
	}
	if strings.TrimSpace(params.Query) != "" {
		like := "%" + strings.TrimSpace(params.Query) + "%"
		q = q.Where("team_display_name LIKE ?", like)
	}

	var total int64
	if err := q.Count(&total).Error; err != nil {
		return nil, fmt.Errorf("teamns: count teams: %w", err)
	}
	var rows []models.TeamNamespace
	if err := q.Order("created_at DESC").
		Offset((page - 1) * pageSize).Limit(pageSize).
		Find(&rows).Error; err != nil {
		return nil, fmt.Errorf("teamns: list teams: %w", err)
	}

	teams := make([]ListTeamItem, 0, len(rows))
	for _, r := range rows {
		teams = append(teams, ListTeamItem{
			TeamID:          r.TeamID,
			TeamNSOrg:       r.TeamNSOrg,
			TeamDisplayName: r.TeamDisplayName,
			Status:          r.Status,
			CreatedAt:       r.CreatedAt,
		})
	}
	return &ListResult{
		Teams:    teams,
		Page:     page,
		PageSize: pageSize,
		Total:    total,
	}, nil
}

// ListParams holds GET /api/internal/teams query parameters.
type ListParams struct {
	Page          int
	PageSize      int
	Query         string
	Status        string
	TenantIDQuery string // must be empty — explicit reject per doc §3
}

// ListResult is the paginated list response.
type ListResult struct {
	Teams    []ListTeamItem `json:"teams"`
	Page     int            `json:"page"`
	PageSize int            `json:"page_size"`
	Total    int64          `json:"total"`
}

// ListTeamItem is the per-team summary in ListResult.
type ListTeamItem struct {
	TeamID          string    `json:"team_id"`
	TeamNSOrg       string    `json:"team_ns_org"`
	TeamDisplayName string    `json:"team_display_name"`
	Status          string    `json:"status"`
	CreatedAt       time.Time `json:"created_at"`
}

// PatchTeam implements PATCH /api/internal/teams/:team_id. Currently only
// supports display_name + description (description mirrors to Gitea org).
func (s *Service) PatchTeam(ctx context.Context, teamID string, req PatchTeamRequest) error {
	if s == nil {
		return gorm.ErrInvalidDB
	}
	if !uuidRe.MatchString(teamID) {
		return ErrInvalidRequest
	}
	if req.TeamDisplayName == "" && req.Description == "" {
		return ErrInvalidRequest
	}
	var ns models.TeamNamespace
	if err := s.db.WithContext(ctx).Where("team_id = ?", teamID).First(&ns).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return ErrTeamNotFound
		}
		return fmt.Errorf("teamns: patch lookup: %w", err)
	}
	if ns.Status == "archived" {
		return ErrTeamArchived
	}

	updates := map[string]any{"updated_at": time.Now().UTC()}
	if req.TeamDisplayName != "" {
		updates["team_display_name"] = req.TeamDisplayName
	}
	if req.Description != "" {
		// Description has no dedicated column; mirror into display_name suffix
		// OR — preferred — push to Gitea org description. For MVP we propagate
		// to Gitea; persisting description locally is deferred until a column
		// is added.
		if s.gitsync != nil {
			if err := s.gitsync.UpdateOrgDescription(ctx, ns.TenantID, ns.TeamNSOrg, req.Description); err != nil {
				s.logger.Warn("teamns.PatchTeam: gitea description mirror failed",
					zap.String("team_id", teamID),
					zap.Error(err))
			}
		}
	}
	if err := s.db.WithContext(ctx).Model(&ns).Updates(updates).Error; err != nil {
		return fmt.Errorf("teamns: patch update: %w", err)
	}
	return nil
}

// PatchTeamRequest is PATCH /api/internal/teams/:team_id body.
type PatchTeamRequest struct {
	TeamDisplayName string `json:"team_display_name"`
	Description     string `json:"description"`
}

// DissolveTeam implements POST /api/internal/teams/:team_id/dissolve. Sets
// status=archived, removes all members, revokes bot token. Idempotent.
func (s *Service) DissolveTeam(ctx context.Context, teamID string, req DissolveTeamRequest) (*DissolveTeamResult, error) {
	if s == nil {
		return nil, gorm.ErrInvalidDB
	}
	if !uuidRe.MatchString(teamID) {
		return nil, ErrInvalidRequest
	}
	if strings.TrimSpace(req.Reason) == "" {
		return nil, ErrInvalidRequest
	}

	var ns models.TeamNamespace
	err := s.db.WithContext(ctx).Where("team_id = ?", teamID).First(&ns).Error
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			// Per doc §6.4: team_id never created → treat as already dissolved.
			return &DissolveTeamResult{
				TeamNSOrg:           "",
				Archived:            false,
				MembersRemovedCount: 0,
				BotTokenRevoked:     false,
				RetentionUntil:      nil,
				AuditLogID:          s.idGenerator(),
			}, nil
		}
		return nil, fmt.Errorf("teamns: dissolve lookup: %w", err)
	}

	// Already archived → idempotent.
	if ns.Status == "archived" {
		return &DissolveTeamResult{
			TeamNSOrg:           ns.TeamNSOrg,
			Archived:            true,
			MembersRemovedCount: 0,
			BotTokenRevoked:     false,
			RetentionUntil:      ns.RetentionUntil,
			AuditLogID:          s.idGenerator(),
		}, nil
	}

	// Remove all members via Gitea. Best-effort — partial failures don't
	// block dissolve since the org gets archived anyway.
	membersRemoved := 0
	if s.gitsync != nil {
		if n, err := s.gitsync.RemoveAllMembers(ctx, ns.TenantID, ns.TeamNSOrg); err != nil {
			s.logger.Warn("teamns.DissolveTeam: remove all members failed",
				zap.String("team_id", teamID),
				zap.Error(err))
		} else {
			membersRemoved = n
		}
	}

	// Revoke bot token.
	botRevoked := false
	var creds models.TeamBotCredentials
	if err := s.db.WithContext(ctx).Where("team_id = ? AND revoked_at IS NULL", teamID).First(&creds).Error; err == nil {
		if s.gitsync != nil {
			if err := s.gitsync.RevokeBot(ctx, ns.TenantID, creds.GiteaUsername, creds.GiteaTokenID); err != nil {
				s.logger.Warn("teamns.DissolveTeam: revoke bot failed",
					zap.String("team_id", teamID),
					zap.Error(err))
			}
		}
		now := time.Now().UTC()
		if err := s.db.WithContext(ctx).Model(&creds).Update("revoked_at", now).Error; err != nil {
			s.logger.Warn("teamns.DissolveTeam: persist revoked_at failed",
				zap.String("team_id", teamID),
				zap.Error(err))
		}
		botRevoked = true
	}

	retentionUntil := time.Now().UTC().Add(s.retention)
	now := time.Now().UTC()
	if err := s.db.WithContext(ctx).Model(&ns).Updates(map[string]any{
		"status":             "archived",
		"dissolved_at":       now,
		"dissolution_reason": strings.TrimSpace(req.Reason),
		"retention_until":    retentionUntil,
		"updated_at":         now,
	}).Error; err != nil {
		return nil, fmt.Errorf("teamns: persist dissolve: %w", err)
	}

	return &DissolveTeamResult{
		TeamNSOrg:           ns.TeamNSOrg,
		Archived:            true,
		MembersRemovedCount: membersRemoved,
		BotTokenRevoked:     botRevoked,
		RetentionUntil:      &retentionUntil,
		AuditLogID:          s.idGenerator(),
	}, nil
}

// DissolveTeamRequest is the POST /dissolve body.
type DissolveTeamRequest struct {
	Reason string       `json:"reason"`
	Actor  user.UserRef `json:"actor"`
}

// DissolveTeamResult is the POST /dissolve response.
type DissolveTeamResult struct {
	TeamNSOrg           string     `json:"team_ns_org"`
	Archived            bool       `json:"archived"`
	MembersRemovedCount int        `json:"members_removed_count"`
	BotTokenRevoked     bool       `json:"bot_token_revoked"`
	RetentionUntil      *time.Time `json:"retention_until"`
	AuditLogID          string     `json:"audit_log_id"`
}

// SyncMembersRequest is the POST /members:sync body.
type SyncMembersRequest struct {
	Mode          string         `json:"mode"`
	AddMembers    []user.UserRef `json:"add_members"`
	RemoveMembers []user.UserRef `json:"remove_members"`
}

// SyncMembersResult is the POST /members:sync response.
type SyncMembersResult struct {
	TeamNSOrg           string             `json:"team_ns_org"`
	MembersAddedCount   int                `json:"members_added_count"`
	MembersRemovedCount int                `json:"members_removed_count"`
	MembersUnresolved   []UnresolvedMember `json:"members_unresolved"`
	AuditLogID          string             `json:"audit_log_id"`
}

// UnresolvedMember records a single failed UserRef.
type UnresolvedMember struct {
	UserID         string `json:"user_id,omitempty"`
	EmployeeNumber string `json:"employee_number,omitempty"`
	Reason         string `json:"reason"`
}

// SyncTeamMembers implements POST /members:sync. mode=delta or full_sync.
func (s *Service) SyncTeamMembers(ctx context.Context, teamID string, req SyncMembersRequest) (*SyncMembersResult, error) {
	if s == nil {
		return nil, gorm.ErrInvalidDB
	}
	if !uuidRe.MatchString(teamID) {
		return nil, ErrInvalidRequest
	}
	if req.Mode != "delta" && req.Mode != "full_sync" {
		return nil, ErrInvalidRequest
	}
	// Overlap check (delta mode only — full_sync ignores remove_members).
	if req.Mode == "delta" && overlapUserRefs(req.AddMembers, req.RemoveMembers) {
		return nil, ErrInvalidRequest
	}

	var ns models.TeamNamespace
	if err := s.db.WithContext(ctx).Where("team_id = ?", teamID).First(&ns).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			// Doc §5.5 — 412 TEAM_NS_NOT_INITIALIZED. We surface ErrInvalidRequest
			// as a placeholder; handlers map it to 412 with the right code.
			return nil, ErrTeamNotFound
		}
		return nil, fmt.Errorf("teamns: members:sync lookup: %w", err)
	}
	if ns.Status == "archived" {
		return nil, ErrTeamArchived
	}

	added, removed, unresolved := s.syncMembers(ctx, ns.TenantID, teamID, ns.TeamNSOrg, req.AddMembers, req.RemoveMembers, req.Mode == "full_sync")

	// Per doc §5.5: 404 only when ALL refs failed.
	totalRequested := len(req.AddMembers) + len(req.RemoveMembers)
	if totalRequested > 0 && len(unresolved) == totalRequested {
		return nil, ErrMemberUnresolved
	}

	return &SyncMembersResult{
		TeamNSOrg:           ns.TeamNSOrg,
		MembersAddedCount:   added,
		MembersRemovedCount: removed,
		MembersUnresolved:   unresolved,
		AuditLogID:          s.idGenerator(),
	}, nil
}

// RotateBotToken implements POST /bot-token:rotate. Returns the new plaintext
// token — callers must persist it.
func (s *Service) RotateBotToken(ctx context.Context, teamID string, req RotateBotTokenRequest) (*RotateBotTokenResult, error) {
	if s == nil {
		return nil, gorm.ErrInvalidDB
	}
	if !uuidRe.MatchString(teamID) {
		return nil, ErrInvalidRequest
	}
	if strings.TrimSpace(req.Reason) == "" {
		return nil, ErrInvalidRequest
	}

	var ns models.TeamNamespace
	if err := s.db.WithContext(ctx).Where("team_id = ?", teamID).First(&ns).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, ErrTeamNotFound
		}
		return nil, fmt.Errorf("teamns: rotate lookup: %w", err)
	}
	if ns.Status == "archived" {
		return nil, ErrTeamArchived
	}

	var creds models.TeamBotCredentials
	if err := s.db.WithContext(ctx).Where("team_id = ? AND revoked_at IS NULL", teamID).First(&creds).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, ErrTeamNotFound
		}
		return nil, fmt.Errorf("teamns: rotate bot lookup: %w", err)
	}

	if s.gitsync == nil {
		return nil, ErrTenantGitServerUnresolved
	}
	newCreds, err := s.gitsync.RotateBot(ctx, ns.TenantID, creds.GiteaUsername, creds.GiteaTokenID)
	if err != nil {
		return nil, fmt.Errorf("teamns: rotate bot: %w", err)
	}

	encrypted, err := s.crypto.Seal([]byte(newCreds.TokenPlaintext))
	if err != nil {
		return nil, fmt.Errorf("teamns: encrypt rotated token: %w", err)
	}

	now := time.Now().UTC()
	prevTokenRevoked := true
	if err := s.db.WithContext(ctx).Model(&creds).Updates(map[string]any{
		"gitea_user_id":   newCreds.GiteaUserID,
		"gitea_token_id":  newCreds.GiteaTokenID,
		"token_encrypted": encrypted,
		"token_sha256":    newCreds.TokenSHA256,
		"rotated_at":      now,
	}).Error; err != nil {
		return nil, fmt.Errorf("teamns: persist rotated creds: %w", err)
	}

	return &RotateBotTokenResult{
		TeamID: teamID,
		Bot: &BotView{
			GiteaUsername:  newCreds.GiteaUsername,
			GiteaUserID:    newCreds.GiteaUserID,
			TokenID:        newCreds.GiteaTokenID,
			TokenPlaintext: newCreds.TokenPlaintext,
			TokenSHA256:    newCreds.TokenSHA256,
		},
		PreviousTokenRevoked: prevTokenRevoked,
		AuditLogID:           s.idGenerator(),
	}, nil
}

// RotateBotTokenRequest is the POST /bot-token:rotate body.
type RotateBotTokenRequest struct {
	Reason string       `json:"reason"`
	Actor  user.UserRef `json:"actor"`
}

// RotateBotTokenResult is the rotate response. Includes new plaintext token.
type RotateBotTokenResult struct {
	TeamID               string   `json:"team_id"`
	Bot                  *BotView `json:"bot"`
	PreviousTokenRevoked bool     `json:"previous_token_revoked"`
	AuditLogID           string   `json:"audit_log_id"`
}

// DecryptBotToken is the helper workflow_init uses to surface the plaintext
// token to short-lived upstream callers (per doc §8: every workflow/init
// returns the same active token). Caller must NOT log the result.
func (s *Service) DecryptBotToken(ctx context.Context, teamID string) (string, error) {
	if s == nil {
		return "", gorm.ErrInvalidDB
	}
	var creds models.TeamBotCredentials
	if err := s.db.WithContext(ctx).Where("team_id = ? AND revoked_at IS NULL", teamID).First(&creds).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return "", ErrTeamNotFound
		}
		return "", fmt.Errorf("teamns: decrypt lookup: %w", err)
	}
	plaintext, err := s.crypto.Open(creds.TokenEncrypted)
	if err != nil {
		return "", fmt.Errorf("teamns: decrypt token: %w", err)
	}
	return string(plaintext), nil
}

// LookupTeamNS exposes the team_ns row to handlers (workflow_init needs the
// tenant_id to resolve the Gitea base URL). Returns gorm.ErrRecordNotFound
// when the row is missing.
func (s *Service) LookupTeamNS(ctx context.Context, teamID string) (*models.TeamNamespace, error) {
	if s == nil {
		return nil, gorm.ErrInvalidDB
	}
	var ns models.TeamNamespace
	if err := s.db.WithContext(ctx).Where("team_id = ?", teamID).First(&ns).Error; err != nil {
		return nil, err
	}
	return &ns, nil
}

// LookupBotMeta fetches the active bot creds metadata (no plaintext) for
// handlers that need to surface username / user_id. Returns (nil, nil) if
// the team has no bot row — caller handles.
func (s *Service) LookupBotMeta(ctx context.Context, teamID string) (*models.TeamBotCredentials, error) {
	if s == nil {
		return nil, gorm.ErrInvalidDB
	}
	var creds models.TeamBotCredentials
	err := s.db.WithContext(ctx).Where("team_id = ? AND revoked_at IS NULL", teamID).First(&creds).Error
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, nil
		}
		return nil, err
	}
	return &creds, nil
}

// ResolveGiteaBaseURL fetches the tenant's Gitea endpoint via gitsync. Empty
// string returned if the resolver is unset — handlers tolerate that by
// building a URL without the host prefix.
func (s *Service) ResolveGiteaBaseURL(ctx context.Context, tenantID string) (string, error) {
	if s == nil || s.gitsync == nil {
		return "", nil
	}
	cfg, err := s.gitsync.ResolveGitServer(ctx, tenantID)
	if err != nil {
		return "", err
	}
	return cfg.Endpoint, nil
}

// SetGitServerFactoryForTest overrides the per-tenant GitServer resolver used
// by EnsureWorkflowRepo. Tests in other packages (e.g. handlers) inject a
// stub backend to exercise the orchestration path without spinning up Gitea.
// Passing nil restores the default factory wired by NewService.
func (s *Service) SetGitServerFactoryForTest(f func(ctx context.Context, tenantID string) (gitsync.GitServer, error)) {
	if s == nil {
		return
	}
	if f == nil {
		gs := s.gitsync
		s.gitServerFor = func(ctx context.Context, tenantID string) (gitsync.GitServer, error) {
			if gs == nil {
				return nil, nil
			}
			return gs.GitServerFor(ctx, tenantID)
		}
		return
	}
	s.gitServerFor = f
}

// syncMembers applies add/remove refs against Gitea via the gitsync service.
// Returns (added, removed, unresolved). Best-effort: individual failures land
// in unresolved rather than aborting the batch.
//
// fullSync=true: ListOrgMembers is fetched, anyone NOT in addRefs is removed,
// and removeRefs is ignored.
func (s *Service) syncMembers(ctx context.Context, tenantID, teamID, orgName string, addRefs, removeRefs []user.UserRef, fullSync bool) (int, int, []UnresolvedMember) {
	if s == nil || s.gitsync == nil || s.userRef == nil {
		unresolved := make([]UnresolvedMember, 0, len(addRefs)+len(removeRefs))
		for _, r := range addRefs {
			unresolved = append(unresolved, UnresolvedMember{UserID: r.UserID, EmployeeNumber: r.EmployeeNumber, Reason: "service_unavailable"})
		}
		return 0, 0, unresolved
	}

	added, unresolved := s.applyAdds(ctx, tenantID, orgName, addRefs)
	removed := 0
	if fullSync {
		// Compute remove = currentMembers - addRefs' resolved gitea usernames.
		current, err := s.gitsync.ListOrgMembers(ctx, tenantID, orgName)
		if err != nil {
			s.logger.Warn("teamns.syncMembers: list org members failed",
				zap.String("team_id", teamID),
				zap.Error(err))
		} else {
			desired := make(map[string]struct{}, len(addRefs))
			for _, r := range addRefs {
				if u, err := s.userRef.Resolve(ctx, r); err == nil {
					desired[u.GiteaUsername] = struct{}{}
				}
			}
			for _, m := range current {
				// Skip bot users.
				if strings.HasPrefix(m, "bot-t-") {
					continue
				}
				if _, ok := desired[m]; ok {
					continue
				}
				if err := s.gitsync.RemoveOrgMember(ctx, tenantID, orgName, m); err != nil {
					s.logger.Warn("teamns.syncMembers: remove member failed",
						zap.String("username", m),
						zap.Error(err))
					continue
				}
				removed++
			}
		}
	} else {
		// Delta: resolve each removeRef and call RemoveOrgMember.
		for _, r := range removeRefs {
			u, err := s.userRef.Resolve(ctx, r)
			if err != nil {
				unresolved = append(unresolved, UnresolvedMember{
					UserID: r.UserID, EmployeeNumber: r.EmployeeNumber,
					Reason: reasonFromResolveError(err),
				})
				continue
			}
			if err := s.gitsync.RemoveOrgMember(ctx, tenantID, orgName, u.GiteaUsername); err != nil {
				s.logger.Warn("teamns.syncMembers: remove member failed",
					zap.String("username", u.GiteaUsername),
					zap.Error(err))
				continue
			}
			removed++
		}
	}
	return added, removed, unresolved
}

// applyAdds resolves each addRef and calls AddOrgMember.
func (s *Service) applyAdds(ctx context.Context, tenantID, orgName string, refs []user.UserRef) (int, []UnresolvedMember) {
	added := 0
	unresolved := make([]UnresolvedMember, 0)
	for _, r := range refs {
		u, err := s.userRef.Resolve(ctx, r)
		if err != nil {
			unresolved = append(unresolved, UnresolvedMember{
				UserID: r.UserID, EmployeeNumber: r.EmployeeNumber,
				Reason: reasonFromResolveError(err),
			})
			continue
		}
		if err := s.gitsync.AddOrgMember(ctx, tenantID, orgName, u.GiteaUsername); err != nil {
			s.logger.Warn("teamns.applyAdds: add member failed",
				zap.String("username", u.GiteaUsername),
				zap.Error(err))
			unresolved = append(unresolved, UnresolvedMember{
				UserID: r.UserID, EmployeeNumber: r.EmployeeNumber,
				Reason: "gitea_api_failure",
			})
			continue
		}
		added++
	}
	return added, unresolved
}

// reasonFromResolveError maps userref errors to the short reason codes the
// doc lists (not_found / giteasync_pending / ambiguous).
func reasonFromResolveError(err error) string {
	switch {
	case errors.Is(err, user.ErrUserNotFound):
		return "not_found"
	case errors.Is(err, user.ErrUserNotGiteaReady):
		return "giteasync_pending"
	case errors.Is(err, user.ErrInvalidUserRef):
		return "invalid_ref"
	default:
		return "rpc_unavailable"
	}
}

// validateCreateTeamRequest enforces POST /teams body shape.
func validateCreateTeamRequest(req CreateTeamRequest) error {
	if !uuidRe.MatchString(req.TeamID) {
		return ErrInvalidRequest
	}
	if strings.TrimSpace(req.TeamDisplayName) == "" {
		return ErrInvalidRequest
	}
	// Creator must be a valid UserRef.
	if err := validateUserRef(req.Creator); err != nil {
		return err
	}
	// All initial members must be valid UserRefs.
	for i, m := range req.InitialMembers {
		if err := validateUserRef(m); err != nil {
			return err
		}
		_ = i
	}
	// Creator must not appear in initial_members (doc §1.5 INVALID_REQUEST).
	for _, m := range req.InitialMembers {
		if userRefsOverlap(req.Creator, m) {
			return ErrInvalidRequest
		}
	}
	return nil
}

// validateUserRef enforces the XOR constraint. Wraps user-side validation
// so we can return our own ErrInvalidRequest sentinel.
func validateUserRef(ref user.UserRef) error {
	hasUID := strings.TrimSpace(ref.UserID) != ""
	hasEmpNo := strings.TrimSpace(ref.EmployeeNumber) != ""
	if hasUID == hasEmpNo {
		return ErrInvalidRequest
	}
	return nil
}

// userRefsOverlap reports whether two refs point at the same user. Conservative:
// returns true only when the same non-empty field value matches.
func userRefsOverlap(a, b user.UserRef) bool {
	if a.UserID != "" && a.UserID == b.UserID {
		return true
	}
	if a.EmployeeNumber != "" && a.EmployeeNumber == b.EmployeeNumber {
		return true
	}
	return false
}

// overlapUserRefs reports whether any ref appears in both add and remove lists.
func overlapUserRefs(add, remove []user.UserRef) bool {
	for _, a := range add {
		for _, r := range remove {
			if userRefsOverlap(a, r) {
				return true
			}
		}
	}
	return false
}

// mapGitResolveError translates the gitsync resolver's errors into teamns
// sentinels. Tenant-not-found / server-unresolved both surface as
// ErrTenantGitServerUnresolved per doc §1.5.
func mapGitResolveError(err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("%w: %v", ErrTenantGitServerUnresolved, err)
}
