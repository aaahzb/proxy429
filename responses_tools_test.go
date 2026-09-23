// responses_tools_test.go — tests for tool-system translation, porting cc-switch's key cases:
// custom tool wrap/unwrap, namespace(MCP) flattening and restoration, tool_search, Read sanitize,
// tool-result media stripping (MCP images / JSON-string nesting), error markers, input_file, usage.
package main

// cc-switch: https://github.com/farion1231/cc-switch — MIT License, Copyright (c) 2025 Jason Young.

import (
	"encoding/json"
	"strings"
	"testing"
)

// ---- custom tools ----

// Mirrors cc-switch test_request_custom_tool_survives_with_required_choice.
func TestCustomToolWrappedAndChoicePreserved(t *testing.T) {
	body := map[string]interface{}{
		"model": "gpt-5-codex",
		"input": "hi",
		"tools": []interface{}{
			map[string]interface{}{
				"type":        "custom",
				"name":        "apply_patch",
				"description": "Apply a patch",
				"format":      map[string]interface{}{"type": "grammar", "syntax": "lark", "definition": "start: ..."},
			},
		},
		"tool_choice": "required",
	}
	out, reg, err := responsesToAnthropic(body)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	tools := out["tools"].([]interface{})
	if len(tools) != 1 {
		t.Fatalf("tools 数=%d，应=1", len(tools))
	}
	tool := tools[0].(map[string]interface{})
	if tool["name"] != "apply_patch" {
		t.Errorf("工具名=%v", tool["name"])
	}
	schema := tool["input_schema"].(map[string]interface{})
	props := schema["properties"].(map[string]interface{})
	if _, ok := props["input"]; !ok {
		t.Errorf("包装 schema 缺 input 字段: %v", schema)
	}
	required := schema["required"].([]interface{})
	if required[0] != "input" {
		t.Errorf("required=%v", required)
	}
	// The original tool definition (including format) must be inlined into the description, so the model can follow the syntax.
	desc := tool["description"].(string)
	if !strings.Contains(desc, "Original tool definition:") || !strings.Contains(desc, "lark") {
		t.Errorf("description 未内嵌原始定义: %v", desc)
	}
	// required → any preserved.
	tc := out["tool_choice"].(map[string]interface{})
	if tc["type"] != "any" {
		t.Errorf("tool_choice=%v", tc)
	}
	if !reg.isCustomTool("apply_patch") {
		t.Errorf("注册表未标记 custom")
	}
}

// String-form tool name = custom tool (mirrors cc-switch add_response_tool's String branch).
func TestStringToolBecomesCustom(t *testing.T) {
	reg := buildToolRegistry([]interface{}{"apply_patch"})
	if !reg.isCustomTool("apply_patch") {
		t.Fatalf("字符串工具应注册为 custom")
	}
	if len(reg.tools) != 1 {
		t.Fatalf("tools 数=%d", len(reg.tools))
	}
}

// custom_tool_call replay → tool_use wrapped {"input": raw value}; custom_tool_call_output → tool_result.
func TestCustomToolCallReplay(t *testing.T) {
	body := map[string]interface{}{
		"model": "m",
		"tools": []interface{}{map[string]interface{}{"type": "custom", "name": "apply_patch"}},
		"input": []interface{}{
			map[string]interface{}{"type": "message", "role": "user", "content": "改个文件"},
			map[string]interface{}{
				"type": "custom_tool_call", "call_id": "c1", "name": "apply_patch",
				"input": "*** Begin Patch\n*** End Patch",
			},
			map[string]interface{}{
				"type": "custom_tool_call_output", "call_id": "c1", "output": "ok",
			},
		},
	}
	out, _, err := responsesToAnthropic(body)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	msgs := out["messages"].([]map[string]interface{})
	// user + assistant(tool_use) + user(tool_result)
	if len(msgs) != 3 {
		t.Fatalf("messages 数=%d: %v", len(msgs), msgs)
	}
	tu := msgs[1]["content"].([]interface{})[0].(map[string]interface{})
	if tu["type"] != "tool_use" || tu["name"] != "apply_patch" {
		t.Fatalf("tool_use=%v", tu)
	}
	input := tu["input"].(map[string]interface{})
	if input["input"] != "*** Begin Patch\n*** End Patch" {
		t.Errorf("包装 input=%v", input)
	}
	tr := msgs[2]["content"].([]interface{})[0].(map[string]interface{})
	if tr["type"] != "tool_result" || tr["tool_use_id"] != "c1" {
		t.Errorf("tool_result=%v", tr)
	}
}

