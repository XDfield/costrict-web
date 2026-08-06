package syncsnapshot

import (
	"encoding/json"
	"errors"
	"testing"
)

// TestDigestFixture_MatchesEmbedded is the guard on the shared contract: the
// fixture csc tests against and the code the server runs must not drift apart
// silently. Regenerating the fixture is possible (see
// TestDigestFixture_Regenerate) but never automatic — an unintended change to
// the canonical shape fails here instead of quietly rewriting the file csc
// also depends on.
func TestDigestFixture_MatchesEmbedded(t *testing.T) {
	embedded, err := LoadDigestFixture()
	if err != nil {
		t.Fatalf("load fixture: %v", err)
	}
	rebuilt, materialized := buildFixtureExpectation(t)

	if rebuilt.Expected.Canonical != embedded.Expected.Canonical {
		t.Fatalf("canonical bytes drifted from the shared fixture:\n got %s\nwant %s",
			rebuilt.Expected.Canonical, embedded.Expected.Canonical)
	}
	if rebuilt.Expected.SnapshotDigest != embedded.Expected.SnapshotDigest {
		t.Fatalf("snapshot digest = %s, fixture says %s",
			rebuilt.Expected.SnapshotDigest, embedded.Expected.SnapshotDigest)
	}
	if rebuilt.Expected.ContentDigest != embedded.Expected.ContentDigest {
		t.Fatalf("content digest = %s, fixture says %s",
			rebuilt.Expected.ContentDigest, embedded.Expected.ContentDigest)
	}
	if got := DigestBytes([]byte(embedded.Expected.Canonical)); got != embedded.Expected.SnapshotDigest {
		t.Fatalf("the fixture is internally inconsistent: sha256(canonical) = %s, digest field = %s",
			got, embedded.Expected.SnapshotDigest)
	}
	if materialized.Manifest.PageCount != len(embedded.Expected.Pages) {
		t.Fatalf("page count = %d, fixture has %d pages",
			materialized.Manifest.PageCount, len(embedded.Expected.Pages))
	}
}

// The client's whole verification procedure, executed against the fixture:
// take the pages, concatenate their elements in page order, rebuild the
// document, canonicalize, hash. This is the ONLY thing that entitles a client
// to act on a snapshot, so it is tested as a client would run it — from the
// pages, not from the server's in-memory state.
func TestFixturePages_ReassembleToTheDigest(t *testing.T) {
	fixture, err := LoadDigestFixture()
	if err != nil {
		t.Fatalf("load fixture: %v", err)
	}

	var items, tombstones []any
	for index, page := range fixture.Expected.Pages {
		if page.PageIndex != index {
			t.Fatalf("page %d reports index %d; pages must be contiguous and ordered", index, page.PageIndex)
		}
		wantComplete := index == len(fixture.Expected.Pages)-1
		if page.Complete != wantComplete {
			t.Fatalf("page %d complete = %v, want %v (only the final page may be complete)",
				index, page.Complete, wantComplete)
		}
		for _, item := range page.Items {
			items = append(items, Raw(item))
		}
		for _, tombstone := range page.Tombstones {
			tombstones = append(tombstones, Raw(tombstone))
		}
	}
	if len(items) != len(fixture.Items) || len(tombstones) != len(fixture.Tombstones) {
		t.Fatalf("reassembled %d items / %d tombstones, want %d / %d",
			len(items), len(tombstones), len(fixture.Items), len(fixture.Tombstones))
	}

	canonical, digest, err := Digest(DocumentFor(Manifest{
		SnapshotID:     fixture.Manifest.SnapshotID,
		Generation:     fixture.Manifest.Generation,
		GeneratedAt:    fixture.Manifest.GeneratedAt,
		PageCount:      len(fixture.Expected.Pages),
		ItemCount:      len(items),
		TombstoneCount: len(tombstones),
	}, items, tombstones))
	if err != nil {
		t.Fatalf("reassemble: %v", err)
	}
	if string(canonical) != fixture.Expected.Canonical {
		t.Fatalf("reassembled canonical differs:\n got %s\nwant %s", canonical, fixture.Expected.Canonical)
	}
	if digest != fixture.Expected.SnapshotDigest {
		t.Fatalf("reassembled digest = %s, want %s", digest, fixture.Expected.SnapshotDigest)
	}
}

