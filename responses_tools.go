// responses_tools.go — the translation registry for the Responses API tool system.
// Mirrors cc-switch 3.20.0's CodexToolContext (transform_codex_chat.rs) and
// tool_media.rs (tool-result media stripping). Four tool kinds: function (ordinary functions),
// namespace (MCP namespaces, flattened to ns__name), custom (Codex freeform tools,
// wrapped into an {"input": string} JSON Schema), toolSearch (fixed proxy tool).
package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strconv"
	"strings"
	"unicode/utf8"
)

// Mirrors cc-switch's same-named constant. The marker string stays verbatim; client/model sides may match it literally.
const (
	toolSearchProxyName    = "tool_search"
	customToolInputField   = "input"
	customToolInputDesc    = "Raw string input for the original custom tool. Preserve formatting exactly and follow the original tool definition embedded in the description."
	customToolMetadataHead = "Original tool definition:"
	chatToolNameMaxLen     = 64
	toolResultErrorMarker  = "[cc-switch:tool-result-error]"
	toolResultMediaMarker  = "[cc-switch: tool result media attached as native media]"
	wholeDataURLMinBytes   = 8 * 1024
	base64ishMinBytes      = 16 * 1024
	maxMediaTraversalDepth = 32
)

// toolKind tool category (decides which Responses item kind an output item translates into).
type toolKind int

const (
	tkFunction   toolKind = iota // Ordinary function tool
	tkNamespace                  // MCP namespace sub-tool (upstream name is the flattened name)
	tkCustom                     // Codex freeform tool (input is a bare string)
	tkToolSearch                 // tool_search proxy tool
)

// toolSpec records an upstream tool name's original identity (for unpacking tool_use blocks back into Responses items).
type toolSpec struct {
	kind      toolKind
	name      string // Original tool name (sub-tool name for namespace tools)
	namespace string // Non-empty only for namespace tools
}

// toolRegistry is one request's tool registry: built at request translation, used for unpacking at response/stream translation.
// The zero value is usable (empty registry: all tools treated as ordinary functions).
type toolRegistry struct {
	specs   map[string]toolSpec // upstream tool name → identity
	nsIndex map[string]string   // namespace\x00sub-tool-name → upstream tool name
	tools   []interface{}       // Converted Anthropic tools (in declaration order)
}

// buildToolRegistry converts a Responses tools array into Anthropic tools and builds the registry.
// Mirrors cc-switch build_codex_tool_context_from_request + chat_tool_to_anthropic_tool.
// Additionally supports web_search/web_search_preview → web_search_20250305 (cc-switch drops them outright;
// we map them to Anthropic's managed search tool — a superset behavior).
func buildToolRegistry(tools []interface{}) *toolRegistry {
	reg := &toolRegistry{
		specs:   map[string]toolSpec{},
		nsIndex: map[string]string{},
	}
	for _, t := range tools {
		// String form = custom tool (the name is the string itself).
		if name, ok := t.(string); ok {
			reg.addCustomTool(map[string]interface{}{"type": "custom", "name": name})
			continue
		}
		tm := asObj(t)
		if tm == nil {
			continue
		}
		switch objStr(tm, "type") {
		case "function":
			reg.addFunctionTool(tm, "")
		case "custom":
			reg.addCustomTool(tm)
		case "tool_search":
			reg.addToolSearchTool()
		case "namespace":
			reg.addNamespaceTool(tm)
		case "web_search", "web_search_preview":
			tool := map[string]interface{}{"type": "web_search_20250305", "name": "web_search"}
			if mu := toInt64(tm["max_uses"]); mu > 0 {
				tool["max_uses"] = mu
			}
			reg.tools = append(reg.tools, tool)
		}
	}
	return reg
}

// addTool registers a tool (deduped by upstream name; empty names skipped).
func (reg *toolRegistry) addTool(chatName string, spec toolSpec, anthTool map[string]interface{}) {
	if strings.TrimSpace(chatName) == "" {
		return
	}
	if _, seen := reg.specs[chatName]; seen {
		return
	}
	reg.specs[chatName] = spec
	if spec.namespace != "" {
		reg.nsIndex[spec.namespace+"\x00"+spec.name] = chatName
	}
	reg.tools = append(reg.tools, anthTool)
}

