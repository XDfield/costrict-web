package services

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
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

type cachedRelease struct {
	version string
	assets  map[string]giteeAsset // key: platform
	expires time.Time
}

type giteeRelease struct {
	TagName string       `json:"tag_name"`
	Assets  []giteeAsset `json:"assets"`
}

type giteeAsset struct {
	Name               string `json:"name"`
	BrowserDownloadURL string `json:"browser_download_url"`
	Size               int64  `json:"size"`
}

type MulticaUpdateService struct {
	DB    *gorm.DB
	cache sync.Map // key: "latest", value: cachedRelease
}

const multicaCacheTTL = 5 * time.Minute

func (s *MulticaUpdateService) CheckForUpdate(req MulticaUpdateCheckRequest) (*MulticaUpdateCheckResponse, error) {
	release, err := s.fetchLatestRelease()
	if err != nil {
		return nil, err
	}

	asset, ok := release.assets[req.Platform]
	if !ok {
		return &MulticaUpdateCheckResponse{CanUpdate: false}, nil
	}

	canUpdate := isNewer(release.version, req.Version)

	return &MulticaUpdateCheckResponse{
		CanUpdate:   canUpdate,
		Version:     release.version,
		DownloadURL: asset.BrowserDownloadURL,
		BinarySize:  asset.Size,
		ReleaseDate: time.Now().Format(time.RFC3339),
	}, nil
}

func (s *MulticaUpdateService) fetchLatestRelease() (*cachedRelease, error) {
	// 1. 检查缓存
	if v, ok := s.cache.Load("latest"); ok {
		cached := v.(cachedRelease)
		if time.Now().Before(cached.expires) {
			return &cached, nil
		}
		s.cache.Delete("latest")
	}

	// 2. 读取配置
	var source models.MulticaSource
	if err := s.DB.Where("name = ?", "default").First(&source).Error; err != nil {
		return nil, fmt.Errorf("multica source not configured: %w", err)
	}

	// 3. 调用 gitee API
	owner, repo := parseRepoURL(source.RepoURL)
	apiURL := fmt.Sprintf("https://gitee.com/api/v5/repos/%s/%s/releases/latest", owner, repo)

	resp, err := http.Get(apiURL)
	if err != nil {
		return nil, fmt.Errorf("failed to fetch gitee release: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("gitee API returned %d", resp.StatusCode)
	}

	var gr giteeRelease
	if err := json.NewDecoder(resp.Body).Decode(&gr); err != nil {
		return nil, fmt.Errorf("failed to decode gitee response: %w", err)
	}

	// 4. 解析 version（去掉 tag_name 的 v 前缀）
	version := strings.TrimPrefix(gr.TagName, "v")

	// 5. 按 platform 索引 assets
	assets := make(map[string]giteeAsset, len(gr.Assets))
	for _, a := range gr.Assets {
		plat := extractPlatformFromAssetName(a.Name, version)
		if plat != "" {
			assets[plat] = a
		}
	}

	cached := cachedRelease{
		version: version,
		assets:  assets,
		expires: time.Now().Add(multicaCacheTTL),
	}
	s.cache.Store("latest", cached)
	return &cached, nil
}

func parseRepoURL(repoURL string) (owner, repo string) {
	parts := strings.Split(strings.TrimSuffix(repoURL, "/"), "/")
	if len(parts) >= 2 {
		return parts[len(parts)-2], parts[len(parts)-1]
	}
	return "", ""
}

func extractPlatformFromAssetName(name, version string) string {
	prefix := fmt.Sprintf("cs-workflow-cli-%s-", version)
	if !strings.HasPrefix(name, prefix) {
		return ""
	}
	rest := strings.TrimPrefix(name, prefix)
	if idx := strings.Index(rest, "."); idx != -1 {
		return rest[:idx]
	}
	return ""
}
