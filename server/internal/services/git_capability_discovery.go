package services

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"sort"
	"strings"
	"time"
	"unicode"

	"github.com/costrict/costrict-web/server/internal/gitserver"
	"github.com/costrict/costrict-web/server/internal/gitsync"
	"github.com/costrict/costrict-web/server/internal/models"
	"github.com/google/uuid"
	"github.com/pelletier/go-toml/v2"
	"gopkg.in/yaml.v3"
	"gorm.io/datatypes"
	"gorm.io/gorm"
)

const (
	maxGitCapabilityDiscoveryCandidates = 1000
	gitCapabilityDiscoverySystemOwner   = "system"

	// GitRegistrySourceType marks a CapabilityRegistry owned by
	// GitCapabilitySyncService: its capabilities are reconciled from Gitea push
	// webhooks against the repository HEAD. The legacy clone pipeline
	// (SyncService + scheduler.Scheduler) must never adopt one, so that a
	// repository never has two independent writers on its capability_items rows.
	GitRegistrySourceType = "git"
)

type gitCapabilityCandidate struct {
	Path     string
	ItemType string
	Optional bool
}

type discoveredGitCapability struct {
	Path     string
	ItemType string
	EntryKey string
	Parsed   *ParsedItem
}

func gitCapabilityManifestIdentity(manifestPath, entryKey string) string {
	return manifestPath + "\x00" + entryKey
}

func (s *GitCapabilitySyncService) scanGitCapabilityManifestSet(
	ctx context.Context,
	reader GitCapabilityReader,
	owner, repoName, headSHA string,
	boundItems []models.CapabilityItem,
) ([]discoveredGitCapability, int, error) {
	tree, err := reader.ListTree(ctx, owner, repoName, headSHA)
	if err != nil {
		return nil, 0, fmt.Errorf("list repository tree at %s: %w", headSHA, err)
	}
	candidates := discoverGitCapabilityReconciliationCandidates(tree, boundItems)
	if len(candidates) > maxGitCapabilityDiscoveryCandidates {
		return nil, 0, fmt.Errorf("repository exposes %d capability manifests; limit is %d", len(candidates), maxGitCapabilityDiscoveryCandidates)
	}

	lockedTypes := make(map[string]string, len(boundItems))
	for _, item := range boundItems {
		if locked, exists := lockedTypes[item.SourceRepoPath]; exists && locked != item.ItemType {
			return nil, 0, fmt.Errorf("manifest %q is bound to conflicting capability types %q and %q", item.SourceRepoPath, locked, item.ItemType)
		}
		lockedTypes[item.SourceRepoPath] = item.ItemType
	}

	discovered := make([]discoveredGitCapability, 0, len(candidates))
	skipped := 0
	seenIdentities := make(map[string]struct{}, len(candidates))
	for _, candidate := range candidates {
		itemType := candidate.ItemType
		lockedType, isLocked := lockedTypes[candidate.Path]
		if isLocked {
			itemType = lockedType
		}
		raw, err := reader.ReadFile(ctx, owner, repoName, headSHA, candidate.Path)
		if err != nil {
			return nil, skipped, fmt.Errorf("read manifest candidate %s at %s: %w", candidate.Path, headSHA, err)
		}
		if len(raw) == 0 {
			return nil, skipped, fmt.Errorf("manifest candidate %s at %s is empty", candidate.Path, headSHA)
		}
		items, err := s.Parser.ParseGitDiscoveryFile(raw, candidate.Path, itemType)
		if err != nil {
			if candidate.Optional && !isLocked && errors.Is(err, errGitCapabilityDiscoveryNotMatched) {
				skipped++
				continue
			}
			return nil, skipped, fmt.Errorf("parse manifest candidate %s: %w", candidate.Path, err)
		}
		for _, parsed := range items {
			if parsed == nil {
				continue
			}
			if err := applyExplicitGitIndexFields(parsed); err != nil {
				return nil, skipped, fmt.Errorf("apply explicit metadata from %s: %w", candidate.Path, err)
			}
			entryKey := ""
			if itemType == "mcp" {
				entryKey, _ = parsed.Metadata["key"].(string)
			}
			identity := gitCapabilityManifestIdentity(candidate.Path, entryKey)
			if _, exists := seenIdentities[identity]; exists {
				return nil, skipped, fmt.Errorf("manifest candidate %s produced duplicate entry identity %q", candidate.Path, entryKey)
			}
			seenIdentities[identity] = struct{}{}
			parsed.ItemType = itemType
			parsed.Slug = discoveredCapabilitySlug(parsed, candidate, repoName, entryKey)
			discovered = append(discovered, discoveredGitCapability{
				Path: candidate.Path, ItemType: itemType, EntryKey: entryKey, Parsed: parsed,
			})
		}
	}
	sort.Slice(discovered, func(i, j int) bool {
		if discovered[i].Path == discovered[j].Path {
			return discovered[i].EntryKey < discovered[j].EntryKey
		}
		return discovered[i].Path < discovered[j].Path
	})
	return discovered, skipped, nil
}

func discoverGitCapabilityReconciliationCandidates(
	entries []gitsync.GitTreeEntry,
	boundItems []models.CapabilityItem,
) []gitCapabilityCandidate {
	candidates := discoverGitCapabilityCandidates(entries)
	byPath := make(map[string]gitCapabilityCandidate, len(candidates)+len(boundItems))
	for _, candidate := range candidates {
		byPath[candidate.Path] = candidate
	}
	treePaths := make(map[string]struct{}, len(entries))
	for _, entry := range entries {
		if entry.Type != "" && !strings.EqualFold(entry.Type, "blob") {
			continue
		}
		treePaths[entry.Path] = struct{}{}
	}
	for _, item := range boundItems {
		if _, exists := treePaths[item.SourceRepoPath]; !exists {
			continue
		}
		if _, exists := byPath[item.SourceRepoPath]; !exists {
			byPath[item.SourceRepoPath] = gitCapabilityCandidate{Path: item.SourceRepoPath, ItemType: item.ItemType}
		}
	}
	result := make([]gitCapabilityCandidate, 0, len(byPath))
	for _, candidate := range byPath {
		result = append(result, candidate)
	}
	sort.Slice(result, func(i, j int) bool { return result[i].Path < result[j].Path })
	return result
}

