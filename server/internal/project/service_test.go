package project

import (
	"errors"
	"testing"
	"time"

	"github.com/costrict/costrict-web/server/internal/models"
	"github.com/google/uuid"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

func setupProjectTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatalf("open test db: %v", err)
	}
	stmts := []string{
		`CREATE TABLE projects (
			id TEXT PRIMARY KEY,
			name TEXT NOT NULL,
			description TEXT,
			creator_id TEXT NOT NULL,
			enabled_at DATETIME,
			archived_at DATETIME,
			metadata JSON,
			created_at DATETIME,
			updated_at DATETIME,
			deleted_at DATETIME
		)`,
		`CREATE UNIQUE INDEX idx_project_creator_name ON projects(creator_id, name)`,
		`CREATE TABLE project_members (
			id TEXT PRIMARY KEY,
			project_id TEXT NOT NULL,
			user_id TEXT NOT NULL,
			role TEXT NOT NULL,
			pinned_at DATETIME,
			joined_at DATETIME NOT NULL,
			created_at DATETIME,
			updated_at DATETIME,
			deleted_at DATETIME
		)`,
		`CREATE UNIQUE INDEX idx_project_user ON project_members(project_id, user_id)`,
		`CREATE TABLE project_invitations (
			id TEXT PRIMARY KEY,
			project_id TEXT NOT NULL,
			inviter_id TEXT NOT NULL,
			invitee_id TEXT NOT NULL,
			role TEXT NOT NULL,
			status TEXT NOT NULL,
			message TEXT,
			responded_at DATETIME,
			expires_at DATETIME,
			created_at DATETIME,
			updated_at DATETIME
		)`,
		`CREATE INDEX idx_project_invitee ON project_invitations(project_id, invitee_id)`,
		`CREATE INDEX idx_invitee_status ON project_invitations(invitee_id, status)`,
		`CREATE TABLE project_repositories (
			id TEXT PRIMARY KEY,
			project_id TEXT NOT NULL,
			git_repo_url TEXT NOT NULL,
			display_name TEXT,
			source TEXT NOT NULL,
			bound_by_user_id TEXT NOT NULL,
			last_activity_at DATETIME,
			created_at DATETIME,
			updated_at DATETIME,
			deleted_at DATETIME
		)`,
		`CREATE UNIQUE INDEX idx_project_repo_unique ON project_repositories(project_id, git_repo_url)`,
	}
	for _, stmt := range stmts {
		if err := db.Exec(stmt).Error; err != nil {
			t.Fatalf("migrate test db: %v", err)
		}
	}
	return db
}

func TestBindAndListProjectRepositories(t *testing.T) {
	db := setupProjectTestDB(t)
	svc := NewProjectService(db, nil, nil, nil)
	project, err := svc.CreateProject("admin", "Project A", "", nil)
	if err != nil {
		t.Fatalf("CreateProject error: %v", err)
	}
	repo, err := svc.BindRepository(project.ID, "admin", "git@github.com:zgsm-ai/opencode.git", "opencode")
	if err != nil {
		t.Fatalf("BindRepository error: %v", err)
	}
	if repo.GitRepoURL != "https://github.com/zgsm-ai/opencode" {
		t.Fatalf("unexpected normalized repo: %+v", repo)
	}
	if _, err := svc.BindRepository(project.ID, "admin", "https://github.com/zgsm-ai/opencode/", "dup"); !errors.Is(err, ErrRepositoryAlreadyBound) {
		t.Fatalf("expected ErrRepositoryAlreadyBound, got %v", err)
	}
	repos, err := svc.ListRepositories(project.ID, "admin")
	if err != nil {
		t.Fatalf("ListRepositories error: %v", err)
	}
	if len(repos) != 1 {
		t.Fatalf("expected 1 repo, got %+v", repos)
	}
}

func TestBindRepositoryRequiresAdminAndRejectsArchivedProject(t *testing.T) {
	db := setupProjectTestDB(t)
	svc := NewProjectService(db, nil, nil, nil)
	project, err := svc.CreateProject("admin", "Project A", "", nil)
	if err != nil {
		t.Fatalf("CreateProject error: %v", err)
	}
	inv, err := svc.CreateInvitation(project.ID, "admin", "member1", RoleMember, "")
	if err != nil {
		t.Fatalf("CreateInvitation error: %v", err)
	}
	if err := svc.RespondInvitation(inv.ID, "member1", true); err != nil {
		t.Fatalf("RespondInvitation error: %v", err)
	}
	if _, err := svc.BindRepository(project.ID, "member1", "https://github.com/test/repo", "repo"); !errors.Is(err, ErrPermissionDenied) {
		t.Fatalf("expected ErrPermissionDenied, got %v", err)
	}
	if err := svc.ArchiveProject(project.ID, "admin"); err != nil {
		t.Fatalf("ArchiveProject error: %v", err)
	}
	if _, err := svc.BindRepository(project.ID, "admin", "https://github.com/test/repo", "repo"); !errors.Is(err, ErrProjectArchived) {
		t.Fatalf("expected ErrProjectArchived, got %v", err)
	}
}

