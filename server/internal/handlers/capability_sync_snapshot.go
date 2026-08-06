// Package handlers — the csc snapshot v2 read endpoint.
//
// This is the only API through which a client may learn that it is allowed to
// DELETE something a user has on their machine, so every branch here is written
// around one rule: a response that is not a complete, verified snapshot must
// leave the device untouched. Every failure mode below (disabled, unknown
// snapshot, expired snapshot, bad page, server error) therefore returns a
// non-200 rather than a partial 200 — an empty or truncated success is exactly
// what "absence means removal" looks like from the client side.
package handlers

import (
	"bytes"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/costrict/costrict-web/server/internal/logger"
	"github.com/costrict/costrict-web/server/internal/middleware"
	"github.com/costrict/costrict-web/server/internal/services"
	"github.com/gin-gonic/gin"
)

// CapabilitySyncSnapshotHandler serves csc snapshot v2.
type CapabilitySyncSnapshotHandler struct {
	svc *services.CapabilitySyncSnapshotService
	// enabled is the endpoint's own gate, distinct from the service's lifecycle
	// propagation switch: one decides whether v2 exists at all, the other
	// whether Git archival is allowed to instruct removals through it.
	enabled bool
}

// NewCapabilitySyncSnapshotHandler wires the handler.
func NewCapabilitySyncSnapshotHandler(svc *services.CapabilitySyncSnapshotService, enabled bool) *CapabilitySyncSnapshotHandler {
	return &CapabilitySyncSnapshotHandler{svc: svc, enabled: enabled}
}

// CapabilitySyncSnapshotPage is one page of a csc snapshot v2 response.
//
// Every field except PageIndex, Complete, Items and Tombstones is repeated
// verbatim on every page of the same snapshot; those four are page-local. The
// digest is computed over the whole reassembled snapshot, so no single page can
// be verified on its own — which is the property that makes a truncated
// response unusable rather than merely incomplete.
type CapabilitySyncSnapshotPage struct {
	// ContractVersion is 2 for this endpoint. A client that does not understand
	// it must not act on the body.
	ContractVersion int `json:"contractVersion"`
	// SnapshotID is opaque and carries no order. Pass it back to fetch further
	// pages of the SAME frozen snapshot.
	SnapshotID string `json:"snapshotId"`
	// Generation is the only ordering signal. It is strictly increasing per
	// principal; a client must reject anything not strictly greater than the
	// generation it last applied, and must never order by generatedAt.
	Generation int64 `json:"generation"`
	// GeneratedAt is display/diagnostic only. Server clocks move backwards.
	GeneratedAt string `json:"generatedAt"`
	PageIndex   int    `json:"pageIndex"`
	PageCount   int    `json:"pageCount"`
	// ItemCount / TombstoneCount are whole-snapshot totals, not page totals.
	// Check them against what you reassembled before verifying the digest.
	ItemCount      int `json:"itemCount"`
	TombstoneCount int `json:"tombstoneCount"`
	// SnapshotDigest is the lowercase SHA-256 hex of the RFC 8785
	// canonicalization of the whole reassembled snapshot.
	SnapshotDigest string `json:"snapshotDigest"`
	// Complete appears only on the final page of a fully materialized snapshot.
	// Nothing may be removed locally until it has been seen, together with every
	// page and a verified digest.
	Complete bool `json:"complete"`
	// Items are the capabilities the principal is currently entitled to. An
	// item's ABSENCE from this list is never an instruction to remove it.
	//
	// The declared element type exists so generated clients get a real shape;
	// the bytes actually transmitted come from rawItems (see MarshalJSON), which
	// carry the canonical form the digest was computed over.
	Items []CapabilitySyncSnapshotItem `json:"items"`
	// Tombstones are explicit removals. This is the ONLY thing that authorizes a
	// client to unload or disable a capability.
	Tombstones []CapabilitySyncSnapshotTombstone `json:"tombstones"`

	// rawItems / rawTombstones hold the stored canonical element bytes.
	//
	// They are unexported and serialized through MarshalJSON because the two
	// requirements pull in opposite directions: a generated client needs a
	// declared element schema, while the digest needs the elements transmitted
	// exactly as they were hashed. Re-encoding the typed structs would satisfy
	// the first and quietly break the second's byte-for-byte property (Go orders
	// keys by struct field, not by the contract's sort).
	rawItems      []json.RawMessage
	rawTombstones []json.RawMessage
}

