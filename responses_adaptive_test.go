package main

// Tests for adaptive thinking / output_config.effort (mirroring cc-switch thinking_optimizer.rs
// and transform_codex_anthropic.rs 312-425).

import (
	"testing"
)

func TestThinkingModelClassification(t *testing.T) {
	// Mapping table copied from cc-switch thinking_optimizer.rs; normalization: lowercase + '.'/'_' → '-'.
	adaptive := []string{
		"claude-fable-5", "anthropic/claude-fable-5", "claude-mythos-5",
		"claude-mythos-preview", "claude-sonnet-5", "anthropic/claude-opus-4.8",
		"claude-opus-4-8-20250514", "claude_opus_4_7", "claude-opus-4-6", "claude-sonnet-4-6",
	}
	for _, m := range adaptive {
		if !usesAdaptiveThinking(m) {
			t.Errorf("usesAdaptiveThinking(%q)=false, want true", m)
		}
	}
	for _, m := range []string{"gpt-5-codex", "deepseek-v4-flash", "claude-sonnet-4-5", "claude-haiku-4-5", "kimi-k3"} {
		if usesAdaptiveThinking(m) {
			t.Errorf("usesAdaptiveThinking(%q)=true, want false", m)
		}
	}
	// The subset that defaults to adaptive on.
	if !adaptiveThinkingIsDefault("claude-fable-5") || !adaptiveThinkingIsDefault("claude-sonnet-5") {
		t.Errorf("fable-5/sonnet-5 应默认开 adaptive")
	}
	if adaptiveThinkingIsDefault("claude-opus-4-8") {
		t.Errorf("opus-4-8 不应默认开 adaptive")
	}
	// Only fable-5/mythos-5 can't disable thinking.
	if !thinkingCannotBeDisabled("claude-fable-5") || !thinkingCannotBeDisabled("claude-mythos-5") {
		t.Errorf("fable-5/mythos-5 应关不掉 thinking")
	}
	if thinkingCannotBeDisabled("claude-sonnet-5") || thinkingCannotBeDisabled("claude-opus-4-8") {
		t.Errorf("sonnet-5/opus-4-8 应能关 thinking")
	}
}

func TestCodexEffortToAnthropic(t *testing.T) {
	cases := map[string]string{
		"minimal": "low", "low": "low", "medium": "medium",
		"high": "high", "xhigh": "max", "max": "max", "ultra": "max",
	}
	for effort, want := range cases {
		if got := codexEffortToAnthropic(effort); got != want {
			t.Errorf("effort=%s got=%q, want %q", effort, got, want)
		}
	}
	for _, effort := range []string{"", "none", "off", "disabled", "weird"} {
		if got := codexEffortToAnthropic(effort); got != "" {
			t.Errorf("effort=%q got=%q, want 空", effort, got)
		}
	}
	for _, effort := range []string{"none", "off", "disabled", "NONE", " Off "} {
		if !reasoningExplicitlyDisabled(effort) {
			t.Errorf("reasoningExplicitlyDisabled(%q)=false, want true", effort)
		}
	}
	if reasoningExplicitlyDisabled("low") || reasoningExplicitlyDisabled("") {
		t.Errorf("low/空 不算显式关闭")
	}
}

func TestAdaptiveThinkingDefaultModel(t *testing.T) {
	// A default-adaptive model without reasoning: thinking:adaptive, no output_config.
	body := map[string]interface{}{"model": "claude-fable-5", "input": "hi"}
	out, _, err := responsesToAnthropic(body)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if objStr(asObj(out["thinking"]), "type") != "adaptive" {
		t.Errorf("thinking=%v, want adaptive", out["thinking"])
	}
	if _, ok := out["output_config"]; ok {
		t.Errorf("无 effort 不应有 output_config: %v", out["output_config"])
	}
	// Thinking on → temperature not passed through.
	if _, ok := out["temperature"]; ok {
		t.Errorf("thinking 开启不应透传 temperature")
	}
}

func TestAdaptiveThinkingEffortMapping(t *testing.T) {
	// A non-default-adaptive model (opus-4-8): only opens adaptive + output_config when effort is present.
	body := map[string]interface{}{
		"model": "claude-opus-4-8", "input": "hi",
		"reasoning": map[string]interface{}{"effort": "high"},
	}
	out, _, err := responsesToAnthropic(body)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if objStr(asObj(out["thinking"]), "type") != "adaptive" {
		t.Errorf("thinking=%v, want adaptive", out["thinking"])
	}
	if objStr(asObj(out["output_config"]), "effort") != "high" {
		t.Errorf("output_config=%v, want effort=high", out["output_config"])
	}
	// Without effort: a non-default-adaptive model opens nothing.
	out2, _, err := responsesToAnthropic(map[string]interface{}{"model": "claude-opus-4-8", "input": "hi"})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if _, ok := out2["thinking"]; ok {
		t.Errorf("无 effort 的 opus-4-8 不应开 thinking: %v", out2["thinking"])
	}
}

