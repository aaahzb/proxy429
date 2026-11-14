package main

import (
	"strings"
	"testing"
)

// mustParseLazy parses a config body and fails the test on any error.
func mustParseLazy(t *testing.T, body string) *Config {
	t.Helper()
	c, _, err := parseConfig([]byte(body))
	if err != nil {
		t.Fatalf("parseConfig: %v", err)
	}
	return c
}

// TestLazyApplyStrictDecodeAndValidation locks the parser contract: lazyApply is strictly decoded (a typo
// must not silently disable a global default), connection fields are rejected by name, nested
// enhance_search is strict too, enums are validated, and null/absent mean "no defaults".
func TestLazyApplyStrictDecodeAndValidation(t *testing.T) {
	t.Run("all supported fields", func(t *testing.T) {
		c := mustParseLazy(t, `{"upstream":"http://u","lazyApply":{
			"text_only":true,"no_search":true,"thinking":"adaptive","convertOff2Low":"translate",
			"enhance_search":{"summary_thinking":true,"summary_level":"high"},
			"summary_mode":true,"summary_thinking":true,"summary_level":"max",
			"claudeSearchThinking":"max","claudeSearchSummary":"forceMid",
			"noBullshitOnEmptyClaudeSearch":true,
			"classifier_thinking":"off","classifier_thinking_cc":"low","classifier_thinking_codex":"off"}}`)
		la := c.LazyApply
		if la == nil {
			t.Fatal("LazyApply is nil")
		}
		if !la.TextOnly || !la.NoSearch || la.Thinking != "adaptive" || la.ConvertOff2Low != "translate" {
			t.Errorf("ordinary fields decoded wrong: %+v", la)
		}
		if la.EnhanceSearch == nil || !la.EnhanceSearch.SummaryThinking || la.EnhanceSearch.SummaryLevel != "high" {
			t.Errorf("enhance_search decoded wrong: %+v", la.EnhanceSearch)
		}
		if !la.SummaryMode || !la.SummaryThinking || la.SummaryLevel != "max" {
			t.Errorf("summary fields decoded wrong: %+v", la)
		}
		if la.ClaudeSearchThinking != "max" || la.ClaudeSearchSummary != "forceMid" {
			t.Errorf("claudeSearch fields decoded wrong: %+v", la)
		}
		if la.NoBullshitOnEmptyClaudeSearch == nil || !*la.NoBullshitOnEmptyClaudeSearch {
			t.Errorf("guard field decoded wrong: %+v", la.NoBullshitOnEmptyClaudeSearch)
		}
		if la.ClassifierThinking != "off" || la.ClassifierThinkingCC != "low" || la.ClassifierThinkingCodex != "off" {
			t.Errorf("classifier fields decoded wrong: %+v", la)
		}
	})

	t.Run("connection and unknown fields rejected", func(t *testing.T) {
		for _, key := range []string{"url", "api", "model", "pattern", "url_response_api", "bogus"} {
			body := `{"upstream":"http://u","lazyApply":{"` + key + `":"x"}}`
			_, _, err := parseConfig([]byte(body))
			if err == nil || !strings.Contains(err.Error(), "lazyApply") {
				t.Errorf("lazyApply.%s: want a lazyApply error, got %v", key, err)
			}
		}
	})

	t.Run("nested enhance_search is strict", func(t *testing.T) {
		body := `{"upstream":"http://u","lazyApply":{"enhance_search":{"bogus":1}}}`
		_, _, err := parseConfig([]byte(body))
		if err == nil || !strings.Contains(err.Error(), "lazyApply.enhance_search") {
			t.Fatalf("want lazyApply.enhance_search error, got %v", err)
		}
	})

	t.Run("enums validated", func(t *testing.T) {
		cases := []struct{ key, value string }{
			{"thinking", "turbo"},
			{"convertOff2Low", "always"},
			{"claudeSearchThinking", "huge"},
			{"claudeSearchSummary", "forceUltra"},
			{"classifier_thinking", "mid"},
			{"classifier_thinking_cc", "high"},
			{"classifier_thinking_codex", "none"},
			{"summary_level", "ultra"},
		}
		for _, tc := range cases {
			body := `{"upstream":"http://u","lazyApply":{"` + tc.key + `":"` + tc.value + `"}}`
			_, _, err := parseConfig([]byte(body))
			if err == nil || !strings.Contains(err.Error(), "lazyApply") {
				t.Errorf("lazyApply.%s=%q: want a lazyApply validation error, got %v", tc.key, tc.value, err)
			}
		}
		body := `{"upstream":"http://u","lazyApply":{"enhance_search":{"summary_level":"ultra"}}}`
		_, _, err := parseConfig([]byte(body))
		if err == nil || !strings.Contains(err.Error(), "lazyApply.enhance_search") {
			t.Errorf("nested summary_level: want a lazyApply.enhance_search error, got %v", err)
		}
	})

	t.Run("null and absent mean no defaults", func(t *testing.T) {
		for _, body := range []string{
			`{"upstream":"http://u"}`,
			`{"upstream":"http://u","lazyApply":null}`,
		} {
			if c := mustParseLazy(t, body); c.LazyApply != nil {
				t.Errorf("%s: LazyApply should stay nil, got %+v", body, c.LazyApply)
			}
		}
	})
}

