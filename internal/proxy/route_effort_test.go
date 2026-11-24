package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"proxy429/internal/config"
	"proxy429/internal/logview"
	"proxy429/internal/translate"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"
)

// TestForceAdaptiveEffort locks the body rewrite contract: only adaptive thinking is touched, any existing
// output_config is replaced whole, mid normalizes to medium, and every non-applicable shape returns the body
// byte-identical (so no request is disturbed and no badge is shown).
func TestForceAdaptiveEffort(t *testing.T) {
	cases := []struct {
		name         string
		body         string
		effort       string
		wantChanged  bool
		wantLevel    string
		wantContains []string
		wantAbsent   []string
	}{
		{
			name:         "replaces the carried effort in place",
			body:         `{"model":"M","thinking":{"type":"adaptive"},"output_config":{"effort":"high"},"messages":[]}`,
			effort:       "low",
			wantChanged:  true,
			wantLevel:    "low",
			wantContains: []string{`"effort":"low"`, `"thinking":{"type":"adaptive"}`},
			wantAbsent:   []string{`"effort":"high"`},
		},
		{
			name:         "appends output_config when absent",
			body:         `{"model":"M","thinking":{"type":"adaptive"},"messages":[]}`,
			effort:       "low",
			wantChanged:  true,
			wantLevel:    "low",
			wantContains: []string{`"effort":"low"`},
		},
		{
			name:        "already at the target level is a no-op",
			body:        `{"model":"M","thinking":{"type":"adaptive"},"output_config":{"effort":"max"},"messages":[]}`,
			effort:      "max",
			wantChanged: false,
		},
		{
			name:        "mid target equals a medium body",
			body:        `{"model":"M","thinking":{"type":"adaptive"},"output_config":{"effort":"medium"},"messages":[]}`,
			effort:      "mid",
			wantChanged: false,
		},
		{
			name:         "medium body is raised to high",
			body:         `{"model":"M","thinking":{"type":"adaptive"},"output_config":{"effort":"medium"},"messages":[]}`,
			effort:       "high",
			wantChanged:  true,
			wantLevel:    "high",
			wantContains: []string{`"effort":"high"`},
			wantAbsent:   []string{`"effort":"medium"`},
		},
		{
			name:         "mid target is written upstream as medium",
			body:         `{"model":"M","thinking":{"type":"adaptive"},"output_config":{"effort":"high"},"messages":[]}`,
			effort:       "mid",
			wantChanged:  true,
			wantLevel:    "medium",
			wantContains: []string{`"effort":"medium"`},
			wantAbsent:   []string{`"effort":"high"`, `"effort":"mid"`},
		},
		{
			name:        "enabled budget passes through",
			body:        `{"model":"M","thinking":{"type":"enabled","budget_tokens":2048},"messages":[]}`,
			effort:      "max",
			wantChanged: false,
		},
		{
			name:        "disabled passes through",
			body:        `{"model":"M","thinking":{"type":"disabled"},"messages":[]}`,
			effort:      "max",
			wantChanged: false,
		},
		{
			name:        "no thinking passes through",
			body:        `{"model":"M","messages":[]}`,
			effort:      "max",
			wantChanged: false,
		},
		{
			name:        "null thinking passes through",
			body:        `{"model":"M","thinking":null,"output_config":{"effort":"high"},"messages":[]}`,
			effort:      "max",
			wantChanged: false,
		},
		{
			name:        "stray adaptive string inside a budget thinking object is not adaptive",
			body:        `{"model":"M","thinking":{"type":"enabled","budget_tokens":2048,"note":"adaptive"},"messages":[]}`,
			effort:      "max",
			wantChanged: false,
		},
		{
			name:        "adaptive mentioned in messages never decides",
			body:        `{"model":"M","thinking":{"type":"enabled","budget_tokens":2048},"messages":[{"role":"user","content":"please set thinking to adaptive"}]}`,
			effort:      "max",
			wantChanged: false,
		},
		{
			name:        "empty effort never touches the body",
			body:        `{"model":"M","thinking":{"type":"adaptive"},"output_config":{"effort":"high"},"messages":[]}`,
			effort:      "",
			wantChanged: false,
		},
		{
			name:        "unknown level never touches the body",
			body:        `{"model":"M","thinking":{"type":"adaptive"},"output_config":{"effort":"high"},"messages":[]}`,
			effort:      "bogus",
			wantChanged: false,
		},
		{
			name:        "malformed json passes through",
			body:        `{"thinking":{"type":"adaptive"}`,
			effort:      "low",
			wantChanged: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out, level, changed := forceAdaptiveEffort([]byte(tc.body), tc.effort)
			if changed != tc.wantChanged {
				t.Fatalf("changed = %v, want %v (out: %s)", changed, tc.wantChanged, out)
			}
			if level != tc.wantLevel {
				t.Errorf("level = %q, want %q", level, tc.wantLevel)
			}
			if !changed {
				if !bytes.Equal(out, []byte(tc.body)) {
					t.Errorf("an untouched body must stay byte-identical, got %s", out)
				}
				return
			}
			s := string(out)
			for _, sub := range tc.wantContains {
				if !strings.Contains(s, sub) {
					t.Errorf("missing %q: %s", sub, s)
				}
			}
			for _, sub := range tc.wantAbsent {
				if strings.Contains(s, sub) {
					t.Errorf("must not contain %q: %s", sub, s)
				}
			}
			var m map[string]any
			if err := json.Unmarshal(out, &m); err != nil {
				t.Errorf("rewritten body is not valid JSON: %v", err)
			}
		})
	}
}

