package main

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// testSearchBlockPair builds a pair of real search blocks (server_tool_use with query + result block with encrypted_content).
func testSearchBlockPair() (map[string]interface{}, map[string]interface{}) {
	use := map[string]interface{}{
		"type": "server_tool_use", "id": "srvtoolu_1", "name": "web_search",
		"input": map[string]interface{}{"query": "测试查询"},
	}
	res := map[string]interface{}{
		"type": "web_search_tool_result", "tool_use_id": "srvtoolu_1",
		"content": []interface{}{map[string]interface{}{
			"type": "web_search_result", "title": "测试结果",
			"url": "https://example.com/a", "encrypted_content": "ENC_BLOB_123",
		}},
	}
	return use, res
}

// TestSearchEnvelopeCodec verifies search-envelope encode/decode round-trip, rejection of malformed shapes, and payload obfuscation
// (key/url never stored bare; a wrong key can't decode).
func TestSearchEnvelopeCodec(t *testing.T) {
	use, res := testSearchBlockPair()
	tri := newSearchTriple("https://api.kimi.com/coding/", "k3-256k", "sk-secret")

	enc := encodeSearchEnvelope(tri, use, res)
	if !strings.HasPrefix(enc, searchEnvelopePrefix) {
		t.Fatalf("信封无前缀: %q", enc)
	}
	decTri, blocks, ts, ok := decodeSearchEnvelope(enc, tri.mask)
	if !ok {
		t.Fatalf("round-trip 解码失败")
	}
	if decTri.URL != tri.URL || decTri.Model != tri.Model || decTri.KeyH != tri.KeyH {
		t.Errorf("三元组=%+v, want %+v", decTri, tri)
	}
	if len(blocks) != 2 || objStr(blocks[0], "id") != "srvtoolu_1" ||
		objStr(blocks[1], "tool_use_id") != "srvtoolu_1" {
		t.Errorf("块还原错误: %v", blocks)
	}
	// The sealing moment round-trips with the payload (the basis for time-rule stripping); old envelopes without the ts field decode to the zero value.
	if ts.IsZero() || time.Since(ts) > time.Minute {
		t.Errorf("封入时刻=%v, want 接近当前", ts)
	}
	noTs := func() string {
		b, _ := json.Marshal(map[string]interface{}{"t": tri, "b": []interface{}{use, res}})
		return searchEnvelopePrefix + base64.RawURLEncoding.EncodeToString(xorSearchMask(tri.mask, b))
	}
	if _, _, ts2, ok := decodeSearchEnvelope(noTs(), tri.mask); !ok || !ts2.IsZero() {
		t.Errorf("无 ts 老信封: ok=%v ts=%v, want ok=true 且 ts 零值（按超龄剥）", ok, ts2)
	}

	// id normalization: Kimi streaming search's stu.id starts with tool_, and the registry only recognizes the result block's srvtoolu_
	// id (2026-09-10 controlled experiment: verbatim replay 400'd, rewriting stu.id returned 200). Normalized at sealing time,
	// without mutating the caller's shared blocks.
	misUse := map[string]interface{}{
		"type": "server_tool_use", "id": "tool_abc", "name": "web_search",
		"input": map[string]interface{}{"query": "测试查询"},
	}
	misRes := map[string]interface{}{
		"type": "web_search_tool_result", "tool_use_id": "srvtoolu_xyz",
		"content": []interface{}{map[string]interface{}{
			"type": "web_search_result", "url": "https://example.com/a", "encrypted_content": "ENC",
		}},
	}
	enc2 := encodeSearchEnvelope(tri, misUse, misRes)
	_, blocks2, _, ok2 := decodeSearchEnvelope(enc2, tri.mask)
	if !ok2 || objStr(blocks2[0], "id") != "srvtoolu_xyz" {
		t.Fatalf("错配 id 未归一: ok=%v blocks=%v", ok2, blocks2)
	}
	if objStr(misUse, "id") != "tool_abc" {
		t.Errorf("encodeSearchEnvelope 改了调用方的块: %v", misUse)
	}

	// payload obfuscation: base64-decoding yields XOR ciphertext — the key, url, and key hash never lie bare
	// (user requirement: no plaintext attribution info in client-side history); hashes are visible after XOR de-obfuscation.
	raw, _ := base64.RawURLEncoding.DecodeString(enc[len(searchEnvelopePrefix):])
	if strings.Contains(string(raw), "sk-secret") {
		t.Errorf("信封 payload 裸存了 key: %s", raw)
	}
	if strings.Contains(string(raw), "api.kimi.com") {
		t.Errorf("信封 payload 裸存了 url: %s", raw)
	}
	if strings.Contains(string(raw), tri.KeyH) {
		t.Errorf("信封 payload 裸存了 key 哈希: %s", raw)
	}
	if !strings.Contains(string(xorSearchMask(tri.mask, raw)), tri.KeyH) {
		t.Errorf("解混淆后应见 key 哈希: %s", raw)
	}
	// Wrong key: different mask, the XOR result isn't JSON, naturally undecodable (the key-leg comparison is contained in de-obfuscation).
	if _, _, _, ok := decodeSearchEnvelope(enc, searchEnvMask("sk-wrong")); ok {
		t.Errorf("错 key 不应解出信封")
	}
	if _, _, _, ok := decodeSearchEnvelope(enc, nil); ok {
		t.Errorf("无掩码不应解出信封")
	}

	// Shapes that don't encode: nil triple/blocks, empty query (empty search), maskless triple.
	if encodeSearchEnvelope(nil, use, res) != "" {
		t.Errorf("nil 三元组不应编码")
	}
	if encodeSearchEnvelope(tri, nil, res) != "" || encodeSearchEnvelope(tri, use, nil) != "" {
		t.Errorf("nil 块不应编码")
	}
	emptyUse := map[string]interface{}{"type": "server_tool_use", "id": "x", "name": "web_search"}
	if encodeSearchEnvelope(tri, emptyUse, res) != "" {
		t.Errorf("空 query 搜索不应编码（空壳没有回放价值）")
	}
	if encodeSearchEnvelope(&searchTriple{URL: "https://u/", Model: "m", KeyH: "h"}, use, res) != "" {
		t.Errorf("无掩码三元组不应编码（宁可不出信封也不发明文）")
	}

	// Shapes that are rejected: foreign prefix, bad base64, malformed payload.
	if _, _, _, ok := decodeSearchEnvelope("p429-ant-thinking-v1:e30", tri.mask); ok {
		t.Errorf("thinking 信封不应被搜索解码器认领")
	}
	if _, _, _, ok := decodeSearchEnvelope(searchEnvelopePrefix+"!!!badb64", tri.mask); ok {
		t.Errorf("坏 base64 不应解出")
	}
	bad := func(v map[string]interface{}) string {
		b, _ := json.Marshal(v)
		return searchEnvelopePrefix + base64.RawURLEncoding.EncodeToString(xorSearchMask(tri.mask, b))
	}
	if _, _, _, ok := decodeSearchEnvelope(bad(map[string]interface{}{"b": []interface{}{use, res}}), tri.mask); ok {
		t.Errorf("缺三元组不应解出")
	}
	if _, _, _, ok := decodeSearchEnvelope(bad(map[string]interface{}{"t": tri, "b": []interface{}{use}}), tri.mask); ok {
		t.Errorf("块数≠2 不应解出")
	}
	if _, _, _, ok := decodeSearchEnvelope(bad(map[string]interface{}{"t": tri, "b": []interface{}{res, use}}), tri.mask); ok {
		t.Errorf("块顺序/类型不对不应解出")
	}
}

