package notification

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/costrict/costrict-web/server/internal/channel"
	"github.com/costrict/costrict-web/server/internal/gateway"
	"github.com/costrict/costrict-web/server/internal/logger"
	"gorm.io/gorm"
)

// CardStatusUpdater abstracts channel.Service.UpdateInteractiveCard for decoupling.
type CardStatusUpdater interface {
	UpdateInteractiveCard(channelType, responseCode, statusText, action string, cardData []byte, externalUserID string)
}

// ActionHandler processes interactive card callbacks (approve/reject/auto_approve/select)
// and routes them to the appropriate cs-cloud endpoint via gateway proxy.
type ActionHandler struct {
	store       *Store
	db          *gorm.DB
	gwClient    *gateway.Client
	gwRegistry  *gateway.GatewayRegistry
	cardUpdater CardStatusUpdater
}

// NewActionHandler creates a new ActionHandler.
func NewActionHandler(
	store *Store,
	db *gorm.DB,
	gwClient *gateway.Client,
	gwRegistry *gateway.GatewayRegistry,
	cardUpdater CardStatusUpdater,
) *ActionHandler {
	return &ActionHandler{
		store:       store,
		db:          db,
		gwClient:    gwClient,
		gwRegistry:  gwRegistry,
		cardUpdater: cardUpdater,
	}
}

// HandleCallback implements channel.ActionCallbackHandler.
// It is wired into channel.Service.SetActionHandler.
func (h *ActionHandler) HandleCallback(ctx context.Context, action, token, responseCode, externalUserID string) error {
	n, err := h.store.ExecuteAction(token, map[string]any{"action": action})
	if err != nil {
		logger.Error("[action-callback] token invalid or expired: %v", err)
		return err
	}

	// Update card status using stored card data
	if responseCode != "" && n.CardData != nil && len(n.CardData) > 0 {
		statusText := actionStatusText(action)
		go h.cardUpdater.UpdateInteractiveCard("wecom", responseCode, statusText, action, n.CardData, externalUserID)
	}

	// Parse action data for the response
	var actionData map[string]any
	if n.ActionData != nil && len(n.ActionData) > 0 {
		if err := json.Unmarshal(n.ActionData, &actionData); err != nil {
			logger.Error("[action-callback] failed to unmarshal action data: %v", err)
		} else {
			logger.Info("[action-callback] parsed actionData: %+v", actionData)
		}
	}

	// Extract id from action data
	var id string
	if actionData != nil {
		if val, ok := actionData["id"].(string); ok {
			id = val
		}
	}
	logger.Info("[action-callback] type=%s, action=%s, id=%s, sessionID=%s", n.Type, action, id, n.SessionID)

	if id == "" || n.DeviceID == "" {
		logger.Error("[action-callback] missing id or deviceID: id=%q, deviceID=%q", id, n.DeviceID)
		return nil
	}

	// Resolve userID
	userID := n.UserID
	if userID == "" {
		if u, ok := ctx.Value("user_id").(string); ok {
			userID = u
		} else {
			logger.Error("[action-callback] no userID available for proxy request")
			return fmt.Errorf("no userID available")
		}
	}

	// Route to appropriate cs-cloud endpoint based on type
	var proxyPath string
	var requestBody map[string]any

	switch n.Type {
	case "permission":
		proxyPath = fmt.Sprintf("/api/v1/permissions/%s/reply", id)
		isApproved := action == "approve" || action == "auto_approve"
		requestBody = map[string]any{"approved": isApproved}
		logger.Info("[action-callback] proxying to cs-cloud: %s, approved=%v", proxyPath, requestBody["approved"])

		if action == "auto_approve" {
			if n.WorkspaceID != nil && *n.WorkspaceID != "" {
				EnableAutoAccept(h.db, *n.WorkspaceID)
			} else {
				logger.Error("[action-callback] auto_approve but no workspaceID on notification")
			}
			go BatchApproveSessionPermissions(h.store, h.gwClient, h.gwRegistry, h.db, n.UserID, n.DeviceID, n.SessionID, n.ID)
		}

	case "question":
		proxyPath = fmt.Sprintf("/api/v1/questions/%s/reply", id)
		answers := ResolveQuestionAnswer(action, actionData)
		requestBody = map[string]any{"answers": answers}
		logger.Info("[action-callback] proxying to cs-cloud: %s, answers=%v", proxyPath, answers)

	default:
		logger.Error("[action-callback] unknown type: %s", n.Type)
		return nil
	}

	// Proxy through gateway to cs-cloud
	bodyBytes, _ := json.Marshal(requestBody)
	logger.Info("[action-callback] proxying with userID=%s, deviceID=%s, sessionID=%s, path=%s", userID, n.DeviceID, n.SessionID, proxyPath)

	var result map[string]any
	if err := gateway.ProxyDeviceSessionRequest(h.gwClient, h.gwRegistry, userID, n.DeviceID, "", "POST", proxyPath, bodyBytes, &result); err != nil {
		logger.Error("[action-callback] proxy request failed: %v", err)
		return err
	}
	logger.Info("[action-callback] proxy response successful: %+v", result)

	return nil
}

// Callback returns a func matching channel.ActionCallbackHandler signature.
func (h *ActionHandler) Callback() func(ctx context.Context, action, token, responseCode, externalUserID string) error {
	return h.HandleCallback
}

// actionStatusText returns the display text for each action type.
func actionStatusText(action string) string {
	switch action {
	case "approve":
		return "已批准"
	case "reject":
		return "已拒绝"
	case "auto_approve":
		return "已启用自批准"
	default:
		return "已处理"
	}
}

// Verify Callback returns channel.ActionCallbackHandler at compile time.
var _ channel.ActionCallbackHandler = (&ActionHandler{}).Callback()
