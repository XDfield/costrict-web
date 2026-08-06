// Package handlers — reading a Git-backed capability's revision history.
//
// This is a DETAIL-scoped endpoint on purpose. History is per item and its
// natural query (`WHERE item_id = ? ORDER BY revision_no DESC LIMIT n`) is one
// index scan per item; routing a list or marketplace response through it would
// reintroduce exactly the N+1 the list endpoints were rewritten to avoid. No
// list handler may call into this file.

package handlers

import (
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/costrict/costrict-web/server/internal/database"
	"github.com/costrict/costrict-web/server/internal/models"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"gorm.io/gorm"
)

const (
	// gitHistoryDefaultLimit / gitHistoryMaxLimit are the contract's numbers.
	// A larger request is CLAMPED rather than rejected: the caller asked for
	// "recent", and answering 20 is a better response to "give me 100" than a
	// 400 that tells a UI nothing it can render.
	gitHistoryDefaultLimit = 5
	gitHistoryMaxLimit     = 20

	// gitHistoryShortSHALength is the display width of the commit fallback used
	// when a manifest declares no version. Seven hex characters is the
	// convention every Git UI already uses.
	gitHistoryShortSHALength = 7
)

// GitRevisionResponse is one successful Git projection transition of an item.
type GitRevisionResponse struct {
	// RevisionNo is strictly increasing within the item and is the paging
	// cursor: pass the smallest one you received as `before_revision`.
	RevisionNo int64 `json:"revisionNo"`
	// GitSHA is the repository default-branch head OBSERVED when this change was
	// detected. It is a coordinate, not a cause: the revision was triggered by
	// this item's own projected content changing, and the head may have advanced
	// past the commit that made that change (a repository holds many
	// capabilities). A UI must not label it "the commit that made this change".
	GitSHA   string `json:"gitSha"`
	ShortSHA string `json:"shortSha"`
	// Version is the item-visible version and is never empty: a manifest that
	// declares none falls back to the short commit SHA, because a history row
	// with a blank version label is unrenderable.
	Version string `json:"version"`
	// ObservedAt is the server time of the successful observation. It is not
	// the commit's authored time.
	ObservedAt string `json:"observedAt"`
	// Source is backfill | provision | push | reconcile | restore.
	Source string `json:"source"`
}

// GitHistoryResponse is a page of an item's revision history, newest first.
type GitHistoryResponse struct {
	Revisions []GitRevisionResponse `json:"revisions"`
	HasMore   bool                  `json:"hasMore"`
	// NextBeforeRevision is the cursor for the next page; absent on the last
	// page. It is item-bound and stable: revisions only ever get HIGHER numbers,
	// so a row appended while a client pages cannot shift the rows behind the
	// cursor.
	NextBeforeRevision int64 `json:"nextBeforeRevision,omitempty"`
	// HistoryBackend is "git" for a Git-backed item and "db" for every other
	// row — a DB-backed capability is versioned through /items/{id}/versions and
	// has no Git history, which this reports as an empty page rather than an
	// error.
	HistoryBackend string `json:"historyBackend"`
}

