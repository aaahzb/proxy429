package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// TestLazyApplyDirectSearchE2E locks the native handler wiring for a search-capable route: omitted
// claudeSearch* keys are filled from lazyApply and drive the same injection/thinking rewrite as local fields.
func TestLazyApplyDirectSearchE2E(t *testing.T) {
	lazy := &LazyApplyConfig{ClaudeSearchThinking: "max", ClaudeSearchSummary: "forceHigh"}
	_, bodies, _, recent := runSearchE2EFull(t,
		RouteRule{Pattern: "claude-opus*", API: "sk-route", Model: "kimi-for-coding"},
		nil, true, searchSubagentReq, lazy)
	if len(bodies) != 1 {
		t.Fatalf("upstream should receive one request, got %d", len(bodies))
	}
	up := string(bodies[0])
	for _, sub := range []string{"detailed comprehensive", `"thinking":{"type":"adaptive"}`, `"effort":"max"`} {
		if !strings.Contains(up, sub) {
			t.Errorf("lazyApply search fields should reach the upstream (missing %q): %s", sub, up)
		}
	}
	for _, sub := range []string{`"searchMarker":true`, `"searchLevel":"high"`, `"searchThinkTo":"max"`} {
		if !strings.Contains(recent, sub) {
			t.Errorf("recent flight badges should reflect lazyApply (missing %q): %s", sub, recent)
		}
	}
}

// TestLazyApplyFallbackSearchE2E locks the diverted-request path: lazyApply fills both objects as if
// written, so the route's inherited claudeSearch* values win over the fallback's explicit ones under the
// existing route-over-fallback precedence; route noLazyApply exposes the fallback locals again.
func TestLazyApplyFallbackSearchE2E(t *testing.T) {
	lazy := &LazyApplyConfig{ClaudeSearchThinking: "max", ClaudeSearchSummary: "forceHigh"}
	_, bodies, _, _ := runSearchE2EFull(t,
		RouteRule{Pattern: "claude-opus*", API: "sk-route", Model: "ark-opus", NoSearch: true},
		&SearchRoute{API: "sk-sf", Model: "kimi-for-coding", ClaudeSearchThinking: "low", ClaudeSearchSummary: "forceLow"},
		false, bigMaxSearchReq, lazy)
	if len(bodies) != 1 {
		t.Fatalf("fallback should receive one request, got %d", len(bodies))
	}
	up := string(bodies[0])
	if !strings.Contains(up, "detailed comprehensive") || !strings.Contains(up, `"effort":"max"`) {
		t.Errorf("route inherited values should beat fallback locals: %s", up)
	}

	// The same route with noLazyApply lets the fallback's explicit low policy apply.
	rr := RouteRule{Pattern: "claude-opus*", API: "sk-route", Model: "ark-opus", NoSearch: true, NoLazyApply: true}
	_, bodies, _, _ = runSearchE2EFull(t, rr,
		&SearchRoute{API: "sk-sf", Model: "kimi-for-coding", ClaudeSearchThinking: "low", ClaudeSearchSummary: "forceLow"},
		false, bigMaxSearchReq, lazy)
	if len(bodies) != 1 {
		t.Fatalf("fallback should receive one request, got %d", len(bodies))
	}
	up = string(bodies[0])
	if !strings.Contains(up, "short plaintext summary") || !strings.Contains(up, `"effort":"low"`) {
		t.Errorf("route noLazyApply should expose fallback locals: %s", up)
	}
}

