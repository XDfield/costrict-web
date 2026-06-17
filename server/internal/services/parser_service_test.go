package services

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestParsePluginJSON_HappyPath(t *testing.T) {
	p := &ParserService{}
	content := []byte(`{
  "name": "GitNexus",
  "description": "Code intelligence powered by a knowledge graph.",
  "category": "tooling",
  "tags": ["git", "graph"],
  "install": {
    "method": "plugin_marketplace",
    "plugin_name": "gitnexus",
    "marketplace_name": "gitnexus-marketplace",
    "marketplace_repo": "abhigyanpatwari/GitNexus",
    "marketplace_verified": true
  },
  "bundle": {
    "skills_count": 0,
    "commands_count": 2
  }
}`)
	items, err := p.ParsePluginJSON(content, "plugins/abhigyanpatwari-gitnexus-marketplace-gitnexus/.plugin.json")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(items) != 1 {
		t.Fatalf("expected 1 item, got %d", len(items))
	}
	it := items[0]
	if it.ItemType != "plugin" {
		t.Errorf("ItemType = %q, want plugin", it.ItemType)
	}
	if it.Name != "GitNexus" {
		t.Errorf("Name = %q, want top-level display name 'GitNexus'", it.Name)
	}
	if it.Description != "Code intelligence powered by a knowledge graph." {
		t.Errorf("Description = %q, want top-level description", it.Description)
	}
	if it.Category != "tooling" {
		t.Errorf("Category = %q, want tooling", it.Category)
	}
	if len(it.Tags) != 2 || it.Tags[0] != "git" || it.Tags[1] != "graph" {
		t.Errorf("Tags = %v, want [git graph]", it.Tags)
	}
	if it.Slug != "abhigyanpatwari-gitnexus-marketplace-gitnexus" {
		t.Errorf("Slug = %q, want kebab from dir name", it.Slug)
	}
	// Content is synthesised markdown — assert key sections are present.
	if !strings.Contains(it.Content, "---\nname: \"GitNexus\"") {
		t.Errorf("Content missing YAML frontmatter; got prefix %q", it.Content[:min(80, len(it.Content))])
	}
	if !strings.Contains(it.Content, "# GitNexus") {
		t.Errorf("Content missing display-name heading")
	}
	if !strings.Contains(it.Content, "## Installation") {
		t.Errorf("Content missing Installation section")
	}
	if strings.Contains(it.Content, "csc plugin marketplace add") {
		t.Errorf("Install copy must not include a marketplace add step: the unified costrict-plugins marketplace ships preconfigured in the csc client")
	}
	if !strings.Contains(it.Content, "csc plugin install gitnexus@costrict-plugins") {
		t.Errorf("Content missing csc plugin install command targeting costrict-plugins")
	}
	if !strings.Contains(it.Content, "## Upstream Source") {
		t.Errorf("Content missing Upstream Source section")
	}
	if !strings.Contains(it.Content, "github.com/abhigyanpatwari/GitNexus") {
		t.Errorf("Content missing upstream repo link")
	}
	install, _ := it.Metadata["install"].(map[string]any)
	if install["plugin_name"] != "gitnexus" {
		t.Errorf("Metadata.install.plugin_name not preserved")
	}
	if _, ok := it.Metadata["bundle"]; !ok {
		t.Errorf("Metadata.bundle missing")
	}
}

func TestParsePluginJSON_DescriptionAndTagsFallback(t *testing.T) {
	p := &ParserService{}
	// Catalog entry with empty description + empty tags — should fall back.
	content := []byte(`{
  "name": "repomix-mcp",
  "description": "",
  "category": "tooling",
  "tags": [],
  "install": {
    "method": "plugin_marketplace",
    "plugin_name": "repomix-mcp",
    "marketplace_name": "repomix",
    "marketplace_repo": "yamadashy/repomix",
    "marketplace_verified": true
  },
  "bundle": {
    "skills_count": 0,
    "commands_count": 0,
    "agents_count": 0,
    "mcp_servers_count": 1,
    "hooks_count": 0
  }
}`)
	items, err := p.ParsePluginJSON(content, "plugins/yamadashy-repomix-repomix-mcp/.plugin.json")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	it := items[0]
	if it.Description == "" {
		t.Errorf("Description fallback should populate when upstream is empty")
	}
	if !strings.Contains(it.Description, "yamadashy/repomix") {
		t.Errorf("Description fallback should mention marketplace_repo, got %q", it.Description)
	}
	wantTags := []string{"mcp", "tooling"}
	if len(it.Tags) != len(wantTags) {
		t.Fatalf("Tags = %v, want %v", it.Tags, wantTags)
	}
	for i, want := range wantTags {
		if it.Tags[i] != want {
			t.Errorf("Tags[%d] = %q, want %q", i, it.Tags[i], want)
		}
	}
}