// Dropping any single page must break verification. If it did not, a truncated
// response would be indistinguishable from a complete one and absence would
// once again become a removal signal.
func TestFixturePages_TruncationBreaksTheDigest(t *testing.T) {
	fixture, err := LoadDigestFixture()
	if err != nil {
		t.Fatalf("load fixture: %v", err)
	}
	for skip := range fixture.Expected.Pages {
		var items, tombstones []any
		for index, page := range fixture.Expected.Pages {
			if index == skip {
				continue
			}
			for _, item := range page.Items {
				items = append(items, Raw(item))
			}
			for _, tombstone := range page.Tombstones {
				tombstones = append(tombstones, Raw(tombstone))
			}
		}
		_, digest, err := Digest(DocumentFor(Manifest{
			SnapshotID:  fixture.Manifest.SnapshotID,
			Generation:  fixture.Manifest.Generation,
			GeneratedAt: fixture.Manifest.GeneratedAt,
			PageCount:   len(fixture.Expected.Pages),
			// The counts come from the manifest, which every page repeats — a
			// client cannot "fix" them from what it received.
			ItemCount:      len(fixture.Items),
			TombstoneCount: len(fixture.Tombstones),
		}, items, tombstones))
		if err != nil {
			t.Fatalf("digest without page %d: %v", skip, err)
		}
		if digest == fixture.Expected.SnapshotDigest {
			t.Fatalf("dropping page %d still produced the contract digest", skip)
		}
	}
}

func TestSlicePage_CutsOneFlatSequence(t *testing.T) {
	items := []json.RawMessage{[]byte(`"i0"`), []byte(`"i1"`), []byte(`"i2"`)}
	tombstones := []json.RawMessage{[]byte(`"t0"`), []byte(`"t1"`)}

	// pageSize 2 over 5 elements: [i0 i1] [i2 t0] [t1]
	wantItems := [][]string{{"i0", "i1"}, {"i2"}, {}}
	wantTombstones := [][]string{{}, {"t0"}, {"t1"}}
	if got := PageCountFor(len(items), len(tombstones), 2); got != 3 {
		t.Fatalf("page count = %d, want 3", got)
	}
	for index := 0; index < 3; index++ {
		gotItems, gotTombstones, err := SlicePage(items, tombstones, 2, index)
		if err != nil {
			t.Fatalf("slice page %d: %v", index, err)
		}
		assertRawEqual(t, index, "items", gotItems, wantItems[index])
		assertRawEqual(t, index, "tombstones", gotTombstones, wantTombstones[index])
	}
	if _, _, err := SlicePage(items, tombstones, 2, 3); err == nil {
		t.Fatal("a page beyond pageCount must be an error, not an empty page")
	}
	if _, _, err := SlicePage(items, tombstones, 2, -1); err == nil {
		t.Fatal("a negative page index must be an error")
	}
}

// An empty snapshot still has one page. Zero pages would leave `complete`
// nowhere to live, so "you are entitled to nothing" would be unspeakable and
// indistinguishable from a failed fetch.
func TestPageCountFor_EmptySnapshotHasOnePage(t *testing.T) {
	if got := PageCountFor(0, 0, 50); got != 1 {
		t.Fatalf("page count for an empty snapshot = %d, want 1", got)
	}
	items, tombstones, err := SlicePage(nil, nil, 50, 0)
	if err != nil {
		t.Fatalf("slice the only page of an empty snapshot: %v", err)
	}
	if len(items) != 0 || len(tombstones) != 0 {
		t.Fatalf("empty snapshot page = %d items / %d tombstones", len(items), len(tombstones))
	}
}

