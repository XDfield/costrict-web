package handlers

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"gorm.io/datatypes"
)

func newTestContext(t *testing.T, target, acceptLanguage string) *gin.Context {
	t.Helper()
	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	req := httptest.NewRequest(http.MethodGet, target, nil)
	if acceptLanguage != "" {
		req.Header.Set("Accept-Language", acceptLanguage)
	}
	c.Request = req
	return c
}

func TestResolveLocale_QueryParamWins(t *testing.T) {
	c := newTestContext(t, "/items?lang=en", "zh-CN")
	if got := ResolveLocale(c); got != "en" {
		t.Errorf("?lang= should override Accept-Language: got %q want %q", got, "en")
	}
}

func TestResolveLocale_AcceptLanguageZhCN(t *testing.T) {
	c := newTestContext(t, "/items", "zh-CN,en;q=0.9")
	if got := ResolveLocale(c); got != "zh" {
		t.Errorf("zh-CN should normalize to zh: got %q", got)
	}
}

func TestResolveLocale_AcceptLanguageZhTW(t *testing.T) {
	c := newTestContext(t, "/items", "zh-TW")
	if got := ResolveLocale(c); got != "zh" {
		t.Errorf("zh-TW should normalize to zh: got %q", got)
	}
}

func TestResolveLocale_AcceptLanguageZhHant(t *testing.T) {
	c := newTestContext(t, "/items", "zh-Hant")
	if got := ResolveLocale(c); got != "zh" {
		t.Errorf("zh-Hant should normalize to zh: got %q", got)
	}
}

func TestResolveLocale_AcceptLanguageEnUS(t *testing.T) {
	c := newTestContext(t, "/items", "en-US")
	if got := ResolveLocale(c); got != "en" {
		t.Errorf("en-US should normalize to en: got %q", got)
	}
}

func TestResolveLocale_UnsupportedFallsBackToEn(t *testing.T) {
	c := newTestContext(t, "/items", "ja-JP")
	if got := ResolveLocale(c); got != "en" {
		t.Errorf("ja-JP should fall back to en: got %q", got)
	}
}

func TestResolveLocale_EmptyDefaultsToEn(t *testing.T) {
	c := newTestContext(t, "/items", "")
	if got := ResolveLocale(c); got != "en" {
		t.Errorf("no header should default to en: got %q", got)
	}
}

func TestResolveLocale_NilContext(t *testing.T) {
	if got := ResolveLocale(nil); got != "en" {
		t.Errorf("nil context should default to en: got %q", got)
	}
}

func TestResolveLocale_MultipleAcceptLanguageTags(t *testing.T) {
	// Accept-Language: ja-JP first, zh-CN second. We only honor the first tag
	// — no q-value parsing.
	c := newTestContext(t, "/items", "ja-JP,zh-CN;q=0.9,en;q=0.8")
	if got := ResolveLocale(c); got != "en" {
		t.Errorf("first tag ja-JP should fall back to en: got %q", got)
	}
}

func TestPickDescription_HitsLocaleKey(t *testing.T) {
	descs := datatypes.JSON([]byte(`{"en":"Hello","zh":"你好"}`))
	if got := PickDescription(descs, "fallback", "zh"); got != "你好" {
		t.Errorf("expected 你好, got %q", got)
	}
}

func TestPickDescription_FallsBackToEn(t *testing.T) {
	descs := datatypes.JSON([]byte(`{"en":"Hello"}`))
	if got := PickDescription(descs, "fallback", "zh"); got != "Hello" {
		t.Errorf("missing zh should fall back to en: got %q", got)
	}
}

func TestPickDescription_FallsBackToText(t *testing.T) {
	descs := datatypes.JSON([]byte(`{}`))
	if got := PickDescription(descs, "Legacy text", "zh"); got != "Legacy text" {
		t.Errorf("empty map should fall back to text column: got %q", got)
	}
}

func TestPickDescription_AllEmpty(t *testing.T) {
	if got := PickDescription(nil, "", "zh"); got != "" {
		t.Errorf("everything empty should return empty string: got %q", got)
	}
}

func TestPickDescription_NilDescriptionsUsesText(t *testing.T) {
	if got := PickDescription(nil, "Plain text", "zh"); got != "Plain text" {
		t.Errorf("nil descriptions should use text column: got %q", got)
	}
}

func TestPickDescription_LocaleKeyExistsButEmpty(t *testing.T) {
	// `"zh": ""` should not be selected — it's semantically the same as the key missing.
	descs := datatypes.JSON([]byte(`{"en":"Hi","zh":""}`))
	if got := PickDescription(descs, "fallback", "zh"); got != "Hi" {
		t.Errorf("empty zh value should fall back to en: got %q", got)
	}
}
