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

// testSearchBlockPair 造一对真搜索块（server_tool_use 带 query + 结果块带 encrypted_content）。
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

// TestSearchEnvelopeCodec 验证搜索信封编解码 round-trip、拒绝异形、payload 混淆
// （key/url 不裸存、错 key 解不出）。
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
	// 封入时刻随 payload 往返（时间规则剥块的依据）；无 ts 字段的老信封解出零值。
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

	// id 归一：Kimi 流式搜索的 stu.id 是 tool_ 开头、注册表只认结果块的 srvtoolu_
	// id（2026-09-10 受控实验：原样回放 400，改写 stu.id 后 200）。封入时归一，
	// 且不得改调用方共享的块。
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

	// payload 混淆：base64 解开是异或密文——key、url、key 哈希都不许裸躺
	// （用户要求：客户端历史里不放明文归属信息）；异或解混淆后哈希可见。
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
	// 错 key：掩码不同，异或出来不是 JSON，自然解不出（key 腿比对含在解混淆里）。
	if _, _, _, ok := decodeSearchEnvelope(enc, searchEnvMask("sk-wrong")); ok {
		t.Errorf("错 key 不应解出信封")
	}
	if _, _, _, ok := decodeSearchEnvelope(enc, nil); ok {
		t.Errorf("无掩码不应解出信封")
	}

	// 不编码的形态：nil 三元组/块、空 query（空搜索）、无掩码三元组。
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

	// 拒绝的形态：非本前缀、坏 base64、payload 形态不对。
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

// TestConvertInputSearchEnvelopeRestore 验证 input 里的搜索信封还原分支：
// url+key 同源 → 还原上行；url 不同源（key 同，解得开但比对不过）→ 跳过；
// key 不同源（掩码解不开）→ 跳过；reqTriple nil（预测不了路由 key）→ 跳过
// （v1 的放行分支随明文 payload 退役）。
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

	// 匹配：还原出两个搜索块。
	msgs, err := convertInputToMessages(items, buildToolRegistry(nil), tri, nil)
	if err != nil {
		t.Fatalf("convertInputToMessages: %v", err)
	}
	if n := countSearchBlocks(msgs); n != 2 {
		b, _ := json.Marshal(msgs)
		t.Errorf("三元组匹配：搜索块数=%d, want 2: %s", n, b)
	}

	// 生产形态（#3/#19 实证）：信封与 web_search_call 调用项一起回放——只还原
	// 信封一对，调用项（代理自造 ws_ id）不出现，否则上行必 400 连坐信封对被剥。
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

	// 不匹配（url 不同、key 同）：解得开但比对不过，跳过还原。
	other := newSearchTriple("https://u2/", "m1", "key1")
	msgs, err = convertInputToMessages(items, buildToolRegistry(nil), other, nil)
	if err != nil {
		t.Fatalf("convertInputToMessages: %v", err)
	}
	if n := countSearchBlocks(msgs); n != 0 {
		b, _ := json.Marshal(msgs)
		t.Errorf("url 不同源：搜索块数=%d, want 0（跳过省 token）: %s", n, b)
	}

	// key 不同源：掩码解不开，直接跳过。
	otherKey := newSearchTriple("https://u1/", "m1", "key2")
	msgs, err = convertInputToMessages(items, buildToolRegistry(nil), otherKey, nil)
	if err != nil {
		t.Fatalf("convertInputToMessages: %v", err)
	}
	if n := countSearchBlocks(msgs); n != 0 {
		b, _ := json.Marshal(msgs)
		t.Errorf("key 不同源：搜索块数=%d, want 0（解不开混淆）: %s", n, b)
	}

	// 模型不同但 url+key 同源：照样还原（实测跨模型回放正常解密，模型腿不参与）。
	crossModel := newSearchTriple("https://u1/", "m2", "key1")
	msgs, err = convertInputToMessages(items, buildToolRegistry(nil), crossModel, nil)
	if err != nil {
		t.Fatalf("convertInputToMessages: %v", err)
	}
	if n := countSearchBlocks(msgs); n != 2 {
		b, _ := json.Marshal(msgs)
		t.Errorf("跨模型同源：搜索块数=%d, want 2（模型腿不参与比对）: %s", n, b)
	}

	// reqTriple nil：预测不了路由 key，解不开混淆，跳过（v1 放行分支已退役）。
	msgs, err = convertInputToMessages(items, buildToolRegistry(nil), nil, nil)
	if err != nil {
		t.Fatalf("convertInputToMessages: %v", err)
	}
	if n := countSearchBlocks(msgs); n != 0 {
		b, _ := json.Marshal(msgs)
		t.Errorf("reqTriple nil：搜索块数=%d, want 0（解不开跳过）: %s", n, b)
	}
}

