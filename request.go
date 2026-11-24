package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"sort"
	"strings"
	"time"

	"proxy429/internal/config"
	"proxy429/internal/jsonutil"
	"proxy429/internal/recorder"
	"proxy429/internal/translate"
)

// classifierSystemPrefix is the fixed system-field prefix of Claude Code classifier (safety check) requests.
// The proxy uses it to recognize classifier requests, route them to a cheap model, and disable thinking. Hardcoded: a client implementation detail that users shouldn't configure.
const classifierSystemPrefix = "You are a security monitor"

// codexGuardianSystemPrefix is the fixed system-field prefix of Codex's guardian (approval-assessment) requests.
// Codex sends it as the Responses API's "instructions" field; the Responses->Anthropic translation maps it to system, so the same prefix-match mechanism recognizes it. Hardcoded: a client implementation detail that users shouldn't configure.
const codexGuardianSystemPrefix = "You are judging one planned coding-agent action."

// classifierSystemMatches reports whether the system field (raw bytes) starts with the given prefix.
// system may be a string or an Anthropic-style [{type,text}] array.
func classifierSystemMatches(body []byte, spans []jsonutil.FieldSpan, prefix string) bool {
	for _, s := range spans {
		if s.Name != "system" {
			continue
		}
		sysRaw := body[s.ValStart:s.ValEnd]
		var str string
		if err := json.Unmarshal(sysRaw, &str); err == nil {
			return strings.HasPrefix(str, prefix)
		}
		var arr []map[string]interface{}
		if err := json.Unmarshal(sysRaw, &arr); err == nil {
			for _, m := range arr {
				if t, _ := m["text"].(string); strings.HasPrefix(t, prefix) {
					return true
				}
			}
		}
		return false
	}
	return false
}

// fieldDeleteRange computes the byte span for deleting a field (key, colon, value included), handling the surrounding commas correctly:
// if a comma follows the field, delete through it; otherwise if a comma precedes the field (after skipping whitespace), delete from that comma.
// This avoids leaving illegal shapes like ,, / {, / ,} behind.
func fieldDeleteRange(body []byte, f *jsonutil.FieldSpan) (int, int) {
	start := f.KeyStart
	end := f.ValEnd
	if end < len(body) && body[end] == ',' {
		return start, end + 1 // Delete through the trailing comma
	}
	// A } follows the field (or whitespace + }): search backwards for the previous field's trailing comma
	i := start - 1
	for i >= 0 && (body[i] == ' ' || body[i] == '\t' || body[i] == '\n' || body[i] == '\r') {
		i--
	}
	if i >= 0 && body[i] == ',' {
		return i, end
	}
	return start, end
}

// lastCloseBrace returns the position of the last } in body (skipping trailing whitespace), or -1 if none.
func lastCloseBrace(body []byte) int {
	for i := len(body) - 1; i >= 0; i-- {
		if body[i] == '}' {
			return i
		}
		if body[i] != ' ' && body[i] != '\t' && body[i] != '\n' && body[i] != '\r' {
			return -1
		}
	}
	return -1
}

