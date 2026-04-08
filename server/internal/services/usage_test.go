package services

import (
	"testing"
	"time"

	"github.com/costrict/costrict-web/server/internal/models"
	userpkg "github.com/costrict/costrict-web/server/internal/user"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

type usageProviderStub struct {
	getActivityFunc                    func(gitRepoURL string, days int) (*UsageActivityResponse, error)
	aggregateProjectRepoActivityFunc   func(userIDs []string, repoURLs []string, days int) ([]UsageRepoUserAggregate, error)
	aggregateProjectRepoDailyFunc      func(userIDs []string, repoURLs []string, days int) ([]UsageRepoDailyAggregate, error)
	aggregateRepositoriesByUsersFunc   func(userIDs []string, days int) ([]UsageRepoUserAggregate, error)
}

func (s *usageProviderStub) BatchUpsert(userID, deviceID, accessToken, clientVersion string, records []*models.SessionUsageReport) UsageReportResponse {
	return UsageReportResponse{}
}

func (s *usageProviderStub) GetActivity(gitRepoURL string, days int) (*UsageActivityResponse, error) {
	if s.getActivityFunc != nil {
		return s.getActivityFunc(gitRepoURL, days)
	}
	return &UsageActivityResponse{}, nil
}

func (s *usageProviderStub) AggregateProjectRepoActivity(userIDs []string, repoURLs []string, days int) ([]UsageRepoUserAggregate, error) {
	if s.aggregateProjectRepoActivityFunc != nil {
		return s.aggregateProjectRepoActivityFunc(userIDs, repoURLs, days)
	}
	return nil, nil
}

func (s *usageProviderStub) AggregateProjectRepoDailyActivity(userIDs []string, repoURLs []string, days int) ([]UsageRepoDailyAggregate, error) {
	if s.aggregateProjectRepoDailyFunc != nil {
		return s.aggregateProjectRepoDailyFunc(userIDs, repoURLs, days)
	}
	return nil, nil
}

func (s *usageProviderStub) AggregateRepositoriesByUsers(userIDs []string, days int) ([]UsageRepoUserAggregate, error) {
	if s.aggregateRepositoriesByUsersFunc != nil {
		return s.aggregateRepositoriesByUsersFunc(userIDs, days)
	}
	return nil, nil
}

func setupUsageTestDB(t *testing.T) (*gorm.DB, *userpkg.UserService) {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	if err := db.AutoMigrate(&models.User{}, &models.SessionUsageReport{}); err != nil {
		t.Fatalf("migrate sqlite: %v", err)
	}
	users := []models.User{
		{ID: "u1", Username: "alice"},
		{ID: "u2", Username: "bob"},
	}
	for _, user := range users {
		if err := db.Create(&user).Error; err != nil {
			t.Fatalf("seed user: %v", err)
		}
	}
	return db, userpkg.NewUserService(db)
}

func TestNormalizeGitRepoURL(t *testing.T) {
	tests := map[string]string{
		"git@github.com:zgsm-ai/opencode.git":         "https://github.com/zgsm-ai/opencode",
		"https://github.com/zgsm-ai/opencode/":        "https://github.com/zgsm-ai/opencode",
		"HTTPS://GitHub.com/zgsm-ai/opencode.git":     "https://github.com/zgsm-ai/opencode",
		"github.com/zgsm-ai/opencode":                 "https://github.com/zgsm-ai/opencode",
	}
	for input, expected := range tests {
		got, err := NormalizeGitRepoURL(input)
		if err != nil {
			t.Fatalf("NormalizeGitRepoURL(%q) error: %v", input, err)
		}
		if got != expected {
			t.Fatalf("NormalizeGitRepoURL(%q)=%q, want %q", input, got, expected)
		}
	}
}

func TestBatchUpsertIdempotent(t *testing.T) {
	db, userSvc := setupUsageTestDB(t)
	svc := NewUsageService(NewSQLiteUsageProvider(db), userSvc)
	req := UsageReportRequest{
		DeviceID:      "d1",
		ClientVersion: "costrict-cli-1.0.0",
		Reports: []UsageReportItem{{
			SessionID:  "s1",
			RequestID:  "r1",
			MessageID:  "m1",
			Date:       "2026-04-01T10:00:00Z",
			Updated:    "2026-04-01T10:00:01Z",
			ModelID:    "glm-4",
			Rounds:     1,
			GitRepoURL: "git@github.com:zgsm-ai/opencode.git",
		}, {
			SessionID:  "s1",
			RequestID:  "r1b",
			MessageID:  "m1",
			Date:       "2026-04-01T10:00:00Z",
			Updated:    "2026-04-01T10:01:00Z",
			ModelID:    "glm-4",
			Rounds:     2,
			GitRepoURL: "https://github.com/zgsm-ai/opencode/",
		}},
	}
	resp := svc.BatchUpsert("u1", "token-1", req)
	if resp.Accepted != 2 || resp.Skipped != 0 {
		t.Fatalf("unexpected response: %+v", resp)
	}
	var count int64
	if err := db.Model(&models.SessionUsageReport{}).Count(&count).Error; err != nil {
		t.Fatalf("count records: %v", err)
	}
	if count != 1 {
		t.Fatalf("expected 1 upserted record, got %d", count)
	}
	var report models.SessionUsageReport
	if err := db.First(&report).Error; err != nil {
		t.Fatalf("load report: %v", err)
	}
	if report.Rounds != 2 {
		t.Fatalf("expected latest rounds persisted, got %d", report.Rounds)
	}
	if report.GitRepoURL != "https://github.com/zgsm-ai/opencode" {
		t.Fatalf("unexpected normalized repo: %s", report.GitRepoURL)
	}
}

func TestGetActivityAggregatesByUserAndDay(t *testing.T) {
	db, userSvc := setupUsageTestDB(t)
	svc := NewUsageService(NewSQLiteUsageProvider(db), userSvc)
	seed := []models.SessionUsageReport{
		{UserID: "u1", SessionID: "s1", MessageID: "m1", Date: time.Date(2026, 4, 1, 10, 0, 0, 0, time.UTC), Updated: time.Now().UTC(), ModelID: "glm", Rounds: 1, GitRepoURL: "https://github.com/zgsm-ai/opencode"},
		{UserID: "u1", SessionID: "s2", MessageID: "m2", Date: time.Date(2026, 4, 1, 11, 0, 0, 0, time.UTC), Updated: time.Now().UTC(), ModelID: "glm", Rounds: 1, GitRepoURL: "https://github.com/zgsm-ai/opencode"},
		{UserID: "u2", SessionID: "s3", MessageID: "m3", Date: time.Date(2026, 4, 2, 11, 0, 0, 0, time.UTC), Updated: time.Now().UTC(), ModelID: "glm", Rounds: 1, GitRepoURL: "https://github.com/zgsm-ai/opencode"},
	}
	for _, item := range seed {
		if err := db.Create(&item).Error; err != nil {
			t.Fatalf("seed usage: %v", err)
		}
	}
	resp, err := svc.GetActivity("git@github.com:zgsm-ai/opencode.git", 90, nil)
	if err != nil {
		t.Fatalf("GetActivity error: %v", err)
	}
	if resp.GitRepoURL != "https://github.com/zgsm-ai/opencode" {
		t.Fatalf("unexpected repo: %s", resp.GitRepoURL)
	}
	if len(resp.Users) != 2 {
		t.Fatalf("expected 2 users, got %+v", resp.Users)
	}
	if resp.Users[0].TotalRequests != 2 {
		t.Fatalf("expected user1 total 2, got %+v", resp.Users[0])
	}
	if resp.Users[0].Username != "alice" {
		t.Fatalf("expected resolved username alice, got %s", resp.Users[0].Username)
	}
}

func TestUsageServiceGetActivityESMapsUniversalIDBackToLocalUser(t *testing.T) {
	db, userSvc := setupUsageTestDB(t)
	uuid1 := "uuid-u1"
	if err := db.Model(&models.User{}).Where("id = ?", "u1").Update("casdoor_universal_id", uuid1).Error; err != nil {
		t.Fatalf("update user universal id: %v", err)
	}
	svc := NewUsageService(&ESUsageProvider{}, userSvc)
	users := []UsageUserActivity{{
		UserID:        uuid1,
		Username:      uuid1,
		TotalRequests: 3,
		Daily:         []UsageDaily{{Date: "2026-04-01", Requests: 3}},
	}}

	if err := svc.rewriteUserIDsFromUniversal(users); err != nil {
		t.Fatalf("rewriteUserIDsFromUniversal error: %v", err)
	}
	if users[0].UserID != "u1" {
		t.Fatalf("expected local user id u1, got %s", users[0].UserID)
	}

	names, err := svc.resolveUserNames([]string{users[0].UserID}, nil)
	if err != nil {
		t.Fatalf("resolveUserNames error: %v", err)
	}
	if names[users[0].UserID] != "alice" {
		t.Fatalf("expected resolved username alice, got %s", names[users[0].UserID])
	}
}

func TestUsageServiceAggregateProjectRepoActivityESQueriesByUniversalIDAndRestoresLocalID(t *testing.T) {
	db, userSvc := setupUsageTestDB(t)
	uuid1 := "uuid-u1"
	if err := db.Model(&models.User{}).Where("id = ?", "u1").Update("casdoor_universal_id", uuid1).Error; err != nil {
		t.Fatalf("update user universal id: %v", err)
	}
	svc := NewUsageService(&ESUsageProvider{}, userSvc)

	queryUserIDs, restore, err := svc.prepareProviderUserIDs([]string{"u1"})
	if err != nil {
		t.Fatalf("prepareProviderUserIDs error: %v", err)
	}
	if len(queryUserIDs) != 1 || queryUserIDs[0] != uuid1 {
		t.Fatalf("expected provider query universal id %s, got %+v", uuid1, queryUserIDs)
	}

	aggs := []UsageRepoUserAggregate{{
		UserID:          uuid1,
		GitRepoURL:      "https://github.com/zgsm-ai/opencode",
		RequestCount:    5,
		FirstActiveDate: "2026-04-01",
		LastActiveDate:  "2026-04-02",
	}}
	restore(aggs)
	if aggs[0].UserID != "u1" {
		t.Fatalf("expected restored local user id u1, got %s", aggs[0].UserID)
	}
}
