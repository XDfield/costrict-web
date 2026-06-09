package filter

import (
	"bufio"
	"encoding/json"
	"io"
	"strings"
)

type SSEEvent struct {
	ID    string
	Event string
	Data  string
}

type SSEFilterResult struct {
	Event string
	Data  string
}

type deltaTracker struct {
	buf         strings.Builder
	fenceCount  int
	suppressing bool
}

func FilterSSEStream(reader io.Reader, writer io.Writer, rules *FilterRules, callback func(*AuditSummary)) error {
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)

	var event SSEEvent
	trackers := make(map[string]*deltaTracker)

	for scanner.Scan() {
		line := scanner.Text()

		if line == "" {
			if event.Data != "" {
				results := processSSEEventWithTrackers(event, rules, trackers)
				for _, result := range results {
					writeSSEEvent(writer, result)
					if callback != nil && result.Summary != nil {
						callback(result.Summary)
					}
				}
			}
			if _, err := io.WriteString(writer, "\n"); err != nil {
				return err
			}
			if flusher, ok := writer.(interface{ Flush() }); ok {
				flusher.Flush()
			}
			event = SSEEvent{}
			continue
		}

		if strings.HasPrefix(line, "id:") {
			event.ID = strings.TrimSpace(strings.TrimPrefix(line, "id:"))
		} else if strings.HasPrefix(line, "event:") {
			event.Event = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
		} else if strings.HasPrefix(line, "data:") {
			data := line[len("data:"):]
			if len(data) > 0 && data[0] == ' ' {
				data = data[1:]
			}
			if event.Data == "" {
				event.Data = data
			} else {
				event.Data += "\n" + data
			}
		} else if strings.HasPrefix(line, ":") {
			if _, err := io.WriteString(writer, line+"\n"); err != nil {
				return err
			}
		}
	}

	return scanner.Err()
}

type sseProcessResult struct {
	Event   string
	Data    string
	Summary *AuditSummary
}

func processSSEEventWithTrackers(event SSEEvent, rules *FilterRules, trackers map[string]*deltaTracker) []*sseProcessResult {
	if event.Data == "" {
		return []*sseProcessResult{{Event: event.Event, Data: event.Data}}
	}

	eventName := event.Event
	if eventName == "" {
		var peek struct {
			Type string `json:"type"`
		}
		if err := json.Unmarshal([]byte(event.Data), &peek); err == nil && peek.Type != "" {
			eventName = peek.Type
		} else {
			eventName = "message"
		}
	}

	switch eventName {
	case "message.part.updated":
		return []*sseProcessResult{processPartUpdatedEvent(event.Data, rules)}
	case "message.part.delta":
		return processDeltaEvent(event.Data, rules, trackers)
	default:
		return []*sseProcessResult{{Event: event.Event, Data: event.Data}}
	}
}

func processDeltaEvent(data string, rules *FilterRules, trackers map[string]*deltaTracker) []*sseProcessResult {
	var envelope map[string]interface{}
	if err := json.Unmarshal([]byte(data), &envelope); err != nil {
		return []*sseProcessResult{{Data: data}}
	}

	props, _ := envelope["properties"].(map[string]interface{})
	if props == nil {
		return []*sseProcessResult{{Data: data}}
	}

	field, _ := props["field"].(string)
	if field != "text" {
		return []*sseProcessResult{{Data: data}}
	}

	partID, _ := props["partID"].(string)
	if partID == "" {
		return []*sseProcessResult{{Data: data}}
	}

	deltaText, _ := props["delta"].(string)

	tracker := trackers[partID]
	if tracker == nil {
		tracker = &deltaTracker{}
		trackers[partID] = tracker
	}

	tracker.buf.WriteString(deltaText)
	fullText := tracker.buf.String()
	newFenceCount := countFences(fullText)

	if newFenceCount > tracker.fenceCount {
		wasEven := tracker.fenceCount%2 == 0
		tracker.fenceCount = newFenceCount

		if wasEven {
			tracker.suppressing = true
			return nil
		}

		tracker.suppressing = false
		lang := extractFenceLanguage(fullText)
		filtered := "\n```filtered-" + lang + "\n[code filtered]\n```\n"
		props["delta"] = filtered
		result, _ := json.Marshal(envelope)
		summary := &AuditSummary{
			Filtered:           true,
			CodeBlocksTotal:    1,
			CodeBlocksFiltered: 1,
		}
		return []*sseProcessResult{{Data: string(result), Summary: summary}}
	}

	if tracker.suppressing {
		return nil
	}

	return []*sseProcessResult{{Data: data}}
}

func countFences(s string) int {
	count := 0
	i := 0
	for i < len(s) {
		idx := strings.Index(s[i:], "```")
		if idx == -1 {
			break
		}
		count++
		i += idx + 3
	}
	return count
}

func extractFenceLanguage(fullText string) string {
	i := 0
	for i < len(fullText) {
		idx := strings.Index(fullText[i:], "```")
		if idx == -1 {
			break
		}
		after := fullText[i+idx+3:]
		if len(after) > 0 && after[0] == '`' {
			i = i + idx + 3
			continue
		}
		newlineIdx := strings.Index(after, "\n")
		if newlineIdx == -1 {
			return strings.TrimSpace(after)
		}
		return strings.TrimSpace(after[:newlineIdx])
	}
	return ""
}

func processSSEEvent(event SSEEvent, rules *FilterRules) *sseProcessResult {
	if event.Data == "" {
		return &sseProcessResult{Event: event.Event, Data: event.Data}
	}

	eventName := event.Event
	if eventName == "" {
		var peek struct {
			Type string `json:"type"`
		}
		if err := json.Unmarshal([]byte(event.Data), &peek); err == nil && peek.Type != "" {
			eventName = peek.Type
		} else {
			eventName = "message"
		}
	}

	switch eventName {
	case "message.part.updated":
		return processPartUpdatedEvent(event.Data, rules)
	default:
		return &sseProcessResult{Event: event.Event, Data: event.Data}
	}
}

func processPartUpdatedEvent(data string, rules *FilterRules) *sseProcessResult {
	var envelope map[string]interface{}
	if err := json.Unmarshal([]byte(data), &envelope); err != nil {
		return &sseProcessResult{Data: data}
	}

	properties, _ := envelope["properties"].(map[string]interface{})
	if properties == nil {
		return &sseProcessResult{Data: data}
	}

	part, _ := properties["part"].(map[string]interface{})
	if part == nil {
		return &sseProcessResult{Data: data}
	}

	summary := &AuditSummary{}
	filtered, actions := FilterPart(part, rules)

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

	properties["part"] = filtered

	result, err := json.Marshal(envelope)
	if err != nil {
		return &sseProcessResult{Data: data}
	}

	return &sseProcessResult{Data: string(result), Summary: summary}
}

func writeSSEEvent(writer io.Writer, result *sseProcessResult) {
	if result.Event != "" {
		io.WriteString(writer, "event: "+result.Event+"\n")
	}
	if result.Data != "" {
		for _, line := range strings.Split(result.Data, "\n") {
			io.WriteString(writer, "data: "+line+"\n")
		}
	}
}
