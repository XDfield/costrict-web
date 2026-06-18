package clawagent

import (
	"context"
	"strings"
	"fmt"
)

// UserIntent represents a parsed user intent from natural language.
type UserIntent struct {
	Type         string  // approve_permission, reject_permission, answer_question, ask_clarification, batch_approve
	Confidence   float64 // 0.0 - 1.0
	DeviceID     string
	PermissionID string
	QuestionID   string
	Answers      map[string]any
	Question     string // clarification question
	Reasoning    string // AI reasoning
}

// IntentHandler handles intent recognition from user responses.
type IntentHandler struct {
	deviceProxy *DeviceProxyClient
}

// NewIntentHandler creates a new IntentHandler.
func NewIntentHandler(deviceProxy *DeviceProxyClient) *IntentHandler {
	return &IntentHandler{deviceProxy: deviceProxy}
}

// HandleUserResponse processes a user's natural language response to an event.
func (h *IntentHandler) HandleUserResponse(ctx context.Context, userID, response string, eventContext *EventContext) error {
	intent := h.parseUserIntent(response, eventContext)

	switch intent.Type {
	case "approve_permission":
		return h.approvePermission(ctx, intent, eventContext)
	case "reject_permission":
		return h.rejectPermission(ctx, intent, eventContext)
	case "answer_question":
		return h.answerQuestion(ctx, intent, eventContext)
	case "ask_clarification":
		return h.continueConversation(ctx, intent.Question)
	case "batch_approve":
		return h.batchApprove(ctx, intent, eventContext)
	default:
		return fmt.Errorf("未知意图: %s", intent.Type)
	}
}

func (h *IntentHandler) parseUserIntent(response string, ctx *EventContext) *UserIntent {
	intent := &UserIntent{
		Type:       "unknown",
		Confidence: 0.0,
		DeviceID:   ctx.DeviceID,
	}

	// Simple pattern-based intent recognition for P5
	// In production, this would use the LLM to analyze the response
	if ctx.EventType == "permission" {
		switch {
		case isApproval(response):
			intent.Type = "approve_permission"
			intent.Confidence = 0.9
			intent.PermissionID = ctx.PermissionID
		case isRejection(response):
			intent.Type = "reject_permission"
			intent.Confidence = 0.9
			intent.PermissionID = ctx.PermissionID
		default:
			intent.Type = "ask_clarification"
			intent.Confidence = 0.4
			intent.Question = "请问您是希望批准还是拒绝这个请求？"
		}
	} else if ctx.EventType == "question" {
		intent.Type = "answer_question"
		intent.Confidence = 0.8
		intent.QuestionID = ctx.QuestionID
		intent.Answers = map[string]any{"answer": response}
	}

	return intent
}

var (
	approvalKeywords  = []string{"批准", "同意", "允许", "好", "可以", "确认", "让他执行"}
	rejectionKeywords = []string{"拒绝", "不同意", "不允许", "不行", "不要", "危险", "禁止"}
)

func isApproval(s string) bool {
	lower := strings.ToLower(s)
	// Check English keywords case-insensitively
	if contains(lower, "ok") || contains(lower, "yes") || contains(lower, "approve") || lower == "y" {
		return true
	}
	// Check Chinese keywords
	for _, a := range approvalKeywords {
		if contains(s, a) {
			return true
		}
	}
	return false
}

func isRejection(s string) bool {
	lower := strings.ToLower(s)
	// Check English keywords case-insensitively
	if contains(lower, "no") || contains(lower, "reject") || contains(lower, "deny") {
		return true
	}
	// Check Chinese keywords
	for _, r := range rejectionKeywords {
		if contains(s, r) {
			return true
		}
	}
	return false
}

func contains(s, substr string) bool {
	return len(s) >= len(substr) && containsStr(s, substr)
}

func containsStr(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}

func (h *IntentHandler) approvePermission(ctx context.Context, intent *UserIntent, eventCtx *EventContext) error {
	if intent.PermissionID != "" {
		return h.deviceProxy.ReplyPermission(ctx, intent.DeviceID, intent.PermissionID, "approve")
	}
	return fmt.Errorf("missing permission ID")
}

func (h *IntentHandler) rejectPermission(ctx context.Context, intent *UserIntent, eventCtx *EventContext) error {
	if intent.PermissionID != "" {
		return h.deviceProxy.ReplyPermission(ctx, intent.DeviceID, intent.PermissionID, "reject")
	}
	return fmt.Errorf("missing permission ID")
}

func (h *IntentHandler) answerQuestion(ctx context.Context, intent *UserIntent, eventCtx *EventContext) error {
	if intent.QuestionID != "" {
		answer, _ := intent.Answers["answer"].(string)
		return h.deviceProxy.ReplyQuestion(ctx, intent.DeviceID, intent.QuestionID, answer)
	}
	return fmt.Errorf("missing question ID")
}

func (h *IntentHandler) batchApprove(ctx context.Context, intent *UserIntent, eventCtx *EventContext) error {
	// P5: implement batch approval logic
	return fmt.Errorf("batch approval not yet implemented")
}

func (h *IntentHandler) continueConversation(ctx context.Context, question string) error {
	// P5: continue the AI conversation for clarification
	return nil
}

// EventContext holds context about a notification event.
type EventContext struct {
	EventType    string
	DeviceID     string
	PermissionID string
	QuestionID   string
	Data         map[string]any
}
