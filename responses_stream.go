package main

// responses_stream.go — Anthropic Messages SSE → OpenAI Responses SSE 实时翻译。
// 对照 cc-switch streaming_codex_anthropic.rs 的状态机与 codex_responses_sse.rs 的事件形状。
//
// 翻译型 ResponseWriter 夹在新监听口 handler 与主 handler 之间：主管线往里写
// Anthropic SSE（或异常时的 JSON/错误体），这里逐块解析、按 Responses 事件生命周期
// 实时转写（客户端 stream:true）或收集后组装一次性 JSON（stream:false）。

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strings"
)

// ---- Responses SSE 事件构造（形状对照 cc-switch codex_responses_sse.rs）----

func respSSEEvent(event string, data map[string]interface{}) string {
	b, _ := json.Marshal(data)
	return "event: " + event + "\ndata: " + string(b) + "\n\n"
}

func respOutputItemAdded(outputIndex int, item map[string]interface{}) string {
	return respSSEEvent("response.output_item.added", map[string]interface{}{
		"type": "response.output_item.added", "output_index": outputIndex, "item": item,
	})
}

func respOutputItemDone(outputIndex int, item map[string]interface{}) string {
	return respSSEEvent("response.output_item.done", map[string]interface{}{
		"type": "response.output_item.done", "output_index": outputIndex, "item": item,
	})
}

func respMessageItem(itemID, status, text string) map[string]interface{} {
	content := []interface{}{}
	if status == "completed" {
		content = []interface{}{map[string]interface{}{
			"type": "output_text", "text": text, "annotations": []interface{}{},
		}}
	}
	return map[string]interface{}{
		"id": itemID, "type": "message", "status": status, "role": "assistant", "content": content,
	}
}

func respReasoningItem(itemID, text, encrypted string) map[string]interface{} {
	summary := []interface{}{}
	if text != "" {
		summary = []interface{}{map[string]interface{}{"type": "summary_text", "text": text}}
	}
	item := map[string]interface{}{"id": itemID, "type": "reasoning", "summary": summary}
	if encrypted != "" {
		item["encrypted_content"] = encrypted
	}
	return item
}

// ---- 状态机 ----

// blockKind 内容块类别。
type blockKind int

const (
	bkText blockKind = iota
	bkThinking
	bkRedactedThinking
	bkToolUse
	bkSearchUse   // server_tool_use：query 可能走 input_json_delta（实测 Kimi 常只给 id/name），stop 补全后才发项
	bkInstantDone // web_search_tool_result 及未知块：start 时块已完整，add+done 已发
)

type blockState struct {
	kind        blockKind
	outputIndex int
	itemID      string
	callID      string
	name        string
	accum       string                 // text/thinking/input_json 累积
	signature   string                 // signature_delta 累积
	startInput  string                 // content_block_start 自带的 tool_use input（无 delta 时兜底）
	searchBlk   map[string]interface{} // server_tool_use 块原文（stop 补全 input 后发项+配对信封）
	heldText    bool                   // text 块待判定是否 Kimi 空搜索前言：分流前 delta 憋着不发
}

// anthToRespStream 把 Anthropic SSE 事件流翻译成 Responses SSE 事件流。
// emit 为 nil 时（客户端 stream:false）只维护内部状态，收完用 buildFinalResponse 组装 JSON。
// reg 是请求侧的工具注册表：tool_use 块据此还原 custom/namespace/tool_search 身份。
type anthToRespStream struct {
	emit            func(string)
	model           string
	reg             *toolRegistry
	responseID      string
	responseStarted bool
	completed       bool
	nextOutputIndex int
	blocks          map[int]*blockState
	items           []map[string]interface{} // 已完成的 output 项（按完成顺序）
	usage           map[string]interface{}
	stopReason      string
	triple          *searchTriple          // 搜索信封归属三元组（路由定案后由 translatingWriter 注入；nil = 不出信封）
	lastSearchUse   map[string]interface{} // 最近一个 server_tool_use 块（结果块到达时配对封信封）
}

func newAnthToRespStream(emit func(string), model string, reg *toolRegistry) *anthToRespStream {
	return &anthToRespStream{
		emit:       emit,
		model:      model,
		reg:        reg,
		responseID: "resp_p429",
		blocks:     map[int]*blockState{},
		usage:      map[string]interface{}{},
	}
}

