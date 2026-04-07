package services

import (
	"errors"
	"fmt"
	"net/url"
	"path"
	"sort"
	"strings"
	"time"

	"github.com/costrict/costrict-web/server/internal/models"
	userpkg "github.com/costrict/costrict-web/server/internal/user"
)

var (
	ErrInvalidUsageReport = errors.New("invalid usage report")
	ErrInvalidRepoURL     = errors.New("invalid git repo url")
	ErrUsageQueryFailed   = errors.New("usage query failed")
)

type UsageService struct {
	provider    UsageProvider
	userService *userpkg.UserService
}

type UsageProvider interface {
	BatchUpsert(userID, deviceID, accessToken, clientVersion string, records []*models.SessionUsageReport) UsageReportResponse
	GetActivity(gitRepoURL string, days int) (*UsageActivityResponse, error)
	AggregateProjectRepoActivity(userIDs []string, repoURLs []string, days int) ([]UsageRepoUserAggregate, error)
	AggregateProjectRepoDailyActivity(userIDs []string, repoURLs []string, days int) ([]UsageRepoDailyAggregate, error)
	AggregateRepositoriesByUsers(userIDs []string, days int) ([]UsageRepoUserAggregate, error)
}

func NewUsageService(provider UsageProvider, userService *userpkg.UserService) *UsageService {
	return &UsageService{provider: provider, userService: userService}
}

type UsageReportItem struct {
	SessionID        string  `json:"session_id" binding:"required"`
	RequestID        string  `json:"request_id"`
	MessageID        string  `json:"message_id" binding:"required"`
	Date             string  `json:"date" binding:"required"`
	Updated          string  `json:"updated" binding:"required"`
	ModelID          string  `json:"model_id" binding:"required"`
	ProviderID       string  `json:"provider_id"`
	InputTokens      int64   `json:"input_tokens"`
	OutputTokens     int64   `json:"output_tokens"`
	ReasoningTokens  int64   `json:"reasoning_tokens"`
	CacheReadTokens  int64   `json:"cache_read_tokens"`
	CacheWriteTokens int64   `json:"cache_write_tokens"`
	Cost             float64 `json:"cost"`
	Rounds           int     `json:"rounds"`
	GitRepoURL       string  `json:"git_repo_url" binding:"required"`
	GitWorktree      string  `json:"git_worktree"`
}

type UsageReportRequest struct {
	Reports       []UsageReportItem `json:"reports" binding:"required,min=1,max=500"`
	DeviceID      string            `json:"device_id"`
	ReportedAt    string            `json:"reported_at"`
	ClientVersion string            `json:"client_version"`
}

type UsageReportResponse struct {
	Accepted int      `json:"accepted"`
	Skipped  int      `json:"skipped"`
	Errors   []string `json:"errors"`
}

type UsageDaily struct {
	Date     string `json:"date"`
	Requests int64  `json:"requests"`
}

type UsageUserActivity struct {
	UserID        string       `json:"user_id"`
	Username      string       `json:"username"`
	Daily         []UsageDaily `json:"daily"`
	TotalRequests int64        `json:"total_requests"`
}

type UsageActivityResponse struct {
	GitRepoURL string `json:"git_repo_url"`
	Range      struct {
		From string `json:"from"`
		To   string `json:"to"`
	} `json:"range"`
	Users []UsageUserActivity `json:"users"`
}

type UsageRepoUserAggregate struct {
	UserID          string  `json:"userId"`
	GitRepoURL      string  `json:"gitRepoUrl"`
	RequestCount    int64   `json:"requestCount"`
	FirstActiveDate string  `json:"firstActiveDate"`
	LastActiveDate  string  `json:"lastActiveDate"`
	InputTokens     int64   `json:"inputTokens"`
	OutputTokens    int64   `json:"outputTokens"`
	TotalCost       float64 `json:"totalCost"`
}

type UsageRepoDailyAggregate struct {
	GitRepoURL   string `json:"gitRepoUrl"`
	Date         string `json:"date"`
	RequestCount int64  `json:"requestCount"`
}

type usageDailyAggregateRow struct {
	UserID   string
	Date     string
	Requests int64
}

