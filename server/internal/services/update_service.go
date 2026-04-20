package services

import (
	"errors"
	"time"

	"github.com/costrict/costrict-web/server/internal/models"
	"golang.org/x/mod/semver"
	"gorm.io/gorm"
)

var (
	ErrReleaseNotFound = errors.New("release not found")
)

type UpdateService struct {
	DB                 *gorm.DB
	ReleaseDownloadURL string
}

type PlatformAsset struct {
	Platform    string `json:"platform" binding:"required"`
	DownloadURL string `json:"downloadUrl" binding:"required"`
	SHA256      string `json:"sha256" binding:"required"`
	BinarySize  int64  `json:"binarySize" binding:"required"`
}

type CreateReleaseRequest struct {
	Version      string           `json:"version" binding:"required"`
	Assets       []PlatformAsset  `json:"assets" binding:"required,min=1"`
	Changelog    string          `json:"changelog"`
	Force        bool            `json:"force"`
	MinClientVer string          `json:"minClientVersion"`
	Channel      string          `json:"channel"`
}

type UpdateCheckRequest struct {
	Platform string `form:"platform" binding:"required"`
	Version  string `form:"version" binding:"required"`
}

type UpdateCheckResponse struct {
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

func (s *UpdateService) CheckForUpdate(req UpdateCheckRequest) (*UpdateCheckResponse, error) {
	var release models.DeviceRelease
	err := s.DB.
		Where("platform = ? AND channel = 'stable'", req.Platform).
		Order("created_at DESC").
		First(&release).Error
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return &UpdateCheckResponse{CanUpdate: false}, nil
		}
		return nil, err
	}

	canUpdate := isNewer(release.Version, req.Version)

	downloadURL := release.DownloadURL
	if s.ReleaseDownloadURL != "" && downloadURL == "" {
		downloadURL = s.ReleaseDownloadURL + "/" + release.Platform + "/cs-cloud-" + release.Platform
	}

	return &UpdateCheckResponse{
		CanUpdate:    canUpdate,
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

func (s *UpdateService) CreateRelease(userID string, req CreateReleaseRequest) ([]models.DeviceRelease, error) {
	channel := req.Channel
	if channel == "" {
		channel = "stable"
	}

	releases := make([]models.DeviceRelease, 0, len(req.Assets))
	for _, asset := range req.Assets {
		release := models.DeviceRelease{
			Version:      req.Version,
			Platform:     asset.Platform,
			Changelog:    req.Changelog,
			DownloadURL:  asset.DownloadURL,
			SHA256:       asset.SHA256,
			BinarySize:   asset.BinarySize,
			Force:        req.Force,
			MinClientVer: req.MinClientVer,
			Channel:      channel,
			CreatedBy:    userID,
		}
		releases = append(releases, release)
	}

	if err := s.DB.Create(&releases).Error; err != nil {
		return nil, err
	}
	return releases, nil
}

func isNewer(candidate, current string) bool {
	cv := normalizeSemver(candidate)
	cur := normalizeSemver(current)
	if cur == "" {
		return cv != ""
	}
	return semver.Compare(cv, cur) > 0
}

func normalizeSemver(v string) string {
	if v == "" || v == "dev" || v == "0.0.0" {
		return ""
	}
	if v[0] != 'v' {
		return "v" + v
	}
	return v
}