func remainingDiscoveredGitCapabilities(
	discovered []discoveredGitCapability,
	remaining map[string]discoveredGitCapability,
) []discoveredGitCapability {
	result := make([]discoveredGitCapability, 0, len(remaining))
	for _, entry := range discovered {
		if _, exists := remaining[gitCapabilityManifestIdentity(entry.Path, entry.EntryKey)]; exists {
			result = append(result, entry)
		}
	}
	return result
}

func discoverGitCapabilityCandidatesFromDiscovered(discovered []discoveredGitCapability) []gitCapabilityCandidate {
	candidates := make([]gitCapabilityCandidate, 0, len(discovered))
	seen := make(map[string]struct{}, len(discovered))
	for _, entry := range discovered {
		if _, exists := seen[entry.Path]; exists {
			continue
		}
		seen[entry.Path] = struct{}{}
		candidates = append(candidates, gitCapabilityCandidate{Path: entry.Path, ItemType: entry.ItemType})
	}
	return candidates
}

var errGitCapabilityDiscoveryNotMatched = errors.New("file did not match the capability heuristic")

// discoverGitCapabilities performs the one-time structure inference for a
// repository with no bound items. The resulting item type/path/entry identity
// is persisted and all later pushes use ParseGitIndexFile, which never infers
// the type again.
func (s *GitCapabilitySyncService) discoverGitCapabilities(
	ctx context.Context,
	cfg *gitserver.Config,
	reader GitCapabilityReader,
	repo *gitsync.Repo,
	owner, repoName, branchName, headSHA string,
	lease GitCapabilitySyncLease,
) (*GitCapabilitySyncResult, error) {
	tree, err := reader.ListTree(ctx, owner, repoName, headSHA)
	if err != nil {
		return nil, fmt.Errorf("list repository tree at %s: %w", headSHA, err)
	}
	candidates := discoverGitCapabilityCandidates(tree)
	if len(candidates) > maxGitCapabilityDiscoveryCandidates {
		return nil, fmt.Errorf("repository exposes %d capability manifests; limit is %d", len(candidates), maxGitCapabilityDiscoveryCandidates)
	}

	discovered := make([]discoveredGitCapability, 0, len(candidates))
	issues := make([]string, 0)
	for _, candidate := range candidates {
		raw, readErr := reader.ReadFile(ctx, owner, repoName, headSHA, candidate.Path)
		if readErr != nil {
			return nil, fmt.Errorf("read discovery candidate %s at %s: %w", candidate.Path, headSHA, readErr)
		}
		if len(raw) == 0 {
			issues = append(issues, fmt.Sprintf("%s is empty", candidate.Path))
			continue
		}
		items, parseErr := s.Parser.ParseGitDiscoveryFile(raw, candidate.Path, candidate.ItemType)
		if parseErr != nil {
			if candidate.Optional && errors.Is(parseErr, errGitCapabilityDiscoveryNotMatched) {
				continue
			}
			issues = append(issues, fmt.Sprintf("%s: %v", candidate.Path, parseErr))
			continue
		}
		for _, parsed := range items {
			if parsed == nil {
				continue
			}
			if err := applyExplicitGitIndexFields(parsed); err != nil {
				issues = append(issues, fmt.Sprintf("%s: %v", candidate.Path, err))
				continue
			}
			entryKey := ""
			if candidate.ItemType == "mcp" {
				entryKey, _ = parsed.Metadata["key"].(string)
			}
			parsed.ItemType = candidate.ItemType
			parsed.Slug = discoveredCapabilitySlug(parsed, candidate, repoName, entryKey)
			discovered = append(discovered, discoveredGitCapability{
				Path: candidate.Path, ItemType: candidate.ItemType, EntryKey: entryKey, Parsed: parsed,
			})
		}
	}

	// capability_items also has a repo/type/slug uniqueness constraint. Preserve
	// every valid manifest by applying a deterministic path-derived suffix when
	// two upstream entries otherwise collapse to the same display slug.
	seenSlugs := map[string]struct{}{}
	for i := range discovered {
		key := discovered[i].ItemType + "\x00" + discovered[i].Parsed.Slug
		if _, exists := seenSlugs[key]; exists {
			suffix := shortDiscoveryHash(discovered[i].Path + "\x00" + discovered[i].EntryKey)
			discovered[i].Parsed.Slug += "-" + suffix
			issues = append(issues, fmt.Sprintf("duplicate %s slug was disambiguated for %s", discovered[i].ItemType, discovered[i].Path))
			key = discovered[i].ItemType + "\x00" + discovered[i].Parsed.Slug
		}
		seenSlugs[key] = struct{}{}
	}

	status := models.GitCapabilityIdentificationClean
	if len(discovered) == 0 {
		status = models.GitCapabilityIdentificationUnknown
		if len(candidates) == 0 {
			issues = append(issues, "no supported capability manifest found")
		} else if len(issues) == 0 {
			issues = append(issues, "candidate manifests did not match a supported capability schema")
		}
	} else if len(issues) > 0 {
		status = models.GitCapabilityIdentificationWarning
	}

	now := time.Now().UTC()
	repoURL := strings.TrimRight(firstGitURL(cfg.WebURL, cfg.Endpoint), "/") + "/" + owner + "/" + repoName
	ownerID, err := resolveDiscoveredRepositoryOwner(s.DB.WithContext(ctx), cfg.ServerID, gitRepositoryOwnerID(repo), owner)
	if err != nil {
		return nil, fmt.Errorf("resolve repository owner %q: %w", owner, err)
	}
	repoKind := inferGitCapabilityRepoKind(repo, candidates)
	result := &GitCapabilitySyncResult{CommitSHA: headSHA, Skipped: len(issues)}

	err = s.DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := assertGitCapabilityLease(tx, lease); err != nil {
			return err
		}
		binding, err := ensureGitCapabilityRepositoryBinding(tx, cfg.ServerID, repo, repoURL, branchName, headSHA, repoKind, status, strings.Join(issues, "; "), ownerID, now)
		if err != nil {
			return err
		}
		for _, entry := range discovered {
			item, version, err := buildDiscoveredCapability(binding, cfg.ServerID, repo, repoURL, branchName, headSHA, repoKind, ownerID, entry, now)
			if err != nil {
				return err
			}
			if err := createDiscoveredCapability(tx, item, version, entry.Parsed.Tags, ownerID); err != nil {
				return err
			}
			result.Created++
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("commit Git capability discovery: %w", err)
	}
	return result, nil
}

