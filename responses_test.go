package main

// cc-switch: https://github.com/farion1231/cc-switch — MIT License, Copyright (c) 2025 Jason Young.

// responses_test.go — Responses API 监听口的翻译层测试。
// 请求转换用例移植自 cc-switch transform_codex_anthropic.rs 的测试集。

import (
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// ---- 请求翻译 ----

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
	// 两个连续 function_call + 两个 function_call_output：
	// 调用并入同一条 assistant 消息，结果合并进同一条 user 消息。
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
	// 孤儿 function_call_output（没有配对的 function_call）：整条丢弃，
	// 留下的空 user 也被清掉，最终只剩开头的用户问题。
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
	// 历史以 assistant 开头（压缩/恢复的会话）：自动补前导 user。
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
	// assistant tool_use 没有对应 tool_result：整个工具轮丢弃，保留普通对话。
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
	// 两条 user 各自保留（未配对工具轮丢弃后，两条普通对话不受影响）。
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
	// budget 压到 max_tokens/2=16000（默认 max_tokens 32000，16384 超半）。
	if objStr(th, "type") != "enabled" || toInt64(th["budget_tokens"]) != 16000 {
		t.Errorf("thinking=%v, want enabled/16000（max_tokens 半压顶）", th)
	}
}

// TestResponsesToAnthropicThinkStyleOverride 锁定路由 thinking 参数（目标模型思考形态
// 声明）对翻译判定的覆盖：auto 维持按客户端 model 名查表；budget 强制经典
// enabled+budget_tokens（即使客户端别名叫 adaptive 表内模型）；adaptive 强制
// adaptive+output_config.effort（即使客户端名不在表内）；强制后关思考仍允许
// （cannotDisable 不再套用 fable/mythos 规则）。
func TestResponsesToAnthropicThinkStyleOverride(t *testing.T) {
	mk := func(model, effort string) map[string]interface{} {
		return map[string]interface{}{
			"model":     model,
			"input":     "hi",
			"reasoning": map[string]interface{}{"effort": effort},
		}
	}

	// auto：fable-5 表内 → adaptive + effort 映射（high→high）。
	out, _, _, err := responsesToAnthropicTriple(mk("claude-fable-5", "high"), nil, nil, "", false)
	if err != nil {
		t.Fatalf("auto err: %v", err)
	}
	th := asObj(out["thinking"])
	if objStr(th, "type") != "adaptive" || objStr(asObj(out["output_config"]), "effort") != "high" {
		t.Errorf("auto fable-5: thinking=%v output_config=%v, want adaptive/high", th, out["output_config"])
	}

	// budget 强制：同为 fable-5 别名，路由声明 budget → enabled+16000（压顶），无 output_config。
	out, _, _, err = responsesToAnthropicTriple(mk("claude-fable-5", "high"), nil, nil, "budget", false)
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

	// adaptive 强制：gpt-5-codex 不在表内，路由声明 adaptive → adaptive+effort high。
	out, _, _, err = responsesToAnthropicTriple(mk("gpt-5-codex", "high"), nil, nil, "adaptive", false)
	if err != nil {
		t.Fatalf("adaptive err: %v", err)
	}
	th = asObj(out["thinking"])
	if objStr(th, "type") != "adaptive" || objStr(asObj(out["output_config"]), "effort") != "high" {
		t.Errorf("adaptive gpt-5-codex: thinking=%v output_config=%v, want adaptive/high", th, out["output_config"])
	}

	// 强制 adaptive + 显式关（effort none）→ disabled（cannotDisable 被覆盖，允许关）。
	out, _, _, err = responsesToAnthropicTriple(mk("gpt-5-codex", "none"), nil, nil, "adaptive", false)
	if err != nil {
		t.Fatalf("adaptive none err: %v", err)
	}
	if objStr(asObj(out["thinking"]), "type") != "disabled" {
		t.Errorf("adaptive+none: thinking=%v, want disabled", out["thinking"])
	}

	// budget 强制 + 显式关 → disabled。
	out, _, _, err = responsesToAnthropicTriple(mk("claude-fable-5", "none"), nil, nil, "budget", false)
	if err != nil {
		t.Fatalf("budget none err: %v", err)
	}
	if objStr(asObj(out["thinking"]), "type") != "disabled" {
		t.Errorf("budget+none: thinking=%v, want disabled", out["thinking"])
	}
}

