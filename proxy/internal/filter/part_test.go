package filter

import "testing"

func TestFilterPart_TextPart(t *testing.T) {
	rules := DefaultRules()
	part := map[string]interface{}{
		"type": "text",
		"text": "```python\nprint('hello')\n```",
	}
	result, actions := FilterPart(part, rules)
	if len(actions) != 1 {
		t.Fatalf("expected 1 action, got %d", len(actions))
	}
	text, _ := result["text"].(string)
	if !containsStr(text, "filtered-python") {
		t.Errorf("expected filtered-python, got %s", text)
	}
}

func TestFilterPart_ToolPart(t *testing.T) {
	rules := DefaultRules()
	part := map[string]interface{}{
		"type": "tool",
		"tool": "read",
		"state": map[string]interface{}{
			"input":  map[string]interface{}{"filePath": "D:/DEV/cs-cloud/cmd/main.go"},
			"status": "completed",
			"output": "file content here",
		},
	}
	result, actions := FilterPart(part, rules)
	if len(actions) != 2 {
		t.Fatalf("expected 2 actions (filter + tool tracking), got %d", len(actions))
	}
	state, _ := result["state"].(map[string]interface{})
	output, _ := state["output"].(string)
	if output != "[code filtered]" {
		t.Errorf("expected [code filtered], got %s", output)
	}
	var trackAction *FilterAction
	for i := range actions {
		if actions[i].Type == "tool_output" {
			trackAction = &actions[i]
		}
	}
	if trackAction == nil {
		t.Fatal("expected tool_output action")
	}
	if trackAction.ToolName != "read" {
		t.Errorf("expected toolName=read, got %s", trackAction.ToolName)
	}
	if trackAction.Path != "D:/DEV/cs-cloud/cmd/main.go" {
		t.Errorf("expected path from state.input.filePath, got %s", trackAction.Path)
	}
	if trackAction.Input == "" || trackAction.Input == "null" {
		t.Errorf("expected input JSON, got %s", trackAction.Input)
	}
}

func TestFilterPart_ToolPart_Pending(t *testing.T) {
	rules := DefaultRules()
	part := map[string]interface{}{
		"type": "tool",
		"tool": "bash",
		"state": map[string]interface{}{
			"input":  map[string]interface{}{"command": "ls -la"},
			"status": "pending",
		},
	}
	_, actions := FilterPart(part, rules)
	if len(actions) != 1 {
		t.Fatalf("expected 1 tracking action for pending tool, got %d", len(actions))
	}
	if actions[0].ToolName != "bash" {
		t.Errorf("expected toolName=bash, got %s", actions[0].ToolName)
	}
	if actions[0].Path != "ls -la" {
		t.Errorf("expected path from command, got %s", actions[0].Path)
	}
}

func TestFilterPart_UnknownType(t *testing.T) {
	rules := DefaultRules()
	part := map[string]interface{}{
		"type": "custom",
		"data": "value",
	}
	result, actions := FilterPart(part, rules)
	if len(actions) != 0 {
		t.Fatalf("expected 0 actions, got %d", len(actions))
	}
	if result["data"] != "value" {
		t.Error("expected unchanged for unknown type")
	}
}

func TestFilterPart_ReasoningThreshold(t *testing.T) {
	rules := DefaultRules()
	rules.ReasoningThreshold = 10
	part := map[string]interface{}{
		"type": "reasoning",
		"text": "this is a very long reasoning text that exceeds the threshold",
	}
	_, actions := FilterPart(part, rules)
	if len(actions) != 1 {
		t.Fatalf("expected 1 action for reasoning over threshold, got %d", len(actions))
	}
}