// incomplete custom_tool_call dropped wholesale (mirrors cc-switch's uniform handling of the three call kinds).
func TestIncompleteCustomToolCallDropped(t *testing.T) {
	body := map[string]interface{}{
		"model": "m",
		"input": []interface{}{
			map[string]interface{}{"type": "message", "role": "user", "content": "hi"},
			map[string]interface{}{
				"type": "custom_tool_call", "call_id": "c1", "name": "apply_patch",
				"status": "incomplete", "input": "*** Begin",
			},
		},
	}
	out, _, err := responsesToAnthropic(body)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	msgs := out["messages"].([]map[string]interface{})
	if len(msgs) != 1 {
		t.Fatalf("incomplete 调用应被丢弃，messages=%v", msgs)
	}
}

// Go version of custom_tool_input_from_chat_arguments: unwraps the bare string from the wrapped JSON.
func TestCustomToolInputUnwrap(t *testing.T) {
	if got := customToolInputFromArguments(`{"input":"raw text"}`); got != "raw text" {
		t.Errorf("解包=%q", got)
	}
	if got := customToolInputFromArguments(`{"other":1}`); got != `{"other":1}` {
		t.Errorf("无 input 字段应原样返回，=%q", got)
	}
	if got := customToolInputFromArguments("not json"); got != "not json" {
		t.Errorf("非 JSON 应原样返回，=%q", got)
	}
	if got := customToolInputFromArguments(""); got != "" {
		t.Errorf("空串=%q", got)
	}
}

// ---- namespace(MCP) tools ----

// Mirrors cc-switch's mcp_files case: namespace flattened to mcp_files__read, responses split back
// into a function_call with the namespace field.
func TestNamespaceToolRoundTrip(t *testing.T) {
	body := map[string]interface{}{
		"model": "m",
		"input": "读个文件",
		"tools": []interface{}{
			map[string]interface{}{
				"type": "namespace", "name": "mcp_files",
				"tools": []interface{}{
					map[string]interface{}{
						"type": "function", "name": "read",
						"description": "Read a file",
						"parameters":  map[string]interface{}{"type": "object"},
					},
				},
			},
		},
	}
	out, reg, err := responsesToAnthropic(body)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	tools := out["tools"].([]interface{})
	if len(tools) != 1 {
		t.Fatalf("tools 数=%d", len(tools))
	}
	if tools[0].(map[string]interface{})["name"] != "mcp_files__read" {
		t.Fatalf("拍平名=%v", tools[0])
	}

	// Response side: tool_use mcp_files__read → function_call with name+namespace restored.
	msg := map[string]interface{}{
		"id":   "msg_1",
		"type": "message",
		"content": []interface{}{
			map[string]interface{}{
				"type": "tool_use", "id": "call_1", "name": "mcp_files__read",
				"input": map[string]interface{}{},
			},
		},
	}
	resp := anthropicToResponsesObject(msg, "m", reg, nil, false)
	item := resp["output"].([]interface{})[0].(map[string]interface{})
	if item["type"] != "function_call" || item["name"] != "read" || item["namespace"] != "mcp_files" {
		t.Fatalf("输出项=%v", item)
	}

	// Replay side: function_call with namespace → tool_use flattened name.
	replay := map[string]interface{}{
		"model": "m",
		"tools": body["tools"],
		"input": []interface{}{
			map[string]interface{}{"type": "message", "role": "user", "content": "读"},
			map[string]interface{}{
				"type": "function_call", "call_id": "call_1", "name": "read",
				"namespace": "mcp_files", "arguments": "{}",
			},
			map[string]interface{}{
				"type": "function_call_output", "call_id": "call_1", "output": "内容",
			},
		},
	}
	out2, _, err := responsesToAnthropic(replay)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	msgs := out2["messages"].([]map[string]interface{})
	tu := msgs[1]["content"].([]interface{})[0].(map[string]interface{})
	if tu["name"] != "mcp_files__read" {
		t.Errorf("回放 tool_use 名=%v", tu["name"])
	}
}

