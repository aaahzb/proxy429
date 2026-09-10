package main

import (
	"bufio"
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
	stats.cacheCreation = 0
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
	var last, lastIn, lastCR, lastCC int64
	var saw bool
	parseSSEStats([]byte(`data: {"type":"message_start","message":{"usage":{"input_tokens":10,"cache_read_input_tokens":5,"cache_creation_input_tokens":3,"output_tokens":1}}}`), &last, &lastIn, &lastCR, &lastCC, &saw)
	if stats.cacheRead != 5 {
		t.Errorf("cacheRead=%d want 5", stats.cacheRead)
	}
	if stats.cacheCreation != 3 {
		t.Errorf("cacheCreation=%d want 3", stats.cacheCreation)
	}
	if stats.inputTokens != 10 {
		t.Errorf("input=%d want 10", stats.inputTokens)
	}
	if stats.outputTokens != 1 {
		t.Errorf("output=%d want 1", stats.outputTokens)
	}
	if last != 1 || lastCC != 3 {
		t.Errorf("last=%d lastCC=%d want 1/3", last, lastCC)
	}
	if saw {
		t.Error("message_start 不应置 sawDeltaUsage")
	}
}

func TestParseSSEStatsOutputDelta(t *testing.T) {
	resetStats()
	var last, lastIn, lastCR, lastCC int64
	var saw bool
	// 单流：output_tokens 是累积值 1→10→20→50，全局应只记最终 50（增量之和）。
	parseSSEStats([]byte(`data: {"type":"message_start","message":{"usage":{"output_tokens":1}}}`), &last, &lastIn, &lastCR, &lastCC, &saw)
	parseSSEStats([]byte(`data: {"type":"message_delta","usage":{"output_tokens":10}}`), &last, &lastIn, &lastCR, &lastCC, &saw)
	parseSSEStats([]byte(`data: {"type":"message_delta","usage":{"output_tokens":20}}`), &last, &lastIn, &lastCR, &lastCC, &saw)
	parseSSEStats([]byte(`data: {"type":"message_delta","usage":{"output_tokens":50}}`), &last, &lastIn, &lastCR, &lastCC, &saw)
	if stats.outputTokens != 50 {
		t.Errorf("output=%d want 50", stats.outputTokens)
	}
}

func TestParseSSEStatsMultiStream(t *testing.T) {
	resetStats()
	var lastA, lastB, inA, inB, crA, crB, ccA, ccB int64
	var sawA, sawB bool
	// 两个并发流各自独立 lastOutput，全局累加两者当前值：100+30=130。
	parseSSEStats([]byte(`data: {"type":"message_delta","usage":{"output_tokens":100}}`), &lastA, &inA, &crA, &ccA, &sawA)
	parseSSEStats([]byte(`data: {"type":"message_delta","usage":{"output_tokens":30}}`), &lastB, &inB, &crB, &ccB, &sawB)
	if stats.outputTokens != 130 {
		t.Errorf("output=%d want 130", stats.outputTokens)
	}
}

func TestParseSSEStatsIgnoresNonData(t *testing.T) {
	resetStats()
	var last, lastIn, lastCR, lastCC int64
	var saw bool
	parseSSEStats([]byte(`event: content_block_delta`), &last, &lastIn, &lastCR, &lastCC, &saw)
	parseSSEStats([]byte(`data: [DONE]`), &last, &lastIn, &lastCR, &lastCC, &saw)
	parseSSEStats([]byte(``), &last, &lastIn, &lastCR, &lastCC, &saw)
	parseSSEStats([]byte(`data: {"type":"content_block_delta","delta":{"text":"x"}}`), &last, &lastIn, &lastCR, &lastCC, &saw)
	if stats.outputTokens != 0 || stats.cacheRead != 0 {
		t.Errorf("非 usage 事件不应计数: output=%d cacheRead=%d", stats.outputTokens, stats.cacheRead)
	}
}

// 回归：message_start 与 message_delta 都带 input_tokens / cache_read 时，
// 取最后出现的值（后值覆盖前值），不累加。Kimi 实测：start=45814, delta=1270。
func TestParseSSEStatsUsageNoDoubleCount(t *testing.T) {
	resetStats()
	var last, lastIn, lastCR, lastCC int64
	var saw bool
	parseSSEStats([]byte(`data: {"type":"message_start","message":{"usage":{"input_tokens":45814,"cache_read_input_tokens":9000,"output_tokens":1}}}`), &last, &lastIn, &lastCR, &lastCC, &saw)
	parseSSEStats([]byte(`data: {"type":"message_delta","usage":{"input_tokens":1270,"cache_read_input_tokens":200,"output_tokens":50}}`), &last, &lastIn, &lastCR, &lastCC, &saw)
	if stats.inputTokens != 1270 {
		t.Errorf("input=%d want 1270 (取最后值，非 max)", stats.inputTokens)
	}
	if stats.cacheRead != 200 {
		t.Errorf("cacheRead=%d want 200 (取最后值)", stats.cacheRead)
	}
	if stats.outputTokens != 50 {
		t.Errorf("output=%d want 50", stats.outputTokens)
	}
	// last 也应是最后值
	if lastIn != 1270 || lastCR != 200 || last != 50 {
		t.Errorf("last=%d/%d/%d want 1270/200/50", lastIn, lastCR, last)
	}
}

