package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// testSSEAllBlocks builds an SSE stream with every block type:
// thinking + server_tool_use + web_search_tool_result (with encrypted_content inlined whole) + tool_use (input_json_delta in fragments) + text.
// A server_tool_use only counts as a real search with a query — an empty query is dropped by the Responses translation layer as a Kimi empty-search placeholder.
func testSSEAllBlocks() string {
	return "" +
		`event: message_start` + "\n" +
		`data: {"type":"message_start","message":{"id":"msg_9","type":"message","role":"assistant","model":"deepseek-v4-flash","content":[],"stop_reason":null,"usage":{"input_tokens":77,"cache_read_input_tokens":11,"output_tokens":1}}}` + "\n\n" +
		`event: content_block_start` + "\n" +
		`data: {"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":"","signature":""}}` + "\n\n" +
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"先想一想"}}` + "\n\n" +
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"signature_delta","signature":"sig_abc"}}` + "\n\n" +
		`event: content_block_stop` + "\n" +
		`data: {"type":"content_block_stop","index":0}` + "\n\n" +
		`event: content_block_start` + "\n" +
		`data: {"type":"content_block_start","index":1,"content_block":{"type":"server_tool_use","id":"call_00_x","name":"web_search","input":{"query":"测试查询"}}}` + "\n\n" +
		`event: content_block_stop` + "\n" +
		`data: {"type":"content_block_stop","index":1}` + "\n\n" +
		`event: content_block_start` + "\n" +
		`data: {"type":"content_block_start","index":2,"content_block":{"type":"web_search_tool_result","tool_use_id":"call_00_x","content":[{"type":"web_search_result","title":"测试结果","url":"https://example.com/a","encrypted_content":"ENC_BLOB_123"}]}}` + "\n\n" +
		`event: content_block_stop` + "\n" +
		`data: {"type":"content_block_stop","index":2}` + "\n\n" +
		`event: content_block_start` + "\n" +
		`data: {"type":"content_block_start","index":3,"content_block":{"type":"tool_use","id":"toolu_1","name":"get_weather","input":{}}}` + "\n\n" +
		`data: {"type":"content_block_delta","index":3,"delta":{"type":"input_json_delta","partial_json":"{\"city\":"}}` + "\n\n" +
		`data: {"type":"content_block_delta","index":3,"delta":{"type":"input_json_delta","partial_json":"\"北京\"}"}}` + "\n\n" +
		`event: content_block_stop` + "\n" +
		`data: {"type":"content_block_stop","index":3}` + "\n\n" +
		`event: content_block_start` + "\n" +
		`data: {"type":"content_block_start","index":4,"content_block":{"type":"text","text":""}}` + "\n\n" +
		`data: {"type":"content_block_delta","index":4,"delta":{"type":"text_delta","text":"答案在这"}}` + "\n\n" +
		`event: content_block_stop` + "\n" +
		`data: {"type":"content_block_stop","index":4}` + "\n\n" +
		`event: message_delta` + "\n" +
		`data: {"type":"message_delta","delta":{"stop_reason":"end_turn","stop_sequence":null},"usage":{"output_tokens":9}}` + "\n\n" +
		`event: message_stop` + "\n" +
		`data: {"type":"message_stop"}` + "\n\n"
}

// TestCollectStreamToJSONAllBlocks verifies all block types rebuild as-is:
// thinking (delta accumulation), server_tool_use, web_search_tool_result (encrypted_content verbatim),
// tool_use (input_json_delta assembled into an object), text, plus usage merging and model write-back.
func TestCollectStreamToJSONAllBlocks(t *testing.T) {
	resetStats()
	sse := testSSEAllBlocks()
	br := bufio.NewReader(bytes.NewReader([]byte(sse)))
	head := peekHead(br, 8192)
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
		Body:       io.NopCloser(bytes.NewReader([]byte(sse))),
	}
	rec := httptest.NewRecorder()
	f := &flight{id: 1, origModel: "claude-sonnet-5", targetModel: "deepseek-v4-flash", start: time.Now()}
	ok, _ := collectStreamToJSON(rec, resp, head, br, f)
	if !ok {
		t.Fatalf("ok=false, want true")
	}
	var m map[string]interface{}
	if err := json.Unmarshal(rec.Body.Bytes(), &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if m["id"] != "msg_9" || m["role"] != "assistant" {
		t.Errorf("骨架字段错误: id=%v role=%v", m["id"], m["role"])
	}
	if m["model"] != "claude-sonnet-5" {
		t.Errorf("model=%v, want 回写 claude-sonnet-5", m["model"])
	}
	if m["stop_reason"] != "end_turn" {
		t.Errorf("stop_reason=%v, want end_turn", m["stop_reason"])
	}
	// Usage merge: start's input/cache_read + delta's output.
	usage, _ := m["usage"].(map[string]interface{})
	if usage["input_tokens"] != float64(77) || usage["cache_read_input_tokens"] != float64(11) || usage["output_tokens"] != float64(9) {
		t.Errorf("usage 合并错误: %v", usage)
	}
	content, _ := m["content"].([]interface{})
	if len(content) != 5 {
		t.Fatalf("content 块数=%d, want 5", len(content))
	}
	b0, _ := content[0].(map[string]interface{})
	if b0["type"] != "thinking" || b0["thinking"] != "先想一想" || b0["signature"] != "sig_abc" {
		t.Errorf("thinking 块重建错误: %v", b0)
	}
	b1, _ := content[1].(map[string]interface{})
	if b1["type"] != "server_tool_use" || b1["id"] != "call_00_x" || b1["name"] != "web_search" {
		t.Errorf("server_tool_use 块重建错误: %v", b1)
	}
	// web_search_tool_result: encrypted_content must be preserved verbatim (replay decryption depends on it).
	b2, _ := content[2].(map[string]interface{})
	items, _ := b2["content"].([]interface{})
	if len(items) != 1 {
		t.Fatalf("搜索结果条数=%d, want 1", len(items))
	}
	r0, _ := items[0].(map[string]interface{})
	if r0["encrypted_content"] != "ENC_BLOB_123" || r0["url"] != "https://example.com/a" || r0["title"] != "测试结果" {
		t.Errorf("web_search_result 重建错误: %v", r0)
	}
	// tool_use: the string assembled from input_json_delta must be parsed into an object.
	b3, _ := content[3].(map[string]interface{})
	inp, _ := b3["input"].(map[string]interface{})
	if inp["city"] != "北京" {
		t.Errorf("tool_use input 重建错误: %v", b3["input"])
	}
	b4, _ := content[4].(map[string]interface{})
	if b4["type"] != "text" || b4["text"] != "答案在这" {
		t.Errorf("text 块重建错误: %v", b4)
	}
}
