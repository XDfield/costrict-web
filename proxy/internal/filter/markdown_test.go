package filter

import "testing"

func TestFilterMarkdown_SingleClosedBlock(t *testing.T) {
	rules := DefaultRules()
	input := "Here:\n```python\nprint('hello')\n```\nDone."
	result, actions := FilterMarkdown(input, rules)
	if len(actions) != 1 {
		t.Fatalf("expected 1 action, got %d", len(actions))
	}
	if !containsStr(result, "filtered-python") {
		t.Errorf("expected filtered-python prefix, got %s", result)
	}
	if !containsStr(result, "[code filtered]") {
		t.Errorf("expected placeholder, got %s", result)
	}
}

func TestFilterMarkdown_MultipleBlocks(t *testing.T) {
	rules := DefaultRules()
	input := "```go\nfmt.Println()\n```\nText\n```js\nconsole.log()\n```"
	result, actions := FilterMarkdown(input, rules)
	if len(actions) != 2 {
		t.Fatalf("expected 2 actions, got %d", len(actions))
	}
	if !containsStr(result, "filtered-go") {
		t.Errorf("expected filtered-go, got %s", result)
	}
	if !containsStr(result, "filtered-js") {
		t.Errorf("expected filtered-js, got %s", result)
	}
}

func TestFilterMarkdown_NoCodeBlocks(t *testing.T) {
	rules := DefaultRules()
	input := "Just plain text"
	result, actions := FilterMarkdown(input, rules)
	if len(actions) != 0 {
		t.Fatalf("expected 0 actions, got %d", len(actions))
	}
	if result != input {
		t.Errorf("expected unchanged, got %s", result)
	}
}

func TestFilterMarkdown_UnclosedBlock(t *testing.T) {
	rules := DefaultRules()
	input := "Code:\n```python\ndef hello():"
	result, actions := FilterMarkdown(input, rules)
	if len(actions) != 1 {
		t.Fatalf("expected 1 action, got %d", len(actions))
	}
	if !containsStr(result, "streaming-python") {
		t.Errorf("expected streaming-python, got %s", result)
	}
}

func TestFilterMarkdown_NoLangBlock(t *testing.T) {
	rules := DefaultRules()
	input := "```\nsome code\n```"
	result, actions := FilterMarkdown(input, rules)
	if len(actions) != 1 {
		t.Fatalf("expected 1 action, got %d", len(actions))
	}
	if !containsStr(result, "filtered\n") {
		t.Errorf("expected filtered prefix (no lang), got %s", result)
	}
}

func TestFilterMarkdown_EmptyBlock(t *testing.T) {
	rules := DefaultRules()
	input := "```\n```"
	result, _ := FilterMarkdown(input, rules)
	if !containsStr(result, "filtered") {
		t.Errorf("expected filtered, got %s", result)
	}
}

func TestFilterMarkdown_IndentedBlockInListItem(t *testing.T) {
	rules := DefaultRules()
	input := "1. **text**：\n   ```typescript\n   const x = 1;\n   ```\n\n2. next item\n"
	result, actions := FilterMarkdown(input, rules)
	if len(actions) != 1 {
		t.Fatalf("expected 1 action, got %d", len(actions))
	}
	if !containsStr(result, "filtered-typescript") {
		t.Errorf("expected filtered-typescript prefix, got %s", result)
	}
	if !containsStr(result, "   [code filtered]\n   ```") {
		t.Errorf("expected indented placeholder + indented closing fence, got %s", result)
	}
	if !containsStr(result, "2. next item") {
		t.Errorf("expected '2. next item' preserved, got %s", result)
	}
}

func TestFilterMarkdown_IndentedBlockMultipleInList(t *testing.T) {
	rules := DefaultRules()
	input := "1. item one:\n   ```go\n   code1\n   ```\n\n2. item two:\n   ```python\n   code2\n   ```\n"
	result, actions := FilterMarkdown(input, rules)
	if len(actions) != 2 {
		t.Fatalf("expected 2 actions, got %d", len(actions))
	}
	if !containsStr(result, "filtered-go") {
		t.Errorf("expected filtered-go, got %s", result)
	}
	if !containsStr(result, "filtered-python") {
		t.Errorf("expected filtered-python, got %s", result)
	}
	if !containsStr(result, "2. item two") {
		t.Errorf("expected '2. item two' preserved, got %s", result)
	}
}

func containsStr(s, sub string) bool {
	for i := 0; i <= len(s)-len(sub); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