func TestParsePluginJSON_BundleTagsOrderStable(t *testing.T) {
	p := &ParserService{}
	content := []byte(`{
  "name": "x",
  "category": "frontend",
  "install": {
    "method": "plugin_marketplace",
    "plugin_name": "x",
    "marketplace_name": "mp",
    "marketplace_repo": "o/r",
    "marketplace_verified": true
  },
  "bundle": {
    "skills_count": 3,
    "commands_count": 2,
    "agents_count": 1,
    "mcp_servers_count": 1,
    "hooks_count": 1
  }
}`)
	items, err := p.ParsePluginJSON(content, "plugins/x/.plugin.json")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	want := []string{"skills", "commands", "agents", "mcp", "hooks", "frontend"}
	if len(items[0].Tags) != len(want) {
		t.Fatalf("Tags = %v, want %v", items[0].Tags, want)
	}
	for i, w := range want {
		if items[0].Tags[i] != w {
			t.Errorf("Tags[%d] = %q, want %q", i, items[0].Tags[i], w)
		}
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func TestParsePluginJSON_FallsBackToPluginNameIfTopLevelNameMissing(t *testing.T) {
	p := &ParserService{}
	content := []byte(`{
  "install": {
    "method": "plugin_marketplace",
    "plugin_name": "foo",
    "marketplace_name": "mp",
    "marketplace_repo": "o/r",
    "marketplace_verified": true
  }
}`)
	items, err := p.ParsePluginJSON(content, "plugins/x/.plugin.json")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if items[0].Name != "foo" {
		t.Errorf("Name = %q, want fallback to install.plugin_name 'foo'", items[0].Name)
	}
}

func TestParsePluginJSON_MissingInstall(t *testing.T) {
	p := &ParserService{}
	_, err := p.ParsePluginJSON([]byte(`{"bundle": {}}`), "plugins/x/.plugin.json")
	if err == nil {
		t.Fatalf("expected error for missing install")
	}
	if !strings.Contains(err.Error(), "install") {
		t.Errorf("error should mention install, got %q", err.Error())
	}
}

func TestParsePluginJSON_MissingRequiredField(t *testing.T) {
	p := &ParserService{}
	content := []byte(`{
  "install": {
    "method": "plugin_marketplace",
    "plugin_name": "foo"
  }
}`)
	_, err := p.ParsePluginJSON(content, "plugins/x/.plugin.json")
	if err == nil {
		t.Fatalf("expected error for missing marketplace_name/marketplace_repo")
	}
}

func TestParsePluginJSON_InvalidJSON(t *testing.T) {
	p := &ParserService{}
	_, err := p.ParsePluginJSON([]byte(`{not json`), "plugins/x/.plugin.json")
	if err == nil {
		t.Fatalf("expected JSON parse error")
	}
}

func TestParsePluginJSON_MetadataStableAcrossWhitespace(t *testing.T) {
	p := &ParserService{}
	compact := []byte(`{"install":{"method":"plugin_marketplace","plugin_name":"foo","marketplace_name":"mp","marketplace_repo":"o/r","marketplace_verified":true}}`)
	pretty := []byte(`{
  "install": {
    "method":              "plugin_marketplace",
    "plugin_name":         "foo",
    "marketplace_name":    "mp",
    "marketplace_repo":    "o/r",
    "marketplace_verified": true
  }
}`)
	a, err := p.ParsePluginJSON(compact, "plugins/foo/.plugin.json")
	if err != nil {
		t.Fatalf("compact parse: %v", err)
	}
	b, err := p.ParsePluginJSON(pretty, "plugins/foo/.plugin.json")
	if err != nil {
		t.Fatalf("pretty parse: %v", err)
	}
	aBytes, _ := json.Marshal(a[0].Metadata)
	bBytes, _ := json.Marshal(b[0].Metadata)
	if string(aBytes) != string(bBytes) {
		t.Errorf("metadata canonical bytes differ:\n  compact=%s\n  pretty =%s", aBytes, bBytes)
	}
}

func TestInferItemType_PluginJSON(t *testing.T) {
	p := &ParserService{}
	if got := p.InferItemType("plugins/foo/.plugin.json"); got != "plugin" {
		t.Errorf("InferItemType = %q, want plugin", got)
	}
}

func TestInferItemType_PluginChildTypes(t *testing.T) {
	p := &ParserService{}
	cases := []struct {
		path string
		want string
	}{
		// Standard Claude-Code plugin layout.
		{"skills/requirement-analysis/SKILL.md", "skill"},
		{"evaluators/aireq-evaluator/SKILL.md", "skill"}, // evaluators are SKILL.md-shaped
		{"agents/reviewer.md", "subagent"},
		{"commands/run-tests.md", "command"},
		{"hooks/post-commit.md", "hook"},
		// New rule/template types (cospower convention, generic).
		{"rules/coding-standards/go-checklist.md", "rule"},
		{"rules/dfx/安全.md", "rule"}, // nested + non-ASCII leaf
		{"templates/system-design.md", "template"},
		// Plugin-prefixed paths (children live under the plugin dir at install).
		{"my-plugin/rules/dfx/安全.md", "rule"},
		{"my-plugin/templates/x.md", "template"},
		{"my-plugin/commands/foo.md", "command"},
		{"my-plugin/agents/bar.md", "subagent"},
		// Non-special markdown still collapses to skill.
		{"docs/usage.md", "skill"},
		{"PROMPT.md", "skill"},
	}
	for _, tc := range cases {
		if got := p.InferItemType(tc.path); got != tc.want {
			t.Errorf("InferItemType(%q) = %q, want %q", tc.path, got, tc.want)
		}
	}
}

func TestInferSlug_NestedRulesAndTemplates(t *testing.T) {
	p := &ParserService{}
	cases := []struct {
		path string
		want string
	}{
		// Top-level dir name dropped; nested group segment kept for uniqueness.
		{"rules/coding-standards/go-checklist.md", "coding-standards-go-checklist"},
		{"templates/system-design.md", "system-design"},
		{"evaluators/aireq-evaluator/SKILL.md", "aireq-evaluator"},
		// Two same-leaf rules under different groups must NOT collide.
		{"rules/dfx/checklist.md", "dfx-checklist"},
		{"rules/security/checklist.md", "security-checklist"},
	}
	seen := map[string]string{}
	for _, tc := range cases {
		got := p.InferSlug(tc.path)
		if got != tc.want {
			t.Errorf("InferSlug(%q) = %q, want %q", tc.path, got, tc.want)
		}
		if prev, ok := seen[got]; ok {
			t.Errorf("InferSlug collision: %q and %q both produced %q", prev, tc.path, got)
		}
		seen[got] = tc.path
	}
}
