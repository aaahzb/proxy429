package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// ---- Marker detection ----

// TestHasSearchSubagentMarker locks the standalone-search marker detection: the Claude Code search sub-agent
// request carries "Perform a web search for the query" in a user message (string or block-array content form).
func TestHasSearchSubagentMarker(t *testing.T) {
	cases := []struct {
		name string
		body string
		want bool
	}{
		{"string content form", `{"messages":[{"role":"user","content":"Perform a web search for the query: 广州天气"}]}`, true},
		{"block content form", `{"messages":[{"role":"user","content":[{"type":"text","text":"Perform a web search for the query: X"}]}]}`, true},
		{"plain search talk without marker", `{"messages":[{"role":"user","content":"搜一下"}]}`, false},
		{"web_search tool but no marker", `{"tools":[{"type":"web_search_20250305"}],"messages":[{"role":"user","content":"搜一下"}]}`, false},
	}
	for _, tc := range cases {
		if got := hasSearchSubagentMarker([]byte(tc.body)); got != tc.want {
			t.Errorf("%s: hasSearchSubagentMarker = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// TestSearchMarkerQuery locks query extraction: the text after the marker inside the same JSON string,
// with the leading colon stripped; escapes are decoded; no marker means empty (the caller's default kicks in).
func TestSearchMarkerQuery(t *testing.T) {
	cases := []struct {
		name string
		body string
		want string
	}{
		{"string form", `{"messages":[{"role":"user","content":"Perform a web search for the query: 广州天气"}]}`, "广州天气"},
		{"block form", `{"messages":[{"role":"user","content":[{"type":"text","text":"Perform a web search for the query: 广州天气 今天 预报"}]}]}`, "广州天气 今天 预报"},
		{"escaped chars decoded", `{"messages":[{"role":"user","content":"Perform a web search for the query: 北京\"晴\"转多云"}]}`, `北京"晴"转多云`},
		{"empty after marker", `{"messages":[{"role":"user","content":"Perform a web search for the query"}]}`, ""},
		{"no marker", `{"messages":[{"role":"user","content":"搜一下"}]}`, ""},
	}
	for _, tc := range cases {
		if got := searchMarkerQuery([]byte(tc.body)); got != tc.want {
			t.Errorf("%s: searchMarkerQuery = %q, want %q", tc.name, got, tc.want)
		}
	}
}

// ---- Summary instruction injection ----

// TestInjectAfterMarkerStringForm locks the byte-level surgery on string-form content: the instruction lands
// inside the very string holding the marker, everything else preserved byte for byte.
func TestInjectAfterMarkerStringForm(t *testing.T) {
	in := `{"model":"m","messages":[{"role":"user","content":"Perform a web search for the query: 广州天气"}]}`
	instr := "Line1\nLine2 \"quoted\""
	out, ok := injectAfterMarker([]byte(in), instr)
	if !ok {
		t.Fatal("injectAfterMarker 应成功")
	}
	var m map[string]any
	if err := json.Unmarshal(out, &m); err != nil {
		t.Fatalf("注入后不是合法 JSON: %v", err)
	}
	content, _ := m["messages"].([]any)[0].(map[string]any)["content"].(string)
	want := "Perform a web search for the query: 广州天气\n\n" + instr
	if content != want {
		t.Errorf("content = %q, want %q", content, want)
	}
	// Byte preservation: everything before the insertion point and after the string stays untouched.
	if !bytes.HasPrefix(out, []byte(`{"model":"m","messages":[{"role":"user","content":"Perform a web search for the query: 广州天气`)) {
		t.Errorf("注入点前的字节被改动: %s", out)
	}
	if !bytes.HasSuffix(out, []byte(`"}]}`)) {
		t.Errorf("字符串收尾后的字节被改动: %s", out)
	}
}

// TestInjectAfterMarkerBlockForm locks injection into the text block holding the marker; sibling blocks and
// other fields (system/tools) stay byte-identical.
func TestInjectAfterMarkerBlockForm(t *testing.T) {
	in := `{"system":[{"type":"text","text":"sys"}],"messages":[{"role":"user","content":[{"type":"text","text":"Perform a web search for the query: X"},{"type":"text","text":"second block"}]}],"tools":[{"type":"web_search_20250305"}]}`
	out, ok := injectAfterMarker([]byte(in), "do the summary")
	if !ok {
		t.Fatal("injectAfterMarker 应成功")
	}
	s := string(out)
	if !strings.Contains(s, `"text":"Perform a web search for the query: X\n\ndo the summary"`) {
		t.Errorf("指令未注入标记所在 text 块: %s", s)
	}
	if !strings.Contains(s, `{"type":"text","text":"second block"}`) {
		t.Errorf("同消息的第二个块被改动: %s", s)
	}
	if !strings.Contains(s, `"system":[{"type":"text","text":"sys"}]`) || !strings.Contains(s, `"tools":[{"type":"web_search_20250305"}]`) {
		t.Errorf("system/tools 字段被改动: %s", s)
	}
}

// TestInjectAfterMarkerNoMarker locks the no-marker passthrough: ok=false and the body is returned untouched.
func TestInjectAfterMarkerNoMarker(t *testing.T) {
	in := `{"messages":[{"role":"user","content":"搜一下"}]}`
	out, ok := injectAfterMarker([]byte(in), "x")
	if ok {
		t.Error("无标记应 ok=false")
	}
	if !bytes.Equal(out, []byte(in)) {
		t.Errorf("无标记时 body 不应变化: %s", out)
	}
}

// ---- Thinking rewrite ----

// TestRewriteSearchThinking locks the claudeSearchThinking mapping, convertOff2Low-style: whatever shape the
// request carried, off stays disabled (sampling knobs kept) and low/mid/high/max always rewrite to the adaptive
// shape (adaptive + output_config.effort; mid=medium; sampling knobs die when thinking turns on). Neither the
// request thinking shape nor any model name is consulted, and the adaptive shape has no budget, so a tiny
// max_tokens never blocks the rewrite. (A field the request did not have is appended pretty-spaced by
// setTopLevelJSONValue, so effort assertions match the inner pair only.)
func TestRewriteSearchThinking(t *testing.T) {
	base := `{"model":"M","max_tokens":64000,"temperature":1,"top_p":0.9,"thinking":{"type":"disabled"},"output_config":{"effort":"low"},"messages":[]}`
	cases := []struct {
		name         string
		body         string
		level        string
		wantOK       bool
		wantContains []string
		wantAbsent   []string
	}{
		{"off keeps disabled, drops output_config, keeps sampling", base, "off", true,
			[]string{`"thinking":{"type":"disabled"}`, `"temperature":1`, `"top_p":0.9`},
			[]string{`"output_config"`, `"budget_tokens"`}},
		{"low rewrites to adaptive low", base, "low", true,
			[]string{`"thinking":{"type":"adaptive"}`, `"effort":"low"`},
			[]string{`"temperature"`, `"top_p"`, `"budget_tokens"`}},
		{"mid maps to medium", base, "mid", true,
			[]string{`"effort":"medium"`}, nil},
		{"high rewrites to adaptive high", base, "high", true,
			[]string{`"effort":"high"`}, nil},
		{"max rewrites to adaptive max", base, "max", true,
			[]string{`"thinking":{"type":"adaptive"}`, `"effort":"max"`},
			[]string{`"budget_tokens"`, `"temperature"`}},
		{"an enabled+budget request still gets the adaptive shape", `{"model":"M","max_tokens":64000,"thinking":{"type":"enabled","budget_tokens":4096},"messages":[]}`, "max", true,
			[]string{`"thinking":{"type":"adaptive"}`, `"effort":"max"`},
			[]string{`"budget_tokens"`}},
		{"an adaptive request only changes level", `{"model":"M","max_tokens":64000,"thinking":{"type":"adaptive"},"output_config":{"effort":"low"},"messages":[]}`, "high", true,
			[]string{`"thinking":{"type":"adaptive"}`, `"effort":"high"`},
			[]string{`"budget_tokens"`}},
		{"tiny max_tokens no longer blocks the rewrite", `{"model":"M","max_tokens":64,"thinking":{"type":"disabled"},"messages":[]}`, "low", true,
			[]string{`"thinking":{"type":"adaptive"}`, `"effort":"low"`}, nil},
		{"empty level is a no-op", base, "", false, nil, nil},
		{"unknown level is a no-op", base, "banana", false, nil, nil},
	}
	for _, tc := range cases {
		out, ok := rewriteSearchThinking([]byte(tc.body), tc.level)

		if ok != tc.wantOK {
			t.Errorf("%s: ok = %v, want %v (out=%s)", tc.name, ok, tc.wantOK, out)
			continue
		}
		if !ok {
			if !bytes.Equal(out, []byte(tc.body)) {
				t.Errorf("%s: 放弃改写时 body 不应变化: %s", tc.name, out)
			}
			continue
		}
		s := string(out)
		for _, sub := range tc.wantContains {
			if !strings.Contains(s, sub) {
				t.Errorf("%s: 缺 %q: %s", tc.name, sub, s)
			}
		}
		for _, sub := range tc.wantAbsent {
			if strings.Contains(s, sub) {
				t.Errorf("%s: 不应含 %q: %s", tc.name, sub, s)
			}
		}
		var m map[string]any
		if err := json.Unmarshal(out, &m); err != nil {
			t.Errorf("%s: 改写后不是合法 JSON: %v", tc.name, err)
		}
	}
}

// ---- Effective-behavior resolution (new fields beat legacy) ----

// TestResolveSearchBehavior locks the precedence table: claudeSearchSummary/claudeSearchThinking win;
// noForce hard-disables injection even with summary_mode:true; legacy summary_mode/summary_thinking map
// to injection-at-level / thinking-low when the new fields are absent.
func TestResolveSearchBehavior(t *testing.T) {
	cases := []struct {
		name       string
		sf         *SearchRoute
		wantInject string
		wantThink  string
	}{
		{"nil route", nil, "", ""},
		{"bare route", &SearchRoute{}, "", ""},
		{"legacy summary mode defaults to low", &SearchRoute{SummaryMode: true}, "low", ""},
		{"legacy summary mode with level", &SearchRoute{SummaryMode: true, SummaryLevel: "max"}, "max", ""},
		{"legacy summary thinking", &SearchRoute{SummaryMode: true, SummaryThinking: true}, "low", "low"},
		{"legacy summary thinking without summary mode", &SearchRoute{SummaryThinking: true}, "", "low"},
		{"forceLow", &SearchRoute{ClaudeSearchSummary: "forceLow"}, "low", ""},
		{"forceMid", &SearchRoute{ClaudeSearchSummary: "forceMid"}, "mid", ""},
		{"forceHigh", &SearchRoute{ClaudeSearchSummary: "forceHigh"}, "high", ""},
		{"new field beats legacy level", &SearchRoute{ClaudeSearchSummary: "forceMid", SummaryMode: true, SummaryLevel: "high"}, "mid", ""},
		{"noForce suppresses summary_mode", &SearchRoute{ClaudeSearchSummary: "noForce", SummaryMode: true, SummaryLevel: "max"}, "", ""},
		{"claudeSearchThinking off", &SearchRoute{ClaudeSearchThinking: "off"}, "", "off"},
		{"claudeSearchThinking beats legacy summary_thinking", &SearchRoute{ClaudeSearchThinking: "high", SummaryThinking: true}, "", "high"},
		{"both new fields", &SearchRoute{ClaudeSearchSummary: "forceHigh", ClaudeSearchThinking: "max"}, "high", "max"},
	}
	for _, tc := range cases {
		inject, think := resolveSearchBehavior(tc.sf)
		if inject != tc.wantInject || think != tc.wantThink {
			t.Errorf("%s: resolveSearchBehavior = (%q,%q), want (%q,%q)", tc.name, inject, think, tc.wantInject, tc.wantThink)
		}
	}
}

// ---- Summary prompt wording (single-request phrasing) ----

// TestSummaryLevelConfig locks the reworded single-request instruction: phrased as an order to the searching
// model ("After performing the web search"), one per detail level; query and Beijing-time lines carried.
func TestSummaryLevelConfig(t *testing.T) {
	cases := []struct {
		level   string
		wantKey string
	}{
		{"low", "short plaintext summary"},
		{"", "short plaintext summary"}, // Empty defaults to low
		{"mid", "medium-detail"},
		{"high", "detailed comprehensive"},
		{"HIGH", "detailed comprehensive"}, // Case-insensitive
		{"max", "verbatim"},                // max: full restatement of steps/methods/code/formulas
		{"full", "verbatim"},               // full equals max
		{"unknown", "short plaintext"},     // Unknown falls back to low
	}
	for _, c := range cases {
		instr := summaryLevelConfig(c.level, "测试查询")
		if !strings.Contains(instr, c.wantKey) {
			t.Errorf("level=%q 指令缺关键词 %q: %s", c.level, c.wantKey, instr)
		}
		if !strings.Contains(instr, "After performing the web search") {
			t.Errorf("level=%q 指令应为单请求语序: %s", c.level, instr)
		}
		if !strings.Contains(instr, "across all search results") {
			t.Errorf("level=%q 指令应指 search results 而非第二个请求里的结果块: %s", c.level, instr)
		}
		if strings.Contains(instr, "web_search_tool_result above") {
			t.Errorf("level=%q 指令残留两步模式措辞: %s", c.level, instr)
		}
		if !strings.Contains(instr, "测试查询") || !strings.Contains(instr, "UTC+8") {
			t.Errorf("level=%q 指令缺 query 或北京时间行: %s", c.level, instr)
		}
	}
	// The four level instructions should all differ
	low := summaryLevelConfig("low", "q")
	mid := summaryLevelConfig("mid", "q")
	high := summaryLevelConfig("high", "q")
	mx := summaryLevelConfig("max", "q")
	if low == mid || mid == high || low == high {
		t.Error("三档指令不应相同")
	}
	if mx == high {
		t.Error("max 指令应与 high 不同（须含完整复述要求）")
	}
}

// ---- End-to-end: search_fallback handles a standalone search in ONE upstream request ----

// searchSubagentReq mirrors the production Claude Code standalone-search shape (small body, forced tool_choice
// omitted here, thinking disabled, server-side web_search tool, marker in a block-form user message).
const searchSubagentReq = `{"model":"claude-opus-4-8","stream":true,"max_tokens":32000,"temperature":1,"thinking":{"type":"disabled"},"tools":[{"type":"web_search_20250305","name":"web_search","max_uses":8}],"messages":[{"role":"user","content":[{"type":"text","text":"Perform a web search for the query: 广州天气 今天 预报"}]}]}`

// searchE2EMock builds a Kimi-shaped SSE upstream: it records every request body and answers with a leading
// thinking block + server_tool_use + web_search_tool_result + summary text in one stream (the single-request
// shape; the thinking block makes the strip differential visible: thinking-off clients get it stripped away).
func searchE2EMock(t *testing.T) (srv *httptest.Server, bodies func() [][]byte) {
	t.Helper()
	var mu sync.Mutex
	var got [][]byte
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		got = append(got, b)
		mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"model\":\"kimi-for-coding\",\"usage\":{\"output_tokens\":1}}}\n\n")
		fmt.Fprint(w, "event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"thinking\",\"thinking\":\"\"}}\n\n")
		fmt.Fprint(w, "event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"thinking_delta\",\"thinking\":\"e2e think\"}}\n\n")
		fmt.Fprint(w, "event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"signature_delta\",\"signature\":\"sig\"}}\n\n")
		fmt.Fprint(w, "event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\n")
		fmt.Fprint(w, "event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":1,\"content_block\":{\"type\":\"server_tool_use\",\"id\":\"srvtoolu_e2e\",\"name\":\"web_search\",\"input\":{\"query\":\"广州天气\"}}}\n\n")
		fmt.Fprint(w, "event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":1}\n\n")
		fmt.Fprint(w, "event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":2,\"content_block\":{\"type\":\"web_search_tool_result\",\"tool_use_id\":\"srvtoolu_e2e\",\"content\":[{\"type\":\"web_search_tool_result_content\",\"url\":\"https://example.com/1\",\"title\":\"广州天气预报\"}]}}\n\n")
		fmt.Fprint(w, "event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":2}\n\n")
		fmt.Fprint(w, "event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":3,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n")
		fmt.Fprint(w, "event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":3,\"delta\":{\"type\":\"text_delta\",\"text\":\"Result 1: 晴转多云\"}}\n\n")
		fmt.Fprint(w, "event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":3}\n\n")

		fmt.Fprint(w, "event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":11}}\n\n")
		fmt.Fprint(w, "event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")
	}))
	return srv, func() [][]byte {
		mu.Lock()
		defer mu.Unlock()
		return append([][]byte(nil), got...)
	}
}

