package main

// responses_stream.go — real-time Anthropic Messages SSE → OpenAI Responses SSE translation.
// Mirrors cc-switch streaming_codex_anthropic.rs's state machine and codex_responses_sse.rs's event shapes.
//
// The translating ResponseWriter sits between the new listener-port handler and the main handler: the main pipeline writes
// Anthropic SSE (or JSON/error bodies on anomalies) into it; here it's parsed block by block and transcribed live per the Responses event lifecycle
// (client stream:true) or collected and assembled into a one-shot JSON (stream:false).

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strings"
)

// ---- Responses SSE event construction (shapes mirror cc-switch codex_responses_sse.rs) ----

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

// ---- State machine ----

// blockKind content-block category.
type blockKind int

const (
	bkText blockKind = iota
	bkThinking
	bkRedactedThinking
	bkToolUse
	bkSearchUse   // server_tool_use: the query may arrive via input_json_delta (Kimi often sends only id/name in the field), so the item ships only after stop completes it
	bkInstantDone // web_search_tool_result and unknown blocks: complete at start; add+done already sent
	bkDropped     // translateNone2Low stripped thinking block: a placeholder keeps index alignment; no events, no items
)

type blockState struct {
	kind        blockKind
	outputIndex int
	itemID      string
	callID      string
	name        string
	accum       string                 // text/thinking/input_json accumulation
	signature   string                 // signature_delta accumulation
	startInput  string                 // content_block_start's own tool_use input (fallback when no delta arrives)
	searchBlk   map[string]interface{} // server_tool_use block original text (item shipped + envelope paired after stop completes input)
	heldText    bool                   // text block pending a Kimi empty-search-preamble decision: deltas held back until the verdict
}

// anthToRespStream translates an Anthropic SSE event stream into a Responses SSE event stream.
// When emit is nil (client stream:false) it only maintains internal state and assembles the JSON with buildFinalResponse at the end.
// reg is the request-side tool registry: tool_use blocks recover their custom/namespace/tool_search identities from it.
type anthToRespStream struct {
	emit            func(string)
	model           string
	reg             *toolRegistry
	responseID      string
	responseStarted bool
	completed       bool
	nextOutputIndex int
	blocks          map[int]*blockState
	items           []map[string]interface{} // Completed output items (in completion order)
	usage           map[string]interface{}
	stopReason      string
	triple          *searchTriple          // Search-envelope attribution triple (injected by translatingWriter after routing is settled; nil = no envelopes)
	lastSearchUse   map[string]interface{} // The most recent server_tool_use block (paired into an envelope when the result block arrives)
	stripThinking   bool                   // translateNone2Low: strip thinking/redacted_thinking blocks (the downstream sees a thinking-off response)
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

// responseSkeleton builds the response skeleton carried by response.created/in_progress.
func (s *anthToRespStream) responseSkeleton(status string) map[string]interface{} {
	return map[string]interface{}{
		"id": s.responseID, "object": "response", "created_at": 0,
		"status": status, "model": s.model, "output": []interface{}{},
	}
}

// handleEvent handles one Anthropic SSE event block (event name + data JSON).
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
		// Keepalive frame, ignored.
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

// emitTextStart emits a text block's output_item.added + content_part.added.
// These two events are deferred from content_block_start to the first preamble-diverging delta (or back-filled at block end),
// so a Kimi empty-search preamble can be silently dropped wholesale (see kimiSearchPreamble).
func (s *anthToRespStream) emitTextStart(bs *blockState) {
	s.send(respOutputItemAdded(bs.outputIndex, respMessageItem(bs.itemID, "in_progress", "")))
	s.send(respSSEEvent("response.content_part.added", map[string]interface{}{
		"type": "response.content_part.added", "item_id": bs.itemID,
		"output_index": bs.outputIndex, "content_index": 0,
		"part": map[string]interface{}{"type": "output_text", "text": "", "annotations": []interface{}{}},
	}))
}

// emitTextDelta emits one output_text.delta.
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
		// item.added / content_part.added are deferred to emitTextStart: the Kimi preamble/query echo
		// (kimiSearchPreamble, stripSearchQueryEcho) needs whole-block judgment (echo lines deleted whole /
		// repeated bare preambles stripped bare / pure preamble dropped), so it must wait for the text to diverge from the prefix (or the block to end).
	case "thinking", "redacted_thinking":
		if s.stripThinking {
			// translateNone2Low: wholesale strip — no events, no items, and the pre-allocated
			// outputIndex is reclaimed to keep later block indices contiguous; delta/stop skip on bkDropped.
			bs.kind = bkDropped
			s.nextOutputIndex--
			break
		}
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
			// When call_id is empty the id degenerates to a bare prefix: back-fill the output index to keep it unique.
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
		// The query may not be given at start (Kimi often sends only id/name in the field, the query arriving
		// via later input_json_delta): hold until stop completes input, then ship the item + record the pairing.
		bs.kind = bkSearchUse
		bs.searchBlk = cb
		if inp := cb["input"]; inp != nil {
			if b := canonicalJSON(inp); b != "" && b != "{}" {
				bs.startInput = b
			}
		}
	case "web_search_tool_result":
		// Complete at start: added+done sent immediately; nothing more at stop.
		bs.kind = bkInstantDone
		if item := webSearchCallItem(cb, s.responseID, itemNum); item != nil {
			s.send(respOutputItemAdded(bs.outputIndex, item))
			s.send(respOutputItemDone(bs.outputIndex, item))
			s.items = append(s.items, item)
		}
		// Search-block envelope: when the result block arrives it's bagged with the preceding server_tool_use, riding along as an extra
		// reasoning item (kept by the client, restored on next-turn replay; see searchEnvelopePrefix).
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
		// Unknown block type: dropped but placeholder-kept, preserving index alignment.
		bs.kind = bkInstantDone
	}
	s.blocks[index] = bs
}

