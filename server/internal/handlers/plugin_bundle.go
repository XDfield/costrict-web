package handlers

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"

	"github.com/costrict/costrict-web/server/internal/database"
	"github.com/costrict/costrict-web/server/internal/middleware"
	"github.com/costrict/costrict-web/server/internal/models"
	"github.com/costrict/costrict-web/server/internal/services"
	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
)

// BundlePackSvc and BundleJobSvc back the DB+HTTP plugin distribution channel.
// They are package-level (set in cmd/api/main.go) to match the existing
// StorageBackend / JobService / ScanJobService wiring in this package, and to keep
// the bundle endpoint a plain gin.HandlerFunc consistent with DownloadPluginZip.
//
// In the API process the pack service is used ONLY for the synchronous upload-plugin
// path (asset reconstruction, no git). Catalog plugins are packed asynchronously by
// the worker process, so a nil BundlePackSvc still serves the catalog 202 + enqueue
// flow correctly.
var (
	BundlePackSvc *services.BundlePackService
	BundleJobSvc  *services.BundleJobService
)

// DownloadPluginBundle streams a plugin's lossless ZIP bundle for the DB+HTTP
// distribution channel (csc main path). Unlike DownloadPluginZip — which rebuilds a
// zip from capability_assets and therefore produces a *truncated* archive for
// catalog-ingested plugins (their assets are empty) — this endpoint serves the
// clone_pack / upload_pack artifact produced by the lazy clone-and-pack pipeline,
// which is byte-faithful to a real git clone.
//
// Behaviour:
//   - Hit: item has an IsLatest clone_pack|upload_pack artifact → stream its ZIP
//     from the StorageBackend (X-Checksum-SHA256 header, async download_count++).
//   - Miss + catalog plugin (has source_url): enqueue a BundleJob and return 202.
//   - Miss + uploaded plugin (no source_url but has assets): pack synchronously from
//     assets and stream the freshly produced ZIP.
//
// @Summary      Download plugin bundle (DB+HTTP distribution)
// @Description  Stream a plugin's lossless ZIP bundle, lazily packing on first request.
// @Tags         plugins
// @Produce      application/zip
// @Param        slug  path  string  true  "Plugin slug"
// @Success      200   {file}    binary
// @Success      202   {object}  object{status=string,message=string}
// @Failure      404   {object}  object{error=string}
// @Router       /plugins/{slug}/bundle [get]
func DownloadPluginBundle(c *gin.Context) {
	db := database.GetDB()
	slug := c.Param("slug")

	var item models.CapabilityItem
	if err := db.Where("slug = ? AND item_type = ? AND status = 'active'", slug, "plugin").First(&item).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			c.JSON(http.StatusNotFound, gin.H{"error": "Plugin not found"})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	// Visibility check (same policy as DownloadPluginZip): public repos allow
	// anonymous; private repos require membership or platform admin.
	if !bundleVisibilityOK(c, db, item.RepoID) {
		return // bundleVisibilityOK already wrote the response
	}

	// Cache hit: serve the latest packed bundle artifact directly.
	if artifact, ok := latestBundleArtifact(db, item.ID); ok {
		streamBundleArtifact(c, db, artifact)
		return
	}

	// Miss. Catalog plugin → async pack + 202. Uploaded plugin → synchronous pack.
	if item.SourceURL != "" {
		enqueueBundleJobAndAccept(c, item.ID)
		return
	}

	// Uploaded plugin (no source_url): its assets are complete, so pack now.
	if BundlePackSvc == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "Bundle service not available"})
		return
	}
	artifact, err := BundlePackSvc.PackUploadBundle(c.Request.Context(), &item)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to pack plugin bundle"})
		return
	}
	streamBundleArtifact(c, db, artifact)
}

