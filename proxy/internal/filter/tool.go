package filter

func FilterToolOutput(toolName, output string, rules *FilterRules) (string, []FilterAction) {
	switch toolName {
	case "bash", "shell":
		return FilterShell(output, rules)
	case "diff", "git_diff":
		return FilterDiff(output, rules)
	default:
		return FilterCode(output, rules)
	}
}
