package clawagent

import (
	"context"
	"fmt"
)

// AIEventRequest represents an event forwarded from the dispatcher.
type AIEventRequest struct {
	UserID     string
	EventType  string // "permission" or "question"
	SessionID  string
	DeviceID   string
	ActionData map[string]any
	Path       string
}

// EventHandler handles AI-driven event processing for notifications.
type EventHandler struct {
	runtime *ClawAgentRuntime
}

// NewEventHandler creates a new EventHandler.
func NewEventHandler(runtime *ClawAgentRuntime) *EventHandler {
	return &EventHandler{runtime: runtime}
}

// HandleAIEvent processes an event through the AI runtime.
func (h *EventHandler) HandleAIEvent(ctx context.Context, req AIEventRequest) error {
	eventDesc := h.describeEvent(req)

	message := fmt.Sprintf("[系统事件] %s\n请根据上下文，用自然语言向用户说明此情况，并询问如何处理。", eventDesc)

	baseKey := fmt.Sprintf("agent:clawagent:event:%s:%s", req.EventType, req.SessionID)
	sessionID, err := h.runtime.resolveActiveSession(req.UserID, baseKey, "event")
	if err != nil {
		return fmt.Errorf("resolve event session: %w", err)
	}

	eventCh, err := h.runtime.runner.Run(ctx, req.UserID, sessionID, message)
	if err != nil {
		return fmt.Errorf("AI event run failed: %w", err)
	}

	// TODO: In P5, wire this to a channel Sender for IM output
	go func() {
		for evt := range eventCh {
			if evt.IsFinal {
				break
			}
		}
	}()

	return nil
}

// describeEvent generates a natural language description of the event.
func (h *EventHandler) describeEvent(req AIEventRequest) string {
	switch req.EventType {
	case "permission":
		return h.describePermission(req)
	case "question":
		return h.describeQuestion(req)
	default:
		return fmt.Sprintf("未知事件类型: %s", req.EventType)
	}
}

func (h *EventHandler) describePermission(req AIEventRequest) string {
	permType, _ := req.ActionData["permission"].(string)
	cmd, _ := req.ActionData["command"].(string)
	if cmd != "" && permType != "" {
		return fmt.Sprintf("设备端请求执行 %s 权限: %s", permType, cmd)
	}
	if cmd != "" {
		return fmt.Sprintf("设备端请求执行命令: %s", cmd)
	}
	return "设备端发出一个权限请求"
}

func (h *EventHandler) describeQuestion(req AIEventRequest) string {
	question, _ := req.ActionData["question"].(string)
	if question != "" {
		return fmt.Sprintf("设备端提问: %s", question)
	}
	return "设备端发出一个提问"
}

// EventForwarder forwards events from the dispatcher to the AI runtime.
type EventForwarder struct {
	eventHandler *EventHandler
}

// NewEventForwarder creates a new EventForwarder.
func NewEventForwarder(eventHandler *EventHandler) *EventForwarder {
	return &EventForwarder{eventHandler: eventHandler}
}

// ShouldUseAIInteraction checks whether AI interaction should be used for this event.
func (f *EventForwarder) ShouldUseAIInteraction(userID string, eventType string) bool {
	// P5: Check user preferences from ai_interaction_preferences table
	// For now, default to true for permission and question events
	return eventType == "permission" || eventType == "question"
}

// ForwardToAI forwards an event to the AI runtime for processing.
func (f *EventForwarder) ForwardToAI(ctx context.Context, input struct {
	UserID    string
	EventType string
	SessionID string
	DeviceID  string
	Path      string
	Data      map[string]any
}) error {
	req := AIEventRequest{
		UserID:     input.UserID,
		EventType:  input.EventType,
		SessionID:  input.SessionID,
		DeviceID:   input.DeviceID,
		ActionData: input.Data,
		Path:       input.Path,
	}
	return f.eventHandler.HandleAIEvent(ctx, req)
}
