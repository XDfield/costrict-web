package services

import (
	"crypto/md5"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

type ContentHashService struct{}

type ArchiveManifestEntry struct {
	Path        string
	ContentHash string
}

func NewContentHashService() *ContentHashService {
	return &ContentHashService{}
}

func (s *ContentHashService) HashTextContent(itemType string, content string) (string, error) {
	normalized, err := s.normalizeTextContent(itemType, content)
	if err != nil {
		return "", err
	}
	return md5Hex(normalized), nil
}

func (s *ContentHashService) HashArchiveContent(mainPath string, mainContent []byte, assets []ArchiveAsset) (string, error) {
	mainHash, err := s.hashArchiveFile(mainPath, mainContent)
	if err != nil {
		return "", err
	}
	entries := make([]ArchiveManifestEntry, 0, len(assets)+1)
	entries = append(entries, ArchiveManifestEntry{Path: mainPath, ContentHash: mainHash})

	for _, asset := range assets {
		if shouldSkipAsset(asset.Path) {
			continue
		}
		assetHash, err := s.hashArchiveFile(asset.Path, asset.Content)
		if err != nil {
			return "", err
		}
		entries = append(entries, ArchiveManifestEntry{Path: asset.Path, ContentHash: assetHash})
	}

	return s.HashArchiveManifest(entries), nil
}

func (s *ContentHashService) HashArchiveManifest(entries []ArchiveManifestEntry) string {
	manifest := make([]string, 0, len(entries))
	for _, entry := range entries {
		if shouldSkipAsset(entry.Path) || strings.TrimSpace(entry.ContentHash) == "" {
			continue
		}
		manifest = append(manifest, normalizeManifestPath(entry.Path)+":"+entry.ContentHash)
	}

	sort.Strings(manifest)
	return md5Hex([]byte(strings.Join(manifest, "\n")))
}

func (s *ContentHashService) BuildVersionLabel(revision int) string {
	if revision < 1 {
		revision = 1
	}
	return "v" + strconv.Itoa(revision)
}

func (s *ContentHashService) hashArchiveFile(relPath string, content []byte) (string, error) {
	if isJSONPath(relPath) {
		var value any
		if err := json.Unmarshal(content, &value); err == nil {
			canonical, err := json.Marshal(value)
			if err != nil {
				return "", err
			}
			return md5Hex(canonical), nil
		}
	}

	if isBinary(content) {
		return md5Hex(content), nil
	}

	return md5Hex([]byte(normalizeNewlines(string(content)))), nil
}

func (s *ContentHashService) normalizeTextContent(itemType string, content string) ([]byte, error) {
	normalized := normalizeNewlines(content)
	if itemType != "mcp" {
		return []byte(normalized), nil
	}

	trimmed := strings.TrimSpace(normalized)
	if trimmed == "" {
		return []byte(normalized), nil
	}

	var value any
	if err := json.Unmarshal([]byte(trimmed), &value); err != nil {
		return nil, fmt.Errorf("canonicalize mcp content: %w", err)
	}
	canonical, err := json.Marshal(value)
	if err != nil {
		return nil, fmt.Errorf("marshal mcp content: %w", err)
	}
	return canonical, nil
}

func normalizeNewlines(content string) string {
	content = strings.ReplaceAll(content, "\r\n", "\n")
	content = strings.ReplaceAll(content, "\r", "\n")
	return content
}

func isJSONPath(relPath string) bool {
	base := strings.ToLower(filepath.Base(relPath))
	return strings.HasSuffix(base, ".json")
}

func normalizeManifestPath(relPath string) string {
	return strings.ReplaceAll(relPath, "\\", "/")
}

func md5Hex(data []byte) string {
	sum := md5.Sum(data)
	return hex.EncodeToString(sum[:])
}
