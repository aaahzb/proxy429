package main

import (
	"bytes"
	"encoding/json"
	"reflect"
	"testing"
)

// resetStats 清空全局 stats，保证各测试互不影响。
func resetStats() {
	stats.mu.Lock()
	defer stats.mu.Unlock()
	stats.active = 0
	stats.waiting = 0
	stats.cacheRead = 0
	stats.inputTokens = 0
	stats.outputTokens = 0
	stats.bytesForward.Store(0)
	stats.statusRetries.Store(0)
	stats.classifierRewrites.Store(0)
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
