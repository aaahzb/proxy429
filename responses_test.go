package main

// cc-switch: https://github.com/farion1231/cc-switch — MIT License, Copyright (c) 2025 Jason Young.

// responses_test.go — translation-layer tests for the Responses API listener port.
// Request-conversion cases ported from cc-switch transform_codex_anthropic.rs's test suite.

import (
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// ---- Request translation ----

func TestResponsesToAnthropicSimple(t *testing.T) {
	body := map[string]interface{}{
		"model":        "gpt-5-codex",
		"instructions": "You are a coding agent.",
		"input":        "hello",
		"stream":       true,
	}
	out, _, err := responsesToAnthropic(body)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if out["model"] != "gpt-5-codex" {
		t.Errorf("model=%v", out["model"])
	}
	if out["system"] != "You are a coding agent." {
		t.Errorf("system=%v", out["system"])
	}
	if out["max_tokens"].(int64) != defaultResponsesMaxTokens {
		t.Errorf("max_tokens=%v, want 默认 %d", out["max_tokens"], defaultResponsesMaxTokens)
	}
	msgs := out["messages"].([]map[string]interface{})
	if len(msgs) != 1 || objStr(msgs[0], "role") != "user" {
		t.Fatalf("messages=%v", msgs)
	}
	blk := asObj(msgs[0]["content"].([]interface{})[0])
	if objStr(blk, "type") != "text" || objStr(blk, "text") != "hello" {
		t.Errorf("content=%v", blk)
	}
}

func TestResponsesToAnthropicFunctionCallMerging(t *testing.T) {
	// Two consecutive function_call + two function_call_output:
	// calls merge into the same assistant message, results merge into the same user message.
	body := map[string]interface{}{
		"model": "gpt-5-codex",
		"input": []interface{}{
			map[string]interface{}{"type": "message", "role": "user", "content": []interface{}{
				map[string]interface{}{"type": "input_text", "text": "天气?"},
			}},
			map[string]interface{}{"type": "function_call", "call_id": "c1", "name": "t", "arguments": `{"a":1}`},
			map[string]interface{}{"type": "function_call", "call_id": "c2", "name": "t", "arguments": `{"b":2}`},
			map[string]interface{}{"type": "function_call_output", "call_id": "c1", "output": "A"},
			map[string]interface{}{"type": "function_call_output", "call_id": "c2", "output": "B"},
		},
	}
	out, _, err := responsesToAnthropic(body)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	msgs := out["messages"].([]map[string]interface{})
	if len(msgs) != 3 {
		t.Fatalf("messages 数=%d, want 3: %v", len(msgs), msgs)
	}
	asst := msgs[1]
	if objStr(asst, "role") != "assistant" {
		t.Fatalf("msgs[1].role=%v", objStr(asst, "role"))
	}
	blocks := asArr(asst["content"])
	if len(blocks) != 2 {
		t.Fatalf("assistant 块数=%d, want 2", len(blocks))
	}
	b0 := asObj(blocks[0])
	if objStr(b0, "type") != "tool_use" || objStr(b0, "id") != "c1" {
		t.Errorf("tool_use[0]=%v", b0)
	}
	user := msgs[2]
	results := messageBlockIDs(user, "tool_result", "tool_use_id")
	if len(results) != 2 || results[0] != "c1" || results[1] != "c2" {
		t.Errorf("tool_result ids=%v, want [c1 c2] 合并一条 user", results)
	}
}

func TestResponsesToAnthropicOrphanOutputDropped(t *testing.T) {
	// Orphan function_call_output (no paired function_call): dropped wholesale,
	// the emptied user message is also cleaned up, leaving only the opening user question.
	body := map[string]interface{}{
		"model": "gpt-5-codex",
		"input": []interface{}{
			map[string]interface{}{"type": "message", "role": "user", "content": []interface{}{
				map[string]interface{}{"type": "input_text", "text": "hi"},
			}},
			map[string]interface{}{"type": "function_call_output", "call_id": "ghost", "output": "X"},
		},
	}
	out, _, err := responsesToAnthropic(body)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	msgs := out["messages"].([]map[string]interface{})
	if len(msgs) != 1 {
		t.Fatalf("messages 数=%d, want 1（孤儿结果应丢弃）: %v", len(msgs), msgs)
	}
}

func TestResponsesToAnthropicLeadingUserInserted(t *testing.T) {
	// History starting with assistant (compacted/resumed session): a leading user is auto-inserted.
	body := map[string]interface{}{
		"model": "gpt-5-codex",
		"input": []interface{}{
			map[string]interface{}{"type": "message", "role": "assistant", "content": []interface{}{
				map[string]interface{}{"type": "output_text", "text": "上一轮回答"},
			}},
			map[string]interface{}{"type": "message", "role": "user", "content": []interface{}{
				map[string]interface{}{"type": "input_text", "text": "继续"},
			}},
		},
	}
	out, _, err := responsesToAnthropic(body)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	msgs := out["messages"].([]map[string]interface{})
	if objStr(msgs[0], "role") != "user" {
		t.Fatalf("首条 role=%v, want user", objStr(msgs[0], "role"))
	}
	if len(msgs) != 3 {
		t.Errorf("messages 数=%d, want 3（含补的前导 user）", len(msgs))
	}
}

func TestResponsesToAnthropicIncompleteToolTurnDropped(t *testing.T) {
	// assistant tool_use without a matching tool_result: the whole tool turn is dropped, ordinary conversation kept.
	body := map[string]interface{}{
		"model": "gpt-5-codex",
		"input": []interface{}{
			map[string]interface{}{"type": "message", "role": "user", "content": []interface{}{
				map[string]interface{}{"type": "input_text", "text": "问题"},
			}},
			map[string]interface{}{"type": "function_call", "call_id": "c1", "name": "t", "arguments": "{}"},
			map[string]interface{}{"type": "message", "role": "user", "content": []interface{}{
				map[string]interface{}{"type": "input_text", "text": "换个问题"},
			}},
		},
	}
	out, _, err := responsesToAnthropic(body)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	msgs := out["messages"].([]map[string]interface{})
	for _, m := range msgs {
		if len(messageBlockIDs(m, "tool_use", "id")) > 0 {
			t.Errorf("仍存在未配对的 tool_use: %v", m)
		}
	}
	// Two user messages each kept (unaffected after the unpaired tool turn is dropped).
	if len(msgs) != 2 {
		t.Errorf("messages=%v, want 两条 user", msgs)
	}
	for _, m := range msgs {
		if objStr(m, "role") != "user" {
			t.Errorf("role=%v, want user", objStr(m, "role"))
		}
	}
}

func TestResponsesToAnthropicThinkingBudget(t *testing.T) {
	cases := map[string]int64{"low": 2048, "medium": 8192, "high": 16384, "xhigh": 24576, "ultra": 24576}
	for effort, want := range cases {
		if got := effortToThinkingBudget(effort); got != want {
			t.Errorf("effort=%s budget=%d, want %d", effort, got, want)
		}
	}
	if effortToThinkingBudget("none") != 0 || effortToThinkingBudget("") != 0 {
		t.Errorf("none/空 effort 应返回 0")
	}

	body := map[string]interface{}{
		"model":     "gpt-5-codex",
		"input":     "hi",
		"reasoning": map[string]interface{}{"effort": "high"},
	}
	out, _, err := responsesToAnthropic(body)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	th := asObj(out["thinking"])
	// Budget clamped to max_tokens/2=16000 (default max_tokens 32000; 16384 exceeds half).
	if objStr(th, "type") != "enabled" || toInt64(th["budget_tokens"]) != 16000 {
		t.Errorf("thinking=%v, want enabled/16000（max_tokens 半压顶）", th)
	}
}

// TestResponsesToAnthropicThinkStyleOverride locks the route thinking parameter (target-model thinking-shape
// declaration) overriding translation decisions: auto keeps looking up by client model name; budget forces classic
// enabled+budget_tokens (even when the client alias names a model in the adaptive table); adaptive forces
// adaptive+output_config.effort (even when the client name isn't in the table); after forcing, turning thinking off is still allowed
// (cannotDisable no longer applies the fable/mythos rules).
func TestResponsesToAnthropicThinkStyleOverride(t *testing.T) {
	mk := func(model, effort string) map[string]interface{} {
		return map[string]interface{}{
			"model":     model,
			"input":     "hi",
			"reasoning": map[string]interface{}{"effort": effort},
		}
	}

	// auto: fable-5 in the table → adaptive + effort mapping (high→high).
	out, _, _, err := responsesToAnthropicTriple(mk("claude-fable-5", "high"), nil, nil, "", false, true)
	if err != nil {
		t.Fatalf("auto err: %v", err)
	}
	th := asObj(out["thinking"])
	if objStr(th, "type") != "adaptive" || objStr(asObj(out["output_config"]), "effort") != "high" {
		t.Errorf("auto fable-5: thinking=%v output_config=%v, want adaptive/high", th, out["output_config"])
	}

	// budget forced: same fable-5 alias, route declares budget → enabled+16000 (capped), no output_config.
	out, _, _, err = responsesToAnthropicTriple(mk("claude-fable-5", "high"), nil, nil, "budget", false, true)
	if err != nil {
		t.Fatalf("budget err: %v", err)
	}
	th = asObj(out["thinking"])
	if objStr(th, "type") != "enabled" || toInt64(th["budget_tokens"]) != 16000 {
		t.Errorf("budget fable-5: thinking=%v, want enabled/16000", th)
	}
	if out["output_config"] != nil {
		t.Errorf("budget 路径不应有 output_config: %v", out["output_config"])
	}

	// adaptive forced: gpt-5-codex not in the table, route declares adaptive → adaptive+effort high.
	out, _, _, err = responsesToAnthropicTriple(mk("gpt-5-codex", "high"), nil, nil, "adaptive", false, true)
	if err != nil {
		t.Fatalf("adaptive err: %v", err)
	}
	th = asObj(out["thinking"])
	if objStr(th, "type") != "adaptive" || objStr(asObj(out["output_config"]), "effort") != "high" {
		t.Errorf("adaptive gpt-5-codex: thinking=%v output_config=%v, want adaptive/high", th, out["output_config"])
	}

	// forced adaptive + explicit off (effort none) → disabled (cannotDisable overridden, off allowed).
	out, _, _, err = responsesToAnthropicTriple(mk("gpt-5-codex", "none"), nil, nil, "adaptive", false, true)
	if err != nil {
		t.Fatalf("adaptive none err: %v", err)
	}
	if objStr(asObj(out["thinking"]), "type") != "disabled" {
		t.Errorf("adaptive+none: thinking=%v, want disabled", out["thinking"])
	}

	// budget forced + explicit off → disabled.
	out, _, _, err = responsesToAnthropicTriple(mk("claude-fable-5", "none"), nil, nil, "budget", false, true)
	if err != nil {
		t.Fatalf("budget none err: %v", err)
	}
	if objStr(asObj(out["thinking"]), "type") != "disabled" {
		t.Errorf("budget+none: thinking=%v, want disabled", out["thinking"])
	}
}

// TestResponsesThinkMode locks the Responses-side vocabulary of the status page's 「API」 column thinking values: reasoning.effort shows only
// the level word (an "effort·" prefix would be redundant — the column's green carries the Responses family), explicit-off values
// (none/off/disabled, case-insensitive) merge into "关"; no reasoning / no effort / non-object reasoning all return empty (column shows -).
func TestResponsesThinkMode(t *testing.T) {
	cases := []struct {
		name string
		body map[string]interface{}
		want string
	}{
		{"无 reasoning 字段", map[string]interface{}{"model": "m"}, ""},
		{"reasoning 非对象", map[string]interface{}{"reasoning": "high"}, ""},
		{"无 effort", map[string]interface{}{"reasoning": map[string]interface{}{"summary": "auto"}}, ""},
		{"effort high", map[string]interface{}{"reasoning": map[string]interface{}{"effort": "high"}}, "high"},
		{"effort minimal", map[string]interface{}{"reasoning": map[string]interface{}{"effort": "minimal"}}, "minimal"},
		{"effort none 归并为关", map[string]interface{}{"reasoning": map[string]interface{}{"effort": "none"}}, "关"},
		{"effort OFF 大小写不敏感", map[string]interface{}{"reasoning": map[string]interface{}{"effort": "OFF"}}, "关"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := responsesThinkMode(tc.body); got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
}

func TestResponsesToAnthropicToolChoiceAndTools(t *testing.T) {
	body := map[string]interface{}{
		"model": "gpt-5-codex",
		"input": "hi",
		"tools": []interface{}{
			map[string]interface{}{
				"type": "function", "name": "get_weather", "description": "查天气",
				"parameters": map[string]interface{}{"type": "object", "properties": map[string]interface{}{}},
			},
			map[string]interface{}{"type": "web_search"},
		},
		"tool_choice":         "required",
		"parallel_tool_calls": false,
	}
	out, _, err := responsesToAnthropic(body)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	tools := asArr(out["tools"])
	if len(tools) != 2 {
		t.Fatalf("tools 数=%d, want 2", len(tools))
	}
	t0 := asObj(tools[0])
	if objStr(t0, "name") != "get_weather" || asObj(t0["input_schema"]) == nil {
		t.Errorf("tool[0]=%v", t0)
	}
	t1 := asObj(tools[1])
	if objStr(t1, "type") != "web_search_20250305" || objStr(t1, "name") != "web_search" {
		t.Errorf("tool[1]=%v, want web_search_20250305", t1)
	}
	tc := asObj(out["tool_choice"])
	if objStr(tc, "type") != "any" {
		t.Errorf("tool_choice=%v, want required→any", tc)
	}
	if v, _ := tc["disable_parallel_tool_use"].(bool); !v {
		t.Errorf("parallel_tool_calls=false 应置 disable_parallel_tool_use")
	}
}

// ---- Thinking-block envelopes ----

func TestThinkingEnvelopeRoundTrip(t *testing.T) {
	blk := map[string]interface{}{
		"type": "thinking", "thinking": "先想一想", "signature": "sig_abc",
	}
	enc := encodeThinkingEnvelope(blk)
	if !strings.HasPrefix(enc, thinkingEnvelopePrefix) {
		t.Fatalf("enc=%q 缺前缀", enc)
	}
	dec := decodeThinkingEnvelope(enc)
	if dec == nil {
		t.Fatalf("解码失败")
	}
	if objStr(dec, "thinking") != "先想一想" || objStr(dec, "signature") != "sig_abc" {
		t.Errorf("还原块=%v", dec)
	}
	// Unsigned thinking isn't encoded; garbled/foreign envelopes don't decode.
	if encodeThinkingEnvelope(map[string]interface{}{"type": "thinking", "thinking": "x"}) != "" {
		t.Errorf("无签名 thinking 不应编码")
	}
	if decodeThinkingEnvelope("ccswitch-openai-reasoning-v1:aaaa") != nil {
		t.Errorf("别家信封不应解出")
	}
}

// ---- Response translation (non-streaming JSON→JSON) ----

func TestAnthropicToResponsesObject(t *testing.T) {
	msg := map[string]interface{}{
		"id":          "msg_1",
		"model":       "deepseek-v4-flash",
		"stop_reason": "end_turn",
		"content": []interface{}{
			map[string]interface{}{"type": "thinking", "thinking": "想", "signature": "sig"},
			map[string]interface{}{"type": "text", "text": "答案"},
			map[string]interface{}{"type": "tool_use", "id": "toolu_1", "name": "get_weather",
				"input": map[string]interface{}{"city": "北京"}},
		},
		"usage": map[string]interface{}{
			"input_tokens": 77, "output_tokens": 9, "cache_read_input_tokens": 11,
		},
	}
	out := anthropicToResponsesObject(msg, "gpt-5-codex", nil, nil, false)
	if out["id"] != "resp_msg_1" || out["object"] != "response" || out["status"] != "completed" {
		t.Errorf("骨架: id=%v object=%v status=%v", out["id"], out["object"], out["status"])
	}
	if out["model"] != "deepseek-v4-flash" {
		t.Errorf("model=%v（应取上游回传的）", out["model"])
	}
	output := asArr(out["output"])
	if len(output) != 3 {
		t.Fatalf("output 数=%d, want 3: %v", len(output), output)
	}
	rs := asObj(output[0])
	if objStr(rs, "type") != "reasoning" || objStr(rs, "encrypted_content") == "" {
		t.Errorf("reasoning 项: %v", rs)
	}
	if decodeThinkingEnvelope(objStr(rs, "encrypted_content")) == nil {
		t.Errorf("reasoning.encrypted_content 应能还原 thinking 块")
	}
	fc := asObj(output[2])
	if objStr(fc, "type") != "function_call" || objStr(fc, "call_id") != "toolu_1" {
		t.Errorf("function_call 项: %v", fc)
	}
	var args map[string]interface{}
	if err := json.Unmarshal([]byte(objStr(fc, "arguments")), &args); err != nil || args["city"] != "北京" {
		t.Errorf("arguments=%v err=%v", objStr(fc, "arguments"), err)
	}
	usage := asObj(out["usage"])
	if toInt64(usage["input_tokens"]) != 88 || toInt64(usage["output_tokens"]) != 9 {
		t.Errorf("usage=%v, want input=88(77+11缓存) output=9", usage)
	}
	if toInt64(asObj(usage["input_tokens_details"])["cached_tokens"]) != 11 {
		t.Errorf("cached_tokens 应=11: %v", usage)
	}
}

// ---- Streaming state machine ----

// feedSSEToConv feeds SSE text into the state machine block by block.
func feedSSEToConv(conv *anthToRespStream, sse string) {
	for _, block := range strings.Split(sse, "\n\n") {
		block = strings.TrimSpace(block)
		if block == "" {
			continue
		}
		var event string
		var dataLines []string
		for _, line := range strings.Split(block, "\n") {
			if strings.HasPrefix(line, "event:") {
				event = strings.TrimSpace(line[len("event:"):])
			} else if strings.HasPrefix(line, "data:") {
				dataLines = append(dataLines, strings.TrimPrefix(line[len("data:"):], " "))
			}
		}
		var data map[string]interface{}
		if err := json.Unmarshal([]byte(strings.Join(dataLines, "\n")), &data); err != nil {
			continue
		}
		conv.handleEvent(event, data)
	}
}

func TestAnthToRespStreamAllBlocks(t *testing.T) {
	var events []string
	conv := newAnthToRespStream(func(ev string) { events = append(events, ev) }, "gpt-5-codex", nil)
	feedSSEToConv(conv, testSSEAllBlocks())

	if !conv.completed {
		t.Fatalf("流未完成")
	}
	joined := strings.Join(events, "")
	for _, want := range []string{
		"event: response.created",
		"event: response.in_progress",
		"event: response.output_item.added",
		"event: response.reasoning_summary_text.delta",
		"event: response.function_call_arguments.delta",
		"event: response.output_text.delta",
		"event: response.completed",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("事件流缺 %s", want)
		}
	}
	// response.created must come before output_item.added.
	if strings.Index(joined, "response.created") > strings.Index(joined, "response.output_item.added") {
		t.Errorf("事件顺序错误：created 应最先")
	}

	final := conv.buildFinalResponse()
	output := asArr(final["output"])
	// reasoning + web_search_call(call) + web_search_call(sources) + function_call + message = 5
	if len(output) != 5 {
		t.Fatalf("output 数=%d, want 5: %v", len(output), output)
	}
	// The reasoning item's envelope restores the thinking block (signature included).
	rs := asObj(output[0])
	dec := decodeThinkingEnvelope(objStr(rs, "encrypted_content"))
	if dec == nil || objStr(dec, "signature") != "sig_abc" {
		t.Errorf("reasoning 信封还原失败: %v", dec)
	}
	// Real search: the call item's action carries query, the result item's action carries sources.
	var wsActions []map[string]interface{}
	for _, it := range output {
		if objStr(asObj(it), "type") == "web_search_call" {
			wsActions = append(wsActions, asObj(asObj(it)["action"]))
		}
	}
	if len(wsActions) != 2 {
		t.Fatalf("web_search_call 数=%d, want 2: %v", len(wsActions), output)
	}
	if objStr(wsActions[0], "query") != "测试查询" {
		t.Errorf("搜索调用项 action=%v, want query=测试查询", wsActions[0])
	}
	if len(asArr(wsActions[1]["sources"])) != 1 {
		t.Errorf("搜索结果项 action=%v, want 1 条 sources", wsActions[1])
	}
	// function_call arguments fully assembled.
	var fcItem map[string]interface{}
	for _, it := range output {
		if objStr(asObj(it), "type") == "function_call" {
			fcItem = asObj(it)
		}
	}
	if fcItem == nil {
		t.Fatalf("缺 function_call 项")
	}
	var args map[string]interface{}
	if err := json.Unmarshal([]byte(objStr(fcItem, "arguments")), &args); err != nil || args["city"] != "北京" {
		t.Errorf("arguments=%v", objStr(fcItem, "arguments"))
	}
	// usage merge: input 77+cache 11=88, output 9.
	usage := asObj(final["usage"])
	if toInt64(usage["input_tokens"]) != 88 || toInt64(usage["output_tokens"]) != 9 {
		t.Errorf("usage=%v", usage)
	}
	if final["status"] != "completed" {
		t.Errorf("status=%v", final["status"])
	}
}

// testSSEEmptySearchTriplet builds the "empty-search triple" stream observed live on Kimi k3-256k:
// an empty-preamble text block ("Search results for query: ", query empty) + an id-less/input-less
// server_tool_use + a content-empty web_search_tool_result, followed by thinking + body.
// (Online probe evidence: every response starts with this placeholder set when the request carries the web_search tool.)
func testSSEEmptySearchTriplet() string {
	return "" +
		`event: message_start` + "\n" +
		`data: {"type":"message_start","message":{"id":"msg_k1","type":"message","role":"assistant","model":"k3-256k","content":[],"stop_reason":null,"usage":{"input_tokens":10,"output_tokens":0}}}` + "\n\n" +
		`event: content_block_start` + "\n" +
		`data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}` + "\n\n" +
		`event: content_block_delta` + "\n" +
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"Search results for query: "}}` + "\n\n" +
		`event: content_block_stop` + "\n" +
		`data: {"type":"content_block_stop","index":0}` + "\n\n" +
		`event: content_block_start` + "\n" +
		`data: {"type":"content_block_start","index":1,"content_block":{"type":"server_tool_use","name":"web_search"}}` + "\n\n" +
		`event: content_block_stop` + "\n" +
		`data: {"type":"content_block_stop","index":1}` + "\n\n" +
		`event: content_block_start` + "\n" +
		`data: {"type":"content_block_start","index":2,"content_block":{"type":"web_search_tool_result","content":[]}}` + "\n\n" +
		`event: content_block_stop` + "\n" +
		`data: {"type":"content_block_stop","index":2}` + "\n\n" +
		`event: content_block_start` + "\n" +
		`data: {"type":"content_block_start","index":3,"content_block":{"type":"thinking","thinking":"","signature":""}}` + "\n\n" +
		`event: content_block_delta` + "\n" +
		`data: {"type":"content_block_delta","index":3,"delta":{"type":"thinking_delta","thinking":"想想"}}` + "\n\n" +
		`event: content_block_delta` + "\n" +
		`data: {"type":"content_block_delta","index":3,"delta":{"type":"signature_delta","signature":"sig_k"}}` + "\n\n" +
		`event: content_block_stop` + "\n" +
		`data: {"type":"content_block_stop","index":3}` + "\n\n" +
		`event: content_block_start` + "\n" +
		`data: {"type":"content_block_start","index":4,"content_block":{"type":"text","text":""}}` + "\n\n" +
		`event: content_block_delta` + "\n" +
		`data: {"type":"content_block_delta","index":4,"delta":{"type":"text_delta","text":"答案"}}` + "\n\n" +
		`event: content_block_stop` + "\n" +
		`data: {"type":"content_block_stop","index":4}` + "\n\n" +
		`event: message_delta` + "\n" +
		`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"input_tokens":10,"output_tokens":5}}` + "\n\n" +
		`event: message_stop` + "\n" +
		`data: {"type":"message_stop"}` + "\n\n"
}

// TestAnthToRespStreamDropsEmptySearchTriplet verifies the streaming path drops the empty-search triple wholesale:
// the preamble text never appears in the event stream, no web_search_call item is produced, thinking/body unaffected.
func TestAnthToRespStreamDropsEmptySearchTriplet(t *testing.T) {
	var events []string
	conv := newAnthToRespStream(func(ev string) { events = append(events, ev) }, "k3-256k", nil)
	feedSSEToConv(conv, testSSEEmptySearchTriplet())
	if !conv.completed {
		t.Fatalf("流未完成")
	}
	joined := strings.Join(events, "")
	if strings.Contains(joined, "Search results for query") {
		t.Errorf("空搜索前言文本应整块丢弃，事件流不应出现")
	}
	if strings.Contains(joined, "web_search_call") {
		t.Errorf("空搜索不应产生 web_search_call 项")
	}
	if !strings.Contains(joined, "答案") {
		t.Errorf("正文文本不应受影响")
	}
	final := conv.buildFinalResponse()
	output := asArr(final["output"])
	// Only reasoning + message remain (preamble/empty-call/empty-result dropped).
	if len(output) != 2 {
		t.Fatalf("output 数=%d, want 2: %v", len(output), output)
	}
	if objStr(asObj(output[0]), "type") != "reasoning" {
		t.Errorf("output[0]=%v, want reasoning", output[0])
	}
	msg := asObj(output[1])
	if objStr(msg, "type") != "message" {
		t.Fatalf("output[1]=%v, want message", output[1])
	}
	text := objStr(asObj(asArr(msg["content"])[0]), "text")
	if text != "答案" {
		t.Errorf("正文=%q, want 答案", text)
	}
}

// TestAnthToRespStreamStripsRepeatedPreamble reproduces the observed stream: the preamble repeats twice, glued token by token
// onto the body's start (the Kimi model imitating the preamble pattern from history). After translation the body should be just "我确认一下",
// and the concatenated output deltas shouldn't contain the preamble either.
func TestAnthToRespStreamStripsRepeatedPreamble(t *testing.T) {
	sse := "" +
		`event: message_start` + "\n" +
		`data: {"type":"message_start","message":{"id":"msg_k4","type":"message","role":"assistant","model":"k3-256k","content":[],"stop_reason":null,"usage":{"input_tokens":10,"output_tokens":0}}}` + "\n\n" +
		`event: content_block_start` + "\n" +
		`data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}` + "\n\n"
	// The preamble is fed in fragments matching the observed broken-delta shape.
	for _, d := range []string{"Search", " results", " for", " query", ":",
		" Search", " results", " for", " query", ":", " ", "我", "确认一下"} {
		b, _ := json.Marshal(map[string]interface{}{"type": "text_delta", "text": d})
		sse += `event: content_block_delta` + "\n" +
			`data: {"type":"content_block_delta","index":0,"delta":` + string(b) + `}` + "\n\n"
	}
	sse += `event: content_block_stop` + "\n" +
		`data: {"type":"content_block_stop","index":0}` + "\n\n" +
		`event: message_delta` + "\n" +
		`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":5}}` + "\n\n" +
		`event: message_stop` + "\n" +
		`data: {"type":"message_stop"}` + "\n\n"

	var events []string
	conv := newAnthToRespStream(func(ev string) { events = append(events, ev) }, "k3-256k", nil)
	feedSSEToConv(conv, sse)
	if !conv.completed {
		t.Fatalf("流未完成")
	}
	// The output_text.delta events concatenated should be exactly the body, preamble-free.
	var deltas strings.Builder
	for _, ev := range events {
		for _, line := range strings.Split(ev, "\n") {
			if !strings.HasPrefix(line, "data: ") || !strings.Contains(line, "output_text.delta") {
				continue
			}
			var m map[string]interface{}
			if json.Unmarshal([]byte(line[6:]), &m) == nil {
				deltas.WriteString(objStr(m, "delta"))
			}
		}
	}
	if deltas.String() != "我确认一下" {
		t.Errorf("delta 拼接=%q, want 我确认一下（前言应剥离）", deltas.String())
	}
	output := asArr(conv.buildFinalResponse()["output"])
	if len(output) != 1 {
		t.Fatalf("output 数=%d, want 1: %v", len(output), output)
	}
	msg := asObj(output[0])
	if got := objStr(asObj(asArr(msg["content"])[0]), "text"); got != "我确认一下" {
		t.Errorf("最终消息=%q, want 我确认一下", got)
	}
}

