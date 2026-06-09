package filter

import "testing"

func TestFilterCode_Multiline(t *testing.T) {
	rules := DefaultRules()
	input := "line1\nline2\nline3"
	result, actions := FilterCode(input, rules)
	if len(actions) != 1 {
		t.Fatalf("expected 1 action, got %d", len(actions))
	}
	if result != "[code filtered]" {
		t.Errorf("expected [code filtered], got %s", result)
	}
}

func TestFilterCode_Empty(t *testing.T) {
	rules := DefaultRules()
	result, actions := FilterCode("", rules)
	if len(actions) != 0 {
		t.Fatalf("expected 0 actions, got %d", len(actions))
	}
	if result != "" {
		t.Errorf("expected empty, got %s", result)
	}
}

func TestFilterCodeByLine_Multiline(t *testing.T) {
	rules := DefaultRules()
	input := "line1\nline2\nline3"
	result, actions := FilterCodeByLine(input, rules)
	if len(actions) != 1 {
		t.Fatalf("expected 1 action, got %d", len(actions))
	}
	expected := "[code filtered]\n[code filtered]\n[code filtered]"
	if result != expected {
		t.Errorf("expected %q, got %q", expected, result)
	}
}

func TestFilterCodeByLine_EmptyLines(t *testing.T) {
	rules := DefaultRules()
	input := "line1\n\nline3"
	result, _ := FilterCodeByLine(input, rules)
	if !containsStr(result, "\n\n") {
		t.Errorf("expected empty line preserved, got %s", result)
	}
}
