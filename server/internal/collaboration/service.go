package collaboration

import (
	"errors"
	"strings"
	"time"

	"github.com/costrict/costrict-web/server/internal/logger"
	"gorm.io/gorm"
)

var (
	ErrSpaceNotFound       = errors.New("space not found")
	ErrSpaceSlugExists     = errors.New("space slug already exists")
	ErrPermissionDenied    = errors.New("permission denied")
	ErrNotSpaceMember      = errors.New("not a space member")
	ErrIssueNotFound       = errors.New("issue not found")
	ErrSquadNotFound       = errors.New("squad not found")
	ErrInvalidRole         = errors.New("invalid role")
	ErrMemberExists        = errors.New("user is already a member")
	ErrCannotRemoveOwner   = errors.New("cannot remove space owner")
	ErrLastAdmin           = errors.New("cannot remove the last admin")
	ErrSquadMemberExists   = errors.New("user is already a squad member")
	ErrSquadMemberNotFound = errors.New("squad member not found")
)

type CollaborationService struct {
	db *gorm.DB
}

func NewCollaborationService(db *gorm.DB) *CollaborationService {
	return &CollaborationService{db: db}
}

// ------------------------------------------------------------------
// Spaces
// ------------------------------------------------------------------

func (s *CollaborationService) CreateSpace(userID string, req *CreateSpaceRequest) (*Space, error) {
	slug := strings.TrimSpace(req.Slug)
	if slug == "" {
		slug = slugify(req.Name)
	}

	var count int64
	if err := s.db.Model(&Space{}).Where("slug = ?", slug).Count(&count).Error; err != nil {
		return nil, err
	}
	if count > 0 {
		return nil, ErrSpaceSlugExists
	}

	space := &Space{
		Name:        strings.TrimSpace(req.Name),
		Slug:        slug,
		Description: req.Description,
	}

	err := s.db.Transaction(func(tx *gorm.DB) error {
		if err := tx.Create(space).Error; err != nil {
			return err
		}
		member := &SpaceMember{
			SpaceID: space.ID,
			UserID:  userID,
			Role:    "owner",
		}
		return tx.Create(member).Error
	})
	if err != nil {
		return nil, err
	}
	return space, nil
}

func (s *CollaborationService) ListSpaces(userID string) ([]Space, error) {
	var spaces []Space
	err := s.db.
		Joins("JOIN space_members ON space_members.space_id = spaces.id").
		Where("space_members.user_id = ? AND spaces.deleted_at IS NULL", userID).
		Order("spaces.updated_at DESC").
		Find(&spaces).Error
	return spaces, err
}

func (s *CollaborationService) GetSpaceBySlug(slug string) (*Space, error) {
	var space Space
	if err := s.db.Where("slug = ? AND deleted_at IS NULL", slug).First(&space).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, ErrSpaceNotFound
		}
		return nil, err
	}
	return &space, nil
}

func (s *CollaborationService) UpdateSpace(spaceID string, req *UpdateSpaceRequest) (*Space, error) {
	var space Space
	if err := s.db.Where("id = ? AND deleted_at IS NULL", spaceID).First(&space).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, ErrSpaceNotFound
		}
		return nil, err
	}
	if req.Name != "" {
		space.Name = strings.TrimSpace(req.Name)
	}
	space.Description = req.Description
	space.UpdatedAt = time.Now()
	if err := s.db.Save(&space).Error; err != nil {
		return nil, err
	}
	return &space, nil
}

func (s *CollaborationService) DeleteSpace(spaceID string) error {
	return s.db.Transaction(func(tx *gorm.DB) error {
		if err := tx.Where("space_id = ?", spaceID).Delete(&SpaceMember{}).Error; err != nil {
			return err
		}
		if err := tx.Where("space_id = ?", spaceID).Delete(&SquadMember{}).Error; err != nil {
			return err
		}
		if err := tx.Where("space_id = ?", spaceID).Delete(&Squad{}).Error; err != nil {
			return err
		}
		if err := tx.Where("space_id = ?", spaceID).Delete(&IssueComment{}).Error; err != nil {
			return err
		}
		if err := tx.Where("space_id = ?", spaceID).Delete(&Issue{}).Error; err != nil {
			return err
		}
		if err := tx.Delete(&Space{ID: spaceID}).Error; err != nil {
			return err
		}
		return nil
	})
}

// ------------------------------------------------------------------
// Space Members
// ------------------------------------------------------------------

func (s *CollaborationService) GetSpaceMember(spaceID, userID string) (*SpaceMember, error) {
	var m SpaceMember
	if err := s.db.Where("space_id = ? AND user_id = ?", spaceID, userID).First(&m).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, ErrNotSpaceMember
		}
		return nil, err
	}
	return &m, nil
}

func (s *CollaborationService) ListSpaceMembers(spaceID string) ([]SpaceMember, error) {
	var members []SpaceMember
	err := s.db.Where("space_id = ?", spaceID).Order("created_at ASC").Find(&members).Error
	return members, err
}

