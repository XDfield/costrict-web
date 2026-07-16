package authidentity

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/golang-jwt/jwt/v4"
)

type NormalizedClaims struct {
	ID                string
	Sub               string
	UniversalID       string
	Name              string
	PreferredUsername string
	Email             string
	Picture           string
	Owner             string
	Provider          string
	ProviderUserID    string
	Phone             string
	Properties        map[string]any
}

func ParseUnverifiedTokenClaims(tokenString string) (map[string]any, error) {
	parser := jwt.Parser{}
	claims := jwt.MapClaims{}

	if _, _, err := parser.ParseUnverified(tokenString, claims); err != nil {
		return nil, fmt.Errorf("failed to parse access token claims: %w", err)
	}

	return map[string]any(claims), nil
}

func NormalizeClaimsMap(claims map[string]any) *NormalizedClaims {
	if claims == nil {
		return &NormalizedClaims{}
	}

	// flat first → nested/renamed fallback（兼容旧 Casdoor flat JWT + 新 cs-user canonical JWT）
	provider := firstNonEmpty(
		str(claims, "provider"),
		str(claims, "primary_provider"),
	)
	phone := firstNonEmpty(
		str(claims, "phone_number"),
		str(claims, "phone"),
		strNested(claims, "user.phone"),
	)
	if provider == "" && phone != "" {
		provider = "phone"
	}
	// Fallback: detect idtrust from signupApplication when provider is not set
	if provider == "" {
		if signupApp := str(claims, "signupApplication"); strings.EqualFold(signupApp, "idtrust") {
			provider = "idtrust"
		}
	}

	properties := mapAny(claims["properties"])
	prefix := providerPropertyPrefix(provider)
	providerUserID := providerProp(properties, prefix, "id")
	providerUsername := providerProp(properties, prefix, "username")
	providerDisplayName := providerProp(properties, prefix, "displayName")
	providerEmail := providerProp(properties, prefix, "email")
	providerAvatar := providerProp(properties, prefix, "avatarUrl")

	email := validatedEmail(firstNonEmpty(str(claims, "email"), strNested(claims, "user.email"), providerEmail))
	if email == "" {
		email = validatedEmail(firstNonEmpty(providerEmail, str(claims, "email"), strNested(claims, "user.email")))
	}

	if phone == "" && isLikelyPhone(providerEmail) {
		phone = providerEmail
	}

	name := firstNonEmpty(str(claims, "name"), strNested(claims, "user.display_name"))
	displayName := firstNonEmpty(
		providerDisplayName,
		str(claims, "preferred_username"),
		strNested(claims, "user.username"),
		str(claims, "displayName"),
		strNested(claims, "user.display_name"),
		name,
	)
	username := ""

	switch normalizedProvider(provider) {
	case "github":
		username = firstNonEmpty(providerUsername, name, usernameFromEmail(email))
	case "idtrust":
		username = firstNonEmpty(providerUserID, providerUsername)
		displayName = firstNonEmpty(str(claims, "displayName"), strNested(claims, "user.display_name"), providerDisplayName, username)
		name = username
	case "phone":
		if phone != "" {
			username = "phone_" + phone
		} else {
			username = stableNameFromSubject(firstNonEmpty(str(claims, "universal_id"), str(claims, "sub"), str(claims, "id")))
		}
		displayName = firstNonEmpty(str(claims, "displayName"), strNested(claims, "user.display_name"), username)
		if looksLikeUUID(name) {
			name = username
		}
	default:
		username = firstNonEmpty(providerUsername, str(claims, "preferred_username"), strNested(claims, "user.username"), name, usernameFromEmail(email))
	}

	if username == "" {
		username = stableNameFromSubject(firstNonEmpty(str(claims, "universal_id"), str(claims, "sub"), str(claims, "id")))
	}
	if name == "" || (normalizedProvider(provider) == "idtrust") {
		name = username
	}
	if displayName == "" {
		displayName = username
	}

	picture := firstNonEmpty(
		providerAvatar,
		str(claims, "picture"),
		str(claims, "avatar"),
		str(claims, "avatar_url"),
		strNested(claims, "user.avatar_url"),
		str(claims, "permanentAvatar"),
	)

	return &NormalizedClaims{
		ID:                firstNonEmpty(str(claims, "id"), strNested(claims, "user.id")),
		Sub:               str(claims, "sub"),
		UniversalID:       str(claims, "universal_id", "universalId"),
		Name:              name,
		PreferredUsername: displayName,
		Email:             email,
		Picture:           picture,
		Owner:             str(claims, "owner"),
		Provider:          normalizedProvider(provider),
		ProviderUserID:    firstNonEmpty(providerUserID, str(claims, "id"), strNested(claims, "user.id")),
		Phone:             phone,
		Properties:        properties,
	}
}

