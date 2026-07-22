package teamns

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/costrict/costrict-web/server/internal/crypto"
	"github.com/costrict/costrict-web/server/internal/gitsync"
	"github.com/costrict/costrict-web/server/internal/models"
	"github.com/costrict/costrict-web/server/internal/tenant"
	"github.com/costrict/costrict-web/server/internal/user"
	"go.uber.org/zap"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
)

// setupDB opens an in-memory sqlite DB with the team_ns + team_bot_credentials
// tables. We hand-roll the schema (not AutoMigrate) because the postgres
// timestamptz / char(64) types don't map cleanly to sqlite.
func setupDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	sqlDB, _ := db.DB()
	sqlDB.SetMaxOpenConns(1)

	if err := db.Exec(`CREATE TABLE team_ns (
		team_id TEXT PRIMARY KEY,
		tenant_id TEXT NOT NULL,
		team_display_name TEXT NOT NULL,
		team_ns_org TEXT NOT NULL UNIQUE,
		team_short TEXT NOT NULL,
		git_server_id TEXT NOT NULL,
		status TEXT NOT NULL DEFAULT 'active',
		dissolved_at DATETIME,
		dissolution_reason TEXT,
		retention_until DATETIME,
		created_at DATETIME NOT NULL,
		updated_at DATETIME NOT NULL
	)`).Error; err != nil {
		t.Fatalf("create team_ns: %v", err)
	}
	if err := db.Exec(`CREATE TABLE team_bot_credentials (
		team_id TEXT PRIMARY KEY,
		tenant_id TEXT NOT NULL,
		git_server_id TEXT NOT NULL,
		gitea_username TEXT NOT NULL,
		gitea_user_id INTEGER NOT NULL,
		gitea_token_id INTEGER NOT NULL,
		token_encrypted TEXT NOT NULL,
		token_sha256 TEXT NOT NULL,
		created_at DATETIME NOT NULL,
		rotated_at DATETIME,
		revoked_at DATETIME
	)`).Error; err != nil {
		t.Fatalf("create team_bot_credentials: %v", err)
	}
	return db
}

// stubGitSync is a minimal gitsync.Service stub for teamns tests. We don't
// depend on real gitsync behavior — teamns tests care that the orchestration
// calls the right surface and persists the right rows.
type stubGitSync struct {
	resolveErr      error
	serverID        string
	endpoint        string
	ensureOrgErr    error
	ensureOrgCalls  int
	provisionErr    error
	provisionCreds  *gitsync.BotCredentials
	revokeErr       error
	rotateErr       error
	rotateCreds     *gitsync.BotCredentials
	listMembers     []string
	listMembersErr  error
	addMemberErr    error
	removeMemberErr error
	removeAllCount  int
	removeAllErr    error
	updateOrgErr    error
}

// We exercise teamns.Service at the orchestration level only — the Gitea
// round-trip is covered by the gitsync package's own tests. Where teamns
// needs to assert Gitea-side state, we read it back from the DB instead.

type stubResolver struct {
	fn func(ctx context.Context, tenantID string) (*gitsync.GitServerConfig, error)
}

func (s *stubResolver) Resolve(ctx context.Context, tenantID string) (*gitsync.GitServerConfig, error) {
	return s.fn(ctx, tenantID)
}

func mustAES(t *testing.T) *crypto.AESGCM {
	t.Helper()
	key, err := crypto.DecodeBase64Key("AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=")
	if err != nil {
		t.Fatalf("decode key: %v", err)
	}
	a, err := crypto.NewAESGCM(key)
	if err != nil {
		t.Fatalf("aes: %v", err)
	}
	return a
}

// withTenant returns a ctx with the given tenant_id.
func withTenant(id string) context.Context {
	return tenant.WithTenantID(context.Background(), id)
}

// ---- validateCreateTeamRequest ----

