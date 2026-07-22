package teamns

import (
	"context"
	"errors"
	"testing"

	"github.com/costrict/costrict-web/server/internal/gitsync"
	"go.uber.org/zap"
	"gorm.io/gorm"
)

// kbTeamID is a stable UUID for kb_repo tests; first 8 hex chars = "f1e2d3c4".
var kbTeamID = validUUID("f1e2d3c4")

func TestEnsureKBRepo_HappyPath_AllCreated(t *testing.T) {
	fake := &fakeGitServer{
		// Repo doesn't exist → ensure created.
		getRepoResult: nil,
	}
	svc, db := newSvcWithFakeGit(t, fake)
	seedTeamNS(t, db, kbTeamID, "tenant-1")

	res, err := svc.EnsureKBRepo(context.Background(), kbTeamID,
		"https://github.com/ownerA/proj.git")
	if err != nil {
		t.Fatalf("EnsureKBRepo: %v", err)
	}
	if !res.KbRepoCreated {
		t.Error("expected KbRepoCreated=true")
	}
	if !res.BranchProtectionSet {
		t.Error("expected BranchProtectionSet=true")
	}
	wantPath := "t-f1e2d3c4/kb-github.com__ownera__proj"
	if res.KbRepoPath != wantPath {
		t.Errorf("KbRepoPath: got %q, want %q", res.KbRepoPath, wantPath)
	}
	if fake.createRepoCalls != 1 {
		t.Errorf("createRepoCalls: got %d, want 1", fake.createRepoCalls)
	}
	// Only main protected (no inst-* glob for kb).
	if len(fake.setBranchProtectionCalls) != 1 || fake.setBranchProtectionCalls[0] != "main" {
		t.Errorf("setBranchProtectionCalls: got %v, want [main]", fake.setBranchProtectionCalls)
	}
}

func TestEnsureKBRepo_Idempotent_RepoAlreadyExists(t *testing.T) {
	fake := &fakeGitServer{
		// Repo already exists → no create.
		getRepoResult: &gitsync.Repo{Name: "kb-github.com__ownera__proj"},
	}
	svc, db := newSvcWithFakeGit(t, fake)
	seedTeamNS(t, db, kbTeamID, "tenant-1")

	res, err := svc.EnsureKBRepo(context.Background(), kbTeamID,
		"https://github.com/ownerA/proj.git")
	if err != nil {
		t.Fatalf("EnsureKBRepo: %v", err)
	}
	if res.KbRepoCreated {
		t.Error("expected KbRepoCreated=false on idempotent re-run")
	}
	if fake.createRepoCalls != 0 {
		t.Errorf("createRepoCalls: got %d, want 0 (idempotent)", fake.createRepoCalls)
	}
	// Branch protection is re-applied (idempotent config).
	if len(fake.setBranchProtectionCalls) != 1 {
		t.Errorf("setBranchProtectionCalls: got %d, want 1 (re-applied)", len(fake.setBranchProtectionCalls))
	}
}

func TestEnsureKBRepo_BranchProtectionAlreadyExists(t *testing.T) {
	// Gitea returns "username taken" 409 when protection rule already exists.
	// applyBranchProtection maps this to nil — idempotent success.
	fake := &fakeGitServer{
		getRepoResult:          nil,
		setBranchProtectionErr: gitsync.ErrGiteaUsernameTaken,
	}
	svc, db := newSvcWithFakeGit(t, fake)
	seedTeamNS(t, db, kbTeamID, "tenant-1")

	res, err := svc.EnsureKBRepo(context.Background(), kbTeamID,
		"https://github.com/ownerA/proj.git")
	if err != nil {
		t.Fatalf("EnsureKBRepo should tolerate already-exists protection: %v", err)
	}
	if !res.BranchProtectionSet {
		t.Error("expected BranchProtectionSet=true even on already-exists")
	}
}

func TestEnsureKBRepo_BranchProtectionGenericError(t *testing.T) {
	fake := &fakeGitServer{
		setBranchProtectionErr: errors.New("gitea 500"),
	}
	svc, db := newSvcWithFakeGit(t, fake)
	seedTeamNS(t, db, kbTeamID, "tenant-1")

	_, err := svc.EnsureKBRepo(context.Background(), kbTeamID,
		"https://github.com/ownerA/proj.git")
	if !errors.Is(err, ErrKBRepoProvisioning) {
		t.Errorf("expected ErrKBRepoProvisioning wrap, got %v", err)
	}
}

func TestEnsureKBRepo_CreateRepoError(t *testing.T) {
	fake := &fakeGitServer{
		getRepoResult: nil,
		createRepoErr: errors.New("gitea 500"),
	}
	svc, db := newSvcWithFakeGit(t, fake)
	seedTeamNS(t, db, kbTeamID, "tenant-1")

	_, err := svc.EnsureKBRepo(context.Background(), kbTeamID,
		"https://github.com/ownerA/proj.git")
	if !errors.Is(err, ErrKBRepoProvisioning) {
		t.Errorf("expected ErrKBRepoProvisioning wrap, got %v", err)
	}
}