// TestLazyApplyPresence locks explicit-key detection: an explicit false/null is written and must stop
// inheritance, while an absent key inherits. A nil presence map (Go literal) keeps the non-zero convention.
func TestLazyApplyPresence(t *testing.T) {
	c := mustParseLazy(t, `{"upstream":"http://u","lazyApply":{"text_only":false,"noBullshitOnEmptyClaudeSearch":null}}`)
	if c.LazyApply == nil {
		t.Fatal("LazyApply is nil")
	}
	if !c.LazyApply.presence.written("text_only", false) {
		t.Error("explicit text_only:false must count as written")
	}
	if c.LazyApply.presence.written("no_search", false) {
		t.Error("absent no_search must not count as written")
	}
	if !c.LazyApply.presence.written("noBullshitOnEmptyClaudeSearch", false) {
		t.Error("explicit guard:null must count as written")
	}
	if c.LazyApply.NoBullshitOnEmptyClaudeSearch != nil {
		t.Errorf("guard null should decode to nil, got %v", *c.LazyApply.NoBullshitOnEmptyClaudeSearch)
	}

	c = mustParseLazy(t, `{"upstream":"http://u","routes":[{"pattern":"a","url":"http://a","text_only":false}]}`)
	if !c.Routes[0].presence.written("text_only", false) {
		t.Error("route explicit text_only:false must count as written")
	}
	if c.Routes[0].presence.written("no_search", false) {
		t.Error("route absent no_search must not count as written")
	}
}