// addFunctionTool registers a function tool; flattens the name when namespace is non-empty (MCP sub-tool).
func (reg *toolRegistry) addFunctionTool(tool map[string]interface{}, namespace string) {
	name := responsesToolName(tool)
	if name == "" {
		return
	}
	chatName := name
	kind := tkFunction
	if namespace != "" {
		chatName = flattenNamespaceToolName(namespace, name)
		kind = tkNamespace
	}
	// parameters normalization: must be an object-type schema (Anthropic strictly validates).
	schema := asObj(tool["parameters"])
	if len(schema) == 0 {
		schema = map[string]interface{}{"type": "object", "properties": map[string]interface{}{}}
	}
	if objStr(schema, "type") != "object" {
		schema["type"] = "object"
	}
	anthTool := map[string]interface{}{"name": chatName, "input_schema": schema}
	if d := objStr(tool, "description"); d != "" {
		anthTool["description"] = d
	}
	if strict, ok := tool["strict"]; ok {
		anthTool["strict"] = strict
	}
	reg.addTool(chatName, toolSpec{kind: kind, name: name, namespace: namespace}, anthTool)
}

// addCustomTool registers a custom tool: wrapped into an {"input": string} JSON Schema,
// the original tool definition (including format syntax) inlined as JSON into the description for the model to follow.
func (reg *toolRegistry) addCustomTool(tool map[string]interface{}) {
	name := responsesToolName(tool)
	if name == "" {
		return
	}
	desc := customToolMetadataHead + "\n```json\n" + canonicalJSON(tool) + "\n```"
	anthTool := map[string]interface{}{
		"name":        name,
		"description": desc,
		"input_schema": map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				customToolInputField: map[string]interface{}{
					"type":        "string",
					"description": customToolInputDesc,
				},
			},
			"required": []interface{}{customToolInputField},
		},
	}
	reg.addTool(name, toolSpec{kind: tkCustom, name: name}, anthTool)
}

// addToolSearchTool registers the tool_search proxy tool (fixed name and parameter shape).
func (reg *toolRegistry) addToolSearchTool() {
	anthTool := map[string]interface{}{
		"name":        toolSearchProxyName,
		"description": "Search and load Codex tools, plugins, connectors, and MCP namespaces for the current task.",
		"input_schema": map[string]interface{}{
			"type": "object",
			"properties": map[string]interface{}{
				"query": map[string]interface{}{
					"type":        "string",
					"description": "Search query for tools or connectors to load.",
				},
				"limit": map[string]interface{}{
					"type":        "integer",
					"description": "Maximum number of tool groups to return.",
				},
			},
			"required": []interface{}{"query"},
		},
	}
	reg.addTool(toolSearchProxyName, toolSpec{kind: tkToolSearch, name: toolSearchProxyName}, anthTool)
}

// addNamespaceTool registers an MCP namespace: each sub-tool (type=function entries in the tools/children array)
// is flattened one by one into an ns__name ordinary function tool.
func (reg *toolRegistry) addNamespaceTool(tool map[string]interface{}) {
	namespace := objStr(tool, "name")
	if namespace == "" {
		return
	}
	children := asArr(tool["tools"])
	if children == nil {
		children = asArr(tool["children"])
	}
	for _, c := range children {
		cm := asObj(c)
		if objStr(cm, "type") == "function" {
			reg.addFunctionTool(cm, namespace)
		}
	}
}

// responsesToolName extracts a tool name (tolerating both nested function form and flat form).
func responsesToolName(tool map[string]interface{}) string {
	if n := objStr(asObj(tool["function"]), "name"); n != "" {
		return strings.TrimSpace(n)
	}
	return strings.TrimSpace(objStr(tool, "name"))
}

