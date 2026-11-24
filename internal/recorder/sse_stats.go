package recorder

import (
	"bytes"
	"encoding/json"
	"strings"

	"proxy429/internal/jsonutil"
)

// parseSSEStats parses one SSE line (data: {...}) and accumulates the usage token counts into the global stats.
// output_tokens is a per-stream cumulative value, accumulated by delta (new - lastOutput) so concurrent streams don't overwrite each other.
// On seeing a message_delta with usage, set *sawDeltaUsage=true: the real usage breakdown arrives in that event
// (ARK's start is all zeros; Kimi's start.input includes cache_read and start.cr=0);
// if the stream breaks before it arrives, the caller should roll back this stream's accumulated deltas (see rollbackUsageStats). Pass nil to skip the flag.
func ParseSSEStats(line []byte, lastOutput, lastInput, lastCacheRead, lastCacheCreation *int64, sawDeltaUsage *bool) {
	s := bytes.TrimRight(line, "\r\n")
	if !bytes.HasPrefix(s, []byte("data:")) {
		return
	}
	payload := bytes.TrimSpace(bytes.TrimPrefix(s, []byte("data:")))
	if len(payload) == 0 || bytes.Equal(payload, []byte("[DONE]")) {
		return
	}
	var ev map[string]interface{}
	if json.Unmarshal(payload, &ev) != nil {
		return
	}
	var usage map[string]interface{}
	switch ev["type"] {
	case "message_start":
		if msg, ok := ev["message"].(map[string]interface{}); ok {
			usage, _ = msg["usage"].(map[string]interface{})
		}
	case "message_delta":
		usage, _ = ev["usage"].(map[string]interface{})
		if usage != nil && sawDeltaUsage != nil {
			*sawDeltaUsage = true
		}
	}
	if usage == nil {
		return
	}
	Stats.Mu.Lock()
	defer Stats.Mu.Unlock()
	// Each usage field takes its last-seen value in this stream (later values overwrite earlier ones); the global total only gains the new-minus-last delta.
	// So once a response completes, both the per-stream and global figures hold each field's final value.
	// Upstream semantics differ: ARK's message_start is all zeros, the real values arrive in message_delta;
	// Kimi's message_start.input includes cache_read while message_delta.input is pure input (smaller) —
	// taking the last value yields the standard-semantics input_tokens. Missing fields keep their previous value (never overwritten with 0).
	// Note: ARK's usage has no cache_creation_input_tokens; cache writes for that upstream count as 0.
	if v, ok := usage["input_tokens"]; ok {
		in := jsonutil.ToInt64(v)
		if in != *lastInput {
			Stats.InputTokens += in - *lastInput
			*lastInput = in
		}
	}
	if v, ok := usage["cache_read_input_tokens"]; ok {
		cr := jsonutil.ToInt64(v)
		if cr != *lastCacheRead {
			Stats.CacheRead += cr - *lastCacheRead
			*lastCacheRead = cr
		}
	}
	if v, ok := usage["cache_creation_input_tokens"]; ok {
		cc := jsonutil.ToInt64(v)
		if cc != *lastCacheCreation {
			Stats.CacheCreation += cc - *lastCacheCreation
			*lastCacheCreation = cc
		}
	}
	if v, ok := usage["output_tokens"]; ok {
		out := jsonutil.ToInt64(v)
		if out != *lastOutput {
			Stats.OutputTokens += out - *lastOutput
			*lastOutput = out
		}
	}
}

// toolCallStart is the parse result of a tool_use/server_tool_use in content_block_start:
// index tracks the same call across lines (input_json_delta/content_block_stop align by it);
// input is the arguments carried by the start event itself (tool_use always carries {}, real arguments arrive in later deltas; server_tool_use
// may already be complete, and Kimi's empty search has no input).
type toolCallStart struct {
	Index int
	Name  string
	Input json.RawMessage
}

// parseToolCallStart parses a tool-call block-start event from an SSE data line: returns a result only for content_block_start
// with block type tool_use/server_tool_use, otherwise ok=false. A substring pre-filter runs before JSON parsing,
// avoiding a full deserialization of every line.
func ParseToolCallStart(line []byte) (ts toolCallStart, ok bool) {
	s := bytes.TrimSpace(line)
	if !bytes.HasPrefix(s, []byte("data:")) {
		return ts, false
	}
	payload := bytes.TrimSpace(s[len("data:"):])
	if !bytes.Contains(payload, []byte("content_block_start")) || !bytes.Contains(payload, []byte("tool_use")) {
		return ts, false
	}
	var ev struct {
		Type         string `json:"type"`
		Index        int    `json:"index"`
		ContentBlock struct {
			Type  string          `json:"type"`
			Name  string          `json:"name"`
			Input json.RawMessage `json:"input"`
		} `json:"content_block"`
	}
	if json.Unmarshal(payload, &ev) != nil || ev.Type != "content_block_start" {
		return ts, false
	}
	switch ev.ContentBlock.Type {
	case "tool_use", "server_tool_use":
		return toolCallStart{Index: ev.Index, Name: ev.ContentBlock.Name, Input: ev.ContentBlock.Input}, true
	}
	return ts, false
}

