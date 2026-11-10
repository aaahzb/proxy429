package main

import (
	"bufio"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

const searchGuardFabricatedAnswer = "FABRICATED_ANSWER_MUST_NOT_REACH_CLAUDE_CODE"

// searchGuardUpstreamSSE is a complete, billable upstream response: empty search followed by a forbidden hard answer.
func searchGuardUpstreamSSE(resultContent, answer string) string {
	return "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_upstream\",\"type\":\"message\",\"role\":\"assistant\",\"model\":\"kimi-internal\",\"content\":[],\"usage\":{\"input_tokens\":100,\"cache_read_input_tokens\":20,\"cache_creation_input_tokens\":5,\"output_tokens\":1}}}\n\n" +
		"event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"server_tool_use\",\"id\":\"srvtoolu_test\",\"name\":\"web_search\",\"input\":{}}}\n\n" +
		"event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\n" +
		searchGuardResultSSE(resultContent) +
		"event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":1}\n\n" +
		"event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":2,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n" +
		"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":2,\"delta\":{\"type\":\"text_delta\",\"text\":" + strconvQuote(answer) + "}}\n\n" +
		"event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":2}\n\n" +
		"event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":321}}\n\n" +
		"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"
}

// strconvQuote keeps the SSE fixture readable while escaping the answer as one JSON string.
func strconvQuote(s string) string {
	return `"` + strings.ReplaceAll(strings.ReplaceAll(s, `\`, `\\`), `"`, `\"`) + `"`
}

type searchGuardE2EResult struct {
	client       string
	status       int
	upstreamBody string
	recent       string
	flight       finishedFlight
}

// runSearchGuardE2E serves a caller-supplied upstream response through the real native handler and archives one flight.
func runSearchGuardE2E(t *testing.T, rr RouteRule, sf *SearchRoute, routeServes bool, clientBody string, status int, contentType, responseBody string) searchGuardE2EResult {
	t.Helper()
	resetStats()
	finishedMu.Lock()
	oldFinished := finished
	finished = nil
	finishedMu.Unlock()
	defer func() {
		finishedMu.Lock()
		finished = oldFinished
		finishedMu.Unlock()
	}()
	oldCfg := cfg.Load()
	defer cfg.Store(oldCfg)

	var mu sync.Mutex
	var upstreamBody []byte
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		upstreamBody = append([]byte(nil), body...)
		mu.Unlock()
		w.Header().Set("Content-Type", contentType)
		w.WriteHeader(status)
		_, _ = io.WriteString(w, responseBody)
	}))
	defer upstream.Close()

	routeCalled := false
	routeUpstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		routeCalled = true
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer routeUpstream.Close()
	if rr.Pattern == "" {
		rr.Pattern = "claude-opus*"
	}
	if routeServes {
		rr.URL = upstream.URL
	} else {
		rr.URL = routeUpstream.URL
	}
	if sf != nil {
		sf.URL = upstream.URL
	}
	cfg.Store(&Config{
		Upstream:           "http://no-default.example.com",
		MaxRetries:         0,
		TotalBudgetSec:     10,
		RecentSampleWindow: 5,
		Routes:             []RouteRule{rr},
		SearchFallback:     sf,
	})
	stats.resetSampleCap(5)

	proxy := httptest.NewServer(http.HandlerFunc(handler))
	defer proxy.Close()
	resp, err := http.Post(proxy.URL+"/v1/messages", "application/json", strings.NewReader(clientBody))
	if err != nil {
		t.Fatalf("proxy request failed: %v", err)
	}
	client, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if routeCalled {
		t.Error("the no_search route upstream must not serve a fallback request")
	}

	result := searchGuardE2EResult{client: string(client), status: resp.StatusCode}
	mu.Lock()
	result.upstreamBody = string(upstreamBody)
	mu.Unlock()
	for i := 0; i < 100; i++ {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, recentFlightsPath, nil)
		req.RemoteAddr = "127.0.0.1:1"
		recentFlightsHandler(rec, req)
		result.recent = rec.Body.String()
		finishedMu.Lock()
		if len(finished) == 1 {
			result.flight = finished[0]
			finishedMu.Unlock()
			break
		}
		finishedMu.Unlock()
		time.Sleep(20 * time.Millisecond)
	}
	if result.flight.id == 0 {
		t.Fatalf("flight was not archived; recent response: %s", result.recent)
	}
	return result
}

func TestSearchGuardReplacesEmptyResponse(t *testing.T) {
	on := true
	upstream := searchGuardUpstreamSSE(`[]`, searchGuardFabricatedAnswer)
	got := runSearchGuardE2E(t,
		RouteRule{Pattern: "claude-opus*", API: "sk-route", Model: "kimi-for-coding", ClaudeSearchGuard: &on},
		nil, true, searchSubagentReq, http.StatusOK, "text/event-stream", upstream)

	if !strings.Contains(got.client, searchGuardFixedText) {
		t.Errorf("client did not receive the fixed no-results text:\n%s", got.client)
	}
	for _, forbidden := range []string{searchGuardFabricatedAnswer, "web_search_tool_result", "server_tool_use", "thinking_delta"} {
		if strings.Contains(got.client, forbidden) {
			t.Errorf("discarded upstream content %q leaked downstream:\n%s", forbidden, got.client)
		}
	}
	for _, required := range []string{`"model":"claude-opus-4-8"`, `"output_tokens":321`, "event: message_delta", "event: message_stop"} {
		if !strings.Contains(got.client, required) {
			t.Errorf("synthetic client stream missing %q:\n%s", required, got.client)
		}
	}
	if !strings.Contains(got.upstreamBody, searchGuardInstruction) {
		t.Errorf("armed request did not carry the guard instruction: %s", got.upstreamBody)
	}
	if !strings.Contains(string(got.flight.content), searchGuardFabricatedAnswer) {
		t.Error("upstream-side flight recording must preserve the discarded hard answer")
	}
	if string(got.flight.contentDown) != got.client {
		t.Errorf("downstream recording must equal the actual synthetic bytes\nrecorded: %s\nactual:   %s", got.flight.contentDown, got.client)
	}
	if got.flight.bytes != int64(len(got.client)) {
		t.Errorf("flight bytes = %d, want actual downstream size %d", got.flight.bytes, len(got.client))
	}
	if got.flight.inTokens != 100 || got.flight.cacheRead != 20 || got.flight.cacheCreation != 5 || got.flight.outTokens != 321 {
		t.Errorf("flight usage = in %d cr %d cc %d out %d, want 100/20/5/321", got.flight.inTokens, got.flight.cacheRead, got.flight.cacheCreation, got.flight.outTokens)
	}
	stats.mu.Lock()
	in, cr, cc, out := stats.inputTokens, stats.cacheRead, stats.cacheCreation, stats.outputTokens
	stats.mu.Unlock()
	if in != 100 || cr != 20 || cc != 5 || out != 321 {
		t.Errorf("global usage = in %d cr %d cc %d out %d, want exactly once 100/20/5/321", in, cr, cc, out)
	}
	if !strings.Contains(got.recent, `"searchGuard":true`) {
		t.Errorf("recentFlights should expose the triggered guard for the status badge: %s", got.recent)
	}
}

func TestSearchGuardConsoleBadge(t *testing.T) {
	if !strings.Contains(logViewerHTML, `[空搜]`) || !strings.Contains(logViewerHTML, `f.searchGuard`) {
		t.Error("Chinese finished-status cell should contain the red [空搜] badge driven by f.searchGuard")
	}
	en := renderLogViewerEN(logViewerHTML)
	if !strings.Contains(en, `[empty-search]`) {
		t.Error("English console should translate the badge to [empty-search]")
	}
	if strings.Contains(en, `[空搜]`) {
		t.Error("Chinese empty-search badge leaked into the English console")
	}
}

func TestSearchGuardNonEmptyMatchesUnguarded(t *testing.T) {
	on := true
	result := `[{"type":"web_search_tool_result_content","url":"https://example.com","title":"real result"}]`
	upstream := searchGuardUpstreamSSE(result, "REAL_SEARCH_ANSWER")
	route := RouteRule{Pattern: "claude-opus*", API: "sk-route", Model: "kimi-for-coding", ClaudeSearchGuard: &on}
	guarded := runSearchGuardE2E(t, route, nil, true, searchSubagentReq, http.StatusOK, "text/event-stream", upstream)
	route.ClaudeSearchGuard = nil
	plain := runSearchGuardE2E(t, route, nil, true, searchSubagentReq, http.StatusOK, "text/event-stream", upstream)
	if guarded.client != plain.client {
		t.Errorf("non-empty guarded stream must be byte-identical to unguarded\nguarded: %s\nplain:   %s", guarded.client, plain.client)
	}
	if !strings.Contains(guarded.client, "REAL_SEARCH_ANSWER") || strings.Contains(guarded.client, searchGuardFixedText) {
		t.Errorf("real search result was incorrectly replaced: %s", guarded.client)
	}
}

func TestSearchGuardDisabledPassesEmptyResponse(t *testing.T) {
	upstream := searchGuardUpstreamSSE(`[]`, searchGuardFabricatedAnswer)
	got := runSearchGuardE2E(t,
		RouteRule{Pattern: "claude-opus*", API: "sk-route", Model: "kimi-for-coding"},
		nil, true, searchSubagentReq, http.StatusOK, "text/event-stream", upstream)
	if !strings.Contains(got.client, searchGuardFabricatedAnswer) || strings.Contains(got.client, searchGuardFixedText) {
		t.Errorf("guard-off response must retain existing behavior: %s", got.client)
	}
	if strings.Contains(got.upstreamBody, searchGuardInstruction) {
		t.Error("guard-off request must not receive the guard instruction")
	}
}

func TestSearchGuardFallbackResolutionEndToEnd(t *testing.T) {
	on, off := true, false
	tests := []struct {
		name        string
		route       *bool
		fallback    *bool
		wantReplace bool
	}{
		{name: "inherit fallback on", fallback: &on, wantReplace: true},
		{name: "explicit route off beats fallback on", route: &off, fallback: &on, wantReplace: false},
		{name: "explicit route on beats fallback off", route: &on, fallback: &off, wantReplace: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := runSearchGuardE2E(t,
				RouteRule{Pattern: "claude-opus*", API: "sk-route", Model: "ark-opus", NoSearch: true, ClaudeSearchGuard: tc.route},
				&SearchRoute{API: "sk-sf", Model: "kimi-for-coding", ClaudeSearchGuard: tc.fallback},
				false, searchSubagentReq, http.StatusOK, "text/event-stream", searchGuardUpstreamSSE(`[]`, searchGuardFabricatedAnswer))
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

func TestSearchGuardNoMarkerAndNonSSEPassThrough(t *testing.T) {
	on := true
	unmarked := `{"model":"claude-opus-4-8","stream":true,"tools":[{"type":"web_search_20250305","name":"web_search"}],"messages":[{"role":"user","content":"ordinary request"}]}`
	upstream := searchGuardUpstreamSSE(`[]`, searchGuardFabricatedAnswer)
	got := runSearchGuardE2E(t,
		RouteRule{Pattern: "claude-opus*", API: "sk-route", Model: "kimi-for-coding", ClaudeSearchGuard: &on},
		nil, true, unmarked, http.StatusOK, "text/event-stream", upstream)
	if !strings.Contains(got.client, searchGuardFabricatedAnswer) || strings.Contains(got.client, searchGuardFixedText) {
		t.Errorf("unmarked response must not be replaced: %s", got.client)
	}

	jsonBody := `{"type":"message","content":[{"type":"text","text":"` + searchGuardFabricatedAnswer + `"},{"type":"web_search_tool_result","content":[]}]}`
	got = runSearchGuardE2E(t,
		RouteRule{Pattern: "claude-opus*", API: "sk-route", Model: "kimi-for-coding", ClaudeSearchGuard: &on},
		nil, true, searchSubagentReq, http.StatusOK, "application/json", jsonBody)
	if got.client != jsonBody {
		t.Errorf("non-SSE JSON changed under the guard:\n got: %s\nwant: %s", got.client, jsonBody)
	}
}

func TestForwardSearchGuardSkipsNon200AndResponsesOrigin(t *testing.T) {
	upstream := searchGuardUpstreamSSE(`[]`, searchGuardFabricatedAnswer)
	for _, tc := range []struct {
		name       string
		status     int
		translated string
	}{
		{name: "non 200", status: http.StatusTooManyRequests},
		{name: "Responses translation", status: http.StatusOK, translated: translatedResponses},
		{name: "Responses native", status: http.StatusOK, translated: translatedResponsesRaw},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resetStats()
			f := &flight{id: 1, start: time.Now(), origModel: "claude-opus-4-8", searchGuardArmed: true, translated: tc.translated}
			resp := &http.Response{StatusCode: tc.status, Header: http.Header{"Content-Type": []string{"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(upstream))}
			rec := httptest.NewRecorder()
			forward(rec, resp, nil, bufio.NewReader(resp.Body), f, false)
			if !strings.Contains(rec.Body.String(), searchGuardFabricatedAnswer) || strings.Contains(rec.Body.String(), searchGuardFixedText) {
				t.Errorf("out-of-scope response was replaced: %s", rec.Body.String())
			}
		})
	}
}

func TestForwardSearchGuardAfterKeepalive(t *testing.T) {
	resetStats()
	f := &flight{id: 9, start: time.Now(), origModel: "claude-opus-4-8", searchGuardArmed: true}
	resp := &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(searchGuardUpstreamSSE(`[]`, searchGuardFabricatedAnswer)))}
	rec := httptest.NewRecorder()
	rec.Header().Set("Content-Type", "text/event-stream")
	rec.WriteHeader(http.StatusOK)
	_, _ = io.WriteString(rec, "event: ping\ndata: {\"type\":\"ping\"}\n\n")
	forward(rec, resp, nil, bufio.NewReader(resp.Body), f, true)
	if !strings.Contains(rec.Body.String(), "event: ping") || !strings.Contains(rec.Body.String(), searchGuardFixedText) {
		t.Errorf("synthetic stream should legally follow keepalive pings: %s", rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), searchGuardFabricatedAnswer) {
		t.Errorf("hard answer leaked after keepalive: %s", rec.Body.String())
	}
}
