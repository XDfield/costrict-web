package filter

import "testing"

func TestFilterDiff_Standard(t *testing.T) {
	rules := DefaultRules()
	input := "--- a/main.go\n+++ b/main.go\n@@ -1,3 +1,4 @@\n package main\n-import \"fmt\"\n+import \"os\"\n func main() {"
	result, actions := FilterDiff(input, rules)
	if len(actions) != 1 {
		t.Fatalf("expected 1 action, got %d", len(actions))
	}
	if !containsStr(result, "[code filtered]") {
		t.Errorf("expected placeholder in output, got %s", result)
	}
	if !containsStr(result, "--- a/main.go") {
		t.Errorf("expected file path preserved, got %s", result)
	}
}

func TestFilterDiff_NoChanges(t *testing.T) {
	rules := DefaultRules()
	input := "--- a/main.go\n+++ b/main.go\n@@ -1,2 +1,2 @@\n package main\n func main() {"
	result, actions := FilterDiff(input, rules)
	if len(actions) != 0 {
		t.Fatalf("expected 0 actions for context-only diff, got %d: %v", len(actions), actions)
	}
	if result != input {
		t.Errorf("expected unchanged, got %s", result)
	}
}

func TestFilterDiff_MultiFile(t *testing.T) {
	rules := DefaultRules()
	input := "--- a/a.go\n+++ b/a.go\n@@ -1 +1 @@\n-old\n+new\n--- a/b.go\n+++ b/b.go\n@@ -1 +1 @@\n-old2\n+new2"
	result, actions := FilterDiff(input, rules)
	if len(actions) != 1 {
		t.Fatalf("expected 1 action, got %d", len(actions))
	}
	if !containsStr(result, "[code filtered]") {
		t.Errorf("expected placeholder, got %s", result)
	}
}

func TestFilterDiff_Empty(t *testing.T) {
	rules := DefaultRules()
	result, actions := FilterDiff("", rules)
	if len(actions) != 0 {
		t.Fatalf("expected 0 actions for empty diff")
	}
	if result != "" {
		t.Errorf("expected empty, got %s", result)
	}
}
