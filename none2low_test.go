package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestNone2LowUpgradeShapes locks the thinking shapes after the convertOff2Low upgrade:
// adaptive model → adaptive+effort:low (fable-5 can't be turned off; low is how an explicit off is expressed there);
// budget model → enabled+2048 (capped at max_tokens/2; if the 1024 floor doesn't fit, the upgrade is abandoned and thinking stays off);
// toggle off or non-thinking-off requests are never upgraded.
func TestNone2LowUpgradeShapes(t *testing.T) {
	mk := func(model, effort string) map[string]interface{} {
		return map[string]interface{}{
			"model":     model,
			"input":     "hi",
			"reasoning": map[string]interface{}{"effort": effort},
		}
	}

	// adaptive: adaptive + effort:low, implicit upgrade.
	out, _, n2l, err := responsesToAnthropicTriple(mk("claude-fable-5", "none"), nil, nil, "", true)
	if err != nil {
		t.Fatalf("adaptive err: %v", err)
	}
	if n2l != n2lStealth {
		t.Errorf("adaptive none: n2l=%d, want n2lStealth", n2l)
	}
	th := asObj(out["thinking"])
	if objStr(th, "type") != "adaptive" || objStr(asObj(out["output_config"]), "effort") != "low" {
		t.Errorf("adaptive 升级: thinking=%v output_config=%v, want adaptive/low", th, out["output_config"])
	}

	// budget: enabled+2048, implicit upgrade, no output_config.
	out, _, n2l, err = responsesToAnthropicTriple(mk("gpt-5-codex", "none"), nil, nil, "", true)
	if err != nil {
		t.Fatalf("budget err: %v", err)
	}
	if n2l != n2lStealth {
		t.Errorf("budget none: n2l=%d, want n2lStealth", n2l)
	}
	th = asObj(out["thinking"])
	if objStr(th, "type") != "enabled" || toInt64(th["budget_tokens"]) != 2048 {
		t.Errorf("budget 升级: thinking=%v, want enabled/2048", th)
	}
	if out["output_config"] != nil {
		t.Errorf("budget 路径不应有 output_config: %v", out["output_config"])
	}

	// max_output_tokens=1000 → capped at 500, below the 1024 floor: upgrade abandoned, stays disabled.
	small := mk("gpt-5-codex", "none")
	small["max_output_tokens"] = 1000
	out, _, n2l, err = responsesToAnthropicTriple(small, nil, nil, "", true)
	if err != nil {
		t.Fatalf("small err: %v", err)
	}
	if n2l != n2lNone {
		t.Errorf("压顶不足下限: n2l=%d, want n2lNone（放弃升级）", n2l)
	}
	if objStr(asObj(out["thinking"]), "type") != "disabled" {
		t.Errorf("压顶不足下限: thinking=%v, want disabled", out["thinking"])
	}

	// Toggle off: explicit-off goes down as disabled, no upgrade.
	out, _, n2l, err = responsesToAnthropicTriple(mk("gpt-5-codex", "none"), nil, nil, "", false)
	if err != nil {
		t.Fatalf("off err: %v", err)
	}
	if n2l != n2lNone {
		t.Errorf("开关关闭: n2l=%d, want n2lNone", n2l)
	}
	if objStr(asObj(out["thinking"]), "type") != "disabled" {
		t.Errorf("开关关闭: thinking=%v, want disabled", out["thinking"])
	}

	// Non-thinking-off requests aren't upgraded: high goes through as enabled/16000 (default 32000 half-capped).
	out, _, n2l, err = responsesToAnthropicTriple(mk("gpt-5-codex", "high"), nil, nil, "", true)
	if err != nil {
		t.Fatalf("high err: %v", err)
	}
	if n2l != n2lNone {
		t.Errorf("high: n2l=%d, want n2lNone（只升级关思考）", n2l)
	}
	th = asObj(out["thinking"])
	if objStr(th, "type") != "enabled" || toInt64(th["budget_tokens"]) != 16000 {
		t.Errorf("high: thinking=%v, want enabled/16000", th)
	}

	// After the upgrade, temperature is not passed through (the thinking-on mutual-exclusion rule, same as the normal path).
	withTemp := mk("gpt-5-codex", "none")
	withTemp["temperature"] = 0.5
	out, _, n2l, err = responsesToAnthropicTriple(withTemp, nil, nil, "", true)
	if err != nil {
		t.Fatalf("temp err: %v", err)
	}
	if n2l != n2lStealth || out["temperature"] != nil {
		t.Errorf("升级流 temperature 应被丢弃: n2l=%d temperature=%v", n2l, out["temperature"])
	}
}

