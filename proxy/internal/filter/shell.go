package filter

func FilterShell(output string, rules *FilterRules) (string, []FilterAction) {
	if output == "" {
		return output, nil
	}

	threshold := rules.ShellCharThreshold
	if threshold < 0 {
		return output, nil
	}

	if len(output) <= threshold {
		return output, nil
	}

	return rules.RedactPlaceholder, []FilterAction{{
		Type:     "shell",
		Strategy: rules.DefaultStrategy,
		Reason:   "tool_shell_threshold",
		Original: output,
	}}
}
