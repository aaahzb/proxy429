package main

// responses_raw_test.go — tests for url_response_api native passthrough (when the Responses port hits a route with this field,
// the original body passes through untranslated to a native Responses upstream): route precheck, URL joining,
// Responses-side stats parsing, keepalive/error event protocol shapes, and end-to-end passthrough.

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestResponsesAPIPath verifies passthrough URL joining: a base filled to …/coding, carrying /v1, or a full …/v1/responses
// each only gets the missing suffix (no double-joining); trailing slashes tolerated.
func TestResponsesAPIPath(t *testing.T) {
	cases := []struct{ base, want string }{
		{"https://api.kimi.com/coding", "/v1/responses"},
		{"https://api.kimi.com/coding/", "/v1/responses"},
		{"https://api.kimi.com/coding/v1", "/responses"},
		{"https://api.kimi.com/coding/v1/responses", ""},
		{"https://api.kimi.com/coding/v1/responses/", ""},
	}
	for _, c := range cases {
		if got := responsesAPIPath(c.base); got != c.want {
			t.Errorf("responsesAPIPath(%q)=%q, want %q", c.base, got, c.want)
		}
	}
}

// TestMatchPassthroughResponsesRoute verifies the passthrough precheck: only a route hit carrying url_response_api passes through;
// reserved names (Fallback/fast_route) are skipped; routes are ordered, first hit wins.
func TestMatchPassthroughResponsesRoute(t *testing.T) {
	c := &Config{Routes: []RouteRule{
		{Pattern: "fast_route", URLResponseAPI: "https://x.example.com"}, // Reserved name, must be skipped
		{Pattern: "k3*", URL: "https://a.example.com"},                   // No url_response_api, no passthrough
		{Pattern: "k3-256k", URL: "https://b.example.com", URLResponseAPI: "https://b.example.com/coding"},
	}}
	if got := matchPassthroughResponsesRoute(c, "fast_route"); got != nil {
		t.Errorf("fast_route 保留名不应命中透传, got %+v", got)
	}
	if got := matchPassthroughResponsesRoute(c, "k3-other"); got != nil {
		t.Errorf("命中路由无 url_response_api 不应透传, got %+v", got)
	}
	got := matchPassthroughResponsesRoute(c, "k3-256k")
	if got == nil || got.URLResponseAPI != "https://b.example.com/coding" {
		t.Errorf("k3-256k 应命中第二条路由的 url_response_api, got %+v", got)
	}
	if got := matchPassthroughResponsesRoute(c, ""); got != nil {
		t.Errorf("空 model 不应命中, got %+v", got)
	}
	// Ordering: a wide wildcard listed first with the passthrough field intercepts a narrower wildcard (same rule as the handler's route loop).
	c2 := &Config{Routes: []RouteRule{
		{Pattern: "k3*", URLResponseAPI: "https://wide.example.com"},
		{Pattern: "k3-256k", URLResponseAPI: "https://narrow.example.com"},
	}}
	if got := matchPassthroughResponsesRoute(c2, "k3-256k"); got == nil || got.URLResponseAPI != "https://wide.example.com" {
		t.Errorf("有序首个命中应截胡, got %+v", got)
	}
}

