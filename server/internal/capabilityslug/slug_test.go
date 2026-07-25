package capabilityslug

import "testing"

func TestCanonical(t *testing.T) {
	const itemID = "12345678-abcd-4321-abcd-1234567890ab"
	cases := []struct {
		name     string
		itemType string
		slug     string
		display  string
		itemID   string
		want     string
	}{
		{name: "skill display slug", itemType: "skill", slug: "Skill with Skill", want: "skill-with-skill"},
		{name: "command separators", itemType: "command", slug: "MD_to DOCX", want: "md-to-docx"},
		{name: "subagent case", itemType: "subagent", slug: "Code Reviewer", want: "code-reviewer"},
		{name: "legacy agent alias", itemType: "agent", slug: "Release Agent", want: "release-agent"},
		{name: "missing slug uses display", itemType: "skill", display: "E2E Skill Tree", want: "e2e-skill-tree"},
		{name: "Chinese skill remains invocable", itemType: "skill", slug: "技能", display: "技能", itemID: itemID, want: "技能"},
		{name: "Chinese command spaces become dashes", itemType: "command", slug: "01 仓库概览", itemID: itemID, want: "01-仓库概览"},
		{name: "Unicode is NFC normalized", itemType: "skill", slug: "Cafe\u0301 技能", want: "café-技能"},
		{name: "non ASCII symbols remain invocable", itemType: "skill", slug: "😀 Skill", want: "😀-skill"},
		{name: "unsafe ASCII separators become dashes", itemType: "skill", slug: "../技能\\仓库", want: "技能-仓库"},
		{name: "separator only skill uses stable id", itemType: "skill", slug: " ! / ", itemID: itemID, want: "skill-12345678abcd4321abcd1234567890ab"},
		{name: "separator only skill waits for id", itemType: "skill", slug: " ! / ", want: ""},
		{name: "explicit mcp identifier is unchanged", itemType: "mcp", slug: "Acme:Server", want: "Acme:Server"},
		{name: "missing mcp slug keeps legacy generation", itemType: "mcp", display: "Acme Server", want: "acme-server"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := Canonical(tc.itemType, tc.slug, tc.display, tc.itemID); got != tc.want {
				t.Fatalf("Canonical(%q, %q, %q, %q) = %q, want %q", tc.itemType, tc.slug, tc.display, tc.itemID, got, tc.want)
			}
		})
	}
}

func TestCanonicalIsIdempotent(t *testing.T) {
	const itemID = "12345678-abcd-4321-abcd-1234567890ab"
	for _, itemType := range []string{"skill", "command", "subagent", "agent"} {
		for _, source := range []string{"Display Name", "技能 仓库", "Cafe\u0301 😀"} {
			first := Canonical(itemType, source, "", itemID)
			if second := Canonical(itemType, first, "", itemID); second != first {
				t.Fatalf("%s canonicalization is not idempotent for %q: first=%q second=%q", itemType, source, first, second)
			}
		}
	}
}

func TestHasAssignedCollisionSuffix(t *testing.T) {
	const compactID = "12345678abcd4321abcd1234567890ab"
	cases := []struct {
		candidate string
		want      bool
	}{
		{candidate: "foo-bar-2", want: true},
		{candidate: "foo-bar-10", want: true},
		{candidate: "foo-bar-migrated-" + compactID, want: true},
		{candidate: "foo-bar-migrated-" + compactID + "-2", want: true},
		{candidate: "foo-bar-1", want: false},
		{candidate: "foo-bar-child", want: false},
		{candidate: "foo-bar-migrated-short", want: false},
		{candidate: "other-2", want: false},
	}
	for _, tc := range cases {
		if got := HasAssignedCollisionSuffix("foo-bar", tc.candidate); got != tc.want {
			t.Errorf("HasAssignedCollisionSuffix(%q, %q) = %v, want %v", "foo-bar", tc.candidate, got, tc.want)
		}
	}
	if HasAssignedCollisionSuffix("", "-2") {
		t.Error("empty base must not match an allocated suffix")
	}
}