const (
	effortAdaptiveHigh = `{"model":"claude-opus-4-8","max_tokens":64,"stream":true,"thinking":{"type":"adaptive"},"output_config":{"effort":"high"},"messages":[{"role":"user","content":"x"}]}`
	effortBudgetBody   = `{"model":"claude-opus-4-8","max_tokens":64,"stream":true,"thinking":{"type":"enabled","budget_tokens":2048},"messages":[{"role":"user","content":"x"}]}`
	effortSearchBody   = `{"model":"claude-opus-4-8","max_tokens":64,"stream":true,"thinking":{"type":"adaptive"},"output_config":{"effort":"high"},"tools":[{"type":"web_search_20250305","name":"web_search"}],"messages":[{"role":"user","content":"x"}]}`
	// effortStealthLow is the body the translated port already holds for an explicit thinking-off client after the
	// quiet off->low upgrade (adaptive+low, n2lMode stealth in ctx).
	effortStealthLow = `{"model":"claude-opus-4-8","max_tokens":64,"stream":true,"thinking":{"type":"adaptive"},"output_config":{"effort":"low"},"messages":[{"role":"user","content":"x"}]}`
	// effortSearchMarker is a standalone search sub-agent request (marker in the final user message, per the
	// last-message-only detector) with web_search available, so the route-level claudeSearchThinking rewrite fires.
	effortSearchMarker = `{"model":"claude-opus-4-8","max_tokens":64,"stream":true,"thinking":{"type":"adaptive"},"output_config":{"effort":"high"},"tools":[{"type":"web_search_20250305","name":"web_search"}],"messages":[{"role":"user","content":"Perform a web search for the query: X"}]}`
)

var thinkToRe = regexp.MustCompile(`"thinkTo":"([^"]*)"`)

// assertNoThinkRewriteBadge checks that no flight in the recent JSON carries a [from->to] rewrite badge. The
// finished map always emits the keys, so only a non-empty thinkTo value means a rewrite happened.
func assertNoThinkRewriteBadge(t *testing.T, recent string) {
	t.Helper()
	for _, m := range thinkToRe.FindAllStringSubmatch(recent, -1) {
		if m[1] != "" {
			t.Errorf("no thinking rewrite expected, got thinkTo=%q: %s", m[1], recent)
		}
	}
}

