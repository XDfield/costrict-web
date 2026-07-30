package middleware

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestMaskSensitiveBody(t *testing.T) {
	cases := []struct {
		name   string
		input  string
		expect string // substring that MUST appear
		deny   string // substring that MUST NOT appear (case-sensitive)
	}{
		{
			name:   "password redacted",
			input:  `{"username":"alice","password":"hunter2"}`,
			expect: `"password":"***REDACTED***"`,
			deny:   "hunter2",
		},
		{
			name:   "api_key variant",
			input:  `{"api_key":"sk-abc123"}`,
			expect: `"api_key":"***REDACTED***"`,
			deny:   "sk-abc123",
		},
		{
			name:   "nested token",
			input:  `{"user":{"name":"bob","access_token":"tok_xyz"},"ok":true}`,
			expect: `"access_token":"***REDACTED***"`,
			deny:   "tok_xyz",
		},
		{
			name:   "array of creds",
			input:  `{"items":[{"client_secret":"s1"},{"client_secret":"s2"}]}`,
			expect: `"client_secret":"***REDACTED***"`,
			deny:   "s1",
		},
		{
			name:   "non-sensitive fields kept",
			input:  `{"name":"alice","email":"a@b.com"}`,
			expect: `alice`,
			deny:   "***REDACTED***",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out := maskSensitiveBody([]byte(tc.input))
			s := string(out)
			if !strings.Contains(s, tc.expect) {
				t.Errorf("expected output to contain %q, got %s", tc.expect, s)
			}
			if tc.deny != "" && strings.Contains(s, tc.deny) {
				t.Errorf("output must NOT contain %q, got %s", tc.deny, s)
			}
		})
	}
}

func TestMaskSensitiveBody_PassthroughNonJSON(t *testing.T) {
	raw := `plain text body with password=secret`
	out := maskSensitiveBody([]byte(raw))
	if string(out) != raw {
		t.Errorf("non-JSON body should pass through unchanged")
	}
}

func TestMaskSensitiveBody_InvalidJSON(t *testing.T) {
	// Broken JSON should pass through, not panic.
	raw := `{"password":"x` // truncated
	out := maskSensitiveBody([]byte(raw))
	if string(out) != raw {
		t.Errorf("invalid JSON should pass through unchanged, got %s", string(out))
	}
}

func TestMaskSensitiveBody_Roundtrip(t *testing.T) {
	out := maskSensitiveBody([]byte(`{"password":"p"}`))
	var m map[string]any
	if err := json.Unmarshal(out, &m); err != nil {
		t.Fatalf("output must be valid JSON: %v", err)
	}
	if m["password"] != sensitiveReplacement {
		t.Errorf("password not redacted: %v", m["password"])
	}
}