func TestAdaptiveThinkingExplicitNone(t *testing.T) {
	// Un-disableable fable-5 + explicit none: still adaptive, effort clamped to low.
	body := map[string]interface{}{
		"model": "claude-fable-5", "input": "hi", "temperature": 0.5,
		"reasoning": map[string]interface{}{"effort": "none"},
	}
	out, _, err := responsesToAnthropic(body)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if objStr(asObj(out["thinking"]), "type") != "adaptive" {
		t.Errorf("thinking=%v, want adaptive（关不掉）", out["thinking"])
	}
	if objStr(asObj(out["output_config"]), "effort") != "low" {
		t.Errorf("output_config=%v, want effort=low", out["output_config"])
	}
	if _, ok := out["temperature"]; ok {
		t.Errorf("thinking 开启不应透传 temperature")
	}
	// Disable-able opus-4-8 + explicit none: thinking:disabled, no output_config, temperature passed through.
	body["model"] = "claude-opus-4-8"
	out2, _, err := responsesToAnthropic(body)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if objStr(asObj(out2["thinking"]), "type") != "disabled" {
		t.Errorf("thinking=%v, want disabled", out2["thinking"])
	}
	if _, ok := out2["output_config"]; ok {
		t.Errorf("disabled 不应有 output_config: %v", out2["output_config"])
	}
	if out2["temperature"] != 0.5 {
		t.Errorf("temperature=%v, want 0.5 透传", out2["temperature"])
	}
}

// toolTurnBody builds a "tool-continuation without signed thinking replay" request:
// a function_call followed by only function_call_output, no reasoning envelope.
func toolTurnBody(model string) map[string]interface{} {
	return map[string]interface{}{
		"model": model,
		"input": []interface{}{
			map[string]interface{}{"type": "message", "role": "user", "content": []interface{}{
				map[string]interface{}{"type": "input_text", "text": "查一下"},
			}},
			map[string]interface{}{"type": "function_call", "call_id": "c1", "name": "t", "arguments": "{}"},
			map[string]interface{}{"type": "function_call_output", "call_id": "c1", "output": "结果"},
		},
	}
}

func TestThinkingHistoryInvalid(t *testing.T) {
	// Tool continuation missing signed thinking replay, under the default allowNoThinkBlock4Anthropic=true (try-first):
	// un-disableable fable-5 → error; disable-able adaptive model (sonnet-5) → adaptive goes out as-is; non-adaptive
	// model with effort → the requested budget goes out (high → 16384 capped at 32000/2). The one-shot thinking-off
	// retry is armed in all sent cases (the wrapper drops the n2l return; arming is locked in none2low_test.go).
	// cc-switch preemptive-off outcomes now require allowNoThinkBlock4Anthropic=false — see TestAllowNoThinkBlock4Anthropic.
	if _, _, err := responsesToAnthropic(toolTurnBody("claude-fable-5")); err == nil {
		t.Errorf("fable-5 历史无效应报错")
	}
	out, _, err := responsesToAnthropic(toolTurnBody("claude-sonnet-5"))
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if objStr(asObj(out["thinking"]), "type") != "adaptive" {
		t.Errorf("thinking=%v, want adaptive（默认先试所请思考模式）", out["thinking"])
	}
	if _, ok := out["output_config"]; ok {
		t.Errorf("sonnet-5 未给档位不应有 output_config: %v", out["output_config"])
	}
	body := toolTurnBody("gpt-5-codex")
	body["reasoning"] = map[string]interface{}{"effort": "high"}
	out2, _, err := responsesToAnthropic(body)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	th := asObj(out2["thinking"])
	if objStr(th, "type") != "enabled" || toInt64(th["budget_tokens"]) != 16000 {
		t.Errorf("thinking=%v, want enabled/16000（默认先试所请 budget）", out2["thinking"])
	}
}