// runEffortE2E posts one native-port request through the real handler with a route mock and a search-fallback
// mock, then returns what each received plus the recent-flights JSON for badge assertions.
func runEffortE2E(t *testing.T, build func(routeURL, fbURL string) *config.Config, body string) (routeBodies, fbBodies [][]byte, routeCalled bool, recent string) {
	t.Helper()
	resetStats()
	var mu sync.Mutex
	routeHit := false
	routeMock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		routeHit = true
		routeBodies = append(routeBodies, b)
		mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		io.WriteString(w, testSSEAllBlocks())
	}))
	defer routeMock.Close()
	fbMock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		fbBodies = append(fbBodies, b)
		mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		io.WriteString(w, testSSEAllBlocks())
	}))
	defer fbMock.Close()

	config.Store(build(routeMock.URL, fbMock.URL))
	defer config.Store(&config.Config{})
	proxy := httptest.NewServer(http.HandlerFunc(handler))
	defer proxy.Close()
	resp, err := http.Post(proxy.URL+"/v1/messages", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()

	for i := 0; i < 100; i++ {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, logview.RecentFlightsPath, nil)
		req.RemoteAddr = "127.0.0.1:1"
		logview.RecentFlightsHandler(rec, req)
		recent = rec.Body.String()
		if strings.Contains(recent, `"model":"claude-opus-4-8`) {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	mu.Lock()
	defer mu.Unlock()
	return append([][]byte(nil), routeBodies...), append([][]byte(nil), fbBodies...), routeHit, recent
}

func effortBaseConfig(routeURL string) *config.Config {
	return &config.Config{
		Upstream:           "http://no-default.example.com",
		MaxRetries:         0,
		TotalBudgetSec:     10,
		RecentSampleWindow: 5,
		Routes:             []config.RouteRule{{Pattern: "claude-opus*", URL: routeURL, API: "sk-route", Model: "", Effort: "max"}},
	}
}

// TestRouteEffortForcesAdaptiveE2E locks the handler wiring: the route-level effort pin rewrites an adaptive
// request's effort whatever the client sent, and the recent flight carries the [from->to] badge fields from
// the pre-rewrite value to the level actually written.
func TestRouteEffortForcesAdaptiveE2E(t *testing.T) {
	bodies, _, _, recent := runEffortE2E(t, func(routeURL, fbURL string) *config.Config {
		return effortBaseConfig(routeURL)
	}, effortAdaptiveHigh)
	if len(bodies) != 1 {
		t.Fatalf("upstream should receive one request, got %d", len(bodies))
	}
	up := string(bodies[0])
	if !strings.Contains(up, `"effort":"max"`) || strings.Contains(up, `"effort":"high"`) {
		t.Errorf("route effort should force max over the client's high: %s", up)
	}
	if !strings.Contains(up, `"thinking":{"type":"adaptive"}`) {
		t.Errorf("thinking shape must stay adaptive: %s", up)
	}
	for _, sub := range []string{`"thinkFrom":"high"`, `"thinkTo":"max"`} {
		if !strings.Contains(recent, sub) {
			t.Errorf("recent flight should carry %q: %s", sub, recent)
		}
	}
}

// TestRouteWithoutEffortLeavesBodyAloneE2E locks the opt-out: a route without effort forwards the client's
// bytes untouched and shows no rewrite badge.
func TestRouteWithoutEffortLeavesBodyAloneE2E(t *testing.T) {
	bodies, _, _, recent := runEffortE2E(t, func(routeURL, fbURL string) *config.Config {
		c := effortBaseConfig(routeURL)
		c.Routes[0].Effort = ""
		return c
	}, effortAdaptiveHigh)
	if len(bodies) != 1 {
		t.Fatalf("upstream should receive one request, got %d", len(bodies))
	}
	if !bytes.Equal(bodies[0], []byte(effortAdaptiveHigh)) {
		t.Errorf("a route without effort must not touch the body:\n got: %s\nwant: %s", bodies[0], effortAdaptiveHigh)
	}
	assertNoThinkRewriteBadge(t, recent)
}

// TestRouteEffortLeavesBudgetShapeAloneE2E locks the adaptive-only gate: an enabled+budget body passes through
// byte-identical even on a route with an effort pin.
func TestRouteEffortLeavesBudgetShapeAloneE2E(t *testing.T) {
	bodies, _, _, recent := runEffortE2E(t, func(routeURL, fbURL string) *config.Config {
		return effortBaseConfig(routeURL)
	}, effortBudgetBody)
	if len(bodies) != 1 {
		t.Fatalf("upstream should receive one request, got %d", len(bodies))
	}
	if !bytes.Equal(bodies[0], []byte(effortBudgetBody)) {
		t.Errorf("budget thinking must pass through untouched:\n got: %s\nwant: %s", bodies[0], effortBudgetBody)
	}
	assertNoThinkRewriteBadge(t, recent)
}

// TestRouteEffortNotAppliedAfterSearchFallbackE2E locks the capture site: a request diverted to search_fallback
// never inherits the hit route's effort pin (the fallback is the route that actually serves it).
func TestRouteEffortNotAppliedAfterSearchFallbackE2E(t *testing.T) {
	routeBodies, fbBodies, routeCalled, recent := runEffortE2E(t, func(routeURL, fbURL string) *config.Config {
		c := effortBaseConfig(routeURL)
		c.Routes[0].NoSearch = true
		c.SearchFallback = &config.SearchRoute{URL: fbURL, API: "sk-sf"}
		return c
	}, effortSearchBody)
	if routeCalled || len(routeBodies) != 0 {
		t.Errorf("the no_search route upstream must not be hit, got %d bodies", len(routeBodies))
	}
	if len(fbBodies) != 1 {
		t.Fatalf("search_fallback should receive one request, got %d", len(fbBodies))
	}
	up := string(fbBodies[0])
	if !strings.Contains(up, `"effort":"high"`) || strings.Contains(up, `"effort":"max"`) {
		t.Errorf("the hit route's effort pin must not apply after a search fallback: %s", up)
	}
	assertNoThinkRewriteBadge(t, recent)
}

// TestSearchRewriteWinsOverRouteEffortE2E locks the rewrite precedence on one request: the route effort pin runs
// first, then the search sub-agent thinking rewrite owns both the final body and the badge — the API column reads
// [max->low] (the pin's value being the prior state), and the upstream receives the search level.
func TestSearchRewriteWinsOverRouteEffortE2E(t *testing.T) {
	bodies, _, _, recent := runEffortE2E(t, func(routeURL, fbURL string) *config.Config {
		c := effortBaseConfig(routeURL)
		c.Routes[0].ClaudeSearchThinking = "low"
		return c
	}, effortSearchMarker)
	if len(bodies) != 1 {
		t.Fatalf("upstream should receive one request, got %d", len(bodies))
	}
	up := string(bodies[0])
	if !strings.Contains(up, `"effort":"low"`) || strings.Contains(up, `"effort":"max"`) {
		t.Errorf("the search thinking rewrite must win over the route effort pin: %s", up)
	}
	for _, sub := range []string{`"thinkFrom":"max"`, `"thinkTo":"low"`} {
		if !strings.Contains(recent, sub) {
			t.Errorf("the badge must show the last rewrite [max->low], missing %q: %s", sub, recent)
		}
	}
}

// TestRouteEffortSkipsStealthUpgradeE2E locks the convertOff2Low precedence: on the translated port the quiet
// off->low upgrade has already rewritten an explicit thinking-off body into adaptive+low before the pin point
// (n2lMode stealth in ctx), and the polluting "off at max" outcome is exactly what must not happen — the pin
// leaves the upgrade body alone so both ports agree on low.
func TestRouteEffortSkipsStealthUpgradeE2E(t *testing.T) {
	resetStats()
	var mu sync.Mutex
	var bodies [][]byte
	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		bodies = append(bodies, b)
		mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		io.WriteString(w, testSSEAllBlocks())
	}))
	defer mock.Close()
	config.Store(effortBaseConfig(mock.URL))
	defer config.Store(&config.Config{})

	// Stand in for the Responses translation port: serve the request through the real handler with the stealth
	// upgrade marker on the context, exactly as the translate entry point leaves it.
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		handler(w, r.WithContext(context.WithValue(r.Context(), translate.CtxKeyNone2Low, translate.N2LStealth)))
	}))
	defer proxy.Close()
	resp, err := http.Post(proxy.URL+"/v1/messages", "application/json", strings.NewReader(effortStealthLow))
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()

	var recent string
	for i := 0; i < 100; i++ {
		rec := httptest.NewRecorder()
		r := httptest.NewRequest(http.MethodGet, logview.RecentFlightsPath, nil)
		r.RemoteAddr = "127.0.0.1:1"
		logview.RecentFlightsHandler(rec, r)
		recent = rec.Body.String()
		if strings.Contains(recent, `"model":"claude-opus-4-8`) {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(bodies) != 1 {
		t.Fatalf("upstream should receive one request, got %d", len(bodies))
	}
	up := string(bodies[0])
	if !strings.Contains(up, `"effort":"low"`) || strings.Contains(up, `"effort":"max"`) {
		t.Errorf("the stealth off->low upgrade owns its level; the route pin must not override it: %s", up)
	}
	assertNoThinkRewriteBadge(t, recent)
}

// TestRouteEffortShownInModelColumnE2E locks the model-column display: a route with effort shows [level] before
// the target model (appended when the model is not rewritten), whether or not the body-side rewrite applied, and
// the level is the wire form (mid shows as medium). The API column's [from->to] badge stays rewrite-only.
func TestRouteEffortShownInModelColumnE2E(t *testing.T) {
	t.Run("rewritten model carries the level before the target", func(t *testing.T) {
		_, _, _, recent := runEffortE2E(t, func(routeURL, fbURL string) *config.Config {
			c := effortBaseConfig(routeURL)
			c.Routes[0].Model = "deepseek-test"
			c.Routes[0].Effort = "mid"
			return c
		}, effortAdaptiveHigh)
		models := recentModels(t, recent)
		if !hasModel(models, "claude-opus-4-8 -> [medium] deepseek-test") {
			t.Errorf("model column should read %q, got %v", "claude-opus-4-8 -> [medium] deepseek-test", models)
		}
		for _, sub := range []string{`"thinkFrom":"high"`, `"thinkTo":"medium"`} {
			if !strings.Contains(recent, sub) {
				t.Errorf("recent flight should carry %q: %s", sub, recent)
			}
		}
	})
	t.Run("model kept but pin configured still shows the level", func(t *testing.T) {
		_, _, _, recent := runEffortE2E(t, func(routeURL, fbURL string) *config.Config {
			return effortBaseConfig(routeURL) // no model rewrite, effort max
		}, effortBudgetBody) // non-adaptive: the pin rewrites nothing
		models := recentModels(t, recent)
		if !hasModel(models, "claude-opus-4-8 [max]") {
			t.Errorf("model column should append [max] when no model rewrite happened, got %v", models)
		}
		assertNoThinkRewriteBadge(t, recent)
	})
}

// recentModels decodes the recent-flights JSON and collects every "model" string. Parsing compares the value the
// page actually renders; the raw body carries encoding/json's > escape for the display arrow.
func recentModels(t *testing.T, recent string) []string {
	t.Helper()
	var v any
	if err := json.Unmarshal([]byte(recent), &v); err != nil {
		t.Fatalf("recent flights is not JSON: %v", err)
	}
	var models []string
	var walk func(any)
	walk = func(x any) {
		switch t := x.(type) {
		case map[string]any:
			if m, ok := t["model"].(string); ok {
				models = append(models, m)
			}
			for _, v := range t {
				walk(v)
			}
		case []any:
			for _, v := range t {
				walk(v)
			}
		}
	}
	walk(v)
	return models
}

// hasModel reports whether any decoded model string starts with the given prefix (the upstream-model third
// segment may follow the display prefix).
func hasModel(models []string, prefix string) bool {
	for _, m := range models {
		if strings.HasPrefix(m, prefix) {
			return true
		}
	}
	return false
}