func TestCreateProjectCreatesAdminMember(t *testing.T) {
	db := setupProjectTestDB(t)
	svc := NewProjectService(db, nil, nil, nil)
	enabledAt := time.Now().UTC()

	project, err := svc.CreateProject("u1", "Project A", "desc", &enabledAt)
	if err != nil {
		t.Fatalf("CreateProject error: %v", err)
	}
	if project.EnabledAt == nil || !project.EnabledAt.Equal(enabledAt) {
		t.Fatalf("expected enabledAt set, got %+v", project)
	}
	member, err := svc.GetMember(project.ID, "u1")
	if err != nil {
		t.Fatalf("GetMember error: %v", err)
	}
	if member.Role != RoleAdmin {
		t.Fatalf("expected creator to be admin, got %s", member.Role)
	}
}

func TestCreateProjectNameUniquePerCreator(t *testing.T) {
	db := setupProjectTestDB(t)
	svc := NewProjectService(db, nil, nil, nil)
	if _, err := svc.CreateProject("u1", "Project A", "", nil); err != nil {
		t.Fatalf("seed CreateProject error: %v", err)
	}
	if _, err := svc.CreateProject("u1", "Project A", "", nil); !errors.Is(err, ErrProjectNameExists) {
		t.Fatalf("expected ErrProjectNameExists, got %v", err)
	}
	if _, err := svc.CreateProject("u2", "Project A", "", nil); err != nil {
		t.Fatalf("expected same name allowed for different creator, got %v", err)
	}
}

func TestCreateProjectAllowsNilEnabledAt(t *testing.T) {
	db := setupProjectTestDB(t)
	svc := NewProjectService(db, nil, nil, nil)
	project, err := svc.CreateProject("u1", "Project A", "", nil)
	if err != nil {
		t.Fatalf("CreateProject error: %v", err)
	}
	if project.EnabledAt != nil {
		t.Fatalf("expected nil enabledAt, got %+v", project.EnabledAt)
	}
}

func TestArchiveAndUnarchiveProject(t *testing.T) {
	db := setupProjectTestDB(t)
	svc := NewProjectService(db, nil, nil, nil)
	enabledAt := time.Now().UTC()
	project, err := svc.CreateProject("u1", "Project A", "", &enabledAt)
	if err != nil {
		t.Fatalf("CreateProject error: %v", err)
	}
	if err := svc.ArchiveProject(project.ID, "u1"); err != nil {
		 t.Fatalf("ArchiveProject error: %v", err)
	}
	archived, _ := svc.GetProject(project.ID)
	if archived.ArchivedAt == nil {
		t.Fatalf("expected archived project, got %+v", archived)
	}
	if err := svc.ArchiveProject(project.ID, "u1"); !errors.Is(err, ErrProjectAlreadyArchived) {
		t.Fatalf("expected ErrProjectAlreadyArchived, got %v", err)
	}
	if err := svc.UnarchiveProject(project.ID, "u1"); err != nil {
		t.Fatalf("UnarchiveProject error: %v", err)
	}
	unarchived, _ := svc.GetProject(project.ID)
	if unarchived.ArchivedAt != nil {
		t.Fatalf("expected archivedAt cleared, got %+v", unarchived.ArchivedAt)
	}
	if unarchived.EnabledAt == nil || !unarchived.EnabledAt.Equal(enabledAt) {
		t.Fatalf("expected enabledAt preserved after unarchive, got %+v", unarchived.EnabledAt)
	}
}

func TestUpdateProjectCanSetEnabledAt(t *testing.T) {
	db := setupProjectTestDB(t)
	svc := NewProjectService(db, nil, nil, nil)
	project, err := svc.CreateProject("u1", "Project A", "", nil)
	if err != nil {
		t.Fatalf("CreateProject error: %v", err)
	}
	enabledAt := time.Now().UTC().Add(2 * time.Hour)
	if err := svc.UpdateProject(project.ID, "u1", map[string]any{"enabled_at": enabledAt}); err != nil {
		t.Fatalf("UpdateProject error: %v", err)
	}
	updated, err := svc.GetProject(project.ID)
	if err != nil {
		t.Fatalf("GetProject error: %v", err)
	}
	if updated.EnabledAt == nil || !updated.EnabledAt.Equal(enabledAt) {
		t.Fatalf("expected enabledAt updated, got %+v", updated.EnabledAt)
	}
}