// TestAnthToRespStreamDropsSearchQueryEcho reproduces the observed 08-30 weather-session shape: a single preamble immediately followed by
// the real query text. The query echo line is deleted whole — neither the event stream nor the final output contains that line (the block produces no events at all).
func TestAnthToRespStreamDropsSearchQueryEcho(t *testing.T) {
	sse := "" +
		`event: message_start` + "\n" +
		`data: {"type":"message_start","message":{"id":"msg_k5","type":"message","role":"assistant","model":"k3-256k","content":[],"stop_reason":null,"usage":{"input_tokens":10,"output_tokens":0}}}` + "\n\n" +
		`event: content_block_start` + "\n" +
		`data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}` + "\n\n"
	for _, d := range []string{"Search", " results", " for", " query", ": ", "广州", " 天气"} {
		b, _ := json.Marshal(map[string]interface{}{"type": "text_delta", "text": d})
		sse += `event: content_block_delta` + "\n" +
			`data: {"type":"content_block_delta","index":0,"delta":` + string(b) + `}` + "\n\n"
	}
	sse += `event: content_block_stop` + "\n" +
		`data: {"type":"content_block_stop","index":0}` + "\n\n" +
		`event: message_delta` + "\n" +
		`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":5}}` + "\n\n" +
		`event: message_stop` + "\n" +
		`data: {"type":"message_stop"}` + "\n\n"

	var events []string
	conv := newAnthToRespStream(func(ev string) { events = append(events, ev) }, "k3-256k", nil)
	feedSSEToConv(conv, sse)
	if !conv.completed {
		t.Fatalf("流未完成")
	}
	joined := strings.Join(events, "")
	if strings.Contains(joined, "Search results for query") || strings.Contains(joined, "广州") {
		t.Errorf("query 回声行应整行删除，事件流不应出现其任何片段")
	}
	if strings.Contains(joined, "output_text") {
		t.Errorf("纯回声块不应产生任何 output_text 事件")
	}
	if output := asArr(conv.buildFinalResponse()["output"]); len(output) != 0 {
		t.Fatalf("output 数=%d, want 0（纯回声块不进输出）: %v", len(output), output)
	}
}