// runSearchE2E wires a no_search route + the given search_fallback, posts clientBody to the proxy, and returns
// the client's full response, the sf upstream's request bodies, and whether the original route was hit.
func runSearchE2E(t *testing.T, sf *SearchRoute, clientBody string) (clientOut string, sfBodies [][]byte, routeCalled bool) {
	t.Helper()
	resetStats()
	sfMock, bodies := searchE2EMock(t)
	defer sfMock.Close()
	sf.URL = sfMock.URL

	routeHit := false
	routeMock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		routeHit = true
		w.WriteHeader(200)
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer routeMock.Close()

	cfg.Store(&Config{
		Upstream:           "http://no-default.example.com",
		MaxRetries:         0,
		TotalBudgetSec:     10,
		RecentSampleWindow: 5,
		Routes: []RouteRule{
			{Pattern: "claude-opus*", URL: routeMock.URL, API: "sk-route-xxx", Model: "ark-opus", NoSearch: true},
		},
		SearchFallback: sf,
	})
	defer cfg.Store(&Config{})
	stats.resetSampleCap(5)

	proxy := httptest.NewServer(http.HandlerFunc(handler))
	defer proxy.Close()
	resp, err := http.Post(proxy.URL+"/v1/messages", "application/json", strings.NewReader(clientBody))
	if err != nil {
		t.Fatalf("请求失败: %v", err)
	}
	out, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	return string(out), bodies(), routeHit
}