// Flattened names over 64 bytes: truncate + sha256 suffix (mirrors cc-switch flatten_namespace_tool_name).
func TestFlattenNamespaceToolNameTruncation(t *testing.T) {
	ns := strings.Repeat("n", 40)
	name := strings.Repeat("t", 40)
	flat := flattenNamespaceToolName(ns, name)
	if len(flat) != chatToolNameMaxLen {
		t.Fatalf("长度=%d，应=%d", len(flat), chatToolNameMaxLen)
	}
	if !strings.HasSuffix(flat[:len(flat)-18], "__") && !strings.Contains(flat, "__") {
		t.Errorf("形状=%q", flat)
	}
	// Same input necessarily same output (deterministic); different inputs get different suffixes.
	if flat != flattenNamespaceToolName(ns, name) {
		t.Errorf("不确定")
	}
	if flat == flattenNamespaceToolName(ns, name+"x") {
		t.Errorf("不同输入撞后缀")
	}
	// Short names not truncated.
	if got := flattenNamespaceToolName("mcp_files", "read"); got != "mcp_files__read" {
		t.Errorf("短名=%q", got)
	}
}

// tool_choice with a namespace resolves back to the flattened name; unknown shapes like allowed_tools degrade to auto.
func TestToolChoiceNamespaceAndUnknown(t *testing.T) {
	reg := buildToolRegistry([]interface{}{
		map[string]interface{}{
			"type": "namespace", "name": "mcp_files",
			"tools": []interface{}{
				map[string]interface{}{"type": "function", "name": "read", "parameters": map[string]interface{}{"type": "object"}},
			},
		},
	})
	tc := mapToolChoiceToAnthropic(map[string]interface{}{
		"type": "function", "name": "read", "namespace": "mcp_files",
	}, reg)
	if tc["type"] != "tool" || tc["name"] != "mcp_files__read" {
		t.Errorf("namespace choice=%v", tc)
	}
	tc = mapToolChoiceToAnthropic(map[string]interface{}{
		"type": "allowed_tools", "tools": []interface{}{},
	}, reg)
	if tc["type"] != "auto" {
		t.Errorf("未知形状应降级 auto，=%v", tc)
	}
	tc = mapToolChoiceToAnthropic(map[string]interface{}{"type": "custom", "name": "apply_patch"}, reg)
	if tc["type"] != "tool" || tc["name"] != "apply_patch" {
		t.Errorf("custom choice=%v", tc)
	}
}

// ---- tool_search ----

func TestToolSearchRoundTrip(t *testing.T) {
	body := map[string]interface{}{
		"model": "m",
		"input": "找工具",
		"tools": []interface{}{map[string]interface{}{"type": "tool_search"}},
	}
	out, reg, err := responsesToAnthropic(body)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	tools := out["tools"].([]interface{})
	if len(tools) != 1 || tools[0].(map[string]interface{})["name"] != "tool_search" {
		t.Fatalf("tools=%v", tools)
	}
	// Response side: tool_use tool_search → tool_search_call item (arguments parsed into an object).
	msg := map[string]interface{}{
		"id": "msg_1", "type": "message",
		"content": []interface{}{
			map[string]interface{}{
				"type": "tool_use", "id": "c1", "name": "tool_search",
				"input": map[string]interface{}{"query": "filesystem"},
			},
		},
	}
	resp := anthropicToResponsesObject(msg, "m", reg, nil, false)
	item := resp["output"].([]interface{})[0].(map[string]interface{})
	if item["type"] != "tool_search_call" || item["execution"] != "client" {
		t.Fatalf("输出项=%v", item)
	}
	args := item["arguments"].(map[string]interface{})
	if args["query"] != "filesystem" {
		t.Errorf("arguments=%v", args)
	}
	// Replay side: tool_search_call → tool_use proxy tool name.
	replay := map[string]interface{}{
		"model": "m",
		"tools": body["tools"],
		"input": []interface{}{
			map[string]interface{}{"type": "message", "role": "user", "content": "找"},
			map[string]interface{}{
				"type": "tool_search_call", "call_id": "c1",
				"arguments": map[string]interface{}{"query": "filesystem"},
			},
			map[string]interface{}{"type": "tool_search_output", "call_id": "c1", "output": "找到 read"},
		},
	}
	out2, _, err := responsesToAnthropic(replay)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	msgs := out2["messages"].([]map[string]interface{})
	tu := msgs[1]["content"].([]interface{})[0].(map[string]interface{})
	if tu["name"] != "tool_search" {
		t.Errorf("回放 tool_use 名=%v", tu["name"])
	}
	tr := msgs[2]["content"].([]interface{})[0].(map[string]interface{})
	if tr["type"] != "tool_result" || tr["tool_use_id"] != "c1" {
		t.Errorf("tool_result=%v", tr)
	}
}