func (s *anthToRespStream) send(ev string) {
	if s.emit != nil {
		s.emit(ev)
	}
}

// responseSkeleton 构造 response.created/in_progress 携带的 response 骨架。
func (s *anthToRespStream) responseSkeleton(status string) map[string]interface{} {
	return map[string]interface{}{
		"id": s.responseID, "object": "response", "created_at": 0,
		"status": status, "model": s.model, "output": []interface{}{},
	}
}

// handleEvent 处理一个 Anthropic SSE 事件块（event 名 + data JSON）。
func (s *anthToRespStream) handleEvent(event string, data map[string]interface{}) {
	typ := objStr(data, "type")
	if typ == "" {
		typ = event
	}
	switch typ {
	case "message_start":
		s.handleMessageStart(asObj(data["message"]))
	case "content_block_start":
		s.handleBlockStart(int(toInt64(data["index"])), asObj(data["content_block"]))
	case "content_block_delta":
		s.handleBlockDelta(int(toInt64(data["index"])), asObj(data["delta"]))
	case "content_block_stop":
		s.handleBlockStop(int(toInt64(data["index"])))
	case "message_delta":
		if d := asObj(data["delta"]); d != nil {
			if sr := objStr(d, "stop_reason"); sr != "" {
				s.stopReason = sr
			}
		}
		for k, v := range asObj(data["usage"]) {
			s.usage[k] = v
		}
	case "message_stop":
		s.handleMessageStop()
	case "error":
		s.handleError(data)
	case "ping":
		// 保活帧，忽略。
	}
}

func (s *anthToRespStream) handleMessageStart(msg map[string]interface{}) {
	id := objStr(msg, "id")
	switch {
	case id == "":
		s.responseID = "resp_p429"
	case strings.HasPrefix(id, "resp_"):
		s.responseID = id
	default:
		s.responseID = "resp_" + id
	}
	if m := objStr(msg, "model"); m != "" {
		s.model = m
	}
	for k, v := range asObj(msg["usage"]) {
		s.usage[k] = v
	}
	if s.responseStarted {
		return
	}
	s.responseStarted = true
	s.send(respSSEEvent("response.created", map[string]interface{}{
		"type": "response.created", "response": s.responseSkeleton("in_progress"),
	}))
	s.send(respSSEEvent("response.in_progress", map[string]interface{}{
		"type": "response.in_progress", "response": s.responseSkeleton("in_progress"),
	}))
}

func (s *anthToRespStream) allocOutputIndex() int {
	i := s.nextOutputIndex
	s.nextOutputIndex++
	return i
}

// emitTextStart 发 text 块的 output_item.added + content_part.added。
// 这两个事件从 content_block_start 推迟到首个岔开前言的 delta（或块收尾时补发），
// 以便 Kimi 空搜索前言整块静默丢弃（见 kimiSearchPreamble）。
func (s *anthToRespStream) emitTextStart(bs *blockState) {
	s.send(respOutputItemAdded(bs.outputIndex, respMessageItem(bs.itemID, "in_progress", "")))
	s.send(respSSEEvent("response.content_part.added", map[string]interface{}{
		"type": "response.content_part.added", "item_id": bs.itemID,
		"output_index": bs.outputIndex, "content_index": 0,
		"part": map[string]interface{}{"type": "output_text", "text": "", "annotations": []interface{}{}},
	}))
}

// emitTextDelta 发一个 output_text.delta。
func (s *anthToRespStream) emitTextDelta(bs *blockState, t string) {
	s.send(respSSEEvent("response.output_text.delta", map[string]interface{}{
		"type": "response.output_text.delta", "item_id": bs.itemID,
		"output_index": bs.outputIndex, "content_index": 0, "delta": t,
	}))
}