// TestNone2LowUpgradeToolContinuation locks that tool-continuation turns (no signed thinking block before the trailing
// tool_result, historyValid=false) upgrade the same way — this is Codex's main request shape; with the toggle off this path doesn't enable thinking.
func TestNone2LowUpgradeToolContinuation(t *testing.T) {
	mk := func() map[string]interface{} {
		return map[string]interface{}{
			"model": "gpt-5-codex",
			"input": []interface{}{
				map[string]interface{}{"type": "message", "role": "user", "content": "查天气"},
				map[string]interface{}{"type": "function_call", "call_id": "c1", "name": "get_weather", "arguments": "{\"city\":\"北京\"}"},
				map[string]interface{}{"type": "function_call_output", "call_id": "c1", "output": "晴"},
			},
			"reasoning": map[string]interface{}{"effort": "none"},
		}
	}

	out, _, n2l, err := responsesToAnthropicTriple(mk(), nil, nil, "", true)
	if err != nil {
		t.Fatalf("续轮 err: %v", err)
	}
	if n2l != n2lStealth {
		t.Errorf("工具续轮: n2l=%d, want n2lStealth", n2l)
	}
	th := asObj(out["thinking"])
	if objStr(th, "type") != "enabled" || toInt64(th["budget_tokens"]) != 2048 {
		t.Errorf("工具续轮升级: thinking=%v, want enabled/2048", th)
	}

	// Control: with the toggle off, tool continuation doesn't enable thinking (the budget path is skipped wholesale; the thinking field doesn't appear).
	out, _, n2l, err = responsesToAnthropicTriple(mk(), nil, nil, "", false)
	if err != nil {
		t.Fatalf("续轮对照 err: %v", err)
	}
	if n2l != n2lNone || out["thinking"] != nil {
		t.Errorf("续轮对照: n2l=%d thinking=%v, want n2lNone/无 thinking 字段", n2l, out["thinking"])
	}
}