// parseToolCallName extracts the tool-call name from an SSE data line (the name part of parseToolCallStart).
func ParseToolCallName(line []byte) string {
	if ts, ok := ParseToolCallStart(line); ok {
		return ts.Name
	}
	return ""
}

// parseToolArgsDelta parses an input_json_delta from an SSE data line, returning the owning block's index and the argument fragment.
func ParseToolArgsDelta(line []byte) (index int, partial string, ok bool) {
	s := bytes.TrimSpace(line)
	if !bytes.HasPrefix(s, []byte("data:")) {
		return 0, "", false
	}
	payload := bytes.TrimSpace(s[len("data:"):])
	if !bytes.Contains(payload, []byte("input_json_delta")) {
		return 0, "", false
	}
	var ev struct {
		Type  string `json:"type"`
		Index int    `json:"index"`
		Delta struct {
			Type    string `json:"type"`
			Partial string `json:"partial_json"`
		} `json:"delta"`
	}
	if json.Unmarshal(payload, &ev) != nil || ev.Type != "content_block_delta" || ev.Delta.Type != "input_json_delta" {
		return 0, "", false
	}
	return ev.Index, ev.Delta.Partial, true
}

// parseToolBlockStop parses a content_block_stop from an SSE data line, returning the ending block's index.
func ParseToolBlockStop(line []byte) (index int, ok bool) {
	s := bytes.TrimSpace(line)
	if !bytes.HasPrefix(s, []byte("data:")) {
		return 0, false
	}
	payload := bytes.TrimSpace(s[len("data:"):])
	if !bytes.Contains(payload, []byte("content_block_stop")) {
		return 0, false
	}
	var ev struct {
		Type  string `json:"type"`
		Index int    `json:"index"`
	}
	if json.Unmarshal(payload, &ev) != nil || ev.Type != "content_block_stop" {
		return 0, false
	}
	return ev.Index, true
}

// isEmptyArgsJSON reports whether a JSON text is equivalent to an "empty arguments object": empty string, {}, or null
// (all whitespace ignored). Dedicated to the *0 tag decision.
func IsEmptyArgsJSON(s string) bool {
	s = strings.Map(func(r rune) rune {
		switch r {
		case ' ', '\t', '\n', '\r':
			return -1
		}
		return r
	}, s)
	return s == "" || s == "{}" || s == "null"
}

// openToolCall tracks one in-progress streaming tool call so the block end can judge whether the arguments object is empty
// (the *0 tag). Argument fragments keep only the first 64 bytes: anything longer can't be an empty object, and large arguments
// (an Edit's full diff) don't deserve a full copy.
type OpenToolCall struct {
	Name    string
	HasArgs bool   // The start event already carried a non-empty input (complete server_tool_use block)
	buf     []byte // input_json_delta fragments (≤64 bytes, judged via isEmptyArgsJSON at stop)
}

// notePartial accumulates argument fragments, capped at 64 bytes.
func (t *OpenToolCall) NotePartial(p string) {
	if len(t.buf) >= 64 {
		return
	}
	n := 64 - len(t.buf)
	if n > len(p) {
		n = len(p)
	}
	t.buf = append(t.buf, p[:n]...)
}

// argsEmpty reports whether the arguments object was empty at block end.
func (t *OpenToolCall) ArgsEmpty() bool {
	return !t.HasArgs && IsEmptyArgsJSON(string(t.buf))
}