func (s *anthToRespStream) handleBlockStart(index int, cb map[string]interface{}) {
	if cb == nil {
		return
	}
	bs := &blockState{outputIndex: s.allocOutputIndex()}
	itemNum := bs.outputIndex
	switch objStr(cb, "type") {
	case "text":
		bs.kind = bkText
		bs.heldText = true
		bs.itemID = fmt.Sprintf("%s_msg_%d", s.responseID, itemNum)
		bs.accum = objStr(cb, "text")
		// item.added / content_part.added 推迟到 emitTextStart：Kimi 前言/query 回声
		// （kimiSearchPreamble、stripSearchQueryEcho）要整块判定（回声行整行删/
		// 重复裸前言剥光/纯前言丢弃），得等文本岔开前缀（或块收尾）再定。
	case "thinking", "redacted_thinking":
		if objStr(cb, "type") == "thinking" {
			bs.kind = bkThinking
			bs.accum = objStr(cb, "thinking")
			bs.signature = objStr(cb, "signature")
		} else {
			bs.kind = bkRedactedThinking
			bs.signature = objStr(cb, "data")
		}
		bs.itemID = fmt.Sprintf("rs_%s_%d", s.responseID, itemNum)
		s.send(respOutputItemAdded(bs.outputIndex, map[string]interface{}{
			"id": bs.itemID, "type": "reasoning", "status": "in_progress", "summary": []interface{}{},
		}))
		if bs.kind == bkThinking {
			s.send(respSSEEvent("response.reasoning_summary_part.added", map[string]interface{}{
				"type": "response.reasoning_summary_part.added", "item_id": bs.itemID,
				"output_index": bs.outputIndex, "summary_index": 0,
				"part": map[string]interface{}{"type": "summary_text", "text": ""},
			}))
		}
	case "tool_use":
		bs.kind = bkToolUse
		bs.callID = objStr(cb, "id")
		bs.name = objStr(cb, "name")
		bs.itemID = toolCallItemID(s.reg, bs.callID, bs.name)
		if bs.callID == "" {
			// call_id 空时 id 退化成纯前缀：补输出序号兜底保证唯一。
			bs.itemID = fmt.Sprintf("%s%s_%d", bs.itemID, s.responseID, itemNum)
		}
		if inp := cb["input"]; inp != nil {
			if b := canonicalJSON(inp); b != "" && b != "{}" {
				bs.startInput = b
			}
		}
		s.send(respOutputItemAdded(bs.outputIndex,
			toolCallItemFromRegistry(s.reg, bs.itemID, "in_progress", bs.callID, bs.name, "")))
	case "server_tool_use":
		// query 可能不在 start 给（实测 Kimi 常只给 id/name，query 经后续
		// input_json_delta 到达）：憋到 stop 补全 input 后再发项+记配对。
		bs.kind = bkSearchUse
		bs.searchBlk = cb
		if inp := cb["input"]; inp != nil {
			if b := canonicalJSON(inp); b != "" && b != "{}" {
				bs.startInput = b
			}
		}
	case "web_search_tool_result":
		// start 时块已完整：直接 added+done，stop 时不再处理。
		bs.kind = bkInstantDone
		if item := webSearchCallItem(cb, s.responseID, itemNum); item != nil {
			s.send(respOutputItemAdded(bs.outputIndex, item))
			s.send(respOutputItemDone(bs.outputIndex, item))
			s.items = append(s.items, item)
		}
		// 搜索块信封：结果块到达时与前面的 server_tool_use 配对封袋，作为额外
		// reasoning 项随行（客户端保管，下轮回放时还原，见 searchEnvelopePrefix）。
		if s.lastSearchUse != nil {
			if enc := encodeSearchEnvelope(s.triple, s.lastSearchUse, cb); enc != "" {
				envItem := map[string]interface{}{
					"id":                fmt.Sprintf("rs_%s_env%d", s.responseID, itemNum),
					"type":              "reasoning",
					"summary":           []interface{}{},
					"encrypted_content": enc,
				}
				envIdx := s.allocOutputIndex()
				s.send(respOutputItemAdded(envIdx, envItem))
				s.send(respOutputItemDone(envIdx, envItem))
				s.items = append(s.items, envItem)
			}
			s.lastSearchUse = nil
		}
	default:
		// 不认识的块类型：丢弃但占位，保持 index 对齐。
		bs.kind = bkInstantDone
	}
	s.blocks[index] = bs
}

