package adminimport

import (
	"errors"
	"net/http"
	"strconv"
	"strings"

	appmiddleware "github.com/costrict/costrict-web/server/internal/middleware"
	"github.com/gin-gonic/gin"
)

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

type createJobRequest struct {
	SourceURL string `json:"sourceUrl"`
	Reparse   bool   `json:"reparse"`
}

// CreateImportJobHandler godoc
//
//	@Summary		Create catalog import job (admin)
//	@Description	Submit a catalog bundle for dry-run import. Two mutually exclusive submit modes: JSON {sourceUrl,reparse} (preferred) or multipart file upload (field "file", optional form "reparse"). Returns 202 with a pending jobId; the leader-elected runner executes the dry-run asynchronously.
//	@Tags			admin/import
//	@Accept			json,multipart/form-data
//	@Produce		json
//	@Security		BearerAuth
//	@Success		202	{object}	object{jobId=string,status=string}
//	@Failure		400	{object}	object{error=string}
//	@Failure		401	{object}	object{error=string}
//	@Failure		413	{object}	object{error=string}
//	@Failure		500	{object}	object{error=string}
//	@Router			/admin/import-jobs [post]
func (m *Module) CreateImportJobHandler() gin.HandlerFunc {
	return func(c *gin.Context) {
		user := c.GetString(appmiddleware.UserIDKey)
		if user == "" {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "Authentication required"})
			return
		}

		if strings.HasPrefix(c.ContentType(), "multipart/form-data") {
			c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, MaxCatalogBundleUploadSize)
			file, header, err := c.Request.FormFile("file")
			if err != nil {
				var maxErr *http.MaxBytesError
				if errors.As(err, &maxErr) {
					c.JSON(http.StatusRequestEntityTooLarge, gin.H{"error": "bundle exceeds maximum size"})
					return
				}
				c.JSON(http.StatusBadRequest, gin.H{"error": "failed to read uploaded file"})
				return
			}
			defer file.Close()
			if header.Size <= 0 {
				c.JSON(http.StatusBadRequest, gin.H{"error": "uploaded file is empty"})
				return
			}
			reparse := c.PostForm("reparse") == "true"
			job, err := m.svc.CreateUploadJob(c.Request.Context(), file, header.Size, header.Filename, reparse, user)
			if err != nil {
				c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to create import job"})
				return
			}
			c.JSON(http.StatusAccepted, gin.H{"jobId": job.ID, "status": job.Status})
			return
		}

		var req createJobRequest
		if err := c.ShouldBindJSON(&req); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request body"})
			return
		}
		job, err := m.svc.CreateURLJob(req.SourceURL, req.Reparse, user)
		if err != nil {
			switch {
			case errors.Is(err, ErrEmptySource), errors.Is(err, ErrInvalidURL):
				c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			default:
				c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to create import job"})
			}
			return
		}
		c.JSON(http.StatusAccepted, gin.H{"jobId": job.ID, "status": job.Status})
	}
}

// GetImportJobHandler godoc
//
//	@Summary		Get import job (admin)
//	@Description	Poll an import job's status and dry-run/import result (platform admin only).
//	@Tags			admin/import
//	@Produce		json
//	@Security		BearerAuth
//	@Param			id	path		string	true	"Job id"
//	@Success		200	{object}	object
//	@Failure		404	{object}	object{error=string}
//	@Failure		500	{object}	object{error=string}
//	@Router			/admin/import-jobs/{id} [get]
func (m *Module) GetImportJobHandler() gin.HandlerFunc {
	return func(c *gin.Context) {
		job, err := m.svc.GetJob(c.Param("id"))
		if err != nil {
			if errors.Is(err, ErrJobNotFound) {
				c.JSON(http.StatusNotFound, gin.H{"error": "import job not found"})
				return
			}
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to load import job"})
			return
		}
		c.JSON(http.StatusOK, job)
	}
}

// ListImportJobsHandler godoc
//
//	@Summary		List import jobs (admin)
//	@Description	Paginated import history, newest first (platform admin only).
//	@Tags			admin/import
//	@Produce		json
//	@Security		BearerAuth
//	@Param			page		query		int	false	"Page number (1-based)"
//	@Param			pageSize	query		int	false	"Page size (default 20, max 100)"
//	@Success		200			{object}	object{items=[]object,total=int,page=int,pageSize=int}
//	@Failure		500			{object}	object{error=string}
//	@Router			/admin/import-jobs [get]
func (m *Module) ListImportJobsHandler() gin.HandlerFunc {
	return func(c *gin.Context) {
		page := atoiDefault(c.Query("page"), 1)
		pageSize := atoiDefault(c.Query("pageSize"), 20)
		jobs, total, err := m.svc.ListJobs(page, pageSize)
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to list import jobs"})
			return
		}
		c.JSON(http.StatusOK, gin.H{"items": jobs, "total": total, "page": page, "pageSize": pageSize})
	}
}

