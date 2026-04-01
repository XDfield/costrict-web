package gateway

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/costrict/costrict-web/server/internal/logger"
)

// OwnershipVerifier is a function type that verifies device ownership.
// This avoids circular dependencies with the services package.
type OwnershipVerifier func(deviceID, userID string) error

// WorkspaceResolver resolves workspace information to device and directory.
// This allows SessionService to work with workspace IDs instead of
// requiring deviceID and directory on every call.
type WorkspaceResolver func(workspaceID, userID string) (deviceID string, directory string, err error)

// SessionService handles device session operations by proxying requests
// through the gateway tunnel to the device's Hono HTTP API.
// This service is placed in the gateway package to avoid circular
// dependencies between the services and gateway packages.
type SessionService struct {
	registry         *GatewayRegistry
	client           *Client
	ownershipChecker OwnershipVerifier
	workspaceResolver WorkspaceResolver
}

// NewSessionService creates a new SessionService.
func NewSessionService(registry *GatewayRegistry, client *Client, ownershipChecker OwnershipVerifier, workspaceResolver WorkspaceResolver) *SessionService {
	return &SessionService{
		registry:         registry,
		client:           client,
		ownershipChecker: ownershipChecker,
		workspaceResolver: workspaceResolver,
	}
}

// ---------- Response Types ----------

// SessionInfo represents a session returned from the device.
type SessionInfo struct {
	ID        string `json:"id"`
	Slug      string `json:"slug"`
	ProjectID  string `json:"projectId"`
	WorkspaceID string `json:"workspaceId,omitempty"`
	ParentID   string `json:"parentId,omitempty"`
	Title      string `json:"title"`
	Version    string `json:"version"`
	Directory  string `json:"directory"`
	Summary   *struct {
		Additions int            `json:"additions,omitempty"`
		Deletions int            `json:"deletions,omitempty"`
		Files      int            `json:"files,omitempty"`
		Diffs      []map[string]any `json:"diffs,omitempty"`
	} `json:"summary,omitempty"`
	Share *struct {
		URL string `json:"url,omitempty"`
	} `json:"share,omitempty"`
	Permission map[string]any `json:"permission,omitempty"`
	Time struct {
		Created   int `json:"created"`
		Updated   int `json:"updated"`
		Compacting *int `json:"compacting,omitempty"`
		Archived  *int `json:"archived,omitempty"`
	} `json:"time"`
}

// SessionStatus represents the status of a session on the device.
type SessionStatus struct {
	Type    string `json:"type"` // idle, busy, retry
	Attempt  int    `json:"attempt,omitempty"`
	Message  string  `json:"message,omitempty"`
	Next     int    `json:"next,omitempty"`
}

// MessageInfo represents a message in a session.
type MessageInfo struct {
	ID        string `json:"id"`
	SessionID string `json:"sessionId"`
	Role      string `json:"role"` // user, assistant, system
	CreatedAt int64  `json:"createdAt"`
	UpdatedAt int64  `json:"updatedAt"`
	Data      any    `json:"data"`
}

// MessagePart represents a part of a message.
type MessagePart struct {
	ID        string `json:"id"`
	MessageID string `json:"messageId"`
	SessionID string `json:"sessionId"`
	CreatedAt int64  `json:"createdAt"`
	UpdatedAt int64  `json:"updatedAt"`
	Data      any    `json:"data"`
}

// ---------- Request Types ----------

// CreateSessionRequest is the request body for creating a session.
type CreateSessionRequest struct {
	Title      string `json:"title,omitempty"`
	Directory  string `json:"directory,omitempty"`
	WorkspaceID string `json:"workspaceId,omitempty"`
	ParentID   string `json:"parentId,omitempty"`
}

// SendMessageRequest is the request body for sending a message.
type SendMessageRequest struct {
	Parts      []MessagePart `json:"parts"`
	ProviderID string       `json:"providerId"`
	ModelID    string       `json:"modelId"`
	Agent      any          `json:"agent,omitempty"`
}

