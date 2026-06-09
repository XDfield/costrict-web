package filter

import "testing"

func TestFilterEngine_Markdown(t *testing.T) {
	rules := DefaultRules()
	input := "```go\nfmt.Println()\n```"
	result, actions := FilterEngine("markdown", input, rules)
	if len(actions) != 1 {
		t.Fatalf("expected 1 action, got %d", len(actions))
	}
	if !containsStr(result, "filtered-go") {
		t.Errorf("expected filtered-go, got %s", result)
	}
}

func TestFilterEngine_Code(t *testing.T) {
	rules := DefaultRules()
	result, actions := FilterEngine("code", "secret code", rules)
	if len(actions) != 1 {
		t.Fatalf("expected 1 action, got %d", len(actions))
	}
	if result != "[code filtered]" {
		t.Errorf("expected [code filtered], got %s", result)
	}
}

func TestFilterEngine_Shell(t *testing.T) {
	rules := DefaultRules()
	long := make([]byte, 200)
	result, actions := FilterEngine("shell", string(long), rules)
	if len(actions) != 1 {
		t.Fatalf("expected 1 action, got %d", len(actions))
	}
	if result != "[code filtered]" {
		t.Errorf("expected [code filtered], got %s", result)
	}
}

func TestFilterEngine_Diff(t *testing.T) {
	rules := DefaultRules()
	input := "--- a/f.go\n+++ b/f.go\n@@ -1 +1 @@\n-old\n+new"
	result, actions := FilterEngine("diff", input, rules)
	if len(actions) != 1 {
		t.Fatalf("expected 1 action, got %d", len(actions))
	}
	if !containsStr(result, "[code filtered]") {
		t.Errorf("expected filtered diff, got %s", result)
	}
}

func TestFilterEngine_Unknown(t *testing.T) {
	rules := DefaultRules()
	result, actions := FilterEngine("unknown", "content", rules)
	if len(actions) != 0 {
		t.Fatalf("expected 0 actions, got %d", len(actions))
	}
	if result != "content" {
		t.Errorf("expected unchanged, got %s", result)
	}
}
