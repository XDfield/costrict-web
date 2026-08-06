package syncsnapshot

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// fixtureSource is the fixture's INPUT, held in code so the expected half of
// the file can never be edited into agreement with a regression: regenerating
// always re-derives canonical/digest/pages from this.
//
// The strings are chosen to hit every rule the two implementations could
// disagree on:
//
//   - `<`, `>`, `&`   — encoding/json escapes these by default, JCS does not.
//   - U+2028          — encoding/json escapes it even with HTML escaping off.
//   - a raw tab, `"` and `\` — the short-escape table.
//   - U+0001          — the \u00xx form, lowercase hex.
//   - CJK and an emoji — literal UTF-8 pass-through, and (via the emoji in a
//     name) a supplementary-plane character that a UTF-16 client re-encodes.
//   - item ids supplied out of order, so a client that trusts arrival order
//     instead of sorting gets a different digest and finds out immediately.
var fixtureSource = DigestFixture{
	Contract: "csc-snapshot-v2",
	Description: "Shared canonicalization fixture for the csc snapshot v2 digest. " +
		"Server (Go) and client (csc) must both reproduce expected.canonical byte-for-byte " +
		"and expected.snapshotDigest as its lowercase SHA-256 hex. " +
		"Rebuild the document with the documented reassembly rule: concatenate every page's " +
		"items in page order, then every page's tombstones in page order, and serialize " +
		"{contractVersion, snapshotId, generation, generatedAt, pageCount, itemCount, " +
		"tombstoneCount, items, tombstones} with RFC 8785. pageIndex, complete and " +
		"snapshotDigest are excluded by construction.",
	PageSize: 2,
	Manifest: FixtureManifest{
		SnapshotID:  "3f2b1c4d-5e6f-4a7b-8c9d-0e1f2a3b4c5d",
		Generation:  42,
		GeneratedAt: "2026-08-06T12:34:56Z",
	},
	Items: []FixtureItem{
		{
			// Deliberately not first once sorted.
			ItemID: "cccccccc-cccc-4ccc-8ccc-cccccccccccc",
			// U+1D11E (𝄞) is a supplementary-plane character: a UTF-16 client
			// holds it as a surrogate pair and must still emit the original
			// UTF-8 bytes.
			Name:       "中文技能 \u2028 𝄞",
			ItemType:   "skill",
			Slug:       "cjk-skill",
			Version:    "2.1.0",
			ContentMD5: "9f86d081884c7d659a2feaa0c55ad015a3bf4f1b2b0b822cd15d6c15b0f00a08",
			GitSHA:     "",
			Sources:    []string{"favorite"},
		},
		{
			ItemID:     "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa",
			Name:       "<script>&\"quoted\"\\path\tand\u0001control",
			ItemType:   "mcp",
			Slug:       "escapes",
			Version:    "1.0.0",
			ContentMD5: "",
			GitSHA:     "1111111111111111111111111111111111111111",
			// Supplied unsorted; the contract sorts them.
			Sources: []string{"favorite", "distribution"},
		},
		{
			ItemID:     "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb",
			Name:       "plain",
			ItemType:   "plugin",
			Slug:       "plain",
			Version:    "0.0.1",
			ContentMD5: "d41d8cd98f00b204e9800998ecf8427e",
			GitSHA:     "2222222222222222222222222222222222222222",
			Sources:    []string{"distribution"},
		},
	},
	Tombstones: []FixtureTombstone{
		{
			ItemID:          "ffffffff-ffff-4fff-8fff-ffffffffffff",
			Reason:          ReasonUnfavorited,
			LifecycleReason: nil,
			Source:          SourceFavorite,
			EventID:         "6c1f8e0a-3d44-4b2e-9a71-2b6f0d5c8e13",
			RemovedAt:       "2026-08-06T12:00:00Z",
		},
		{
			ItemID:          "dddddddd-dddd-4ddd-8ddd-dddddddddddd",
			Reason:          ReasonGitArchived,
			LifecycleReason: stringPtr("repository_deleted"),
			Source:          SourceGitLifecycle,
			EventID:         "a0b1c2d3-e4f5-4061-8273-8495a6b7c8d9",
			RemovedAt:       "2026-08-06T11:59:59Z",
		},
	},
}

func stringPtr(s string) *string { return &s }

// buildFixtureExpectation derives the whole expected half of the fixture from
// fixtureSource.
func buildFixtureExpectation(t *testing.T) (DigestFixture, *Materialized) {
	t.Helper()
	fixture := fixtureSource
	items := fixture.ToItems()
	tombstones := fixture.ToTombstones()

	materialized, err := Materialize(Manifest{
		SnapshotID:  fixture.Manifest.SnapshotID,
		Generation:  fixture.Manifest.Generation,
		GeneratedAt: fixture.Manifest.GeneratedAt,
	}, fixture.PageSize, items, tombstones)
	if err != nil {
		t.Fatalf("materialize fixture: %v", err)
	}
	contentDigest, err := ContentDigest(fixture.PageSize, items, tombstones)
	if err != nil {
		t.Fatalf("content digest: %v", err)
	}

	pages := make([]FixturePage, 0, materialized.Manifest.PageCount)
	for index := 0; index < materialized.Manifest.PageCount; index++ {
		pageItems, pageTombstones, err := SlicePage(materialized.Items, materialized.Tombstones, fixture.PageSize, index)
		if err != nil {
			t.Fatalf("slice page %d: %v", index, err)
		}
		pages = append(pages, FixturePage{
			PageIndex:  index,
			Complete:   index == materialized.Manifest.PageCount-1,
			Items:      asStrings(pageItems),
			Tombstones: asStrings(pageTombstones),
		})
	}

	fixture.Expected = FixtureExpected{
		Canonical:      string(materialized.Canonical),
		SnapshotDigest: materialized.Digest,
		ContentDigest:  contentDigest,
		Pages:          pages,
	}
	return fixture, materialized
}

func asStrings(values []json.RawMessage) []string {
	out := make([]string, 0, len(values))
	for _, value := range values {
		out = append(out, string(value))
	}
	return out
}

// TestDigestFixture_Regenerate rewrites the shared fixture when
// SYNCSNAPSHOT_UPDATE_FIXTURE=1. It is skipped otherwise, so an accidental
// contract change cannot quietly rewrite the file the client also depends on —
// the normal run FAILS instead (see TestDigestFixture_MatchesEmbedded).
func TestDigestFixture_Regenerate(t *testing.T) {
	if os.Getenv("SYNCSNAPSHOT_UPDATE_FIXTURE") != "1" {
		t.Skip("set SYNCSNAPSHOT_UPDATE_FIXTURE=1 to regenerate fixtures/snapshot_digest_v2.json")
	}
	fixture, _ := buildFixtureExpectation(t)
	encoded, err := json.MarshalIndent(fixture, "", "  ")
	if err != nil {
		t.Fatalf("encode fixture: %v", err)
	}
	encoded = append(encoded, '\n')
	path := filepath.Join("fixtures", "snapshot_digest_v2.json")
	if err := os.WriteFile(path, encoded, 0o644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	t.Logf("regenerated %s (%d bytes)", path, len(encoded))
}