func (s *anthToRespStream) handleBlockDelta(index int, delta map[string]interface{}) {
	bs := s.blocks[index]
	if bs == nil || delta == nil {
		return
	}
	switch objStr(delta, "type") {
	case "text_delta":
		t := objStr(delta, "text")
		bs.accum += t
		if bs.heldText {
			// 前言碎片未集齐、或按 stripSearchQueryEcho 剥完为空（回声行未收完/
			// 块内还没有正文）→ 继续憋着；否则补发开始事件，剩余文本作为一个
			// delta 补发（hold 条件保证岔开时剥完非空）。
			if holdSearchQueryEchoText(bs.accum) {
				return
			}
			bs.accum = stripSearchQueryEcho(bs.accum)
			s.emitTextStart(bs)
			s.emitTextDelta(bs, bs.accum)
			bs.heldText = false
			return
		}
		if t != "" {
			s.emitTextDelta(bs, t)
		}
	case "thinking_delta":
		t := objStr(delta, "thinking")
		bs.accum += t
		if t != "" {
			s.send(respSSEEvent("response.reasoning_summary_text.delta", map[string]interface{}{
				"type": "response.reasoning_summary_text.delta", "item_id": bs.itemID,
				"output_index": bs.outputIndex, "summary_index": 0, "delta": t,
			}))
		}
	case "signature_delta":
		bs.signature += objStr(delta, "signature")
	case "input_json_delta":
		t := objStr(delta, "partial_json")
		bs.accum += t
		// custom 工具与 Read 不在中途转发参数 delta：custom 要在收拢时解包成裸
		// 字符串发 custom_tool_call_input.done，Read 要在收拢时做 pages:"" sanitize
		// （中途发会把待清理的片段漏给客户端）。对照 cc-switch 同款抑制。
		// bkSearchUse 的参数碎片只累积（web_search_call 没有参数流概念，stop 时
		// 拼进 action.query），不能走 function_call_arguments.delta。
		if t != "" && bs.kind == bkToolUse && bs.name != "Read" && !s.reg.isCustomTool(bs.name) {
			s.send(respSSEEvent("response.function_call_arguments.delta", map[string]interface{}{
				"type": "response.function_call_arguments.delta", "item_id": bs.itemID,
				"output_index": bs.outputIndex, "delta": t,
			}))
		}
	}
}

