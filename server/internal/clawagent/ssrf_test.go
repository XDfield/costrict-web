package clawagent

import "testing"

func TestValidateProviderBaseURL(t *testing.T) {
	cases := []struct {
		name    string
		url     string
		wantErr bool
	}{
		{"empty allowed", "", false},
		{"public https", "https://api.openai.com/v1", false},
		{"public http", "http://api.openai.com/v1", false},
		{"loopback literal", "http://127.0.0.1:8080", true},
		{"loopback v6", "http://[::1]:8080", true},
		{"localhost name", "http://localhost:8080", true},
		{"localhost subdomain", "http://api.localhost", true},
		{"link-local", "http://169.254.169.254/latest", true},
		{"private 10.x", "http://10.0.0.1", true},
		{"private 192.168", "http://192.168.1.1", true},
		{"private 172.16", "http://172.16.0.1", true},
		{"bad scheme", "ftp://example.com", true},
		{"non-absolute", "/v1/api", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateProviderBaseURL(tc.url)
			if tc.wantErr && err == nil {
				t.Errorf("expected error for %q, got nil", tc.url)
			}
			if !tc.wantErr && err != nil {
				t.Errorf("unexpected error for %q: %v", tc.url, err)
			}
		})
	}
}
