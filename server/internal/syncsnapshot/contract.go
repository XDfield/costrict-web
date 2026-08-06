package syncsnapshot

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
)

// ContractVersion is the csc sync contract this package materializes.
const ContractVersion = 2

// Tombstone reasons and sources, mirrored from
// models.SyncTombstoneReason*/Source* so the wire vocabulary is defined in one
// place that both the serializer and its fixture can reference.
//
// These constants are NOT an allowlist, and nothing in this package validates a
// tombstone's reason against them. The contract keeps the reason set open on
// purpose: a client that closes it welds the two enumerations together, and
// every future reason then needs a client release, a legacy-drain window and a
// minimum-version gate before the server may emit it. Removal safety comes from
// completeness, digest, generation, item id, event id and local-ownership
// checks — none of which consult the reason.
const (
	ReasonGitArchived         = "git_archived"
	ReasonUnfavorited         = "unfavorited"
	ReasonDistributionRevoked = "distribution_revoked"
	ReasonAdminArchived       = "admin_archived"
	ReasonItemDeleted         = "item_deleted"
	ReasonPackageFlattened    = "package_flattened"

	SourceGitLifecycle  = "git_lifecycle"
	SourceFavorite      = "favorite"
	SourceDistribution  = "distribution"
	SourceModeration    = "moderation"
	SourceCatalog       = "catalog"
	SourceDataMigration = "data_migration"
)

// Entitlement sources reported on an active item.
const (
	EntitlementFavorite     = "favorite"
	EntitlementDistribution = "distribution"
)

// DigestBytes is the contract's hash: lowercase SHA-256 hex over UTF-8 bytes.
func DigestBytes(canonical []byte) string {
	sum := sha256.Sum256(canonical)
	return hex.EncodeToString(sum[:])
}

// Item is one capability the principal is currently entitled to.
//
// The field set is deliberately small. It answers "which capabilities do I
// have, and has any of them changed?" and nothing else — content is fetched
// separately, so a snapshot stays proportional to the number of entitlements
// rather than to their size.
//
// It also excludes UpdatedAt on purpose. UpdatedAt moves whenever anything
// writes the row (a re-scan, a tag rebuild, a moderation edit), and since the
// snapshot's content digest is what decides whether a new generation is
// allocated, including it would mint generations for changes no client can
// observe. ContentMD5, GitSHA and Version cover every change that reaches a
// device.
type Item struct {
	ItemID     string
	ItemType   string
	Slug       string
	Name       string
	Version    string
	ContentMD5 string
	GitSHA     string
	// Sources is why the principal has this item: favorite, distribution, or
	// both. Sorted, so two servers computing the same entitlement agree.
	Sources []string
}

func (i Item) canonicalObject() Object {
	sources := make([]any, 0, len(i.Sources))
	sorted := append([]string(nil), i.Sources...)
	sort.Strings(sorted)
	for _, source := range sorted {
		sources = append(sources, source)
	}
	return Object{
		"itemId":     i.ItemID,
		"itemType":   i.ItemType,
		"slug":       i.Slug,
		"name":       i.Name,
		"version":    i.Version,
		"contentMd5": i.ContentMD5,
		"gitSha":     i.GitSHA,
		"sources":    Array(sources),
	}
}

// Tombstone is an EXPLICIT instruction to remove one capability for one
// principal. It is the only thing in the contract that may authorize a client
// to delete anything; an item's absence from `Items` never may.
type Tombstone struct {
	ItemID string
	Reason string
	// LifecycleReason is non-empty only for Reason == git_archived. It is
	// serialized as JSON null when empty rather than omitted: a client that has
	// to distinguish "key missing" from "key null" has two encodings of one
	// state, and only one of them was hashed.
	LifecycleReason string
	Source          string
	EventID         string
	RemovedAt       string
}

