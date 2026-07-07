package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"

	"github.com/costrict/costrict-web/server/internal/models"
	"gorm.io/gorm"
)

// PermissionTool handles reply_permission tool calls.
type PermissionTool struct{}

// NewPermissionTool creates a permission tool.
func NewPermissionTool() *PermissionTool {
	return &PermissionTool{}
}

func (t *PermissionTool) Name() string {
	return "reply_permission"
}

func (t *PermissionTool) Definition() Definition {
	return Definition{
		Name:        "reply_permission",
		Description: "回复权限请求，批准或拒绝。permissionID 必须用申请来源段里给出的真实 ID，不要自己编。",
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"permissionID": map[string]any{
					"type":        "string",
					"description": "权限请求的 ID，从申请来源段取真实 ID",
				},
				"approved": map[string]any{
					"type":        "boolean",
					"description": "是否批准该权限请求",
				},
				"enableAutoAccept": map[string]any{
					"type":        "boolean",
					"description": "可选，默认 false。设为 true 会把该 workspace 的 autoAccept 配置打开——后续该 workspace 的权限请求由系统自动批准，不再询问。属于持久配置变更，必须保守：(1) 仅当用户当前明确表达「以后都自动同意」「记住这次选择」「别再问我了」等持久化意图时才设为 true，用户只对当前这一次表态（「这次允许」「批准一下」）时绝对不要设；(2) 仅当当前申请是低风险常规操作（读目录/查看状态/跑测试等），不可逆或高风险动作（删除/覆盖/推送/外发）绝不要擅自开启。即使你判断该 workspace 适合自动接受，也不能自作主张开启——正确做法是先向用户推荐「这是 X workspace 常规操作，要不要开启自动接受？」等用户明确同意后再带 enableAutoAccept=true 调用。",
				},
			},
			"required": []string{"permissionID", "approved"},
		},
	}
}

func (t *PermissionTool) Execute(ctx context.Context, argsJSON string, toolCtx *Context) (string, error) {
	slog.Debug("[tool] reply_permission: execute", "args", argsJSON, "deviceID", toolCtx.DeviceID)

	var args struct {
		PermissionID      string `json:"permissionID"`
		Approved          bool   `json:"approved"`
		EnableAutoAccept  bool   `json:"enableAutoAccept"`
	}
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		slog.Error("[tool] reply_permission: parse args failed", "args", argsJSON, "error", err)
		return "", fmt.Errorf("parse args: %w", err)
	}
	slog.Debug("[tool] reply_permission: parsed args",
		"permissionID", args.PermissionID,
		"approved", args.Approved,
		"enableAutoAccept", args.EnableAutoAccept,
	)

	if toolCtx.DeviceID == "" {
		slog.Error("[tool] reply_permission: deviceID is empty", "args", argsJSON)
		return "", fmt.Errorf("cannot resolve device ID for permission reply")
	}

	if err := toolCtx.DeviceProxy.ReplyPermission(ctx, toolCtx.DeviceID, args.PermissionID, args.Approved, toolCtx.Directory); err != nil {
		slog.Error("[tool] reply_permission: device proxy call failed", "permissionID", args.PermissionID, "deviceID", toolCtx.DeviceID, "approved", args.Approved, "error", err)
		return "", fmt.Errorf("device proxy reply permission failed: %w", err)
	}
	slog.Debug("[tool] reply_permission: device proxy call succeeded", "permissionID", args.PermissionID, "deviceID", toolCtx.DeviceID)

	action := "已拒绝"
	if args.Approved {
		action = "已批准"
	}
	result := fmt.Sprintf("权限请求 %s 已被%s", args.PermissionID, action)

	// Optional: flip the workspace's autoAccept flag so future permission
	// requests in this workspace are auto-approved by the dispatcher. Only
	// do this when the AI explicitly sets enableAutoAccept=true based on the
	// user expressing a "remember this choice" intent.
	if args.EnableAutoAccept {
		if wsID, err := resolveWorkspaceIDForAutoAccept(toolCtx); err != nil {
			slog.Warn("[tool] reply_permission: enableAutoAccept set but workspace lookup failed",
				"userID", toolCtx.UserID, "deviceID", toolCtx.DeviceID, "directory", toolCtx.Directory, "error", err)
			result += "；但开启自动接受失败：" + err.Error()
		} else if wsID == "" {
			slog.Warn("[tool] reply_permission: enableAutoAccept set but no workspace bound to this directory",
				"userID", toolCtx.UserID, "deviceID", toolCtx.DeviceID, "directory", toolCtx.Directory)
			result += "；但当前目录未绑定 workspace，无法开启自动接受"
		} else if err := enableWorkspaceAutoAccept(toolCtx.DB, wsID); err != nil {
			slog.Error("[tool] reply_permission: failed to enable autoAccept on workspace",
				"workspaceID", wsID, "error", err)
			result += "；但开启自动接受失败：" + err.Error()
		} else {
			slog.Info("[tool] reply_permission: enabled autoAccept on workspace",
				"workspaceID", wsID, "userID", toolCtx.UserID)
			result += "；已为该 workspace 开启自动接受，后续权限请求将自动批准"
			// Drain MUST run before MarkProcessed below — MarkEventResolvedByID
			// marks the whole batch event row resolved, which would hide
			// sibling permissionIDs in the same batch from the drainer's
			// LoadAllPendingEvents query. Calling drain first ensures batch
			// siblings (e.g. the 2nd/3rd permission in a permission_batch
			// event) get auto-approved on the device side.
			result += buildDrainSuffix(ctx, toolCtx, args.PermissionID)
		}
	}

	// Mark as processed LAST — after drain has grabbed its pending snapshot.
	// See note above: MarkEventResolvedByID resolves the entire batch row.
	if toolCtx.MarkProcessed != nil {
		toolCtx.MarkProcessed()
	} else {
		slog.Warn("[tool] reply_permission: MarkProcessed is nil", "permissionID", args.PermissionID)
	}

	slog.Debug("[tool] reply_permission: completed", "permissionID", args.PermissionID, "result", result)
	return result, nil
}