// ---------- Public Methods (Device-based) ----------

// CreateSession creates a new session on the specified device.
func (s *SessionService) CreateSession(userID, deviceID, directory string, req CreateSessionRequest) (*SessionInfo, error) {
	if err := s.ownershipChecker(deviceID, userID); err != nil {
		logger.Error("[SessionService] ownership check failed: %v", err)
		return nil, fmt.Errorf("device not found or access denied")
	}

	var result SessionInfo
	err := ProxyDeviceSessionRequest(s.client, s.registry, userID, deviceID, directory, "POST", "/session/", marshalJSON(req), &result)
	if err != nil {
		logger.Error("[SessionService] create session failed: %v", err)
		return nil, err
	}
	return &result, nil
}

// ListSessions lists sessions on the specified device with optional filters.
func (s *SessionService) ListSessions(userID, deviceID, directory string, params map[string]string) ([]SessionInfo, error) {
	if err := s.ownershipChecker(deviceID, userID); err != nil {
		logger.Error("[SessionService] ownership check failed: %v", err)
		return nil, fmt.Errorf("device not found or access denied")
	}

	var result []SessionInfo
	qs := buildQuery(params)
	path := "/session/"
	if qs != "" {
		path += "?" + qs
	}
	if directory != "" {
		// Include directory in query for device's routing context
		if qs != "" {
			path += "&directory=" + directory
		} else {
			path += "?directory=" + directory
		}
	}

	err := ProxyDeviceSessionRequest(s.client, s.registry, userID, deviceID, "", "GET", path, nil, &result)
	if err != nil {
		logger.Error("[SessionService] list sessions failed: %v", err)
		return nil, err
	}
	return result, nil
}

// GetSessionStatus retrieves the status of all sessions on the specified device.
func (s *SessionService) GetSessionStatus(userID, deviceID, directory string) (map[string]SessionStatus, error) {
	if err := s.ownershipChecker(deviceID, userID); err != nil {
		logger.Error("[SessionService] ownership check failed: %v", err)
		return nil, fmt.Errorf("device not found or access denied")
	}

	var result map[string]SessionStatus
	err := ProxyDeviceSessionRequest(s.client, s.registry, userID, deviceID, directory, "GET", "/session/status", nil, &result)
	if err != nil {
		logger.Error("[SessionService] get session status failed: %v", err)
		return nil, err
	}
	return result, nil
}

// GetSession retrieves a specific session from the specified device.
func (s *SessionService) GetSession(userID, deviceID, sessionID string) (*SessionInfo, error) {
	if err := s.ownershipChecker(deviceID, userID); err != nil {
		logger.Error("[SessionService] ownership check failed: %v", err)
		return nil, fmt.Errorf("device not found or access denied")
	}

	var result SessionInfo
	err := ProxyDeviceSessionRequest(s.client, s.registry, userID, deviceID, "", "GET", "/session/"+sessionID, nil, &result)
	if err != nil {
		logger.Error("[SessionService] get session failed: %v", err)
		return nil, err
	}
	return &result, nil
}

// GetSessionMessages retrieves all messages for a session on the specified device.
func (s *SessionService) GetSessionMessages(userID, deviceID, sessionID string, params map[string]string) ([]MessageInfo, error) {
	if err := s.ownershipChecker(deviceID, userID); err != nil {
		logger.Error("[SessionService] ownership check failed: %v", err)
		return nil, fmt.Errorf("device not found or access denied")
	}

	var result []MessageInfo
	qs := buildQuery(params)
	path := "/session/" + sessionID + "/message"
	if qs != "" {
		path += "?" + qs
	}

	err := ProxyDeviceSessionRequest(s.client, s.registry, userID, deviceID, "", "GET", path, nil, &result)
	if err != nil {
		logger.Error("[SessionService] get session messages failed: %v", err)
		return nil, err
	}
	return result, nil
}