// applyClassifierEdits performs text replacements/deletions/appends on the original body, touching only the target fields and preserving every other byte.
// thinking->{"type":"disabled"}, reasoning_effort->"none", reasoning->deleted.
// Fields absent from the original are appended before the top-level closing }. Edits are spliced by ascending start, all based on the original body, without interfering with each other.
func applyClassifierEdits(body []byte, spans []jsonutil.FieldSpan) []byte {
	have := map[string]*jsonutil.FieldSpan{}
	for i := range spans {
		s := &spans[i]
		switch s.Name {
		case "thinking", "reasoning_effort", "reasoning":
			have[s.Name] = s
		}
	}

	type edit struct {
		start, end int
		repl       []byte
	}
	var edits []edit

	// Replace/delete existing target fields
	if r, ok := have["reasoning"]; ok {
		ds, de := fieldDeleteRange(body, r)
		edits = append(edits, edit{ds, de, nil})
	}
	if t, ok := have["thinking"]; ok {
		edits = append(edits, edit{t.ValStart, t.ValEnd, []byte(`{"type":"disabled"}`)})
	}
	if re, ok := have["reasoning_effort"]; ok {
		edits = append(edits, edit{re.ValStart, re.ValEnd, []byte(`"none"`)})
	}

	// Append fields absent from the original body: inserted before the top-level closing } (original field order preserved, new fields at the end)
	var appendFields []byte
	if _, ok := have["thinking"]; !ok {
		appendFields = append(appendFields, []byte(`"thinking":{"type":"disabled"}`)...)
	}
	if _, ok := have["reasoning_effort"]; !ok {
		if len(appendFields) > 0 {
			appendFields = append(appendFields, ',')
		}
		appendFields = append(appendFields, []byte(`"reasoning_effort":"none"`)...)
	}
	if len(appendFields) > 0 {
		braceOff := bytes.IndexByte(body, '{')
		closeOff := lastCloseBrace(body)
		if braceOff < 0 || closeOff <= braceOff {
			return body
		}
		// If the object is empty (only whitespace between { }), insert after {; otherwise insert before } with a leading comma
		empty := true
		for i := braceOff + 1; i < closeOff; i++ {
			if c := body[i]; c != ' ' && c != '\t' && c != '\n' && c != '\r' {
				empty = false
				break
			}
		}
		if empty {
			edits = append(edits, edit{braceOff + 1, braceOff + 1, appendFields})
		} else {
			edits = append(edits, edit{closeOff, closeOff, append([]byte{','}, appendFields...)})
		}
	}

	if len(edits) == 0 {
		return body
	}

	// Splice in segments by ascending start (based on the original body, edits don't interfere with each other)
	sort.Slice(edits, func(i, j int) bool { return edits[i].start < edits[j].start })
	var buf bytes.Buffer
	prev := 0
	for _, e := range edits {
		if e.start < prev {
			continue // Defensive: skip overlapping edits
		}
		buf.Write(body[prev:e.start])
		buf.Write(e.repl)
		prev = e.end
	}
	buf.Write(body[prev:])
	return buf.Bytes()
}

// isFastRequest reports whether a request comes from Claude Code's /fast mode.
// The explicit top-level "speed":"fast" field in the request body is authoritative; the Anthropic-Beta request header is only a pre-check hint
// and never triggers routing, so a lingering header after /fast off doesn't keep traffic on the fast route.
func isFastRequest(body []byte, r *http.Request) bool {
	return getStringField(body, "speed") == "fast"
}

// removeSpeedField removes the top-level "speed" field from a JSON body, preserving a legal JSON structure.
// Upstreams don't support the speed field; passing it through could cause rejection or undefined behavior. Uses streaming field locating,
// unaffected by field order or whitespace.
func removeSpeedField(body []byte) []byte {
	spans, ok := jsonutil.LocateTopFields(body)
	if !ok {
		return body
	}
	for i, s := range spans {
		if s.Name != "speed" {
			continue
		}
		return deleteFieldSpan(body, spans, i)
	}
	return body
}

// getStringField reads a field's string value from the body's top-level JSON object; missing or non-string returns empty.
func getStringField(body []byte, key string) string {
	spans, ok := jsonutil.LocateTopFields(body)
	if !ok {
		return ""
	}
	for _, s := range spans {
		if s.Name != key {
			continue
		}
		var v string
		if err := json.Unmarshal(body[s.ValStart:s.ValEnd], &v); err != nil {
			return ""
		}
		return v
	}
	return ""
}

// getBoolField reads a field's boolean value from the body's top-level JSON object; missing or non-boolean returns ok=false.
func getBoolField(body []byte, key string) (val bool, ok bool) {
	spans, ok2 := jsonutil.LocateTopFields(body)
	if !ok2 {
		return false, false
	}
	for _, s := range spans {
		if s.Name != key {
			continue
		}
		var b bool
		if err := json.Unmarshal(body[s.ValStart:s.ValEnd], &b); err != nil {
			return false, false
		}
		return b, true
	}
	return false, false
}