func normalizedProvider(provider string) string {
	normalized := strings.ToLower(strings.TrimSpace(provider))
	// Handle compound provider names like "IDTrust - Sangfor"
	if idx := strings.Index(normalized, " - "); idx > 0 {
		normalized = strings.TrimSpace(normalized[:idx])
	}
	return normalized
}

func providerPropertyPrefix(provider string) string {
	switch normalizedProvider(provider) {
	case "github":
		return "oauth_GitHub"
	case "idtrust", "custom":
		return "oauth_Custom"
	default:
		return ""
	}
}

func providerProp(properties map[string]any, prefix, suffix string) string {
	if len(properties) == 0 || prefix == "" {
		return ""
	}
	if v, ok := properties[prefix+"_"+suffix]; ok {
		if s, ok := v.(string); ok {
			return strings.TrimSpace(s)
		}
	}
	return ""
}

func str(claims map[string]any, keys ...string) string {
	for _, key := range keys {
		if v, ok := claims[key]; ok {
			if s, ok := v.(string); ok && strings.TrimSpace(s) != "" {
				return strings.TrimSpace(s)
			}
		}
	}
	return ""
}

// lookupNested 解析点号路径 "user.email" → claims["user"].(map)["email"]。
// 用于新 cs-user JWT canonical 嵌套结构（MULTI_TENANCY §12.1）的字段访问。
// 任一中段不是 map 或键缺失即返回 nil。
func lookupNested(claims map[string]any, path string) any {
	if claims == nil || path == "" {
		return nil
	}
	parts := strings.Split(path, ".")
	var cur any = claims
	for _, p := range parts {
		m, ok := cur.(map[string]any)
		if !ok {
			return nil
		}
		v, exists := m[p]
		if !exists {
			return nil
		}
		cur = v
	}
	return cur
}

// strNested 等价于 str()，但从嵌套路径取（如 "user.email"）。
// 多个 path 时按顺序取第一个非空字符串。
func strNested(claims map[string]any, paths ...string) string {
	for _, p := range paths {
		if v, ok := lookupNested(claims, p).(string); ok && strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

func mapAny(v any) map[string]any {
	if v == nil {
		return nil
	}
	if m, ok := v.(map[string]any); ok {
		return m
	}
	if m, ok := v.(map[string]interface{}); ok {
		return m
	}
	return nil
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

func validatedEmail(v string) string {
	v = strings.TrimSpace(v)
	if v == "" || !strings.Contains(v, "@") {
		return ""
	}
	return v
}

var digitsOnly = regexp.MustCompile(`^\d{6,20}$`)
var uuidLike = regexp.MustCompile(`^[0-9a-fA-F-]{32,36}$`)

func isLikelyPhone(v string) bool {
	v = strings.TrimSpace(v)
	return digitsOnly.MatchString(v)
}

func looksLikeUUID(v string) bool {
	v = strings.TrimSpace(v)
	return uuidLike.MatchString(v) && strings.Count(v, "-") >= 4
}

func usernameFromEmail(email string) string {
	if email == "" {
		return ""
	}
	parts := strings.Split(email, "@")
	if len(parts) == 0 {
		return ""
	}
	return strings.TrimSpace(parts[0])
}

func stableNameFromSubject(subject string) string {
	subject = strings.TrimSpace(subject)
	if subject == "" {
		return "user"
	}
	if len(subject) > 12 {
		subject = subject[:12]
	}
	return "user_" + strings.ToLower(subject)
}
