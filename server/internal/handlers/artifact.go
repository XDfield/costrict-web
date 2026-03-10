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
	"github.com/costrict/costrict-web/server/internal/models"
	"github.com/costrict/costrict-web/server/internal/storage"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"gorm.io/gorm"
)

var StorageBackend storage.Backend

func UploadArtifact(c *gin.Context) {
	file, header, err := c.Request.FormFile("file")
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Failed to read file"})
		return
	}
	defer file.Close()

	itemID := c.PostForm("item_id")
	version := c.PostForm("version")
	uploadedBy := c.PostForm("description")

	db := database.GetDB()
	var item models.SkillItem
	if result := db.First(&item, "id = ?", itemID); result.Error != nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "Item not found"})
		return
	}

	hasher := sha256.New()
	tee := io.TeeReader(file, hasher)

	content, err := io.ReadAll(tee)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to read file content"})
		return
	}

	checksum := hex.EncodeToString(hasher.Sum(nil))
	filename := filepath.Base(header.Filename)
	storageKey := fmt.Sprintf("%s/v%s/%s", itemID, version, filename)
	fileSize := int64(len(content))

	ctx := context.Background()
	if err := StorageBackend.Put(ctx, storageKey, io.NopCloser(
		newBytesReader(content),
	), fileSize); err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to store file"})
		return
	}

	db.Model(&models.SkillArtifact{}).Where("item_id = ?", itemID).Update("is_latest", false)

	artifact := models.SkillArtifact{
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

	if result := db.Create(&artifact); result.Error != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to create artifact record"})
		return
	}

	c.JSON(http.StatusCreated, artifact)
}

func DownloadArtifact(c *gin.Context) {
	id := c.Param("id")
	db := database.GetDB()
	var artifact models.SkillArtifact
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
		db.Model(&models.SkillArtifact{}).Where("id = ?", id).UpdateColumn("download_count", gorm.Expr("download_count + 1"))
	}()

	io.Copy(c.Writer, reader)
}

func ListArtifacts(c *gin.Context) {
	id := c.Param("id")
	db := database.GetDB()
	var artifacts []models.SkillArtifact
	result := db.Where("item_id = ?", id).Order("created_at DESC").Find(&artifacts)
	if result.Error != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to fetch artifacts"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"artifacts": artifacts})
}

func DeleteArtifact(c *gin.Context) {
	id := c.Param("id")
	db := database.GetDB()
	var artifact models.SkillArtifact
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

type bytesReader struct {
	data []byte
	pos  int
}

func newBytesReader(data []byte) *bytesReader {
	return &bytesReader{data: data}
}

func (r *bytesReader) Read(p []byte) (n int, err error) {
	if r.pos >= len(r.data) {
		return 0, io.EOF
	}
	n = copy(p, r.data[r.pos:])
	r.pos += n
	return n, nil
}