func TestEnsureKBRepo_GetRepoError(t *testing.T) {
	fake := &fakeGitServer{
		getRepoErr: errors.New("gitea 500"),
	}
	svc, db := newSvcWithFakeGit(t, fake)
	seedTeamNS(t, db, kbTeamID, "tenant-1")

	_, err := svc.EnsureKBRepo(context.Background(), kbTeamID,
		"https://github.com/ownerA/proj.git")
	if !errors.Is(err, ErrKBRepoProvisioning) {
		t.Errorf("expected ErrKBRepoProvisioning wrap, got %v", err)
	}
}

func TestEnsureKBRepo_TeamNSMissing_404(t *testing.T) {
	fake := &fakeGitServer{}
	svc, _ := newSvcWithFakeGit(t, fake)
	// No seedTeamNS → team_ns row doesn't exist.

	_, err := svc.EnsureKBRepo(context.Background(), kbTeamID,
		"https://github.com/ownerA/proj.git")
	if !errors.Is(err, ErrTeamNotFound) {
		t.Errorf("expected ErrTeamNotFound, got %v", err)
	}
	if fake.getRepoCalls != 0 {
		t.Errorf("getRepoCalls: got %d, want 0 (fail fast before git ops)", fake.getRepoCalls)
	}
}

func TestEnsureKBRepo_InvalidURL_400(t *testing.T) {
	fake := &fakeGitServer{}
	svc, db := newSvcWithFakeGit(t, fake)
	seedTeamNS(t, db, kbTeamID, "tenant-1")

	_, err := svc.EnsureKBRepo(context.Background(), kbTeamID, "not-a-url")
	if !errors.Is(err, ErrInvalidRequest) {
		t.Errorf("expected ErrInvalidRequest, got %v", err)
	}
	if fake.getRepoCalls != 0 {
		t.Errorf("getRepoCalls: got %d, want 0 (fail fast on bad URL)", fake.getRepoCalls)
	}
}

func TestEnsureKBRepo_InvalidTeamID_400(t *testing.T) {
	fake := &fakeGitServer{}
	svc, _ := newSvcWithFakeGit(t, fake)

	_, err := svc.EnsureKBRepo(context.Background(), "not-a-uuid",
		"https://github.com/ownerA/proj.git")
	if !errors.Is(err, ErrInvalidRequest) {
		t.Errorf("expected ErrInvalidRequest, got %v", err)
	}
}

func TestEnsureKBRepo_TenantGitServerUnresolved(t *testing.T) {
	// Override gitServerFor to return nil — tenant has no bound git server.
	db := setupDB(t)
	svc := NewService(db, nil, nil, mustAES(t), zap.NewNop())
	svc.gitServerFor = func(ctx context.Context, tenantID string) (gitsync.GitServer, error) {
		return nil, nil
	}
	seedTeamNS(t, db, kbTeamID, "tenant-1")

	_, err := svc.EnsureKBRepo(context.Background(), kbTeamID,
		"https://github.com/ownerA/proj.git")
	if !errors.Is(err, ErrTenantGitServerUnresolved) {
		t.Errorf("expected ErrTenantGitServerUnresolved, got %v", err)
	}
}

func TestEnsureKBRepo_NoDriftCheckAndNoInstanceBranch(t *testing.T) {
	// Ensures kb path doesn't accidentally call ReadFile / WriteFile /
	// GetBranch / CreateBranch (workflow-only ops).
	fake := &fakeGitServer{
		getRepoResult: nil,
	}
	svc, db := newSvcWithFakeGit(t, fake)
	seedTeamNS(t, db, kbTeamID, "tenant-1")

	_, err := svc.EnsureKBRepo(context.Background(), kbTeamID,
		"https://github.com/ownerA/proj.git")
	if err != nil {
		t.Fatalf("EnsureKBRepo: %v", err)
	}
	if fake.readFileCalls != 0 || fake.writeFileCalls != 0 {
		t.Errorf("kb should not read/write snapshot files: read=%d write=%d",
			fake.readFileCalls, fake.writeFileCalls)
	}
	if fake.getBranchCalls != 0 || fake.createBranchCalls != 0 {
		t.Errorf("kb should not touch instance branches: get=%d create=%d",
			fake.getBranchCalls, fake.createBranchCalls)
	}
}

func TestEnsureKBRepo_PathDeterminismPerURL(t *testing.T) {
	// Same team, different code_repo_url → different kb_repo_path.
	fake := &fakeGitServer{getRepoResult: &gitsync.Repo{}}
	svc, db := newSvcWithFakeGit(t, fake)
	seedTeamNS(t, db, kbTeamID, "tenant-1")

	r1, err := svc.EnsureKBRepo(context.Background(), kbTeamID,
		"https://github.com/ownerA/proj.git")
	if err != nil {
		t.Fatal(err)
	}
	r2, err := svc.EnsureKBRepo(context.Background(), kbTeamID,
		"https://gitlab.com/group/subteam/tool.git")
	if err != nil {
		t.Fatal(err)
	}
	if r1.KbRepoPath == r2.KbRepoPath {
		t.Errorf("expected distinct paths for distinct URLs, got %q twice", r1.KbRepoPath)
	}
	want2 := "t-f1e2d3c4/kb-gitlab.com__group__subteam__tool"
	if r2.KbRepoPath != want2 {
		t.Errorf("r2.KbRepoPath: got %q, want %q", r2.KbRepoPath, want2)
	}
}

// Silence unused import warnings if a future edit drops gorm usage.
var _ = gorm.ErrRecordNotFound
