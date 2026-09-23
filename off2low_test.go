package main

// off2low_test.go: unit tests for the convertOff2Low native-port half — maybeUpgradeOffToLow (detection, shapes,
// guards, key-order preservation) and thinkingStripper (SSE drop/renumber, JSON filtering, byte-exact fallbacks).

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestLowThinkingBudget locks the shared budget math: 2048 uncapped (max_tokens absent/broken), capped at
// max_tokens/2, and the 1024 floor below which the upgrade is abandoned.
func TestLowThinkingBudget(t *testing.T) {
	if b, ok := lowThinkingBudget(0); !ok || b != 2048 {
		t.Errorf("无 max_tokens 时不压顶: got %d,%v want 2048,true", b, ok)
	}
	if b, ok := lowThinkingBudget(3000); !ok || b != 1500 {
		t.Errorf("应压到 max_tokens/2: got %d,%v want 1500,true", b, ok)
	}
	if b, ok := lowThinkingBudget(2048); !ok || b != 1024 {
		t.Errorf("2048/2=1024 恰好踩线应放行: got %d,%v want 1024,true", b, ok)
	}
	if _, ok := lowThinkingBudget(1500); ok {
		t.Errorf("750<1024 应放弃升级")
	}
}

// TestUpgradeOffToLowShapes locks the two upgrade shapes: budget models get enabled+budget_tokens (2048 capped at
// max_tokens/2), adaptive models get adaptive+output_config.effort:"low".
func TestUpgradeOffToLowShapes(t *testing.T) {
	// Budget shape.
	nb, up, ab := maybeUpgradeOffToLow([]byte(`{"model":"m","max_tokens":8192,"thinking":{"type":"disabled"}}`), "deepseek-v4-flash")
	if !up || ab {
		t.Fatalf("budget 模型应升级: up=%v ab=%v", up, ab)
	}
	var p map[string]interface{}
	if err := json.Unmarshal(nb, &p); err != nil {
		t.Fatalf("升级后非法 JSON: %v", err)
	}
	th := p["thinking"].(map[string]interface{})
	if th["type"] != "enabled" || th["budget_tokens"].(float64) != 2048 {
		t.Errorf("thinking=%v, want enabled/2048", th)
	}
	if p["reasoning_effort"] != "low" {
		t.Errorf("reasoning_effort=%v, want low", p["reasoning_effort"])
	}
	if _, ok := p["output_config"]; ok {
		t.Errorf("budget 形态不应带 output_config: %v", p["output_config"])
	}

	// Budget cap: 3000/2=1500.
	nb, up, _ = maybeUpgradeOffToLow([]byte(`{"max_tokens":3000,"thinking":{"type":"disabled"}}`), "deepseek-v4-flash")
	if !up {
		t.Fatal("应升级")
	}
	json.Unmarshal(nb, &p)
	if th := p["thinking"].(map[string]interface{}); th["budget_tokens"].(float64) != 1500 {
		t.Errorf("压顶应为 1500: %v", th)
	}

	// Adaptive shape: output_config is replaced in place with effort:"low".
	nb, up, _ = maybeUpgradeOffToLow([]byte(`{"max_tokens":8192,"thinking":{"type":"disabled"},"output_config":{"effort":"high"}}`), "claude-sonnet-5")
	if !up {
		t.Fatal("adaptive 模型应升级")
	}
	json.Unmarshal(nb, &p)
	if th := p["thinking"].(map[string]interface{}); th["type"] != "adaptive" {
		t.Errorf("thinking=%v, want adaptive", th)
	}
	if oc := p["output_config"].(map[string]interface{}); oc["effort"] != "low" {
		t.Errorf("output_config=%v, want effort low", oc)
	}
}

// TestUpgradeOffToLowDetection: only an explicit thinking-off triggers the upgrade; every other shape passes through untouched.
func TestUpgradeOffToLowDetection(t *testing.T) {
	cases := []struct {
		name   string
		body   string
		wantUp bool
	}{
		{"无思考字段不动", `{"model":"m","max_tokens":4096,"messages":[]}`, false},
		{"thinking enabled 不动", `{"thinking":{"type":"enabled","budget_tokens":5000},"max_tokens":8192}`, false},
		{"adaptive 不动", `{"thinking":{"type":"adaptive"},"output_config":{"effort":"high"}}`, false},
		{"thinking disabled 升级", `{"thinking":{"type":"disabled"},"max_tokens":8192}`, true},
		{"effort none 升级", `{"reasoning_effort":"none","max_tokens":8192}`, true},
		{"effort off 升级", `{"reasoning_effort":"off","max_tokens":8192}`, true},
		{"effort disabled 升级", `{"reasoning_effort":"disabled","max_tokens":8192}`, true},
		{"effort low 不动", `{"reasoning_effort":"low","max_tokens":8192}`, false},
		{"thinking 在则不看 effort", `{"thinking":{"type":"enabled"},"reasoning_effort":"none","max_tokens":8192}`, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			nb, up, ab := maybeUpgradeOffToLow([]byte(tc.body), "deepseek-v4-flash")
			if up != tc.wantUp || ab {
				t.Errorf("up=%v ab=%v, want up=%v ab=false", up, ab, tc.wantUp)
			}
			if !up && string(nb) != tc.body {
				t.Errorf("未升级时 body 应原样: %s", nb)
			}
		})
	}
}

