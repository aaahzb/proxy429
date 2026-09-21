package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestNone2LowUpgradeShapes 锁定 translateNone2Low 升级后的 thinking 形态：
// adaptive 模型 → adaptive+effort:low（fable-5 关不掉，low 本就是显式关的表达方式）；
// budget 模型 → enabled+2048（压顶 max_tokens/2，容不下 1024 下限则放弃升级保持关闭）；
// 开关关闭或非关思考请求一律不升级。
func TestNone2LowUpgradeShapes(t *testing.T) {
	mk := func(model, effort string) map[string]interface{} {
		return map[string]interface{}{
			"model":     model,
			"input":     "hi",
			"reasoning": map[string]interface{}{"effort": effort},
		}
	}

	// adaptive：adaptive + effort:low，标记升级。
	out, _, upgraded, err := responsesToAnthropicTriple(mk("claude-fable-5", "none"), nil, nil, "", true)
	if err != nil {
		t.Fatalf("adaptive err: %v", err)
	}
	if !upgraded {
		t.Errorf("adaptive none: upgraded=false, want true")
	}
	th := asObj(out["thinking"])
	if objStr(th, "type") != "adaptive" || objStr(asObj(out["output_config"]), "effort") != "low" {
		t.Errorf("adaptive 升级: thinking=%v output_config=%v, want adaptive/low", th, out["output_config"])
	}

	// budget：enabled+2048，标记升级，无 output_config。
	out, _, upgraded, err = responsesToAnthropicTriple(mk("gpt-5-codex", "none"), nil, nil, "", true)
	if err != nil {
		t.Fatalf("budget err: %v", err)
	}
	if !upgraded {
		t.Errorf("budget none: upgraded=false, want true")
	}
	th = asObj(out["thinking"])
	if objStr(th, "type") != "enabled" || toInt64(th["budget_tokens"]) != 2048 {
		t.Errorf("budget 升级: thinking=%v, want enabled/2048", th)
	}
	if out["output_config"] != nil {
		t.Errorf("budget 路径不应有 output_config: %v", out["output_config"])
	}

	// max_output_tokens=1000 → 压顶 500 不足 1024 下限：放弃升级，保持 disabled。
	small := mk("gpt-5-codex", "none")
	small["max_output_tokens"] = 1000
	out, _, upgraded, err = responsesToAnthropicTriple(small, nil, nil, "", true)
	if err != nil {
		t.Fatalf("small err: %v", err)
	}
	if upgraded {
		t.Errorf("压顶不足下限: upgraded=true, want false（放弃升级）")
	}
	if objStr(asObj(out["thinking"]), "type") != "disabled" {
		t.Errorf("压顶不足下限: thinking=%v, want disabled", out["thinking"])
	}

	// 开关关闭：显式关原样下发 disabled，不升级。
	out, _, upgraded, err = responsesToAnthropicTriple(mk("gpt-5-codex", "none"), nil, nil, "", false)
	if err != nil {
		t.Fatalf("off err: %v", err)
	}
	if upgraded {
		t.Errorf("开关关闭: upgraded=true, want false")
	}
	if objStr(asObj(out["thinking"]), "type") != "disabled" {
		t.Errorf("开关关闭: thinking=%v, want disabled", out["thinking"])
	}

	// 非关思考请求不升级：high 照常 enabled/16000（默认 32000 半压顶）。
	out, _, upgraded, err = responsesToAnthropicTriple(mk("gpt-5-codex", "high"), nil, nil, "", true)
	if err != nil {
		t.Fatalf("high err: %v", err)
	}
	if upgraded {
		t.Errorf("high: upgraded=true, want false（只升级关思考）")
	}
	th = asObj(out["thinking"])
	if objStr(th, "type") != "enabled" || toInt64(th["budget_tokens"]) != 16000 {
		t.Errorf("high: thinking=%v, want enabled/16000", th)
	}

	// 升级后 temperature 不透传（thinking 开启的互斥规则与正常路径一致）。
	withTemp := mk("gpt-5-codex", "none")
	withTemp["temperature"] = 0.5
	out, _, upgraded, err = responsesToAnthropicTriple(withTemp, nil, nil, "", true)
	if err != nil {
		t.Fatalf("temp err: %v", err)
	}
	if !upgraded || out["temperature"] != nil {
		t.Errorf("升级流 temperature 应被丢弃: upgraded=%v temperature=%v", upgraded, out["temperature"])
	}
}

// TestNone2LowUpgradeToolContinuation 锁定工具续轮（末轮 tool_result 前无签名思考块，
// historyValid=false）同样升级——这是 Codex 的主要请求形态；开关关闭时该路径不开思考。
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

	out, _, upgraded, err := responsesToAnthropicTriple(mk(), nil, nil, "", true)
	if err != nil {
		t.Fatalf("续轮 err: %v", err)
	}
	if !upgraded {
		t.Errorf("工具续轮: upgraded=false, want true")
	}
	th := asObj(out["thinking"])
	if objStr(th, "type") != "enabled" || toInt64(th["budget_tokens"]) != 2048 {
		t.Errorf("工具续轮升级: thinking=%v, want enabled/2048", th)
	}

	// 对照：开关关闭时工具续轮不开思考（budget 路径整体跳过，thinking 字段不出现）。
	out, _, upgraded, err = responsesToAnthropicTriple(mk(), nil, nil, "", false)
	if err != nil {
		t.Fatalf("续轮对照 err: %v", err)
	}
	if upgraded || out["thinking"] != nil {
		t.Errorf("续轮对照: upgraded=%v thinking=%v, want false/无 thinking 字段", upgraded, out["thinking"])
	}
}

