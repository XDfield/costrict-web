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
	if it.Content != "" {
		t.Errorf("Content = %q, want empty (plugin has no server-side content)", it.Content)
	}
	install, _ := it.Metadata["install"].(map[string]any)
	if install["plugin_name"] != "gitnexus" {
		t.Errorf("Metadata.install.plugin_name not preserved")
	}
	if _, ok := it.Metadata["bundle"]; !ok {
		t.Errorf("Metadata.bundle missing")
	}
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
