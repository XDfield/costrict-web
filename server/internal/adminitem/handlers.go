package adminitem

import (
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/costrict/costrict-web/server/internal/audit"
	appmiddleware "github.com/costrict/costrict-web/server/internal/middleware"
	"github.com/gin-gonic/gin"
)

// maxBatchDelete bounds a single batch-delete request. The cap guards against an
// accidental "select all → delete" wiping a huge slice in one transaction and
// keeps the cascade's transaction size reasonable. The frontend's "select all
// matching" path pulls ids in pages and must respect the same ceiling.
const maxBatchDelete = 200

func atoiDefault(s string, def int) int {
	if s == "" {
		return def
	}
	v, err := strconv.Atoi(s)
	if err != nil {
		return def
	}
	return v
}

// ListItemsHandler godoc
//
//	@Summary		List items (admin)
//	@Description	Cross-registry capability-item list with type/status/security-status filters for the content-management console (platform admin only). Defaults to all statuses (active + archived).
//	@Tags			admin/items
//	@Produce		json
//	@Security		BearerAuth
//	@Param			type			query		string	false	"Exact item type filter (skill|plugin|mcp|...)"
//	@Param			status			query		string	false	"Exact status filter (active|archived); empty = all"
//	@Param			securityStatus	query		string	false	"Security risk group (unknown|low|medium|high) or exact security_status"
//	@Param			search			query		string	false	"name/description LIKE"
//	@Param			createdBy		query		string	false	"Exact author subject_id filter"
//	@Param			page			query		int		false	"Page number (1-based)"
//	@Param			pageSize		query		int		false	"Page size (default 20, max 200)"
//	@Success		200				{object}	object{items=[]object,total=int,page=int,pageSize=int}
//	@Failure		401				{object}	object{error=string}
//	@Failure		500				{object}	object{error=string}
//	@Router			/admin/items [get]
func (m *Module) ListItemsHandler() gin.HandlerFunc {
	return func(c *gin.Context) {
		page := atoiDefault(c.Query("page"), 1)
		pageSize := atoiDefault(c.Query("pageSize"), 20)

		rows, total, err := m.svc.ListItems(ListParams{
			ItemType:       c.Query("type"),
			Status:         c.Query("status"),
			SecurityStatus: c.Query("securityStatus"),
			Search:         c.Query("search"),
			CreatedBy:      c.Query("createdBy"),
			Page:           page,
			PageSize:       pageSize,
		})
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to list items"})
			return
		}

		c.JSON(http.StatusOK, gin.H{
			"items":    rows,
			"total":    total,
			"page":     page,
			"pageSize": pageSize,
		})
	}
}

type setStatusRequest struct {
	Status string `json:"status" binding:"required"`
}

// SetItemStatusHandler godoc
//
//	@Summary		Set item status (admin)
//	@Description	Take an item online/offline (active|archived) across any author (platform admin only).
//	@Tags			admin/items
//	@Accept			json
//	@Produce		json
//	@Security		BearerAuth
//	@Param			id		path		string					true	"Item id"
//	@Param			body	body		object{status=string}	true	"New status (active|archived)"
//	@Success		200		{object}	object{success=bool}
//	@Failure		400		{object}	object{error=string}
//	@Failure		401		{object}	object{error=string}
//	@Failure		404		{object}	object{error=string}
//	@Failure		500		{object}	object{error=string}
//	@Router			/admin/items/{id}/status [put]
func (m *Module) SetItemStatusHandler() gin.HandlerFunc {
	return func(c *gin.Context) {
		operatorID := c.GetString(appmiddleware.UserIDKey)
		if operatorID == "" {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "Authentication required"})
			return
		}

		id := c.Param("id")

		var req setStatusRequest
		if err := c.ShouldBindJSON(&req); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request body"})
			return
		}

		// Capture the prior status before mutating so the audit log records the
		// transition (from→to), not just the new value. Best-effort: a read
		// miss leaves "from" empty but never blocks the status change, which is
		// the authoritative path for not-found handling below.
		var fromStatus string
		if meta, err := m.svc.GetItemMeta(id); err == nil {
			fromStatus = meta.Status
		}

		if err := m.svc.SetStatus(id, req.Status); err != nil {
			switch {
			case errors.Is(err, ErrInvalidStatus):
				c.JSON(http.StatusBadRequest, gin.H{"error": "invalid status"})
			case errors.Is(err, ErrItemNotFound):
				c.JSON(http.StatusNotFound, gin.H{"error": "item not found"})
			default:
				c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to update item status"})
			}
			return
		}

		audit.Record(operatorID, audit.ActionItemStatusChange, audit.TargetItem, id, gin.H{
			"from": fromStatus,
			"to":   req.Status,
		})

		c.JSON(http.StatusOK, gin.H{"success": true})
	}
}

