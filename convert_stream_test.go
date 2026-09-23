package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// --- convertAlltoStream: global non-streaming to streaming conversion ---

// TestForceStreamTrue verifies rewriting the top-level stream field to true (streaming locate + text replacement, no re-serialization).
func TestForceStreamTrue(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"false改true", `{"stream":false,"model":"x"}`, `{"stream":true,"model":"x"}`},
		{"已是true不变", `{"stream":true,"model":"x"}`, `{"stream":true,"model":"x"}`},
		{"缺失则追加", `{"model":"x"}`, `{"model":"x","stream":true}`},
		{"空对象追加", `{}`, `{"stream":true}`},
		{"中间字段缺失", `{"a":1,"model":"x"}`, `{"a":1,"model":"x","stream":true}`},
		{"非JSON原样", `not json`, `not json`},
	}
	for _, tc := range cases {
		got := string(forceStreamTrue([]byte(tc.in)))
		if got != tc.want {
			t.Errorf("%s: forceStreamTrue(%q) = %q, want %q", tc.name, tc.in, got, tc.want)
		}
	}
}

// testSSEStream builds an Anthropic SSE stream (message_start -> text -> message_delta -> message_stop).
func testSSEStream() string {
	return "" +
		`event: message_start` + "\n" +
		`data: {"type":"message_start","message":{"id":"msg_123","type":"message","role":"assistant","model":"deepseek-v4-flash","content":[],"usage":{"input_tokens":100,"cache_read_input_tokens":50,"output_tokens":1}}}` + "\n\n" +
		`event: content_block_start` + "\n" +
		`data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}` + "\n\n" +
		`event: content_block_delta` + "\n" +
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"你好，"}}` + "\n\n" +
		`event: content_block_delta` + "\n" +
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"世界"}}` + "\n\n" +
		`event: message_delta` + "\n" +
		`data: {"type":"message_delta","delta":{"stop_reason":"end_turn","stop_sequence":null},"usage":{"output_tokens":42}}` + "\n\n" +
		`event: message_stop` + "\n" +
		`data: {"type":"message_stop"}` + "\n\n"
}

// TestCollectStreamToJSON verifies rebuilding a complete SSE stream into a non-streaming message JSON returned in one shot.
func TestCollectStreamToJSON(t *testing.T) {
	resetStats()
	sse := testSSEStream()
	br := bufio.NewReader(bytes.NewReader([]byte(sse)))
	head := peekHead(br, 8192)
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
		Body:       io.NopCloser(bytes.NewReader([]byte(sse))),
	}
	rec := httptest.NewRecorder()
	f := &flight{id: 1, origModel: "claude-opus-4-1", targetModel: "deepseek-v4-flash", start: time.Now()}
	ok, out := collectStreamToJSON(rec, resp, head, br, f)
	if !ok {
		t.Fatalf("ok=false, want true")
	}
	if out != 42 {
		t.Errorf("out=%d, want 42", out)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type=%q, want application/json", ct)
	}
	var m map[string]interface{}
	if err := json.Unmarshal(rec.Body.Bytes(), &m); err != nil {
		t.Fatalf("响应不是合法 JSON: %v", err)
	}
	if m["type"] != "message" || m["role"] != "assistant" {
		t.Errorf("type/role 错误: %v / %v", m["type"], m["role"])
	}
	if m["id"] != "msg_123" {
		t.Errorf("id=%v, want msg_123", m["id"])
	}
	// Route-rewrite scenario: model is written back to the client's original model.
	if m["model"] != "claude-opus-4-1" {
		t.Errorf("model=%v, want 回写 claude-opus-4-1", m["model"])
	}
	content, _ := m["content"].([]interface{})
	if len(content) != 1 {
		t.Fatalf("content 块数=%d, want 1", len(content))
	}
	blk, _ := content[0].(map[string]interface{})
	if blk["text"] != "你好，世界" {
		t.Errorf("text=%v, want 你好，世界", blk["text"])
	}
	usage, _ := m["usage"].(map[string]interface{})
	if usage["input_tokens"] != float64(100) || usage["cache_read_input_tokens"] != float64(50) || usage["output_tokens"] != float64(42) {
		t.Errorf("usage 错误: %v", usage)
	}
	if m["stop_reason"] != "end_turn" {
		t.Errorf("stop_reason=%v, want end_turn", m["stop_reason"])
	}
	if f.outTokens != 42 || f.inTokens != 100 || f.cacheRead != 50 {
		t.Errorf("flight 统计错误: out=%d in=%d cr=%d", f.outTokens, f.inTokens, f.cacheRead)
	}
	if stats.outputTokens != 42 {
		t.Errorf("全局 outputTokens=%d, want 42", stats.outputTokens)
	}
}