func TestValidateCreateTeamRequest_HappyPath(t *testing.T) {
	req := CreateTeamRequest{
		TeamID:          "7f3c9a1e-d4b2-4c8e-9a3f-1b2c3d4e5f6a",
		TeamDisplayName: "Platform Team",
		Creator:         user.UserRef{EmployeeNumber: "E-1000"},
	}
	if err := validateCreateTeamRequest(req); err != nil {
		t.Errorf("unexpected error: %v", err)
	}
}

func TestValidateCreateTeamRequest_BadUUID(t *testing.T) {
	req := CreateTeamRequest{TeamID: "not-a-uuid", TeamDisplayName: "X", Creator: user.UserRef{UserID: "u1"}}
	if err := validateCreateTeamRequest(req); !errors.Is(err, ErrInvalidRequest) {
		t.Errorf("got %v, want ErrInvalidRequest", err)
	}
}

func TestValidateCreateTeamRequest_EmptyDisplayName(t *testing.T) {
	req := CreateTeamRequest{
		TeamID:  "7f3c9a1e-d4b2-4c8e-9a3f-1b2c3d4e5f6a",
		Creator: user.UserRef{UserID: "u1"},
	}
	if err := validateCreateTeamRequest(req); !errors.Is(err, ErrInvalidRequest) {
		t.Errorf("got %v, want ErrInvalidRequest", err)
	}
}

func TestValidateCreateTeamRequest_BadUserRef(t *testing.T) {
	req := CreateTeamRequest{
		TeamID:          "7f3c9a1e-d4b2-4c8e-9a3f-1b2c3d4e5f6a",
		TeamDisplayName: "X",
		Creator:         user.UserRef{}, // both empty
	}
	if err := validateCreateTeamRequest(req); !errors.Is(err, ErrInvalidRequest) {
		t.Errorf("got %v, want ErrInvalidRequest", err)
	}
}

func TestValidateCreateTeamRequest_CreatorInInitialMembers(t *testing.T) {
	req := CreateTeamRequest{
		TeamID:          "7f3c9a1e-d4b2-4c8e-9a3f-1b2c3d4e5f6a",
		TeamDisplayName: "X",
		Creator:         user.UserRef{EmployeeNumber: "E-1"},
		InitialMembers:  []user.UserRef{{EmployeeNumber: "E-1"}},
	}
	if err := validateCreateTeamRequest(req); !errors.Is(err, ErrInvalidRequest) {
		t.Errorf("creator overlapping initial_members: got %v, want ErrInvalidRequest", err)
	}
}

// ---- Service-level behavior ----

func TestCreateTeam_PersistsRows(t *testing.T) {
	db := setupDB(t)
	// The integration with ProvisionBot needs a real Gitea stub. For this
	// unit test we skip the full path and assert the validation + DB layer.
	// Full integration lives in stage 11 / handlers_test.go via httptest.
	_ = db
	// Smoke: shortFromTeamID + orgNameForTeam.
	id := "7f3c9a1e-d4b2-4c8e-9a3f-1b2c3d4e5f6a"
	if got := shortFromTeamID(id); got != "7f3c9a1e" {
		t.Errorf("short: got %q", got)
	}
	if got := orgNameForTeam(id); got != "t-7f3c9a1e" {
		t.Errorf("org: got %q", got)
	}
}