// ParseGitDiscoveryFile parses a manifest whose type was inferred from the
// first repository tree. It is intentionally separate from ParseGitIndexFile:
// the latter requires and preserves an already-locked row identity.
func (p *ParserService) ParseGitDiscoveryFile(content []byte, sourcePath, itemType string) ([]*ParsedItem, error) {
	base := strings.ToLower(path.Base(sourcePath))
	var (
		items []*ParsedItem
		err   error
	)
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
		switch base {
		case "package.json":
			items, err = p.parseMCPPackageJSON(content, sourcePath)
		case "pyproject.toml":
			items, err = p.parseMCPPyproject(content, sourcePath)
		case "manifest.json":
			items, err = p.parseMCPManifestJSON(content, sourcePath)
		default:
			if strings.HasSuffix(base, ".json") {
				items, err = p.ParseMCPJSON(content, sourcePath)
			} else {
				var item *ParsedItem
				item, err = p.ParseSKILLMD(content, sourcePath)
				if item != nil {
					items = []*ParsedItem{item}
				}
			}
		}
	case "subagent":
		if strings.HasSuffix(base, ".yaml") || strings.HasSuffix(base, ".yml") {
			var item *ParsedItem
			item, err = p.parseAgentYAML(content, sourcePath)
			if item != nil {
				items = []*ParsedItem{item}
			}
		} else {
			items, err = p.ParseAgentsMD(content, sourcePath)
		}
	case "skill", "command":
		var item *ParsedItem
		item, err = p.ParseSKILLMD(content, sourcePath)
		if item != nil {
			items = []*ParsedItem{item}
		}
	default:
		return nil, fmt.Errorf("unsupported discovered capability type %q", itemType)
	}
	if err != nil {
		return nil, err
	}
	if len(items) == 0 {
		return nil, fmt.Errorf("manifest %q produced no capability", sourcePath)
	}
	for _, item := range items {
		if item == nil {
			continue
		}
		item.ItemType = itemType
		if strings.TrimSpace(item.Name) == "" {
			return nil, fmt.Errorf("manifest %q has no capability name", sourcePath)
		}
	}
	return items, nil
}

func discoverGitCapabilityCandidates(entries []gitsync.GitTreeEntry) []gitCapabilityCandidate {
	byPath := map[string]gitCapabilityCandidate{}
	for _, entry := range entries {
		if entry.Type != "" && !strings.EqualFold(entry.Type, "blob") {
			continue
		}
		candidate, ok := classifyGitCapabilityManifest(entry.Path)
		if ok {
			byPath[candidate.Path] = candidate
		}
	}
	result := make([]gitCapabilityCandidate, 0, len(byPath))
	for _, candidate := range byPath {
		result = append(result, candidate)
	}
	sort.Slice(result, func(i, j int) bool { return result[i].Path < result[j].Path })
	return result
}

func classifyGitCapabilityManifest(filePath string) (gitCapabilityCandidate, bool) {
	normalized := strings.TrimSpace(strings.ReplaceAll(filePath, "\\", "/"))
	if normalized == "" || strings.HasPrefix(normalized, "/") || path.Clean(normalized) != normalized || strings.HasPrefix(normalized, "../") {
		return gitCapabilityCandidate{}, false
	}
	lower := strings.ToLower(normalized)
	parts := strings.Split(lower, "/")
	base := parts[len(parts)-1]
	root := len(parts) == 1

	if root {
		switch base {
		case "skill.md":
			return gitCapabilityCandidate{Path: normalized, ItemType: "skill"}, true
		case "agent.md", "agents.md", "subagent.md":
			return gitCapabilityCandidate{Path: normalized, ItemType: "subagent"}, true
		case "command.md":
			return gitCapabilityCandidate{Path: normalized, ItemType: "command"}, true
		case "mcp.md", "mcp.json", ".mcp.json":
			return gitCapabilityCandidate{Path: normalized, ItemType: "mcp"}, true
		case "manifest.json", "package.json", "pyproject.toml":
			return gitCapabilityCandidate{Path: normalized, ItemType: "mcp", Optional: true}, true
		case ".plugin.json", "plugin.json", "plugin-manifest.json":
			return gitCapabilityCandidate{Path: normalized, ItemType: "plugin"}, true
		}
	}
	if len(parts) == 2 && parts[0] == ".claude-plugin" && base == "plugin.json" {
		return gitCapabilityCandidate{Path: normalized, ItemType: "plugin"}, true
	}
	if len(parts) == 2 && parts[0] == ".agent" && (base == "agent.yaml" || base == "agent.yml") {
		return gitCapabilityCandidate{Path: normalized, ItemType: "subagent"}, true
	}
	if len(parts) == 3 && parts[0] == "skills" && base == "skill.md" {
		return gitCapabilityCandidate{Path: normalized, ItemType: "skill"}, true
	}
	if strings.HasSuffix(base, ".md") && ((len(parts) >= 2 && parts[0] == "commands") ||
		(len(parts) >= 3 && parts[0] == ".claude" && parts[1] == "commands")) {
		return gitCapabilityCandidate{Path: normalized, ItemType: "command"}, true
	}
	if strings.HasSuffix(base, ".md") && ((len(parts) >= 2 && (parts[0] == "agents" || parts[0] == "subagents")) ||
		(len(parts) >= 3 && parts[0] == ".claude" && parts[1] == "agents")) {
		return gitCapabilityCandidate{Path: normalized, ItemType: "subagent"}, true
	}
	if len(parts) == 3 && parts[0] == "plugins" && base == ".plugin.json" {
		return gitCapabilityCandidate{Path: normalized, ItemType: "plugin"}, true
	}
	if len(parts) == 3 && parts[0] == "mcp" && (base == "mcp.md" || base == "mcp.json" || base == ".mcp.json") {
		return gitCapabilityCandidate{Path: normalized, ItemType: "mcp"}, true
	}
	return gitCapabilityCandidate{}, false
}