// forceStreamTrue rewrites the request body's top-level "stream" field to true, appending it at the object's end if missing.
// Uses streaming field locating + text replacement without wholesale re-serialization; untouched fields keep their exact bytes (key order and formatting included),
// preserving upstream cache hits. body: the original request body. Returns the rewritten body.
func forceStreamTrue(body []byte) []byte {
	spans, ok := jsonutil.LocateTopFields(body)
	if !ok {
		return body
	}
	for _, s := range spans {
		if s.Name != "stream" {
			continue
		}
		// Already true: return the original body, avoiding a pointless rewrite.
		if bytes.Equal(bytes.TrimSpace(body[s.ValStart:s.ValEnd]), []byte("true")) {
			return body
		}
		out := make([]byte, 0, len(body))
		out = append(out, body[:s.ValStart]...)
		out = append(out, "true"...)
		out = append(out, body[s.ValEnd:]...)
		return out
	}
	// stream field missing: append ,"stream":true before the object's closing } (no comma for an empty object).
	if len(body) == 0 || body[len(body)-1] != '}' {
		return body
	}
	sep := ","
	if len(spans) == 0 {
		sep = ""
	}
	out := make([]byte, 0, len(body)+len(sep)+len(`"stream":true}`))
	out = append(out, body[:len(body)-1]...)
	out = append(out, sep...)
	out = append(out, `"stream":true}`...)
	return out
}

// deleteFieldSpan deletes the idx-th field of a top-level JSON object, keeping the other fields' order and formatting.
func deleteFieldSpan(body []byte, spans []jsonutil.FieldSpan, idx int) []byte {
	if len(spans) == 0 || idx < 0 || idx >= len(spans) {
		return body
	}
	s := spans[idx]
	// Span to delete: from this field's keyStart to its valEnd.
	// The surrounding commas are handled afterwards to keep the JSON legal.
	start := s.KeyStart
	end := s.ValEnd

	if idx == 0 {
		// First field: after deletion, drop the immediately following comma (if any).
		if end < len(body) && body[end] == ',' {
			end++
		}
	} else {
		// Non-first field: a comma precedes it; delete the comma too (start moves back one).
		start--
	}

	out := make([]byte, 0, len(body)-(end-start))
	out = append(out, body[:start]...)
	out = append(out, body[end:]...)
	return out
}

// disableThinkingInBody rewrites the request body's top-level thinking to {"type":"disabled"} and deletes
// output_config: the one-shot fallback surgery after a convertOff2Low upgrade is rejected by an upstream 400
// (the downstream wanted thinking off anyway, so the fallback is semantically lossless). Text-level editing; all other fields preserved.
func disableThinkingInBody(body []byte) ([]byte, bool) {
	nb, ok := jsonutil.SetTopLevelJSONValue(body, "thinking", []byte(`{"type":"disabled"}`))
	if !ok {
		return nil, false
	}
	return jsonutil.SetTopLevelJSONValue(nb, "output_config", nil)
}

// detectClassifier reports which classifier signature the body's system field matches (prefix at the very start of the system text).
// A bytes.Contains pre-filter keeps normal requests (without either prefix substring) away from JSON parsing — near-zero cost.
// Codex's guardian prefix arrives via the Responses port's instructions->system mapping, so this one check covers both ports.
func detectClassifier(body []byte) config.ClassifierSource {
	hasCC := bytes.Contains(body, []byte(classifierSystemPrefix))
	hasCodex := bytes.Contains(body, []byte(codexGuardianSystemPrefix))
	if !hasCC && !hasCodex {
		return config.ClassifierNone
	}
	spans, ok := jsonutil.LocateTopFields(body)
	if !ok {
		return config.ClassifierNone
	}
	if hasCC && classifierSystemMatches(body, spans, classifierSystemPrefix) {
		return config.ClassifierCC
	}
	if hasCodex && classifierSystemMatches(body, spans, codexGuardianSystemPrefix) {
		return config.ClassifierCodex
	}
	return config.ClassifierNone
}

// classifierTag returns the per-source log tag fragment ("(codex)" for Codex guardian hits, "" for Claude Code).
func classifierTag(kind config.ClassifierSource) string {
	if kind == config.ClassifierCodex {
		return "(codex)"
	}
	return ""
}

// effortWordOf extracts the lowercase effort word from a raw output_config object value ("" when absent or unparseable).
func effortWordOf(v []byte) string {
	var r struct {
		Effort string `json:"effort"`
	}
	if err := json.Unmarshal(v, &r); err != nil {
		return ""
	}
	return strings.ToLower(strings.TrimSpace(r.Effort))
}

