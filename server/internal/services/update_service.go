package services

import (
	"errors"
	"time"

	"github.com/costrict/costrict-web/server/internal/models"
	"gorm.io/gorm"
)

var (
	ErrReleaseNotFound = errors.New("release not found")
)

type UpdateService struct {
	DB                  *gorm.DB
	ReleaseDownloadURL  string
}

type CreateReleaseRequest struct {
	Version      string `json:"version" binding:"required"`
	Platform     string `json:"platform" binding:"required"`
	Changelog    string `json:"changelog"`
	DownloadURL  string `json:"downloadUrl" binding:"required"`
	SHA256       string `json:"sha256" binding:"required"`
	BinarySize   int64  `json:"binarySize" binding:"required"`
	Force        bool   `json:"force"`
	MinClientVer string `json:"minClientVersion"`
	Channel      string `json:"channel"`
}

type UpdateCheckRequest struct {
	Platform string `form:"platform" binding:"required"`
	Version  string `form:"version" binding:"required"`
}

type UpdateCheckResponse struct {
	Available     bool   `json:"available"`
	Version       string `json:"version"`
	Changelog     string `json:"changelog,omitempty"`
	DownloadURL   string `json:"download_url,omitempty"`
	SHA256        string `json:"sha256,omitempty"`
	Force         bool   `json:"force,omitempty"`
	MinClientVer  string `json:"min_client_version,omitempty"`
	ReleaseDate   string `json:"release_date,omitempty"`
	BinarySize    int64  `json:"size,omitempty"`
}

func (s *UpdateService) CheckForUpdate(req UpdateCheckRequest) (*UpdateCheckResponse, error) {
	var release models.DeviceRelease
	err := s.DB.
		Where("platform = ? AND channel = 'stable'", req.Platform).
		Order("created_at DESC").
		First(&release).Error
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return &UpdateCheckResponse{Available: false}, nil
		}
		return nil, err
	}

	if !isNewer(release.Version, req.Version) {
		return &UpdateCheckResponse{Available: false}, nil
	}

	downloadURL := release.DownloadURL
	if s.ReleaseDownloadURL != "" && downloadURL == "" {
		downloadURL = s.ReleaseDownloadURL + "/" + release.Platform + "/cs-cloud-" + release.Platform
	}

	return &UpdateCheckResponse{
		Available:    true,
		Version:      release.Version,
		Changelog:    release.Changelog,
		DownloadURL:  downloadURL,
		SHA256:       release.SHA256,
		Force:        release.Force,
		MinClientVer: release.MinClientVer,
		ReleaseDate:  release.CreatedAt.Format(time.RFC3339),
		BinarySize:   release.BinarySize,
	}, nil
}

func (s *UpdateService) CreateRelease(userID string, req CreateReleaseRequest) (*models.DeviceRelease, error) {
	channel := req.Channel
	if channel == "" {
		channel = "stable"
	}

	release := &models.DeviceRelease{
		Version:      req.Version,
		Platform:     req.Platform,
		Changelog:    req.Changelog,
		DownloadURL:  req.DownloadURL,
		SHA256:       req.SHA256,
		BinarySize:   req.BinarySize,
		Force:        req.Force,
		MinClientVer: req.MinClientVer,
		Channel:      channel,
		CreatedBy:    userID,
	}

	if err := s.DB.Create(release).Error; err != nil {
		return nil, err
	}
	return release, nil
}

func (s *UpdateService) ListReleases(platform string) ([]models.DeviceRelease, error) {
	var releases []models.DeviceRelease
	q := s.DB.Order("created_at DESC")
	if platform != "" {
		q = q.Where("platform = ?", platform)
	}
	if err := q.Find(&releases).Error; err != nil {
		return nil, err
	}
	return releases, nil
}

func (s *UpdateService) GetRelease(id string) (*models.DeviceRelease, error) {
	var release models.DeviceRelease
	if err := s.DB.Where("id = ?", id).First(&release).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, ErrReleaseNotFound
		}
		return nil, err
	}
	return &release, nil
}

func (s *UpdateService) DeleteRelease(id string) error {
	result := s.DB.Where("id = ?", id).Delete(&models.DeviceRelease{})
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected == 0 {
		return ErrReleaseNotFound
	}
	return nil
}

func isNewer(candidate, current string) bool {
	if candidate == current {
		return false
	}
	if current == "" || current == "dev" || current == "0.0.0" {
		return candidate != ""
	}
	return candidate > current
}
