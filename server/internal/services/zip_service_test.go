package services

import (
	"archive/zip"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"testing"
)

func createTestZip(t *testing.T, files map[string][]byte) []byte {
	t.Helper()

	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)

	names := make([]string, 0, len(files))
	for name := range files {
		names = append(names, name)
	}
	sort.Strings(names)

	for _, name := range names {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatalf("create zip entry %s: %v", name, err)
		}
		if _, err := w.Write(files[name]); err != nil {
			t.Fatalf("write zip entry %s: %v", name, err)
		}
	}

	if err := zw.Close(); err != nil {
		t.Fatalf("close zip writer: %v", err)
	}

	return buf.Bytes()
}

func createTestZipWithDir(t *testing.T, files map[string][]byte, topDir string) []byte {
	t.Helper()

	wrapped := make(map[string][]byte, len(files))
	for name, content := range files {
		wrapped[topDir+"/"+name] = content
	}
	return createTestZip(t, wrapped)
}

func testSHA256Hex(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func assetByPath(t *testing.T, assets []ZipAsset) map[string]ZipAsset {
	t.Helper()

	result := make(map[string]ZipAsset, len(assets))
	for _, asset := range assets {
		result[asset.Path] = asset
	}
	return result
}

func TestParseZip_SkillHappyPath(t *testing.T) {
	t.Parallel()

	skill := []byte("---\nname: Demo Skill\ndescription: Skill description\nversion: 2.3.4\n---\n# Demo\n")
	setup := []byte("#!/bin/sh\necho setup\n")
	demo := []byte("print('demo')\n")
	data := createTestZip(t, map[string][]byte{
		"SKILL.md":         skill,
		"scripts/setup.sh": setup,
		"examples/demo.py": demo,
	})

	svc := ZipService{Parser: &ParserService{}}
	result, err := svc.ParseZip(bytes.NewReader(data), int64(len(data)), "skill")
	if err != nil {
		t.Fatalf("ParseZip() error = %v", err)
	}

	if result.MainContent != string(skill) {
		t.Fatalf("MainContent mismatch")
	}
	if result.MainPath != "SKILL.md" {
		t.Fatalf("MainPath = %q, want %q", result.MainPath, "SKILL.md")
	}
	if result.MainSHA != testSHA256Hex(skill) {
		t.Fatalf("MainSHA = %q, want %q", result.MainSHA, testSHA256Hex(skill))
	}
	if result.Parsed == nil {
		t.Fatal("Parsed is nil")
	}
	if result.Parsed.Name != "Demo Skill" {
		t.Fatalf("Parsed.Name = %q, want %q", result.Parsed.Name, "Demo Skill")
	}
	if len(result.Assets) != 2 {
		t.Fatalf("len(Assets) = %d, want 2", len(result.Assets))
	}

	assets := assetByPath(t, result.Assets)
	checks := map[string]struct {
		content  []byte
		mimeType string
		binary   bool
	}{
		"scripts/setup.sh": {content: setup, mimeType: "text/x-sh", binary: false},
		"examples/demo.py": {content: demo, mimeType: "text/x-python", binary: false},
	}
	for path, check := range checks {
		asset, ok := assets[path]
		if !ok {
			t.Fatalf("missing asset %q", path)
		}
		if asset.Path != path {
			t.Fatalf("asset.Path = %q, want %q", asset.Path, path)
		}
		if asset.MimeType != check.mimeType {
			t.Fatalf("asset %q MimeType = %q, want %q", path, asset.MimeType, check.mimeType)
		}
		if asset.ContentSHA != testSHA256Hex(check.content) {
			t.Fatalf("asset %q ContentSHA = %q, want %q", path, asset.ContentSHA, testSHA256Hex(check.content))
		}
		if asset.Binary != check.binary {
			t.Fatalf("asset %q Binary = %v, want %v", path, asset.Binary, check.binary)
		}
		if asset.Size != int64(len(check.content)) {
			t.Fatalf("asset %q Size = %d, want %d", path, asset.Size, len(check.content))
		}
		if !bytes.Equal(asset.Content, check.content) {
			t.Fatalf("asset %q Content mismatch", path)
		}
	}
}

func TestParseZip_MCPHappyPath(t *testing.T) {
	t.Parallel()

	content, err := json.Marshal(map[string]any{
		"mcpServers": map[string]any{
			"my-server": map[string]any{
				"command": "npx",
				"args":    []string{"-y", "@tool/foo"},
			},
		},
	})
	if err != nil {
		t.Fatalf("json.Marshal() error = %v", err)
	}
	data := createTestZip(t, map[string][]byte{".mcp.json": content})

	svc := ZipService{Parser: &ParserService{}}
	result, err := svc.ParseZip(bytes.NewReader(data), int64(len(data)), "mcp")
	if err != nil {
		t.Fatalf("ParseZip() error = %v", err)
	}

	if result.Parsed == nil {
		t.Fatal("Parsed is nil")
	}
	if result.MainContent != string(content) {
		t.Fatalf("MainContent mismatch")
	}
	if got := result.NormalizedMeta["hosting_type"]; got != "command" {
		t.Fatalf("hosting_type = %#v, want %q", got, "command")
	}
	if got := result.NormalizedMeta["command"]; got != "npx" {
		t.Fatalf("command = %#v, want %q", got, "npx")
	}
	if got := result.NormalizedMeta["args"]; !reflect.DeepEqual(got, []any{"-y", "@tool/foo"}) {
		t.Fatalf("args = %#v, want %#v", got, []any{"-y", "@tool/foo"})
	}
}

func TestParseZip_MCPRemoteServer(t *testing.T) {
	t.Parallel()

	content, err := json.Marshal(map[string]any{
		"mcpServers": map[string]any{
			"remote-api": map[string]any{
				"url": "https://api.example.com/mcp",
			},
		},
	})
	if err != nil {
		t.Fatalf("json.Marshal() error = %v", err)
	}
	data := createTestZip(t, map[string][]byte{".mcp.json": content})

	svc := ZipService{Parser: &ParserService{}}
	result, err := svc.ParseZip(bytes.NewReader(data), int64(len(data)), "mcp")
	if err != nil {
		t.Fatalf("ParseZip() error = %v", err)
	}

	if got := result.NormalizedMeta["hosting_type"]; got != "remote" {
		t.Fatalf("hosting_type = %#v, want %q", got, "remote")
	}
	if got := result.NormalizedMeta["server_type"]; got != "http" {
		t.Fatalf("server_type = %#v, want %q", got, "http")
	}
	if got := result.NormalizedMeta["url"]; got != "https://api.example.com/mcp" {
		t.Fatalf("url = %#v, want %q", got, "https://api.example.com/mcp")
	}
}

func TestParseZip_MCPRemoteSSE(t *testing.T) {
	t.Parallel()

	content, err := json.Marshal(map[string]any{
		"mcpServers": map[string]any{
			"sse-api": map[string]any{
				"url":       "https://api.example.com/sse",
				"transport": "sse",
			},
		},
	})
	if err != nil {
		t.Fatalf("json.Marshal() error = %v", err)
	}
	data := createTestZip(t, map[string][]byte{".mcp.json": content})

	svc := ZipService{Parser: &ParserService{}}
	result, err := svc.ParseZip(bytes.NewReader(data), int64(len(data)), "mcp")
	if err != nil {
		t.Fatalf("ParseZip() error = %v", err)
	}

	if got := result.NormalizedMeta["hosting_type"]; got != "remote" {
		t.Fatalf("hosting_type = %#v, want %q", got, "remote")
	}
	if got := result.NormalizedMeta["server_type"]; got != "sse" {
		t.Fatalf("server_type = %#v, want %q", got, "sse")
	}
}

func TestParseZip_MainFileOnly(t *testing.T) {
	t.Parallel()

	skill := []byte("# Skill\n")
	data := createTestZip(t, map[string][]byte{"SKILL.md": skill})

	svc := ZipService{Parser: &ParserService{}}
	result, err := svc.ParseZip(bytes.NewReader(data), int64(len(data)), "skill")
	if err != nil {
		t.Fatalf("ParseZip() error = %v", err)
	}
	if len(result.Assets) != 0 {
		t.Fatalf("len(Assets) = %d, want 0", len(result.Assets))
	}
}

func TestParseZip_TopDirStrip(t *testing.T) {
	t.Parallel()

	data := createTestZipWithDir(t, map[string][]byte{
		"SKILL.md":       []byte("# Skill\n"),
		"scripts/run.sh": []byte("#!/bin/sh\n"),
	}, "my-skill")

	svc := ZipService{Parser: &ParserService{}}
	result, err := svc.ParseZip(bytes.NewReader(data), int64(len(data)), "skill")
	if err != nil {
		t.Fatalf("ParseZip() error = %v", err)
	}
	if result.MainPath != "SKILL.md" {
		t.Fatalf("MainPath = %q, want %q", result.MainPath, "SKILL.md")
	}
	if len(result.Assets) != 1 {
		t.Fatalf("len(Assets) = %d, want 1", len(result.Assets))
	}
	if result.Assets[0].Path != "scripts/run.sh" {
		t.Fatalf("Assets[0].Path = %q, want %q", result.Assets[0].Path, "scripts/run.sh")
	}
}

func TestParseZip_NoTopDirStrip(t *testing.T) {
	t.Parallel()

	data := createTestZip(t, map[string][]byte{
		"SKILL.md":       []byte("# Skill\n"),
		"other/file.txt": []byte("other\n"),
	})

	svc := ZipService{Parser: &ParserService{}}
	result, err := svc.ParseZip(bytes.NewReader(data), int64(len(data)), "skill")
	if err != nil {
		t.Fatalf("ParseZip() error = %v", err)
	}
	if result.MainPath != "SKILL.md" {
		t.Fatalf("MainPath = %q, want %q", result.MainPath, "SKILL.md")
	}
}

func TestParseZip_MissingMainFile_Skill(t *testing.T) {
	t.Parallel()

	data := createTestZip(t, map[string][]byte{"README.md": []byte("# Readme\n")})

	svc := ZipService{Parser: &ParserService{}}
	_, err := svc.ParseZip(bytes.NewReader(data), int64(len(data)), "skill")
	if err == nil || !strings.Contains(err.Error(), "SKILL.md") {
		t.Fatalf("ParseZip() error = %v, want missing SKILL.md", err)
	}
}

func TestParseZip_MissingMainFile_MCP(t *testing.T) {
	t.Parallel()

	data := createTestZip(t, map[string][]byte{"config.json": []byte("{}")})

	svc := ZipService{Parser: &ParserService{}}
	_, err := svc.ParseZip(bytes.NewReader(data), int64(len(data)), "mcp")
	if err == nil || !strings.Contains(err.Error(), ".mcp.json") {
		t.Fatalf("ParseZip() error = %v, want missing .mcp.json", err)
	}
}

func TestParseZip_MCPMultipleServers(t *testing.T) {
	t.Parallel()

	content, err := json.Marshal(map[string]any{
		"mcpServers": map[string]any{
			"one": map[string]any{"command": "one"},
			"two": map[string]any{"command": "two"},
		},
	})
	if err != nil {
		t.Fatalf("json.Marshal() error = %v", err)
	}
	data := createTestZip(t, map[string][]byte{".mcp.json": content})

	svc := ZipService{Parser: &ParserService{}}
	_, err = svc.ParseZip(bytes.NewReader(data), int64(len(data)), "mcp")
	if err == nil {
		t.Fatal("ParseZip() error = nil, want error")
	}
}

func TestParseZip_PathTraversal(t *testing.T) {
	t.Parallel()

	data := createTestZip(t, map[string][]byte{
		"SKILL.md":         []byte("# Skill\n"),
		"../../etc/passwd": []byte("root:x:0:0\n"),
	})

	svc := ZipService{Parser: &ParserService{}}
	_, err := svc.ParseZip(bytes.NewReader(data), int64(len(data)), "skill")
	if err == nil {
		t.Fatal("ParseZip() error = nil, want error")
	}
}

func TestParseZip_MacOSXFiltered(t *testing.T) {
	t.Parallel()

	data := createTestZip(t, map[string][]byte{
		"__MACOSX/._SKILL.md": []byte("junk"),
		"SKILL.md":            []byte("# Skill\n"),
	})

	svc := ZipService{Parser: &ParserService{}}
	result, err := svc.ParseZip(bytes.NewReader(data), int64(len(data)), "skill")
	if err != nil {
		t.Fatalf("ParseZip() error = %v", err)
	}
	if len(result.Assets) != 0 {
		t.Fatalf("len(Assets) = %d, want 0", len(result.Assets))
	}
}

func TestParseZip_HiddenFilesFiltered(t *testing.T) {
	t.Parallel()

	data := createTestZip(t, map[string][]byte{
		"SKILL.md":  []byte("# Skill\n"),
		".hidden":   []byte("secret"),
		".DS_Store": []byte("store"),
	})

	svc := ZipService{Parser: &ParserService{}}
	result, err := svc.ParseZip(bytes.NewReader(data), int64(len(data)), "skill")
	if err != nil {
		t.Fatalf("ParseZip() error = %v", err)
	}
	if len(result.Assets) != 0 {
		t.Fatalf("len(Assets) = %d, want 0", len(result.Assets))
	}
}

func TestParseZip_FileCountExceeded(t *testing.T) {
	t.Parallel()

	files := map[string][]byte{"SKILL.md": []byte("# Skill\n")}
	for i := 0; i < 500; i++ {
		files[fmt.Sprintf("assets/file-%03d.txt", i)] = []byte("x")
	}
	data := createTestZip(t, files)

	svc := ZipService{Parser: &ParserService{}}
	_, err := svc.ParseZip(bytes.NewReader(data), int64(len(data)), "skill")
	if err == nil {
		t.Fatal("ParseZip() error = nil, want error")
	}
}

func TestParseZip_SingleFileTooLarge(t *testing.T) {
	t.Parallel()

	large := bytes.Repeat([]byte("a"), MaxSingleFileSize+1)
	data := createTestZip(t, map[string][]byte{
		"SKILL.md":  []byte("# Skill\n"),
		"large.bin": large,
	})

	svc := ZipService{Parser: &ParserService{}}
	_, err := svc.ParseZip(bytes.NewReader(data), int64(len(data)), "skill")
	if err == nil {
		t.Fatal("ParseZip() error = nil, want error")
	}
}

func TestParseZip_CumulativeSizeTooLarge(t *testing.T) {
	t.Parallel()

	files := map[string][]byte{"SKILL.md": []byte("# Skill\n")}
	chunk := bytes.Repeat([]byte("b"), 9<<20)
	for i := 0; i < 6; i++ {
		files[fmt.Sprintf("assets/file-%d.bin", i)] = chunk
	}
	data := createTestZip(t, files)

	svc := ZipService{Parser: &ParserService{}}
	_, err := svc.ParseZip(bytes.NewReader(data), int64(len(data)), "skill")
	if err == nil {
		t.Fatal("ParseZip() error = nil, want error")
	}
}

func TestParseZip_UnsupportedItemType(t *testing.T) {
	t.Parallel()

	data := createTestZip(t, map[string][]byte{"SKILL.md": []byte("# Skill\n")})

	svc := ZipService{Parser: &ParserService{}}
	_, err := svc.ParseZip(bytes.NewReader(data), int64(len(data)), "hook")
	if err == nil {
		t.Fatal("ParseZip() error = nil, want error")
	}
}

func TestParseZip_AssetBinaryDetection(t *testing.T) {
	t.Parallel()

	png := []byte{0x89, 'P', 'N', 'G', 0x0d, 0x0a, 0x1a, 0x0a, 0x00}
	data := createTestZip(t, map[string][]byte{
		"SKILL.md":  []byte("# Skill\n"),
		"image.png": png,
	})

	svc := ZipService{Parser: &ParserService{}}
	result, err := svc.ParseZip(bytes.NewReader(data), int64(len(data)), "skill")
	if err != nil {
		t.Fatalf("ParseZip() error = %v", err)
	}
	if len(result.Assets) != 1 {
		t.Fatalf("len(Assets) = %d, want 1", len(result.Assets))
	}
	if !result.Assets[0].Binary {
		t.Fatal("asset Binary = false, want true")
	}
	if result.Assets[0].MimeType != "image/png" {
		t.Fatalf("MimeType = %q, want %q", result.Assets[0].MimeType, "image/png")
	}
}

func TestNormalizeMCPMetadata_NoServers(t *testing.T) {
	t.Parallel()

	parsed := &ParsedItem{Metadata: map[string]any{
		"command": "npx",
		"args":    []any{"-y", "@tool/foo"},
	}}

	meta, err := normalizeMCPMetadata(parsed)
	if err != nil {
		t.Fatalf("normalizeMCPMetadata() error = %v", err)
	}
	if got := meta["hosting_type"]; got != "command" {
		t.Fatalf("hosting_type = %#v, want %q", got, "command")
	}
	if got := meta["command"]; got != "npx" {
		t.Fatalf("command = %#v, want %q", got, "npx")
	}
}

func TestNormalizeMCPMetadata_CannotDetermine(t *testing.T) {
	t.Parallel()

	_, err := normalizeMCPMetadata(&ParsedItem{Metadata: map[string]any{"name": "demo"}})
	if err == nil {
		t.Fatal("normalizeMCPMetadata() error = nil, want error")
	}
}