// ---- Read sanitize ----

// Mirrors cc-switch sanitize_anthropic_tool_use_input: stripping the Read tool's pages:"".
func TestReadToolPagesSanitize(t *testing.T) {
	in := map[string]interface{}{"file_path": "/a", "pages": ""}
	out := sanitizeToolUseInput("Read", in)
	if _, ok := out["pages"]; ok {
		t.Errorf("pages 未剔除: %v", out)
	}
	in2 := map[string]interface{}{"file_path": "/a", "pages": "1-3"}
	out2 := sanitizeToolUseInput("Read", in2)
	if out2["pages"] != "1-3" {
		t.Errorf("非空 pages 应保留: %v", out2)
	}
	in3 := map[string]interface{}{"pages": ""}
	sanitizeToolUseInput("Bash", in3)
	if _, ok := in3["pages"]; !ok {
		t.Errorf("非 Read 工具不应动")
	}
	// JSON-string version (for streaming close-out).
	if got := sanitizeToolUseInputJSON("Read", `{"file_path":"/a","pages":""}`); strings.Contains(got, "pages") {
		t.Errorf("JSON 版未剔除: %v", got)
	}
	if got := sanitizeToolUseInputJSON("Read", "not json"); got != "not json" {
		t.Errorf("坏 JSON 应原样: %v", got)
	}
	// Replay path: arguments of function_call name=Read should also be cleaned.
	body := map[string]interface{}{
		"model": "m",
		"input": []interface{}{
			map[string]interface{}{"type": "message", "role": "user", "content": "读"},
			map[string]interface{}{
				"type": "function_call", "call_id": "c1", "name": "Read",
				"arguments": `{"file_path":"/a","pages":""}`,
			},
			map[string]interface{}{"type": "function_call_output", "call_id": "c1", "output": "x"},
		},
	}
	anth, _, err := responsesToAnthropic(body)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	tu := anth["messages"].([]map[string]interface{})[1]["content"].([]interface{})[0].(map[string]interface{})
	if _, ok := tu["input"].(map[string]interface{})["pages"]; ok {
		t.Errorf("回放 input 未 sanitize: %v", tu["input"])
	}
}

// ---- Streaming: custom / namespace / Read ----

// Build an Anthropic SSE stream with one tool_use.
func toolUseSSE(toolID, toolName, delta1, delta2 string) string {
	var b strings.Builder
	b.WriteString("event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_1\",\"model\":\"m\",\"usage\":{\"input_tokens\":1}}}\n\n")
	cb, _ := json.Marshal(map[string]interface{}{"type": "tool_use", "id": toolID, "name": toolName, "input": map[string]interface{}{}})
	b.WriteString("event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":" + string(cb) + "}\n\n")
	if delta1 != "" {
		d, _ := json.Marshal(map[string]interface{}{"type": "input_json_delta", "partial_json": delta1})
		b.WriteString("event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":" + string(d) + "}\n\n")
	}
	if delta2 != "" {
		d, _ := json.Marshal(map[string]interface{}{"type": "input_json_delta", "partial_json": delta2})
		b.WriteString("event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":" + string(d) + "}\n\n")
	}
	b.WriteString("event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\n")
	b.WriteString("event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"tool_use\"},\"usage\":{\"output_tokens\":2}}\n\n")
	b.WriteString("event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")
	return b.String()
}