// TestConvertInputSearchEnvelopeRestore verifies the search-envelope restoration branch in input:
// url+key same-origin → restored upstream; url different-origin (same key, unmaskable but comparison fails) → skipped;
// key different-origin (unmask fails) → skipped; reqTriple nil (route key unpredictable) → skipped
// (v1's pass-through branch retired together with the plaintext payload).
func TestConvertInputSearchEnvelopeRestore(t *testing.T) {
	use, res := testSearchBlockPair()
	tri := newSearchTriple("https://u1/", "m1", "key1")
	enc := encodeSearchEnvelope(tri, use, res)
	items := []interface{}{
		map[string]interface{}{"type": "message", "role": "user", "content": "搜下X"},
		map[string]interface{}{"type": "reasoning", "encrypted_content": enc},
		map[string]interface{}{"type": "message", "role": "user", "content": "追问"},
	}
	countSearchBlocks := func(msgs []map[string]interface{}) int {
		n := 0
		for _, m := range msgs {
			for _, c := range asArr(m["content"]) {
				if tp := objStr(asObj(c), "type"); tp == "server_tool_use" || tp == "web_search_tool_result" {
					n++
				}
			}
		}
		return n
	}

	// Match: two search blocks restored.
	msgs, err := convertInputToMessages(items, buildToolRegistry(nil), tri, nil)
	if err != nil {
		t.Fatalf("convertInputToMessages: %v", err)
	}
	if n := countSearchBlocks(msgs); n != 2 {
		b, _ := json.Marshal(msgs)
		t.Errorf("三元组匹配：搜索块数=%d, want 2: %s", n, b)
	}

	// Production shape (#3/#19 evidence): the envelope replays together with the web_search_call item — only
	// the envelope pair is restored; the call item (proxy-minted ws_ id) must not appear, otherwise going upstream 400s and the envelope pair is punished along.
	mixed := []interface{}{
		map[string]interface{}{"type": "message", "role": "user", "content": "搜下X"},
		map[string]interface{}{"type": "reasoning", "encrypted_content": enc},
		map[string]interface{}{"type": "web_search_call", "id": "ws_resp_msg_x_1", "status": "completed",
			"action": map[string]interface{}{"type": "search", "query": "X",
				"sources": []interface{}{map[string]interface{}{"type": "url", "url": "https://a.cn"}}}},
		map[string]interface{}{"type": "message", "role": "user", "content": "追问"},
	}
	msgs, err = convertInputToMessages(mixed, buildToolRegistry(nil), tri, nil)
	if err != nil {
		t.Fatalf("convertInputToMessages: %v", err)
	}
	if n := countSearchBlocks(msgs); n != 2 {
		b, _ := json.Marshal(msgs)
		t.Errorf("信封+调用项混排：搜索块数=%d, want 2（仅信封一对）: %s", n, b)
	}
	if b, _ := json.Marshal(msgs); strings.Contains(string(b), "ws_resp_msg_x_1") {
		t.Errorf("调用项的代理自造 id 不应上行: %s", b)
	}

	// No match (different url, same key): unmaskable but comparison fails, restoration skipped.
	other := newSearchTriple("https://u2/", "m1", "key1")
	msgs, err = convertInputToMessages(items, buildToolRegistry(nil), other, nil)
	if err != nil {
		t.Fatalf("convertInputToMessages: %v", err)
	}
	if n := countSearchBlocks(msgs); n != 0 {
		b, _ := json.Marshal(msgs)
		t.Errorf("url 不同源：搜索块数=%d, want 0（跳过省 token）: %s", n, b)
	}

	// Different key: unmasking fails, skipped outright.
	otherKey := newSearchTriple("https://u1/", "m1", "key2")
	msgs, err = convertInputToMessages(items, buildToolRegistry(nil), otherKey, nil)
	if err != nil {
		t.Fatalf("convertInputToMessages: %v", err)
	}
	if n := countSearchBlocks(msgs); n != 0 {
		b, _ := json.Marshal(msgs)
		t.Errorf("key 不同源：搜索块数=%d, want 0（解不开混淆）: %s", n, b)
	}

	// Different model but url+key same-origin: restored all the same (cross-model replay field-tested decrypting fine; the model leg doesn't participate).
	crossModel := newSearchTriple("https://u1/", "m2", "key1")
	msgs, err = convertInputToMessages(items, buildToolRegistry(nil), crossModel, nil)
	if err != nil {
		t.Fatalf("convertInputToMessages: %v", err)
	}
	if n := countSearchBlocks(msgs); n != 2 {
		b, _ := json.Marshal(msgs)
		t.Errorf("跨模型同源：搜索块数=%d, want 2（模型腿不参与比对）: %s", n, b)
	}

	// reqTriple nil: route key unpredictable, de-obfuscation impossible, skipped (v1's pass-through branch retired).
	msgs, err = convertInputToMessages(items, buildToolRegistry(nil), nil, nil)
	if err != nil {
		t.Fatalf("convertInputToMessages: %v", err)
	}
	if n := countSearchBlocks(msgs); n != 0 {
		b, _ := json.Marshal(msgs)
		t.Errorf("reqTriple nil：搜索块数=%d, want 0（解不开跳过）: %s", n, b)
	}
}

