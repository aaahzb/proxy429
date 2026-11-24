package proxy

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log"
	"math"
	"math/rand"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"proxy429/internal/config"
	"proxy429/internal/jsonutil"
	"proxy429/internal/recorder"
	"proxy429/internal/translate"
)

// shouldRetry reports whether the given status code is on the retry list.
func shouldRetry(code int) bool {
	c := config.Current()
	for _, rc := range c.RetryStatusCodes {
		if rc == code {
			return true
		}
	}
	return false
}

// computeBackoff computes the wait before this retry:
// prefer the upstream's Retry-After header (seconds), otherwise exponential backoff + jitter.
func computeBackoff(resp *http.Response, attempt int) time.Duration {
	c := config.Current()
	if c.RespectRetryAfter && resp != nil {
		if ra := resp.Header.Get("Retry-After"); ra != "" {
			if secs, err := strconv.Atoi(ra); err == nil {
				return time.Duration(secs) * time.Second
			}
		}
	}
	backoffSec := c.BaseDelaySec * math.Pow(2, float64(attempt))
	if backoffSec > c.MaxDelaySec {
		backoffSec = c.MaxDelaySec
	}
	// Add jitter so simultaneous retries don't thundering-herd.
	jitter := backoffSec * 0.3 * rand.Float64()
	return time.Duration((backoffSec + jitter) * float64(time.Second))
}

// copyHeaders copies src's headers to dst, skipping hop-by-hop headers and Content-Length.
// Content-Length must be skipped: rewriting the request body changes the length; let Go recompute it from the body.
func copyHeaders(dst, src http.Header) {
	for k, vv := range src {
		if k == "Connection" || k == "Keep-Alive" ||
			k == "Transfer-Encoding" || k == "Upgrade" ||
			k == "Content-Length" {
			continue
		}
		for _, v := range vv {
			dst.Add(k, v)
		}
	}
}

// peekHead reads the start of the response body: until the first SSE event ends (blank line), maxBytes are read,
// or EOF. Returns the bytes read (they have been consumed from br and must be put back when forwarding).
func peekHead(br *bufio.Reader, maxBytes int) []byte {
	var buf bytes.Buffer
	for buf.Len() < maxBytes {
		line, err := br.ReadBytes('\n')
		buf.Write(line)
		if err != nil {
			break // EOF or error: return what was read
		}
		// SSE events are separated by blank lines: content seen + a blank line means the first event is complete.
		if len(bytes.TrimRight(line, "\r\n")) == 0 && buf.Len() > 2 {
			break
		}
	}
	return buf.Bytes()
}

// headHasError checks whether the response head is an error event (rate limiting etc.).
// The error event of Responses passthrough streams is response.failed (the type field in the SSE data); recognized as well.
func headHasError(head []byte) bool {
	s := strings.ToLower(string(head))
	return strings.Contains(s, "event: error") ||
		strings.Contains(s, "event:error") ||
		strings.Contains(s, `"type":"error"`) ||
		strings.Contains(s, `"type": "error"`) ||
		strings.Contains(s, "response.failed") ||
		strings.Contains(s, "rate_limit") ||
		strings.Contains(s, "overloaded") ||
		strings.Contains(s, "too many requests")
}

// firstLine takes the first line of a byte stream (truncated to 200 chars), for logging.
func firstLine(b []byte) string {
	s := string(b)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	if len(s) > 200 {
		s = s[:200]
	}
	return strings.TrimSpace(s)
}

// truncate collapses a string to one line and cuts it to n chars, for logging.
func truncate(s string, n int) string {
	s = strings.ReplaceAll(s, "\n", " ")
	if len(s) > n {
		return s[:n] + "..."
	}
	return s
}

