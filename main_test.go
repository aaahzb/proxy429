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
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

// resetStats clears the global stats so tests don't affect each other.
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
	stats.resetSampleCap(0) // Clear the "recent N" latency/throughput samples
	stats.mu.Unlock()

	// The flight registry and log buffer are reset too (handlers always register flights; the map must not be nil).
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
	// Single stream: output_tokens is cumulative 1→10→20→50; the global should record only the final 50 (sum of deltas).
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
	// Two concurrent streams each keep their own lastOutput; the global accumulates both currents: 100+30=130.
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

// Regression: when both message_start and message_delta carry input_tokens / cache_read,
// take the last-occurring value (later overwrites earlier), don't accumulate. Kimi field measurement: start=45814, delta=1270.
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
	// last should also be the final value
	if lastIn != 1270 || lastCR != 200 || last != 50 {
		t.Errorf("last=%d/%d/%d want 1270/200/50", lastIn, lastCR, last)
	}
}

// Missing fields keep the old value: message_delta without input_tokens must not overwrite it with 0.
func TestParseSSEStatsUsageMissingField(t *testing.T) {
	resetStats()
	var last, lastIn, lastCR, lastCC int64
	var saw bool
	parseSSEStats([]byte(`data: {"type":"message_start","message":{"usage":{"input_tokens":100,"cache_read_input_tokens":50,"output_tokens":1}}}`), &last, &lastIn, &lastCR, &lastCC, &saw)
	// message_delta carries only output_tokens; input/cache_read are missing and should stay 100/50
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

// sawDeltaUsage flag: set only by a message_delta carrying usage (real usage split arriving);
// message_start / other events don't set it. Aggregate stats rely on this flag to count only complete responses.
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

// Interrupted-stream rollback: when the stream breaks after only message_start (Kimi field measurement: start.input includes cache_read, start.cr=0),
// the estimate already added to the global must roll back exactly, otherwise the whole context gets counted as uncached input and the aggregate hit rate drops.
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
	// Anthropic non-streaming (classifier response)
	in, cr, cc, out, ok := parseNonStreamUsage([]byte(`{"type":"message","content":[{"type":"text","text":"x"}],"usage":{"input_tokens":143,"cache_read_input_tokens":61568,"cache_creation_input_tokens":7,"output_tokens":5}}`))
	if !ok || in != 143 || cr != 61568 || cc != 7 || out != 5 {
		t.Errorf("anthropic: ok=%v in=%d cr=%d cc=%d out=%d want 143/61568/7/5", ok, in, cr, cc, out)
	}
	// OpenAI style (prompt_tokens/completion_tokens, no cache_*)
	in, cr, cc, out, ok = parseNonStreamUsage([]byte(`{"choices":[],"usage":{"prompt_tokens":100,"completion_tokens":50}}`))
	if !ok || in != 100 || cr != 0 || cc != 0 || out != 50 {
		t.Errorf("openai: ok=%v in=%d cr=%d cc=%d out=%d want 100/0/0/50", ok, in, cr, cc, out)
	}
	// No usage field
	_, _, _, _, ok = parseNonStreamUsage([]byte(`{"foo":"bar"}`))
	if ok {
		t.Errorf("no usage: should return ok=false")
	}
	// Non-JSON
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
	// Empty model name / all-zero should be ignored
	stats.addModelUsage("", 1, 2, 3, 4)
	stats.addModelUsage("zero", 0, 0, 0, 0)
	got := stats.snapshotModelStats()
	if len(got) != 2 {
		t.Fatalf("want 2 models, got %d: %+v", len(got), got)
	}
	// Descending by total: deepseek-v4=150+300+30+80=560, kimi=10+500+0+5=515
	if got[0].Model != "deepseek-v4" {
		t.Errorf("first should be deepseek-v4 (total 560), got %s", got[0].Model)
	}
	ds := got[0]
	if ds.Input != 150 || ds.CacheRead != 300 || ds.CacheCreation != 30 || ds.Output != 80 {
		t.Errorf("deepseek-v4累加错: in=%d cr=%d cc=%d out=%d want 150/300/30/80", ds.Input, ds.CacheRead, ds.CacheCreation, ds.Output)
	}
	// Cleanup
	stats.mu.Lock()
	stats.modelStats = nil
	stats.mu.Unlock()
}