// 字段缺失时保持原值：message_delta 不含 input_tokens 时不应覆盖为 0。
func TestParseSSEStatsUsageMissingField(t *testing.T) {
	resetStats()
	var last, lastIn, lastCR, lastCC int64
	var saw bool
	parseSSEStats([]byte(`data: {"type":"message_start","message":{"usage":{"input_tokens":100,"cache_read_input_tokens":50,"output_tokens":1}}}`), &last, &lastIn, &lastCR, &lastCC, &saw)
	// message_delta 只带 output_tokens，input/cache_read 缺失，应保持 100/50
	parseSSEStats([]byte(`data: {"type":"message_delta","usage":{"output_tokens":80}}`), &last, &lastIn, &lastCR, &lastCC, &saw)
	if stats.inputTokens != 100 {
		t.Errorf("input=%d want 100 (缺失字段不应覆盖)", stats.inputTokens)
	}
	if stats.cacheRead != 50 {
		t.Errorf("cacheRead=%d want 50 (缺失字段不应覆盖)", stats.cacheRead)
	}
	if stats.outputTokens != 80 {
		t.Errorf("output=%d want 80", stats.outputTokens)
	}
	if !saw {
		t.Error("带 usage 的 message_delta 应置 sawDeltaUsage")
	}
}

// sawDeltaUsage 标记：带 usage 的 message_delta 才置位（真实用量拆分到达），
// message_start / 其他事件不置位。聚合只统计完整响应靠这个标记区分。
func TestParseSSEStatsDeltaUsageMarker(t *testing.T) {
	resetStats()
	var last, lastIn, lastCR, lastCC int64
	var saw bool
	parseSSEStats([]byte(`data: {"type":"message_start","message":{"usage":{"input_tokens":2899,"cache_read_input_tokens":0,"output_tokens":1}}}`), &last, &lastIn, &lastCR, &lastCC, &saw)
	if saw {
		t.Error("message_start 不应置 sawDeltaUsage")
	}
	parseSSEStats([]byte(`data: {"type":"message_delta","usage":{"input_tokens":0,"cache_read_input_tokens":2899,"output_tokens":16}}`), &last, &lastIn, &lastCR, &lastCC, &saw)
	if !saw {
		t.Error("带 usage 的 message_delta 应置 sawDeltaUsage")
	}
}

// 中断流回滚：只收到 message_start 就断流时（Kimi 实测 start.input 含 cache_read、start.cr=0），
// 已计入全局的预估值必须能精确回滚，否则整个上下文被算成未命中输入、聚合命中率被拉低。
func TestRollbackUsageStats(t *testing.T) {
	resetStats()
	var last, lastIn, lastCR, lastCC int64
	var saw bool
	parseSSEStats([]byte(`data: {"type":"message_start","message":{"usage":{"input_tokens":2899,"cache_read_input_tokens":0,"cache_creation_input_tokens":0,"output_tokens":1}}}`), &last, &lastIn, &lastCR, &lastCC, &saw)
	if stats.inputTokens != 2899 || stats.outputTokens != 1 {
		t.Fatalf("中断前: in=%d out=%d want 2899/1", stats.inputTokens, stats.outputTokens)
	}
	rollbackUsageStats(lastIn, lastCR, lastCC, last)
	if stats.inputTokens != 0 || stats.cacheRead != 0 || stats.cacheCreation != 0 || stats.outputTokens != 0 {
		t.Errorf("回滚后应全零: in=%d cr=%d cc=%d out=%d", stats.inputTokens, stats.cacheRead, stats.cacheCreation, stats.outputTokens)
	}
}

func TestParseNonStreamUsage(t *testing.T) {
	// Anthropic 非流式（分类器响应）
	in, cr, cc, out, ok := parseNonStreamUsage([]byte(`{"type":"message","content":[{"type":"text","text":"x"}],"usage":{"input_tokens":143,"cache_read_input_tokens":61568,"cache_creation_input_tokens":7,"output_tokens":5}}`))
	if !ok || in != 143 || cr != 61568 || cc != 7 || out != 5 {
		t.Errorf("anthropic: ok=%v in=%d cr=%d cc=%d out=%d want 143/61568/7/5", ok, in, cr, cc, out)
	}
	// OpenAI 风格（prompt_tokens/completion_tokens，无 cache_*）
	in, cr, cc, out, ok = parseNonStreamUsage([]byte(`{"choices":[],"usage":{"prompt_tokens":100,"completion_tokens":50}}`))
	if !ok || in != 100 || cr != 0 || cc != 0 || out != 50 {
		t.Errorf("openai: ok=%v in=%d cr=%d cc=%d out=%d want 100/0/0/50", ok, in, cr, cc, out)
	}
	// 无 usage 字段
	_, _, _, _, ok = parseNonStreamUsage([]byte(`{"foo":"bar"}`))
	if ok {
		t.Errorf("no usage: should return ok=false")
	}
	// 非 JSON
	_, _, _, _, ok = parseNonStreamUsage([]byte(`not json`))
	if ok {
		t.Errorf("non-json: should return ok=false")
	}
}

