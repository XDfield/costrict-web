package handlers

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"strconv"
	"time"

	"github.com/costrict/costrict-web/server/internal/database"
	"github.com/costrict/costrict-web/server/internal/middleware"
	"github.com/costrict/costrict-web/server/internal/models"
	"github.com/costrict/costrict-web/server/internal/storage"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"gorm.io/gorm"
)

var StorageBackend storage.Backend

// UploadArtifact godoc
// @Summary      Upload artifact
// @Description  Upload a file artifact for a skill item
// @Tags         artifacts
// @Accept       multipart/form-data
// @Produce      json
// @Param        file        formData  file    true   "File to upload"
// @Param        item_id     formData  string  true   "Item ID"
// @Param        version     formData  string  false  "Artifact version"
// @Param        uploaded_by formData  string  false  "Uploader user ID"
// @Success      201         {object}  models.SkillArtifact
// @Failure      400         {object}  object{error=string}
// @Failure      404         {object}  object{error=string}
// @Failure      500         {object}  object{error=string}
// @Router       /artifacts/upload [post]
func UploadArtifact(c *gin.Context) {
	file, header, err := c.Request.FormFile("file")
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Failed to read file"})
		return
	}
	defer file.Close()

	itemID := c.PostForm("item_id")
	version := c.PostForm("version")

	userIDVal, _ := c.Get(middleware.UserIDKey)
	uploadedBy, _ := userIDVal.(string)
	if uploadedBy == "" {
		uploadedBy = c.PostForm("uploaded_by")
	}

	db := database.GetDB()
	var item models.CapabilityItem
	if result := db.Preload("Registry").First(&item, "id = ?", itemID); result.Error != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Item not found"})
		return
	}

	orgID := ""
	if item.Registry != nil {
		orgID = item.Registry.OrgID
	}

	filename := filepath.Base(header.Filename)
	var storageKey string
	if orgID != "" {
		storageKey = fmt.Sprintf("%s/%s/v%s/%s", orgID, itemID, version, filename)
	} else {
		storageKey = fmt.Sprintf("%s/v%s/%s", itemID, version, filename)
	}
	fileSize := header.Size

	hasher := sha256.New()
	tee := io.TeeReader(file, hasher)

	ctx := context.Background()
	if err := StorageBackend.Put(ctx, storageKey, tee, fileSize); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to store file"})
		return
	}

	checksum := hex.EncodeToString(hasher.Sum(nil))

	var artifact models.CapabilityArtifact
	err = db.Transaction(func(tx *gorm.DB) error {
		if err := tx.Model(&models.CapabilityArtifact{}).Where("item_id = ?", itemID).Update("is_latest", false).Error; err != nil {
			return err
		}
		artifact = models.CapabilityArtifact{
			ID:              uuid.New().String(),
			ItemID:          itemID,
			Filename:        filename,
			FileSize:        fileSize,
			ChecksumSHA256:  checksum,
			MimeType:        header.Header.Get("Content-Type"),
			StorageBackend:  "local",
			StorageKey:      storageKey,
			ArtifactVersion: version,
			IsLatest:        true,
			UploadedBy:      uploadedBy,
			CreatedAt:       time.Now(),
		}
		return tx.Create(&artifact).Error
	})
	if err != nil {
		StorageBackend.Delete(context.Background(), storageKey)
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to create artifact record"})
		return
	}

	c.JSON(http.StatusCreated, artifact)
}

// DownloadArtifact godoc
// @Summary      Download artifact
// @Description  Download a file artifact by ID
// @Tags         artifacts
// @Produce      application/octet-stream
// @Param        id   path      string  true  "Artifact ID"
// @Success      200  {file}    binary
// @Failure      404  {object}  object{error=string}
// @Failure      500  {object}  object{error=string}
// @Router       /artifacts/{id}/download [get]
func DownloadArtifact(c *gin.Context) {
	id := c.Param("id")
	db := database.GetDB()
	var artifact models.CapabilityArtifact
	if result := db.First(&artifact, "id = ?", id); result.Error != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Artifact not found"})
		return
	}

	ctx := context.Background()
	reader, _, err := StorageBackend.Get(ctx, artifact.StorageKey)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to retrieve file"})
		return
	}
	defer reader.Close()

	c.Header("Content-Disposition", fmt.Sprintf("attachment; filename=\"%s\"", artifact.Filename))
	c.Header("X-Checksum-SHA256", artifact.ChecksumSHA256)
	c.Header("Content-Type", "application/octet-stream")
	if artifact.FileSize > 0 {
		c.Header("Content-Length", strconv.FormatInt(artifact.FileSize, 10))
	}

	go func() {
		db.Model(&models.CapabilityArtifact{}).Where("id = ?", id).UpdateColumn("download_count", gorm.Expr("download_count + 1"))
	}()

	io.Copy(c.Writer, reader)
}

// ListArtifacts godoc
// @Summary      List item artifacts
// @Description  Get all artifacts for a skill item
// @Tags         artifacts
// @Produce      json
// @Param        id   path      string  true  "Item ID"
// @Success      200  {object}  object{artifacts=[]models.SkillArtifact}
// @Failure      500  {object}  object{error=string}
// @Router       /items/{id}/artifacts [get]
func ListArtifacts(c *gin.Context) {
	id := c.Param("id")
	db := database.GetDB()
	var artifacts []models.CapabilityArtifact
	result := db.Where("item_id = ?", id).Order("created_at DESC").Find(&artifacts)
	if result.Error != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to fetch artifacts"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"artifacts": artifacts})
}

// DeleteArtifact godoc
// @Summary      Delete artifact
// @Description  Delete an artifact by ID and remove its stored file
// @Tags         artifacts
// @Produce      json
// @Param        id   path      string  true  "Artifact ID"
// @Success      200  {object}  object{message=string}
// @Failure      404  {object}  object{error=string}
// @Failure      500  {object}  object{error=string}
// @Router       /artifacts/{id} [delete]
func DeleteArtifact(c *gin.Context) {
	id := c.Param("id")
	db := database.GetDB()
	var artifact models.CapabilityArtifact
	if result := db.First(&artifact, "id = ?", id); result.Error != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Artifact not found"})
		return
	}

	ctx := context.Background()
	StorageBackend.Delete(ctx, artifact.StorageKey)

	if result := db.Delete(&artifact); result.Error != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to delete artifact"})
		return
	}

	c.JSON(http.StatusOK, gin.H{"message": "Artifact deleted"})
}


