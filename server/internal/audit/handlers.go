package audit

import (
	"net/http"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"
)

// parseTime accepts either an RFC3339 timestamp or a YYYY-MM-DD date. A bare
// date is interpreted as the start of that UTC day. Returns nil for empty input.
func parseTime(v string) *time.Time {
	if v == "" {
		return nil
	}
	if t, err := time.Parse(time.RFC3339, v); err == nil {
		return &t
	}
	if t, err := time.Parse("2006-01-02", v); err == nil {
		return &t
	}
	return nil
}

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

// ListAuditLogsHandler godoc
//
//	@Summary		List admin audit logs
//	@Description	List management write-operation audit logs with optional filters and pagination (platform admin only)
//	@Tags			admin/audit-logs
//	@Produce		json
//	@Security		BearerAuth
//	@Param			action		query		string	false	"Filter by action (e.g. enterprise.create)"
//	@Param			actorId		query		string	false	"Filter by actor subject id"
//	@Param			targetType	query		string	false	"Filter by target type"
//	@Param			from		query		string	false	"Lower bound on created_at (RFC3339 or YYYY-MM-DD)"
//	@Param			to			query		string	false	"Upper bound on created_at (RFC3339 or YYYY-MM-DD)"
//	@Param			page		query		int		false	"Page number (1-based)"
//	@Param			pageSize	query		int		false	"Page size (default 20, max 200)"
//	@Success		200			{object}	object{logs=[]models.AdminAuditLog,total=int,page=int,pageSize=int}
//	@Failure		401			{object}	object{error=string}
//	@Failure		500			{object}	object{error=string}
//	@Router			/admin/audit-logs [get]
func ListAuditLogsHandler(svc *Service) gin.HandlerFunc {
	return func(c *gin.Context) {
		// A bare YYYY-MM-DD "to" is the start of the day; bump it to the end of
		// the day so a same-day range is inclusive of that whole day.
		to := parseTime(c.Query("to"))
		if to != nil && len(c.Query("to")) == len("2006-01-02") {
			end := to.Add(24*time.Hour - time.Nanosecond)
			to = &end
		}

		filter := Filter{
			Action:     c.Query("action"),
			ActorID:    c.Query("actorId"),
			TargetType: c.Query("targetType"),
			From:       parseTime(c.Query("from")),
			To:         to,
		}
		page := atoiDefault(c.Query("page"), 1)
		pageSize := atoiDefault(c.Query("pageSize"), 20)

		logs, total, err := svc.List(filter, page, pageSize)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to list audit logs"})
			return
		}

		c.JSON(http.StatusOK, gin.H{
			"logs":     logs,
			"total":    total,
			"page":     page,
			"pageSize": pageSize,
		})
	}
}