// writeSSEPing writes an Anthropic-standard SSE ping event to the client and flushes, keeping the connection alive during 429 retries.
// Claude Code ignores ping events (Anthropic's official streams intersperse pings too); no message content is produced.
// Responses passthrough streams get an SSE comment line (": ping") instead — every SSE client ignores comment lines,
// whereas feeding an Anthropic-shaped ping JSON to a native Responses client could trigger parse warnings.
// w: response writer; flusher: flush after write when non-nil; f: this flight (for protocol shape, may be nil).
func writeSSEPing(w http.ResponseWriter, flusher http.Flusher, f *recorder.Flight) {
	if f != nil && f.ResponsesRaw() {
		w.Write([]byte(": ping\n\n"))
	} else {
		w.Write([]byte("event: ping\ndata: {\"type\":\"ping\"}\n\n"))
	}
	if flusher != nil {
		flusher.Flush()
	}
}

// writeSSEError is the upstream-failure fallback after a 200 header has been sent: it writes an Anthropic SSE error event and flushes.
// Once the 200 header is sent, the status code can no longer pass a 429 through; an SSE error is the only way for the client to recognize the failure (used when retries run out).
// Responses passthrough streams get a response.failed event instead (native Responses clients recognize that shape).
// w: response writer; flusher: flush after write when non-nil; msg: error description; f: this flight (a copy is teed for web review).
func writeSSEError(w http.ResponseWriter, flusher http.Flusher, msg string, f *recorder.Flight) {
	var buf bytes.Buffer
	b, _ := json.Marshal(msg) // JSON-encode the message so special characters don't break the SSE
	if f != nil && f.ResponsesRaw() {
		buf.WriteString("event: response.failed\ndata: {\"type\":\"response.failed\",\"response\":{\"id\":\"resp_p429\",\"object\":\"response\",\"status\":\"failed\",\"error\":{\"code\":\"server_error\",\"message\":")
		buf.Write(b)
		buf.WriteString("}}}\n\n")
	} else {
		buf.WriteString("event: error\ndata: {\"type\":\"error\",\"error\":{\"type\":\"overloaded_error\",\"message\":")
		buf.Write(b)
		buf.WriteString("}}\n\n")
	}
	data := buf.Bytes()
	w.Write(data)
	if flusher != nil {
		flusher.Flush()
	}
	if f != nil {
		f.GaveUp = true       // The finished-streams status column shows [重试尽] from this flag (distinguishing it from other status=0 shapes)
		f.AppendContent(data) // Tee a copy for web review (the retries-exhausted error is recorded too)
	}
}

// startSSEKeepalive sends 200 + SSE headers + the first ping to the client on the first retry, starting the keepalive stream.
// Pings keep flowing during subsequent retries so Claude Code doesn't time out on the long silence and report an API error.
// Returns the flusher for later pings/forwarding. f: this flight (id for logging); reason: retry reason (logging).
func startSSEKeepalive(w http.ResponseWriter, f *recorder.Flight, reason string) http.Flusher {
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)
	flusher, _ := w.(http.Flusher)
	writeSSEPing(w, flusher, f)
	log.Printf("[keepalive] #%d retrying (%s); SSE ping keepalive on", f.ID, reason)
	return flusher
}

// sleepWithPing pings periodically to keep alive during the backoff wait, and can be interrupted by a client disconnect (ctx cancel).
// Replaces the original time.Sleep(wait): that one was uninterruptible — after a client timeout-disconnect the proxy kept sleeping and retrying, burning upstream quota for nothing.
// ctx: the client connection's context; w/flusher: for pings; wait: backoff duration; interval: ping interval; f: flight.
// A nil flusher means wait silently without pings: under convertAlltoStream the client expects a non-streaming response,
// so writing any SSE byte early would pollute the response; all we can do is wait (the client connection stays up, just dataless).
// Returning false means ctx was cancelled (client disconnected): stop retrying and end.
func sleepWithPing(ctx context.Context, w http.ResponseWriter, flusher http.Flusher, wait, interval time.Duration, f *recorder.Flight) bool {
	timer := time.NewTimer(wait)
	defer timer.Stop()
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			log.Printf("[keepalive] #%d client disconnected; stopping retries", f.ID)
			return false
		case <-ticker.C:
			if flusher != nil {
				writeSSEPing(w, flusher, f)
			}
		case <-timer.C:
			return true
		}
	}
}