// SendMessage sends a message to a session on the specified device and receives the assistant's response.
func (s *SessionService) SendMessage(userID, deviceID, sessionID string, req SendMessageRequest) (*MessageInfo, error) {
	if err := s.ownershipChecker(deviceID, userID); err != nil {
		logger.Error("[SessionService] ownership check failed: %v", err)
		return nil, fmt.Errorf("device not found or access denied")
	}

	var result MessageInfo
	err := ProxyDeviceSessionRequest(s.client, s.registry, userID, deviceID, "", "POST", "/session/"+sessionID+"/message", marshalJSON(req), &result)
	if err != nil {
		logger.Error("[SessionService] send message failed: %v", err)
		return nil, err
	}
	return &result, nil
}

// SendMessageAsync sends a message to a session asynchronously on the specified device.
func (s *SessionService) SendMessageAsync(userID, deviceID, sessionID string, req SendMessageRequest) error {
	if err := s.ownershipChecker(deviceID, userID); err != nil {
		logger.Error("[SessionService] ownership check failed: %v", err)
		return fmt.Errorf("device not found or access denied")
	}

	err := ProxyDeviceSessionRequest(s.client, s.registry, userID, deviceID, "", "POST", "/session/"+sessionID+"/prompt_async", marshalJSON(req), nil)
	if err != nil {
		logger.Error("[SessionService] send async message failed: %v", err)
		return err
	}
	return nil
}

// AbortSession aborts an active session on the specified device.
func (s *SessionService) AbortSession(userID, deviceID, sessionID string) error {
	if err := s.ownershipChecker(deviceID, userID); err != nil {
		logger.Error("[SessionService] ownership check failed: %v", err)
		return fmt.Errorf("device not found or access denied")
	}

	err := ProxyDeviceSessionRequest(s.client, s.registry, userID, deviceID, "", "POST", "/session/"+sessionID+"/abort", nil, nil)
	if err != nil {
		logger.Error("[SessionService] abort session failed: %v", err)
		return err
	}
	return nil
}

// DeleteSession deletes a session and all associated data on the specified device.
func (s *SessionService) DeleteSession(userID, deviceID, sessionID string) error {
	if err := s.ownershipChecker(deviceID, userID); err != nil {
		logger.Error("[SessionService] ownership check failed: %v", err)
		return fmt.Errorf("device not found or access denied")
	}

	err := ProxyDeviceSessionRequest(s.client, s.registry, userID, deviceID, "", "DELETE", "/session/"+sessionID, nil, nil)
	if err != nil {
		logger.Error("[SessionService] delete session failed: %v", err)
		return err
	}
	return nil
}

// ---------- Public Methods (Workspace-based) ----------

// CreateWorkspaceSession creates a new session in the specified workspace.
// The workspace's bound device and default directory are resolved automatically.
func (s *SessionService) CreateWorkspaceSession(userID, workspaceID string, req CreateSessionRequest) (*SessionInfo, error) {
	// Resolve workspace to get deviceID and default directory
	deviceID, directory, err := s.workspaceResolver(workspaceID, userID)
	if err != nil {
		logger.Error("[SessionService] workspace resolution failed: %v", err)
		return nil, err
	}

	// Override directory from request if provided
	if req.Directory == "" {
		req.Directory = directory
	}

	return s.CreateSession(userID, deviceID, req.Directory, req)
}

// ListWorkspaceSessions lists sessions in the specified workspace.
// Uses the workspace's bound device and default directory.
func (s *SessionService) ListWorkspaceSessions(userID, workspaceID string, params map[string]string) ([]SessionInfo, error) {
	// Resolve workspace to get deviceID and default directory
	deviceID, directory, err := s.workspaceResolver(workspaceID, userID)
	if err != nil {
		logger.Error("[SessionService] workspace resolution failed: %v", err)
		return nil, err
	}

	return s.ListSessions(userID, deviceID, directory, params)
}