// TestLazyApplyNoSearchFallbackE2E locks the capability-fallback gate: lazy no_search diverts a search
// request to search_fallback, while an explicit local false on the route prevents the diversion.
func TestLazyApplyNoSearchFallbackE2E(t *testing.T) {
	lazy := &LazyApplyConfig{NoSearch: true}
	_, bodies, routeCalled, _ := runSearchE2EFull(t,
		RouteRule{Pattern: "claude-opus*", API: "sk-route", Model: "ark-opus"},
		&SearchRoute{API: "sk-sf", Model: "kimi-for-coding"},
		false, searchSubagentReq, lazy)
	if routeCalled {
		t.Error("lazy no_search should divert away from the route upstream")
	}
	if len(bodies) != 1 {
		t.Fatalf("search_fallback should serve the request, got %d bodies", len(bodies))
	}

	// Explicit local false must stop inheritance; presence metadata is set directly because the
	// E2E helper takes a Go literal, where false would otherwise mean "unset".
	rr := RouteRule{Pattern: "claude-opus*", API: "sk-route", Model: "ark-opus",
		presence: configPresence{"no_search": {}}}
	_, bodies, routeCalled, _ = runSearchE2EFull(t, rr,
		&SearchRoute{API: "sk-sf", Model: "kimi-for-coding"},
		false, searchSubagentReq, lazy)
	if !routeCalled {
		t.Error("explicit no_search:false should keep the request on the route")
	}
	if len(bodies) != 0 {
		t.Errorf("search_fallback should not be called, got %d bodies", len(bodies))
	}
}

// TestLazyApplyTextOnlyMultimodalE2E locks the capability gate: lazy text_only diverts image requests to
// multimodal_fallback, while an explicit local false or noLazyApply keeps them on the route.
func TestLazyApplyTextOnlyMultimodalE2E(t *testing.T) {
	lazy := &LazyApplyConfig{TextOnly: true}
	imageBody := `{"model":"claude-opus-4-8","messages":[{"role":"user","content":[{"type":"text","text":"look"},{"type":"image","source":{"type":"base64","media_type":"image/png","data":"iVBOR"}}]}]}`
	cases := []struct {
		name         string
		route        RouteRule
		wantRoute    bool
		wantFallback bool
	}{
		{name: "lazy text_only diverts", route: RouteRule{}, wantFallback: true},
		{name: "local false stops inheritance", route: RouteRule{presence: configPresence{"text_only": {}}}, wantRoute: true},
		{name: "noLazyApply stops inheritance", route: RouteRule{NoLazyApply: true}, wantRoute: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resetStats()
			var mu sync.Mutex
			routeCalled, fallbackCalled := false, false
			routeMock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				routeCalled = true
				mu.Unlock()
				w.Header().Set("Content-Type", "text/event-stream")
				w.WriteHeader(200)
				fmt.Fprint(w, "data: [DONE]\n\n")
			}))
			defer routeMock.Close()
			fbMock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				fallbackCalled = true
				mu.Unlock()
				w.Header().Set("Content-Type", "text/event-stream")
				w.WriteHeader(200)
				fmt.Fprint(w, "data: [DONE]\n\n")
			}))
			defer fbMock.Close()

			rr := tc.route
			rr.Pattern = "claude-opus*"
			rr.URL = routeMock.URL
			rr.API = "sk-route"
			rr.Model = "deepseek-V4-pro"
			cfg.Store(&Config{
				Upstream:           "http://no-default.example.com",
				MaxRetries:         0,
				TotalBudgetSec:     10,
				RecentSampleWindow: 5,
				Routes:             []RouteRule{rr},
				MultimodalFallback: &MultimodalRoute{URL: fbMock.URL, API: "sk-mm", Model: "glm-4.5v"},
				LazyApply:          lazy,
			})
			stats.resetSampleCap(5)
			proxy := httptest.NewServer(http.HandlerFunc(handler))
			defer proxy.Close()

			resp, err := http.Post(proxy.URL+"/v1/messages", "application/json", strings.NewReader(imageBody))
			if err != nil {
				t.Fatalf("request failed: %v", err)
			}
			io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
			mu.Lock()
			defer mu.Unlock()
			if routeCalled != tc.wantRoute {
				t.Errorf("routeCalled = %v, want %v", routeCalled, tc.wantRoute)
			}
			if fallbackCalled != tc.wantFallback {
				t.Errorf("fallbackCalled = %v, want %v", fallbackCalled, tc.wantFallback)
			}
		})
	}
}