func inferGitCapabilityRepoKind(repo *gitsync.Repo, candidates []gitCapabilityCandidate) string {
	if repo != nil && repo.Mirror {
		return "mirror"
	}
	seedRoots := map[string]struct{}{}
	packOnly := false
	hasRequiredCandidate := false
	for _, candidate := range candidates {
		if candidate.Optional {
			continue
		}
		if !hasRequiredCandidate {
			packOnly = true
			hasRequiredCandidate = true
		}
		parts := strings.Split(strings.ToLower(candidate.Path), "/")
		if len(parts) > 1 {
			switch parts[0] {
			case "skills", "commands", "agents", "subagents", "mcp", "plugins":
				seedRoots[parts[0]] = struct{}{}
			}
		}
		if !(len(parts) == 3 && parts[0] == "plugins" && parts[2] == ".plugin.json") {
			packOnly = false
		}
	}
	if len(seedRoots) >= 2 {
		return "seed"
	}
	if hasRequiredCandidate && packOnly {
		return "pack"
	}
	return "standalone"
}

func (p *ParserService) parseAgentYAML(content []byte, sourcePath string) (*ParsedItem, error) {
	data := map[string]any{}
	if err := yaml.Unmarshal(content, &data); err != nil {
		return nil, fmt.Errorf("failed to parse agent YAML: %w", err)
	}
	name, _ := data["name"].(string)
	if strings.TrimSpace(name) == "" {
		name = inferNameFromPath(sourcePath)
	}
	item := &ParsedItem{
		Name: name, ItemType: "subagent", Version: "1.0.0", Content: string(content),
		Metadata: data, SourcePath: sourcePath, Slug: p.InferSlug(sourcePath),
	}
	item.Description, _ = data["description"].(string)
	item.Category, _ = data["category"].(string)
	if version, _ := data["version"].(string); version != "" {
		item.Version = version
	}
	if tags, ok := data["tags"].([]any); ok {
		for _, raw := range tags {
			if tag, ok := raw.(string); ok {
				item.Tags = append(item.Tags, tag)
			}
		}
	}
	return item, nil
}

func (p *ParserService) parseMCPPackageJSON(content []byte, sourcePath string) ([]*ParsedItem, error) {
	data := map[string]any{}
	if err := json.Unmarshal(content, &data); err != nil {
		return nil, fmt.Errorf("failed to parse package.json: %w", err)
	}
	name, _ := data["name"].(string)
	if !strings.Contains(strings.ToLower(name), "-mcp") {
		return nil, errGitCapabilityDiscoveryNotMatched
	}
	return []*ParsedItem{mcpProjectItem(data, content, sourcePath, name)}, nil
}

func (p *ParserService) parseMCPManifestJSON(content []byte, sourcePath string) ([]*ParsedItem, error) {
	data := map[string]any{}
	if err := json.Unmarshal(content, &data); err != nil {
		return nil, fmt.Errorf("failed to parse manifest.json: %w", err)
	}
	if _, ok := data["mcpServers"]; ok {
		return p.ParseMCPJSON(content, sourcePath)
	}
	if _, ok := data["mcp"]; !ok {
		return nil, errGitCapabilityDiscoveryNotMatched
	}
	name, _ := data["name"].(string)
	if name == "" {
		name = "mcp-manifest"
	}
	return []*ParsedItem{mcpProjectItem(data, content, sourcePath, name)}, nil
}

func (p *ParserService) parseMCPPyproject(content []byte, sourcePath string) ([]*ParsedItem, error) {
	data := map[string]any{}
	if err := toml.Unmarshal(content, &data); err != nil {
		return nil, fmt.Errorf("failed to parse pyproject.toml: %w", err)
	}
	project := nestedStringMap(data, "project")
	if len(project) == 0 {
		project = nestedStringMap(data, "tool", "poetry")
	}
	name, _ := project["name"].(string)
	if !strings.Contains(strings.ToLower(name), "-mcp") {
		return nil, errGitCapabilityDiscoveryNotMatched
	}
	metadata := map[string]any{"name": name, "format": "pyproject.toml"}
	for _, key := range []string{"version", "description"} {
		if value, ok := project[key].(string); ok {
			metadata[key] = value
		}
	}
	return []*ParsedItem{mcpProjectItem(metadata, content, sourcePath, name)}, nil
}

func nestedStringMap(root map[string]any, keys ...string) map[string]any {
	current := root
	for _, key := range keys {
		next, ok := current[key].(map[string]any)
		if !ok {
			return nil
		}
		current = next
	}
	return current
}