// TestParseResponsesStreamStats verifies Responses SSE usage parsing:
// usage appears once, only in the terminal event; input_tokens includes the cached total and must split fresh=input-cached; negatives clamp to zero;
// usage-less events like response.created don't count; terminal arrival sets sawDeltaUsage.
func TestParseResponsesStreamStats(t *testing.T) {
	resetStats()
	var last, lastIn, lastCR, lastCC int64
	var saw bool

	// Non-terminal event (no usage): ignored, saw not set.
	parseResponsesStreamStats([]byte(`data: {"type":"response.created","response":{"id":"r1","status":"in_progress"}}`), &last, &lastIn, &lastCR, &lastCC, &saw)
	parseResponsesStreamStats([]byte(`data: {"type":"response.output_text.delta","delta":"hi"}`), &last, &lastIn, &lastCR, &lastCC, &saw)
	if stats.inputTokens != 0 || stats.outputTokens != 0 || saw {
		t.Fatalf("非终局事件不应计数/置位: in=%d out=%d saw=%v", stats.inputTokens, stats.outputTokens, saw)
	}

	// Terminal response.completed: input 110 including cached 100 → fresh=10, cr=100, out=7.
	parseResponsesStreamStats([]byte(`data: {"type":"response.completed","response":{"id":"r1","status":"completed","usage":{"input_tokens":110,"input_tokens_details":{"cached_tokens":100},"output_tokens":7,"total_tokens":117}}}`), &last, &lastIn, &lastCR, &lastCC, &saw)
	if !saw {
		t.Error("终局 usage 到达应置 sawDeltaUsage")
	}
	if stats.inputTokens != 10 || stats.cacheRead != 100 || stats.outputTokens != 7 {
		t.Errorf("拆分错误: in=%d(want 10) cr=%d(want 100) out=%d(want 7)", stats.inputTokens, stats.cacheRead, stats.outputTokens)
	}
	if lastIn != 10 || lastCR != 100 || last != 7 {
		t.Errorf("追踪器: lastIn=%d lastCR=%d last=%d", lastIn, lastCR, last)
	}

	// response.incomplete also counts (max_tokens truncation); the abnormal case of cache exceeding input clamps fresh to zero instead of going negative.
	// The same trackers get a second terminal event, also verifying "later overwrites earlier": this event has fresh=0 (50-80<0 clamped),
	// cr=80 — global in 10→0, cr 100→80.
	parseResponsesStreamStats([]byte(`data: {"type":"response.incomplete","response":{"id":"r2","status":"incomplete","usage":{"input_tokens":50,"input_tokens_details":{"cached_tokens":80},"output_tokens":3}}}`), &last, &lastIn, &lastCR, &lastCC, &saw)
	if stats.inputTokens != 0 {
		t.Errorf("负 fresh 应归零: in=%d want 0（fresh 0 覆盖 10，全局加 -10）", stats.inputTokens)
	}
	if stats.cacheRead != 80 {
		t.Errorf("cr 后值覆盖: %d want 80（全局加 80-100=-20）", stats.cacheRead)
	}
}

// TestParseResponsesToolName verifies Responses SSE tool-name extraction:
// output_item.added of function_call/custom_tool_call yields the name; web_search_call is fixed "web_search".
func TestParseResponsesToolName(t *testing.T) {
	cases := []struct{ line, want string }{
		{`data: {"type":"response.output_item.added","item":{"id":"fc1","type":"function_call","name":"exec_command","arguments":""}}`, "exec_command"},
		{`data: {"type":"response.output_item.added","item":{"id":"c1","type":"custom_tool_call","name":"apply_patch"}}`, "apply_patch"},
		{`data: {"type":"response.output_item.added","item":{"id":"ws1","type":"web_search_call","status":"in_progress"}}`, "web_search"},
		{`data: {"type":"response.output_item.added","item":{"id":"m1","type":"message","content":[]}}`, ""},
		{`data: {"type":"response.output_item.done","item":{"id":"fc1","type":"function_call","name":"exec_command"}}`, ""}, // done doesn't count
		{`data: {"type":"response.output_text.delta","delta":"x"}`, ""},
		{`event: response.completed`, ""},
	}
	for _, c := range cases {
		if got := parseResponsesToolName([]byte(c.line)); got != c.want {
			t.Errorf("parseResponsesToolName(%q)=%q, want %q", c.line, got, c.want)
		}
	}
}

