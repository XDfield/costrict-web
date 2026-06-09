package filter

import "encoding/json"

func FilterPart(partJSON map[string]interface{}, rules *FilterRules) (map[string]interface{}, []FilterAction) {
	partType, _ := partJSON["type"].(string)

	switch partType {
	case "text":
		return filterTextPart(partJSON, rules)
	case "tool":
		return filterToolPart(partJSON, rules)
	case "tool-result":
		return filterToolResultPart(partJSON, rules)
	case "reasoning":
		return filterReasoningPart(partJSON, rules)
	case "snapshot":
		return filterSnapshotPart(partJSON, rules)
	default:
		return partJSON, nil
	}
}

func filterTextPart(part map[string]interface{}, rules *FilterRules) (map[string]interface{}, []FilterAction) {
	text, _ := part["text"].(string)
	if text == "" {
		return part, nil
	}

	filtered, actions := FilterMarkdown(text, rules)
	if filtered != text {
		part["text"] = filtered
	}

	return part, actions
}

func filterToolPart(part map[string]interface{}, rules *FilterRules) (map[string]interface{}, []FilterAction) {
	toolName, _ := part["tool"].(string)

	state, _ := part["state"].(map[string]interface{})
	if state == nil {
		return part, nil
	}

	input, _ := state["input"].(map[string]interface{})
	inputJSON, _ := json.Marshal(input)
	filePath := extractToolFilePath(toolName, input)

	trackAction := FilterAction{
		Type:     "tool_output",
		ToolName: toolName,
		Path:     filePath,
		Input:    string(inputJSON),
	}

	status, _ := state["status"].(string)
	if status != "completed" {
		return part, []FilterAction{trackAction}
	}

	output, _ := state["output"].(string)
	if output == "" {
		return part, []FilterAction{trackAction}
	}

	filtered, actions := FilterToolOutput(toolName, output, rules)

	if filtered != output {
		state["output"] = filtered
		trackAction.Filtered = true

		metadata, _ := state["metadata"].(map[string]interface{})
		if metadata == nil {
			metadata = make(map[string]interface{})
		}
		metadata["_filtered"] = map[string]interface{}{
			"strategy":     rules.DefaultStrategy,
			"reason":       "tool_output",
			"toolName":     toolName,
			"originalSize": len(output),
		}
		state["metadata"] = metadata
		trackAction.Strategy = rules.DefaultStrategy
	}

	return part, append(actions, trackAction)
}

func extractToolFilePath(toolName string, input map[string]interface{}) string {
	if input == nil {
		return ""
	}

	switch toolName {
	case "read", "write", "edit", "read_file", "write_file", "edit_file", "file_read", "file_write", "file_edit", "apply_patch":
		if p, ok := input["filePath"].(string); ok {
			return p
		}
		if p, ok := input["file_path"].(string); ok {
			return p
		}
		if p, ok := input["path"].(string); ok {
			return p
		}
	case "bash", "shell":
		if cmd, ok := input["command"].(string); ok {
			return cmd
		}
	case "grep", "search", "glob", "list":
		if p, ok := input["path"].(string); ok {
			return p
		}
		if p, ok := input["directory"].(string); ok {
			return p
		}
	}
	return ""
}

func filterToolResultPart(part map[string]interface{}, rules *FilterRules) (map[string]interface{}, []FilterAction) {
	content := part["content"]

	if arr, ok := content.([]interface{}); ok {
		return filterToolResultArrayContent(part, arr, rules)
	}

	contentStr, _ := content.(string)
	if contentStr == "" {
		return part, nil
	}

	filtered, actions := FilterCode(contentStr, rules)
	if filtered != contentStr {
		part["content"] = filtered

		metadata, _ := part["metadata"].(map[string]interface{})
		if metadata == nil {
			metadata = make(map[string]interface{})
		}
		metadata["_filtered"] = map[string]interface{}{
			"strategy":     rules.DefaultStrategy,
			"reason":       "tool_result",
			"originalSize": len(contentStr),
		}
		part["metadata"] = metadata
	}

	return part, actions
}

func filterToolResultArrayContent(part map[string]interface{}, parts []interface{}, rules *FilterRules) (map[string]interface{}, []FilterAction) {
	var allActions []FilterAction
	modified := false

	for i, item := range parts {
		itemMap, ok := item.(map[string]interface{})
		if !ok {
			continue
		}
		text, _ := itemMap["text"].(string)
		if text == "" {
			continue
		}

		filtered, actions := FilterCode(text, rules)
		allActions = append(allActions, actions...)
		if filtered != text {
			itemMap["text"] = filtered
			parts[i] = itemMap
			modified = true
		}
	}

	if modified {
		part["content"] = parts

		metadata, _ := part["metadata"].(map[string]interface{})
		if metadata == nil {
			metadata = make(map[string]interface{})
		}
		metadata["_filtered"] = map[string]interface{}{
			"strategy": rules.DefaultStrategy,
			"reason":   "tool_result",
		}
		part["metadata"] = metadata
	}

	return part, allActions
}

func filterReasoningPart(part map[string]interface{}, rules *FilterRules) (map[string]interface{}, []FilterAction) {
	if rules.ReasoningThreshold < 0 {
		return part, nil
	}

	text, _ := part["text"].(string)
	if len(text) <= rules.ReasoningThreshold {
		return part, nil
	}

	filtered := rules.RedactPlaceholder
	part["text"] = filtered

	metadata, _ := part["metadata"].(map[string]interface{})
	if metadata == nil {
		metadata = make(map[string]interface{})
	}
	metadata["_filtered"] = map[string]interface{}{
		"strategy":     rules.DefaultStrategy,
		"reason":       "reasoning_threshold",
		"originalSize": len(text),
	}
	part["metadata"] = metadata

	return part, []FilterAction{{
		Type:     "reasoning",
		Strategy: rules.DefaultStrategy,
		Reason:   "reasoning_threshold",
		Original: text,
	}}
}

func filterSnapshotPart(part map[string]interface{}, rules *FilterRules) (map[string]interface{}, []FilterAction) {
	snapshot, _ := part["snapshot"].(string)
	if snapshot == "" {
		return part, nil
	}

	filtered, actions := FilterCode(snapshot, rules)
	if filtered != snapshot {
		part["snapshot"] = filtered
	}

	return part, actions
}
