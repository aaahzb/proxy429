package translate

// none2low_test.go — in-package translation-layer tests for the convertOff2Low upgrade (thinking shapes,
// history fallbacks, stripping) plus the allowNoThinkBlock4Anthropic toggle.

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"

	"proxy429/internal/config"
	"proxy429/internal/jsonutil"
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
	out, _, n2l, err := responsesToAnthropicTriple(mk("claude-fable-5", "none"), nil, nil, "", true, true)
	if err != nil {
		t.Fatalf("adaptive err: %v", err)
	}
	if n2l != N2LStealth {
		t.Errorf("adaptive none: n2l=%d, want N2LStealth", n2l)
	}
	th := jsonutil.AsObj(out["thinking"])
	if jsonutil.ObjStr(th, "type") != "adaptive" || jsonutil.ObjStr(jsonutil.AsObj(out["output_config"]), "effort") != "low" {
		t.Errorf("adaptive 升级: thinking=%v output_config=%v, want adaptive/low", th, out["output_config"])
	}

	// budget: enabled+2048, implicit upgrade, no output_config.
	out, _, n2l, err = responsesToAnthropicTriple(mk("gpt-5-codex", "none"), nil, nil, "", true, true)
	if err != nil {
		t.Fatalf("budget err: %v", err)
	}
	if n2l != N2LStealth {
		t.Errorf("budget none: n2l=%d, want N2LStealth", n2l)
	}
	th = jsonutil.AsObj(out["thinking"])
	if jsonutil.ObjStr(th, "type") != "enabled" || jsonutil.ToInt64(th["budget_tokens"]) != 2048 {
		t.Errorf("budget 升级: thinking=%v, want enabled/2048", th)
	}
	if out["output_config"] != nil {
		t.Errorf("budget 路径不应有 output_config: %v", out["output_config"])
	}

	// max_output_tokens=1000 → capped at 500, below the 1024 floor: upgrade abandoned, stays disabled.
	small := mk("gpt-5-codex", "none")
	small["max_output_tokens"] = 1000
	out, _, n2l, err = responsesToAnthropicTriple(small, nil, nil, "", true, true)
	if err != nil {
		t.Fatalf("small err: %v", err)
	}
	if n2l != N2LNone {
		t.Errorf("压顶不足下限: n2l=%d, want N2LNone（放弃升级）", n2l)
	}
	if jsonutil.ObjStr(jsonutil.AsObj(out["thinking"]), "type") != "disabled" {
		t.Errorf("压顶不足下限: thinking=%v, want disabled", out["thinking"])
	}

	// Toggle off: explicit-off goes down as disabled, no upgrade.
	out, _, n2l, err = responsesToAnthropicTriple(mk("gpt-5-codex", "none"), nil, nil, "", false, true)
	if err != nil {
		t.Fatalf("off err: %v", err)
	}
	if n2l != N2LNone {
		t.Errorf("开关关闭: n2l=%d, want N2LNone", n2l)
	}
	if jsonutil.ObjStr(jsonutil.AsObj(out["thinking"]), "type") != "disabled" {
		t.Errorf("开关关闭: thinking=%v, want disabled", out["thinking"])
	}

	// Non-thinking-off requests aren't upgraded: high goes through as enabled/16000 (default 32000 half-capped).
	out, _, n2l, err = responsesToAnthropicTriple(mk("gpt-5-codex", "high"), nil, nil, "", true, true)
	if err != nil {
		t.Fatalf("high err: %v", err)
	}
	if n2l != N2LNone {
		t.Errorf("high: n2l=%d, want N2LNone（只升级关思考）", n2l)
	}
	th = jsonutil.AsObj(out["thinking"])
	if jsonutil.ObjStr(th, "type") != "enabled" || jsonutil.ToInt64(th["budget_tokens"]) != 16000 {
		t.Errorf("high: thinking=%v, want enabled/16000", th)
	}

	// After the upgrade, temperature is not passed through (the thinking-on mutual-exclusion rule, same as the normal path).
	withTemp := mk("gpt-5-codex", "none")
	withTemp["temperature"] = 0.5
	out, _, n2l, err = responsesToAnthropicTriple(withTemp, nil, nil, "", true, true)
	if err != nil {
		t.Fatalf("temp err: %v", err)
	}
	if n2l != N2LStealth || out["temperature"] != nil {
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

	out, _, n2l, err := responsesToAnthropicTriple(mk(), nil, nil, "", true, true)
	if err != nil {
		t.Fatalf("续轮 err: %v", err)
	}
	if n2l != N2LStealth {
		t.Errorf("工具续轮: n2l=%d, want N2LStealth", n2l)
	}
	th := jsonutil.AsObj(out["thinking"])
	if jsonutil.ObjStr(th, "type") != "enabled" || jsonutil.ToInt64(th["budget_tokens"]) != 2048 {
		t.Errorf("工具续轮升级: thinking=%v, want enabled/2048", th)
	}

	// Control: with the toggle off, tool continuation doesn't enable thinking (the budget path is skipped wholesale; the thinking field doesn't appear).
	out, _, n2l, err = responsesToAnthropicTriple(mk(), nil, nil, "", false, true)
	if err != nil {
		t.Fatalf("续轮对照 err: %v", err)
	}
	if n2l != N2LNone || out["thinking"] != nil {
		t.Errorf("续轮对照: n2l=%d thinking=%v, want N2LNone/无 thinking 字段", n2l, out["thinking"])
	}
}

