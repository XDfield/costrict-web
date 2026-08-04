package services

import (
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
)

// ErrGitCapabilityManifestEntryMissing means a multi-entry manifest no
// longer contains an already-indexed entry. The caller archives that row
// without treating a normal Git deletion or MCP key rename as a repository
// read/parse failure.
var ErrGitCapabilityManifestEntryMissing = errors.New("Git manifest entry is missing")

// ParseGitIndexFile parses one already-bound Git-backed capability manifest.
// itemType and slug come from the existing index row: type identity is locked
// after the row is created and is never inferred again during webhook sync.
func (p *ParserService) ParseGitIndexFile(content []byte, sourcePath, itemType, slug, entryKey string) (*ParsedItem, error) {
	var (
		items []*ParsedItem
		err   error
	)

	base := strings.ToLower(filepath.Base(filepath.ToSlash(sourcePath)))
	switch itemType {
	case "plugin":
		if base == ".plugin.json" {
			items, err = p.parseGitPluginJSON(content, sourcePath)
		} else {
			var item *ParsedItem
			item, err = p.ParsePluginManifestJSON(content, sourcePath)
			if item != nil {
				items = []*ParsedItem{item}
			}
		}
	case "mcp":
		items, err = p.ParseMCPJSON(content, sourcePath)
	case "subagent":
		items, err = p.ParseAgentsMD(content, sourcePath)
	case "skill", "command", "rule", "template":
		var item *ParsedItem
		item, err = p.ParseSKILLMD(content, sourcePath)
		if item != nil {
			items = []*ParsedItem{item}
		}
	default:
		return nil, fmt.Errorf("unsupported Git-backed capability type %q", itemType)
	}
	if err != nil {
		return nil, err
	}
	if len(items) == 0 {
		return nil, fmt.Errorf("manifest %q produced no capability", sourcePath)
	}

	selected := items[0]
	if itemType == "mcp" {
		selected = selectMCPGitIndexItem(items, slug, entryKey)
		if selected == nil {
			identity := entryKey
			if identity == "" {
				identity = slug
			}
			return nil, fmt.Errorf("%w: manifest %q has no entry for %q", ErrGitCapabilityManifestEntryMissing, sourcePath, identity)
		}
	} else if len(items) > 1 {
		selected = nil
		for _, item := range items {
			if item != nil && item.Slug == slug {
				selected = item
				break
			}
		}
		if selected == nil {
			return nil, fmt.Errorf("%w: manifest %q no longer contains capability slug %q", ErrGitCapabilityManifestEntryMissing, sourcePath, slug)
		}
	}
	if strings.TrimSpace(selected.Name) == "" {
		return nil, fmt.Errorf("manifest %q has no capability name", sourcePath)
	}

	selected.ItemType = itemType
	selected.Slug = slug
	return selected, nil
}

func (p *ParserService) parseGitPluginJSON(content []byte, sourcePath string) ([]*ParsedItem, error) {
	var document struct {
		Install struct {
			Method string `json:"method"`
		} `json:"install"`
	}
	if err := json.Unmarshal(content, &document); err != nil {
		return nil, fmt.Errorf("failed to parse .plugin.json: %w", err)
	}
	if strings.TrimSpace(document.Install.Method) == "" {
		return nil, errors.New(".plugin.json missing required install.method")
	}
	if strings.EqualFold(document.Install.Method, "plugin_marketplace") {
		return p.ParsePluginJSON(content, sourcePath)
	}
	item, err := p.ParsePluginManifestJSON(content, sourcePath)
	if err != nil {
		return nil, err
	}
	return []*ParsedItem{item}, nil
}

func selectMCPGitIndexItem(items []*ParsedItem, slug, entryKey string) *ParsedItem {
	for _, item := range items {
		if item == nil {
			continue
		}
		if entryKey != "" {
			key, _ := item.Metadata["key"].(string)
			if key == entryKey {
				return item
			}
			continue
		}
		if item.Slug == slug {
			return item
		}
	}
	return nil
}
