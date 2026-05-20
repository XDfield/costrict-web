package services

import "testing"

func TestParseFile_DispatchesPluginJSON(t *testing.T) {
	s := &SyncService{Parser: &ParserService{}}
	content := []byte(`{
  "install": {
    "method": "plugin_marketplace",
    "plugin_name": "foo",
    "marketplace_name": "mp",
    "marketplace_repo": "o/r",
    "marketplace_verified": true
  }
}`)
	items, err := s.parseFile(content, "plugins/foo-bar/.plugin.json")
	if err != nil {
		t.Fatalf("parseFile: %v", err)
	}
	if len(items) != 1 {
		t.Fatalf("expected 1 ParsedItem, got %d", len(items))
	}
	if items[0].ItemType != "plugin" {
		t.Errorf("ItemType = %q, want plugin (dispatched to wrong parser)", items[0].ItemType)
	}
	if items[0].Slug != "foo-bar" {
		t.Errorf("Slug = %q, want foo-bar (from dir name)", items[0].Slug)
	}
}
