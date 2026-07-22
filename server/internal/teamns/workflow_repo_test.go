package teamns

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/costrict/costrict-web/server/internal/gitsync"
	"github.com/costrict/costrict-web/server/internal/models"
	"go.uber.org/zap"
	"gorm.io/gorm"
)

// fakeGitServer is a programmable GitServer stub. Each method records its
// invocation; behavior is driven by the per-method err / ret fields.
// Methods not exercised by a test can stay nil-valued (zero returns).
type fakeGitServer struct {
	// GetRepo
	getRepoCalls  int
	getRepoResult *gitsync.Repo
	getRepoErr    error
	// CreateRepo
	createRepoCalls  int
	createRepoResult *gitsync.Repo
	createRepoErr    error
	// WriteFile
	writeFileCalls int
	writeFileErr   error
	// ReadFile
	readFileCalls  int
	readFileResult []byte
	readFileErr    error
	// SetBranchProtection
	setBranchProtectionCalls []string // records rule_name of each call
	setBranchProtectionErr   error
	// GetBranch
	getBranchCalls  int
	getBranchResult *gitsync.Branch
	getBranchErr    error
	// CreateBranch
	createBranchCalls int
	createBranchErr   error
}

func (f *fakeGitServer) GetRepo(ctx context.Context, owner, name string) (*gitsync.Repo, error) {
	f.getRepoCalls++
	return f.getRepoResult, f.getRepoErr
}
func (f *fakeGitServer) CreateRepo(ctx context.Context, owner string, opts gitsync.CreateRepoOptions) (*gitsync.Repo, error) {
	f.createRepoCalls++
	if f.createRepoResult != nil {
		return f.createRepoResult, f.createRepoErr
	}
	if f.createRepoErr != nil {
		return nil, f.createRepoErr
	}
	return &gitsync.Repo{Name: opts.Name, FullName: owner + "/" + opts.Name, Private: opts.Private}, nil
}
func (f *fakeGitServer) WriteFile(ctx context.Context, owner, repo, branch, path string, content []byte, message string) error {
	f.writeFileCalls++
	return f.writeFileErr
}
func (f *fakeGitServer) ReadFile(ctx context.Context, owner, repo, branch, path string) ([]byte, error) {
	f.readFileCalls++
	return f.readFileResult, f.readFileErr
}
func (f *fakeGitServer) SetBranchProtection(ctx context.Context, owner, repo string, opts gitsync.BranchProtectionOptions) error {
	f.setBranchProtectionCalls = append(f.setBranchProtectionCalls, opts.RuleName)
	return f.setBranchProtectionErr
}
func (f *fakeGitServer) GetBranch(ctx context.Context, owner, repo, branch string) (*gitsync.Branch, error) {
	f.getBranchCalls++
	return f.getBranchResult, f.getBranchErr
}
func (f *fakeGitServer) CreateBranch(ctx context.Context, owner, repo, newBranch, fromRef string) error {
	f.createBranchCalls++
	return f.createBranchErr
}

// Stubs for the embedded interfaces (GiteaTeamMemberAPI + BotAccountAPI)
// — not exercised by EnsureWorkflowRepo tests but required for *fakeGitServer
// to satisfy gitsync.GitServer.
func (f *fakeGitServer) ListTeamMembers(ctx context.Context, teamID int64) ([]gitsync.GiteaMember, error) {
	return nil, nil
}
func (f *fakeGitServer) AddTeamMember(ctx context.Context, teamID int64, username string) error {
	return nil
}
func (f *fakeGitServer) RemoveTeamMember(ctx context.Context, teamID int64, username string) error {
	return nil
}
func (f *fakeGitServer) CreateUser(ctx context.Context, opts gitsync.CreateUserOptions) (*gitsync.GiteaUser, error) {
	return nil, nil
}
func (f *fakeGitServer) GetUserByName(ctx context.Context, username string) (*gitsync.GiteaUser, error) {
	return nil, nil
}
func (f *fakeGitServer) CreateUserToken(ctx context.Context, username string, opts gitsync.CreateUserTokenOptions) (*gitsync.GiteaToken, error) {
	return nil, nil
}
func (f *fakeGitServer) DeleteUserToken(ctx context.Context, username string, tokenID int64) error {
	return nil
}
func (f *fakeGitServer) CreateOrg(ctx context.Context, opts gitsync.CreateOrgOptions) (*gitsync.GiteaOrg, error) {
	return nil, nil
}
func (f *fakeGitServer) GetOrgByName(ctx context.Context, name string) (*gitsync.GiteaOrg, error) {
	return nil, nil
}
func (f *fakeGitServer) ListOrgTeams(ctx context.Context, org string) ([]gitsync.GiteaTeam, error) {
	return nil, nil
}
func (f *fakeGitServer) UpdateOrg(ctx context.Context, org string, opts gitsync.UpdateOrgOptions) error {
	return nil
}
func (f *fakeGitServer) ListOrgMembers(ctx context.Context, org string) ([]string, error) {
	return nil, nil
}
func (f *fakeGitServer) AddOrgMember(ctx context.Context, org, username string) error    { return nil }
func (f *fakeGitServer) RemoveOrgMember(ctx context.Context, org, username string) error { return nil }

