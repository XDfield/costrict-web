package gitcapability

import "testing"

func TestDiscoveryOwnerExcludedIncludesMirrorAndConfiguredOwners(t *testing.T) {
	t.Setenv("PLUGIN_GIT_MIRROR_OWNER", "Private-Mirror")
	t.Setenv("GIT_CAPABILITY_DISCOVERY_EXCLUDED_OWNERS", "archive, Legacy_Seed")

	for _, owner := range []string{"private-mirror", "PRIVATE-MIRROR", "archive", "legacy_seed"} {
		if !DiscoveryOwnerExcluded(owner) {
			t.Fatalf("owner %q was not excluded", owner)
		}
	}
	if DiscoveryOwnerExcluded("alice") {
		t.Fatal("ordinary user owner was excluded")
	}
}

func TestPluginMirrorOwnerDefaults(t *testing.T) {
	t.Setenv("PLUGIN_GIT_MIRROR_OWNER", "")
	if got := PluginMirrorOwner(); got != DefaultPluginMirrorOwner {
		t.Fatalf("PluginMirrorOwner() = %q, want %q", got, DefaultPluginMirrorOwner)
	}
}