// TestResponsesThinkMode 锁定状态页「API」列思考值的 Responses 口径：reasoning.effort 只显
// 档位词（"effort·" 前缀多余——Responses 口径由列绿色承担），显式关闭值
// （none/off/disabled，大小写不敏感）归并为 "关"；无 reasoning / 无 effort / reasoning 非对象都返回空（列显 -）。
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

// ---- 思考块信封 ----

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
	// 无签名的 thinking 不编码；乱码/别家信封解不出。
	if encodeThinkingEnvelope(map[string]interface{}{"type": "thinking", "thinking": "x"}) != "" {
		t.Errorf("无签名 thinking 不应编码")
	}
	if decodeThinkingEnvelope("ccswitch-openai-reasoning-v1:aaaa") != nil {
		t.Errorf("别家信封不应解出")
	}
}

// ---- 响应翻译（非流式 JSON→JSON）----

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

// ---- 流式状态机 ----

// feedSSEToConv 把 SSE 文本逐块喂给状态机。
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
	// response.created 必须先于 output_item.added。
	if strings.Index(joined, "response.created") > strings.Index(joined, "response.output_item.added") {
		t.Errorf("事件顺序错误：created 应最先")
	}

	final := conv.buildFinalResponse()
	output := asArr(final["output"])
	// reasoning + web_search_call(调用) + web_search_call(来源) + function_call + message = 5
	if len(output) != 5 {
		t.Fatalf("output 数=%d, want 5: %v", len(output), output)
	}
	// reasoning 项的信封能还原 thinking 块（含签名）。
	rs := asObj(output[0])
	dec := decodeThinkingEnvelope(objStr(rs, "encrypted_content"))
	if dec == nil || objStr(dec, "signature") != "sig_abc" {
		t.Errorf("reasoning 信封还原失败: %v", dec)
	}
	// 真搜索：调用项 action 带 query，结果项 action 带 sources。
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
	// function_call 参数拼全。
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
	// usage 合并：input 77+cache 11=88，output 9。
	usage := asObj(final["usage"])
	if toInt64(usage["input_tokens"]) != 88 || toInt64(usage["output_tokens"]) != 9 {
		t.Errorf("usage=%v", usage)
	}
	if final["status"] != "completed" {
		t.Errorf("status=%v", final["status"])
	}
}

// testSSEEmptySearchTriplet 构造 Kimi k3-256k 实测的「空搜索三连」流：
// 空前言 text 块（"Search results for query: "，query 为空）+ 无 id/input 的
// server_tool_use + content 为空的 web_search_tool_result，随后 thinking + 正文。
// （在线探针实证：请求带 web_search 工具时每轮响应开头都有这组占位。）
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

// TestAnthToRespStreamDropsEmptySearchTriplet 验证流式路径整块丢弃空搜索三连：
// 前言文本不出现在事件流、不产生 web_search_call 项，thinking/正文不受影响。
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
	// 只剩 reasoning + message（前言/空调用/空结果三项被丢）。
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