// TestLazyApplyEffectiveRouteFields locks the per-object rule for ordinary fields: explicit local value
// wins (including explicit false / empty), otherwise lazyApply, otherwise the built-in zero. noLazyApply
// on the object removes only the inherited layer; local values stay authoritative.
func TestLazyApplyEffectiveRouteFields(t *testing.T) {
	c := mustParseLazy(t, `{
		"upstream":"http://u",
		"lazyApply":{"text_only":true,"no_search":true,"thinking":"adaptive","convertOff2Low":"translate"},
		"routes":[
			{"pattern":"a","url":"http://a"},
			{"pattern":"b","url":"http://b","noLazyApply":true},
			{"pattern":"c","url":"http://c","text_only":false,"no_search":false,"thinking":"budget","convertOff2Low":"all"},
			{"pattern":"d","url":"http://d","noLazyApply":true,"text_only":true,"no_search":true,"thinking":"adaptive","convertOff2Low":"all"}
		],
		"fast_route":{"url":"http://f"},
		"multimodal_fallback":{"url":"http://m","noLazyApply":true},
		"search_fallback":{"url":"http://s"}
	}`)
	r := c.Routes

	if !effectiveRouteTextOnly(c, &r[0]) || !effectiveRouteNoSearch(c, &r[0]) {
		t.Error("route a should inherit text_only/no_search")
	}
	if effectiveRouteTextOnly(c, &r[1]) || effectiveRouteNoSearch(c, &r[1]) {
		t.Error("route b noLazyApply should ignore the inherited booleans")
	}
	if effectiveRouteTextOnly(c, &r[2]) || effectiveRouteNoSearch(c, &r[2]) {
		t.Error("route c explicit false should beat lazyApply true")
	}
	if !effectiveRouteTextOnly(c, &r[3]) || !effectiveRouteNoSearch(c, &r[3]) {
		t.Error("route d noLazyApply keeps its local true values")
	}

	if got := effectiveRouteThinking(c, &r[0]); got != "adaptive" {
		t.Errorf("route a thinking = %q, want adaptive", got)
	}
	if got := effectiveRouteThinking(c, &r[1]); got != "" {
		t.Errorf("route b noLazyApply thinking = %q, want empty", got)
	}
	if got := effectiveRouteThinking(c, &r[2]); got != "budget" {
		t.Errorf("route c explicit thinking = %q, want budget", got)
	}
	if got := effectiveRouteThinking(c, &r[3]); got != "adaptive" {
		t.Errorf("route d local thinking = %q, want adaptive", got)
	}

	conv := func(local string, written, optOut bool) string {
		return effectiveConvertOff2Low(c, local, written, optOut)
	}
	if got := conv(r[0].ConvertOff2Low, r[0].presence.written("convertOff2Low", r[0].ConvertOff2Low != ""), r[0].NoLazyApply); got != "translate" {
		t.Errorf("route a convertOff2Low = %q, want translate", got)
	}
	if got := conv(r[1].ConvertOff2Low, r[1].presence.written("convertOff2Low", r[1].ConvertOff2Low != ""), r[1].NoLazyApply); got != "" {
		t.Errorf("route b convertOff2Low = %q, want empty", got)
	}
	if got := conv(r[2].ConvertOff2Low, r[2].presence.written("convertOff2Low", r[2].ConvertOff2Low != ""), r[2].NoLazyApply); got != "all" {
		t.Errorf("route c convertOff2Low = %q, want all", got)
	}
	if got := conv(c.FastRoute.ConvertOff2Low, c.FastRoute.presence.written("convertOff2Low", c.FastRoute.ConvertOff2Low != ""), c.FastRoute.NoLazyApply); got != "translate" {
		t.Errorf("fast_route convertOff2Low = %q, want translate", got)
	}
	if got := conv(c.MultimodalFallback.ConvertOff2Low, c.MultimodalFallback.presence.written("convertOff2Low", c.MultimodalFallback.ConvertOff2Low != ""), c.MultimodalFallback.NoLazyApply); got != "" {
		t.Errorf("multimodal noLazyApply convertOff2Low = %q, want empty", got)
	}
	if got := conv(c.SearchFallback.ConvertOff2Low, c.SearchFallback.presence.written("convertOff2Low", c.SearchFallback.ConvertOff2Low != ""), c.SearchFallback.NoLazyApply); got != "translate" {
		t.Errorf("search_fallback convertOff2Low = %q, want translate", got)
	}

	// Go literals have no presence metadata: non-zero means "written", and lazyApply still fills the rest.
	lit := &Config{LazyApply: &LazyApplyConfig{TextOnly: true, Thinking: "adaptive", ConvertOff2Low: "translate"},
		Routes: []RouteRule{{Pattern: "x", URL: "http://x"}, {Pattern: "y", URL: "http://y", TextOnly: true}}}
	if !effectiveRouteTextOnly(lit, &lit.Routes[0]) {
		t.Error("Go literal route should inherit text_only")
	}
	if effectiveRouteNoSearch(lit, &lit.Routes[0]) {
		t.Error("Go literal route with no lazy no_search should stay false")
	}
	if got := effectiveRouteThinking(lit, &lit.Routes[0]); got != "adaptive" {
		t.Errorf("Go literal route thinking = %q, want adaptive", got)
	}
	if !effectiveRouteTextOnly(lit, &lit.Routes[1]) {
		t.Error("Go literal local true should win")
	}
}

