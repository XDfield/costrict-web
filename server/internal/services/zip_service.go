package services

import (
	"archive/zip"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"path"
	"strings"
)

const (
	MaxZipUploadSize    = 50 << 20 // 50MB
	MaxUncompressedSize = 50 << 20
	MaxSingleFileSize   = 10 << 20
	MaxFileCount        = 500
	MaxMultipartMemory  = 32 << 20
)

type ZipParseResult struct {
	MainContent    string
	MainPath       string
	MainSHA        string
	Parsed         *ParsedItem
	NormalizedMeta map[string]any
	Assets         []ZipAsset
}

type ZipAsset struct {
	Path       string
	Content    []byte
	Size       int64
	MimeType   string
	Binary     bool
	ContentSHA string
}

type ZipService struct {
	Parser *ParserService
}

type zipEntry struct {
	file *zip.File
	path string
}

func (z *ZipService) ParseZip(r io.ReaderAt, size int64, itemType string) (*ZipParseResult, error) {
	if z == nil || z.Parser == nil {
		return nil, fmt.Errorf("zip parser is not configured")
	}
	if size < 0 {
		return nil, fmt.Errorf("invalid zip size: %d", size)
	}
	if size > MaxZipUploadSize {
		return nil, fmt.Errorf("zip upload exceeds maximum size of %d bytes", MaxZipUploadSize)
	}

	mainFile := resolveMainFile(itemType)
	if mainFile == "" {
		return nil, fmt.Errorf("unsupported item type: %s", itemType)
	}

	reader, err := zip.NewReader(r, size)
	if err != nil {
		return nil, fmt.Errorf("open zip archive: %w", err)
	}

	entries := make([]zipEntry, 0, len(reader.File))
	var totalSize uint64
	for _, file := range reader.File {
		if file.FileInfo().IsDir() {
			continue
		}

		if len(entries)+1 > MaxFileCount {
			return nil, fmt.Errorf("zip archive contains more than %d files", MaxFileCount)
		}
		if file.UncompressedSize64 > MaxSingleFileSize {
			return nil, fmt.Errorf("zip entry %q exceeds maximum file size of %d bytes", file.Name, MaxSingleFileSize)
		}

		normalizedPath, err := normalizeZipPath(file.Name)
		if err != nil {
			return nil, err
		}

		totalSize += file.UncompressedSize64
		if totalSize > MaxUncompressedSize {
			return nil, fmt.Errorf("zip archive exceeds maximum uncompressed size of %d bytes", MaxUncompressedSize)
		}

		entries = append(entries, zipEntry{file: file, path: normalizedPath})
	}

	stripPrefix := commonTopLevelDir(entries)
	if stripPrefix != "" {
		prefix := stripPrefix + "/"
		for i := range entries {
			entries[i].path = strings.TrimPrefix(entries[i].path, prefix)
		}
	}

	var mainEntry *zip.File
	for _, entry := range entries {
		if entry.path == mainFile {
			mainEntry = entry.file
			break
		}
	}
	if mainEntry == nil {
		return nil, fmt.Errorf("zip archive must include %s", mainFile)
	}

	mainContent, err := readZipFile(mainEntry, MaxSingleFileSize)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", mainFile, err)
	}
	mainSHA := sha256Hex(mainContent)

	var (
		parsed         *ParsedItem
		normalizedMeta map[string]any
	)

	switch itemType {
	case "skill":
		parsed, err = z.Parser.ParseSKILLMD(mainContent, mainFile)
	case "mcp":
		var items []*ParsedItem
		items, err = z.Parser.ParseMCPJSON(mainContent, mainFile)
		if err == nil {
			if len(items) != 1 {
				return nil, fmt.Errorf(".mcp.json must contain exactly 1 server entry")
			}
			parsed = items[0]
			normalizedMeta, err = normalizeMCPMetadata(parsed)
		}
	}
	if err != nil {
		return nil, err
	}
	if parsed == nil {
		return nil, fmt.Errorf("failed to parse %s", mainFile)
	}
	parsed.ContentHash = mainSHA

	assets := make([]ZipAsset, 0)
	assetPaths := make([]string, 0)
	for _, entry := range entries {
		if entry.path == mainFile {
			continue
		}
		if shouldSkipAsset(entry.path) {
			continue
		}

		content, err := readZipFile(entry.file, MaxSingleFileSize)
		if err != nil {
			return nil, fmt.Errorf("read %s: %w", entry.path, err)
		}

		assets = append(assets, ZipAsset{
			Path:       entry.path,
			Content:    content,
			Size:       int64(len(content)),
			MimeType:   InferMimeType(entry.path),
			Binary:     isBinary(content),
			ContentSHA: sha256Hex(content),
		})
		assetPaths = append(assetPaths, entry.path)
	}
	parsed.AssetPaths = assetPaths

	return &ZipParseResult{
		MainContent:    string(mainContent),
		MainPath:       mainFile,
		MainSHA:        mainSHA,
		Parsed:         parsed,
		NormalizedMeta: normalizedMeta,
		Assets:         assets,
	}, nil
}