// TestAnthToRespStreamStripsRepeatedPreamble 复刻实测流：前言重复两次、逐 token 粘在
// 正文开头（Kimi 模型从历史模仿前言模式的产物）。翻译后正文应只剩 "我确认一下"，
// 且吐字 delta 拼接起来也不含前言。
func TestAnthToRespStreamStripsRepeatedPreamble(t *testing.T) {
	sse := "" +
		`event: message_start` + "\n" +
		`data: {"type":"message_start","message":{"id":"msg_k4","type":"message","role":"assistant","model":"k3-256k","content":[],"stop_reason":null,"usage":{"input_tokens":10,"output_tokens":0}}}` + "\n\n" +
		`event: content_block_start` + "\n" +
		`data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}` + "\n\n"
	// 前言按实测的碎 delta 形状逐段喂入。
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
	// 事件流里的 output_text.delta 拼起来应恰好是正文，不含前言。
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

// TestAnthToRespStreamDropsSearchQueryEcho 复刻 08-30 天气会话实测形状：单条前言后紧跟
// 真 query 文本。query 回声行整行删除——事件流与最终输出都不含该行（该块不产生任何事件）。
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

// TestAnthropicToResponsesObjectDropsEmptySearch 验证非流式 JSON 整转路径同样过滤空搜索三连。
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

// TestAnthropicToResponsesObjectKeepsRealSearch 验证真搜索的结构化项不受影响：
// 调用项带 query、结果项带 sources；query 回声文本行整行删除（不再进输出）。
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
	// 回声文本已删：web_search_call(调用) + web_search_call(来源) + message(答案) = 3
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

// TestStripRepeatedPreamble 锁定前言分流规则：≥2 条连续前言才剥光（模型模仿签名），
// 单条前言+文本与无前言文本一律原样保留（回声行整行删除是 stripSearchQueryEcho 的职责）。
func TestStripRepeatedPreamble(t *testing.T) {
	const m = "Search results for query: "
	cases := []struct{ in, want string }{
		{"", ""},
		{"正文", "正文"},
		{m, m}, // 单条纯前言：本函数不动（整行删除在 stripSearchQueryEcho）
		{m + "广州 天气", m + "广州 天气"}, // 单条前言+query：本函数不动
		{m + m, ""}, // 重复纯前言：剥光为空
		{m + m + "我确认一下", "我确认一下"},
		{m + m + m + "我确认一下", "我确认一下"},
	}
	for _, c := range cases {
		if got := stripRepeatedPreamble(c.in); got != c.want {
			t.Errorf("stripRepeatedPreamble(%q)=%q, want %q", c.in, got, c.want)
		}
	}
}

// TestStripSearchQueryEcho 锁定回声行删除规则：单条前言开头的行（含 query）整行删除；
// 重复裸前言粘连正文剥光留正文；无回声的文本逐字节原样；删除留下的行首空行去掉。
func TestStripSearchQueryEcho(t *testing.T) {
	const m = "Search results for query: "
	cases := []struct{ in, want string }{
		{"", ""},
		{"正文", "正文"},
		{"a\n\nb", "a\n\nb"},        // 无回声：空行原样保留，逐字节不动
		{m, ""},                     // 纯前言
		{strings.TrimSpace(m), ""},  // 无尾空格的裸前言形态
		{m + "广州 天气", ""},           // 回声行独占一块：删空
		{m + "广州 天气\n正文", "正文"},     // 回声行+正文：删行留正文
		{m + "广州 天气\n\n\n正文", "正文"}, // 删除留下的行首空行去掉
		{"前文\n" + m + "广州\n后文", "前文\n后文"}, // 文本中间的回声行也删（历史回放方向）
		{m + m + "我确认一下", "我确认一下"},        // 模仿签名：剥裸前言留同行正文
		{m + m + m + "我确认一下", "我确认一下"},
	}
	for _, c := range cases {
		if got := stripSearchQueryEcho(c.in); got != c.want {
			t.Errorf("stripSearchQueryEcho(%q)=%q, want %q", c.in, got, c.want)
		}
	}
}

// TestConvertInputDropsSearchQueryEcho 验证上游回放方向：整段请求体各消息文本里的
// 回声行删除（只剩回声的助手消息整条消失），Codex 丢成空壳的 web_search_call 不回放。
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
	// user(广州天气如何) + assistant(今天广州晴，25°C) + user(详细说说) = 3 条
	// （只剩回声的那条助手消息整块消失）
	if len(msgs) != 3 {
		t.Fatalf("消息数=%d, want 3: %s", len(msgs), b)
	}
	if got := objStr(asObj(asArr(msgs[1]["content"])[0]), "text"); got != "今天广州晴，25°C" {
		t.Errorf("assistant 文本=%q, want 今天广州晴，25°C（回声行已删）", got)
	}
}