// TestAnthropicToResponsesObjectDropsEmptySearch verifies the non-streaming JSON wholesale-conversion path also filters the empty-search triple.
func TestAnthropicToResponsesObjectDropsEmptySearch(t *testing.T) {
	msg := map[string]interface{}{
		"id": "msg_k2", "model": "k3-256k", "stop_reason": "end_turn",
		"content": []interface{}{
			map[string]interface{}{"type": "text", "text": "Search results for query: "},
			map[string]interface{}{"type": "server_tool_use", "name": "web_search"},
			map[string]interface{}{"type": "web_search_tool_result", "content": []interface{}{}},
			map[string]interface{}{"type": "text", "text": "正文"},
		},
		"usage": map[string]interface{}{"input_tokens": 1, "output_tokens": 1},
	}
	out := anthropicToResponsesObject(msg, "k3-256k", nil, nil, false)
	output := asArr(out["output"])
	if len(output) != 1 {
		t.Fatalf("output 数=%d, want 1（空搜索三连全丢）: %v", len(output), output)
	}
	m := asObj(output[0])
	text := objStr(asObj(asArr(m["content"])[0]), "text")
	if objStr(m, "type") != "message" || text != "正文" {
		t.Errorf("output[0]=%v, want message(正文)", output[0])
	}
}

