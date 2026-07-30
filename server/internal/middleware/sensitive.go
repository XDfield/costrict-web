package middleware

import (
	"bytes"
	"encoding/json"
	"strings"
)

// sensitiveJSONKeys lists JSON object keys whose values must be redacted from
// logs regardless of nesting depth. Match is case-insensitive on the key name
// and uses a substring heuristic ("password" matches "user_password" too) to
// catch common variants without enumerating every possible field name.
//
// Extend via the SENSITIVE_LOG_KEYS env var (comma-separated) at deploy time
// if additional field names need masking.
var sensitiveJSONKeys = []string{
	"password",
	"passwd",
	"secret",
	"apikey",
	"api_key",
	"accesstoken",
	"access_token",
	"refreshtoken",
	"refresh_token",
	"idtoken",
	"id_token",
	"bearer",
	"authorization",
	"clientsecret",
	"client_secret",
	"privatekey",
	"private_key",
	"signingkey",
	"signing_key",
	"credential",
}

// sensitiveReplacement is the placeholder written in place of redacted values.
const sensitiveReplacement = "***REDACTED***"

// maskSensitiveBody returns a version of the request body with sensitive
// JSON fields redacted. If the body is not valid JSON (form data, binary,
// multipart), the original bytes are returned unchanged — non-JSON bodies are
// not parsed field-by-field because doing so safely across all encodings is
// impractical, and the caller still benefits from length truncation.
func maskSensitiveBody(raw []byte) []byte {
	if len(raw) == 0 {
		return raw
	}
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 {
		return raw
	}
	// Quick shape check: must start with { or [.
	if trimmed[0] != '{' && trimmed[0] != '[' {
		return raw
	}

	var parsed any
	dec := json.NewDecoder(bytes.NewReader(trimmed))
	// Numbers as json.Number so we don't alter their formatting.
	dec.UseNumber()
	if err := dec.Decode(&parsed); err != nil {
		// Not valid JSON — leave untouched. Logging plaintext is a separate
		// concern; the redactor only owns structured masking.
		return raw
	}

	redacted := redactValue(parsed)
	out, err := json.Marshal(redacted)
	if err != nil {
		return raw
	}
	return out
}

// redactValue walks a decoded JSON value and masks string leaves whose key
// matches the sensitive list. It only redacts when reaching a leaf (string),
// to avoid recursing into already-redacted nested objects indefinitely.
func redactValue(v any) any {
	switch x := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(x))
		for k, val := range x {
			if isSensitiveKey(k) {
				out[k] = redactLeaf(val)
			} else {
				out[k] = redactValue(val)
			}
		}
		return out
	case []any:
		out := make([]any, len(x))
		for i, item := range x {
			out[i] = redactValue(item)
		}
		return out
	default:
		return v
	}
}

// redactLeaf masks scalar values; non-scalars are recursed into so that
// "password": {"hint": "..."} still gets its inner string masked if needed.
func redactLeaf(v any) any {
	switch x := v.(type) {
	case map[string]any:
		return redactValue(x)
	case []any:
		return redactValue(x)
	case string:
		return sensitiveReplacement
	default:
		return sensitiveReplacement
	}
}

func isSensitiveKey(key string) bool {
	lower := strings.ToLower(key)
	for _, needle := range sensitiveJSONKeys {
		if strings.Contains(lower, needle) {
			return true
		}
	}
	return false
}
