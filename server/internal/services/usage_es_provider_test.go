package services

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/costrict/costrict-web/server/internal/models"
)

func TestESUsageProviderReportRecordUsesRequestTime(t *testing.T) {
	var body map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer r.Body.Close()
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("decode body: %v", err)
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	p := NewESUsageProvider(ESUsageProviderConfig{ReportBaseURL: server.URL, ReportPath: "/report"})
	err := p.reportRecord("d1", "", "costrict-cli-1.0.0", &models.SessionUsageReport{
		SessionID:   "s1",
		RequestID:   "r1",
		MessageID:   "m1",
		RequestTime: time.Date(2026, 4, 1, 9, 59, 30, 0, time.UTC),
		Date:        time.Date(2026, 4, 1, 10, 0, 0, 0, time.UTC),
		Updated:     time.Date(2026, 4, 1, 10, 0, 1, 0, time.UTC),
		ModelID:     "glm-4",
		ProviderID:  "p",
		GitRepoURL:  "https://github.com/zgsm-ai/opencode",
	})
	if err != nil {
		t.Fatalf("reportRecord error: %v", err)
	}
	label, ok := body["label"].(map[string]any)
	if !ok {
		t.Fatalf("missing label payload: %+v", body)
	}
	if got := label["request_time"]; got != "2026-04-01T09:59:30Z" {
		t.Fatalf("unexpected request_time: %v", got)
	}
}