func TestAddModelUsage(t *testing.T) {
	stats.mu.Lock()
	stats.modelStats = nil
	stats.mu.Unlock()
	stats.addModelUsage("deepseek-v4", 100, 200, 20, 50)
	stats.addModelUsage("kimi", 10, 500, 0, 5)
	stats.addModelUsage("deepseek-v4", 50, 100, 10, 30)
	// 空模型名 / 全零应被忽略
	stats.addModelUsage("", 1, 2, 3, 4)
	stats.addModelUsage("zero", 0, 0, 0, 0)
	got := stats.snapshotModelStats()
	if len(got) != 2 {
		t.Fatalf("want 2 models, got %d: %+v", len(got), got)
	}
	// 按 total 降序：deepseek-v4=150+300+30+80=560, kimi=10+500+0+5=515
	if got[0].Model != "deepseek-v4" {
		t.Errorf("first should be deepseek-v4 (total 560), got %s", got[0].Model)
	}
	ds := got[0]
	if ds.Input != 150 || ds.CacheRead != 300 || ds.CacheCreation != 30 || ds.Output != 80 {
		t.Errorf("deepseek-v4累加错: in=%d cr=%d cc=%d out=%d want 150/300/30/80", ds.Input, ds.CacheRead, ds.CacheCreation, ds.Output)
	}
	// 清理
	stats.mu.Lock()
	stats.modelStats = nil
	stats.mu.Unlock()
}