func (s *CollaborationService) AddSpaceMember(spaceID string, req *AddMemberRequest) (*SpaceMember, error) {
	if req.Role != "admin" && req.Role != "member" {
		return nil, ErrInvalidRole
	}
	var count int64
	if err := s.db.Model(&SpaceMember{}).Where("space_id = ? AND user_id = ?", spaceID, req.UserID).Count(&count).Error; err != nil {
		return nil, err
	}
	if count > 0 {
		return nil, ErrMemberExists
	}
	m := &SpaceMember{
		SpaceID: spaceID,
		UserID:  req.UserID,
		Role:    req.Role,
	}
	if err := s.db.Create(m).Error; err != nil {
		return nil, err
	}
	return m, nil
}

func (s *CollaborationService) RemoveSpaceMember(spaceID, userID string) error {
	var member SpaceMember
	if err := s.db.Where("space_id = ? AND user_id = ?", spaceID, userID).First(&member).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return ErrNotSpaceMember
		}
		return err
	}
	if member.Role == "owner" {
		return ErrCannotRemoveOwner
	}
	var adminCount int64
	if err := s.db.Model(&SpaceMember{}).Where("space_id = ? AND role IN (?)", spaceID, []string{"owner", "admin"}).Count(&adminCount).Error; err != nil {
		return err
	}
	if adminCount <= 1 && member.Role == "admin" {
		return ErrLastAdmin
	}
	return s.db.Where("space_id = ? AND user_id = ?", spaceID, userID).Delete(&SpaceMember{}).Error
}

// ------------------------------------------------------------------
// Issues
// ------------------------------------------------------------------

func (s *CollaborationService) CreateIssue(spaceID, creatorID string, req *CreateIssueRequest) (*Issue, error) {
	var maxNumber int
	row := s.db.Raw("SELECT COALESCE(MAX(number), 0) + 1 FROM issues WHERE space_id = ?", spaceID).Row()
	if err := row.Scan(&maxNumber); err != nil {
		logger.Warn("[collaboration] failed to get next issue number: %v", err)
		maxNumber = 1
	}

	issue := &Issue{
		SpaceID:       spaceID,
		Number:        maxNumber,
		Title:         strings.TrimSpace(req.Title),
		Description:   req.Description,
		Status:        defaultString(req.Status, "backlog"),
		Priority:      defaultString(req.Priority, "none"),
		AssigneeType:  req.AssigneeType,
		AssigneeID:    req.AssigneeID,
		CreatorID:     creatorID,
		ParentIssueID: req.ParentIssueID,
		Position:      req.Position,
		DueDate:       req.DueDate,
		Metadata:      req.Metadata,
	}
	if err := s.db.Create(issue).Error; err != nil {
		return nil, err
	}
	return issue, nil
}

func (s *CollaborationService) ListIssues(spaceID string, status, priority, assigneeID string) ([]Issue, error) {
	var issues []Issue
	q := s.db.Where("space_id = ? AND deleted_at IS NULL", spaceID)
	if status != "" {
		q = q.Where("status = ?", status)
	}
	if priority != "" {
		q = q.Where("priority = ?", priority)
	}
	if assigneeID != "" {
		q = q.Where("assignee_id = ?", assigneeID)
	}
	err := q.Order("position ASC, created_at DESC").Find(&issues).Error
	return issues, err
}

func (s *CollaborationService) GetIssue(spaceID, issueID string) (*Issue, error) {
	var issue Issue
	if err := s.db.Where("id = ? AND space_id = ? AND deleted_at IS NULL", issueID, spaceID).First(&issue).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, ErrIssueNotFound
		}
		return nil, err
	}
	return &issue, nil
}

func (s *CollaborationService) UpdateIssue(spaceID, issueID string, req *UpdateIssueRequest) (*Issue, error) {
	issue, err := s.GetIssue(spaceID, issueID)
	if err != nil {
		return nil, err
	}
	if req.Title != nil {
		issue.Title = strings.TrimSpace(*req.Title)
	}
	if req.Description != nil {
		issue.Description = *req.Description
	}
	if req.Status != nil {
		issue.Status = *req.Status
	}
	if req.Priority != nil {
		issue.Priority = *req.Priority
	}
	if req.AssigneeType != nil {
		issue.AssigneeType = req.AssigneeType
	}
	if req.AssigneeID != nil {
		issue.AssigneeID = req.AssigneeID
	}
	if req.ParentIssueID != nil {
		issue.ParentIssueID = req.ParentIssueID
	}
	if req.Position != nil {
		issue.Position = *req.Position
	}
	if req.DueDate != nil {
		issue.DueDate = req.DueDate
	}
	if req.Metadata != nil {
		issue.Metadata = req.Metadata
	}
	issue.UpdatedAt = time.Now()
	if err := s.db.Save(issue).Error; err != nil {
		return nil, err
	}
	return issue, nil
}

func (s *CollaborationService) DeleteIssue(spaceID, issueID string) error {
	result := s.db.Where("id = ? AND space_id = ?", issueID, spaceID).Delete(&Issue{})
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected == 0 {
		return ErrIssueNotFound
	}
	return nil
}

// ------------------------------------------------------------------
// Issue Comments
// ------------------------------------------------------------------

