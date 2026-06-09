package filter

import (
	"encoding/json"
	"testing"
)

func TestProcessConversationResponse_StructurePreserved(t *testing.T) {
	messages := []map[string]interface{}{
		{
			"type": "user",
			"role": "user",
			"content": []map[string]interface{}{
				{"type": "text", "text": "Hello, please help me with this code:\n```python\nprint('hello')\n```\nThanks!"},
			},
		},
		{
			"type": "assistant",
			"role": "assistant",
			"content": []map[string]interface{}{
				{"type": "text", "text": "Here is your code:\n```python\nprint('world')\n```"},
				{
					"type": "tool",
					"tool": "write",
					"state": map[string]interface{}{
						"status": "completed",
						"output": "file written successfully",
						"metadata": map[string]interface{}{
							"filePath": "/tmp/test.py",
						},
					},
				},
				{
					"type": "tool-result",
					"content": []map[string]interface{}{
						{"type": "text", "text": "Result: OK"},
					},
				},
			},
		},
		{
			"type": "assistant",
			"role": "assistant",
			"content": []map[string]interface{}{
				{
					"type": "text",
					"text": "I also ran this:\n```bash\nls -la\n```",
				},
			},
		},
	}

	body, err := json.Marshal(messages)
	if err != nil {
		t.Fatalf("Failed to marshal test data: %v", err)
	}
	t.Logf("Input size: %d bytes", len(body))

	rules := DefaultRules()
	filtered, summary := processConversationResponse(body, rules)

	t.Logf("Output size: %d bytes", len(filtered))
	if summary != nil {
		t.Logf("Summary: filtered=%v, codeBlocks=%d/%d", summary.Filtered, summary.CodeBlocksFiltered, summary.CodeBlocksTotal)
	}

	if !json.Valid(filtered) {
		t.Fatalf("Filtered output is not valid JSON!\nFirst 500 bytes: %s", string(filtered[:min(500, len(filtered))]))
	}

	var result []map[string]interface{}
	if err := json.Unmarshal(filtered, &result); err != nil {
		t.Fatalf("Failed to unmarshal filtered output: %v\nFirst 500 bytes: %s", err, string(filtered[:min(500, len(filtered))]))
	}

	if len(result) != len(messages) {
		t.Fatalf("Message count changed: %d -> %d", len(messages), len(result))
	}

	t.Logf("Successfully preserved structure with %d messages", len(result))

	for i, msg := range result {
		msgType, _ := msg["type"].(string)
		role, _ := msg["role"].(string)
		t.Logf("  Message[%d]: type=%s, role=%s", i, msgType, role)

		content, ok := msg["content"].([]interface{})
		if !ok {
			t.Logf("    content type: %T", msg["content"])
			continue
		}
		for j, part := range content {
			partMap, ok := part.(map[string]interface{})
			if !ok {
				t.Errorf("    Part[%d] is not a map: %T", j, part)
				continue
			}
			partType, _ := partMap["type"].(string)
			t.Logf("    Part[%d]: type=%s", j, partType)

			if partType == "tool-result" {
				contentField := partMap["content"]
				t.Logf("      content type: %T", contentField)
				if _, isArray := contentField.([]interface{}); isArray {
					t.Logf("      content is array (preserved correctly)")
				} else if _, isString := contentField.(string); isString {
					t.Logf("      content was converted to string (POTENTIAL BUG)")
				}
			}
		}
	}
}

func TestProcessConversationResponse_ToolResultArrayContent(t *testing.T) {
	messages := []map[string]interface{}{
		{
			"type": "assistant",
			"content": []map[string]interface{}{
				{
					"type":    "tool-result",
					"content": []map[string]interface{}{{"type": "text", "text": "some output with code\n```js\nconsole.log(1)\n```"}},
				},
			},
		},
	}

	body, _ := json.Marshal(messages)
	t.Logf("Input: %s", string(body))

	rules := DefaultRules()
	filtered, _ := processConversationResponse(body, rules)

	t.Logf("Output: %s", string(filtered))

	var result []map[string]interface{}
	if err := json.Unmarshal(filtered, &result); err != nil {
		t.Fatalf("Invalid JSON after filtering: %v", err)
	}

	content := result[0]["content"].([]interface{})
	toolResult := content[0].(map[string]interface{})
	contentField := toolResult["content"]

	switch contentField.(type) {
	case []interface{}:
		t.Log("PASS: content is still an array")
	case string:
		t.Fatal("FAIL: content was converted from array to string - this breaks the UI!")
	default:
		t.Fatalf("FAIL: unexpected content type: %T", contentField)
	}
}

func TestProcessConversationResponse_NestedSteps(t *testing.T) {
	messages := map[string]interface{}{
		"id":   "test-123",
		"type": "conversation",
		"messages": []map[string]interface{}{
			{
				"type": "assistant",
				"content": []map[string]interface{}{
					{"type": "step-start"},
					{"type": "text", "text": "thinking..."},
				},
			},
		},
	}

	body, _ := json.Marshal(messages)
	rules := DefaultRules()
	filtered, _ := processConversationResponse(body, rules)

	if !json.Valid(filtered) {
		t.Fatalf("Invalid JSON: %s", string(filtered[:min(200, len(filtered))]))
	}

	var result map[string]interface{}
	json.Unmarshal(filtered, &result)

	msgs := result["messages"].([]interface{})
	msg := msgs[0].(map[string]interface{})
	content := msg["content"].([]interface{})

	if len(content) != 2 {
		t.Fatalf("Expected 2 content parts, got %d", len(content))
	}

	stepStart := content[0].(map[string]interface{})
	if stepStart["type"] != "step-start" {
		t.Fatal("step-start part was modified")
	}
	t.Log("PASS: step-start preserved correctly")
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