// TestLazyApplyEffectiveClassifier locks the classifier chain: local per-source > local shared >
// lazy per-source > lazy shared > empty. A key explicitly written as "" blocks the lazy value for that key.
func TestLazyApplyEffectiveClassifier(t *testing.T) {
	c := mustParseLazy(t, `{
		"upstream":"http://u",
		"lazyApply":{"classifier_thinking":"off","classifier_thinking_cc":"low","classifier_thinking_codex":"off"},
		"classifier_route":{"url":"http://c"}
	}`)
	if got := classifierThinkingModeFor(c, classifierCC); got != "low" {
		t.Errorf("cc inherited mode = %q, want low", got)
	}
	if got := classifierThinkingModeFor(c, classifierCodex); got != "off" {
		t.Errorf("codex inherited mode = %q, want off", got)
	}

	c = mustParseLazy(t, `{
		"upstream":"http://u",
		"lazyApply":{"classifier_thinking":"off","classifier_thinking_cc":"low"},
		"classifier_route":{"url":"http://c","classifier_thinking":"","classifier_thinking_cc":""}
	}`)
	if got := classifierThinkingModeFor(c, classifierCC); got != "" {
		t.Errorf("explicit empty cc/shared must block lazy values, got %q", got)
	}
	if got := classifierThinkingModeFor(c, classifierCodex); got != "" {
		t.Errorf("explicit empty shared must block lazy shared, got %q", got)
	}

	c = mustParseLazy(t, `{
		"upstream":"http://u",
		"lazyApply":{"classifier_thinking":"off","classifier_thinking_cc":"low","classifier_thinking_codex":"off"},
		"classifier_route":{"url":"http://c","noLazyApply":true}
	}`)
	for _, kind := range []classifierSource{classifierCC, classifierCodex} {
		if got := classifierThinkingModeFor(c, kind); got != "" {
			t.Errorf("noLazyApply classifier mode = %q, want empty", got)
		}
	}

	// Local shared still coexists with an inherited per-source value.
	c = mustParseLazy(t, `{
		"upstream":"http://u",
		"lazyApply":{"classifier_thinking_cc":"low"},
		"classifier_route":{"url":"http://c","classifier_thinking":"off"}
	}`)
	if got := classifierThinkingModeFor(c, classifierCC); got != "low" {
		t.Errorf("inherited cc should win over local shared off, got %q", got)
	}
}

