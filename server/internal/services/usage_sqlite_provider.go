package services

import (
	"fmt"
	"time"

	"github.com/costrict/costrict-web/server/internal/models"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

type SQLiteUsageProvider struct {
	db *gorm.DB
}

func NewSQLiteUsageProvider(db *gorm.DB) *SQLiteUsageProvider {
	return &SQLiteUsageProvider{db: db}
}

func (p *SQLiteUsageProvider) BatchUpsert(userID, deviceID string, records []*models.SessionUsageReport) UsageReportResponse {
	result := UsageReportResponse{}
	if p == nil || p.db == nil {
		result.Errors = append(result.Errors, "usage storage is not configured")
		result.Skipped = len(records)
		return result
	}
	for i, record := range records {
		if err := p.db.Clauses(clause.OnConflict{
			Columns: []clause.Column{{Name: "user_id"}, {Name: "session_id"}, {Name: "message_id"}},
			DoUpdates: clause.AssignmentColumns([]string{
				"device_id", "request_id", "date", "updated", "model_id", "provider_id",
				"input_tokens", "output_tokens", "reasoning_tokens", "cache_read_tokens", "cache_write_tokens",
				"cost", "rounds", "git_repo_url", "git_worktree", "updated_at",
			}),
		}).Create(record).Error; err != nil {
			result.Skipped++
			result.Errors = append(result.Errors, fmt.Sprintf("records[%d]: %v", i, err))
			continue
		}
		result.Accepted++
	}
	return result
}

func (p *SQLiteUsageProvider) GetActivity(gitRepoURL string, days int) (*UsageActivityResponse, error) {
	if p == nil || p.db == nil {
		return nil, ErrUsageQueryFailed
	}
	if days < 1 || days > 90 {
		return nil, fmt.Errorf("days must be between 1 and 90")
	}
	fromDate, toDate := usageDateRange(days)
	var rows []usageDailyAggregateRow
	if err := p.db.Model(&models.SessionUsageReport{}).
		Select("user_id, date(date) as date, COUNT(*) as requests").
		Where("git_repo_url = ?", gitRepoURL).
		Where("date(date) >= ? AND date(date) <= ?", fromDate.Format("2006-01-02"), toDate.Format("2006-01-02")).
		Group("user_id, date(date)").
		Order("user_id ASC, date ASC").
		Scan(&rows).Error; err != nil {
		return nil, fmt.Errorf("%w: %v", ErrUsageQueryFailed, err)
	}

	userMap := make(map[string]*UsageUserActivity)
	for _, row := range rows {
		entry, ok := userMap[row.UserID]
		if !ok {
			entry = &UsageUserActivity{UserID: row.UserID, Username: row.UserID}
			userMap[row.UserID] = entry
		}
		entry.Daily = append(entry.Daily, UsageDaily{Date: row.Date, Requests: row.Requests})
		entry.TotalRequests += row.Requests
	}

	userActivities := make([]UsageUserActivity, 0, len(userMap))
	for _, userID := range sortedKeys(userMap) {
		userActivities = append(userActivities, *userMap[userID])
	}

	resp := &UsageActivityResponse{GitRepoURL: gitRepoURL, Users: userActivities}
	resp.Range.From = fromDate.Format("2006-01-02")
	resp.Range.To = toDate.Format("2006-01-02")
	return resp, nil
}

func (p *SQLiteUsageProvider) AggregateProjectRepoActivity(userIDs []string, repoURLs []string, days int) ([]UsageRepoUserAggregate, error) {
	if p == nil || p.db == nil {
		return nil, ErrUsageQueryFailed
	}
	if len(userIDs) == 0 || len(repoURLs) == 0 {
		return []UsageRepoUserAggregate{}, nil
	}
	if days < 1 || days > 90 {
		return nil, fmt.Errorf("days must be between 1 and 90")
	}
	fromDate, toDate := usageDateRange(days)

	type row struct {
		UserID          string
		GitRepoURL      string
		RequestCount    int64
		FirstActiveDate string
		LastActiveDate  string
		InputTokens     int64
		OutputTokens    int64
		TotalCost       float64
	}
	var rows []row
	if err := p.db.Model(&models.SessionUsageReport{}).
		Select(`user_id, git_repo_url, COUNT(*) AS request_count, MIN(date(date)) AS first_active_date, MAX(date(date)) AS last_active_date, SUM(input_tokens) AS input_tokens, SUM(output_tokens) AS output_tokens, SUM(cost) AS total_cost`).
		Where("user_id IN ?", userIDs).
		Where("git_repo_url IN ?", repoURLs).
		Where("date(date) >= ? AND date(date) <= ?", fromDate.Format("2006-01-02"), toDate.Format("2006-01-02")).
		Group("user_id, git_repo_url").
		Order("request_count DESC, user_id ASC, git_repo_url ASC").
		Scan(&rows).Error; err != nil {
		return nil, fmt.Errorf("%w: %v", ErrUsageQueryFailed, err)
	}
	result := make([]UsageRepoUserAggregate, 0, len(rows))
	for _, item := range rows {
		result = append(result, UsageRepoUserAggregate(item))
	}
	return result, nil
}

func usageDateRange(days int) (fromDate, toDate time.Time) {
	toDate = time.Now().UTC()
	fromDate = toDate.AddDate(0, 0, -(days - 1))
	return fromDate, toDate
}
