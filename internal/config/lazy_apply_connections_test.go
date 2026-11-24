package config

import "testing"

// TestLazyApplyConnectionDefaults locks the connection trio's inheritance contract: a route-like object
// that omits url/api/model receives lazyApply's values, a locally written key wins even when empty (an
// explicit "" is not inherited and keeps the object's empty-value behavior), and noLazyApply opts an
// object out entirely.
func TestLazyApplyConnectionDefaults(t *testing.T) {
	c := mustParseLazy(t, `{
		"upstream": "http://u",
		"lazyApply": {"url": "http://d", "api": "sk-d", "model": "m-d"},
		"routes": [
			{"pattern": "a"},
			{"pattern": "b", "url": "http://b", "api": "", "model": ""},
			{"pattern": "c", "noLazyApply": true}
		],
		"classifier_route": {},
		"fast_route": {},
		"multimodal_fallback": {},
		"search_fallback": {}
	}`)

	wantURLs := []string{"http://d", "http://b", ""}
	for i, want := range wantURLs {
		rr := c.Routes[i]
		if rr.URL != want {
			t.Errorf("route %d url = %q, want %q", i, rr.URL, want)
		}
	}
	if rr := c.Routes[0]; rr.API != "sk-d" || rr.Model != "m-d" {
		t.Errorf("route 0 should inherit api/model: %+v", rr)
	}
	if rr := c.Routes[1]; rr.API != "" || rr.Model != "" {
		t.Errorf("locally written empty api/model must beat lazyApply: %+v", rr)
	}
	if rr := c.Routes[2]; rr.API != "" || rr.Model != "" {
		t.Errorf("noLazyApply route must inherit nothing: %+v", rr)
	}

	objects := map[string]struct{ url, api, model string }{
		"classifier_route":    {c.ClassifierRoute.URL, c.ClassifierRoute.API, c.ClassifierRoute.Model},
		"fast_route":          {c.FastRoute.URL, c.FastRoute.API, c.FastRoute.Model},
		"multimodal_fallback": {c.MultimodalFallback.URL, c.MultimodalFallback.API, c.MultimodalFallback.Model},
		"search_fallback":     {c.SearchFallback.URL, c.SearchFallback.API, c.SearchFallback.Model},
	}
	for name, got := range objects {
		if got.url != "http://d" || got.api != "sk-d" || got.model != "m-d" {
			t.Errorf("%s should inherit url/api/model, got %+v", name, got)
		}
	}
}

// TestLazyApplyConnectionPatternArray locks the documented compact shape: a pattern array expands into
// adjacent copies and both copies still inherit the connection defaults, so
// {"pattern":["*fable*","*opus*"],"effort":"max"} needs no url/api/model of its own.
func TestLazyApplyConnectionPatternArray(t *testing.T) {
	c := mustParseLazy(t, `{"upstream":"http://u",
		"lazyApply":{"url":"http://d","api":"sk-d","model":"m-d"},
		"routes":[{"pattern":["*fable*","*opus*"],"effort":"max"}]}`)
	if len(c.Routes) != 2 {
		t.Fatalf("pattern array should expand to 2 routes, got %d", len(c.Routes))
	}
	for i, rr := range c.Routes {
		if rr.URL != "http://d" || rr.API != "sk-d" || rr.Model != "m-d" {
			t.Errorf("expanded route %d should inherit connection defaults: %+v", i, rr)
		}
		if rr.Effort != "max" {
			t.Errorf("expanded route %d should keep its effort pin: %+v", i, rr)
		}
	}
}

// TestLazyApplyConnectionGoLiteral locks the programmatic-config convention: without presence metadata a
// non-zero field counts as locally set and an empty one inherits.
func TestLazyApplyConnectionGoLiteral(t *testing.T) {
	c := &Config{
		LazyApply: &LazyApplyConfig{URL: "http://d", API: "sk-d", Model: "m-d"},
		Routes:    []RouteRule{{Pattern: "a"}, {Pattern: "b", URL: "http://b"}},
		FastRoute: &FastRoute{},
	}
	applyLazyApplyConnections(c)
	if rr := c.Routes[0]; rr.URL != "http://d" || rr.API != "sk-d" || rr.Model != "m-d" {
		t.Errorf("empty route should inherit, got %+v", rr)
	}
	if rr := c.Routes[1]; rr.URL != "http://b" || rr.API != "sk-d" || rr.Model != "m-d" {
		t.Errorf("non-zero url should win while empty api/model inherit, got %+v", rr)
	}
	if c.FastRoute.URL != "http://d" {
		t.Errorf("fast_route should inherit, got %+v", c.FastRoute)
	}
}

// TestLazyApplyConnectionCaseVariantKeys locks parity with encoding/json's case-insensitive field
// matching: "URL" / "Api" / "MODEL" inside a route count as locally written (never overwritten),
// "NoLazyApply" still opts an object out, and the top-level "LazyApply" / "Routes" / "Fast_Route"
// spellings resolve exactly like the canonical keys (including pattern-array expansion).
func TestLazyApplyConnectionCaseVariantKeys(t *testing.T) {
	c := mustParseLazy(t, `{
		"upstream": "http://u",
		"LazyApply": {"URL": "http://d", "Api": "sk-d", "Model": "m-d"},
		"Routes": [
			{"pattern": "a", "URL": "http://own", "Api": "", "MODEL": ""},
			{"pattern": "b"},
			{"pattern": ["c1", "c2"], "NoLazyApply": true}
		],
		"Fast_Route": {"Url": "http://f"}
	}`)
	if len(c.Routes) != 4 {
		t.Fatalf("pattern array under case-variant Routes should expand to 4 routes, got %d", len(c.Routes))
	}
	if rr := c.Routes[0]; rr.URL != "http://own" || rr.API != "" || rr.Model != "" {
		t.Errorf("case-variant local keys must beat lazyApply (empty included): %+v", rr)
	}
	if rr := c.Routes[1]; rr.URL != "http://d" || rr.API != "sk-d" || rr.Model != "m-d" {
		t.Errorf("bare route should inherit case-variant lazyApply connection defaults: %+v", rr)
	}
	for i, pattern := range []string{"c1", "c2"} {
		rr := c.Routes[2+i]
		if rr.Pattern != pattern {
			t.Errorf("expanded route %d pattern = %q, want %q", 2+i, rr.Pattern, pattern)
		}
		if rr.URL != "" || rr.API != "" || rr.Model != "" {
			t.Errorf("case-variant NoLazyApply route must inherit nothing: %+v", rr)
		}
	}
	if c.FastRoute == nil || c.FastRoute.URL != "http://f" || c.FastRoute.API != "sk-d" || c.FastRoute.Model != "m-d" {
		t.Errorf("case-variant Fast_Route should keep its Url and inherit api/model: %+v", c.FastRoute)
	}
}

// TestLazyApplyConnectionNoDefaults locks the no-op cases: without lazyApply, or without connection
// defaults in it, every object keeps exactly what it wrote.
func TestLazyApplyConnectionNoDefaults(t *testing.T) {
	for _, body := range []string{
		`{"upstream":"http://u","routes":[{"pattern":"a","url":"http://a"}]}`,
		`{"upstream":"http://u","lazyApply":{"text_only":true},"routes":[{"pattern":"a","url":"http://a"}]}`,
	} {
		c := mustParseLazy(t, body)
		if c.Routes[0].URL != "http://a" || c.Routes[0].API != "" || c.Routes[0].Model != "" {
			t.Errorf("%s: route must keep its own values, got %+v", body, c.Routes[0])
		}
	}
}
