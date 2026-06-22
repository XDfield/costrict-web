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
	Slug            string
	ItemType        string
	Name            string
	Description     string
	Category        string
	Tags            []string
	Version         string
	Content         string
	Metadata        map[string]any
	SourcePath      string
	ContentHash     string
	AssetPaths      []string
	Source          string
	ExperienceScore float64
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
	if v, ok := frontmatter["tags"].([]any); ok {
		for _, t := range v {
			if s, ok := t.(string); ok {
				item.Tags = append(item.Tags, s)
			}
		}
	}

	if v, ok := frontmatter["version"].(string); ok {
		item.Version = v
	}
	if item.Version == "" {
		item.Version = "1.0.0"
	}

	if v, ok := frontmatter["source"].(string); ok {
		item.Source = v
	}
	if v, ok := frontmatter["experienceScore"].(float64); ok {
		item.ExperienceScore = v
	} else if v, ok := frontmatter["experience_score"].(float64); ok {
		item.ExperienceScore = v
	} else if v, ok := frontmatter["experienceScore"].(int); ok {
		item.ExperienceScore = float64(v)
	} else if v, ok := frontmatter["experience_score"].(int); ok {
		item.ExperienceScore = float64(v)
	}

	item.Slug = p.InferSlug(sourcePath)

	return item, nil
}

// ParsePluginManifestJSON parses a `.claude-plugin/plugin.json` marketplace
// manifest. The manifest enriches a CapabilityRegistry (name/description/etc.)
// and is NOT the same as the per-plugin `.plugin.json` consumed by
// ParsePluginJSON below.
func (p *ParserService) ParsePluginManifestJSON(content []byte, sourcePath string) (*ParsedItem, error) {
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
	if v, ok := data["tags"].([]any); ok {
		for _, t := range v {
			if s, ok := t.(string); ok {
				item.Tags = append(item.Tags, s)
			}
		}
	}
	if v, ok := data["source"].(string); ok {
		item.Source = v
	}
	if v, ok := data["experienceScore"].(float64); ok {
		item.ExperienceScore = v
	} else if v, ok := data["experience_score"].(float64); ok {
		item.ExperienceScore = v
	} else if v, ok := data["experienceScore"].(int); ok {
		item.ExperienceScore = float64(v)
	} else if v, ok := data["experience_score"].(int); ok {
		item.ExperienceScore = float64(v)
	}

	item.Slug = p.InferSlug(sourcePath)
	if item.Name == "" {
		item.Name = inferNameFromPath(sourcePath)
	}

	return item, nil
}

