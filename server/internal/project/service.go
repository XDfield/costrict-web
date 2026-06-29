package project

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"path"
	"strings"
	"time"

	"github.com/costrict/costrict-web/server/internal/models"
	"github.com/costrict/costrict-web/server/internal/notification"
	"github.com/costrict/costrict-web/server/internal/notification/sender"
	userpkg "github.com/costrict/costrict-web/server/internal/user"
	"github.com/google/uuid"
	"gorm.io/datatypes"
	"gorm.io/gorm"
)

var ErrInvalidRepoURL = errors.New("invalid repository url")

const (
	RoleAdmin  = "admin"
	RoleMember = "member"

	InvitationPending   = "pending"
	InvitationAccepted  = "accepted"
	InvitationRejected  = "rejected"
	InvitationCancelled = "cancelled"
)

var (
	ErrProjectNotFound          = errors.New("project not found")
	ErrInvitationNotFound       = errors.New("invitation not found")
	ErrNotMember                = errors.New("not a project member")
	ErrPermissionDenied         = errors.New("permission denied")
	ErrProjectNameExists        = errors.New("project name already exists")
	ErrCannotInviteSelf         = errors.New("cannot invite yourself")
	ErrUserAlreadyInProject     = errors.New("user already in project")
	ErrInvitationAlreadyExists  = errors.New("invitation already exists")
	ErrInvitationExpired        = errors.New("invitation expired")
	ErrInvitationHandled        = errors.New("invitation already responded")
	ErrOnlyInviterCanCancel     = errors.New("only inviter can cancel invitation")
	ErrInvalidRole              = errors.New("invalid role")
	ErrProjectArchived          = errors.New("project is archived")
	ErrCannotRemoveLastAdmin    = errors.New("cannot remove last admin")
	ErrProjectAlreadyArchived   = errors.New("project already archived")
	ErrProjectNotArchived       = errors.New("project is not archived")
	ErrRepositoryAlreadyBound    = errors.New("repository already bound to project")
	ErrRepositoryBindingNotFound = errors.New("project repository binding not found")
)

type ProjectService struct {
	db              *gorm.DB
	userService     *userpkg.UserService
	notificationSvc *notification.NotificationService
}

func NewProjectService(db *gorm.DB, userService *userpkg.UserService, notificationSvc *notification.NotificationService) *ProjectService {
	return &ProjectService{db: db, userService: userService, notificationSvc: notificationSvc}
}

func isValidRole(role string) bool {
	return role == RoleAdmin || role == RoleMember
}

func textEquals(column string) string {
	return fmt.Sprintf("%s = CAST(? AS TEXT)", column)
}

func (s *ProjectService) CreateProject(creatorID, name, description string, enabledAt *time.Time) (*models.Project, error) {
	if name == "" {
		return nil, fmt.Errorf("name is required")
	}

	var existing models.Project
	if err := s.db.Where(textEquals("creator_id")+" AND name = ? AND deleted_at IS NULL", creatorID, name).First(&existing).Error; err == nil {
		return nil, ErrProjectNameExists
	} else if err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, err
	}

	now := time.Now()
	project := &models.Project{
		ID:          uuid.NewString(),
		Name:        name,
		Description: description,
		CreatorID:   creatorID,
		EnabledAt:   enabledAt,
		Metadata:    datatypes.JSON([]byte("{}")),
	}

	err := s.db.Transaction(func(tx *gorm.DB) error {
		if err := tx.Create(project).Error; err != nil {
			return err
		}
		member := &models.ProjectMember{
			ID:        uuid.NewString(),
			ProjectID: project.ID,
			UserID:    creatorID,
			Role:      RoleAdmin,
			JoinedAt:  now,
		}
		return tx.Create(member).Error
	})
	if err != nil {
		return nil, err
	}
	return project, nil
}

