package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// lazyApplyRouteKeys lists the fields a routes[] entry may inherit from lazyApply. Only fields the route
// object already accepts are listed; connection fields (url/api/model/pattern/url_response_api) never inherit.
var lazyApplyRouteKeys = []string{
	"text_only",
	"no_search",
	"thinking",
	"convertOff2Low",
	"enhance_search",
	"claudeSearchThinking",
	"claudeSearchSummary",
	"noBullshitOnEmptyClaudeSearch",
}

// lazyApplySearchFallbackKeys lists the fields search_fallback may inherit. A nested enhance_search never
// applies here: the fallback serves diverted requests through its own summary/thinking fields.
var lazyApplySearchFallbackKeys = []string{
	"convertOff2Low",
	"summary_mode",
	"summary_thinking",
	"summary_level",
	"claudeSearchThinking",
	"claudeSearchSummary",
	"noBullshitOnEmptyClaudeSearch",
}

// lazyApplyClassifierKeys lists the fields classifier_route may inherit.
var lazyApplyClassifierKeys = []string{
	"classifier_thinking",
	"classifier_thinking_cc",
	"classifier_thinking_codex",
}

// lazyApplyConvertOnlyKeys is shared by fast_route and multimodal_fallback.
var lazyApplyConvertOnlyKeys = []string{"convertOff2Low"}

// jsonSpan is the half-open byte range [start,end) of one JSON value in the raw config text.
type jsonSpan struct {
	start int
	end   int
}

// jsonMember is one object member in document order; keyStart anchors the indentation of its line.
type jsonMember struct {
	key      string
	keyStart int
	value    jsonSpan
}

// jsonEdit is one insertion into the original text; pos is a byte offset in that text.
type jsonEdit struct {
	pos  int
	text []byte
}

// materializeLazyApply proves what lazyApply does to the edited JSON and returns it with the inherited
// keys appended in place. It validates the whole config first (parseConfig), then works on the raw text
// as a text edit: existing bytes keep their order and formatting, $VAR references stay unexpanded, and
// the lazyApply object itself is kept, so copying the preview back into the editor is behavior-preserving.
// Every route-like object receives the lazyApply keys it supports and does not already write;
// noLazyApply:true skips an object entirely, and optional routes are never created.
func materializeLazyApply(data []byte) ([]byte, error) {
	if _, _, err := parseConfig(data); err != nil {
		return nil, err
	}
	rootMembers, err := objectMembers(data, jsonSpan{start: 0, end: len(data)})
	if err != nil {
		return nil, err
	}
	var lazy *jsonMember
	for i := range rootMembers {
		if rootMembers[i].key == "lazyApply" {
			lazy = &rootMembers[i]
			break
		}
	}
	if lazy == nil || isJSONNull(data[lazy.value.start:lazy.value.end]) {
		return data, nil
	}
	lazyMembers, err := objectMembers(data, lazy.value)
	if err != nil {
		return nil, err
	}
	if len(lazyMembers) == 0 {
		return data, nil
	}
	lazyValues := make(map[string][]byte, len(lazyMembers))
	for _, m := range lazyMembers {
		compact, err := compactJSON(data[m.value.start:m.value.end])
		if err != nil {
			return nil, err
		}
		lazyValues[m.key] = compact
	}
	var edits []jsonEdit
	fill := func(span jsonSpan, keys []string) error {
		edit, ok, err := buildLazyInsertion(data, span, keys, lazyValues)
		if err != nil {
			return err
		}
		if ok {
			edits = append(edits, edit)
		}
		return nil
	}
	for _, m := range rootMembers {
		switch m.key {
		case "search_fallback":
			if err := fill(m.value, lazyApplySearchFallbackKeys); err != nil {
				return nil, err
			}
		case "fast_route":
			if err := fill(m.value, lazyApplyConvertOnlyKeys); err != nil {
				return nil, err
			}
		case "multimodal_fallback":
			if err := fill(m.value, lazyApplyConvertOnlyKeys); err != nil {
				return nil, err
			}
		case "classifier_route":
			if err := fill(m.value, lazyApplyClassifierKeys); err != nil {
				return nil, err
			}
		case "routes":
			elems, err := arrayElements(data, m.value)
			if err != nil {
				return nil, err
			}
			for _, elem := range elems {
				if err := fill(elem, lazyApplyRouteKeys); err != nil {
					return nil, err
				}
			}
		}
	}
	return applyJSONEdits(data, edits), nil
}