var _ gitsync.GitServer = (*fakeGitServer)(nil)

// seedTeamNS inserts one team_ns row for the test team. Returns the row's
// tenantID so tests can assert context propagation if needed.
func seedTeamNS(t *testing.T, db *gorm.DB, teamID, tenantID string) {
	t.Helper()
	if len(teamID) < 8 {
		t.Fatalf("test teamID too short: %q", teamID)
	}
	short := teamID[:8]
	now := time.Now().UTC()
	ns := &models.TeamNamespace{
		TeamID:          teamID,
		TenantID:        tenantID,
		TeamDisplayName: "Test Team",
		TeamNSOrg:       "t-" + short,
		TeamShort:       short,
		GitServerID:     "srv-test",
		Status:          "active",
		CreatedAt:       now,
		UpdatedAt:       now,
	}
	if err := db.Create(ns).Error; err != nil {
		t.Fatalf("seed team_ns: %v", err)
	}
}

// validUUID returns a 36-char UUID string (8-4-4-4-12) whose first 8 hex
// chars are the supplied prefix. Pass distinct prefixes per test to avoid
// collisions when tests share a DB. The prefix must already be hex.
func validUUID(prefix string) string {
	if len(prefix) < 8 {
		prefix = prefix + "00000000"[:8-len(prefix)]
	}
	return fmt.Sprintf("%s-4123-4123-8123-aaaabbbbcccc", prefix[:8])
}

// newSvcWithFakeGit builds a Service with the fakeGitServer injected via
// the gitServerFor hook. Returns the Service + the fake for assertions.
func newSvcWithFakeGit(t *testing.T, fake *fakeGitServer) (*Service, *gorm.DB) {
	t.Helper()
	db := setupDB(t)
	svc := NewService(db, nil, nil, mustAES(t), zap.NewNop())
	svc.gitServerFor = func(ctx context.Context, tenantID string) (gitsync.GitServer, error) {
		return fake, nil
	}
	return svc, db
}

func TestEnsureWorkflowRepo_HappyPath_AllCreated(t *testing.T) {
	fake := &fakeGitServer{
		// Repo doesn't exist → ensure created.
		getRepoResult: nil,
		// File doesn't exist → ensure written.
		readFileResult: nil,
		// Branch doesn't exist → ensure created.
		getBranchResult: nil,
	}
	svc, db := newSvcWithFakeGit(t, fake)
	teamID := validUUID("abcdef01")
	seedTeamNS(t, db, teamID, "tenant-1")

	res, err := svc.EnsureWorkflowRepo(context.Background(), teamID,
		"my-wf", `{"version":1}`, validUUID("11111111"))
	if err != nil {
		t.Fatalf("EnsureWorkflowRepo: %v", err)
	}
	if !res.TypeRepoCreated {
		t.Error("expected TypeRepoCreated=true")
	}
	if !res.InstanceBranchCreated {
		t.Error("expected InstanceBranchCreated=true")
	}
	if !res.BranchProtectionSet {
		t.Error("expected BranchProtectionSet=true")
	}
	if res.SnapshotHash == "" {
		t.Error("expected non-empty SnapshotHash")
	}
	// Two protection calls: main + inst-* glob.
	if len(fake.setBranchProtectionCalls) != 2 {
		t.Errorf("expected 2 SetBranchProtection calls, got %d", len(fake.setBranchProtectionCalls))
	}
	if fake.writeFileCalls != 1 {
		t.Errorf("expected 1 WriteFile call, got %d", fake.writeFileCalls)
	}
}

func TestEnsureWorkflowRepo_Idempotent_AllExist(t *testing.T) {
	existingContent := `{"version":1}`
	fake := &fakeGitServer{
		getRepoResult:   &gitsync.Repo{ID: 42, Name: "wf-my-wf", FullName: "t-abcdef01/wf-my-wf"},
		readFileResult:  []byte(existingContent),
		getBranchResult: &gitsync.Branch{Name: "inst-11111111", CommitSHA: "abc"},
	}
	svc, db := newSvcWithFakeGit(t, fake)
	teamID := validUUID("abcdef01")
	seedTeamNS(t, db, teamID, "tenant-1")

	res, err := svc.EnsureWorkflowRepo(context.Background(), teamID,
		"my-wf", existingContent, validUUID("11111111"))
	if err != nil {
		t.Fatalf("EnsureWorkflowRepo idempotent: %v", err)
	}
	if res.TypeRepoCreated {
		t.Error("expected TypeRepoCreated=false (existed)")
	}
	if res.InstanceBranchCreated {
		t.Error("expected InstanceBranchCreated=false (existed)")
	}
	if fake.createRepoCalls != 0 {
		t.Errorf("expected 0 CreateRepo calls, got %d", fake.createRepoCalls)
	}
	if fake.writeFileCalls != 0 {
		t.Errorf("expected 0 WriteFile calls (hashes match), got %d", fake.writeFileCalls)
	}
	if fake.createBranchCalls != 0 {
		t.Errorf("expected 0 CreateBranch calls, got %d", fake.createBranchCalls)
	}
}

