package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
)

// QuestionTool handles reply_question tool calls.
type QuestionTool struct{}

// NewQuestionTool creates a question tool.
func NewQuestionTool() *QuestionTool {
	return &QuestionTool{}
}

func (t *QuestionTool) Name() string {
	return "reply_question"
}

func (t *QuestionTool) Definition() Definition {
	return Definition{
		Name:        "reply_question",
		Description: "回答设备端提出的问题。questionID 必须用申请来源段里给出的真实 ID，不要自己编。",
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"questionID": map[string]any{
					"type":        "string",
					"description": "问题的 ID，从申请来源段取真实 ID",
				},
				"answers": map[string]any{
					"type":        "array",
					"items":       map[string]any{"type": "string"},
					"description": "每道题的回答，按题目顺序排列。每项是对应题目的答案内容（选项文本或自由输入）。单选题直接填答案，多选题多填几项。",
				},
			},
			"required": []string{"questionID", "answers"},
		},
	}
}

func (t *QuestionTool) Execute(ctx context.Context, argsJSON string, toolCtx *Context) (string, error) {
	slog.Debug("[tool] reply_question: execute", "args", argsJSON, "deviceID", toolCtx.DeviceID)

	var args struct {
		QuestionID string   `json:"questionID"`
		Answers    []string `json:"answers"`
	}
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		slog.Error("[tool] reply_question: parse args failed", "args", argsJSON, "error", err)
		return "", fmt.Errorf("parse args: %w", err)
	}
	slog.Info("[tool] reply_question: parsed args", "questionID", args.QuestionID, "answers", args.Answers)

	if toolCtx.DeviceID == "" {
		slog.Error("[tool] reply_question: deviceID is empty")
		return "", fmt.Errorf("cannot resolve device ID for question reply")
	}

	if len(args.Answers) == 0 {
		return "", fmt.Errorf("answers cannot be empty")
	}

	answers := [][]string{}
	for _, a := range args.Answers {
		answers = append(answers, []string{a})
	}

	if err := toolCtx.DeviceProxy.ReplyQuestion(ctx, toolCtx.DeviceID, args.QuestionID, answers, toolCtx.Directory); err != nil {
		slog.Error("[tool] reply_question: device proxy call failed", "questionID", args.QuestionID, "deviceID", toolCtx.DeviceID, "error", err)
		return "", fmt.Errorf("device proxy reply question failed: %w", err)
	}
	slog.Debug("[tool] reply_question: device proxy call succeeded", "questionID", args.QuestionID, "deviceID", toolCtx.DeviceID)

	// Mark as processed on success
	if toolCtx.MarkProcessed != nil {
		slog.Debug("[tool] reply_question: calling MarkProcessed", "questionID", args.QuestionID)
		toolCtx.MarkProcessed()
	} else {
		slog.Warn("[tool] reply_question: MarkProcessed is nil", "questionID", args.QuestionID)
	}

	result := fmt.Sprintf("问题 %s 的回答已提交", args.QuestionID)
	slog.Debug("[tool] reply_question: completed", "questionID", args.QuestionID, "result", result)
	return result, nil
}