// cacheHitRate 与 Claude Code 的 cache hit 算法一致：
// cache_read / (input + cache_read + cache_creation)。
func TestCacheHitRate(t *testing.T) {
	cases := []struct {
		cr, in, cc int64
		want       string
	}{
		{50, 40, 10, "50.0%"},  // 50/(40+50+10)=50%
		{5, 10, 3, "27.8%"},    // 5/18≈27.78%
		{0, 0, 0, "-"},         // 无 usage 数据
		{0, 0, 7, "0.0%"},      // 只有写入没有命中
		{9000, 45814, 0, "16.4%"}, // Kimi 实测值量级：9000/54814
	}
	for _, c := range cases {
		if got := cacheHitRate(c.cr, c.in, c.cc); got != c.want {
			t.Errorf("cacheHitRate(%d,%d,%d)=%q want %q", c.cr, c.in, c.cc, got, c.want)
		}
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

// TestCountTokensFlightTag 验证 /v1/messages/count_tokens 探针流：原样透传上游响应，
// 完成流归档带 countTokens 标记（网页 model 列据此显示 [count_tokens] 前缀），
// 且响应体（顶层 input_tokens，无 usage 键、无 data: 行）不污染聚合统计。
func TestCountTokensFlightTag(t *testing.T) {
	resetStats()
	// mock 上游：count_tokens 的真实响应形态，只有一个顶层 input_tokens 字段。
	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"input_tokens":42}`)
	}))
	defer mock.Close()

	cfg.Store(&Config{Upstream: mock.URL, MaxRetries: 0, TotalBudgetSec: 10})

	proxy := httptest.NewServer(http.HandlerFunc(handler))
	defer proxy.Close()

	resp, err := http.Post(proxy.URL+countTokensPath, "application/json",
		strings.NewReader(`{"model":"x","messages":[{"role":"user","content":"hi"}]}`))
	if err != nil {
		t.Fatalf("请求失败: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("状态码 = %d（应为 200），响应: %s", resp.StatusCode, body)
	}
	if string(body) != `{"input_tokens":42}` {
		t.Errorf("响应体 = %q（应原样透传上游）", body)
	}

	// handler defer 在响应 EOF 前完成归档，此处完成流列表必含本流。
	finishedMu.Lock()
	if len(finished) == 0 {
		finishedMu.Unlock()
		t.Fatal("完成流列表为空（handler defer 应已归档本流）")
	}
	ff := finished[len(finished)-1]
	finishedMu.Unlock()
	if !ff.countTokens {
		t.Error("完成流缺 countTokens 标记（网页将无法区分探针与空响应）")
	}
	if !strings.Contains(string(ff.content), `"input_tokens":42`) {
		t.Errorf("完成流内容 = %q（应含 tee 下来的上游原文）", ff.content)
	}

	// 统计免疫：input/output 均应保持 0（parseSSEStats 要 data: 前缀，
	// parseNonStreamUsage 要顶层 usage 键，count_tokens 响应两者皆无）。
	stats.mu.Lock()
	in, out := stats.inputTokens, stats.outputTokens
	stats.mu.Unlock()
	if in != 0 || out != 0 {
		t.Errorf("count_tokens 响应污染了统计: input=%d output=%d（应均为 0）", in, out)
	}
}

// TestAddFinishedTotalMs 验证完成流归档的总耗时列：totalMs 从 flight 建立
// （代理收到下游请求）算到归档（handler 返回、响应已全部发回下游），且与 ended 同源。
func TestAddFinishedTotalMs(t *testing.T) {
	f := &flight{id: flights.nextID.Add(1), start: time.Now().Add(-123 * time.Millisecond)}
	addFinished(f)
	finishedMu.Lock()
	ff := finished[len(finished)-1]
	finishedMu.Unlock()
	if ff.id != f.id {
		t.Fatalf("归档末位 id=%d, want %d", ff.id, f.id)
	}
	if ff.totalMs < 123 {
		t.Errorf("totalMs=%d, want ≥123（应为 flight 建立到归档的耗时）", ff.totalMs)
	}
	if ff.ended.Before(f.start) {
		t.Errorf("ended=%v 不应早于 start=%v", ff.ended, f.start)
	}
}

// TestFlightAttemptMs 验证当前尝试计时：attemptStart=0（重试退避中）返回 -1；已发出则返回已等待毫秒数。
func TestFlightAttemptMs(t *testing.T) {
	f := &flight{}
	if got := f.attemptMs(); got != -1 {
		t.Fatalf("attemptStart=0 应返回 -1（退避中），got %d", got)
	}
	f.attemptStart.Store(time.Now().Add(-1500 * time.Millisecond).UnixNano())
	if got := f.attemptMs(); got < 1400 || got > 5000 {
		t.Fatalf("attemptMs 应约 1500ms，got %d", got)
	}
}

// TestHasCatchAllRoute 验证 pattern:"*" 兜底判定（有兜底时顶层 upstream 允许留空）。
func TestHasCatchAllRoute(t *testing.T) {
	if hasCatchAllRoute(nil) {
		t.Errorf("空 routes 不应判定有兜底")
	}
	if hasCatchAllRoute([]RouteRule{{Pattern: "claude-*"}}) {
		t.Errorf("普通通配 claude-* 不应判定有兜底")
	}
	if !hasCatchAllRoute([]RouteRule{{Pattern: "claude-*"}, {Pattern: "*"}}) {
		t.Errorf("含 pattern:\"*\" 应判定有兜底")
	}
}

// TestClearStatsFinished 验证「清空统计」一并清空最近完成的流列表（在途流与流编号不动）。
func TestClearStatsFinished(t *testing.T) {
	finishedMu.Lock()
	finished = append(finished, finishedFlight{id: 999999})
	finishedMu.Unlock()
	clearStats(&Config{RecentSampleWindow: 5})
	finishedMu.Lock()
	n := len(finished)
	finishedMu.Unlock()
	if n != 0 {
		t.Errorf("clearStats 后 finished 剩 %d 条, want 0", n)
	}
}

// TestComputeRateCounterReset 验证清空统计后累计字节回零，速率不算出负值。
func TestComputeRateCounterReset(t *testing.T) {
	rateMu.Lock()
	prevRateBytes, prevRateT, lastRate = 1000, time.Now().Add(-time.Second), 500
	rateMu.Unlock()
	if got := computeRate(100); got != 0 {
		t.Errorf("计数器回零后 rate=%d, want 0（负增量应被钳位）", got)
	}
	rateMu.Lock()
	prevRateBytes, prevRateT, lastRate = 0, time.Time{}, 0
	rateMu.Unlock()
}

// TestMarkClientGone499 验证归档状态码按上游口径校正：499 只问"上游有没有发完"
// （delivered），与上游提供商后台口径一致；本地错误与非 200 状态不受影响。
func TestMarkClientGone499(t *testing.T) {
	mark := func(status int, delivered, cancelCtx bool) int {
		f := &flight{id: flights.nextID.Add(1), start: time.Now(), status: status}
		if delivered {
			f.delivered.Store(true)
		}
		ctx := context.Background()
		if cancelCtx {
			c, cancel := context.WithCancel(context.Background())
			cancel() // 模拟下游断开（handler 收尾 defer 时 ctx 已取消）
			ctx = c
		}
		markClientGone(f, ctx)
		addFinished(f)
		finishedMu.Lock()
		got := finished[len(finished)-1].status
		finishedMu.Unlock()
		return got
	}

	if got := mark(200, false, true); got != 499 {
		t.Errorf("上游流没发完、下游断开 status=%d, want 499", got)
	}
	if got := mark(200, false, false); got != 499 {
		t.Errorf("上游流没发完就结束（重试用尽等，非下游原因）status=%d, want 499", got)
	}
	if got := mark(200, true, true); got != 200 {
		t.Errorf("上游发完后下游才断开（Codex 收完即关连接）status=%d, want 200", got)
	}
	if got := mark(200, true, false); got != 200 {
		t.Errorf("完全正常 status=%d, want 200", got)
	}
	if got := mark(0, false, true); got != 499 {
		t.Errorf("等上游响应期间下游取消 status=%d, want 499（请求已发到上游被中止）", got)
	}
	if got := mark(0, false, false); got != 0 {
		t.Errorf("本地错误（未触达上游）status=%d, want 0（不受 499 校正影响）", got)
	}
	if got := mark(429, false, true); got != 429 {
		t.Errorf("非 200 上游状态 status=%d, want 429（不动）", got)
	}
}

// TestForwardDeliveredOnMessageStop 验证透传的完整送达标记：读到 message_stop 即标记
// delivered（客户端收完即断连时上游 EOF 往往读不到，不能等 EOF）；上游流中途出错不标记。
func TestForwardDeliveredOnMessageStop(t *testing.T) {
	resetStats()
	sseFull := "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"usage\":{\"output_tokens\":1}}}\n\n" +
		"event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":2}}\n\n" +
		"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"

	run := func(body io.Reader) *flight {
		f := &flight{id: flights.nextID.Add(1), start: time.Now()}
		resp := &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(body)}
		rec := httptest.NewRecorder()
		forward(rec, resp, nil, bufio.NewReader(resp.Body), f, false)
		return f
	}

	if f := run(strings.NewReader(sseFull)); !f.delivered.Load() {
		t.Error("含 message_stop 的完整流应标记 delivered")
	}
	// 流中途断开（读到非 EOF 错误）：不标记（收尾 defer 会把这类流改记 499）。
	if f := run(&errAfterReader{b: []byte("event: message_start\ndata: {\"type\":\"message_start\"}\n\n"), err: io.ErrUnexpectedEOF}); f.delivered.Load() {
		t.Error("上游流中途出错不应标记 delivered")
	}
}

// errAfterReader 把 b 读完后返回指定错误，模拟上游流中途断开（非干净 EOF）。
type errAfterReader struct {
	b   []byte
	err error
}

func (r *errAfterReader) Read(p []byte) (int, error) {
	if len(r.b) == 0 {
		return 0, r.err
	}
	n := copy(p, r.b)
	r.b = r.b[n:]
	return n, nil
}

// TestParseToolCallName 锁定 SSE 行工具名提取：tool_use/server_tool_use 的 content_block_start
// 返回名字；text 块、web_search_tool_result（含 tool_use_id 子串但块类型不是调用）、
// 非 data 行、[DONE]、坏 JSON 都返回空。
func TestParseToolCallName(t *testing.T) {
	cases := []struct{ line, want string }{
		{`data: {"type":"content_block_start","index":1,"content_block":{"type":"tool_use","id":"t1","name":"Read","input":{}}}`, "Read"},
		{`data: {"type":"content_block_start","index":0,"content_block":{"type":"server_tool_use","id":"s1","name":"web_search","input":{}}}`, "web_search"},
		{`data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`, ""},
		{`data: {"type":"content_block_start","index":2,"content_block":{"type":"web_search_tool_result","tool_use_id":"s1","content":[]}}`, ""},
		{`data: {"type":"message_stop"}`, ""},
		{`data: [DONE]`, ""},
		{`event: content_block_start`, ""},
		{`data: {garbage content_block_start tool_use`, ""},
		{"", ""},
	}
	for _, c := range cases {
		if got := parseToolCallName([]byte(c.line)); got != c.want {
			t.Errorf("parseToolCallName(%q)=%q, want %q", c.line, got, c.want)
		}
	}
}

// TestParseToolEmptySignals 锁定 *0 空参判定的各解析器：start 带 index/input、
// input_json_delta 碎片、content_block_stop 的 index、Responses output_item.done
// 三类项的终态空参（function_call 看 arguments、custom_tool_call 看 input、
// web_search_call 看 action 的 query+sources）。
func TestParseToolEmptySignals(t *testing.T) {
	// start：index 与 start 自带 input 都要拿到（server_tool_use 完整块直接定论非空）
	ts, ok := parseToolCallStart([]byte(`data: {"type":"content_block_start","index":3,"content_block":{"type":"server_tool_use","id":"s1","name":"web_search","input":{"query":"q"}}}`))
	if !ok || ts.index != 3 || ts.name != "web_search" || isEmptyArgsJSON(string(ts.input)) {
		t.Errorf("parseToolCallStart 非空 input=%+v ok=%v", ts, ok)
	}
	// delta：只有 input_json_delta 才认，text_delta 不认
	idx, partial, ok := parseToolArgsDelta([]byte(`data: {"type":"content_block_delta","index":2,"delta":{"type":"input_json_delta","partial_json":"{\"a\":"}}`))
	if !ok || idx != 2 || partial != `{"a":` {
		t.Errorf("parseToolArgsDelta=(%d,%q,%v)", idx, partial, ok)
	}
	if _, _, ok := parseToolArgsDelta([]byte(`data: {"type":"content_block_delta","index":2,"delta":{"type":"text_delta","text":"x"}}`)); ok {
		t.Error("text_delta 不应认作参数 delta")
	}
	// stop：只要 index
	if idx, ok := parseToolBlockStop([]byte(`data: {"type":"content_block_stop","index":2}`)); !ok || idx != 2 {
		t.Errorf("parseToolBlockStop=(%d,%v)", idx, ok)
	}
	// Responses done：三类项的终态空参
	doneCases := []struct {
		line  string
		id    string
		empty bool
		ok    bool
	}{
		{`data: {"type":"response.output_item.done","item":{"id":"f1","type":"function_call","name":"Read","arguments":"{\"file_path\":\"/a\"}"}}`, "f1", false, true},
		{`data: {"type":"response.output_item.done","item":{"id":"f2","type":"function_call","name":"Ls","arguments":"{}"}}`, "f2", true, true},
		{`data: {"type":"response.output_item.done","item":{"id":"f3","type":"function_call","name":"Ls","arguments":""}}`, "f3", true, true},
		{`data: {"type":"response.output_item.done","item":{"id":"c1","type":"custom_tool_call","name":"apply_patch","input":"*** Patch"}}`, "c1", false, true},
		{`data: {"type":"response.output_item.done","item":{"id":"w1","type":"web_search_call","action":{"type":"search","query":"q","sources":[{"url":"https://a.cn"}]}}}`, "w1", false, true},
		{`data: {"type":"response.output_item.done","item":{"id":"w2","type":"web_search_call","action":{"type":"search"}}}`, "w2", true, true},
		{`data: {"type":"response.output_item.done","item":{"id":"m1","type":"message"}}`, "", false, false},
		{`data: {"type":"response.output_item.added","item":{"id":"f1","type":"function_call","name":"Read"}}`, "", false, false},
	}
	for _, c := range doneCases {
		id, empty, ok := parseResponsesToolDone([]byte(c.line))
		if id != c.id || empty != c.empty || ok != c.ok {
			t.Errorf("parseResponsesToolDone(%q)=(%q,%v,%v), want (%q,%v,%v)", c.line, id, empty, ok, c.id, c.empty, c.ok)
		}
	}
	// isEmptyArgsJSON 边界
	for _, s := range []string{"", "{}", " { } ", "null", "\n\t"} {
		if !isEmptyArgsJSON(s) {
			t.Errorf("isEmptyArgsJSON(%q) 应为 true", s)
		}
	}
	for _, s := range []string{`{"a":1}`, `"{}"`, `["x"]`} {
		if isEmptyArgsJSON(s) {
			t.Errorf("isEmptyArgsJSON(%q) 应为 false", s)
		}
	}
}

// TestForwardToolCallCount 端到端透传路径：tool_use/server_tool_use 的 content_block_start
// 各计一次，同名重复合并计数；toolCallsTag 按首次出现顺序输出，单次调用显 *1（参数
// 经 input_json_delta 到达）、参数结构体为空的单次调用显 *0、多次调用显原始 *N。
func TestForwardToolCallCount(t *testing.T) {
	resetStats()
	sse := "data: {\"type\":\"message_start\",\"message\":{\"usage\":{\"output_tokens\":1}}}\n\n" +
		"data: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"tool_use\",\"id\":\"t1\",\"name\":\"Read\",\"input\":{}}}\n\n" +
		"data: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"input_json_delta\",\"partial_json\":\"{\\\"file_path\\\":\\\"/a\\\"}\"}}\n\n" +
		"data: {\"type\":\"content_block_stop\",\"index\":0}\n\n" +
		"data: {\"type\":\"content_block_start\",\"index\":1,\"content_block\":{\"type\":\"tool_use\",\"id\":\"t2\",\"name\":\"Edit\",\"input\":{}}}\n\n" +
		"data: {\"type\":\"content_block_stop\",\"index\":1}\n\n" +
		"data: {\"type\":\"content_block_start\",\"index\":2,\"content_block\":{\"type\":\"tool_use\",\"id\":\"t3\",\"name\":\"Edit\",\"input\":{}}}\n\n" +
		"data: {\"type\":\"content_block_stop\",\"index\":2}\n\n" +
		"data: {\"type\":\"content_block_start\",\"index\":3,\"content_block\":{\"type\":\"server_tool_use\",\"id\":\"s1\",\"name\":\"web_search\",\"input\":{}}}\n\n" +
		"data: {\"type\":\"message_stop\"}\n\n"
	f := &flight{id: flights.nextID.Add(1), start: time.Now()}
	resp := &http.Response{StatusCode: 200, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(sse))}
	rec := httptest.NewRecorder()
	forward(rec, resp, nil, bufio.NewReader(resp.Body), f, false)
	// Read 单次带参→*1；Edit 两次空参仍显原始次数 *2；web_search 单次空参→*0
	// （index 3 无 content_block_stop：流收尾时对未关闭调用按已收内容定论空参）。
	if got, want := f.toolCallsTag(), "[Read*1][Edit*2][web_search*0]"; got != want {
		t.Errorf("toolCallsTag=%q, want %q", got, want)
	}
}

// TestToolCallsTag 锁格式化边界：无调用空串、空名忽略、同名计数合并且保首次出现顺序；
// 单次调用显 *1、单次空参显 *0、多次调用显原始 *N（空参标记不影响）。
func TestToolCallsTag(t *testing.T) {
	f := &flight{}
	if got := f.toolCallsTag(); got != "" {
		t.Errorf("无工具调用 want 空串, got %q", got)
	}
	f.noteToolCall("") // 空名忽略
	f.noteToolCall("Bash")
	f.noteToolCall("Read")
	f.noteToolCall("Bash")
	f.noteToolCall("Bash")
	if got, want := f.toolCallsTag(), "[Bash*3][Read*1]"; got != want {
		t.Errorf("toolCallsTag=%q, want %q", got, want)
	}
	f.noteToolCallEmpty("Read")   // 单次调用空参 → *0
	f.noteToolCallEmpty("Bash")   // 多次调用显原始 *N，空参标记不影响
	f.noteToolCallEmpty("Nobody") // 未计数的工具：忽略
	if got, want := f.toolCallsTag(), "[Bash*3][Read*0]"; got != want {
		t.Errorf("toolCallsTag=%q, want %q", got, want)
	}
}

// setCfg 设置测试用配置（maybeRewriteClassifier 读全局 cfg）。
func setCfg(thinkingDisabled bool, maxTokens int) {
	cfg.Store(&Config{
		ClassifierThinkingDisabled: thinkingDisabled,
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

// TestClassifierHitCounting 端到端验证分类器双计数口径：classifierHits 命中即计
// （无论是否分流/关思考），classifierRewrites 只在实际改写 body 关 thinking 时 +1。
func TestClassifierHitCounting(t *testing.T) {
	resetStats()
	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"id":"m1","usage":{"input_tokens":10,"output_tokens":1}}`)
	}))
	defer mock.Close()
	proxy := httptest.NewServer(http.HandlerFunc(handler))
	defer proxy.Close()

	post := func(body string) {
		resp, err := http.Post(proxy.URL+"/v1/messages", "application/json", strings.NewReader(body))
		if err != nil {
			t.Fatalf("请求失败: %v", err)
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
	}
	// 带 thinking 的分类器请求：关思考开关打开时必然产生真实改写（字节变化才计改写数）。
	cls := `{"model":"x","system":"You are a security monitor.","thinking":{"type":"enabled"},"max_tokens":100,"messages":[{"role":"user","content":"hi"}]}`

	cfg.Store(&Config{Upstream: mock.URL, MaxRetries: 0, TotalBudgetSec: 10, ClassifierThinkingDisabled: false})
	post(cls)
	post(cls)
	if h, rw := stats.classifierHits.Load(), stats.classifierRewrites.Load(); h != 2 || rw != 0 {
		t.Errorf("关思考关闭时：hits=%d want 2, rewrites=%d want 0", h, rw)
	}

	cfg.Store(&Config{Upstream: mock.URL, MaxRetries: 0, TotalBudgetSec: 10, ClassifierThinkingDisabled: true})
	post(cls)
	if h, rw := stats.classifierHits.Load(), stats.classifierRewrites.Load(); h != 3 || rw != 1 {
		t.Errorf("关思考开启后：hits=%d want 3, rewrites=%d want 1", h, rw)
	}

	post(`{"model":"x","messages":[{"role":"user","content":"hi"}]}`) // 非分类器请求：两计数都不涨
	if h, rw := stats.classifierHits.Load(), stats.classifierRewrites.Load(); h != 3 || rw != 1 {
		t.Errorf("普通请求后：hits=%d want 3, rewrites=%d want 1", h, rw)
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

// TestReservedRoutePattern 验证保留名规则：pattern 全字撞保留名（"Fallback"=Codex 菜单 * 兜底、
// "fast_route"=fast 通道）的路由不生效——撞名请求由后面的合法路由接；Fall* 之类通配不受影响。
func TestReservedRoutePattern(t *testing.T) {
	resetStats()
	var mu sync.Mutex
	hit := map[string]bool{}
	mk := func(name string) *httptest.Server {
		return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			mu.Lock()
			hit[name] = true
			mu.Unlock()
			w.WriteHeader(200)
			fmt.Fprint(w, "data: {\"type\":\"message_start\",\"message\":{\"usage\":{\"output_tokens\":1}}}\n\ndata: [DONE]\n\n")
		}))
	}
	srvFB := mk("fb")     // pattern 全字 Fallback（保留名，不生效）
	srvFR := mk("fr")     // pattern 全字 fast_route（保留名，不生效）
	srvWild := mk("wild") // Fall* 合法通配
	srvAll := mk("all")   // * 兜底
	defer srvFB.Close()
	defer srvFR.Close()
	defer srvWild.Close()
	defer srvAll.Close()

	cfg.Store(&Config{
		MaxRetries: 0, TotalBudgetSec: 10, RecentSampleWindow: 5,
		Routes: []RouteRule{
			{Pattern: "Fallback", URL: srvFB.URL},
			{Pattern: "fast_route", URL: srvFR.URL},
			{Pattern: "Fall*", URL: srvWild.URL},
			{Pattern: "*", URL: srvAll.URL},
		},
	})
	defer cfg.Store(&Config{})
	stats.resetSampleCap(5)

	proxy := httptest.NewServer(http.HandlerFunc(handler))
	defer proxy.Close()

	post := func(model string) {
		mu.Lock()
		hit = map[string]bool{}
		mu.Unlock()
		resp, err := http.Post(proxy.URL+"/v1/messages", "application/json",
			strings.NewReader(`{"model":"`+model+`","messages":[{"role":"user","content":"hi"}]}`))
		if err != nil {
			t.Fatalf("model=%s 请求失败: %v", model, err)
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		if resp.StatusCode != 200 {
			t.Fatalf("model=%s code=%d, want 200", model, resp.StatusCode)
		}
	}
	check := func(model string, want map[string]bool) {
		mu.Lock()
		got := hit
		mu.Unlock()
		for _, name := range []string{"fb", "fr", "wild", "all"} {
			if got[name] != want[name] {
				t.Errorf("model=%s %s 命中=%v, want %v（全量 hit=%v）", model, name, got[name], want[name], got)
			}
		}
	}

	// 跳过保留名路由，由 Fall* 通配接
	post("Fallback")
	check("Fallback", map[string]bool{"wild": true})

	// 保留名路由不生效（请求无 speed 字段，fast 分支不触发），由 * 兜底接
	post("fast_route")
	check("fast_route", map[string]bool{"all": true})

	// 保留名 pattern 不当通配用，* 兜底接
	post("claude-x")
	check("claude-x", map[string]bool{"all": true})
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
		Upstream:           defaultMock.URL,
		MaxRetries:         0,
		TotalBudgetSec:     10,
		RecentSampleWindow: 5,
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
		Upstream:           defaultMock.URL,
		MaxRetries:         0,
		TotalBudgetSec:     10,
		RecentSampleWindow: 5,
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
		Upstream:           defaultMock.URL,
		MaxRetries:         0,
		TotalBudgetSec:     10,
		RecentSampleWindow: 5,
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
		Upstream:           defaultMock.URL,
		MaxRetries:         0,
		TotalBudgetSec:     10,
		RecentSampleWindow: 5,
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
		Upstream:           "http://no-default-should-not-hit.example.com",
		MaxRetries:         0,
		TotalBudgetSec:     10,
		RecentSampleWindow: 5,
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
		Upstream:           "http://no-default-should-not-hit.example.com",
		MaxRetries:         0,
		TotalBudgetSec:     10,
		RecentSampleWindow: 5,
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

// TestEnhanceSearchRoute 端到端：route.EnhanceSearch 非 nil + 带搜索工具 + no_search:false，
// 应走增强搜索（searchAndRespond 用 route 的 url/api/model），不调主力默认 upstream。
func TestEnhanceSearchRoute(t *testing.T) {
	resetStats()
	var rg struct {
		mu     sync.Mutex
		called bool
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/messages", func(w http.ResponseWriter, r *http.Request) {
		rg.mu.Lock()
		rg.called = true
		rg.mu.Unlock()
		var m map[string]any
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &m)
		stream, _ := m["stream"].(bool)
		if !stream {
			// step1：非流式 JSON，返回 server_tool_use + web_search_tool_result。
			resp := map[string]any{
				"id":          "msg_step1",
				"model":       "deepseek-V4-pro",
				"stop_reason": "end_turn",
				"content": []map[string]any{
					{"type": "server_tool_use", "id": "srvtoolu_enh", "name": "web_search", "input": map[string]any{}},
					{"type": "web_search_tool_result", "tool_use_id": "srvtoolu_enh", "content": []map[string]any{
						{"type": "web_search_tool_result_content", "url": "https://example.com/1", "title": "Enh Result"},
					}},
				},
			}
			b, _ := json.Marshal(resp)
			w.Header().Set("content-type", "application/json")
			w.Write(b)
			return
		}
		// step2：流式摘要。
		w.Header().Set("content-type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		fmt.Fprintf(w, "event: message_start\ndata: %s\n\n", `{"type":"message_start","message":{"model":"deepseek-V4-pro"}}`)
		fmt.Fprintf(w, "event: content_block_start\ndata: %s\n\n", `{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`)
		fmt.Fprintf(w, "event: content_block_delta\ndata: %s\n\n", `{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"Enh Result: summary"}}`)
		fmt.Fprintf(w, "event: content_block_stop\ndata: %s\n\n", `{"type":"content_block_stop","index":0}`)
		fmt.Fprintf(w, "event: message_delta\ndata: %s\n\n", `{"type":"message_delta","delta":{"stop_reason":"end_turn"}}`)
		fmt.Fprintf(w, "event: message_stop\ndata: %s\n\n", `{"type":"message_stop"}`)
	})
	routeMock := httptest.NewServer(mux)
	defer routeMock.Close()

	cfg.Store(&Config{
		Upstream:           "http://no-default.example.com",
		MaxRetries:         0,
		TotalBudgetSec:     10,
		RecentSampleWindow: 5,
		Routes: []RouteRule{
			{Pattern: "claude-haiku*", URL: routeMock.URL, API: "sk-route-xxx", Model: "deepseek-V4-pro", EnhanceSearch: &EnhanceSearchConfig{SummaryLevel: "mid"}},
		},
	})
	defer cfg.Store(&Config{})
	stats.resetSampleCap(5)

	proxy := httptest.NewServer(http.HandlerFunc(handler))
	defer proxy.Close()

	resp, err := http.Post(proxy.URL+"/v1/messages", "application/json",
		strings.NewReader(`{"model":"claude-haiku-4-5","stream":true,"tools":[{"type":"web_search_20250305","name":"web_search"}],"messages":[{"role":"user","content":"搜一下"}]}`))
	if err != nil {
		t.Fatalf("请求失败: %v", err)
	}
	out, _ := io.ReadAll(resp.Body)
	resp.Body.Close()

	rg.mu.Lock()
	defer rg.mu.Unlock()
	if !rg.called {
		t.Fatalf("应命中 route 上游做 step1/step2（增强搜索）")
	}
	s := string(out)
	if !strings.Contains(s, "server_tool_use") {
		t.Errorf("响应缺 server_tool_use 块")
	}
	if !strings.Contains(s, "web_search_tool_result") {
		t.Errorf("响应缺 web_search_tool_result 块")
	}
	if !strings.Contains(s, "Enh Result: summary") {
		t.Errorf("响应缺摘要文本")
	}
}