// buildLazyInsertion builds one splice that appends the missing supported keys to a single object.
// The insertion preserves the object's existing member order and indentation; a compact object stays
// compact, and an empty object gains one member line per inherited key.
func buildLazyInsertion(data []byte, span jsonSpan, keys []string, lazy map[string][]byte) (jsonEdit, bool, error) {
	pos := skipJSONSpace(data, span.start)
	if pos >= len(data) || data[pos] != '{' {
		return jsonEdit{}, false, nil // null / non-object: the tolerant config parser owns that error path
	}
	members, err := objectMembers(data, span)
	if err != nil {
		return jsonEdit{}, false, err
	}
	present := make(map[string]bool, len(members))
	for _, m := range members {
		present[m.key] = true
		if m.key == "noLazyApply" && jsonBoolTrue(bytes.TrimSpace(data[m.value.start:m.value.end])) {
			return jsonEdit{}, false, nil
		}
	}
	var entries []string
	for _, key := range keys {
		if present[key] {
			continue
		}
		value, ok := lazy[key]
		if !ok {
			continue
		}
		entries = append(entries, `"`+key+`":`+string(value))
	}
	if len(entries) == 0 {
		return jsonEdit{}, false, nil
	}
	multiline := bytes.Contains(data[span.start:span.end], []byte("\n"))
	if len(members) > 0 {
		last := members[len(members)-1]
		if multiline {
			indent := memberIndent(data, last.keyStart, span)
			return jsonEdit{pos: last.value.end, text: []byte(",\n" + indent + strings.Join(entries, ",\n"+indent))}, true, nil
		}
		return jsonEdit{pos: last.value.end, text: []byte("," + strings.Join(entries, ","))}, true, nil
	}
	// Empty object: insert one line per member, indented one level past the closing brace.
	if multiline {
		indent := lineIndent(data, span.end-1) + "  "
		return jsonEdit{pos: pos + 1, text: []byte("\n" + indent + strings.Join(entries, ",\n"+indent))}, true, nil
	}
	return jsonEdit{pos: pos + 1, text: []byte(strings.Join(entries, ","))}, true, nil
}

// memberIndent returns the whitespace prefix of the line holding keyStart. When that member does not
// start its own line, it falls back to one level deeper than the object's closing brace.
func memberIndent(data []byte, keyStart int, span jsonSpan) string {
	start := bytes.LastIndexByte(data[:keyStart], '\n') + 1
	prefix := data[start:keyStart]
	if len(bytes.Trim(prefix, " \t")) == 0 {
		return string(prefix)
	}
	return lineIndent(data, span.end-1) + "  "
}

// lineIndent returns the whitespace before pos on its line, or "" when pos is not preceded only by
// whitespace (compact or single-line input).
func lineIndent(data []byte, pos int) string {
	start := bytes.LastIndexByte(data[:pos], '\n') + 1
	prefix := data[start:pos]
	if len(bytes.Trim(prefix, " \t")) != 0 {
		return ""
	}
	return string(prefix)
}

// applyJSONEdits splices ascending insertions into the original text without re-encoding anything else.
func applyJSONEdits(data []byte, edits []jsonEdit) []byte {
	if len(edits) == 0 {
		return data
	}
	var out bytes.Buffer
	out.Grow(len(data) + len(edits)*24)
	last := 0
	for _, edit := range edits {
		out.Write(data[last:edit.pos])
		out.Write(edit.text)
		last = edit.pos
	}
	out.Write(data[last:])
	return out.Bytes()
}

// compactJSON re-renders one raw value without surrounding whitespace. String content (including
// $VAR references) and object member order stay exactly as written in the lazyApply block.
func compactJSON(raw []byte) ([]byte, error) {
	var out bytes.Buffer
	if err := json.Compact(&out, raw); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}

// skipJSONSpace advances past JSON whitespace.
func skipJSONSpace(data []byte, pos int) int {
	for pos < len(data) {
		switch data[pos] {
		case ' ', '\t', '\r', '\n':
			pos++
		default:
			return pos
		}
	}
	return pos
}

// scanJSONString returns the index just past a JSON string starting at pos.
func scanJSONString(data []byte, pos int) (int, error) {
	if pos >= len(data) || data[pos] != '"' {
		return 0, fmt.Errorf("expected string at byte %d", pos)
	}
	for i := pos + 1; i < len(data); i++ {
		switch data[i] {
		case 0x5c: // 0x5c is the backslash byte; skip it so escaped quotes stay aligned.
			i++ // Skip the escaped byte, keeping escaped quotes aligned.
		case '"':
			return i + 1, nil
		}
	}
	return 0, fmt.Errorf("unterminated string at byte %d", pos)
}

