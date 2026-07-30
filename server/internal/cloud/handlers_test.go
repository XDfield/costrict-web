package cloud

import (
	"bytes"
	"database/sql"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/costrict/costrict-web/server/internal/services"
	"github.com/gin-gonic/gin"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// setupDevicesDBForCloud creates an in-memory sqlite DB with a minimal
// devices table for DeviceService.VerifyDeviceToken lookups.
func setupDevicesDBForCloud(t *testing.T) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	if err := db.Exec(`CREATE TABLE devices (
		id TEXT PRIMARY KEY,
		device_id TEXT NOT NULL,
		display_name TEXT,
		platform TEXT,
		version TEXT,
		user_id TEXT,
		status TEXT DEFAULT 'offline',
		label TEXT,
		description TEXT,
		token TEXT,
		token_rotated_at DATETIME,
		last_connected_at DATETIME,
		last_seen_at DATETIME,
		created_at DATETIME,
		updated_at DATETIME,
		deleted_at DATETIME
	)`).Error; err != nil {
		t.Fatalf("create devices table: %v", err)
	}
	return db
}

// TestNotifyRespondedHandler_Auth verifies the handler enforces device token
// validation. Regression for the "Bearer accepted without verification" bug
// that allowed anyone to mark any session as responded.
func TestNotifyRespondedHandler_Auth(t *testing.T) {
	gin.SetMode(gin.TestMode)

	db := setupDevicesDBForCloud(t)
	deviceSvc := &services.DeviceService{DB: db}

	// Seed a real device with a known token.
	validToken := "real-device-token-abc"
	if err := db.Exec(`INSERT INTO devices (id, device_id, display_name, platform, version, user_id, status, token, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, datetime('now'), datetime('now'))`,
		"dev-1", "dev-id-1", "Test Device", "linux", "1.0", "user-1", "online", validToken).Error; err != nil {
		t.Fatalf("seed device: %v", err)
	}

	r := gin.New()
	r.POST("/cloud/device/notify/responded", NotifyRespondedHandler(nil, deviceSvc))

	validBody := `{"sessionID":"sess-1","type":"message"}`

	cases := []struct {
		name       string
		authHeader string
		wantStatus int
	}{
		{
			name:       "missing authorization header rejected",
			authHeader: "",
			wantStatus: http.StatusUnauthorized,
		},
		{
			name:       "malformed bearer rejected",
			authHeader: "Bearer",
			wantStatus: http.StatusUnauthorized,
		},
		{
			name:       "unknown token rejected",
			authHeader: "Bearer not-a-real-token",
			wantStatus: http.StatusUnauthorized,
		},
		{
			name:       "valid device token accepted",
			authHeader: "Bearer " + validToken,
			wantStatus: http.StatusOK,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := httptest.NewRecorder()
			req, _ := http.NewRequest("POST", "/cloud/device/notify/responded", bytes.NewBufferString(validBody))
			req.Header.Set("Content-Type", "application/json")
			if tc.authHeader != "" {
				req.Header.Set("Authorization", tc.authHeader)
			}
			r.ServeHTTP(w, req)
			if w.Code != tc.wantStatus {
				t.Errorf("auth=%q: status=%d want=%d body=%s",
					tc.authHeader, w.Code, tc.wantStatus, w.Body.String())
			}
		})
	}
}

// silence unused import warning when sql.Rows is referenced indirectly.
var _ = sql.ErrNoRows