func (s *ProjectService) GetProject(projectID string) (*models.Project, error) {
	var project models.Project
	if err := s.db.Where("id = ?", projectID).First(&project).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, ErrProjectNotFound
		}
		return nil, err
	}
	return &project, nil
}

func (s *ProjectService) GetProjectForUser(projectID, userID string) (*models.Project, error) {
	if err := s.checkPermission(projectID, userID, RoleMember); err != nil {
		return nil, err
	}

	var project models.Project
	if err := s.db.Model(&models.Project{}).
		Select("projects.*, CASE WHEN pm.pinned_at IS NULL THEN FALSE ELSE TRUE END AS is_pinned").
		Joins("JOIN project_members pm ON pm.project_id = projects.id AND pm.deleted_at IS NULL").
		Where("projects.id = ? AND "+textEquals("pm.user_id"), projectID, userID).
		Where("projects.deleted_at IS NULL").
		First(&project).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, ErrProjectNotFound
		}
		return nil, err
	}
	return &project, nil
}

func (s *ProjectService) GetProjectBasicInfoForUser(projectID, userID string) (*models.Project, error) {
	if project, err := s.GetProjectForUser(projectID, userID); err == nil {
		return project, nil
	} else if !errors.Is(err, ErrPermissionDenied) && !errors.Is(err, ErrNotMember) && !errors.Is(err, ErrProjectNotFound) {
		return nil, err
	}

	var invitation models.ProjectInvitation
	now := time.Now()
	if err := s.db.
		Where("project_id = ? AND "+textEquals("invitee_id")+" AND status = ?", projectID, userID, InvitationPending).
		Where("expires_at IS NULL OR expires_at >= ?", now).
		Order("created_at DESC").
		First(&invitation).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, ErrPermissionDenied
		}
		return nil, err
	}

	return s.GetProject(projectID)
}

func (s *ProjectService) ListProjects(userID string, includeArchived bool, pinnedOnly bool) ([]models.Project, error) {
	var projects []models.Project
	query := s.db.Table("project_members AS pm").
		Select("projects.id, projects.name, projects.description, projects.creator_id, projects.enabled_at, projects.archived_at, projects.created_at, projects.updated_at, CASE WHEN pm.pinned_at IS NULL THEN FALSE ELSE TRUE END AS is_pinned").
		Joins("JOIN projects ON projects.id = pm.project_id").
		Where(textEquals("pm.user_id"), userID).
		Where("pm.deleted_at IS NULL").
		Where("projects.deleted_at IS NULL")
	if !includeArchived {
		query = query.Where("projects.archived_at IS NULL")
	}
	if pinnedOnly {
		query = query.Where("pm.pinned_at IS NOT NULL")
	}
	if err := query.Order("projects.created_at DESC").Find(&projects).Error; err != nil {
		return nil, err
	}
	return projects, nil
}

func (s *ProjectService) SetProjectPin(projectID, userID string, pinned bool) error {
	project, err := s.GetProject(projectID)
	if err != nil {
		return err
	}
	if project.ArchivedAt != nil {
		return ErrProjectArchived
	}
	if err := s.checkPermission(projectID, userID, RoleMember); err != nil {
		return err
	}

	updates := map[string]any{"pinned_at": nil}
	if pinned {
		now := time.Now()
		updates["pinned_at"] = &now
	}

	return s.db.Model(&models.ProjectMember{}).
		Where("project_id = ? AND "+textEquals("user_id"), projectID, userID).
		Updates(updates).Error
}

func (s *ProjectService) UpdateProject(projectID, userID string, updates map[string]any) error {
	if err := s.checkPermission(projectID, userID, RoleAdmin); err != nil {
		return err
	}
	project, err := s.GetProject(projectID)
	if err != nil {
		return err
	}
	if project.ArchivedAt != nil {
		return ErrProjectArchived
	}
	if name, ok := updates["name"].(string); ok && name != "" && name != project.Name {
		var existing models.Project
		if err := s.db.Where(textEquals("creator_id")+" AND name = ? AND id <> ? AND deleted_at IS NULL", project.CreatorID, name, projectID).First(&existing).Error; err == nil {
			return ErrProjectNameExists
		}
	}
	return s.db.Model(&models.Project{}).Where("id = ?", projectID).Updates(updates).Error
}

