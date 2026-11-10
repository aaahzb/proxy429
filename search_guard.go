package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
)

const (
	// searchGuardInstruction is appended only to marker requests on routes that opt into the guard.
	searchGuardInstruction = "If the web search returns no results, state only that no results were found for this query. Do not answer from your own knowledge, and do not suggest alternative keywords."
	// searchGuardFixedText is the complete client-bound answer when every upstream search result is empty.
	searchGuardFixedText = "No results were found for this search query. Retry with different or broader keywords; do not fabricate an answer from prior knowledge."
)

// searchGuardUsage carries the real upstream token counts into the synthesized response without re-counting them.
type searchGuardUsage struct {
	input         int64
	cacheRead     int64
	cacheCreation int64
	output        int64
}

// resolveSearchGuard applies the same route-wins rule as the other claudeSearch* fields.
// A pointer distinguishes an explicit false from an unset route that inherits search_fallback.
func resolveSearchGuard(route *RouteRule, fallback *SearchRoute) bool {
	if route != nil && route.NoBullshitOnEmptyClaudeSearch != nil {
		return *route.NoBullshitOnEmptyClaudeSearch
	}
	if fallback != nil && fallback.NoBullshitOnEmptyClaudeSearch != nil {
		return *fallback.NoBullshitOnEmptyClaudeSearch
	}
	return false
}

// onlyEmptySearchResults reports whether an Anthropic SSE stream contains at least one web search result block
// and every such block has the exact empty-array shape content:[]. Unknown or malformed shapes fail closed to passthrough.
func onlyEmptySearchResults(raw []byte) bool {
	seen := false
	for _, line := range bytes.Split(raw, []byte("\n")) {
		line = bytes.TrimSpace(line)
		if !bytes.HasPrefix(line, []byte("data:")) {
			continue
		}
		payload := bytes.TrimSpace(bytes.TrimPrefix(line, []byte("data:")))
		if len(payload) == 0 || bytes.Equal(payload, []byte("[DONE]")) {
			continue
		}
		var ev struct {
			Type         string          `json:"type"`
			ContentBlock json.RawMessage `json:"content_block"`
		}
		if json.Unmarshal(payload, &ev) != nil {
			return false // A partially unparsable stream cannot prove that every result is empty.
		}
		if ev.Type != "content_block_start" || len(ev.ContentBlock) == 0 {
			continue
		}
		var block struct {
			Type    string          `json:"type"`
			Content json.RawMessage `json:"content"`
		}
		if json.Unmarshal(ev.ContentBlock, &block) != nil {
			return false
		}
		if block.Type != "web_search_tool_result" {
			continue
		}
		seen = true
		content := bytes.TrimSpace(block.Content)
		if len(content) == 0 || content[0] != '[' {
			return false // null, an error object, or a missing field is not the observed empty-array failure.
		}
		var items []json.RawMessage
		if json.Unmarshal(content, &items) != nil || len(items) != 0 {
			return false // Any real result disables replacement for the whole stream.
		}
	}
	return seen
}

// synthesizeSearchGuardSSE builds a complete Anthropic stream containing only the fixed no-results sentence.
// The usage values describe the discarded upstream work, so the client and proxy see the real cost exactly once.
func synthesizeSearchGuardSSE(id uint64, model string, usage searchGuardUsage) []byte {
	var buf bytes.Buffer
	write := func(name string, payload interface{}) {
		data, _ := json.Marshal(payload) // All payloads are fixed structs/maps of JSON-safe values; marshal cannot fail.
		fmt.Fprintf(&buf, "event: %s\ndata: %s\n\n", name, data)
	}
	write("message_start", map[string]interface{}{
		"type": "message_start",
		"message": map[string]interface{}{
			"id":            fmt.Sprintf("msg_proxy429_search_guard_%d", id),
			"type":          "message",
			"role":          "assistant",
			"model":         model,
			"content":       []interface{}{},
			"stop_reason":   nil,
			"stop_sequence": nil,
			"usage":         map[string]interface{}{"input_tokens": usage.input, "cache_read_input_tokens": usage.cacheRead, "cache_creation_input_tokens": usage.cacheCreation, "output_tokens": 0},
		},
	})
	write("content_block_start", map[string]interface{}{
		"type": "content_block_start", "index": 0,
		"content_block": map[string]interface{}{"type": "text", "text": ""},
	})
	write("content_block_delta", map[string]interface{}{
		"type": "content_block_delta", "index": 0,
		"delta": map[string]interface{}{"type": "text_delta", "text": searchGuardFixedText},
	})
	write("content_block_stop", map[string]interface{}{"type": "content_block_stop", "index": 0})
	write("message_delta", map[string]interface{}{
		"type":  "message_delta",
		"delta": map[string]interface{}{"stop_reason": "end_turn", "stop_sequence": nil},
		"usage": map[string]interface{}{"output_tokens": usage.output},
	})
	write("message_stop", map[string]interface{}{"type": "message_stop"})
	return buf.Bytes()
}

