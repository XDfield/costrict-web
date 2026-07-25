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
		{name: "non ASCII skill uses stable id", itemType: "skill", slug: "技能", display: "技能", itemID: itemID, want: "skill-12345678abcd4321abcd1234567890ab"},
		{name: "non ASCII command uses stable id", itemType: "command", slug: "命令", itemID: itemID, want: "command-12345678abcd4321abcd1234567890ab"},
		{name: "non ASCII waits for id", itemType: "skill", slug: "技能", want: ""},
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
		first := Canonical(itemType, "Display Name", "", itemID)
		if second := Canonical(itemType, first, "", itemID); second != first {
			t.Fatalf("%s canonicalization is not idempotent: first=%q second=%q", itemType, first, second)
		}
	}
}