func TestEnsureWorkflowRepo_DefinitionDrift_Returns409(t *testing.T) {
	fake := &fakeGitServer{
		getRepoResult:  &gitsync.Repo{ID: 42, Name: "wf-my-wf", FullName: "t-abcdef01/wf-my-wf"},
		readFileResult: []byte(`{"version":"OLD"}`), // different from input
	}
	svc, db := newSvcWithFakeGit(t, fake)
	teamID := validUUID("abcdef01")
	seedTeamNS(t, db, teamID, "tenant-1")

	_, err := svc.EnsureWorkflowRepo(context.Background(), teamID,
		"my-wf", `{"version":"NEW"}`, validUUID("11111111"))
	if !errors.Is(err, ErrDefinitionDrift) {
		t.Fatalf("expected ErrDefinitionDrift, got %v", err)
	}
	// Should not have written or set protection on drift.
	if fake.writeFileCalls != 0 {
		t.Errorf("expected no WriteFile call on drift, got %d", fake.writeFileCalls)
	}
	if len(fake.setBranchProtectionCalls) != 0 {
		t.Errorf("expected no SetBranchProtection call on drift, got %d", len(fake.setBranchProtectionCalls))
	}
}

func TestEnsureWorkflowRepo_BranchProtectionAlreadyExists_Tolerated(t *testing.T) {
	// Gitea returns 409 on already-existing rule — our impl converts to
	// ErrGiteaUsernameTaken; applyBranchProtection must swallow it.
	fake := &fakeGitServer{
		getRepoResult:          &gitsync.Repo{ID: 42, Name: "wf-my-wf"},
		readFileResult:         nil, // missing → will write
		getBranchResult:        nil, // missing → will create
		setBranchProtectionErr: gitsync.ErrGiteaUsernameTaken,
	}
	svc, db := newSvcWithFakeGit(t, fake)
	teamID := validUUID("abcdef01")
	seedTeamNS(t, db, teamID, "tenant-1")

	_, err := svc.EnsureWorkflowRepo(context.Background(), teamID,
		"my-wf", `{"v":1}`, validUUID("11111111"))
	if err != nil {
		t.Fatalf("expected nil error on already-exists protection, got %v", err)
	}
}

func TestEnsureWorkflowRepo_GitServerNil_ReturnsErrTenantGitServerUnresolved(t *testing.T) {
	db := setupDB(t)
	svc := NewService(db, nil, nil, mustAES(t), zap.NewNop())
	svc.gitServerFor = func(ctx context.Context, tenantID string) (gitsync.GitServer, error) {
		return nil, nil
	}
	teamID := validUUID("abcdef01")
	seedTeamNS(t, db, teamID, "tenant-1")

	_, err := svc.EnsureWorkflowRepo(context.Background(), teamID, "my-wf", `{}`, validUUID("11111111"))
	if !errors.Is(err, ErrTenantGitServerUnresolved) {
		t.Fatalf("expected ErrTenantGitServerUnresolved, got %v", err)
	}
}

func TestEnsureWorkflowRepo_TeamNotFound_Returns404(t *testing.T) {
	fake := &fakeGitServer{}
	svc, _ := newSvcWithFakeGit(t, fake)
	// No seed — team_ns row missing.
	_, err := svc.EnsureWorkflowRepo(context.Background(),
		validUUID("deadbeef"), "my-wf", `{}`, validUUID("11111111"))
	if !errors.Is(err, ErrTeamNotFound) {
		t.Fatalf("expected ErrTeamNotFound, got %v", err)
	}
}

func TestEnsureWorkflowRepo_InvalidSlug_Returns400(t *testing.T) {
	fake := &fakeGitServer{}
	svc, db := newSvcWithFakeGit(t, fake)
	teamID := validUUID("abcdef01")
	seedTeamNS(t, db, teamID, "tenant-1")

	// Slug with leading dot — workflow.EscapeDefSlug should still produce
	// a valid path, but passing an empty slug is an explicit invalid input.
	_, err := svc.EnsureWorkflowRepo(context.Background(), teamID, "", `{}`, validUUID("11111111"))
	if !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("expected ErrInvalidRequest for empty slug, got %v", err)
	}
}

func TestEnsureWorkflowRepo_GetRepoError_Propagates(t *testing.T) {
	fake := &fakeGitServer{
		getRepoErr: errors.New("gitea: internal server error (status=500)"),
	}
	svc, db := newSvcWithFakeGit(t, fake)
	teamID := validUUID("abcdef01")
	seedTeamNS(t, db, teamID, "tenant-1")

	_, err := svc.EnsureWorkflowRepo(context.Background(), teamID, "my-wf", `{}`, validUUID("11111111"))
	if !errors.Is(err, ErrWorkflowRepoProvisioning) {
		t.Fatalf("expected ErrWorkflowRepoProvisioning, got %v", err)
	}
}
