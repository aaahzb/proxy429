package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

// resetStats 清空全局 stats，保证各测试互不影响。
func resetStats() {
	stats.mu.Lock()
	stats.active = 0
	stats.waiting = 0
	stats.cacheRead = 0
	stats.inputTokens = 0
	stats.outputTokens = 0
	stats.bytesForward.Store(0)
	stats.statusRetries.Store(0)
	stats.classifierRewrites.Store(0)
	stats.resetSampleCap(0) // 清空"最近X次"延迟/吞吐样本
	stats.mu.Unlock()

	// flight 注册表与日志缓冲区也重置（handler 总会 register flight，map 不能为 nil）。
	flights.mu.Lock()
	flights.m = make(map[uint64]*flight)
	flights.nextID.Store(0)
	flights.mu.Unlock()

	logBuf.mu.Lock()
	logBuf.lines = make([]string, 0, maxLogBuf)
	logBuf.head = 0
	logBuf.len = 0
	logBuf.mu.Unlock()
}

func TestParseSSEStatsMessageStart(t *testing.T) {
	resetStats()
	var last int64
	parseSSEStats([]byte(`data: {"type":"message_start","message":{"usage":{"input_tokens":10,"cache_read_input_tokens":5,"cache_creation_input_tokens":3,"output_tokens":1}}}`), &last)
	if stats.cacheRead != 5 {
		t.Errorf("cacheRead=%d want 5", stats.cacheRead)
	}
	if stats.inputTokens != 10 {
		t.Errorf("input=%d want 10", stats.inputTokens)
	}
	if stats.outputTokens != 1 {
		t.Errorf("output=%d want 1", stats.outputTokens)
	}
	if last != 1 {
		t.Errorf("last=%d want 1", last)
	}
}

func TestParseSSEStatsOutputDelta(t *testing.T) {
	resetStats()
	var last int64
	// 单流：output_tokens 是累积值 1→10→20→50，全局应只记最终 50（增量之和）。
	parseSSEStats([]byte(`data: {"type":"message_start","message":{"usage":{"output_tokens":1}}}`), &last)
	parseSSEStats([]byte(`data: {"type":"message_delta","usage":{"output_tokens":10}}`), &last)
	parseSSEStats([]byte(`data: {"type":"message_delta","usage":{"output_tokens":20}}`), &last)
	parseSSEStats([]byte(`data: {"type":"message_delta","usage":{"output_tokens":50}}`), &last)
	if stats.outputTokens != 50 {
		t.Errorf("output=%d want 50", stats.outputTokens)
	}
}

func TestParseSSEStatsMultiStream(t *testing.T) {
	resetStats()
	var lastA, lastB int64
	// 两个并发流各自独立 lastOutput，全局累加两者当前值：100+30=130。
	parseSSEStats([]byte(`data: {"type":"message_delta","usage":{"output_tokens":100}}`), &lastA)
	parseSSEStats([]byte(`data: {"type":"message_delta","usage":{"output_tokens":30}}`), &lastB)
	if stats.outputTokens != 130 {
		t.Errorf("output=%d want 130", stats.outputTokens)
	}
}

func TestParseSSEStatsIgnoresNonData(t *testing.T) {
	resetStats()
	var last int64
	parseSSEStats([]byte(`event: content_block_delta`), &last)
	parseSSEStats([]byte(`data: [DONE]`), &last)
	parseSSEStats([]byte(``), &last)
	parseSSEStats([]byte(`data: {"type":"content_block_delta","delta":{"text":"x"}}`), &last)
	if stats.outputTokens != 0 || stats.cacheRead != 0 {
		t.Errorf("非 usage 事件不应计数: output=%d cacheRead=%d", stats.outputTokens, stats.cacheRead)
	}
}

func TestHumanNum(t *testing.T) {
	cases := []struct {
		in   int64
		want string
	}{
		{0, "0"}, {999, "999"}, {1234, "1.2k"}, {1500, "1.5k"}, {1000000, "1.00M"}, {2500000, "2.50M"},
	}
	for _, c := range cases {
		if got := humanNum(c.in); got != c.want {
			t.Errorf("humanNum(%d)=%q want %q", c.in, got, c.want)
		}
	}
}

func TestHumanBytes(t *testing.T) {
	cases := []struct {
		in   int64
		want string
	}{
		{0, "0B"}, {512, "512B"}, {1024, "1.0KB"}, {2048, "2.0KB"}, {1024 * 1024, "1.00MB"}, {int64(1.5 * 1024 * 1024), "1.50MB"},
	}
	for _, c := range cases {
		if got := humanBytes(c.in); got != c.want {
			t.Errorf("humanBytes(%d)=%q want %q", c.in, got, c.want)
		}
	}
}

// TestRecentLatency 验证加权吞吐与平均首字延迟计算。
func TestRecentLatency(t *testing.T) {
	resetStats()
	stats.resetSampleCap(3)
	// 3 条样本：首字 100/200/300ms，流式 1000/2000/3000ms，output 10/20/30 tokens
	stats.pushFirstByte(100)
	stats.pushThroughput(1000, 10)
	stats.pushFirstByte(200)
	stats.pushThroughput(2000, 20)
	stats.pushFirstByte(300)
	stats.pushThroughput(3000, 30)
	avgFB, tps := stats.recentLatency()
	// 平均首字 = (100+200+300)/3 = 200ms
	if avgFB != 200 {
		t.Errorf("avgFB=%v want 200", avgFB)
	}
	// 加权吞吐 = (10+20+30) / ((1000+2000+3000)/1000) = 60/6 = 10 tok/s
	if tps != 10 {
		t.Errorf("tps=%v want 10", tps)
	}
}

// TestRecentLatencyRingOverwrite 验证环形缓冲超过容量时覆盖最旧样本。
func TestRecentLatencyRingOverwrite(t *testing.T) {
	resetStats()
	stats.resetSampleCap(2)
	stats.pushFirstByte(100)
	stats.pushThroughput(1000, 10)
	stats.pushFirstByte(200)
	stats.pushThroughput(1000, 20)
	stats.pushFirstByte(300)
	stats.pushThroughput(1000, 30) // 容量2，覆盖第1条(100,10)
	avgFB, tps := stats.recentLatency()
	// 窗口剩 200/300：平均首字 = 250ms
	if avgFB != 250 {
		t.Errorf("avgFB=%v want 250（环形覆盖后）", avgFB)
	}
	// 吞吐 = (20+30)/(2000/1000) = 25 tok/s
	if tps != 25 {
		t.Errorf("tps=%v want 25", tps)
	}
}

// TestRecentLatencyEmpty 验证无样本时返回 0。
func TestRecentLatencyEmpty(t *testing.T) {
	resetStats()
	stats.resetSampleCap(5)
	avgFB, tps := stats.recentLatency()
	if avgFB != 0 || tps != 0 {
		t.Errorf("空窗口 avgFB=%v tps=%v want 0,0", avgFB, tps)
	}
}

