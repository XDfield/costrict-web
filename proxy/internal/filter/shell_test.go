package filter

import "testing"

func TestFilterShell_BelowThreshold(t *testing.T) {
	rules := DefaultRules()
	output := "short output"
	result, actions := FilterShell(output, rules)
	if len(actions) != 0 {
		t.Fatalf("expected 0 actions, got %d", len(actions))
	}
	if result != output {
		t.Errorf("expected unchanged, got %s", result)
	}
}

func TestFilterShell_ExactThreshold(t *testing.T) {
	rules := DefaultRules()
	output := make([]byte, 120)
	for i := range output {
		output[i] = 'x'
	}
	result, actions := FilterShell(string(output), rules)
	if len(actions) != 0 {
		t.Fatalf("expected 0 actions at exact threshold, got %d", len(actions))
	}
	if result != string(output) {
		t.Error("expected unchanged at exact threshold")
	}
}

func TestFilterShell_OverThreshold(t *testing.T) {
	rules := DefaultRules()
	output := make([]byte, 121)
	for i := range output {
		output[i] = 'x'
	}
	result, actions := FilterShell(string(output), rules)
	if len(actions) != 1 {
		t.Fatalf("expected 1 action, got %d", len(actions))
	}
	if result != "[code filtered]" {
		t.Errorf("expected [code filtered], got %s", result)
	}
}

func TestFilterShell_Empty(t *testing.T) {
	rules := DefaultRules()
	result, actions := FilterShell("", rules)
	if len(actions) != 0 {
		t.Fatalf("expected 0 actions, got %d", len(actions))
	}
	if result != "" {
		t.Errorf("expected empty, got %s", result)
	}
}

func TestFilterShell_Threshold119(t *testing.T) {
	rules := DefaultRules()
	output := make([]byte, 119)
	for i := range output {
		output[i] = 'a'
	}
	_, actions := FilterShell(string(output), rules)
	if len(actions) != 0 {
		t.Fatalf("expected 0 actions for 119 chars, got %d", len(actions))
	}
}

func TestFilterShell_Threshold121(t *testing.T) {
	rules := DefaultRules()
	output := make([]byte, 121)
	for i := range output {
		output[i] = 'a'
	}
	_, actions := FilterShell(string(output), rules)
	if len(actions) != 1 {
		t.Fatalf("expected 1 action for 121 chars, got %d", len(actions))
	}
}