// TestLazyApplyGuardE2E locks the empty-search guard layer end to end: lazyApply arms the guard for a
// native route, noLazyApply or an explicit local false disarms it, and the fallback lane can inherit it
// through the fallback object's own lazyApply layer.
func TestLazyApplyGuardE2E(t *testing.T) {
	on, off := true, false
	upstream := searchGuardUpstreamSSE(`[]`, searchGuardFabricatedAnswer)
	cases := []struct {
		name        string
		route       RouteRule
		fallback    *SearchRoute
		routeServes bool
		lazy        *LazyApplyConfig
		wantReplace bool
	}{
		{
			name:        "lazy arms the route guard",
			route:       RouteRule{Pattern: "claude-opus*", API: "sk-route", Model: "kimi-for-coding"},
			routeServes: true,
			lazy:        &LazyApplyConfig{NoBullshitOnEmptyClaudeSearch: &on},
			wantReplace: true,
		},
		{
			name:        "route noLazyApply disarms the guard",
			route:       RouteRule{Pattern: "claude-opus*", API: "sk-route", Model: "kimi-for-coding", NoLazyApply: true},
			routeServes: true,
			lazy:        &LazyApplyConfig{NoBullshitOnEmptyClaudeSearch: &on},
		},
		{
			name:        "explicit local false beats lazy",
			route:       RouteRule{Pattern: "claude-opus*", API: "sk-route", Model: "kimi-for-coding", NoBullshitOnEmptyClaudeSearch: &off},
			routeServes: true,
			lazy:        &LazyApplyConfig{NoBullshitOnEmptyClaudeSearch: &on},
		},
		{
			name:     "fallback lazy layer arms the fallback lane",
			route:    RouteRule{Pattern: "claude-opus*", API: "sk-route", Model: "ark-opus", NoSearch: true, NoLazyApply: true},
			fallback: &SearchRoute{API: "sk-sf", Model: "kimi-for-coding"},
			lazy:     &LazyApplyConfig{NoBullshitOnEmptyClaudeSearch: &on},
			// The fallback object does not opt out, so it inherits lazyApply and arms the guard.
			wantReplace: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := runSearchGuardE2E(t, tc.route, tc.fallback, tc.routeServes, searchSubagentReq,
				http.StatusOK, "text/event-stream", upstream, tc.lazy)
			replaced := strings.Contains(got.client, searchGuardFixedText)
			if replaced != tc.wantReplace {
				t.Errorf("replacement = %v, want %v; client: %s", replaced, tc.wantReplace, got.client)
			}
			if strings.Contains(got.client, searchGuardFabricatedAnswer) == tc.wantReplace {
				t.Errorf("hard-answer leakage disagrees with replacement=%v: %s", tc.wantReplace, got.client)
			}
		})
	}
}

