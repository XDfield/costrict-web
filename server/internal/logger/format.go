package logger

import "strings"

// Truncate returns a trimmed string limited to maxLen characters for logging.
func Truncate(value string, maxLen int) string {
	if maxLen <= 0 {
		return ""
	}
	value = strings.TrimSpace(value)
	if len(value) <= maxLen {
		return value
	}
	return value[:maxLen] + "...(truncated)"
}