type confirmRequest struct {
	ConfirmLargeDelete bool `json:"confirmLargeDelete"`
}

// ConfirmImportJobHandler godoc
//
//	@Summary		Confirm import job (admin)
//	@Description	Promote a previewed job to the real-import phase. Rejects when the dry-run had failures, or when the delete count exceeds threshold and confirmLargeDelete is not set. Returns 202; the runner executes asynchronously.
//	@Tags			admin/import
//	@Accept			json
//	@Produce		json
//	@Security		BearerAuth
//	@Param			id		path		string							true	"Job id"
//	@Param			body	body		object{confirmLargeDelete=bool}	false	"Confirmation flags"
//	@Success		202		{object}	object{status=string}
//	@Failure		400		{object}	object{error=string}
//	@Failure		401		{object}	object{error=string}
//	@Failure		404		{object}	object{error=string}
//	@Failure		409		{object}	object{error=string}
//	@Failure		500		{object}	object{error=string}
//	@Router			/admin/import-jobs/{id}/confirm [post]
func (m *Module) ConfirmImportJobHandler() gin.HandlerFunc {
	return func(c *gin.Context) {
		user := c.GetString(appmiddleware.UserIDKey)
		if user == "" {
			c.JSON(http.StatusUnauthorized, gin.H{"error": "Authentication required"})
			return
		}
		var req confirmRequest
		_ = c.ShouldBindJSON(&req) // body is optional

		err := m.svc.ConfirmJob(c.Param("id"), req.ConfirmLargeDelete)
		if err != nil {
			switch {
			case errors.Is(err, ErrJobNotFound):
				c.JSON(http.StatusNotFound, gin.H{"error": "import job not found"})
			case errors.Is(err, ErrNotPreviewed):
				c.JSON(http.StatusConflict, gin.H{"error": "job is not awaiting confirmation"})
			case errors.Is(err, ErrPreviewHasFailures):
				c.JSON(http.StatusBadRequest, gin.H{"error": "dry-run reported failed entries; cannot confirm"})
			case errors.Is(err, ErrLargeDeleteUnconfirmed):
				c.JSON(http.StatusConflict, gin.H{"error": "delete count exceeds threshold", "code": "large_delete_unconfirmed"})
			default:
				c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to confirm import job"})
			}
			return
		}
		c.JSON(http.StatusAccepted, gin.H{"status": "pending"})
	}
}

// ImportJobErrorsLogHandler godoc
//
//	@Summary		Download import errors log (admin)
//	@Description	Download the job's errors + incompleteErrors as a plain-text attachment (platform admin only).
//	@Tags			admin/import
//	@Produce		plain
//	@Security		BearerAuth
//	@Param			id	path	string	true	"Job id"
//	@Success		200	{string}	string	"plain text log"
//	@Failure		404	{object}	object{error=string}
//	@Failure		500	{object}	object{error=string}
//	@Router			/admin/import-jobs/{id}/errors.log [get]
func (m *Module) ImportJobErrorsLogHandler() gin.HandlerFunc {
	return func(c *gin.Context) {
		text, err := m.svc.ErrorsLog(c.Param("id"))
		if err != nil {
			if errors.Is(err, ErrJobNotFound) {
				c.JSON(http.StatusNotFound, gin.H{"error": "import job not found"})
				return
			}
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to render errors log"})
			return
		}
		c.Header("Content-Disposition", "attachment; filename=import-errors.log")
		c.Data(http.StatusOK, "text/plain; charset=utf-8", []byte(text))
	}
}

// ImportStatsHandler godoc
//
//	@Summary		Import inventory stats (admin)
//	@Description	Current active inventory grouped by item_type, plus total (platform admin only).
//	@Tags			admin/import
//	@Produce		json
//	@Security		BearerAuth
//	@Success		200	{object}	object{byType=[]object,total=int}
//	@Failure		500	{object}	object{error=string}
//	@Router			/admin/import-stats [get]
func (m *Module) ImportStatsHandler() gin.HandlerFunc {
	return func(c *gin.Context) {
		rows, total, err := m.svc.Stats()
		if err != nil {
			c.JSON(http.StatusInternalServerError, gin.H{"error": "failed to load import stats"})
			return
		}
		c.JSON(http.StatusOK, gin.H{"byType": rows, "total": total})
	}
}