// TestAnthToRespStreamSearchEnvelope verifies streaming translation seals one search into
// an accompanying envelope reasoning item (its own output_index) when the triple is injected, and emits no envelope when not.
func TestAnthToRespStreamSearchEnvelope(t *testing.T) {
	tri := newSearchTriple("https://u/", "k3-256k", "key1")
	var events []string
	conv := newAnthToRespStream(func(ev string) { events = append(events, ev) }, "k3-256k", nil)
	conv.triple = tri
	feedSSEToConv(conv, testSSEAllBlocks())

	final := conv.buildFinalResponse()
	output := asArr(final["output"])
	// Without the envelope it's 5 (reasoning + 2×web_search_call + function_call + message); the envelope adds 1.
	if len(output) != 6 {
		t.Fatalf("output 数=%d, want 6（含搜索信封项）: %v", len(output), output)
	}
	var envItem map[string]interface{}
	for _, it := range output {
		o := asObj(it)
		if objStr(o, "type") == "reasoning" && strings.HasPrefix(objStr(o, "encrypted_content"), searchEnvelopePrefix) {
			envItem = o
		}
	}
	if envItem == nil {
		t.Fatalf("缺搜索信封 reasoning 项: %v", output)
	}
	decTri, blocks, _, ok := decodeSearchEnvelope(objStr(envItem, "encrypted_content"), tri.mask)
	if !ok || decTri.URL != tri.URL || decTri.Model != tri.Model || decTri.KeyH != tri.KeyH {
		t.Fatalf("信封解码失败: ok=%v tri=%v", ok, decTri)
	}
	if objStr(asObj(blocks[0]["input"]), "query") != "测试查询" {
		t.Errorf("信封里 server_tool_use.query=%v, want 测试查询", blocks[0]["input"])
	}
	// The envelope item's id starts with rs_ and carries the env marker, and both added+done were sent.
	joined := strings.Join(events, "")
	if !strings.Contains(joined, `"id":"rs_resp_msg_9_env`) {
		t.Errorf("事件流缺信封项 id（rs_..._envN）")
	}
	if strings.Count(joined, "event: response.output_item.added") != 6 {
		t.Errorf("added 事件数≠6（信封应占独立 output_index）")
	}

	// Control: no triple injected → no envelope (output stays 5).
	var events2 []string
	conv2 := newAnthToRespStream(func(ev string) { events2 = append(events2, ev) }, "k3-256k", nil)
	feedSSEToConv(conv2, testSSEAllBlocks())
	if n := len(asArr(conv2.buildFinalResponse()["output"])); n != 5 {
		t.Errorf("无三元组 output 数=%d, want 5（不出信封）", n)
	}
}

