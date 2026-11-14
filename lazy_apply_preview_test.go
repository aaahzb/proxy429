package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// lazyPreviewObjects decodes the preview output's top-level object fields into raw key maps.
func lazyPreviewObjects(t *testing.T, out []byte) (map[string]json.RawMessage, []map[string]json.RawMessage) {
	t.Helper()
	var top map[string]json.RawMessage
	if err := json.Unmarshal(out, &top); err != nil {
		t.Fatalf("preview is not JSON: %v\n%s", err, out)
	}
	var routes []map[string]json.RawMessage
	if raw, ok := top["routes"]; ok {
		if err := json.Unmarshal(raw, &routes); err != nil {
			t.Fatalf("preview routes: %v", err)
		}
	}
	return top, routes
}

// object decodes one preview object field; a missing field fails the test.
func lazyPreviewObject(t *testing.T, top map[string]json.RawMessage, key string) map[string]json.RawMessage {
	t.Helper()
	raw, ok := top[key]
	if !ok {
		t.Fatalf("preview is missing %q", key)
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(raw, &obj); err != nil {
		t.Fatalf("preview %s: %v", key, err)
	}
	return obj
}

// TestMaterializeLazyApply locks the mechanical preview: supported keys are inserted into every
// route-like object that does not already write them, noLazyApply skips an object entirely, explicit
// false / "" / null survive, optional routes are never created, and $VAR references stay unexpanded.
func TestMaterializeLazyApply(t *testing.T) {
	raw := `{
		"vars":{"TH":"max"},
		"upstream":"http://u",
		"lazyApply":{
			"text_only":true,"no_search":true,"thinking":"adaptive","convertOff2Low":"translate",
			"enhance_search":{"summary_thinking":true,"summary_level":"high"},
			"summary_mode":true,"summary_thinking":true,"summary_level":"max",
			"claudeSearchThinking":"$TH","claudeSearchSummary":"forceHigh",
			"noBullshitOnEmptyClaudeSearch":true,
			"classifier_thinking":"off","classifier_thinking_cc":"low","classifier_thinking_codex":"off"
		},
		"routes":[
			{"pattern":"a","url":"http://a"},
			{"pattern":"b","url":"http://b","noLazyApply":true},
			{"pattern":"c","url":"http://c","text_only":false,"no_search":false,"thinking":"","convertOff2Low":"all",
				"enhance_search":null,"claudeSearchThinking":"off","claudeSearchSummary":"noForce",
				"noBullshitOnEmptyClaudeSearch":false}
		],
		"search_fallback":{"url":"http://s"},
		"fast_route":{"url":"http://f","noLazyApply":true},
		"classifier_route":{"url":"http://c"}
	}`
	out, err := materializeLazyApply([]byte(raw))
	if err != nil {
		t.Fatalf("materializeLazyApply: %v", err)
	}
	text := string(out)
	if !strings.Contains(text, `"$TH"`) {
		t.Errorf("preview must keep $VAR references unexpanded:\n%s", text)
	}
	if !strings.Contains(text, `"lazyApply"`) {
		t.Errorf("preview must keep the lazyApply object itself:\n%s", text)
	}
	top, routes := lazyPreviewObjects(t, out)
	if len(routes) != 3 {
		t.Fatalf("preview routes = %d, want 3", len(routes))
	}

	// Route a: every supported lazy key is inserted.
	for _, key := range lazyApplyRouteKeys {
		if _, ok := routes[0][key]; !ok {
			t.Errorf("route a is missing inherited key %q: %v", key, routes[0])
		}
	}
	if string(routes[0]["text_only"]) != "true" || string(routes[0]["claudeSearchThinking"]) != `"$TH"` {
		t.Errorf("route a inherited values wrong: %v", routes[0])
	}
	if _, ok := routes[0]["summary_mode"]; ok {
		t.Error("routes must not receive search_fallback-only summary_mode")
	}

	// Route b: noLazyApply leaves only its own keys.
	if len(routes[1]) != 3 {
		t.Errorf("noLazyApply route should stay untouched, got %v", routes[1])
	}

	// Route c: explicit false / "" / null / local values are all preserved, missing keys are still filled.
	for key, want := range map[string]string{
		"text_only": "false", "no_search": "false", "thinking": `""`, "convertOff2Low": `"all"`,
		"enhance_search": "null", "claudeSearchThinking": `"off"`, "claudeSearchSummary": `"noForce"`,
		"noBullshitOnEmptyClaudeSearch": "false",
	} {
		if got := string(routes[2][key]); got != want {
			t.Errorf("route c %s = %s, want %s", key, got, want)
		}
	}

	// search_fallback receives its supported fields; fast_route opted out; no multimodal route is created.
	sf := lazyPreviewObject(t, top, "search_fallback")
	for _, key := range lazyApplySearchFallbackKeys {
		if _, ok := sf[key]; !ok {
			t.Errorf("search_fallback is missing inherited key %q: %v", key, sf)
		}
	}
	fr := lazyPreviewObject(t, top, "fast_route")
	if len(fr) != 2 {
		t.Errorf("fast_route noLazyApply should stay untouched, got %v", fr)
	}
	if _, ok := top["multimodal_fallback"]; ok {
		t.Error("lazyApply must not create a missing optional route")
	}
	cr := lazyPreviewObject(t, top, "classifier_route")
	for _, key := range lazyApplyClassifierKeys {
		if _, ok := cr[key]; !ok {
			t.Errorf("classifier_route is missing inherited key %q: %v", key, cr)
		}
	}
}

// lazyApplySnapshot stringifies every effective value the runtime can derive from a config. Preview
// materialization must reproduce the same snapshot: the preview is only useful if copying it back is
// behavior-preserving.
func lazyApplySnapshot(c *Config) string {
	var b strings.Builder
	conv := func(local string, written, optOut bool) string {
		return effectiveConvertOff2Low(c, local, written, optOut)
	}
	for i := range c.Routes {
		rr := &c.Routes[i]
		inject, think := resolveSearchBehavior(effectiveDirectSearchSettings(c, rr))
		fmt.Fprintf(&b, "route%d text=%v no_search=%v thinking=%q conv=%q direct=(%q,%q) guardDirect=%v guardFallback=%v\n",
			i, effectiveRouteTextOnly(c, rr), effectiveRouteNoSearch(c, rr), effectiveRouteThinking(c, rr),
			conv(rr.ConvertOff2Low, rr.presence.written("convertOff2Low", rr.ConvertOff2Low != ""), rr.NoLazyApply),
			inject, think, effectiveSearchGuard(c, rr, nil), effectiveSearchGuard(c, rr, c.SearchFallback))
		if c.SearchFallback != nil {
			eff := effectiveFallbackSearchSettings(c, rr, c.SearchFallback)
			inject, think = resolveSearchBehavior(eff)
			fmt.Fprintf(&b, "route%d fallback=(%q,%q) conv=%q thinking=%q\n", i, inject, think, eff.ConvertOff2Low, effectiveFallbackThinking(c, c.SearchFallback))
		}
	}
	if sf := c.SearchFallback; sf != nil {
		eff := effectiveFallbackSearchSettings(c, nil, sf)
		inject, think := resolveSearchBehavior(eff)
		fmt.Fprintf(&b, "fallback-only conv=%q search=(%q,%q) guard=%v thinking=%q\n",
			conv(sf.ConvertOff2Low, sf.presence.written("convertOff2Low", sf.ConvertOff2Low != ""), sf.NoLazyApply),
			inject, think, effectiveSearchGuard(c, nil, sf), effectiveFallbackThinking(c, sf))
	}
	fmt.Fprintf(&b, "classifier cc=%q codex=%q\n",
		classifierThinkingModeFor(c, classifierCC), classifierThinkingModeFor(c, classifierCodex))
	if fr := c.FastRoute; fr != nil {
		fmt.Fprintf(&b, "fast conv=%q\n", conv(fr.ConvertOff2Low, fr.presence.written("convertOff2Low", fr.ConvertOff2Low != ""), fr.NoLazyApply))
	}
	if mf := c.MultimodalFallback; mf != nil {
		fmt.Fprintf(&b, "multimodal conv=%q\n", conv(mf.ConvertOff2Low, mf.presence.written("convertOff2Low", mf.ConvertOff2Low != ""), mf.NoLazyApply))
	}
	return b.String()
}

// TestMaterializeLazyApplyBehaviorEquivalence locks the preview's core promise across representative
// configs: parsing the materialized JSON yields exactly the same effective behavior as the original.
func TestMaterializeLazyApplyBehaviorEquivalence(t *testing.T) {
	bodies := []string{
		`{"upstream":"http://u",
			"lazyApply":{"text_only":true,"no_search":true,"thinking":"adaptive","convertOff2Low":"translate",
				"enhance_search":{"summary_thinking":true,"summary_level":"high"},
				"claudeSearchThinking":"max","claudeSearchSummary":"forceHigh",
				"noBullshitOnEmptyClaudeSearch":true,
				"classifier_thinking":"off","classifier_thinking_cc":"low","classifier_thinking_codex":"off"},
			"routes":[{"pattern":"a","url":"http://a"}],
			"search_fallback":{"url":"http://s"},
			"fast_route":{"url":"http://f"},
			"multimodal_fallback":{"url":"http://m"},
			"classifier_route":{"url":"http://c"}}`,
		`{"upstream":"http://u",
			"lazyApply":{"summary_mode":true,"summary_thinking":true,"summary_level":"max",
				"claudeSearchThinking":"max","claudeSearchSummary":"forceHigh","noBullshitOnEmptyClaudeSearch":true},
			"routes":[{"pattern":"a","url":"http://a","no_search":true},
				{"pattern":"b","url":"http://b","no_search":true,"noLazyApply":true}],
			"search_fallback":{"url":"http://s","claudeSearchThinking":"low","claudeSearchSummary":"forceLow",
				"noBullshitOnEmptyClaudeSearch":false}}`,
		`{"upstream":"http://u",
			"lazyApply":{"text_only":true,"no_search":true,"thinking":"adaptive","convertOff2Low":"translate"},
			"routes":[{"pattern":"a","url":"http://a","text_only":false,"no_search":false,"thinking":"","convertOff2Low":""},
				{"pattern":"b","url":"http://b","noLazyApply":true,"text_only":true}],
			"fast_route":{"url":"http://f","convertOff2Low":"all"},
			"multimodal_fallback":{"url":"http://m","noLazyApply":true}}`,
		`{"vars":{"TH":"max","LV":"high"},"upstream":"http://u",
			"lazyApply":{"claudeSearchThinking":"$TH","claudeSearchSummary":"forceHigh","summary_level":"$LV",
				"enhance_search":{"summary_level":"$LV"}},
			"routes":[{"pattern":["a*","b*"],"url":"http://a"}],
			"search_fallback":{"url":"http://s"}}`,
		`{"upstream":"http://u","lazyApply":{"text_only":true,"no_search":true,"classifier_thinking":"off"}}`,
	}
	for i, body := range bodies {
		before := mustParseLazy(t, body)
		out, err := materializeLazyApply([]byte(body))
		if err != nil {
			t.Errorf("config %d: materializeLazyApply: %v", i, err)
			continue
		}
		after, _, err := parseConfig(out)
		if err != nil {
			t.Errorf("config %d: materialized output does not parse: %v\n%s", i, err, out)
			continue
		}
		want, got := lazyApplySnapshot(before), lazyApplySnapshot(after)
		if want != got {
			t.Errorf("config %d: materialized behavior differs\nbefore:\n%s\nafter:\n%s\njson:\n%s", i, want, got, out)
		}
	}
}

// TestLazyApplyPreviewHandler locks the console endpoint: local-only POST, no cache, parse errors
// rejected as 400, and non-POST methods refused.
func TestLazyApplyPreviewHandler(t *testing.T) {
	const valid = `{"upstream":"http://u","lazyApply":{"text_only":true},"routes":[{"pattern":"a","url":"http://a"}]}`
	call := func(method, remote, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, lazyPreviewPath, strings.NewReader(body))
		req.RemoteAddr = remote
		rec := httptest.NewRecorder()
		lazyApplyPreviewHandler(rec, req)
		return rec
	}

	rec := call(http.MethodPost, "127.0.0.1:1", valid)
	if rec.Code != http.StatusOK {
		t.Fatalf("valid preview status = %d, body %s", rec.Code, rec.Body.String())
	}
	if rec.Header().Get("Cache-Control") != "no-store" {
		t.Errorf("Cache-Control = %q, want no-store", rec.Header().Get("Cache-Control"))
	}
	if !strings.Contains(rec.Body.String(), `"text_only":true`) {
		t.Errorf("preview should contain the materialized field:\n%s", rec.Body.String())
	}
	if _, err := materializeLazyApply(rec.Body.Bytes()); err != nil {
		t.Errorf("preview output should itself be valid: %v", err)
	}

	if rec := call(http.MethodGet, "127.0.0.1:1", ""); rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("GET status = %d, want 405", rec.Code)
	}
	if rec := call(http.MethodPost, "192.168.1.5:1", valid); rec.Code != http.StatusForbidden {
		t.Errorf("remote status = %d, want 403", rec.Code)
	}
	rec = call(http.MethodPost, "127.0.0.1:1", `{"upstream":`)
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "invalid config") {
		t.Errorf("invalid config = %d %q, want 400 invalid config", rec.Code, rec.Body.String())
	}

	// No lazyApply: the preview returns the editor text unchanged.
	rec = call(http.MethodPost, "127.0.0.1:1", `{"upstream":"http://u"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("no-lazy preview status = %d, body %s", rec.Code, rec.Body.String())
	}
	var top map[string]json.RawMessage
	if err := json.Unmarshal(rec.Body.Bytes(), &top); err != nil {
		t.Errorf("no-lazy preview is not JSON: %v", err)
	}
	if _, ok := top["lazyApply"]; ok {
		t.Error("no-lazy preview must not invent a lazyApply object")
	}
}

// TestMaterializeLazyApplyPreservesLayout locks the preview's edit-like behavior: every existing byte
// keeps its position, inherited keys are appended inside each object, and no map re-serialization is
// allowed to reorder top-level keys or nested route members.
func TestMaterializeLazyApplyPreservesLayout(t *testing.T) {
	raw := "{\n" +
		"  \"listen\": \"127.0.0.1:8080\",\n" +
		"  \"lazyApply\": {\"text_only\": true, \"convertOff2Low\": \"translate\"},\n" +
		"  \"routes\": [\n" +
		"    {\n" +
		"      \"pattern\": \"claude-opus*\",\n" +
		"      \"url\": \"http://x\"\n" +
		"    }\n" +
		"  ],\n" +
		"  \"fast_route\": {}\n" +
		"}\n"
	want := "{\n" +
		"  \"listen\": \"127.0.0.1:8080\",\n" +
		"  \"lazyApply\": {\"text_only\": true, \"convertOff2Low\": \"translate\"},\n" +
		"  \"routes\": [\n" +
		"    {\n" +
		"      \"pattern\": \"claude-opus*\",\n" +
		"      \"url\": \"http://x\",\n" +
		"      \"text_only\":true,\n" +
		"      \"convertOff2Low\":\"translate\"\n" +
		"    }\n" +
		"  ],\n" +
		"  \"fast_route\": {\"convertOff2Low\":\"translate\"}\n" +
		"}\n"
	out, err := materializeLazyApply([]byte(raw))
	if err != nil {
		t.Fatalf("materializeLazyApply: %v", err)
	}
	if string(out) != want {
		t.Errorf("preview restructured the config\ngot:\n%s\nwant:\n%s", out, want)
	}
}

// TestMaterializeLazyApplyLeavesUnchangedConfigAlone locks the no-op cases: without effective defaults,
// or when every supported object already writes the key, the preview returns the editor text byte-for-byte.
func TestMaterializeLazyApplyLeavesUnchangedConfigAlone(t *testing.T) {
	for _, raw := range []string{
		"{\n  \"upstream\": \"http://u\"\n}\n",
		"{\"upstream\":\"http://u\",\"lazyApply\":null}\n",
		"{\"upstream\":\"http://u\",\"lazyApply\":{}}\n",
		"{\"upstream\":\"http://u\",\"lazyApply\":{\"text_only\":true},\"routes\":[{\"pattern\":\"a\",\"url\":\"http://a\",\"text_only\":false}]}",
	} {
		out, err := materializeLazyApply([]byte(raw))
		if err != nil {
			t.Errorf("%s: materializeLazyApply: %v", raw, err)
			continue
		}
		if string(out) != raw {
			t.Errorf("unchanged config was rewritten\ngot:  %q\nwant: %q", out, raw)
		}
	}
}

// TestMaterializeLazyApplyCompactObjects locks insertion into single-line objects: keys are appended in
// the lazyApply field order with no reformatting of the surrounding text.
func TestMaterializeLazyApplyCompactObjects(t *testing.T) {
	raw := `{"upstream":"http://u","lazyApply":{"text_only":true,"thinking":"adaptive"},"routes":[{"pattern":"a","url":"http://a"}]}`
	want := `{"upstream":"http://u","lazyApply":{"text_only":true,"thinking":"adaptive"},"routes":[{"pattern":"a","url":"http://a","text_only":true,"thinking":"adaptive"}]}`
	out, err := materializeLazyApply([]byte(raw))
	if err != nil {
		t.Fatalf("materializeLazyApply: %v", err)
	}
	if string(out) != want {
		t.Errorf("compact insertion\ngot:  %s\nwant: %s", out, want)
	}
}

// TestMaterializeLazyApplyTemplate locks the shipped config template: the preview keeps the document's
// top-level order and existing route formatting, appends inherited keys, and stays behavior-equivalent.
func TestMaterializeLazyApplyTemplate(t *testing.T) {
	out, err := materializeLazyApply(configExampleBytes)
	if err != nil {
		t.Fatalf("materializeLazyApply(config.example.json): %v", err)
	}
	if string(out) == string(configExampleBytes) {
		t.Fatal("template preview should gain inherited keys")
	}
	last := -1
	for _, key := range []string{`"listen":`, `"upstream":`, `"vars":`, `"lazyApply":`, `"routes":`, `"classifier_route":`, `"search_fallback":`} {
		idx := strings.Index(string(out), key)
		if idx <= last {
			t.Fatalf("top-level order changed or key missing: %s", key)
		}
		last = idx
	}
	if !strings.Contains(string(out), `"pattern": "claude-opus*"`) {
		t.Error("existing route formatting was rewritten")
	}
	after, _, err := parseConfig(out)
	if err != nil {
		t.Fatalf("materialized template does not parse: %v", err)
	}
	before, _, err := parseConfig(configExampleBytes)
	if err != nil {
		t.Fatalf("template does not parse: %v", err)
	}
	if want, got := lazyApplySnapshot(before), lazyApplySnapshot(after); want != got {
		t.Errorf("materialized template behavior differs\nbefore:\n%s\nafter:\n%s", want, got)
	}
}

// TestMaterializeLazyApplyOnlySupportedFields locks the field matrix behind "never add a key the object
// does not already accept": search_fallback is a route, so it inherits thinking like routes[]; fast_route /
// multimodal_fallback only inherit convertOff2Low, and classifier_route only its own thinking tiers.
func TestMaterializeLazyApplyOnlySupportedFields(t *testing.T) {
	raw := `{
  "upstream": "http://u",
  "lazyApply": {
    "thinking": "adaptive",
    "convertOff2Low": "all",
    "claudeSearchThinking": "max",
    "noBullshitOnEmptyClaudeSearch": true
  },
  "routes": [{"pattern": "*", "url": "http://r"}],
  "search_fallback": {"url": "http://s"},
  "fast_route": {"url": "http://f"},
  "multimodal_fallback": {"url": "http://m"},
  "classifier_route": {"url": "http://c"}
}`
	out, err := materializeLazyApply([]byte(raw))
	if err != nil {
		t.Fatalf("materializeLazyApply: %v", err)
	}
	top, routes := lazyPreviewObjects(t, out)
	route := routes[0]
	for _, key := range []string{"thinking", "convertOff2Low", "claudeSearchThinking", "noBullshitOnEmptyClaudeSearch"} {
		if _, ok := route[key]; !ok {
			t.Errorf("routes[] is missing inherited key %q: %v", key, route)
		}
	}
	sf := lazyPreviewObject(t, top, "search_fallback")
	if got := string(sf["thinking"]); got != `"adaptive"` {
		t.Errorf("search_fallback thinking = %s, want adaptive: %v", got, sf)
	}
	for _, key := range []string{"thinking", "convertOff2Low", "claudeSearchThinking", "noBullshitOnEmptyClaudeSearch"} {
		if _, ok := sf[key]; !ok {
			t.Errorf("search_fallback is missing inherited key %q: %v", key, sf)
		}
	}
	for _, name := range []string{"fast_route", "multimodal_fallback"} {
		obj := lazyPreviewObject(t, top, name)
		if _, ok := obj["thinking"]; ok {
			t.Errorf("%s must not receive routes-only thinking: %v", name, obj)
		}
		if _, ok := obj["convertOff2Low"]; !ok {
			t.Errorf("%s is missing inherited convertOff2Low: %v", name, obj)
		}
	}
	cr := lazyPreviewObject(t, top, "classifier_route")
	for _, key := range []string{"thinking", "convertOff2Low", "claudeSearchThinking", "noBullshitOnEmptyClaudeSearch"} {
		if _, ok := cr[key]; ok {
			t.Errorf("classifier_route must not receive unsupported key %q: %v", key, cr)
		}
	}
}
