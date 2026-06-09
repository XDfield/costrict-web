package filter

import (
	"bytes"
	"strings"
	"testing"
)

func TestFilterSSEStream_MessagePartUpdated(t *testing.T) {
	rules := DefaultRules()
	input := "event: message.part.updated\ndata: {\"type\":\"message.part.updated\",\"properties\":{\"part\":{\"type\":\"text\",\"text\":\"```go\\nfmt.Println()\\n```\"}}}\n\n"
	reader := bytes.NewReader([]byte(input))
	var buf bytes.Buffer

	err := FilterSSEStream(reader, &buf, rules, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	output := buf.String()
	if !containsStr(output, "filtered-go") {
		t.Errorf("expected filtered-go in SSE output, got %s", output)
	}
}

func TestFilterSSEStream_PassThrough(t *testing.T) {
	rules := DefaultRules()
	input := "event: session.status\ndata: {\"type\":\"session.status\"}\n\n"
	reader := bytes.NewReader([]byte(input))
	var buf bytes.Buffer

	err := FilterSSEStream(reader, &buf, rules, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	output := buf.String()
	if !containsStr(output, "session.status") {
		t.Errorf("expected session.status in output, got %s", output)
	}
}

func TestFilterSSEStream_NoEventLine_UsesJsonType(t *testing.T) {
	rules := DefaultRules()
	input := "data: {\"type\":\"message.part.updated\",\"properties\":{\"part\":{\"type\":\"text\",\"text\":\"```python\\ndef quicksort(arr):\\n    return arr\\n```\"}}}\n\n"
	reader := bytes.NewReader([]byte(input))
	var buf bytes.Buffer

	err := FilterSSEStream(reader, &buf, rules, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	output := buf.String()
	if !containsStr(output, "filtered-python") {
		t.Errorf("expected filtered-python in SSE output (no event: line), got %s", output)
	}
	if containsStr(output, "quicksort") {
		t.Errorf("code content should be filtered, got %s", output)
	}
}

func TestFilterSSEStream_Callback(t *testing.T) {
	rules := DefaultRules()
	input := "event: message.part.updated\ndata: {\"type\":\"message.part.updated\",\"properties\":{\"part\":{\"type\":\"text\",\"text\":\"```go\\ncode\\n```\"}}}\n\n"
	reader := bytes.NewReader([]byte(input))
	var buf bytes.Buffer

	var summaries []*AuditSummary
	callback := func(s *AuditSummary) {
		summaries = append(summaries, s)
	}

	err := FilterSSEStream(reader, &buf, rules, callback)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(summaries) != 1 {
		t.Fatalf("expected 1 callback, got %d", len(summaries))
	}
	if !summaries[0].Filtered {
		t.Error("expected filtered=true")
	}
}

func TestFilterSSEStream_DeltaCodeBlock_Suppressed(t *testing.T) {
	rules := DefaultRules()
	pid := "part-123"
	mid := "msg-456"
	sid := "sess-789"

	delta := func(text string) string {
		return "data: {\"type\":\"message.part.delta\",\"properties\":{\"delta\":\"" + escapeJSON(text) + "\",\"field\":\"text\",\"messageID\":\"" + mid + "\",\"partID\":\"" + pid + "\",\"sessionID\":\"" + sid + "\"}}\n\n"
	}

	input := delta("快速排序算法示例：\n\n") +
		delta("```") +
		delta("python") +
		delta("\n") +
		delta("def quicksort(arr):") +
		delta("\n    return arr") +
		delta("\n```") +
		delta("\n\n以上是代码。")

	reader := bytes.NewReader([]byte(input))
	var buf bytes.Buffer

	err := FilterSSEStream(reader, &buf, rules, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	output := buf.String()

	if containsStr(output, "quicksort") {
		t.Errorf("code content should be suppressed in deltas, got: %s", output)
	}
	if containsStr(output, "return arr") {
		t.Errorf("code body should be suppressed, got: %s", output)
	}
	if !containsStr(output, "filtered-python") {
		t.Errorf("expected filtered-python replacement, got: %s", output)
	}
	if !containsStr(output, "[code filtered]") {
		t.Errorf("expected [code filtered] replacement, got: %s", output)
	}
	if !containsStr(output, "快速排序算法示例") {
		t.Errorf("pre-code text should pass through, got: %s", output)
	}
	if !containsStr(output, "以上是代码") {
		t.Errorf("post-code text should pass through, got: %s", output)
	}
}

func TestFilterSSEStream_DeltaNoCode_PassThrough(t *testing.T) {
	rules := DefaultRules()

	input := "data: {\"type\":\"message.part.delta\",\"properties\":{\"delta\":\"Hello world\",\"field\":\"text\",\"messageID\":\"m1\",\"partID\":\"p1\",\"sessionID\":\"s1\"}}\n\n" +
		"data: {\"type\":\"message.part.delta\",\"properties\":{\"delta\":\" more text\",\"field\":\"text\",\"messageID\":\"m1\",\"partID\":\"p1\",\"sessionID\":\"s1\"}}\n\n"

	reader := bytes.NewReader([]byte(input))
	var buf bytes.Buffer

	err := FilterSSEStream(reader, &buf, rules, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	output := buf.String()
	if !containsStr(output, "Hello world") {
		t.Errorf("plain text should pass through, got: %s", output)
	}
	if !containsStr(output, " more text") {
		t.Errorf("plain text should pass through, got: %s", output)
	}
}

func TestFilterSSEStream_DeltaNonTextField_PassThrough(t *testing.T) {
	rules := DefaultRules()

	input := "data: {\"type\":\"message.part.delta\",\"properties\":{\"delta\":\"some data\",\"field\":\"output\",\"messageID\":\"m1\",\"partID\":\"p1\",\"sessionID\":\"s1\"}}\n\n"

	reader := bytes.NewReader([]byte(input))
	var buf bytes.Buffer

	err := FilterSSEStream(reader, &buf, rules, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	output := buf.String()
	if !containsStr(output, "some data") {
		t.Errorf("non-text field delta should pass through, got: %s", output)
	}
}

func escapeJSON(s string) string {
	s = strings.ReplaceAll(s, "\n", "\\n")
	s = strings.ReplaceAll(s, "\"", "\\\"")
	s = strings.ReplaceAll(s, "\t", "\\t")
	return s
}

func TestFilterSSEStream_ToolPart_TracksInputAndPath(t *testing.T) {
	rules := DefaultRules()

	toolUpdated := `data: {"type":"message.part.updated","properties":{"part":{"callID":"call_111","id":"p1","type":"tool","tool":"read","state":{"input":{"filePath":"D:/DEV/cs-cloud/cmd/main.go"},"output":"package main\nfunc main() {}\n","status":"completed"}},"sessionID":"s1","session_id":"s1","type":"message.part.updated"}}

`

	reader := bytes.NewReader([]byte(toolUpdated))
	var buf bytes.Buffer

	var summaries []*AuditSummary
	callback := func(s *AuditSummary) {
		summaries = append(summaries, s)
	}

	err := FilterSSEStream(reader, &buf, rules, callback)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(summaries) != 1 {
		t.Fatalf("expected 1 callback, got %d", len(summaries))
	}
	s := summaries[0]
	if len(s.ToolActions) == 0 {
		t.Fatal("expected tool actions")
	}
	ta := s.ToolActions[0]
	if ta.ToolName != "read" {
		t.Errorf("expected toolName=read, got %s", ta.ToolName)
	}
	if ta.Path != "D:/DEV/cs-cloud/cmd/main.go" {
		t.Errorf("expected filePath as path, got %s", ta.Path)
	}
	if ta.Input == "" || ta.Input == "null" {
		t.Errorf("expected input JSON, got %s", ta.Input)
	}
	if !containsStr(ta.Input, "filePath") {
		t.Errorf("input should contain filePath, got %s", ta.Input)
	}
}