// TestLazyApplyEffectiveDirectSearch locks the direct-route search merge: each field resolves
// independently (local > lazy > built-in), while enhance_search is atomic (local object or null wins whole).
func TestLazyApplyEffectiveDirectSearch(t *testing.T) {
	c := mustParseLazy(t, `{
		"upstream":"http://u",
		"lazyApply":{"claudeSearchThinking":"max","claudeSearchSummary":"forceHigh",
			"enhance_search":{"summary_thinking":true,"summary_level":"high"}},
		"routes":[{"pattern":"a","url":"http://a"}]
	}`)
	inject, think := resolveSearchBehavior(effectiveDirectSearchSettings(c, &c.Routes[0]))
	if inject != "high" || think != "max" {
		t.Errorf("inherited direct search = (%q,%q), want (high,max)", inject, think)
	}

	c = mustParseLazy(t, `{
		"upstream":"http://u",
		"lazyApply":{"claudeSearchThinking":"max","claudeSearchSummary":"forceHigh"},
		"routes":[{"pattern":"a","url":"http://a","claudeSearchThinking":"off","claudeSearchSummary":"noForce"}]
	}`)
	inject, think = resolveSearchBehavior(effectiveDirectSearchSettings(c, &c.Routes[0]))
	if inject != "" || think != "off" {
		t.Errorf("local direct search = (%q,%q), want empty/off", inject, think)
	}

	c = mustParseLazy(t, `{
		"upstream":"http://u",
		"lazyApply":{"enhance_search":{"summary_thinking":true,"summary_level":"high"}},
		"routes":[{"pattern":"a","url":"http://a","enhance_search":null}]
	}`)
	inject, think = resolveSearchBehavior(effectiveDirectSearchSettings(c, &c.Routes[0]))
	if inject != "" || think != "" {
		t.Errorf("explicit null enhance_search = (%q,%q), want empty", inject, think)
	}

	c = mustParseLazy(t, `{
		"upstream":"http://u",
		"lazyApply":{"claudeSearchThinking":"max","enhance_search":{"summary_thinking":true,"summary_level":"high"}},
		"routes":[{"pattern":"a","url":"http://a","enhance_search":{}}]
	}`)
	inject, think = resolveSearchBehavior(effectiveDirectSearchSettings(c, &c.Routes[0]))
	if inject != "low" || think != "max" {
		t.Errorf("local empty enhance object = (%q,%q), want (low,max)", inject, think)
	}

	c = mustParseLazy(t, `{
		"upstream":"http://u",
		"lazyApply":{"claudeSearchThinking":"max","claudeSearchSummary":"forceHigh","enhance_search":{"summary_level":"high"}},
		"routes":[{"pattern":"a","url":"http://a","noLazyApply":true}]
	}`)
	inject, think = resolveSearchBehavior(effectiveDirectSearchSettings(c, &c.Routes[0]))
	if inject != "" || think != "" {
		t.Errorf("direct noLazyApply = (%q,%q), want empty", inject, think)
	}
}

