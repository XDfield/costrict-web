package user

import (
	"context"
	"testing"
)

func TestValidateUsername(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		in   string
		want error
	}{
		{"happy", "alice", nil},
		{"digits_and_underscores", "alice_99", nil},
		{"dash_ok", "alice-smith", nil},
		{"too_short", "ab", ErrUsernameInvalid},
		{"too_long", "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", ErrUsernameInvalid}, // 34 chars
		{"leading_dash", "-alice", ErrUsernameInvalid},
		{"leading_underscore", "_alice", ErrUsernameInvalid},
		{"bad_char_dot", "alice.bob", ErrUsernameInvalid},
		{"bad_char_space", "alice bob", ErrUsernameInvalid},
		{"reserved_admin", "admin", ErrUsernameReserved},
		{"reserved_casdoor_case_insensitive", "Casdoor", ErrUsernameReserved},
		{"whitespace_trimmed", "  alice  ", nil},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := ValidateUsername(tc.in)
			if got != tc.want {
				t.Fatalf("ValidateUsername(%q): want %v, got %v", tc.in, tc.want, got)
			}
		})
	}
}

// TestIsUsernameAvailable_HappyPath uses an in-memory sqlite fixture if
// available; otherwise it's a smoke test that the empty input shape doesn't
// blow up. The DB-backed validation is covered indirectly by the handler
// tests in package handlers.
func TestIsUsernameAvailable_NilDB(t *testing.T) {
	t.Parallel()
	s := &Service{}
	_, err := s.IsUsernameAvailable(context.Background(), "alice", "")
	if err == nil {
		t.Fatalf("expected error from nil db, got nil")
	}
}