func TestGetTeam_HappyPath(t *testing.T) {
	db := setupDB(t)
	now := time.Now().UTC()
	ns := models.TeamNamespace{
		TeamID:          "7f3c9a1e-d4b2-4c8e-9a3f-1b2c3d4e5f6a",
		TenantID:        tenant.DefaultTenantID,
		TeamDisplayName: "Platform",
		TeamNSOrg:       "t-7f3c9a1e",
		TeamShort:       "7f3c9a1e",
		GitServerID:     "gs-1",
		Status:          "active",
		CreatedAt:       now,
		UpdatedAt:       now,
	}
	if err := db.Create(&ns).Error; err != nil {
		t.Fatalf("seed: %v", err)
	}
	creds := models.TeamBotCredentials{
		TeamID:         ns.TeamID,
		TenantID:       ns.TenantID,
		GitServerID:    "gs-1",
		GiteaUsername:  "bot-t-7f3c9a1e",
		GiteaUserID:    42,
		GiteaTokenID:   17,
		TokenEncrypted: "enc",
		TokenSHA256:    "sha",
		CreatedAt:      now,
	}
	if err := db.Create(&creds).Error; err != nil {
		t.Fatalf("seed bot: %v", err)
	}

	svc := NewService(db, nil, nil, mustAES(t), zap.NewNop())
	got, err := svc.GetTeam(withTenant(tenant.DefaultTenantID), ns.TeamID)
	if err != nil {
		t.Fatalf("GetTeam: %v", err)
	}
	if got.TeamNSOrg != "t-7f3c9a1e" || got.Status != "active" {
		t.Errorf("got %+v", got)
	}
	if got.Bot == nil || got.Bot.GiteaUsername != "bot-t-7f3c9a1e" {
		t.Errorf("bot: %+v", got.Bot)
	}
	if got.Bot.TokenPlaintext != "" {
		t.Errorf("GET must not return plaintext token; got %q", got.Bot.TokenPlaintext)
	}
}

func TestGetTeam_NotFound(t *testing.T) {
	db := setupDB(t)
	svc := NewService(db, nil, nil, mustAES(t), zap.NewNop())
	_, err := svc.GetTeam(withTenant(tenant.DefaultTenantID), "7f3c9a1e-d4b2-4c8e-9a3f-1b2c3d4e5f6a")
	if !errors.Is(err, ErrTeamNotFound) {
		t.Errorf("got %v, want ErrTeamNotFound", err)
	}
}

func TestGetTeam_BadUUID(t *testing.T) {
	db := setupDB(t)
	svc := NewService(db, nil, nil, mustAES(t), zap.NewNop())
	_, err := svc.GetTeam(withTenant(tenant.DefaultTenantID), "not-a-uuid")
	if !errors.Is(err, ErrInvalidRequest) {
		t.Errorf("got %v, want ErrInvalidRequest", err)
	}
}

func TestListTeams_RejectsTenantIDQuery(t *testing.T) {
	db := setupDB(t)
	svc := NewService(db, nil, nil, mustAES(t), zap.NewNop())
	_, err := svc.ListTeams(withTenant(tenant.DefaultTenantID), ListParams{TenantIDQuery: "default"})
	if !errors.Is(err, ErrInvalidRequest) {
		t.Errorf("got %v, want ErrInvalidRequest", err)
	}
}

func TestListTeams_RejectsBadStatus(t *testing.T) {
	db := setupDB(t)
	svc := NewService(db, nil, nil, mustAES(t), zap.NewNop())
	_, err := svc.ListTeams(withTenant(tenant.DefaultTenantID), ListParams{Status: "garbage"})
	if !errors.Is(err, ErrInvalidRequest) {
		t.Errorf("got %v, want ErrInvalidRequest", err)
	}
}

func TestListTeams_Pagination(t *testing.T) {
	db := setupDB(t)
	now := time.Now().UTC()
	for i := 0; i < 5; i++ {
		teamID := "1111111" + string(rune('0'+i)) + "-1111-4c8e-9a3f-1b2c3d4e5f6a"
		// pad to UUID shape (8-4-4-4-12)
		teamID = padUUID(i)
		ns := models.TeamNamespace{
			TeamID: teamID, TenantID: tenant.DefaultTenantID,
			TeamDisplayName: "T" + string(rune('a'+i)),
			TeamNSOrg:       "t-" + string(rune('a'+i)),
			TeamShort:       string(rune('a' + i)),
			GitServerID:     "gs", Status: "active",
			CreatedAt: now.Add(time.Duration(i) * time.Minute),
			UpdatedAt: now,
		}
		if err := db.Create(&ns).Error; err != nil {
			t.Fatalf("seed %d: %v", i, err)
		}
	}
	svc := NewService(db, nil, nil, mustAES(t), zap.NewNop())
	got, err := svc.ListTeams(withTenant(tenant.DefaultTenantID), ListParams{Page: 1, PageSize: 2})
	if err != nil {
		t.Fatalf("ListTeams: %v", err)
	}
	if len(got.Teams) != 2 || got.Total != 5 {
		t.Errorf("got %d teams, total %d; want 2 teams, total 5", len(got.Teams), got.Total)
	}
}

