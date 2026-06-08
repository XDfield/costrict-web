package services

import (
	"errors"
	"time"

	"github.com/costrict/costrict-web/server/internal/models"
	"gorm.io/gorm"
)

type MulticaUpdateCheckRequest struct {
	Platform string `form:"platform" binding:"required"`
	Version  string `form:"version" binding:"required"`
}

type MulticaUpdateCheckResponse struct {
	CanUpdate     bool   `json:"can_update"`
	Version       string `json:"version"`
	Changelog     string `json:"changelog,omitempty"`
	DownloadURL   string `json:"download_url,omitempty"`
	SHA256        string `json:"sha256,omitempty"`
	Force         bool   `json:"force,omitempty"`
	MinClientVer  string `json:"min_client_version,omitempty"`
	ReleaseDate   string `json:"release_date,omitempty"`
	BinarySize    int64  `json:"size,omitempty"`
}

type MulticaUpdateService struct {
	DB *gorm.DB
}

func (s *MulticaUpdateService) CheckForUpdate(req MulticaUpdateCheckRequest) (*MulticaUpdateCheckResponse, error) {
	var release models.MulticaRelease
	err := s.DB.
		Where("platform = ? AND channel = 'stable'", req.Platform).
		Order("created_at DESC").
		First(&release).Error
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return &MulticaUpdateCheckResponse{CanUpdate: false}, nil
		}
		return nil, err
	}

	canUpdate := isNewer(release.Version, req.Version)

	return &MulticaUpdateCheckResponse{
		CanUpdate:    canUpdate,
		Version:      release.Version,
		Changelog:    release.Changelog,
		DownloadURL:  release.DownloadURL,
		SHA256:       release.SHA256,
		Force:        release.Force,
		MinClientVer: release.MinClientVer,
		ReleaseDate:  release.CreatedAt.Format(time.RFC3339),
		BinarySize:   release.BinarySize,
	}, nil
}