// TestNone2LowTryOnInvalidHistory locks the #11/#12 live-incident fix: when tool-continuation history isn't replayable and the downstream did
// NOT explicitly disable thinking (effort=max — Codex config's norm; enabled-reasoning-efforts has no
// none level at all), the proxy's fallback is no longer disabling thinking itself (off = Kimi routes K3 to the K2.8
// no-thinking variant); with convertOff2Low on, it sends at the downstream-requested effort as-is: a budget route gets the matching budget (max→16384 capped at
// max_tokens/2=16000), an adaptive route (the user's configured k3-256k route shape) gets output_config.effort=
// the requested level; none/unknown → low as the floor. n2l=n2lTryOn rather than n2lStealth — the downstream asked for thinking, so the return
// side must not strip thinking blocks (blocks ride the return carrying signatures; next-turn history self-heals). Toggle off keeps the old behavior.
func TestNone2LowTryOnInvalidHistory(t *testing.T) {
	mk := func(model, effort string) map[string]interface{} {
		return map[string]interface{}{
			"model": model,
			"input": []interface{}{
				map[string]interface{}{"type": "message", "role": "user", "content": "查天气"},
				map[string]interface{}{"type": "function_call", "call_id": "c1", "name": "get_weather", "arguments": "{\"city\":\"北京\"}"},
				map[string]interface{}{"type": "function_call_output", "call_id": "c1", "output": "晴"},
			},
			"reasoning":   map[string]interface{}{"effort": effort},
			"temperature": 0.5,
		}
	}

	// budget route + effort max + non-replayable history + toggle on → sent at the requested level (not an implicit upgrade).
	out, _, n2l, err := responsesToAnthropicTriple(mk("gpt-5-codex", "max"), nil, nil, "", true)
	if err != nil {
		t.Fatalf("tryon budget err: %v", err)
	}
	if n2l != n2lTryOn {
		t.Errorf("tryon budget: n2l=%d, want n2lTryOn", n2l)
	}
	th := asObj(out["thinking"])
	if objStr(th, "type") != "enabled" || toInt64(th["budget_tokens"]) != 16000 {
		t.Errorf("tryon budget: thinking=%v, want enabled/16000（16384 压顶 32000/2）", th)
	}
	if out["temperature"] != nil {
		t.Errorf("tryon 开了 thinking，temperature 应丢弃: %v", out["temperature"])
	}

	// adaptive route (the thinkStyle shape of the user's k3-256k route) + effort max → the requested level max.
	out, _, n2l, err = responsesToAnthropicTriple(mk("fable", "max"), nil, nil, "adaptive", true)
	if err != nil {
		t.Fatalf("tryon adaptive err: %v", err)
	}
	if n2l != n2lTryOn {
		t.Errorf("tryon adaptive: n2l=%d, want n2lTryOn", n2l)
	}
	th = asObj(out["thinking"])
	if objStr(th, "type") != "adaptive" || objStr(asObj(out["output_config"]), "effort") != "max" {
		t.Errorf("tryon adaptive: thinking=%v output_config=%v, want adaptive/max", th, out["output_config"])
	}

	// adaptive route + downstream gave no level (reasoning absent) → low as the floor, still a fallback thinking-on.
	noEffort := mk("fable", "")
	delete(noEffort, "reasoning")
	out, _, n2l, err = responsesToAnthropicTriple(noEffort, nil, nil, "adaptive", true)
	if err != nil {
		t.Fatalf("tryon 保底 err: %v", err)
	}
	if n2l != n2lTryOn {
		t.Errorf("tryon 保底: n2l=%d, want n2lTryOn", n2l)
	}
	th = asObj(out["thinking"])
	if objStr(th, "type") != "adaptive" || objStr(asObj(out["output_config"]), "effort") != "low" {
		t.Errorf("tryon 保底: thinking=%v output_config=%v, want adaptive/low", th, out["output_config"])
	}

	// Control: toggle off → keep the cc-switch fallback, sending explicit thinking-off upstream.
	out, _, n2l, err = responsesToAnthropicTriple(mk("fable", "max"), nil, nil, "adaptive", false)
	if err != nil {
		t.Fatalf("tryon 对照 err: %v", err)
	}
	if n2l != n2lNone {
		t.Errorf("tryon 对照: n2l=%d, want n2lNone", n2l)
	}
	if objStr(asObj(out["thinking"]), "type") != "disabled" {
		t.Errorf("tryon 对照: thinking=%v, want disabled", out["thinking"])
	}
}

// TestNone2LowToolChoiceConflict locks the handling of forced tool_choice conflicting with thinking:
// thinking is turned off (disabled, output_config deleted, temperature restored), the upgrade flag reset —
// the upstream won't produce thinking blocks anymore, so the return side needs no stripping.
func TestNone2LowToolChoiceConflict(t *testing.T) {
	body := map[string]interface{}{
		"model": "gpt-5-codex",
		"input": "hi",
		"tools": []interface{}{
			map[string]interface{}{
				"type":        "custom",
				"name":        "apply_patch",
				"description": "Apply a patch",
				"format":      map[string]interface{}{"type": "grammar", "syntax": "lark", "definition": "start: ..."},
			},
		},
		"tool_choice": "required",
		"reasoning":   map[string]interface{}{"effort": "none"},
		"temperature": 0.5,
	}
	out, _, n2l, err := responsesToAnthropicTriple(body, nil, nil, "", true)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if n2l != n2lNone {
		t.Errorf("tool_choice 冲突: n2l=%d, want n2lNone（思考已关无需剥离）", n2l)
	}
	if objStr(asObj(out["thinking"]), "type") != "disabled" {
		t.Errorf("tool_choice 冲突: thinking=%v, want disabled", out["thinking"])
	}
	if out["output_config"] != nil {
		t.Errorf("tool_choice 冲突: output_config 应删除: %v", out["output_config"])
	}
	if objStr(asObj(out["tool_choice"]), "type") != "any" {
		t.Errorf("tool_choice 冲突: tool_choice=%v, want any（required 映射）", out["tool_choice"])
	}
	if out["temperature"] != 0.5 {
		t.Errorf("tool_choice 冲突: temperature=%v, want 0.5（恢复透传）", out["temperature"])
	}
}