func (s *anthToRespStream) handleBlockStop(index int) {
	bs := s.blocks[index]
	if bs == nil {
		return
	}
	delete(s.blocks, index)
	switch bs.kind {
	case bkText:
		if bs.heldText {
			// 憋到块收尾：剥完回声/前言为空 → 整块丢弃（没发过事件、不进 items）；
			// 否则（整块内容在 start 自带、被截断的前言前缀等）按同款规则补发全流程事件。
			rest := stripSearchQueryEcho(bs.accum)
			if strings.TrimSpace(rest) == "" {
				return
			}
			bs.accum = rest
			s.emitTextStart(bs)
			s.emitTextDelta(bs, bs.accum)
			bs.heldText = false
		}
		s.send(respSSEEvent("response.output_text.done", map[string]interface{}{
			"type": "response.output_text.done", "item_id": bs.itemID,
			"output_index": bs.outputIndex, "content_index": 0, "text": bs.accum,
		}))
		s.send(respSSEEvent("response.content_part.done", map[string]interface{}{
			"type": "response.content_part.done", "item_id": bs.itemID,
			"output_index": bs.outputIndex, "content_index": 0,
			"part": map[string]interface{}{"type": "output_text", "text": bs.accum, "annotations": []interface{}{}},
		}))
		item := respMessageItem(bs.itemID, "completed", bs.accum)
		s.send(respOutputItemDone(bs.outputIndex, item))
		s.items = append(s.items, item)
	case bkThinking, bkRedactedThinking:
		if bs.kind == bkThinking {
			s.send(respSSEEvent("response.reasoning_summary_text.done", map[string]interface{}{
				"type": "response.reasoning_summary_text.done", "item_id": bs.itemID,
				"output_index": bs.outputIndex, "summary_index": 0, "text": bs.accum,
			}))
			s.send(respSSEEvent("response.reasoning_summary_part.done", map[string]interface{}{
				"type": "response.reasoning_summary_part.done", "item_id": bs.itemID,
				"output_index": bs.outputIndex, "summary_index": 0,
				"part": map[string]interface{}{"type": "summary_text", "text": bs.accum},
			}))
		}
		// 签名打进信封：下轮客户端回放 reasoning.encrypted_content 时还原 thinking 块。
		var blk map[string]interface{}
		if bs.kind == bkThinking {
			blk = map[string]interface{}{"type": "thinking", "thinking": bs.accum, "signature": bs.signature}
		} else {
			blk = map[string]interface{}{"type": "redacted_thinking", "data": bs.signature}
		}
		item := respReasoningItem(bs.itemID, bs.accum, encodeThinkingEnvelope(blk))
		s.send(respOutputItemDone(bs.outputIndex, item))
		s.items = append(s.items, item)
	case bkSearchUse:
		// 补全 input：优先 input_json_delta 累积，回退 start 自带（与 bkToolUse 同优先级）。
		raw := bs.accum
		if raw == "" {
			raw = bs.startInput
		}
		if raw != "" {
			var inp interface{}
			if json.Unmarshal([]byte(raw), &inp) == nil {
				bs.searchBlk["input"] = inp
			}
		}
		// input 终局后块才完整：此刻发 web_search_call 项（有 query 才出调用项），
		// 并记为最近一个搜索调用，供紧随的结果块配对封信封。
		if item := webSearchCallItem(bs.searchBlk, s.responseID, bs.outputIndex); item != nil {
			s.send(respOutputItemAdded(bs.outputIndex, item))
			s.send(respOutputItemDone(bs.outputIndex, item))
			s.items = append(s.items, item)
		}
		s.lastSearchUse = bs.searchBlk
	case bkToolUse:
		// 优先用流式 input_json_delta 的累积；网关没发 delta 时回退到
		// content_block_start 自带的 input（对照 cc-switch close_block 的优先级）。
		args := bs.accum
		if args == "" {
			args = bs.startInput
		}
		if args == "" {
			args = "{}"
		} else if bs.name == "Read" {
			args = sanitizeToolUseInputJSON(bs.name, args)
		} else {
			args = canonicalizeToolArguments(args)
		}
		item := toolCallItemFromRegistry(s.reg, bs.itemID, "completed", bs.callID, bs.name, args)
		if s.reg.isCustomTool(bs.name) {
			// custom 工具：收拢时解包成裸字符串，发 custom_tool_call_input.done。
			s.send(respSSEEvent("response.custom_tool_call_input.done", map[string]interface{}{
				"type": "response.custom_tool_call_input.done", "item_id": bs.itemID,
				"output_index": bs.outputIndex, "input": objStr(item, "input"),
			}))
		} else {
			s.send(respSSEEvent("response.function_call_arguments.done", map[string]interface{}{
				"type": "response.function_call_arguments.done", "item_id": bs.itemID,
				"output_index": bs.outputIndex, "arguments": args,
			}))
		}
		s.send(respOutputItemDone(bs.outputIndex, item))
		s.items = append(s.items, item)
	}
}

func (s *anthToRespStream) handleMessageStop() {
	if s.completed || !s.responseStarted {
		return
	}
	s.completed = true
	s.send(respSSEEvent("response.completed", map[string]interface{}{
		"type": "response.completed", "response": s.buildFinalResponse(),
	}))
}

func (s *anthToRespStream) handleError(data map[string]interface{}) {
	errObj := asObj(data["error"])
	msg := objStr(errObj, "message")
	if msg == "" {
		msg = "upstream stream error"
	}
	typ := objStr(errObj, "type")
	if typ == "" {
		typ = "api_error"
	}
	resp := s.responseSkeleton("failed")
	resp["error"] = map[string]interface{}{"type": typ, "message": msg}
	s.completed = true
	s.send(respSSEEvent("response.failed", map[string]interface{}{
		"type": "response.failed", "response": resp,
	}))
}

// buildFinalResponse 用状态机累积的结果组装最终 Responses 对象
// （response.completed 载荷、stream:false 客户端的一次性 JSON 都用它）。
func (s *anthToRespStream) buildFinalResponse() map[string]interface{} {
	status, incompleteReason := mapStopReasonToStatus(s.stopReason)
	output := make([]interface{}, 0, len(s.items))
	for _, it := range s.items {
		output = append(output, it)
	}
	result := map[string]interface{}{
		"id":         s.responseID,
		"object":     "response",
		"created_at": 0,
		"status":     status,
		"model":      s.model,
		"output":     output,
		"usage":      buildResponsesUsage(s.usage),
		"error":      nil,
	}
	if incompleteReason != "" {
		result["incomplete_details"] = map[string]interface{}{"reason": incompleteReason}
	}
	return result
}