// TestHandlerLatencySampling 端到端验证：正常流（情况C）透传结束后，
// 首字延迟与 token/s 被采样进滑动窗口，recentLatency 返回非零。
func TestHandlerLatencySampling(t *testing.T) {
	resetStats()
	// mock 上游：返回带 usage 的 SSE 流（message_start -> message_delta -> [DONE]）。
	// 加 sleep 模拟真实首字延迟与流式吐字耗时，避免本地太快被 Milliseconds() 截断为 0。
	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		time.Sleep(15 * time.Millisecond) // 模拟"等首字"
		w.WriteHeader(200)
		f := w.(http.Flusher)
		fmt.Fprint(w, "data: {\"type\":\"message_start\",\"message\":{\"usage\":{\"output_tokens\":1}}}\n\n")
		f.Flush()
		time.Sleep(25 * time.Millisecond) // 模拟流式吐字
		fmt.Fprint(w, "data: {\"type\":\"message_delta\",\"usage\":{\"output_tokens\":50}}\n\n")
		f.Flush()
		fmt.Fprint(w, "data: [DONE]\n\n")
		f.Flush()
	}))
	defer mock.Close()

	cfg.Store(&Config{
		Upstream:           mock.URL,
		MaxRetries:         0,
		TotalBudgetSec:     10,
		RecentSampleWindow: 5,
	})
	stats.resetSampleCap(5)

	proxy := httptest.NewServer(http.HandlerFunc(handler))
	defer proxy.Close()

	resp, err := http.Post(proxy.URL+"/v1/messages", "application/json",
		strings.NewReader(`{"model":"x","messages":[{"role":"user","content":"hi"}]}`))
	if err != nil {
		t.Fatalf("请求失败: %v", err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()

	avgFB, tps := stats.recentLatency()
	if avgFB <= 0 {
		t.Errorf("avgFB=%v want >0（应采样到首字延迟）", avgFB)
	}
	if tps <= 0 {
		t.Errorf("tps=%v want >0（应采样到 token/s）", tps)
	}
	t.Logf("采样结果: 首字 %.2fms, %.1f tok/s", avgFB, tps)
}

// setCfg 设置测试用配置（maybeRewriteClassifier 读全局 cfg）。
func setCfg(thinkingDisabled bool, maxTokens int) {
	cfg.Store(&Config{
		ClassifierThinkingDisabled: thinkingDisabled,
		ClassifierSystemPrefix:     "You are a security monitor",
		ClassifierMaxTokens:        maxTokens,
	})
}

// parseBody 解析 JSON body 为 map。
func parseBody(t *testing.T, b []byte) map[string]interface{} {
	t.Helper()
	var p map[string]interface{}
	if err := json.Unmarshal(b, &p); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	return p
}

// TestMaybeRewriteClassifierString 验证字符串 system 命中分类器时关 thinking。
func TestMaybeRewriteClassifierString(t *testing.T) {
	resetStats()
	setCfg(true, 0)
	body := []byte(`{"model":"x","system":"You are a security monitor. Check this.","thinking":{"type":"enabled","budget_tokens":1024},"reasoning_effort":"high","reasoning":{"effort":"high"},"max_tokens":2048,"messages":[]}`)
	p := parseBody(t, maybeRewriteClassifier(body))
	if th, _ := p["thinking"].(map[string]interface{}); th["type"] != "disabled" {
		t.Errorf("thinking=%v want type=disabled", p["thinking"])
	}
	if p["reasoning_effort"] != "none" {
		t.Errorf("reasoning_effort=%v want none", p["reasoning_effort"])
	}
	if _, ok := p["reasoning"]; ok {
		t.Errorf("reasoning 应被删除，got %v", p["reasoning"])
	}
	// ClassifierMaxTokens=0 不压，保持原值。
	if mt, _ := p["max_tokens"].(float64); mt != 2048 {
		t.Errorf("max_tokens=%v want 2048（0 不压）", p["max_tokens"])
	}
}

// TestMaybeRewriteClassifierArray 验证数组 system 命中分类器时关 thinking。
func TestMaybeRewriteClassifierArray(t *testing.T) {
	resetStats()
	setCfg(true, 0)
	body := []byte(`{"system":[{"type":"text","text":"You are a security monitor."}],"thinking":{"type":"enabled"},"max_tokens":1024}`)
	out := maybeRewriteClassifier(body)
	p := parseBody(t, out)
	if th, _ := p["thinking"].(map[string]interface{}); th["type"] != "disabled" {
		t.Errorf("thinking=%v want type=disabled", p["thinking"])
	}
	// 原 body 没有 reasoning_effort，改写后应追加。
	if p["reasoning_effort"] != "none" {
		t.Errorf("reasoning_effort=%v want none（应追加到 body）", p["reasoning_effort"])
	}
	// key 顺序保留：原字段在前，reasoning_effort 追加到末尾。
	want := []string{"system", "thinking", "max_tokens", "reasoning_effort"}
	if keys := topLevelKeys(t, out); !reflect.DeepEqual(keys, want) {
		t.Errorf("key 顺序:\n got %v\nwant %v", keys, want)
	}
}

// TestMaybeRewriteClassifierNonClassifier 验证普通请求（system 不匹配）不改写。
func TestMaybeRewriteClassifierNonClassifier(t *testing.T) {
	resetStats()
	setCfg(true, 0)
	body := []byte(`{"system":"You are a helpful assistant.","thinking":{"type":"enabled"},"max_tokens":1024}`)
	if out := maybeRewriteClassifier(body); !bytes.Equal(body, out) {
		t.Errorf("普通请求不应被改写")
	}
}

// TestMaybeRewriteClassifierDisabled 验证开关关闭时不改写。
func TestMaybeRewriteClassifierDisabled(t *testing.T) {
	resetStats()
	setCfg(false, 0)
	body := []byte(`{"system":"You are a security monitor.","thinking":{"type":"enabled"},"max_tokens":1024}`)
	if out := maybeRewriteClassifier(body); !bytes.Equal(body, out) {
		t.Errorf("ClassifierThinkingDisabled=false 时不应改写")
	}
}

// TestMaybeRewriteClassifierMaxTokens 验证 max_tokens>0 时被压到配置值。
func TestMaybeRewriteClassifierMaxTokens(t *testing.T) {
	resetStats()
	setCfg(true, 512)
	body := []byte(`{"system":"You are a security monitor.","thinking":{"type":"enabled"},"max_tokens":4096}`)
	p := parseBody(t, maybeRewriteClassifier(body))
	if mt, _ := p["max_tokens"].(float64); mt != 512 {
		t.Errorf("max_tokens=%v want 512", p["max_tokens"])
	}
}

// TestMaybeRewriteClassifierPreservesOrder 验证改写保留原始 key 顺序（不重排）。
// 这是修 bug 的核心：旧实现 json.Unmarshal 到 map 再 Marshal 会按字典序重排 key。
func TestMaybeRewriteClassifierPreservesOrder(t *testing.T) {
	resetStats()
	setCfg(true, 0)
	// 故意用非字母序的 key 顺序（model 在前，messages 在后）。
	body := []byte(`{"model":"x","system":"You are a security monitor.","thinking":{"type":"enabled"},"reasoning_effort":"high","reasoning":{"effort":"high"},"max_tokens":2048,"messages":[]}`)
	out := maybeRewriteClassifier(body)
	// reasoning 被删，其余保留原顺序。
	want := []string{"model", "system", "thinking", "reasoning_effort", "max_tokens", "messages"}
	if keys := topLevelKeys(t, out); !reflect.DeepEqual(keys, want) {
		t.Errorf("key 顺序:\n got %v\nwant %v", keys, want)
	}
}

// topLevelKeys 提取 JSON 顶层 object 的 key 顺序。
func topLevelKeys(t *testing.T, b []byte) []string {
	t.Helper()
	dec := json.NewDecoder(bytes.NewReader(b))
	tok, err := dec.Token()
	if err != nil || tok != json.Delim('{') {
		t.Fatalf("不是 object: %v", tok)
	}
	var keys []string
	for dec.More() {
		k, err := dec.Token()
		if err != nil {
			t.Fatalf("读 key: %v", err)
		}
		keys = append(keys, k.(string))
		var v json.RawMessage
		dec.Decode(&v)
	}
	return keys
}

// TestExtractModel 验证从请求体轻量提取 model 字段值（[请求] 日志展示用）。
func TestExtractModel(t *testing.T) {
	cases := []struct {
		name string
		body string
		want string
	}{
		{"正常", `{"model":"glm-5.2","messages":[]}`, "glm-5.2"},
		{"带空格", `{ "model" : "claude-sonnet-5", "max_tokens": 100 }`, "claude-sonnet-5"},
		{"model在后", `{"max_tokens":100,"model":"deepseek-v3"}`, "deepseek-v3"},
		{"无model字段", `{"messages":[]}`, ""},
		{"model非字符串", `{"model":123}`, ""},
		{"转义引号不提前终止", `{"model":"a\"b","messages":[]}`, `a\"b`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := extractModel([]byte(tc.body)); got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
}

// TestMatchModel 验证模型名 * 通配匹配（前后中 *、多 *、精确）。
func TestMatchModel(t *testing.T) {
	cases := []struct {
		pattern, name string
		want          bool
	}{
		{"claude-opus*", "claude-opus-4-8", true},
		{"claude-opus*", "claude-opus", true},
		{"claude-opus*", "claude-sonnet-5", false},
		{"*opus", "claude-opus", true},
		{"*opus", "claude-sonnet", false},
		{"claude-*", "claude-opus", true},
		{"claude-*", "glm-5.2", false},
		{"a*b*c", "axxxbyyyc", true},
		{"a*b*c", "abc", true},
		{"a*b*c", "ac", false}, // 中间段 b 必须出现
		{"*", "anything", true},
		{"exact", "exact", true},
		{"exact", "exact2", false},
	}
	for _, tc := range cases {
		if got := matchModel(tc.pattern, tc.name); got != tc.want {
			t.Errorf("matchModel(%q, %q) = %v, want %v", tc.pattern, tc.name, got, tc.want)
		}
	}
}

// TestReplaceModelValue 验证替换 model 字段值，长度变化且 JSON 仍合法；无 model 字段原样返回。
func TestReplaceModelValue(t *testing.T) {
	out := replaceModelValue([]byte(`{"model":"a","x":1}`), "bb")
	if got := extractModel(out); got != "bb" {
		t.Errorf("model got %q want bb", got)
	}
	var p map[string]interface{}
	if err := json.Unmarshal(out, &p); err != nil {
		t.Errorf("改写后非合法 JSON: %v (out=%s)", err, out)
	}
	out2 := replaceModelValue([]byte(`{"messages":[]}`), "x")
	if string(out2) != `{"messages":[]}` {
		t.Errorf("无 model 字段应原样返回，got %s", out2)
	}
}

// TestRouteHandler 端到端：命中路由规则的请求改走目标上游，model 与 Authorization 被替换，默认上游不被命中。
func TestRouteHandler(t *testing.T) {
	resetStats()
	var rg struct {
		mu     sync.Mutex
		model  string
		auth   string
		called bool
	}
	routeMock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		rg.mu.Lock()
		rg.called = true
		rg.model = extractModel(body)
		rg.auth = r.Header.Get("Authorization")
		rg.mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		fmt.Fprint(w, "data: {\"type\":\"message_start\",\"message\":{\"usage\":{\"output_tokens\":1}}}\n\ndata: [DONE]\n\n")
	}))
	defer routeMock.Close()

	defaultCalled := false
	defaultMock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defaultCalled = true
		w.WriteHeader(200)
		fmt.Fprint(w, "data: {\"type\":\"message_start\",\"message\":{\"usage\":{\"output_tokens\":1}}}\n\ndata: [DONE]\n\n")
	}))
	defer defaultMock.Close()

	cfg.Store(&Config{
		Upstream:           defaultMock.URL,
		MaxRetries:         0,
		TotalBudgetSec:     10,
		RecentSampleWindow: 5,
		Routes: []RouteRule{
			{Pattern: "claude-opus*", URL: routeMock.URL, API: "sk-route-xxx", Model: "deepseek-V4-pro"},
		},
	})
	stats.resetSampleCap(5)

	proxy := httptest.NewServer(http.HandlerFunc(handler))
	defer proxy.Close()

	resp, err := http.Post(proxy.URL+"/v1/messages", "application/json",
		strings.NewReader(`{"model":"claude-opus-4-8","messages":[{"role":"user","content":"hi"}]}`))
	if err != nil {
		t.Fatalf("请求失败: %v", err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()

	if defaultCalled {
		t.Errorf("不应命中默认上游（应被路由到目标）")
	}
	rg.mu.Lock()
	defer rg.mu.Unlock()
	if !rg.called {
		t.Fatalf("应命中路由目标上游")
	}
	if rg.model != "deepseek-V4-pro" {
		t.Errorf("目标上游收到的 model = %q, want deepseek-V4-pro", rg.model)
	}
	if rg.auth != "Bearer sk-route-xxx" {
		t.Errorf("目标上游 Authorization = %q, want Bearer sk-route-xxx", rg.auth)
	}
}

// TestNoRouteFallback 端到端：未配置 routes 时走默认上游，model 不改、客户端 token 透传。
func TestNoRouteFallback(t *testing.T) {
	resetStats()
	var g struct {
		mu     sync.Mutex
		model  string
		auth   string
		called bool
	}
	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		g.mu.Lock()
		g.called = true
		g.model = extractModel(body)
		g.auth = r.Header.Get("Authorization")
		g.mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		fmt.Fprint(w, "data: {\"type\":\"message_start\",\"message\":{\"usage\":{\"output_tokens\":1}}}\n\ndata: [DONE]\n\n")
	}))
	defer mock.Close()

	cfg.Store(&Config{
		Upstream:           mock.URL,
		MaxRetries:         0,
		TotalBudgetSec:     10,
		RecentSampleWindow: 5,
		// 不设 Routes：走默认上游、透传客户端 token
	})
	stats.resetSampleCap(5)

	proxy := httptest.NewServer(http.HandlerFunc(handler))
	defer proxy.Close()

	req, _ := http.NewRequest("POST", proxy.URL+"/v1/messages", strings.NewReader(`{"model":"glm-5.2","messages":[]}`))
	req.Header.Set("Authorization", "Bearer client-token-xxx")
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("请求失败: %v", err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()

	g.mu.Lock()
	defer g.mu.Unlock()
	if !g.called {
		t.Fatalf("应命中默认上游")
	}
	if g.model != "glm-5.2" {
		t.Errorf("model = %q, want glm-5.2（未路由不应改 model）", g.model)
	}
	if g.auth != "Bearer client-token-xxx" {
		t.Errorf("Authorization = %q, want 透传客户端 token", g.auth)
	}
}