func mcpProjectItem(metadata map[string]any, content []byte, sourcePath, name string) *ParsedItem {
	item := &ParsedItem{
		Name: name, ItemType: "mcp", Version: "1.0.0", Content: string(content),
		Metadata: metadata, SourcePath: sourcePath, Slug: "mcp-" + normalizeDiscoveredCapabilitySlug(name),
	}
	item.Description, _ = metadata["description"].(string)
	if version, _ := metadata["version"].(string); version != "" {
		item.Version = version
	}
	return item
}

func resolveDiscoveredRepositoryOwner(db *gorm.DB, serverID string, gitUID int64, gitUsername string) (string, error) {
	type ownerMatch struct {
		UserSubjectID string `gorm:"column:user_subject_id"`
	}
	find := func(predicate string, args ...any) ([]ownerMatch, error) {
		var matches []ownerMatch
		err := db.Table("user_git_binding AS ugb").
			Select("ugb.user_subject_id").
			Joins("JOIN tenant_git_server_binding tgb ON tgb.tenant_id = ugb.tenant_id").
			Where("tgb.git_server_id = ? AND ugb.sync_status = ?", serverID, models.GitSyncStatusSynced).
			Where(predicate, args...).
			Order("ugb.user_subject_id ASC").
			Limit(2).
			Scan(&matches).Error
		return matches, err
	}

	var matches []ownerMatch
	var err error
	if gitUID > 0 {
		matches, err = find("ugb.git_uid = ?", gitUID)
		if err == nil && len(matches) == 0 {
			// Legacy/backfilled rows may have reached synced before git_uid was
			// populated. Only fall back to the mutable login when no conflicting
			// numeric identity is present.
			matches, err = find("ugb.git_uid IS NULL AND ugb.git_username = ?", gitUsername)
		}
	} else {
		matches, err = find("ugb.git_username = ?", gitUsername)
	}
	if err != nil {
		return "", err
	}
	if len(matches) > 1 {
		return "", fmt.Errorf("multiple synced user bindings match Git owner uid=%d login=%q", gitUID, gitUsername)
	}
	if len(matches) == 0 || strings.TrimSpace(matches[0].UserSubjectID) == "" {
		return gitCapabilityDiscoverySystemOwner, nil
	}
	return matches[0].UserSubjectID, nil
}

// gitCapabilityOwnerResolver defers resolveDiscoveredRepositoryOwner until a
// caller that actually stores the owner asks for it.
//
// Repositories bound before the discovery pipeline existed carry no
// repository-level record, so the owner projection is skipped for them
// entirely. Resolving eagerly made those rows inherit a failure they can never
// consume: an ambiguous Git identity aborted the whole repository sync on every
// push. A stalled sync is not merely an outage — it also leaves the registry's
// last_sync_sha behind HEAD, which is what lets the legacy clone poller decide
// the repository has changes worth ingesting.
//
// One sync runs in a single goroutine and both consumers sit in the same
// transaction, so plain fields are enough; the error is memoised too, so an
// ambiguous identity costs one query rather than one per consumer.
//
// Construct it with the index transaction's own handle, never with the pooled
// *gorm.DB: every consumer runs inside that transaction, so a pooled handle
// would read outside its snapshot and would deadlock outright against a
// single-connection pool.
type gitCapabilityOwnerResolver struct {
	resolve func() (string, error)
	done    bool
	id      string
	err     error
}

func newGitCapabilityOwnerResolver(tx *gorm.DB, serverID string, gitUID int64, gitUsername string) *gitCapabilityOwnerResolver {
	return &gitCapabilityOwnerResolver{resolve: func() (string, error) {
		return resolveDiscoveredRepositoryOwner(tx, serverID, gitUID, gitUsername)
	}}
}

func (r *gitCapabilityOwnerResolver) OwnerID() (string, error) {
	if !r.done {
		r.id, r.err = r.resolve()
		r.done = true
	}
	return r.id, r.err
}

func gitRepositoryOwnerID(repo *gitsync.Repo) int64 {
	if repo == nil || repo.Owner == nil {
		return 0
	}
	return repo.Owner.ID
}