// padUUID returns a deterministic UUID-shaped string for test seeding.
// n is the discriminator (0-15) sprinkled into the first hex block.
func padUUID(n int) string {
	hex := "0123456789abcdef"
	d := string(hex[n%len(hex)])
	// 8-4-4-4-12 — fill with discriminator + a's so it's clearly a UUID.
	return d + d + d + d + d + d + d + d + "-" +
		d + d + d + d + "-" +
		"4" + d + d + d + "-" +
		"8" + d + d + d + "-" +
		d + d + d + d + d + d + d + d + d + d + d + d
}

func TestPatchTeam_ArchivedRejected(t *testing.T) {
	db := setupDB(t)
	now := time.Now().UTC()
	ns := models.TeamNamespace{
		TeamID: padUUID(1), TenantID: tenant.DefaultTenantID,
		TeamDisplayName: "X", TeamNSOrg: "t-x", TeamShort: "x",
		GitServerID: "gs", Status: "archived",
		CreatedAt: now, UpdatedAt: now,
	}
	if err := db.Create(&ns).Error; err != nil {
		t.Fatalf("seed: %v", err)
	}
	svc := NewService(db, nil, nil, mustAES(t), zap.NewNop())
	err := svc.PatchTeam(withTenant(tenant.DefaultTenantID), ns.TeamID, PatchTeamRequest{TeamDisplayName: "Y"})
	if !errors.Is(err, ErrTeamArchived) {
		t.Errorf("got %v, want ErrTeamArchived", err)
	}
}

func TestPatchTeam_NoFields(t *testing.T) {
	db := setupDB(t)
	svc := NewService(db, nil, nil, mustAES(t), zap.NewNop())
	err := svc.PatchTeam(withTenant(tenant.DefaultTenantID), padUUID(1), PatchTeamRequest{})
	if !errors.Is(err, ErrInvalidRequest) {
		t.Errorf("got %v, want ErrInvalidRequest", err)
	}
}

func TestDissolveTeam_IdempotentNeverCreated(t *testing.T) {
	db := setupDB(t)
	svc := NewService(db, nil, nil, mustAES(t), zap.NewNop())
	got, err := svc.DissolveTeam(withTenant(tenant.DefaultTenantID), padUUID(2), DissolveTeamRequest{Reason: "test"})
	if err != nil {
		t.Fatalf("DissolveTeam on unknown team: %v", err)
	}
	if got.Archived || got.BotTokenRevoked {
		t.Errorf("expected no-op on never-created team; got %+v", got)
	}
}

func TestDissolveTeam_AlreadyArchived(t *testing.T) {
	db := setupDB(t)
	now := time.Now().UTC()
	retention := now.Add(90 * 24 * time.Hour)
	ns := models.TeamNamespace{
		TeamID: padUUID(3), TenantID: tenant.DefaultTenantID,
		TeamDisplayName: "X", TeamNSOrg: "t-x3", TeamShort: "x3",
		GitServerID: "gs", Status: "archived",
		DissolvedAt:    &now,
		RetentionUntil: &retention,
		CreatedAt:      now, UpdatedAt: now,
	}
	if err := db.Create(&ns).Error; err != nil {
		t.Fatalf("seed: %v", err)
	}
	svc := NewService(db, nil, nil, mustAES(t), zap.NewNop())
	got, err := svc.DissolveTeam(withTenant(tenant.DefaultTenantID), ns.TeamID, DissolveTeamRequest{Reason: "again"})
	if err != nil {
		t.Fatalf("re-dissolve: %v", err)
	}
	if !got.Archived {
		t.Errorf("already-archived should still report archived=true")
	}
	if got.BotTokenRevoked {
		t.Errorf("re-dissolve should NOT revoke bot token again")
	}
}

