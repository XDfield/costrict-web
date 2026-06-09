package filter

import (
	"regexp"
	"strings"
)

var diffHunkHeaderRe = regexp.MustCompile(`^@@ -(\d+)(?:,\d+)? \+(\d+)(?:,\d+)? @@`)

type DiffLine struct {
	Type     string
	Content  string
	LineNum  string
}

func ParseDiff(diff string) []DiffLine {
	lines := strings.Split(diff, "\n")
	var result []DiffLine
	for _, line := range lines {
		if len(line) == 0 {
			result = append(result, DiffLine{Type: "empty", Content: ""})
			continue
		}
		if strings.HasPrefix(line, "--- ") || strings.HasPrefix(line, "+++ ") {
			result = append(result, DiffLine{Type: "header", Content: line})
			continue
		}
		if strings.HasPrefix(line, "@@ ") {
			result = append(result, DiffLine{Type: "hunk", Content: line})
			continue
		}
		switch line[0] {
		case '+':
			result = append(result, DiffLine{Type: "add", Content: line})
		case '-':
			result = append(result, DiffLine{Type: "remove", Content: line})
		case ' ':
			result = append(result, DiffLine{Type: "context", Content: line})
		default:
			result = append(result, DiffLine{Type: "other", Content: line})
		}
	}
	return result
}

func FilterDiff(diff string, rules *FilterRules) (string, []FilterAction) {
	if diff == "" {
		return diff, nil
	}

	lines := ParseDiff(diff)
	placeholder := rules.RedactPlaceholder
	filtered := make([]string, len(lines))
	var actions []FilterAction
	hasFiltered := false

	for i, line := range lines {
		switch line.Type {
		case "add", "remove":
			if rules.PreserveFilePaths {
				filtered[i] = string(line.Content[0:0]) + placeholder
			} else {
				filtered[i] = placeholder
			}
			hasFiltered = true
		case "hunk", "header", "context", "empty", "other":
			filtered[i] = line.Content
		}
	}

	if !hasFiltered {
		return diff, nil
	}

	result := strings.Join(filtered, "\n")
	actions = append(actions, FilterAction{
		Type:     "diff",
		Strategy: rules.DefaultStrategy,
		Reason:   "runtime_diff",
		Original: diff,
	})

	return result, actions
}
