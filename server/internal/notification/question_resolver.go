package notification

import (
	"log"
	"strconv"
	"strings"
)

// ResolveQuestionAnswer parses the action string (e.g. "select:opt_0") and
// actionData to produce the answers field that cs-cloud expects: string[][].
// Each inner array contains the selected option label strings for one question.
func ResolveQuestionAnswer(action string, actionData map[string]any) [][]string {
	var optIndices []int
	if strings.HasPrefix(action, "select:opt_") {
		idxStr := action[len("select:opt_"):]
		for _, part := range strings.Split(idxStr, ",") {
			if n, err := strconv.Atoi(strings.TrimPrefix(part, "opt_")); err == nil {
				optIndices = append(optIndices, n)
			}
		}
	}
	log.Printf("[action-callback] resolveQuestionAnswer: action=%q optIndices=%v", action, optIndices)

	questionsRaw, _ := actionData["questions"].([]any)
	if len(questionsRaw) == 0 {
		log.Printf("[action-callback] resolveQuestionAnswer: no questions in actionData")
		return [][]string{}
	}

	answers := make([][]string, len(questionsRaw))
	for qi, qRaw := range questionsRaw {
		q, _ := qRaw.(map[string]any)
		optionsRaw, _ := q["options"].([]any)

		var labels []string
		if qi == 0 && len(optIndices) > 0 {
			for _, idx := range optIndices {
				if idx >= 0 && idx < len(optionsRaw) {
					opt, _ := optionsRaw[idx].(map[string]any)
					if label, ok := opt["label"].(string); ok {
						labels = append(labels, label)
					}
				}
			}
		}
		if len(labels) == 0 && len(optionsRaw) > 0 {
			opt, _ := optionsRaw[0].(map[string]any)
			if label, ok := opt["label"].(string); ok {
				labels = []string{label}
			}
		}
		answers[qi] = labels
	}
	log.Printf("[action-callback] resolveQuestionAnswer: resolved answers=%v", answers)
	return answers
}
