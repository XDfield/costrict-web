package handlers

import (
	"encoding/json"
	"net/http"
	"sort"
	"testing"

	"github.com/costrict/costrict-web/server/internal/database"
	"github.com/costrict/costrict-web/server/internal/models"
	"github.com/costrict/costrict-web/server/internal/services"
)

// asset is a tiny helper for building non-binary text ArchiveAssets in tests.
func asset(path, content string) services.ArchiveAsset {
	return services.ArchiveAsset{Path: path, Content: []byte(content), Size: int64(len(content))}
}

// indexBySourcePath returns extracted children keyed by source_path for easy
// assertions.
func indexBySourcePath(children []pluginChildAsset) map[string]pluginChildAsset {
	out := make(map[string]pluginChildAsset, len(children))
	for _, c := range children {
		out[c.SourcePath] = c
	}
	return out
}

func TestExtractSubSkillAssets_SkillsAndEvaluators(t *testing.T) {
	assets := []services.ArchiveAsset{
		asset("skills/requirement-analysis/SKILL.md", "---\nname: Req\n---\nbody"),
		asset("skills/requirement-analysis/references/notes.md", "ref"),
		// evaluators are SKILL.md-shaped and must be extracted under a
		// non-skills/ prefix (generic, not cospower-specific).
		asset("evaluators/aireq-evaluator/SKILL.md", "---\nname: Eval\n---\neval body"),
		asset("evaluators/aireq-evaluator/agents/runner.md", "agent attached to evaluator"),
		// noise that must NOT become a child
		asset("examples/demo.md", "doc"),
		asset("CLAUDE.md", "doc"),
	}

	children := extractSubSkillAssets(assets)
	byPath := indexBySourcePath(children)

	if len(children) != 2 {
		t.Fatalf("expected 2 directory children (skill + evaluator), got %d: %+v", len(children), children)
	}

	skill, ok := byPath["skills/requirement-analysis/SKILL.md"]
	if !ok {
		t.Fatalf("missing skill child; got paths %v", sourcePaths(children))
	}
	if skill.ItemType != "skill" {
		t.Errorf("skill child item_type = %q, want skill", skill.ItemType)
	}
	if len(skill.Assets) != 1 || skill.Assets[0].Path != "references/notes.md" {
		t.Errorf("skill child should carry its sibling assets relative to its dir, got %+v", skill.Assets)
	}

	eval, ok := byPath["evaluators/aireq-evaluator/SKILL.md"]
	if !ok {
		t.Fatalf("evaluator under non-skills/ prefix was not extracted; got %v", sourcePaths(children))
	}
	if eval.ItemType != "skill" {
		t.Errorf("evaluator child item_type = %q, want skill", eval.ItemType)
	}
	if eval.SourcePath != "evaluators/aireq-evaluator/SKILL.md" {
		t.Errorf("evaluator source_path not faithful: %q", eval.SourcePath)
	}
	if len(eval.Assets) != 1 || eval.Assets[0].Path != "agents/runner.md" {
		t.Errorf("evaluator child should carry sibling assets, got %+v", eval.Assets)
	}
}

func TestExtractPluginFileChildren_AllTypes(t *testing.T) {
	assets := []services.ArchiveAsset{
		asset("commands/run-tests.md", "# run tests"),
		asset("agents/reviewer.md", "# reviewer"),
		// rules are nested under a group dir and may use non-ASCII filenames.
		asset("rules/dfx/安全.md", "# security rule"),
		asset("rules/coding-standards/go-checklist.md", "# go checklist"),
		asset("templates/system-design.md", "# template"),
		// must NOT match: SKILL.md (directory type, handled elsewhere), non-md,
		// and unrelated dirs.
		asset("skills/foo/SKILL.md", "---\nname: Foo\n---\nx"),
		asset("templates/logo.png", "binarystub"),
		asset("examples/demo.md", "doc"),
	}
	// mark the png binary so it is ignored
	for i := range assets {
		if assets[i].Path == "templates/logo.png" {
			assets[i].Binary = true
		}
	}

	children := extractPluginFileChildren(assets)
	byPath := indexBySourcePath(children)

	want := map[string]string{
		"commands/run-tests.md":                  "command",
		"agents/reviewer.md":                     "subagent",
		"rules/dfx/安全.md":                        "rule",
		"rules/coding-standards/go-checklist.md": "rule",
		"templates/system-design.md":             "template",
	}
	if len(children) != len(want) {
		t.Fatalf("expected %d file children, got %d: %v", len(want), len(children), sourcePaths(children))
	}
	for sp, wantType := range want {
		ch, ok := byPath[sp]
		if !ok {
			t.Errorf("missing child for %q", sp)
			continue
		}
		if ch.ItemType != wantType {
			t.Errorf("child %q item_type = %q, want %q", sp, ch.ItemType, wantType)
		}
		// source_path must be the verbatim repo-relative path.
		if ch.SourcePath != sp {
			t.Errorf("child source_path not faithful: got %q want %q", ch.SourcePath, sp)
		}
	}

	// Nested rules sharing a leaf would collide if only the leaf were used; the
	// group segment must keep slug suffixes unique.
	if byPath["rules/dfx/安全.md"].SlugSuffix == byPath["rules/coding-standards/go-checklist.md"].SlugSuffix {
		t.Errorf("nested rule slug suffixes collided")
	}
	if got := byPath["rules/coding-standards/go-checklist.md"].SlugSuffix; got != "coding-standards-go-checklist" {
		t.Errorf("nested rule slug suffix = %q, want coding-standards-go-checklist", got)
	}
	// Non-ASCII leaf: the group segment survives even when the leaf slugifies
	// to empty downstream, keeping it distinguishable.
	if got := byPath["rules/dfx/安全.md"].SlugSuffix; got != "dfx-安全" {
		t.Errorf("non-ascii rule slug suffix = %q, want dfx-安全", got)
	}
}