// TestLazyApplyClassifierE2E locks the classifier wiring through the real JSON config: per-source lazy
// values drive maybeRewriteClassifier, noLazyApply disables inheritance, and an explicitly written empty
// per-source key blocks the lazy per-source override while the shared lazy value still applies.
func TestLazyApplyClassifierE2E(t *testing.T) {
	const ccSystem = "You are a security monitor. Check this."
	cases := []struct {
		name          string
		config        string
		kind          classifierSource
		wantMode      string
		wantRewritten bool
	}{
		{
			name:          "cc inherits lazy off",
			config:        `{"upstream":"http://u","classifier_route":{"url":"http://c","api":"sk","model":"m"},"lazyApply":{"classifier_thinking_cc":"off"}}`,
			kind:          classifierCC,
			wantMode:      "off",
			wantRewritten: true,
		},
		{
			name:     "noLazyApply disables inheritance",
			config:   `{"upstream":"http://u","classifier_route":{"url":"http://c","api":"sk","model":"m","noLazyApply":true},"lazyApply":{"classifier_thinking_cc":"off"}}`,
			kind:     classifierCC,
			wantMode: "",
		},
		{
			name:          "explicit empty cc blocks lazy cc and falls back to shared",
			config:        `{"upstream":"http://u","classifier_route":{"url":"http://c","api":"sk","model":"m","classifier_thinking_cc":""},"lazyApply":{"classifier_thinking":"off","classifier_thinking_cc":"low"}}`,
			kind:          classifierCC,
			wantMode:      "off",
			wantRewritten: true,
		},
		{
			name:          "lazy per-source beats lazy shared",
			config:        `{"upstream":"http://u","classifier_route":{"url":"http://c","api":"sk","model":"m"},"lazyApply":{"classifier_thinking":"off","classifier_thinking_codex":"low"}}`,
			kind:          classifierCodex,
			wantMode:      "low",
			wantRewritten: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resetStats()
			c, _, err := parseConfig([]byte(tc.config))
			if err != nil {
				t.Fatalf("parse config: %v", err)
			}
			cfg.Store(c)
			defer cfg.Store(&Config{})
			if got := classifierThinkingModeFor(c, tc.kind); got != tc.wantMode {
				t.Fatalf("classifierThinkingModeFor = %q, want %q", got, tc.wantMode)
			}
			body := []byte(`{"model":"x","system":"` + ccSystem + `","thinking":{"type":"enabled"},"max_tokens":1024}`)
			out, rewritten := maybeRewriteClassifier(body)
			if rewritten != tc.wantRewritten {
				t.Fatalf("rewritten = %v, want %v (out: %s)", rewritten, tc.wantRewritten, out)
			}
			if tc.wantRewritten {
				if th := asObj(parseBody(t, out)["thinking"]); objStr(th, "type") != "disabled" {
					t.Errorf("thinking=%v, want disabled", th)
				}
			}
		})
	}
}

// TestLazyApplyResponsesThinkingE2E locks the Responses translation pre-match for routes[].thinking:
// lazyApply supplies the thinking shape when the matched route omits it, and noLazyApply restores the
// client model's built-in auto mapping.
func TestLazyApplyResponsesThinkingE2E(t *testing.T) {
	cases := []struct {
		name   string
		route  RouteRule
		lazy   *LazyApplyConfig
		want   string
		budget int64
	}{
		{name: "auto default stays adaptive", route: RouteRule{}},
		{name: "lazy budget forces enabled", route: RouteRule{}, lazy: &LazyApplyConfig{Thinking: "budget"}, want: "enabled", budget: 16000},
		{name: "route noLazyApply restores auto", route: RouteRule{NoLazyApply: true}, lazy: &LazyApplyConfig{Thinking: "budget"}},
		{name: "local auto beats lazy budget", route: RouteRule{Thinking: "auto"}, lazy: &LazyApplyConfig{Thinking: "budget"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resetStats()
			var got map[string]interface{}
			mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				b, _ := io.ReadAll(r.Body)
				_ = json.Unmarshal(b, &got)
				w.Header().Set("Content-Type", "text/event-stream")
				w.WriteHeader(200)
				io.WriteString(w, testSSEAllBlocks())
			}))
			defer mock.Close()
			rr := tc.route
			rr.Pattern = "claude-opus*"
			rr.URL = mock.URL
			rr.Model = "kimi-for-coding"
			cfg.Store(&Config{
				Upstream:       mock.URL,
				MaxRetries:     0,
				TotalBudgetSec: 10,
				Routes:         []RouteRule{rr},
				LazyApply:      tc.lazy,
			})
			defer cfg.Store(&Config{})
			proxy := httptest.NewServer(http.HandlerFunc(responsesHandler))
			defer proxy.Close()

			resp, err := http.Post(proxy.URL+"/v1/responses", "application/json",
				strings.NewReader(`{"model":"claude-opus-4-8","input":"hi","reasoning":{"effort":"high"}}`))
			if err != nil {
				t.Fatalf("request failed: %v", err)
			}
			io.Copy(io.Discard, resp.Body)
			resp.Body.Close()

			th := asObj(got["thinking"])
			if tc.want == "enabled" {
				if objStr(th, "type") != "enabled" || toInt64(th["budget_tokens"]) != tc.budget {
					t.Errorf("thinking=%v, want enabled/%d", th, tc.budget)
				}
				if got["output_config"] != nil {
					t.Errorf("budget path should not send output_config: %v", got["output_config"])
				}
				return
			}
			if objStr(th, "type") != "adaptive" || objStr(asObj(got["output_config"]), "effort") != "high" {
				t.Errorf("thinking=%v output_config=%v, want adaptive/high", th, got["output_config"])
			}
		})
	}
}