func (t Tombstone) canonicalObject() Object {
	var lifecycle any
	if t.LifecycleReason != "" {
		lifecycle = t.LifecycleReason
	}
	return Object{
		"itemId":          t.ItemID,
		"reason":          t.Reason,
		"lifecycleReason": lifecycle,
		"source":          t.Source,
		"eventId":         t.EventID,
		"removedAt":       t.RemovedAt,
	}
}

// Manifest is the header every page of a paged snapshot repeats verbatim.
//
// PageIndex, Complete and SnapshotDigest are NOT part of it: the first two are
// page-local and the third is the output. See DocumentFor.
type Manifest struct {
	SnapshotID     string
	Generation     int64
	GeneratedAt    string
	PageCount      int
	ItemCount      int
	TombstoneCount int
}

// Materialized is a whole snapshot frozen into canonical bytes.
//
// The elements are retained individually because paging slices the snapshot at
// the element level: page k carries elements [k*pageSize, (k+1)*pageSize) of
// Items followed by Tombstones, and those elements go on the wire as the exact
// bytes that were hashed.
type Materialized struct {
	Manifest   Manifest
	Items      []json.RawMessage
	Tombstones []json.RawMessage
	// Canonical is the whole document; Digest is its SHA-256. Digest is what
	// the client verifies after reassembling every page.
	Canonical []byte
	Digest    string
}

// ContentDigest is the CHANGE-DETECTION key, and it is deliberately not the
// contract digest.
//
// The contract digest covers snapshotId, generation and generatedAt, so it
// differs between two builds of identical content — which makes it useless for
// answering "did anything actually change?". Allocating a generation per
// request instead of per change is not merely wasteful: csc rejects anything
// not strictly greater than what it applied, so a polling fleet burning
// numbers is a fleet of clients doing full re-verification for nothing.
//
// ContentDigest therefore covers exactly the observable content plus the page
// size (a different page size is a differently-shaped snapshot, so it must
// produce a new generation rather than reuse a frozen artifact built for the
// old shape).
func ContentDigest(pageSize int, items []Item, tombstones []Tombstone) (string, error) {
	itemValues := make([]any, 0, len(items))
	for _, item := range items {
		itemValues = append(itemValues, item.canonicalObject())
	}
	tombstoneValues := make([]any, 0, len(tombstones))
	for _, tombstone := range tombstones {
		tombstoneValues = append(tombstoneValues, tombstone.canonicalObject())
	}
	canonical, err := Canonicalize(Object{
		"contractVersion": ContractVersion,
		"pageSize":        pageSize,
		"items":           Array(itemValues),
		"tombstones":      Array(tombstoneValues),
	})
	if err != nil {
		return "", err
	}
	return DigestBytes(canonical), nil
}

// PageCountFor is the number of pages a snapshot occupies.
//
// An empty snapshot still occupies ONE page. Zero pages would make `complete`
// undeliverable — the marker only ever appears on the final page — so a
// principal with no entitlements could never receive an authoritative "you have
// nothing", and a client would be unable to distinguish it from a failed fetch.
func PageCountFor(itemCount, tombstoneCount, pageSize int) int {
	if pageSize < 1 {
		pageSize = 1
	}
	total := itemCount + tombstoneCount
	if total <= 0 {
		return 1
	}
	return (total + pageSize - 1) / pageSize
}

// SlicePage returns page pageIndex of a materialized snapshot.
//
// Pages cut one flat sequence — every item, then every tombstone — so a page
// may hold a tail of items, a head of tombstones, or both. Concatenating the
// pages' items in page order therefore reproduces the full ordered item array,
// which is what makes the client's digest verification possible.
func SlicePage(items, tombstones []json.RawMessage, pageSize, pageIndex int) (pageItems, pageTombstones []json.RawMessage, err error) {
	if pageSize < 1 {
		return nil, nil, errors.New("syncsnapshot: page size must be positive")
	}
	pageCount := PageCountFor(len(items), len(tombstones), pageSize)
	if pageIndex < 0 || pageIndex >= pageCount {
		return nil, nil, fmt.Errorf("syncsnapshot: page %d is outside 0..%d", pageIndex, pageCount-1)
	}
	start := pageIndex * pageSize
	end := start + pageSize

	itemStart := clampRange(start, len(items))
	itemEnd := clampRange(end, len(items))
	tombstoneStart := clampRange(start-len(items), len(tombstones))
	tombstoneEnd := clampRange(end-len(items), len(tombstones))

	return items[itemStart:itemEnd], tombstones[tombstoneStart:tombstoneEnd], nil
}