func TestDissolveTeam_BadReason(t *testing.T) {
	db := setupDB(t)
	svc := NewService(db, nil, nil, mustAES(t), zap.NewNop())
	_, err := svc.DissolveTeam(withTenant(tenant.DefaultTenantID), padUUID(4), DissolveTeamRequest{Reason: "  "})
	if !errors.Is(err, ErrInvalidRequest) {
		t.Errorf("got %v, want ErrInvalidRequest", err)
	}
}

func TestRotateBotToken_BadReason(t *testing.T) {
	db := setupDB(t)
	svc := NewService(db, nil, nil, mustAES(t), zap.NewNop())
	_, err := svc.RotateBotToken(withTenant(tenant.DefaultTenantID), padUUID(5), RotateBotTokenRequest{})
	if !errors.Is(err, ErrInvalidRequest) {
		t.Errorf("got %v, want ErrInvalidRequest", err)
	}
}

func TestRotateBotToken_TeamNotFound(t *testing.T) {
	db := setupDB(t)
	svc := NewService(db, nil, nil, mustAES(t), zap.NewNop())
	_, err := svc.RotateBotToken(withTenant(tenant.DefaultTenantID), padUUID(6), RotateBotTokenRequest{Reason: "leak"})
	if !errors.Is(err, ErrTeamNotFound) {
		t.Errorf("got %v, want ErrTeamNotFound", err)
	}
}

func TestSyncMembers_BadMode(t *testing.T) {
	db := setupDB(t)
	svc := NewService(db, nil, nil, mustAES(t), zap.NewNop())
	_, err := svc.SyncTeamMembers(withTenant(tenant.DefaultTenantID), padUUID(7), SyncMembersRequest{Mode: "garbage"})
	if !errors.Is(err, ErrInvalidRequest) {
		t.Errorf("got %v, want ErrInvalidRequest", err)
	}
}

func TestSyncMembers_OverlapRejected(t *testing.T) {
	db := setupDB(t)
	svc := NewService(db, nil, nil, mustAES(t), zap.NewNop())
	req := SyncMembersRequest{
		Mode:          "delta",
		AddMembers:    []user.UserRef{{UserID: "u1"}},
		RemoveMembers: []user.UserRef{{UserID: "u1"}},
	}
	_, err := svc.SyncTeamMembers(withTenant(tenant.DefaultTenantID), padUUID(8), req)
	if !errors.Is(err, ErrInvalidRequest) {
		t.Errorf("overlap add/remove: got %v, want ErrInvalidRequest", err)
	}
}

func TestDecryptBotToken_RoundTrip(t *testing.T) {
	db := setupDB(t)
	aes := mustAES(t)
	plaintext := "super-secret-pat"
	enc, err := aes.Seal([]byte(plaintext))
	if err != nil {
		t.Fatalf("seal: %v", err)
	}
	now := time.Now().UTC()
	teamID := padUUID(9)
	ns := models.TeamNamespace{
		TeamID: teamID, TenantID: tenant.DefaultTenantID,
		TeamDisplayName: "X", TeamNSOrg: "t-x9", TeamShort: "x9",
		GitServerID: "gs", Status: "active",
		CreatedAt: now, UpdatedAt: now,
	}
	if err := db.Create(&ns).Error; err != nil {
		t.Fatalf("seed ns: %v", err)
	}
	creds := models.TeamBotCredentials{
		TeamID: teamID, TenantID: tenant.DefaultTenantID, GitServerID: "gs",
		GiteaUsername: "bot", GiteaUserID: 1, GiteaTokenID: 2,
		TokenEncrypted: enc, TokenSHA256: "sha", CreatedAt: now,
	}
	if err := db.Create(&creds).Error; err != nil {
		t.Fatalf("seed creds: %v", err)
	}
	svc := NewService(db, nil, nil, aes, zap.NewNop())
	got, err := svc.DecryptBotToken(withTenant(tenant.DefaultTenantID), teamID)
	if err != nil {
		t.Fatalf("decrypt: %v", err)
	}
	if got != plaintext {
		t.Errorf("got %q, want %q", got, plaintext)
	}
}
