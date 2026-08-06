package syncsnapshot

import (
	_ "embed"
	"encoding/json"
	"fmt"
)

// The shared digest fixture.
//
// The snapshot-v2 contract requires the server and csc to agree on the digest
// byte-for-byte, and "we both implemented RFC 8785" is not evidence of that —
// the two implementations share no code and the failure is silent (a client
// that computes a different digest discards every snapshot forever and simply
// stops converging). This file is the evidence: one input, one canonical
// string, one digest. The Go test asserts against it; csc ships a test that
// asserts against the same file.
//
// It is embedded rather than read from testdata/ so it is part of the binary's
// contract surface and cannot be edited to match a regression.
//
//go:embed fixtures/snapshot_digest_v2.json
var digestFixtureJSON []byte

// DigestFixture is the parsed shared fixture.
type DigestFixture struct {
	Contract    string             `json:"contract"`
	Description string             `json:"description"`
	PageSize    int                `json:"pageSize"`
	Manifest    FixtureManifest    `json:"manifest"`
	Items       []FixtureItem      `json:"items"`
	Tombstones  []FixtureTombstone `json:"tombstones"`
	Expected    FixtureExpected    `json:"expected"`
}

// FixtureManifest mirrors Manifest with JSON names, so the fixture file reads
// like the wire contract rather than like Go field names.
type FixtureManifest struct {
	SnapshotID  string `json:"snapshotId"`
	Generation  int64  `json:"generation"`
	GeneratedAt string `json:"generatedAt"`
}

// FixtureItem mirrors Item with JSON names.
type FixtureItem struct {
	ItemID     string   `json:"itemId"`
	ItemType   string   `json:"itemType"`
	Slug       string   `json:"slug"`
	Name       string   `json:"name"`
	Version    string   `json:"version"`
	ContentMD5 string   `json:"contentMd5"`
	GitSHA     string   `json:"gitSha"`
	Sources    []string `json:"sources"`
}

// FixtureTombstone mirrors Tombstone with JSON names. An absent
// lifecycleReason is written as JSON null in the fixture, exactly as it appears
// on the wire.
type FixtureTombstone struct {
	ItemID          string  `json:"itemId"`
	Reason          string  `json:"reason"`
	LifecycleReason *string `json:"lifecycleReason"`
	Source          string  `json:"source"`
	EventID         string  `json:"eventId"`
	RemovedAt       string  `json:"removedAt"`
}

// FixtureExpected is what every implementation must reproduce.
type FixtureExpected struct {
	// Canonical is the whole canonical document as a string. It is spelled out
	// rather than only hashed so a mismatch tells an implementer WHICH byte is
	// wrong instead of only that something is.
	Canonical string `json:"canonical"`
	// SnapshotDigest is SHA-256 of Canonical.
	SnapshotDigest string `json:"snapshotDigest"`
	// ContentDigest is the server-internal change-detection key. csc never sees
	// it; it is pinned here so a change to the change-detection rule cannot be
	// made accidentally.
	ContentDigest string `json:"contentDigest"`
	// Pages is the deterministic page slicing of this snapshot at PageSize.
	Pages []FixturePage `json:"pages"`
}

// FixturePage is one page exactly as it goes on the wire.
//
// Elements are strings holding the canonical element text, not embedded JSON
// values, and that is the point: a JSON pretty-printer would reformat an
// embedded value and the fixture would then pin bytes nobody ever transmits.
// As strings they survive any formatting of the fixture file itself.
type FixturePage struct {
	PageIndex  int      `json:"pageIndex"`
	Complete   bool     `json:"complete"`
	Items      []string `json:"items"`
	Tombstones []string `json:"tombstones"`
}

// LoadDigestFixture returns the embedded shared fixture.
func LoadDigestFixture() (*DigestFixture, error) {
	var fixture DigestFixture
	if err := json.Unmarshal(digestFixtureJSON, &fixture); err != nil {
		return nil, fmt.Errorf("syncsnapshot: parse embedded digest fixture: %w", err)
	}
	return &fixture, nil
}

// ToItems converts the fixture's items to contract items.
func (f *DigestFixture) ToItems() []Item {
	items := make([]Item, 0, len(f.Items))
	for _, item := range f.Items {
		items = append(items, Item{
			ItemID:     item.ItemID,
			ItemType:   item.ItemType,
			Slug:       item.Slug,
			Name:       item.Name,
			Version:    item.Version,
			ContentMD5: item.ContentMD5,
			GitSHA:     item.GitSHA,
			Sources:    item.Sources,
		})
	}
	return items
}

// ToTombstones converts the fixture's tombstones to contract tombstones.
func (f *DigestFixture) ToTombstones() []Tombstone {
	tombstones := make([]Tombstone, 0, len(f.Tombstones))
	for _, tombstone := range f.Tombstones {
		lifecycle := ""
		if tombstone.LifecycleReason != nil {
			lifecycle = *tombstone.LifecycleReason
		}
		tombstones = append(tombstones, Tombstone{
			ItemID:          tombstone.ItemID,
			Reason:          tombstone.Reason,
			LifecycleReason: lifecycle,
			Source:          tombstone.Source,
			EventID:         tombstone.EventID,
			RemovedAt:       tombstone.RemovedAt,
		})
	}
	return tombstones
}