func (s *ProjectService) DeleteProject(projectID, userID string) error {
	if err := s.checkPermission(projectID, userID, RoleAdmin); err != nil {
		return err
	}
	return s.db.Delete(&models.Project{}, "id = ?", projectID).Error
}

func (s *ProjectService) ArchiveProject(projectID, userID string) error {
	if err := s.checkPermission(projectID, userID, RoleAdmin); err != nil {
		return err
	}
	project, err := s.GetProject(projectID)
	if err != nil {
		return err
	}
	if project.ArchivedAt != nil {
		return ErrProjectAlreadyArchived
	}
	now := time.Now()
	return s.db.Model(&models.Project{}).Where("id = ?", projectID).Updates(map[string]any{
		"archived_at": &now,
	}).Error
}

func (s *ProjectService) UnarchiveProject(projectID, userID string) error {
	if err := s.checkPermission(projectID, userID, RoleAdmin); err != nil {
		return err
	}
	project, err := s.GetProject(projectID)
	if err != nil {
		return err
	}
	if project.ArchivedAt == nil {
		return ErrProjectNotArchived
	}
	return s.db.Model(&models.Project{}).Where("id = ?", projectID).Updates(map[string]any{
		"archived_at": nil,
	}).Error
}

func (s *ProjectService) UpdateProjectArchiveTime(projectID, userID string, archivedAt time.Time) error {
	if err := s.checkPermission(projectID, userID, RoleAdmin); err != nil {
		return err
	}
	project, err := s.GetProject(projectID)
	if err != nil {
		return err
	}
	if project.ArchivedAt == nil {
		return ErrProjectNotArchived
	}
	return s.db.Model(&models.Project{}).Where("id = ?", projectID).Update("archived_at", &archivedAt).Error
}

func (s *ProjectService) ListMembers(projectID string) ([]models.ProjectMember, error) {
	var members []models.ProjectMember
	if err := s.db.Where("project_id = ?", projectID).Order("joined_at ASC").Find(&members).Error; err != nil {
		return nil, err
	}
	return members, nil
}

func (s *ProjectService) GetMember(projectID, userID string) (*models.ProjectMember, error) {
	var member models.ProjectMember
	if err := s.db.Where("project_id = ? AND "+textEquals("user_id"), projectID, userID).First(&member).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, ErrNotMember
		}
		return nil, err
	}
	return &member, nil
}

func (s *ProjectService) RemoveMember(projectID, operatorID, targetUserID string) error {
	if err := s.checkPermission(projectID, operatorID, RoleAdmin); err != nil {
		return err
	}
	project, err := s.GetProject(projectID)
	if err != nil {
		return err
	}
	if project.ArchivedAt != nil {
		return ErrProjectArchived
	}
	target, err := s.GetMember(projectID, targetUserID)
	if err != nil {
		return err
	}
	if target.Role == RoleAdmin {
		var adminCount int64
		if err := s.db.Model(&models.ProjectMember{}).Where("project_id = ? AND role = ?", projectID, RoleAdmin).Count(&adminCount).Error; err != nil {
			return err
		}
		if adminCount <= 1 {
			return ErrCannotRemoveLastAdmin
		}
	}
	return s.db.Delete(&models.ProjectMember{}, "project_id = ? AND "+textEquals("user_id"), projectID, targetUserID).Error
}