// checkPassthrough asserts the upstream's Kimi-shaped stream reached the client verbatim.
func checkPassthrough(t *testing.T, out string) {
	t.Helper()
	for _, sub := range []string{"server_tool_use", "srvtoolu_e2e", "web_search_tool_result", "广州天气预报", "Result 1: 晴转多云", "message_stop"} {
		if !strings.Contains(out, sub) {
			t.Errorf("响应未逐字透传（缺 %q）: %s", sub, out)
		}
	}
}

// TestSearchFallbackSummarySingleRequest locks the core refactor: legacy summary_mode injects the instruction
// into the search request itself — ONE upstream POST, response passed through, no synthesized stream.
func TestSearchFallbackSummarySingleRequest(t *testing.T) {
	out, bodies, routeCalled := runSearchE2E(t,
		&SearchRoute{API: "sk-sf", Model: "kimi-for-coding", SummaryMode: true, SummaryLevel: "mid"},
		searchSubagentReq)
	if routeCalled {
		t.Error("不应命中原 no_search 路由上游")
	}
	if len(bodies) != 1 {
		t.Fatalf("单请求改造：上游应只收到 1 个 POST，实收 %d", len(bodies))
	}
	up := string(bodies[0])
	for _, sub := range []string{"After performing the web search", "medium-detail", "广州天气 今天 预报", `"model":"kimi-for-coding"`} {
		if !strings.Contains(up, sub) {
			t.Errorf("注入后的上游请求缺 %q: %s", sub, up)
		}
	}
	// No thinking rewrite without configuration: the client's disabled thinking and sampling knobs survive.
	if !strings.Contains(up, `"thinking":{"type":"disabled"}`) || !strings.Contains(up, `"temperature":1`) {
		t.Errorf("未配置思考改写时原 thinking/temperature 应保留: %s", up)
	}
	checkPassthrough(t, out)
}