// TestAnthropicToResponsesObjectKeepsRealSearch verifies real search's structured items are unaffected:
// the call item carries query, the result item carries sources; the query echo text line is deleted whole (no longer enters the output).
func TestAnthropicToResponsesObjectKeepsRealSearch(t *testing.T) {
	msg := map[string]interface{}{
		"id": "msg_k3", "model": "k3-256k", "stop_reason": "end_turn",
		"content": []interface{}{
			map[string]interface{}{"type": "text", "text": "Search results for query: pandas 版本"},
			map[string]interface{}{"type": "server_tool_use", "id": "srvtoolu_r", "name": "web_search",
				"input": map[string]interface{}{"query": "pandas 版本"}},
			map[string]interface{}{"type": "web_search_tool_result", "tool_use_id": "srvtoolu_r",
				"content": []interface{}{map[string]interface{}{"type": "web_search_result", "title": "pypi", "url": "https://pypi.org/p/pandas"}}},
			map[string]interface{}{"type": "text", "text": "答案"},
		},
		"usage": map[string]interface{}{"input_tokens": 1, "output_tokens": 1},
	}
	out := anthropicToResponsesObject(msg, "k3-256k", nil, nil, false)
	output := asArr(out["output"])
	// Echo text deleted: web_search_call(call) + web_search_call(sources) + message(answer) = 3
	if len(output) != 3 {
		t.Fatalf("output 数=%d, want 3: %v", len(output), output)
	}
	if b, _ := json.Marshal(output); strings.Contains(string(b), "Search results for query") {
		t.Errorf("query 回声行不应出现在输出: %s", b)
	}
	call := asObj(output[0])
	if objStr(asObj(call["action"]), "query") != "pandas 版本" {
		t.Errorf("调用项 action=%v, want query=pandas 版本", call["action"])
	}
	src := asObj(output[1])
	if len(asArr(asObj(src["action"])["sources"])) != 1 {
		t.Errorf("结果项 action=%v, want 1 条 sources", src["action"])
	}
	m := asObj(output[2])
	if got := objStr(asObj(asArr(m["content"])[0]), "text"); got != "答案" {
		t.Errorf("正文=%q, want 答案", got)
	}
}