// classifierThinkingShape reads the request's thinking mode from the Anthropic-native thinking knobs alone (the
// thinking field, plus output_config's effort word for adaptive models): "off" (explicitly disabled), "low"
// (adaptive at low/minimal effort, or an enabled budget at/below the low default), or "" (anything else, including
// thinking absent — adaptive models think by default, and an effort word alone does not enforce a mode upstream).
// The belt-and-braces fields the proxy itself would add when forcing a mode (reasoning_effort, reasoning) are never
// read here: their presence or absence doesn't change what the request's thinking mode already is.
func classifierThinkingShape(body []byte, spans []jsonutil.FieldSpan) string {
	var thinkVal, ocVal []byte
	for i := range spans {
		s := &spans[i]
		switch s.Name {
		case "thinking":
			thinkVal = body[s.ValStart:s.ValEnd]
		case "output_config":
			ocVal = body[s.ValStart:s.ValEnd]
		}
	}
	if len(thinkVal) == 0 {
		return ""
	}
	if bytes.Contains(thinkVal, []byte(`"disabled"`)) {
		return "off"
	}
	effort := ""
	if len(ocVal) > 0 {
		effort = effortWordOf(ocVal)
	}
	if bytes.Contains(thinkVal, []byte(`"adaptive"`)) {
		if effort == "low" || effort == "minimal" {
			return "low"
		}
		return "" // Adaptive at default or higher effort is not low
	}
	if bytes.Contains(thinkVal, []byte(`"enabled"`)) {
		if effort != "" && effort != "low" && effort != "minimal" {
			return "" // A contradicting native effort word means not clearly low
		}
		var th struct {
			BudgetTokens int64 `json:"budget_tokens"`
		}
		if json.Unmarshal(thinkVal, &th) != nil || th.BudgetTokens <= 0 || th.BudgetTokens > translate.EffortToThinkingBudget("low") {
			return ""
		}
		return "low"
	}
	return ""
}

// maybeRewriteClassifier applies the configured thinking policy when a classifier request matches, so classification returns fast.
// Both policies first normalize thinking to off; under "low" the post-routing upgrade point then quietly raises it to low
// upstream (that step needs the final model, which routing settles later) and thinking blocks are stripped from the response.
// Idempotence, judged by the Anthropic-native thinking knobs alone (the thinking field, plus output_config's
// effort for adaptive models): a request already thinking-off (explicit thinking:"disabled") passes through
// byte-identical and uncounted — the mode is already enforced (under "low" the upgrade point then performs and
// counts the one real rewrite); under "low" a request already thinking-low (adaptive at low/minimal effort, or
// an enabled budget at/below the low default) also passes through untouched and uncounted. The belt-and-braces
// fields the proxy itself would add when forcing a mode (reasoning_effort, reasoning) are never judgment
// criteria: their presence or absence doesn't change what the request's thinking mode already is.
// thinking absent is neither off nor low (adaptive models think by default), so bare bodies still normalize.
// A bytes.Contains pre-filter keeps normal requests away from JSON parsing — near-zero cost.
// On a rewrite, json.Decoder streaming-locates the target fields' byte positions, then text replacement — no wholesale re-serialization;
// untouched fields keep their exact bytes (key order and formatting included), preserving upstream cache hits.
// rewritten reports whether a real rewrite happened (and was counted); false covers non-hits, pass-throughs, and no-op edits.
func maybeRewriteClassifier(body []byte) (newBody []byte, rewritten bool) {
	kind := detectClassifier(body)
	if kind == config.ClassifierNone {
		return body, false
	}
	mode := config.ClassifierThinkingModeFor(config.Current(), kind)
	if mode == "" {
		return body, false
	}
	spans, ok := jsonutil.LocateTopFields(body)
	if !ok {
		return body, false
	}
	switch classifierThinkingShape(body, spans) {
	case "off":
		// Already thinking-off: pass through byte-identical and uncounted (under "low" the post-route
		// upgrade performs and counts the one real rewrite).
		return body, false
	case "low":
		if mode == "low" {
			// Already thinking-low under the low policy: pass through untouched and uncounted — no off
			// detour, and the response keeps its thinking blocks (the client asked for them).
			return body, false
		}
	}
	newBody = applyClassifierEdits(body, spans)
	if len(newBody) == len(body) && bytes.Equal(newBody, body) {
		return body, false
	}
	log.Printf("[rewrite] classifier%s signature hit; thinking disabled (body %d->%d bytes)", classifierTag(kind), len(body), len(newBody))
	recorder.Stats.ClassifierRewrites.Add(1) // Live status row count: classifier thinking rewrites (off, or the off phase of low)
	if kind == config.ClassifierCodex {
		recorder.Stats.ClassifierRewritesCodex.Add(1)
	} else {
		recorder.Stats.ClassifierRewritesCC.Add(1)
	}
	return newBody, true
}