func TestUpdateProjectArchiveTime(t *testing.T) {
	db := setupProjectTestDB(t)
	svc := NewProjectService(db, nil, nil, nil)
	project, err := svc.CreateProject("u1", "Project A", "", nil)
	if err != nil {
		t.Fatalf("CreateProject error: %v", err)
	}
	if err := svc.ArchiveProject(project.ID, "u1"); err != nil {
		t.Fatalf("ArchiveProject error: %v", err)
	}
	archivedAt := time.Now().UTC().Add(-2 * time.Hour).Truncate(time.Second)
	if err := svc.UpdateProjectArchiveTime(project.ID, "u1", archivedAt); err != nil {
		t.Fatalf("UpdateProjectArchiveTime error: %v", err)
	}
	updated, err := svc.GetProject(project.ID)
	if err != nil {
		t.Fatalf("GetProject error: %v", err)
	}
	if updated.ArchivedAt == nil || !updated.ArchivedAt.Equal(archivedAt) {
		t.Fatalf("expected archivedAt updated, got %+v", updated.ArchivedAt)
	}
}

func TestUpdateProjectArchiveTimeRequiresArchivedProject(t *testing.T) {
	db := setupProjectTestDB(t)
	svc := NewProjectService(db, nil, nil, nil)
	project, err := svc.CreateProject("u1", "Project A", "", nil)
	if err != nil {
		t.Fatalf("CreateProject error: %v", err)
	}
	err = svc.UpdateProjectArchiveTime(project.ID, "u1", time.Now().UTC())
	if !errors.Is(err, ErrProjectNotArchived) {
		t.Fatalf("expected ErrProjectNotArchived, got %v", err)
	}
}

func TestListProjectsKeepsCreatedAtOrder(t *testing.T) {
	db := setupProjectTestDB(t)
	svc := NewProjectService(db, nil, nil, nil)

	first, err := svc.CreateProject("u1", "Project A", "", nil)
	if err != nil {
		t.Fatalf("CreateProject first error: %v", err)
	}
	time.Sleep(time.Millisecond)
	second, err := svc.CreateProject("u1", "Project B", "", nil)
	if err != nil {
		t.Fatalf("CreateProject second error: %v", err)
	}

	if err := svc.SetProjectPin(first.ID, "u1", true); err != nil {
		t.Fatalf("SetProjectPin error: %v", err)
	}

	projects, err := svc.ListProjects("u1", false, false)
	if err != nil {
		t.Fatalf("ListProjects error: %v", err)
	}
	if len(projects) != 2 {
		t.Fatalf("expected 2 projects, got %d", len(projects))
	}
	if projects[0].ID != second.ID || projects[0].IsPinned {
		t.Fatalf("expected newest project first regardless of pin, got %+v", projects)
	}
	if projects[1].ID != first.ID || !projects[1].IsPinned {
		t.Fatalf("expected older pinned project second with pin flag preserved, got %+v", projects)
	}
}

func TestListProjectsCanFilterPinnedOnly(t *testing.T) {
	db := setupProjectTestDB(t)
	svc := NewProjectService(db, nil, nil, nil)

	projectA, err := svc.CreateProject("u1", "Project A", "", nil)
	if err != nil {
		t.Fatalf("CreateProject A error: %v", err)
	}
	_, err = svc.CreateProject("u1", "Project B", "", nil)
	if err != nil {
		t.Fatalf("CreateProject B error: %v", err)
	}
	if err := svc.SetProjectPin(projectA.ID, "u1", true); err != nil {
		t.Fatalf("SetProjectPin error: %v", err)
	}

	projects, err := svc.ListProjects("u1", false, true)
	if err != nil {
		t.Fatalf("ListProjects pinned error: %v", err)
	}
	if len(projects) != 1 {
		t.Fatalf("expected 1 pinned project, got %+v", projects)
	}
	if projects[0].ID != projectA.ID || !projects[0].IsPinned {
		t.Fatalf("expected pinned project only, got %+v", projects)
	}
}