// flattenNamespaceToolName flattens namespace__name; over 64 bytes it's truncated prefix + "__" +
// first 8 bytes of sha256 hex (16 chars) for uniqueness (mirrors cc-switch's same-named function).
func flattenNamespaceToolName(namespace, name string) string {
	full := namespace + "__" + name
	if len(full) <= chatToolNameMaxLen {
		return full
	}
	sum := sha256.Sum256([]byte(full))
	suffix := "__" + hex.EncodeToString(sum[:8])
	prefixLen := chatToolNameMaxLen - len(suffix)
	var b strings.Builder
	used := 0
	for _, r := range full {
		w := utf8.RuneLen(r)
		if used+w > prefixLen {
			break
		}
		b.WriteRune(r)
		used += w
	}
	return b.String() + suffix
}

// lookup queries an identity by upstream tool name.
func (reg *toolRegistry) lookup(chatName string) (toolSpec, bool) {
	if reg == nil {
		return toolSpec{}, false
	}
	spec, ok := reg.specs[chatName]
	return spec, ok
}

// isCustomTool reports whether an upstream tool name is a custom tool.
func (reg *toolRegistry) isCustomTool(chatName string) bool {
	spec, ok := reg.lookup(chatName)
	return ok && spec.kind == tkCustom
}

// chatNameForFunction resolves a client function_call's (name, namespace) back to the upstream tool name:
// registered names hit the table; unregistered ones are recomputed per the flattening rule (names outside the registry in replayed history also match).
func (reg *toolRegistry) chatNameForFunction(name, namespace string) string {
	if namespace == "" {
		return name
	}
	if reg != nil {
		if chatName, ok := reg.nsIndex[namespace+"\x00"+name]; ok {
			return chatName
		}
	}
	return flattenNamespaceToolName(namespace, name)
}

// canonicalJSON serializes to canonical JSON (map keys sorted — Go encoding/json is naturally ordered;
// HTML escaping off, aligned with cc-switch canonical_json_string's output habits).
func canonicalJSON(v interface{}) string {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return ""
	}
	return strings.TrimSuffix(buf.String(), "\n")
}

// sanitizeToolUseInput strips the pages:"" Anthropic models attach to Read tool calls
// (a known quirk; same workaround as cc-switch sanitize_anthropic_tool_use_input).
func sanitizeToolUseInput(name string, input map[string]interface{}) map[string]interface{} {
	if name != "Read" || input == nil {
		return input
	}
	if p, ok := input["pages"].(string); ok && p == "" {
		delete(input, "pages")
	}
	return input
}

// sanitizeToolUseInputJSON same as above, on an unparsed JSON string (used at streaming close-out).
func sanitizeToolUseInputJSON(name, raw string) string {
	if name != "Read" || raw == "" {
		return raw
	}
	var input map[string]interface{}
	if err := json.Unmarshal([]byte(raw), &input); err != nil {
		return raw
	}
	return canonicalJSON(sanitizeToolUseInput(name, input))
}

// canonicalizeToolArguments re-serializes canonically after parsing; returns as-is on parse failure
// (mirrors cc-switch canonicalize_tool_arguments_str).
func canonicalizeToolArguments(raw string) string {
	var v interface{}
	if err := json.Unmarshal([]byte(raw), &v); err != nil {
		return raw
	}
	return canonicalJSON(v)
}

// customToolInputFromArguments unwraps a custom tool's bare input
// string from the wrapped arguments JSON; on failure (not an object / no input string field) returns as-is.
func customToolInputFromArguments(arguments string) string {
	if strings.TrimSpace(arguments) == "" {
		return ""
	}
	var v interface{}
	if err := json.Unmarshal([]byte(arguments), &v); err != nil {
		return arguments
	}
	if m, ok := v.(map[string]interface{}); ok {
		if s, ok := m[customToolInputField].(string); ok {
			return s
		}
	}
	return arguments
}

// parseToolArgumentsObject parses arguments into an object (for tool_search_call items);
// empty string yields {}, parse failure wraps into {"query": original text}.
func parseToolArgumentsObject(arguments string) interface{} {
	if strings.TrimSpace(arguments) == "" {
		return map[string]interface{}{}
	}
	var v interface{}
	if err := json.Unmarshal([]byte(arguments), &v); err == nil {
		if m, ok := v.(map[string]interface{}); ok {
			return m
		}
	}
	return map[string]interface{}{"query": arguments}
}