// TestUpgradeOffToLowGuard: the budget-floor guard abandons the upgrade (max_tokens too small), body byte-untouched.
func TestUpgradeOffToLowGuard(t *testing.T) {
	body := `{"max_tokens":1500,"thinking":{"type":"disabled"}}`
	nb, up, ab := maybeUpgradeOffToLow([]byte(body), "deepseek-v4-flash")
	if up || !ab || string(nb) != body {
		t.Errorf("up=%v ab=%v body=%s; want up=false ab=true body 原样", up, ab, nb)
	}
}

// TestUpgradeOffToLowKeyOrder: positional edits — original key order kept, deletions clean, appended keys land at the end.
func TestUpgradeOffToLowKeyOrder(t *testing.T) {
	body := `{"model":"m","max_tokens":8192,"thinking":{"type":"disabled"},"temperature":0.7,"top_p":0.9,"reasoning":{"effort":"none"}}`
	nb, up, _ := maybeUpgradeOffToLow([]byte(body), "deepseek-v4-flash")
	if !up {
		t.Fatal("应升级")
	}
	if got, want := strings.Join(topLevelKeys(t, nb), ","), "model,max_tokens,thinking,reasoning_effort"; got != want {
		t.Errorf("key 序 %q, want %q（temperature/top_p/reasoning 删除，reasoning_effort 追加在尾）", got, want)
	}
	var p map[string]interface{}
	json.Unmarshal(nb, &p)
	for _, gone := range []string{"temperature", "top_p", "reasoning"} {
		if _, ok := p[gone]; ok {
			t.Errorf("%s 应删除", gone)
		}
	}
}

// TestUpgradeOffToLowEffortOnlyOff: detection via reasoning_effort with no thinking field — thinking gets appended at the end.
func TestUpgradeOffToLowEffortOnlyOff(t *testing.T) {
	body := `{"model":"gpt-5-codex","max_tokens":8192,"reasoning_effort":"none"}`
	nb, up, _ := maybeUpgradeOffToLow([]byte(body), "deepseek-v4-flash")
	if !up {
		t.Fatal("应升级")
	}
	if got, want := strings.Join(topLevelKeys(t, nb), ","), "model,max_tokens,reasoning_effort,thinking"; got != want {
		t.Errorf("key 序 %q, want %q", got, want)
	}
	var p map[string]interface{}
	json.Unmarshal(nb, &p)
	if p["reasoning_effort"] != "low" {
		t.Errorf("effort 应改为 low: %v", p["reasoning_effort"])
	}
	if th := p["thinking"].(map[string]interface{}); th["type"] != "enabled" {
		t.Errorf("thinking=%v, want enabled", th)
	}
}

// stripSSEInput is a thinking-then-text stream: block 0 thinking (with signature_delta), block 1 text.
const stripSSEInput = "event: message_start\n" +
	"data: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":10}}}\n\n" +
	"event: content_block_start\n" +
	"data: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"thinking\",\"thinking\":\"\"}}\n\n" +
	"event: content_block_delta\n" +
	"data: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"thinking_delta\",\"thinking\":\"hmm\"}}\n\n" +
	"data: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"signature_delta\",\"signature\":\"sig\"}}\n\n" +
	"data: {\"type\":\"content_block_stop\",\"index\":0}\n\n" +
	"event: content_block_start\n" +
	"data: {\"type\":\"content_block_start\",\"index\":1,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n" +
	"data: {\"type\":\"content_block_delta\",\"index\":1,\"delta\":{\"type\":\"text_delta\",\"text\":\"hi\"}}\n\n" +
	"data: {\"type\":\"content_block_stop\",\"index\":1}\n\n" +
	"event: message_delta\n" +
	"data: {\"type\":\"message_delta\",\"usage\":{\"output_tokens\":5}}\n\n" +
	"data: {\"type\":\"message_stop\"}\n\n"

// TestStripperSSEDropAndRenumber: the thinking block (start/deltas/stop) vanishes wholesale, the text block is
// renumbered 1->0 (re-marshaled with alphabetical keys), and all other lines pass through byte-exact.
func TestStripperSSEDropAndRenumber(t *testing.T) {
	s := newThinkingStripper("text/event-stream; charset=utf-8")
	out := string(s.feed([]byte(stripSSEInput))) + string(s.flush())
	for _, gone := range []string{"thinking_delta", "signature_delta", `"thinking":"hmm"`, `"content_block\":{\"type\":\"thinking\"`} {
		if strings.Contains(out, gone) {
			t.Errorf("应剥离 %q: %s", gone, out)
		}
	}
	if !strings.Contains(out, `"content_block":{"text":"","type":"text"},"index":0,"type":"content_block_start"`) {
		t.Errorf("text 块应重编号为 index 0: %s", out)
	}
	for _, keep := range []string{
		`data: {"type":"message_start","message":{"usage":{"input_tokens":10}}}`,
		`data: {"type":"message_delta","usage":{"output_tokens":5}}`,
		`data: {"type":"message_stop"}`,
	} {
		if !strings.Contains(out, keep) {
			t.Errorf("应原样保留 %q: %s", keep, out)
		}
	}
	if s.stripped != 1 {
		t.Errorf("stripped=%d, want 1", s.stripped)
	}
}

