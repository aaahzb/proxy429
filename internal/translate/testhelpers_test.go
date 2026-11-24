package translate

// testhelpers_test.go: helpers shared by this package's tests. The root package keeps its own copies for its
// integration tests (the two packages' tests cannot share unexported helpers).

import (
	"bytes"
	"encoding/json"
	"testing"
)

// topLevelKeys extracts the key order of a JSON top-level object.
func topLevelKeys(t *testing.T, b []byte) []string {
	t.Helper()
	dec := json.NewDecoder(bytes.NewReader(b))
	tok, err := dec.Token()
	if err != nil || tok != json.Delim('{') {
		t.Fatalf("不是 object: %v", tok)
	}
	var keys []string
	for dec.More() {
		k, err := dec.Token()
		if err != nil {
			t.Fatalf("读 key: %v", err)
		}
		keys = append(keys, k.(string))
		var v json.RawMessage
		dec.Decode(&v)
	}
	return keys
}

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
