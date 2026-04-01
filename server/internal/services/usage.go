package services

import (
	"fmt"
	"log"
)

type UsageService struct{}

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
	Reports    []UsageReportItem `json:"reports" binding:"required,min=1,max=500"`
	DeviceID   string            `json:"device_id"`
	ReportedAt string            `json:"reported_at"`
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

func (s *UsageService) BatchUpsert(userID string, req UsageReportRequest) UsageReportResponse {
	result := UsageReportResponse{}
	for i, item := range req.Reports {
		if item.SessionID == "" || item.MessageID == "" || item.ModelID == "" || item.GitRepoURL == "" {
			result.Skipped++
			result.Errors = append(result.Errors, fmt.Sprintf("reports[%d]: missing required fields", i))
			continue
		}
		if item.Rounds < 1 {
			result.Skipped++
			result.Errors = append(result.Errors, fmt.Sprintf("reports[%d]: rounds must be >= 1", i))
			continue
		}
		log.Printf("[usage.report] user=%s device=%s request=%s session=%s repo=%s model=%s provider=%s cost=%f input=%d output=%d reasoning=%d cache_read=%d cache_write=%d updated=%s date=%s message=%s", userID, req.DeviceID, item.RequestID, item.SessionID, item.GitRepoURL, item.ModelID, item.ProviderID, item.Cost, item.InputTokens, item.OutputTokens, item.ReasoningTokens, item.CacheReadTokens, item.CacheWriteTokens, item.Updated, item.Date, item.MessageID)
		result.Accepted++
	}
	return result
}

func (s *UsageService) GetActivity(gitRepoURL string, days int, names map[string]string) (*UsageActivityResponse, error) {
	_ = gitRepoURL
	_ = days
	_ = names
	return nil, nil
}