// TestSearchFallbackClaudeSearchFields locks the new fields: forceHigh injects the high-level instruction and
// claudeSearchThinking:"low" rewrites thinking to adaptive+effort low (sampling knobs dropped); since the
// client asked thinking off, the mock thinking block is stripped from the response it sees.
func TestSearchFallbackClaudeSearchFields(t *testing.T) {
	out, bodies, _ := runSearchE2E(t,
		&SearchRoute{API: "sk-sf", Model: "kimi-for-coding", ClaudeSearchSummary: "forceHigh", ClaudeSearchThinking: "low"},
		searchSubagentReq)
	if len(bodies) != 1 {
		t.Fatalf("上游应只收到 1 个 POST，实收 %d", len(bodies))
	}
	up := string(bodies[0])
	for _, sub := range []string{"detailed comprehensive", `"thinking":{"type":"adaptive"}`, `"effort":"low"`} {
		if !strings.Contains(up, sub) {
			t.Errorf("上游请求缺 %q: %s", sub, up)
		}
	}
	for _, sub := range []string{`"temperature"`, `"budget_tokens"`} {
		if strings.Contains(up, sub) {
			t.Errorf("开思考后不应残留 %q: %s", sub, up)
		}
	}
	if strings.Contains(out, "thinking_delta") {
		t.Errorf("客户端本来关思考：回程思考块应被剥离: %s", out)
	}
	checkPassthrough(t, out)

}

