package handlers

import (
	"context"
	"sync"

	"github.com/costrict/costrict-web/server/internal/gitsync"
)

// fakeGitServerHandler is a programmable GitServer stub for handler tests.
// It records every call (for assertions) and returns whatever Result/Error
// fields are populated. Only the repo/branch/file surface needs configuring
// — the team-member and bot-account methods are no-ops because the handlers
// under test don't exercise them.
type fakeGitServerHandler struct {
	mu sync.Mutex

	getRepoCalls  []struct{ Owner, Name string }
	getRepoResult *gitsync.Repo
	getRepoErr    error

	createRepoCalls []struct {
		Owner string
		Opts  gitsync.CreateRepoOptions
	}
	createRepoErr error

	readFileCalls []struct {
		Owner, Repo, Branch, Path string
	}
	readFileResult []byte
	readFileErr    error

	writeFileCalls []struct {
		Owner, Repo, Branch, Path string
		Content                   []byte
		Message                   string
	}
	writeFileErr error

	setBranchProtectionCalls []struct {
		Owner, Repo string
		Opts        gitsync.BranchProtectionOptions
	}
	setBranchProtectionErr error

	getBranchCalls []struct {
		Owner, Repo, Branch string
	}
	getBranchResult *gitsync.Branch
	getBranchErr    error

	createBranchCalls []struct {
		Owner, Repo, NewBranch, FromRef string
	}
	createBranchErr error
}

func (f *fakeGitServerHandler) GetRepo(ctx context.Context, owner, name string) (*gitsync.Repo, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.getRepoCalls = append(f.getRepoCalls, struct{ Owner, Name string }{owner, name})
	return f.getRepoResult, f.getRepoErr
}

func (f *fakeGitServerHandler) CreateRepo(ctx context.Context, owner string, opts gitsync.CreateRepoOptions) (*gitsync.Repo, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.createRepoCalls = append(f.createRepoCalls, struct {
		Owner string
		Opts  gitsync.CreateRepoOptions
	}{owner, opts})
	if f.createRepoErr != nil {
		return nil, f.createRepoErr
	}
	return &gitsync.Repo{Name: opts.Name, FullName: owner + "/" + opts.Name, Private: opts.Private}, nil
}

func (f *fakeGitServerHandler) WriteFile(ctx context.Context, owner, repo, branch, path string, content []byte, message string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.writeFileCalls = append(f.writeFileCalls, struct {
		Owner, Repo, Branch, Path string
		Content                   []byte
		Message                   string
	}{owner, repo, branch, path, content, message})
	return f.writeFileErr
}

func (f *fakeGitServerHandler) ReadFile(ctx context.Context, owner, repo, branch, path string) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.readFileCalls = append(f.readFileCalls, struct {
		Owner, Repo, Branch, Path string
	}{owner, repo, branch, path})
	return f.readFileResult, f.readFileErr
}

func (f *fakeGitServerHandler) SetBranchProtection(ctx context.Context, owner, repo string, opts gitsync.BranchProtectionOptions) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.setBranchProtectionCalls = append(f.setBranchProtectionCalls, struct {
		Owner, Repo string
		Opts        gitsync.BranchProtectionOptions
	}{owner, repo, opts})
	return f.setBranchProtectionErr
}

func (f *fakeGitServerHandler) GetBranch(ctx context.Context, owner, repo, branch string) (*gitsync.Branch, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.getBranchCalls = append(f.getBranchCalls, struct {
		Owner, Repo, Branch string
	}{owner, repo, branch})
	return f.getBranchResult, f.getBranchErr
}

func (f *fakeGitServerHandler) CreateBranch(ctx context.Context, owner, repo, newBranch, fromRef string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.createBranchCalls = append(f.createBranchCalls, struct {
		Owner, Repo, NewBranch, FromRef string
	}{owner, repo, newBranch, fromRef})
	return f.createBranchErr
}

// Stubs for embedded interface methods not exercised by EnsureWorkflowRepo.
func (f *fakeGitServerHandler) ListTeamMembers(ctx context.Context, teamID int64) ([]gitsync.GiteaMember, error) {
	return nil, nil
}
func (f *fakeGitServerHandler) AddTeamMember(ctx context.Context, teamID int64, username string) error {
	return nil
}
func (f *fakeGitServerHandler) RemoveTeamMember(ctx context.Context, teamID int64, username string) error {
	return nil
}
func (f *fakeGitServerHandler) CreateUser(ctx context.Context, opts gitsync.CreateUserOptions) (*gitsync.GiteaUser, error) {
	return nil, nil
}
func (f *fakeGitServerHandler) GetUserByName(ctx context.Context, username string) (*gitsync.GiteaUser, error) {
	return nil, nil
}
func (f *fakeGitServerHandler) CreateUserToken(ctx context.Context, username string, opts gitsync.CreateUserTokenOptions) (*gitsync.GiteaToken, error) {
	return nil, nil
}
func (f *fakeGitServerHandler) DeleteUserToken(ctx context.Context, username string, tokenID int64) error {
	return nil
}
func (f *fakeGitServerHandler) CreateOrg(ctx context.Context, opts gitsync.CreateOrgOptions) (*gitsync.GiteaOrg, error) {
	return nil, nil
}
func (f *fakeGitServerHandler) GetOrgByName(ctx context.Context, name string) (*gitsync.GiteaOrg, error) {
	return nil, nil
}
func (f *fakeGitServerHandler) ListOrgTeams(ctx context.Context, org string) ([]gitsync.GiteaTeam, error) {
	return nil, nil
}
func (f *fakeGitServerHandler) UpdateOrg(ctx context.Context, org string, opts gitsync.UpdateOrgOptions) error {
	return nil
}
func (f *fakeGitServerHandler) ListOrgMembers(ctx context.Context, org string) ([]string, error) {
	return nil, nil
}
func (f *fakeGitServerHandler) AddOrgMember(ctx context.Context, org, username string) error {
	return nil
}
func (f *fakeGitServerHandler) RemoveOrgMember(ctx context.Context, org, username string) error {
	return nil
}

var _ gitsync.GitServer = (*fakeGitServerHandler)(nil)
