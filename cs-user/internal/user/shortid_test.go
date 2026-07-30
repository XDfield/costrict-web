package user

import (
	"regexp"
	"testing"
)

func TestBuildShortID_Deterministic(t *testing.T) {
	a1 := BuildShortID("usr_abc-123")
	a2 := BuildShortID("usr_abc-123")
	if a1 != a2 {
		t.Errorf("non-deterministic: %q vs %q", a1, a2)
	}
}

func TestBuildShortID_Distinct(t *testing.T) {
	if BuildShortID("usr_a") == BuildShortID("usr_b") {
		t.Errorf("distinct subject_ids collapsed to same short_id")
	}
}

func TestBuildShortID_Shape(t *testing.T) {
	matcher := regexp.MustCompile(`^u-[0-9a-f]{16}$`)
	for _, sid := range []string{"usr_550e8400-e29b-41d4-a716-446655440000", "usr-x", ""} {
		got := BuildShortID(sid)
		if !matcher.MatchString(got) {
			t.Errorf("got %q, want shape ^u-[0-9a-f]{16}$", got)
		}
		if len(got) > 32 {
			t.Errorf("got %q (%d chars), must fit column size 32", got, len(got))
		}
	}
}