// custom tool streaming: no arguments delta midway, custom_tool_call_input.done at close-out,
// the output item is a custom_tool_call (ctc_ prefixed id). Mirrors cc-switch's streaming custom branch.
func TestStreamCustomToolCall(t *testing.T) {
	reg := buildToolRegistry([]interface{}{map[string]interface{}{"type": "custom", "name": "apply_patch"}})
	var events []string
	conv := newAnthToRespStream(func(ev string) { events = append(events, ev) }, "m", reg)
	feedSSEToConv(conv, toolUseSSE("call_1", "apply_patch", `{"input":"*** Begin`, ` Patch"}`))

	var names []string
	var doneInput string
	var doneItem map[string]interface{}
	for _, raw := range events {
		for _, chunk := range strings.Split(strings.TrimSpace(raw), "\n") {
			if !strings.HasPrefix(chunk, "data: ") {
				continue
			}
			var m map[string]interface{}
			if err := json.Unmarshal([]byte(chunk[6:]), &m); err != nil {
				continue
			}
			typ := objStr(m, "type")
			names = append(names, typ)
			if typ == "response.custom_tool_call_input.done" {
				doneInput = objStr(m, "input")
			}
			if typ == "response.output_item.done" {
				doneItem = asObj(m["item"])
			}
		}
	}
	for _, n := range names {
		if n == "response.function_call_arguments.delta" || n == "response.function_call_arguments.done" {
			t.Errorf("custom 工具不应发 function_call_arguments 事件: %v", names)
		}
	}
	if doneInput != "*** Begin Patch" {
		t.Errorf("custom_tool_call_input.done input=%q", doneInput)
	}
	if doneItem == nil || doneItem["type"] != "custom_tool_call" {
		t.Fatalf("done 项=%v", doneItem)
	}
	if !strings.HasPrefix(objStr(doneItem, "id"), "ctc_") {
		t.Errorf("custom 项 id 应 ctc_ 前缀: %v", doneItem["id"])
	}
}

// namespace tool streaming: arguments deltas pass through as usual, the done item restores name+namespace.
func TestStreamNamespaceToolCall(t *testing.T) {
	reg := buildToolRegistry([]interface{}{
		map[string]interface{}{
			"type": "namespace", "name": "mcp_files",
			"tools": []interface{}{
				map[string]interface{}{"type": "function", "name": "read", "parameters": map[string]interface{}{"type": "object"}},
			},
		},
	})
	var events []string
	conv := newAnthToRespStream(func(ev string) { events = append(events, ev) }, "m", reg)
	feedSSEToConv(conv, toolUseSSE("call_1", "mcp_files__read", `{"path":`, `"/a"}`))

	var sawDelta bool
	var doneItem map[string]interface{}
	for _, raw := range events {
		for _, chunk := range strings.Split(strings.TrimSpace(raw), "\n") {
			if !strings.HasPrefix(chunk, "data: ") {
				continue
			}
			var m map[string]interface{}
			if err := json.Unmarshal([]byte(chunk[6:]), &m); err != nil {
				continue
			}
			switch objStr(m, "type") {
			case "response.function_call_arguments.delta":
				sawDelta = true
			case "response.output_item.done":
				doneItem = asObj(m["item"])
			}
		}
	}
	if !sawDelta {
		t.Errorf("namespace（普通函数）应透传 arguments delta")
	}
	if doneItem == nil || doneItem["type"] != "function_call" || doneItem["name"] != "read" || doneItem["namespace"] != "mcp_files" {
		t.Fatalf("done 项=%v", doneItem)
	}
}