// TestParseNonStreamUsageResponses verifies Responses non-streaming usage splitting (the passthrough stream:false fallback):
// the presence of input_tokens_details marks the Responses shape, input splits fresh+cached;
// and it can't be confused with the Anthropic shape (which has no input_tokens_details).
func TestParseNonStreamUsageResponses(t *testing.T) {
	in, cr, cc, out, ok := parseNonStreamUsage([]byte(`{"object":"response","usage":{"input_tokens":110,"input_tokens_details":{"cached_tokens":100},"output_tokens":7}}`))
	if !ok || in != 10 || cr != 100 || cc != 0 || out != 7 {
		t.Errorf("Responses 拆分: in=%d cr=%d cc=%d out=%d ok=%v, want 10/100/0/7/true", in, cr, cc, out, ok)
	}
	// The Anthropic shape still goes through the original branch (no input_tokens_details).
	in, cr, cc, out, ok = parseNonStreamUsage([]byte(`{"usage":{"input_tokens":110,"cache_read_input_tokens":100,"cache_creation_input_tokens":2,"output_tokens":7}}`))
	if !ok || in != 110 || cr != 100 || cc != 2 || out != 7 {
		t.Errorf("Anthropic 分支: in=%d cr=%d cc=%d out=%d ok=%v, want 110/100/2/7/true", in, cr, cc, out, ok)
	}
}

// TestWriteSSEPingRawShape verifies the keepalive ping's protocol split: a passthrough stream writes an SSE comment line (clients ignore it),
// translated/Anthropic streams keep the Anthropic ping event.
func TestWriteSSEPingRawShape(t *testing.T) {
	rec := httptest.NewRecorder()
	writeSSEPing(rec, nil, &flight{translated: translatedResponsesRaw})
	if rec.Body.String() != ": ping\n\n" {
		t.Errorf("透传流 ping=%q, want SSE 注释行", rec.Body.String())
	}
	rec2 := httptest.NewRecorder()
	writeSSEPing(rec2, nil, &flight{translated: translatedResponses})
	if !strings.Contains(rec2.Body.String(), `"type":"ping"`) {
		t.Errorf("翻译流 ping=%q, want Anthropic ping 事件", rec2.Body.String())
	}
}

// TestWriteSSEErrorRawShape verifies the SSE error protocol split at retry exhaustion: a passthrough stream gets a response.failed event.
func TestWriteSSEErrorRawShape(t *testing.T) {
	rec := httptest.NewRecorder()
	writeSSEError(rec, nil, "upstream status 429", &flight{translated: translatedResponsesRaw})
	s := rec.Body.String()
	if !strings.HasPrefix(s, "event: response.failed\n") || !strings.Contains(s, `"type":"response.failed"`) || !strings.Contains(s, "upstream status 429") {
		t.Errorf("透传流 error=%q, want response.failed 事件含消息", s)
	}
	// The data part must be legal JSON (containing response.error.message).
	data := strings.TrimSpace(strings.TrimPrefix(s[strings.IndexByte(s, '\n')+1:], "data:"))
	var ev map[string]interface{}
	if err := json.Unmarshal([]byte(data), &ev); err != nil {
		t.Errorf("response.failed data 非合法 JSON: %v (%s)", err, data)
	}
	rec2 := httptest.NewRecorder()
	writeSSEError(rec2, nil, "x", &flight{})
	if !strings.HasPrefix(rec2.Body.String(), "event: error\n") {
		t.Errorf("Anthropic 流 error=%q, want event: error", rec2.Body.String())
	}
}

// responsesRawSSE is the Responses SSE stream the fake upstream returns: function_call tool call + text + terminal usage
// (input 110 including cached 100, output 7). The model in response.completed is the upstream's real model name,
// for the pipeline to write back as the client's original name.
const responsesRawSSE = `event: response.created
data: {"type":"response.created","response":{"id":"resp_1","object":"response","status":"in_progress","model":"k3-256k"}}

event: response.output_item.added
data: {"type":"response.output_item.added","output_index":0,"item":{"id":"fc_1","type":"function_call","name":"exec_command","arguments":"","call_id":"c1"}}

event: response.function_call_arguments.delta
data: {"type":"response.function_call_arguments.delta","item_id":"fc_1","delta":"{\"cmd\":\"ls\"}"}

event: response.output_item.done
data: {"type":"response.output_item.done","output_index":0,"item":{"id":"fc_1","type":"function_call","name":"exec_command","arguments":"{\"cmd\":\"ls\"}","call_id":"c1"}}

event: response.output_text.delta
data: {"type":"response.output_text.delta","item_id":"m_1","delta":"done"}

event: response.completed
data: {"type":"response.completed","response":{"id":"resp_1","object":"response","status":"completed","model":"k3-256k","output":[],"usage":{"input_tokens":110,"input_tokens_details":{"cached_tokens":100},"output_tokens":7,"total_tokens":117}}}

`