func clampRange(value, limit int) int {
	if value < 0 {
		return 0
	}
	if value > limit {
		return limit
	}
	return value
}

// Materialize freezes a snapshot: it sorts, serializes, hashes, and returns
// both the whole-document bytes and the per-element bytes paging will slice.
//
// Sort order is the contract's: items by item id, tombstones by (itemId,
// eventId). Sorting here rather than trusting the caller's query means the
// digest cannot depend on a database's collation or on which index the planner
// happened to choose.
//
// pageSize is an input because PageCount is inside the digest: a snapshot is
// frozen together with its pagination, and re-paginating stored bytes would
// invalidate the digest the client verifies.
func Materialize(manifest Manifest, pageSize int, items []Item, tombstones []Tombstone) (*Materialized, error) {
	if pageSize < 1 {
		return nil, errors.New("syncsnapshot: page size must be positive")
	}
	sortedItems := append([]Item(nil), items...)
	sort.Slice(sortedItems, func(i, j int) bool { return sortedItems[i].ItemID < sortedItems[j].ItemID })
	sortedTombstones := append([]Tombstone(nil), tombstones...)
	sort.Slice(sortedTombstones, func(i, j int) bool {
		if sortedTombstones[i].ItemID != sortedTombstones[j].ItemID {
			return sortedTombstones[i].ItemID < sortedTombstones[j].ItemID
		}
		return sortedTombstones[i].EventID < sortedTombstones[j].EventID
	})

	if err := assertNoConflictingState(sortedItems, sortedTombstones); err != nil {
		return nil, err
	}

	manifest.ItemCount = len(sortedItems)
	manifest.TombstoneCount = len(sortedTombstones)
	manifest.PageCount = PageCountFor(manifest.ItemCount, manifest.TombstoneCount, pageSize)

	itemBytes := make([]json.RawMessage, 0, len(sortedItems))
	itemValues := make([]any, 0, len(sortedItems))
	for _, item := range sortedItems {
		encoded, err := Canonicalize(item.canonicalObject())
		if err != nil {
			return nil, fmt.Errorf("canonicalize item %s: %w", item.ItemID, err)
		}
		itemBytes = append(itemBytes, encoded)
		itemValues = append(itemValues, Raw(encoded))
	}
	tombstoneBytes := make([]json.RawMessage, 0, len(sortedTombstones))
	tombstoneValues := make([]any, 0, len(sortedTombstones))
	for _, tombstone := range sortedTombstones {
		encoded, err := Canonicalize(tombstone.canonicalObject())
		if err != nil {
			return nil, fmt.Errorf("canonicalize tombstone %s: %w", tombstone.ItemID, err)
		}
		tombstoneBytes = append(tombstoneBytes, encoded)
		tombstoneValues = append(tombstoneValues, Raw(encoded))
	}

	canonical, digest, err := Digest(DocumentFor(manifest, itemValues, tombstoneValues))
	if err != nil {
		return nil, err
	}
	return &Materialized{
		Manifest:   manifest,
		Items:      itemBytes,
		Tombstones: tombstoneBytes,
		Canonical:  canonical,
		Digest:     digest,
	}, nil
}