// testSSEStreamedQuerySearch builds a search stream with the query arriving via input_json_delta (production #56's observed
// shape): the server_tool_use block's start only gives id/name, the query arrives via input_json_delta;
// the result block is complete at start. The stu block's id and the result block's tool_use_id are naturally inconsistent (Kimi's native behavior).
func testSSEStreamedQuerySearch() string {
	return "" +
		`event: message_start` + "\n" +
		`data: {"type":"message_start","message":{"id":"msg_s1","type":"message","role":"assistant","model":"k3-256k","content":[],"stop_reason":null,"usage":{"input_tokens":10,"output_tokens":1}}}` + "\n\n" +
		`event: content_block_start` + "\n" +
		`data: {"type":"content_block_start","index":0,"content_block":{"type":"server_tool_use","id":"tool_abc","name":"web_search"}}` + "\n\n" +
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"{\"query\":\"测试查询\"}"}}` + "\n\n" +
		`event: content_block_stop` + "\n" +
		`data: {"type":"content_block_stop","index":0}` + "\n\n" +
		`event: content_block_start` + "\n" +
		`data: {"type":"content_block_start","index":1,"content_block":{"type":"web_search_tool_result","tool_use_id":"srvtoolu_xyz","content":[{"type":"web_search_result","title":"测试结果","url":"https://example.com/a","encrypted_content":"ENC_BLOB_123"}]}}` + "\n\n" +
		`event: content_block_stop` + "\n" +
		`data: {"type":"content_block_stop","index":1}` + "\n\n" +
		`event: message_delta` + "\n" +
		`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":5}}` + "\n\n" +
		`event: message_stop` + "\n" +
		`data: {"type":"message_stop"}` + "\n\n"
}

// TestAnthToRespStreamSearchStreamedQuery locks production #56's observed shape: a server_tool_use whose query arrives via input_json_delta
// must still produce an envelope and a query-carrying call item (before the fix: start without input → envelope skipped,
// call item lost — so the #57 follow-up had no blocks to restore).
func TestAnthToRespStreamSearchStreamedQuery(t *testing.T) {
	tri := newSearchTriple("https://u/", "k3-256k", "key1")
	var events []string
	conv := newAnthToRespStream(func(ev string) { events = append(events, ev) }, "k3-256k", nil)
	conv.triple = tri
	feedSSEToConv(conv, testSSEStreamedQuerySearch())

	final := conv.buildFinalResponse()
	output := asArr(final["output"])
	// Call item + sources item + envelope item = 3.
	if len(output) != 3 {
		t.Fatalf("output 数=%d, want 3: %v", len(output), output)
	}
	var callItem, envItem map[string]interface{}
	for _, it := range output {
		o := asObj(it)
		switch objStr(o, "type") {
		case "web_search_call":
			if objStr(asObj(o["action"]), "query") != "" {
				callItem = o
			}
		case "reasoning":
			if strings.HasPrefix(objStr(o, "encrypted_content"), searchEnvelopePrefix) {
				envItem = o
			}
		}
	}
	if callItem == nil {
		t.Fatalf("缺带 query 的 web_search_call 调用项: %v", output)
	}
	if objStr(asObj(callItem["action"]), "query") != "测试查询" {
		t.Errorf("调用项 query=%v, want 测试查询", asObj(callItem["action"])["query"])
	}
	if envItem == nil {
		t.Fatalf("缺搜索信封项: %v", output)
	}
	_, blocks, _, ok := decodeSearchEnvelope(objStr(envItem, "encrypted_content"), tri.mask)
	if !ok || objStr(asObj(blocks[0]["input"]), "query") != "测试查询" {
		t.Fatalf("信封里的 server_tool_use 缺 query: ok=%v blocks=%v", ok, blocks)
	}
	// id normalization: the streaming stu's native id (tool_abc) isn't recognized by the registry; inside the envelope it must be replaced with the result block's id.
	if objStr(blocks[0], "id") != "srvtoolu_xyz" {
		t.Errorf("信封 stu.id=%v, want srvtoolu_xyz（结果块的注册 id）", blocks[0]["id"])
	}
	// Search blocks' argument fragments must not go through function_call_arguments events (web_search_call has no argument stream).
	joined := strings.Join(events, "")
	if strings.Contains(joined, "function_call_arguments") {
		t.Errorf("搜索块不应发 function_call_arguments 事件")
	}
}