func assertRawEqual(t *testing.T, page int, label string, got []json.RawMessage, want []string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("page %d %s = %d elements, want %d", page, label, len(got), len(want))
	}
	for i := range want {
		if string(got[i]) != `"`+want[i]+`"` {
			t.Fatalf("page %d %s[%d] = %s, want %q", page, label, i, got[i], want[i])
		}
	}
}

// Materialization refuses to produce any snapshot the contract calls invalid.
// The alternative — emitting it and letting the client pick a precedence rule —
// puts the deletion decision in the place least able to audit it.
func TestMaterialize_RejectsConflictingState(t *testing.T) {
	item := Item{ItemID: "11111111-1111-4111-8111-111111111111", ItemType: "skill",
		Slug: "s", Name: "S", Version: "1", Sources: []string{EntitlementFavorite}}
	tombstone := Tombstone{ItemID: item.ItemID, Reason: ReasonUnfavorited,
		Source: SourceFavorite, EventID: "e1", RemovedAt: "2026-08-06T00:00:00Z"}

	cases := []struct {
		name       string
		items      []Item
		tombstones []Tombstone
	}{
		{"active and tombstoned at once", []Item{item}, []Tombstone{tombstone}},
		{"the same item twice as active", []Item{item, item}, nil},
		{
			"two non-identical tombstones for one item",
			nil,
			[]Tombstone{tombstone, {ItemID: item.ItemID, Reason: ReasonDistributionRevoked,
				Source: SourceDistribution, EventID: "e2", RemovedAt: "2026-08-06T00:00:01Z"}},
		},
		{
			"one event id reused across two items",
			nil,
			[]Tombstone{tombstone, {ItemID: "22222222-2222-4222-8222-222222222222",
				Reason: ReasonUnfavorited, Source: SourceFavorite, EventID: "e1",
				RemovedAt: "2026-08-06T00:00:01Z"}},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := Materialize(Manifest{SnapshotID: "s", Generation: 1,
				GeneratedAt: "2026-08-06T00:00:00Z"}, 50, tc.items, tc.tombstones); !errors.Is(err, ErrConflictingState) {
				t.Fatalf("err = %v, want ErrConflictingState", err)
			}
		})
	}

	// An exact duplicate of one tombstone is a query artifact, not a
	// contradiction, and is collapsed rather than rejected.
	if _, err := Materialize(Manifest{SnapshotID: "s", Generation: 1,
		GeneratedAt: "2026-08-06T00:00:00Z"}, 50, nil, []Tombstone{tombstone, tombstone}); err != nil {
		t.Fatalf("an identical duplicate tombstone must be tolerated, got %v", err)
	}
}

// Sorting is the server's job. Two servers running the same query against the
// same data may receive rows in different orders (different plan, different
// collation); if that reached the digest, the two would disagree.
func TestMaterialize_IsIndependentOfInputOrder(t *testing.T) {
	fixture, err := LoadDigestFixture()
	if err != nil {
		t.Fatalf("load fixture: %v", err)
	}
	items := fixture.ToItems()
	tombstones := fixture.ToTombstones()
	reversedItems := make([]Item, len(items))
	for i := range items {
		reversedItems[i] = items[len(items)-1-i]
	}
	reversedTombstones := make([]Tombstone, len(tombstones))
	for i := range tombstones {
		reversedTombstones[i] = tombstones[len(tombstones)-1-i]
	}

	manifest := Manifest{SnapshotID: fixture.Manifest.SnapshotID,
		Generation: fixture.Manifest.Generation, GeneratedAt: fixture.Manifest.GeneratedAt}
	forward, err := Materialize(manifest, fixture.PageSize, items, tombstones)
	if err != nil {
		t.Fatalf("materialize forward: %v", err)
	}
	backward, err := Materialize(manifest, fixture.PageSize, reversedItems, reversedTombstones)
	if err != nil {
		t.Fatalf("materialize reversed: %v", err)
	}
	if forward.Digest != backward.Digest {
		t.Fatalf("input order changed the digest: %s vs %s", forward.Digest, backward.Digest)
	}
	if forward.Digest != fixture.Expected.SnapshotDigest {
		t.Fatalf("digest = %s, fixture says %s", forward.Digest, fixture.Expected.SnapshotDigest)
	}
}