// logRequestDetail parses the request body and prints the stream/tools/system prefixes, for diagnosing classifier fingerprints.
// Called only when log_request_detail=true; off by default.
func logRequestDetail(r *http.Request, body []byte) {
	var p map[string]interface{}
	if err := json.Unmarshal(body, &p); err != nil {
		log.Printf("[detail] %s %s (non-JSON)", r.Method, r.URL.Path)
		return
	}
	stream, _ := p["stream"].(bool)
	tools := 0
	toolNames := []string{}
	if arr, ok := p["tools"].([]interface{}); ok {
		tools = len(arr)
		for _, t := range arr {
			if m, ok := t.(map[string]interface{}); ok {
				name, _ := m["name"].(string)
				typ, _ := m["type"].(string)
				if name != "" {
					toolNames = append(toolNames, name)
				} else if typ != "" {
					toolNames = append(toolNames, typ)
				}
			}
		}
	}
	sysPrefix := ""
	switch s := p["system"].(type) {
	case string:
		sysPrefix = truncate(s, 60)
	case []interface{}:
		for _, blk := range s {
			if m, ok := blk.(map[string]interface{}); ok {
				if t, _ := m["text"].(string); t != "" {
					sysPrefix = truncate(t, 60)
					break
				}
			}
		}
	}
	log.Printf("[detail] %s %s stream=%v tools=%d %v sys=%q image=%v search=%v", r.Method, r.URL.Path, stream, tools, toolNames, sysPrefix, hasImage(body), hasWebSearch(body))
}

// locateModel locates the string value span of the request body's top-level "model" field.
// Lightweight: after locating the "model" key it reads the string literal right after it, without parsing the whole JSON — no per-request Unmarshal.
// body: the request body bytes. Returns value=model name, start/end=the value's byte span in body (quotes excluded), ok=whether it was located.
func locateModel(body []byte) (value string, start, end int, ok bool) {
	key := []byte(`"model"`)
	i := bytes.Index(body, key)
	if i < 0 {
		return "", 0, 0, false
	}
	i += len(key)
	// Skip the colon and whitespace to land on the value.
	for i < len(body) {
		c := body[i]
		if c == ':' || c == ' ' || c == '\t' || c == '\n' || c == '\r' {
			i++
			continue
		}
		break
	}
	if i >= len(body) || body[i] != '"' {
		return "", 0, 0, false
	}
	i++ // Skip the opening quote
	start = i
	for i < len(body) && body[i] != '"' {
		if body[i] == '\\' && i+1 < len(body) { // Skip escaped characters
			i += 2
			continue
		}
		i++
	}
	return string(body[start:i]), start, i, true
}

// extractModel extracts the request body's "model" field value, only for display in the [请求] log line. Returns empty on failure.
func extractModel(body []byte) string {
	v, _, _, ok := locateModel(body)
	if !ok {
		return ""
	}
	return v
}

// extractConvID extracts the session identifier of an Anthropic-port request body, anchoring the status page's "缓存年龄" column per session.
// Claude Code always carries a top-level metadata.user_id whose value is a JSON string
// ({"device_id":"...","account_uuid":"...","session_id":"<uuid>"}); the session_id inside is used.
// Free text stuffed by a bare API caller is used as-is (stability is enough for correlation). No metadata → empty.
// jsonutil.LocateTopFields locates the top-level metadata and only that small object is unmarshaled — the body is never fully parsed; read-only.
func extractConvID(body []byte) string {
	spans, ok := jsonutil.LocateTopFields(body)
	if !ok {
		return ""
	}
	for _, s := range spans {
		if s.Name != "metadata" {
			continue
		}
		var md struct {
			UserID string `json:"user_id"`
		}
		if err := json.Unmarshal(body[s.ValStart:s.ValEnd], &md); err != nil || md.UserID == "" {
			return ""
		}
		if strings.HasPrefix(md.UserID, "{") {
			var inner struct {
				SessionID string `json:"session_id"`
			}
			if err := json.Unmarshal([]byte(md.UserID), &inner); err == nil && inner.SessionID != "" {
				return inner.SessionID
			}
		}
		return md.UserID
	}
	return ""
}