// TestStripRepeatedPreamble locks the preamble split rules: only ≥2 consecutive preambles are stripped bare (the model's imitation signature);
// single preamble+text and preamble-free text are kept as-is (whole-line echo deletion is stripSearchQueryEcho's job).
func TestStripRepeatedPreamble(t *testing.T) {
	const m = "Search results for query: "
	cases := []struct{ in, want string }{
		{"", ""},
		{"正文", "正文"},
		{m, m}, // Single bare preamble: untouched by this function (whole-line deletion lives in stripSearchQueryEcho)
		{m + "广州 天气", m + "广州 天气"}, // Single preamble+query: untouched by this function
		{m + m, ""}, // Repeated bare preambles: stripped to empty
		{m + m + "我确认一下", "我确认一下"},
		{m + m + m + "我确认一下", "我确认一下"},
	}
	for _, c := range cases {
		if got := stripRepeatedPreamble(c.in); got != c.want {
			t.Errorf("stripRepeatedPreamble(%q)=%q, want %q", c.in, got, c.want)
		}
	}
}

// TestStripSearchQueryEcho locks the echo-line deletion rules: lines starting with a single preamble (query included) are deleted whole;
// repeated bare preambles glued to body text are stripped bare keeping the body; echo-free text passes byte for byte; line-leading blank lines left by deletion are removed.
func TestStripSearchQueryEcho(t *testing.T) {
	const m = "Search results for query: "
	cases := []struct{ in, want string }{
		{"", ""},
		{"正文", "正文"},
		{"a\n\nb", "a\n\nb"},        // No echo: blank lines kept as-is, not a byte touched
		{m, ""},                     // Pure preamble
		{strings.TrimSpace(m), ""},  // Bare-preamble form without the trailing space
		{m + "广州 天气", ""},           // An echo line owning a whole block: deleted empty
		{m + "广州 天气\n正文", "正文"},     // Echo line + body: delete the line, keep the body
		{m + "广州 天气\n\n\n正文", "正文"}, // Line-leading blank lines left by deletion are removed
		{"前文\n" + m + "广州\n后文", "前文\n后文"}, // Echo lines mid-text are also deleted (history replay direction)
		{m + m + "我确认一下", "我确认一下"},        // Imitation signature: strip bare preambles, keep the same-line body
		{m + m + m + "我确认一下", "我确认一下"},
	}
	for _, c := range cases {
		if got := stripSearchQueryEcho(c.in); got != c.want {
			t.Errorf("stripSearchQueryEcho(%q)=%q, want %q", c.in, got, c.want)
		}
	}
}

