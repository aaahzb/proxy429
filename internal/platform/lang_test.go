package platform

import "testing"

// TestApplyUILangFallback: when the config doesn't specify a language, fall back to system detection (the result is always zh or en).
func TestApplyUILangFallback(t *testing.T) {
	ApplyUILang("")
	if got := CurrentUILang(); got != "zh" && got != "en" {
		t.Errorf("currentUILang=%q, want zh 或 en", got)
	}
	ApplyUILang("en")
	if got := CurrentUILang(); got != "en" {
		t.Errorf("ApplyUILang(en) 后 currentUILang=%q", got)
	}
	ApplyUILang("zh")
	if got := CurrentUILang(); got != "zh" {
		t.Errorf("ApplyUILang(zh) 后 currentUILang=%q", got)
	}
}
