package proxy

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"proxy429/internal/config"
	"proxy429/internal/jsonutil"
	"proxy429/internal/translate"
	"strings"
	"testing"
)

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
			th := jsonutil.AsObj(m["thinking"])
			if jsonutil.ObjStr(th, "type") != "enabled" || jsonutil.ToInt64(th["budget_tokens"]) != 2048 {
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

	config.Store(&config.Config{
		Upstream:       mock.URL,
		MaxRetries:     0,
		TotalBudgetSec: 10,
		Routes:         []config.RouteRule{{Pattern: "gpt-5*", URL: mock.URL, Model: "deepseek-v4-flash", ConvertOff2Low: "translate"}},
	})
	defer config.Store(&config.Config{})

	proxy := httptest.NewServer(http.HandlerFunc(translate.ResponsesHandler(handler)))
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
	if jsonutil.ObjStr(jsonutil.AsObj(secondBody["thinking"]), "type") != "disabled" {
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
	output := jsonutil.AsArr(out["output"])
	if len(output) != 5 {
		t.Errorf("output 数=%d, want 5（思考剥离+搜索信封随行）: %v", len(output), output)
	}
}

// TestNone2LowFallbackOn400 pins the order between the semantic fallbacks and the deterministic-4xx fast-fail:
// the same thinking rejection arrives as an HTTP 400 JSON error body here (not a 200 SSE error); the n2l fallback
// must still get its one shot — thinking flipped off and resent — before the 400 is passed downstream.
func TestNone2LowFallbackOn400(t *testing.T) {
	resetStats()
	calls := 0
	var secondBody map[string]interface{}
	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		var m map[string]interface{}
		_ = json.Unmarshal(b, &m)
		calls++
		if calls == 1 {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			io.WriteString(w, `{"type":"error","error":{"type":"invalid_request_error","message":"thinking is not allowed without signed history"}}`)
			return
		}
		secondBody = m
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		io.WriteString(w, testSSEAllBlocks())
	}))
	defer mock.Close()

	config.Store(&config.Config{
		Upstream:       mock.URL,
		MaxRetries:     0,
		TotalBudgetSec: 10,
		Routes:         []config.RouteRule{{Pattern: "gpt-5*", URL: mock.URL, Model: "deepseek-v4-flash", ConvertOff2Low: "translate"}},
	})
	defer config.Store(&config.Config{})

	proxy := httptest.NewServer(http.HandlerFunc(translate.ResponsesHandler(handler)))
	defer proxy.Close()

	resp, err := http.Post(proxy.URL+"/v1/responses", "application/json",
		strings.NewReader(`{"model":"gpt-5-codex","input":"hi","reasoning":{"effort":"none"}}`))
	if err != nil {
		t.Fatalf("请求失败: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()

	if calls != 2 {
		t.Fatalf("上游请求数=%d, want 2（400 也必须先走一次兜底，不能被快停吞掉）", calls)
	}
	if jsonutil.ObjStr(jsonutil.AsObj(secondBody["thinking"]), "type") != "disabled" {
		t.Errorf("兜底重发 thinking=%v, want disabled", secondBody["thinking"])
	}
	if resp.StatusCode != 200 {
		t.Fatalf("客户端状态=%d, want 200: %s", resp.StatusCode, body)
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

	config.Store(&config.Config{
		Upstream:       mock.URL,
		MaxRetries:     0,
		TotalBudgetSec: 10,
		Routes:         []config.RouteRule{{Pattern: "gpt-5*", URL: mock.URL, Model: "deepseek-v4-flash", ConvertOff2Low: "translate"}},
	})
	defer config.Store(&config.Config{})

	proxy := httptest.NewServer(http.HandlerFunc(translate.ResponsesHandler(handler)))
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
	th := jsonutil.AsObj(gotBody["thinking"])
	if jsonutil.ObjStr(th, "type") != "enabled" || jsonutil.ToInt64(th["budget_tokens"]) != 16000 {
		t.Errorf("上游 thinking=%v, want enabled/16000（所请 max 压顶 32000/2）", th)
	}

	// No stripping on return: the output must carry reasoning items (thinking blocks return to the downstream as-is).
	var out map[string]interface{}
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("响应不是 JSON: %v (%s)", err, raw)
	}
	foundReasoning := false
	for _, it := range jsonutil.AsArr(out["output"]) {
		if jsonutil.ObjStr(jsonutil.AsObj(it), "type") == "reasoning" {
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

	config.Store(&config.Config{
		Upstream:       mock.URL,
		MaxRetries:     0,
		TotalBudgetSec: 10,
		Routes:         []config.RouteRule{{Pattern: "gpt-5*", URL: mock.URL, Model: "deepseek-v4-flash"}},
	})
	defer config.Store(&config.Config{})

	proxy := httptest.NewServer(http.HandlerFunc(translate.ResponsesHandler(handler)))
	defer proxy.Close()

	resp, err := http.Post(proxy.URL+"/v1/responses", "application/json",
		strings.NewReader(`{"model":"gpt-5-codex","input":"hi","reasoning":{"effort":"none"}}`))
	if err != nil {
		t.Fatalf("请求失败: %v", err)
	}
	resp.Body.Close()
	if jsonutil.ObjStr(jsonutil.AsObj(gotBody["thinking"]), "type") != "disabled" {
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

	config.Store(&config.Config{
		Upstream:       mock.URL,
		MaxRetries:     0,
		TotalBudgetSec: 10,
		Routes:         []config.RouteRule{{Pattern: "gpt-5*", URL: mock.URL, Model: "deepseek-v4-flash", ConvertOff2Low: "all"}},
	})
	defer config.Store(&config.Config{})

	proxy := httptest.NewServer(http.HandlerFunc(translate.ResponsesHandler(handler)))
	defer proxy.Close()

	resp, err := http.Post(proxy.URL+"/v1/responses", "application/json",
		strings.NewReader(`{"model":"gpt-5-codex","input":"hi","reasoning":{"effort":"none"}}`))
	if err != nil {
		t.Fatalf("请求失败: %v", err)
	}
	resp.Body.Close()
	th := jsonutil.AsObj(gotBody["thinking"])
	if jsonutil.ObjStr(th, "type") != "enabled" || jsonutil.ToInt64(th["budget_tokens"]) != 2048 {
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

	config.Store(&config.Config{
		Upstream:       mock.URL,
		MaxRetries:     0,
		TotalBudgetSec: 10,
		Routes:         []config.RouteRule{{Pattern: "*", URL: mock.URL, Model: "deepseek-v4-flash"}},
		FastRoute:      &config.FastRoute{URL: mock.URL, Model: "deepseek-v4-flash", ConvertOff2Low: "translate"},
	})
	defer config.Store(&config.Config{})

	proxy := httptest.NewServer(http.HandlerFunc(translate.ResponsesHandler(handler)))
	defer proxy.Close()

	resp, err := http.Post(proxy.URL+"/v1/responses", "application/json",
		strings.NewReader(`{"model":"fast_route","input":"hi","reasoning":{"effort":"none"}}`))
	if err != nil {
		t.Fatalf("请求失败: %v", err)
	}
	resp.Body.Close()
	th := jsonutil.AsObj(gotBody["thinking"])
	if jsonutil.ObjStr(th, "type") != "enabled" || jsonutil.ToInt64(th["budget_tokens"]) != 2048 {
		t.Errorf("fast_route 自带 convertOff2Low 应升级: thinking=%v, want enabled/2048", th)
	}
}

// TestAllowNoThinkBlockDefaultEndToEnd: with NO convertOff2Low on the route and the config key unset (default true),
// a tool-continuation with effort max still goes upstream thinking-on at the requested level (fallback armed) —
// the pre-toggle coupling of the history door to convertOff2Low is gone.
func TestAllowNoThinkBlockDefaultEndToEnd(t *testing.T) {
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

	config.Store(&config.Config{
		Upstream:       mock.URL,
		MaxRetries:     0,
		TotalBudgetSec: 10,
		Routes:         []config.RouteRule{{Pattern: "gpt-5*", URL: mock.URL, Model: "deepseek-v4-flash"}},
	})
	defer config.Store(&config.Config{})

	proxy := httptest.NewServer(http.HandlerFunc(translate.ResponsesHandler(handler)))
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

	// What the upstream receives is thinking-on at the requested level (enabled/16000) even without convertOff2Low.
	th := jsonutil.AsObj(gotBody["thinking"])
	if jsonutil.ObjStr(th, "type") != "enabled" || jsonutil.ToInt64(th["budget_tokens"]) != 16000 {
		t.Errorf("默认 true: 上游 thinking=%v, want enabled/16000（所请 max 压顶 32000/2）", th)
	}

	// Armed but not rejected: the return side keeps thinking blocks (tryOn never strips).
	var out map[string]interface{}
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("响应不是 JSON: %v (%s)", err, raw)
	}
	foundReasoning := false
	for _, it := range jsonutil.AsArr(out["output"]) {
		if jsonutil.ObjStr(jsonutil.AsObj(it), "type") == "reasoning" {
			foundReasoning = true
		}
	}
	if !foundReasoning {
		t.Errorf("默认 true: output 应有 reasoning 项（兜底开思考不剥离）: %v", out["output"])
	}
}

// TestAllowNoThinkBlockFalseEndToEnd: config false restores cc-switch mode end-to-end — the same tool-continuation
// goes upstream with no thinking field at all (thinking preemptively off over the non-replayable history).
func TestAllowNoThinkBlockFalseEndToEnd(t *testing.T) {
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

	off := false
	config.Store(&config.Config{
		Upstream:                    mock.URL,
		MaxRetries:                  0,
		TotalBudgetSec:              10,
		Routes:                      []config.RouteRule{{Pattern: "gpt-5*", URL: mock.URL, Model: "deepseek-v4-flash"}},
		AllowNoThinkBlock4Anthropic: &off,
	})
	defer config.Store(&config.Config{})

	proxy := httptest.NewServer(http.HandlerFunc(translate.ResponsesHandler(handler)))
	defer proxy.Close()

	body := `{"model":"gpt-5-codex","input":[
		{"type":"message","role":"user","content":"查天气"},
		{"type":"function_call","call_id":"c1","name":"get_weather","arguments":"{\"city\":\"北京\"}"},
		{"type":"function_call_output","call_id":"c1","output":"晴"}
	],"reasoning":{"effort":"max"}}`
	resp, err := http.Post(proxy.URL+"/v1/responses", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("请求失败: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("客户端状态=%d, want 200", resp.StatusCode)
	}
	if gotBody["thinking"] != nil {
		t.Errorf("false: 上游不应带 thinking 字段: %v", gotBody["thinking"])
	}
}