// CapabilitySyncSnapshotItem is one entitled capability inside a snapshot page.
//
// The field set answers "which capabilities do I have, and has any of them
// changed?" — content is fetched separately, so a snapshot stays proportional
// to the NUMBER of entitlements rather than their size.
type CapabilitySyncSnapshotItem struct {
	ItemID   string `json:"itemId"`
	ItemType string `json:"itemType"`
	Slug     string `json:"slug"`
	Name     string `json:"name"`
	Version  string `json:"version"`
	// ContentMD5 / GitSHA are the change keys. Either may be empty depending on
	// where the capability's content lives.
	ContentMD5 string `json:"contentMd5"`
	GitSHA     string `json:"gitSha"`
	// Sources is why the principal has this item — "favorite", "distribution",
	// or both — sorted so the value is stable across servers.
	Sources []string `json:"sources"`
}

// CapabilitySyncSnapshotTombstone is an explicit instruction to remove one
// capability for the calling principal.
//
// It is the ONLY thing in this contract that authorizes a deletion, which is
// why it carries its own durable identity: eventId is the client's dedup key
// and is rotated on every new removal transition, so a re-removal after a
// restore is a distinct event rather than one the client has already applied.
type CapabilitySyncSnapshotTombstone struct {
	ItemID string `json:"itemId"`
	// Reason explains the removal; it does not authorize it. The tombstone's
	// PRESENCE is the instruction, so the set is OPEN by contract and a client
	// MUST NOT reject or ignore a value it does not recognise — report it
	// verbatim, fall back to generic user-facing wording, and still remove.
	// Values emitted today: git_archived | unfavorited | distribution_revoked |
	// admin_archived | item_deleted | package_flattened.
	Reason string `json:"reason"`
	// LifecycleReason is manifest_removed | default_branch_missing |
	// repository_deleted, and is present only for reason=git_archived. It is
	// null — never omitted — for the other reasons, so the key set of a
	// tombstone is fixed.
	LifecycleReason *string `json:"lifecycleReason"`
	// Source is the producing subsystem, determined by Reason and paired with it
	// one-to-one: git_lifecycle | favorite | distribution | moderation |
	// catalog | data_migration. Open for the same reason Reason is.
	Source string `json:"source"`
	// EventID is durable and globally unique. Apply each id at most once.
	EventID string `json:"eventId"`
	// RemovedAt is the server time of the removal transition (RFC 3339).
	RemovedAt string `json:"removedAt"`
}

// MarshalJSON emits the stored canonical element bytes rather than re-encoding
// the declared structs.
//
// Encoding is disabled for HTML here as well as at the gin layer: a capability
// named with `<` or `&` would otherwise be re-escaped on the way out, and while
// a client that re-canonicalizes still reaches the right digest, there is no
// reason to transmit something other than what was hashed.
func (p CapabilitySyncSnapshotPage) MarshalJSON() ([]byte, error) {
	wire := struct {
		ContractVersion int               `json:"contractVersion"`
		SnapshotID      string            `json:"snapshotId"`
		Generation      int64             `json:"generation"`
		GeneratedAt     string            `json:"generatedAt"`
		PageIndex       int               `json:"pageIndex"`
		PageCount       int               `json:"pageCount"`
		ItemCount       int               `json:"itemCount"`
		TombstoneCount  int               `json:"tombstoneCount"`
		SnapshotDigest  string            `json:"snapshotDigest"`
		Complete        bool              `json:"complete"`
		Items           []json.RawMessage `json:"items"`
		Tombstones      []json.RawMessage `json:"tombstones"`
	}{
		ContractVersion: p.ContractVersion,
		SnapshotID:      p.SnapshotID,
		Generation:      p.Generation,
		GeneratedAt:     p.GeneratedAt,
		PageIndex:       p.PageIndex,
		PageCount:       p.PageCount,
		ItemCount:       p.ItemCount,
		TombstoneCount:  p.TombstoneCount,
		SnapshotDigest:  p.SnapshotDigest,
		Complete:        p.Complete,
		Items:           p.rawItems,
		Tombstones:      p.rawTombstones,
	}
	if wire.Items == nil {
		wire.Items = []json.RawMessage{}
	}
	if wire.Tombstones == nil {
		wire.Tombstones = []json.RawMessage{}
	}

	var buf bytes.Buffer
	encoder := json.NewEncoder(&buf)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(wire); err != nil {
		return nil, err
	}
	// Encoder.Encode appends a newline; MarshalJSON must not.
	return bytes.TrimRight(buf.Bytes(), "\n"), nil
}

