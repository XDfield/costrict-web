package authidentity

import (
	"testing"
)

func TestNormalizeClaimsMapLegacyFlatOnly(t *testing.T) {
	// 旧 Casdoor JWT：仅顶层扁平字段
	claims := map[string]any{
		"sub":                "u_legacy_001",
		"universal_id":       "u_legacy_001",
		"id":                 "u_legacy_001",
		"preferred_username": "alice_legacy",
		"name":               "Alice Legacy",
		"displayName":        "Alice Legacy Display",
		"email":              "alice@legacy.com",
		"phone":              "+86-13800000001",
		"provider":           "github",
		"exp":                int64(1893456000),
	}
	c := NormalizeClaimsMap(claims)
	if c.UniversalID != "u_legacy_001" {
		t.Errorf("UniversalID = %q, want %q", c.UniversalID, "u_legacy_001")
	}
	if c.Sub != "u_legacy_001" {
		t.Errorf("Sub = %q, want %q", c.Sub, "u_legacy_001")
	}
	if c.Email != "alice@legacy.com" {
		t.Errorf("Email = %q, want %q", c.Email, "alice@legacy.com")
	}
	if c.Phone != "+86-13800000001" {
		t.Errorf("Phone = %q, want %q", c.Phone, "+86-13800000001")
	}
	if c.Provider != "github" {
		t.Errorf("Provider = %q, want %q", c.Provider, "github")
	}
}

func TestNormalizeClaimsMapNewNestedOnly(t *testing.T) {
	// 新 cs-user strict canonical JWT：仅嵌套 user Map + primary_provider
	claims := map[string]any{
		"sub":              "u_canonical_002",
		"universal_id":     "u_canonical_002",
		"primary_provider": "idtrust",
		"exp":              int64(1893456000),
		"user": map[string]any{
			"id":           "u_canonical_002",
			"username":     "bob_canonical",
			"display_name": "Bob Canonical",
			"email":        "bob@canonical.com",
			"phone":        "+86-13900000002",
			"avatar_url":   "https://avatars.example.com/bob.png",
		},
	}
	c := NormalizeClaimsMap(claims)
	if c.UniversalID != "u_canonical_002" {
		t.Errorf("UniversalID = %q, want %q", c.UniversalID, "u_canonical_002")
	}
	if c.ID != "u_canonical_002" {
		t.Errorf("ID = %q, want %q (user.id fallback)", c.ID, "u_canonical_002")
	}
	if c.Email != "bob@canonical.com" {
		t.Errorf("Email = %q, want %q (user.email fallback)", c.Email, "bob@canonical.com")
	}
	if c.Phone != "+86-13900000002" {
		t.Errorf("Phone = %q, want %q (user.phone fallback)", c.Phone, "+86-13900000002")
	}
	if c.Provider != "idtrust" {
		t.Errorf("Provider = %q, want %q (primary_provider fallback)", c.Provider, "idtrust")
	}
	if c.Picture != "https://avatars.example.com/bob.png" {
		t.Errorf("Picture = %q, want %q (user.avatar_url fallback)", c.Picture, "https://avatars.example.com/bob.png")
	}
}

func TestNormalizeClaimsMapCompatSameValue(t *testing.T) {
	// 新 compat 模式：flat + nested 都填，值相同
	claims := map[string]any{
		"sub":                "u_compat_003",
		"universal_id":       "u_compat_003",
		"id":                 "u_compat_003",
		"preferred_username": "charlie_compat",
		"name":               "Charlie Compat",
		"displayName":        "Charlie Compat Display",
		"email":              "charlie@compat.com",
		"phone":              "+86-13700000003",
		"provider":           "github",
		"primary_provider":   "github",
		"exp":                int64(1893456000),
		"user": map[string]any{
			"id":           "u_compat_003",
			"username":     "charlie_compat",
			"display_name": "Charlie Compat Display",
			"email":        "charlie@compat.com",
			"phone":        "+86-13700000003",
		},
	}
	c := NormalizeClaimsMap(claims)
	if c.UniversalID != "u_compat_003" {
		t.Errorf("UniversalID = %q, want %q", c.UniversalID, "u_compat_003")
	}
	if c.Email != "charlie@compat.com" {
		t.Errorf("Email = %q, want %q", c.Email, "charlie@compat.com")
	}
	if c.Provider != "github" {
		t.Errorf("Provider = %q, want %q", c.Provider, "github")
	}
}