// ParsePluginJSON parses a per-plugin `.plugin.json` catalog file emitted by
// costrict-skills-repo's download_catalog.py. The schema mirrors SKILL.md's
// frontmatter shape so that ParsedItem fields populate during the sync phase,
// without depending on the catalog/index.json backfill:
//
//	{
//	  "name": "<display name>",
//	  "description": "...",
//	  "category": "...",
//	  "tags": ["..."],
//	  "install": {
//	    "method": "plugin_marketplace",
//	    "plugin_name": "...",
//	    "marketplace_name": "...",
//	    "marketplace_repo": "<owner>/<repo>",
//	    "marketplace_verified": true,
//	    ...
//	  },
//	  "bundle": { ... }   // optional
//	}
//
// Content is synthesised as a markdown summary (YAML frontmatter + body with
// install + bundle sections) so the detail page can render the same
// "Metadata block + body" shape as a skill — plugin's executable still lives
// in the marketplace repo and isn't stored here.
//
// Slug is derived from the per-plugin catalog directory name (encodes
// marketplace owner / repo + plugin name; plain plugin_name alone collides
// across marketplaces).
func (p *ParserService) ParsePluginJSON(content []byte, sourcePath string) ([]*ParsedItem, error) {
	var data map[string]any
	if err := json.Unmarshal(content, &data); err != nil {
		return nil, fmt.Errorf("failed to parse .plugin.json: %w", err)
	}

	install, ok := data["install"].(map[string]any)
	if !ok {
		return nil, fmt.Errorf(".plugin.json missing required 'install' object")
	}

	pluginName, _ := install["plugin_name"].(string)
	marketplaceName, _ := install["marketplace_name"].(string)
	marketplaceRepo, _ := install["marketplace_repo"].(string)
	if pluginName == "" || marketplaceName == "" || marketplaceRepo == "" {
		return nil, fmt.Errorf(".plugin.json missing required install fields (plugin_name, marketplace_name, marketplace_repo)")
	}

	dir := filepath.Base(filepath.Dir(filepath.ToSlash(sourcePath)))
	slug := strings.ToLower(dir)
	if slug == "" || slug == "." {
		slug = strings.ToLower(pluginName)
	}

	// Display name: prefer the top-level `name` (carried over from catalog
	// `entry.name`), fall back to `install.plugin_name`.
	displayName := pluginName
	if v, ok := data["name"].(string); ok && v != "" {
		displayName = v
	}

	item := &ParsedItem{
		ItemType:   "plugin",
		Name:       displayName,
		Slug:       slug,
		Version:    "1.0.0",
		Metadata:   data,
		SourcePath: sourcePath,
	}

	if v, ok := data["description"].(string); ok {
		item.Description = v
	}
	if v, ok := data["category"].(string); ok {
		item.Category = v
	}
	if rawTags, ok := data["tags"].([]any); ok {
		for _, t := range rawTags {
			if s, ok := t.(string); ok && s != "" {
				item.Tags = append(item.Tags, s)
			}
		}
	}

	bundle, _ := data["bundle"].(map[string]any)

	// Description fallback when upstream has none — keeps cards from showing
	// blank for the ~1% of entries with empty catalog descriptions.
	if item.Description == "" {
		item.Description = fmt.Sprintf("Marketplace plugin from %s", marketplaceRepo)
	}

	// Tags fallback: derive a short, deterministic set from bundle composition
	// + category when upstream tags are empty. Two-thirds of catalog plugin
	// entries ship with empty tags; without this fallback the Tags column
	// renders as em-dashes for most plugin rows.
	if len(item.Tags) == 0 {
		item.Tags = synthesizePluginTags(bundle, item.Category)
	}

	item.Content = synthesizePluginContent(pluginContentInput{
		DisplayName:     displayName,
		PluginName:      pluginName,
		MarketplaceName: marketplaceName,
		MarketplaceRepo: marketplaceRepo,
		Category:        item.Category,
		Description:     item.Description,
		Bundle:          bundle,
	})

	return []*ParsedItem{item}, nil
}

// pluginBundleCount safely extracts an integer count from a bundle map's
// JSON-decoded value (which arrives as float64 from encoding/json).
func pluginBundleCount(bundle map[string]any, key string) int {
	if bundle == nil {
		return 0
	}
	if v, ok := bundle[key].(float64); ok {
		return int(v)
	}
	if v, ok := bundle[key].(int); ok {
		return v
	}
	return 0
}

// synthesizePluginTags derives a short tag list from a plugin's bundle
// composition + category. Order is stable so the same input always yields
// the same output (matters for content_md5 stability across re-imports).
func synthesizePluginTags(bundle map[string]any, category string) []string {
	tags := make([]string, 0, 6)
	if pluginBundleCount(bundle, "skills_count") > 0 {
		tags = append(tags, "skills")
	}
	if pluginBundleCount(bundle, "commands_count") > 0 {
		tags = append(tags, "commands")
	}
	if pluginBundleCount(bundle, "agents_count") > 0 {
		tags = append(tags, "agents")
	}
	if pluginBundleCount(bundle, "mcp_servers_count") > 0 {
		tags = append(tags, "mcp")
	}
	if pluginBundleCount(bundle, "hooks_count") > 0 {
		tags = append(tags, "hooks")
	}
	if category != "" {
		tags = append(tags, category)
	}
	return tags
}

type pluginContentInput struct {
	DisplayName     string
	PluginName      string
	MarketplaceName string
	MarketplaceRepo string
	Category        string
	Description     string
	Bundle          map[string]any
}

