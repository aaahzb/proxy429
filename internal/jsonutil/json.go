package jsonutil

import (
	"bytes"
	"encoding/json"
)

// ToInt64 safely converts a JSON-parsed number (float64 / json.Number etc.) to int64.
func ToInt64(v interface{}) int64 {
	switch n := v.(type) {
	case float64:
		return int64(n)
	case int:
		return int64(n)
	case int64:
		return n
	case json.Number:
		if i, err := n.Int64(); err == nil {
			return i
		}
	}
	return 0
}

// FieldSpan records a field's byte positions within a top-level JSON object (based on the original body, quotes/colon included in the locating).
// KeyStart/KeyEnd are the key's span (including the double quotes); ValStart/ValEnd are the value's byte span.
type FieldSpan struct {
	Name     string
	KeyStart int
	KeyEnd   int
	ValStart int
	ValEnd   int
}

// LocateTopFields streaming-parses a top-level JSON object and returns each field's byte position in the original body (appearance order preserved).
// It reads keys with json.Decoder's Token and values into RawMessage with Decode, then calibrates the value's byte span using InputOffset +
// the RawMessage content. Only top-level keys match; same-named fields in nested objects are untouched.
// On locating failure it returns ok=false; the caller then passes the original body through (forwarding unaffected).
func LocateTopFields(body []byte) ([]FieldSpan, bool) {
	dec := json.NewDecoder(bytes.NewReader(body))
	if t, err := dec.Token(); err != nil || t != json.Delim('{') {
		return nil, false
	}
	var spans []FieldSpan
	for dec.More() {
		tk, err := dec.Token()
		if err != nil {
			return nil, false
		}
		key, ok := tk.(string)
		if !ok {
			return nil, false
		}
		var v json.RawMessage
		if err := dec.Decode(&v); err != nil {
			return nil, false
		}
		valStart, valEnd, ok := locateValueRange(body, v, dec.InputOffset())
		if !ok {
			return nil, false
		}
		keyStart, keyEnd, ok := findKeySpan(body, valStart, key)
		if !ok {
			return nil, false
		}
		spans = append(spans, FieldSpan{Name: key, KeyStart: keyStart, KeyEnd: keyEnd, ValStart: valStart, ValEnd: valEnd})
	}
	return spans, true
}

// locateValueRange derives the value's byte span from the Decoder's InputOffset and calibrates it with the RawMessage content.
// After Decode, InputOffset usually points at the value's end, but scanp/trailing whitespace can shift it a few bytes;
// so an exact match (body slice == RawMessage) is attempted within ±8 bytes nearby; no match means locating failed.
func locateValueRange(body, v []byte, off int64) (valStart, valEnd int, ok bool) {
	approx := int(off)
	for d := 0; d <= 8; d++ {
		for _, end := range []int{approx + d, approx - d} {
			start := end - len(v)
			if start >= 0 && end >= 0 && end <= len(body) && bytes.Equal(body[start:end], v) {
				return start, end, true
			}
		}
	}
	return 0, 0, false
}

// findKeySpan searches backwards from the value's start for the "key": pattern, locating the key's byte span (including the double quotes).
// The target field names are all simple identifiers without escapes, so plain literal matching suffices; LastIndex takes the most recent occurrence.
func findKeySpan(body []byte, valStart int, key string) (keyStart, keyEnd int, ok bool) {
	needle := []byte(`"` + key + `":`)
	end := valStart
	if end > len(body) {
		end = len(body)
	}
	idx := bytes.LastIndex(body[:end], needle)
	if idx < 0 {
		return 0, 0, false
	}
	return idx, idx + len(key) + 2, true // +2 = opening " + key + closing "
}

// SetTopLevelJSONValue precisely sets one key's value at the top level of a JSON text (rawValue is complete JSON
// value text, e.g. `"en"`, `{"type":"disabled"}`); rawValue=nil deletes the key.
// Fields are located via LocateTopFields (same-named nested keys untouched), and all editing is text-level: every other field's
// content, order, indentation, and newline style is preserved byte for byte. Returns a new slice; the input is not modified.
// ok=false when the top level is not a legal JSON object; the caller should abandon the write.
func SetTopLevelJSONValue(body []byte, key string, rawValue []byte) ([]byte, bool) {
	spans, ok := LocateTopFields(body)
	if !ok {
		return nil, false
	}
	idx := -1
	for i := range spans {
		if spans[i].Name == key {
			idx = i
			break
		}
	}
	if idx >= 0 {
		s := spans[idx]
		if rawValue != nil {
			out := make([]byte, 0, len(body)-(s.ValEnd-s.ValStart)+len(rawValue))
			out = append(out, body[:s.ValStart]...)
			out = append(out, rawValue...)
			out = append(out, body[s.ValEnd:]...)
			return out, true
		}
		start, end := s.KeyStart, s.ValEnd
		if idx == 0 {
			for start > 0 && (body[start-1] == ' ' || body[start-1] == '\t') {
				start--
			}
			for end < len(body) && (body[end] == ' ' || body[end] == '\t') {
				end++
			}
			if end < len(body) && body[end] == ',' {
				end++
			}
			for end < len(body) && (body[end] == ' ' || body[end] == '\t') {
				end++
			}
			if end < len(body) && body[end] == '\r' {
				end++
			}
			if end < len(body) && body[end] == '\n' {
				end++
			}
		} else {
			for start > 0 && (body[start-1] == ' ' || body[start-1] == '\t' || body[start-1] == '\r' || body[start-1] == '\n') {
				start--
			}
			if start > 0 && body[start-1] == ',' {
				start--
			}
		}
		out := make([]byte, 0, len(body)-(end-start))
		out = append(out, body[:start]...)
		out = append(out, body[end:]...)
		return out, true
	}
	if rawValue == nil {
		return body, true // The key to delete doesn't exist; nothing to do
	}
	closePos := len(bytes.TrimRight(body, " \t\r\n")) - 1
	if closePos < 0 || body[closePos] != '}' {
		return nil, false
	}
	indent := "  "
	if len(spans) > 0 {
		lineStart := bytes.LastIndexByte(body[:spans[0].KeyStart], '\n') + 1
		ind := body[lineStart:spans[0].KeyStart]
		allWS := len(ind) > 0
		for _, c := range ind {
			if c != ' ' && c != '\t' {
				allWS = false
				break
			}
		}
		if allWS {
			indent = string(ind)
		}
	}
	nl := "\n"
	if bytes.Contains(body, []byte("\r\n")) {
		nl = "\r\n"
	}
	out := make([]byte, 0, len(body)+len(key)+len(rawValue)+16)
	trimEnd := len(bytes.TrimRight(body[:closePos], " \t\r\n"))
	out = append(out, body[:trimEnd]...)
	if len(spans) > 0 {
		out = append(out, ',')
	}
	out = append(out, nl...)
	out = append(out, indent...)
	out = append(out, '"')
	out = append(out, key...)
	out = append(out, []byte(`": `)...)
	out = append(out, rawValue...)
	out = append(out, nl...)
	out = append(out, body[closePos:]...)
	return out, true
}