// searchGuardWriter delays an opted-in native Anthropic response until the upstream outcome is known.
// The ordinary forward path still performs model write-back, thinking stripping, usage parsing, and both tees;
// only the actual socket write is deferred. Flush is intentionally held so no discarded prefix can escape.
type searchGuardWriter struct {
	dst         http.ResponseWriter
	flight      *flight
	headersSent bool
	status      int
	wroteHeader bool
	upstream    bytes.Buffer
	downstream  bytes.Buffer
}

func newSearchGuardWriter(dst http.ResponseWriter, f *flight, headersSent bool) *searchGuardWriter {
	return &searchGuardWriter{dst: dst, flight: f, headersSent: headersSent, status: http.StatusOK, wroteHeader: headersSent}
}

func (w *searchGuardWriter) Header() http.Header { return w.dst.Header() }

func (w *searchGuardWriter) WriteHeader(status int) {
	if w.wroteHeader {
		return
	}
	w.status = status
	w.wroteHeader = true
}

func (w *searchGuardWriter) Write(p []byte) (int, error) {
	if !w.wroteHeader {
		w.WriteHeader(http.StatusOK)
	}
	return w.downstream.Write(p)
}

// Flush implements http.Flusher while keeping every client-bound byte inside the guard buffer.
func (w *searchGuardWriter) Flush() {}

// observeUpstream captures the exact pre-transform bytes; detection never relies on a truncated flight preview.
func (w *searchGuardWriter) observeUpstream(p []byte) {
	w.upstream.Write(p)
}

// finish releases either the unchanged transformed response or the fixed no-results replacement.
// Original usage has already been parsed by forward; the synthetic bytes are deliberately not parsed again.
func (w *searchGuardWriter) finish(usage searchGuardUsage) {
	replace := onlyEmptySearchResults(w.upstream.Bytes())
	if !w.headersSent {
		if replace {
			w.dst.Header().Del("Content-Length") // The fixed stream has a different length from the discarded upstream body.
		}
		w.dst.WriteHeader(w.status)
	}
	payload := w.downstream.Bytes()
	if replace {
		payload = synthesizeSearchGuardSSE(w.flight.id, w.flight.origModel, usage)
		w.flight.searchGuard = true
		w.flight.bytes.Store(0)
		// Bytes buffered internally were never sent. Discard their downstream mirror before recording the real replacement.
		w.flight.contentMu.Lock()
		w.flight.contentDown = nil
		w.flight.fullContentDown = nil
		w.flight.fullPartialDown = false
		w.flight.contentMu.Unlock()
		log.Printf("[search-guard] #%d all web search results were empty; discarded %s of upstream response and synthesized the fixed no-results stream", w.flight.id, humanBytes(int64(w.downstream.Len())))
	}
	if len(payload) > 0 {
		n, err := w.dst.Write(payload)
		if replace {
			if n > 0 {
				w.flight.bytes.Add(int64(n))
				w.flight.appendContentDown(payload[:n])
			}
			if err == nil && n == len(payload) {
				w.flight.delivered.Store(true)
			}
		}
	}
	if flusher, ok := w.dst.(http.Flusher); ok {
		flusher.Flush()
	}
}
