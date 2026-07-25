package capabilityslug

import (
	"strconv"
	"strings"
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

// Slugify converts presentation text to lowercase ASCII kebab-case.
func Slugify(value string) string {
	result := make([]byte, 0, len(value))
	prevDash := false
	for i := 0; i < len(value); i++ {
		c := value[i]
		if (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') {
			result = append(result, c)
			prevDash = false
		} else if c >= 'A' && c <= 'Z' {
			result = append(result, c+32)
			prevDash = false
		} else if !prevDash && len(result) > 0 {
			result = append(result, '-')
			prevDash = true
		}
	}
	if len(result) > 0 && result[len(result)-1] == '-' {
		result = result[:len(result)-1]
	}
	return string(result)
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
