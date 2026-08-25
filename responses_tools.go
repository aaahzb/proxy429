// responses_tools.go — Responses API 工具体系的翻译注册表。
// 对照 cc-switch 3.20.0 的 CodexToolContext（transform_codex_chat.rs）与
// tool_media.rs（工具结果媒体剥离）。四类工具：function（普通函数）、
// namespace（MCP 命名空间，拍平成 ns__name）、custom（Codex freeform 工具，
// 包装成 {"input": string} 的 JSON Schema）、toolSearch（固定代理工具）。
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

// 对照 cc-switch 同名常量。标记字符串沿用原文，客户端/模型侧可能按字面匹配。
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

// toolKind 工具类别（决定输出项翻译成哪种 Responses item）。
type toolKind int

const (
	tkFunction   toolKind = iota // 普通函数工具
	tkNamespace                  // MCP 命名空间子工具（上游名是拍平名）
	tkCustom                     // Codex freeform 工具（输入是裸字符串）
	tkToolSearch                 // tool_search 代理工具
)

// toolSpec 记录一个上游工具名的原始身份（用于把 tool_use 块翻译回 Responses 项时拆包）。
type toolSpec struct {
	kind      toolKind
	name      string // 原始工具名（namespace 工具是子工具名）
	namespace string // 仅 namespace 工具非空
}

// toolRegistry 一个请求的工具注册表：请求翻译时建立，响应/流式翻译时据此拆包。
// 零值可用（空注册表：所有工具按普通 function 处理）。
type toolRegistry struct {
	specs   map[string]toolSpec // 上游工具名 → 身份
	nsIndex map[string]string   // namespace\x00子工具名 → 上游工具名
	tools   []interface{}       // 转换后的 Anthropic tools（按声明顺序）
}

