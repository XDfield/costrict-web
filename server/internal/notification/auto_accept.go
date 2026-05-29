package notification

import (
	"encoding/json"
	"fmt"
	"log"

	"github.com/costrict/costrict-web/server/internal/gateway"
	"github.com/costrict/costrict-web/server/internal/models"
	"gorm.io/gorm"
)

// EnableAutoAccept marks a specific workspace as autoAccept=true.
func EnableAutoAccept(db *gorm.DB, workspaceID string) {
	log.Printf("[auto-accept] enabling for workspaceID=%s", workspaceID)
	sql := `UPDATE workspaces SET settings = COALESCE(settings, '{}'::jsonb) || '{"autoAccept": true}'::jsonb WHERE id = ? AND deleted_at IS NULL`

	result := db.Exec(sql, workspaceID)
	if result.Error != nil {
		log.Printf("[auto-accept] failed to update workspace settings: %v", result.Error)
	} else {
		log.Printf("[auto-accept] enabled autoAccept for workspace=%s rows=%d", workspaceID, result.RowsAffected)
	}
}

// BatchApproveSessionPermissions approves all pending permissions for a session.
func BatchApproveSessionPermissions(store *Store, gwClient *gateway.Client, gwRegistry *gateway.GatewayRegistry, db *gorm.DB, userID, deviceID, sessionID, excludeID string) {
	var pending []models.SystemNotification
	if err := db.Where("session_id = ? AND type = 'permission' AND status = 'pending' AND deleted_at IS NULL", sessionID).
		Find(&pending).Error; err != nil {
		log.Printf("[auto-accept] failed to query pending permissions: %v", err)
		return
	}
	for _, p := range pending {
		if p.ID == excludeID {
			continue
		}
		store.ExecuteAction(p.ActionToken, map[string]any{"action": "auto_approve"})

		var ad map[string]any
		if p.ActionData != nil {
			json.Unmarshal(p.ActionData, &ad)
		}
		id, _ := ad["id"].(string)
		if id == "" {
			continue
		}
		proxyPath := fmt.Sprintf("/api/v1/permissions/%s/reply", id)
		bodyBytes, _ := json.Marshal(map[string]any{"approved": true})
		var result map[string]any
		if err := gateway.ProxyDeviceSessionRequest(gwClient, gwRegistry, userID, deviceID, "", "POST", proxyPath, bodyBytes, &result); err != nil {
			log.Printf("[auto-accept] failed to approve permission %s: %v", id, err)
		} else {
			log.Printf("[auto-accept] approved pending permission %s", id)
		}
	}
}