// scanJSONValue returns the index just past the JSON value starting at pos.
func scanJSONValue(data []byte, pos int) (int, error) {
	pos = skipJSONSpace(data, pos)
	if pos >= len(data) {
		return 0, fmt.Errorf("unexpected end of JSON at byte %d", pos)
	}
	switch data[pos] {
	case '"':
		return scanJSONString(data, pos)
	case '{', '[':
		open, closer := data[pos], byte('}')
		if open == '[' {
			closer = ']'
		}
		depth := 0
		for i := pos; i < len(data); i++ {
			switch data[i] {
			case '"':
				end, err := scanJSONString(data, i)
				if err != nil {
					return 0, err
				}
				i = end - 1
			case open:
				depth++
			case closer:
				depth--
				if depth == 0 {
					return i + 1, nil
				}
			}
		}
		return 0, fmt.Errorf("unterminated container at byte %d", pos)
	default:
		for i := pos; i < len(data); i++ {
			switch data[i] {
			case ',', '}', ']', ' ', '\t', '\r', '\n':
				return i, nil
			}
		}
		return len(data), nil
	}
}

// objectMembers lists one object's members in document order; span must cover the whole object.
func objectMembers(data []byte, span jsonSpan) ([]jsonMember, error) {
	pos := skipJSONSpace(data, span.start)
	if pos >= len(data) || data[pos] != '{' {
		return nil, fmt.Errorf("expected object at byte %d", pos)
	}
	pos++
	var members []jsonMember
	for {
		pos = skipJSONSpace(data, pos)
		if pos >= span.end {
			return nil, fmt.Errorf("unterminated object at byte %d", span.start)
		}
		if data[pos] == '}' {
			return members, nil
		}
		if len(members) > 0 {
			if data[pos] != ',' {
				return nil, fmt.Errorf("expected comma at byte %d", pos)
			}
			pos = skipJSONSpace(data, pos+1)
		}
		keyStart := pos
		keyEnd, err := scanJSONString(data, pos)
		if err != nil {
			return nil, err
		}
		var key string
		if err := json.Unmarshal(data[keyStart:keyEnd], &key); err != nil {
			return nil, err
		}
		pos = skipJSONSpace(data, keyEnd)
		if pos >= span.end || data[pos] != ':' {
			return nil, fmt.Errorf("expected colon at byte %d", pos)
		}
		pos = skipJSONSpace(data, pos+1)
		valueStart := pos
		valueEnd, err := scanJSONValue(data, pos)
		if err != nil {
			return nil, err
		}
		members = append(members, jsonMember{key: key, keyStart: keyStart, value: jsonSpan{start: valueStart, end: valueEnd}})
		pos = valueEnd
	}
}

// arrayElements lists one array's element spans in document order. A null / non-array value yields no
// elements: the tolerant config parser already owns that error path.
func arrayElements(data []byte, span jsonSpan) ([]jsonSpan, error) {
	pos := skipJSONSpace(data, span.start)
	if pos >= len(data) || data[pos] != '[' {
		return nil, nil
	}
	pos++
	var elems []jsonSpan
	for {
		pos = skipJSONSpace(data, pos)
		if pos >= span.end {
			return nil, fmt.Errorf("unterminated array at byte %d", span.start)
		}
		if data[pos] == ']' {
			return elems, nil
		}
		if len(elems) > 0 {
			if data[pos] != ',' {
				return nil, fmt.Errorf("expected comma at byte %d", pos)
			}
			pos = skipJSONSpace(data, pos+1)
		}
		start := pos
		end, err := scanJSONValue(data, pos)
		if err != nil {
			return nil, err
		}
		elems = append(elems, jsonSpan{start: start, end: end})
		pos = end
	}
}

// isJSONNull reports whether raw is the JSON null literal (surrounding whitespace ignored).
func isJSONNull(raw []byte) bool {
	return string(bytes.TrimSpace(raw)) == "null"
}

// jsonBoolTrue reports whether raw is the JSON boolean true. Missing or malformed values report false;
// the real parser reports malformed noLazyApply values before the preview is rendered.
func jsonBoolTrue(raw []byte) bool {
	var b bool
	return json.Unmarshal(raw, &b) == nil && b
}

// lazyApplyPreviewHandler renders the edited config with lazyApply materialized, for the console's
// "preview lazyApply" button. It never writes the file and never publishes a config: the body is the
// editor's current text, and only the insertion-edited JSON is returned. Localhost only, like every
// /__* route.
func lazyApplyPreviewHandler(w http.ResponseWriter, r *http.Request) {
	if !isLocalRequest(r) {
		http.Error(w, "forbidden (local only)", http.StatusForbidden)
		return
	}
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "failed to read request body: "+err.Error(), http.StatusBadRequest)
		return
	}
	out, err := materializeLazyApply(body)
	if err != nil {
		http.Error(w, "invalid config: "+err.Error(), http.StatusBadRequest)
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(out)
}