// TestSearchFallbackNoForce locks noForce beating summary_mode: the body goes upstream byte-identical
// except the model rewrite.
func TestSearchFallbackNoForce(t *testing.T) {
	_, bodies, _ := runSearchE2E(t,
		&SearchRoute{API: "sk-sf", Model: "kimi-for-coding", SummaryMode: true, SummaryLevel: "max", ClaudeSearchSummary: "noForce"},
		searchSubagentReq)
	if len(bodies) != 1 {
		t.Fatalf("上游应只收到 1 个 POST，实收 %d", len(bodies))
	}
	want := strings.Replace(searchSubagentReq, `"model":"claude-opus-4-8"`, `"model":"kimi-for-coding"`, 1)
	if !bytes.Equal(bodies[0], []byte(want)) {
		t.Errorf("noForce 时请求体应仅替换 model，其余逐字节不动:\n got: %s\nwant: %s", bodies[0], want)
	}
}

// TestSearchFallbackNoMarkerNoInjection locks the marker gate: a web_search-carrying request without the
// standalone-search marker still reroutes to search_fallback (capability), but no instruction is injected.
func TestSearchFallbackNoMarkerNoInjection(t *testing.T) {
	_, bodies, routeCalled := runSearchE2E(t,
		&SearchRoute{API: "sk-sf", Model: "kimi-for-coding", SummaryMode: true, SummaryLevel: "mid"},
		`{"model":"claude-opus-4-8","stream":true,"tools":[{"type":"web_search_20250305","name":"web_search"}],"messages":[{"role":"user","content":"搜一下"}]}`)
	if routeCalled {
		t.Error("带搜索工具的请求仍应分流到 search_fallback，而不是原路由")
	}
	if len(bodies) != 1 {
		t.Fatalf("上游应只收到 1 个 POST，实收 %d", len(bodies))
	}
	if strings.Contains(string(bodies[0]), "After performing the web search") {
		t.Errorf("无标记请求不应注入摘要指令: %s", bodies[0])
	}
}

