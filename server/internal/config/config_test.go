package config

import (
	"os"
	"reflect"
	"testing"
)

func TestGetEnvSliceLower(t *testing.T) {
	const key = "GET_ENV_SLICE_LOWER_TEST"

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

// TestGetEnvSlice_PreservesCase pins the bootstrap path (now keyed on Casdoor
// universal_id) to getEnvSlice, which trims but does NOT lowercase — universal_id
// is case-sensitive, so lowercasing would corrupt the allowlist match.
func TestGetEnvSlice_PreservesCase(t *testing.T) {
	const key = "BOOTSTRAP_PLATFORM_ADMIN_UNIVERSAL_IDS_TEST"

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
			name:  "single id preserves case",
			set:   true,
			value: "AbC-123-XyZ",
			want:  []string{"AbC-123-XyZ"},
		},
		{
			name:  "comma separated, trimmed, case preserved",
			set:   true,
			value: "  AbC-1 , dEf-2 ,GhI-3",
			want:  []string{"AbC-1", "dEf-2", "GhI-3"},
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
			got := getEnvSlice(key, tc.fallback)
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("getEnvSlice(%q) = %#v, want %#v", tc.value, got, tc.want)
			}
		})
	}
}

// --- Phase A8: JWTSignMode three-state flag (off → dual → single) ---

// TestJWTSignMode_DefaultIsOff verifies the boot default. Production keeps
// Casdoor JWT as authoritative until A8 灰度 flips this to dual.
func TestJWTSignMode_DefaultIsOff(t *testing.T) {
	os.Unsetenv("JWT_SIGN_MODE")
	os.Unsetenv("JWT_SELF_SIGN_ENABLED")
	got := loadJWTSignMode()
	if got != JWTSignModeOff {
		t.Errorf("loadJWTSignMode() with no env = %q, want %q", got, JWTSignModeOff)
	}
}

// TestJWTSignMode_JWT_SIGN_MODE_ParsesAllThreeStates covers the preferred
// A8 vocabulary: off/dual/single, case-insensitive.
func TestJWTSignMode_JWT_SIGN_MODE_ParsesAllThreeStates(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		want string
	}{
		{"off lowercase", "off", JWTSignModeOff},
		{"dual lowercase", "dual", JWTSignModeDual},
		{"single lowercase", "single", JWTSignModeSingle},
		{"OFF uppercase", "OFF", JWTSignModeOff},
		{"DUAL uppercase", "DUAL", JWTSignModeDual},
		{"SINGLE uppercase", "SINGLE", JWTSignModeSingle},
		{"Dual mixedcase", "Dual", JWTSignModeDual},
		{"with surrounding whitespace", "  dual  ", JWTSignModeDual},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			os.Setenv("JWT_SIGN_MODE", tc.raw)
			t.Cleanup(func() { os.Unsetenv("JWT_SIGN_MODE") })
			// Clear legacy var so precedence test doesn't bleed in.
			os.Unsetenv("JWT_SELF_SIGN_ENABLED")
			got := loadJWTSignMode()
			if got != tc.want {
				t.Errorf("JWT_SIGN_MODE=%q: got %q, want %q", tc.raw, got, tc.want)
			}
		})
	}
}

// TestJWTSignMode_JWT_SELF_SIGN_ENABLED_LegacyMapping verifies the A7b
// vocabulary still works (true → dual; false/unset → off). Accepts the full
// strconv.ParseBool truth vocabulary.
func TestJWTSignMode_JWT_SELF_SIGN_ENABLED_LegacyMapping(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		want string
	}{
		{"true → dual", "true", JWTSignModeDual},
		{"TRUE → dual", "TRUE", JWTSignModeDual},
		{"1 → dual", "1", JWTSignModeDual},
		{"t → dual", "t", JWTSignModeDual},
		{"false → off", "false", JWTSignModeOff},
		{"0 → off", "0", JWTSignModeOff},
		{"empty → off", "", JWTSignModeOff},
		{"yes-not-supported → off", "yes", JWTSignModeOff},
		{"garbage → off", "not-a-bool", JWTSignModeOff},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			os.Unsetenv("JWT_SIGN_MODE") // ensure legacy var wins
			if tc.raw == "" {
				os.Unsetenv("JWT_SELF_SIGN_ENABLED")
			} else {
				os.Setenv("JWT_SELF_SIGN_ENABLED", tc.raw)
			}
			t.Cleanup(func() { os.Unsetenv("JWT_SELF_SIGN_ENABLED") })
			got := loadJWTSignMode()
			if got != tc.want {
				t.Errorf("JWT_SELF_SIGN_ENABLED=%q: got %q, want %q", tc.raw, got, tc.want)
			}
		})
	}
}

// TestJWTSignMode_JWT_SIGN_MODE_WinsOverLegacyBool verifies precedence: when
// both are set, the explicit three-state value wins (so operators can downgrade
// from dual back to off without unsetting JWT_SELF_SIGN_ENABLED first).
func TestJWTSignMode_JWT_SIGN_MODE_WinsOverLegacyBool(t *testing.T) {
	os.Setenv("JWT_SIGN_MODE", "off")
	os.Setenv("JWT_SELF_SIGN_ENABLED", "true")
	t.Cleanup(func() {
		os.Unsetenv("JWT_SIGN_MODE")
		os.Unsetenv("JWT_SELF_SIGN_ENABLED")
	})
	got := loadJWTSignMode()
	if got != JWTSignModeOff {
		t.Errorf("JWT_SIGN_MODE=off + JWT_SELF_SIGN_ENABLED=true: got %q, want %q (JWT_SIGN_MODE must win)", got, JWTSignModeOff)
	}
}