// ---- 翻译型 ResponseWriter ----

// translatingWriter 实现 http.ResponseWriter，夹在主管线与 Responses 客户端之间。
// 主管线写什么这里都能接：200+SSE → 状态机实时翻译；200+JSON（上游对流式请求回了
// 非流式）→ 缓冲后整转；非 200 → 缓冲错误体转 Responses 错误 JSON。
type translatingWriter struct {
	dst          http.ResponseWriter
	clientStream bool
	model        string
	reg          *toolRegistry
	triple       *searchTriple // 搜索信封归属三元组（主 handler 路由定案后经 setSearchTriple 注入）

	header http.Header
	status int

	mode    int // 0=未定 1=SSE 2=缓冲
	buf     []byte
	conv    *anthToRespStream
	flusher http.Flusher

	headWritten bool // 流式客户端的响应头是否已发
}

const (
	modeUndecided = iota
	modeSSE
	modeBuffered
)

func newTranslatingWriter(dst http.ResponseWriter, clientStream bool, model string, reg *toolRegistry) *translatingWriter {
	tw := &translatingWriter{
		dst:          dst,
		clientStream: clientStream,
		model:        model,
		reg:          reg,
		header:       http.Header{},
	}
	tw.flusher, _ = dst.(http.Flusher)
	emit := func(ev string) {
		if !clientStream {
			return
		}
		if !tw.headWritten {
			tw.headWritten = true
			tw.writeDstHeader(http.StatusOK, "text/event-stream")
		}
		_, _ = tw.dst.Write([]byte(ev))
		if tw.flusher != nil {
			tw.flusher.Flush()
		}
	}
	tw.conv = newAnthToRespStream(emit, model, reg)
	return tw
}

func (tw *translatingWriter) Header() http.Header { return tw.header }

// setSearchTriple 由主 handler 在路由定案后注入搜索信封的归属三元组（Responses 翻译口
// 专用；Anthropic 口的 ResponseWriter 没有此方法，handler 的类型断言自然跳过）。
// api 参数是实际生效的鉴权 token（与 effectiveKey 同口径），方法内只存其哈希与
// 派生掩码，不存原文。
func (tw *translatingWriter) setSearchTriple(url, api, model string) {
	tw.triple = newSearchTriple(url, model, api)
	tw.conv.triple = tw.triple
}

func (tw *translatingWriter) WriteHeader(status int) { tw.status = status }

// writeDstHeader 向真实客户端发响应头（只发一次）。
func (tw *translatingWriter) writeDstHeader(status int, contentType string) {
	h := tw.dst.Header()
	h.Set("Content-Type", contentType)
	if strings.HasPrefix(contentType, "text/event-stream") {
		h.Set("Cache-Control", "no-cache")
		h.Set("Connection", "keep-alive")
	}
	tw.dst.WriteHeader(status)
}

func (tw *translatingWriter) Write(p []byte) (int, error) {
	if tw.status == 0 {
		tw.status = http.StatusOK
	}
	if tw.mode == modeUndecided {
		// 首写定模式：非 200 或 Content-Type 非 SSE → 缓冲路径。
		if tw.status != http.StatusOK ||
			!strings.Contains(tw.header.Get("Content-Type"), "text/event-stream") {
			tw.mode = modeBuffered
		} else {
			tw.mode = modeSSE
		}
	}
	if tw.mode == modeBuffered {
		tw.buf = append(tw.buf, p...)
		return len(p), nil
	}
	// SSE 模式：累积后按空行切完整事件块，残块留到下次。
	tw.buf = append(tw.buf, p...)
	for {
		idx := bytes.Index(tw.buf, []byte("\n\n"))
		if idx < 0 {
			break
		}
		block := tw.buf[:idx]
		tw.buf = tw.buf[idx+2:]
		tw.handleSSEBlock(block)
	}
	return len(p), nil
}