// TestAnthToRespStreamSearchEnvelope 验证流式翻译在三元组已注入时把一次搜索封成
// 信封 reasoning 项随行（独立 output_index），未注入时不出信封。
func TestAnthToRespStreamSearchEnvelope(t *testing.T) {
	tri := newSearchTriple("https://u/", "k3-256k", "key1")
	var events []string
	conv := newAnthToRespStream(func(ev string) { events = append(events, ev) }, "k3-256k", nil)
	conv.triple = tri
	feedSSEToConv(conv, testSSEAllBlocks())

	final := conv.buildFinalResponse()
	output := asArr(final["output"])
	// 无信封时是 5（reasoning + 2×web_search_call + function_call + message），信封 +1。
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
	// 信封项 id 以 rs_ 开头、含 env 标记，且 added+done 都发了。
	joined := strings.Join(events, "")
	if !strings.Contains(joined, `"id":"rs_resp_msg_9_env`) {
		t.Errorf("事件流缺信封项 id（rs_..._envN）")
	}
	if strings.Count(joined, "event: response.output_item.added") != 6 {
		t.Errorf("added 事件数≠6（信封应占独立 output_index）")
	}

	// 对照：不注入三元组 → 不出信封（output 仍是 5）。
	var events2 []string
	conv2 := newAnthToRespStream(func(ev string) { events2 = append(events2, ev) }, "k3-256k", nil)
	feedSSEToConv(conv2, testSSEAllBlocks())
	if n := len(asArr(conv2.buildFinalResponse()["output"])); n != 5 {
		t.Errorf("无三元组 output 数=%d, want 5（不出信封）", n)
	}
}

// testSSEStreamedQuerySearch 构造 query 走 input_json_delta 的搜索流（生产 #56 实测
// 形态）：server_tool_use 块 start 只给 id/name，query 由 input_json_delta 送达；
// 结果块 start 完整。stu 块 id 与结果块 tool_use_id 天生不一致（Kimi 原生行为）。
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

// TestAnthToRespStreamSearchStreamedQuery 锁生产 #56 实测形态：query 走 input_json_delta
// 的 server_tool_use 也要出信封、出带 query 的调用项（修复前 start 无 input → 信封跳过、
// 调用项丢失，#57 追问因此无块可还原）。
func TestAnthToRespStreamSearchStreamedQuery(t *testing.T) {
	tri := newSearchTriple("https://u/", "k3-256k", "key1")
	var events []string
	conv := newAnthToRespStream(func(ev string) { events = append(events, ev) }, "k3-256k", nil)
	conv.triple = tri
	feedSSEToConv(conv, testSSEStreamedQuerySearch())

	final := conv.buildFinalResponse()
	output := asArr(final["output"])
	// 调用项 + 来源项 + 信封项 = 3。
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
	// id 归一：流式 stu 原生 id（tool_abc）注册表不认，信封里必须换成结果块的 id。
	if objStr(blocks[0], "id") != "srvtoolu_xyz" {
		t.Errorf("信封 stu.id=%v, want srvtoolu_xyz（结果块的注册 id）", blocks[0]["id"])
	}
	// 搜索块的参数碎片不得走 function_call_arguments 事件（web_search_call 无参数流）。
	joined := strings.Join(events, "")
	if strings.Contains(joined, "function_call_arguments") {
		t.Errorf("搜索块不应发 function_call_arguments 事件")
	}
}

// TestStripSearchBlocksInBody 验证 fail-soft 剥块：搜索块剥光、空壳消息整条删、
// 相邻同 role 合并；无搜索块/非法 JSON 时 ok=false。
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
	// assistant 空壳整条删 → user user 相邻 → 合并成 1 条 user 消息（2 个 text 块）。
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

	// assistant 消息里混有正文：剥后保留 text 块，消息不删。
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

	// 无搜索块 → ok=false（无需重试）。
	if _, _, ok := stripSearchBlocksInBody([]byte(`{"messages":[{"role":"user","content":"hi"}]}`)); ok {
		t.Errorf("无搜索块应 ok=false")
	}
	// 非法 JSON → ok=false。
	if _, _, ok := stripSearchBlocksInBody([]byte(`{bad`)); ok {
		t.Errorf("非法 JSON 应 ok=false")
	}
}