func NormalizeGitRepoURL(raw string) (string, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return "", ErrInvalidRepoURL
	}

	if strings.HasPrefix(trimmed, "git@") {
		withoutPrefix := strings.TrimPrefix(trimmed, "git@")
		parts := strings.SplitN(withoutPrefix, ":", 2)
		if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
			return "", ErrInvalidRepoURL
		}
		trimmed = "https://" + parts[0] + "/" + parts[1]
	}

	parsed, err := url.Parse(trimmed)
	if err != nil {
		return "", fmt.Errorf("%w: %v", ErrInvalidRepoURL, err)
	}
	if parsed.Host == "" && parsed.Scheme == "" {
		trimmed = "https://" + trimmed
		parsed, err = url.Parse(trimmed)
		if err != nil {
			return "", fmt.Errorf("%w: %v", ErrInvalidRepoURL, err)
		}
	}
	if parsed.Host == "" {
		return "", ErrInvalidRepoURL
	}

	repoPath := strings.TrimSuffix(parsed.Path, ".git")
	repoPath = strings.TrimSuffix(repoPath, "/")
	repoPath = path.Clean(repoPath)
	if repoPath == "." || repoPath == "/" || repoPath == "" {
		return "", ErrInvalidRepoURL
	}
	if !strings.HasPrefix(repoPath, "/") {
		repoPath = "/" + repoPath
	}

	normalized := "https://" + strings.ToLower(parsed.Host) + strings.ToLower(repoPath)
	return normalized, nil
}

func (s *UsageService) BatchUpsert(userID, accessToken string, req UsageReportRequest) UsageReportResponse {
	result := UsageReportResponse{}
	if s == nil || s.provider == nil {
		result.Errors = append(result.Errors, "usage storage is not configured")
		return result
	}

	records := make([]*models.SessionUsageReport, 0, len(req.Reports))

	for i, item := range req.Reports {
		record, err := s.buildUsageRecord(userID, req.DeviceID, item)
		if err != nil {
			result.Skipped++
			result.Errors = append(result.Errors, fmt.Sprintf("reports[%d]: %v", i, err))
			continue
		}
		records = append(records, record)
	}

	providerResult := s.provider.BatchUpsert(userID, req.DeviceID, accessToken, req.ClientVersion, records)
	providerResult.Skipped += result.Skipped
	providerResult.Errors = append(result.Errors, providerResult.Errors...)
	return providerResult
}

func (s *UsageService) GetActivity(gitRepoURL string, days int, names map[string]string) (*UsageActivityResponse, error) {
	if s == nil || s.provider == nil {
		return nil, ErrUsageQueryFailed
	}
	normalizedRepo, err := NormalizeGitRepoURL(gitRepoURL)
	if err != nil {
		return nil, err
	}
	if days < 1 || days > 90 {
		return nil, fmt.Errorf("days must be between 1 and 90")
	}

	resp, err := s.provider.GetActivity(normalizedRepo, days)
	if err != nil {
		return nil, err
	}
	userIDs := make([]string, 0, len(resp.Users))
	for _, item := range resp.Users {
		userIDs = append(userIDs, item.UserID)
	}
	resolvedNames, err := s.resolveUserNames(userIDs, names)
	if err != nil {
		return nil, err
	}
	for i := range resp.Users {
		resp.Users[i].Username = resolvedNames[resp.Users[i].UserID]
	}
	return resp, nil
}

func (s *UsageService) AggregateProjectRepoActivity(userIDs []string, repoURLs []string, days int) ([]UsageRepoUserAggregate, error) {
	if s == nil || s.provider == nil {
		return nil, ErrUsageQueryFailed
	}
	if len(userIDs) == 0 || len(repoURLs) == 0 {
		return []UsageRepoUserAggregate{}, nil
	}
	if days < 1 || days > 90 {
		return nil, fmt.Errorf("days must be between 1 and 90")
	}
	return s.provider.AggregateProjectRepoActivity(userIDs, repoURLs, days)
}