// Read tool streaming: deltas suppressed midway (avoiding leaking unsanitized fragments), pages:"" sanitized at close-out.
func TestStreamReadToolSanitize(t *testing.T) {
	var events []string
	conv := newAnthToRespStream(func(ev string) { events = append(events, ev) }, "m", nil)
	feedSSEToConv(conv, toolUseSSE("call_1", "Read", `{"file_path":"/a","pages":""}`, ""))

	var sawDelta bool
	var doneArgs string
	for _, raw := range events {
		for _, chunk := range strings.Split(strings.TrimSpace(raw), "\n") {
			if !strings.HasPrefix(chunk, "data: ") {
				continue
			}
			var m map[string]interface{}
			if err := json.Unmarshal([]byte(chunk[6:]), &m); err != nil {
				continue
			}
			switch objStr(m, "type") {
			case "response.function_call_arguments.delta":
				sawDelta = true
			case "response.function_call_arguments.done":
				doneArgs = objStr(m, "arguments")
			}
		}
	}
	if sawDelta {
		t.Errorf("Read 不应中途发 delta")
	}
	if strings.Contains(doneArgs, "pages") {
		t.Errorf("收拢 arguments 未 sanitize: %v", doneArgs)
	}
	if !strings.Contains(doneArgs, "file_path") {
		t.Errorf("收拢 arguments 丢字段: %v", doneArgs)
	}
}

// ---- Tool-result media stripping ----

// Mirrors cc-switch test_alternate_mcp_tool_image_is_not_stringified_for_anthropic:
// an MCP image block in the output array → [marker text, image block], base64 doesn't enter text.
func TestMCPToolImageNotStringified(t *testing.T) {
	body := map[string]interface{}{
		"model": "m",
		"input": []interface{}{
			map[string]interface{}{"type": "message", "role": "user", "content": "看下"},
			map[string]interface{}{"type": "function_call", "call_id": "c1", "name": "inspect", "arguments": "{}"},
			map[string]interface{}{
				"type": "function_call_output", "call_id": "c1",
				"output": []interface{}{
					map[string]interface{}{"type": "image", "mimeType": "image/webp", "data": "MCP_SENTINEL"},
				},
			},
		},
	}
	out, _, err := responsesToAnthropic(body)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	msgs := out["messages"].([]map[string]interface{})
	tr := msgs[2]["content"].([]interface{})[0].(map[string]interface{})
	content := tr["content"].([]interface{})
	if len(content) != 2 {
		t.Fatalf("content=%v", content)
	}
	first := content[0].(map[string]interface{})
	if first["type"] != "text" || strings.Contains(objStr(first, "text"), "MCP_SENTINEL") {
		t.Errorf("首块应是不含 base64 的文本: %v", first)
	}
	img := content[1].(map[string]interface{})
	if img["type"] != "image" {
		t.Fatalf("次块应是 image: %v", img)
	}
	src := img["source"].(map[string]interface{})
	if src["media_type"] != "image/webp" || src["data"] != "MCP_SENTINEL" {
		t.Errorf("source=%v", src)
	}
}

// Mirrors cc-switch test_json_string_nested_tool_image_is_not_text_for_anthropic:
// a JSON-nested image_url in the output string → extracted into an image block, leftover big base64 clamped.
func TestJSONStringNestedToolImage(t *testing.T) {
	residual := strings.Repeat("A", 20000)
	encoded, _ := json.Marshal(map[string]interface{}{
		"content": []interface{}{
			map[string]interface{}{"type": "input_text", "text": "caption"},
			map[string]interface{}{"type": "image_url", "image_url": map[string]interface{}{"url": "data:image/png;base64,STRING_SENTINEL"}},
			map[string]interface{}{"type": "video", "data": residual},
		},
	})
	body := map[string]interface{}{
		"model": "m",
		"input": []interface{}{
			map[string]interface{}{"type": "message", "role": "user", "content": "看下"},
			map[string]interface{}{"type": "function_call", "call_id": "c1", "name": "inspect", "arguments": "{}"},
			map[string]interface{}{"type": "function_call_output", "call_id": "c1", "output": string(encoded)},
		},
	}
	out, _, err := responsesToAnthropic(body)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	msgs := out["messages"].([]map[string]interface{})
	tr := msgs[2]["content"].([]interface{})[0].(map[string]interface{})
	content := tr["content"].([]interface{})
	var foundImage bool
	for _, c := range content {
		cm := c.(map[string]interface{})
		switch objStr(cm, "type") {
		case "image":
			src := cm["source"].(map[string]interface{})
			if src["data"] == "STRING_SENTINEL" {
				foundImage = true
			}
		case "text":
			txt := objStr(cm, "text")
			if strings.Contains(txt, "STRING_SENTINEL") {
				t.Errorf("文本块不应残留图片 base64: %v", txt[:100])
			}
			if strings.Contains(txt, residual) {
				t.Errorf("大 base64 残留应被 clamp: %.100v", txt)
			}
		}
	}
	if !foundImage {
		t.Fatalf("字符串里的图片应被提成 image 块: %v", content)
	}
}