func normalizeMCPMetadata(parsed *ParsedItem) (map[string]any, error) {
	if parsed == nil {
		return nil, fmt.Errorf("parsed item is required")
	}

	serverConfig := cloneMap(parsed.Metadata)
	if serversRaw, ok := parsed.Metadata["mcpServers"]; ok {
		servers, ok := serversRaw.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("mcpServers must be an object")
		}
		if len(servers) != 1 {
			return nil, fmt.Errorf(".mcp.json must contain exactly 1 server entry")
		}

		for key, value := range servers {
			serverMap, ok := value.(map[string]any)
			if !ok {
				return nil, fmt.Errorf("mcp server %q must be an object", key)
			}
			serverConfig = cloneMap(serverMap)
			if _, exists := serverConfig["key"]; !exists {
				serverConfig["key"] = key
			}
			break
		}
	}

	result := cloneMap(serverConfig)
	if command, ok := result["command"].(string); ok && strings.TrimSpace(command) != "" {
		result["hosting_type"] = "command"
		return result, nil
	}

	if url, ok := result["url"].(string); ok && strings.TrimSpace(url) != "" {
		result["hosting_type"] = "remote"
		serverType := "http"
		if transport, ok := result["transport"].(string); ok && strings.EqualFold(transport, "sse") {
			serverType = "sse"
		}
		result["server_type"] = serverType
		return result, nil
	}

	return nil, fmt.Errorf("unable to determine MCP hosting type")
}

func resolveMainFile(itemType string) string {
	switch itemType {
	case "skill":
		return "SKILL.md"
	case "mcp":
		return ".mcp.json"
	default:
		return ""
	}
}

func isBinary(data []byte) bool {
	limit := len(data)
	if limit > 8192 {
		limit = 8192
	}
	for _, b := range data[:limit] {
		if b == 0 {
			return true
		}
	}
	return false
}

func normalizeZipPath(name string) (string, error) {
	slashed := strings.ReplaceAll(name, "\\", "/")
	if path.IsAbs(slashed) {
		return "", fmt.Errorf("zip entry %q has an absolute path", name)
	}

	parts := strings.Split(slashed, "/")
	cleanParts := make([]string, 0, len(parts))
	for _, part := range parts {
		switch part {
		case "", ".":
			continue
		case "..":
			return "", fmt.Errorf("zip entry %q contains path traversal", name)
		default:
			cleanParts = append(cleanParts, part)
		}
	}
	if len(cleanParts) == 0 {
		return "", fmt.Errorf("zip entry %q has an empty path", name)
	}

	return strings.Join(cleanParts, "/"), nil
}

func commonTopLevelDir(entries []zipEntry) string {
	if len(entries) == 0 {
		return ""
	}

	var prefix string
	for i, entry := range entries {
		parts := strings.Split(entry.path, "/")
		if len(parts) < 2 {
			return ""
		}
		first := parts[0]
		if strings.Contains(first, ".") {
			return ""
		}
		if i == 0 {
			prefix = first
			continue
		}
		if first != prefix {
			return ""
		}
	}

	return prefix
}

func shouldSkipAsset(relPath string) bool {
	if relPath == "__MACOSX" || strings.HasPrefix(relPath, "__MACOSX/") {
		return true
	}

	for _, part := range strings.Split(relPath, "/") {
		if strings.HasPrefix(part, ".") {
			return true
		}
	}

	return false
}

func readZipFile(file *zip.File, limit uint64) ([]byte, error) {
	if file.UncompressedSize64 > limit {
		return nil, fmt.Errorf("zip entry %q exceeds maximum file size of %d bytes", file.Name, limit)
	}

	rc, err := file.Open()
	if err != nil {
		return nil, err
	}
	defer rc.Close()

	content, err := io.ReadAll(io.LimitReader(rc, int64(limit)+1))
	if err != nil {
		return nil, err
	}
	if len(content) > int(limit) {
		return nil, fmt.Errorf("zip entry %q exceeds maximum file size of %d bytes", file.Name, limit)
	}
	return content, nil
}

func sha256Hex(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func cloneMap(input map[string]any) map[string]any {
	if len(input) == 0 {
		return map[string]any{}
	}

	result := make(map[string]any, len(input))
	for key, value := range input {
		result[key] = value
	}
	return result
}