// TestConvertInputKeepsNonEmptySearch 验证回放方向的搜索结构取舍：web_search_call
// 调用项（哪怕带 query/来源）一律不还原——其 id 是代理自造，上游注册表从未登记，
// 上行必 400 且连坐信封对被剥（生产 #3/#19 实证）；已是 Anthropic 形状的
// server_tool_use/web_search_tool_result 有内容原样上行；空壳（无 input/无
// content/无有效来源）删除。
func TestConvertInputKeepsNonEmptySearch(t *testing.T) {
	items := []interface{}{
		map[string]interface{}{"type": "message", "role": "user", "content": "查下 pandas 版本"},
		map[string]interface{}{"type": "web_search_call", "id": "ws_1", "status": "completed",
			"action": map[string]interface{}{"type": "search", "query": "pandas 最新版本",
				"sources": []interface{}{
					map[string]interface{}{"type": "url", "url": "https://pandas.pydata.org"},
					map[string]interface{}{"type": "url"}, // 无 URL 的来源
				}}},
		map[string]interface{}{"type": "server_tool_use", "id": "srvtoolu_1", "name": "web_search",
			"input": map[string]interface{}{"query": "广州天气"}},
		map[string]interface{}{"type": "server_tool_use", "id": "srvtoolu_2", "name": "web_search"}, // 无 input：删
		map[string]interface{}{"type": "web_search_tool_result", "tool_use_id": "srvtoolu_1",
			"content": []interface{}{map[string]interface{}{"type": "web_search_result", "url": "https://a.cn"}}},
		map[string]interface{}{"type": "web_search_tool_result", "tool_use_id": "srvtoolu_2"}, // 无 content：删
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
	// user + assistant（只剩防御性覆盖的 srvtoolu_1 一对 + 文本）
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
	// 原样上行的一对：id 保持原生注册 id
	stu := asObj(content[0])
	if objStr(asObj(stu["input"]), "query") != "广州天气" || objStr(stu, "id") != "srvtoolu_1" {
		t.Errorf("原样上行的 server_tool_use 不对: %v", stu)
	}
	wstr := asObj(content[1])
	if objStr(wstr, "tool_use_id") != "srvtoolu_1" {
		t.Errorf("原样上行的 web_search_tool_result 不对: %v", wstr)
	}
}

// TestConvertInputMediaFallback 验证附件部件不静默丢：标准形态（data URL 图片、
// 顶层裸 input_file）正常转 image/document 块；认不出的形态（blob: URL、file_id
// 云端引用）序列化成文本块兜底（见 pushMediaPart）。
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
	// 全部进同一条 user 消息：image + document + 文本兜底×2 = 4 块
	if len(msgs) != 1 {
		t.Fatalf("消息数=%d, want 1: %s", len(msgs), s)
	}
	if n := len(asArr(msgs[0]["content"])); n != 4 {
		t.Fatalf("块数=%d, want 4: %s", n, s)
	}
}