// TestResponsesRawPassthrough end-to-end: a Responses-port request hits a route with url_response_api →
// the original body passes through (untranslated) to the upstream named by that field, with model rewrite and key override as usual; the client receives verbatim
// Responses SSE (model written back to the client's original name); stats follow the Responses semantics (cache split), tools are counted,
// and the finished archive carries the "responses-raw" flag.
func TestResponsesRawPassthrough(t *testing.T) {
	resetStats()
	var gotPath, gotAuth string
	var gotBody []byte
	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		gotBody, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		io.WriteString(w, responsesRawSSE)
	}))
	defer mock.Close()

	cfg.Store(&Config{
		Upstream:       "http://default-upstream.invalid", // Must not be used once the passthrough route hits
		MaxRetries:     0,
		TotalBudgetSec: 10,
		Routes: []RouteRule{
			{Pattern: "k3*", URL: "http://anthropic-url.invalid", API: "route-key", Model: "k3-256k", URLResponseAPI: mock.URL + "/coding"},
		},
	})
	defer cfg.Store(&Config{})

	proxy := httptest.NewServer(http.HandlerFunc(responsesHandler))
	defer proxy.Close()

	reqBody := `{"model":"k3-coding","instructions":"Be helpful.","input":"hi","stream":true}`
	resp, err := http.Post(proxy.URL+"/v1/responses", "application/json", strings.NewReader(reqBody))
	if err != nil {
		t.Fatalf("请求失败: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("状态码 = %d（应 200），响应: %s", resp.StatusCode, body)
	}

	// What the upstream receives should be the Responses original (only model rewritten by routing): no translation traces (no messages/system fields),
	// stream keeps the client's value (unlike the translation port, which forces true and converts to Anthropic).
	if gotPath != "/coding/v1/responses" {
		t.Errorf("上游路径=%q, want /coding/v1/responses（base 补差额拼接）", gotPath)
	}
	if gotAuth != "Bearer route-key" {
		t.Errorf("Authorization=%q, want Bearer route-key（路由 key 覆盖）", gotAuth)
	}
	var upstreamBody map[string]interface{}
	if err := json.Unmarshal(gotBody, &upstreamBody); err != nil {
		t.Fatalf("上游收到非 JSON: %v", err)
	}
	if upstreamBody["model"] != "k3-256k" {
		t.Errorf("上游 model=%v, want 路由改写 k3-256k", upstreamBody["model"])
	}
	if _, has := upstreamBody["messages"]; has {
		t.Errorf("透传不应翻译成 Anthropic（出现 messages 字段）: %s", gotBody)
	}
	if upstreamBody["instructions"] != "Be helpful." || upstreamBody["input"] != "hi" {
		t.Errorf("Responses 原文字段应原样透传: %s", gotBody)
	}

	// The client receives verbatim Responses SSE; the model in response.completed is written back to the client's original name.
	s := string(body)
	if !strings.Contains(s, "response.completed") || !strings.Contains(s, "function_call_arguments.delta") {
		t.Errorf("客户端应收到 Responses SSE 原文: %s", s)
	}
	if !strings.Contains(s, `"model":"k3-coding"`) {
		t.Errorf("响应 model 应回写客户端原名 k3-coding: %s", s)
	}

	// Stats semantics: input 110 splits into fresh 10 + cached-hit 100, output 7.
	stats.mu.Lock()
	in, cr, out := stats.inputTokens, stats.cacheRead, stats.outputTokens
	stats.mu.Unlock()
	if in != 10 || cr != 100 || out != 7 {
		t.Errorf("聚合统计 in=%d cr=%d out=%d, want 10/100/7", in, cr, out)
	}

	// Finished archive: responses-raw flag, token split, tool tags, delivered (recorded as 200, not 499).
	finishedMu.Lock()
	if len(finished) == 0 {
		finishedMu.Unlock()
		t.Fatal("完成流列表为空（handler defer 应已归档本流）")
	}
	ff := finished[len(finished)-1]
	finishedMu.Unlock()
	if ff.translated != translatedResponsesRaw {
		t.Errorf("translated=%q, want %q（网页 API 列显示 [Response]）", ff.translated, translatedResponsesRaw)
	}
	if ff.inTokens != 10 || ff.cacheRead != 100 || ff.outTokens != 7 {
		t.Errorf("完成流 token in=%d cr=%d out=%d, want 10/100/7", ff.inTokens, ff.cacheRead, ff.outTokens)
	}
	if ff.tools != "[exec_command*1]" {
		t.Errorf("工具标签=%q, want [exec_command*1]（单次调用带参显 *1）", ff.tools)
	}
	if ff.status != http.StatusOK {
		t.Errorf("状态=%d, want 200（response.completed 已标记 delivered，不应误记 499）", ff.status)
	}
}