// toolCallItemID generates a tool-call output item id: ctc_ prefix for custom, fc_ for the rest
// (mirrors cc-switch response_tool_call_item_id_from_chat_name).
func toolCallItemID(reg *toolRegistry, callID, chatName string) string {
	if reg.isCustomTool(chatName) {
		return "ctc_" + callID
	}
	return "fc_" + callID
}

// toolCallItemFromRegistry translates an upstream tool_use block into the corresponding Responses
// output item per the tool identity (mirrors cc-switch response_tool_call_item_from_chat_name):
// toolSearch → tool_search_call; custom → custom_tool_call (bare input unwrapped);
// namespace → function_call with the namespace field and the sub-tool name restored; the rest → ordinary function_call.
func toolCallItemFromRegistry(reg *toolRegistry, itemID, status, callID, chatName, arguments string) map[string]interface{} {
	spec, ok := reg.lookup(chatName)
	if ok {
		switch {
		case spec.kind == tkToolSearch:
			return map[string]interface{}{
				"type":      "tool_search_call",
				"call_id":   callID,
				"status":    status,
				"execution": "client",
				"arguments": parseToolArgumentsObject(arguments),
			}
		case spec.kind == tkCustom:
			return map[string]interface{}{
				"id":      itemID,
				"type":    "custom_tool_call",
				"status":  status,
				"call_id": callID,
				"name":    spec.name,
				"input":   customToolInputFromArguments(arguments),
			}
		case spec.namespace != "":
			item := functionCallItem(itemID, status, callID, spec.name, arguments)
			item["namespace"] = spec.namespace
			return item
		}
	}
	return functionCallItem(itemID, status, callID, chatName, arguments)
}

// documentBlockFromInputFile converts a Responses input_file into an Anthropic document block
// (file_url → url source; file_data data URL → base64 source; filename into title).
func documentBlockFromInputFile(part map[string]interface{}) map[string]interface{} {
	if part == nil {
		return nil
	}
	filename := objStr(part, "filename")
	var block map[string]interface{}
	if url := objStr(part, "file_url"); strings.HasPrefix(url, "http://") || strings.HasPrefix(url, "https://") {
		block = map[string]interface{}{
			"type":   "document",
			"source": map[string]interface{}{"type": "url", "url": url},
		}
	} else {
		fileData := objStr(part, "file_data")
		if !strings.HasPrefix(fileData, "data:") {
			return nil
		}
		rest := fileData[len("data:"):]
		comma := strings.IndexByte(rest, ',')
		if comma < 0 {
			return nil
		}
		meta, data := rest[:comma], rest[comma+1:]
		if data == "" {
			return nil
		}
		mediaType := strings.SplitN(meta, ";", 2)[0]
		if mediaType == "" {
			mediaType = "application/pdf"
		}
		block = map[string]interface{}{
			"type": "document",
			"source": map[string]interface{}{
				"type": "base64", "media_type": mediaType, "data": data,
			},
		}
	}
	if filename != "" {
		block["title"] = filename
	}
	return block
}

// ---- Tool-result media stripping (mirrors cc-switch tool_media.rs, ImagesOnly scope) ----

// normalizedImageURL normalizes the image_url field into a {url: ...} object (string form wrapped once).
func normalizedImageURL(part map[string]interface{}) map[string]interface{} {
	switch u := part["image_url"].(type) {
	case string:
		if strings.TrimSpace(u) == "" {
			return nil
		}
		return map[string]interface{}{"url": u}
	case map[string]interface{}:
		if strings.TrimSpace(objStr(u, "url")) == "" {
			return nil
		}
		return u
	}
	return nil
}

// isImageMimeType reports whether a MIME type has the image/ prefix (case-insensitive).
func isImageMimeType(v string) bool {
	return len(v) >= 6 && strings.EqualFold(v[:6], "image/")
}

