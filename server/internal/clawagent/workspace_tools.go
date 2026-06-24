package clawagent

// WorkspaceInfo represents a workspace available for delegation.
type WorkspaceInfo struct {
	ID            string   `json:"workspace_id"`
	Name          string   `json:"name"`
	DeviceID      string   `json:"device_id"`
	DeviceStatus  string   `json:"device_status"`
	Directories   []string `json:"directories"`
	IsDefault     bool     `json:"is_default"`
}

// WorkspaceDelegateInput is the input for the workspace_delegate tool.
type WorkspaceDelegateInput struct {
	WorkspaceID string `json:"workspace_id" description:"目标工作区 ID"`
	Task        string `json:"task" description:"任务描述"`
	Skill       string `json:"skill,omitempty" description:"指定 agent 模式（如 build/code）"`
	Blocking    bool   `json:"blocking,omitempty" description:"是否等待完成（默认 true）"`
}

// WorkspaceDelegateOutput is the output of the workspace_delegate tool.
type WorkspaceDelegateOutput struct {
	TaskID         string `json:"task_id"`
	WorkspaceID    string `json:"workspace_id"`
	DeviceID       string `json:"device_id"`
	ConversationID string `json:"conversation_id"`
	Status         string `json:"status"`
	Output         string `json:"output,omitempty"`
	Error          string `json:"error,omitempty"`
}

// WorkspaceInfoInput is the input for the workspace_info tool.
type WorkspaceInfoInput struct {
	WorkspaceID string `json:"workspace_id"`
}

// WorkspaceInfoOutput is the output of the workspace_info tool.
type WorkspaceInfoOutput struct {
	WorkspaceName string     `json:"workspace_name"`
	DeviceID      string     `json:"device_id"`
	Directory     string     `json:"directory"`
	VCS           *VCSInfo   `json:"vcs,omitempty"`
	Files         []FileInfo `json:"files,omitempty"`
	Health        string     `json:"health"`
}