// ContentDigest must ignore identity and time, or "nothing changed" would never
// be true and every poll would allocate a generation.
func TestContentDigest_IgnoresSnapshotIdentity(t *testing.T) {
	fixture, err := LoadDigestFixture()
	if err != nil {
		t.Fatalf("load fixture: %v", err)
	}
	items := fixture.ToItems()
	tombstones := fixture.ToTombstones()

	first, err := ContentDigest(fixture.PageSize, items, tombstones)
	if err != nil {
		t.Fatalf("content digest: %v", err)
	}
	second, err := ContentDigest(fixture.PageSize, items, tombstones)
	if err != nil {
		t.Fatalf("content digest: %v", err)
	}
	if first != second {
		t.Fatalf("content digest is not stable: %s vs %s", first, second)
	}

	// Two materializations with different ids/generations/times share the
	// content digest but not the contract digest.
	a, err := Materialize(Manifest{SnapshotID: "a", Generation: 1, GeneratedAt: "2026-01-01T00:00:00Z"},
		fixture.PageSize, items, tombstones)
	if err != nil {
		t.Fatalf("materialize a: %v", err)
	}
	b, err := Materialize(Manifest{SnapshotID: "b", Generation: 2, GeneratedAt: "2026-02-02T00:00:00Z"},
		fixture.PageSize, items, tombstones)
	if err != nil {
		t.Fatalf("materialize b: %v", err)
	}
	if a.Digest == b.Digest {
		t.Fatal("the contract digest must cover snapshot identity")
	}

	// A different page size is a differently shaped snapshot and must NOT reuse
	// the frozen artifact, so the content digest has to move with it.
	other, err := ContentDigest(fixture.PageSize+1, items, tombstones)
	if err != nil {
		t.Fatalf("content digest: %v", err)
	}
	if other == first {
		t.Fatal("page size must participate in the content digest")
	}
}

// A change to any observable field must move the content digest, or a device
// would never learn about it.
func TestContentDigest_MovesWithObservableChange(t *testing.T) {
	base := []Item{{ItemID: "11111111-1111-4111-8111-111111111111", ItemType: "skill",
		Slug: "s", Name: "S", Version: "1.0.0", ContentMD5: "abc", GitSHA: "",
		Sources: []string{EntitlementFavorite}}}
	baseline, err := ContentDigest(50, base, nil)
	if err != nil {
		t.Fatalf("content digest: %v", err)
	}

	mutations := map[string]func(*Item){
		"name":       func(i *Item) { i.Name = "S2" },
		"slug":       func(i *Item) { i.Slug = "s2" },
		"version":    func(i *Item) { i.Version = "1.0.1" },
		"contentMd5": func(i *Item) { i.ContentMD5 = "def" },
		"gitSha":     func(i *Item) { i.GitSHA = "1111111111111111111111111111111111111111" },
		"sources":    func(i *Item) { i.Sources = []string{EntitlementFavorite, EntitlementDistribution} },
	}
	for field, mutate := range mutations {
		t.Run(field, func(t *testing.T) {
			mutated := append([]Item(nil), base...)
			mutate(&mutated[0])
			got, err := ContentDigest(50, mutated, nil)
			if err != nil {
				t.Fatalf("content digest: %v", err)
			}
			if got == baseline {
				t.Fatalf("changing %s did not move the content digest", field)
			}
		})
	}
}