func TestThinkingHistoryValidWithEnvelope(t *testing.T) {
	// Same-shape tool continuation, but with a reasoning envelope replaying signed thinking → history valid, thinking opens normally.
	enc := encodeThinkingEnvelope(map[string]interface{}{
		"type": "thinking", "thinking": "想", "signature": "sig_abc",
	})
	body := toolTurnBody("claude-sonnet-5")
	body["input"] = append(asArr(body["input"]),
		map[string]interface{}{"type": "message", "role": "user", "content": []interface{}{
			map[string]interface{}{"type": "input_text", "text": "x"},
		}},
		map[string]interface{}{"type": "reasoning", "encrypted_content": enc},
		map[string]interface{}{"type": "function_call", "call_id": "c2", "name": "t", "arguments": "{}"},
		map[string]interface{}{"type": "function_call_output", "call_id": "c2", "output": "y"},
	)
	out, _, err := responsesToAnthropic(body)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if objStr(asObj(out["thinking"]), "type") != "adaptive" {
		t.Errorf("thinking=%v, want adaptive（历史有效）", out["thinking"])
	}
}

func TestForcedToolChoiceThinkingConflict(t *testing.T) {
	tool := map[string]interface{}{
		"type": "function", "name": "get_weather",
		"parameters": map[string]interface{}{"type": "object", "properties": map[string]interface{}{}},
	}
	// Un-disableable fable-5 (default adaptive on) + forced tool_choice → error.
	body := map[string]interface{}{
		"model": "claude-fable-5", "input": "hi",
		"tools":       []interface{}{tool},
		"tool_choice": map[string]interface{}{"type": "function", "name": "get_weather"},
	}
	if _, _, err := responsesToAnthropic(body); err == nil {
		t.Errorf("fable-5 强制 tool_choice 应报错")
	}
	// Disable-able opus-4-8 + effort high + forced tool_choice → thinking:disabled,
	// output_config deleted, temperature restored, tool_choice kept.
	body["model"] = "claude-opus-4-8"
	body["reasoning"] = map[string]interface{}{"effort": "high"}
	body["temperature"] = 0.7
	out, _, err := responsesToAnthropic(body)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if objStr(asObj(out["thinking"]), "type") != "disabled" {
		t.Errorf("thinking=%v, want disabled", out["thinking"])
	}
	if _, ok := out["output_config"]; ok {
		t.Errorf("output_config 应删除: %v", out["output_config"])
	}
	if out["temperature"] != 0.7 {
		t.Errorf("temperature=%v, want 0.7 恢复", out["temperature"])
	}
	if objStr(asObj(out["tool_choice"]), "type") != "tool" {
		t.Errorf("tool_choice=%v, want tool 保留", out["tool_choice"])
	}
}

func TestTrailingTurnSupportsThinking(t *testing.T) {
	userText := map[string]interface{}{"role": "user", "content": []interface{}{
		map[string]interface{}{"type": "text", "text": "hi"},
	}}
	// Pure user question → true.
	if !trailingTurnSupportsThinking([]map[string]interface{}{userText}) {
		t.Errorf("纯 user 应支持 thinking")
	}
	assistantWithThinking := map[string]interface{}{"role": "assistant", "content": []interface{}{
		map[string]interface{}{"type": "thinking", "thinking": "想", "signature": "s"},
		map[string]interface{}{"type": "tool_use", "id": "c1", "name": "t", "input": map[string]interface{}{}},
	}}
	toolResult := map[string]interface{}{"role": "user", "content": []interface{}{
		map[string]interface{}{"type": "tool_result", "tool_use_id": "c1", "content": "结果"},
	}}
	// Signed thinking + paired ids → true.
	if !trailingTurnSupportsThinking([]map[string]interface{}{userText, assistantWithThinking, toolResult}) {
		t.Errorf("签名 thinking 配对应支持")
	}
	// Missing signed thinking → false.
	assistantNoThinking := map[string]interface{}{"role": "assistant", "content": []interface{}{
		map[string]interface{}{"type": "tool_use", "id": "c1", "name": "t", "input": map[string]interface{}{}},
	}}
	if trailingTurnSupportsThinking([]map[string]interface{}{userText, assistantNoThinking, toolResult}) {
		t.Errorf("缺签名 thinking 不应支持")
	}
	// Unpaired ids → false.
	badResult := map[string]interface{}{"role": "user", "content": []interface{}{
		map[string]interface{}{"type": "tool_result", "tool_use_id": "c9", "content": "结果"},
	}}
	if trailingTurnSupportsThinking([]map[string]interface{}{userText, assistantWithThinking, badResult}) {
		t.Errorf("id 不配对不应支持")
	}
	// Last message not user → false.
	if trailingTurnSupportsThinking([]map[string]interface{}{userText, assistantWithThinking}) {
		t.Errorf("末条 assistant 不应支持")
	}
}