// DocumentFor builds the exact structure the digest is computed over. It is
// exported because it doubles as the contract's specification: a client
// reassembles every page's elements in page order, calls this, canonicalizes,
// hashes, and must obtain snapshotDigest.
//
// pageIndex, complete and snapshotDigest are absent by construction — they are
// page-local or derived, and including them would make the digest unverifiable
// from a reassembled snapshot.
func DocumentFor(manifest Manifest, items, tombstones []any) Object {
	if items == nil {
		items = []any{}
	}
	if tombstones == nil {
		tombstones = []any{}
	}
	return Object{
		"contractVersion": ContractVersion,
		"snapshotId":      manifest.SnapshotID,
		"generation":      manifest.Generation,
		"generatedAt":     manifest.GeneratedAt,
		"pageCount":       manifest.PageCount,
		"itemCount":       manifest.ItemCount,
		"tombstoneCount":  manifest.TombstoneCount,
		"items":           Array(items),
		"tombstones":      Array(tombstones),
	}
}

// SplitStoredPayload recovers the per-element bytes of a stored snapshot.
//
// Because the payload is canonical (no insignificant whitespace), each
// json.RawMessage is byte-identical to the element that was hashed, so a page
// carries the exact bytes a client needs in order to reassemble and verify —
// not a re-encoding that happens to look the same.
func SplitStoredPayload(payload []byte) (items, tombstones []json.RawMessage, err error) {
	var document struct {
		ContractVersion int               `json:"contractVersion"`
		Items           []json.RawMessage `json:"items"`
		Tombstones      []json.RawMessage `json:"tombstones"`
	}
	if err := json.Unmarshal(payload, &document); err != nil {
		return nil, nil, fmt.Errorf("syncsnapshot: parse stored payload: %w", err)
	}
	if document.ContractVersion != ContractVersion {
		return nil, nil, fmt.Errorf("syncsnapshot: stored payload is contract version %d, expected %d",
			document.ContractVersion, ContractVersion)
	}
	return document.Items, document.Tombstones, nil
}

// ErrConflictingState reports a snapshot that asserts two different final
// states for one item.
var ErrConflictingState = errors.New("syncsnapshot: conflicting final state for one item in one snapshot")

// assertNoConflictingState enforces "at most one final state per user/item per
// generation" at the point of materialization.
//
// The contract makes this the SERVER's problem on purpose. Handing a client
// both "you have this" and "delete this" and asking it to pick would mean every
// client implementation encodes its own precedence rule, and the first one to
// choose differently deletes something it should have kept. Refusing to
// produce the snapshot converts a silent divergence into a build failure with
// a name.
func assertNoConflictingState(items []Item, tombstones []Tombstone) error {
	active := make(map[string]struct{}, len(items))
	for _, item := range items {
		if _, duplicate := active[item.ItemID]; duplicate {
			return fmt.Errorf("%w: item %s appears twice as active", ErrConflictingState, item.ItemID)
		}
		active[item.ItemID] = struct{}{}
	}
	seenTombstones := make(map[string]Tombstone, len(tombstones))
	seenEvents := make(map[string]string, len(tombstones))
	for _, tombstone := range tombstones {
		if _, isActive := active[tombstone.ItemID]; isActive {
			return fmt.Errorf("%w: item %s is both active and tombstoned", ErrConflictingState, tombstone.ItemID)
		}
		if previous, duplicate := seenTombstones[tombstone.ItemID]; duplicate {
			if previous != tombstone {
				return fmt.Errorf("%w: item %s carries two non-identical tombstones", ErrConflictingState, tombstone.ItemID)
			}
			continue
		}
		// A repeated event id with different content is the failure mode that
		// makes a client dedupe away a removal it never applied, so it is
		// rejected even when the item ids differ.
		if previousItem, duplicate := seenEvents[tombstone.EventID]; duplicate && previousItem != tombstone.ItemID {
			return fmt.Errorf("%w: event id %s is reused across items %s and %s",
				ErrConflictingState, tombstone.EventID, previousItem, tombstone.ItemID)
		}
		seenTombstones[tombstone.ItemID] = tombstone
		seenEvents[tombstone.EventID] = tombstone.ItemID
	}
	return nil
}
