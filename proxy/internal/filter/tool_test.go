package filter

import "testing"

func TestFilterToolOutput_ReadFile(t *testing.T) {
	rules := DefaultRules()
	result, actions := FilterToolOutput("read_file", "file content", rules)
	if len(actions) != 1 {
		t.Fatalf("expected 1 action, got %d", len(actions))
	}
	if result != "[code filtered]" {
		t.Errorf("expected [code filtered], got %s", result)
	}
}

func TestFilterToolOutput_BashShort(t *testing.T) {
	rules := DefaultRules()
	result, actions := FilterToolOutput("bash", "short output", rules)
	if len(actions) != 0 {
		t.Fatalf("expected 0 actions for short bash, got %d", len(actions))
	}
	if result != "short output" {
		t.Errorf("expected unchanged, got %s", result)
	}
}

func TestFilterToolOutput_BashLong(t *testing.T) {
	rules := DefaultRules()
	long := make([]byte, 200)
	result, actions := FilterToolOutput("bash", string(long), rules)
	if len(actions) != 1 {
		t.Fatalf("expected 1 action, got %d", len(actions))
	}
	if result != "[code filtered]" {
		t.Errorf("expected [code filtered], got %s", result)
	}
}

func TestFilterToolOutput_Diff(t *testing.T) {
	rules := DefaultRules()
	input := "--- a/f.go\n+++ b/f.go\n@@ -1 +1 @@\n-old\n+new"
	result, actions := FilterToolOutput("diff", input, rules)
	if len(actions) != 1 {
		t.Fatalf("expected 1 action, got %d", len(actions))
	}
	if !containsStr(result, "[code filtered]") {
		t.Errorf("expected filtered, got %s", result)
	}
}

func TestFilterToolOutput_Unknown(t *testing.T) {
	rules := DefaultRules()
	result, actions := FilterToolOutput("unknown_tool", "content", rules)
	if len(actions) != 1 {
		t.Fatalf("expected 1 action for unknown tool (default redact), got %d", len(actions))
	}
	if result != "[code filtered]" {
		t.Errorf("expected [code filtered], got %s", result)
	}
}