// TestNone2LowTryOnInvalidHistory locks the #11/#12 live-incident fix: when tool-continuation history isn't replayable and the downstream did
// NOT explicitly disable thinking (effort=max — Codex config's norm; enabled-reasoning-efforts has no
// none level at all), the proxy's fallback is no longer disabling thinking itself (off = Kimi routes K3 to the K2.8
// no-thinking variant); with convertOff2Low on, it sends at the downstream-requested effort as-is: a budget route gets the matching budget (max→16384 capped at
// max_tokens/2=16000), an adaptive route (the user's configured k3-256k route shape) gets output_config.effort=
// the requested level; none/unknown → low as the floor. n2l=N2LTryOn rather than N2LStealth — the downstream asked for thinking, so the return
// side must not strip thinking blocks (blocks ride the return carrying signatures; next-turn history self-heals). allowNoThinkBlock4Anthropic=false keeps the cc-switch behavior (false wins over convertOff2Low's try-on door).
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
	out, _, n2l, err := responsesToAnthropicTriple(mk("gpt-5-codex", "max"), nil, nil, "", true, true)
	if err != nil {
		t.Fatalf("tryon budget err: %v", err)
	}
	if n2l != N2LTryOn {
		t.Errorf("tryon budget: n2l=%d, want N2LTryOn", n2l)
	}
	th := jsonutil.AsObj(out["thinking"])
	if jsonutil.ObjStr(th, "type") != "enabled" || jsonutil.ToInt64(th["budget_tokens"]) != 16000 {
		t.Errorf("tryon budget: thinking=%v, want enabled/16000（16384 压顶 32000/2）", th)
	}
	if out["temperature"] != nil {
		t.Errorf("tryon 开了 thinking，temperature 应丢弃: %v", out["temperature"])
	}

	// adaptive route (the thinkStyle shape of the user's k3-256k route) + effort max → the requested level max.
	out, _, n2l, err = responsesToAnthropicTriple(mk("fable", "max"), nil, nil, "adaptive", true, true)
	if err != nil {
		t.Fatalf("tryon adaptive err: %v", err)
	}
	if n2l != N2LTryOn {
		t.Errorf("tryon adaptive: n2l=%d, want N2LTryOn", n2l)
	}
	th = jsonutil.AsObj(out["thinking"])
	if jsonutil.ObjStr(th, "type") != "adaptive" || jsonutil.ObjStr(jsonutil.AsObj(out["output_config"]), "effort") != "max" {
		t.Errorf("tryon adaptive: thinking=%v output_config=%v, want adaptive/max", th, out["output_config"])
	}

	// adaptive route + downstream gave no level (reasoning absent) → low as the floor, still a fallback thinking-on.
	noEffort := mk("fable", "")
	delete(noEffort, "reasoning")
	out, _, n2l, err = responsesToAnthropicTriple(noEffort, nil, nil, "adaptive", true, true)
	if err != nil {
		t.Fatalf("tryon 保底 err: %v", err)
	}
	if n2l != N2LTryOn {
		t.Errorf("tryon 保底: n2l=%d, want N2LTryOn", n2l)
	}
	th = jsonutil.AsObj(out["thinking"])
	if jsonutil.ObjStr(th, "type") != "adaptive" || jsonutil.ObjStr(jsonutil.AsObj(out["output_config"]), "effort") != "low" {
		t.Errorf("tryon 保底: thinking=%v output_config=%v, want adaptive/low", th, out["output_config"])
	}

	// Control: allowNoThinkBlock4Anthropic=false wins over convertOff2Low's try-on door → keep the cc-switch fallback, sending explicit thinking-off upstream.
	out, _, n2l, err = responsesToAnthropicTriple(mk("fable", "max"), nil, nil, "adaptive", true, false)
	if err != nil {
		t.Fatalf("tryon 对照 err: %v", err)
	}
	if n2l != N2LNone {
		t.Errorf("tryon 对照: n2l=%d, want N2LNone", n2l)
	}
	if jsonutil.ObjStr(jsonutil.AsObj(out["thinking"]), "type") != "disabled" {
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
	out, _, n2l, err := responsesToAnthropicTriple(body, nil, nil, "", true, true)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if n2l != N2LNone {
		t.Errorf("tool_choice 冲突: n2l=%d, want N2LNone（思考已关无需剥离）", n2l)
	}
	if jsonutil.ObjStr(jsonutil.AsObj(out["thinking"]), "type") != "disabled" {
		t.Errorf("tool_choice 冲突: thinking=%v, want disabled", out["thinking"])
	}
	if out["output_config"] != nil {
		t.Errorf("tool_choice 冲突: output_config 应删除: %v", out["output_config"])
	}
	if jsonutil.ObjStr(jsonutil.AsObj(out["tool_choice"]), "type") != "any" {
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
	output := jsonutil.AsArr(final["output"])
	if len(output) != 4 {
		t.Fatalf("output 数=%d, want 4（思考块剥离，其余 4 块不动）", len(output))
	}
	for _, it := range output {
		if jsonutil.ObjStr(jsonutil.AsObj(it), "type") == "reasoning" {
			t.Errorf("items 里不应有 reasoning 项: %v", it)
		}
	}
	// 4 items occupy output_index 0..3: an index 4 appearing means stripped blocks didn't reclaim their indices.
	if strings.Contains(joined, `"output_index":4`) {
		t.Errorf("output_index 应连续（0..3），不应出现 4")
	}
	// usage untouched: the fixture's output_tokens=9 lands in the final response as-is.
	if jsonutil.ToInt64(jsonutil.AsObj(final["usage"])["output_tokens"]) != 9 {
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
	output := jsonutil.AsArr(m["output"])
	if len(output) != 4 {
		t.Fatalf("output 数=%d, want 4（思考块剥离）", len(output))
	}
	for _, it := range output {
		if jsonutil.ObjStr(jsonutil.AsObj(it), "type") == "reasoning" {
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
	output := jsonutil.AsArr(out["output"])
	if len(output) != 1 || jsonutil.ObjStr(jsonutil.AsObj(output[0]), "type") != "message" {
		t.Fatalf("剥离后 output=%v, want 仅 message 一项", output)
	}
	if jsonutil.ToInt64(jsonutil.AsObj(out["usage"])["output_tokens"]) != 5 {
		t.Errorf("usage 应原样保留: %v", out["usage"])
	}

	// Control: without stripping, the two thinking blocks each produce a reasoning item.
	out = anthropicToResponsesObject(msg, "m", nil, nil, false)
	output = jsonutil.AsArr(out["output"])
	if len(output) != 3 || jsonutil.ObjStr(jsonutil.AsObj(output[0]), "type") != "reasoning" || jsonutil.ObjStr(jsonutil.AsObj(output[1]), "type") != "reasoning" {
		t.Fatalf("不剥离 output=%v, want reasoning×2 + message", output)
	}
}

// TestAllowNoThinkBlock4Anthropic locks the top-level toggle for tool-continuation histories with no replayable signed
// thinking block (Responses translation port): true/unset (the default) sends the requested thinking mode even without
// convertOff2Low and arms the main handler's one-shot thinking-off retry (N2LTryOn); false restores cc-switch preemptive
// thinking-off — and wins over convertOff2Low's try-on door. Explicit thinking-off requests are unaffected (the stealth
// upgrade still applies). A request that wouldn't think anyway doesn't arm the fallback (no wasted round trip).
func TestAllowNoThinkBlock4Anthropic(t *testing.T) {
	mk := func(model, effort string) map[string]interface{} {
		return map[string]interface{}{
			"model": model,
			"input": []interface{}{
				map[string]interface{}{"type": "message", "role": "user", "content": "查天气"},
				map[string]interface{}{"type": "function_call", "call_id": "c1", "name": "get_weather", "arguments": "{\"city\":\"北京\"}"},
				map[string]interface{}{"type": "function_call_output", "call_id": "c1", "output": "晴"},
			},
			"reasoning": map[string]interface{}{"effort": effort},
		}
	}

	// Default (true) without convertOff2Low, budget model: the requested budget goes upstream, fallback armed.
	out, _, n2l, err := responsesToAnthropicTriple(mk("gpt-5-codex", "max"), nil, nil, "", false, true)
	if err != nil {
		t.Fatalf("default budget err: %v", err)
	}
	if n2l != N2LTryOn {
		t.Errorf("default budget: n2l=%d, want N2LTryOn", n2l)
	}
	th := jsonutil.AsObj(out["thinking"])
	if jsonutil.ObjStr(th, "type") != "enabled" || jsonutil.ToInt64(th["budget_tokens"]) != 16000 {
		t.Errorf("default budget: thinking=%v, want enabled/16000", th)
	}

	// Default (true) without convertOff2Low, adaptive route: the requested level max goes upstream, fallback armed.
	out, _, n2l, err = responsesToAnthropicTriple(mk("fable", "max"), nil, nil, "adaptive", false, true)
	if err != nil {
		t.Fatalf("default adaptive err: %v", err)
	}
	if n2l != N2LTryOn {
		t.Errorf("default adaptive: n2l=%d, want N2LTryOn", n2l)
	}
	if jsonutil.ObjStr(jsonutil.AsObj(out["thinking"]), "type") != "adaptive" || jsonutil.ObjStr(jsonutil.AsObj(out["output_config"]), "effort") != "max" {
		t.Errorf("default adaptive: thinking=%v output_config=%v, want adaptive/max", out["thinking"], out["output_config"])
	}

	// false restores cc-switch preemptive-off — and wins over convertOff2Low's try-on door.
	out, _, n2l, err = responsesToAnthropicTriple(mk("fable", "max"), nil, nil, "adaptive", true, false)
	if err != nil {
		t.Fatalf("false adaptive err: %v", err)
	}
	if n2l != N2LNone || jsonutil.ObjStr(jsonutil.AsObj(out["thinking"]), "type") != "disabled" {
		t.Errorf("false adaptive: n2l=%d thinking=%v, want N2LNone/disabled", n2l, out["thinking"])
	}
	out, _, n2l, err = responsesToAnthropicTriple(mk("gpt-5-codex", "max"), nil, nil, "", true, false)
	if err != nil {
		t.Fatalf("false budget err: %v", err)
	}
	if n2l != N2LNone || out["thinking"] != nil {
		t.Errorf("false budget: n2l=%d thinking=%v, want N2LNone/无 thinking 字段", n2l, out["thinking"])
	}

	// false does not touch the explicit-off door: effort none + convertOff2Low still stealth-upgrades.
	out, _, n2l, err = responsesToAnthropicTriple(mk("gpt-5-codex", "none"), nil, nil, "", true, false)
	if err != nil {
		t.Fatalf("false 显式关 err: %v", err)
	}
	if n2l != N2LStealth {
		t.Errorf("false 显式关: n2l=%d, want N2LStealth", n2l)
	}
	th = jsonutil.AsObj(out["thinking"])
	if jsonutil.ObjStr(th, "type") != "enabled" || jsonutil.ToInt64(th["budget_tokens"]) != 2048 {
		t.Errorf("false 显式关: thinking=%v, want enabled/2048", th)
	}

	// A request that wouldn't think anyway (no effort → no budget) doesn't arm the fallback: nothing sent, no wasted round trip.
	noEffort := mk("gpt-5-codex", "")
	delete(noEffort, "reasoning")
	out, _, n2l, err = responsesToAnthropicTriple(noEffort, nil, nil, "", false, true)
	if err != nil {
		t.Fatalf("无思考意图 err: %v", err)
	}
	if n2l != N2LNone || out["thinking"] != nil {
		t.Errorf("无思考意图: n2l=%d thinking=%v, want N2LNone/无 thinking 字段（不空跑一次）", n2l, out["thinking"])
	}
}

// TestAllowNoThinkBlock4AnthropicConfigDefault: the top-level pointer defaults to true when unset (nil config / nil field).
func TestAllowNoThinkBlock4AnthropicConfigDefault(t *testing.T) {
	if !config.AllowNoThinkBlock4Anthropic(nil) || !config.AllowNoThinkBlock4Anthropic(&config.Config{}) {
		t.Errorf("未设置应默认 true")
	}
	f := false
	if config.AllowNoThinkBlock4Anthropic(&config.Config{AllowNoThinkBlock4Anthropic: &f}) {
		t.Errorf("显式 false 应生效")
	}
	tr := true
	if !config.AllowNoThinkBlock4Anthropic(&config.Config{AllowNoThinkBlock4Anthropic: &tr}) {
		t.Errorf("显式 true 应生效")
	}
}
