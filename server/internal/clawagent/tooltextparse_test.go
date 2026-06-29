package clawagent

import (
	"testing"
)

func TestParseTextToolCalls_Variants(t *testing.T) {
	cases := []struct {
		name    string
		content string
		wantN   int
		wantC   string
	}{
		{
			name:    "standard with underscores",
			content: "<tool_call>reply_permission\n<arg_key>permissionID</arg_key>\n<arg_value>b3de</arg_value>\n<arg_key>approved</arg_key>\n<arg_value>true</arg_value>\n</tool_call>",
			wantN:   1,
			wantC:   "",
		},
		{
			name:    "no underscores",
			content: "<toolcall>replypermission\n<argkey>permissionID</argkey>\n<argvalue>b3de</argvalue>\n<argkey>approved</argkey>\n<argvalue>true</argvalue>\n</toolcall>",
			wantN:   1,
			wantC:   "",
		},
		{
			name:    "mixed: opening no underscore, closing with",
			content: "<toolcall>replypermission\n<argkey>permissionID</argkey>\n<argvalue>b3de</argvalue>\n<argkey>approved</argkey>\n<argvalue>true</argvalue>\n</tool_call>",
			wantN:   1,
			wantC:   "",
		},
		{
			name:    "preamble + block",
			content: "好的，已经批准了。\n<tool_call>reply_permission\n<arg_key>permissionID</arg_key>\n<arg_value>abc</arg_value>\n<arg_key>approved</arg_key>\n<arg_value>true</arg_value>\n</tool_call>",
			wantN:   1,
			wantC:   "好的，已经批准了。",
		},
		{
			name:    "truncated no closing - orphan strip leaves inner text",
			content: "<tool_call>reply_permission\n<arg_key>x</arg_key>",
			wantN:   0,
			wantC:   "reply_permission\nx",
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			parsed, cleaned := parseTextToolCalls(c.content)
			if len(parsed) != c.wantN {
				t.Errorf("parsed count = %d, want %d", len(parsed), c.wantN)
			}
			if cleaned != c.wantC {
				t.Errorf("cleaned = %q, want %q", cleaned, c.wantC)
			}
		})
	}
}
