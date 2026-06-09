package filter

import "strings"

func ApplyStrategy(content, strategy, placeholder string) string {
	switch strategy {
	case "redact":
		return placeholder
	case "strip":
		return ""
	case "mask":
		return maskContent(content)
	default:
		return placeholder
	}
}

func maskContent(content string) string {
	lines := strings.Split(content, "\n")
	masked := make([]string, len(lines))
	for i, line := range lines {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			masked[i] = ""
			continue
		}
		masked[i] = "***"
	}
	return strings.Join(masked, "\n")
}