// synthesizePluginContent renders a YAML-frontmatter + markdown body summary
// for a plugin so the detail page has the same shape as a skill's content:
// a "Metadata" block (rendered from frontmatter) followed by body sections.
// The body covers description, bundle composition, csc install commands and
// a marketplace repo link.
func synthesizePluginContent(in pluginContentInput) string {
	var sb strings.Builder

	// YAML frontmatter — quoted strings to avoid breaking on special chars.
	sb.WriteString("---\n")
	sb.WriteString(fmt.Sprintf("name: %s\n", yamlQuote(in.DisplayName)))
	sb.WriteString(fmt.Sprintf("plugin_name: %s\n", yamlQuote(in.PluginName)))
	sb.WriteString(fmt.Sprintf("marketplace: %s\n", yamlQuote(in.MarketplaceName)))
	sb.WriteString(fmt.Sprintf("marketplace_repo: %s\n", yamlQuote(in.MarketplaceRepo)))
	if in.Category != "" {
		sb.WriteString(fmt.Sprintf("category: %s\n", yamlQuote(in.Category)))
	}
	if in.Description != "" {
		sb.WriteString(fmt.Sprintf("description: %s\n", yamlQuote(in.Description)))
	}
	sb.WriteString("---\n\n")

	// Body.
	sb.WriteString(fmt.Sprintf("# %s\n\n", in.DisplayName))
	if in.Description != "" {
		sb.WriteString(in.Description)
		sb.WriteString("\n\n")
	}

	// Bundle composition.
	bundleLines := make([]string, 0, 5)
	if c := pluginBundleCount(in.Bundle, "skills_count"); c > 0 {
		bundleLines = append(bundleLines, fmt.Sprintf("- **Skills:** %d", c))
	}
	if c := pluginBundleCount(in.Bundle, "commands_count"); c > 0 {
		bundleLines = append(bundleLines, fmt.Sprintf("- **Commands:** %d", c))
	}
	if c := pluginBundleCount(in.Bundle, "agents_count"); c > 0 {
		bundleLines = append(bundleLines, fmt.Sprintf("- **Agents:** %d", c))
	}
	if c := pluginBundleCount(in.Bundle, "mcp_servers_count"); c > 0 {
		bundleLines = append(bundleLines, fmt.Sprintf("- **MCP Servers:** %d", c))
	}
	if c := pluginBundleCount(in.Bundle, "hooks_count"); c > 0 {
		bundleLines = append(bundleLines, fmt.Sprintf("- **Hooks:** %d", c))
	}
	if len(bundleLines) > 0 {
		sb.WriteString("## Bundle Contents\n\nThis plugin includes:\n\n")
		for _, line := range bundleLines {
			sb.WriteString(line)
			sb.WriteString("\n")
		}
		sb.WriteString("\n")
	}

	// Installation — every plugin is distributed through the unified
	// costrict-plugins marketplace, which the csc client ships preconfigured;
	// no per-user `marketplace add` step is needed, so the copy is a single
	// `csc plugin install <plugin_name>@costrict-plugins` line.
	sb.WriteString("## Installation\n\n")
	sb.WriteString("Install via the csc client:\n\n")
	sb.WriteString("```bash\n")
	sb.WriteString(fmt.Sprintf("csc plugin install %s@costrict-plugins\n", in.PluginName))
	sb.WriteString("```\n\n")

	// Upstream source link — still useful for users who want to inspect the
	// original repo, file issues with the plugin author, etc.
	sb.WriteString("## Upstream Source\n\n")
	sb.WriteString(fmt.Sprintf("[github.com/%s](https://github.com/%s)\n", in.MarketplaceRepo, in.MarketplaceRepo))

	return sb.String()
}

