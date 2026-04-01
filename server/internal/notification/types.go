package notification

import "github.com/costrict/costrict-web/server/internal/notification/sender"

type NotificationMessage = sender.NotificationMessage
type ConfigField = sender.ConfigField

const (
	EventSessionCompleted        = "session.completed"
	EventSessionFailed           = "session.failed"
	EventSessionAborted          = "session.aborted"
	EventDeviceOffline           = "device.offline"
	EventPermissionRequired      = "permission"
	EventQuestionRequired        = "question"
	EventSessionIdle             = "idle"
	EventSystemNotification       = "system.notification"
	EventProjectInvitationCreated = "project.invitation.created"
	EventProjectInvitationAccepted = "project.invitation.accepted"
	EventProjectInvitationRejected = "project.invitation.rejected"
)

type ProjectInvitationMessage struct {
	InvitationID string `json:"invitationId"`
	ProjectID    string `json:"projectId"`
	ProjectName  string `json:"projectName"`
	InviterName  string `json:"inviterName"`
	Role         string `json:"role"`
	Message      string `json:"message"`
	ExpiresAt    string `json:"expiresAt"`
}

type SystemNotificationMessage struct {
	Type        string `json:"type"`
	Title       string `json:"title"`
	Content     string `json:"content"`
	RelatedID   string `json:"relatedId"`
	RelatedType string `json:"relatedType"`
	ActionURL   string `json:"actionUrl"`
}
