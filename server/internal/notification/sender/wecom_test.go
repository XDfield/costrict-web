package sender

import "testing"

// TestValidateWeComURL focuses on the WeCom-specific https-only restriction.
// The underlying public-IP / loopback / DNS checks are exercised by
// safetch_test.go; here we only verify the additional https-only layer that
// validateWeComURL layers on top of safetch.ValidateURL.
//
// NOTE: the https-accepted case requires the host to resolve to a public IP.
// "api.openai.com" is a stable public endpoint; if it ever moves behind a
// private IP the test will fail loudly rather than silently mislead.
func TestValidateWeComURL(t *testing.T) {
	cases := []struct {
		name    string
		url     string
		wantErr bool
	}{
		{"empty", "", true},
		{"http rejected", "http://qyapi.weixin.qq.com/cgi-bin/webhook/send", true},
		{"https public accepted", "https://qyapi.weixin.qq.com/cgi-bin/webhook/send?key=xxx", false},
		{"https loopback rejected", "https://127.0.0.1:8080", true},
		{"bad scheme rejected", "ftp://qyapi.weixin.qq.com", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := validateWeComURL(tc.url)
			if tc.wantErr && err == nil {
				t.Errorf("validateWeComURL(%q): expected error, got nil", tc.url)
			}
			if !tc.wantErr && err != nil {
				t.Errorf("validateWeComURL(%q): unexpected error: %v", tc.url, err)
			}
		})
	}
}

// TestValidateWeComUserConfigConfigHTTPSOnly verifies the write-time gate
// rejects an http:// WebhookURL, so attackers cannot persist an unsafe config
// and wait for a later Send.
func TestValidateWeComUserConfigRejectsHTTP(t *testing.T) {
	s := NewWeComSender()
	cfg := []byte(`{"webhookUrl":"http://qyapi.weixin.qq.com/cgi-bin/webhook/send?key=xxx"}`)
	if err := s.ValidateUserConfig(cfg); err == nil {
		t.Errorf("expected ValidateUserConfig to reject http:// webhookUrl, got nil")
	}
}