// cacheHitRate matches Claude Code's cache-hit algorithm:
// cache_read / (input + cache_read + cache_creation).
func TestCacheHitRate(t *testing.T) {
	cases := []struct {
		cr, in, cc int64
		want       string
	}{
		{50, 40, 10, "50.0%"},  // 50/(40+50+10)=50%
		{5, 10, 3, "27.8%"},    // 5/18≈27.78%
		{0, 0, 0, "-"},         // No usage data
		{0, 0, 7, "0.0%"},      // Writes without any hits
		{9000, 45814, 0, "16.4%"}, // Kimi field-measured magnitude: 9000/54814
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

// TestRecentLatency verifies weighted throughput and average first-byte latency calculations.
func TestRecentLatency(t *testing.T) {
	resetStats()
	stats.resetSampleCap(3)
	// 3 samples: first-byte 100/200/300ms, streaming 1000/2000/3000ms, output 10/20/30 tokens
	stats.pushFirstByte(100)
	stats.pushThroughput(1000, 10)
	stats.pushFirstByte(200)
	stats.pushThroughput(2000, 20)
	stats.pushFirstByte(300)
	stats.pushThroughput(3000, 30)
	avgFB, tps := stats.recentLatency()
	// Average first-byte = (100+200+300)/3 = 200ms
	if avgFB != 200 {
		t.Errorf("avgFB=%v want 200", avgFB)
	}
	// Weighted throughput = (10+20+30) / ((1000+2000+3000)/1000) = 60/6 = 10 tok/s
	if tps != 10 {
		t.Errorf("tps=%v want 10", tps)
	}
}

// TestRecentLatencyRingOverwrite verifies the ring buffer overwrites the oldest sample past capacity.
func TestRecentLatencyRingOverwrite(t *testing.T) {
	resetStats()
	stats.resetSampleCap(2)
	stats.pushFirstByte(100)
	stats.pushThroughput(1000, 10)
	stats.pushFirstByte(200)
	stats.pushThroughput(1000, 20)
	stats.pushFirstByte(300)
	stats.pushThroughput(1000, 30) // Capacity 2; overwrites the 1st sample (100,10)
	avgFB, tps := stats.recentLatency()
	// Window holds 200/300: average first-byte = 250ms
	if avgFB != 250 {
		t.Errorf("avgFB=%v want 250（环形覆盖后）", avgFB)
	}
	// Throughput = (20+30)/(2000/1000) = 25 tok/s
	if tps != 25 {
		t.Errorf("tps=%v want 25", tps)
	}
}

// TestRecentLatencyEmpty verifies 0 is returned with no samples.
func TestRecentLatencyEmpty(t *testing.T) {
	resetStats()
	stats.resetSampleCap(5)
	avgFB, tps := stats.recentLatency()
	if avgFB != 0 || tps != 0 {
		t.Errorf("空窗口 avgFB=%v tps=%v want 0,0", avgFB, tps)
	}
}

// TestHandlerLatencySampling end-to-end: after a normal stream (case C) finishes pass-through,
// first-byte latency and token/s are sampled into the sliding window and recentLatency returns non-zero.
func TestHandlerLatencySampling(t *testing.T) {
	resetStats()
	// Mock upstream: returns an SSE stream with usage (message_start -> message_delta -> [DONE]).
	// The sleeps simulate real first-byte latency and streaming duration, avoiding a too-fast local run being truncated to 0 by Milliseconds().
	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		time.Sleep(15 * time.Millisecond) // Simulate "awaiting first byte"
		w.WriteHeader(200)
		f := w.(http.Flusher)
		fmt.Fprint(w, "data: {\"type\":\"message_start\",\"message\":{\"usage\":{\"output_tokens\":1}}}\n\n")
		f.Flush()
		time.Sleep(25 * time.Millisecond) // Simulate streaming output
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

// TestCountTokensFlightTag verifies the /v1/messages/count_tokens probe stream: the upstream response passes through verbatim,
// the finished archive carries the countTokens flag (the web model column shows the [count_tokens] prefix from it),
// and the response body (top-level input_tokens, no usage key, no data: lines) doesn't pollute aggregate stats.
func TestCountTokensFlightTag(t *testing.T) {
	resetStats()
	// Mock upstream: the real count_tokens response shape — a single top-level input_tokens field.
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

	// The handler defer archives before response EOF; the finished list here must contain this stream.
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

	// Stats immunity: input/output must both stay 0 (parseSSEStats needs data: prefixes,
	// parseNonStreamUsage needs a top-level usage key; the count_tokens response has neither).
	stats.mu.Lock()
	in, out := stats.inputTokens, stats.outputTokens
	stats.mu.Unlock()
	if in != 0 || out != 0 {
		t.Errorf("count_tokens 响应污染了统计: input=%d output=%d（应均为 0）", in, out)
	}
}

// TestAddFinishedTotalMs verifies the finished-archive totalMs column: totalMs counts from flight creation
// (proxy received the downstream request) to archiving (handler returns, response fully sent downstream), and shares ended's clock.
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

// TestAddFinishedAttempts verifies the finished archive keeps the attempt count (the data source of the status column's [重试N次]):
// flight.attempt is archived as-is — first-try success = 1, 2 retries = 3.
func TestAddFinishedAttempts(t *testing.T) {
	f := &flight{id: flights.nextID.Add(1), start: time.Now()}
	f.attempt.Store(3)
	addFinished(f)
	finishedMu.Lock()
	ff := finished[len(finished)-1]
	finishedMu.Unlock()
	if ff.attempts != 3 {
		t.Errorf("attempts=%d, want 3（重试 2 次）", ff.attempts)
	}
}

// TestFlightAttemptMs verifies current-attempt timing: attemptStart=0 (in retry backoff) returns -1; once sent it returns the awaited milliseconds.
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

// TestHasCatchAllRoute verifies the pattern:"*" catch-all determination (with a catch-all, the top-level upstream may be empty).
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

// TestClearStatsFinished verifies 「清空统计」 also clears the finished-streams list (in-flight streams and stream numbering untouched).
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

// TestComputeRateCounterReset verifies that after clearing stats the cumulative bytes reset to zero and the rate doesn't go negative.
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

// TestMarkClientGone499 verifies archived status codes follow upstream bookkeeping: 499 only asks "did the upstream finish sending"
// (delivered), consistent with the upstream provider's backend view; local errors and non-200 statuses are unaffected.
func TestMarkClientGone499(t *testing.T) {
	mark := func(status int, delivered, cancelCtx bool) int {
		f := &flight{id: flights.nextID.Add(1), start: time.Now(), status: status}
		if delivered {
			f.delivered.Store(true)
		}
		ctx := context.Background()
		if cancelCtx {
			c, cancel := context.WithCancel(context.Background())
			cancel() // Simulate a downstream disconnect (ctx already cancelled at handler wrap-up defer time)
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

// TestForwardDeliveredOnMessageStop verifies pass-through's complete-delivery marker: reading message_stop marks
// delivered (when the client disconnects right after receiving everything, upstream EOF often never arrives, so we can't wait for EOF); a mid-stream upstream error doesn't mark it.
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
	// Stream breaks midway (non-EOF error read): not marked (the wrap-up defer re-marks such streams as 499).
	if f := run(&errAfterReader{b: []byte("event: message_start\ndata: {\"type\":\"message_start\"}\n\n"), err: io.ErrUnexpectedEOF}); f.delivered.Load() {
		t.Error("上游流中途出错不应标记 delivered")
	}
}

// errAfterReader returns the given error after b is fully read, simulating a mid-stream upstream break (not a clean EOF).
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

// TestParseToolCallName locks SSE-line tool-name extraction: content_block_start of tool_use/server_tool_use
// returns the name; text blocks, web_search_tool_result (contains a tool_use_id substring but the block type isn't a call),
// non-data lines, [DONE], and bad JSON all return empty.
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

// TestParseToolEmptySignals locks the *0 empty-args detection across parsers: start carrying index/input,
// input_json_delta fragments, content_block_stop's index, and the terminal empty-args state of Responses output_item.done's
// three item kinds (function_call looks at arguments, custom_tool_call at input,
// web_search_call at action's query+sources).
func TestParseToolEmptySignals(t *testing.T) {
	// start: index and start's own input must both be captured (a complete server_tool_use block directly concludes non-empty)
	ts, ok := parseToolCallStart([]byte(`data: {"type":"content_block_start","index":3,"content_block":{"type":"server_tool_use","id":"s1","name":"web_search","input":{"query":"q"}}}`))
	if !ok || ts.index != 3 || ts.name != "web_search" || isEmptyArgsJSON(string(ts.input)) {
		t.Errorf("parseToolCallStart 非空 input=%+v ok=%v", ts, ok)
	}
	// delta: only input_json_delta counts, text_delta doesn't
	idx, partial, ok := parseToolArgsDelta([]byte(`data: {"type":"content_block_delta","index":2,"delta":{"type":"input_json_delta","partial_json":"{\"a\":"}}`))
	if !ok || idx != 2 || partial != `{"a":` {
		t.Errorf("parseToolArgsDelta=(%d,%q,%v)", idx, partial, ok)
	}
	if _, _, ok := parseToolArgsDelta([]byte(`data: {"type":"content_block_delta","index":2,"delta":{"type":"text_delta","text":"x"}}`)); ok {
		t.Error("text_delta 不应认作参数 delta")
	}
	// stop: only the index is needed
	if idx, ok := parseToolBlockStop([]byte(`data: {"type":"content_block_stop","index":2}`)); !ok || idx != 2 {
		t.Errorf("parseToolBlockStop=(%d,%v)", idx, ok)
	}
	// Responses done: terminal empty-args state of the three item kinds
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
	// isEmptyArgsJSON boundaries
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

// TestForwardToolCallCount end-to-end pass-through path: each content_block_start of tool_use/server_tool_use
// counts once, same-name repeats merge; toolCallsTag outputs in first-appearance order — a single call shows *1 (arguments
// arriving via input_json_delta), a single call with an empty args object shows *0, multiple calls show the raw *N.
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
	// Read single with args→*1; Edit twice with empty args still shows the raw count *2; web_search single empty-args→*0
	// (index 3 has no content_block_stop: at stream wrap-up, unclosed calls are judged empty from received content).
	if got, want := f.toolCallsTag(), "[Read*1][Edit*2][web_search*0]"; got != want {
		t.Errorf("toolCallsTag=%q, want %q", got, want)
	}
}

// TestToolCallsTag locks formatting boundaries: no calls → empty string, empty names ignored, same-name counts merge keeping first-appearance order;
// a single call shows *1, a single empty-args call shows *0, multiple calls show the raw *N (empty-args marks don't affect it).
func TestToolCallsTag(t *testing.T) {
	f := &flight{}
	if got := f.toolCallsTag(); got != "" {
		t.Errorf("无工具调用 want 空串, got %q", got)
	}
	f.noteToolCall("") // Empty names ignored
	f.noteToolCall("Bash")
	f.noteToolCall("Read")
	f.noteToolCall("Bash")
	f.noteToolCall("Bash")
	if got, want := f.toolCallsTag(), "[Bash*3][Read*1]"; got != want {
		t.Errorf("toolCallsTag=%q, want %q", got, want)
	}
	f.noteToolCallEmpty("Read")   // Single call with empty args → *0
	f.noteToolCallEmpty("Bash")   // Multiple calls show the raw *N; empty-args marks don't affect it
	f.noteToolCallEmpty("Nobody") // Uncounted tools: ignored
	if got, want := f.toolCallsTag(), "[Bash*3][Read*0]"; got != want {
		t.Errorf("toolCallsTag=%q, want %q", got, want)
	}
}

// setCfg sets the test config (maybeRewriteClassifier reads the global cfg): thinkingOff maps to
// classifier_route.classifier_thinking="off"; false leaves the classifier route unset (requests pass through untouched).
func setCfg(thinkingOff bool) {
	var cr *ClassifierRoute
	if thinkingOff {
		cr = &ClassifierRoute{ClassifierThinking: "off"}
	}
	cfg.Store(&Config{ClassifierRoute: cr})
}

// parseBody parses a JSON body into a map.
func parseBody(t *testing.T, b []byte) map[string]interface{} {
	t.Helper()
	var p map[string]interface{}
	if err := json.Unmarshal(b, &p); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	return p
}

// TestMaybeRewriteClassifierString verifies a string system matching the classifier disables thinking.
func TestMaybeRewriteClassifierString(t *testing.T) {
	resetStats()
	setCfg(true)
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
	// max_tokens is never touched: the original value is kept.
	if mt, _ := p["max_tokens"].(float64); mt != 2048 {
		t.Errorf("max_tokens=%v want 2048（不动）", p["max_tokens"])
	}
}

// TestMaybeRewriteClassifierArray verifies an array system matching the classifier disables thinking.
func TestMaybeRewriteClassifierArray(t *testing.T) {
	resetStats()
	setCfg(true)
	body := []byte(`{"system":[{"type":"text","text":"You are a security monitor."}],"thinking":{"type":"enabled"},"max_tokens":1024}`)
	out := maybeRewriteClassifier(body)
	p := parseBody(t, out)
	if th, _ := p["thinking"].(map[string]interface{}); th["type"] != "disabled" {
		t.Errorf("thinking=%v want type=disabled", p["thinking"])
	}
	// The original body has no reasoning_effort; the rewrite should append it.
	if p["reasoning_effort"] != "none" {
		t.Errorf("reasoning_effort=%v want none（应追加到 body）", p["reasoning_effort"])
	}
	// Key order preserved: original fields first, reasoning_effort appended at the end.
	want := []string{"system", "thinking", "max_tokens", "reasoning_effort"}
	if keys := topLevelKeys(t, out); !reflect.DeepEqual(keys, want) {
		t.Errorf("key 顺序:\n got %v\nwant %v", keys, want)
	}
}

// TestMaybeRewriteClassifierNonClassifier verifies ordinary requests (system doesn't match) aren't rewritten.
func TestMaybeRewriteClassifierNonClassifier(t *testing.T) {
	resetStats()
	setCfg(true)
	body := []byte(`{"system":"You are a helpful assistant.","thinking":{"type":"enabled"},"max_tokens":1024}`)
	if out := maybeRewriteClassifier(body); !bytes.Equal(body, out) {
		t.Errorf("普通请求不应被改写")
	}
}

// TestMaybeRewriteClassifierDisabled verifies no rewriting when the toggle is off.
func TestMaybeRewriteClassifierDisabled(t *testing.T) {
	resetStats()
	setCfg(false)
	body := []byte(`{"system":"You are a security monitor.","thinking":{"type":"enabled"},"max_tokens":1024}`)
	if out := maybeRewriteClassifier(body); !bytes.Equal(body, out) {
		t.Errorf("classifier_thinking 未配 off 时不应改写")
	}
}

// TestMaybeRewriteClassifierPreservesOrder verifies the rewrite preserves original key order (no reordering).
// This is the core of the bug fix: the old implementation's json.Unmarshal into a map + Marshal reordered keys alphabetically.
func TestMaybeRewriteClassifierPreservesOrder(t *testing.T) {
	resetStats()
	setCfg(true)
	// Deliberately uses a non-alphabetical key order (model first, messages after).
	body := []byte(`{"model":"x","system":"You are a security monitor.","thinking":{"type":"enabled"},"reasoning_effort":"high","reasoning":{"effort":"high"},"max_tokens":2048,"messages":[]}`)
	out := maybeRewriteClassifier(body)
	// reasoning is deleted; the rest keep their original order.
	want := []string{"model", "system", "thinking", "reasoning_effort", "max_tokens", "messages"}
	if keys := topLevelKeys(t, out); !reflect.DeepEqual(keys, want) {
		t.Errorf("key 顺序:\n got %v\nwant %v", keys, want)
	}
}

// TestClassifierHitCounting end-to-end verifies the classifier dual-count semantics: classifierHits counts on match
// (whether or not rerouted/de-thought); classifierRewrites only +1 when the body is actually rewritten to disable thinking.
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
	// A classifier request with thinking: with the thinking-off toggle on, a real rewrite is guaranteed (only byte changes count as rewrites).
	cls := `{"model":"x","system":"You are a security monitor.","thinking":{"type":"enabled"},"max_tokens":100,"messages":[{"role":"user","content":"hi"}]}`

	cfg.Store(&Config{Upstream: mock.URL, MaxRetries: 0, TotalBudgetSec: 10})
	post(cls)
	post(cls)
	if h, rw := stats.classifierHits.Load(), stats.classifierRewrites.Load(); h != 2 || rw != 0 {
		t.Errorf("关思考关闭时：hits=%d want 2, rewrites=%d want 0", h, rw)
	}

	cfg.Store(&Config{Upstream: mock.URL, MaxRetries: 0, TotalBudgetSec: 10, ClassifierRoute: &ClassifierRoute{URL: mock.URL, ClassifierThinking: "off"}})
	post(cls)
	if h, rw := stats.classifierHits.Load(), stats.classifierRewrites.Load(); h != 3 || rw != 1 {
		t.Errorf("关思考开启后：hits=%d want 3, rewrites=%d want 1", h, rw)
	}

	post(`{"model":"x","messages":[{"role":"user","content":"hi"}]}`) // Non-classifier request: neither counter rises
	if h, rw := stats.classifierHits.Load(), stats.classifierRewrites.Load(); h != 3 || rw != 1 {
		t.Errorf("普通请求后：hits=%d want 3, rewrites=%d want 1", h, rw)
	}
}

// topLevelKeys extracts the key order of a JSON top-level object.
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

// TestExtractModel verifies lightweight model-field extraction from the request body (for the [请求] log display).
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

// TestExtractThinkMode locks the Anthropic-side vocabulary of the status page's 「API」 column thinking values: thinking.type=disabled shows "off",
// enabled shows "on <budget>" (just "on" without a budget), adaptive without effort shows "adaptive" (with effort only the effort word —
// which API family the vocabulary belongs to is carried by the column color), effort without thinking likewise shows only the effort word, no field returns empty.
func TestExtractThinkMode(t *testing.T) {
	cases := []struct {
		name string
		body string
		want string
	}{
		{"关思考", `{"model":"m","thinking":{"type":"disabled"}}`, "off"},
		{"开带预算", `{"model":"m","thinking":{"type":"enabled","budget_tokens":2048}}`, "on 2048"},
		{"开无预算", `{"thinking":{"type":"enabled"}}`, "on"},
		{"adaptive 无 effort", `{"thinking":{"type":"adaptive"}}`, "adaptive"},
		{"adaptive 带 effort 只显档位", `{"thinking":{"type":"adaptive"},"output_config":{"effort":"high"}}`, "high"},
		{"仅 output_config 只显档位", `{"output_config":{"effort":"low"}}`, "low"},
		{"未知 type 原样", `{"thinking":{"type":"future_mode"}}`, "future_mode"},
		{"无思考字段", `{"model":"m","messages":[]}`, ""},
		{"thinking 非对象", `{"thinking":true}`, ""},
		{"非 JSON", `not json`, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := extractThinkMode([]byte(tc.body)); got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
}

// TestLoadConfigThinkingValidation locks validation of the route thinking parameter: ""/auto/adaptive/budget
// are legal, anything else fails at load — a misconfiguration isn't silently degraded to auto while the thinking shape keeps being misjudged from the client model name.
func TestLoadConfigThinkingValidation(t *testing.T) {
	writeCfg := func(t *testing.T, thinking string) string {
		body := `{"upstream":"http://x","routes":[{"pattern":"m*","url":"http://y","thinking":"` + thinking + `"}]}`
		p := filepath.Join(t.TempDir(), "config.json")
		if err := os.WriteFile(p, []byte(body), 0644); err != nil {
			t.Fatal(err)
		}
		return p
	}
	for _, v := range []string{"", "auto", "adaptive", "budget"} {
		if _, _, err := loadConfig(writeCfg(t, v)); err != nil {
			t.Errorf("thinking=%q 应合法: %v", v, err)
		}
	}
	if _, _, err := loadConfig(writeCfg(t, "turbo")); err == nil {
		t.Errorf("thinking=turbo 应报错")
	}
}

// TestLoadConfigRemovedKeys locks the degraded-migration semantics: the three removed top-level keys no longer fail
// loadConfig — the config loads and runs (the keys stay inert), and each removed key comes back as a non-fatal warning
// that drives the red tray icon. (Only the web save handler still rejects them, see TestConfigPostRemovedKeysRejected.)
func TestLoadConfigRemovedKeys(t *testing.T) {
	cases := []struct{ key, hint string }{
		{"classifier_thinking_disabled", `"classifier_thinking": "off"`},
		{"classifier_max_tokens", "no longer modified"},
		{"translateNone2Low", "convertOff2Low"},
	}
	for _, tc := range cases {
		body := `{"upstream":"http://x","` + tc.key + `":true}`
		p := filepath.Join(t.TempDir(), "config.json")
		if err := os.WriteFile(p, []byte(body), 0644); err != nil {
			t.Fatal(err)
		}
		c, warns, err := loadConfig(p)
		if err != nil {
			t.Errorf("旧键 %q 不应再拒载（降级启动）: %v", tc.key, err)
			continue
		}
		if c == nil || c.Upstream != "http://x" {
			t.Errorf("旧键 %q：配置应正常加载: %+v", tc.key, c)
		}
		if len(warns) != 1 || warns[0] != tc.key {
			t.Errorf("旧键 %q 应产生对应警告: %v", tc.key, warns)
			continue
		}
		if got := removedKeyWarningEN(tc.key); !strings.Contains(got, tc.hint) {
			t.Errorf("旧键 %q 的英文警告应含迁移提示 %q: %s", tc.key, tc.hint, got)
		}
	}
}

// TestLoadConfigEnumValidation locks the new enums: classifier_route.classifier_thinking accepts only ""/"off",
// and convertOff2Low on all four route kinds (routes[]/fast_route/multimodal_fallback/search_fallback) accepts
// only ""/"translate"/"all" — anything else fails at load instead of silently degrading to off.
func TestLoadConfigEnumValidation(t *testing.T) {
	writeCfg := func(t *testing.T, body string) string {
		p := filepath.Join(t.TempDir(), "config.json")
		if err := os.WriteFile(p, []byte(body), 0644); err != nil {
			t.Fatal(err)
		}
		return p
	}
	// classifier_thinking: only off is legal.
	if _, _, err := loadConfig(writeCfg(t, `{"upstream":"http://x","classifier_route":{"url":"http://y","classifier_thinking":"off"}}`)); err != nil {
		t.Errorf("classifier_thinking=off 应合法: %v", err)
	}
	if _, _, err := loadConfig(writeCfg(t, `{"upstream":"http://x","classifier_route":{"url":"http://y","classifier_thinking":"low"}}`)); err == nil {
		t.Errorf("classifier_thinking=low 应报错（只有 off）")
	}
	// convertOff2Low: translate/all legal on all four kinds...
	good := []string{
		`{"upstream":"http://x","routes":[{"pattern":"m*","url":"http://y","convertOff2Low":"translate"}]}`,
		`{"upstream":"http://x","fast_route":{"url":"http://y","convertOff2Low":"all"}}`,
		`{"upstream":"http://x","multimodal_fallback":{"url":"http://y","convertOff2Low":"translate"}}`,
		`{"upstream":"http://x","search_fallback":{"url":"http://y","model":"m","convertOff2Low":"all"}}`,
	}
	for _, body := range good {
		if _, _, err := loadConfig(writeCfg(t, body)); err != nil {
			t.Errorf("应合法: %v (%s)", err, body)
		}
	}
	// ...anything else rejected on all four kinds.
	bad := []string{
		`{"upstream":"http://x","routes":[{"pattern":"m*","url":"http://y","convertOff2Low":"yes"}]}`,
		`{"upstream":"http://x","fast_route":{"url":"http://y","convertOff2Low":"native"}}`,
		`{"upstream":"http://x","multimodal_fallback":{"url":"http://y","convertOff2Low":"1"}}`,
		`{"upstream":"http://x","search_fallback":{"url":"http://y","model":"m","convertOff2Low":"on"}}`,
	}
	for _, body := range bad {
		if _, _, err := loadConfig(writeCfg(t, body)); err == nil {
			t.Errorf("应报错拒载: %s", body)
		}
	}
}

// TestMatchModel verifies model-name * wildcard matching (leading/trailing/middle *, multiple *, exact).
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
		{"a*b*c", "ac", false}, // The middle segment b must appear
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

// TestReplaceModelValue verifies replacing the model field value, with length change and still-legal JSON; no model field returns as-is.
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

// TestRouteHandler end-to-end: requests matching a route rule go to the target upstream with model and Authorization replaced; the default upstream is not hit.
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

// TestReservedRoutePattern verifies reserved-name rules: routes whose pattern exactly equals a reserved name ("Fallback"=Codex menu * catch-all,
// "fast_route"=fast lane) don't take effect — colliding requests are caught by later legal routes; wildcards like Fall* are unaffected.
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
	srvFB := mk("fb")     // pattern exactly Fallback (reserved name, ineffective)
	srvFR := mk("fr")     // pattern exactly fast_route (reserved name, ineffective)
	srvWild := mk("wild") // Fall* legal wildcard
	srvAll := mk("all")   // * catch-all
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

	// The reserved-name route is skipped; the Fall* wildcard catches it
	post("Fallback")
	check("Fallback", map[string]bool{"wild": true})

	// The reserved-name route is ineffective (the request has no speed field, so the fast branch doesn't trigger); the * catch-all catches it
	post("fast_route")
	check("fast_route", map[string]bool{"all": true})

	// Reserved-name patterns don't act as wildcards; the * catch-all catches it
	post("claude-x")
	check("claude-x", map[string]bool{"all": true})
}

// TestNoRouteFallback end-to-end: without routes configured, requests go to the default upstream with model unchanged and client token passed through.
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
		// No Routes set: default upstream, client token passed through
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

// TestClassifierRouteHit end-to-end: when a request matches the classifier and classifier_route is configured,
// it routes to the classifier target regardless of the original model, taking priority over model routing (the model-route target must not be hit).
func TestClassifierRouteHit(t *testing.T) {
	resetStats()
	var cg struct {
		mu     sync.Mutex
		model  string
		auth   string
		called bool
	}
	// Classifier route target
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

	// Model route target (must not be hit: classifier route takes priority)
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

	// Classifier request: system prefix matches; the original model is claude-opus-4-8 (would have hit the model route, but classifier route takes priority)
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

// TestClassifierRouteFallback end-to-end: when the classifier matches but no classifier_route is configured,
// it still follows the original model's routes (backwards compatible, unchanged by the new feature).
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
		// ClassifierRoute deliberately unset: classifier requests should fall back to model routing
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

// TestClassifierRouteNotClassifier end-to-end: a non-classifier request doesn't take the classifier route even with classifier_route configured,
// still following model routing.
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

	// Ordinary request: system lacks the classifier prefix; should follow model routing, not trigger the classifier route
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

// TestFastRouteHit end-to-end: a fast request (non-classifier request containing "speed":"fast") hits fast_route,
// the speed field is removed, the Anthropic-Beta header deleted, the model route must not be hit, and the response carries fast rate-limit headers.
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

// TestFastRouteNotConfigured end-to-end: a fast request without fast_route configured
// still follows the original model's routes (backwards compatible), the speed field passing through unchanged.
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

// TestFastRouteClassifierPriority end-to-end: a classifier request (system prefix match) doesn't take the fast route
// even with "speed":"fast" present (classifier route takes priority).
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

// TestFastRouteHeaderOnly header-only trigger: only the Anthropic-Beta header (no "speed":"fast" body field),
// simulating the leftover header after Claude Code /fast off. Verifies the fast route must not be hit.
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

	// The body has no "speed":"fast"; the Anthropic-Beta request header alone must not trigger the fast route
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

// TestFastRouteBodyWhitespace verifies "speed": "fast" with whitespace, not in first position, still hits and is correctly removed.
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

	// speed field with whitespace and not in first position
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

// TestHasImage unit-tests hasImage: true with an image block, false for pure text.
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

// TestImageFallbackRoute end-to-end: image + text_only model + multimodal_fallback configured
// should switch to the fallback upstream, model changed to fallback.model, and the original route upstream must not be hit.
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

	// Contains an image block
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

// TestImageFallbackNoImage end-to-end: no image + text_only model + fallback configured
// should go to the original route upstream (fallback not triggered).
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

	// Pure-text request
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

// TestImageFallbackNotTextOnly end-to-end: image + non-text_only model + fallback configured
// should go to the original route upstream (the target model supports multimodal itself; no fallback).
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

// TestImageFallbackNotConfigured end-to-end: image + text_only model + no fallback configured
// degrades to the original route upstream (the upstream handles the image itself, may error, but the proxy must not crash).
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
		// MultimodalFallback not configured
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

// TestHasWebSearch unit-tests hasWebSearch: both server-side web_search and client-side WebSearch are recognized; pure text is false.
func TestHasWebSearch(t *testing.T) {
	cases := []struct {
		name string
		body string
		want bool
	}{
		{"server-side web_search", `{"tools":[{"type":"web_search_20250305","name":"web_search","max_uses":5}]}`, true},
		{"client-side WebSearch", `{"tools":[{"name":"WebSearch","description":"x","input_schema":{}}]}`, false}, // Client-side tool definitions are not recognized (every Code request carries them; would false-positive)
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

// TestSearchFallbackRoute end-to-end: pure search request + no_search model + search_fallback configured
// should switch to the search-fallback upstream, model changed to sf.model, and the original route upstream must not be hit.
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

// TestSearchFallbackNoSearch end-to-end: no search tool + no_search model + sf configured
// should go to the original route upstream (search fallback not triggered).
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

// TestSearchFallbackNotNoSearch end-to-end: with search + non-no_search model + sf configured
// should go to the original route upstream (the target model supports search itself; no fallback).
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

// TestEnhanceSearchRoute end-to-end: route.EnhanceSearch non-nil + search tool present + no_search:false
// should take enhanced search (searchAndRespond uses the route's url/api/model), not calling the main default upstream.
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
			// step1: non-streaming JSON returning server_tool_use + web_search_tool_result.
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
		// step2: streaming summary.
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

// TestSearchFallbackWithImage end-to-end: search requests with images always take search_fallback (image or not), multimodal_fallback must not be hit.
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
		MultimodalFallback: &MultimodalRoute{URL: mfMock.URL, API: "sk-mf-xxx", Model: "kimi-vl"}, // NoSearch defaults to false; search supported
	})
	stats.resetSampleCap(5)

	proxy := httptest.NewServer(http.HandlerFunc(handler))
	defer proxy.Close()

	// Carries both an image and a search tool
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

// TestSearchFallbackWithImageSfSupports end-to-end: image search takes search_fallback (mf not hit).
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

// TestSearchFallbackNotConfigured end-to-end: with search + no_search + neither sf/mf configured,
// degrades to the original route upstream.
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
		// Neither SearchFallback nor MultimodalFallback configured
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

// TestRetryPingKeepalive end-to-end: upstream 429s twice, then 200 on the third.
// The proxy should send SSE pings during retries and finally pass through the 200 message_start stream.
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

// TestRetryBackoffCancellable end-to-end: persistent upstream 429 + long backoff,
// client cancels midway; the proxy should stop fast via sleepWithPing's ctx.Done, not sleep the full backoff.
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
		BaseDelaySec:       10, // Long backoff, to ensure it doesn't end via that
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
	// After the client cancels, sleepWithPing should immediately detect ctx.Done and return; it must not sleep the full 10s backoff.
	if elapsed > 2*time.Second {
		t.Errorf("客户端取消后代理耗时 %v，应快速停止(<2s)", elapsed)
	}
}

// TestRetryExhaustedSSEError end-to-end: persistent upstream 429, retries exhausted.
// The first retry already sent 200 keepalive headers, so the 429 can't be passed through; an SSE error (overloaded_error) is sent instead.
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

	// 200 keepalive headers already sent; an SSE error is sent at exhaustion instead of 429.
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

// TestWriteSSEErrorGaveUp: the retry-exhaustion fallback writeSSEError sets flight.gaveUp,
// which the finished-stream status column shows as [重试尽]; the Anthropic port sends overloaded_error,
// the Responses native passthrough port sends response.failed.
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
