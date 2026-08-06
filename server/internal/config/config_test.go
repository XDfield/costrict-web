package config

import (
	"os"
	"reflect"
	"testing"
)

func TestLoadGitSystemWebhookBaseURLIsIndependentAndDisabledByDefault(t *testing.T) {
	t.Setenv("BIND_STATE_SECRET", "test-bind-secret")
	t.Setenv("JWT_SIGN_MODE", "off")
	t.Setenv("COSTRICT_CLOUD_BASE_URL", "https://frontend.example/cloud")
	t.Setenv("WEBHOOK_BASE_URL", "https://callbacks.example/cloud-api")
	t.Setenv("GIT_SYSTEM_WEBHOOK_BASE_URL", "")

	cfg := Load()
	if cfg.WebhookBaseURL != "https://callbacks.example/cloud-api" {
		t.Fatalf("WebhookBaseURL = %q", cfg.WebhookBaseURL)
	}
	if cfg.GitSystemWebhookBaseURL != "" {
		t.Fatalf("GitSystemWebhookBaseURL = %q, want disabled", cfg.GitSystemWebhookBaseURL)
	}
}

func TestLoadGitSystemWebhookBaseURLExplicitOptIn(t *testing.T) {
	t.Setenv("BIND_STATE_SECRET", "test-bind-secret")
	t.Setenv("JWT_SIGN_MODE", "off")
	t.Setenv("GIT_SYSTEM_WEBHOOK_BASE_URL", "https://api.example/cloud-api")

	cfg := Load()
	if cfg.GitSystemWebhookBaseURL != "https://api.example/cloud-api" {
		t.Fatalf("GitSystemWebhookBaseURL = %q", cfg.GitSystemWebhookBaseURL)
	}
}

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

func TestResolveBindStateSecret(t *testing.T) {
	cases := []struct {
		name        string
		bindState   string
		internal    string
		want        string
		wantErr     bool
	}{
		{
			name:      "dedicated secret preferred",
			bindState: "bind-secret-xyz",
			internal:  "internal-fallback",
			want:      "bind-secret-xyz",
		},
		{
			name:      "fallback to InternalSecret",
			bindState: "",
			internal:  "internal-fallback",
			want:      "internal-fallback",
		},
		{
			name:      "whitespace-only bind treated as empty",
			bindState: "   ",
			internal:  "internal-fallback",
			want:      "internal-fallback",
		},
		{
			name:     "both empty errors",
			bindState: "",
			internal: "",
			wantErr:  true,
		},
		{
			name:     "both whitespace errors",
			bindState: "  ",
			internal: "\t\n",
			wantErr:  true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := resolveBindStateSecret(tc.bindState, tc.internal)
			if tc.wantErr {
				if err != ErrBindStateSecretMissing {
					t.Errorf("expected ErrBindStateSecretMissing, got %v", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
}

// Both csc snapshot v2 switches must default OFF, and the two must be
// independent.
//
// The endpoint gate decides whether v2 exists; lifecycle propagation decides
// whether Git archival may instruct a device to delete files. Those are
// different blast radii, so enabling the first must never imply the second —
// this is the property a rollout depends on, and a default that drifted to true
// would ship the deletion path to a fleet nobody has checked.
func TestLoadCapabilitySyncSnapshotSwitchesDefaultOffAndAreIndependent(t *testing.T) {
	t.Run("both default off", func(t *testing.T) {
		setSnapshotSwitchBaseline(t)

		cfg := Load()
		if cfg.CapabilitySyncSnapshotV2Enabled {
			t.Error("CSC_SNAPSHOT_V2_ENABLED must default to false")
		}
		if cfg.CapabilitySyncLifecyclePropagationEnabled {
			t.Error("CSC_SNAPSHOT_LIFECYCLE_PROPAGATION_ENABLED must default to false")
		}
	})

	t.Run("serving v2 does not enable lifecycle propagation", func(t *testing.T) {
		setSnapshotSwitchBaseline(t)
		t.Setenv("CSC_SNAPSHOT_V2_ENABLED", "true")

		cfg := Load()
		if !cfg.CapabilitySyncSnapshotV2Enabled {
			t.Error("CSC_SNAPSHOT_V2_ENABLED=true was not honoured")
		}
		if cfg.CapabilitySyncLifecyclePropagationEnabled {
			t.Error("enabling the endpoint must not enable the removal path")
		}
	})

	t.Run("propagation is opt-in on its own", func(t *testing.T) {
		setSnapshotSwitchBaseline(t)
		t.Setenv("CSC_SNAPSHOT_LIFECYCLE_PROPAGATION_ENABLED", "true")

		cfg := Load()
		if !cfg.CapabilitySyncLifecyclePropagationEnabled {
			t.Error("CSC_SNAPSHOT_LIFECYCLE_PROPAGATION_ENABLED=true was not honoured")
		}
		if cfg.CapabilitySyncSnapshotV2Enabled {
			t.Error("the endpoint gate must stay independent")
		}
	})
}

// setSnapshotSwitchBaseline gives Load() the minimum it refuses to start
// without, so these tests state a default rather than inheriting whatever the
// developer's shell happens to export.
func setSnapshotSwitchBaseline(t *testing.T) {
	t.Helper()
	t.Setenv("BIND_STATE_SECRET", "test-bind-secret")
	t.Setenv("JWT_SIGN_MODE", "off")
	t.Setenv("DATABASE_URL", "postgres://user:pass@localhost:5432/db?sslmode=disable")
	t.Setenv("CSC_SNAPSHOT_V2_ENABLED", "")
	t.Setenv("CSC_SNAPSHOT_LIFECYCLE_PROPAGATION_ENABLED", "")
}