func (s *anthToRespStream) handleBlockDelta(index int, delta map[string]interface{}) {
	bs := s.blocks[index]
	if bs == nil || delta == nil {
		return
	}
	if bs.kind == bkDropped {
		return // Stripped thinking block: deltas dropped directly
	}
	switch objStr(delta, "type") {
	case "text_delta":
		t := objStr(delta, "text")
		bs.accum += t
		if bs.heldText {
			// Preamble fragments not all in yet, or stripped-empty per stripSearchQueryEcho (echo lines not fully received /
			// no body text yet in the block) → keep holding; otherwise back-fill the start events and send the remaining text as one
			// delta (the hold condition guarantees the stripped result is non-empty when it diverges).
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
		// custom tools and Read don't forward argument deltas midway: custom must unwrap into a bare
		// string at closing time for custom_tool_call_input.done, and Read must do the pages:"" sanitize
		// at closing time (midway sends would leak unsanitized fragments to the client). Mirrors cc-switch's same suppression.
		// bkSearchUse argument fragments only accumulate (web_search_call has no argument-stream concept; they're
		// assembled into action.query at stop), they can't go through function_call_arguments.delta.
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
	if bs.kind == bkDropped {
		return // Stripped thinking block: no done event, no items
	}
	switch bs.kind {
	case bkText:
		if bs.heldText {
			// Held until block end: stripped echo/preamble empty → the whole block is dropped (no events ever sent, no items);
			// otherwise (whole content arrived at start, a truncated preamble prefix, etc.) the full event flow is back-filled per the same rules.
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
		// Signature sealed into an envelope: restored to a thinking block when the client replays reasoning.encrypted_content next turn.
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
		// Completing input: input_json_delta accumulation first, falling back to start's own (same priority as bkToolUse).
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
		// The block is complete only once input finalizes: the web_search_call item ships now (only with a query),
		// and it's recorded as the most recent search call for the immediately following result block's envelope pairing.
		if item := webSearchCallItem(bs.searchBlk, s.responseID, bs.outputIndex); item != nil {
			s.send(respOutputItemAdded(bs.outputIndex, item))
			s.send(respOutputItemDone(bs.outputIndex, item))
			s.items = append(s.items, item)
		}
		s.lastSearchUse = bs.searchBlk
	case bkToolUse:
		// Prefer the streaming input_json_delta accumulation; when the gateway sent no deltas, fall back to
		// content_block_start's own input (mirrors cc-switch close_block's priority).
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
			// custom tool: unwrapped into a bare string at closing time, sent as custom_tool_call_input.done.
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

// buildFinalResponse assembles the final Responses object from the state machine's accumulation
// (used for both the response.completed payload and the one-shot JSON for stream:false clients).
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

// ---- Translating ResponseWriter ----

// translatingWriter implements http.ResponseWriter, sitting between the main pipeline and the Responses client.
// Whatever the main pipeline writes is accepted: 200+SSE → state-machine live translation; 200+JSON (upstream answered a
// streaming request with non-streaming) → buffered wholesale conversion; non-200 → buffered error body converted to Responses error JSON.
type translatingWriter struct {
	dst           http.ResponseWriter
	clientStream  bool
	model         string
	reg           *toolRegistry
	triple        *searchTriple // Search-envelope attribution triple (injected by the main handler via setSearchTriple after routing is settled)
	stripThinking bool          // translateNone2Low upgrade stream: non-streaming wholesale conversion (finishBuffered) strips thinking blocks; streaming is handled by conv.stripThinking

	header http.Header
	status int

	mode    int // 0=undecided 1=SSE 2=buffered
	buf     []byte
	conv    *anthToRespStream
	flusher http.Flusher

	headWritten bool // Whether the streaming client's response headers have been sent

	downFlight *flight // flight reference for dual-link recording (injected by the main handler via setDownTap), for back-flushing the downstream-side archive after archiving
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

// tapResponseWriter wraps the downstream ResponseWriter: before Write it tees the bytes into the flight's
// contentDown (the proxy→downstream side record); Flush and other behaviors pass through to the original writer.
type tapResponseWriter struct {
	http.ResponseWriter
	tap func([]byte)
}

func (t tapResponseWriter) Write(p []byte) (int, error) {
	t.tap(p)
	return t.ResponseWriter.Write(p)
}

// Flush passes through the underlying http.Flusher (tw.flusher in emit still holds the original dst, unaffected).
func (t tapResponseWriter) Flush() {
	if f, ok := t.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// setDownTap is injected by the main handler after flight creation as the downstream-side record point: with dst swapped for the tap wrapper,
// all dst.Write paths — emit/finish/finishBuffered/error writes — are naturally teed
// (same type-assertion injection as setSearchTriple; the Anthropic port's writer lacks this method and is naturally skipped).
func (tw *translatingWriter) setDownTap(f *flight) {
	tw.dst = tapResponseWriter{ResponseWriter: tw.dst, tap: f.appendContentDown}
	tw.downFlight = f // For the handler's post-archive tail back-fill (flushing finish's downstream-side bytes into the archive)
}

// setSearchTriple is injected by the main handler after routing is settled with the search envelope's attribution triple (Responses
// translation port only; the Anthropic port's ResponseWriter lacks this method, so the handler's type assertion naturally skips).
// The api parameter is the actually-effective auth token (same semantics as effectiveKey); only its hash and
// derived mask are stored, never the plaintext.
func (tw *translatingWriter) setSearchTriple(url, api, model string) {
	tw.triple = newSearchTriple(url, model, api)
	tw.conv.triple = tw.triple
}

func (tw *translatingWriter) WriteHeader(status int) { tw.status = status }

// writeDstHeader sends response headers to the real client (only once).
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
		// First write decides the mode: non-200 or non-SSE Content-Type → buffered path.
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
	// SSE mode: accumulate and cut complete event blocks at blank lines, keeping the remainder for next time.
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

// handleSSEBlock parses one complete SSE block (possibly multi-line data/comment) and feeds the state machine.
func (tw *translatingWriter) handleSSEBlock(block []byte) {
	var event string
	var dataLines []string
	for _, line := range strings.Split(string(block), "\n") {
		line = strings.TrimSuffix(line, "\r")
		switch {
		case strings.HasPrefix(line, ":"):
			// Comment (keepalive), skipped.
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
		log.Printf("[Responses] failed to parse SSE data (event=%s): %v", event, err)
		return
	}
	tw.conv.handleEvent(event, data)
}

// finish is called after the main pipeline returns: flush the remainder, wrap up per mode.
func (tw *translatingWriter) finish() {
	switch tw.mode {
	case modeSSE:
		// Tolerate a truncated trailing block missing its blank line.
		if len(bytes.TrimSpace(tw.buf)) > 0 {
			tw.handleSSEBlock(tw.buf)
			tw.buf = nil
		}
		if !tw.conv.completed && tw.conv.responseStarted {
			// Stream broke midway (retries exhausted, etc.): append a failed so the client gets a definite terminal state.
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
		// The main pipeline wrote nothing (unreachable in theory): answer 502.
		writeResponsesError(tw.dst, http.StatusBadGateway, "api_error", "no upstream response")
	}
}

// finishBuffered handles the buffered path: error bodies convert to Responses errors; 200 JSON converts wholesale to a Responses object.
func (tw *translatingWriter) finishBuffered() {
	if tw.status != http.StatusOK {
		// Anthropic error JSON {error:{type,message}} → Responses error, status code as-is.
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
	// 200 + non-SSE: the upstream answered a stream:true request with non-streaming JSON (rare); convert wholesale.
	var msg map[string]interface{}
	if err := json.Unmarshal(tw.buf, &msg); err != nil {
		writeResponsesError(tw.dst, http.StatusBadGateway, "api_error", "invalid upstream JSON: "+err.Error())
		return
	}
	respObj := anthropicToResponsesObject(msg, tw.model, tw.reg, tw.triple, tw.stripThinking)
	if !tw.clientStream {
		b, _ := json.Marshal(respObj)
		tw.writeDstHeader(http.StatusOK, "application/json")
		_, _ = tw.dst.Write(b)
		return
	}
	// The client wanted streaming but got a one-shot JSON: back-fill the canonical event sequence (created → item add/done → completed).
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
