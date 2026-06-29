package clawagent

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"regexp"
	"strings"
)

// Some LLMs (notably GLM-family models) occasionally emit tool calls as text
// in the message content using an XML-like convention instead of the structured
// OpenAI-compatible tool_calls field:
//
//	<tool_call>reply_permission
//	<arg_key>permissionID</arg_key>
//	<arg_value>5df63692-...</arg_value>
//	<arg_key>approved</arg_key>
//	<arg_value>true</arg_value>
//	</tool_call>
//
// GLM is inconsistent about the underscores — opening tags sometimes arrive
// as <toolcall>, <argkey>, <argvalue> (no underscore), while closing tags
// tend to keep the underscore. The regexes below treat the separator as
// optional so both forms are recognized.
//
// parseTextToolCalls scans content for these blocks and converts them to
// structured ToolCalls. The returned cleaned content has all matched blocks
// removed so it can be safely displayed to users. When no blocks are found,
// parsed is nil and cleaned is the original content unchanged.

var (
	// `[_]?` makes the separator underscore optional — tolerates both
	// <tool_call> and <toolcall> forms that GLM emits inconsistently.
	toolCallBlockRe = regexp.MustCompile(`(?is)<tool[_]?call>\s*(.*?)\s*</tool[_]?call>`)
	argKeyRe        = regexp.MustCompile(`(?is)<arg[_]?key>\s*(.*?)\s*</arg[_]?key>`)
	argValueRe      = regexp.MustCompile(`(?is)<arg[_]?value>\s*(.*?)\s*</arg[_]?value>`)

	// Sentinel pattern to detect suspicious-looking leaked XML even when the
	// block regex doesn't match (e.g. truncated or malformed output). Used
	// only for warning logs so we know to extend the parser.
	suspiciousLeakRe = regexp.MustCompile(`(?i)<[/]?tool[_]?call|<[/]?arg[_]?(?:key|value)`)

	// Defense-in-depth: strip orphan opening/closing tags that survive block
	// removal (e.g. when the model emits a truncated block with no closing
	// tag). Matches both underscore and no-underscore variants, with optional
	// self-closing slash. Anything caught here is logged so we can trace how
	// it escaped the block regex.
	orphanTagRe = regexp.MustCompile(`(?i)</?(?:tool[_]?call|arg[_]?(?:key|value))\s*/?>`)
)

// parseTextToolCalls extracts text-encoded tool calls from content.
// Returns the parsed calls (or nil) and the content with all blocks removed.
func parseTextToolCalls(content string) (parsed []ToolCall, cleaned string) {
	matches := toolCallBlockRe.FindAllStringSubmatchIndex(content, -1)
	if len(matches) == 0 {
		if suspiciousLeakRe.MatchString(content) {
			preview := content
			if len(preview) > 400 {
				preview = preview[:400] + "…"
			}
			slog.Warn("[tooltextparse] suspicious tool-call-like content detected but no block matched — regex may need extension",
				"contentLen", len(content), "preview", preview)
		}
		// Even without a complete block, strip any orphan tags so they never
		// reach the user. This is a no-op when content has no leaked tags.
		return nil, stripOrphanTags(content)
	}

	for _, m := range matches {
		body := content[m[2]:m[3]]
		tc := parseToolCallBody(body)
		if tc.Function.Name != "" {
			parsed = append(parsed, tc)
		}
	}

	cleaned = toolCallBlockRe.ReplaceAllString(content, "")
	cleaned = stripOrphanTags(cleaned)
	// Tidy up leftover blank lines from removal so the displayed text is clean.
	cleaned = regexp.MustCompile(`\n{3,}`).ReplaceAllString(cleaned, "\n\n")
	cleaned = strings.TrimSpace(cleaned)
	return parsed, cleaned
}

// stripOrphanTags removes any leftover <tool...> / <arg...> tags that escaped
// block-level removal. Logs when it actually strips something so we can trace
// how the content leaked past the block regex.
func stripOrphanTags(content string) string {
	if !orphanTagRe.MatchString(content) {
		return content
	}
	stripped := orphanTagRe.ReplaceAllString(content, "")
	slog.Warn("[tooltextparse] stripped orphan tool-call tags from content",
		"originalLen", len(content), "strippedLen", len(stripped))
	return stripped
}

// parseToolCallBody parses a single tool_call body. The first non-empty line is
// the tool name; subsequent <arg_key>/<arg_value> pairs become JSON arguments.
func parseToolCallBody(body string) ToolCall {
	lines := strings.Split(body, "\n")
	name := ""
	for _, ln := range lines {
		ln = strings.TrimSpace(ln)
		if ln != "" {
			name = ln
			break
		}
	}

	args := buildArgMap(body)

	argsJSON, _ := json.Marshal(args)

	return ToolCall{
		ID:   fmt.Sprintf("text_%d", hashStr(name+string(argsJSON))),
		Type: "function",
		Function: ToolCallFunction{
			Name:      name,
			Arguments: string(argsJSON),
		},
	}
}

// buildArgMap pairs <arg_key>...</arg_key> and <arg_value>...</arg_value> in
// document order. Keys and values are matched positionally — first key with
// first value, etc. Extra unpaired values are ignored; extra keys map to "".
func buildArgMap(body string) map[string]any {
	keys := argKeyRe.FindAllStringSubmatch(body, -1)
	values := argValueRe.FindAllStringSubmatch(body, -1)

	m := make(map[string]any, len(keys))
	n := len(keys)
	if len(values) < n {
		n = len(values)
	}
	for i := 0; i < len(keys); i++ {
		key := strings.TrimSpace(keys[i][1])
		if i < n {
			m[key] = strings.TrimSpace(values[i][1])
		} else {
			m[key] = ""
		}
	}
	return m
}

// hashStr returns a small non-negative integer used to derive a stable ID for
// text-derived tool calls. Not cryptographic — only needs to be deterministic
// so the same name+args map to the same ID within one call.
func hashStr(s string) uint32 {
	const fnvInit uint32 = 2166136261
	h := fnvInit
	for i := 0; i < len(s); i++ {
		h ^= uint32(s[i])
		h *= 16777619
	}
	return h
}
