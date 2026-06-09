package filter

import (
	"encoding/json"
	"strings"
)

func ProcessResponse(path, method string, statusCode int, body []byte, rules *FilterRules) ([]byte, *AuditSummary) {
	if len(body) == 0 {
		return body, nil
	}

	if strings.Contains(path, "/runtime/files/content") && !strings.HasSuffix(path, "/meta") {
		return processRuntimeFileContent(body, rules)
	}
	if strings.Contains(path, "/runtime/diff/content") {
		return processRuntimeDiffContent(body, rules)
	}

	if strings.Contains(path, "/conversations/") {
		return processConversationResponse(body, rules)
	}

	return body, nil
}

type AuditSummary struct {
	ToolsCount         int
	CodeBlocksTotal    int
	CodeBlocksFiltered int
	Filtered           bool
	ToolActions        []FilterAction
}

func processConversationResponse(body []byte, rules *FilterRules) ([]byte, *AuditSummary) {
	var data json.RawMessage
	if err := json.Unmarshal(body, &data); err != nil {
		return body, nil
	}

	summary := &AuditSummary{}
	modified := processJSONTree(data, rules, summary)

	result, err := json.Marshal(modified)
	if err != nil {
		return body, nil
	}

	return result, summary
}

func processJSONTree(data json.RawMessage, rules *FilterRules, summary *AuditSummary) interface{} {
	var raw interface{}
	if err := json.Unmarshal(data, &raw); err != nil {
		return string(data)
	}

	return walkJSON(raw, rules, summary)
}

func walkJSON(v interface{}, rules *FilterRules, summary *AuditSummary) interface{} {
	switch val := v.(type) {
	case map[string]interface{}:
		if partType, ok := val["type"].(string); ok {
			if isPartType(partType) {
				filtered, actions := FilterPart(val, rules)
				for _, action := range actions {
					summary.Filtered = true
					if action.Type == "code_block" || action.Type == "code_block_streaming" {
						summary.CodeBlocksTotal++
						if action.Strategy != "streaming" {
							summary.CodeBlocksFiltered++
						}
					}
					if action.ToolName != "" {
						summary.ToolActions = append(summary.ToolActions, action)
						summary.ToolsCount++
					}
				}
				return filtered
			}
		}

		for k, child := range val {
			val[k] = walkJSON(child, rules, summary)
		}
		return val

	case []interface{}:
		for i, child := range val {
			val[i] = walkJSON(child, rules, summary)
		}
		return val

	default:
		return v
	}
}

func isPartType(t string) bool {
	switch t {
	case "text", "tool", "tool-result", "reasoning", "snapshot", "step-start", "patch":
		return true
	default:
		return false
	}
}

func processRuntimeFileContent(body []byte, rules *FilterRules) ([]byte, *AuditSummary) {
	var data map[string]interface{}
	if err := json.Unmarshal(body, &data); err != nil {
		return body, nil
	}

	content, _ := data["content"].(string)
	if content == "" {
		return body, nil
	}

	filtered, _ := FilterCode(content, rules)
	data["content"] = filtered
	data["_filtered"] = map[string]interface{}{
		"strategy":     rules.DefaultStrategy,
		"reason":       "runtime_file",
		"originalSize": len(content),
	}

	result, err := json.Marshal(data)
	if err != nil {
		return body, nil
	}

	return result, &AuditSummary{
		Filtered: true,
		ToolActions: []FilterAction{{
			Type:     "runtime_file",
			Strategy: rules.DefaultStrategy,
		}},
	}
}

func processRuntimeDiffContent(body []byte, rules *FilterRules) ([]byte, *AuditSummary) {
	var data map[string]interface{}
	if err := json.Unmarshal(body, &data); err != nil {
		return body, nil
	}

	summary := &AuditSummary{}
	modified := false

	if diff, _ := data["diff"].(string); diff != "" {
		filtered, _ := FilterDiff(diff, rules)
		if filtered != diff {
			data["diff"] = filtered
			modified = true
		}
	}

	if before, _ := data["before"].(string); before != "" {
		filtered, _ := FilterCode(before, rules)
		if filtered != before {
			data["before"] = filtered
			modified = true
		}
	}

	if after, _ := data["after"].(string); after != "" {
		filtered, _ := FilterCode(after, rules)
		if filtered != after {
			data["after"] = filtered
			modified = true
		}
	}

	if modified {
		data["_filtered"] = map[string]interface{}{
			"strategy": rules.DefaultStrategy,
			"reason":   "runtime_diff",
		}
		summary.Filtered = true
	}

	result, err := json.Marshal(data)
	if err != nil {
		return body, nil
	}

	return result, summary
}