// TestCollectStreamToJSONInterrupted verifies nothing is written to the client when the stream breaks midway (wholesale retry stays possible).
func TestCollectStreamToJSONInterrupted(t *testing.T) {
	resetStats()
	// Missing message_stop: the stream is incomplete.
	sse := `event: message_start` + "\n" +
		`data: {"type":"message_start","message":{"id":"msg_1","model":"x","usage":{"input_tokens":5}}}` + "\n\n" +
		`event: content_block_delta` + "\n" +
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"半截"}}` + "\n\n"
	br := bufio.NewReader(bytes.NewReader([]byte(sse)))
	head := peekHead(br, 8192)
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
		Body:       io.NopCloser(bytes.NewReader([]byte(sse))),
	}
	rec := httptest.NewRecorder()
	f := &flight{id: 1, origModel: "x", start: time.Now()}
	ok, _ := collectStreamToJSON(rec, resp, head, br, f)
	if ok {
		t.Fatalf("ok=true, want false（未收完不能返回）")
	}
	if rec.Body.Len() != 0 {
		t.Errorf("不完整流不应写客户端, got %q", rec.Body.String())
	}
}

// TestHandlerConvertAllToStream end-to-end: a non-streaming request is converted to streaming upstream, and the client receives non-streaming JSON.
func TestHandlerConvertAllToStream(t *testing.T) {
	resetStats()
	var gotBody map[string]interface{}
	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		gotBody = parseBody(t, b)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		fmt.Fprint(w, testSSEStream())
	}))
	defer mock.Close()

	cfg.Store(&Config{
		Upstream:          mock.URL,
		MaxRetries:        0,
		TotalBudgetSec:    10,
		ConvertAllToStream: true,
		Routes: []RouteRule{
			{Pattern: "claude-opus*", URL: mock.URL, Model: "deepseek-v4-flash"},
		},
	})

	proxy := httptest.NewServer(http.HandlerFunc(handler))
	defer proxy.Close()

	resp, err := http.Post(proxy.URL+"/v1/messages", "application/json",
		strings.NewReader(`{"model":"claude-opus-4-1","stream":false,"messages":[{"role":"user","content":"hi"}]}`))
	if err != nil {
		t.Fatalf("请求失败: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()

	// The request the upstream received should have been converted to streaming, with model rewritten by routing.
	if s, ok := gotBody["stream"].(bool); !ok || !s {
		t.Errorf("上游收到 stream=%v, want true", gotBody["stream"])
	}
	if gotBody["model"] != "deepseek-v4-flash" {
		t.Errorf("上游收到 model=%v, want deepseek-v4-flash（路由改写）", gotBody["model"])
	}
	// The client should receive non-streaming JSON (no SSE traces), with model written back to the original value.
	if ct := resp.Header.Get("Content-Type"); ct != "application/json" {
		t.Errorf("Content-Type=%q, want application/json", ct)
	}
	if strings.Contains(string(body), "event:") || strings.Contains(string(body), "data:") {
		t.Errorf("响应含 SSE 痕迹，应为非流式 JSON: %q", string(body))
	}
	var m map[string]interface{}
	if err := json.Unmarshal(body, &m); err != nil {
		t.Fatalf("响应不是合法 JSON: %v", err)
	}
	if m["model"] != "claude-opus-4-1" {
		t.Errorf("model=%v, want 回写 claude-opus-4-1", m["model"])
	}
}

// TestHandlerConvertAllToStreamDisabled verifies non-streaming requests pass through unchanged when the config is off.
func TestHandlerConvertAllToStreamDisabled(t *testing.T) {
	resetStats()
	var gotStream interface{}
	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		gotStream = parseBody(t, b)["stream"]
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(200)
		fmt.Fprint(w, `{"type":"message","id":"m1","model":"x","content":[{"type":"text","text":"hi"}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`)
	}))
	defer mock.Close()

	cfg.Store(&Config{Upstream: mock.URL, MaxRetries: 0, TotalBudgetSec: 10})

	proxy := httptest.NewServer(http.HandlerFunc(handler))
	defer proxy.Close()

	resp, err := http.Post(proxy.URL+"/v1/messages", "application/json",
		strings.NewReader(`{"model":"x","stream":false,"messages":[{"role":"user","content":"hi"}]}`))
	if err != nil {
		t.Fatalf("请求失败: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()

	if s, ok := gotStream.(bool); !ok || s {
		t.Errorf("上游收到 stream=%v, want false（未开启时不改写）", gotStream)
	}
	if !strings.HasPrefix(string(body), `{"type":"message"`) {
		t.Errorf("响应应原样透传, got %q", string(body))
	}
}
