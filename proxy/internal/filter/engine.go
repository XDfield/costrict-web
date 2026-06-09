package filter

func FilterEngine(contentType, content string, rules *FilterRules) (string, []FilterAction) {
	switch contentType {
	case "markdown":
		return FilterMarkdown(content, rules)
	case "code":
		return FilterCode(content, rules)
	case "diff":
		return FilterDiff(content, rules)
	case "shell":
		return FilterShell(content, rules)
	default:
		return content, nil
	}
}