// TestSearchFallbackLegacySummaryThinking locks legacy summary_thinking:true mapping to a low-thinking rewrite.
func TestSearchFallbackLegacySummaryThinking(t *testing.T) {
	_, bodies, _ := runSearchE2E(t,
		&SearchRoute{API: "sk-sf", Model: "kimi-for-coding", SummaryThinking: true},
		searchSubagentReq)
	if len(bodies) != 1 {
		t.Fatalf("上游应只收到 1 个 POST，实收 %d", len(bodies))
	}
	up := string(bodies[0])
	if !strings.Contains(up, `"thinking":{"type":"adaptive"}`) || !strings.Contains(up, `"effort":"low"`) {
		t.Errorf("legacy summary_thinking 应改写为 adaptive low: %s", up)
	}
	if strings.Contains(up, `"temperature"`) || strings.Contains(up, `"budget_tokens"`) {
		t.Errorf("开思考后不应残留 temperature/budget_tokens: %s", up)

	}
}

// ---- Per-route claudeSearch* fields (routes[] entries) ----

// bigMaxSearchReq mirrors searchSubagentReq with max_tokens doubled (a leftover from the budget-shape days;
// the adaptive shape has no budget to fit, the bigger ceiling simply keeps the fixture varied).
var bigMaxSearchReq = strings.Replace(searchSubagentReq, `"max_tokens":32000`, `"max_tokens":64000`, 1)

