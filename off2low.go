package main

// off2low.go: the Anthropic native-port half of convertOff2Low ("all"). An explicit downstream thinking-off is quietly
// sent upstream as low thinking (maybeUpgradeOffToLow), and the response's thinking blocks are stripped on the way back
// (thinkingStripper) so the client stays unaware — it asked for thinking off and sees a thinking-off response.
// The translation-port half lives in responses.go (responsesToAnthropicTriple + the anthToRespStream/translatingWriter strip wiring).

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
)

// lowThinkingBudget computes budget_tokens for a quiet low upgrade: 2048 capped at max_tokens/2;
// ok=false when even the 1024 floor doesn't fit (the caller keeps thinking off). maxTokens<=0
// (field absent or unparseable) means no ceiling. Shared by the translation port (upgradeNoneToLow)
// and the native port (maybeUpgradeOffToLow) so both ports pick the same shape.
func lowThinkingBudget(maxTokens int64) (budget int64, ok bool) {
	b := effortToThinkingBudget("low")
	if maxTokens > 0 {
		if ceiling := maxTokens / 2; b > ceiling {
			b = ceiling
		}
	}
	if b < 1024 {
		return 0, false
	}
	return b, true
}

// maybeUpgradeOffToLow rewrites an explicit downstream thinking-off into low thinking (native port, convertOff2Low:"all").
// Detection: the thinking value contains "disabled", or thinking is absent and reasoning_effort carries an explicit off
// word (none/off/disabled). A request carrying no explicit off signal passes through untouched.
// Shape: adaptive models (usesAdaptiveThinking) get thinking:{"type":"adaptive"}+output_config:{"effort":"low"};
// budget models get thinking:{"type":"enabled","budget_tokens":N} from lowThinkingBudget — when the 1024 floor doesn't
// fit, the upgrade is abandoned (abandoned=true, body untouched, the caller logs and stays off).
// On upgrade: reasoning deleted, reasoning_effort set to "low" (set or appended — belt-and-braces for
// OpenAI-vocabulary upstreams, mirroring the classifier rewrite's "none"), temperature/top_p deleted
// (thinking-on forbids sampling knobs, same rule as the translation port). All edits go through
// setTopLevelJSONValue: positional, every other field's bytes and order preserved.
// Returns (newBody, upgraded, abandoned); newBody is the original body when upgraded=false.
func maybeUpgradeOffToLow(body []byte, effModel string) (newBody []byte, upgraded, abandoned bool) {
	spans, ok := locateTopFields(body)
	if !ok {
		return body, false, false
	}
	var thinkVal, effortVal []byte
	var maxTokens int64
	for _, s := range spans {
		switch s.name {
		case "thinking":
			thinkVal = body[s.valStart:s.valEnd]
		case "reasoning_effort":
			effortVal = body[s.valStart:s.valEnd]
		case "max_tokens":
			mt, _ := strconv.ParseInt(strings.TrimSpace(string(body[s.valStart:s.valEnd])), 10, 64)
			maxTokens = mt // Unparseable = no ceiling (the request is broken anyway; the upstream will complain)
		}
	}
	// Explicit-off detection: thinking disabled, or (thinking absent) reasoning_effort carrying an off word.
	explicitOff := false
	if len(thinkVal) > 0 {
		explicitOff = bytes.Contains(thinkVal, []byte(`"disabled"`))
	} else if len(effortVal) > 0 {
		explicitOff = reasoningExplicitlyDisabled(strings.Trim(string(effortVal), `"`))
	}
	if !explicitOff {
		return body, false, false
	}
	var thinkingJSON, outputConfigJSON []byte
	if usesAdaptiveThinking(effModel) {
		thinkingJSON = []byte(`{"type":"adaptive"}`)
		outputConfigJSON = []byte(`{"effort":"low"}`)
	} else {
		b, ok := lowThinkingBudget(maxTokens)
		if !ok {
			return body, false, true
		}
		thinkingJSON = []byte(fmt.Sprintf(`{"type":"enabled","budget_tokens":%d}`, b))
	}
	nb, ok := setTopLevelJSONValue(body, "thinking", thinkingJSON)
	if !ok {
		return body, false, false
	}
	if outputConfigJSON != nil {
		nb, _ = setTopLevelJSONValue(nb, "output_config", outputConfigJSON)
	} else {
		nb, _ = setTopLevelJSONValue(nb, "output_config", nil) // Budget shape: a stale output_config would contradict the budget form
	}
	nb, _ = setTopLevelJSONValue(nb, "reasoning_effort", []byte(`"low"`))
	nb, _ = setTopLevelJSONValue(nb, "reasoning", nil)   // Responses-vocabulary field: gone
	nb, _ = setTopLevelJSONValue(nb, "temperature", nil) // Thinking-on forbids sampling knobs upstream-side
	nb, _ = setTopLevelJSONValue(nb, "top_p", nil)
	return nb, true, false
}

// thinkingStripper removes thinking/redacted_thinking blocks from an Anthropic response (the response half of a
// convertOff2Low off->low upgrade: the upstream thought at low; the client asked for thinking off and must see exactly that).
// Two modes, picked from the response Content-Type:
//   - SSE (text/event-stream): line-wise. content_block_start/delta/stop events of thinking blocks are dropped (their
//     thinking_delta/signature_delta vanish with them); kept blocks get their index renumbered gapless from 0 so the
//     client sees a well-formed stream. All other lines (event: lines, message_*, ping, usage) pass through untouched —
//     until the first drop happens, the whole stream is byte-exact; afterwards only the renumbered content_block_* data
//     lines are re-marshaled (their key order inside that one event may change; semantics unchanged).
//   - JSON (anything else): the whole body is buffered and stripped at flush (content[] filtered); malformed JSON
//     passes through byte-identical.
type thinkingStripper struct {
	sse      bool
	pending  []byte       // SSE: an unterminated partial line carried over to the next chunk
	dropped  map[int]bool // SSE: indexes of stripped thinking blocks
	renum    map[int]int  // SSE: kept blocks, old index -> new index
	nextNew  int          // SSE: next new index to assign
	buf      []byte       // JSON: whole-body accumulation
	stripped int          // Thinking blocks removed (for the log line at stream end)
}