// DeleteItemHandler godoc
//
//	@Summary		Delete item (admin)
//	@Description	Delete any author's item and its dependent records (platform admin only).
//	@Tags			admin/items
//	@Produce		json
//	@Security		BearerAuth
//	@Param			id	path		string	true	"Item id"
//	@Success		200	{object}	object{success=bool}
//	@Failure		401	{object}	object{error=string}
//	@Failure		404	{object}	object{error=string}
//	@Failure		500	{object}	object{error=string}
//	@Router			/admin/items/{id} [delete]
func (m *Module) DeleteItemHandler() gin.HandlerFunc {
	return func(c *gin.Context) {
		operatorID := c.GetString(appmiddleware.UserIDKey)
		if operatorID == "" {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "Authentication required"})
			return
		}

		id := c.Param("id")

		// Pre-read the victim's identity before the hard delete so the audit log
		// describes what was removed rather than leaving a dangling UUID. A read
		// miss leaves the payload empty but never blocks the delete, which is the
		// authoritative not-found path below.
		var payload gin.H
		if meta, err := m.svc.GetItemMeta(id); err == nil {
			payload = gin.H{
				"createdBy": meta.CreatedBy,
				"itemType":  meta.ItemType,
				"name":      meta.Name,
			}
		}

		if err := m.svc.DeleteItem(id); err != nil {
			switch {
			case errors.Is(err, ErrItemNotFound):
				c.JSON(http.StatusNotFound, gin.H{"error": "item not found"})
			default:
				c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to delete item"})
			}
			return
		}

		audit.Record(operatorID, audit.ActionItemDelete, audit.TargetItem, id, payload)

		c.JSON(http.StatusOK, gin.H{"success": true})
	}
}

type batchDeleteRequest struct {
	IDs []string `json:"ids" binding:"required"`
}

// BatchDeleteItemsHandler godoc
//
//	@Summary		Batch delete items (admin)
//	@Description	Delete up to 200 items (any author) and their dependent records in a single transaction (platform admin only). All succeed or none do; ids that no longer exist are reported as skipped.
//	@Tags			admin/items
//	@Accept			json
//	@Produce		json
//	@Security		BearerAuth
//	@Param			body	body		object{ids=[]string}	true	"Item ids to delete"
//	@Success		200		{object}	object{success=bool,deleted=int,skipped=int,skippedIds=[]string}
//	@Failure		400		{object}	object{error=string}
//	@Failure		401		{object}	object{error=string}
//	@Failure		500		{object}	object{error=string}
//	@Router			/admin/items/batch-delete [post]
func (m *Module) BatchDeleteItemsHandler() gin.HandlerFunc {
	return func(c *gin.Context) {
		operatorID := c.GetString(appmiddleware.UserIDKey)
		if operatorID == "" {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "Authentication required"})
			return
		}

		var req batchDeleteRequest
		if err := c.ShouldBindJSON(&req); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request body"})
			return
		}

		// Normalize: trim, drop blanks, de-duplicate while preserving order so the
		// cap and the cascade both see a clean id set.
		seen := make(map[string]bool, len(req.IDs))
		ids := make([]string, 0, len(req.IDs))
		for _, raw := range req.IDs {
			id := strings.TrimSpace(raw)
			if id == "" || seen[id] {
				continue
			}
			seen[id] = true
			ids = append(ids, id)
		}
		if len(ids) == 0 {
			c.JSON(http.StatusBadRequest, gin.H{"error": "no item ids provided"})
			return
		}
		if len(ids) > maxBatchDelete {
			c.JSON(http.StatusBadRequest, gin.H{"error": fmt.Sprintf("too many items: %d (max %d)", len(ids), maxBatchDelete)})
			return
		}

		deleted, skipped, err := m.svc.BatchDeleteItems(ids)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to delete items"})
			return
		}

		// One audit record for the batch; payload lists what was actually removed
		// so the log describes the effect, not merely the request.
		audit.Record(operatorID, audit.ActionItemDelete, audit.TargetItem, "batch", gin.H{
			"requested":  len(ids),
			"deleted":    len(deleted),
			"skipped":    len(skipped),
			"deletedIds": deleted,
		})

		c.JSON(http.StatusOK, gin.H{
			"success":    true,
			"deleted":    len(deleted),
			"skipped":    len(skipped),
			"skippedIds": skipped,
		})
	}
}