func ensureGitCapabilityRepositoryBinding(
	tx *gorm.DB,
	serverID string,
	repo *gitsync.Repo,
	repoURL, branchName, headSHA, repoKind, identificationStatus, lastError, ownerID string,
	now time.Time,
) (*models.GitCapabilityRepository, error) {
	visibility := "public"
	if repo.Private {
		visibility = "private"
	}
	var binding models.GitCapabilityRepository
	err := tx.Where("git_server_id = ? AND git_repo_id = ?", serverID, repo.ID).First(&binding).Error
	if err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, err
	}
	if errors.Is(err, gorm.ErrRecordNotFound) {
		platformRepo := models.Repository{
			ID: uuid.NewString(), Name: discoveredRepositoryName(serverID, repo.ID), DisplayName: repo.FullName,
			Description: "Discovered from " + repoURL, Visibility: visibility, RepoType: "sync", OwnerID: ownerID,
		}
		if err := tx.Create(&platformRepo).Error; err != nil {
			return nil, fmt.Errorf("create repository projection: %w", err)
		}
		if err := syncDiscoveredRepositoryOwnerMembership(tx, platformRepo.ID, ownerID, strings.Split(repo.FullName, "/")[0]); err != nil {
			return nil, err
		}
		// SyncEnabled stays false: this registry exists to carry the
		// repository-level projection, and its capabilities are reconciled from
		// push webhooks. The legacy clone scheduler picks registries by
		// sync_enabled + external_url alone, and its closing sweep archives (and
		// drops the assets of) every registry row whose source path its own
		// include patterns do not match — which is most of the layouts the Git
		// discovery heuristic accepts. One repository, one sync handler.
		registry := models.CapabilityRegistry{
			ID: uuid.NewString(), Name: repo.FullName, Description: "Git-backed capabilities discovered from " + repo.FullName,
			SourceType: GitRegistrySourceType, ExternalURL: repoURL, ExternalBranch: branchName, SyncEnabled: false,
			LastSyncedAt: &now, LastSyncSHA: headSHA, SyncStatus: "idle", RepoID: platformRepo.ID, OwnerID: ownerID,
		}
		if err := tx.Create(&registry).Error; err != nil {
			return nil, fmt.Errorf("create capability registry: %w", err)
		}
		binding = models.GitCapabilityRepository{
			ID: uuid.NewString(), GitServerID: serverID, GitRepoID: repo.ID, RepositoryID: platformRepo.ID,
			RegistryID: registry.ID, FullName: repo.FullName, RepoKind: repoKind, IdentificationStatus: identificationStatus,
			Visibility: visibility, GitRemoteURL: repoURL, DefaultBranch: branchName, LastSyncedCommit: headSHA,
			LastSyncedAt: &now, LastError: lastError, CreatedBy: ownerID, CreatedAt: now, UpdatedAt: now,
		}
		if err := tx.Create(&binding).Error; err != nil {
			return nil, fmt.Errorf("create Git capability repository binding: %w", err)
		}
		return &binding, nil
	}

	if err := tx.Model(&models.Repository{}).Where("id = ?", binding.RepositoryID).Updates(map[string]any{
		"display_name": repo.FullName, "description": "Discovered from " + repoURL, "visibility": visibility, "owner_id": ownerID,
	}).Error; err != nil {
		return nil, err
	}
	if err := syncDiscoveredRepositoryOwnerMembership(tx, binding.RepositoryID, ownerID, strings.Split(repo.FullName, "/")[0]); err != nil {
		return nil, err
	}
	// sync_enabled is written on every pass so that registries created before
	// this guard existed converge to false without a data migration.
	if err := tx.Model(&models.CapabilityRegistry{}).Where("id = ?", binding.RegistryID).Updates(map[string]any{
		"name": repo.FullName, "external_url": repoURL, "external_branch": branchName, "last_synced_at": now,
		"last_sync_sha": headSHA, "sync_status": "idle", "owner_id": ownerID, "sync_enabled": false,
	}).Error; err != nil {
		return nil, err
	}
	updates := map[string]any{
		"full_name": repo.FullName, "repo_kind": repoKind, "identification_status": identificationStatus,
		"visibility": visibility, "git_remote_url": repoURL, "default_branch": branchName,
		"last_synced_commit": headSHA, "last_synced_at": now, "last_error": lastError, "updated_at": now,
	}
	if err := tx.Model(&models.GitCapabilityRepository{}).Where("id = ?", binding.ID).Updates(updates).Error; err != nil {
		return nil, err
	}
	if err := tx.Where("id = ?", binding.ID).First(&binding).Error; err != nil {
		return nil, err
	}
	return &binding, nil
}

func syncDiscoveredRepositoryOwnerMembership(tx *gorm.DB, repositoryID, ownerID, gitUsername string) error {
	if err := tx.Where("repo_id = ? AND role = ? AND user_id <> ?", repositoryID, "owner", ownerID).
		Delete(&models.RepoMember{}).Error; err != nil {
		return fmt.Errorf("remove stale repository owner membership: %w", err)
	}
	if ownerID == gitCapabilityDiscoverySystemOwner {
		return nil
	}

	var member models.RepoMember
	err := tx.Where("repo_id = ? AND user_id = ? AND role = ?", repositoryID, ownerID, "owner").First(&member).Error
	if err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
		return err
	}
	if errors.Is(err, gorm.ErrRecordNotFound) {
		member = models.RepoMember{ID: uuid.NewString(), RepoID: repositoryID, UserID: ownerID, Username: gitUsername, Role: "owner"}
		if err := tx.Create(&member).Error; err != nil {
			return fmt.Errorf("create repository owner membership: %w", err)
		}
		return nil
	}
	if member.Username != gitUsername {
		if err := tx.Model(&models.RepoMember{}).Where("id = ?", member.ID).Update("username", gitUsername).Error; err != nil {
			return fmt.Errorf("update repository owner membership: %w", err)
		}
	}
	return nil
}

func updateGitCapabilityRepositoryProjection(
	tx *gorm.DB,
	serverID string,
	repoID int64,
	fullName, repoURL, branchName, headSHA string,
	private bool,
	owner *gitCapabilityOwnerResolver,
	gitUsername string,
	now time.Time,
) error {
	var binding models.GitCapabilityRepository
	err := tx.Where("git_server_id = ? AND git_repo_id = ?", serverID, repoID).First(&binding).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		// Pre-bound rows from the incremental rollout have no repository-level
		// discovery record. They remain valid and are not retroactively inferred.
		// The owner is deliberately still unresolved at this point: this early
		// return is what keeps an unresolvable owner from failing their sync.
		return nil
	}
	if err != nil {
		return err
	}
	ownerID, err := owner.OwnerID()
	if err != nil {
		return fmt.Errorf("resolve repository owner: %w", err)
	}
	visibility := "public"
	if private {
		visibility = "private"
	}
	if err := tx.Model(&models.GitCapabilityRepository{}).Where("id = ?", binding.ID).Updates(map[string]any{
		"full_name": fullName, "visibility": visibility, "git_remote_url": repoURL,
		"default_branch": branchName, "last_synced_commit": headSHA, "last_synced_at": now,
		"last_error": "", "updated_at": now,
	}).Error; err != nil {
		return err
	}
	if err := tx.Model(&models.Repository{}).Where("id = ?", binding.RepositoryID).Updates(map[string]any{
		"display_name": fullName, "description": "Discovered from " + repoURL, "visibility": visibility, "owner_id": ownerID,
	}).Error; err != nil {
		return err
	}
	if err := syncDiscoveredRepositoryOwnerMembership(tx, binding.RepositoryID, ownerID, gitUsername); err != nil {
		return err
	}
	return tx.Model(&models.CapabilityRegistry{}).Where("id = ?", binding.RegistryID).Updates(map[string]any{
		"name": fullName, "external_url": repoURL, "external_branch": branchName,
		"last_synced_at": now, "last_sync_sha": headSHA, "sync_status": "idle", "owner_id": ownerID,
		"sync_enabled": false,
	}).Error
}

