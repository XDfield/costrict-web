package config

import (
	"os"
	"reflect"
	"testing"
)

func TestGetEnvSliceLower(t *testing.T) {
	const key = "BOOTSTRAP_PLATFORM_ADMINS_TEST"

	tests := []struct {
		name     string
		set      bool
		value    string
		fallback []string
		want     []string
	}{
		{
			name:     "unset returns default",
			set:      false,
			fallback: nil,
			want:     nil,
		},
		{
			name:     "empty returns default",
			set:      true,
			value:    "",
			fallback: []string{"fallback@example.com"},
			want:     []string{"fallback@example.com"},
		},
		{
			name:  "single email lowercased",
			set:   true,
			value: "Admin@Example.COM",
			want:  []string{"admin@example.com"},
		},
		{
			name:  "comma separated, trimmed and lowercased",
			set:   true,
			value: "  Alice@EXAMPLE.com , BOB@example.com ,carol@Example.Com",
			want:  []string{"alice@example.com", "bob@example.com", "carol@example.com"},
		},
		{
			name:     "blank-only entries fall back to default",
			set:      true,
			value:    " , , ",
			fallback: nil,
			want:     nil,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if tc.set {
				t.Setenv(key, tc.value)
			} else {
				os.Unsetenv(key)
			}
			got := getEnvSliceLower(key, tc.fallback)
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("getEnvSliceLower(%q) = %#v, want %#v", tc.value, got, tc.want)
			}
		})
	}
}
