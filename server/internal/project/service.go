package project

import (
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/costrict/costrict-web/server/internal/models"
	"github.com/costrict/costrict-web/server/internal/notification"
	"github.com/costrict/costrict-web/server/internal/notification/sender"
	"github.com/google/uuid"
	"gorm.io/datatypes"
	"gorm.io/gorm"
)

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
)

type ProjectService struct {
	db              *gorm.DB
	notificationSvc *notification.NotificationService
}

func NewProjectService(db *gorm.DB, notificationSvc *notification.NotificationService) *ProjectService {
	return &ProjectService{db: db, notificationSvc: notificationSvc}
}

func isValidRole(role string) bool {
	return role == RoleAdmin || role == RoleMember
}

func (s *ProjectService) CreateProject(creatorID, name, description string, enabledAt *time.Time) (*models.Project, error) {
	if name == "" {
		return nil, fmt.Errorf("name is required")
	}

	var existing models.Project
	if err := s.db.Where("creator_id = ? AND name = ? AND deleted_at IS NULL", creatorID, name).First(&existing).Error; err == nil {
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

func (s *ProjectService) ListProjects(userID string, includeArchived bool) ([]models.Project, error) {
	var projects []models.Project
	query := s.db.Model(&models.Project{}).
		Joins("JOIN project_members pm ON pm.project_id = projects.id AND pm.deleted_at IS NULL").
		Where("pm.user_id = ?", userID)
	if !includeArchived {
		query = query.Where("projects.archived_at IS NULL")
	}
	if err := query.Order("projects.created_at DESC").Find(&projects).Error; err != nil {
		return nil, err
	}
	return projects, nil
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
		if err := s.db.Where("creator_id = ? AND name = ? AND id <> ? AND deleted_at IS NULL", project.CreatorID, name, projectID).First(&existing).Error; err == nil {
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

func (s *ProjectService) ListMembers(projectID string) ([]models.ProjectMember, error) {
	var members []models.ProjectMember
	if err := s.db.Where("project_id = ?", projectID).Order("joined_at ASC").Find(&members).Error; err != nil {
		return nil, err
	}
	return members, nil
}

func (s *ProjectService) GetMember(projectID, userID string) (*models.ProjectMember, error) {
	var member models.ProjectMember
	if err := s.db.Where("project_id = ? AND user_id = ?", projectID, userID).First(&member).Error; err != nil {
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
	return s.db.Delete(&models.ProjectMember{}, "project_id = ? AND user_id = ?", projectID, targetUserID).Error
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
	return s.db.Model(&models.ProjectMember{}).Where("project_id = ? AND user_id = ?", projectID, targetUserID).Update("role", newRole).Error
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
	if err := s.db.Where("project_id = ? AND invitee_id = ? AND status = ?", projectID, inviteeID, InvitationPending).First(&pending).Error; err == nil {
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
	if err := s.db.Where("project_id = ?", projectID).Order("created_at DESC").Find(&invitations).Error; err != nil {
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
	if err := s.db.Where("invitee_id = ?", userID).Order("created_at DESC").Find(&invitations).Error; err != nil {
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
