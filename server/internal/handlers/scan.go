package handlers

import (
	"net/http"
	"strconv"

	"github.com/costrict/costrict-web/server/internal/database"
	"github.com/costrict/costrict-web/server/internal/middleware"
	"github.com/costrict/costrict-web/server/internal/models"
	"github.com/costrict/costrict-web/server/internal/services"
	"github.com/gin-gonic/gin"
)

// TriggerItemScan godoc
// @Summary      Trigger security scan
// @Description  Manually trigger a security scan for a capability item
// @Tags         scan
// @Produce      json
// @Param        id   path      string  true  "Item ID"
// @Success      202  {object}  object{jobId=string,status=string}
// @Failure      404  {object}  object{error=string}
// @Failure      409  {object}  object{error=string}
// @Failure      500  {object}  object{error=string}
// @Failure      503  {object}  object{error=string}
// @Router       /items/{id}/scan [post]
func TriggerItemScan(c *gin.Context) {
	itemID := c.Param("id")
	db := database.GetDB()

	var item models.CapabilityItem
	if err := db.First(&item, "id = ?", itemID).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Item not found"})
		return
	}

	svc, ok := any(ScanJobService).(*services.ScanJobService)
	if !ok || svc == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "Scan service not available"})
		return
	}

	userID, _ := c.Get(middleware.UserIDKey)
	uid, _ := userID.(string)

	var currentRevision int
	db.Model(&models.CapabilityVersion{}).
		Where("item_id = ?", itemID).
		Select("COALESCE(MAX(revision), 1)").
		Scan(&currentRevision)

	job, err := svc.Enqueue(itemID, currentRevision, "manual", uid, services.ScanEnqueueOptions{Priority: 1})
	if err != nil {
		if err == services.ErrScanJobAlreadyQueued {
			c.JSON(http.StatusConflict, gin.H{"error": "已有扫描任务在队列中，请稍后再试"})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to enqueue scan job"})
		return
	}

	c.JSON(http.StatusAccepted, gin.H{"jobId": job.ID, "status": job.Status})
}

// GetItemScanStatus godoc
// @Summary      Get scan status
// @Description  Get current scan status and latest result summary for a capability item
// @Tags         scan
// @Produce      json
// @Param        id   path      string  true  "Item ID"
// @Success      200  {object}  object{scanStatus=string,lastScannedAt=string,latestResult=object}
// @Failure      404  {object}  object{error=string}
// @Router       /items/{id}/scan-status [get]
func GetItemScanStatus(c *gin.Context) {
	itemID := c.Param("id")
	db := database.GetDB()

	var item models.CapabilityItem
	if err := db.First(&item, "id = ?", itemID).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Item not found"})
		return
	}

	resp := gin.H{
		"scanStatus": item.SecurityStatus,
	}

	if item.LastScanID != nil && *item.LastScanID != "" {
		var scan models.SecurityScan
		if err := db.First(&scan, "id = ?", *item.LastScanID).Error; err == nil {
			resp["lastScannedAt"] = scan.FinishedAt
			resp["latestResult"] = gin.H{
				"id":        scan.ID,
				"riskLevel": scan.RiskLevel,
				"verdict":   scan.Verdict,
				"summary":   scan.Summary,
				"scanModel": scan.ScanModel,
			}
		}
	}

	c.JSON(http.StatusOK, resp)
}

// ListItemScanResults godoc
// @Summary      List scan results
// @Description  Get paginated scan result history for a capability item
// @Tags         scan
// @Produce      json
// @Param        id    path      string   true   "Item ID"
// @Param        page  query     integer  false  "Page number (default: 1)"
// @Param        size  query     integer  false  "Page size (default: 10)"
// @Success      200   {object}  object{results=[]models.SecurityScan,total=integer}
// @Failure      500   {object}  object{error=string}
// @Router       /items/{id}/scan-results [get]
func ListItemScanResults(c *gin.Context) {
	itemID := c.Param("id")
	db := database.GetDB()

	page := 1
	pageSize := 10
	if p := c.Query("page"); p != "" {
		if n, err := strconv.Atoi(p); err == nil && n > 0 {
			page = n
		}
	}
	if s := c.Query("size"); s != "" {
		if n, err := strconv.Atoi(s); err == nil && n > 0 && n <= 50 {
			pageSize = n
		}
	}

	var results []models.SecurityScan
	var total int64

	db.Model(&models.SecurityScan{}).Where("item_id = ?", itemID).Count(&total)
	err := db.Where("item_id = ?", itemID).
		Order("created_at DESC").
		Offset((page - 1) * pageSize).
		Limit(pageSize).
		Find(&results).Error

	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to fetch scan results"})
		return
	}

	c.JSON(http.StatusOK, gin.H{"results": results, "total": total})
}

// GetScanResult godoc
// @Summary      Get scan result detail
// @Description  Get full scan report for a specific scan result
// @Tags         scan
// @Produce      json
// @Param        id   path      string  true  "Scan result ID"
// @Success      200  {object}  models.SecurityScan
// @Failure      404  {object}  object{error=string}
// @Router       /scan-results/{id} [get]
func GetScanResult(c *gin.Context) {
	id := c.Param("id")
	db := database.GetDB()

	var scan models.SecurityScan
	if err := db.First(&scan, "id = ?", id).Error; err != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Scan result not found"})
		return
	}

	c.JSON(http.StatusOK, scan)
}

// CancelScanJob godoc
// @Summary      Cancel scan job
// @Description  Cancel a pending scan job
// @Tags         scan
// @Produce      json
// @Param        id   path      string  true  "Scan job ID"
// @Success      200  {object}  object{message=string}
// @Failure      400  {object}  object{error=string}
// @Failure      503  {object}  object{error=string}
// @Router       /scan-jobs/{id}/cancel [post]
func CancelScanJob(c *gin.Context) {
	jobID := c.Param("id")

	svc, ok := any(ScanJobService).(*services.ScanJobService)
	if !ok || svc == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "Scan service not available"})
		return
	}

	if err := svc.Cancel(jobID); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}

	c.JSON(http.StatusOK, gin.H{"message": "Scan job cancelled"})
}