func (s *ProjectService) UpdateMemberRole(projectID, operatorID, targetUserID, newRole string) error {
	if !isValidRole(newRole) {
		return ErrInvalidRole
	}
	if err := s.checkPermission(projectID, operatorID, RoleAdmin); err != nil {
		return err
	}
	project, err := s.GetProject(projectID)
	if err != nil {
		return err
	}
	if project.ArchivedAt != nil {
		return ErrProjectArchived
	}
	target, err := s.GetMember(projectID, targetUserID)
	if err != nil {
		return err
	}
	if target.Role == RoleAdmin && newRole != RoleAdmin {
		var adminCount int64
		if err := s.db.Model(&models.ProjectMember{}).Where("project_id = ? AND role = ?", projectID, RoleAdmin).Count(&adminCount).Error; err != nil {
			return err
		}
		if adminCount <= 1 {
			return ErrCannotRemoveLastAdmin
		}
	}
	return s.db.Model(&models.ProjectMember{}).Where("project_id = ? AND "+textEquals("user_id"), projectID, targetUserID).Update("role", newRole).Error
}

func (s *ProjectService) CreateInvitation(projectID, inviterID, inviteeID, role, message string) (*models.ProjectInvitation, error) {
	if role == "" {
		role = RoleMember
	}
	if !isValidRole(role) {
		return nil, ErrInvalidRole
	}
	if inviterID == inviteeID {
		return nil, ErrCannotInviteSelf
	}
	if err := s.checkPermission(projectID, inviterID, RoleAdmin); err != nil {
		return nil, err
	}
	project, err := s.GetProject(projectID)
	if err != nil {
		return nil, err
	}
	if project.ArchivedAt != nil {
		return nil, ErrProjectArchived
	}
	if _, err := s.GetMember(projectID, inviteeID); err == nil {
		return nil, ErrUserAlreadyInProject
	} else if err != nil && !errors.Is(err, ErrNotMember) {
		return nil, err
	}

	now := time.Now()
	var pending models.ProjectInvitation
	if err := s.db.Where("project_id = ? AND "+textEquals("invitee_id")+" AND status = ?", projectID, inviteeID, InvitationPending).First(&pending).Error; err == nil {
		if pending.ExpiresAt == nil || pending.ExpiresAt.After(now) {
			return nil, ErrInvitationAlreadyExists
		}
	}
	expiresAt := now.Add(7 * 24 * time.Hour)
	invitation := &models.ProjectInvitation{
		ID:        uuid.NewString(),
		ProjectID: projectID,
		InviterID: inviterID,
		InviteeID: inviteeID,
		Role:      role,
		Status:    InvitationPending,
		Message:   message,
		ExpiresAt: &expiresAt,
	}
	if err := s.db.Create(invitation).Error; err != nil {
		return nil, err
	}
	invitation.ProjectName = project.Name
	s.notifyInvitationCreated(project, invitation)
	return invitation, nil
}

func (s *ProjectService) RespondInvitation(invitationID, userID string, accept bool) error {
	var invitation models.ProjectInvitation
	if err := s.db.Where("id = ?", invitationID).First(&invitation).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return ErrInvitationNotFound
		}
		return err
	}
	if invitation.InviteeID != userID {
		return ErrPermissionDenied
	}
	if invitation.Status != InvitationPending {
		return ErrInvitationHandled
	}
	if invitation.ExpiresAt != nil && invitation.ExpiresAt.Before(time.Now()) {
		return ErrInvitationExpired
	}
	now := time.Now()
	status := InvitationRejected
	if accept {
		status = InvitationAccepted
	}
	return s.db.Transaction(func(tx *gorm.DB) error {
		updates := map[string]any{"status": status, "responded_at": &now}
		if err := tx.Model(&models.ProjectInvitation{}).Where("id = ?", invitation.ID).Updates(updates).Error; err != nil {
			return err
		}
		if accept {
			member := &models.ProjectMember{ID: uuid.NewString(), ProjectID: invitation.ProjectID, UserID: userID, Role: invitation.Role, JoinedAt: now}
			if err := tx.Create(member).Error; err != nil {
				return err
			}
		}
		return nil
	})
}

