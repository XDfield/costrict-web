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
		{"too_long", "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", ErrUsernameInvalid},
		{"leading_dash", "-alice", ErrUsernameInvalid},
		{"bad_char_dot", "alice.bob", ErrUsernameInvalid},
		{"reserved_admin", "admin", ErrUsernameReserved},
		{"reserved_casdoor_case_insensitive", "Casdoor", ErrUsernameReserved},
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

// TestIsUsernameAvailable_NilDB is a smoke test ensuring the empty-service
// path returns an error (and not a panic).
func TestIsUsernameAvailable_NilDB(t *testing.T) {
	t.Parallel()
	s := &UserService{}
	_, err := s.IsUsernameAvailable(context.Background(), "alice", "")
	if err == nil {
		t.Fatalf("expected error from nil db, got nil")
	}
}
