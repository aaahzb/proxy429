package main

import "testing"

// resetStats 清空全局 stats，保证各测试互不影响。
func resetStats() {
	stats.mu.Lock()
	defer stats.mu.Unlock()
	stats.active = 0
	stats.cacheRead = 0
	stats.cacheCreation = 0
	stats.inputTokens = 0
	stats.outputTokens = 0
}

func TestParseSSEStatsMessageStart(t *testing.T) {
	resetStats()
	var last int64
	parseSSEStats([]byte(`data: {"type":"message_start","message":{"usage":{"input_tokens":10,"cache_read_input_tokens":5,"cache_creation_input_tokens":3,"output_tokens":1}}}`), &last)
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