// TestLazyApplyEffectiveFallbackSearch locks the diverted-request merge. lazyApply behaves as if the
// supported keys were written into each object; the existing route-over-fallback precedence then applies.
func TestLazyApplyEffectiveFallbackSearch(t *testing.T) {
	base := `{
		"upstream":"http://u",
		"lazyApply":{"convertOff2Low":"translate","summary_mode":true,"summary_thinking":true,
			"summary_level":"max","claudeSearchThinking":"max","claudeSearchSummary":"forceHigh",
			"noBullshitOnEmptyClaudeSearch":true},
		"routes":[{"pattern":"a","url":"http://a","no_search":true}],
		"search_fallback":{"url":"http://s"}
	}`
	c := mustParseLazy(t, base)
	eff := effectiveFallbackSearchSettings(c, &c.Routes[0], c.SearchFallback)
	inject, think := resolveSearchBehavior(eff)
	if inject != "high" || think != "max" {
		t.Errorf("inherited fallback search = (%q,%q), want (high,max)", inject, think)
	}
	if eff.ConvertOff2Low != "translate" {
		t.Errorf("fallback convertOff2Low = %q, want translate", eff.ConvertOff2Low)
	}
	if !effectiveSearchGuard(c, &c.Routes[0], c.SearchFallback) {
		t.Error("inherited guard should be true")
	}

	// A route's inherited value participates in route-over-fallback precedence.
	c = mustParseLazy(t, `{
		"upstream":"http://u",
		"lazyApply":{"claudeSearchThinking":"max","claudeSearchSummary":"forceHigh"},
		"routes":[{"pattern":"a","url":"http://a","no_search":true}],
		"search_fallback":{"url":"http://s","claudeSearchThinking":"low","claudeSearchSummary":"forceLow"}
	}`)
	inject, think = resolveSearchBehavior(effectiveFallbackSearchSettings(c, &c.Routes[0], c.SearchFallback))
	if inject != "high" || think != "max" {
		t.Errorf("route inherited values should beat fallback locals = (%q,%q), want (high,max)", inject, think)
	}

	// A route-local explicit value still beats its own inherited value and the fallback.
	c = mustParseLazy(t, `{
		"upstream":"http://u",
		"lazyApply":{"claudeSearchThinking":"max","claudeSearchSummary":"forceHigh"},
		"routes":[{"pattern":"a","url":"http://a","no_search":true,"claudeSearchThinking":"mid","claudeSearchSummary":"forceMid"}],
		"search_fallback":{"url":"http://s","claudeSearchThinking":"low","claudeSearchSummary":"forceLow"}
	}`)
	inject, think = resolveSearchBehavior(effectiveFallbackSearchSettings(c, &c.Routes[0], c.SearchFallback))
	if inject != "mid" || think != "mid" {
		t.Errorf("route locals = (%q,%q), want (mid,mid)", inject, think)
	}

	// Route noLazyApply removes the route layer; the fallback's own explicit value wins.
	c = mustParseLazy(t, `{
		"upstream":"http://u",
		"lazyApply":{"claudeSearchThinking":"max","claudeSearchSummary":"forceHigh"},
		"routes":[{"pattern":"a","url":"http://a","no_search":true,"noLazyApply":true}],
		"search_fallback":{"url":"http://s","claudeSearchThinking":"low","claudeSearchSummary":"forceLow"}
	}`)
	inject, think = resolveSearchBehavior(effectiveFallbackSearchSettings(c, &c.Routes[0], c.SearchFallback))
	if inject != "low" || think != "low" {
		t.Errorf("route noLazyApply should expose fallback locals = (%q,%q), want (low,low)", inject, think)
	}

	// Fallback noLazyApply removes only the fallback layer.
	c = mustParseLazy(t, `{
		"upstream":"http://u",
		"lazyApply":{"claudeSearchThinking":"max","claudeSearchSummary":"forceHigh"},
		"routes":[{"pattern":"a","url":"http://a","no_search":true}],
		"search_fallback":{"url":"http://s","noLazyApply":true,"claudeSearchThinking":"low","claudeSearchSummary":"forceLow"}
	}`)
	inject, think = resolveSearchBehavior(effectiveFallbackSearchSettings(c, &c.Routes[0], c.SearchFallback))
	if inject != "high" || think != "max" {
		t.Errorf("fallback noLazyApply should still let route inherited values win = (%q,%q), want (high,max)", inject, think)
	}

	// nil fallback: the resolver returns the built-in zero object.
	if eff := effectiveFallbackSearchSettings(c, &c.Routes[0], nil); eff == nil || eff.ClaudeSearchThinking != "" {
		t.Errorf("nil fallback should return an empty view, got %+v", eff)
	}
}

