package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
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
			},
			"required": []string{"permissionID", "approved"},
		},
	}
}

func (t *PermissionTool) Execute(ctx context.Context, argsJSON string, toolCtx *Context) (string, error) {
	slog.Debug("[tool] reply_permission: execute", "args", argsJSON, "deviceID", toolCtx.DeviceID)

	var args struct {
		PermissionID string `json:"permissionID"`
		Approved     bool   `json:"approved"`
	}
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		slog.Error("[tool] reply_permission: parse args failed", "args", argsJSON, "error", err)
		return "", fmt.Errorf("parse args: %w", err)
	}
	slog.Debug("[tool] reply_permission: parsed args", "permissionID", args.PermissionID, "approved", args.Approved)

	if toolCtx.DeviceID == "" {
		slog.Error("[tool] reply_permission: deviceID is empty", "args", argsJSON)
		return "", fmt.Errorf("cannot resolve device ID for permission reply")
	}

	if err := toolCtx.DeviceProxy.ReplyPermission(ctx, toolCtx.DeviceID, args.PermissionID, args.Approved, toolCtx.Directory); err != nil {
		slog.Error("[tool] reply_permission: device proxy call failed", "permissionID", args.PermissionID, "deviceID", toolCtx.DeviceID, "approved", args.Approved, "error", err)
		return "", fmt.Errorf("device proxy reply permission failed: %w", err)
	}
	slog.Debug("[tool] reply_permission: device proxy call succeeded", "permissionID", args.PermissionID, "deviceID", toolCtx.DeviceID)

	// Mark as processed on success
	if toolCtx.MarkProcessed != nil {
		slog.Debug("[tool] reply_permission: calling MarkProcessed", "permissionID", args.PermissionID)
		toolCtx.MarkProcessed()
		slog.Debug("[tool] reply_permission: MarkProcessed done", "permissionID", args.PermissionID)
	} else {
		slog.Warn("[tool] reply_permission: MarkProcessed is nil", "permissionID", args.PermissionID)
	}

	action := "已拒绝"
	if args.Approved {
		action = "已批准"
	}
	result := fmt.Sprintf("权限请求 %s 已被%s", args.PermissionID, action)
	slog.Debug("[tool] reply_permission: completed", "permissionID", args.PermissionID, "result", result)
	return result, nil
}
