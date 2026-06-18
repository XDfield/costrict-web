package clawagent

import (
	"testing"
)

func TestIsApproval(t *testing.T) {
	tests := []struct {
		input string
		want  bool
	}{
		{"批准", true},
		{"批准执行", true},
		{"同意", true},
		{"允许", true},
		{"好", true},
		{"可以", true},
		{"ok", true},
		{"OK", true},
		{"yes", true},
		{"Yes", true},
		{"approve", true},
		{"y", true},
		{"确认", true},
		{"让他执行", true},
		{"拒绝", false},
		{"不行", false},
		{"no", false},
		{"危险", false},
		{"随便", false},
		{"帮我看看是什么", false},
		{"", false},
		{"abc", false},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			got := isApproval(tt.input)
			if got != tt.want {
				t.Errorf("isApproval(%q) = %v, want %v", tt.input, got, tt.want)
			}
		})
	}
}

func TestIsRejection(t *testing.T) {
	tests := []struct {
		input string
		want  bool
	}{
		{"拒绝", true},
		{"拒绝执行", true},
		{"不同意", true},
		{"不允许", true},
		{"不行", true},
		{"不要", true},
		{"no", true},
		{"No", true},
		{"reject", true},
		{"deny", true},
		{"危险", true},
		{"禁止", true},
		{"批准", false},
		{"同意", false},
		{"可以", false},
		{"随便", false},
		{"帮我看看", false},
		{"", false},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			got := isRejection(tt.input)
			if got != tt.want {
				t.Errorf("isRejection(%q) = %v, want %v", tt.input, got, tt.want)
			}
		})
	}
}

func TestContains(t *testing.T) {
	tests := []struct {
		s      string
		substr string
		want   bool
	}{
		{"hello world", "world", true},
		{"hello world", "hello", true},
		{"hello world", "xyz", false},
		{"", "", true},
		{"abc", "", true},
		{"", "abc", false},
		{"你好世界", "世界", true},
		{"你好世界", "你好", true},
		{"你好世界", "再见", false},
	}

	for _, tt := range tests {
		t.Run(tt.s+"_"+tt.substr, func(t *testing.T) {
			got := contains(tt.s, tt.substr)
			if got != tt.want {
				t.Errorf("contains(%q, %q) = %v, want %v", tt.s, tt.substr, got, tt.want)
			}
		})
	}
}

func TestParseUserIntent_PermissionApproval(t *testing.T) {
	h := &IntentHandler{}

	ctx := &EventContext{
		EventType:    "permission",
		DeviceID:     "dev-001",
		PermissionID: "perm-123",
	}

	// Approval scenarios
	approvalTests := []string{
		"批准",
		"可以执行",
		"同意",
		"ok，让他执行",
		"yes",
	}

	for _, input := range approvalTests {
		t.Run(input, func(t *testing.T) {
			intent := h.parseUserIntent(input, ctx)
			if intent.Type != "approve_permission" {
				t.Errorf("parseUserIntent(%q).Type = %q, want %q", input, intent.Type, "approve_permission")
			}
			if intent.PermissionID != "perm-123" {
				t.Errorf("PermissionID = %q, want %q", intent.PermissionID, "perm-123")
			}
			if intent.Confidence < 0.5 {
				t.Errorf("Confidence = %f, want >= 0.5", intent.Confidence)
			}
		})
	}
}

func TestParseUserIntent_PermissionRejection(t *testing.T) {
	h := &IntentHandler{}

	ctx := &EventContext{
		EventType:    "permission",
		DeviceID:     "dev-001",
		PermissionID: "perm-456",
	}

	rejectionTests := []string{
		"拒绝",
		"不行",
		"不要执行",
		"no",
		"危险",
	}

	for _, input := range rejectionTests {
		t.Run(input, func(t *testing.T) {
			intent := h.parseUserIntent(input, ctx)
			if intent.Type != "reject_permission" {
				t.Errorf("parseUserIntent(%q).Type = %q, want %q", input, intent.Type, "reject_permission")
			}
			if intent.PermissionID != "perm-456" {
				t.Errorf("PermissionID = %q", intent.PermissionID)
			}
		})
	}
}

func TestParseUserIntent_Question(t *testing.T) {
	h := &IntentHandler{}

	ctx := &EventContext{
		EventType:  "question",
		DeviceID:   "dev-001",
		QuestionID: "q-789",
		Data:       map[string]any{"question": "选择部署环境", "options": []string{"生产", "测试"}},
	}

	intent := h.parseUserIntent("用生产环境", ctx)
	if intent.Type != "answer_question" {
		t.Errorf("parseUserIntent for question.Type = %q, want %q", intent.Type, "answer_question")
	}
	if intent.QuestionID != "q-789" {
		t.Errorf("QuestionID = %q", intent.QuestionID)
	}
	if intent.Answers == nil || intent.Answers["answer"] != "用生产环境" {
		t.Errorf("Answers = %v", intent.Answers)
	}
}

func TestParseUserIntent_Unknown(t *testing.T) {
	h := &IntentHandler{}

	ctx := &EventContext{
		EventType:    "permission",
		DeviceID:     "dev-001",
		PermissionID: "perm-123",
	}

	// Ambiguous input should trigger clarification
	intent := h.parseUserIntent("帮我看看是什么请求", ctx)
	if intent.Type != "ask_clarification" {
		t.Errorf("Type = %q, want %q", intent.Type, "ask_clarification")
	}
	if intent.Question == "" {
		t.Error("clarification Question should not be empty")
	}
}

func TestParseUserIntent_EmptyInput(t *testing.T) {
	h := &IntentHandler{}

	ctx := &EventContext{
		EventType:    "permission",
		DeviceID:     "dev-001",
		PermissionID: "perm-123",
	}

	intent := h.parseUserIntent("", ctx)
	if intent.Confidence != 0.4 {
		t.Errorf("Confidence for empty input = %f, want 0.4", intent.Confidence)
	}
}