func (s *UsageService) AggregateProjectRepoDailyActivity(userIDs []string, repoURLs []string, days int) ([]UsageRepoDailyAggregate, error) {
	if s == nil || s.provider == nil {
		return nil, ErrUsageQueryFailed
	}
	if len(userIDs) == 0 || len(repoURLs) == 0 {
		return []UsageRepoDailyAggregate{}, nil
	}
	if days < 1 || days > 90 {
		return nil, fmt.Errorf("days must be between 1 and 90")
	}
	return s.provider.AggregateProjectRepoDailyActivity(userIDs, repoURLs, days)
}

func (s *UsageService) AggregateRepositoriesByUsers(userIDs []string, days int) ([]UsageRepoUserAggregate, error) {
	if s == nil || s.provider == nil {
		return nil, ErrUsageQueryFailed
	}
	if len(userIDs) == 0 {
		return []UsageRepoUserAggregate{}, nil
	}
	if days < 1 || days > 90 {
		return nil, fmt.Errorf("days must be between 1 and 90")
	}
	return s.provider.AggregateRepositoriesByUsers(userIDs, days)
}

func (s *UsageService) buildUsageRecord(userID, deviceID string, item UsageReportItem) (*models.SessionUsageReport, error) {
	if item.SessionID == "" || item.MessageID == "" || item.ModelID == "" || item.GitRepoURL == "" {
		return nil, fmt.Errorf("%w: missing required fields", ErrInvalidUsageReport)
	}
	if item.Rounds < 1 {
		return nil, fmt.Errorf("%w: rounds must be >= 1", ErrInvalidUsageReport)
	}
	recordedDate, err := parseUsageTime(item.Date)
	if err != nil {
		return nil, fmt.Errorf("%w: invalid date", ErrInvalidUsageReport)
	}
	updatedAt, err := parseUsageTime(item.Updated)
	if err != nil {
		return nil, fmt.Errorf("%w: invalid updated", ErrInvalidUsageReport)
	}
	normalizedRepo, err := NormalizeGitRepoURL(item.GitRepoURL)
	if err != nil {
		return nil, err
	}

	return &models.SessionUsageReport{
		UserID:           userID,
		DeviceID:         deviceID,
		SessionID:        item.SessionID,
		RequestID:        item.RequestID,
		MessageID:        item.MessageID,
		Date:             recordedDate,
		Updated:          updatedAt,
		ModelID:          item.ModelID,
		ProviderID:       item.ProviderID,
		InputTokens:      item.InputTokens,
		OutputTokens:     item.OutputTokens,
		ReasoningTokens:  item.ReasoningTokens,
		CacheReadTokens:  item.CacheReadTokens,
		CacheWriteTokens: item.CacheWriteTokens,
		Cost:             item.Cost,
		Rounds:           item.Rounds,
		GitRepoURL:       normalizedRepo,
		GitWorktree:      item.GitWorktree,
	}, nil
}

func (s *UsageService) resolveUserNames(userIDs []string, provided map[string]string) (map[string]string, error) {
	result := make(map[string]string, len(userIDs))
	missing := make([]string, 0, len(userIDs))
	for _, userID := range userIDs {
		if userID == "" {
			continue
		}
		if name := strings.TrimSpace(provided[userID]); name != "" {
			result[userID] = name
			continue
		}
		missing = append(missing, userID)
	}

	if len(missing) > 0 && s.userService != nil {
		users, err := s.userService.GetUsersByIDs(missing)
		if err != nil {
			return nil, err
		}
		for _, userID := range missing {
			if user, ok := users[userID]; ok && user != nil {
				displayName := user.Username
				if user.DisplayName != nil && *user.DisplayName != "" {
					displayName = *user.DisplayName
				}
				result[userID] = displayName
			}
		}
	}

	for _, userID := range userIDs {
		if _, ok := result[userID]; !ok {
			result[userID] = userID
		}
	}
	return result, nil
}

func parseUsageTime(raw string) (time.Time, error) {
	for _, layout := range []string{time.RFC3339, "2006-01-02", time.RFC3339Nano} {
		if parsed, err := time.Parse(layout, raw); err == nil {
			return parsed, nil
		}
	}
	return time.Time{}, fmt.Errorf("invalid time %q", raw)
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