// forward passes the response through to the client: first puts back the peeked head, then streams the rest.
// While forwarding, it parses the SSE usage fields line by line, accumulating into the global stats for the status row;
// the forwarded bytes are simultaneously added to flight.bytes for the upper-half icon display, and phase=1 is marked.
// Returns this stream's final output_tokens, for the handler's token/s sample.
func forward(w http.ResponseWriter, resp *http.Response, head []byte, br *bufio.Reader, f *recorder.Flight, headersSent bool) int64 {
	defer resp.Body.Close()
	// convertOff2Low native-port upgrade (off->low): strip thinking blocks from the response so the client keeps seeing the
	// thinking-off shape it asked for. Only on a 200 (error bodies pass through untouched), never on Responses passthrough.
	var stripper *translate.ThinkingStripper
	if f.StripThinking.Load() && !f.ResponsesRaw() && resp.StatusCode == http.StatusOK {
		stripper = translate.NewThinkingStripper(resp.Header.Get("Content-Type"))
	}

	// SSE gate for the no-stop badge: only streamed responses carry terminal events (plain JSON has none by nature).
	sseStream := strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream")
	// noBullshitOnEmptyClaudeSearch is deliberately limited to native Anthropic streaming. The writer holds client-bound bytes,
	// while the normal loop below continues to parse and record the exact upstream response.
	var guard *searchGuardWriter
	if f.SearchGuardArmed && f.Translated == "" && sseStream && resp.StatusCode == http.StatusOK {
		guard = newSearchGuardWriter(w, f, headersSent)
		w = guard
	}
	if !headersSent {
		copyHeaders(w.Header(), resp.Header)
		if stripper != nil && !stripper.SSE {
			// JSON-mode stripping changes the body length: drop the upstream's Content-Length, let net/http re-frame.
			w.Header().Del("Content-Length")
		}
		w.WriteHeader(resp.StatusCode)
	}

	flusher, canFlush := w.(http.Flusher)

	// Response received, forwarding starts: the icon switches from "awaiting first byte (elapsed)" to "forwarding (bytes)".
	f.Phase.Store(1)
	f.Status = resp.StatusCode
	f.SetStage(recorder.StageForward) // The web 「状态」 column switches to showing the status code

	// Counted into active streams; the exit defer subtracts one.
	recorder.Stats.Mu.Lock()
	recorder.Stats.Active++
	recorder.Stats.Mu.Unlock()

	var lastOutput int64                          // This stream's cumulative output_tokens, for computing deltas
	var lastInput int64                           // This stream's cumulative input_tokens
	var lastCacheRead int64                       // This stream's cumulative cache_read
	var lastCacheCreation int64                   // This stream's cumulative cache_creation
	var sawDeltaUsage bool                        // A message_delta with usage has been seen (the real usage breakdown has arrived)
	var sawStop bool                              // Terminal event seen (message_stop / response.completed|failed|incomplete); a 200 SSE stream ending without it gets the noStop badge
	interrupted := false                          // Whether this stream broke midway (usage rolled back, not counted in the aggregation)
	openTools := map[int]*recorder.OpenToolCall{} // Anthropic: block index → in-progress tool call (for the *0 empty-args decision, survives across chunks)
	respTools := map[string]string{}              // Responses passthrough: item_id → tool name (aligns with output_item.done for the empty-args decision)
	defer func() {
		counted := false
		if lastInput == 0 && lastCacheRead == 0 && lastCacheCreation == 0 && lastOutput == 0 {
			// Non-streaming JSON response: SSE yields no usage, so extract it from the body (classifier and similar requests)
			if in, cr, cc, out, ok := recorder.ParseNonStreamUsage(f.SnapshotContent()); ok {
				f.InTokens = in
				f.CacheRead = cr
				f.CacheCreation = cc
				f.OutTokens = out
				recorder.Stats.Mu.Lock()
				recorder.Stats.InputTokens += in
				recorder.Stats.CacheRead += cr
				recorder.Stats.CacheCreation += cc
				recorder.Stats.OutputTokens += out
				recorder.Stats.Mu.Unlock()
				counted = true
			}
		} else if sawDeltaUsage {
			// Stream completed normally (real breakdown obtained): count into the aggregation.
			f.InTokens = lastInput
			f.CacheRead = lastCacheRead
			f.CacheCreation = lastCacheCreation
			f.OutTokens = lastOutput
			counted = true
		} else {
			// Stream interrupted: only message_start's estimated usage has been counted globally; roll it back wholesale —
			// the aggregation counts only complete responses (consistent with Claude Code). f.* zeroed; the per-stream hit rate shows "-".
			recorder.RollbackUsageStats(lastInput, lastCacheRead, lastCacheCreation, lastOutput)
			f.InTokens, f.CacheRead, f.CacheCreation, f.OutTokens = 0, 0, 0, 0
			interrupted = true
		}
		// Real upstream model: prefer the model name the upstream response actually returned (the upstream may route the request onward to another model);
		// when the upstream returned no model, fall back to the route target targetModel, or origModel for passthrough.
		realModel := f.UpstreamModel
		if realModel == "" {
			realModel = f.TargetModel
		}
		if realModel == "" {
			realModel = f.OrigModel
		}
		if counted {
			recorder.Stats.AddModelUsage(realModel, f.InTokens, f.CacheRead, f.CacheCreation, f.OutTokens)
		}
		if interrupted {
			log.Printf("[interrupted] #%d response status %d size %s (stream incomplete; usage excluded from aggregates)", f.ID, f.Status, recorder.HumanBytes(f.Bytes.Load()))
		} else {
			log.Printf("[done] #%d response status %d size %s cache-hit %s", f.ID, f.Status, recorder.HumanBytes(f.Bytes.Load()), recorder.CacheHitRate(f.CacheRead, f.InTokens, f.CacheCreation))
		}
		recorder.Stats.Mu.Lock()
		recorder.Stats.Active--
		recorder.Stats.Mu.Unlock()
	}()
	// writeAndCount forwards a chunk of bytes and parses its data: lines to update stats.
	// Order: tee the exact upstream bytes to the web upstream side (the full truth, before model write-back and stripping) →
	// rewrite the model back to the client's original (route transparency) → strip thinking blocks (convertOff2Low) →
	// write the client-bound bytes downstream → byte counts post-strip → stats parse the original upstream bytes.
	// The downstream side gets its own tee whenever the client-bound bytes can differ from the upstream side — the route rewrote
	// the model (response model names written back below) and/or the stripper is active — except when w is a translatingWriter:
	// the translation port tees its own downstream side (the translated client-bound bytes) via its tap.
	_, downstreamTapped := w.(interface{ SetDownTap(translate.FlightTap) })
	teeDown := !downstreamTapped && (guard != nil || stripper != nil || (f.TargetModel != "" && f.TargetModel != f.OrigModel))
	writeAndCount := func(data []byte) {
		if len(data) == 0 {
			return
		}
		if guard != nil {
			guard.observeUpstream(data) // Full-fidelity detector input, independent of the capped web preview.
		}
		f.AppendContent(data) // Tee a copy for the web upstream side (the exact upstream bytes, before model write-back and stripping)
		out := data
		if f.TargetModel != "" && f.TargetModel != f.OrigModel {
			lines := bytes.Split(data, []byte("\n"))
			modified := false
			var actualModel string
			for i, line := range lines {
				newLine, am, mod := rewriteResponseModel(line, f.OrigModel)
				if mod {
					modified = true
					if actualModel == "" {
						actualModel = am
					}
				}
				lines[i] = newLine
			}
			if modified {
				out = bytes.Join(lines, []byte("\n"))
				if f.UpstreamModel == "" && actualModel != "" {
					f.UpstreamModel = actualModel
				}
				if !f.ModelLogged.Swap(true) {
					log.Printf("[rewrite] #%d response stream model rewritten back %s -> %s", f.ID, actualModel, f.OrigModel)
				}
			}
		}
		if stripper != nil {
			out = stripper.Feed(out) // convertOff2Low: thinking blocks dropped, kept block indexes renumbered (the client sees thinking-off)
		}
		if len(out) > 0 {
			w.Write(out)
			if canFlush {
				flusher.Flush()
			}
			n := int64(len(out))
			f.Bytes.Add(n) // Per-stream bytes, for the icon display (post-strip: what the client actually received)
			if teeDown {
				f.AppendContentDown(out) // The client-bound bytes can differ from the upstream side (model written back and/or thinking stripped): the downstream side gets its own tee
			}
		}
		responsesRaw := f.ResponsesRaw() // Computed once outside the loop: passthrough streams parse by Responses semantics
		for _, line := range bytes.Split(data, []byte("\n")) {
			if responsesRaw {
				recorder.ParseResponsesStreamStats(line, &lastOutput, &lastInput, &lastCacheRead, &lastCacheCreation, &sawDeltaUsage)
				if itemID, name, ok := recorder.ParseResponsesToolStart(line); ok {
					f.NoteToolCall(name) // Tool-call counting: the finished-streams list shows tags like [exec_command*1]
					respTools[itemID] = name
				}
				if itemID, empty, ok := recorder.ParseResponsesToolDone(line); ok {
					if name := respTools[itemID]; name != "" {
						if empty {
							f.NoteToolCallEmpty(name) // Arguments object empty: a single call's tag shows *0
						}
						delete(respTools, itemID)
					}
				}
				// response.completed/failed/incomplete = Responses-protocol terminal markers (the counterpart of Anthropic's
				// message_stop; same semantics as below: mark delivered as soon as it completes, don't wait for a clean EOF, so Codex's quick disconnect isn't misrecorded as 499).
				if bytes.HasPrefix(bytes.TrimSpace(line), []byte("data:")) &&
					(bytes.Contains(line, []byte("response.completed")) || bytes.Contains(line, []byte("response.failed")) || bytes.Contains(line, []byte("response.incomplete"))) {
					sawStop = true
					f.Delivered.Store(true)
				}
				continue
			}
			recorder.ParseSSEStats(line, &lastOutput, &lastInput, &lastCacheRead, &lastCacheCreation, &sawDeltaUsage)
			if ts, ok := recorder.ParseToolCallStart(line); ok {
				f.NoteToolCall(ts.Name) // Tool-call counting: the finished-streams list shows [Read*1][Edit*3]
				tr := &recorder.OpenToolCall{Name: ts.Name}
				if !recorder.IsEmptyArgsJSON(string(ts.Input)) {
					tr.HasArgs = true // A server_tool_use's start is already the complete block: a non-empty input settles it immediately
				}
				openTools[ts.Index] = tr
			}
			if idx, partial, ok := recorder.ParseToolArgsDelta(line); ok {
				if tr := openTools[idx]; tr != nil {
					tr.NotePartial(partial) // Accumulate argument fragments (≤64B); empty-args decided at stop
				}
			}
			if idx, ok := recorder.ParseToolBlockStop(line); ok {
				if tr := openTools[idx]; tr != nil {
					if tr.ArgsEmpty() {
						f.NoteToolCallEmpty(tr.Name)
					}
					delete(openTools, idx)
				}
			}
			// message_stop = the stream is semantically fully sent; mark delivered immediately: clients often disconnect right after completion
			// (Codex closes the connection as soon as response.completed arrives); once ctx is cancelled the upstream EOF becomes unreadable,
			// and waiting for a clean EOF to mark would be too late (normal translation-port streams were once misrecorded as 499).
			if bytes.HasPrefix(bytes.TrimSpace(line), []byte("data:")) && bytes.Contains(line, []byte("message_stop")) {
				sawStop = true
				f.Delivered.Store(true)
			}
		}
	}

	writeAndCount(head)
	for {
		line, err := br.ReadBytes('\n')
		if len(line) > 0 {
			writeAndCount(line)
		}
		if err != nil {
			if err == io.EOF {
				// A cleanly exhausted upstream stream means the last chunk has also been written downstream; mark fully delivered
				// (the closing defer uses this to tell "disconnected after completion" from "disconnected midway"; only the latter is re-recorded as 499).
				f.Delivered.Store(true)
			}
			break
		}
	}
	// A 200 SSE stream that ended without any terminal event was cut off upstream-side: flag it for the status page's
	// red [无stop] badge. Client-side disconnects (Esc) cancel the upstream request context — excluded here; that case
	// is already re-recorded as 499 by the wrap-up defer.
	if sseStream && resp.StatusCode == http.StatusOK && !sawStop && (resp.Request == nil || resp.Request.Context().Err() == nil) {
		f.NoStop = true
	}
	if stripper != nil {
		// Flush what the stripper still holds: a partial trailing line (SSE), or the whole transformed document (JSON mode).
		if tail := stripper.Flush(); len(tail) > 0 {
			w.Write(tail)
			if canFlush {
				flusher.Flush()
			}
			f.Bytes.Add(int64(len(tail)))
			f.AppendContentDown(tail)
		}
		if stripper.Stripped > 0 {
			log.Printf("[off->low] #%d stripped %d thinking block(s) from the response (client sees thinking-off)", f.ID, stripper.Stripped)
		}
	}
	// Tool calls still open at stream end (missing content_block_stop, e.g. a truncated stream) get their empty-args verdict from what was received,
	// a best-effort *0 backfill; for normally ended streams openTools is already empty here.
	for _, tr := range openTools {
		if tr.ArgsEmpty() {
			f.NoteToolCallEmpty(tr.Name)
		}
	}
	if guard != nil {
		guard.finish(searchGuardUsage{input: lastInput, cacheRead: lastCacheRead, cacheCreation: lastCacheCreation, output: lastOutput})
	}
	return lastOutput
}