// TestLazyApplyResponsesConvertOff2LowE2E locks the pre-match convertOff2Low wiring for both the
// matched route and the fast_route literal branch.
func TestLazyApplyResponsesConvertOff2LowE2E(t *testing.T) {
	cases := []struct {
		name  string
		route RouteRule
		fast  *FastRoute
		lazy  *LazyApplyConfig
		want  string
	}{
		{name: "client off stays off", route: RouteRule{}},
		{name: "route inherits lazy translate", route: RouteRule{}, lazy: &LazyApplyConfig{ConvertOff2Low: "translate"}, want: "translate"},
		{name: "route noLazyApply keeps off", route: RouteRule{NoLazyApply: true}, lazy: &LazyApplyConfig{ConvertOff2Low: "translate"}},
		{name: "fast_route inherits lazy translate", fast: &FastRoute{Model: "k3-256k"}, lazy: &LazyApplyConfig{ConvertOff2Low: "translate"}, want: "translate"},
		{name: "fast_route noLazyApply keeps off", fast: &FastRoute{Model: "k3-256k", NoLazyApply: true}, lazy: &LazyApplyConfig{ConvertOff2Low: "translate"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resetStats()
			var got map[string]interface{}
			mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				b, _ := io.ReadAll(r.Body)
				_ = json.Unmarshal(b, &got)
				w.Header().Set("Content-Type", "text/event-stream")
				w.WriteHeader(200)
				io.WriteString(w, testSSEAllBlocks())
			}))
			defer mock.Close()
			route := tc.route
			route.Pattern = "claude-opus*"
			route.URL = mock.URL
			route.Model = "kimi-for-coding"
			fast := tc.fast
			if fast != nil {
				fast.URL = mock.URL
			}
			cfg.Store(&Config{
				Upstream:       mock.URL,
				MaxRetries:     0,
				TotalBudgetSec: 10,
				Routes:         []RouteRule{route},
				FastRoute:      fast,
				LazyApply:      tc.lazy,
			})
			defer cfg.Store(&Config{})
			proxy := httptest.NewServer(http.HandlerFunc(responsesHandler))
			defer proxy.Close()

			model := "claude-opus-4-8"
			if fast != nil {
				model = "fast_route"
			}
			resp, err := http.Post(proxy.URL+"/v1/responses", "application/json",
				strings.NewReader(`{"model":"`+model+`","input":"hi","reasoning":{"effort":"none"}}`))
			if err != nil {
				t.Fatalf("request failed: %v", err)
			}
			io.Copy(io.Discard, resp.Body)
			resp.Body.Close()

			th := asObj(got["thinking"])
			gotType := objStr(th, "type")
			if tc.want == "translate" {
				// Adaptive route models upgrade a disabled request to adaptive thinking; budget models (fast_route)
				// get the low floor. Either shape proves convertOff2Low reached the translation layer.
				if gotType != "adaptive" && !(gotType == "enabled" && toInt64(th["budget_tokens"]) == 2048) {
					t.Errorf("thinking=%v, want adaptive or enabled/2048", th)
				}
				return
			}
			if gotType == "adaptive" || gotType == "enabled" {
				t.Errorf("client effort none should stay off without convertOff2Low: %v", th)
			}
		})
	}
}