// TestStripSearchBlocksInBody verifies fail-soft stripping: search blocks stripped bare, shell messages deleted wholesale,
// adjacent same-role messages merged; ok=false with no search blocks / invalid JSON.
func TestStripSearchBlocksInBody(t *testing.T) {
	body := []byte(`{"model":"m","messages":[` +
		`{"role":"user","content":[{"type":"text","text":"搜下X"}]},` +
		`{"role":"assistant","content":[` +
		`{"type":"server_tool_use","id":"srvtoolu_1","name":"web_search","input":{"query":"X"}},` +
		`{"type":"web_search_tool_result","tool_use_id":"srvtoolu_1","content":[{"type":"web_search_result","url":"https://a.cn"}]}]},` +
		`{"role":"user","content":[{"type":"text","text":"追问"}]}]}`)
	nb, n, ok := stripSearchBlocksInBody(body)
	if !ok {
		t.Fatalf("ok=false, want true")
	}
	if n != 2 {
		t.Errorf("剥块数=%d, want 2（stu+result 各算 1）", n)
	}
	var m map[string]interface{}
	if err := json.Unmarshal(nb, &m); err != nil {
		t.Fatalf("剥后非法 JSON: %v", err)
	}
	msgs := asArr(m["messages"])
	// assistant shell deleted wholesale → user user adjacent → merged into 1 user message (2 text blocks).
	if len(msgs) != 1 {
		b, _ := json.Marshal(msgs)
		t.Fatalf("消息数=%d, want 1（合并后）: %s", len(msgs), b)
	}
	if objStr(asObj(msgs[0]), "role") != "user" || len(asArr(asObj(msgs[0])["content"])) != 2 {
		b, _ := json.Marshal(msgs)
		t.Errorf("合并形态错误: %s", b)
	}
	if strings.Contains(string(nb), "server_tool_use") || strings.Contains(string(nb), "web_search_tool_result") {
		t.Errorf("剥后仍含搜索块: %s", nb)
	}

	// An assistant message mixed with body text: text blocks kept after stripping, message not deleted.
	body2 := []byte(`{"messages":[` +
		`{"role":"user","content":"搜下X"},` +
		`{"role":"assistant","content":[` +
		`{"type":"text","text":"搜到这些"},` +
		`{"type":"server_tool_use","id":"srvtoolu_2","name":"web_search","input":{"query":"Y"}}]},` +
		`{"role":"user","content":"追问"}]}`)
	nb2, n2, ok := stripSearchBlocksInBody(body2)
	if !ok {
		t.Fatalf("body2 ok=false, want true")
	}
	if n2 != 1 {
		t.Errorf("body2 剥块数=%d, want 1", n2)
	}
	var m2 map[string]interface{}
	_ = json.Unmarshal(nb2, &m2)
	msgs2 := asArr(m2["messages"])
	if len(msgs2) != 3 {
		b, _ := json.Marshal(msgs2)
		t.Fatalf("body2 消息数=%d, want 3（assistant 留 text）: %s", len(msgs2), b)
	}
	if n := len(asArr(asObj(msgs2[1])["content"])); n != 1 {
		t.Errorf("body2 assistant 块数=%d, want 1（只剩 text）", n)
	}

	// No search blocks → ok=false (no retry needed).
	if _, _, ok := stripSearchBlocksInBody([]byte(`{"messages":[{"role":"user","content":"hi"}]}`)); ok {
		t.Errorf("无搜索块应 ok=false")
	}
	// Invalid JSON → ok=false.
	if _, _, ok := stripSearchBlocksInBody([]byte(`{bad`)); ok {
		t.Errorf("非法 JSON 应 ok=false")
	}
}