// imageMediaPartFromToolPart recognizes whether a tool-result node is image media, and if so normalizes it into
// {type:"image_url", image_url:{url}} (mirrors cc-switch chat_media_part_from_tool_part's
// image branch: input_image/image_url, Anthropic image+source, MCP image+data+mimeType,
// type-less loose image_url data URLs).
func imageMediaPartFromToolPart(part map[string]interface{}) map[string]interface{} {
	wrap := func(url string) map[string]interface{} {
		return map[string]interface{}{"type": "image_url", "image_url": map[string]interface{}{"url": url}}
	}
	dataURL := func(mediaType, data string) string {
		// data already being a data URL is used directly; otherwise wrapped as data:<mt>;base64,<data>.
		if len(data) >= 11 && strings.EqualFold(data[:11], "data:image/") {
			return data
		}
		return "data:" + mediaType + ";base64," + data
	}
	switch objStr(part, "type") {
	case "input_image", "image_url":
		if u := normalizedImageURL(part); u != nil {
			return map[string]interface{}{"type": "image_url", "image_url": u}
		}
	case "image":
		if src := asObj(part["source"]); src != nil {
			// A missing media_type counts as an image; when present it must have the image/ prefix.
			mt := objStr(src, "media_type")
			if mt == "" {
				mt = objStr(src, "mime_type")
			}
			if mt == "" {
				mt = objStr(src, "mimeType")
			}
			if mt == "" || isImageMimeType(mt) {
				if url := objStr(src, "url"); strings.TrimSpace(url) != "" {
					return wrap(url)
				}
				if data := objStr(src, "data"); data != "" {
					if mt == "" {
						mt = "image/png"
					}
					return wrap(dataURL(mt, data))
				}
			}
			return nil
		}
		if data := objStr(part, "data"); data != "" {
			mt := objStr(part, "mimeType")
			if mt == "" {
				mt = objStr(part, "mime_type")
			}
			if mt != "" && isImageMimeType(mt) {
				return wrap(dataURL(mt, data))
			}
		}
	case "":
		// Loose shape: no type but carries image_url as a data: URL.
		if u := normalizedImageURL(part); u != nil {
			if url := objStr(u, "url"); len(url) >= 5 && strings.EqualFold(url[:5], "data:") {
				return map[string]interface{}{"type": "image_url", "image_url": u}
			}
		}
	}
	return nil
}

// wholeStringImageDataURL recognizes a string that is entirely one image data URL (≥8KB and base64-encoded).
func wholeStringImageDataURL(s string) string {
	trimmed := strings.TrimSpace(s)
	if len(trimmed) < wholeDataURLMinBytes {
		return ""
	}
	comma := strings.IndexByte(trimmed, ',')
	if comma < 0 {
		return ""
	}
	header := strings.ToLower(trimmed[:comma])
	if strings.HasPrefix(header, "data:image/") && strings.HasSuffix(header, ";base64") {
		return trimmed
	}
	return ""
}

// looksLikeBase64Payload roughly judges whether a string looks like a base64 payload (≥16KB of pure base64 characters).
func looksLikeBase64Payload(s string) bool {
	if len(s) < base64ishMinBytes {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '+' || c == '/' || c == '=') {
			return false
		}
	}
	return true
}

// clampBase64ishStrings replaces leftover data:/base64 long strings with an elision marker
// inside structures already confirmed to contain media (mirrors cc-switch clamp_base64ish_strings).
func clampBase64ishStrings(v interface{}) {
	switch t := v.(type) {
	case []interface{}:
		for i, item := range t {
			if s, ok := item.(string); ok {
				t[i] = clampedString(s)
			} else {
				clampBase64ishStrings(item)
			}
		}
	case map[string]interface{}:
		for k, item := range t {
			if s, ok := item.(string); ok {
				t[k] = clampedString(s)
			} else {
				clampBase64ishStrings(item)
			}
		}
	}
}

func clampedString(s string) string {
	trimmed := strings.TrimSpace(s)
	isDataURL := len(trimmed) >= wholeDataURLMinBytes && len(trimmed) >= 5 && strings.EqualFold(trimmed[:5], "data:")
	if isDataURL || looksLikeBase64Payload(trimmed) {
		return "[cc-switch: omitted " + strconv.Itoa(len(s)) + " bytes]"
	}
	return s
}