// newThinkingStripper picks the mode from the response Content-Type.
func newThinkingStripper(contentType string) *thinkingStripper {
	return &thinkingStripper{
		sse:     strings.Contains(contentType, "text/event-stream"),
		dropped: map[int]bool{},
		renum:   map[int]int{},
	}
}

// feed consumes one chunk and returns the bytes safe to write downstream right now (nil = fully buffered for now).
func (s *thinkingStripper) feed(data []byte) []byte {
	if !s.sse {
		s.buf = append(s.buf, data...)
		return nil
	}
	chunk := make([]byte, 0, len(s.pending)+len(data))
	chunk = append(chunk, s.pending...)
	chunk = append(chunk, data...)
	s.pending = nil
	last := bytes.LastIndexByte(chunk, '\n')
	if last < 0 {
		s.pending = chunk
		return nil
	}
	if tail := chunk[last+1:]; len(tail) > 0 {
		s.pending = append([]byte(nil), tail...)
	}
	var out bytes.Buffer
	full := chunk[:last+1] // Complete lines only, newline included
	for len(full) > 0 {
		nl := bytes.IndexByte(full, '\n')
		if kept := s.feedLine(full[:nl+1]); kept != nil {
			out.Write(kept)
		}
		full = full[nl+1:]
	}
	return out.Bytes()
}

// feedLine processes one complete SSE line (newline included); nil return = the line is dropped.
func (s *thinkingStripper) feedLine(line []byte) []byte {
	// Cheap filter: only data: lines of the three content_block_* event types can carry a block index worth rewriting.
	if !bytes.HasPrefix(line, []byte("data:")) || !bytes.Contains(line, []byte("content_block_")) {
		return line
	}
	var ev struct {
		Type         string `json:"type"`
		Index        *int   `json:"index"`
		ContentBlock struct {
			Type string `json:"type"`
		} `json:"content_block"`
	}
	if err := json.Unmarshal(bytes.TrimSpace(line[len("data:"):]), &ev); err != nil {
		return line // Unparseable data line: pass through untouched
	}
	switch ev.Type {
	case "content_block_start":
		if ev.Index == nil {
			return line
		}
		idx := *ev.Index
		if ev.ContentBlock.Type == "thinking" || ev.ContentBlock.Type == "redacted_thinking" {
			s.dropped[idx] = true
			s.stripped++
			return nil // The block's start vanishes; its delta/stop lines drop on the dropped mark
		}
		s.renum[idx] = s.nextNew
		s.nextNew++
		if s.renum[idx] == idx {
			return line // Index unchanged (nothing dropped before it): byte-exact
		}
		return rewriteBlockIndex(line, s.renum[idx])
	case "content_block_delta", "content_block_stop":
		if ev.Index == nil {
			return line
		}
		idx := *ev.Index
		if s.dropped[idx] {
			return nil
		}
		if newIdx, ok := s.renum[idx]; ok && newIdx != idx {
			return rewriteBlockIndex(line, newIdx)
		}
		return line
	}
	return line
}

// rewriteBlockIndex returns the data: line with its top-level "index" field set to newIdx (re-marshaled — only ever
// called on a line whose JSON just parsed). Returns the original line on any surprise.
func rewriteBlockIndex(line []byte, newIdx int) []byte {
	hasNL := bytes.HasSuffix(line, []byte("\n"))
	trimmed := bytes.TrimRight(line, "\r\n")
	var obj map[string]interface{}
	if err := json.Unmarshal(trimmed[len("data:"):], &obj); err != nil {
		return line
	}
	obj["index"] = newIdx
	nb, err := json.Marshal(obj)
	if err != nil {
		return line
	}
	out := make([]byte, 0, len(nb)+8)
	out = append(out, []byte("data: ")...)
	out = append(out, nb...)
	if hasNL {
		out = append(out, '\n')
	}
	return out
}

// flush returns what the stripper still holds at stream end: SSE mode yields the unterminated tail as-is;
// JSON mode strips the buffered document (malformed JSON passes through byte-identical).
func (s *thinkingStripper) flush() []byte {
	if s.sse {
		return s.pending
	}
	if len(s.buf) == 0 {
		return nil
	}
	var obj map[string]interface{}
	if err := json.Unmarshal(s.buf, &obj); err != nil {
		return s.buf
	}
	arr, ok := obj["content"].([]interface{})
	if !ok {
		return s.buf
	}
	kept := make([]interface{}, 0, len(arr))
	for _, b := range arr {
		if m, ok := b.(map[string]interface{}); ok {
			if t, _ := m["type"].(string); t == "thinking" || t == "redacted_thinking" {
				s.stripped++
				continue
			}
		}
		kept = append(kept, b)
	}
	if s.stripped == 0 {
		return s.buf // Nothing stripped: byte-exact passthrough
	}
	obj["content"] = kept
	nb, err := json.Marshal(obj)
	if err != nil {
		return s.buf
	}
	return nb
}