// TestClassifierRouteHit 端到端：命中分类器请求且配了 classifier_route 时，
// 无视原 model 统一路由到分类器目标，且优先于 model 路由（model 路由目标不应被命中）。
func TestClassifierRouteHit(t *testing.T) {
	resetStats()
	var cg struct {
		mu     sync.Mutex
		model  string
		auth   string
		called bool
	}
	// 分类器路由目标
	clfMock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		cg.mu.Lock()
		cg.called = true
		cg.model = extractModel(body)
		cg.auth = r.Header.Get("Authorization")
		cg.mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		fmt.Fprint(w, "data: {\"type\":\"message_start\",\"message\":{\"usage\":{\"output_tokens\":1}}}\n\ndata: [DONE]\n\n")
	}))
	defer clfMock.Close()

	// model 路由目标（不应命中：分类器路由优先）
	modelRouteCalled := false
	modelRouteMock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		modelRouteCalled = true
		w.WriteHeader(200)
		fmt.Fprint(w, "data: {\"type\":\"message_start\",\"message\":{\"usage\":{\"output_tokens\":1}}}\n\ndata: [DONE]\n\n")
	}))
	defer modelRouteMock.Close()

	defaultCalled := false
	defaultMock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defaultCalled = true
		w.WriteHeader(200)
		fmt.Fprint(w, "data: {\"type\":\"message_start\",\"message\":{\"usage\":{\"output_tokens\":1}}}\n\ndata: [DONE]\n\n")
	}))
	defer defaultMock.Close()

	cfg.Store(&Config{
		Upstream:               defaultMock.URL,
		MaxRetries:             0,
		TotalBudgetSec:         10,
		RecentSampleWindow:     5,
		ClassifierSystemPrefix: "You are a security monitor",
		ClassifierRoute: &ClassifierRoute{
			URL:   clfMock.URL,
			API:   "sk-clf-xxx",
			Model: "deepseek-v4-flash",
		},
		Routes: []RouteRule{
			{Pattern: "claude-opus*", URL: modelRouteMock.URL, API: "sk-model-xxx", Model: "glm-5.2"},
		},
	})
	stats.resetSampleCap(5)

	proxy := httptest.NewServer(http.HandlerFunc(handler))
	defer proxy.Close()

	// 分类器请求：system 前缀匹配，原 model 是 claude-opus-4-8（本应命中 model 路由，但分类器路由优先）
	resp, err := http.Post(proxy.URL+"/v1/messages", "application/json",
		strings.NewReader(`{"model":"claude-opus-4-8","system":"You are a security monitor. Check this.","messages":[]}`))
	if err != nil {
		t.Fatalf("请求失败: %v", err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()

	if defaultCalled {
		t.Errorf("不应命中默认上游")
	}
	if modelRouteCalled {
		t.Errorf("不应命中 model 路由目标（分类器路由应优先）")
	}
	cg.mu.Lock()
	defer cg.mu.Unlock()
	if !cg.called {
		t.Fatalf("应命中分类器路由目标")
	}
	if cg.model != "deepseek-v4-flash" {
		t.Errorf("分类器目标收到的 model = %q, want deepseek-v4-flash", cg.model)
	}
	if cg.auth != "Bearer sk-clf-xxx" {
		t.Errorf("分类器目标 Authorization = %q, want Bearer sk-clf-xxx", cg.auth)
	}
}

// TestClassifierRouteFallback 端到端：命中分类器但未配 classifier_route 时，
// 仍按原 model 走 routes（兼容旧行为，不因新功能而改变）。
func TestClassifierRouteFallback(t *testing.T) {
	resetStats()
	var mg struct {
		mu     sync.Mutex
		model  string
		called bool
	}
	modelMock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mg.mu.Lock()
		mg.called = true
		mg.model = extractModel(body)
		mg.mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		fmt.Fprint(w, "data: {\"type\":\"message_start\",\"message\":{\"usage\":{\"output_tokens\":1}}}\n\ndata: [DONE]\n\n")
	}))
	defer modelMock.Close()

	defaultCalled := false
	defaultMock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defaultCalled = true
		w.WriteHeader(200)
		fmt.Fprint(w, "data: {\"type\":\"message_start\",\"message\":{\"usage\":{\"output_tokens\":1}}}\n\ndata: [DONE]\n\n")
	}))
	defer defaultMock.Close()

	cfg.Store(&Config{
		Upstream:               defaultMock.URL,
		MaxRetries:             0,
		TotalBudgetSec:         10,
		RecentSampleWindow:     5,
		ClassifierSystemPrefix: "You are a security monitor",
		// ClassifierRoute 故意不设：分类器请求应回退到 model 路由
		Routes: []RouteRule{
			{Pattern: "claude-opus*", URL: modelMock.URL, API: "sk-model-xxx", Model: "glm-5.2"},
		},
	})
	stats.resetSampleCap(5)

	proxy := httptest.NewServer(http.HandlerFunc(handler))
	defer proxy.Close()

	resp, err := http.Post(proxy.URL+"/v1/messages", "application/json",
		strings.NewReader(`{"model":"claude-opus-4-8","system":"You are a security monitor. Check this.","messages":[]}`))
	if err != nil {
		t.Fatalf("请求失败: %v", err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()

	if defaultCalled {
		t.Errorf("不应命中默认上游（未配 classifier_route 应按 model 路由）")
	}
	mg.mu.Lock()
	defer mg.mu.Unlock()
	if !mg.called {
		t.Fatalf("应命中 model 路由目标（未配 classifier_route 时回退）")
	}
	if mg.model != "glm-5.2" {
		t.Errorf("model 路由目标 model = %q, want glm-5.2", mg.model)
	}
}

// TestClassifierRouteNotClassifier 端到端：非分类器请求即使配了 classifier_route 也不走分类器路由，
// 仍按 model 路由走。
func TestClassifierRouteNotClassifier(t *testing.T) {
	resetStats()
	clfCalled := false
	clfMock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		clfCalled = true
		w.WriteHeader(200)
		fmt.Fprint(w, "data: {\"type\":\"message_start\",\"message\":{\"usage\":{\"output_tokens\":1}}}\n\ndata: [DONE]\n\n")
	}))
	defer clfMock.Close()

	var mg struct {
		mu     sync.Mutex
		model  string
		called bool
	}
	modelMock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mg.mu.Lock()
		mg.called = true
		mg.model = extractModel(body)
		mg.mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		fmt.Fprint(w, "data: {\"type\":\"message_start\",\"message\":{\"usage\":{\"output_tokens\":1}}}\n\ndata: [DONE]\n\n")
	}))
	defer modelMock.Close()

	defaultCalled := false
	defaultMock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defaultCalled = true
		w.WriteHeader(200)
		fmt.Fprint(w, "data: {\"type\":\"message_start\",\"message\":{\"usage\":{\"output_tokens\":1}}}\n\ndata: [DONE]\n\n")
	}))
	defer defaultMock.Close()

	cfg.Store(&Config{
		Upstream:               defaultMock.URL,
		MaxRetries:             0,
		TotalBudgetSec:         10,
		RecentSampleWindow:     5,
		ClassifierSystemPrefix: "You are a security monitor",
		ClassifierRoute: &ClassifierRoute{
			URL:   clfMock.URL,
			API:   "sk-clf-xxx",
			Model: "deepseek-v4-flash",
		},
		Routes: []RouteRule{
			{Pattern: "claude-opus*", URL: modelMock.URL, API: "sk-model-xxx", Model: "glm-5.2"},
		},
	})
	stats.resetSampleCap(5)

	proxy := httptest.NewServer(http.HandlerFunc(handler))
	defer proxy.Close()

	// 普通请求：system 不含分类器前缀，应按 model 路由走，不触发分类器路由
	resp, err := http.Post(proxy.URL+"/v1/messages", "application/json",
		strings.NewReader(`{"model":"claude-opus-4-8","system":"You are a helpful assistant.","messages":[]}`))
	if err != nil {
		t.Fatalf("请求失败: %v", err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()

	if clfCalled {
		t.Errorf("不应命中分类器路由目标（非分类器请求）")
	}
	if defaultCalled {
		t.Errorf("不应命中默认上游（应按 model 路由）")
	}
	mg.mu.Lock()
	defer mg.mu.Unlock()
	if !mg.called {
		t.Fatalf("应命中 model 路由目标")
	}
	if mg.model != "glm-5.2" {
		t.Errorf("model 路由目标 model = %q, want glm-5.2", mg.model)
	}
}

// TestFastRouteHit 端到端：fast 请求（含 "speed":"fast" 的非分类器请求）命中 fast_route，
// speed 字段移除、Anthropic-Beta 头删除、model 路由不应命中、响应含 fast 限流 headers。
func TestFastRouteHit(t *testing.T) {
	resetStats()
	var cg struct {
		mu     sync.Mutex
		model  string
		auth   string
		body   []byte
		beta   string
		called bool
	}
	fastMock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		cg.mu.Lock()
		cg.called = true
		cg.model = extractModel(body)
		cg.auth = r.Header.Get("Authorization")
		cg.body = body
		cg.beta = r.Header.Get("Anthropic-Beta")
		cg.mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		fmt.Fprint(w, "data: {\"type\":\"message_start\",\"message\":{\"usage\":{\"output_tokens\":1}}}\n\ndata: [DONE]\n\n")
	}))
	defer fastMock.Close()

	modelRouteCalled := false
	modelRouteMock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		modelRouteCalled = true
		w.WriteHeader(200)
		fmt.Fprint(w, "data: {\"type\":\"message_start\",\"message\":{\"usage\":{\"output_tokens\":1}}}\n\ndata: [DONE]\n\n")
	}))
	defer modelRouteMock.Close()

	defaultCalled := false
	defaultMock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defaultCalled = true
		w.WriteHeader(200)
		fmt.Fprint(w, "data: {\"type\":\"message_start\",\"message\":{\"usage\":{\"output_tokens\":1}}}\n\ndata: [DONE]\n\n")
	}))
	defer defaultMock.Close()

	cfg.Store(&Config{
		Upstream:               defaultMock.URL,
		MaxRetries:             0,
		TotalBudgetSec:         10,
		RecentSampleWindow:     5,
		ClassifierSystemPrefix: "You are a security monitor",
		FastRoute: &FastRoute{
			URL:   fastMock.URL,
			API:   "sk-fast-xxx",
			Model: "deepseek-v4-pro",
		},
		Routes: []RouteRule{
			{Pattern: "claude-opus*", URL: modelRouteMock.URL, API: "sk-model-xxx", Model: "glm-5.2"},
		},
	})
	stats.resetSampleCap(5)

	proxy := httptest.NewServer(http.HandlerFunc(handler))
	defer proxy.Close()

	reqBody := `{"model":"claude-opus-4-8","speed":"fast","max_tokens":100,"stream":true,"messages":[]}`
	resp, err := http.Post(proxy.URL+"/v1/messages", "application/json", strings.NewReader(reqBody))
	if err != nil {
		t.Fatalf("请求失败: %v", err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()

	if defaultCalled {
		t.Errorf("不应命中默认上游")
	}
	if modelRouteCalled {
		t.Errorf("不应命中 model 路由目标（fast 路由应优先）")
	}
	cg.mu.Lock()
	defer cg.mu.Unlock()
	if !cg.called {
		t.Fatalf("应命中 fast 路由目标")
	}
	if cg.model != "deepseek-v4-pro" {
		t.Errorf("fast 目标收到的 model = %q, want deepseek-v4-pro", cg.model)
	}
	if cg.auth != "Bearer sk-fast-xxx" {
		t.Errorf("fast 目标 Authorization = %q, want Bearer sk-fast-xxx", cg.auth)
	}
	if bytes.Contains(cg.body, []byte(`"speed":"fast"`)) {
		t.Errorf("上游收到的 body 不应含 speed 字段，got: %s", cg.body)
	}
	if cg.beta != "" {
		t.Errorf("上游不应收到 Anthropic-Beta 头，got: %q", cg.beta)
	}
	if v := resp.Header.Get("anthropic-fast-output-tokens-remaining"); v != "999999" {
		t.Errorf("响应头 anthropic-fast-output-tokens-remaining = %q, want 999999", v)
	}
	if v := resp.Header.Get("anthropic-fast-input-tokens-remaining"); v != "999999" {
		t.Errorf("响应头 anthropic-fast-input-tokens-remaining = %q, want 999999", v)
	}
}

// TestFastRouteNotConfigured 端到端：fast 请求但未配 fast_route 时，
// 仍按原 model 走 routes（向后兼容），speed 字段原样透传。
func TestFastRouteNotConfigured(t *testing.T) {
	resetStats()
	var mg struct {
		mu     sync.Mutex
		model  string
		body   []byte
		called bool
	}
	modelRouteMock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mg.mu.Lock()
		mg.called = true
		mg.model = extractModel(b)
		mg.body = b
		mg.mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		fmt.Fprint(w, "data: {\"type\":\"message_start\",\"message\":{\"usage\":{\"output_tokens\":1}}}\n\ndata: [DONE]\n\n")
	}))
	defer modelRouteMock.Close()

	cfg.Store(&Config{
		Upstream:               "http://no-default-should-not-hit.example.com",
		MaxRetries:             0,
		TotalBudgetSec:         10,
		RecentSampleWindow:     5,
		ClassifierSystemPrefix: "You are a security monitor",
		Routes: []RouteRule{
			{Pattern: "claude-opus*", URL: modelRouteMock.URL, API: "sk-model-xxx", Model: "glm-5.2"},
		},
	})
	stats.resetSampleCap(5)

	proxy := httptest.NewServer(http.HandlerFunc(handler))
	defer proxy.Close()

	resp, err := http.Post(proxy.URL+"/v1/messages", "application/json",
		strings.NewReader(`{"model":"claude-opus-4-8","speed":"fast","messages":[]}`))
	if err != nil {
		t.Fatalf("请求失败: %v", err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()

	mg.mu.Lock()
	defer mg.mu.Unlock()
	if !mg.called {
		t.Fatalf("应走 model 路由（未配 fast_route 时回退）")
	}
	if mg.model != "glm-5.2" {
		t.Errorf("model 路由目标 model = %q, want glm-5.2", mg.model)
	}
	if !bytes.Contains(mg.body, []byte(`"speed":"fast"`)) {
		t.Errorf("未配 fast_route 时 speed 字段应原样透传，got: %s", mg.body)
	}
}

// TestFastRouteClassifierPriority 端到端：分类器请求（system 前缀匹配）即使含 "speed":"fast"，
// 也不走 fast 路由（分类器路由优先）。
func TestFastRouteClassifierPriority(t *testing.T) {
	resetStats()
	clfCalled := false
	clfMock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		clfCalled = true
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		fmt.Fprint(w, "data: {\"type\":\"message_start\",\"message\":{\"usage\":{\"output_tokens\":1}}}\n\ndata: [DONE]\n\n")
	}))
	defer clfMock.Close()

	fastCalled := false
	fastMock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fastCalled = true
		w.WriteHeader(200)
		fmt.Fprint(w, "data: {\"type\":\"message_start\",\"message\":{\"usage\":{\"output_tokens\":1}}}\n\ndata: [DONE]\n\n")
	}))
	defer fastMock.Close()

	cfg.Store(&Config{
		Upstream:               "http://no-default-should-not-hit.example.com",
		MaxRetries:             0,
		TotalBudgetSec:         10,
		RecentSampleWindow:     5,
		ClassifierSystemPrefix: "You are a security monitor",
		ClassifierRoute: &ClassifierRoute{
			URL:   clfMock.URL,
			API:   "sk-clf-xxx",
			Model: "deepseek-v4-flash",
		},
		FastRoute: &FastRoute{
			URL:   fastMock.URL,
			API:   "sk-fast-xxx",
			Model: "deepseek-v4-pro",
		},
	})
	stats.resetSampleCap(5)

	proxy := httptest.NewServer(http.HandlerFunc(handler))
	defer proxy.Close()

	resp, err := http.Post(proxy.URL+"/v1/messages", "application/json",
		strings.NewReader(`{"model":"claude-opus-4-8","speed":"fast","system":"You are a security monitor. Check this.","messages":[]}`))
	if err != nil {
		t.Fatalf("请求失败: %v", err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()

	if !clfCalled {
		t.Errorf("应命中分类器路由（分类器优先于 fast）")
	}
	if fastCalled {
		t.Errorf("不应命中 fast 路由（分类器应优先）")
	}
}

// TestFastRouteHeaderOnly header-only 触发：只有 Anthropic-Beta 头（无 "speed":"fast" body 字段），
// 模拟 Claude Code /fast off 后的残留 header。验证 fast 路由不应命中。
func TestFastRouteHeaderOnly(t *testing.T) {
	resetStats()
	fastCalled := false
	fastMock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fastCalled = true
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		fmt.Fprint(w, "data: {\"type\":\"message_start\",\"message\":{\"usage\":{\"output_tokens\":1}}}\n\ndata: [DONE]\n\n")
	}))
	defer fastMock.Close()

	modelRouteCalled := false
	modelRouteMock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		modelRouteCalled = true
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer modelRouteMock.Close()

	cfg.Store(&Config{
		Upstream:           "http://no-default-should-not-hit.example.com",
		MaxRetries:         0,
		TotalBudgetSec:     10,
		RecentSampleWindow: 5,
		FastRoute: &FastRoute{
			URL:   fastMock.URL,
			API:   "sk-fast-xxx",
			Model: "deepseek-v4-pro",
		},
		Routes: []RouteRule{
			{Pattern: "claude-sonnet*", URL: modelRouteMock.URL, API: "sk-model-xxx", Model: "glm-5.2"},
		},
	})
	stats.resetSampleCap(5)

	proxy := httptest.NewServer(http.HandlerFunc(handler))
	defer proxy.Close()

	// body 不带 "speed":"fast"，只靠请求头 Anthropic-Beta 不应触发 fast 路由
	req, _ := http.NewRequest("POST", proxy.URL+"/v1/messages",
		strings.NewReader(`{"model":"claude-sonnet-5","messages":[]}`))
	req.Header.Set("Anthropic-Beta", "fast-mode-2026-02-01")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("请求失败: %v", err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()

	if fastCalled {
		t.Errorf("不应命中 fast 路由（仅靠 header 不应触发）")
	}
	if !modelRouteCalled {
		t.Errorf("应按 model 路由回退")
	}
}