// TestLazyApplyEffectiveGuard locks the guard layers: route local > route lazy > fallback local >
// fallback lazy > false, with noLazyApply removing only its own object's inherited layer. An unmatched
// request (no route and no fallback) never arms the guard.
func TestLazyApplyEffectiveGuard(t *testing.T) {
	cases := []struct {
		name string
		body string
		want bool
	}{
		{"route local false beats all", `{"upstream":"http://u","lazyApply":{"noBullshitOnEmptyClaudeSearch":true},
			"routes":[{"pattern":"a","url":"http://a","noBullshitOnEmptyClaudeSearch":false}],
			"search_fallback":{"url":"http://s","noBullshitOnEmptyClaudeSearch":true}}`, false},
		{"route lazy beats fallback local false", `{"upstream":"http://u","lazyApply":{"noBullshitOnEmptyClaudeSearch":true},
			"routes":[{"pattern":"a","url":"http://a"}],
			"search_fallback":{"url":"http://s","noBullshitOnEmptyClaudeSearch":false}}`, true},
		{"route noLazyApply exposes fallback local false", `{"upstream":"http://u","lazyApply":{"noBullshitOnEmptyClaudeSearch":true},
			"routes":[{"pattern":"a","url":"http://a","noLazyApply":true}],
			"search_fallback":{"url":"http://s","noBullshitOnEmptyClaudeSearch":false}}`, false},
		{"fallback local true", `{"upstream":"http://u",
			"routes":[{"pattern":"a","url":"http://a"}],
			"search_fallback":{"url":"http://s","noBullshitOnEmptyClaudeSearch":true}}`, true},
		{"fallback lazy true", `{"upstream":"http://u","lazyApply":{"noBullshitOnEmptyClaudeSearch":true},
			"routes":[{"pattern":"a","url":"http://a"}],
			"search_fallback":{"url":"http://s"}}`, true},
		{"fallback noLazyApply", `{"upstream":"http://u","lazyApply":{"noBullshitOnEmptyClaudeSearch":true},
			"routes":[{"pattern":"a","url":"http://a","noLazyApply":true}],
			"search_fallback":{"url":"http://s","noLazyApply":true}}`, false},
		{"route explicit null beats lazy", `{"upstream":"http://u","lazyApply":{"noBullshitOnEmptyClaudeSearch":true},
			"routes":[{"pattern":"a","url":"http://a","noBullshitOnEmptyClaudeSearch":null}],
			"search_fallback":{"url":"http://s","noBullshitOnEmptyClaudeSearch":true}}`, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := mustParseLazy(t, tc.body)
			rr := &c.Routes[0]
			if got := effectiveSearchGuard(c, rr, c.SearchFallback); got != tc.want {
				t.Errorf("effectiveSearchGuard = %v, want %v", got, tc.want)
			}
		})
	}

	// Direct route (no fallback object): route local > route lazy > false.
	c := mustParseLazy(t, `{"upstream":"http://u","lazyApply":{"noBullshitOnEmptyClaudeSearch":true},
		"routes":[{"pattern":"a","url":"http://a"}]}`)
	if !effectiveSearchGuard(c, &c.Routes[0], nil) {
		t.Error("direct route should inherit guard true")
	}
	if effectiveSearchGuard(c, nil, nil) {
		t.Error("unmatched request must not arm the guard")
	}

	// Fallback-only scope (no matched route): fallback local > fallback lazy > false.
	c = mustParseLazy(t, `{"upstream":"http://u","lazyApply":{"noBullshitOnEmptyClaudeSearch":true},
		"search_fallback":{"url":"http://s"}}`)
	if !effectiveSearchGuard(c, nil, c.SearchFallback) {
		t.Error("fallback-only scope should inherit guard true")
	}
}

// TestLazyApplyPatternArrayAndVars locks the two expansion interactions: a lazy string may use $VAR, and
// every route expanded from a pattern array receives the same lazy defaults and explicit local values.
func TestLazyApplyPatternArrayAndVars(t *testing.T) {
	c := mustParseLazy(t, `{
		"vars":{"TH":"adaptive","ST":"max"},
		"upstream":"http://u",
		"lazyApply":{"thinking":"$TH","claudeSearchThinking":"$ST","text_only":true},
		"routes":[{"pattern":["a*","b*"],"url":"http://x","text_only":false}]
	}`)
	if c.LazyApply.Thinking != "adaptive" || c.LazyApply.ClaudeSearchThinking != "max" {
		t.Errorf("lazyApply var expansion = thinking %q / claudeSearchThinking %q, want adaptive / max", c.LazyApply.Thinking, c.LazyApply.ClaudeSearchThinking)
	}
	if len(c.Routes) != 2 {
		t.Fatalf("pattern array should expand to 2 routes, got %d", len(c.Routes))
	}
	for i := range c.Routes {
		if got := effectiveRouteThinking(c, &c.Routes[i]); got != "adaptive" {
			t.Errorf("expanded route %d thinking = %q, want adaptive", i, got)
		}
		if _, got := resolveSearchBehavior(effectiveDirectSearchSettings(c, &c.Routes[i])); got != "max" {
			t.Errorf("expanded route %d claudeSearchThinking = %q, want max", i, got)
		}
		if effectiveRouteTextOnly(c, &c.Routes[i]) {
			t.Errorf("expanded route %d should keep its explicit text_only:false", i)
		}
		if !c.Routes[i].presence.written("text_only", false) {
			t.Errorf("expanded route %d lost presence metadata", i)
		}
	}
}