// TestAnthToRespStreamStripThinking locks streaming stripping: thinking blocks emit no events and enter no items,
// the pre-allocated outputIndex is reclaimed keeping later block indices contiguous; usage is kept truthfully.
func TestAnthToRespStreamStripThinking(t *testing.T) {
	var events []string
	conv := newAnthToRespStream(func(ev string) { events = append(events, ev) }, "gpt-5-codex", nil)
	conv.stripThinking = true
	feedSSEToConv(conv, testSSEAllBlocks())

	if !conv.completed {
		t.Fatalf("流未完成")
	}
	joined := strings.Join(events, "")
	// Note: response.completed's usage always carries the reasoning_tokens field name (even 0),
	// so a bare "reasoning" substring match won't do; this nails down the item types and summary events a thinking block would produce.
	if strings.Contains(joined, `"type":"reasoning"`) {
		t.Errorf("剥离后不应有 reasoning 项事件")
	}
	if strings.Contains(joined, "reasoning_summary") {
		t.Errorf("剥离后不应有 reasoning_summary 事件")
	}
	if !strings.Contains(joined, "response.output_text.delta") {
		t.Errorf("正文 delta 不应受剥离影响")
	}
	final := conv.buildFinalResponse()
	output := asArr(final["output"])
	if len(output) != 4 {
		t.Fatalf("output 数=%d, want 4（思考块剥离，其余 4 块不动）", len(output))
	}
	for _, it := range output {
		if objStr(asObj(it), "type") == "reasoning" {
			t.Errorf("items 里不应有 reasoning 项: %v", it)
		}
	}
	// 4 items occupy output_index 0..3: an index 4 appearing means stripped blocks didn't reclaim their indices.
	if strings.Contains(joined, `"output_index":4`) {
		t.Errorf("output_index 应连续（0..3），不应出现 4")
	}
	// usage untouched: the fixture's output_tokens=9 lands in the final response as-is.
	if toInt64(asObj(final["usage"])["output_tokens"]) != 9 {
		t.Errorf("usage 应如实透传: %v", final["usage"])
	}
}

// TestTranslatingWriterStripThinkingNonStream locks non-streaming wholesale-conversion (finishBuffered) stripping:
// the one-shot JSON the client receives contains no reasoning items.
func TestTranslatingWriterStripThinkingNonStream(t *testing.T) {
	rec := httptest.NewRecorder()
	tw := newTranslatingWriter(rec, false, "gpt-5-codex", nil)
	// Same wiring as responsesHandler: conv handles the SSE stream (this case goes that way),
	// tw.stripThinking handles the wholesale-conversion fallback (finishBuffered) when the upstream returns non-SSE JSON.
	tw.stripThinking = true
	tw.conv.stripThinking = true
	tw.Header().Set("Content-Type", "text/event-stream")
	tw.WriteHeader(200)
	if _, err := tw.Write([]byte(testSSEAllBlocks())); err != nil {
		t.Fatalf("Write: %v", err)
	}
	tw.finish()

	var m map[string]interface{}
	if err := json.Unmarshal(rec.Body.Bytes(), &m); err != nil {
		t.Fatalf("响应不是 JSON: %v", err)
	}
	output := asArr(m["output"])
	if len(output) != 4 {
		t.Fatalf("output 数=%d, want 4（思考块剥离）", len(output))
	}
	for _, it := range output {
		if objStr(asObj(it), "type") == "reasoning" {
			t.Errorf("非流式输出不应有 reasoning 项: %v", it)
		}
	}
}

