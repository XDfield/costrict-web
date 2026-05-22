package services

import (
	"encoding/json"
	"testing"

	"gorm.io/datatypes"
)

func decodeDescriptions(t *testing.T, raw datatypes.JSON) map[string]string {
	t.Helper()
	m := map[string]string{}
	if len(raw) == 0 {
		return m
	}
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("unmarshal descriptions: %v", err)
	}
	return m
}

func TestBuildDescriptionsJSON_BothLocales(t *testing.T) {
	entry := catalogEntry{
		Description:   "A skill that does X",
		DescriptionZh: "一个执行 X 的技能",
	}
	got := decodeDescriptions(t, buildDescriptionsJSON(entry))
	want := map[string]string{
		"en": "A skill that does X",
		"zh": "一个执行 X 的技能",
	}
	if len(got) != len(want) {
		t.Fatalf("unexpected key count: got=%v want=%v", got, want)
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("descriptions[%q] = %q; want %q", k, got[k], v)
		}
	}
}

func TestBuildDescriptionsJSON_OnlyEnglish(t *testing.T) {
	entry := catalogEntry{Description: "Only english here"}
	got := decodeDescriptions(t, buildDescriptionsJSON(entry))
	if _, hasZh := got["zh"]; hasZh {
		t.Errorf("zh key should not be present when entry.DescriptionZh is empty; got=%v", got)
	}
	if got["en"] != "Only english here" {
		t.Errorf("en mismatch: %q", got["en"])
	}
}

func TestBuildDescriptionsJSON_OnlyChinese(t *testing.T) {
	entry := catalogEntry{DescriptionZh: "只有中文"}
	got := decodeDescriptions(t, buildDescriptionsJSON(entry))
	if _, hasEn := got["en"]; hasEn {
		t.Errorf("en key should not be present when entry.Description is empty; got=%v", got)
	}
	if got["zh"] != "只有中文" {
		t.Errorf("zh mismatch: %q", got["zh"])
	}
}

func TestBuildDescriptionsJSON_BothEmptyReturnsEmptyObject(t *testing.T) {
	got := buildDescriptionsJSON(catalogEntry{})
	if string(got) != "{}" {
		t.Errorf("expected empty object, got %q", string(got))
	}
}

func TestDescriptionsJSONEqual_KeyOrderInsensitive(t *testing.T) {
	a := datatypes.JSON([]byte(`{"en":"hi","zh":"你好"}`))
	b := datatypes.JSON([]byte(`{"zh":"你好","en":"hi"}`))
	if !descriptionsJSONEqual(a, b) {
		t.Errorf("equal maps with different key order should be equal")
	}
}

func TestDescriptionsJSONEqual_DifferentValues(t *testing.T) {
	a := datatypes.JSON([]byte(`{"en":"hi"}`))
	b := datatypes.JSON([]byte(`{"en":"hello"}`))
	if descriptionsJSONEqual(a, b) {
		t.Errorf("different values should not compare equal")
	}
}

func TestDescriptionsJSONEqual_MissingKey(t *testing.T) {
	a := datatypes.JSON([]byte(`{"en":"hi","zh":"你好"}`))
	b := datatypes.JSON([]byte(`{"en":"hi"}`))
	if descriptionsJSONEqual(a, b) {
		t.Errorf("upstream removed zh should not compare equal — that triggers re-write")
	}
}

func TestDescriptionsJSONEqual_EmptyVsAbsent(t *testing.T) {
	a := datatypes.JSON(nil)
	b := datatypes.JSON([]byte(`{}`))
	if !descriptionsJSONEqual(a, b) {
		t.Errorf("nil and empty-object should compare equal")
	}
}

// TestBuildDescriptionsJSON_IntegralReplacement encodes the "ingest re-writes
// integrally, no merging" semantic from the spec. When upstream drops the zh
// translation in a later bundle, the next ingest pass MUST produce a JSON
// without the zh key — not merge with the previously stored value.
func TestBuildDescriptionsJSON_IntegralReplacement(t *testing.T) {
	first := decodeDescriptions(t, buildDescriptionsJSON(catalogEntry{
		Description:   "v1",
		DescriptionZh: "旧",
	}))
	second := decodeDescriptions(t, buildDescriptionsJSON(catalogEntry{
		Description: "v2",
	}))
	if first["zh"] != "旧" {
		t.Fatalf("setup: expected first.zh='旧', got %q", first["zh"])
	}
	if _, hasZh := second["zh"]; hasZh {
		t.Errorf("second pass should drop zh key when upstream removed description_zh; got=%v", second)
	}
	if second["en"] != "v2" {
		t.Errorf("second.en should be 'v2', got %q", second["en"])
	}
}