// bundleVisibilityOK enforces the same access policy as DownloadPluginZip. It
// writes the error response itself and returns false on denial.
func bundleVisibilityOK(c *gin.Context, db *gorm.DB, repoID string) bool {
	visibility := getRepoVisibility(repoID)
	if visibility == "public" {
		return true
	}
	userID := c.GetString(middleware.UserIDKey)
	if userID == "" {
		c.JSON(http.StatusUnauthorized, gin.H{"error": "Authentication required"})
		return false
	}
	if callerIsPlatformAdmin(c, db) {
		return true
	}
	var count int64
	db.Model(&models.RepoMember{}).Where("repo_id = ? AND user_id = ?", repoID, userID).Count(&count)
	if count == 0 {
		c.JSON(http.StatusForbidden, gin.H{"error": "You don't have access to this plugin"})
		return false
	}
	return true
}

// latestBundleArtifact returns the item's current IsLatest bundle artifact
// (clone_pack or upload_pack), if any.
func latestBundleArtifact(db *gorm.DB, itemID string) (*models.CapabilityArtifact, bool) {
	var artifact models.CapabilityArtifact
	err := db.Where("item_id = ? AND is_latest = ? AND source_type IN ?", itemID, true, services.BundleSourceTypes).
		Order("created_at DESC").
		First(&artifact).Error
	if err != nil {
		return nil, false
	}
	return &artifact, true
}

// latestBundleArtifactFrom picks the IsLatest clone_pack|upload_pack artifact out of
// an already-loaded slice (e.g. GetItem's Preload("Artifacts")), avoiding an extra
// query in buildItemResponse. Returns the newest IsLatest bundle artifact if any.
func latestBundleArtifactFrom(artifacts []models.CapabilityArtifact) (*models.CapabilityArtifact, bool) {
	var best *models.CapabilityArtifact
	for i := range artifacts {
		a := &artifacts[i]
		if !a.IsLatest {
			continue
		}
		if a.SourceType != services.BundleSourceTypeClonePack && a.SourceType != services.BundleSourceTypeUploadPack {
			continue
		}
		if best == nil || a.CreatedAt.After(best.CreatedAt) {
			best = a
		}
	}
	if best == nil {
		return nil, false
	}
	return best, true
}

// streamBundleArtifact streams the artifact's stored ZIP, mirroring DownloadArtifact
// (checksum header + async download_count++) but with the ZIP content type.
func streamBundleArtifact(c *gin.Context, db *gorm.DB, artifact *models.CapabilityArtifact) {
	if StorageBackend == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "Storage backend not available"})
		return
	}
	reader, _, err := StorageBackend.Get(c.Request.Context(), artifact.StorageKey)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to retrieve bundle"})
		return
	}
	defer reader.Close()

	c.Header("Content-Type", "application/zip")
	c.Header("Content-Disposition", fmt.Sprintf("attachment; filename=\"%s\"", artifact.Filename))
	c.Header("X-Checksum-SHA256", artifact.ChecksumSHA256)
	c.Header("X-Bundle-Version", artifact.ArtifactVersion)
	if artifact.FileSize > 0 {
		c.Header("Content-Length", strconv.FormatInt(artifact.FileSize, 10))
	}

	artifactID := artifact.ID
	go func() {
		db.Model(&models.CapabilityArtifact{}).Where("id = ?", artifactID).
			UpdateColumn("download_count", gorm.Expr("download_count + 1"))
	}()

	io.Copy(c.Writer, reader)
}

// enqueueBundleJobAndAccept queues a lazy clone-and-pack job (de-duplicated against
// in-flight jobs) and returns 202 so the client can poll the bundle/item endpoint
// until the artifact appears.
func enqueueBundleJobAndAccept(c *gin.Context, itemID string) {
	if BundleJobSvc == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "Bundle service not available"})
		return
	}
	userID := c.GetString(middleware.UserIDKey)
	_, err := BundleJobSvc.Enqueue(itemID, services.BundleEnqueueOptions{
		TriggerType: "subscribe",
		TriggerUser: userID,
	})
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to queue bundle job"})
		return
	}
	c.JSON(http.StatusAccepted, gin.H{
		"status":  "packing",
		"message": "Plugin bundle is being prepared; retry shortly.",
	})
}