// extractThinkMode extracts the thinking config of an Anthropic-format request body, so the status page's 「API」 column thinking value shows
// the shape actually sent upstream (the call site is after the classifier's thinking-off rewrite; the translation port passes in the already-mapped body).
// Values take the shortest form (which API family the vocabulary belongs to is carried by the column color, not by prefix text):
// thinking.type=disabled → "off"; enabled → "on <budget_tokens>" (just "on" without a budget);
// adaptive without effort → "adaptive"; with output_config.effort → just the effort word ("high"/"max"…);
// output_config.effort without thinking → likewise just the effort word; unknown types display as-is.
// No thinking/output_config field → empty (column shows -).
// jsonutil.LocateTopFields locates the relevant small objects for unmarshaling; the body is never fully parsed; read-only.
func extractThinkMode(body []byte) string {
	spans, ok := jsonutil.LocateTopFields(body)
	if !ok {
		return ""
	}
	mode := ""
	effort := ""
	for _, s := range spans {
		switch s.Name {
		case "thinking":
			var t struct {
				Type   string `json:"type"`
				Budget int64  `json:"budget_tokens"`
			}
			if err := json.Unmarshal(body[s.ValStart:s.ValEnd], &t); err != nil {
				continue
			}
			switch t.Type {
			case "disabled":
				mode = "off"
			case "enabled":
				if t.Budget > 0 {
					mode = fmt.Sprintf("on %d", t.Budget)
				} else {
					mode = "on"
				}
			default:
				mode = t.Type // adaptive and unknown types display as-is
			}
		case "output_config":
			var oc struct {
				Effort string `json:"effort"`
			}
			if err := json.Unmarshal(body[s.ValStart:s.ValEnd], &oc); err == nil {
				effort = oc.Effort
			}
		}
	}
	// Effort word alone: adaptive-with-effort and effort-only both collapse to the effort itself (a prefix like "adaptive·high" would be redundant;
	// the web uses color to tell which API family the vocabulary belongs to).
	if effort != "" && (mode == "" || mode == "adaptive") {
		return effort
	}
	return mode
}

// replaceModelValue replaces the request body's "model" field value with newModel, for rewriting on route hits.
// locateModel locates the value span, then byte splicing; the length change is picked up when bytes.NewReader recomputes Content-Length later.
// body: original request body; newModel: target model name. Returns the rewritten body. Returns the original when no model field is located.
func replaceModelValue(body []byte, newModel string) []byte {
	_, start, end, ok := locateModel(body)
	if !ok {
		return body
	}
	out := make([]byte, 0, len(body)-end+start+len(newModel))
	out = append(out, body[:start]...)
	out = append(out, []byte(newModel)...)
	out = append(out, body[end:]...)
	return out
}

// hasImage reports whether the request body contains an Anthropic image content block.
// An Anthropic image block looks like {"type":"image","source":{"type":"base64",...}}; "type":"image" appears only in image blocks within a request body.
// Uses bytes.Contains for zero-allocation substring detection instead of full deserialization, following the streaming-locate/text-replace forwarding-safety principle.
// body: the request body. Returns whether an image block is present.
func hasImage(body []byte) bool {
	return bytes.Contains(body, []byte(`"type":"image"`))
}

// hasWebSearch reports whether the request body carries an Anthropic server-side web_search tool (the upstream performs the search).
// Only "type":"web_search_YYYYMMDD" server-side tools are recognized; the client-side WebSearch tool is deliberately not recognized
// (Claude Code carries its definition in every request, so it can't distinguish a real intent to search and would misjudge every request as a search request).
// body: the request body. Returns whether a server-side web_search tool is present.
func hasWebSearch(body []byte) bool {
	return bytes.Contains(body, []byte(`"type":"web_search`))
}