// TestUploadPlugin_PromotesAllChildTypes is the end-to-end archive-upload parity
// check: a cospower-shaped zip promotes skills, evaluators, commands, agents,
// rules (nested + non-ASCII), and templates as parent-linked children with
// path-faithful source_path.
func TestUploadPlugin_PromotesAllChildTypes(t *testing.T) {
	defer setupTestDB(t)()
	createPublicRegistry(t)
	setMemoryStorageBackend(t)

	files := map[string][]byte{
		"CLAUDE.md":                              []byte("# Cospower Plugin"),
		"cospowers.config.json":                  []byte(`{"rules":{"安全":"rules/dfx/安全.md"}}`),
		"skills/requirement-analysis/SKILL.md":   skillMD("Requirement Analysis", "body"),
		"evaluators/aireq-evaluator/SKILL.md":    skillMD("Aireq Evaluator", "eval body"),
		"commands/run-tests.md":                  []byte("# run tests"),
		"agents/reviewer.md":                     []byte("# reviewer"),
		"rules/dfx/安全.md":                        []byte("# security rule"),
		"rules/coding-standards/go-checklist.md": []byte("# go checklist"),
		"templates/system-design.md":             []byte("# template"),
		// excluded noise
		"examples/demo.md": []byte("doc"),
	}
	zipBytes := createTestZip(files)
	w := postMultipart(newItemRouter("u1"), "/api/items", map[string]string{
		"itemType": "plugin",
		"name":     "Cospower Plugin",
		"slug":     "cospower-plugin",
	}, zipBytes)
	if w.Code != http.StatusCreated {
		t.Fatalf("plugin upload: expected 201, got %d: %s", w.Code, w.Body.String())
	}
	var created map[string]any
	if err := json.NewDecoder(w.Body).Decode(&created); err != nil {
		t.Fatalf("decode plugin response: %v", err)
	}
	pluginID, _ := created["id"].(string)
	if pluginID == "" {
		t.Fatal("expected plugin id")
	}

	var children []models.CapabilityItem
	if err := database.DB.Where("parent_plugin_id = ?", pluginID).
		Order("source_path asc").Find(&children).Error; err != nil {
		t.Fatalf("load children: %v", err)
	}

	gotByPath := make(map[string]models.CapabilityItem, len(children))
	for _, ch := range children {
		gotByPath[ch.SourcePath] = ch
		if ch.ParentPluginID == nil || *ch.ParentPluginID != pluginID {
			t.Errorf("child %q parent_plugin_id = %v, want %s", ch.SourcePath, ch.ParentPluginID, pluginID)
		}
	}

	wantTypes := map[string]string{
		"skills/requirement-analysis/SKILL.md":   "skill",
		"evaluators/aireq-evaluator/SKILL.md":    "skill",
		"commands/run-tests.md":                  "command",
		"agents/reviewer.md":                     "subagent",
		"rules/dfx/安全.md":                        "rule",
		"rules/coding-standards/go-checklist.md": "rule",
		"templates/system-design.md":             "template",
	}
	if len(children) != len(wantTypes) {
		t.Fatalf("expected %d promoted children, got %d: %v", len(wantTypes), len(children), childSourcePaths(children))
	}
	for sp, wantType := range wantTypes {
		ch, ok := gotByPath[sp]
		if !ok {
			t.Errorf("missing promoted child for %q; got %v", sp, childSourcePaths(children))
			continue
		}
		if ch.ItemType != wantType {
			t.Errorf("child %q item_type = %q, want %q", sp, ch.ItemType, wantType)
		}
	}

	// noise dirs must not be promoted
	for _, ch := range children {
		if ch.SourcePath == "examples/demo.md" || ch.SourcePath == "CLAUDE.md" {
			t.Errorf("non-functional file %q was wrongly promoted", ch.SourcePath)
		}
	}

	// slugs must be unique (nested rules with shared/non-ASCII leaves rely on
	// the slug-collision retry + group-prefixed suffix).
	slugSeen := map[string]bool{}
	for _, ch := range children {
		if slugSeen[ch.Slug] {
			t.Errorf("duplicate child slug %q", ch.Slug)
		}
		slugSeen[ch.Slug] = true
	}
}

func sourcePaths(children []pluginChildAsset) []string {
	out := make([]string, 0, len(children))
	for _, c := range children {
		out = append(out, c.SourcePath)
	}
	sort.Strings(out)
	return out
}

func childSourcePaths(children []models.CapabilityItem) []string {
	out := make([]string, 0, len(children))
	for _, c := range children {
		out = append(out, c.SourcePath)
	}
	sort.Strings(out)
	return out
}