// GetCapabilitySyncSnapshot godoc
// @Summary      Fetch a page of the caller's capability sync snapshot (csc contract v2)
// @Description  Returns one page of a frozen, versioned snapshot of everything the caller is entitled to, plus explicit tombstones for entitlements that ended.
// @Description
// @Description  **Removal semantics.** A client may unload or disable a locally managed capability ONLY when it holds every page of one snapshot, the reassembled counts match `itemCount`/`tombstoneCount`, the recomputed digest equals `snapshotDigest`, the final page carried `complete=true`, `generation` is strictly greater than the last generation it applied, and that snapshot contains an explicit tombstone for the item. An item that is merely missing from `items` is NOT a removal signal — a network failure, an auth failure, a truncated page and an empty error body all look identical to absence.
// @Description
// @Description  **Paging.** Call without `snapshotId` to obtain the current snapshot's page 0; the response pins `snapshotId` and `pageCount`. Pass that `snapshotId` back with `page=1..pageCount-1` to collect the rest. Pages are deterministic slices of one stored artifact, so the set never shifts underneath a paging client. A snapshot that expires or is superseded past its grace window answers 410; restart from page 0.
// @Description
// @Description  **Digest.** Concatenate every page's `items` in page order, then every page's `tombstones` in page order, build `{contractVersion, snapshotId, generation, generatedAt, pageCount, itemCount, tombstoneCount, items, tombstones}`, serialize with RFC 8785 (JCS) and take the lowercase SHA-256 hex. `pageIndex`, `complete` and `snapshotDigest` are excluded. Re-canonicalize the parsed elements rather than concatenating the transferred text: JSON transports are free to re-escape characters such as `<` and U+2028, so raw bytes may differ while the canonical form does not.
// @Description
// @Description  **Ordering.** `generation` is the only ordering signal. `snapshotId` is opaque and `generatedAt` is diagnostic.
// @Tags         sync
// @Produce      json
// @Param        snapshotId  query  string  false  "Continue a snapshot already begun; required for page > 0"
// @Param        page        query  int     false  "Zero-based page index, default 0"
// @Success      200  {object}  CapabilitySyncSnapshotPage
// @Failure      400  {object}  object{error=string}  "Malformed page or a page > 0 without snapshotId"
// @Failure      401  {object}  object{error=string}
// @Failure      404  {object}  object{error=string}  "Contract v2 is not enabled on this deployment; fall back to the legacy favorites endpoint"
// @Failure      410  {object}  object{error=string}  "Snapshot expired or superseded; restart from page 0"
// @Failure      500  {object}  object{error=string}
// @Router       /sync/v2/snapshot [get]
func (h *CapabilitySyncSnapshotHandler) GetCapabilitySyncSnapshot(c *gin.Context) {
	if !h.enabled || h.svc == nil {
		// 404 rather than 501/503 so a mixed fleet has an unambiguous "this
		// deployment does not speak v2, use v1" signal that needs no new client
		// logic. Legacy /api/items?favorited=true is unchanged and still served.
		c.JSON(http.StatusNotFound, gin.H{"error": "Capability sync snapshot v2 is not enabled"})
		return
	}
	principalID := c.GetString(middleware.UserIDKey)
	if principalID == "" {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "Authentication required"})
		return
	}

	pageIndex, ok := parseSnapshotPage(c.Query("page"))
	if !ok {
		c.JSON(http.StatusBadRequest, gin.H{"error": "page must be a non-negative integer"})
		return
	}
	snapshotID := strings.TrimSpace(c.Query("snapshotId"))

	if snapshotID == "" {
		if pageIndex != 0 {
			// Without a pinned snapshot, page N would be page N of whatever
			// snapshot happened to be current at that moment — i.e. pages from
			// different snapshots, which is precisely the mixing the frozen
			// artifact exists to prevent. Refuse rather than silently rebuild.
			c.JSON(http.StatusBadRequest, gin.H{"error": "snapshotId is required for pages after 0"})
			return
		}
		snapshot, reused, err := h.svc.EnsureSnapshot(c.Request.Context(), principalID)
		if err != nil {
			logger.Error("[sync-snapshot] build failed principal=%s: %v", principalID, err)
			c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to build capability sync snapshot"})
			return
		}
		page, err := h.svc.PageOfSnapshot(c.Request.Context(), snapshot, 0, reused)
		if err != nil {
			h.writePageError(c, principalID, err)
			return
		}
		h.writePage(c, principalID, page)
		return
	}

	page, err := h.svc.GetSnapshotPage(c.Request.Context(), principalID, snapshotID, pageIndex)
	if err != nil {
		h.writePageError(c, principalID, err)
		return
	}
	h.writePage(c, principalID, page)
}