// summaryLevelConfig returns the summary instruction injected into a standalone search sub-agent request
// (single-request mode: the searching upstream summarizes its own results within the same request).
// low=brief summary (default), mid=medium detail (key facts included), high=thorough (all data points/citations/context),
// max=high plus steps/methods/code/formulas reproduced verbatim.
// level: the configured summary level; query: the user's original question (search intent), carried into the instruction so the summary focuses on relevant content. Returns the instruction text.
func summaryLevelConfig(level, query string) string {
	// The instruction carries the user's original question (search intent): the summary focuses on question-relevant content while still covering each result's key information,
	// avoiding a brainless generic summary. An empty query degrades to a generic summary.
	q := strings.TrimSpace(query)
	if q == "" {
		q = "search"
	}
	// Search results often contain multiple dates/times ("today/latest/recent/now"); carrying the current date-time lets the model weight time-sensitive content correctly.
	now := time.Now().In(time.FixedZone("UTC+8", 8*60*60)).Format("2006-01-02 15:04")
	base := "Current date and time is " + now + " (UTC+8, Beijing time).\n" +
		"The user's original question (the search intent) is: \"%s\".\n" +
		"After performing the web search, %s IN ORDER. " +
		"While summarizing each result, pay special attention to and explicitly call out information that addresses or relates to the user's question above, but still cover the other key content of each result. " +
		"Format each as 'Result N: <summary>' where N starts at 1 and goes in order across all search results. " +
		"Do not answer the original question, just summarize each result."
	switch strings.ToLower(strings.TrimSpace(level)) {
	case "mid", "medium":
		return fmt.Sprintf(base, q, "write a medium-detail plaintext summary for EACH search result, including key facts and relevant data points")
	case "max", "full":
		// max: on top of high, steps/methods/code/formulas must be reproduced verbatim in full.
		return fmt.Sprintf(base, q, "write a detailed comprehensive plaintext summary for EACH search result, including all key facts, data points, direct quotes, dates, numbers, and relevant context; be thorough. When you encounter any steps, methods, code, or formulas, you MUST reproduce them completely and verbatim in full detail")
	case "high":
		return fmt.Sprintf(base, q, "write a detailed comprehensive plaintext summary for EACH search result, including all key facts, data points, direct quotes, dates, numbers, and relevant context; be thorough")
	default: // low / empty / unknown
		return fmt.Sprintf(base, q, "write a short plaintext summary for EACH search result")
	}
}

// searchSubagentMarker marks a standalone search sub-agent request (Claude Code spins one off per web search):
// the request's only job is "Perform a web search for the query: ...", with a server-side web_search tool attached.
const searchSubagentMarker = "Perform a web search for the query"

// hasSearchSubagentMarker reports whether the request is a standalone search sub-agent request.
// body: the request body. Returns whether the marker is present.
func hasSearchSubagentMarker(body []byte) bool {
	return bytes.Contains(body, []byte(searchSubagentMarker))
}

// searchStringEnd locates the closing quote of the JSON string containing position `from`
// (scanning forward, backslash-escape aware). Returns -1 when the string never terminates.
func searchStringEnd(body []byte, from int) int {
	for i := from; i < len(body); i++ {
		if body[i] == '\\' {
			i++
			continue
		}
		if body[i] == '"' {
			return i
		}
	}
	return -1
}

// searchMarkerQuery extracts the search intent: the text after the marker inside the same JSON string
// (a leading colon is stripped, JSON escapes decoded). Returns "" when no marker is present or decoding fails.
// body: the request body. Returns the query text (may be empty).
func searchMarkerQuery(body []byte) string {
	idx := bytes.Index(body, []byte(searchSubagentMarker))
	if idx < 0 {
		return ""
	}
	end := searchStringEnd(body, idx+len(searchSubagentMarker))
	if end < 0 {
		return ""
	}
	raw := body[idx+len(searchSubagentMarker) : end]
	// raw contains no unescaped quote by construction, so wrapping it in quotes and unmarshalling decodes the escapes.
	var q string
	if err := json.Unmarshal([]byte(`"`+string(raw)+`"`), &q); err != nil {
		return ""
	}
	q = strings.TrimSpace(q)
	q = strings.TrimPrefix(q, ":")
	return strings.TrimSpace(q)
}

