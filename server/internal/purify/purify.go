// Package purify provides a defense-in-depth input purification layer for
// user-supplied messages before they enter the AI runtime. It runs only on
// the user → system direction (inbound from IM channels); AI-generated
// output is NOT purified.
//
// The rule-based pipeline has two stages, applied in fixed order:
//
//  1. Standardize — strip control chars, collapse whitespace, enforce length
//     cap by BLOCKING (not truncating). Always applied; produces a normalized
//     `Cleaned`.
//
//  2. Detect injection — scans for prompt injection patterns ("ignore
//     previous instructions", fake role markers, large base64 payloads,
//     role-hijack attempts). Matching any pattern BLOCKS the input.
//
// Hard blocking triggers when: input is empty after standardization, length
// exceeds the cap, or an injection pattern matches. Callers can add custom
// rules via WithRule; the redact rule (secret-pattern replacement) is defined
// but NOT registered by default — opt in via WithRedact().
package purify

import (
	"strings"
	"unicode"
)

// Result is the outcome of purifying a single input string.
type Result struct {
	// Original is the raw input as received.
	Original string

	// Cleaned is the standardized + redacted output. Callers should use this
	// in place of Original when persisting or passing to the LLM.
	Cleaned string

	// Blocked is true when a rule rejected the input. The caller should drop
	// the message (Cleaned is still set to a safe stub in case the caller
	// needs to ack the user).
	Blocked bool

	// BlockReason is the human-readable reason when Blocked is true.
	BlockReason string

	// Warnings is non-blocking observations (suspected injection, unusual
	// encoding, etc.). Caller should log these for visibility but must not
	// reject the message based on warnings alone.
	Warnings []string

	// Redactions lists each secret type and how many were replaced.
	Redactions []Redaction
}

// Redaction records a single secret-pattern replacement.
type Redaction struct {
	Type  string // "openai-key", "aws-access-key", etc.
	Count int
}

// Purifier is the top-level entry point. Apply rules in registration order.
type Purifier struct {
	rules     []Rule
	maxLength int
}

// Rule mutates a Result in place. Rules are chained; each sees the output of
// the previous rule's modifications.
type Rule interface {
	Apply(r *Result)
}

// Option configures a Purifier at construction time.
type Option func(*Purifier)

// WithRule appends a custom rule to the pipeline. Rules are applied in the
// order they're registered. Built-in rules (Standardize + Injection) run
// first; custom rules run after.
func WithRule(r Rule) Option {
	return func(p *Purifier) { p.rules = append(p.rules, r) }
}

// WithRedact opts in to the redact rule (secret-pattern replacement).
// Disabled by default — only register when you actually want secrets
// replaced with [REDACTED:type] markers.
func WithRedact() Option {
	return func(p *Purifier) { p.rules = append(p.rules, &redactRule{}) }
}

// WithMaxLength caps input length (default 120 runes). Inputs exceeding
// this are BLOCKED (not truncated). Pass 0 to disable the cap entirely.
func WithMaxLength(n int) Option {
	return func(p *Purifier) {
		// Allow n == 0 (disable cap); reject negative as no-op.
		if n >= 0 {
			p.maxLength = n
		}
	}
}

// New constructs a Purifier with built-in standardize + injection rules.
// Redact is opt-in (see WithRedact). Custom rules can be appended via
// WithRule.
func New(opts ...Option) *Purifier {
	p := &Purifier{
		maxLength: 120,
	}
	// Built-in rule order: standardize → injection detection.
	p.rules = []Rule{
		&standardizeRule{maxLength: 0}, // maxLength patched in by Purifier.Apply
		&injectionRule{},
	}
	for _, opt := range opts {
		opt(p)
	}
	return p
}

// MaxLength returns the configured length cap (0 = disabled). Used by callers
// to surface the actual limit back to the user when input is blocked.
func (p *Purifier) MaxLength() int {
	return p.maxLength
}

// Purify runs the pipeline. Always returns a non-nil Result; the caller
// decides what to do with Blocked / Warnings.
func (p *Purifier) Purify(input string) Result {
	r := Result{Original: input, Cleaned: input}
	for _, rule := range p.rules {
		// Patch maxLength into the standardize rule dynamically so WithMaxLength
		// can be changed without re-instantiating the standardize rule.
		if sr, ok := rule.(*standardizeRule); ok {
			sr.maxLength = p.maxLength
		}
		rule.Apply(&r)
		// Short-circuit once blocked: no point running further rules.
		if r.Blocked {
			return r
		}
	}
	// Post-pipeline: drop entirely-empty inputs.
	if strings.TrimSpace(r.Cleaned) == "" {
		r.Blocked = true
		r.BlockReason = "empty after standardization"
		r.Cleaned = ""
	}
	return r
}

// HasRedactions is a convenience for callers that just want to know if any
// secret was replaced.
func (r Result) HasRedactions() bool {
	for _, x := range r.Redactions {
		if x.Count > 0 {
			return true
		}
	}
	return false
}

// addRedaction increments the count for a given type (or starts it at 1).
func (r *Result) addRedaction(typ string) {
	for i := range r.Redactions {
		if r.Redactions[i].Type == typ {
			r.Redactions[i].Count++
			return
		}
	}
	r.Redactions = append(r.Redactions, Redaction{Type: typ, Count: 1})
}

// addWarning appends a non-blocking observation. Deduplicates exact strings.
func (r *Result) addWarning(msg string) {
	for _, w := range r.Warnings {
		if w == msg {
			return
		}
	}
	r.Warnings = append(r.Warnings, msg)
}

// --- helpers shared across rules ---

// isControl reports whether r is a control character that should be stripped.
// Newlines (\n), tabs (\t), and carriage returns (\r) are preserved (CR is
// later normalized to LF by the standardize rule).
func isControl(r rune) bool {
	if r == '\n' || r == '\t' || r == '\r' {
		return false
	}
	return unicode.IsControl(r)
}