// TestSearchEnvelopeFailSoftRetry end-to-end fail-soft: a request carrying a replayed search block, the upstream first
// 400s with tool_call_id (registry doesn't recognize the old id) → the proxy strips blocks and retries immediately → the client gets 200,
// and the second upstream request body contains no search blocks.
func TestSearchEnvelopeFailSoftRetry(t *testing.T) {
	resetStats()
	var rawBodies []string
	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		rawBodies = append(rawBodies, string(b))
		if len(rawBodies) == 1 {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(400)
			fmt.Fprint(w, `{"type":"error","error":{"type":"invalid_request_error","message":"tool_call_id srvtoolu_1 is not found"}}`)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		fmt.Fprint(w, testSSEAllBlocks())
	}))
	defer mock.Close()

	cfg.Store(&Config{Upstream: mock.URL, MaxRetries: 0, TotalBudgetSec: 10})

	proxy := httptest.NewServer(http.HandlerFunc(handler))
	defer proxy.Close()

	reqBody := `{"model":"k3-256k","stream":true,"messages":[` +
		`{"role":"user","content":[{"type":"text","text":"搜下X"}]},` +
		`{"role":"assistant","content":[` +
		`{"type":"server_tool_use","id":"srvtoolu_1","name":"web_search","input":{"query":"X"}},` +
		`{"type":"web_search_tool_result","tool_use_id":"srvtoolu_1","content":[{"type":"web_search_result","title":"t","url":"https://a.cn","encrypted_content":"ENC"}]}]},` +
		`{"role":"user","content":[{"type":"text","text":"追问"}]}]}`
	resp, err := http.Post(proxy.URL+"/v1/messages", "application/json", strings.NewReader(reqBody))
	if err != nil {
		t.Fatalf("请求失败: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()

	// The 400 isn't passed to the client: the fail-soft retry succeeds with 200.
	if resp.StatusCode != 200 {
		t.Fatalf("客户端状态码=%d, want 200（fail-soft 应兜底）: %s", resp.StatusCode, body)
	}
	// The upstream receives exactly 2 requests: the first with search blocks, the retry stripped bare.
	if len(rawBodies) != 2 {
		t.Fatalf("上游请求数=%d, want 2", len(rawBodies))
	}
	if !strings.Contains(rawBodies[0], "server_tool_use") {
		t.Errorf("首次请求应带搜索块: %s", rawBodies[0])
	}
	if strings.Contains(rawBodies[1], "server_tool_use") || strings.Contains(rawBodies[1], "web_search_tool_result") {
		t.Errorf("重试请求应剥光搜索块: %s", rawBodies[1])
	}
	if !strings.Contains(rawBodies[1], "搜下X") || !strings.Contains(rawBodies[1], "追问") {
		t.Errorf("重试请求丢了正文: %s", rawBodies[1])
	}

	// [剥N] counting: the finished archive should record 2 400-fallback strips (stu+result each count 1).
	finishedMu.Lock()
	ff := finished[len(finished)-1]
	finishedMu.Unlock()
	if ff.searchStripped != 2 {
		t.Errorf("完成流剥块计数=%d, want 2（400 兜底剥 stu+result）", ff.searchStripped)
	}
}

// encodeSearchEnvelopeWithTs builds an envelope with a given sealing moment (for testing time-rule stripping; ts=0 omits
// the field, simulating old v2-initial envelopes without ts).
func encodeSearchEnvelopeWithTs(t_ *searchTriple, useBlk, resBlk map[string]interface{}, ts int64) string {
	p := map[string]interface{}{"t": t_, "b": []map[string]interface{}{useBlk, resBlk}}
	if ts > 0 {
		p["ts"] = ts
	}
	payload, _ := json.Marshal(p)
	return searchEnvelopePrefix + base64.RawURLEncoding.EncodeToString(xorSearchMask(t_.mask, payload))
}

// resetSearchCutoffForTest clears the global conversation-watermark table (preventing cross-test interference), returning a restore function.
func resetSearchCutoffForTest() func() {
	searchCutoff.Lock()
	searchCutoff.m = make(map[string]time.Time)
	searchCutoff.Unlock()
	return func() {
		searchCutoff.Lock()
		searchCutoff.m = make(map[string]time.Time)
		searchCutoff.Unlock()
	}
}

// TestConvertInputSearchEnvelopeWaterLevel verifies proactive watermark stripping: no fixed age ceiling
// (old envelopes restore optimistically even without a watermark — direct-send probes proved a 1.7h-old sealed search id still alive); once this conversation
// has learned a watermark, envelopes no newer than it (exactly equal included) are watermark-stripped; ts-less old envelopes count as oldest —
// stripped when a watermark exists, optimistically restored without one. Proactive stripping doesn't restore and doesn't hit 400s; counts go into replay for [剥N] and logs.
func TestConvertInputSearchEnvelopeWaterLevel(t *testing.T) {
	defer resetSearchCutoffForTest()()
	use, res := testSearchBlockPair()
	tri := newSearchTriple("https://u1/", "m1", "key1")
	countSearchBlocks := func(msgs []map[string]interface{}) int {
		n := 0
		for _, m := range msgs {
			for _, c := range asArr(m["content"]) {
				if tp := objStr(asObj(c), "type"); tp == "server_tool_use" || tp == "web_search_tool_result" {
					n++
				}
			}
		}
		return n
	}
	itemsWith := func(enc string) []interface{} {
		return []interface{}{
			map[string]interface{}{"type": "message", "role": "user", "content": "搜下X"},
			map[string]interface{}{"type": "reasoning", "encrypted_content": enc},
			map[string]interface{}{"type": "message", "role": "user", "content": "追问"},
		}
	}

	// Age doesn't strip: sealed 2 hours ago but this conversation has no watermark → restored as usual (the 1h hard rule was removed).
	replay := &searchReplayCtx{convID: "c-age"}
	msgs, err := convertInputToMessages(itemsWith(encodeSearchEnvelopeWithTs(tri, use, res, time.Now().Add(-2*time.Hour).Unix())), buildToolRegistry(nil), tri, replay)
	if err != nil {
		t.Fatalf("convertInputToMessages: %v", err)
	}
	if n := countSearchBlocks(msgs); n != 2 || len(replay.restored) != 1 {
		t.Errorf("无水位的老信封：还原块数=%d(want 2) 还原时刻数=%d(want 1)", n, len(replay.restored))
	}
	// ts-less old envelopes without a watermark also restore optimistically (no ts to collect; restore moments stay 0).
	replay = &searchReplayCtx{convID: "c-age"}
	msgs, _ = convertInputToMessages(itemsWith(encodeSearchEnvelopeWithTs(tri, use, res, 0)), buildToolRegistry(nil), tri, replay)
	if n := countSearchBlocks(msgs); n != 2 || len(replay.restored) != 0 {
		t.Errorf("无 ts 信封（无水位）：还原块数=%d(want 2) 还原时刻数=%d(want 0)", n, len(replay.restored))
	}

	// Watermark: the conversation watermark is set 10 minutes ago (second precision, same semantics as envelope ts decoding) — envelopes from 20 minutes
	// ago are watermark-stripped, one exactly equal to the watermark is stripped too (the registry expires by age; same-age ones are certainly dead), ts-less ones
	// count as oldest and are stripped along, 5-minutes-ago restores as usual.
	cut := time.Unix(time.Now().Add(-10*time.Minute).Unix(), 0)
	learnSearchCutoff("c-water", cut)
	replay = &searchReplayCtx{convID: "c-water"}
	msgs, _ = convertInputToMessages(itemsWith(encodeSearchEnvelopeWithTs(tri, use, res, time.Now().Add(-20*time.Minute).Unix())), buildToolRegistry(nil), tri, replay)
	if n := countSearchBlocks(msgs); n != 0 || replay.proactiveCutoff != 2 {
		t.Errorf("老于水位的信封：还原块数=%d(want 0) 水位剥=%d(want 2)", n, replay.proactiveCutoff)
	}
	replay = &searchReplayCtx{convID: "c-water"}
	msgs, _ = convertInputToMessages(itemsWith(encodeSearchEnvelopeWithTs(tri, use, res, cut.Unix())), buildToolRegistry(nil), tri, replay)
	if n := countSearchBlocks(msgs); n != 0 || replay.proactiveCutoff != 2 {
		t.Errorf("恰等于水位的信封：还原块数=%d(want 0) 水位剥=%d(want 2)", n, replay.proactiveCutoff)
	}
	replay = &searchReplayCtx{convID: "c-water"}
	msgs, _ = convertInputToMessages(itemsWith(encodeSearchEnvelopeWithTs(tri, use, res, 0)), buildToolRegistry(nil), tri, replay)
	if n := countSearchBlocks(msgs); n != 0 || replay.proactiveCutoff != 2 {
		t.Errorf("无 ts 信封（有水位）：还原块数=%d(want 0) 水位剥=%d(want 2)", n, replay.proactiveCutoff)
	}
	replay = &searchReplayCtx{convID: "c-water"}
	msgs, _ = convertInputToMessages(itemsWith(encodeSearchEnvelopeWithTs(tri, use, res, time.Now().Add(-5*time.Minute).Unix())), buildToolRegistry(nil), tri, replay)
	if n := countSearchBlocks(msgs); n != 2 || len(replay.restored) != 1 {
		t.Errorf("新于水位的信封：还原块数=%d(want 2) 还原时刻数=%d(want 1)", n, len(replay.restored))
	}
}

// TestSearchEnvelopeCutoffLearning end-to-end: the Responses port replays an envelope → upstream 400
// tool_call_id → fallback stripping retries and learns the conversation watermark; same-conversation envelopes older than or exactly at the watermark get
// watermark-stripped directly next turn (upstream 200 on first shot, no search blocks, no 400), envelopes newer than the watermark restore as usual.
// [剥N] counting includes both watermark strips and 400-fallback strips.
func TestSearchEnvelopeCutoffLearning(t *testing.T) {
	defer resetSearchCutoffForTest()()
	resetStats()
	var rawBodies []string
	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		rawBodies = append(rawBodies, string(b))
		if len(rawBodies) == 1 {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(400)
			fmt.Fprint(w, `{"type":"error","error":{"type":"invalid_request_error","message":"tool_call_id srvtoolu_1 is not found"}}`)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		fmt.Fprint(w, testSSEAllBlocks())
	}))
	defer mock.Close()

	cfg.Store(&Config{Upstream: mock.URL, MaxRetries: 0, TotalBudgetSec: 10})
	proxy := httptest.NewServer(http.HandlerFunc(responsesHandler))
	defer proxy.Close()

	tri := newSearchTriple(mock.URL, "k3-256k", "sk-test")
	use, res := testSearchBlockPair()
	doReq := func(enc string) {
		body := `{"model":"k3-256k","prompt_cache_key":"conv-1","input":[` +
			`{"type":"message","role":"user","content":[{"type":"input_text","text":"搜下X"}]},` +
			`{"type":"reasoning","encrypted_content":"` + enc + `"},` +
			`{"type":"message","role":"user","content":[{"type":"input_text","text":"追问"}]}]}`
		req, _ := http.NewRequest("POST", proxy.URL+"/v1/responses", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "sk-test")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("请求失败: %v", err)
		}
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != 200 {
			t.Fatalf("客户端状态码=%d, want 200: %s", resp.StatusCode, b)
		}
	}
	lastStripped := func() int {
		finishedMu.Lock()
		defer finishedMu.Unlock()
		return finished[len(finished)-1].searchStripped
	}

	// First round: a new envelope restores upstream → upstream 400 → fallback strips 2 blocks, retry 200, watermark set to the envelope's sealing moment.
	t0 := time.Now().Unix()
	doReq(encodeSearchEnvelopeWithTs(tri, use, res, t0))
	if len(rawBodies) != 2 {
		t.Fatalf("第一轮上游请求数=%d, want 2（400+兜底重试）", len(rawBodies))
	}
	if !strings.Contains(rawBodies[0], "server_tool_use") || strings.Contains(rawBodies[1], "server_tool_use") {
		t.Errorf("第一轮：首发应带搜索块、重试应剥光: %s / %s", rawBodies[0], rawBodies[1])
	}
	if n := lastStripped(); n != 2 {
		t.Errorf("第一轮剥块计数=%d, want 2（400 兜底）", n)
	}
	if w := searchCutoffFor("conv-1"); w.Unix() != t0 {
		t.Errorf("对话水位=%v, want 封入时刻 %v", w, time.Unix(t0, 0))
	}

	// Second round: an older envelope from the same conversation → watermark-stripped directly; upstream 200 on first shot with no search blocks (zero 400s).
	doReq(encodeSearchEnvelopeWithTs(tri, use, res, t0-60))
	if len(rawBodies) != 3 {
		t.Fatalf("第二轮后上游请求数=%d, want 3（水位剥不撞 400）", len(rawBodies))
	}
	if strings.Contains(rawBodies[2], "server_tool_use") || strings.Contains(rawBodies[2], "web_search_tool_result") {
		t.Errorf("第二轮：水位剥后不应带搜索块上行: %s", rawBodies[2])
	}
	if n := lastStripped(); n != 2 {
		t.Errorf("第二轮剥块计数=%d, want 2（水位剥）", n)
	}

	// Equal-to-watermark round: an envelope sealed exactly at the watermark → watermark-stripped the same (same-age ones are certainly dead; no 400 hit).
	doReq(encodeSearchEnvelopeWithTs(tri, use, res, t0))
	if len(rawBodies) != 4 {
		t.Fatalf("等于水位轮后上游请求数=%d, want 4（水位剥不撞 400）", len(rawBodies))
	}
	if strings.Contains(rawBodies[3], "server_tool_use") || strings.Contains(rawBodies[3], "web_search_tool_result") {
		t.Errorf("等于水位轮：水位剥后不应带搜索块上行: %s", rawBodies[3])
	}
	if n := lastStripped(); n != 2 {
		t.Errorf("等于水位轮剥块计数=%d, want 2（水位剥）", n)
	}

	// Third round: an envelope newer than the watermark → restored as usual (search blocks go upstream; the mock already returns 200, no more stripping).
	doReq(encodeSearchEnvelopeWithTs(tri, use, res, t0+60))
	if len(rawBodies) != 5 {
		t.Fatalf("第三轮后上游请求数=%d, want 5", len(rawBodies))
	}
	if !strings.Contains(rawBodies[4], "server_tool_use") {
		t.Errorf("第三轮：新于水位的信封应还原上行: %s", rawBodies[4])
	}
	if n := lastStripped(); n != 0 {
		t.Errorf("第三轮剥块计数=%d, want 0（还原被上游收下）", n)
	}
}