// TestAnthropicToResponsesObjectStripThinking locks wholesale-conversion stripping at the object level:
// thinking and redacted_thinking blocks are dropped wholesale; usage is kept as-is.
func TestAnthropicToResponsesObjectStripThinking(t *testing.T) {
	msg := map[string]interface{}{
		"id": "msg_t", "model": "m", "stop_reason": "end_turn",
		"content": []interface{}{
			map[string]interface{}{"type": "thinking", "thinking": "想", "signature": "sig"},
			map[string]interface{}{"type": "redacted_thinking", "data": "ENC"},
			map[string]interface{}{"type": "text", "text": "正文"},
		},
		"usage": map[string]interface{}{"input_tokens": 3, "output_tokens": 5},
	}

	out := anthropicToResponsesObject(msg, "m", nil, nil, true)
	output := asArr(out["output"])
	if len(output) != 1 || objStr(asObj(output[0]), "type") != "message" {
		t.Fatalf("剥离后 output=%v, want 仅 message 一项", output)
	}
	if toInt64(asObj(out["usage"])["output_tokens"]) != 5 {
		t.Errorf("usage 应原样保留: %v", out["usage"])
	}

	// Control: without stripping, the two thinking blocks each produce a reasoning item.
	out = anthropicToResponsesObject(msg, "m", nil, nil, false)
	output = asArr(out["output"])
	if len(output) != 3 || objStr(asObj(output[0]), "type") != "reasoning" || objStr(asObj(output[1]), "type") != "reasoning" {
		t.Fatalf("不剥离 output=%v, want reasoning×2 + message", output)
	}
}

// TestDisableThinkingInBody locks the 400-fallback surgery: thinking set to disabled, output_config deleted,
// all other fields and formatting as-is; a non-JSON object returns ok=false.
func TestDisableThinkingInBody(t *testing.T) {
	cases := []struct {
		name string
		src  string
		want string
		ok   bool
	}{
		{"紧凑-改值加删尾键", `{"model":"x","thinking":{"type":"enabled","budget_tokens":2048},"output_config":{"effort":"low"}}`, `{"model":"x","thinking":{"type":"disabled"}}`, true},
		{"多行-删中间键", "{\n  \"model\": \"x\",\n  \"output_config\": {\"effort\": \"low\"},\n  \"thinking\": {\"type\": \"adaptive\"}\n}\n", "{\n  \"model\": \"x\",\n  \"thinking\": {\"type\":\"disabled\"}\n}\n", true},
		{"无 output_config", `{"thinking":{"type":"enabled","budget_tokens":2048}}`, `{"thinking":{"type":"disabled"}}`, true},
		{"非对象", `[]`, ``, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := disableThinkingInBody([]byte(tc.src))
			if ok != tc.ok {
				t.Fatalf("ok=%v, want %v", ok, tc.ok)
			}
			if !tc.ok {
				return
			}
			if string(got) != tc.want {
				t.Errorf("got  %q\nwant %q", got, tc.want)
			}
			var probe any
			if err := json.Unmarshal(got, &probe); err != nil {
				t.Errorf("输出不是合法 JSON: %v", err)
			}
		})
	}
}

// TestNone2LowFallbackRetry end-to-end: a Responses thinking-off request upgraded to enabled/2048 goes upstream →
// upstream 200 with an in-body error rejecting thinking → fallback flips back to disabled and resends immediately (no backoff budget burned) →
// the client gets 200, thinking blocks stripped on the return side.
func TestNone2LowFallbackRetry(t *testing.T) {
	resetStats()
	calls := 0
	var secondBody map[string]interface{}
	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		var m map[string]interface{}
		_ = json.Unmarshal(b, &m)
		calls++
		if calls == 1 {
			// First time: the upstream should receive the upgraded low thinking (budget-style enabled+2048).
			th := asObj(m["thinking"])
			if objStr(th, "type") != "enabled" || toInt64(th["budget_tokens"]) != 2048 {
				t.Errorf("首请求 thinking=%v, want enabled/2048", th)
			}
			// Reject with 200 + in-body error (Kimi style); the error text mentions thinking, triggering the fallback.
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(200)
			io.WriteString(w, "event: error\ndata: {\"type\":\"error\",\"error\":{\"message\":\"thinking is not allowed without signed history\"}}\n\n")
			return
		}
		secondBody = m
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		io.WriteString(w, testSSEAllBlocks())
	}))
	defer mock.Close()

	cfg.Store(&Config{
		Upstream:       mock.URL,
		MaxRetries:     0,
		TotalBudgetSec: 10,
		Routes:         []RouteRule{{Pattern: "gpt-5*", URL: mock.URL, Model: "deepseek-v4-flash", ConvertOff2Low: "translate"}},
	})
	defer cfg.Store(&Config{})

	proxy := httptest.NewServer(http.HandlerFunc(responsesHandler))
	defer proxy.Close()

	resp, err := http.Post(proxy.URL+"/v1/responses", "application/json",
		strings.NewReader(`{"model":"gpt-5-codex","input":"hi","reasoning":{"effort":"none"}}`))
	if err != nil {
		t.Fatalf("请求失败: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()

	if calls != 2 {
		t.Fatalf("上游请求数=%d, want 2（兜底重试一次）", calls)
	}
	if objStr(asObj(secondBody["thinking"]), "type") != "disabled" {
		t.Errorf("兜底重发 thinking=%v, want disabled", secondBody["thinking"])
	}
	if secondBody["output_config"] != nil {
		t.Errorf("兜底重发不应带 output_config: %v", secondBody["output_config"])
	}
	if resp.StatusCode != 200 {
		t.Fatalf("客户端状态=%d, want 200: %s", resp.StatusCode, body)
	}
	var out map[string]interface{}
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("响应不是 JSON: %v (%s)", err, body)
	}
	// Return-side stripping of thinking blocks (stripThinking still in effect): 5 blocks - thinking + search envelope = 5 items.
	output := asArr(out["output"])
	if len(output) != 5 {
		t.Errorf("output 数=%d, want 5（思考剥离+搜索信封随行）: %v", len(output), output)
	}
}