// TestStripperSSEChunkSplit: a data: line split across two chunks is processed exactly as if fed in one piece.
func TestStripperSSEChunkSplit(t *testing.T) {
	one := newThinkingStripper("text/event-stream")
	want := string(one.feed([]byte(stripSSEInput))) + string(one.flush())
	two := newThinkingStripper("text/event-stream")
	cut := 117 // deliberately mid-line
	got := string(two.feed([]byte(stripSSEInput[:cut]))) + string(two.feed([]byte(stripSSEInput[cut:]))) + string(two.flush())
	if got != want {
		t.Errorf("分块喂入结果应与一次性一致:\n%q\n---\n%q", got, want)
	}
}

// TestStripperSSEByteExactNoThinking: a stream without thinking blocks passes through byte-for-byte (no re-marshaling).
func TestStripperSSEByteExactNoThinking(t *testing.T) {
	in := "event: message_start\ndata: {\"type\":\"message_start\"}\n\n" +
		"data: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n" +
		"data: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"hi\"}}\n\n" +
		"data: {\"type\":\"content_block_stop\",\"index\":0}\n\n" +
		"data: {\"type\":\"message_stop\"}\n\n"
	s := newThinkingStripper("text/event-stream")
	out := string(s.feed([]byte(in))) + string(s.flush())
	if out != in {
		t.Errorf("无思考块应字节不动:\n%q\n---\n%q", out, in)
	}
	if s.stripped != 0 {
		t.Errorf("stripped=%d, want 0", s.stripped)
	}
}

// TestStripperSSERedacted: redacted_thinking blocks drop the same way, and a following tool_use block renumbers to 0.
func TestStripperSSERedacted(t *testing.T) {
	in := "data: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"redacted_thinking\",\"data\":\"xyz\"}}\n\n" +
		"data: {\"type\":\"content_block_stop\",\"index\":0}\n\n" +
		"data: {\"type\":\"content_block_start\",\"index\":1,\"content_block\":{\"type\":\"tool_use\",\"id\":\"t1\",\"name\":\"Read\"}}\n\n"
	s := newThinkingStripper("text/event-stream")
	out := string(s.feed([]byte(in)))
	if strings.Contains(out, "redacted_thinking") || strings.Contains(out, "xyz") {
		t.Errorf("redacted 块应剥离: %s", out)
	}
	if !strings.Contains(out, `"index":0,"type":"content_block_start"`) || !strings.Contains(out, `"tool_use"`) {
		t.Errorf("tool_use 块应重编号为 index 0: %s", out)
	}
}

// TestStripperJSON: the buffered JSON document comes out at flush with thinking blocks filtered and usage kept.
func TestStripperJSON(t *testing.T) {
	s := newThinkingStripper("application/json")
	body := `{"id":"m1","content":[{"type":"thinking","thinking":"hmm","signature":"s"},{"type":"text","text":"hi"}],"usage":{"output_tokens":5}}`
	if out := s.feed([]byte(body)); out != nil {
		t.Errorf("JSON 模式应整段缓冲, got %q", out)
	}
	out := s.flush()
	var p map[string]interface{}
	if err := json.Unmarshal(out, &p); err != nil {
		t.Fatalf("剥离后不是合法 JSON: %v (%s)", err, out)
	}
	content := p["content"].([]interface{})
	if len(content) != 1 || content[0].(map[string]interface{})["type"] != "text" {
		t.Errorf("content 应只剩 text 块: %v", content)
	}
	if p["usage"].(map[string]interface{})["output_tokens"].(float64) != 5 {
		t.Errorf("usage 应保留: %v", p["usage"])
	}
	if s.stripped != 1 {
		t.Errorf("stripped=%d, want 1", s.stripped)
	}
}

// TestStripperJSONPassthrough: malformed JSON and thinking-free documents both come back byte-identical.
func TestStripperJSONPassthrough(t *testing.T) {
	s := newThinkingStripper("application/json")
	s.feed([]byte("not json"))
	if out := string(s.flush()); out != "not json" {
		t.Errorf("坏 JSON 应原样返回: %q", out)
	}
	s2 := newThinkingStripper("application/json")
	body := `{"content":[{"type":"text","text":"hi"}]}`
	s2.feed([]byte(body))
	if out := string(s2.flush()); out != body {
		t.Errorf("无思考块应原样返回: %q", out)
	}
}
