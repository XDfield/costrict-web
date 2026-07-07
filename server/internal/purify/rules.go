package purify

import (
	"fmt"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"
)

// --- 1. Standardize rule ---

// standardizeRule normalizes input: Unicode NFC, control char stripping,
// whitespace collapsing, CRLF → LF, length cap. Idempotent — running twice
// yields the same output as running once.
type standardizeRule struct {
	maxLength int
}

func (r *standardizeRule) Apply(res *Result) {
	s := res.Cleaned

	// Unicode NFC would require golang.org/x/text/unicode/norm; for now we
	// rely on Go's default UTF-8 handling which is sufficient for whitespace
	// and control char cleanup. NFC normalization can be added later if
	// needed.

	// CRLF / CR → LF (single pass before control stripping so we don't nuke
	// the LF we just produced).
	s = strings.ReplaceAll(s, "\r\n", "\n")
	s = strings.ReplaceAll(s, "\r", "\n")

	// Strip control chars except \n \t (preserved above).
	var b strings.Builder
	b.Grow(len(s))
	for _, ch := range s {
		if isControl(ch) {
			continue
		}
		b.WriteRune(ch)
	}
	s = b.String()

	// Collapse runs of spaces/tabs into one space, but preserve newlines as
	// structural. Empty lines collapsed to single empty line.
	lines := strings.Split(s, "\n")
	for i, line := range lines {
		// Trim leading/trailing whitespace on each line.
		line = strings.TrimFunc(line, func(r rune) bool {
			return r != '\n' && unicode.IsSpace(r)
		})
		// Collapse internal whitespace runs into one space.
		var lb strings.Builder
		lb.Grow(len(line))
		inWS := false
		for _, ch := range line {
			if ch == ' ' || ch == '\t' {
				if !inWS {
					lb.WriteRune(' ')
					inWS = true
				}
				continue
			}
			inWS = false
			lb.WriteRune(ch)
		}
		lines[i] = lb.String()
	}
	s = strings.Join(lines, "\n")

	// Trim leading/trailing blank lines + whitespace.
	s = strings.TrimSpace(s)

	// Length cap. Count by UTF-8 runes to be locale-fair. BLOCK overlong
	// inputs rather than truncating — the user must learn to keep messages
	// short, and the LLM should never see truncated content.
	if r.maxLength > 0 {
		if runeCount := utf8.RuneCountInString(s); runeCount > r.maxLength {
			res.Blocked = true
			res.BlockReason = fmt.Sprintf("input length %d exceeds maximum %d", runeCount, r.maxLength)
			res.Cleaned = ""
			return
		}
	}

	res.Cleaned = s
}

// --- 2. Redact rule ---

// redactRule replaces known secret patterns with [REDACTED:type] markers.
// Patterns are intentionally conservative — only flagging shapes that have
// near-zero legitimate use as user input.
type redactRule struct{}

// secretPattern pairs a regex with a human-friendly type label.
type secretPattern struct {
	re    *regexp.Regexp
	label string
}

var secretPatterns = []secretPattern{
	// AWS Access Key ID: AKIA followed by 16 base32 chars.
	{regexp.MustCompile(`AKIA[0-9A-Z]{16}`), "aws-access-key"},
	// AWS Secret Access Key in quoted/assigned context (avoid false positives
	// from random 40-char strings by requiring key=value or JSON shape).
	{regexp.MustCompile(`(?i)(aws_secret_access_key|secretkey|secret_key)\s*[:=]\s*["']?[A-Za-z0-9/+=]{40}["']?`), "aws-secret-key"},
	// OpenAI API key (old + new formats).
	{regexp.MustCompile(`sk-[a-zA-Z0-9]{20,}`), "openai-key"},
	// GitHub PAT.
	{regexp.MustCompile(`ghp_[A-Za-z0-9]{36}`), "github-pat"},
	{regexp.MustCompile(`github_pat_[A-Za-z0-9_]{82}`), "github-pat"},
	// Slack tokens.
	{regexp.MustCompile(`xox[abps]-[A-Za-z0-9-]{10,}`), "slack-token"},
	// JWT (three base64url segments, dotted).
	{regexp.MustCompile(`eyJ[A-Za-z0-9_-]{8,}\.eyJ[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}`), "jwt"},
	// Generic Bearer token in Authorization header context.
	{regexp.MustCompile(`(?i)authorization\s*[:=]\s*["']?bearer\s+[A-Za-z0-9._-]{20,}`), "bearer-token"},
}

func (r *redactRule) Apply(res *Result) {
	s := res.Cleaned
	for _, p := range secretPatterns {
		matches := p.re.FindAllString(s, -1)
		if len(matches) == 0 {
			continue
		}
		s = p.re.ReplaceAllString(s, "[REDACTED:"+p.label+"]")
		res.addRedaction(p.label)
		if len(matches) > 1 {
			// addRedaction increments count; subtract the 1 already added and
			// add the actual count.
			for i := 1; i < len(matches); i++ {
				res.addRedaction(p.label)
			}
		}
	}
	res.Cleaned = s
}

// --- 3. Injection detection rule (BLOCKING) ---

// injectionRule scans for prompt-injection patterns. BLOCKS the input on
// first match — non-blocking warn-only was retired because injection
// attempts are cheap to reject and risky to permit. The matched pattern is
// also appended to Warnings so callers can log the reason.
type injectionRule struct{}

var injectionPatterns = []*regexp.Regexp{
	// "Ignore previous instructions" / "disregard all prior rules" etc.
	regexp.MustCompile(`(?i)\b(ignore|disregard|forget|override)\s+(all\s+)?(previous|prior|earlier|above)\s+(instructions?|prompts?|rules?|messages?|system)\b`),
	// Fake role markers: <system>, </assistant>, etc.
	regexp.MustCompile(`(?i)</?\s*(system|assistant|developer|tool)\s*>`),
	regexp.MustCompile(`(?i)\[(?:system|assistant|developer|tool)\s*(?:message|prompt|instruction)\s*\]`),
	// "You are now a ..." role-hijack.
	regexp.MustCompile(`(?i)\byou\s+are\s+(now|actually|really)\s+(a|an)\s+`),
	// "From now on, ..." directive override.
	regexp.MustCompile(`(?i)\bfrom\s+now\s+on\b.*\b(always|never|must|do\s+not)\b`),
}

// largeBase64Pattern detects long base64-encoded blobs (>200 chars), which
// often indicate encoded payloads designed to bypass text-based filters.
var largeBase64Pattern = regexp.MustCompile(`[A-Za-z0-9+/]{200,}={0,2}`)

// largeHexPattern detects long hex strings (>100 chars).
var largeHexPattern = regexp.MustCompile(`\b[0-9a-fA-F]{100,}\b`)

func (r *injectionRule) Apply(res *Result) {
	s := res.Cleaned
	for _, p := range injectionPatterns {
		if p.MatchString(s) {
			res.Blocked = true
			res.BlockReason = "suspected prompt injection: matched " + p.String()
			res.addWarning(res.BlockReason)
			res.Cleaned = ""
			return
		}
	}
	if largeBase64Pattern.MatchString(s) {
		res.Blocked = true
		res.BlockReason = "large base64-encoded blob detected (possible payload)"
		res.addWarning(res.BlockReason)
		res.Cleaned = ""
		return
	}
	if largeHexPattern.MatchString(s) {
		res.Blocked = true
		res.BlockReason = "large hex string detected (possible payload)"
		res.addWarning(res.BlockReason)
		res.Cleaned = ""
		return
	}
}