// TestLazyApplyResponsesFallbackThinkingE2E locks the translation-time prediction of the search
// diversion: when the hit route effectively has no_search and the Responses request carries a
// server-side web_search tool, the translated thinking shape describes search_fallback's target model
// (explicit > lazyApply > auto), not the hit route's. Without the diversion the route's shape still wins.
func TestLazyApplyResponsesFallbackThinkingE2E(t *testing.T) {
	const searchTool = `,"tools":[{"type":"web_search"}]`
	cases := []struct {
		name        string
		route       RouteRule
		sf          *SearchRoute
		lazy        *LazyApplyConfig
		tools       string
		wantEnabled bool // true = budget shape enabled/16000; false = adaptive/high
	}{
		{
			name:  "fallback thinking beats the hit route's",
			route: RouteRule{NoSearch: true, Thinking: "budget"},
			sf:    &SearchRoute{Thinking: "adaptive"},
			tools: searchTool,
		},
		{
			name:  "fallback local beats lazy",
			route: RouteRule{NoSearch: true},
			sf:    &SearchRoute{Thinking: "adaptive"},
			lazy:  &LazyApplyConfig{Thinking: "budget"},
			tools: searchTool,
		},
		{
			name:        "fallback inherits lazy",
			route:       RouteRule{NoSearch: true},
			sf:          &SearchRoute{},
			lazy:        &LazyApplyConfig{Thinking: "budget"},
			tools:       searchTool,
			wantEnabled: true,
		},
		{
			name:        "no diversion keeps the route shape",
			route:       RouteRule{NoSearch: true, Thinking: "budget"},
			sf:          &SearchRoute{Thinking: "adaptive"},
			wantEnabled: true,
		},
		{
			name:        "search-capable route keeps the route shape",
			route:       RouteRule{Thinking: "budget"},
			sf:          &SearchRoute{Thinking: "adaptive"},
			tools:       searchTool,
			wantEnabled: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resetStats()
			var got map[string]interface{}
			// Route and fallback share one mock; the thinking shape proves which config was predicted.
			mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				b, _ := io.ReadAll(r.Body)
				_ = json.Unmarshal(b, &got)
				w.Header().Set("Content-Type", "text/event-stream")
				w.WriteHeader(200)
				io.WriteString(w, testSSEAllBlocks())
			}))
			defer mock.Close()
			rr := tc.route
			rr.Pattern = "claude-opus*"
			rr.URL = mock.URL
			rr.Model = "kimi-for-coding"
			sf := tc.sf
			sf.URL = mock.URL
			sf.Model = "ark-search"
			sf.API = "sk-sf"
			cfg.Store(&Config{
				Upstream:       mock.URL,
				MaxRetries:     0,
				TotalBudgetSec: 10,
				Routes:         []RouteRule{rr},
				SearchFallback: sf,
				LazyApply:      tc.lazy,
			})
			defer cfg.Store(&Config{})
			proxy := httptest.NewServer(http.HandlerFunc(responsesHandler))
			defer proxy.Close()

			resp, err := http.Post(proxy.URL+"/v1/responses", "application/json",
				strings.NewReader(`{"model":"claude-opus-4-8","input":"hi","reasoning":{"effort":"high"}`+tc.tools+`}`))
			if err != nil {
				t.Fatalf("request failed: %v", err)
			}
			io.Copy(io.Discard, resp.Body)
			resp.Body.Close()

			th := asObj(got["thinking"])
			if tc.wantEnabled {
				if objStr(th, "type") != "enabled" || toInt64(th["budget_tokens"]) != 16000 {
					t.Errorf("thinking=%v, want enabled/16000", th)
				}
				return
			}
			if objStr(th, "type") != "adaptive" || objStr(asObj(got["output_config"]), "effort") != "high" {
				t.Errorf("thinking=%v output_config=%v, want adaptive/high", th, got["output_config"])
			}
		})
	}
}