// stripMediaFromToolValue recursively strips image-media nodes from a value: media nodes are replaced by marker blocks,
// normalized into mediaParts; JSON strings are parsed and recursed, re-serialized when replacements happened.
// Returns the cleaned value and the replacement count (mirrors cc-switch strip_media_from_tool_value_at_depth).
func stripMediaFromToolValue(v interface{}, mediaParts *[]interface{}, depth int) (interface{}, int) {
	if depth > maxMediaTraversalDepth {
		return v, 0
	}
	switch t := v.(type) {
	case string:
		if url := wholeStringImageDataURL(t); url != "" {
			*mediaParts = append(*mediaParts, map[string]interface{}{
				"type": "image_url", "image_url": map[string]interface{}{"url": url},
			})
			return toolResultMediaMarker, 1
		}
		trimmed := strings.TrimSpace(t)
		if trimmed == "" {
			return v, 0
		}
		var parsed interface{}
		if err := json.Unmarshal([]byte(trimmed), &parsed); err != nil {
			return v, 0
		}
		cleaned, n := stripMediaFromToolValue(parsed, mediaParts, depth+1)
		if n == 0 {
			return v, 0
		}
		clampBase64ishStrings(cleaned)
		return canonicalJSON(cleaned), n
	case []interface{}:
		total := 0
		for i, item := range t {
			cleaned, n := stripMediaFromToolValue(item, mediaParts, depth+1)
			if n > 0 {
				t[i] = cleaned
				total += n
			}
		}
		return t, total
	case map[string]interface{}:
		if part := imageMediaPartFromToolPart(t); part != nil {
			*mediaParts = append(*mediaParts, part)
			return map[string]interface{}{"type": "input_text", "text": toolResultMediaMarker}, 1
		}
		if content, ok := t["content"]; ok {
			cleaned, n := stripMediaFromToolValue(content, mediaParts, depth+1)
			if n > 0 {
				t["content"] = cleaned
				return t, n
			}
		}
		return t, 0
	}
	return v, 0
}

// appendSanitizedToolResultValue flattens a cleaned value into Anthropic text blocks
// (mirrors cc-switch append_sanitized_tool_result_value): strings/text parts go to text,
// error markers set is_error, other shapes serialize into JSON text.
func appendSanitizedToolResultValue(v interface{}, content *[]interface{}, isError *bool) {
	switch t := v.(type) {
	case string:
		if t == toolResultErrorMarker {
			*isError = true
		} else if t != "" {
			*content = append(*content, map[string]interface{}{"type": "text", "text": t})
		}
	case []interface{}:
		for _, p := range t {
			pm := asObj(p)
			switch objStr(pm, "type") {
			case "input_text", "output_text", "text":
				if txt := objStr(pm, "text"); txt == toolResultErrorMarker {
					*isError = true
				} else if txt != "" {
					*content = append(*content, map[string]interface{}{"type": "text", "text": txt})
				}
			default:
				*content = append(*content, map[string]interface{}{"type": "text", "text": canonicalJSON(p)})
			}
		}
	case map[string]interface{}:
		switch objStr(t, "type") {
		case "input_text", "output_text", "text":
			if txt := objStr(t, "text"); txt == toolResultErrorMarker {
				*isError = true
			} else if txt != "" {
				*content = append(*content, map[string]interface{}{"type": "text", "text": txt})
			}
		default:
			*content = append(*content, map[string]interface{}{"type": "text", "text": canonicalJSON(t)})
		}
	default:
		*content = append(*content, map[string]interface{}{"type": "text", "text": canonicalJSON(t)})
	}
}

// alternateImageToolResultContent tries converting a tool-result value containing image media into
// [text..., image...] Anthropic content blocks; when no media is recognized, ok=false lets the caller take the as-is path
// (mirrors cc-switch alternate_image_tool_result_content).
func alternateImageToolResultContent(v interface{}) (content []interface{}, isError bool, ok bool) {
	var mediaParts []interface{}
	cleaned, n := stripMediaFromToolValue(v, &mediaParts, 0)
	if n == 0 {
		return nil, false, false
	}
	if s, isStr := cleaned.(string); isStr {
		cleaned = clampedString(s)
	} else {
		clampBase64ishStrings(cleaned)
	}
	appendSanitizedToolResultValue(cleaned, &content, &isError)
	for _, p := range mediaParts {
		if b := imageBlockFromInputImage(asObj(p)); b != nil {
			content = append(content, b)
		}
	}
	return content, isError, true
}
