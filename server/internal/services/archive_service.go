package services

import (
	"archive/tar"
	"archive/zip"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"path"
	"strings"
)

const (
	MaxArchiveUploadSize = 50 << 20 // 50MB
	MaxUncompressedSize  = 50 << 20
	MaxSingleFileSize    = 10 << 20
	MaxFileCount         = 500
	MaxMultipartMemory   = 32 << 20
)

type ArchiveParseResult struct {
	MainContent    string
	MainPath       string
	MainSHA        string
	Parsed         *ParsedItem
	NormalizedMeta map[string]any
	Assets         []ArchiveAsset
}

type ArchiveAsset struct {
	Path       string
	Content    []byte
	Size       int64
	MimeType   string
	Binary     bool
	ContentSHA string
}

type ArchiveService struct {
	Parser *ParserService
}

type archiveEntry struct {
	path string
	size uint64
	read func(limit uint64) ([]byte, error)
}

func (a *ArchiveService) ParseArchive(r io.ReaderAt, size int64, filename string, itemType string) (*ArchiveParseResult, error) {
	if a == nil || a.Parser == nil {
		return nil, fmt.Errorf("archive parser is not configured")
	}
	if size < 0 {
		return nil, fmt.Errorf("invalid archive size: %d", size)
	}
	if size > MaxArchiveUploadSize {
		return nil, fmt.Errorf("archive upload exceeds maximum size of %d bytes", MaxArchiveUploadSize)
	}

	mainFile := resolveMainFile(itemType)
	if mainFile == "" {
		return nil, fmt.Errorf("unsupported item type: %s", itemType)
	}

	format, err := detectArchiveFormat(filename)
	if err != nil {
		return nil, err
	}

	var rawEntries []archiveEntry
	switch format {
	case "zip":
		rawEntries, err = enumerateZipEntries(r, size)
	case "targz":
		rawEntries, err = enumerateTarGzEntries(r, size)
	default:
		return nil, fmt.Errorf("unsupported archive format: %s", format)
	}
	if err != nil {
		return nil, err
	}

	entries := make([]archiveEntry, 0, len(rawEntries))
	var totalSize uint64
	for _, entry := range rawEntries {
		if len(entries)+1 > MaxFileCount {
			return nil, fmt.Errorf("archive contains more than %d files", MaxFileCount)
		}
		if entry.size > MaxSingleFileSize {
			return nil, fmt.Errorf("archive entry %q exceeds maximum file size of %d bytes", entry.path, MaxSingleFileSize)
		}

		totalSize += entry.size
		if totalSize > MaxUncompressedSize {
			return nil, fmt.Errorf("archive exceeds maximum uncompressed size of %d bytes", MaxUncompressedSize)
		}

		normalizedPath, err := normalizeArchivePath(entry.path)
		if err != nil {
			return nil, err
		}
		entry.path = normalizedPath
		entries = append(entries, entry)
	}

	stripPrefix := commonTopLevelDir(entries)
	if stripPrefix != "" {
		prefix := stripPrefix + "/"
		for i := range entries {
			entries[i].path = strings.TrimPrefix(entries[i].path, prefix)
		}
	}

	var mainEntry *archiveEntry
	for i := range entries {
		if entries[i].path == mainFile {
			mainEntry = &entries[i]
			break
		}
	}
	if mainEntry == nil {
		return nil, fmt.Errorf("archive must include %s", mainFile)
	}

	mainContent, err := mainEntry.read(MaxSingleFileSize)
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
		parsed, err = a.Parser.ParseSKILLMD(mainContent, mainFile)
	case "mcp":
		var items []*ParsedItem
		items, err = a.Parser.ParseMCPJSON(mainContent, mainFile)
		if err == nil {
			if len(items) != 1 {
				return nil, fmt.Errorf(".mcp.json must contain exactly 1 server entry")
			}
			parsed = items[0]
			normalizedMeta, err = NormalizeMCPMetadata(parsed.Metadata)
		}
	}
	if err != nil {
		return nil, err
	}
	if parsed == nil {
		return nil, fmt.Errorf("failed to parse %s", mainFile)
	}
	parsed.ContentHash = mainSHA

	assets := make([]ArchiveAsset, 0)
	assetPaths := make([]string, 0)
	for _, entry := range entries {
		if entry.path == mainFile {
			continue
		}
		if shouldSkipAsset(entry.path) {
			continue
		}

		content, err := entry.read(MaxSingleFileSize)
		if err != nil {
			return nil, fmt.Errorf("read %s: %w", entry.path, err)
		}

		assets = append(assets, ArchiveAsset{
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

	return &ArchiveParseResult{
		MainContent:    string(mainContent),
		MainPath:       mainFile,
		MainSHA:        mainSHA,
		Parsed:         parsed,
		NormalizedMeta: normalizedMeta,
		Assets:         assets,
	}, nil
}

func detectArchiveFormat(filename string) (string, error) {
	lower := strings.ToLower(filename)
	switch {
	case strings.HasSuffix(lower, ".tar.gz"), strings.HasSuffix(lower, ".tgz"):
		return "targz", nil
	case strings.HasSuffix(lower, ".zip"):
		return "zip", nil
	default:
		return "", fmt.Errorf("unsupported archive format %q: supported formats are .zip, .tar.gz, .tgz", filename)
	}
}

func ArchiveMimeType(filename string) string {
	lower := strings.ToLower(filename)
	switch {
	case strings.HasSuffix(lower, ".tar.gz"), strings.HasSuffix(lower, ".tgz"):
		return "application/gzip"
	case strings.HasSuffix(lower, ".zip"):
		return "application/zip"
	default:
		return "application/octet-stream"
	}
}

func enumerateZipEntries(r io.ReaderAt, size int64) ([]archiveEntry, error) {
	reader, err := zip.NewReader(r, size)
	if err != nil {
		return nil, fmt.Errorf("open zip archive: %w", err)
	}

	entries := make([]archiveEntry, 0, len(reader.File))
	for _, file := range reader.File {
		if file.FileInfo().IsDir() {
			continue
		}

		current := file
		entries = append(entries, archiveEntry{
			path: current.Name,
			size: current.UncompressedSize64,
			read: func(limit uint64) ([]byte, error) {
				if current.UncompressedSize64 > limit {
					return nil, fmt.Errorf("zip entry %q exceeds maximum file size of %d bytes", current.Name, limit)
				}

				rc, err := current.Open()
				if err != nil {
					return nil, err
				}
				defer rc.Close()

				content, err := io.ReadAll(io.LimitReader(rc, int64(limit)+1))
				if err != nil {
					return nil, err
				}
				if len(content) > int(limit) {
					return nil, fmt.Errorf("zip entry %q exceeds maximum file size of %d bytes", current.Name, limit)
				}
				return content, nil
			},
		})
	}

	return entries, nil
}

func enumerateTarGzEntries(r io.ReaderAt, size int64) ([]archiveEntry, error) {
	sectionReader := io.NewSectionReader(r, 0, size)
	gzipReader, err := gzip.NewReader(sectionReader)
	if err != nil {
		return nil, fmt.Errorf("open tar.gz archive: %w", err)
	}
	defer gzipReader.Close()

	limitedGzipReader := io.LimitReader(gzipReader, MaxUncompressedSize+1)
	tarReader := tar.NewReader(limitedGzipReader)

	entries := make([]archiveEntry, 0)
	var totalRead uint64
	for {
		header, err := tarReader.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			if totalRead >= MaxUncompressedSize {
				return nil, fmt.Errorf("archive exceeds maximum uncompressed size of %d bytes", MaxUncompressedSize)
			}
			return nil, fmt.Errorf("read tar.gz archive: %w", err)
		}
		if header.Typeflag != tar.TypeReg {
			continue
		}
		if header.Size > MaxSingleFileSize {
			return nil, fmt.Errorf("archive entry %q exceeds maximum file size of %d bytes", header.Name, MaxSingleFileSize)
		}

		content, err := io.ReadAll(io.LimitReader(tarReader, MaxSingleFileSize+1))
		if err != nil {
			return nil, fmt.Errorf("read tar.gz entry %q: %w", header.Name, err)
		}
		if len(content) > MaxSingleFileSize {
			return nil, fmt.Errorf("archive entry %q exceeds maximum file size of %d bytes", header.Name, MaxSingleFileSize)
		}

		totalRead += uint64(len(content))
		if totalRead > MaxUncompressedSize {
			return nil, fmt.Errorf("archive exceeds maximum uncompressed size of %d bytes", MaxUncompressedSize)
		}

		name := header.Name
		bufferedContent := append([]byte(nil), content...)
		entries = append(entries, archiveEntry{
			path: name,
			size: uint64(len(bufferedContent)),
			read: func(limit uint64) ([]byte, error) {
				if uint64(len(bufferedContent)) > limit {
					return nil, fmt.Errorf("archive entry %q exceeds maximum file size of %d bytes", name, limit)
				}
				return append([]byte(nil), bufferedContent...), nil
			},
		})
	}

	return entries, nil
}

// NormalizeMCPMetadata converts any supported MCP input format into the
// standard MCP configuration format (Claude / MCP protocol).
//
// Standard format:
//   stdio: {"command": "npx", "args": ["-y", "@foo/bar"], "env": {...}}
//   http:  {"type": "http", "url": "https://...", "headers": {...}}
//   sse:   {"type": "sse",  "url": "https://...", "headers": {...}}
//
// Accepted proprietary inputs:
//   - {"type":"local",  "command":["npx","-y","@foo"]}  → stdio
//   - {"type":"remote", "url":"https://..."}             → http (type becomes "http")
//   - {"mcpServers":{"name":{...}}}                        → unwrap then normalize
func NormalizeMCPMetadata(meta map[string]any) (map[string]any, error) {
	if len(meta) == 0 {
		return nil, fmt.Errorf("metadata is required")
	}

	var serverConfig map[string]any
	if serversRaw, ok := meta["mcpServers"]; ok {
		servers, ok := serversRaw.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("mcpServers must be an object")
		}
		if len(servers) != 1 {
			return nil, fmt.Errorf(".mcp.json must contain exactly 1 server entry")
		}
		for _, value := range servers {
			serverMap, ok := value.(map[string]any)
			if !ok {
				return nil, fmt.Errorf("mcpServers entry must be an object")
			}
			serverConfig = cloneMap(serverMap)
			break
		}
	} else {
		serverConfig = cloneMap(meta)
	}

	result := serverConfig
	t, _ := result["type"].(string)

	switch t {
	case "local", "stdio":
		// Proprietary format: command is []any. Split to command + args.
		delete(result, "type")
		if cmdArr, ok := result["command"].([]any); ok && len(cmdArr) > 0 {
			result["command"] = cmdArr[0]
			if len(cmdArr) > 1 {
				args := make([]string, 0, len(cmdArr)-1)
				for _, a := range cmdArr[1:] {
					if s, ok := a.(string); ok {
						args = append(args, s)
					}
				}
				result["args"] = args
			}
		}
		return result, nil

	case "remote":
		// Proprietary: "remote" → standard "http".
		result["type"] = "http"
		return result, nil

	case "http", "sse":
		// Already standard remote format.
		return result, nil
	}

	// No type field — detect from content.
	if cmd, ok := result["command"].(string); ok && strings.TrimSpace(cmd) != "" {
		// Standard stdio format.
		return result, nil
	}
	if url, ok := result["url"].(string); ok && strings.TrimSpace(url) != "" {
		// Respect transport hint if present.
		if transport, ok := result["transport"].(string); ok && strings.EqualFold(transport, "sse") {
			result["type"] = "sse"
		} else {
			result["type"] = "http"
		}
		delete(result, "transport")
		return result, nil
	}

	return nil, fmt.Errorf("unable to determine MCP server type: need command or url")
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

func normalizeArchivePath(name string) (string, error) {
	slashed := strings.ReplaceAll(name, "\\", "/")
	if path.IsAbs(slashed) {
		return "", fmt.Errorf("archive entry %q has an absolute path", name)
	}

	parts := strings.Split(slashed, "/")
	cleanParts := make([]string, 0, len(parts))
	for _, part := range parts {
		switch part {
		case "", ".":
			continue
		case "..":
			return "", fmt.Errorf("archive entry %q contains path traversal", name)
		default:
			cleanParts = append(cleanParts, part)
		}
	}
	if len(cleanParts) == 0 {
		return "", fmt.Errorf("archive entry %q has an empty path", name)
	}

	return strings.Join(cleanParts, "/"), nil
}

func commonTopLevelDir(entries []archiveEntry) string {
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