// injectAfterMarker appends the instruction to the JSON string holding the search sub-agent marker (pure byte
// surgery: the string's closing quote is located escape-aware, the JSON-escaped instruction inserted before it;
// every other byte is preserved; content in string or block-array form alike).
// body: the request body; instruction: the summary instruction to append. Returns (newBody, ok).
func injectAfterMarker(body []byte, instruction string) ([]byte, bool) {
	idx := bytes.Index(body, []byte(searchSubagentMarker))
	if idx < 0 {
		return body, false
	}
	end := searchStringEnd(body, idx+len(searchSubagentMarker))
	if end < 0 {
		return body, false
	}
	esc, err := json.Marshal("\n\n" + instruction) // A JSON string literal; the insertion lands inside the existing string, so the quotes are stripped.
	if err != nil {
		return body, false
	}
	ins := esc[1 : len(esc)-1]
	out := make([]byte, 0, len(body)+len(ins))
	out = append(out, body[:end]...)
	out = append(out, ins...)
	out = append(out, body[end:]...)
	return out, true
}

// rewriteSearchThinking rewrites a standalone search sub-agent request's thinking per claudeSearchThinking,
// convertOff2Low-style: whatever shape the request carried, the upstream gets the configured level in the
// adaptive shape — "off" forces {"type":"disabled"} (output_config dropped, sampling knobs kept);
// low/mid/high/max set {"type":"adaptive"} + output_config {"effort":low|medium|high|max} (mid=medium),
// deleting temperature/top_p (thinking-on forbids sampling knobs, same rule as translate.MaybeUpgradeOffToLow). The
// response side lives with the caller: when the client never asked for thinking it flags stripThinking so the
// returning thinking blocks are stripped and the client keeps seeing a thinking-off response. All edits are
// positional via jsonutil.SetTopLevelJSONValue.
// body: the request body; level: the effective level. Returns (newBody, rewritten).
func rewriteSearchThinking(body []byte, level string) ([]byte, bool) {
	if level == "" {
		return body, false
	}
	if level == "off" {
		nb, ok := jsonutil.SetTopLevelJSONValue(body, "thinking", []byte(`{"type":"disabled"}`))
		if !ok {
			return body, false
		}
		nb, _ = jsonutil.SetTopLevelJSONValue(nb, "output_config", nil)
		return nb, true
	}
	if level == "mid" { // The user-facing enum says mid; the effort tables say medium.
		level = "medium"
	}
	effort := translate.CodexEffortToAnthropic(level)
	if effort == "" {
		return body, false // Unknown level (config validation should have caught it): leave the body alone
	}
	nb, ok := jsonutil.SetTopLevelJSONValue(body, "thinking", []byte(`{"type":"adaptive"}`))
	if !ok {
		return body, false
	}
	nb, _ = jsonutil.SetTopLevelJSONValue(nb, "output_config", []byte(fmt.Sprintf(`{"effort":%q}`, effort)))
	nb, _ = jsonutil.SetTopLevelJSONValue(nb, "temperature", nil) // Thinking-on forbids sampling knobs upstream-side
	nb, _ = jsonutil.SetTopLevelJSONValue(nb, "top_p", nil)
	return nb, true
}

// rewriteResponseModel rewrites the "model" field value in SSE data: lines back to origModel.
// It doesn't rely on the model name the upstream actually returned (which may differ from the targetModel written in the request); any data: line containing a model field is rewritten.
// Non-data: lines are returned unchanged, as are lines where no model field is located or the value already equals origModel.
// Returns (rewritten line, model value actually returned by the upstream, whether a rewrite happened).
func rewriteResponseModel(line []byte, origModel string) ([]byte, string, bool) {
	prefix := []byte("data:")
	if !bytes.HasPrefix(line, prefix) {
		return line, "", false
	}
	jsonStart := len(prefix)
	for jsonStart < len(line) && (line[jsonStart] == ' ' || line[jsonStart] == '\t') {
		jsonStart++
	}
	curModel, _, _, ok := locateModel(line[jsonStart:])
	if !ok || curModel == origModel {
		return line, "", false
	}
	newJSON := replaceModelValue(line[jsonStart:], origModel)
	out := make([]byte, 0, jsonStart+len(newJSON))
	out = append(out, line[:jsonStart]...)
	out = append(out, newJSON...)
	return out, curModel, true
}

// responsesAPIPath computes the join path for native passthrough: url_response_api may hold a base (…/coding),
// one with /v1, or a full …/v1/responses; only the missing difference is appended, never doubled. Used with upstream+upPath in the handler.
func responsesAPIPath(base string) string {
	b := strings.TrimRight(base, "/")
	if strings.HasSuffix(b, "/v1/responses") {
		return ""
	}
	if strings.HasSuffix(b, "/v1") {
		return "/responses"
	}
	return "/v1/responses"
}