// TestSearchEnvelopeFailSoftRetry 端到端 fail-soft：请求带回放的搜索块，上游首次
// 400 tool_call_id（注册表不认旧 id）→ 代理剥块立即重试 → 客户端拿到 200，
// 第二次上游请求体里没有任何搜索块。
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

	// 400 不透给客户端：fail-soft 重试成功拿到 200。
	if resp.StatusCode != 200 {
		t.Fatalf("客户端状态码=%d, want 200（fail-soft 应兜底）: %s", resp.StatusCode, body)
	}
	// 上游恰好收 2 次：首次带搜索块，重试剥光。
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

	// [剥N] 计数：完成流归档应记 2 个 400 兜底剥块（stu+result 各算 1）。
	finishedMu.Lock()
	ff := finished[len(finished)-1]
	finishedMu.Unlock()
	if ff.searchStripped != 2 {
		t.Errorf("完成流剥块计数=%d, want 2（400 兜底剥 stu+result）", ff.searchStripped)
	}
}

// encodeSearchEnvelopeWithTs 造指定封入时刻的信封（测试时间规则剥块用；ts=0 省略
// 字段，模拟 v2 初版无 ts 的老信封）。
func encodeSearchEnvelopeWithTs(t_ *searchTriple, useBlk, resBlk map[string]interface{}, ts int64) string {
	p := map[string]interface{}{"t": t_, "b": []map[string]interface{}{useBlk, resBlk}}
	if ts > 0 {
		p["ts"] = ts
	}
	payload, _ := json.Marshal(p)
	return searchEnvelopePrefix + base64.RawURLEncoding.EncodeToString(xorSearchMask(t_.mask, payload))
}

// resetSearchCutoffForTest 清对话水位全局表（防测试间串扰），返回恢复函数。
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

// TestConvertInputSearchEnvelopeWaterLevel 验证水位主动剥：不设固定年龄上限
// （老信封无水位也乐观还原——直发探测实证封入 1.7h 的搜索 id 仍存活）；本对话
// 学到水位后，不比水位新的信封（含恰等于水位）水位剥；无 ts 老信封视同最老——
// 有水位剥、无水位乐观还原。主动剥不还原、不撞 400，计数进 replay 供 [剥N] 与日志。
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

	// 年龄不剥：封入 2 小时前但本对话无水位 → 照常还原（1h 硬规则已撤）。
	replay := &searchReplayCtx{convID: "c-age"}
	msgs, err := convertInputToMessages(itemsWith(encodeSearchEnvelopeWithTs(tri, use, res, time.Now().Add(-2*time.Hour).Unix())), buildToolRegistry(nil), tri, replay)
	if err != nil {
		t.Fatalf("convertInputToMessages: %v", err)
	}
	if n := countSearchBlocks(msgs); n != 2 || len(replay.restored) != 1 {
		t.Errorf("无水位的老信封：还原块数=%d(want 2) 还原时刻数=%d(want 1)", n, len(replay.restored))
	}
	// 无 ts 老信封无水位时同样乐观还原（无 ts 可收，还原时刻保持 0）。
	replay = &searchReplayCtx{convID: "c-age"}
	msgs, _ = convertInputToMessages(itemsWith(encodeSearchEnvelopeWithTs(tri, use, res, 0)), buildToolRegistry(nil), tri, replay)
	if n := countSearchBlocks(msgs); n != 2 || len(replay.restored) != 0 {
		t.Errorf("无 ts 信封（无水位）：还原块数=%d(want 2) 还原时刻数=%d(want 0)", n, len(replay.restored))
	}

	// 水位：对话水位记在 10 分钟前（秒级精度，与信封 ts 解码同口径）——20 分钟
	// 前的信封水位剥，恰等于水位的也剥（注册表按龄淘汰，同龄必死），无 ts 的
	// 视同最老一并剥，5 分钟前的照常还原。
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

// TestSearchEnvelopeCutoffLearning 端到端：Responses 口带回放信封 → 上游 400
// tool_call_id → 兜底剥块重试并学到对话水位；同对话更老与恰等于水位的信封下一轮
// 被水位直接剥（上游一发即 200、无搜索块、无 400），比水位新的信封照常还原。
// [剥N] 计数含水位剥与 400 兜底剥两部分。
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

	// 第一轮：新信封还原上行 → 上游 400 → 兜底剥 2 块重试 200，水位记为信封封入时刻。
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

	// 第二轮：同对话更老的信封 → 水位直接剥，上游一发即 200 且无搜索块（零 400）。
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

	// 等于水位轮：封入时刻恰等于水位的信封 → 同样水位剥（同龄必死，不撞 400）。
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

	// 第三轮：比水位新的信封 → 照常还原（上行带搜索块，mock 已回 200，不再剥）。
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
