package main

// mock_429.go: local test server simulating upstream behaviors, switched via ?mode=:
//   mode=429      → returns HTTP 429 (case A: status-code retry)
//   mode=bodyerr  → returns HTTP 200 + SSE error event (case B: in-body error retry)
//   mode=ok       → returns HTTP 200 + normal SSE (case C: normal pass-through)
//   mode=think    → returns HTTP 200 + SSE with a leading thinking block (exercises the convertOff2Low response stripper)
// It also prints the received thinking/reasoning_effort/max_tokens to verify classifier rewriting.
// Usage (from the project root): go run ./test — listens on 127.0.0.1:9099.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"time"
)

func main() {
	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		// Mode: ?mode= query, or a path prefix (/think/..., /ok/..., /bodyerr/...) so routed upstreams
		// (Base URL + /v1/messages join) can pick a behavior; bare path defaults to 429.
		mode := r.URL.Query().Get("mode")
		if mode == "" {
			switch {
			case strings.HasPrefix(r.URL.Path, "/ok/"):
				mode = "ok"
			case strings.HasPrefix(r.URL.Path, "/think/"):
				mode = "think"
			case strings.HasPrefix(r.URL.Path, "/bodyerr/"):
				mode = "bodyerr"
			default:
				mode = "429"
			}
		}

		// Parse the request body and print thinking-related fields to verify proxy rewriting.
		var p map[string]interface{}
		if err := json.Unmarshal(body, &p); err == nil {
			thinking := p["thinking"]
			reasoningEffort := p["reasoning_effort"]
			maxTokens := p["max_tokens"]
			log.Printf("[MOCK] 收到 %s %s mode=%s thinking=%v reasoning_effort=%v max_tokens=%v key顺序=%v",
				r.Method, r.URL.Path, mode, thinking, reasoningEffort, maxTokens, topKeys(body))
		} else {
			log.Printf("[MOCK] 收到 %s %s mode=%s (body=%d字节)", r.Method, r.URL.Path, mode, len(body))
		}
		// Echo the request's model into message_start so model-routing proxies exercise the response-model write-back path.
		reqModel, _ := p["model"].(string)

		switch mode {
		case "bodyerr":
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(200)
			w.Write([]byte("event: error\ndata: {\"type\":\"error\",\"error\":{\"type\":\"rate_limit_error\",\"message\":\"rate limited\"}}\n\n"))
			return
		case "ok":
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(200)
			flusher, _ := w.(http.Flusher)
			// message_start carries usage: initial input/cache_read/cache_creation/output values,
			// so the proxy's live status row shows cache hits/writes. The request's model is echoed back like a real upstream.
			msgStart := `{"type":"message_start","message":{"usage":{"input_tokens":10,"cache_read_input_tokens":5,"cache_creation_input_tokens":3,"output_tokens":1}}}`
			if reqModel != "" {
				msgStart = fmt.Sprintf(`{"type":"message_start","message":{"model":%q,"usage":{"input_tokens":10,"cache_read_input_tokens":5,"cache_creation_input_tokens":3,"output_tokens":1}}}`, reqModel)
			}
			w.Write([]byte("event: message_start\ndata: " + msgStart + "\n\n"))
			if flusher != nil {
				flusher.Flush()
			}
			// Faithful block framing (start/delta/stop with index and typed text_delta) so downstream
			// converters that key on delta.type see real text. Each delta is followed by a message_delta
			// updating output_tokens (cumulative) — the status row's "output" and "tok/s" tick up.
			w.Write([]byte("event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n"))
			for i := 1; i <= 5; i++ {
				w.Write([]byte("event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"hello\"}}\n\n"))
				w.Write([]byte(fmt.Sprintf("event: message_delta\ndata: {\"type\":\"message_delta\",\"usage\":{\"output_tokens\":%d}}\n\n", i*10)))
				if flusher != nil {
					flusher.Flush()
				}
				time.Sleep(100 * time.Millisecond)
			}
			w.Write([]byte("event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\n"))
			w.Write([]byte("event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"))
			if flusher != nil {
				flusher.Flush()
			}
			return
		case "think":
			// Thinking-block SSE: block 0 is thinking (with thinking_delta + signature_delta), block 1 is text —
			// a stripper downstream must drop block 0 wholesale and renumber block 1 to index 0.
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(200)
			flusher, _ := w.(http.Flusher)
			msgStart := `{"type":"message_start","message":{"usage":{"input_tokens":10,"output_tokens":1}}}`
			if reqModel != "" {
				msgStart = fmt.Sprintf(`{"type":"message_start","message":{"model":%q,"usage":{"input_tokens":10,"output_tokens":1}}}`, reqModel)
			}
			w.Write([]byte("event: message_start\ndata: " + msgStart + "\n\n"))
			w.Write([]byte("event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"thinking\",\"thinking\":\"\"}}\n\n"))
			w.Write([]byte("event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"thinking_delta\",\"thinking\":\"let me think\"}}\n\n"))
			w.Write([]byte("event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"signature_delta\",\"signature\":\"sig123\"}}\n\n"))
			w.Write([]byte("event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\n"))
			if flusher != nil {
				flusher.Flush()
			}
			w.Write([]byte("event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":1,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n"))
			for i := 1; i <= 3; i++ {
				w.Write([]byte("event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":1,\"delta\":{\"type\":\"text_delta\",\"text\":\"hello\"}}\n\n"))
				w.Write([]byte(fmt.Sprintf("event: message_delta\ndata: {\"type\":\"message_delta\",\"usage\":{\"output_tokens\":%d}}\n\n", i*10)))
				if flusher != nil {
					flusher.Flush()
				}
				time.Sleep(50 * time.Millisecond)
			}
			w.Write([]byte("event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":1}\n\n"))
			w.Write([]byte("event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"))
			if flusher != nil {
				flusher.Flush()
			}
			return
		default: // 429
			w.Header().Set("Retry-After", "1")
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(429)
			w.Write([]byte(`{"error":{"type":"rate_limit_error","message":"mock 429"}}`))
			return
		}
	})
	log.Println("mock 服务器启动: 监听 127.0.0.1:9099 (?mode=429|bodyerr|ok|think，或路径前缀 /ok/ /think/ /bodyerr/)")
	log.Fatal(http.ListenAndServe("127.0.0.1:9099", nil))
}

// topKeys streams the top-level object's key order via Decoder (preserving appearance order), to verify rewrites don't reorder.
func topKeys(body []byte) []string {
	dec := json.NewDecoder(bytes.NewReader(body))
	if t, err := dec.Token(); err != nil || t != json.Delim('{') {
		return nil
	}
	var keys []string
	for dec.More() {
		tk, err := dec.Token()
		if err != nil {
			return keys
		}
		k, ok := tk.(string)
		if !ok {
			return keys
		}
		keys = append(keys, k)
		var v json.RawMessage
		if err := dec.Decode(&v); err != nil {
			return keys
		}
	}
	return keys
}