// TestNone2LowToolChoiceConflict 锁定强制 tool_choice 与 thinking 互斥的处置：
// 思考被关掉（disabled、output_config 删除、temperature 恢复），升级标记复位——
// 上游不会再产思考块，回传侧无需剥离。
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
	out, _, upgraded, err := responsesToAnthropicTriple(body, nil, nil, "", true)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if upgraded {
		t.Errorf("tool_choice 冲突: upgraded=true, want false（思考已关无需剥离）")
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

// TestAnthToRespStreamStripThinking 锁定流式剥离：thinking 块不发事件、不进 items，
// 预分配的 outputIndex 回收保持后续块序号连续；usage 如实保留。
func TestAnthToRespStreamStripThinking(t *testing.T) {
	var events []string
	conv := newAnthToRespStream(func(ev string) { events = append(events, ev) }, "gpt-5-codex", nil)
	conv.stripThinking = true
	feedSSEToConv(conv, testSSEAllBlocks())

	if !conv.completed {
		t.Fatalf("流未完成")
	}
	joined := strings.Join(events, "")
	// 注意：response.completed 的 usage 恒带 reasoning_tokens 字段名（0 也在），
	// 不能拿裸 "reasoning" 子串判；钉死思考块会产的项类型与 summary 事件。
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
	// 4 个项占 output_index 0..3：序号 4 出现说明剥离块没回收序号。
	if strings.Contains(joined, `"output_index":4`) {
		t.Errorf("output_index 应连续（0..3），不应出现 4")
	}
	// usage 不碰：fixture 的 output_tokens=9 如实进最终响应。
	if toInt64(asObj(final["usage"])["output_tokens"]) != 9 {
		t.Errorf("usage 应如实透传: %v", final["usage"])
	}
}

// TestTranslatingWriterStripThinkingNonStream 锁定非流式整转（finishBuffered）剥离：
// 客户端拿到的一次性 JSON 里没有 reasoning 项。
func TestTranslatingWriterStripThinkingNonStream(t *testing.T) {
	rec := httptest.NewRecorder()
	tw := newTranslatingWriter(rec, false, "gpt-5-codex", nil)
	// 与 responsesHandler 同款接线：conv 管 SSE 流（本用例走这里），
	// tw.stripThinking 管上游回非 SSE JSON 的整转兜底（finishBuffered）。
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

// TestAnthropicToResponsesObjectStripThinking 锁定整转剥离的对象级行为：
// thinking 与 redacted_thinking 整块丢弃；usage 原样保留。
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

	// 对照：不剥离时两个思考块各产一个 reasoning 项。
	out = anthropicToResponsesObject(msg, "m", nil, nil, false)
	output = asArr(out["output"])
	if len(output) != 3 || objStr(asObj(output[0]), "type") != "reasoning" || objStr(asObj(output[1]), "type") != "reasoning" {
		t.Fatalf("不剥离 output=%v, want reasoning×2 + message", output)
	}
}

// TestDisableThinkingInBody 锁定 400 兜底手术：thinking 改 disabled、output_config 删除，
// 其余字段与排版原样；非 JSON 对象返回 ok=false。
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

// TestNone2LowFallbackRetry 端到端：Responses 关思考请求被升级为 enabled/2048 发上游 →
// 上游 200+体内错误拒 thinking → 兜底改回 disabled 立即重发（不烧退避预算）→
// 客户端拿到 200，回传侧思考块被剥离。
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
			// 第一次：上游应收到升级后的 low 思考（budget 口径 enabled+2048）。
			th := asObj(m["thinking"])
			if objStr(th, "type") != "enabled" || toInt64(th["budget_tokens"]) != 2048 {
				t.Errorf("首请求 thinking=%v, want enabled/2048", th)
			}
			// 以 200+体内错误拒绝（Kimi 风格），错误文本含 thinking 触发兜底。
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
		Upstream:          mock.URL,
		MaxRetries:        0,
		TotalBudgetSec:    10,
		TranslateNone2Low: true,
		Routes:            []RouteRule{{Pattern: "gpt-5*", URL: mock.URL, Model: "deepseek-v4-flash"}},
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
	// 回传侧剥离思考块（stripThinking 仍在生效）：5 块 - 思考 + 搜索信封 = 5 项。
	output := asArr(out["output"])
	if len(output) != 5 {
		t.Errorf("output 数=%d, want 5（思考剥离+搜索信封随行）: %v", len(output), output)
	}
}