// runSearchE2EFull generalizes runSearchE2E: the route rule is caller-supplied (per-route claudeSearch* fields);
// when routeServesSearch the route's own URL points at the recording search mock (a search-capable route serving
// the request itself, no search_fallback involved), otherwise the route points at a hit-detecting dummy and
// search_fallback (sf, nil allowed) serves the diverted request.
func runSearchE2EFull(t *testing.T, rr RouteRule, sf *SearchRoute, routeServesSearch bool, clientBody string) (clientOut string, sfBodies [][]byte, routeCalled bool, recent string) {
	t.Helper()
	resetStats()
	sfMock, bodies := searchE2EMock(t)
	defer sfMock.Close()
	if sf != nil {
		sf.URL = sfMock.URL
	}

	routeHit := false
	routeMock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		routeHit = true
		w.WriteHeader(200)
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer routeMock.Close()
	if routeServesSearch {
		rr.URL = sfMock.URL
	} else {
		rr.URL = routeMock.URL
	}

	cfg.Store(&Config{
		Upstream:           "http://no-default.example.com",
		MaxRetries:         0,
		TotalBudgetSec:     10,
		RecentSampleWindow: 5,
		Routes:             []RouteRule{rr},
		SearchFallback:     sf,
	})
	defer cfg.Store(&Config{})
	stats.resetSampleCap(5)

	proxy := httptest.NewServer(http.HandlerFunc(handler))
	defer proxy.Close()
	resp, err := http.Post(proxy.URL+"/v1/messages", "application/json", strings.NewReader(clientBody))
	if err != nil {
		t.Fatalf("请求失败: %v", err)
	}
	out, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	// The finished flight lands in the recent ring asynchronously; poll the handler directly (it is not
	// registered on the test server's handler) until this flight shows up, and hand the JSON to callers
	// for status-page badge assertions.
	for i := 0; i < 100; i++ {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, recentFlightsPath, nil)
		req.RemoteAddr = "127.0.0.1:1" // The console handlers gate on a local peer
		recentFlightsHandler(rec, req)
		recent = rec.Body.String()
		if strings.Contains(recent, `"searchPrompt"`) {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	return string(out), bodies(), routeHit, recent
}

// TestRouteSearchFieldsOverrideSearchFallback locks per-field route-wins on the search_fallback path: the
// no_search route's forceHigh/max beat search_fallback's own forceLow/low, while sf still serves the request.
func TestRouteSearchFieldsOverrideSearchFallback(t *testing.T) {
	out, bodies, routeCalled, recent := runSearchE2EFull(t,
		RouteRule{Pattern: "claude-opus*", API: "sk-route-xxx", Model: "ark-opus", NoSearch: true,
			ClaudeSearchSummary: "forceHigh", ClaudeSearchThinking: "max"},
		&SearchRoute{API: "sk-sf", Model: "kimi-for-coding", ClaudeSearchSummary: "forceLow", ClaudeSearchThinking: "low"},
		false, bigMaxSearchReq)
	if routeCalled {
		t.Error("不应命中原 no_search 路由上游")
	}
	if len(bodies) != 1 {
		t.Fatalf("上游应只收到 1 个 POST，实收 %d", len(bodies))
	}
	up := string(bodies[0])
	for _, sub := range []string{"detailed comprehensive", `"thinking":{"type":"adaptive"}`, `"effort":"max"`, `"model":"kimi-for-coding"`} {
		if !strings.Contains(up, sub) {
			t.Errorf("路由字段应逐字段压过 search_fallback（缺 %q）: %s", sub, up)
		}
	}
	if strings.Contains(up, "short plaintext summary") {
		t.Errorf("sf 自己的 forceLow 不应生效: %s", up)
	}
	checkPassthrough(t, out)
	// Status-page badges: marker hit + injection + thinking rewrite all surfaced on the recent flight.
	for _, sub := range []string{`"searchMarker":true`, `"searchLevel":"high"`, `"searchThinkFrom":"off"`, `"searchThinkTo":"max"`} {
		if !strings.Contains(recent, sub) {
			t.Errorf("recentflights 应带搜索徽标字段（缺 %q）: %s", sub, recent)
		}
	}
}

// TestRouteSearchFieldsPartialOverride locks per-field independence: the route's noForce kills the injection
// while search_fallback's own claudeSearchThinking still applies (the route left thinking unset).
func TestRouteSearchFieldsPartialOverride(t *testing.T) {
	_, bodies, _, recent := runSearchE2EFull(t,
		RouteRule{Pattern: "claude-opus*", API: "sk-route-xxx", Model: "ark-opus", NoSearch: true,
			ClaudeSearchSummary: "noForce"},
		&SearchRoute{API: "sk-sf", Model: "kimi-for-coding", ClaudeSearchSummary: "forceMid", ClaudeSearchThinking: "low"},
		false, searchSubagentReq)
	if len(bodies) != 1 {
		t.Fatalf("上游应只收到 1 个 POST，实收 %d", len(bodies))
	}
	up := string(bodies[0])
	if strings.Contains(up, "After performing the web search") {
		t.Errorf("路由的 noForce 应压过 sf 的 forceMid（不注入）: %s", up)
	}
	if !strings.Contains(up, `"thinking":{"type":"adaptive"}`) || !strings.Contains(up, `"effort":"low"`) {
		t.Errorf("路由未设 thinking 时应回落 sf 的 claudeSearchThinking:low: %s", up)

	}
	// Badges: noForce kills the injection (level stays empty), the thinking rewrite still shows from->to.
	for _, sub := range []string{`"searchMarker":true`, `"searchLevel":""`, `"searchThinkFrom":"off"`, `"searchThinkTo":"low"`} {
		if !strings.Contains(recent, sub) {
			t.Errorf("recentflights 徽标字段（缺 %q）: %s", sub, recent)
		}
	}
}

// TestRouteSearchFieldsOnSearchCapableRoute locks the new standalone use: a search-capable route with no
// enhance_search still gets injection + thinking rewrite from its own claudeSearch* fields, served by the
// route's own upstream in one request.
func TestRouteSearchFieldsOnSearchCapableRoute(t *testing.T) {
	out, bodies, _, recent := runSearchE2EFull(t,
		RouteRule{Pattern: "claude-opus*", API: "sk-route", Model: "kimi-for-coding",
			ClaudeSearchSummary: "forceMid", ClaudeSearchThinking: "low"},
		nil, true, searchSubagentReq)
	if len(bodies) != 1 {
		t.Fatalf("路由上游应只收到 1 个 POST，实收 %d", len(bodies))
	}
	up := string(bodies[0])
	for _, sub := range []string{"After performing the web search", "medium-detail", `"thinking":{"type":"adaptive"}`, `"effort":"low"`, `"model":"kimi-for-coding"`} {
		if !strings.Contains(up, sub) {
			t.Errorf("路由自身服务时其 claudeSearch* 字段应生效（缺 %q）: %s", sub, up)
		}
	}
	checkPassthrough(t, out)
	for _, sub := range []string{`"searchMarker":true`, `"searchLevel":"mid"`, `"searchThinkFrom":"off"`, `"searchThinkTo":"low"`} {
		if !strings.Contains(recent, sub) {
			t.Errorf("recentflights 徽标字段（缺 %q）: %s", sub, recent)
		}
	}
}

// TestSearchRewriteStripsThinkingForOffClient locks the convertOff2Low-style response side: the client never
// asked for thinking (explicitly disabled here, plus the no-thinking-field variant), the rewrite turned thinking
// on upstream (adaptive low), so the returning thinking block is stripped and the surviving blocks renumber
// gapless — the client sees a thinking-off answer while the upstream request shows what really went out.
func TestSearchRewriteStripsThinkingForOffClient(t *testing.T) {
	noThinkingReq := strings.Replace(searchSubagentReq, `"thinking":{"type":"disabled"},`, ``, 1)
	for name, body := range map[string]string{"explicit disabled": searchSubagentReq, "no thinking field": noThinkingReq} {
		out, bodies, _ := runSearchE2E(t,
			&SearchRoute{API: "sk-sf", Model: "kimi-for-coding", ClaudeSearchThinking: "low"},
			body)
		if len(bodies) != 1 {
			t.Fatalf("%s: 上游应只收到 1 个 POST，实收 %d", name, len(bodies))
		}
		if !strings.Contains(string(bodies[0]), `"effort":"low"`) {
			t.Errorf("%s: 上游应收到 adaptive low: %s", name, bodies[0])
		}
		if strings.Contains(out, "thinking_delta") || strings.Contains(out, `"type":"thinking"`) {
			t.Errorf("%s: 没主动要思考的客户端不应看到思考块: %s", name, out)
		}
		// The surviving first block must be renumbered to index 0 (a gapless stream; re-marshaled key order).
		if !strings.Contains(out, `"index":0,"type":"content_block_start"`) {
			t.Errorf("%s: 剥离后块序号应无缝重排: %s", name, out)
		}
		checkPassthrough(t, out)
	}
}

// TestSearchRewriteKeepsThinkingForThinkingClient locks the other half of the differential: a client whose
// request already had thinking on (adaptive high) still gets the rewrite to low upstream but keeps seeing the
// thinking blocks — only the level changed, never the response shape the client asked for.
func TestSearchRewriteKeepsThinkingForThinkingClient(t *testing.T) {
	adaptiveReq := strings.Replace(searchSubagentReq, `"thinking":{"type":"disabled"}`, `"thinking":{"type":"adaptive"},"output_config":{"effort":"high"}`, 1)
	out, bodies, _, recent := runSearchE2EFull(t,
		RouteRule{Pattern: "claude-opus*", API: "sk-route-xxx", Model: "ark-opus", NoSearch: true},
		&SearchRoute{API: "sk-sf", Model: "kimi-for-coding", ClaudeSearchSummary: "forceLow", ClaudeSearchThinking: "low"},
		false, adaptiveReq)
	if len(bodies) != 1 {
		t.Fatalf("上游应只收到 1 个 POST，实收 %d", len(bodies))
	}
	if !strings.Contains(string(bodies[0]), `"effort":"low"`) {
		t.Errorf("上游应收到改写后的 adaptive low: %s", bodies[0])
	}
	if !strings.Contains(out, "thinking_delta") {
		t.Errorf("开思考客户端应照常看到思考块（不剥离）: %s", out)
	}
	for _, sub := range []string{`"searchThinkFrom":"high"`, `"searchThinkTo":"low"`} {
		if !strings.Contains(recent, sub) {
			t.Errorf("recentflights 徽标字段（缺 %q）: %s", sub, recent)
		}
	}
}

// TestRouteSearchFieldsBeatEnhanceLegacy locks new-beats-legacy on the route itself: noForce suppresses the
// enhance_search legacy injection; the body reaches the route upstream unchanged except the model rewrite.
func TestRouteSearchFieldsBeatEnhanceLegacy(t *testing.T) {
	_, bodies, _, _ := runSearchE2EFull(t,
		RouteRule{Pattern: "claude-opus*", API: "sk-route", Model: "kimi-for-coding",
			EnhanceSearch: &EnhanceSearchConfig{SummaryLevel: "high"}, ClaudeSearchSummary: "noForce"},
		nil, true, searchSubagentReq)
	if len(bodies) != 1 {
		t.Fatalf("路由上游应只收到 1 个 POST，实收 %d", len(bodies))
	}
	want := strings.Replace(searchSubagentReq, `"model":"claude-opus-4-8"`, `"model":"kimi-for-coding"`, 1)
	if !bytes.Equal(bodies[0], []byte(want)) {
		t.Errorf("noForce 时请求体应仅替换 model，其余逐字节不动:\n got: %s\nwant: %s", bodies[0], want)
	}
}

// TestValidateRouteClaudeSearchEnums locks config validation for the route-level fields: bad enums are
// rejected naming the field, good ones pass.
func TestValidateRouteClaudeSearchEnums(t *testing.T) {
	badThink := &Config{Routes: []RouteRule{{Pattern: "x", URL: "http://u", ClaudeSearchThinking: "banana"}}}
	if err := validateConfigEnums(badThink); err == nil || !strings.Contains(err.Error(), "claudeSearchThinking") {
		t.Errorf("非法 claudeSearchThinking 应报错并点名字段: %v", err)
	}
	badSummary := &Config{Routes: []RouteRule{{Pattern: "x", URL: "http://u", ClaudeSearchSummary: "forceMax"}}}
	if err := validateConfigEnums(badSummary); err == nil || !strings.Contains(err.Error(), "claudeSearchSummary") {
		t.Errorf("非法 claudeSearchSummary 应报错并点名字段: %v", err)
	}
	ok := &Config{Routes: []RouteRule{{Pattern: "x", URL: "http://u", ClaudeSearchThinking: "mid", ClaudeSearchSummary: "forceHigh"}}}
	if err := validateConfigEnums(ok); err != nil {
		t.Errorf("合法枚举不应报错: %v", err)
	}
}