// TestLazyApplyGoLiteralPresenceFallback locks the no-metadata convention used throughout the test suite:
// Go-literal route structs treat non-zero values as written while still inheriting omitted lazy defaults.
func TestLazyApplyGoLiteralPresenceFallback(t *testing.T) {
	c := &Config{
		LazyApply: &LazyApplyConfig{NoSearch: true, ConvertOff2Low: "translate"},
		Routes:    []RouteRule{{Pattern: "a", URL: "http://a"}},
	}
	if !effectiveRouteNoSearch(c, &c.Routes[0]) {
		t.Error("Go literal route should inherit no_search")
	}
	// A Go literal cannot express explicit false without metadata; it inherits. This is the documented convention.
	if !effectiveRouteNoSearch(c, &RouteRule{Pattern: "b", URL: "http://b"}) {
		t.Error("Go literal with no metadata should inherit no_search")
	}
	if got := effectiveConvertOff2Low(c, "", false, false); got != "translate" {
		t.Errorf("Go literal convertOff2Low = %q, want translate", got)
	}
	if got := effectiveConvertOff2Low(c, "all", true, false); got != "all" {
		t.Errorf("Go literal local convertOff2Low = %q, want all", got)
	}
	if got := effectiveConvertOff2Low(c, "", false, true); got != "" {
		t.Errorf("Go literal opt-out convertOff2Low = %q, want empty", got)
	}
}

// TestLazyApplyEffectiveFallbackThinking locks search_fallback.thinking: when the fallback serves a
// diverted search request, its own value (explicit > lazyApply > "") describes the target model. The
// hit route's thinking describes the route's own model and must not participate.
func TestLazyApplyEffectiveFallbackThinking(t *testing.T) {
	c := mustParseLazy(t, `{
		"upstream":"http://u",
		"lazyApply":{"thinking":"adaptive"},
		"routes":[
			{"pattern":"a","url":"http://a","thinking":"budget"},
			{"pattern":"b","url":"http://b"}
		],
		"search_fallback":{"url":"http://s","thinking":"auto"}
	}`)
	if got := effectiveFallbackThinking(c, c.SearchFallback); got != "auto" {
		t.Errorf("explicit fallback thinking = %q, want auto", got)
	}
	if got := effectiveRouteThinking(c, &c.Routes[0]); got != "budget" {
		t.Errorf("route thinking = %q, want budget", got)
	}

	inherit := mustParseLazy(t, `{
		"upstream":"http://u",
		"lazyApply":{"thinking":"budget"},
		"search_fallback":{"url":"http://s"}
	}`)
	if got := effectiveFallbackThinking(inherit, inherit.SearchFallback); got != "budget" {
		t.Errorf("lazy fallback thinking = %q, want budget", got)
	}

	optOut := mustParseLazy(t, `{
		"upstream":"http://u",
		"lazyApply":{"thinking":"budget"},
		"search_fallback":{"url":"http://s","noLazyApply":true}
	}`)
	if got := effectiveFallbackThinking(optOut, optOut.SearchFallback); got != "" {
		t.Errorf("noLazyApply fallback thinking = %q, want empty", got)
	}

	if got := effectiveFallbackThinking(inherit, nil); got != "" {
		t.Errorf("nil fallback thinking = %q, want empty", got)
	}
}