func (s *ProjectService) ListInvitations(projectID string) ([]models.ProjectInvitation, error) {
	var invitations []models.ProjectInvitation
	if err := s.db.Model(&models.ProjectInvitation{}).
		Select("project_invitations.*, projects.name AS project_name").
		Joins("JOIN projects ON projects.id = project_invitations.project_id AND projects.deleted_at IS NULL").
		Where("project_invitations.project_id = ?", projectID).
		Order("project_invitations.created_at DESC").
		Find(&invitations).Error; err != nil {
		return nil, err
	}
	return invitations, nil
}

func (s *ProjectService) ListMyInvitations(userID string) ([]models.ProjectInvitation, error) {
	now := time.Now()
	_ = s.db.Model(&models.ProjectInvitation{}).
		Where("status = ? AND expires_at IS NOT NULL AND expires_at < ?", InvitationPending, now).
		Update("status", InvitationCancelled).Error

	var invitations []models.ProjectInvitation
	if err := s.db.Model(&models.ProjectInvitation{}).
		Select("project_invitations.*, projects.name AS project_name").
		Joins("JOIN projects ON projects.id = project_invitations.project_id AND projects.deleted_at IS NULL").
		Where(textEquals("project_invitations.invitee_id"), userID).
		Order("project_invitations.created_at DESC").
		Find(&invitations).Error; err != nil {
		return nil, err
	}
	return invitations, nil
}

func (s *ProjectService) CancelInvitation(invitationID, operatorID string) error {
	var invitation models.ProjectInvitation
	if err := s.db.Where("id = ?", invitationID).First(&invitation).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return ErrInvitationNotFound
		}
		return err
	}
	if invitation.InviterID != operatorID {
		return ErrOnlyInviterCanCancel
	}
	if invitation.Status != InvitationPending {
		return ErrInvitationHandled
	}
	return s.db.Model(&models.ProjectInvitation{}).Where("id = ?", invitationID).Update("status", InvitationCancelled).Error
}

func (s *ProjectService) IsProjectAdmin(projectID, userID string) (bool, error) {
	member, err := s.GetMember(projectID, userID)
	if err != nil {
		if errors.Is(err, ErrNotMember) {
			return false, nil
		}
		return false, err
	}
	return member.Role == RoleAdmin, nil
}