// GetWorkspaceSessionStatus retrieves the status of all sessions in the specified workspace.
func (s *SessionService) GetWorkspaceSessionStatus(userID, workspaceID string) (map[string]SessionStatus, error) {
	// Resolve workspace to get deviceID and default directory
	deviceID, directory, err := s.workspaceResolver(workspaceID, userID)
	if err != nil {
		logger.Error("[SessionService] workspace resolution failed: %v", err)
		return nil, err
	}

	return s.GetSessionStatus(userID, deviceID, directory)
}

// GetWorkspaceSession retrieves a specific session in the specified workspace.
func (s *SessionService) GetWorkspaceSession(userID, workspaceID, sessionID string) (*SessionInfo, error) {
	// Resolve workspace to get deviceID and default directory
	deviceID, _, err := s.workspaceResolver(workspaceID, userID)
	if err != nil {
		logger.Error("[SessionService] workspace resolution failed: %v", err)
		return nil, err
	}

	return s.GetSession(userID, deviceID, sessionID)
}

// GetWorkspaceSessionMessages retrieves all messages for a session in the specified workspace.
func (s *SessionService) GetWorkspaceSessionMessages(userID, workspaceID, sessionID string, params map[string]string) ([]MessageInfo, error) {
	// Resolve workspace to get deviceID and default directory
	deviceID, _, err := s.workspaceResolver(workspaceID, userID)
	if err != nil {
		logger.Error("[SessionService] workspace resolution failed: %v", err)
		return nil, err
	}

	return s.GetSessionMessages(userID, deviceID, sessionID, params)
}

// SendWorkspaceMessage sends a message to a session in the specified workspace.
func (s *SessionService) SendWorkspaceMessage(userID, workspaceID, sessionID string, req SendMessageRequest) (*MessageInfo, error) {
	// Resolve workspace to get deviceID and default directory
	deviceID, _, err := s.workspaceResolver(workspaceID, userID)
	if err != nil {
		logger.Error("[SessionService] workspace resolution failed: %v", err)
		return nil, err
	}

	return s.SendMessage(userID, deviceID, sessionID, req)
}

// SendWorkspaceMessageAsync sends a message asynchronously to a session in the specified workspace.
func (s *SessionService) SendWorkspaceMessageAsync(userID, workspaceID, sessionID string, req SendMessageRequest) error {
	// Resolve workspace to get deviceID and default directory
	deviceID, _, err := s.workspaceResolver(workspaceID, userID)
	if err != nil {
		logger.Error("[SessionService] workspace resolution failed: %v", err)
		return err
	}

	return s.SendMessageAsync(userID, deviceID, sessionID, req)
}

// AbortWorkspaceSession aborts an active session in the specified workspace.
func (s *SessionService) AbortWorkspaceSession(userID, workspaceID, sessionID string) error {
	// Resolve workspace to get deviceID and default directory
	deviceID, _, err := s.workspaceResolver(workspaceID, userID)
	if err != nil {
		logger.Error("[SessionService] workspace resolution failed: %v", err)
		return err
	}

	return s.AbortSession(userID, deviceID, sessionID)
}

// DeleteWorkspaceSession deletes a session in the specified workspace.
func (s *SessionService) DeleteWorkspaceSession(userID, workspaceID, sessionID string) error {
	// Resolve workspace to get deviceID and default directory
	deviceID, _, err := s.workspaceResolver(workspaceID, userID)
	if err != nil {
		logger.Error("[SessionService] workspace resolution failed: %v", err)
		return err
	}

	return s.DeleteSession(userID, deviceID, sessionID)
}

// ---------- Helper Methods ----------

// marshalJSON is a convenience wrapper around json.Marshal.
func marshalJSON(v any) []byte {
	if v == nil {
		return []byte{}
	}
	b, _ := json.Marshal(v)
	return b
}

// buildQuery constructs a URL query string from a map.
func buildQuery(params map[string]string) string {
	if len(params) == 0 {
		return ""
	}
	var builder strings.Builder
	first := true
	for k, v := range params {
		if !first {
			builder.WriteByte('&')
		}
		builder.WriteString(k)
		builder.WriteByte('=')
		builder.WriteString(v)
		first = false
	}
	return builder.String()
}
