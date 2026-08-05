package gitcapability

import (
	"os"
	"strings"
)

const DefaultPluginMirrorOwner = "costrict-plugins-repo"

// PluginMirrorOwner is the namespace used by the catalog-to-Gitea mirror
// importer and the plugin fork lookup path.
func PluginMirrorOwner() string {
	if value := strings.TrimSpace(os.Getenv("PLUGIN_GIT_MIRROR_OWNER")); value != "" {
		return value
	}
	return DefaultPluginMirrorOwner
}

// DiscoveryOwnerExcluded reports whether an unbound repository owner must be
// ignored by capability discovery. The plugin mirror owner is always included:
// a system webhook covers every repository, while those repositories already
// have catalog-owned capability identities and must never be rediscovered.
// Operators may add comma-separated namespaces through the environment.
func DiscoveryOwnerExcluded(owner string) bool {
	owner = normalizeOwner(owner)
	if owner == "" {
		return false
	}
	excluded := map[string]struct{}{normalizeOwner(PluginMirrorOwner()): {}}
	for _, value := range strings.Split(os.Getenv("GIT_CAPABILITY_DISCOVERY_EXCLUDED_OWNERS"), ",") {
		if normalized := normalizeOwner(value); normalized != "" {
			excluded[normalized] = struct{}{}
		}
	}
	_, found := excluded[owner]
	return found
}

func OwnerFromFullName(fullName string) string {
	owner, _, ok := strings.Cut(strings.TrimSpace(fullName), "/")
	if !ok {
		return ""
	}
	return owner
}

func normalizeOwner(owner string) string {
	return strings.ToLower(strings.TrimSpace(owner))
}