// TestSearchFallbackWithImage 端到端：带图搜索请求一律走 search_fallback（不管是否含图片），multimodal_fallback 不应被命中。
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
		SearchFallback:     &SearchRoute{URL: sfMock.URL, API: "sk-sf-xxx", Model: "deepseek-search"},
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

	if !sfCalled {
		t.Fatalf("应命中 search_fallback（带图搜索一律走 sf）")
	}
	if routeCalled {
		t.Errorf("不应命中原 route 上游")
	}
	mg.mu.Lock()
	defer mg.mu.Unlock()
	if mg.called {
		t.Errorf("不应命中 multimodal_fallback（搜索请求不走 mf）")
	}
}

// TestSearchFallbackWithImageSfSupports 端到端：带图搜索走 search_fallback（mf 不命中）。
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
		SearchFallback:     &SearchRoute{URL: sfMock.URL, API: "sk-sf-xxx", Model: "kimi-vl"},
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

// TestWriteSSEErrorGaveUp：重试用尽兜底 writeSSEError 置 flight.gaveUp，
// 完成流状态码列据此显 [重试尽]；Anthropic 口发 overloaded_error，
// Responses 原生透传口发 response.failed。
func TestWriteSSEErrorGaveUp(t *testing.T) {
	f := &flight{}
	rec := httptest.NewRecorder()
	writeSSEError(rec, nil, "upstream status 429", f)
	if !f.gaveUp {
		t.Error("writeSSEError 未置 gaveUp")
	}
	if s := rec.Body.String(); !strings.Contains(s, "overloaded_error") || !strings.Contains(s, "upstream status 429") {
		t.Errorf("Anthropic 兜底事件形态不对: %q", s)
	}

	f2 := &flight{translated: translatedResponsesRaw}
	rec2 := httptest.NewRecorder()
	writeSSEError(rec2, nil, "x", f2)
	if !f2.gaveUp {
		t.Error("responses-raw 未置 gaveUp")
	}
	if s := rec2.Body.String(); !strings.Contains(s, "response.failed") {
		t.Errorf("responses-raw 兜底事件形态不对: %q", s)
	}
}
