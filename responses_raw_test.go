package main

// responses_raw_test.go — url_response_api 原生透传（Responses 口命中带该字段的路由时不翻译、
// 原文透传到原生 Responses 上游）的测试：路由预检、URL 拼接、Responses 口径统计解析、
// 保活/错误事件的协议形状，以及端到端透传。

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestResponsesAPIPath 验证透传 URL 拼接：base 填到 …/coding、带 /v1、或完整 …/v1/responses
// 都只补差额不双拼；尾斜杠容错。
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

// TestMatchPassthroughResponsesRoute 验证透传预检：命中带 url_response_api 的路由才透传；
// 保留名（Fallback/fast_route）跳过；routes 有序首个命中生效。
func TestMatchPassthroughResponsesRoute(t *testing.T) {
	c := &Config{Routes: []RouteRule{
		{Pattern: "fast_route", URLResponseAPI: "https://x.example.com"}, // 保留名，必须跳过
		{Pattern: "k3*", URL: "https://a.example.com"},                   // 无 url_response_api，不透传
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
	// 顺序：宽通配在前且带透传字段时截胡窄通配（与 handler 路由循环同规则）。
	c2 := &Config{Routes: []RouteRule{
		{Pattern: "k3*", URLResponseAPI: "https://wide.example.com"},
		{Pattern: "k3-256k", URLResponseAPI: "https://narrow.example.com"},
	}}
	if got := matchPassthroughResponsesRoute(c2, "k3-256k"); got == nil || got.URLResponseAPI != "https://wide.example.com" {
		t.Errorf("有序首个命中应截胡, got %+v", got)
	}
}

// TestParseResponsesStreamStats 验证 Responses SSE 的 usage 解析：
// 只在终局事件出现一次；input_tokens 含缓存总量需拆 fresh=input-cached；负值归零；
// response.created 等无 usage 事件不计数；终局到达置 sawDeltaUsage。
func TestParseResponsesStreamStats(t *testing.T) {
	resetStats()
	var last, lastIn, lastCR, lastCC int64
	var saw bool

	// 非终局事件（无 usage）：忽略，saw 不置位。
	parseResponsesStreamStats([]byte(`data: {"type":"response.created","response":{"id":"r1","status":"in_progress"}}`), &last, &lastIn, &lastCR, &lastCC, &saw)
	parseResponsesStreamStats([]byte(`data: {"type":"response.output_text.delta","delta":"hi"}`), &last, &lastIn, &lastCR, &lastCC, &saw)
	if stats.inputTokens != 0 || stats.outputTokens != 0 || saw {
		t.Fatalf("非终局事件不应计数/置位: in=%d out=%d saw=%v", stats.inputTokens, stats.outputTokens, saw)
	}

	// 终局 response.completed：input 110 含缓存 100 → fresh=10, cr=100, out=7。
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

	// response.incomplete 也认（max_tokens 截断）；缓存超过 input 的异常口径 fresh 归零而不是加成负。
	// 同一组追踪器再喂一个终局事件，同时验证"后值覆盖前值"：本事件 fresh=0（50-80<0 钳位）、
	// cr=80——全局 in 10→0、cr 100→80。
	parseResponsesStreamStats([]byte(`data: {"type":"response.incomplete","response":{"id":"r2","status":"incomplete","usage":{"input_tokens":50,"input_tokens_details":{"cached_tokens":80},"output_tokens":3}}}`), &last, &lastIn, &lastCR, &lastCC, &saw)
	if stats.inputTokens != 0 {
		t.Errorf("负 fresh 应归零: in=%d want 0（fresh 0 覆盖 10，全局加 -10）", stats.inputTokens)
	}
	if stats.cacheRead != 80 {
		t.Errorf("cr 后值覆盖: %d want 80（全局加 80-100=-20）", stats.cacheRead)
	}
}

// TestParseResponsesToolName 验证 Responses SSE 工具名提取：
// output_item.added 的 function_call/custom_tool_call 取 name，web_search_call 固定 "web_search"。
func TestParseResponsesToolName(t *testing.T) {
	cases := []struct{ line, want string }{
		{`data: {"type":"response.output_item.added","item":{"id":"fc1","type":"function_call","name":"exec_command","arguments":""}}`, "exec_command"},
		{`data: {"type":"response.output_item.added","item":{"id":"c1","type":"custom_tool_call","name":"apply_patch"}}`, "apply_patch"},
		{`data: {"type":"response.output_item.added","item":{"id":"ws1","type":"web_search_call","status":"in_progress"}}`, "web_search"},
		{`data: {"type":"response.output_item.added","item":{"id":"m1","type":"message","content":[]}}`, ""},
		{`data: {"type":"response.output_item.done","item":{"id":"fc1","type":"function_call","name":"exec_command"}}`, ""}, // done 不计数
		{`data: {"type":"response.output_text.delta","delta":"x"}`, ""},
		{`event: response.completed`, ""},
	}
	for _, c := range cases {
		if got := parseResponsesToolName([]byte(c.line)); got != c.want {
			t.Errorf("parseResponsesToolName(%q)=%q, want %q", c.line, got, c.want)
		}
	}
}

// TestParseNonStreamUsageResponses 验证 Responses 非流式 usage 拆分（透传 stream:false 兜底）：
// input_tokens_details 存在即 Responses 形状，input 拆 fresh+cached；
// 且不会与 Anthropic 形状混淆（Anthropic 无 input_tokens_details）。
func TestParseNonStreamUsageResponses(t *testing.T) {
	in, cr, cc, out, ok := parseNonStreamUsage([]byte(`{"object":"response","usage":{"input_tokens":110,"input_tokens_details":{"cached_tokens":100},"output_tokens":7}}`))
	if !ok || in != 10 || cr != 100 || cc != 0 || out != 7 {
		t.Errorf("Responses 拆分: in=%d cr=%d cc=%d out=%d ok=%v, want 10/100/0/7/true", in, cr, cc, out, ok)
	}
	// Anthropic 形状仍走原分支（无 input_tokens_details）。
	in, cr, cc, out, ok = parseNonStreamUsage([]byte(`{"usage":{"input_tokens":110,"cache_read_input_tokens":100,"cache_creation_input_tokens":2,"output_tokens":7}}`))
	if !ok || in != 110 || cr != 100 || cc != 2 || out != 7 {
		t.Errorf("Anthropic 分支: in=%d cr=%d cc=%d out=%d ok=%v, want 110/100/2/7/true", in, cr, cc, out, ok)
	}
}

// TestWriteSSEPingRawShape 验证保活 ping 的协议分流：透传流写 SSE 注释行（客户端都忽略），
// 翻译/Anthropic 流维持 Anthropic ping 事件。
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

// TestWriteSSEErrorRawShape 验证重试用尽时的 SSE error 协议分流：透传流写 response.failed 事件。
func TestWriteSSEErrorRawShape(t *testing.T) {
	rec := httptest.NewRecorder()
	writeSSEError(rec, nil, "upstream status 429", &flight{translated: translatedResponsesRaw})
	s := rec.Body.String()
	if !strings.HasPrefix(s, "event: response.failed\n") || !strings.Contains(s, `"type":"response.failed"`) || !strings.Contains(s, "upstream status 429") {
		t.Errorf("透传流 error=%q, want response.failed 事件含消息", s)
	}
	// data 部分须是合法 JSON（含 response.error.message）。
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

// responsesRawSSE 是假上游返回的 Responses SSE 流：function_call 工具调用 + 文本 + 终局 usage
// （input 110 含缓存 100，output 7）。response.completed 里 model 是上游真实模型名，
// 供管线回写成客户端原名。
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

// TestResponsesRawPassthrough 端到端：Responses 口请求命中带 url_response_api 的路由 →
// 原文透传（不翻译）到该字段指定的上游，model 改写、key 覆盖照常；客户端收到原样的
// Responses SSE（model 回写客户端原名）；统计按 Responses 口径（缓存拆分）、工具计数、
// 完成流归档带 "responses-raw" 标记。
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
		Upstream:       "http://default-upstream.invalid", // 透传路由命中后不应使用
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

	// 上游收到的应是 Responses 原文（仅 model 被路由改写）：没有翻译痕迹（无 messages/system 字段），
	// stream 保持客户端原值（不像翻译口强制 true 后翻 Anthropic）。
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

	// 客户端收到原样 Responses SSE；response.completed 里的 model 回写成客户端原名。
	s := string(body)
	if !strings.Contains(s, "response.completed") || !strings.Contains(s, "function_call_arguments.delta") {
		t.Errorf("客户端应收到 Responses SSE 原文: %s", s)
	}
	if !strings.Contains(s, `"model":"k3-coding"`) {
		t.Errorf("响应 model 应回写客户端原名 k3-coding: %s", s)
	}

	// 统计口径：input 110 拆成 fresh 10 + 缓存命中 100，output 7。
	stats.mu.Lock()
	in, cr, out := stats.inputTokens, stats.cacheRead, stats.outputTokens
	stats.mu.Unlock()
	if in != 10 || cr != 100 || out != 7 {
		t.Errorf("聚合统计 in=%d cr=%d out=%d, want 10/100/7", in, cr, out)
	}

	// 完成流归档：responses-raw 标记、token 拆分、工具标签、delivered（记 200 而非 499）。
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

// TestResponsesRawPassthroughNonStream 透传 + 客户端 stream:false：上游回非流式 response JSON，
// 统计走 parseNonStreamUsage 的 Responses 分支。
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

// TestResponsesRawRouteLost 配置热重载 race：预检时路由带 url_response_api，
// 到 handler 路由循环时字段没了——502 明说，不静默错路。
func TestResponsesRawRouteLost(t *testing.T) {
	resetStats()
	// 预检与路由循环读同一份 cfg，中途无法插入换配置；等价验证路径：
	// 直接构造带 responses-raw 标记的内部请求打 handler，路由循环看到无字段的路由 → 502。
	cfg.Store(&Config{
		MaxRetries:     0,
		TotalBudgetSec: 10,
		Routes:         []RouteRule{{Pattern: "k3*", URL: "http://a.invalid"}}, // 无 url_response_api
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