// yamlQuote returns a double-quoted YAML scalar for a string, escaping the
// few characters that would otherwise break the encoding. Keeps the output
// terse for typical plugin descriptions while staying valid YAML.
func yamlQuote(s string) string {
	escaped := strings.ReplaceAll(s, `\`, `\\`)
	escaped = strings.ReplaceAll(escaped, `"`, `\"`)
	escaped = strings.ReplaceAll(escaped, "\n", `\n`)
	return `"` + escaped + `"`
}

func (p *ParserService) ParseAgentsMD(content []byte, sourcePath string) ([]*ParsedItem, error) {
	item, err := p.ParseSKILLMD(content, sourcePath)
	if err != nil {
		return nil, err
	}
	item.ItemType = "subagent"
	return []*ParsedItem{item}, nil
}

func (p *ParserService) ParseHooksJSON(content []byte, sourcePath string) (*ParsedItem, error) {
	var data map[string]any
	if err := json.Unmarshal(content, &data); err != nil {
		return nil, fmt.Errorf("failed to parse hooks.json: %w", err)
	}
	return &ParsedItem{
		Slug:       "hooks",
		ItemType:   "hook",
		Name:       "Hooks",
		Content:    string(content),
		Metadata:   data,
		SourcePath: sourcePath,
		Version:    "1.0.0",
	}, nil
}

// ParseMCPJSON parses .mcp.json and returns one ParsedItem per mcpServers entry.
func (p *ParserService) ParseMCPJSON(content []byte, sourcePath string) ([]*ParsedItem, error) {
	var data map[string]any
	if err := json.Unmarshal(content, &data); err != nil {
		return nil, fmt.Errorf("failed to parse .mcp.json: %w", err)
	}

	servers, _ := data["mcpServers"].(map[string]any)
	if len(servers) == 0 {
		return []*ParsedItem{{
			Slug:       "mcp-config",
			ItemType:   "mcp",
			Name:       "MCP Config",
			Content:    string(content),
			Metadata:   data,
			SourcePath: sourcePath,
			Version:    "1.0.0",
		}}, nil
	}

	items := make([]*ParsedItem, 0, len(servers))
	for key, val := range servers {
		serverMeta := map[string]any{"key": key}
		if m, ok := val.(map[string]any); ok {
			for k, v := range m {
				serverMeta[k] = v
			}
		}

		name := key
		if v, ok := serverMeta["name"].(string); ok && v != "" {
			name = v
		}

		description := ""
		if v, ok := serverMeta["description"].(string); ok {
			description = v
		}

		items = append(items, &ParsedItem{
			Slug:        "mcp-" + slugifyKey(key),
			ItemType:    "mcp",
			Name:        name,
			Description: description,
			Content:     string(content),
			Metadata:    serverMeta,
			SourcePath:  sourcePath,
			Version:     "1.0.0",
		})
	}
	return items, nil
}

func (p *ParserService) InferItemType(filePath string) string {
	lower := strings.ToLower(filepath.ToSlash(filePath))
	base := filepath.Base(lower)
	switch {
	case base == ".mcp.json":
		return "mcp"
	case base == ".plugin.json":
		return "plugin"
	case strings.Contains(lower, "agents/") || strings.HasSuffix(lower, "agents.md"):
		return "subagent"
	case strings.Contains(lower, "commands/"):
		return "command"
	case strings.Contains(lower, "hooks/"):
		return "hook"
	case strings.Contains(lower, "rules/"):
		// rules/<group>/<file>.md (cospower convention, nestable). The plugin
		// "work tree" now renders a Rule node type, so we no longer collapse
		// these into skill. Keep this BEFORE the default branch.
		return "rule"
	case strings.Contains(lower, "templates/"):
		// templates/<file>.md (cospower convention). Surfaced as a Template
		// node in the plugin work tree.
		return "template"
	default:
		// PROMPT.md and other non-special .md files land here. They collapse
		// to "skill" so they show up under the Skills tab. The item_type
		// contract shared across catalog / web / frontend is:
		//   skill / subagent / command / mcp / rule / template / plugin / hook
		// (rules/ and templates/ are handled above; keep this list in sync
		// with catalog/schema.json and the frontend TYPE_META).
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
		// Strip the top-level capability directory names. For nested layouts
		// (e.g. rules/<group>/<file>.md, templates/<file>.md, evaluators/<name>/SKILL.md)
		// the intermediate group/name segments are KEPT so the slug stays
		// unique across siblings that share a leaf filename.
		if lower == "skills" || lower == "agents" || lower == "commands" || lower == "hooks" || lower == "mcp" ||
			lower == "rules" || lower == "templates" || lower == "evaluators" {
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
	if strings.ToUpper(base) == "SKILL.MD" {
		dir := filepath.Dir(filePath)
		base = filepath.Base(dir)
	} else {
		ext := filepath.Ext(base)
		base = strings.TrimSuffix(base, ext)
	}
	name := strings.ReplaceAll(base, "-", " ")
	name = strings.ReplaceAll(name, "_", " ")
	if len(name) > 0 {
		name = strings.ToUpper(name[:1]) + name[1:]
	}
	return name
}

func slugifyKey(key string) string {
	result := strings.ToLower(key)
	result = strings.ReplaceAll(result, "_", "-")
	result = strings.ReplaceAll(result, " ", "-")
	return result
}
