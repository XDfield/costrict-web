package services

import (
	"bytes"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/costrict/costrict-web/server/internal/logger"
	"github.com/costrict/costrict-web/server/internal/models"
)

const (
	esRepoField        = "user_metrics.git_repo"
	esUserField        = "user_id"
	esTimeField        = "@timestamp"
	esDayFormat        = "2006-01-02"
	esDateHistogramFormat = "yyyy-MM-dd"
	defaultReportPath  = "/internal/indicator/api/v1/session_turn_metrics"
	defaultSearchPath  = "/costrict_session_turn_metrics/_search"
	maxLogBodyLength   = 2000
)

type ESUsageProviderConfig struct {
	ReportBaseURL string
	QueryBaseURL  string
	ReportPath string
	QueryPath  string
	Timeout    time.Duration
	BasicUser  string
	BasicPass  string
	InsecureSkipVerify bool
}

type ESUsageProvider struct {
	reportBaseURL string
	queryBaseURL  string
	reportURL string
	queryURL  string
	client    *http.Client
	basicUser string
	basicPass string
}

func NewESUsageProvider(cfg ESUsageProviderConfig) *ESUsageProvider {
	reportBaseURL := strings.TrimRight(strings.TrimSpace(cfg.ReportBaseURL), "/")
	queryBaseURL := strings.TrimRight(strings.TrimSpace(cfg.QueryBaseURL), "/")
	reportPath := strings.TrimSpace(cfg.ReportPath)
	queryPath := strings.TrimSpace(cfg.QueryPath)
	if reportPath == "" {
		reportPath = defaultReportPath
	}
	if queryPath == "" {
		queryPath = defaultSearchPath
	}
	timeout := cfg.Timeout
	if timeout <= 0 {
		timeout = 15 * time.Second
	}
	transport := &http.Transport{
		Proxy: http.ProxyFromEnvironment,
		DialContext: (&net.Dialer{
			Timeout:   30 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          100,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
		TLSClientConfig: &tls.Config{
			InsecureSkipVerify: cfg.InsecureSkipVerify,
		},
	}
	return &ESUsageProvider{
		reportBaseURL: reportBaseURL,
		queryBaseURL:  queryBaseURL,
		reportURL: joinURL(reportBaseURL, reportPath),
		queryURL:  joinURL(queryBaseURL, queryPath),
		client:    &http.Client{Timeout: timeout, Transport: transport},
		basicUser: strings.TrimSpace(cfg.BasicUser),
		basicPass: cfg.BasicPass,
	}
}

func (p *ESUsageProvider) BatchUpsert(userID, deviceID, accessToken, clientVersion string, records []*models.SessionUsageReport) UsageReportResponse {
	result := UsageReportResponse{}
	if p == nil || p.reportBaseURL == "" {
		result.Errors = append(result.Errors, "usage es provider is not configured")
		result.Skipped = len(records)
		return result
	}

	for i, record := range records {
		if record == nil {
			result.Skipped++
			result.Errors = append(result.Errors, fmt.Sprintf("records[%d]: nil record", i))
			continue
		}
		if err := p.reportRecord(deviceID, accessToken, clientVersion, record); err != nil {
			result.Skipped++
			result.Errors = append(result.Errors, fmt.Sprintf("records[%d]: %v", i, err))
			continue
		}
		result.Accepted++
	}
	return result
}

func (p *ESUsageProvider) GetActivity(gitRepoURL string, days int) (*UsageActivityResponse, error) {
	if p == nil || p.queryBaseURL == "" {
		return nil, ErrUsageQueryFailed
	}
	if days < 1 || days > 90 {
		return nil, fmt.Errorf("days must be between 1 and 90")
	}
	fromDate, toDate := usageDateRange(days)
	items, err := p.search(esSearchRequest{
		Query: buildESBoolQuery([]string{gitRepoURL}, nil, fromDate, toDate),
		Aggs:  buildRepoUserDayAggs(),
		Size:  0,
	})
	if err != nil {
		return nil, err
	}

	userMap := map[string]*UsageUserActivity{}
	for _, item := range items {
		if item.Repo != gitRepoURL || item.UserID == "" || item.Day == "" {
			continue
		}
		entry, ok := userMap[item.UserID]
		if !ok {
			entry = &UsageUserActivity{UserID: item.UserID, Username: item.UserID}
			userMap[item.UserID] = entry
		}
		entry.Daily = append(entry.Daily, UsageDaily{Date: item.Day, Requests: item.RequestCount})
		entry.TotalRequests += item.RequestCount
	}

	users := make([]UsageUserActivity, 0, len(userMap))
	for _, userID := range sortedKeys(userMap) {
		entry := userMap[userID]
		sort.Slice(entry.Daily, func(i, j int) bool { return entry.Daily[i].Date < entry.Daily[j].Date })
		users = append(users, *entry)
	}

	resp := &UsageActivityResponse{GitRepoURL: gitRepoURL, Users: users}
	resp.Range.From = fromDate.Format(esDayFormat)
	resp.Range.To = toDate.Format(esDayFormat)
	return resp, nil
}

func (p *ESUsageProvider) AggregateProjectRepoActivity(userIDs []string, repoURLs []string, days int) ([]UsageRepoUserAggregate, error) {
	if p == nil || p.queryBaseURL == "" {
		return nil, ErrUsageQueryFailed
	}
	if len(userIDs) == 0 || len(repoURLs) == 0 {
		return []UsageRepoUserAggregate{}, nil
	}
	if days < 1 || days > 90 {
		return nil, fmt.Errorf("days must be between 1 and 90")
	}
	fromDate, toDate := usageDateRange(days)
	payload := esSearchRequest{
		Query: buildESBoolQuery(repoURLs, userIDs, fromDate, toDate),
		Aggs:  buildRepoUserDayAggs(),
		Size:  0,
	}
	items, err := p.search(payload)
	if err != nil {
		return nil, err
	}
	return aggregateRepoUsers(items), nil
}

func (p *ESUsageProvider) AggregateProjectRepoDailyActivity(userIDs []string, repoURLs []string, days int) ([]UsageRepoDailyAggregate, error) {
	if p == nil || p.queryBaseURL == "" {
		return nil, ErrUsageQueryFailed
	}
	if len(userIDs) == 0 || len(repoURLs) == 0 {
		return []UsageRepoDailyAggregate{}, nil
	}
	if days < 1 || days > 90 {
		return nil, fmt.Errorf("days must be between 1 and 90")
	}
	fromDate, toDate := usageDateRange(days)
	payload := esSearchRequest{
		Query: buildESBoolQuery(repoURLs, userIDs, fromDate, toDate),
		Aggs:  buildRepoUserDayAggs(),
		Size:  0,
	}
	items, err := p.search(payload)
	if err != nil {
		return nil, err
	}
	return aggregateRepoDaily(items), nil
}

func (p *ESUsageProvider) AggregateRepositoriesByUsers(userIDs []string, days int) ([]UsageRepoUserAggregate, error) {
	if p == nil || p.queryBaseURL == "" {
		return nil, ErrUsageQueryFailed
	}
	if len(userIDs) == 0 {
		return []UsageRepoUserAggregate{}, nil
	}
	if days < 1 || days > 90 {
		return nil, fmt.Errorf("days must be between 1 and 90")
	}
	fromDate, toDate := usageDateRange(days)
	items, err := p.search(esSearchRequest{
		Query: buildESBoolQuery(nil, userIDs, fromDate, toDate),
		Aggs:  buildRepoUserDayAggs(),
		Size:  0,
	})
	if err != nil {
		return nil, err
	}
	return aggregateRepoUsers(items), nil
}

type esSearchRequest struct {
	Query map[string]any `json:"query"`
	Aggs  map[string]any `json:"aggs"`
	Size  int            `json:"size"`
}

type esSearchResponse struct {
	Aggregations struct {
		Repos struct {
			Buckets []esRepoBucket `json:"buckets"`
		} `json:"repos"`
	} `json:"aggregations"`
}

type esRepoBucket struct {
	Key      string `json:"key"`
	Users    struct {
		Buckets []esUserBucket `json:"buckets"`
	} `json:"users"`
}

type esUserBucket struct {
	Key  string `json:"key"`
	Days struct {
		Buckets []esDayBucket `json:"buckets"`
	} `json:"days"`
}

type esDayBucket struct {
	KeyAsString string `json:"key_as_string"`
	DocCount    int64  `json:"doc_count"`
	PromptTokens struct {
		Value float64 `json:"value"`
	} `json:"prompt_tokens"`
	CompletionTokens struct {
		Value float64 `json:"value"`
	} `json:"completion_tokens"`
	ReasoningTokens struct {
		Value float64 `json:"value"`
	} `json:"reasoning_tokens"`
	CacheReadTokens struct {
		Value float64 `json:"value"`
	} `json:"cache_read_tokens"`
	CacheWriteTokens struct {
		Value float64 `json:"value"`
	} `json:"cache_write_tokens"`
	Cost struct {
		Value float64 `json:"value"`
	} `json:"cost"`
}

type esFlatRepoUserDay struct {
	Repo             string
	UserID           string
	Day              string
	RequestCount     int64
	PromptTokens     int64
	CompletionTokens int64
	ReasoningTokens  int64
	CacheReadTokens  int64
	CacheWriteTokens int64
	Cost             float64
}

func (p *ESUsageProvider) reportRecord(deviceID, accessToken, clientVersion string, record *models.SessionUsageReport) error {
	body, err := json.Marshal(map[string]any{
		"request_id":  record.RequestID,
		"message_id":  record.MessageID,
		"occurred_at": record.Date.Format(time.RFC3339),
		"token_metrics": map[string]any{
			"prompt_tokens":      record.InputTokens,
			"completions_tokens": record.OutputTokens,
			"reasoning_tokens":   record.ReasoningTokens,
			"cache_read_tokens":  record.CacheReadTokens,
			"cache_write_tokens": record.CacheWriteTokens,
		},
		"cost_metrics": map[string]any{
			"cost": record.Cost,
		},
		"user_metrics": map[string]any{
			"git_repo":   record.GitRepoURL,
			"session_id": record.SessionID,
			"device_id":  firstNonEmpty(deviceID, record.DeviceID),
		},
		"label": map[string]any{
			"model":          record.ModelID,
			"provider":       record.ProviderID,
			"client_version": strings.TrimSpace(clientVersion),
			"request_time":   record.Date.Format(time.RFC3339),
			"mode":           "code",
		},
	})
	if err != nil {
		return err
	}
	req, err := http.NewRequest(http.MethodPut, p.reportURL, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	if strings.TrimSpace(accessToken) != "" {
		req.Header.Set("Authorization", "Bearer "+strings.TrimSpace(accessToken))
	}

	resp, err := p.client.Do(req)
	if err != nil {
		logger.Error("[usage.es.report] request failed url=%s err=%v payload=%s", p.reportURL, err, logger.Truncate(string(body), maxLogBodyLength))
		return fmt.Errorf("report to es failed: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		payload, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		logger.Error("[usage.es.report] unexpected response status=%d url=%s payload=%s response=%s", resp.StatusCode, p.reportURL, logger.Truncate(string(body), maxLogBodyLength), logger.Truncate(string(payload), maxLogBodyLength))
		return fmt.Errorf("report to es failed: status=%d body=%s", resp.StatusCode, strings.TrimSpace(string(payload)))
	}
	return nil
}

func (p *ESUsageProvider) search(payload esSearchRequest) ([]esFlatRepoUserDay, error) {
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequest(http.MethodPost, p.queryURL, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	p.applyAuth(req)

	resp, err := p.client.Do(req)
	if err != nil {
		logger.Error("[usage.es.query] request failed url=%s err=%v payload=%s", p.queryURL, err, logger.Truncate(string(body), maxLogBodyLength))
		return nil, fmt.Errorf("query es failed: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		payload, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		logger.Error("[usage.es.query] unexpected response status=%d url=%s payload=%s response=%s", resp.StatusCode, p.queryURL, logger.Truncate(string(body), maxLogBodyLength), logger.Truncate(string(payload), maxLogBodyLength))
		return nil, fmt.Errorf("query es failed: status=%d body=%s", resp.StatusCode, strings.TrimSpace(string(payload)))
	}

	var parsed esSearchResponse
	if err := json.NewDecoder(resp.Body).Decode(&parsed); err != nil {
		return nil, fmt.Errorf("decode es response failed: %w", err)
	}
	return flattenSearchBuckets(parsed.Aggregations.Repos.Buckets), nil
}

func buildESBoolQuery(repoURLs, userIDs []string, fromDate, toDate time.Time) map[string]any {
	filters := []any{
		map[string]any{
			"range": map[string]any{
				esTimeField: map[string]any{
					"gte": time.Date(fromDate.Year(), fromDate.Month(), fromDate.Day(), 0, 0, 0, 0, time.UTC).Format(time.RFC3339),
					"lte": time.Date(toDate.Year(), toDate.Month(), toDate.Day(), 23, 59, 59, 0, time.UTC).Format(time.RFC3339),
				},
			},
		},
	}
	if len(repoURLs) > 0 {
		filters = append(filters, map[string]any{
			"terms": map[string]any{esRepoField: repoURLs},
		})
	}
	if len(userIDs) > 0 {
		filters = append(filters, map[string]any{
			"terms": map[string]any{esUserField: userIDs},
		})
	}
	return map[string]any{
		"bool": map[string]any{
			"filter": filters,
		},
	}
}

func buildRepoUserDayAggs() map[string]any {
	return map[string]any{
		"repos": map[string]any{
			"terms": map[string]any{
				"field": esRepoField,
				"size":  1000,
			},
			"aggs": map[string]any{
				"users": map[string]any{
					"terms": map[string]any{
						"field": esUserField,
						"size":  1000,
					},
					"aggs": map[string]any{
						"days": map[string]any{
							"date_histogram": map[string]any{
								"field":             esTimeField,
								"calendar_interval": "day",
								"format":            esDateHistogramFormat,
							},
						},
					},
				},
			},
		},
	}
}

func flattenSearchBuckets(repos []esRepoBucket) []esFlatRepoUserDay {
	result := make([]esFlatRepoUserDay, 0)
	for _, repoBucket := range repos {
		for _, userBucket := range repoBucket.Users.Buckets {
			for _, dayBucket := range userBucket.Days.Buckets {
				result = append(result, esFlatRepoUserDay{
					Repo:             repoBucket.Key,
					UserID:           userBucket.Key,
					Day:              formatDay(dayBucket.KeyAsString),
					RequestCount:     dayBucket.DocCount,
					PromptTokens:     int64(dayBucket.PromptTokens.Value),
					CompletionTokens: int64(dayBucket.CompletionTokens.Value),
					ReasoningTokens:  int64(dayBucket.ReasoningTokens.Value),
					CacheReadTokens:  int64(dayBucket.CacheReadTokens.Value),
					CacheWriteTokens: int64(dayBucket.CacheWriteTokens.Value),
					Cost:             dayBucket.Cost.Value,
				})
			}
		}
	}
	return result
}

func aggregateRepoUsers(items []esFlatRepoUserDay) []UsageRepoUserAggregate {
	type key struct {
		Repo   string
		UserID string
	}
	byKey := map[key]*UsageRepoUserAggregate{}
	for _, item := range items {
		if item.Repo == "" || item.UserID == "" {
			continue
		}
		k := key{Repo: item.Repo, UserID: item.UserID}
		agg, ok := byKey[k]
		if !ok {
			agg = &UsageRepoUserAggregate{UserID: item.UserID, GitRepoURL: item.Repo}
			byKey[k] = agg
		}
		agg.RequestCount += item.RequestCount
		agg.InputTokens += item.PromptTokens
		agg.OutputTokens += item.CompletionTokens
		agg.TotalCost += item.Cost
		if agg.FirstActiveDate == "" || item.Day < agg.FirstActiveDate {
			agg.FirstActiveDate = item.Day
		}
		if item.Day > agg.LastActiveDate {
			agg.LastActiveDate = item.Day
		}
	}
	result := make([]UsageRepoUserAggregate, 0, len(byKey))
	for _, agg := range byKey {
		result = append(result, *agg)
	}
	sort.Slice(result, func(i, j int) bool {
		if result[i].RequestCount == result[j].RequestCount {
			if result[i].UserID == result[j].UserID {
				return result[i].GitRepoURL < result[j].GitRepoURL
			}
			return result[i].UserID < result[j].UserID
		}
		return result[i].RequestCount > result[j].RequestCount
	})
	return result
}

func aggregateRepoDaily(items []esFlatRepoUserDay) []UsageRepoDailyAggregate {
	type key struct {
		Repo string
		Day  string
	}
	byKey := map[key]int64{}
	for _, item := range items {
		if item.Repo == "" || item.Day == "" {
			continue
		}
		byKey[key{Repo: item.Repo, Day: item.Day}] += item.RequestCount
	}
	result := make([]UsageRepoDailyAggregate, 0, len(byKey))
	for k, count := range byKey {
		result = append(result, UsageRepoDailyAggregate{GitRepoURL: k.Repo, Date: k.Day, RequestCount: count})
	}
	sort.Slice(result, func(i, j int) bool {
		if result[i].GitRepoURL == result[j].GitRepoURL {
			return result[i].Date < result[j].Date
		}
		return result[i].GitRepoURL < result[j].GitRepoURL
	})
	return result
}

func joinURL(baseURL, path string) string {
	if baseURL == "" {
		return ""
	}
	if strings.HasPrefix(path, "http://") || strings.HasPrefix(path, "https://") {
		return path
	}
	if !strings.HasPrefix(path, "/") {
		path = "/" + path
	}
	return baseURL + path
}

func (p *ESUsageProvider) applyAuth(req *http.Request) {
	if p == nil || req == nil {
		return
	}
	if p.basicUser != "" || p.basicPass != "" {
		req.SetBasicAuth(p.basicUser, p.basicPass)
	}
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}

func formatDay(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	if t, err := time.Parse(time.RFC3339, raw); err == nil {
		return t.UTC().Format(esDayFormat)
	}
	if t, err := time.Parse(time.RFC3339Nano, raw); err == nil {
		return t.UTC().Format(esDayFormat)
	}
	if len(raw) >= 10 {
		return raw[:10]
	}
	return raw
}