// TestConvertInputDropsSearchQueryEcho verifies the upstream replay direction: echo lines are deleted from message texts
// across the whole request body (an assistant message reduced to only echo vanishes wholesale), and web_search_call items Codex hollowed out aren't replayed.
func TestConvertInputDropsSearchQueryEcho(t *testing.T) {
	const m = "Search results for query: "
	items := []interface{}{
		map[string]interface{}{"type": "message", "role": "user", "content": "广州天气如何"},
		map[string]interface{}{"type": "message", "role": "assistant", "content": []interface{}{
			map[string]interface{}{"type": "output_text", "text": m + "广州 天气"},
		}},
		map[string]interface{}{"type": "web_search_call", "id": "ws_1", "status": "completed"},
		map[string]interface{}{"type": "message", "role": "assistant", "content": []interface{}{
			map[string]interface{}{"type": "output_text", "text": m + "广州 天气\n今天广州晴，25°C"},
		}},
		map[string]interface{}{"type": "message", "role": "user", "content": "详细说说"},
	}
	msgs, err := convertInputToMessages(items, buildToolRegistry(nil), nil, nil)
	if err != nil {
		t.Fatalf("convertInputToMessages: %v", err)
	}
	b, _ := json.Marshal(msgs)
	if strings.Contains(string(b), "Search results for query") {
		t.Errorf("回声行应全部删除: %s", b)
	}
	if strings.Contains(string(b), "web_search") {
		t.Errorf("空 web_search 结构不应回放: %s", b)
	}
	// user(广州天气如何) + assistant(今天广州晴，25°C) + user(详细说说) = 3 messages
	// (the assistant message reduced to only echo vanishes wholesale)
	if len(msgs) != 3 {
		t.Fatalf("消息数=%d, want 3: %s", len(msgs), b)
	}
	if got := objStr(asObj(asArr(msgs[1]["content"])[0]), "text"); got != "今天广州晴，25°C" {
		t.Errorf("assistant 文本=%q, want 今天广州晴，25°C（回声行已删）", got)
	}
}