func TestNormalizeClaimsMapCompatFlatPriority(t *testing.T) {
	// 新 compat 模式但 flat 与 nested 值不同 → 验证 flat 优先
	claims := map[string]any{
		"sub":                "u_compat_004",
		"universal_id":       "u_compat_004",
		"id":                 "flat_id",
		"preferred_username": "flat_username",
		"name":               "Flat Name",
		"displayName":        "Flat Display",
		"email":              "flat@example.com",
		"phone":              "+86-13600000004",
		"provider":           "github",
		"primary_provider":   "idtrust",
		"exp":                int64(1893456000),
		"user": map[string]any{
			"id":           "nested_id",
			"username":     "nested_username",
			"display_name": "Nested Display",
			"email":        "nested@example.com",
			"phone":        "+86-13500000005",
			"avatar_url":   "https://avatars.example.com/nested.png",
		},
	}
	c := NormalizeClaimsMap(claims)
	// flat 优先
	if c.Email != "flat@example.com" {
		t.Errorf("Email = %q, want flat value (flat-first policy)", c.Email)
	}
	if c.Phone != "+86-13600000004" {
		t.Errorf("Phone = %q, want flat value (flat-first policy)", c.Phone)
	}
	if c.ID != "flat_id" {
		t.Errorf("ID = %q, want flat value (flat-first policy)", c.ID)
	}
	if c.Provider != "github" {
		t.Errorf("Provider = %q, want flat value (flat-first policy)", c.Provider)
	}
}

func TestNormalizeClaimsMapPartialFallback(t *testing.T) {
	// 混合：部分字段仅 flat，部分仅 nested
	claims := map[string]any{
		"sub":              "u_mixed_005",
		"universal_id":     "u_mixed_005",
		"email":            "mixed@example.com",
		"provider":         "email",
		"primary_provider": "email",
		"exp":              int64(1893456000),
		"user": map[string]any{
			"id":           "u_mixed_005",
			"username":     "nested_only_username",
			"display_name": "Nested Only Display",
		},
	}
	c := NormalizeClaimsMap(claims)
	// email 走 flat
	if c.Email != "mixed@example.com" {
		t.Errorf("Email = %q, want flat value", c.Email)
	}
	// id 走 nested（flat 缺失）
	if c.ID != "u_mixed_005" {
		t.Errorf("ID = %q, want nested fallback %q", c.ID, "u_mixed_005")
	}
}

func TestLookupNested(t *testing.T) {
	claims := map[string]any{
		"user": map[string]any{
			"email": "deep@example.com",
			"nested": map[string]any{
				"key": "deep_value",
			},
		},
	}
	if v, ok := lookupNested(claims, "user.email").(string); !ok || v != "deep@example.com" {
		t.Errorf("lookupNested(user.email) = %v, want deep@example.com", v)
	}
	if v, ok := lookupNested(claims, "user.nested.key").(string); !ok || v != "deep_value" {
		t.Errorf("lookupNested(user.nested.key) = %v, want deep_value", v)
	}
	if lookupNested(claims, "user.missing") != nil {
		t.Errorf("lookupNested(user.missing) should be nil")
	}
	if lookupNested(claims, "missing.path") != nil {
		t.Errorf("lookupNested(missing.path) should be nil")
	}
	if lookupNested(nil, "user.email") != nil {
		t.Errorf("lookupNested on nil map should be nil")
	}
}