// TestNone2LowTryOnNoStrip end-to-end locks the #11/#12 fix: tool continuation + effort max (downstream didn't disable
// thinking) + toggle on → the upstream receives enabled/16000 at the requested level, and the return side does NOT strip thinking blocks — the downstream asked for
// thinking, so blocks ride the return carrying signatures to self-heal next turn's history (unlike implicit-upgrade streams whose downstream wants thinking off).
func TestNone2LowTryOnNoStrip(t *testing.T) {
	resetStats()
	var gotBody map[string]interface{}
	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &gotBody)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		io.WriteString(w, testSSEAllBlocks())
	}))
	defer mock.Close()

	cfg.Store(&Config{
		Upstream:       mock.URL,
		MaxRetries:     0,
		TotalBudgetSec: 10,
		Routes:         []RouteRule{{Pattern: "gpt-5*", URL: mock.URL, Model: "deepseek-v4-flash", ConvertOff2Low: "translate"}},
	})
	defer cfg.Store(&Config{})

	proxy := httptest.NewServer(http.HandlerFunc(responsesHandler))
	defer proxy.Close()

	// Tool-continuation shape (no signed thinking block before function_call_output) + effort max.
	body := `{"model":"gpt-5-codex","input":[
		{"type":"message","role":"user","content":"查天气"},
		{"type":"function_call","call_id":"c1","name":"get_weather","arguments":"{\"city\":\"北京\"}"},
		{"type":"function_call_output","call_id":"c1","output":"晴"}
	],"reasoning":{"effort":"max"}}`
	resp, err := http.Post(proxy.URL+"/v1/responses", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("请求失败: %v", err)
	}
	raw, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("客户端状态=%d, want 200: %s", resp.StatusCode, raw)
	}

	// What the upstream receives is thinking-on at the requested level (enabled/16000), not thinking-off.
	th := asObj(gotBody["thinking"])
	if objStr(th, "type") != "enabled" || toInt64(th["budget_tokens"]) != 16000 {
		t.Errorf("上游 thinking=%v, want enabled/16000（所请 max 压顶 32000/2）", th)
	}

	// No stripping on return: the output must carry reasoning items (thinking blocks return to the downstream as-is).
	var out map[string]interface{}
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("响应不是 JSON: %v (%s)", err, raw)
	}
	foundReasoning := false
	for _, it := range asArr(out["output"]) {
		if objStr(asObj(it), "type") == "reasoning" {
			foundReasoning = true
		}
	}
	if !foundReasoning {
		t.Errorf("兜底开思考不得剥思考块: output=%v", out["output"])
	}
}