// TestConvertInputKeepsNonEmptySearch verifies search-structure triage in the replay direction: web_search_call
// call items (even with query/sources) are never restored — their ids are proxy-minted, never registered in the upstream registry,
// so going upstream must 400 with the envelope pair punished along (production #3/#19 evidence); ones already in Anthropic shape
// server_tool_use/web_search_tool_result go upstream as-is when content-bearing; shells (no input/no
// content/no valid sources) are deleted.
func TestConvertInputKeepsNonEmptySearch(t *testing.T) {
	items := []interface{}{
		map[string]interface{}{"type": "message", "role": "user", "content": "查下 pandas 版本"},
		map[string]interface{}{"type": "web_search_call", "id": "ws_1", "status": "completed",
			"action": map[string]interface{}{"type": "search", "query": "pandas 最新版本",
				"sources": []interface{}{
					map[string]interface{}{"type": "url", "url": "https://pandas.pydata.org"},
					map[string]interface{}{"type": "url"}, // Sources without URLs
				}}},
		map[string]interface{}{"type": "server_tool_use", "id": "srvtoolu_1", "name": "web_search",
			"input": map[string]interface{}{"query": "广州天气"}},
		map[string]interface{}{"type": "server_tool_use", "id": "srvtoolu_2", "name": "web_search"}, // No input: delete
		map[string]interface{}{"type": "web_search_tool_result", "tool_use_id": "srvtoolu_1",
			"content": []interface{}{map[string]interface{}{"type": "web_search_result", "url": "https://a.cn"}}},
		map[string]interface{}{"type": "web_search_tool_result", "tool_use_id": "srvtoolu_2"}, // No content: delete
		map[string]interface{}{"type": "message", "role": "assistant", "content": "pandas 2.3"},
	}
	msgs, err := convertInputToMessages(items, buildToolRegistry(nil), nil, nil)
	if err != nil {
		t.Fatalf("convertInputToMessages: %v", err)
	}
	b, _ := json.Marshal(msgs)
	if strings.Contains(string(b), "srvtoolu_2") {
		t.Errorf("空 server_tool_use/web_search_tool_result 应删除: %s", b)
	}
	if strings.Contains(string(b), "ws_1") {
		t.Errorf("web_search_call 调用项不应还原上行（代理自造 id 必 400）: %s", b)
	}
	// user + assistant (left with only the defensively covered srvtoolu_1 pair + text)
	if len(msgs) != 2 {
		t.Fatalf("消息数=%d, want 2: %s", len(msgs), b)
	}
	content := asArr(msgs[1]["content"])
	wantTypes := []string{"server_tool_use", "web_search_tool_result", "text"}
	if len(content) != len(wantTypes) {
		t.Fatalf("assistant 块数=%d, want %d: %s", len(content), len(wantTypes), b)
	}
	for i, wt := range wantTypes {
		if got := objStr(asObj(content[i]), "type"); got != wt {
			t.Errorf("块[%d].type=%q, want %q", i, got, wt)
		}
	}
	// The pair going upstream as-is: ids stay the natively registered ids
	stu := asObj(content[0])
	if objStr(asObj(stu["input"]), "query") != "广州天气" || objStr(stu, "id") != "srvtoolu_1" {
		t.Errorf("原样上行的 server_tool_use 不对: %v", stu)
	}
	wstr := asObj(content[1])
	if objStr(wstr, "tool_use_id") != "srvtoolu_1" {
		t.Errorf("原样上行的 web_search_tool_result 不对: %v", wstr)
	}
}

// TestConvertInputMediaFallback verifies attachment parts aren't silently dropped: standard shapes (data URL images,
// top-level bare input_file) convert normally into image/document blocks; unrecognized shapes (blob: URLs, file_id
// cloud references) serialize into text blocks as fallback (see pushMediaPart).
func TestConvertInputMediaFallback(t *testing.T) {
	items := []interface{}{
		map[string]interface{}{"type": "input_image",
			"image_url": "data:image/png;base64,iVBORw0KGgo="},
		map[string]interface{}{"type": "input_file", "filename": "a.pdf",
			"file_data": "data:application/pdf;base64,JVBERi0="},
		map[string]interface{}{"type": "message", "role": "user", "content": []interface{}{
			map[string]interface{}{"type": "input_image", "image_url": "blob:https://app/x-y"},
			map[string]interface{}{"type": "input_file", "file_id": "file-abc123"},
		}},
	}
	msgs, err := convertInputToMessages(items, buildToolRegistry(nil), nil, nil)
	if err != nil {
		t.Fatalf("convertInputToMessages: %v", err)
	}
	b, _ := json.Marshal(msgs)
	s := string(b)
	if !strings.Contains(s, `"image"`) || !strings.Contains(s, "iVBORw0KGgo=") {
		t.Errorf("标准图片应转 image 块: %s", s)
	}
	if !strings.Contains(s, `"document"`) || !strings.Contains(s, "JVBERi0=") {
		t.Errorf("顶层裸 input_file 应转 document 块: %s", s)
	}
	if !strings.Contains(s, "blob:https://app/x-y") {
		t.Errorf("blob: 图片应序列化成文本兜底，不应静默丢: %s", s)
	}
	if !strings.Contains(s, "file-abc123") {
		t.Errorf("file_id 附件应序列化成文本兜底，不应静默丢: %s", s)
	}
	// All into the same user message: image + document + text fallback×2 = 4 blocks
	if len(msgs) != 1 {
		t.Fatalf("消息数=%d, want 1: %s", len(msgs), s)
	}
	if n := len(asArr(msgs[0]["content"])); n != 4 {
		t.Fatalf("块数=%d, want 4: %s", n, s)
	}
}

// TestTranslatingWriterNonStream verifies a stream:false client gets a one-shot Responses JSON.
func TestTranslatingWriterNonStream(t *testing.T) {
	rec := httptest.NewRecorder()
	tw := newTranslatingWriter(rec, false, "gpt-5-codex", nil)
	tw.Header().Set("Content-Type", "text/event-stream")
	tw.WriteHeader(200)
	if _, err := tw.Write([]byte(testSSEAllBlocks())); err != nil {
		t.Fatalf("Write: %v", err)
	}
	tw.finish()

	resp := rec.Result()
	if ct := resp.Header.Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type=%q, want application/json", ct)
	}
	var m map[string]interface{}
	if err := json.NewDecoder(resp.Body).Decode(&m); err != nil {
		t.Fatalf("响应不是 JSON: %v", err)
	}
	if m["object"] != "response" || m["status"] != "completed" {
		t.Errorf("object=%v status=%v", m["object"], m["status"])
	}
	if len(asArr(m["output"])) != 5 {
		t.Errorf("output 数=%d, want 5", len(asArr(m["output"])))
	}
}

