package handlers

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func createGitContractReq(t *testing.T, body []byte) *httptest.ResponseRecorder {
	t.Helper()
	r := newGitCreateRouter("bob")
	w := httptest.NewRecorder()
	req, err := http.NewRequest(http.MethodPost, "/api/items/git", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(w, req)
	return w
}

func assertGitCreateError(t *testing.T, w *httptest.ResponseRecorder, status int, code string) {
	t.Helper()
	if w.Code != status {
		t.Fatalf("expected %d, got %d (%s)", status, w.Code, w.Body.String())
	}
	var got struct {
		ErrorCode string `json:"error_code"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if got.ErrorCode != code {
		t.Fatalf("expected error_code %q, got %q (%s)", code, got.ErrorCode, w.Body.String())
	}
}

func TestCreateGitBackedItem_RequestContractLimitsAndPresence(t *testing.T) {
	tests := []struct {
		name   string
		body   func() []byte
		status int
		code   string
	}{
		{"unknown key", func() []byte { return []byte(`{"itemType":"skill","name":"x","unknown":1}`) }, http.StatusBadRequest, "GIT_CREATE_INVALID_REQUEST"},
		{"visibility null", func() []byte { return []byte(`{"itemType":"skill","name":"x","visibility":null}`) }, http.StatusBadRequest, "GIT_CREATE_FIELD_UNSUPPORTED"},
		{"visibility empty", func() []byte { return []byte(`{"itemType":"skill","name":"x","visibility":""}`) }, http.StatusBadRequest, "GIT_CREATE_FIELD_UNSUPPORTED"},
		{"registryId null", func() []byte { return []byte(`{"itemType":"skill","name":"x","registryId":null}`) }, http.StatusBadRequest, "GIT_CREATE_FIELD_UNSUPPORTED"},
		{"trailing json", func() []byte { return []byte(`{"itemType":"skill","name":"x"}{}`) }, http.StatusBadRequest, "GIT_CREATE_INVALID_REQUEST"},
		{"manifest over 8MiB unicode", func() []byte {
			b, _ := json.Marshal(map[string]any{"itemType": "skill", "name": "x", "content": strings.Repeat("é", 4*1024*1024+1)})
			return b
		}, http.StatusBadRequest, "GIT_MANIFEST_TOO_LARGE"},
		{"asset count over 64", func() []byte {
			assets := make([]map[string]string, 65)
			for i := range assets {
				assets[i] = map[string]string{"relPath": "a.txt", "textContent": "x"}
			}
			b, _ := json.Marshal(map[string]any{"itemType": "skill", "name": "x", "assets": assets})
			return b
		}, http.StatusBadRequest, "GIT_ASSET_COUNT_LIMIT"},
		{"asset aggregate over 16MiB", func() []byte {
			assets := make([]map[string]string, 5)
			chunk := strings.Repeat("x", 4*1024*1024)
			for i := range assets {
				assets[i] = map[string]string{"relPath": fmt.Sprintf("a-%d.txt", i), "textContent": chunk}
			}
			b, _ := json.Marshal(map[string]any{"itemType": "skill", "name": "x", "assets": assets})
			return b
		}, http.StatusBadRequest, "GIT_ASSET_TOTAL_TOO_LARGE"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			defer setupTestDB(t)()
			createPublicRegistry(t)
			fx := setupGitForkFixture(t)
			seedUserGitAccount(t, fx.db, "bob", "10001", true)
			w := createGitContractReq(t, tc.body())
			assertGitCreateError(t, w, tc.status, tc.code)
			if len(fx.gitea.createCalls) != 0 {
				t.Fatalf("validation failure provisioned %d repositories", len(fx.gitea.createCalls))
			}
		})
	}
}

func TestCreateGitBackedItem_RequestBodyOver32MiB(t *testing.T) {
	defer setupTestDB(t)()
	createPublicRegistry(t)
	fx := setupGitForkFixture(t)
	seedUserGitAccount(t, fx.db, "bob", "10001", true)
	body := []byte(`{"itemType":"skill","name":"x","description":"` + strings.Repeat("x", 33*1024*1024) + `"}`)
	w := createGitContractReq(t, body)
	assertGitCreateError(t, w, http.StatusRequestEntityTooLarge, "GIT_CREATE_BODY_TOO_LARGE")
	if len(fx.gitea.createCalls) != 0 {
		t.Fatalf("oversized body provisioned %d repositories", len(fx.gitea.createCalls))
	}
}
