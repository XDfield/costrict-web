package filter

import "testing"

func TestApplyStrategy_Redact(t *testing.T) {
	result := ApplyStrategy("secret code", "redact", "[code filtered]")
	if result != "[code filtered]" {
		t.Errorf("expected [code filtered], got %s", result)
	}
}

func TestApplyStrategy_Strip(t *testing.T) {
	result := ApplyStrategy("secret code", "strip", "[code filtered]")
	if result != "" {
		t.Errorf("expected empty, got %s", result)
	}
}

func TestApplyStrategy_Mask(t *testing.T) {
	result := ApplyStrategy("line1\nline2\n", "mask", "[code filtered]")
	if result != "***\n***\n" {
		t.Errorf("expected masked output, got %s", result)
	}
}

func TestApplyStrategy_EmptyContent(t *testing.T) {
	result := ApplyStrategy("", "redact", "[code filtered]")
	if result != "[code filtered]" {
		t.Errorf("expected [code filtered], got %s", result)
	}
}

func TestApplyStrategy_CustomPlaceholder(t *testing.T) {
	result := ApplyStrategy("code", "redact", "[REDACTED]")
	if result != "[REDACTED]" {
		t.Errorf("expected [REDACTED], got %s", result)
	}
}