// TestTranslatingWriterStreamSplitWrite verifies a streaming client + parsing of partial lines across Writes.
func TestTranslatingWriterStreamSplitWrite(t *testing.T) {
	rec := httptest.NewRecorder()
	tw := newTranslatingWriter(rec, true, "gpt-5-codex", nil)
	tw.Header().Set("Content-Type", "text/event-stream")
	tw.WriteHeader(200)
	// Split a complete SSE block in half across two writes, simulating a partial line.
	full := testSSEAllBlocks()
	mid := len(full) / 2
	if _, err := tw.Write([]byte(full[:mid])); err != nil {
		t.Fatalf("Write1: %v", err)
	}
	if _, err := tw.Write([]byte(full[mid:])); err != nil {
		t.Fatalf("Write2: %v", err)
	}
	tw.finish()

	body := rec.Body.String()
	if !strings.Contains(body, "event: response.created") ||
		!strings.Contains(body, "event: response.completed") {
		t.Errorf("流式输出缺 created/completed 事件")
	}
	if !strings.Contains(body, "response.output_text.delta") {
		t.Errorf("流式输出缺 output_text.delta")
	}
	if rec.Result().Header.Get("Content-Type") != "text/event-stream" {
		t.Errorf("Content-Type 应为 text/event-stream")
	}
}

// TestResponsesHandlerEndToEnd end-to-end: Responses request → translation → main pipeline → fake-upstream SSE →
// translated back to a Responses JSON (non-streaming client).
func TestResponsesHandlerEndToEnd(t *testing.T) {
	resetStats()
	var gotBody map[string]interface{}
	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		var m map[string]interface{}
		_ = json.Unmarshal(b, &m)
		gotBody = m
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		io.WriteString(w, testSSEAllBlocks())
	}))
	defer mock.Close()

	cfg.Store(&Config{
		Upstream:       mock.URL,
		MaxRetries:     0,
		TotalBudgetSec: 10,
		Routes: []RouteRule{
			{Pattern: "gpt-5*", URL: mock.URL, Model: "deepseek-v4-flash"},
		},
	})

	proxy := httptest.NewServer(http.HandlerFunc(responsesHandler))
	defer proxy.Close()

	reqBody := `{"model":"gpt-5-codex","input":"hello","instructions":"Be helpful."}`
	resp, err := http.Post(proxy.URL+"/v1/responses", "application/json", strings.NewReader(reqBody))
	if err != nil {
		t.Fatalf("请求失败: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()

	// What the upstream receives should be the translated Anthropic request: system from instructions, stream forced true,
	// model rewritten by routing.
	if objStr(gotBody, "system") != "Be helpful." {
		t.Errorf("上游 system=%v", gotBody["system"])
	}
	if s, _ := gotBody["stream"].(bool); !s {
		t.Errorf("上游 stream=%v, want true（内部强制流式）", gotBody["stream"])
	}
	if objStr(gotBody, "model") != "deepseek-v4-flash" {
		t.Errorf("上游 model=%v, want 路由改写 deepseek-v4-flash", gotBody["model"])
	}
	msgs := asArr(gotBody["messages"])
	if len(msgs) != 1 {
		t.Errorf("上游 messages=%v", gotBody["messages"])
	}

	// The client gets a Responses JSON.
	var m map[string]interface{}
	if err := json.Unmarshal(body, &m); err != nil {
		t.Fatalf("响应不是 JSON: %v (%s)", err, string(body))
	}
	if m["object"] != "response" {
		t.Fatalf("object=%v, body=%s", m["object"], string(body))
	}
	// model written back to the client's original name (the pipeline's model write-back logic).
	if m["model"] != "gpt-5-codex" {
		t.Errorf("model=%v, want gpt-5-codex", m["model"])
	}
	// Search envelope riding along (triple injected after routing settled): original 5 items + envelope reasoning item = 6.
	if len(asArr(m["output"])) != 6 {
		t.Errorf("output 数=%d, want 6（含搜索信封项）", len(asArr(m["output"])))
	}
}

// TestResponsesFastRouteEntry verifies the Codex menu's fast entry (model name literal "fast_route") gets
// speed:"fast" injected by the translation layer, and the main pipeline's fast branch takes over: the upstream receives the model
// rewritten to fast_route.model, with the speed field already removed.
func TestResponsesFastRouteEntry(t *testing.T) {
	resetStats()
	var gotBody map[string]interface{}
	fastMock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &gotBody)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		io.WriteString(w, testSSEAllBlocks())
	}))
	defer fastMock.Close()

	cfg.Store(&Config{
		MaxRetries: 0, TotalBudgetSec: 10,
		FastRoute: &FastRoute{URL: fastMock.URL, Model: "k3-256k"},
	})
	defer cfg.Store(&Config{})

	proxy := httptest.NewServer(http.HandlerFunc(responsesHandler))
	defer proxy.Close()

	resp, err := http.Post(proxy.URL+"/v1/responses", "application/json",
		strings.NewReader(`{"model":"fast_route","input":"hi"}`))
	if err != nil {
		t.Fatalf("请求失败: %v", err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()

	if objStr(gotBody, "model") != "k3-256k" {
		t.Errorf("fast 通道上游 model=%v, want k3-256k（fast_route.model 改写）", gotBody["model"])
	}
	if _, has := gotBody["speed"]; has {
		t.Errorf("speed 字段应被 fast 分支移除，上游却收到: %v", gotBody["speed"])
	}
}

// TestReconcileResponsesServer verifies the listener port starts/stops dynamically with config:
// startup → same-address idempotent → re-address (old closed, new opened) → disabled (closed).
func TestReconcileResponsesServer(t *testing.T) {
	cfg.Store(&Config{}) // The handler does cfg.Load() per request; must not be nil
	freeAddr := func() string {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		a := ln.Addr().String()
		_ = ln.Close()
		return a
	}
	get := func(addr string) error {
		resp, err := http.Get("http://" + addr + "/responses")
		if err == nil {
			resp.Body.Close()
		}
		return err
	}

	addr := freeAddr()
	reconcileResponsesServer(addr)
	defer reconcileResponsesServer("") // Leave no listener behind at wrap-up
	if err := get(addr); err != nil {
		t.Fatalf("监听口未起来: %v", err)
	}

	reconcileResponsesServer(addr) // Same address is idempotent, no rebinding
	if err := get(addr); err != nil {
		t.Fatalf("同地址 reconcile 后监听丢了: %v", err)
	}

	addr2 := freeAddr()
	reconcileResponsesServer(addr2) // Address change: old closed, new opened
	if err := get(addr); err == nil {
		t.Fatal("旧地址应已关闭")
	}
	if err := get(addr2); err != nil {
		t.Fatalf("新地址未起来: %v", err)
	}

	reconcileResponsesServer("") // Disabled
	if err := get(addr2); err == nil {
		t.Fatal("停用后地址应已关闭")
	}
}
