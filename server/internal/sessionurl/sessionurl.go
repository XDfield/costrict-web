package sessionurl

import (
	"fmt"

	"github.com/costrict/costrict-web/server/internal/pathutil"
	"gorm.io/gorm"
)

// ResolveWorkspaceID finds the workspace ID for a given device + path.
// deviceID is devices.device_id (external identifier); workspaces.device_id
// stores devices.id (UUID PK), so we JOIN through devices to translate.
// Tries the normalized path first, falls back to the raw path.
func ResolveWorkspaceID(db *gorm.DB, deviceID, path string) (string, error) {
	var workspaceID string
	normalizedPath := pathutil.NormalizeWorkspacePath(path)
	err := db.Table("workspace_directories wd").
		Select("w.id").
		Joins("JOIN workspaces w ON w.id = wd.workspace_id").
		Joins("JOIN devices d ON CAST(d.id AS TEXT) = w.device_id").
		Where("wd.path = ? AND d.device_id = ?", normalizedPath, deviceID).
		Where("wd.deleted_at IS NULL AND w.deleted_at IS NULL AND d.deleted_at IS NULL").
		Scan(&workspaceID).Error
	if err == nil && workspaceID == "" && normalizedPath != path {
		err = db.Table("workspace_directories wd").
			Select("w.id").
			Joins("JOIN workspaces w ON w.id = wd.workspace_id").
			Joins("JOIN devices d ON CAST(d.id AS TEXT) = w.device_id").
			Where("wd.path = ? AND d.device_id = ?", path, deviceID).
			Where("wd.deleted_at IS NULL AND w.deleted_at IS NULL AND d.deleted_at IS NULL").
			Scan(&workspaceID).Error
	}
	if err != nil {
		return "", err
	}
	if workspaceID == "" {
		return "", fmt.Errorf("workspace not found for deviceID=%s, path=%s", deviceID, normalizedPath)
	}
	return workspaceID, nil
}

// Build constructs a frontend session URL. The route is
// `{base}/workspace/{workspaceID}/?session={sessionID}` — session is carried
// as a query parameter, no path segment encoding.
// Returns empty string if any required component is missing.
func Build(cloudBaseURL, workspaceID, sessionID string) string {
	if cloudBaseURL == "" || workspaceID == "" || sessionID == "" {
		return ""
	}
	return fmt.Sprintf("%s/workspace/%s/?session=%s", cloudBaseURL, workspaceID, sessionID)
}