func (s *ProjectService) IsProjectMember(projectID, userID string) (bool, error) {
	_, err := s.GetMember(projectID, userID)
	if err != nil {
		if errors.Is(err, ErrNotMember) {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

func (s *ProjectService) ListRepositories(projectID, userID string) ([]models.ProjectRepository, error) {
	if err := s.checkPermission(projectID, userID, RoleMember); err != nil {
		return nil, err
	}
	var repositories []models.ProjectRepository
	if err := s.db.Where("project_id = ?", projectID).Order("created_at ASC").Find(&repositories).Error; err != nil {
		return nil, err
	}
	return repositories, nil
}

func (s *ProjectService) BindRepository(projectID, operatorID, gitRepoURL, displayName string) (*models.ProjectRepository, error) {
	if err := s.checkPermission(projectID, operatorID, RoleAdmin); err != nil {
		return nil, err
	}
	project, err := s.GetProject(projectID)
	if err != nil {
		return nil, err
	}
	if project.ArchivedAt != nil {
		return nil, ErrProjectArchived
	}
	normalizedRepo, err := normalizeGitRepoURL(gitRepoURL)
	if err != nil {
		return nil, err
	}
	var existing models.ProjectRepository
	if err := s.db.Where("project_id = ? AND git_repo_url = ?", projectID, normalizedRepo).First(&existing).Error; err == nil {
		return nil, ErrRepositoryAlreadyBound
	} else if err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, err
	}
	repository := &models.ProjectRepository{
		ID:            uuid.NewString(),
		ProjectID:     projectID,
		GitRepoURL:    normalizedRepo,
		DisplayName:   displayName,
		Source:        "manual",
		BoundByUserID: operatorID,
	}
	if err := s.db.Create(repository).Error; err != nil {
		return nil, err
	}
	return repository, nil
}

func (s *ProjectService) UnbindRepository(projectID, repoBindingID, operatorID string) error {
	if err := s.checkPermission(projectID, operatorID, RoleAdmin); err != nil {
		return err
	}
	project, err := s.GetProject(projectID)
	if err != nil {
		return err
	}
	if project.ArchivedAt != nil {
		return ErrProjectArchived
	}
	result := s.db.Delete(&models.ProjectRepository{}, "project_id = ? AND id = ?", projectID, repoBindingID)
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected == 0 {
		return ErrRepositoryBindingNotFound
	}
	return nil
}

func (s *ProjectService) checkPermission(projectID, userID, requiredRole string) error {
	member, err := s.GetMember(projectID, userID)
	if err != nil {
		return err
	}
	if requiredRole == RoleAdmin && member.Role != RoleAdmin {
		return ErrPermissionDenied
	}
	return nil
}

func normalizeGitRepoURL(raw string) (string, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return "", ErrInvalidRepoURL
	}

	if strings.HasPrefix(trimmed, "git@") {
		withoutPrefix := strings.TrimPrefix(trimmed, "git@")
		parts := strings.SplitN(withoutPrefix, ":", 2)
		if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
			return "", ErrInvalidRepoURL
		}
		trimmed = "https://" + parts[0] + "/" + parts[1]
	}

	parsed, err := url.Parse(trimmed)
	if err != nil {
		return "", fmt.Errorf("%w: %v", ErrInvalidRepoURL, err)
	}
	if parsed.Host == "" && parsed.Scheme == "" {
		trimmed = "https://" + trimmed
		parsed, err = url.Parse(trimmed)
		if err != nil {
			return "", fmt.Errorf("%w: %v", ErrInvalidRepoURL, err)
		}
	}
	if parsed.Host == "" {
		return "", ErrInvalidRepoURL
	}

	repoPath := strings.TrimSuffix(parsed.Path, ".git")
	repoPath = strings.TrimSuffix(repoPath, "/")
	repoPath = path.Clean(repoPath)
	if repoPath == "." || repoPath == "/" || repoPath == "" {
		return "", ErrInvalidRepoURL
	}
	if !strings.HasPrefix(repoPath, "/") {
		repoPath = "/" + repoPath
	}

	return "https://" + strings.ToLower(parsed.Host) + strings.ToLower(repoPath), nil
}

func (s *ProjectService) notifyInvitationCreated(project *models.Project, invitation *models.ProjectInvitation) {
	if s.notificationSvc == nil {
		return
	}
	payload := notification.ProjectInvitationMessage{
		InvitationID: invitation.ID,
		ProjectID:    invitation.ProjectID,
		ProjectName:  project.Name,
		InviterName:  invitation.InviterID,
		Role:         invitation.Role,
		Message:      invitation.Message,
	}
	if invitation.ExpiresAt != nil {
		payload.ExpiresAt = invitation.ExpiresAt.Format(time.RFC3339)
	}
	customBody, _ := json.Marshal(payload)
	s.notificationSvc.TriggerMessage(invitation.InviteeID, notification.EventProjectInvitationCreated, sender.NotificationMessage{
		Title:     "项目邀请",
		Body:      fmt.Sprintf("您收到项目“%s”的加入邀请", project.Name),
		EventType: notification.EventProjectInvitationCreated,
		Metadata:  map[string]any{"projectInvitation": json.RawMessage(customBody)},
	})
	s.notificationSvc.TriggerMessage(invitation.InviteeID, notification.EventSystemNotification, sender.NotificationMessage{
		Title:     "系统通知",
		Body:      fmt.Sprintf("您收到项目“%s”的邀请，请尽快处理", project.Name),
		EventType: notification.EventSystemNotification,
		Metadata: map[string]any{
			"type":        "project.invitation",
			"relatedId":   invitation.ID,
			"relatedType": "invitation",
		},
	})
}
