package services

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
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

func createTestTarGz(t *testing.T, files map[string][]byte) []byte {
	t.Helper()

	var buf bytes.Buffer
	gw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gw)

	names := make([]string, 0, len(files))
	for name := range files {
		names = append(names, name)
	}
	sort.Strings(names)

	for _, name := range names {
		content := files[name]
		hdr := &tar.Header{
			Name:     name,
			Size:     int64(len(content)),
			Mode:     0o644,
			Typeflag: tar.TypeReg,
		}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatalf("write tar header %s: %v", name, err)
		}
		if _, err := tw.Write(content); err != nil {
			t.Fatalf("write tar entry %s: %v", name, err)
		}
	}

	if err := tw.Close(); err != nil {
		t.Fatalf("close tar writer: %v", err)
	}
	if err := gw.Close(); err != nil {
		t.Fatalf("close gzip writer: %v", err)
	}

	return buf.Bytes()
}

func createTestTarGzWithDir(t *testing.T, files map[string][]byte, topDir string) []byte {
	t.Helper()

	wrapped := make(map[string][]byte, len(files))
	for name, content := range files {
		wrapped[topDir+"/"+name] = content
	}
	return createTestTarGz(t, wrapped)
}

func testSHA256Hex(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func assetByPath(t *testing.T, assets []ArchiveAsset) map[string]ArchiveAsset {
	t.Helper()

	result := make(map[string]ArchiveAsset, len(assets))
	for _, asset := range assets {
		result[asset.Path] = asset
	}
	return result
}

func TestParseArchive_Zip_SkillHappyPath(t *testing.T) {
	t.Parallel()

	skill := []byte("---\nname: Demo Skill\ndescription: Skill description\nversion: 2.3.4\n---\n# Demo\n")
	setup := []byte("#!/bin/sh\necho setup\n")
	demo := []byte("print('demo')\n")
	data := createTestZip(t, map[string][]byte{
		"SKILL.md":         skill,
		"scripts/setup.sh": setup,
		"examples/demo.py": demo,
	})

	svc := ArchiveService{Parser: &ParserService{}}
	result, err := svc.ParseArchive(bytes.NewReader(data), int64(len(data)), "test.zip", "skill")
	if err != nil {
		t.Fatalf("ParseArchive() error = %v", err)
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

func TestParseArchive_Zip_MCPHappyPath(t *testing.T) {
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

	svc := ArchiveService{Parser: &ParserService{}}
	result, err := svc.ParseArchive(bytes.NewReader(data), int64(len(data)), "test.zip", "mcp")
	if err != nil {
		t.Fatalf("ParseArchive() error = %v", err)
	}

	if result.Parsed == nil {
		t.Fatal("Parsed is nil")
	}
	if result.MainContent != string(content) {
		t.Fatalf("MainContent mismatch")
	}
	if _, hasType := result.NormalizedMeta["type"]; hasType {
		t.Fatalf("stdio format should not have type, got %v", result.NormalizedMeta["type"])
	}
	if got := result.NormalizedMeta["command"]; got != "npx" {
		t.Fatalf("command = %#v, want %q", got, "npx")
	}
	expectedArgs := []any{"-y", "@tool/foo"}
	if got := result.NormalizedMeta["args"]; !reflect.DeepEqual(got, expectedArgs) {
		t.Fatalf("args = %#v, want %#v", got, expectedArgs)
	}
}

func TestParseArchive_Zip_MCPRemoteServer(t *testing.T) {
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

	svc := ArchiveService{Parser: &ParserService{}}
	result, err := svc.ParseArchive(bytes.NewReader(data), int64(len(data)), "test.zip", "mcp")
	if err != nil {
		t.Fatalf("ParseArchive() error = %v", err)
	}

	if got := result.NormalizedMeta["type"]; got != "http" {
		t.Fatalf("type = %#v, want %q", got, "http")
	}
	if got := result.NormalizedMeta["url"]; got != "https://api.example.com/mcp" {
		t.Fatalf("url = %#v, want %q", got, "https://api.example.com/mcp")
	}
}

func TestParseArchive_Zip_MCPRemoteSSE(t *testing.T) {
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

	svc := ArchiveService{Parser: &ParserService{}}
	result, err := svc.ParseArchive(bytes.NewReader(data), int64(len(data)), "test.zip", "mcp")
	if err != nil {
		t.Fatalf("ParseArchive() error = %v", err)
	}

	if got := result.NormalizedMeta["type"]; got != "http" {
		t.Fatalf("type = %#v, want %q", got, "http")
	}
}

func TestParseArchive_Zip_MainFileOnly(t *testing.T) {
	t.Parallel()

	skill := []byte("# Skill\n")
	data := createTestZip(t, map[string][]byte{"SKILL.md": skill})

	svc := ArchiveService{Parser: &ParserService{}}
	result, err := svc.ParseArchive(bytes.NewReader(data), int64(len(data)), "test.zip", "skill")
	if err != nil {
		t.Fatalf("ParseArchive() error = %v", err)
	}
	if len(result.Assets) != 0 {
		t.Fatalf("len(Assets) = %d, want 0", len(result.Assets))
	}
}

func TestParseArchive_Zip_TopDirStrip(t *testing.T) {
	t.Parallel()

	data := createTestZipWithDir(t, map[string][]byte{
		"SKILL.md":       []byte("# Skill\n"),
		"scripts/run.sh": []byte("#!/bin/sh\n"),
	}, "my-skill")

	svc := ArchiveService{Parser: &ParserService{}}
	result, err := svc.ParseArchive(bytes.NewReader(data), int64(len(data)), "test.zip", "skill")
	if err != nil {
		t.Fatalf("ParseArchive() error = %v", err)
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

func TestParseArchive_Zip_NoTopDirStrip(t *testing.T) {
	t.Parallel()

	data := createTestZip(t, map[string][]byte{
		"SKILL.md":       []byte("# Skill\n"),
		"other/file.txt": []byte("other\n"),
	})

	svc := ArchiveService{Parser: &ParserService{}}
	result, err := svc.ParseArchive(bytes.NewReader(data), int64(len(data)), "test.zip", "skill")
	if err != nil {
		t.Fatalf("ParseArchive() error = %v", err)
	}
	if result.MainPath != "SKILL.md" {
		t.Fatalf("MainPath = %q, want %q", result.MainPath, "SKILL.md")
	}
}

func TestParseArchive_Zip_MissingMainFile_Skill(t *testing.T) {
	t.Parallel()

	data := createTestZip(t, map[string][]byte{"README.md": []byte("# Readme\n")})

	svc := ArchiveService{Parser: &ParserService{}}
	_, err := svc.ParseArchive(bytes.NewReader(data), int64(len(data)), "test.zip", "skill")
	if err == nil || !strings.Contains(err.Error(), "SKILL.md") {
		t.Fatalf("ParseArchive() error = %v, want missing SKILL.md", err)
	}
}

func TestParseArchive_Zip_MissingMainFile_MCP(t *testing.T) {
	t.Parallel()

	data := createTestZip(t, map[string][]byte{"config.json": []byte("{}")})

	svc := ArchiveService{Parser: &ParserService{}}
	_, err := svc.ParseArchive(bytes.NewReader(data), int64(len(data)), "test.zip", "mcp")
	if err == nil || !strings.Contains(err.Error(), ".mcp.json") {
		t.Fatalf("ParseArchive() error = %v, want missing .mcp.json", err)
	}
}

func TestParseArchive_Zip_MCPMultipleServers(t *testing.T) {
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

	svc := ArchiveService{Parser: &ParserService{}}
	_, err = svc.ParseArchive(bytes.NewReader(data), int64(len(data)), "test.zip", "mcp")
	if err == nil {
		t.Fatal("ParseArchive() error = nil, want error")
	}
}

func TestParseArchive_Zip_PathTraversal(t *testing.T) {
	t.Parallel()

	data := createTestZip(t, map[string][]byte{
		"SKILL.md":         []byte("# Skill\n"),
		"../../etc/passwd": []byte("root:x:0:0\n"),
	})

	svc := ArchiveService{Parser: &ParserService{}}
	_, err := svc.ParseArchive(bytes.NewReader(data), int64(len(data)), "test.zip", "skill")
	if err == nil {
		t.Fatal("ParseArchive() error = nil, want error")
	}
}

func TestParseArchive_Zip_MacOSXFiltered(t *testing.T) {
	t.Parallel()

	data := createTestZip(t, map[string][]byte{
		"__MACOSX/._SKILL.md": []byte("junk"),
		"SKILL.md":            []byte("# Skill\n"),
	})

	svc := ArchiveService{Parser: &ParserService{}}
	result, err := svc.ParseArchive(bytes.NewReader(data), int64(len(data)), "test.zip", "skill")
	if err != nil {
		t.Fatalf("ParseArchive() error = %v", err)
	}
	if len(result.Assets) != 0 {
		t.Fatalf("len(Assets) = %d, want 0", len(result.Assets))
	}
}

func TestParseArchive_Zip_HiddenFilesFiltered(t *testing.T) {
	t.Parallel()

	data := createTestZip(t, map[string][]byte{
		"SKILL.md":  []byte("# Skill\n"),
		".hidden":   []byte("secret"),
		".DS_Store": []byte("store"),
	})

	svc := ArchiveService{Parser: &ParserService{}}
	result, err := svc.ParseArchive(bytes.NewReader(data), int64(len(data)), "test.zip", "skill")
	if err != nil {
		t.Fatalf("ParseArchive() error = %v", err)
	}
	if len(result.Assets) != 0 {
		t.Fatalf("len(Assets) = %d, want 0", len(result.Assets))
	}
}

func TestParseArchive_Zip_FileCountExceeded(t *testing.T) {
	t.Parallel()

	files := map[string][]byte{"SKILL.md": []byte("# Skill\n")}
	for i := 0; i < 500; i++ {
		files[fmt.Sprintf("assets/file-%03d.txt", i)] = []byte("x")
	}
	data := createTestZip(t, files)

	svc := ArchiveService{Parser: &ParserService{}}
	_, err := svc.ParseArchive(bytes.NewReader(data), int64(len(data)), "test.zip", "skill")
	if err == nil {
		t.Fatal("ParseArchive() error = nil, want error")
	}
}

func TestParseArchive_Zip_SingleFileTooLarge(t *testing.T) {
	t.Parallel()

	large := bytes.Repeat([]byte("a"), MaxSingleFileSize+1)
	data := createTestZip(t, map[string][]byte{
		"SKILL.md":  []byte("# Skill\n"),
		"large.bin": large,
	})

	svc := ArchiveService{Parser: &ParserService{}}
	_, err := svc.ParseArchive(bytes.NewReader(data), int64(len(data)), "test.zip", "skill")
	if err == nil {
		t.Fatal("ParseArchive() error = nil, want error")
	}
}

func TestParseArchive_Zip_CumulativeSizeTooLarge(t *testing.T) {
	t.Parallel()

	files := map[string][]byte{"SKILL.md": []byte("# Skill\n")}
	chunk := bytes.Repeat([]byte("b"), 9<<20)
	for i := 0; i < 6; i++ {
		files[fmt.Sprintf("assets/file-%d.bin", i)] = chunk
	}
	data := createTestZip(t, files)

	svc := ArchiveService{Parser: &ParserService{}}
	_, err := svc.ParseArchive(bytes.NewReader(data), int64(len(data)), "test.zip", "skill")
	if err == nil {
		t.Fatal("ParseArchive() error = nil, want error")
	}
}

func TestParseArchive_Zip_UnsupportedItemType(t *testing.T) {
	t.Parallel()

	data := createTestZip(t, map[string][]byte{"SKILL.md": []byte("# Skill\n")})

	svc := ArchiveService{Parser: &ParserService{}}
	_, err := svc.ParseArchive(bytes.NewReader(data), int64(len(data)), "test.zip", "hook")
	if err == nil {
		t.Fatal("ParseArchive() error = nil, want error")
	}
}

func TestParseArchive_Zip_AssetBinaryDetection(t *testing.T) {
	t.Parallel()

	png := []byte{0x89, 'P', 'N', 'G', 0x0d, 0x0a, 0x1a, 0x0a, 0x00}
	data := createTestZip(t, map[string][]byte{
		"SKILL.md":  []byte("# Skill\n"),
		"image.png": png,
	})

	svc := ArchiveService{Parser: &ParserService{}}
	result, err := svc.ParseArchive(bytes.NewReader(data), int64(len(data)), "test.zip", "skill")
	if err != nil {
		t.Fatalf("ParseArchive() error = %v", err)
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

func TestParseArchive_TarGz_SkillHappyPath(t *testing.T) {
	t.Parallel()

	skill := []byte("---\nname: Demo Skill\ndescription: Skill description\nversion: 2.3.4\n---\n# Demo\n")
	setup := []byte("#!/bin/sh\necho setup\n")
	demo := []byte("print('demo')\n")
	data := createTestTarGz(t, map[string][]byte{
		"SKILL.md":         skill,
		"scripts/setup.sh": setup,
		"examples/demo.py": demo,
	})
	svc := ArchiveService{Parser: &ParserService{}}
	result, err := svc.ParseArchive(bytes.NewReader(data), int64(len(data)), "test.tar.gz", "skill")
	if err != nil {
		t.Fatalf("ParseArchive() error = %v", err)
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

func TestParseArchive_TarGz_MCPHappyPath(t *testing.T) {
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
	data := createTestTarGz(t, map[string][]byte{".mcp.json": content})

	svc := ArchiveService{Parser: &ParserService{}}
	result, err := svc.ParseArchive(bytes.NewReader(data), int64(len(data)), "test.tar.gz", "mcp")
	if err != nil {
		t.Fatalf("ParseArchive() error = %v", err)
	}

	if result.Parsed == nil {
		t.Fatal("Parsed is nil")
	}
	if result.MainContent != string(content) {
		t.Fatalf("MainContent mismatch")
	}
	if _, hasType := result.NormalizedMeta["type"]; hasType {
		t.Fatalf("stdio format should not have type, got %v", result.NormalizedMeta["type"])
	}
	if got := result.NormalizedMeta["command"]; got != "npx" {
		t.Fatalf("command = %#v, want %q", got, "npx")
	}
	expectedArgs := []any{"-y", "@tool/foo"}
	if got := result.NormalizedMeta["args"]; !reflect.DeepEqual(got, expectedArgs) {
		t.Fatalf("args = %#v, want %#v", got, expectedArgs)
	}
}

func TestParseArchive_TarGz_TopDirStrip(t *testing.T) {
	t.Parallel()

	data := createTestTarGzWithDir(t, map[string][]byte{
		"SKILL.md":       []byte("# Skill\n"),
		"scripts/run.sh": []byte("#!/bin/sh\n"),
	}, "my-skill")

	svc := ArchiveService{Parser: &ParserService{}}
	result, err := svc.ParseArchive(bytes.NewReader(data), int64(len(data)), "test.tar.gz", "skill")
	if err != nil {
		t.Fatalf("ParseArchive() error = %v", err)
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

func TestParseArchive_TarGz_PathTraversal(t *testing.T) {
	t.Parallel()

	data := createTestTarGz(t, map[string][]byte{
		"SKILL.md":         []byte("# Skill\n"),
		"../../etc/passwd": []byte("root:x:0:0\n"),
	})

	svc := ArchiveService{Parser: &ParserService{}}
	_, err := svc.ParseArchive(bytes.NewReader(data), int64(len(data)), "test.tar.gz", "skill")
	if err == nil {
		t.Fatal("ParseArchive() error = nil, want error")
	}
}

func TestParseArchive_TarGz_Tgz(t *testing.T) {
	t.Parallel()

	skill := []byte("---\nname: Demo Skill\ndescription: Skill description\nversion: 2.3.4\n---\n# Demo\n")
	setup := []byte("#!/bin/sh\necho setup\n")
	demo := []byte("print('demo')\n")
	data := createTestTarGz(t, map[string][]byte{
		"SKILL.md":         skill,
		"scripts/setup.sh": setup,
		"examples/demo.py": demo,
	})
	svc := ArchiveService{Parser: &ParserService{}}
	result, err := svc.ParseArchive(bytes.NewReader(data), int64(len(data)), "test.tgz", "skill")
	if err != nil {
		t.Fatalf("ParseArchive() error = %v", err)
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

func TestParseArchive_UnsupportedFormat(t *testing.T) {
	t.Parallel()

	data := createTestZip(t, map[string][]byte{"SKILL.md": []byte("# Skill\n")})

	svc := ArchiveService{Parser: &ParserService{}}
	_, err := svc.ParseArchive(bytes.NewReader(data), int64(len(data)), "test.rar", "skill")
	if err == nil || !strings.Contains(err.Error(), "unsupported") {
		t.Fatalf("ParseArchive() error = %v, want unsupported format", err)
	}
}

func TestParseArchive_EmptyFilename(t *testing.T) {
	t.Parallel()

	data := createTestZip(t, map[string][]byte{"SKILL.md": []byte("# Skill\n")})

	svc := ArchiveService{Parser: &ParserService{}}
	_, err := svc.ParseArchive(bytes.NewReader(data), int64(len(data)), "", "skill")
	if err == nil || !strings.Contains(err.Error(), "unsupported archive format") {
		t.Fatalf("ParseArchive() error = %v, want empty filename error", err)
	}
}

func TestNormalizeMCPMetadata_NoServers(t *testing.T) {
	t.Parallel()

	parsed := &ParsedItem{Metadata: map[string]any{
		"command": "npx",
		"args":    []any{"-y", "@tool/foo"},
	}}

	meta, err := NormalizeMCPMetadata(parsed.Metadata)
	if err != nil {
		t.Fatalf("NormalizeMCPMetadata() error = %v", err)
	}
	if _, hasType := meta["type"]; hasType {
		t.Fatalf("stdio format should not have type, got %v", meta["type"])
	}
	if got := meta["command"]; got != "npx" {
		t.Fatalf("command = %#v, want %q", got, "npx")
	}
	expectedArgs := []any{"-y", "@tool/foo"}
	if got := meta["args"]; !reflect.DeepEqual(got, expectedArgs) {
		t.Fatalf("args = %#v, want %#v", got, expectedArgs)
	}

}

func TestNormalizeMCPMetadata_CannotDetermine(t *testing.T) {
	t.Parallel()

	_, err := NormalizeMCPMetadata(map[string]any{"name": "demo"})
	if err == nil {
		t.Fatal("NormalizeMCPMetadata() error = nil, want error")
	}
}
