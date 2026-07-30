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
	// "u-" prefix + 8 base62 chars = 10 total.
	matcher := regexp.MustCompile(`^u-[0-9a-zA-Z]{8}$`)
	for _, sid := range []string{"usr_550e8400-e29b-41d4-a716-446655440000", "usr-x", ""} {
		got := BuildShortID(sid)
		if !matcher.MatchString(got) {
			t.Errorf("got %q, want shape ^u-[0-9a-zA-Z]{8}$", got)
		}
		if len(got) > 32 {
			t.Errorf("got %q (%d chars), must fit column size 32", got, len(got))
		}
	}
}

func TestBuildShortID_NoCollisionAcrossSamples(t *testing.T) {
	// Cheap sanity check that distinct inputs don't accidentally collide in a
	// small sample — does NOT prove 48-bit collision resistance, just guards
	// against trivial alphabet/encoding bugs.
	seen := make(map[string]string, 1000)
	for i := 0; i < 1000; i++ {
		sid := "usr_" + string(rune('a'+i%26)) + string(rune('0'+i/26))
		got := BuildShortID(sid)
		if prev, dup := seen[got]; dup {
			t.Errorf("collision: %q and %q both → %q", prev, sid, got)
		}
		seen[got] = sid
	}
}