// TestNone2LowOffByDefault: a route WITHOUT convertOff2Low never upgrades — the translation port sends
// thinking-off through as-is (the feature is strictly opt-in per route; the old global switch is gone).
func TestNone2LowOffByDefault(t *testing.T) {
	resetStats()
	var gotBody map[string]interface{}
	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &gotBody)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		io.WriteString(w, testSSEAllBlocks())
	}))
	defer mock.Close()

	cfg.Store(&Config{
		Upstream:       mock.URL,
		MaxRetries:     0,
		TotalBudgetSec: 10,
		Routes:         []RouteRule{{Pattern: "gpt-5*", URL: mock.URL, Model: "deepseek-v4-flash"}},
	})
	defer cfg.Store(&Config{})

	proxy := httptest.NewServer(http.HandlerFunc(responsesHandler))
	defer proxy.Close()

	resp, err := http.Post(proxy.URL+"/v1/responses", "application/json",
		strings.NewReader(`{"model":"gpt-5-codex","input":"hi","reasoning":{"effort":"none"}}`))
	if err != nil {
		t.Fatalf("请求失败: %v", err)
	}
	resp.Body.Close()
	if objStr(asObj(gotBody["thinking"]), "type") != "disabled" {
		t.Errorf("未设 convertOff2Low 时上游 thinking=%v, want disabled（不升级）", gotBody["thinking"])
	}
}

// TestNone2LowAllAlsoTranslates: convertOff2Low:"all" upgrades at the translation port too (translate+native both).
func TestNone2LowAllAlsoTranslates(t *testing.T) {
	resetStats()
	var gotBody map[string]interface{}
	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &gotBody)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		io.WriteString(w, testSSEAllBlocks())
	}))
	defer mock.Close()

	cfg.Store(&Config{
		Upstream:       mock.URL,
		MaxRetries:     0,
		TotalBudgetSec: 10,
		Routes:         []RouteRule{{Pattern: "gpt-5*", URL: mock.URL, Model: "deepseek-v4-flash", ConvertOff2Low: "all"}},
	})
	defer cfg.Store(&Config{})

	proxy := httptest.NewServer(http.HandlerFunc(responsesHandler))
	defer proxy.Close()

	resp, err := http.Post(proxy.URL+"/v1/responses", "application/json",
		strings.NewReader(`{"model":"gpt-5-codex","input":"hi","reasoning":{"effort":"none"}}`))
	if err != nil {
		t.Fatalf("请求失败: %v", err)
	}
	resp.Body.Close()
	th := asObj(gotBody["thinking"])
	if objStr(th, "type") != "enabled" || toInt64(th["budget_tokens"]) != 2048 {
		t.Errorf(`convertOff2Low:"all" 在翻译口也应升级: thinking=%v, want enabled/2048`, th)
	}
}

// TestNone2LowFastRouteFlag: the Codex menu's literal "fast_route" model name takes fast_route's own
// convertOff2Low, overriding whatever a pre-matched catch-all routes[] entry carries (here: nothing).
func TestNone2LowFastRouteFlag(t *testing.T) {
	resetStats()
	var gotBody map[string]interface{}
	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &gotBody)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		io.WriteString(w, testSSEAllBlocks())
	}))
	defer mock.Close()

	cfg.Store(&Config{
		Upstream:       mock.URL,
		MaxRetries:     0,
		TotalBudgetSec: 10,
		Routes:         []RouteRule{{Pattern: "*", URL: mock.URL, Model: "deepseek-v4-flash"}},
		FastRoute:      &FastRoute{URL: mock.URL, Model: "deepseek-v4-flash", ConvertOff2Low: "translate"},
	})
	defer cfg.Store(&Config{})

	proxy := httptest.NewServer(http.HandlerFunc(responsesHandler))
	defer proxy.Close()

	resp, err := http.Post(proxy.URL+"/v1/responses", "application/json",
		strings.NewReader(`{"model":"fast_route","input":"hi","reasoning":{"effort":"none"}}`))
	if err != nil {
		t.Fatalf("请求失败: %v", err)
	}
	resp.Body.Close()
	th := asObj(gotBody["thinking"])
	if objStr(th, "type") != "enabled" || toInt64(th["budget_tokens"]) != 2048 {
		t.Errorf("fast_route 自带 convertOff2Low 应升级: thinking=%v, want enabled/2048", th)
	}
}