// TestFastRouteBodyWhitespace 验证 "speed": "fast" 带空格、且字段不在首位时仍能命中并正确移除。
func TestFastRouteBodyWhitespace(t *testing.T) {
	resetStats()
	var cg struct {
		mu     sync.Mutex
		model  string
		body   []byte
		called bool
	}
	fastMock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		cg.mu.Lock()
		cg.called = true
		cg.model = extractModel(body)
		cg.body = body
		cg.mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer fastMock.Close()

	cfg.Store(&Config{
		Upstream:           "http://no-default-should-not-hit.example.com",
		MaxRetries:         0,
		TotalBudgetSec:     10,
		RecentSampleWindow: 5,
		FastRoute: &FastRoute{
			URL:   fastMock.URL,
			API:   "sk-fast-xxx",
			Model: "deepseek-v4-pro",
		},
	})
	stats.resetSampleCap(5)

	proxy := httptest.NewServer(http.HandlerFunc(handler))
	defer proxy.Close()

	// speed 字段带空格且不在首位
	resp, err := http.Post(proxy.URL+"/v1/messages", "application/json",
		strings.NewReader(`{"model":"claude-sonnet-5", "speed": "fast", "messages":[]}`))
	if err != nil {
		t.Fatalf("请求失败: %v", err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()

	cg.mu.Lock()
	defer cg.mu.Unlock()
	if !cg.called {
		t.Fatalf("应命中 fast 路由目标")
	}
	if cg.model != "deepseek-v4-pro" {
		t.Errorf("fast 目标收到的 model = %q, want deepseek-v4-pro", cg.model)
	}
	if bytes.Contains(cg.body, []byte("speed")) {
		t.Errorf("上游 body 不应含 speed 字段，got: %s", cg.body)
	}
}

// TestHasImage 单测 hasImage：含图片块为 true，纯文本为 false。
func TestHasImage(t *testing.T) {
	cases := []struct {
		name string
		body string
		want bool
	}{
		{"图片块", `{"messages":[{"content":[{"type":"text","text":"看图"},{"type":"image","source":{"type":"base64","media_type":"image/png","data":"iVBOR"}}]}]}`, true},
		{"纯文本字符串content", `{"messages":[{"role":"user","content":"hi"}]}`, false},
		{"纯文本数组content", `{"messages":[{"content":[{"type":"text","text":"hi"}]}]}`, false},
		{"空body", ``, false},
		{"image_url不误判", `{"messages":[{"content":[{"type":"image_url","image_url":{"url":"x"}}]}]}`, false},
	}
	for _, tc := range cases {
		if got := hasImage([]byte(tc.body)); got != tc.want {
			t.Errorf("hasImage(%s) = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// TestImageFallbackRoute 端到端：含图片 + text_only 模型 + 配了 multimodal_fallback，
// 应改走兜底上游，model 改成 fallback.model，原 route 上游不应被命中。
func TestImageFallbackRoute(t *testing.T) {
	resetStats()
	var fg struct {
		mu     sync.Mutex
		model  string
		auth   string
		called bool
	}
	fbMock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		fg.mu.Lock()
		fg.called = true
		fg.model = extractModel(body)
		fg.auth = r.Header.Get("Authorization")
		fg.mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		fmt.Fprint(w, "data: {\"type\":\"message_start\",\"message\":{\"model\":\"mm-actual-1\",\"usage\":{\"output_tokens\":1}}}\n\ndata: [DONE]\n\n")
	}))
	defer fbMock.Close()

	routeCalled := false
	routeMock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		routeCalled = true
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
			{Pattern: "claude-opus*", URL: routeMock.URL, API: "sk-route-xxx", Model: "deepseek-V4-pro", TextOnly: true},
		},
		MultimodalFallback: &MultimodalRoute{URL: fbMock.URL, API: "sk-mm-xxx", Model: "glm-4.5v"},
	})
	stats.resetSampleCap(5)

	proxy := httptest.NewServer(http.HandlerFunc(handler))
	defer proxy.Close()

	// 含图片块
	resp, err := http.Post(proxy.URL+"/v1/messages", "application/json",
		strings.NewReader(`{"model":"claude-opus-4-8","messages":[{"role":"user","content":[{"type":"text","text":"看图"},{"type":"image","source":{"type":"base64","media_type":"image/png","data":"iVBOR"}}]}]}`))
	if err != nil {
		t.Fatalf("请求失败: %v", err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()

	if routeCalled {
		t.Errorf("不应命中原 text_only 路由上游（应改走多模态兜底）")
	}
	fg.mu.Lock()
	defer fg.mu.Unlock()
	if !fg.called {
		t.Fatalf("应命中多模态兜底上游")
	}
	if fg.model != "glm-4.5v" {
		t.Errorf("兜底上游收到的 model = %q, want glm-4.5v", fg.model)
	}
	if fg.auth != "Bearer sk-mm-xxx" {
		t.Errorf("兜底上游 Authorization = %q, want Bearer sk-mm-xxx", fg.auth)
	}
}

// TestImageFallbackNoImage 端到端：无图片 + text_only 模型 + 配了 fallback，
// 应走原 route 上游（兜底不触发）。
func TestImageFallbackNoImage(t *testing.T) {
	resetStats()
	var rg struct {
		mu     sync.Mutex
		called bool
		model  string
	}
	routeMock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		rg.mu.Lock()
		rg.called = true
		rg.model = extractModel(body)
		rg.mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer routeMock.Close()

	fbCalled := false
	fbMock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fbCalled = true
		w.WriteHeader(200)
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer fbMock.Close()

	cfg.Store(&Config{
		Upstream:           "http://no-default.example.com",
		MaxRetries:         0,
		TotalBudgetSec:     10,
		RecentSampleWindow: 5,
		Routes: []RouteRule{
			{Pattern: "claude-opus*", URL: routeMock.URL, API: "sk-route-xxx", Model: "deepseek-V4-pro", TextOnly: true},
		},
		MultimodalFallback: &MultimodalRoute{URL: fbMock.URL, API: "sk-mm-xxx", Model: "glm-4.5v"},
	})
	stats.resetSampleCap(5)

	proxy := httptest.NewServer(http.HandlerFunc(handler))
	defer proxy.Close()

	// 纯文本请求
	resp, err := http.Post(proxy.URL+"/v1/messages", "application/json",
		strings.NewReader(`{"model":"claude-opus-4-8","messages":[{"role":"user","content":"hi"}]}`))
	if err != nil {
		t.Fatalf("请求失败: %v", err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()

	if fbCalled {
		t.Errorf("不应命中多模态兜底（请求无图片）")
	}
	rg.mu.Lock()
	defer rg.mu.Unlock()
	if !rg.called {
		t.Fatalf("应命中原 route 上游")
	}
	if rg.model != "deepseek-V4-pro" {
		t.Errorf("原 route 上游收到的 model = %q, want deepseek-V4-pro", rg.model)
	}
}

// TestImageFallbackNotTextOnly 端到端：含图片 + 非 text_only 模型 + 配了 fallback，
// 应走原 route 上游（目标模型自己支持多模态，不兜底）。
func TestImageFallbackNotTextOnly(t *testing.T) {
	resetStats()
	var rg struct {
		mu     sync.Mutex
		called bool
	}
	routeMock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rg.mu.Lock()
		rg.called = true
		rg.mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer routeMock.Close()

	fbCalled := false
	fbMock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fbCalled = true
		w.WriteHeader(200)
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer fbMock.Close()

	cfg.Store(&Config{
		Upstream:           "http://no-default.example.com",
		MaxRetries:         0,
		TotalBudgetSec:     10,
		RecentSampleWindow: 5,
		Routes: []RouteRule{
			{Pattern: "claude-opus*", URL: routeMock.URL, API: "sk-route-xxx", Model: "deepseek-V4-pro", TextOnly: false},
		},
		MultimodalFallback: &MultimodalRoute{URL: fbMock.URL, API: "sk-mm-xxx", Model: "glm-4.5v"},
	})
	stats.resetSampleCap(5)

	proxy := httptest.NewServer(http.HandlerFunc(handler))
	defer proxy.Close()

	resp, err := http.Post(proxy.URL+"/v1/messages", "application/json",
		strings.NewReader(`{"model":"claude-opus-4-8","messages":[{"role":"user","content":[{"type":"image","source":{"type":"base64","media_type":"image/png","data":"iVBOR"}}]}]}`))
	if err != nil {
		t.Fatalf("请求失败: %v", err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()

	if fbCalled {
		t.Errorf("不应命中多模态兜底（目标模型非 text_only）")
	}
	rg.mu.Lock()
	defer rg.mu.Unlock()
	if !rg.called {
		t.Fatalf("应命中原 route 上游")
	}
}

// TestImageFallbackNotConfigured 端到端：含图片 + text_only 模型 + 未配 fallback，
// 降级走原 route 上游（由上游自行处理图片，可能报错，但代理不应崩）。
func TestImageFallbackNotConfigured(t *testing.T) {
	resetStats()
	var rg struct {
		mu     sync.Mutex
		called bool
	}
	routeMock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rg.mu.Lock()
		rg.called = true
		rg.mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
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
			{Pattern: "claude-opus*", URL: routeMock.URL, API: "sk-route-xxx", Model: "deepseek-V4-pro", TextOnly: true},
		},
		// 不配 MultimodalFallback
	})
	stats.resetSampleCap(5)

	proxy := httptest.NewServer(http.HandlerFunc(handler))
	defer proxy.Close()

	resp, err := http.Post(proxy.URL+"/v1/messages", "application/json",
		strings.NewReader(`{"model":"claude-opus-4-8","messages":[{"role":"user","content":[{"type":"image","source":{"type":"base64","media_type":"image/png","data":"iVBOR"}}]}]}`))
	if err != nil {
		t.Fatalf("请求失败: %v", err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()

	rg.mu.Lock()
	defer rg.mu.Unlock()
	if !rg.called {
		t.Fatalf("未配兜底时应降级走原 route 上游")
	}
}

// TestHasWebSearch 单测 hasWebSearch：server-side web_search 与 client-side WebSearch 都识别，纯文本为 false。
func TestHasWebSearch(t *testing.T) {
	cases := []struct {
		name string
		body string
		want bool
	}{
		{"server-side web_search", `{"tools":[{"type":"web_search_20250305","name":"web_search","max_uses":5}]}`, true},
		{"client-side WebSearch", `{"tools":[{"name":"WebSearch","description":"x","input_schema":{}}]}`, false}, // client-side 工具定义不识别（每个 Code 请求都带，会误判）
		{"纯文本无工具", `{"messages":[{"role":"user","content":"hi"}]}`, false},
		{"普通工具非搜索", `{"tools":[{"name":"computer","input_schema":{}}]}`, false},
		{"空body", ``, false},
	}
	for _, tc := range cases {
		if got := hasWebSearch([]byte(tc.body)); got != tc.want {
			t.Errorf("hasWebSearch(%s) = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// TestSearchFallbackRoute 端到端：纯搜索请求 + no_search 模型 + 配了 search_fallback，
// 应改走搜索兜底上游，model 改成 sf.model，原 route 上游不应被命中。
func TestSearchFallbackRoute(t *testing.T) {
	resetStats()
	var fg struct {
		mu     sync.Mutex
		model  string
		auth   string
		called bool
	}
	sfMock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		fg.mu.Lock()
		fg.called = true
		fg.model = extractModel(body)
		fg.auth = r.Header.Get("Authorization")
		fg.mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		fmt.Fprint(w, "data: {\"type\":\"message_start\",\"message\":{\"model\":\"ds-search-1\",\"usage\":{\"output_tokens\":1}}}\n\ndata: [DONE]\n\n")
	}))
	defer sfMock.Close()

	routeCalled := false
	routeMock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		routeCalled = true
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
		SearchFallback: &SearchRoute{URL: sfMock.URL, API: "sk-sf-xxx", Model: "deepseek-search"},
	})
	stats.resetSampleCap(5)

	proxy := httptest.NewServer(http.HandlerFunc(handler))
	defer proxy.Close()

	resp, err := http.Post(proxy.URL+"/v1/messages", "application/json",
		strings.NewReader(`{"model":"claude-opus-4-8","tools":[{"type":"web_search_20250305","name":"web_search","max_uses":5}],"messages":[{"role":"user","content":"搜一下"}]}`))
	if err != nil {
		t.Fatalf("请求失败: %v", err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()

	if routeCalled {
		t.Errorf("不应命中原 no_search 路由上游（应改走搜索兜底）")
	}
	fg.mu.Lock()
	defer fg.mu.Unlock()
	if !fg.called {
		t.Fatalf("应命中搜索兜底上游")
	}
	if fg.model != "deepseek-search" {
		t.Errorf("兜底上游收到的 model = %q, want deepseek-search", fg.model)
	}
	if fg.auth != "Bearer sk-sf-xxx" {
		t.Errorf("兜底上游 Authorization = %q, want Bearer sk-sf-xxx", fg.auth)
	}
}

// TestSearchFallbackNoSearch 端到端：无搜索工具 + no_search 模型 + 配了 sf，
// 应走原 route 上游（搜索兜底不触发）。
func TestSearchFallbackNoSearch(t *testing.T) {
	resetStats()
	var rg struct {
		mu     sync.Mutex
		called bool
		model  string
	}
	routeMock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		rg.mu.Lock()
		rg.called = true
		rg.model = extractModel(body)
		rg.mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer routeMock.Close()

	sfCalled := false
	sfMock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sfCalled = true
		w.WriteHeader(200)
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer sfMock.Close()

	cfg.Store(&Config{
		Upstream:           "http://no-default.example.com",
		MaxRetries:         0,
		TotalBudgetSec:     10,
		RecentSampleWindow: 5,
		Routes: []RouteRule{
			{Pattern: "claude-opus*", URL: routeMock.URL, API: "sk-route-xxx", Model: "ark-opus", NoSearch: true},
		},
		SearchFallback: &SearchRoute{URL: sfMock.URL, API: "sk-sf-xxx", Model: "deepseek-search"},
	})
	stats.resetSampleCap(5)

	proxy := httptest.NewServer(http.HandlerFunc(handler))
	defer proxy.Close()

	resp, err := http.Post(proxy.URL+"/v1/messages", "application/json",
		strings.NewReader(`{"model":"claude-opus-4-8","messages":[{"role":"user","content":"hi"}]}`))
	if err != nil {
		t.Fatalf("请求失败: %v", err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()

	if sfCalled {
		t.Errorf("不应命中搜索兜底（请求无搜索工具）")
	}
	rg.mu.Lock()
	defer rg.mu.Unlock()
	if !rg.called {
		t.Fatalf("应命中原 route 上游")
	}
	if rg.model != "ark-opus" {
		t.Errorf("原 route 上游收到的 model = %q, want ark-opus", rg.model)
	}
}

// TestSearchFallbackNotNoSearch 端到端：带搜索 + 非 no_search 模型 + 配了 sf，
// 应走原 route 上游（目标模型自己支持搜索，不兜底）。
func TestSearchFallbackNotNoSearch(t *testing.T) {
	resetStats()
	var rg struct {
		mu     sync.Mutex
		called bool
	}
	routeMock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rg.mu.Lock()
		rg.called = true
		rg.mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer routeMock.Close()

	sfCalled := false
	sfMock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sfCalled = true
		w.WriteHeader(200)
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer sfMock.Close()

	cfg.Store(&Config{
		Upstream:           "http://no-default.example.com",
		MaxRetries:         0,
		TotalBudgetSec:     10,
		RecentSampleWindow: 5,
		Routes: []RouteRule{
			{Pattern: "claude-opus*", URL: routeMock.URL, API: "sk-route-xxx", Model: "ark-opus", NoSearch: false},
		},
		SearchFallback: &SearchRoute{URL: sfMock.URL, API: "sk-sf-xxx", Model: "deepseek-search"},
	})
	stats.resetSampleCap(5)

	proxy := httptest.NewServer(http.HandlerFunc(handler))
	defer proxy.Close()

	resp, err := http.Post(proxy.URL+"/v1/messages", "application/json",
		strings.NewReader(`{"model":"claude-opus-4-8","tools":[{"type":"web_search_20250305","name":"web_search"}],"messages":[{"role":"user","content":"搜一下"}]}`))
	if err != nil {
		t.Fatalf("请求失败: %v", err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()

	if sfCalled {
		t.Errorf("不应命中搜索兜底（目标模型非 no_search）")
	}
	rg.mu.Lock()
	defer rg.mu.Unlock()
	if !rg.called {
		t.Fatalf("应命中原 route 上游")
	}
}

// TestSearchFallbackWithImage 端到端：带图搜索 + text_only+no_search 模型 + sf.TextOnly（搜索强但不认图）+ mf 配（支持图+搜索），
// 应走 multimodal_fallback（kimi），不认图的 search_fallback 不应被命中。
func TestSearchFallbackWithImage(t *testing.T) {
	resetStats()
	var mg struct {
		mu     sync.Mutex
		model  string
		called bool
	}
	mfMock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mg.mu.Lock()
		mg.called = true
		mg.model = extractModel(body)
		mg.mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer mfMock.Close()

	sfCalled := false
	sfMock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sfCalled = true
		w.WriteHeader(200)
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer sfMock.Close()

	routeCalled := false
	routeMock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		routeCalled = true
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
			{Pattern: "claude-opus*", URL: routeMock.URL, API: "sk-route-xxx", Model: "ark-opus", TextOnly: true, NoSearch: true},
		},
		SearchFallback:     &SearchRoute{URL: sfMock.URL, API: "sk-sf-xxx", Model: "deepseek-search", TextOnly: true},
		MultimodalFallback: &MultimodalRoute{URL: mfMock.URL, API: "sk-mf-xxx", Model: "kimi-vl"}, // NoSearch 默认 false，支持搜索
	})
	stats.resetSampleCap(5)

	proxy := httptest.NewServer(http.HandlerFunc(handler))
	defer proxy.Close()

	// 同时带图片和搜索工具
	resp, err := http.Post(proxy.URL+"/v1/messages", "application/json",
		strings.NewReader(`{"model":"claude-opus-4-8","tools":[{"type":"web_search_20250305","name":"web_search"}],"messages":[{"role":"user","content":[{"type":"text","text":"搜图里的"},{"type":"image","source":{"type":"base64","media_type":"image/png","data":"iVBOR"}}]}]}`))
	if err != nil {
		t.Fatalf("请求失败: %v", err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()

	if sfCalled {
		t.Errorf("不应命中 search_fallback（它 TextOnly 不认图）")
	}
	if routeCalled {
		t.Errorf("不应命中原 route 上游")
	}
	mg.mu.Lock()
	defer mg.mu.Unlock()
	if !mg.called {
		t.Fatalf("应命中 multimodal_fallback（支持图+搜索）")
	}
	if mg.model != "kimi-vl" {
		t.Errorf("multimodal_fallback 收到的 model = %q, want kimi-vl", mg.model)
	}
}

// TestSearchFallbackWithImageSfSupports 端到端：带图搜索 + sf 非 TextOnly（支持图+搜索），
// 应走 search_fallback（它两个能力都满足）。
func TestSearchFallbackWithImageSfSupports(t *testing.T) {
	resetStats()
	var fg struct {
		mu     sync.Mutex
		called bool
		model  string
	}
	sfMock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		fg.mu.Lock()
		fg.called = true
		fg.model = extractModel(body)
		fg.mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer sfMock.Close()

	mfCalled := false
	mfMock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mfCalled = true
		w.WriteHeader(200)
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer mfMock.Close()

	cfg.Store(&Config{
		Upstream:           "http://no-default.example.com",
		MaxRetries:         0,
		TotalBudgetSec:     10,
		RecentSampleWindow: 5,
		Routes: []RouteRule{
			{Pattern: "claude-opus*", URL: "http://no-route.example.com", API: "sk-route-xxx", Model: "ark-opus", TextOnly: true, NoSearch: true},
		},
		SearchFallback:     &SearchRoute{URL: sfMock.URL, API: "sk-sf-xxx", Model: "kimi-vl", TextOnly: false}, // 支持图+搜索
		MultimodalFallback: &MultimodalRoute{URL: mfMock.URL, API: "sk-mf-xxx", Model: "kimi-vl"},
	})
	stats.resetSampleCap(5)

	proxy := httptest.NewServer(http.HandlerFunc(handler))
	defer proxy.Close()

	resp, err := http.Post(proxy.URL+"/v1/messages", "application/json",
		strings.NewReader(`{"model":"claude-opus-4-8","tools":[{"type":"web_search_20250305","name":"web_search"}],"messages":[{"role":"user","content":[{"type":"image","source":{"type":"base64","media_type":"image/png","data":"iVBOR"}}]}]}`))
	if err != nil {
		t.Fatalf("请求失败: %v", err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()

	if mfCalled {
		t.Errorf("不应命中 multimodal_fallback（search_fallback 两个能力都满足，应优先）")
	}
	fg.mu.Lock()
	defer fg.mu.Unlock()
	if !fg.called {
		t.Fatalf("应命中 search_fallback")
	}
	if fg.model != "kimi-vl" {
		t.Errorf("search_fallback 收到的 model = %q, want kimi-vl", fg.model)
	}
}

// TestSearchFallbackNotConfigured 端到端：带搜索 + no_search + sf/mf 都没配，
// 降级走原 route 上游。
func TestSearchFallbackNotConfigured(t *testing.T) {
	resetStats()
	var rg struct {
		mu     sync.Mutex
		called bool
	}
	routeMock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rg.mu.Lock()
		rg.called = true
		rg.mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
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
		// 不配 SearchFallback / MultimodalFallback
	})
	stats.resetSampleCap(5)

	proxy := httptest.NewServer(http.HandlerFunc(handler))
	defer proxy.Close()

	resp, err := http.Post(proxy.URL+"/v1/messages", "application/json",
		strings.NewReader(`{"model":"claude-opus-4-8","tools":[{"type":"web_search_20250305","name":"web_search"}],"messages":[{"role":"user","content":"搜一下"}]}`))
	if err != nil {
		t.Fatalf("请求失败: %v", err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()

	rg.mu.Lock()
	defer rg.mu.Unlock()
	if !rg.called {
		t.Fatalf("未配兜底时应降级走原 route 上游")
	}
}

// TestRetryPingKeepalive 端到端：上游前 2 次 429、第 3 次 200。
// 代理应在重试期间发 SSE ping 保活，且最终透传 200 的 message_start 流。
func TestRetryPingKeepalive(t *testing.T) {
	resetStats()
	var mu sync.Mutex
	count := 0
	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		count++
		n := count
		mu.Unlock()
		if n < 3 {
			w.WriteHeader(429)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		fmt.Fprint(w, "data: {\"type\":\"message_start\",\"message\":{\"model\":\"x\",\"usage\":{\"output_tokens\":1}}}\n\ndata: [DONE]\n\n")
	}))
	defer mock.Close()

	cfg.Store(&Config{
		Upstream:           mock.URL,
		RetryStatusCodes:   []int{429, 500, 502, 503, 504},
		MaxRetries:         5,
		BaseDelaySec:       0.1,
		MaxDelaySec:        0.3,
		TotalBudgetSec:     10,
		PingIntervalSec:    0.03,
		RecentSampleWindow: 5,
	})
	stats.resetSampleCap(5)

	proxy := httptest.NewServer(http.HandlerFunc(handler))
	defer proxy.Close()

	resp, err := http.Post(proxy.URL+"/v1/messages", "application/json",
		strings.NewReader(`{"model":"claude-opus-4-8","messages":[{"role":"user","content":"hi"}]}`))
	if err != nil {
		t.Fatalf("请求失败: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()

	if resp.StatusCode != 200 {
		t.Errorf("状态码 = %d, want 200", resp.StatusCode)
	}
	s := string(body)
	if !strings.Contains(s, "event: ping") {
		t.Errorf("重试期间应发 SSE ping 保活, got: %q", s)
	}
	if !strings.Contains(s, "message_start") {
		t.Errorf("重试成功后应透传 message_start, got: %q", s)
	}
}

// TestRetryBackoffCancellable 端到端：上游持续 429 + 长 backoff，
// 客户端中途取消请求，代理应通过 sleepWithPing 的 ctx.Done 快速停止，不睡满 backoff。
func TestRetryBackoffCancellable(t *testing.T) {
	resetStats()
	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(429)
	}))
	defer mock.Close()

	cfg.Store(&Config{
		Upstream:           mock.URL,
		RetryStatusCodes:   []int{429, 500, 502, 503, 504},
		MaxRetries:         50,
		BaseDelaySec:       10, // 长 backoff，确保不靠它结束
		MaxDelaySec:        20,
		TotalBudgetSec:     120,
		PingIntervalSec:    1,
		RecentSampleWindow: 5,
	})
	stats.resetSampleCap(5)

	proxy := httptest.NewServer(http.HandlerFunc(handler))
	defer proxy.Close()

	ctx, cancel := context.WithCancel(context.Background())
	req, _ := http.NewRequestWithContext(ctx, "POST", proxy.URL+"/v1/messages",
		strings.NewReader(`{"model":"x","messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Content-Type", "application/json")

	start := time.Now()
	go func() {
		time.Sleep(200 * time.Millisecond)
		cancel()
	}()
	resp, err := http.DefaultClient.Do(req)
	elapsed := time.Since(start)
	if err == nil {
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
	}
	// 客户端取消后，sleepWithPing 应立即检测到 ctx.Done 返回；不应睡满 10s backoff。
	if elapsed > 2*time.Second {
		t.Errorf("客户端取消后代理耗时 %v，应快速停止(<2s)", elapsed)
	}
}

// TestRetryExhaustedSSEError 端到端：上游持续 429、重试用尽。
// 首次重试已发 200 保活头，用尽时无法透传 429，应改发 SSE error(overloaded_error)。
func TestRetryExhaustedSSEError(t *testing.T) {
	resetStats()
	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(429)
	}))
	defer mock.Close()

	cfg.Store(&Config{
		Upstream:           mock.URL,
		RetryStatusCodes:   []int{429, 500, 502, 503, 504},
		MaxRetries:         1,
		BaseDelaySec:       0.05,
		MaxDelaySec:        0.1,
		TotalBudgetSec:     10,
		PingIntervalSec:    0.02,
		RecentSampleWindow: 5,
	})
	stats.resetSampleCap(5)

	proxy := httptest.NewServer(http.HandlerFunc(handler))
	defer proxy.Close()

	resp, err := http.Post(proxy.URL+"/v1/messages", "application/json",
		strings.NewReader(`{"model":"x","messages":[{"role":"user","content":"hi"}]}`))
	if err != nil {
		t.Fatalf("请求失败: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()

	// 已发 200 保活头，用尽时改发 SSE error，不应是 429。
	if resp.StatusCode != 200 {
		t.Errorf("状态码 = %d, want 200(已发保活头)", resp.StatusCode)
	}
	s := string(body)
	if !strings.Contains(s, "event: error") {
		t.Errorf("用尽应发 SSE error 事件, got: %q", s)
	}
	if !strings.Contains(s, "overloaded_error") {
		t.Errorf("SSE error 应含 overloaded_error, got: %q", s)
	}
}