// buildToolRegistry 把 Responses tools 数组转换成 Anthropic tools 并建立注册表。
// 对照 cc-switch build_codex_tool_context_from_request + chat_tool_to_anthropic_tool。
// 额外支持 web_search/web_search_preview → web_search_20250305（cc-switch 直接丢弃，
// 我们映射成 Anthropic 托管搜索工具，是超集行为）。
func buildToolRegistry(tools []interface{}) *toolRegistry {
	reg := &toolRegistry{
		specs:   map[string]toolSpec{},
		nsIndex: map[string]string{},
	}
	for _, t := range tools {
		// 字符串形式 = custom 工具（名字即字符串）。
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

// addTool 注册一个工具（按上游名去重，名字空跳过）。
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

// addFunctionTool 注册 function 工具；namespace 非空时拍平名字（MCP 子工具）。
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
	// parameters 规整：必须是 object 类型 schema（Anthropic 强校验）。
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

// addCustomTool 注册 custom 工具：包装成 {"input": string} 的 JSON Schema，
// 原始工具定义（含 format 语法）以 JSON 形式内嵌进 description 供模型遵循。
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

// addToolSearchTool 注册 tool_search 代理工具（固定名字与参数形状）。
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

// addNamespaceTool 注册 MCP namespace：子工具（tools/children 数组里 type=function 的）
// 逐个拍平成 ns__name 的普通函数工具。
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

// responsesToolName 取工具名（兼容 function 嵌套形与扁平形）。
func responsesToolName(tool map[string]interface{}) string {
	if n := objStr(asObj(tool["function"]), "name"); n != "" {
		return strings.TrimSpace(n)
	}
	return strings.TrimSpace(objStr(tool, "name"))
}

// flattenNamespaceToolName 拍平 namespace__name；超 64 字节截断前缀 + "__" +
// sha256 前 8 字节 hex（16 字符）保证唯一（对照 cc-switch 同名函数）。
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

// lookup 按上游工具名查身份。
func (reg *toolRegistry) lookup(chatName string) (toolSpec, bool) {
	if reg == nil {
		return toolSpec{}, false
	}
	spec, ok := reg.specs[chatName]
	return spec, ok
}

// isCustomTool 判断上游工具名是不是 custom 工具。
func (reg *toolRegistry) isCustomTool(chatName string) bool {
	spec, ok := reg.lookup(chatName)
	return ok && spec.kind == tkCustom
}

// chatNameForFunction 把客户端 function_call 的 (name, namespace) 解析回上游工具名：
// 注册过的查表，没注册的按拍平规则重算（回放历史里注册表外的名字也能对上）。
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

// canonicalJSON 序列化成规范 JSON（map 键排序——Go encoding/json 天然有序；
// 关闭 HTML 转义，与 cc-switch canonical_json_string 的输出习惯对齐）。
func canonicalJSON(v interface{}) string {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return ""
	}
	return strings.TrimSuffix(buf.String(), "\n")
}

// sanitizeToolUseInput 剔除 Anthropic 模型在 Read 工具调用里附加的 pages:""
// （已知怪癖，cc-switch sanitize_anthropic_tool_use_input 同款 workaround）。
func sanitizeToolUseInput(name string, input map[string]interface{}) map[string]interface{} {
	if name != "Read" || input == nil {
		return input
	}
	if p, ok := input["pages"].(string); ok && p == "" {
		delete(input, "pages")
	}
	return input
}

// sanitizeToolUseInputJSON 同上，作用于未解析的 JSON 字符串（流式收拢时用）。
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

// canonicalizeToolArguments 解析后重新规范序列化；解析失败原样返回
// （对照 cc-switch canonicalize_tool_arguments_str）。
func canonicalizeToolArguments(raw string) string {
	var v interface{}
	if err := json.Unmarshal([]byte(raw), &v); err != nil {
		return raw
	}
	return canonicalJSON(v)
}

// customToolInputFromArguments 从包装后的 arguments JSON 解出 custom 工具的裸输入
// 字符串；解不出（不是对象/没有 input 字符串字段）时原样返回。
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

// parseToolArgumentsObject 把 arguments 解析成对象（tool_search_call 项用）；
// 空串给 {}，解析失败包成 {"query": 原文}。
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

// toolCallItemID 生成工具调用输出项 id：custom=ctc_ 前缀，其他=fc_ 前缀
// （对照 cc-switch response_tool_call_item_id_from_chat_name）。
func toolCallItemID(reg *toolRegistry, callID, chatName string) string {
	if reg.isCustomTool(chatName) {
		return "ctc_" + callID
	}
	return "fc_" + callID
}

// toolCallItemFromRegistry 按工具身份把上游 tool_use 块翻译成对应的 Responses
// 输出项（对照 cc-switch response_tool_call_item_from_chat_name）：
// toolSearch → tool_search_call；custom → custom_tool_call（解包裸输入）；
// namespace → 带 namespace 字段、还原子工具名的 function_call；其余 → 普通 function_call。
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

// documentBlockFromInputFile 把 Responses input_file 转成 Anthropic document 块
// （file_url → url 源；file_data data URL → base64 源；filename 进 title）。
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

// ---- 工具结果媒体剥离（对照 cc-switch tool_media.rs，ImagesOnly 范围）----

// normalizedImageURL 规整 image_url 字段为 {url: ...} 对象（字符串形式包一层）。
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

// isImageMimeType 判断 MIME 类型是否 image/ 前缀（大小写不敏感）。
func isImageMimeType(v string) bool {
	return len(v) >= 6 && strings.EqualFold(v[:6], "image/")
}

// imageMediaPartFromToolPart 识别工具结果节点是否是图片媒体，是则归一化成
// {type:"image_url", image_url:{url}}（对照 cc-switch chat_media_part_from_tool_part
// 的图片分支：input_image/image_url、Anthropic image+source、MCP image+data+mimeType、
// 无 type 的松散 image_url data URL）。
func imageMediaPartFromToolPart(part map[string]interface{}) map[string]interface{} {
	wrap := func(url string) map[string]interface{} {
		return map[string]interface{}{"type": "image_url", "image_url": map[string]interface{}{"url": url}}
	}
	dataURL := func(mediaType, data string) string {
		// data 本身已是 data URL 就直接用，否则包成 data:<mt>;base64,<data>。
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
			// media_type 缺席视为图片，在场必须 image/ 前缀。
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
		// 松散形状：没有 type 但带 image_url 且是 data: URL。
		if u := normalizedImageURL(part); u != nil {
			if url := objStr(u, "url"); len(url) >= 5 && strings.EqualFold(url[:5], "data:") {
				return map[string]interface{}{"type": "image_url", "image_url": u}
			}
		}
	}
	return nil
}

// wholeStringImageDataURL 识别整串就是一个图片 data URL（≥8KB 且 base64 编码）的字符串。
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

// looksLikeBase64Payload 粗判字符串是否像 base64 载荷（≥16KB 且全是 base64 字符）。
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

// clampBase64ishStrings 在已确认含媒体的结构里，把残留的 data:/base64 长串
// 替换成省略标记（对照 cc-switch clamp_base64ish_strings）。
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

// stripMediaFromToolValue 递归剥离值里的图片媒体节点：媒体节点被标记块替换、
// 归一化后进 mediaParts；JSON 字符串会解析后递归并在有替换时重新序列化。
// 返回清理后的值与替换次数（对照 cc-switch strip_media_from_tool_value_at_depth）。
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

// appendSanitizedToolResultValue 把清理后的值摊平成 Anthropic text 块
// （对照 cc-switch append_sanitized_tool_result_value）：字符串/文本型部件进文本，
// error marker 置 is_error，其余形状序列化成 JSON 文本。
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

// alternateImageToolResultContent 尝试把含图片媒体的工具结果值转换成
// [text..., image...] 的 Anthropic 内容块；没识别到媒体时 ok=false 让调用方走原样路径
// （对照 cc-switch alternate_image_tool_result_content）。
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
