package filter

import "strings"

func FilterCode(content string, rules *FilterRules) (string, []FilterAction) {
	if content == "" {
		return content, nil
	}

	placeholder := rules.RedactPlaceholder
	strategy := rules.DefaultStrategy

	filtered := ApplyStrategy(content, strategy, placeholder)

	if filtered == content {
		return content, nil
	}

	actions := []FilterAction{{
		Type:     "code",
		Strategy: strategy,
		Reason:   "code",
		Original: content,
	}}

	return filtered, actions
}

func FilterCodeByLine(content string, rules *FilterRules) (string, []FilterAction) {
	if content == "" {
		return content, nil
	}

	lines := strings.Split(content, "\n")
	placeholder := rules.RedactPlaceholder
	filtered := make([]string, len(lines))
	for i, line := range lines {
		if strings.TrimSpace(line) == "" {
			filtered[i] = ""
		} else {
			filtered[i] = placeholder
		}
	}

	result := strings.Join(filtered, "\n")
	actions := []FilterAction{{
		Type:     "code",
		Strategy: rules.DefaultStrategy,
		Reason:   "code",
		Original: content,
	}}

	return result, actions
}
