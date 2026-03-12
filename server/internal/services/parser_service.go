package services

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

type ParserService struct{}

type ParsedItem struct {
	Slug        string
	ItemType    string
	Name        string
	Description string
	Category    string
	Version     string
	Content     string
	Metadata    map[string]any
	SourcePath  string
	ContentHash string
}

func (p *ParserService) ParseSKILLMD(content []byte, sourcePath string) (*ParsedItem, error) {
	raw := string(content)

	var frontmatter map[string]any
	var body string

	if strings.HasPrefix(raw, "---") {
		parts := strings.SplitN(raw, "---", 3)
		if len(parts) >= 3 {
			if err := yaml.Unmarshal([]byte(parts[1]), &frontmatter); err != nil {
				return nil, fmt.Errorf("failed to parse frontmatter: %w", err)
			}
			body = strings.TrimSpace(parts[2])
		} else {
			body = raw
		}
	} else {
		body = raw
	}

	if frontmatter == nil {
		frontmatter = make(map[string]any)
	}

	item := &ParsedItem{
		Content:    raw,
		SourcePath: sourcePath,
		Metadata:   frontmatter,
	}

	if v, ok := frontmatter["name"].(string); ok {
		item.Name = v
	}
	if item.Name == "" {
		item.Name = inferNameFromPath(sourcePath)
	}

	if v, ok := frontmatter["description"].(string); ok {
		item.Description = v
	}
	if item.Description == "" && body != "" {
		lines := strings.Split(body, "\n")
		for _, line := range lines {
			line = strings.TrimSpace(line)
			if line != "" && !strings.HasPrefix(line, "#") {
				item.Description = line
				break
			}
		}
	}

	if v, ok := frontmatter["type"].(string); ok {
		item.ItemType = v
	}
	if item.ItemType == "" {
		item.ItemType = p.InferItemType(sourcePath)
	}

	if v, ok := frontmatter["category"].(string); ok {
		item.Category = v
	}

	if v, ok := frontmatter["version"].(string); ok {
		item.Version = v
	}
	if item.Version == "" {
		item.Version = "1.0.0"
	}

	item.Slug = p.InferSlug(sourcePath)

	return item, nil
}

func (p *ParserService) ParsePluginJSON(content []byte, sourcePath string) (*ParsedItem, error) {
	var data map[string]any
	if err := json.Unmarshal(content, &data); err != nil {
		return nil, fmt.Errorf("failed to parse plugin.json: %w", err)
	}

	item := &ParsedItem{
		Content:    string(content),
		ItemType:   "skill",
		Version:    "1.0.0",
		Metadata:   data,
		SourcePath: sourcePath,
	}

	if v, ok := data["name"].(string); ok {
		item.Name = v
	}
	if v, ok := data["description"].(string); ok {
		item.Description = v
	}
	if v, ok := data["version"].(string); ok {
		item.Version = v
	}
	if v, ok := data["type"].(string); ok {
		item.ItemType = v
	}
	if v, ok := data["category"].(string); ok {
		item.Category = v
	}

	item.Slug = p.InferSlug(sourcePath)
	if item.Name == "" {
		item.Name = inferNameFromPath(sourcePath)
	}

	return item, nil
}

func (p *ParserService) ParseAgentsMD(content []byte, sourcePath string) ([]*ParsedItem, error) {
	item, err := p.ParseSKILLMD(content, sourcePath)
	if err != nil {
		return nil, err
	}
	item.ItemType = "agent"
	return []*ParsedItem{item}, nil
}

func (p *ParserService) InferItemType(filePath string) string {
	lower := strings.ToLower(filepath.ToSlash(filePath))
	switch {
	case strings.Contains(lower, "agents/") || strings.HasSuffix(lower, "agents.md"):
		return "agent"
	case strings.Contains(lower, "commands/"):
		return "command"
	case strings.Contains(lower, "hooks/"):
		return "hook"
	case strings.Contains(lower, "mcp/"):
		return "mcp"
	default:
		return "skill"
	}
}

func (p *ParserService) InferSlug(filePath string) string {
	filePath = filepath.ToSlash(filePath)
	ext := filepath.Ext(filePath)
	withoutExt := strings.TrimSuffix(filePath, ext)

	parts := strings.Split(withoutExt, "/")
	var meaningful []string
	for _, part := range parts {
		lower := strings.ToLower(part)
		if lower == "skills" || lower == "agents" || lower == "commands" || lower == "hooks" || lower == "mcp" {
			continue
		}
		if lower == "skill" || lower == "readme" || lower == "index" {
			continue
		}
		meaningful = append(meaningful, lower)
	}

	slug := strings.Join(meaningful, "-")
	slug = strings.ReplaceAll(slug, "_", "-")
	slug = strings.ReplaceAll(slug, " ", "-")

	if slug == "" {
		slug = "unnamed"
	}
	return slug
}

func inferNameFromPath(filePath string) string {
	base := filepath.Base(filePath)
	ext := filepath.Ext(base)
	name := strings.TrimSuffix(base, ext)
	name = strings.ReplaceAll(name, "-", " ")
	name = strings.ReplaceAll(name, "_", " ")
	if len(name) > 0 {
		name = strings.ToUpper(name[:1]) + name[1:]
	}
	return name
}
