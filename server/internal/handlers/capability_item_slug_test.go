package handlers

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

func TestCanonicalCapabilitySlug(t *testing.T) {
	cases := []struct {
		name        string
		itemType    string
		slug        string
		displayName string
		want        string
	}{
		{name: "normalizes explicit display-style skill slug", itemType: "skill", slug: "Skill with Skill", displayName: "ignored", want: "skill-with-skill"},
		{name: "normalizes skill separators and case", itemType: "skill", slug: "MD_to DOCX", displayName: "ignored", want: "md-to-docx"},
		{name: "generates skill slug from display name", itemType: "skill", displayName: "E2E Skill Tree", want: "e2e-skill-tree"},
		{name: "uses stable fallback for skill values without ASCII identifier characters", itemType: "skill", slug: "技能", displayName: "技能", want: "skill-12345678abcd4321abcd1234567890ab"},
		{name: "normalizes commands", itemType: "command", slug: "Deploy Command", displayName: "ignored", want: "deploy-command"},
		{name: "normalizes subagents", itemType: "subagent", slug: "Review Agent", displayName: "ignored", want: "review-agent"},
		{name: "preserves explicit non-skill identifiers", itemType: "mcp", slug: "Acme:Server", displayName: "ignored", want: "Acme:Server"},
		{name: "generates missing non-skill slug with existing behavior", itemType: "mcp", displayName: "Acme Server", want: "acme-server"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := canonicalCapabilitySlug(tc.itemType, tc.slug, tc.displayName, "12345678-abcd-4321-abcd-1234567890ab"); got != tc.want {
				t.Fatalf("canonicalCapabilitySlug(%q, %q, %q) = %q, want %q", tc.itemType, tc.slug, tc.displayName, got, tc.want)
			}
		})
	}
}

func TestCreateItemDirect_NonASCIISkillUsesStableFallback(t *testing.T) {
	defer setupTestDB(t)()
	createPublicRegistry(t)

	w := postJSON(newItemRouter("u1"), "/api/items", map[string]interface{}{
		"itemType": "skill",
		"name":     "技能",
		"slug":     "技能",
		"content":  "# 技能",
	})
	if w.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", w.Code, w.Body.String())
	}

	var item map[string]interface{}
	if err := json.NewDecoder(w.Body).Decode(&item); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	slug, _ := item["slug"].(string)
	id, _ := item["id"].(string)
	if slug != "skill-"+strings.ReplaceAll(id, "-", "") {
		t.Fatalf("expected stable id fallback, got slug=%q id=%q", slug, id)
	}
	if item["name"] != "技能" {
		t.Fatalf("expected display name to remain unchanged, got %v", item["name"])
	}
}

func TestCreateItemDirect_NormalizesExplicitSlugAndPreservesDisplayName(t *testing.T) {
	defer setupTestDB(t)()
	createPublicRegistry(t)

	w := postJSON(newItemRouter("u1"), "/api/items", map[string]interface{}{
		"itemType": "skill",
		"name":     "Skill with Skill",
		"slug":     "Skill with Skill",
		"content":  "# Skill with Skill",
	})
	if w.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", w.Code, w.Body.String())
	}

	var item map[string]interface{}
	if err := json.NewDecoder(w.Body).Decode(&item); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if item["slug"] != "skill-with-skill" {
		t.Fatalf("expected canonical slug, got %v", item["slug"])
	}
	if item["name"] != "Skill with Skill" {
		t.Fatalf("expected display name to remain unchanged, got %v", item["name"])
	}
}

func TestCreateItemDirect_NormalizedSlugConflict(t *testing.T) {
	defer setupTestDB(t)()
	createPublicRegistry(t)

	first := postJSON(newItemRouter("u1"), "/api/items", map[string]interface{}{
		"itemType": "skill",
		"name":     "First",
		"slug":     "skill-with-skill",
		"content":  "# First",
	})
	if first.Code != http.StatusCreated {
		t.Fatalf("create first item: expected 201, got %d: %s", first.Code, first.Body.String())
	}

	second := postJSON(newItemRouter("u1"), "/api/items", map[string]interface{}{
		"itemType": "skill",
		"name":     "Second",
		"slug":     "Skill with Skill",
		"content":  "# Second",
	})
	if second.Code != http.StatusConflict {
		t.Fatalf("expected normalized slug conflict, got %d: %s", second.Code, second.Body.String())
	}
}

func TestCreateItemDirect_ZipSkill_NormalizesExplicitSlug(t *testing.T) {
	defer setupTestDB(t)()
	createPublicRegistry(t)
	setMemoryStorageBackend(t)

	zipBytes := createTestZip(map[string][]byte{
		"SKILL.md": []byte("---\nname: MD to DOCX\n---\n# Convert"),
	})
	w := postMultipart(newItemRouter("u1"), "/api/items", map[string]string{
		"itemType": "skill",
		"name":     "MD to DOCX",
		"slug":     "MD to DOCX",
	}, zipBytes)
	if w.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", w.Code, w.Body.String())
	}

	var item map[string]interface{}
	if err := json.NewDecoder(w.Body).Decode(&item); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if item["slug"] != "md-to-docx" {
		t.Fatalf("expected canonical slug, got %v", item["slug"])
	}
	if item["name"] != "MD to DOCX" {
		t.Fatalf("expected display name to remain unchanged, got %v", item["name"])
	}
}