func (s *CollaborationService) CreateComment(issueID, userID string, req *CreateCommentRequest) (*IssueComment, error) {
	comment := &IssueComment{
		IssueID: issueID,
		UserID:  userID,
		Content: strings.TrimSpace(req.Content),
	}
	if err := s.db.Create(comment).Error; err != nil {
		return nil, err
	}
	return comment, nil
}

func (s *CollaborationService) ListComments(issueID string) ([]IssueComment, error) {
	var comments []IssueComment
	err := s.db.Where("issue_id = ?", issueID).Order("created_at ASC").Find(&comments).Error
	return comments, err
}

// ------------------------------------------------------------------
// Squads
// ------------------------------------------------------------------

func (s *CollaborationService) CreateSquad(spaceID string, req *CreateSquadRequest) (*Squad, error) {
	squad := &Squad{
		SpaceID:      spaceID,
		Name:         strings.TrimSpace(req.Name),
		Description:  req.Description,
		LeaderID:     req.LeaderID,
		Instructions: req.Instructions,
		AvatarURL:    req.AvatarURL,
	}
	if err := s.db.Create(squad).Error; err != nil {
		return nil, err
	}
	return squad, nil
}

func (s *CollaborationService) ListSquads(spaceID string, includeArchived bool) ([]Squad, error) {
	var squads []Squad
	q := s.db.Where("space_id = ?", spaceID)
	if !includeArchived {
		q = q.Where("archived_at IS NULL")
	}
	err := q.Order("created_at DESC").Find(&squads).Error
	return squads, err
}

func (s *CollaborationService) GetSquad(spaceID, squadID string) (*Squad, error) {
	var squad Squad
	if err := s.db.Where("id = ? AND space_id = ?", squadID, spaceID).First(&squad).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, ErrSquadNotFound
		}
		return nil, err
	}
	return &squad, nil
}

func (s *CollaborationService) UpdateSquad(spaceID, squadID string, req *UpdateSquadRequest) (*Squad, error) {
	squad, err := s.GetSquad(spaceID, squadID)
	if err != nil {
		return nil, err
	}
	if req.Name != nil {
		squad.Name = strings.TrimSpace(*req.Name)
	}
	if req.Description != nil {
		squad.Description = *req.Description
	}
	if req.LeaderID != nil {
		squad.LeaderID = *req.LeaderID
	}
	if req.Instructions != nil {
		squad.Instructions = *req.Instructions
	}
	if req.AvatarURL != nil {
		squad.AvatarURL = *req.AvatarURL
	}
	if req.ArchivedAt != nil {
		squad.ArchivedAt = req.ArchivedAt
	}
	squad.UpdatedAt = time.Now()
	if err := s.db.Save(squad).Error; err != nil {
		return nil, err
	}
	return squad, nil
}

func (s *CollaborationService) DeleteSquad(spaceID, squadID string) error {
	return s.db.Transaction(func(tx *gorm.DB) error {
		if err := tx.Where("squad_id = ?", squadID).Delete(&SquadMember{}).Error; err != nil {
			return err
		}
		result := tx.Where("id = ? AND space_id = ?", squadID, spaceID).Delete(&Squad{})
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected == 0 {
			return ErrSquadNotFound
		}
		return nil
	})
}

// ------------------------------------------------------------------
// Squad Members
// ------------------------------------------------------------------

func (s *CollaborationService) ListSquadMembers(squadID string) ([]SquadMember, error) {
	var members []SquadMember
	err := s.db.Where("squad_id = ?", squadID).Order("joined_at ASC").Find(&members).Error
	return members, err
}

func (s *CollaborationService) AddSquadMember(squadID string, req *AddSquadMemberRequest) (*SquadMember, error) {
	var count int64
	if err := s.db.Model(&SquadMember{}).Where("squad_id = ? AND user_id = ?", squadID, req.UserID).Count(&count).Error; err != nil {
		return nil, err
	}
	if count > 0 {
		return nil, ErrSquadMemberExists
	}
	role := req.Role
	if role == "" {
		role = "member"
	}
	m := &SquadMember{
		SquadID:  squadID,
		UserID:   req.UserID,
		Role:     role,
		JoinedAt: time.Now(),
	}
	if err := s.db.Create(m).Error; err != nil {
		return nil, err
	}
	return m, nil
}

func (s *CollaborationService) RemoveSquadMember(squadID, userID string) error {
	result := s.db.Where("squad_id = ? AND user_id = ?", squadID, userID).Delete(&SquadMember{})
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected == 0 {
		return ErrSquadMemberNotFound
	}
	return nil
}

// ------------------------------------------------------------------
// Helpers
// ------------------------------------------------------------------

func defaultString(val, fallback string) string {
	if strings.TrimSpace(val) == "" {
		return fallback
	}
	return val
}

func slugify(name string) string {
	// Simple slugify: lowercase, replace spaces with hyphens.
	// Production code may want a more robust implementation.
	s := strings.ToLower(strings.TrimSpace(name))
	s = strings.ReplaceAll(s, " ", "-")
	var sb strings.Builder
	for _, r := range s {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-' {
			sb.WriteRune(r)
		}
	}
	return sb.String()
}
