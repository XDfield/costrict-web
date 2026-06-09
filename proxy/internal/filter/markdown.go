package filter

import (
	"regexp"
	"strings"
)

var codeBlockRe = regexp.MustCompile("(?s)```(\\w*)\\n(.*?)```")

type CodeBlock struct {
	Language        string
	Content         string
	Full            string
	Start           int
	End             int
	ClosingIndent   string
}

func ExtractCodeBlocks(text string) []CodeBlock {
	matches := codeBlockRe.FindAllStringSubmatchIndex(text, -1)
	var blocks []CodeBlock
	for _, loc := range matches {
		lang := ""
		if loc[2] != -1 && loc[3] != -1 {
			lang = text[loc[2]:loc[3]]
		}
		content := ""
		if loc[4] != -1 && loc[5] != -1 {
			content = text[loc[4]:loc[5]]
		}

		closingIndent := ""
		if content != "" {
			lastNL := strings.LastIndex(content, "\n")
			if lastNL >= 0 && lastNL+1 < len(content) {
				afterNL := content[lastNL+1:]
				if strings.TrimSpace(afterNL) == "" {
					closingIndent = afterNL
				}
			}
		}

		blocks = append(blocks, CodeBlock{
			Language:      lang,
			Content:       content,
			Full:          text[loc[0]:loc[1]],
			Start:         loc[0],
			End:           loc[1],
			ClosingIndent: closingIndent,
		})
	}
	return blocks
}

func HasUnclosedCodeBlock(text string) bool {
	backtickRe := regexp.MustCompile("```")
	matches := backtickRe.FindAllStringIndex(text, -1)
	total := len(matches)

	closedCount := len(codeBlockRe.FindAllStringSubmatchIndex(text, -1))

	return total > closedCount*2
}

func FindUnclosedBlock(text string) (start, end int, lang string, found bool) {
	backtickRe := regexp.MustCompile("```")
	allBackticks := backtickRe.FindAllStringIndex(text, -1)
	closedBlocks := codeBlockRe.FindAllStringSubmatchIndex(text, -1)

	usedBackticks := make(map[int]bool)
	for _, block := range closedBlocks {
		for _, bk := range allBackticks {
			if bk[0] >= block[0] && bk[1] <= block[1] {
				usedBackticks[bk[0]] = true
			}
		}
	}

	for _, bk := range allBackticks {
		if usedBackticks[bk[0]] {
			continue
		}

		langRe := regexp.MustCompile("```(\\w*)")
		langMatch := langRe.FindStringSubmatchIndex(text[bk[0]:])
		if langMatch == nil {
			continue
		}

		lang = ""
		if langMatch[2] != -1 && langMatch[3] != -1 {
			lang = text[bk[0]+langMatch[2] : bk[0]+langMatch[3]]
		}

		return bk[0], len(text), lang, true
	}

	return 0, 0, "", false
}

func FilterMarkdown(text string, rules *FilterRules) (string, []FilterAction) {
	placeholder := rules.RedactPlaceholder

	closedBlocks := ExtractCodeBlocks(text)
	hasUnclosed := HasUnclosedCodeBlock(text)

	if len(closedBlocks) == 0 {
		if hasUnclosed {
			return filterUnclosedBlock(text, rules)
		}
		return text, nil
	}

	result := text
	var actions []FilterAction
	offset := 0

	for _, block := range closedBlocks {
		filtered := ApplyStrategy(block.Content, rules.DefaultStrategy, placeholder)
		if filtered != block.Content {
			replacement := "```"
			if rules.PreserveLanguageHint && block.Language != "" {
				replacement = "```filtered-" + block.Language
			} else {
				replacement = "```filtered"
			}

			if block.ClosingIndent != "" {
				indented := block.ClosingIndent + filtered
				replacement += "\n" + indented + "\n" + block.ClosingIndent + "```"
			} else {
				replacement += "\n" + filtered + "\n```"
			}

			newResult := result[:block.Start+offset] + replacement + result[block.End+offset:]
			offset += len(replacement) - len(block.Full)
			result = newResult

			actions = append(actions, FilterAction{
				Type:     "code_block",
				Strategy: rules.DefaultStrategy,
				Reason:   "code_block",
				Language: block.Language,
				Original: block.Content,
			})
		}
	}

	if hasUnclosed && len(closedBlocks) > 0 {
		afterClosed := result
		unclosedResult, unclosedActions := filterUnclosedBlock(afterClosed, rules)
		if len(unclosedActions) > 0 {
			result = unclosedResult
			actions = append(actions, unclosedActions...)
		}
	}

	return result, actions
}

func filterUnclosedBlock(text string, rules *FilterRules) (string, []FilterAction) {
	start, _, lang, found := FindUnclosedBlock(text)
	if !found {
		return text, nil
	}

	replacement := "```"
	if rules.PreserveLanguageHint && lang != "" {
		replacement = "```streaming-" + lang
	} else {
		replacement = "```streaming"
	}
	replacement += "\n```"

	result := text[:start] + replacement

	actions := []FilterAction{{
		Type:     "code_block_streaming",
		Strategy: "streaming",
		Reason:   "code_block_streaming",
		Language: lang,
	}}

	return result, actions
}