// parseResponsesStreamStats is the Responses-protocol version of parseSSEStats (for url_response_api passthrough streams).
// In Responses SSE, usage appears only once, in the terminal event (response.completed/response.incomplete);
// and OpenAI semantics have input_tokens including the cache total, with cached_tokens the hit portion — split into
// fresh = input - cached (floor 0) + cr = cached, aligning with Anthropic semantics before joining the global aggregation.
// On seeing the terminal usage, set *sawDeltaUsage=true (same semantics as Anthropic's message_delta usage:
// the real usage has arrived; if the stream breaks first, the caller rolls back — the trackers are all 0 in that case, so the rollback is a no-op).
func ParseResponsesStreamStats(line []byte, lastOutput, lastInput, lastCacheRead, lastCacheCreation *int64, sawDeltaUsage *bool) {
	s := bytes.TrimSpace(line)
	if !bytes.HasPrefix(s, []byte("data:")) {
		return
	}
	payload := bytes.TrimSpace(s[len("data:"):])
	// Pre-filter: only terminal events get a full deserialization (response.created etc. carry no usage or all zeros; skipped directly).
	if !bytes.Contains(payload, []byte("response.completed")) && !bytes.Contains(payload, []byte("response.incomplete")) {
		return
	}
	var ev struct {
		Type     string `json:"type"`
		Response struct {
			Usage *struct {
				InputTokens  int64 `json:"input_tokens"`
				OutputTokens int64 `json:"output_tokens"`
				// cache_creation_input_tokens is not a standard OpenAI field; some Anthropic-compatible endpoints carry it. Missing = 0.
				CacheCreation     int64 `json:"cache_creation_input_tokens"`
				InputTokenDetails struct {
					CachedTokens int64 `json:"cached_tokens"`
				} `json:"input_tokens_details"`
			} `json:"usage"`
		} `json:"response"`
	}
	if json.Unmarshal(payload, &ev) != nil {
		return
	}
	if (ev.Type != "response.completed" && ev.Type != "response.incomplete") || ev.Response.Usage == nil {
		return
	}
	u := ev.Response.Usage
	in := u.InputTokens - u.InputTokenDetails.CachedTokens
	if in < 0 {
		in = 0 // Defensive: don't let anomalous upstream semantics push the global input negative
	}
	cr := u.InputTokenDetails.CachedTokens
	cc := u.CacheCreation
	out := u.OutputTokens
	if sawDeltaUsage != nil {
		*sawDeltaUsage = true
	}
	Stats.Mu.Lock()
	defer Stats.Mu.Unlock()
	// Same pattern as parseSSEStats: later values overwrite earlier ones, the global total only gains the delta (usage arrives only once, but keeping the same shape lets the trackers be reused).
	if in != *lastInput {
		Stats.InputTokens += in - *lastInput
		*lastInput = in
	}
	if cr != *lastCacheRead {
		Stats.CacheRead += cr - *lastCacheRead
		*lastCacheRead = cr
	}
	if cc != *lastCacheCreation {
		Stats.CacheCreation += cc - *lastCacheCreation
		*lastCacheCreation = cc
	}
	if out != *lastOutput {
		Stats.OutputTokens += out - *lastOutput
		*lastOutput = out
	}
}

// parseResponsesToolStart is the Responses-protocol version of parseToolCallStart: it extracts (itemID, tool name)
// from response.output_item.added — function_call/custom_tool_call take item.name;
// web_search_call (a server-side tool, no name) fixedly returns "web_search".
// itemID aligns with output_item.done (the *0 empty-args decision). Pre-filter before parsing, avoiding per-line deserialization.
func ParseResponsesToolStart(line []byte) (itemID, name string, ok bool) {
	s := bytes.TrimSpace(line)
	if !bytes.HasPrefix(s, []byte("data:")) {
		return "", "", false
	}
	payload := bytes.TrimSpace(s[len("data:"):])
	if !bytes.Contains(payload, []byte("output_item.added")) {
		return "", "", false
	}
	var ev struct {
		Type string `json:"type"`
		Item struct {
			ID   string `json:"id"`
			Type string `json:"type"`
			Name string `json:"name"`
		} `json:"item"`
	}
	if json.Unmarshal(payload, &ev) != nil || ev.Type != "response.output_item.added" {
		return "", "", false
	}
	switch ev.Item.Type {
	case "function_call", "custom_tool_call":
		return ev.Item.ID, ev.Item.Name, true
	case "web_search_call":
		return ev.Item.ID, "web_search", true
	}
	return "", "", false
}

// parseResponsesToolName extracts the tool-call name from an SSE data line (the name part of parseResponsesToolStart).
func ParseResponsesToolName(line []byte) string {
	if _, name, ok := ParseResponsesToolStart(line); ok {
		return name
	}
	return ""
}

