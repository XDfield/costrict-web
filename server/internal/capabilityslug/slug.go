package capabilityslug

import (
	"strconv"
	"strings"
	"unicode"

	"golang.org/x/text/unicode/norm"
)

// Canonical returns the persisted invocation identifier for a capability.
// Display names remain separate and are never modified here.
func Canonical(itemType, slug, name, itemID string) string {
	source := slug
	if strings.TrimSpace(source) == "" {
		source = name
	}

	if !RequiresCanonical(itemType) {
		if strings.TrimSpace(slug) == "" {
			return Slugify(source)
		}
		return slug
	}

	if canonical := Slugify(source); canonical != "" {
		return canonical
	}

	compactID := strings.ReplaceAll(itemID, "-", "")
	if compactID == "" {
		return ""
	}
	return fallbackPrefix(itemType) + "-" + compactID
}

// RequiresCanonical reports whether CSC uses this capability's slug as a
// command token or filesystem identifier.
func RequiresCanonical(itemType string) bool {
	switch strings.ToLower(strings.TrimSpace(itemType)) {
	case "skill", "command", "subagent", "agent":
		return true
	default:
		return false
	}
}

// Slugify converts ASCII separators to kebab-case while preserving normalized
// non-ASCII token characters that CSC accepts in invocation identifiers.
func Slugify(value string) string {
	value = norm.NFC.String(value)
	var result strings.Builder
	result.Grow(len(value))
	prevDash := false

	for _, r := range value {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			result.WriteRune(r)
			prevDash = false
		} else if r >= 'A' && r <= 'Z' {
			result.WriteRune(r + ('a' - 'A'))
			prevDash = false
		} else if r > unicode.MaxASCII && !unicode.IsSpace(r) && !unicode.IsControl(r) {
			result.WriteRune(unicode.ToLower(r))
			prevDash = false
		} else if !prevDash && result.Len() > 0 {
			result.WriteByte('-')
			prevDash = true
		}
	}

	return strings.TrimSuffix(result.String(), "-")
}

// HasAssignedCollisionSuffix reports whether candidate is a persisted variant
// allocated by the canonical-slug migration or insert collision retry.
func HasAssignedCollisionSuffix(base, candidate string) bool {
	if base == "" {
		return false
	}
	suffix, ok := strings.CutPrefix(candidate, base+"-")
	if !ok || suffix == "" {
		return false
	}
	if attempt, err := strconv.Atoi(suffix); err == nil {
		return attempt >= 2
	}

	const migratedPrefix = "migrated-"
	if !strings.HasPrefix(suffix, migratedPrefix) {
		return false
	}
	parts := strings.Split(strings.TrimPrefix(suffix, migratedPrefix), "-")
	if len(parts) < 1 || len(parts) > 2 || len(parts[0]) != 32 || !isLowerHex(parts[0]) {
		return false
	}
	if len(parts) == 1 {
		return true
	}
	attempt, err := strconv.Atoi(parts[1])
	return err == nil && attempt >= 2
}

func isLowerHex(value string) bool {
	for i := 0; i < len(value); i++ {
		if !((value[i] >= '0' && value[i] <= '9') || (value[i] >= 'a' && value[i] <= 'f')) {
			return false
		}
	}
	return true
}

func fallbackPrefix(itemType string) string {
	switch strings.ToLower(strings.TrimSpace(itemType)) {
	case "agent":
		return "agent"
	case "command":
		return "command"
	case "subagent":
		return "subagent"
	default:
		return "skill"
	}
}
