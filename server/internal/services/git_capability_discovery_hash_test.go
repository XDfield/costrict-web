package services

import (
	"encoding/hex"
	"testing"
	"time"

	"github.com/costrict/costrict-web/server/internal/gitsync"
	"github.com/costrict/costrict-web/server/internal/models"
)

func buildTestDiscoveredCapability(t *testing.T, itemType, manifestPath, content string) (*models.CapabilityItem, *models.CapabilityVersion, error) {
	t.Helper()
	binding := &models.GitCapabilityRepository{RegistryID: "registry-1", RepositoryID: "repo-projection-1"}
	repo := &gitsync.Repo{ID: gitCapabilityTestRepoID, FullName: "alice/demo"}
	entry := discoveredGitCapability{
		Path:     manifestPath,
		ItemType: itemType,
		Parsed: &ParsedItem{
			Slug: "demo", Name: "Demo", Version: "1.0.0",
			Content: content, Metadata: map[string]any{}, SourcePath: manifestPath,
		},
	}
	return buildDiscoveredCapability(
		binding, gitCapabilityTestServerID, repo,
		"https://git.example.com/alice/demo", "main", gitCapabilityTestSHA, "standalone", "user-alice",
		entry, time.Now(),
	)
}

func TestGitCapabilityDiscovery_HashesSkillContentWithSHA256(t *testing.T) {
	item, version, err := buildTestDiscoveredCapability(t, "skill", "skills/demo/skill.md", "hello world\n")
	if err != nil {
		t.Fatalf("build discovered capability: %v", err)
	}
	if len(item.ContentMD5) != 64 {
		t.Fatalf("expected 64-char SHA-256 hex, got %d chars: %q", len(item.ContentMD5), item.ContentMD5)
	}
	if _, err := hex.DecodeString(item.ContentMD5); err != nil {
		t.Fatalf("content hash is not hex: %v", err)
	}
	expected, err := NewContentHashService().HashTextContent("skill", "hello world\n")
	if err != nil {
		t.Fatalf("hash via ContentHashService: %v", err)
	}
	if item.ContentMD5 != expected {
		t.Fatalf("git discovery hash %q != DB path hash %q", item.ContentMD5, expected)
	}
	if version.ContentMD5 != item.ContentMD5 {
		t.Fatalf("version hash %q != item hash %q", version.ContentMD5, item.ContentMD5)
	}
}

func TestGitCapabilityDiscovery_HashesMCPJSONLikeDBPath(t *testing.T) {
	item, _, err := buildTestDiscoveredCapability(t, "mcp", "mcp.json", "{\n  \"a\": 1,\n  \"b\": 2\n}")
	if err != nil {
		t.Fatalf("build discovered capability: %v", err)
	}
	expected, err := NewContentHashService().HashTextContent("mcp", `{"b":2,"a":1}`)
	if err != nil {
		t.Fatalf("hash via ContentHashService: %v", err)
	}
	if item.ContentMD5 != expected {
		t.Fatalf("mcp JSON was not canonicalized like the DB path: %q != %q", item.ContentMD5, expected)
	}
}

// Git discovery classifies pyproject.toml and Markdown manifests as "mcp"
// (discoverGitCapabilityCandidates) and stores their raw text as Content. The
// JSON canonicalizer cannot hash those. Dropping them would lose capabilities
// the previous md5 hash indexed without complaint, so they must fall back to a
// plain-text SHA-256 and still produce a row.
func TestGitCapabilityDiscovery_NonJSONMCPManifestFallsBackToTextHash(t *testing.T) {
	const tomlContent = "[project]\nname = \"demo\"\n"
	item, version, err := buildTestDiscoveredCapability(t, "mcp", "pyproject.toml", tomlContent)
	if err != nil {
		t.Fatalf("non-JSON mcp manifest must not fail discovery: %v", err)
	}
	if item == nil || version == nil {
		t.Fatal("expected an item and a version for a non-JSON mcp manifest")
	}
	if len(item.ContentMD5) != 64 {
		t.Fatalf("expected 64-char SHA-256 hex, got %d chars: %q", len(item.ContentMD5), item.ContentMD5)
	}
	expected, err := NewContentHashService().HashTextContent("", tomlContent)
	if err != nil {
		t.Fatalf("hash via ContentHashService: %v", err)
	}
	if item.ContentMD5 != expected {
		t.Fatalf("TOML manifest hash %q != plain-text hash %q", item.ContentMD5, expected)
	}
}

// The common case, not an edge case: ParsePluginJSON stores a synthesized
// frontmatter+markdown summary rather than the .plugin.json bytes, so every
// discovered plugin reaches the plain-text branch even though its manifest
// path ends in .json.
func TestGitCapabilityDiscovery_PluginSynthesizedMarkdownHashesAsText(t *testing.T) {
	const synthesized = "---\nname: demo\ncategory: tools\n---\n\n## Description\n\nA demo plugin.\n"
	item, version, err := buildTestDiscoveredCapability(t, "plugin", ".plugin.json", synthesized)
	if err != nil {
		t.Fatalf("synthesized plugin content must not fail discovery: %v", err)
	}
	expected, err := NewContentHashService().HashTextContent("", synthesized)
	if err != nil {
		t.Fatalf("hash via ContentHashService: %v", err)
	}
	if item.ContentMD5 != expected {
		t.Fatalf("plugin markdown hash %q != plain-text hash %q", item.ContentMD5, expected)
	}
	if version.ContentMD5 != item.ContentMD5 {
		t.Fatalf("version hash %q != item hash %q", version.ContentMD5, item.ContentMD5)
	}
}

// Defensive: every .json parser rejects invalid JSON before discovery builds a
// capability, so this state is unreachable through the real pipeline. Pinned so
// a future parser change cannot turn it into a dropped row.
func TestGitCapabilityDiscovery_MalformedJSONManifestStillHashes(t *testing.T) {
	item, _, err := buildTestDiscoveredCapability(t, "mcp", "mcp.json", "{not json")
	if err != nil {
		t.Fatalf("malformed .json manifest must not fail discovery: %v", err)
	}
	if len(item.ContentMD5) != 64 {
		t.Fatalf("expected 64-char SHA-256 hex, got %d chars: %q", len(item.ContentMD5), item.ContentMD5)
	}
}