func ensureGitCapabilityReconciliationBinding(
	tx *gorm.DB,
	serverID string,
	repo *gitsync.Repo,
	repoURL, branchName, headSHA, repoKind string,
	owner *gitCapabilityOwnerResolver,
	boundItems []models.CapabilityItem,
	now time.Time,
) (*models.GitCapabilityRepository, error) {
	var binding models.GitCapabilityRepository
	err := tx.Where("git_server_id = ? AND git_repo_id = ?", serverID, repo.ID).First(&binding).Error
	if err == nil {
		return &binding, nil
	}
	if !errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, err
	}
	if len(boundItems) == 0 {
		return nil, errors.New("cannot create repository binding without an existing Git-backed item")
	}
	repositoryID := boundItems[0].RepoID
	registryID := boundItems[0].RegistryID
	if strings.TrimSpace(repositoryID) == "" || strings.TrimSpace(registryID) == "" {
		return nil, errors.New("existing Git-backed item has no repository or registry identity")
	}
	for _, item := range boundItems[1:] {
		if item.RepoID != repositoryID || item.RegistryID != registryID {
			return nil, errors.New("Git-backed items for one repository span multiple repository projections")
		}
	}
	ownerID, err := owner.OwnerID()
	if err != nil {
		return nil, fmt.Errorf("resolve repository owner: %w", err)
	}
	visibility := "public"
	if repo.Private {
		visibility = "private"
	}
	binding = models.GitCapabilityRepository{
		ID: uuid.NewString(), GitServerID: serverID, GitRepoID: repo.ID,
		RepositoryID: repositoryID, RegistryID: registryID, FullName: repo.FullName,
		RepoKind: repoKind, IdentificationStatus: models.GitCapabilityIdentificationClean,
		Visibility: visibility, GitRemoteURL: repoURL, DefaultBranch: branchName,
		LastSyncedCommit: headSHA, LastSyncedAt: &now, CreatedBy: ownerID,
		CreatedAt: now, UpdatedAt: now,
	}
	if err := tx.Create(&binding).Error; err != nil {
		return nil, fmt.Errorf("create Git capability repository binding for reconciliation: %w", err)
	}
	return &binding, nil
}

// hashDiscoveredCapabilityContent produces the same hash the DB write path
// produces for manifests the DB path also accepts, and degrades to plain text
// hashing for the manifest formats only Git discovery accepts.
//
// ContentHashService canonicalizes "mcp" and "plugin" content as JSON, which
// holds for every DB-path row of those types. Git discovery is wider, and for
// two of its formats the stored content is deliberately not JSON:
//
//   - plugin: ParsePluginJSON renders a frontmatter+markdown summary via
//     synthesizePluginContent (parser_service.go) instead of keeping the
//     .plugin.json bytes, so the plain-text branch is the *normal* path for
//     every discovered plugin, not an error case.
//   - mcp: pyproject.toml manifests keep the raw TOML (parseMCPPyproject
//     below), and non-JSON mcp manifests fall back to ParseSKILLMD.
//
// Handing those to the JSON canonicalizer fails, and skipping them would
// silently drop capabilities the previous md5 hash indexed without complaint.
// The manifest extension therefore selects the normalization. A .json manifest
// that still fails to canonicalize cannot reach here in practice — every .json
// parser rejects invalid JSON during parsing — so that last fallback is purely
// defensive. Every branch yields a 64-char SHA-256, keeping the column format
// uniform across backends.
// Exported so the fork path in package handlers hashes identically: a forked
// git-backed row copies the source content verbatim, so it must derive its hash
// the same way discovery would, not inherit the source row's (possibly empty or
// still-MD5) value.
func HashGitCapabilityContent(itemType, manifestPath, content string) string {
	return hashDiscoveredCapabilityContent(itemType, manifestPath, content)
}

func hashDiscoveredCapabilityContent(itemType, manifestPath, content string) string {
	hashSvc := NewContentHashService()
	if isJSONPath(manifestPath) {
		if hash, err := hashSvc.HashTextContent(itemType, content); err == nil {
			return hash
		}
	}
	hash, err := hashSvc.HashTextContent("", content)
	if err != nil {
		// The empty item type never takes the JSON branch, so this is
		// unreachable; hash the raw bytes rather than inventing an error path.
		return sha256Hex([]byte(content))
	}
	return hash
}