// ListItemGitHistory godoc
// @Summary      List a Git-backed item's recent revisions
// @Description  Returns the item's most recent successful Git projection transitions, newest first. A revision exists only where a successful projection changed THIS item's own projected content, so a duplicate webhook delivery, a same-content reconcile, and a commit that only touched another capability in the same repository all add nothing, while a revert back to earlier content adds a new revision. `gitSha` is the repository head observed when the change was detected — a coordinate, not the commit that made the change, and it must not be presented as one. `version` is never empty: when the manifest declares no version the short commit SHA is returned instead. Paging uses the item-bound `before_revision` cursor. Authorization is identical to item detail: a caller who relies on the repository being public has that visibility re-verified against the Git server, so a repository that has gone private answers 404 and an unreachable Git server answers 503 (error_code GIT_VISIBILITY_UNVERIFIED) rather than serving the timeline. A DB-backed item returns an empty page with historyBackend=db.
// @Tags         items
// @Produce      json
// @Param        id               path      string  true   "Item ID"
// @Param        limit            query     int     false  "Page size, default 5, values above 20 are clamped to 20"
// @Param        before_revision  query     int     false  "Return revisions strictly older than this revision number"
// @Success      200  {object}  GitHistoryResponse
// @Failure      400  {object}  object{error=string}
// @Failure      403  {object}  object{error=string}
// @Failure      404  {object}  object{error=string}
// @Failure      500  {object}  object{error=string}
// @Failure      503  {object}  object{error=string,error_code=string}
// @Failure      504  {object}  object{error=string,error_code=string}
// @Router       /items/{id}/git-history [get]
func ListItemGitHistory(c *gin.Context) {
	id := c.Param("id")
	db := database.GetDB()

	limit, ok := parseGitHistoryLimit(c.Query("limit"))
	if !ok {
		c.JSON(http.StatusBadRequest, gin.H{"error": "limit must be a positive integer"})
		return
	}
	beforeRevision, ok := parseGitHistoryCursor(c.Query("before_revision"))
	if !ok {
		c.JSON(http.StatusBadRequest, gin.H{"error": "before_revision must be a positive integer"})
		return
	}

	// capability_items.id is a PostgreSQL `uuid`, so a malformed id makes the
	// lookup fail with 22P02 rather than simply not matching. Without this check
	// the error branch below correctly refuses to call that a not-found and
	// answers 500 — telling the caller the server broke when in fact the id they
	// sent cannot name any item. Rejecting it here keeps the 500 for real
	// database failures, where it belongs.
	if _, err := uuid.Parse(strings.TrimSpace(id)); err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Item not found"})
		return
	}

	// Object-level access control, identical to GetItem / ListItemVersions.
	// History leaks the same facts item detail does — that the capability
	// exists, its versions, and where in Git it lives — so it must be gated the
	// same way or it becomes the unauthenticated read path around the detail
	// endpoint (the IDOR that /items/{id}/versions was fixed for).
	//
	// authorizeItemRead, not canAccessItem: the local repository visibility this
	// row is judged by is refreshed only by the periodic reconcile (Gitea emits
	// no event for a visibility change), so for a Git-backed row it has to be
	// confirmed against the Git server before a caller who relies on it is
	// handed the commit timeline. AC-LH11 / AC-LH16.
	var item models.CapabilityItem
	if err := db.Preload("Registry").First(&item, "id = ?", id).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			c.JSON(http.StatusNotFound, gin.H{"error": "Item not found"})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to fetch item"})
		return
	}
	if herr := authorizeItemRead(c, &item); herr != nil {
		c.JSON(herr.status, herr.body)
		return
	}

	if !isGitBacked(&item) {
		// Not an error: the item simply has no Git history. Saying so with the
		// backend marker lets a client fall back to /items/{id}/versions without
		// having to interpret a 404 that also means "no such item".
		c.JSON(http.StatusOK, GitHistoryResponse{
			Revisions:      []GitRevisionResponse{},
			HistoryBackend: contentBackendDB,
		})
		return
	}

	// limit+1 answers hasMore without a second COUNT query.
	query := db.Model(&models.CapabilityItemGitRevision{}).Where("item_id = ?", item.ID)
	if beforeRevision > 0 {
		query = query.Where("revision_no < ?", beforeRevision)
	}
	var revisions []models.CapabilityItemGitRevision
	if err := query.Order("revision_no DESC").Limit(limit + 1).Find(&revisions).Error; err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to fetch item git history"})
		return
	}

	hasMore := len(revisions) > limit
	if hasMore {
		revisions = revisions[:limit]
	}
	resp := GitHistoryResponse{
		Revisions:      make([]GitRevisionResponse, 0, len(revisions)),
		HasMore:        hasMore,
		HistoryBackend: contentBackendGit,
	}
	for _, revision := range revisions {
		resp.Revisions = append(resp.Revisions, newGitRevisionResponse(revision))
	}
	if hasMore && len(revisions) > 0 {
		resp.NextBeforeRevision = revisions[len(revisions)-1].RevisionNo
	}
	c.JSON(http.StatusOK, resp)
}

func newGitRevisionResponse(revision models.CapabilityItemGitRevision) GitRevisionResponse {
	short := shortGitSHA(revision.GitSHA)
	version := strings.TrimSpace(revision.VersionLabel)
	if version == "" {
		// The stored label is legitimately empty whenever the manifest declares
		// no version, so the fallback belongs here rather than in the writer:
		// storing a synthesized value would make "the manifest said 1.0.0"
		// indistinguishable from "we made this up", forever.
		version = short
	}
	return GitRevisionResponse{
		RevisionNo: revision.RevisionNo,
		GitSHA:     revision.GitSHA,
		ShortSHA:   short,
		Version:    version,
		ObservedAt: revision.ObservedAt.UTC().Format("2006-01-02T15:04:05Z07:00"),
		Source:     revision.Source,
	}
}

func shortGitSHA(sha string) string {
	trimmed := strings.TrimSpace(sha)
	if len(trimmed) <= gitHistoryShortSHALength {
		return trimmed
	}
	return trimmed[:gitHistoryShortSHALength]
}

// parseGitHistoryLimit clamps rather than rejects an oversized page, and treats
// an absent/blank value as the default. A value that is not a positive integer
// is a client bug worth reporting.
func parseGitHistoryLimit(raw string) (int, bool) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return gitHistoryDefaultLimit, true
	}
	value, err := strconv.Atoi(trimmed)
	if err != nil || value <= 0 {
		return 0, false
	}
	if value > gitHistoryMaxLimit {
		return gitHistoryMaxLimit, true
	}
	return value, true
}

// parseGitHistoryCursor returns 0 for "no cursor". A malformed cursor is
// rejected rather than ignored: silently serving page 1 would loop a paging
// client forever instead of telling it its cursor is wrong.
func parseGitHistoryCursor(raw string) (int64, bool) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return 0, true
	}
	value, err := strconv.ParseInt(trimmed, 10, 64)
	if err != nil || value <= 0 {
		return 0, false
	}
	return value, true
}