// resolveWorkspaceIDForAutoAccept mirrors the dispatcher's workspace lookup:
// device_id (hash) → devices.id (internal) → workspace_directories JOIN workspaces,
// matched on the normalized path. Returns "" when the directory isn't bound to
// any workspace (which is a valid, non-error outcome).
func resolveWorkspaceIDForAutoAccept(toolCtx *Context) (string, error) {
	if toolCtx.DB == nil {
		return "", fmt.Errorf("DB not available in tool context")
	}
	if toolCtx.UserID == "" || toolCtx.Directory == "" {
		return "", fmt.Errorf("missing userID or directory for workspace lookup")
	}

	var dev models.Device
	if err := toolCtx.DB.Where("device_id = ?", toolCtx.DeviceID).First(&dev).Error; err != nil {
		return "", fmt.Errorf("lookup device: %w", err)
	}

	normalizedPath := strings.ReplaceAll(toolCtx.Directory, "\\", "/")
	var ws models.Workspace
	if err := toolCtx.DB.
		Joins("JOIN workspace_directories ON workspace_directories.workspace_id = workspaces.id").
		Where("workspaces.user_id = ? AND workspaces.device_id = ?", toolCtx.UserID, dev.ID).
		Where("REPLACE(workspace_directories.path, chr(92), chr(47)) = ?", normalizedPath).
		Where("workspace_directories.deleted_at IS NULL").
		First(&ws).Error; err != nil {
		return "", nil // not bound — caller treats as soft-fail
	}
	return ws.ID, nil
}

// enableWorkspaceAutoAccept flips settings.autoAccept=true on the workspace.
// Uses jsonb concatenation so existing settings keys are preserved. Mirrors the
// SQL in notification.EnableAutoAccept — kept inline to avoid pulling the
// notification package (and its transitive deps) into the tools package.
func enableWorkspaceAutoAccept(db *gorm.DB, workspaceID string) error {
	if db == nil {
		return fmt.Errorf("DB not available")
	}
	res := db.Exec(`UPDATE workspaces SET settings = COALESCE(settings, '{}'::jsonb) || '{"autoAccept": true}'::jsonb WHERE id = ? AND deleted_at IS NULL`, workspaceID)
	if res.Error != nil {
		return fmt.Errorf("update workspace settings: %w", res.Error)
	}
	return nil
}

// buildDrainSuffix invokes the session-permission drain (if wired) and returns
// the user-facing result suffix describing what happened. Returns empty string
// when there's nothing notable to report (drain not wired, or wired but found
// 0 siblings). Extracted from Execute so the drain contract can be tested in
// isolation — the workspace-resolution path uses Postgres-specific SQL that
// sqlite test DBs can't run, which would otherwise block drain testing.
func buildDrainSuffix(ctx context.Context, toolCtx *Context, permissionID string) string {
	if toolCtx == nil || toolCtx.DrainSessionPermissions == nil {
		return ""
	}
	drainedIDs, drainErr := toolCtx.DrainSessionPermissions(ctx, permissionID)
	if drainErr != nil {
		slog.Warn("[tool] reply_permission: drain partial failure",
			"sessionID", toolCtx.SessionID, "excludePermissionID", permissionID, "error", drainErr)
		return fmt.Sprintf("；批量处理其它待审权限时部分失败: %v", drainErr)
	}
	if len(drainedIDs) > 0 {
		slog.Info("[tool] reply_permission: drained additional pending permissions",
			"sessionID", toolCtx.SessionID, "excludePermissionID", permissionID,
			"drained", len(drainedIDs), "drainedIDs", drainedIDs)
		return fmt.Sprintf("；同时已批量批准 %d 条同类待审权限", len(drainedIDs))
	}
	return ""
}