func buildDiscoveredCapability(
	binding *models.GitCapabilityRepository,
	serverID string,
	repo *gitsync.Repo,
	repoURL, branchName, headSHA, repoKind, ownerID string,
	entry discoveredGitCapability,
	now time.Time,
) (*models.CapabilityItem, *models.CapabilityVersion, error) {
	metadata := metadataJSON(entry.Parsed.Metadata)
	// Must stay identical to the DB write path (handlers create/update and the
	// migrate backfill all use ContentHashService), otherwise git-backed rows
	// hash differently and CheckItemConsistency can never compare across
	// backends.
	contentHash := hashDiscoveredCapabilityContent(entry.ItemType, entry.Path, entry.Parsed.Content)
	item := &models.CapabilityItem{
		ID: uuid.NewString(), RegistryID: binding.RegistryID, RepoID: binding.RepositoryID,
		Slug: entry.Parsed.Slug, ItemType: entry.ItemType, Name: entry.Parsed.Name,
		Description: entry.Parsed.Description, Descriptions: datatypes.JSON([]byte("{}")), Category: entry.Parsed.Category,
		Version: entry.Parsed.Version, Content: entry.Parsed.Content, ContentMD5: contentHash, CurrentRevision: 1,
		Metadata: metadata, SourcePath: entry.Path, SourceSHA: headSHA, SourceType: "git", Source: entry.Parsed.Source,
		SourceRepoURL: repoURL, SourceRepoRef: branchName, SourceRepoPath: entry.Path, ContentBackend: "git",
		SourceGitServerID: serverID, SourceGitRepoID: repo.ID, SourceGitEntryKey: entry.EntryKey,
		GitSHA: headSHA, GitLastSyncedAt: &now, GitSyncStatus: gitCapabilitySyncSynced,
		Status: "active", SecurityStatus: "unscanned", CreatedBy: ownerID, UpdatedBy: ownerID,
		IsBuiltIn: strings.EqualFold(strings.Split(repo.FullName, "/")[0], "costrict"),
	}
	version := &models.CapabilityVersion{
		ID: uuid.NewString(), ItemID: item.ID, Revision: 1, Name: item.Name, Description: item.Description,
		Descriptions: datatypes.JSON([]byte("{}")), Category: item.Category, Version: item.Version,
		Content: item.Content, ContentMD5: item.ContentMD5, Metadata: item.Metadata,
		CommitMsg: "Discovered from Git at " + headSHA, CreatedBy: ownerID, SourcePath: entry.Path, CreatedAt: now,
	}
	return item, version, nil
}

func createDiscoveredCapability(
	tx *gorm.DB,
	item *models.CapabilityItem,
	version *models.CapabilityVersion,
	tagSlugs []string,
	ownerID string,
) error {
	if err := tx.Select(
		"ID", "RegistryID", "RepoID", "Slug", "ItemType", "Name", "Description", "Descriptions", "Category", "Version",
		"Content", "ContentMD5", "CurrentRevision", "Metadata", "SourcePath", "SourceSHA", "SourceType", "Source",
		"SourceRepoURL", "SourceRepoRef", "SourceRepoPath", "ContentBackend", "SourceGitServerID", "SourceGitRepoID",
		"SourceGitEntryKey", "GitSHA", "GitLastSyncedAt", "GitSyncStatus", "GitSyncError", "Status", "SecurityStatus",
		"CreatedBy", "UpdatedBy", "IsBuiltIn", "CreatedAt", "UpdatedAt",
	).Create(item).Error; err != nil {
		return fmt.Errorf("create discovered capability %s: %w", item.SourceRepoPath, err)
	}
	if err := tx.Create(version).Error; err != nil {
		return fmt.Errorf("create initial version for %s: %w", item.SourceRepoPath, err)
	}
	if len(tagSlugs) == 0 {
		return nil
	}
	tagSvc := &TagService{DB: tx}
	tags, err := tagSvc.ResolveOrCreateForAssignment(tagSlugs, ownerID)
	if err != nil {
		return err
	}
	tagIDs := make([]string, 0, len(tags))
	for _, tag := range tags {
		tagIDs = append(tagIDs, tag.ID)
	}
	return tagSvc.SetItemTags(item.ID, tagIDs, TagSourceGit)
}

func uniqueDiscoveredCapabilitySlug(entry discoveredGitCapability, used map[string]struct{}) string {
	base := normalizeDiscoveredCapabilitySlug(entry.Parsed.Slug)
	candidate := base
	for attempt := 0; ; attempt++ {
		key := entry.ItemType + "\x00" + candidate
		if _, exists := used[key]; !exists {
			used[key] = struct{}{}
			return candidate
		}
		suffix := shortDiscoveryHash(entry.Path + "\x00" + entry.EntryKey)
		candidate = base + "-" + suffix
		if attempt > 0 {
			candidate = fmt.Sprintf("%s-%s-%d", base, suffix, attempt+1)
		}
	}
}

func discoveredCapabilitySlug(parsed *ParsedItem, candidate gitCapabilityCandidate, repoName, entryKey string) string {
	if explicit, _ := parsed.Metadata["slug"].(string); strings.TrimSpace(explicit) != "" {
		return normalizeDiscoveredCapabilitySlug(explicit)
	}
	slug := strings.TrimSpace(parsed.Slug)
	base := strings.ToLower(path.Base(candidate.Path))
	if entryKey != "" {
		slug = parsed.Slug
	} else if !strings.Contains(candidate.Path, "/") &&
		(base == "skill.md" || base == "agent.md" || base == "agents.md" || base == "subagent.md" || base == "command.md" || base == "mcp.md") {
		slug = repoName
	} else if candidate.ItemType == "plugin" && !strings.HasPrefix(strings.ToLower(candidate.Path), "plugins/") {
		slug = parsed.Name
	}
	return normalizeDiscoveredCapabilitySlug(slug)
}

func normalizeDiscoveredCapabilitySlug(value string) string {
	var out strings.Builder
	lastDash := false
	for _, r := range strings.ToLower(strings.TrimSpace(value)) {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			out.WriteRune(r)
			lastDash = false
			continue
		}
		if !lastDash && out.Len() > 0 {
			out.WriteByte('-')
			lastDash = true
		}
	}
	result := strings.Trim(out.String(), "-")
	if result == "" {
		return "unnamed"
	}
	return result
}

func discoveredRepositoryName(serverID string, repoID int64) string {
	return fmt.Sprintf("git-%s-%d", shortDiscoveryHash(serverID), repoID)
}

func shortDiscoveryHash(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:4])
}
