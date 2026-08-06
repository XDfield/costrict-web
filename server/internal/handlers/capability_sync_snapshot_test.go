package handlers

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/costrict/costrict-web/server/internal/middleware"
	"github.com/costrict/costrict-web/server/internal/services"
	"github.com/costrict/costrict-web/server/internal/syncsnapshot"
	"github.com/gin-gonic/gin"
)

func newSnapshotRouter(t *testing.T, userID string, enabled bool) (*gin.Engine, *CapabilitySyncSnapshotHandler) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	r := gin.New()
	// A service with no DB: every test here exercises a branch that returns
	// before touching one. The DB-backed behaviour is covered by the PostgreSQL
	// suite in internal/services, where the isolation and constraint mechanisms
	// that decide correctness actually exist.
	handler := NewCapabilitySyncSnapshotHandler(&services.CapabilitySyncSnapshotService{}, enabled)
	r.GET("/api/sync/v2/snapshot", func(c *gin.Context) {
		if userID != "" {
			c.Set(middleware.UserIDKey, userID)
		}
		c.Next()
	}, handler.GetCapabilitySyncSnapshot)
	return r, handler
}

func snapshotRequest(t *testing.T, router *gin.Engine, target string) *httptest.ResponseRecorder {
	t.Helper()
	w := httptest.NewRecorder()
	router.ServeHTTP(w, httptest.NewRequest(http.MethodGet, target, nil))
	return w
}

// A deployment that has not enabled contract v2 answers 404, which is the "fall
// back to the legacy favorites endpoint" signal a mixed fleet needs. It must not
// answer 200 with an empty body — an empty success is precisely what a client
// that infers removal from absence would act on.
func TestCapabilitySyncSnapshot_DisabledReturnsNotFound(t *testing.T) {
	router, _ := newSnapshotRouter(t, "user-a", false)
	w := snapshotRequest(t, router, "/api/sync/v2/snapshot")
	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404; body = %s", w.Code, w.Body.String())
	}
}

func TestCapabilitySyncSnapshot_RequiresAuthentication(t *testing.T) {
	router, _ := newSnapshotRouter(t, "", true)
	w := snapshotRequest(t, router, "/api/sync/v2/snapshot")
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401; body = %s", w.Code, w.Body.String())
	}
}

// A page after 0 without a pinned snapshot id would be page N of whatever
// snapshot happened to be current at that moment, i.e. pages from different
// snapshots in one reassembly — the exact mixing the frozen artifact removes.
func TestCapabilitySyncSnapshot_LaterPageRequiresSnapshotID(t *testing.T) {
	router, _ := newSnapshotRouter(t, "user-a", true)
	w := snapshotRequest(t, router, "/api/sync/v2/snapshot?page=2")
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body = %s", w.Code, w.Body.String())
	}
}

// A malformed page is rejected rather than defaulted to 0: silently serving
// page 0 to a client asking for page 3 makes it assemble the same page twice,
// fail the digest, and retry forever without ever learning why.
func TestCapabilitySyncSnapshot_MalformedPageIsRejected(t *testing.T) {
	router, _ := newSnapshotRouter(t, "user-a", true)
	for _, page := range []string{"abc", "-1", "1.5"} {
		w := snapshotRequest(t, router, "/api/sync/v2/snapshot?snapshotId=x&page="+page)
		if w.Code != http.StatusBadRequest {
			t.Fatalf("page=%q status = %d, want 400; body = %s", page, w.Code, w.Body.String())
		}
	}
}

// The wire encoding must not disturb the canonical element bytes.
//
// gin's default c.JSON HTML-escapes `<`, `>` and `&`, which would rewrite the
// stored bytes of any capability whose name contains one. A re-canonicalizing
// client still lands on the right digest, but there is no reason to transmit
// something other than what was hashed — and every reason not to make a correct
// client's job depend on undoing an escape the server chose to add.
func TestCapabilitySyncSnapshot_ResponsePreservesCanonicalElementBytes(t *testing.T) {
	fixture, err := syncsnapshot.LoadDigestFixture()
	if err != nil {
		t.Fatalf("load fixture: %v", err)
	}
	// Page 0 of the shared fixture holds the element with `<`, `&`, an escaped
	// quote, a tab and a control character.
	page := fixture.Expected.Pages[0]
	if len(page.Items) == 0 {
		t.Fatal("the fixture's first page must carry items")
	}

	handler := NewCapabilitySyncSnapshotHandler(&services.CapabilitySyncSnapshotService{}, true)
	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)

	served := &services.SnapshotPage{
		ContractVersion: syncsnapshot.ContractVersion,
		SnapshotID:      fixture.Manifest.SnapshotID,
		Generation:      fixture.Manifest.Generation,
		GeneratedAt:     time.Date(2026, 8, 6, 12, 34, 56, 0, time.UTC),
		PageIndex:       0,
		PageCount:       len(fixture.Expected.Pages),
		ItemCount:       len(fixture.Items),
		TombstoneCount:  len(fixture.Tombstones),
		SnapshotDigest:  fixture.Expected.SnapshotDigest,
		Complete:        false,
	}
	for _, item := range page.Items {
		served.Items = append(served.Items, []byte(item))
	}
	handler.writePage(c, "user-a", served)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}
	// Decoded with raw elements, the way a verifying client must: the declared
	// element structs exist for generated clients, but the digest is computed
	// over the bytes.
	var decoded struct {
		ContractVersion int               `json:"contractVersion"`
		Complete        bool              `json:"complete"`
		Items           []json.RawMessage `json:"items"`
		Tombstones      []json.RawMessage `json:"tombstones"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &decoded); err != nil {
		t.Fatalf("decode response: %v\nbody = %s", err, w.Body.String())
	}
	if decoded.Tombstones == nil {
		t.Fatal("tombstones must serialize as [] rather than null; a client should never have to distinguish the two")
	}
	if len(decoded.Items) != len(page.Items) {
		t.Fatalf("response carried %d items, want %d", len(decoded.Items), len(page.Items))
	}
	for i, item := range decoded.Items {
		if string(item) != page.Items[i] {
			t.Fatalf("item %d was re-encoded in transit:\n got %s\nwant %s", i, item, page.Items[i])
		}
		// And the property a client actually relies on: parsing and
		// re-canonicalizing lands on the hashed bytes regardless.
		canonical, err := syncsnapshot.CanonicalizeJSON(item)
		if err != nil {
			t.Fatalf("re-canonicalize item %d: %v", i, err)
		}
		if string(canonical) != page.Items[i] {
			t.Fatalf("item %d does not re-canonicalize to the hashed bytes:\n got %s\nwant %s",
				i, canonical, page.Items[i])
		}
	}
	if decoded.Complete {
		t.Fatal("a non-final page must never claim completeness")
	}
	if decoded.ContractVersion != syncsnapshot.ContractVersion {
		t.Fatalf("contractVersion = %d, want %d", decoded.ContractVersion, syncsnapshot.ContractVersion)
	}
}
