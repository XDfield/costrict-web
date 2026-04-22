package services

import "testing"

func TestContentHashService_HashTextContent_NormalizesNewlines(t *testing.T) {
	svc := NewContentHashService()
	a, err := svc.HashTextContent("skill", "line1\r\nline2\r\n")
	if err != nil {
		t.Fatalf("hash A: %v", err)
	}
	b, err := svc.HashTextContent("skill", "line1\nline2\n")
	if err != nil {
		t.Fatalf("hash B: %v", err)
	}
	if a != b {
		t.Fatalf("expected equal hashes, got %s != %s", a, b)
	}
}

func TestContentHashService_HashTextContent_CanonicalizesMCPJSON(t *testing.T) {
	svc := NewContentHashService()
	a, err := svc.HashTextContent("mcp", `{"b":2,"a":1}`)
	if err != nil {
		t.Fatalf("hash A: %v", err)
	}
	b, err := svc.HashTextContent("mcp", "{\n  \"a\": 1,\n  \"b\": 2\n}")
	if err != nil {
		t.Fatalf("hash B: %v", err)
	}
	if a != b {
		t.Fatalf("expected equal hashes, got %s != %s", a, b)
	}
}

func TestContentHashService_HashArchiveContent_IgnoresOrderAndNormalizesText(t *testing.T) {
	svc := NewContentHashService()
	assetsA := []ArchiveAsset{{Path: "scripts/a.sh", Content: []byte("echo a\r\n")}, {Path: "config.json", Content: []byte(`{"b":2,"a":1}`)}}
	assetsB := []ArchiveAsset{{Path: "config.json", Content: []byte("{\n  \"a\": 1,\n  \"b\": 2\n}")}, {Path: "scripts/a.sh", Content: []byte("echo a\n")}}
	a, err := svc.HashArchiveContent("SKILL.md", []byte("# Demo\r\n"), assetsA)
	if err != nil {
		t.Fatalf("hash A: %v", err)
	}
	b, err := svc.HashArchiveContent("SKILL.md", []byte("# Demo\n"), assetsB)
	if err != nil {
		t.Fatalf("hash B: %v", err)
	}
	if a != b {
		t.Fatalf("expected equal hashes, got %s != %s", a, b)
	}
}

func TestContentHashService_HashArchiveContent_DetectsAssetChanges(t *testing.T) {
	svc := NewContentHashService()
	a, err := svc.HashArchiveContent("SKILL.md", []byte("# Demo\n"), []ArchiveAsset{{Path: "scripts/a.sh", Content: []byte("echo a\n")}})
	if err != nil {
		t.Fatalf("hash A: %v", err)
	}
	b, err := svc.HashArchiveContent("SKILL.md", []byte("# Demo\n"), []ArchiveAsset{{Path: "scripts/a.sh", Content: []byte("echo b\n")}})
	if err != nil {
		t.Fatalf("hash B: %v", err)
	}
	if a == b {
		t.Fatal("expected different hashes when asset content changes")
	}
}

func TestContentHashService_HashArchiveManifest_IgnoresOrder(t *testing.T) {
	svc := NewContentHashService()
	a := svc.HashArchiveManifest([]ArchiveManifestEntry{{Path: "b.txt", ContentHash: "222"}, {Path: "a.txt", ContentHash: "111"}})
	b := svc.HashArchiveManifest([]ArchiveManifestEntry{{Path: "a.txt", ContentHash: "111"}, {Path: "b.txt", ContentHash: "222"}})
	if a != b {
		t.Fatalf("expected equal manifest hashes, got %s != %s", a, b)
	}
}
