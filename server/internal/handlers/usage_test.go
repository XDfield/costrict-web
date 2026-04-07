package handlers

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/costrict/costrict-web/server/internal/middleware"
	"github.com/costrict/costrict-web/server/internal/models"
	"github.com/costrict/costrict-web/server/internal/services"
	userpkg "github.com/costrict/costrict-web/server/internal/user"
	"github.com/gin-gonic/gin"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

func setupUsageHandlerTest(t *testing.T) (*gin.Engine, *gorm.DB) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	if err := db.AutoMigrate(&models.User{}, &models.SessionUsageReport{}); err != nil {
		t.Fatalf("migrate sqlite: %v", err)
	}
	if err := db.Create(&models.User{ID: "u1", Username: "alice"}).Error; err != nil {
		t.Fatalf("seed user: %v", err)
	}
	r := gin.New()
	r.Use(func(c *gin.Context) {
		if userID := c.GetHeader("X-User-ID"); userID != "" {
			c.Set(middleware.UserIDKey, userID)
		}
		c.Next()
	})
	h := NewUsageHandler(services.NewUsageService(services.NewSQLiteUsageProvider(db), userpkg.NewUserService(db)))
	r.POST("/usage/report", h.Report)
	r.GET("/usage/activity", h.Activity)
	return r, db
}

func TestUsageReportHandler(t *testing.T) {
	r, db := setupUsageHandlerTest(t)
	body, _ := json.Marshal(map[string]any{
		"device_id": "d1",
		"reports": []map[string]any{{
			"session_id":   "s1",
			"message_id":   "m1",
			"date":         "2026-04-01T10:00:00Z",
			"updated":      "2026-04-01T10:00:01Z",
			"model_id":     "glm-4",
			"rounds":       1,
			"git_repo_url": "git@github.com:zgsm-ai/opencode.git",
		}},
	})
	req := httptest.NewRequest(http.MethodPost, "/usage/report", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-User-ID", "u1")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d, body=%s", w.Code, w.Body.String())
	}
	var count int64
	if err := db.Model(&models.SessionUsageReport{}).Count(&count).Error; err != nil {
		t.Fatalf("count usage: %v", err)
	}
	if count != 1 {
		t.Fatalf("expected 1 usage row, got %d", count)
	}
}

func TestUsageActivityHandler(t *testing.T) {
	r, db := setupUsageHandlerTest(t)
	seed := models.SessionUsageReport{
		UserID:      "u1",
		SessionID:   "s1",
		MessageID:   "m1",
		Date:        time.Now().UTC(),
		Updated:     time.Now().UTC(),
		ModelID:     "glm-4",
		Rounds:      1,
		GitRepoURL:  "https://github.com/zgsm-ai/opencode",
	}
	if err := db.Create(&seed).Error; err != nil {
		t.Fatalf("seed usage: %v", err)
	}
	req := httptest.NewRequest(http.MethodGet, "/usage/activity?git_repo_url=https://github.com/zgsm-ai/opencode&days=7", nil)
	req.Header.Set("X-User-ID", "u1")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d, body=%s", w.Code, w.Body.String())
	}
}