// handleSSEBlock 解析一个完整 SSE 块（可能多行 data/comment），喂给状态机。
func (tw *translatingWriter) handleSSEBlock(block []byte) {
	var event string
	var dataLines []string
	for _, line := range strings.Split(string(block), "\n") {
		line = strings.TrimSuffix(line, "\r")
		switch {
		case strings.HasPrefix(line, ":"):
			// 注释（保活），跳过。
		case strings.HasPrefix(line, "event:"):
			event = strings.TrimSpace(line[len("event:"):])
		case strings.HasPrefix(line, "data:"):
			dataLines = append(dataLines, strings.TrimPrefix(line[len("data:"):], " "))
		}
	}
	if len(dataLines) == 0 {
		return
	}
	var data map[string]interface{}
	if err := json.Unmarshal([]byte(strings.Join(dataLines, "\n")), &data); err != nil {
		log.Printf("[Responses] SSE data 解析失败（event=%s）: %v", event, err)
		return
	}
	tw.conv.handleEvent(event, data)
}

// finish 在主管线返回后调用：冲刷残块、按模式收尾。
func (tw *translatingWriter) finish() {
	switch tw.mode {
	case modeSSE:
		// 容忍末尾缺空行的截断块。
		if len(bytes.TrimSpace(tw.buf)) > 0 {
			tw.handleSSEBlock(tw.buf)
			tw.buf = nil
		}
		if !tw.conv.completed && tw.conv.responseStarted {
			// 流中途断了（重试用尽等）：补一个 failed，让客户端拿到明确终态。
			tw.conv.handleError(map[string]interface{}{
				"error": map[string]interface{}{"type": "api_error", "message": "upstream stream interrupted"},
			})
		}
		if !tw.clientStream {
			if !tw.conv.completed {
				writeResponsesError(tw.dst, http.StatusBadGateway, "api_error", "upstream stream interrupted")
				return
			}
			b, _ := json.Marshal(tw.conv.buildFinalResponse())
			tw.writeDstHeader(http.StatusOK, "application/json")
			_, _ = tw.dst.Write(b)
		}
	case modeBuffered:
		tw.finishBuffered()
	default:
		// 主管线什么都没写（理论上到不了）：回 502。
		writeResponsesError(tw.dst, http.StatusBadGateway, "api_error", "no upstream response")
	}
}

// finishBuffered 处理缓冲路径：错误体转 Responses 错误；200 JSON 整转 Responses 对象。
func (tw *translatingWriter) finishBuffered() {
	if tw.status != http.StatusOK {
		// Anthropic 错误 JSON {error:{type,message}} → Responses 错误，状态码原样。
		var body map[string]interface{}
		typ, msg := "api_error", strings.TrimSpace(string(tw.buf))
		if json.Unmarshal(tw.buf, &body) == nil {
			if errObj := asObj(body["error"]); errObj != nil {
				if t := objStr(errObj, "type"); t != "" {
					typ = t
				}
				if m := objStr(errObj, "message"); m != "" {
					msg = m
				}
			}
		}
		if msg == "" {
			msg = http.StatusText(tw.status)
		}
		writeResponsesError(tw.dst, tw.status, typ, msg)
		return
	}
	// 200 + 非 SSE：上游对 stream:true 请求回了非流式 JSON（少见），整转。
	var msg map[string]interface{}
	if err := json.Unmarshal(tw.buf, &msg); err != nil {
		writeResponsesError(tw.dst, http.StatusBadGateway, "api_error", "invalid upstream JSON: "+err.Error())
		return
	}
	respObj := anthropicToResponsesObject(msg, tw.model, tw.reg, tw.triple)
	if !tw.clientStream {
		b, _ := json.Marshal(respObj)
		tw.writeDstHeader(http.StatusOK, "application/json")
		_, _ = tw.dst.Write(b)
		return
	}
	// 客户端要流式但只拿到一次性 JSON：补发规范事件序列（created → item add/done → completed）。
	emit := tw.conv.emit
	emit(respSSEEvent("response.created", map[string]interface{}{
		"type": "response.created", "response": map[string]interface{}{
			"id": respObj["id"], "object": "response", "created_at": 0,
			"status": "in_progress", "model": respObj["model"], "output": []interface{}{},
		},
	}))
	for i, it := range asArr(respObj["output"]) {
		item := asObj(it)
		emit(respOutputItemAdded(i, item))
		emit(respOutputItemDone(i, item))
	}
	emit(respSSEEvent("response.completed", map[string]interface{}{
		"type": "response.completed", "response": respObj,
	}))
}