// parseResponsesToolDone extracts (itemID, arguments-object-empty) from a response.output_item.done event:
// function_call checks the final arguments, custom_tool_call checks input, web_search_call checks action
// (no query and no sources = empty). done events carry complete arguments, so no cross-line accumulation is needed. Other item types return ok=false.
func ParseResponsesToolDone(line []byte) (itemID string, argsEmpty bool, ok bool) {
	s := bytes.TrimSpace(line)
	if !bytes.HasPrefix(s, []byte("data:")) {
		return "", false, false
	}
	payload := bytes.TrimSpace(s[len("data:"):])
	if !bytes.Contains(payload, []byte("output_item.done")) {
		return "", false, false
	}
	var ev struct {
		Type string `json:"type"`
		Item struct {
			ID        string          `json:"id"`
			Type      string          `json:"type"`
			Arguments string          `json:"arguments"`
			Input     json.RawMessage `json:"input"`
			Action    json.RawMessage `json:"action"`
		} `json:"item"`
	}
	if json.Unmarshal(payload, &ev) != nil || ev.Type != "response.output_item.done" {
		return "", false, false
	}
	switch ev.Item.Type {
	case "function_call":
		return ev.Item.ID, IsEmptyArgsJSON(ev.Item.Arguments), true
	case "custom_tool_call":
		// A custom tool's input is a bare value: empty = absent / null / empty string.
		return ev.Item.ID, len(ev.Item.Input) == 0 || IsEmptyArgsJSON(string(ev.Item.Input)) || string(ev.Item.Input) == `""`, true
	case "web_search_call":
		var action struct {
			Query   string        `json:"query"`
			Sources []interface{} `json:"sources"`
		}
		json.Unmarshal(ev.Item.Action, &action)
		return ev.Item.ID, action.Query == "" && len(action.Sources) == 0, true
	}
	return "", false, false
}

// rollbackUsageStats rolls back all usage deltas this stream has added to the global stats.
// parseSSEStats's streaming deltas sum exactly to the trackers' current values, so subtracting them wholesale is an exact undo.
// Used for interrupted streams (client disconnect / upstream dropout, before the message_delta with the real breakdown arrived):
// all that has been counted is message_start's estimated usage — Kimi's start.input measurably includes cache_read
// and start.cr=0, so keeping it would count the entire context as missed input, severely dragging down the aggregate hit rate.
func RollbackUsageStats(in, cr, cc, out int64) {
	Stats.Mu.Lock()
	Stats.InputTokens -= in
	Stats.CacheRead -= cr
	Stats.CacheCreation -= cc
	Stats.OutputTokens -= out
	Stats.Mu.Unlock()
}

// parseNonStreamUsage extracts the usage field from a non-streaming JSON response body (classifier and similar requests return non-streaming JSON).
// Supports Anthropic non-streaming (usage.input_tokens/cache_read_input_tokens/cache_creation_input_tokens/output_tokens),
// OpenAI non-streaming (usage.prompt_tokens/completion_tokens, no cache_*, the two cache fields return 0),
// and Responses non-streaming (when a passthrough-stream client sends stream:false, the upstream returns a whole response object:
// usage.input_tokens includes the cache total and input_tokens_details.cached_tokens is the hit portion, split with the same semantics as streaming).
// Returns ok=false when nothing can be extracted.
func ParseNonStreamUsage(content []byte) (input, cacheRead, cacheCreation, output int64, ok bool) {
	var obj map[string]interface{}
	if json.Unmarshal(content, &obj) != nil {
		return 0, 0, 0, 0, false
	}
	u, _ := obj["usage"].(map[string]interface{})
	if u == nil {
		return 0, 0, 0, 0, false
	}
	// The Responses shape must be checked first: it also has top-level input_tokens/output_tokens;
	// the criterion is the presence of input_tokens_details (neither Anthropic's nor OpenAI's usage has this sub-object).
	if det, _ := u["input_tokens_details"].(map[string]interface{}); det != nil {
		cached := jsonutil.ToInt64(det["cached_tokens"])
		input = jsonutil.ToInt64(u["input_tokens"]) - cached
		if input < 0 {
			input = 0
		}
		cacheRead = cached
		cacheCreation = jsonutil.ToInt64(u["cache_creation_input_tokens"])
		output = jsonutil.ToInt64(u["output_tokens"])
		if input == 0 && cacheRead == 0 && cacheCreation == 0 && output == 0 {
			return 0, 0, 0, 0, false
		}
		return input, cacheRead, cacheCreation, output, true
	}
	input = jsonutil.ToInt64(u["input_tokens"])
	cacheRead = jsonutil.ToInt64(u["cache_read_input_tokens"])
	cacheCreation = jsonutil.ToInt64(u["cache_creation_input_tokens"])
	output = jsonutil.ToInt64(u["output_tokens"])
	if input == 0 && cacheRead == 0 && cacheCreation == 0 && output == 0 {
		// OpenAI style: prompt_tokens / completion_tokens (no cache_*)
		input = jsonutil.ToInt64(u["prompt_tokens"])
		output = jsonutil.ToInt64(u["completion_tokens"])
	}
	if input == 0 && cacheRead == 0 && cacheCreation == 0 && output == 0 {
		return 0, 0, 0, 0, false
	}
	return input, cacheRead, cacheCreation, output, true
}