func (h *CapabilitySyncSnapshotHandler) writePage(c *gin.Context, principalID string, page *services.SnapshotPage) {
	logger.Info("[sync-snapshot] served contract=v2 principal=%s snapshot=%s generation=%d page=%d/%d items=%d tombstones=%d complete=%v reused=%v",
		principalID, page.SnapshotID, page.Generation, page.PageIndex, page.PageCount,
		page.ItemCount, page.TombstoneCount, page.Complete, page.Reused)

	body := CapabilitySyncSnapshotPage{
		ContractVersion: page.ContractVersion,
		SnapshotID:      page.SnapshotID,
		Generation:      page.Generation,
		GeneratedAt:     page.GeneratedAt.UTC().Format(time.RFC3339),
		PageIndex:       page.PageIndex,
		PageCount:       page.PageCount,
		ItemCount:       page.ItemCount,
		TombstoneCount:  page.TombstoneCount,
		SnapshotDigest:  page.SnapshotDigest,
		Complete:        page.Complete,
		rawItems:        make([]json.RawMessage, 0, len(page.Items)),
		rawTombstones:   make([]json.RawMessage, 0, len(page.Tombstones)),
	}
	for _, item := range page.Items {
		body.rawItems = append(body.rawItems, item)
	}
	for _, tombstone := range page.Tombstones {
		body.rawTombstones = append(body.rawTombstones, tombstone)
	}

	// PureJSON, not JSON: gin's default encoder HTML-escapes `<`, `>` and `&`,
	// which would rewrite the stored canonical bytes of any capability whose
	// name contains one. A client that re-canonicalizes still lands on the right
	// digest, but there is no reason to transmit something other than what was
	// hashed, and every reason not to make a correct client's job depend on
	// undoing an escape the server chose to add.
	c.PureJSON(http.StatusOK, body)
}

func (h *CapabilitySyncSnapshotHandler) writePageError(c *gin.Context, principalID string, err error) {
	switch {
	case errors.Is(err, services.ErrSnapshotNotFound):
		// 410, not 404: the snapshot was real and is now unavailable, and the
		// correct client response is "restart from page 0", not "this endpoint
		// does not exist".
		c.JSON(http.StatusGone, gin.H{"error": "Snapshot expired or superseded; restart from page 0"})
	case errors.Is(err, services.ErrSnapshotPageOutOfRange):
		c.JSON(http.StatusBadRequest, gin.H{"error": "page is outside this snapshot's page count"})
	default:
		logger.Error("[sync-snapshot] page failed principal=%s: %v", principalID, err)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to read capability sync snapshot"})
	}
}

// parseSnapshotPage rejects a malformed page rather than defaulting to 0.
// Serving page 0 to a client that asked for page 3 would make it reassemble the
// same page twice, fail the digest, and retry forever without ever being told
// why.
func parseSnapshotPage(raw string) (int, bool) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return 0, true
	}
	value, err := strconv.Atoi(trimmed)
	if err != nil || value < 0 {
		return 0, false
	}
	return value, true
}