// collectStreamToJSON drains the SSE stream returned by the upstream, rebuilds an Anthropic non-streaming message JSON as-is,
// and writes it to the client in one shot. Under convertAlltoStream the request was sent upstream as a stream, but the client still expects
// non-streaming JSON, so the stream must be fully buffered first (the web in-flight view stays live: every chunk is teed to the flight).
// Rebuild rules (streaming is an event stream, non-streaming is whole blocks; the structures differ and need reassembly):
//   - message_start's message object is kept as the skeleton (id/type/role/model etc.);
//   - each content_block_start's block skeleton is kept by index — web_search_tool_result (with
//     encrypted_content), server_tool_use and similar wholly inlined fields stay untouched;
//   - delta events only append to known fields: text_delta->text, thinking_delta->thinking,
//     signature_delta->signature, input_json_delta->input (tool_use arguments, parsed into an object once complete);
//   - usage merge: message_start as the base, message_delta overwriting (last-value semantics, consistent with the stats logic).
//
// w: client response writer; resp/head/br: the upstream response (head is what peekHead already read, handled first);
// f: this flight. Returns (whether message_stop was fully received, output token count).
// On an incomplete stream nothing is written to the client, so the caller can safely retry the whole thing (resend the streaming request, drain again).
func collectStreamToJSON(w http.ResponseWriter, resp *http.Response, head []byte, br *bufio.Reader, f *recorder.Flight) (bool, int64) {
	defer resp.Body.Close()
	f.Phase.Store(1)
	f.Status = resp.StatusCode
	f.SetStage(recorder.StageForward)
	recorder.Stats.Mu.Lock()
	recorder.Stats.Active++
	recorder.Stats.Mu.Unlock()
	defer func() {
		recorder.Stats.Mu.Lock()
		recorder.Stats.Active--
		recorder.Stats.Mu.Unlock()
	}()

	var msgObj map[string]interface{}          // message_start's message skeleton
	var usageObj map[string]interface{}        // Merged usage: start as the base, delta overwriting
	blocks := map[int]map[string]interface{}{} // index -> content block (start skeleton + delta accumulation)
	var lastOutput, lastInput, lastCacheRead, lastCacheCreation int64
	complete := false

	// strVal takes a string field from a map (missing or non-string returns an empty string).
	strVal := func(m map[string]interface{}, k string) string {
		if m == nil {
			return ""
		}
		if v, ok := m[k].(string); ok {
			return v
		}
		return ""
	}

	// handleChunk processes a chunk of raw SSE bytes (possibly multiple lines): tees each line to the web and stats, and parses the event structure.
	handleChunk := func(chunk []byte) {
		for _, line := range bytes.SplitAfter(chunk, []byte("\n")) {
			if len(line) == 0 {
				continue
			}
			f.Bytes.Add(int64(len(line)))
			f.AppendContent(line)
			recorder.ParseSSEStats(line, &lastOutput, &lastInput, &lastCacheRead, &lastCacheCreation, nil)

			s := bytes.TrimRight(line, "\r\n")
			if !bytes.HasPrefix(s, []byte("data:")) {
				continue
			}
			payload := bytes.TrimSpace(bytes.TrimPrefix(s, []byte("data:")))
			if len(payload) == 0 || bytes.Equal(payload, []byte("[DONE]")) {
				continue
			}
			var ev map[string]interface{}
			if json.Unmarshal(payload, &ev) != nil {
				continue
			}
			switch ev["type"] {
			case "message_start":
				if msg, ok := ev["message"].(map[string]interface{}); ok {
					msgObj = msg
					if u, ok := msg["usage"].(map[string]interface{}); ok {
						usageObj = make(map[string]interface{}, len(u))
						for k, v := range u {
							usageObj[k] = v
						}
					}
					if m, ok := msg["model"].(string); ok && m != "" && f.UpstreamModel == "" {
						f.UpstreamModel = m
					}
				}
			case "content_block_start":
				if cb, ok := ev["content_block"].(map[string]interface{}); ok {
					blocks[int(jsonutil.ToInt64(ev["index"]))] = cb
					if t, _ := cb["type"].(string); t == "tool_use" || t == "server_tool_use" {
						f.NoteToolCall(strVal(cb, "name")) // Tool-call counting (the JSON-assembly path doesn't go through writeAndCount)
					}
				}
			case "content_block_delta":
				b := blocks[int(jsonutil.ToInt64(ev["index"]))]
				d, _ := ev["delta"].(map[string]interface{})
				if b == nil || d == nil {
					continue
				}
				switch d["type"] {
				case "text_delta":
					b["text"] = strVal(b, "text") + strVal(d, "text")
				case "thinking_delta":
					b["thinking"] = strVal(b, "thinking") + strVal(d, "thinking")
				case "signature_delta":
					b["signature"] = strVal(b, "signature") + strVal(d, "signature")
				case "input_json_delta":
					b["input"] = strVal(b, "input") + strVal(d, "partial_json")
				}
			case "message_delta":
				if d, ok := ev["delta"].(map[string]interface{}); ok && msgObj != nil {
					if sr, ok := d["stop_reason"].(string); ok {
						msgObj["stop_reason"] = sr
					}
					if ss, ok := d["stop_sequence"]; ok {
						msgObj["stop_sequence"] = ss
					}
				}
				if u, ok := ev["usage"].(map[string]interface{}); ok {
					if usageObj == nil {
						usageObj = make(map[string]interface{}, len(u))
					}
					for k, v := range u {
						usageObj[k] = v
					}
				}
			case "message_stop":
				complete = true
			}
		}
	}

	handleChunk(head)
	for {
		line, err := br.ReadBytes('\n')
		if len(line) > 0 {
			handleChunk(line)
		}
		if err != nil {
			break
		}
	}
	if !complete {
		// Incompletely received streams don't count in the aggregation (they only have message_start's estimated usage); rolled back, ready for a wholesale retry
		recorder.RollbackUsageStats(lastInput, lastCacheRead, lastCacheCreation, lastOutput)
		log.Printf("[streamify] #%d upstream stream incomplete (no message_stop); nothing written to client, whole request retried", f.ID)
		return false, lastOutput
	}

	// Rebuild the non-streaming message JSON. model is written back to the client's original model (route rewriting is transparent to the client).
	if msgObj == nil {
		// Abnormal streams missing message_start: build a minimal skeleton so the returned structure stays complete.
		msgObj = map[string]interface{}{"type": "message", "role": "assistant"}
	}

	// Blocks are backfilled into content sorted by index; a string input (assembled from input_json_deltas) is parsed into an object when possible.
	indexes := make([]int, 0, len(blocks))
	for i := range blocks {
		indexes = append(indexes, i)
	}
	sort.Ints(indexes)
	content := make([]interface{}, 0, len(indexes))
	for _, i := range indexes {
		b := blocks[i]
		// convertOff2Low upgrade: thinking blocks never reach the assembled response (the client asked for thinking-off)
		if f.StripThinking.Load() {
			if t, _ := b["type"].(string); t == "thinking" || t == "redacted_thinking" {
				continue
			}
		}
		if s, ok := b["input"].(string); ok && s != "" {
			var obj interface{}
			if json.Unmarshal([]byte(s), &obj) == nil {
				b["input"] = obj
			}
		}
		// A tool block's input is final at this point: judge emptiness and backfill the *0 tag (start only counted; empty-args looks at the final state).
		if t, _ := b["type"].(string); t == "tool_use" || t == "server_tool_use" {
			empty := false
			switch inp := b["input"].(type) {
			case nil:
				empty = true
			case string:
				empty = recorder.IsEmptyArgsJSON(inp)
			case map[string]interface{}:
				empty = len(inp) == 0
			}
			if empty {
				f.NoteToolCallEmpty(strVal(b, "name"))
			}
		}
		content = append(content, b)
	}
	msgObj["content"] = content
	if usageObj != nil {
		msgObj["usage"] = usageObj
	}
	// model is written back to the client's original model (route rewriting is transparent to the client).
	if f.TargetModel != "" && f.TargetModel != f.OrigModel {
		msgObj["model"] = f.OrigModel
		if f.UpstreamModel != "" && !f.ModelLogged.Swap(true) {
			log.Printf("[rewrite] #%d response stream model rewritten back %s -> %s", f.ID, f.UpstreamModel, f.OrigModel)
		}
	}
	out, err := json.Marshal(msgObj)
	if err != nil {
		return false, lastOutput
	}

	// Write the complete response in one shot (the client expects non-streaming JSON; not a single byte may be written before it's complete).
	f.InTokens = lastInput
	f.CacheRead = lastCacheRead
	f.CacheCreation = lastCacheCreation
	f.OutTokens = lastOutput
	recorder.Stats.AddModelUsage(f.RealModel(), f.InTokens, f.CacheRead, f.CacheCreation, f.OutTokens)
	log.Printf("[done] #%d response status %d size %s cache-hit %s", f.ID, f.Status, recorder.HumanBytes(f.Bytes.Load()), recorder.CacheHitRate(f.CacheRead, f.InTokens, f.CacheCreation))

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	w.Write(out)
	if flusher, ok := w.(http.Flusher); ok {
		flusher.Flush()
	}
	f.Bytes.Add(int64(len(out)))
	f.AppendContentDown(out) // Tee the rebuilt non-streaming JSON to the downstream side (the actual proxy→client shape; the upstream-side SSE lines are teed line by line into content by handleChunk)
	// The assembled JSON has been written in one shot = fully delivered (499 re-marking only looks at incompletely sent streams)
	f.Delivered.Store(true)
	log.Printf("[streamify] #%d stream complete; non-streaming JSON returned to client in one piece (%d output tokens)", f.ID, lastOutput)
	return true, lastOutput
}

// handler is the entry point of all requests: buffer the request body → turn off thinking on classifier hits →
// forward with retries → stream the response through.
// Three retry cases:
//
//	A. the upstream's HTTP status code itself is 429/5xx;
//	B. status 200, but the error hides in the SSE response body (event:error / rate_limit etc.);
//	C. normal response, passed straight through.
//
// countTokensPath is Anthropic's token-counting endpoint: Claude Code calls it periodically for context-length estimation;
// the response is just {"input_tokens":N}, generating no content. These streams are forwarded upstream as usual, merely tagged so the web can tell them apart.
const countTokensPath = "/v1/messages/count_tokens"
