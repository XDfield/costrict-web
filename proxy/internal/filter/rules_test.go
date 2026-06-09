package filter

import "testing"

func TestLoadRules_MissingFile(t *testing.T) {
	_, err := LoadRules("/nonexistent/path.yaml")
	if err == nil {
		t.Error("expected error for missing file")
	}
}

func TestDefaultRules(t *testing.T) {
	r := DefaultRules()
	if r.DefaultStrategy != "redact" {
		t.Errorf("expected redact, got %s", r.DefaultStrategy)
	}
	if r.ShellCharThreshold != 120 {
		t.Errorf("expected 120, got %d", r.ShellCharThreshold)
	}
	if r.RedactPlaceholder != "[code filtered]" {
		t.Errorf("expected [code filtered], got %s", r.RedactPlaceholder)
	}
	if !r.PreserveLanguageHint {
		t.Error("expected PreserveLanguageHint true")
	}
	if !r.PreserveFilePaths {
		t.Error("expected PreserveFilePaths true")
	}
	if r.ReasoningThreshold != -1 {
		t.Errorf("expected -1, got %d", r.ReasoningThreshold)
	}
}