// TestResponsesRawPassthroughNonStream passthrough + client stream:false: the upstream returns a non-streaming response JSON,
// stats go through parseNonStreamUsage's Responses branch.
func TestResponsesRawPassthroughNonStream(t *testing.T) {
	resetStats()
	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"id":"resp_9","object":"response","status":"completed","model":"k3-256k","output":[{"type":"message","content":[{"type":"output_text","text":"hi"}]}],"usage":{"input_tokens":55,"input_tokens_details":{"cached_tokens":50},"output_tokens":2}}`)
	}))
	defer mock.Close()

	cfg.Store(&Config{
		MaxRetries:     0,
		TotalBudgetSec: 10,
		Routes: []RouteRule{
			{Pattern: "k3*", URL: "http://anthropic-url.invalid", Model: "k3-256k", URLResponseAPI: mock.URL},
		},
	})
	defer cfg.Store(&Config{})

	proxy := httptest.NewServer(http.HandlerFunc(responsesHandler))
	defer proxy.Close()

	resp, err := http.Post(proxy.URL+"/v1/responses", "application/json",
		strings.NewReader(`{"model":"k3-coding","input":"hi","stream":false}`))
	if err != nil {
		t.Fatalf("请求失败: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("状态码 = %d，响应: %s", resp.StatusCode, body)
	}
	if !strings.Contains(string(body), `"object":"response"`) {
		t.Errorf("客户端应拿到原样 Responses JSON: %s", body)
	}
	stats.mu.Lock()
	in, cr, out := stats.inputTokens, stats.cacheRead, stats.outputTokens
	stats.mu.Unlock()
	if in != 5 || cr != 50 || out != 2 {
		t.Errorf("非流式聚合 in=%d cr=%d out=%d, want 5/50/2", in, cr, out)
	}
}

// TestResponsesRawRouteLost config hot-reload race: at precheck the route had url_response_api,
// but by the handler's route loop the field is gone — a clear 502, no silent misrouting.
func TestResponsesRawRouteLost(t *testing.T) {
	resetStats()
	// Precheck and the route loop read the same cfg, so no config swap can slip in between; equivalent verification path:
	// build an internal request carrying the responses-raw flag straight into the handler; the route loop sees a route without the field → 502.
	cfg.Store(&Config{
		MaxRetries:     0,
		TotalBudgetSec: 10,
		Routes:         []RouteRule{{Pattern: "k3*", URL: "http://a.invalid"}}, // No url_response_api
	})
	defer cfg.Store(&Config{})

	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r2 := r.WithContext(context.WithValue(r.Context(), ctxKeyTranslated, translatedResponsesRaw))
		handler(w, r2)
	}))
	defer proxy.Close()

	resp, err := http.Post(proxy.URL+"/v1/responses", "application/json",
		strings.NewReader(`{"model":"k3-coding","input":"hi"}`))
	if err != nil {
		t.Fatalf("请求失败: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadGateway {
		t.Errorf("状态码=%d, want 502（路由丢失 url_response_api 应明说）: %s", resp.StatusCode, body)
	}
}