func TestSetProjectPinUpdatesMemberPreference(t *testing.T) {
	db := setupProjectTestDB(t)
	svc := NewProjectService(db, nil, nil, nil)

	project, err := svc.CreateProject("admin", "Project A", "", nil)
	if err != nil {
		t.Fatalf("CreateProject error: %v", err)
	}
	if err := svc.SetProjectPin(project.ID, "admin", true); err != nil {
		t.Fatalf("SetProjectPin true error: %v", err)
	}
	member, err := svc.GetMember(project.ID, "admin")
	if err != nil {
		t.Fatalf("GetMember error: %v", err)
	}
	if member.PinnedAt == nil {
		t.Fatalf("expected pinned_at to be set")
	}

	if err := svc.SetProjectPin(project.ID, "admin", false); err != nil {
		t.Fatalf("SetProjectPin false error: %v", err)
	}
	member, err = svc.GetMember(project.ID, "admin")
	if err != nil {
		t.Fatalf("GetMember error after unpin: %v", err)
	}
	if member.PinnedAt != nil {
		t.Fatalf("expected pinned_at to be nil, got %+v", member.PinnedAt)
	}
}

func TestInvitationFlowAcceptAndReject(t *testing.T) {
	db := setupProjectTestDB(t)
	svc := NewProjectService(db, nil, nil, nil)
	project, err := svc.CreateProject("admin", "Project A", "", nil)
	if err != nil {
		t.Fatalf("CreateProject error: %v", err)
	}

	inv, err := svc.CreateInvitation(project.ID, "admin", "user1", RoleMember, "join us")
	if err != nil {
		t.Fatalf("CreateInvitation error: %v", err)
	}
	if _, err := svc.CreateInvitation(project.ID, "admin", "user1", RoleMember, "dup"); !errors.Is(err, ErrInvitationAlreadyExists) {
		t.Fatalf("expected ErrInvitationAlreadyExists, got %v", err)
	}
	if err := svc.RespondInvitation(inv.ID, "user1", true); err != nil {
		t.Fatalf("RespondInvitation accept error: %v", err)
	}
	member, err := svc.GetMember(project.ID, "user1")
	if err != nil {
		t.Fatalf("expected member created, got %v", err)
	}
	if member.Role != RoleMember {
		t.Fatalf("unexpected member role: %s", member.Role)
	}

	inv2, err := svc.CreateInvitation(project.ID, "admin", "user2", RoleAdmin, "admin join")
	if err != nil {
		t.Fatalf("CreateInvitation second error: %v", err)
	}
	if err := svc.RespondInvitation(inv2.ID, "user2", false); err != nil {
		t.Fatalf("RespondInvitation reject error: %v", err)
	}
	if _, err := svc.GetMember(project.ID, "user2"); !errors.Is(err, ErrNotMember) {
		t.Fatalf("expected no member for rejected invite, got %v", err)
	}
}

func TestOnlyAdminCanInviteAndCannotRemoveLastAdmin(t *testing.T) {
	db := setupProjectTestDB(t)
	svc := NewProjectService(db, nil, nil, nil)
	project, err := svc.CreateProject("admin", "Project A", "", nil)
	if err != nil {
		t.Fatalf("CreateProject error: %v", err)
	}
	inv, err := svc.CreateInvitation(project.ID, "admin", "member1", RoleMember, "")
	if err != nil {
		t.Fatalf("CreateInvitation error: %v", err)
	}
	if err := svc.RespondInvitation(inv.ID, "member1", true); err != nil {
		t.Fatalf("accept invite error: %v", err)
	}
	if _, err := svc.CreateInvitation(project.ID, "member1", "member2", RoleMember, ""); !errors.Is(err, ErrPermissionDenied) {
		t.Fatalf("expected ErrPermissionDenied, got %v", err)
	}
	if err := svc.RemoveMember(project.ID, "admin", "admin"); !errors.Is(err, ErrCannotRemoveLastAdmin) {
		t.Fatalf("expected ErrCannotRemoveLastAdmin, got %v", err)
	}
	if err := svc.UpdateMemberRole(project.ID, "admin", "admin", RoleMember); !errors.Is(err, ErrCannotRemoveLastAdmin) {
		t.Fatalf("expected ErrCannotRemoveLastAdmin, got %v", err)
	}
}

func TestExpiredInvitationCannotBeAccepted(t *testing.T) {
	db := setupProjectTestDB(t)
	svc := NewProjectService(db, nil, nil, nil)
	project, err := svc.CreateProject("admin", "Project A", "", nil)
	if err != nil {
		t.Fatalf("CreateProject error: %v", err)
	}
	past := time.Now().Add(-time.Hour)
	inv := &models.ProjectInvitation{ID: uuid.NewString(), ProjectID: project.ID, InviterID: "admin", InviteeID: "user1", Role: RoleMember, Status: InvitationPending, ExpiresAt: &past}
	if err := db.Create(inv).Error; err != nil {
		t.Fatalf("seed invitation error: %v", err)
	}
	if err := svc.RespondInvitation(inv.ID, "user1", true); !errors.Is(err, ErrInvitationExpired) {
		t.Fatalf("expected ErrInvitationExpired, got %v", err)
	}
}