// TestTranslatingWriterNonStream 验证 stream:false 客户端拿到一次性 Responses JSON。
func TestTranslatingWriterNonStream(t *testing.T) {
	rec := httptest.NewRecorder()
	tw := newTranslatingWriter(rec, false, "gpt-5-codex", nil, false)
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

// TestTranslatingWriterStreamSplitWrite 验证流式客户端 + 残行跨 Write 的解析。
func TestTranslatingWriterStreamSplitWrite(t *testing.T) {
	rec := httptest.NewRecorder()
	tw := newTranslatingWriter(rec, true, "gpt-5-codex", nil, false)
	tw.Header().Set("Content-Type", "text/event-stream")
	tw.WriteHeader(200)
	// 把一个完整 SSE 块从中间劈开分两次写，模拟残行。
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

// TestResponsesHandlerEndToEnd 端到端：Responses 请求 → 翻译 → 主管线 → 假上游 SSE →
// 翻译回 Responses JSON（非流式客户端）。
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

	// 上游收到的应是翻译后的 Anthropic 请求：system 来自 instructions，stream 被强制 true，
	// model 被路由改写。
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

	// 客户端拿到 Responses JSON。
	var m map[string]interface{}
	if err := json.Unmarshal(body, &m); err != nil {
		t.Fatalf("响应不是 JSON: %v (%s)", err, string(body))
	}
	if m["object"] != "response" {
		t.Fatalf("object=%v, body=%s", m["object"], string(body))
	}
	// model 回写客户端原名（管线 model 回写逻辑）。
	if m["model"] != "gpt-5-codex" {
		t.Errorf("model=%v, want gpt-5-codex", m["model"])
	}
	// 搜索信封随行（路由定案后三元组已注入）：原 5 项 + 信封 reasoning 项 = 6。
	if len(asArr(m["output"])) != 6 {
		t.Errorf("output 数=%d, want 6（含搜索信封项）", len(asArr(m["output"])))
	}
}

// TestResponsesFastRouteEntry 验证 Codex 菜单里的 fast 条目（model 名字面名 "fast_route"）被
// 翻译层注入 speed:"fast"，由主管线 fast 分支接管：上游收到 fast_route.model 改写后的
// model，且 speed 字段已被移除。
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

// TestReconcileResponsesServer 验证监听口随配置动态启停：
// 启动 → 同地址幂等 → 换地址（旧关新开）→ 停用（关闭）。
func TestReconcileResponsesServer(t *testing.T) {
	cfg.Store(&Config{}) // handler 每请求 cfg.Load()，不能为 nil
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
	defer reconcileResponsesServer("") // 收尾别留监听
	if err := get(addr); err != nil {
		t.Fatalf("监听口未起来: %v", err)
	}

	reconcileResponsesServer(addr) // 同地址幂等，不重新绑定
	if err := get(addr); err != nil {
		t.Fatalf("同地址 reconcile 后监听丢了: %v", err)
	}

	addr2 := freeAddr()
	reconcileResponsesServer(addr2) // 换地址：旧关新开
	if err := get(addr); err == nil {
		t.Fatal("旧地址应已关闭")
	}
	if err := get(addr2); err != nil {
		t.Fatalf("新地址未起来: %v", err)
	}

	reconcileResponsesServer("") // 停用
	if err := get(addr2); err == nil {
		t.Fatal("停用后地址应已关闭")
	}
}

// ---- translateNone2Low：关思考请求悄悄升级 low，回传剥离思考块、usage 如实 ----

// TestTranslateNone2LowUpgrade 验证翻译侧：translateNone2Low 开启时，显式关思考
// （reasoning.effort=none）被升级成 low 思考发上游，upgraded 标记为 true；
// 关闭时仍是 disabled、不升级。
func TestTranslateNone2LowUpgrade(t *testing.T) {
	mk := func(model, effort string) map[string]interface{} {
		return map[string]interface{}{
			"model":     model,
			"input":     "hi",
			"reasoning": map[string]interface{}{"effort": effort},
		}
	}

	// budget 模型（gpt-5-codex 不在 adaptive 表）：none + none2Low → enabled+2048，upgraded=true。
	out, _, up, err := responsesToAnthropicTriple(mk("gpt-5-codex", "none"), nil, nil, "", true)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if !up {
		t.Errorf("budget none2Low 应 upgraded=true")
	}
	th := asObj(out["thinking"])
	if objStr(th, "type") != "enabled" || toInt64(th["budget_tokens"]) != 2048 {
		t.Errorf("budget none2Low thinking=%v, want enabled/2048", th)
	}

	// adaptive 模型（fable-5）：none + none2Low → adaptive + effort low，upgraded=true。
	out, _, up, err = responsesToAnthropicTriple(mk("claude-fable-5", "none"), nil, nil, "", true)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if !up {
		t.Errorf("adaptive none2Low 应 upgraded=true")
	}
	if objStr(asObj(out["thinking"]), "type") != "adaptive" || objStr(asObj(out["output_config"]), "effort") != "low" {
		t.Errorf("adaptive none2Low: thinking=%v output_config=%v, want adaptive/low", out["thinking"], out["output_config"])
	}

	// none2Low 关闭：none 仍 → disabled，upgraded=false。
	out, _, up, err = responsesToAnthropicTriple(mk("gpt-5-codex", "none"), nil, nil, "", false)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if up {
		t.Errorf("none2Low 关闭不应 upgraded")
	}
	if objStr(asObj(out["thinking"]), "type") != "disabled" {
		t.Errorf("none2Low 关闭 none 应 disabled: %v", out["thinking"])
	}

	// 非关思考请求（high）即使开 none2Low 也不升级。
	_, _, up, err = responsesToAnthropicTriple(mk("gpt-5-codex", "high"), nil, nil, "", true)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if up {
		t.Errorf("high 请求不应被 none2Low 升级")
	}
}

// TestTranslateNone2LowStripObject 验证非流式响应：升级流的 thinking 块被剥离
// （不进 output），但 usage 里的 reasoning_tokens 如实保留。
func TestTranslateNone2LowStripObject(t *testing.T) {
	msg := map[string]interface{}{
		"id":          "msg_1",
		"model":       "k3-256k",
		"stop_reason": "end_turn",
		"content": []interface{}{
			map[string]interface{}{"type": "thinking", "thinking": "想", "signature": "sig"},
			map[string]interface{}{"type": "text", "text": "答案"},
		},
		"usage": map[string]interface{}{
			"input_tokens": 77, "output_tokens": 9, "cache_read_input_tokens": 11,
			"output_tokens_details": map[string]interface{}{"thinking_tokens": 5},
		},
	}
	out := anthropicToResponsesObject(msg, "k3-256k", nil, nil, true)
	output := asArr(out["output"])
	for _, it := range output {
		if objStr(asObj(it), "type") == "reasoning" {
			t.Errorf("升级流不应有 reasoning 项: %v", it)
		}
	}
	if len(output) != 1 || objStr(asObj(output[0]), "type") != "message" {
		t.Errorf("升级流 output 应只剩 message: %v", output)
	}
	usage := asObj(out["usage"])
	if toInt64(asObj(usage["output_tokens_details"])["reasoning_tokens"]) != 5 {
		t.Errorf("reasoning_tokens 应如实保留 5: %v", usage)
	}
	if toInt64(usage["output_tokens"]) != 9 {
		t.Errorf("output_tokens 应=9: %v", usage)
	}
}

// TestTranslateNone2LowStripStream 验证流式响应：升级流的思考块不发 reasoning 事件、
// 不占 output index（后续文本块 index 连续），usage 如实。
func TestTranslateNone2LowStripStream(t *testing.T) {
	var events []string
	conv := newAnthToRespStream(func(ev string) { events = append(events, ev) }, "gpt-5-codex", nil)
	conv.noneUpgraded = true
	sse := `event: message_start
data: {"type":"message_start","message":{"id":"msg_1","model":"k3-256k","usage":{"input_tokens":10,"output_tokens":1}}}

event: content_block_start
data: {"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":""}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"想"}}

event: content_block_stop
data: {"type":"content_block_stop","index":0}

event: content_block_start
data: {"type":"content_block_start","index":1,"content_block":{"type":"text","text":""}}

event: content_block_delta
data: {"type":"content_block_delta","index":1,"delta":{"type":"text_delta","text":"答"}}

event: content_block_stop
data: {"type":"content_block_stop","index":1}

event: message_delta
data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":9,"output_tokens_details":{"thinking_tokens":5}}}

event: message_stop
data: {"type":"message_stop"}

`
	feedSSEToConv(conv, sse)
	if !conv.completed {
		t.Fatalf("流未完成")
	}
	joined := strings.Join(events, "")
	// 思考块被剥离：不应有 reasoning 项或 reasoning_summary 事件。
	// （usage 里的 reasoning_tokens 是如实保留的，不在此判据内。）
	for _, bad := range []string{"reasoning_summary", `"type":"reasoning"`} {
		if strings.Contains(joined, bad) {
			t.Errorf("升级流不应发 %s 事件:\n%s", bad, joined)
		}
	}
	if !strings.Contains(joined, `"output_index":0`) {
		t.Errorf("文本块应占 output_index 0（思考块已回退）:\n%s", joined)
	}
	final := conv.buildFinalResponse()
	output := asArr(final["output"])
	for _, it := range output {
		if objStr(asObj(it), "type") == "reasoning" {
			t.Errorf("升级流 final 不应有 reasoning 项: %v", it)
		}
	}
	usage := asObj(final["usage"])
	if toInt64(asObj(usage["output_tokens_details"])["reasoning_tokens"]) != 5 {
		t.Errorf("流式 reasoning_tokens 应如实保留 5: %v", usage)
	}
}