// error-marker text → tool_result is_error (mirrors cc-switch's TOOL_RESULT_ERROR_MARKER branch).
func TestToolResultErrorMarker(t *testing.T) {
	item := map[string]interface{}{
		"type": "function_call_output", "call_id": "c1",
		"output": []interface{}{
			map[string]interface{}{"type": "input_text", "text": toolResultErrorMarker},
			map[string]interface{}{"type": "input_text", "text": "boom"},
		},
	}
	content, isError := toolResultContentFromResponsesItem(item)
	if !isError {
		t.Fatalf("error marker 应置 is_error")
	}
	blocks := content.([]interface{})
	if len(blocks) != 1 || objStr(blocks[0].(map[string]interface{}), "text") != "boom" {
		t.Errorf("marker 本身不进内容: %v", blocks)
	}
	// Plain string output unaffected.
	c2, e2 := toolResultContentFromResponsesItem(map[string]interface{}{"output": "正常结果"})
	if e2 || c2 != "正常结果" {
		t.Errorf("普通字符串=%v err=%v", c2, e2)
	}
}

// input_file → document block (both the message-content path and the tool-result path covered).
func TestInputFileToDocument(t *testing.T) {
	blk := documentBlockFromInputFile(map[string]interface{}{
		"type": "input_file", "filename": "a.pdf",
		"file_data": "data:application/pdf;base64,AAA=",
	})
	if blk == nil || blk["type"] != "document" || blk["title"] != "a.pdf" {
		t.Fatalf("document=%v", blk)
	}
	src := blk["source"].(map[string]interface{})
	if src["type"] != "base64" || src["media_type"] != "application/pdf" || src["data"] != "AAA=" {
		t.Errorf("source=%v", src)
	}
	// file_url form.
	blk2 := documentBlockFromInputFile(map[string]interface{}{
		"type": "input_file", "file_url": "https://x.com/a.pdf",
	})
	if blk2 == nil || blk2["source"].(map[string]interface{})["url"] != "https://x.com/a.pdf" {
		t.Errorf("url 形式=%v", blk2)
	}
	// Message-content path.
	body := map[string]interface{}{
		"model": "m",
		"input": []interface{}{
			map[string]interface{}{
				"type": "message", "role": "user",
				"content": []interface{}{
					map[string]interface{}{
						"type": "input_file", "filename": "a.pdf",
						"file_data": "data:application/pdf;base64,AAA=",
					},
				},
			},
		},
	}
	out, _, err := responsesToAnthropic(body)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	msg := out["messages"].([]map[string]interface{})[0]
	first := msg["content"].([]interface{})[0].(map[string]interface{})
	if first["type"] != "document" {
		t.Errorf("消息里的 input_file=%v", first)
	}
}

// ---- usage ----

// Mirrors cc-switch build_responses_usage_from_anthropic: cache_write_tokens nested + a top-level alias.
func TestUsageCacheWriteTokens(t *testing.T) {
	u := buildResponsesUsage(map[string]interface{}{
		"input_tokens":                float64(10),
		"output_tokens":               float64(5),
		"cache_read_input_tokens":     float64(100),
		"cache_creation_input_tokens": float64(20),
	})
	if u["input_tokens"] != int64(130) {
		t.Errorf("input_tokens=%v", u["input_tokens"])
	}
	details := u["input_tokens_details"].(map[string]interface{})
	if details["cached_tokens"] != int64(100) || details["cache_write_tokens"] != int64(20) {
		t.Errorf("details=%v", details)
	}
	if u["cache_creation_input_tokens"] != int64(20) {
		t.Errorf("顶层别名=%v", u["cache_creation_input_tokens"])
	}
}
