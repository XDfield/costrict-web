package handlers

import (
	"encoding/json"
	"strings"

	"github.com/costrict/costrict-web/server/internal/models"
	"github.com/gin-gonic/gin"
	"gorm.io/datatypes"
)

// DefaultLocale is the fallback when neither `?lang=` nor `Accept-Language`
// yields a recognized locale, and the safe choice for "no header" callers
// (matches the pre-i18n API behavior so external consumers see no change).
const DefaultLocale = "en"

// ResolveLocale picks a locale tag from the request. Priority:
//
//  1. `?lang=` query parameter (highest)
//  2. `Accept-Language` header — first language tag
//  3. DefaultLocale ("en")
//
// Returned value is normalized to the primary subtag (zh-CN → zh, en-US → en);
// unrecognized locales fall back to DefaultLocale.
func ResolveLocale(c *gin.Context) string {
	if c == nil {
		return DefaultLocale
	}
	if q := strings.TrimSpace(c.Query("lang")); q != "" {
		return normalizeLocale(q)
	}
	if h := strings.TrimSpace(c.GetHeader("Accept-Language")); h != "" {
		// Accept-Language: zh-CN,en;q=0.9 → take "zh-CN" (first tag), ignore q values.
		first := strings.SplitN(h, ",", 2)[0]
		first = strings.SplitN(first, ";", 2)[0]
		return normalizeLocale(first)
	}
	return DefaultLocale
}

// normalizeLocale collapses a BCP-47-ish tag down to the primary subtag we
// actually serve. Upstream currently ships only en + zh translations; any
// other language (ja/ko/de/…) falls back to en until upstream produces a
// matching `description_<locale>` field.
func normalizeLocale(raw string) string {
	if raw == "" {
		return DefaultLocale
	}
	primary := strings.ToLower(strings.SplitN(raw, "-", 2)[0])
	primary = strings.SplitN(primary, "_", 2)[0]
	switch primary {
	case "zh":
		return "zh"
	case "en":
		return "en"
	default:
		return DefaultLocale
	}
}

// PickDescription resolves a single localized description string from the
// per-item descriptions JSONB map, falling back gracefully to keep
// pre-i18n rows readable.
//
// Order:
//
//  1. descriptions[locale] if non-empty
//  2. descriptions[DefaultLocale] if non-empty
//  3. fallbackText (the legacy capability_items.description column)
//  4. ""
func PickDescription(descriptions datatypes.JSON, fallbackText string, locale string) string {
	if len(descriptions) > 0 {
		var m map[string]string
		if err := json.Unmarshal(descriptions, &m); err == nil {
			if v := m[locale]; v != "" {
				return v
			}
			if v := m[DefaultLocale]; v != "" {
				return v
			}
		}
	}
	return fallbackText
}

// ResolveItemListLocale rewrites the `Description` field of each item in
// place to the locale-resolved value, so list endpoints that serialize
// raw `[]models.CapabilityItem` (or types embedding it) still honor
// Accept-Language without needing to wrap every row in ItemResponse.
// The original `descriptions` JSONB stays on the row so frontends can
// re-resolve on locale switch without re-fetching.
func ResolveItemListLocale(c *gin.Context, items []models.CapabilityItem) {
	if len(items) == 0 {
		return
	}
	locale := ResolveLocale(c)
	for i := range items {
		items[i].Description = PickDescription(items[i].Descriptions, items[i].Description, locale)
	}
}

// ResolveCapabilityItemPointersLocale handles the case where the caller has
// pointers (e.g. inside a result struct field) and wants in-place rewrite
// without exposing the full slice surface to ResolveItemListLocale.
func ResolveCapabilityItemPointersLocale(c *gin.Context, items []*models.CapabilityItem) {
	if len(items) == 0 {
		return
	}
	locale := ResolveLocale(c)
	for _, it := range items {
		if it == nil {
			continue
		}
		it.Description = PickDescription(it.Descriptions, it.Description, locale)
	}
}
