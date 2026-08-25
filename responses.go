package main

// responses.go — OpenAI Responses API 监听口。
// 把 Responses 协议请求翻译成 Anthropic Messages 协议，内部调用主 handler 走现有
// 路由/重试/流式管线，再把 Anthropic 响应翻译回 Responses 协议（流式见 responses_stream.go）。
// 翻译规则参考 cc-switch transform_codex_anthropic.rs（请求方向）与
// anthropic_response_to_responses（响应方向），按本代理需要裁剪。

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"strings"
)

// ctxKeyTranslated 是内部请求 context 的键：标记本请求来自 Responses 翻译口。
// 主 handler 据此给 flight 打 [translate] 标记。用 context 而非 header——
// copyHeaders 会把 header 透传到上游，context 不会泄露。
type ctxKeyTranslatedT struct{}

var ctxKeyTranslated ctxKeyTranslatedT

// thinkingEnvelopePrefix 是思考块信封前缀：把 Anthropic 签名 thinking 块 JSON
// base64url 后加此前缀，塞进 Responses reasoning.encrypted_content 返回给客户端；
// 下轮客户端回放历史时识别此前缀还原 thinking 块。自包含、不依赖上游解密
// （抄 cc-switch reasoning_bridge 的思路，前缀换成自己的避免与别家信封混淆）。
const thinkingEnvelopePrefix = "p429-ant-thinking-v1:"

// defaultResponsesMaxTokens 是 Responses 请求没带 max_output_tokens 时的默认 max_tokens
// （Anthropic 必填，缺了 400）。
const defaultResponsesMaxTokens = 32000

// runResponsesServer 启动 OpenAI Responses API 监听口（独立于主端口的 mux）。
// 监听失败只告警禁用，不影响主代理。
func runResponsesServer(listen string) {
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/responses", responsesHandler)
	mux.HandleFunc("/responses", responsesHandler)
	ln, err := net.Listen("tcp", listen)
	if err != nil {
		log.Printf("[Responses] 监听 %s 失败: %v（Responses API 功能禁用，主代理不受影响）", listen, err)
		return
	}
	log.Printf("[Responses] OpenAI Responses API 监听 http://%s（请求翻译成 Anthropic 走主管线）", listen)
	if err := http.Serve(ln, mux); err != nil {
		log.Printf("[Responses] 服务退出: %v", err)
	}
}

// responsesHandler 处理一个 Responses API 请求：翻译成 Anthropic 后内部调用主 handler。
func responsesHandler(w http.ResponseWriter, r *http.Request) {
	c := cfg.Load()
	// 与主 handler 同语义：allow_remote=false 时仅本机可连。
	if !c.AllowRemote && !isLocalRequest(r) {
		http.Error(w, "forbidden (local only)", http.StatusForbidden)
		return
	}
	if r.Method == http.MethodHead {
		w.WriteHeader(http.StatusOK)
		return
	}
	if r.Method != http.MethodPost {
		writeResponsesError(w, http.StatusMethodNotAllowed, "invalid_request_error", "only POST is supported")
		return
	}
	raw, err := io.ReadAll(r.Body)
	if err != nil {
		writeResponsesError(w, http.StatusBadRequest, "invalid_request_error", "read body failed")
		return
	}
	r.Body.Close()
	var body map[string]interface{}
	if err := json.Unmarshal(raw, &body); err != nil {
		writeResponsesError(w, http.StatusBadRequest, "invalid_request_error", "invalid JSON: "+err.Error())
		return
	}

	clientStream, _ := body["stream"].(bool)
	origModel, _ := body["model"].(string)

	anth, reg, err := responsesToAnthropic(body)
	if err != nil {
		writeResponsesError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}
	// 上游永远走流式（与 convertAlltoStream 同哲学）：网页可监控吐字，回传侧再按客户端需要
	// 实时翻译 SSE 或收集后一次性返回 Responses JSON。
	anth["stream"] = true
	newBody, err := json.Marshal(anth)
	if err != nil {
		writeResponsesError(w, http.StatusInternalServerError, "api_error", "marshal converted body failed")
		return
	}

	// 构造内部请求：路径换成 /v1/messages，头只保留鉴权（路由命中时会被目标 key 覆盖），
	// 不带 Codex 客户端的 OpenAI 专用头去骚扰 Anthropic 上游。
	r2 := r.Clone(r.Context())
	r2 = r2.WithContext(context.WithValue(r2.Context(), ctxKeyTranslated, "responses"))
	r2.Method = http.MethodPost
	r2.URL.Path = "/v1/messages"
	r2.Body = io.NopCloser(strings.NewReader(string(newBody)))
	r2.ContentLength = int64(len(newBody))
	r2.Header = http.Header{}
	r2.Header.Set("Content-Type", "application/json")
	r2.Header.Set("Anthropic-Version", "2023-06-01")
	if v := r.Header.Get("Authorization"); v != "" {
		r2.Header.Set("Authorization", v)
	}
	if v := r.Header.Get("X-Api-Key"); v != "" {
		r2.Header.Set("X-Api-Key", v)
	}

	tw := newTranslatingWriter(w, clientStream, origModel, reg)
	handler(tw, r2)
	tw.finish()
}

// writeResponsesError 返回 Responses 协议风格的错误 JSON。
func writeResponsesError(w http.ResponseWriter, status int, typ, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]interface{}{
		"error": map[string]interface{}{"type": typ, "message": msg},
	})
}

// ---- 小工具：map 取值 ----

func asObj(v interface{}) map[string]interface{} {
	m, _ := v.(map[string]interface{})
	return m
}

func asArr(v interface{}) []interface{} {
	a, _ := v.([]interface{})
	return a
}

func asStr(v interface{}) string {
	s, _ := v.(string)
	return s
}

func objStr(m map[string]interface{}, k string) string { return asStr(m[k]) }

func isMeaningfulText(s string) bool { return strings.TrimSpace(s) != "" }

// ---- 请求翻译：Responses → Anthropic ----

// responsesToAnthropic 把一个 Responses API 请求体翻译成 Anthropic Messages 请求体。
// 对照 cc-switch responses_request_to_anthropic。返回的工具注册表记录 custom/
// namespace/tool_search 工具的原始身份，响应翻译（流式与非流式）据此拆包。
func responsesToAnthropic(body map[string]interface{}) (map[string]interface{}, *toolRegistry, error) {
	result := map[string]interface{}{}
	if model := objStr(body, "model"); model != "" {
		result["model"] = model
	}

	// 工具注册表先于 messages 转换建立：function_call 回放要靠它反解 namespace 名字。
	reg := buildToolRegistry(asArr(body["tools"]))

	// instructions + input 里 role=system/developer 的文本 → system（\n\n 拼接）。
	var sysParts []string
	if ins := objStr(body, "instructions"); isMeaningfulText(ins) {
		sysParts = append(sysParts, strings.TrimSpace(ins))
	}
	if items := asArr(body["input"]); items != nil {
		for _, it := range items {
			m := asObj(it)
			if m == nil {
				continue
			}
			if role := objStr(m, "role"); role == "system" || role == "developer" {
				sysParts = append(sysParts, responsesSystemText(m)...)
			}
		}
	}
	if len(sysParts) > 0 {
		result["system"] = strings.Join(sysParts, "\n\n")
	}

	// input → messages（字符串形式 = 单条 user 文本）。
	var msgs []map[string]interface{}
	var err error
	switch inp := body["input"].(type) {
	case string:
		if isMeaningfulText(inp) {
			msgs = []map[string]interface{}{{
				"role":    "user",
				"content": []interface{}{map[string]interface{}{"type": "text", "text": inp}},
			}}
		}
	case []interface{}:
		msgs, err = convertInputToMessages(inp, reg)
		if err != nil {
			return nil, nil, err
		}
	}

	// 规整：先丢不完整工具轮（Anthropic 要求 tool_use 与 tool_result 紧邻配对），
	// 再保证首条是 user；末尾 assistant 文本按 prefill 规则修剪。
	msgs = dropIncompleteToolTurns(msgs)
	msgs = dropEmptyMessages(msgs)
	msgs = ensureLeadingUserMessage(msgs)
	if len(msgs) == 0 {
		return nil, nil, fmt.Errorf("cannot convert request: empty messages")
	}
	trimTrailingAssistantText(msgs)
	msgs = dropEmptyMessages(msgs)
	if len(msgs) == 0 {
		return nil, nil, fmt.Errorf("cannot convert request: empty messages")
	}
	result["messages"] = msgs

	// max_output_tokens → max_tokens（必填）。
	maxTokens := toInt64(body["max_output_tokens"])
	if maxTokens <= 0 {
		maxTokens = defaultResponsesMaxTokens
	}
	result["max_tokens"] = maxTokens

	// reasoning.effort → thinking。自适应模型（usesAdaptiveThinking 映射表）走
	// thinking:{type:"adaptive"} + output_config.effort；其余模型走 enabled+budget_tokens。
	// 判定用客户端发来的 model 名（路由改写在更后面的 handler 里发生），与 cc-switch
	// 读 body.model 一致。对照 transform_codex_anthropic.rs 312-367。
	effort := objStr(asObj(body["reasoning"]), "effort")
	model := objStr(body, "model")
	adaptiveModel := usesAdaptiveThinking(model)
	cannotDisable := thinkingCannotBeDisabled(model)
	explicitlyDisabled := reasoningExplicitlyDisabled(effort)
	adaptiveEffort := codexEffortToAnthropic(effort)
	adaptiveShouldThink := adaptiveModel && (adaptiveThinkingIsDefault(model) || adaptiveEffort != "")
	historyValid := trailingTurnSupportsThinking(msgs)
	thinkingEnabled := false
	budget := effortToThinkingBudget(effort)
	switch {
	case !historyValid:
		// 工具续轮缺签名 thinking 块可回放：关不掉的模型直接报错（照抄 cc-switch 文案），
		// 能关的 adaptive 模型显式关闭，其余模型不开 thinking（budget 路径一并跳过）。
		if cannotDisable {
			return nil, nil, fmt.Errorf("Anthropic model requires thinking, but the tool history has no signed thinking block to replay")
		}
		if adaptiveShouldThink {
			result["thinking"] = map[string]interface{}{"type": "disabled"}
		}
	case adaptiveShouldThink && (!explicitlyDisabled || cannotDisable):
		thinkingEnabled = true
		result["thinking"] = map[string]interface{}{"type": "adaptive"}
		if adaptiveEffort != "" {
			result["output_config"] = map[string]interface{}{"effort": adaptiveEffort}
		} else if explicitlyDisabled && cannotDisable {
			// Fable/Mythos 关不掉 thinking：用 low 表达 Codex 显式的 none。
			result["output_config"] = map[string]interface{}{"effort": "low"}
		}
	case explicitlyDisabled:
		result["thinking"] = map[string]interface{}{"type": "disabled"}
	case budget > 0:
		// budget 上限压到 max_tokens 的一半（给可见回答留空间），不足 1024 下限则不开。
		if ceiling := maxTokens / 2; budget > ceiling {
			budget = ceiling
		}
		if budget >= 1024 {
			thinkingEnabled = true
		}
	}
	if thinkingEnabled && !adaptiveModel {
		result["thinking"] = map[string]interface{}{"type": "enabled", "budget_tokens": budget}
	}
	// thinking 开启时 Anthropic 要求丢 temperature/top_p（互斥），不开才透传。
	if !thinkingEnabled {
		if v, ok := body["temperature"]; ok {
			result["temperature"] = v
		}
		if v, ok := body["top_p"]; ok {
			result["top_p"] = v
		}
	}

	// tools：经注册表转换（function/custom/namespace/tool_search/web_search 各自映射）。
	if len(reg.tools) > 0 {
		result["tools"] = reg.tools
		// 只在 tools 非空时转 tool_choice（Anthropic 无 tools 带 tool_choice 会 400）。
		if tc, ok := body["tool_choice"]; ok && tc != nil {
			mapped := mapToolChoiceToAnthropic(tc, reg)
			// Anthropic 拒绝 thinking 开启时的强制 tool_choice：关不掉的模型报错；
			// 其余保留用户的工具约束、本请求关掉 thinking（恢复 temperature/top_p），
			// 不悄悄弱化 required/指定选择。对照 cc-switch 398-425。
			if t := objStr(mapped, "type"); thinkingEnabled && (t == "any" || t == "tool") {
				if cannotDisable {
					return nil, nil, fmt.Errorf("Anthropic model requires adaptive thinking and cannot honor a forced tool_choice")
				}
				result["thinking"] = map[string]interface{}{"type": "disabled"}
				delete(result, "output_config")
				if v, ok := body["temperature"]; ok {
					result["temperature"] = v
				}
				if v, ok := body["top_p"]; ok {
					result["top_p"] = v
				}
			}
			result["tool_choice"] = mapped
		}
		if v, ok := body["parallel_tool_calls"].(bool); ok && !v {
			tc := asObj(result["tool_choice"])
			if tc == nil {
				tc = map[string]interface{}{"type": "auto"}
				result["tool_choice"] = tc
			}
			tc["disable_parallel_tool_use"] = true
		}
	}
	return result, reg, nil
}

// responsesSystemText 提取 system/developer 消息项的文本（content 为字符串或 parts 数组）。
func responsesSystemText(item map[string]interface{}) []string {
	var out []string
	switch c := item["content"].(type) {
	case string:
		if isMeaningfulText(c) {
			out = append(out, strings.TrimSpace(c))
		}
	case []interface{}:
		for _, p := range c {
			pm := asObj(p)
			switch objStr(pm, "type") {
			case "input_text", "output_text", "text":
				if t := objStr(pm, "text"); isMeaningfulText(t) {
					out = append(out, strings.TrimSpace(t))
				}
			}
		}
	}
	return out
}

// convertInputToMessages 把扁平的 Responses input[] 重新嵌套成 Anthropic messages。
// 对照 cc-switch convert_input_to_messages：
//   - input_text/output_text → 对应 role 的 text 块
//   - input_image → image 块；input_file → document 块；refusal → text 块
//   - function_call → assistant 的 tool_use 块（并入前一条 assistant 消息；带
//     namespace 时经注册表反解成拍平名，input 过 Read sanitize）
//   - custom_tool_call → tool_use（input 包成 {"input": 裸值}）
//   - tool_search_call → tool_use（代理工具名，arguments 对象作 input）
//   - function_call_output/custom_tool_call_output/tool_search_output → user 的
//     tool_result 块（连续的合并进同一条 user 消息）
//   - reasoning.encrypted_content 带我们信封前缀 → 还原签名 thinking 块
func convertInputToMessages(items []interface{}, reg *toolRegistry) ([]map[string]interface{}, error) {
	var msgs []map[string]interface{}
	for _, it := range items {
		item := asObj(it)
		if item == nil {
			continue
		}
		itemType := objStr(item, "type")
		// 历史上未完成（incomplete）的工具调用整个丢弃，回放会 400。
		switch itemType {
		case "function_call", "custom_tool_call", "tool_search_call":
			if objStr(item, "status") == "incomplete" {
				continue
			}
		}
		switch itemType {
		case "function_call":
			callID := objStr(item, "call_id")
			if callID == "" {
				callID = objStr(item, "id")
			}
			name := objStr(item, "name")
			upstreamName := reg.chatNameForFunction(name, objStr(item, "namespace"))
			argsStr := objStr(item, "arguments")
			var input interface{}
			if strings.TrimSpace(argsStr) == "" {
				input = map[string]interface{}{}
			} else if err := json.Unmarshal([]byte(argsStr), &input); err != nil {
				return nil, fmt.Errorf("invalid function_call arguments for '%s': %v", name, err)
			}
			if asObj(input) == nil {
				return nil, fmt.Errorf("function_call arguments for '%s' must be a JSON object", name)
			}
			pushBlock(&msgs, "assistant", map[string]interface{}{
				"type": "tool_use", "id": callID, "name": upstreamName,
				"input": sanitizeToolUseInput(name, asObj(input)),
			})
		case "custom_tool_call":
			callID := objStr(item, "call_id")
			if callID == "" {
				callID = objStr(item, "id")
			}
			// custom 工具输入是裸值（通常是字符串）：包进 {"input": ...} 对上包装 schema。
			input := item["input"]
			pushBlock(&msgs, "assistant", map[string]interface{}{
				"type": "tool_use", "id": callID, "name": objStr(item, "name"),
				"input": map[string]interface{}{customToolInputField: input},
			})
		case "tool_search_call":
			callID := objStr(item, "call_id")
			if callID == "" {
				callID = objStr(item, "id")
			}
			input := asObj(item["arguments"])
			if input == nil {
				input = map[string]interface{}{}
			}
			pushBlock(&msgs, "assistant", map[string]interface{}{
				"type": "tool_use", "id": callID, "name": toolSearchProxyName, "input": input,
			})
		case "function_call_output", "custom_tool_call_output", "tool_search_output":
			callID := objStr(item, "call_id")
			content, isError := toolResultContentFromResponsesItem(item)
			block := map[string]interface{}{
				"type": "tool_result", "tool_use_id": callID, "content": content,
			}
			if isError {
				block["is_error"] = true
			}
			pushToolResultBlock(&msgs, block)
		case "input_text":
			if t := objStr(item, "text"); isMeaningfulText(t) {
				pushBlock(&msgs, "user", map[string]interface{}{"type": "text", "text": t})
			}
		case "input_image":
			if b := imageBlockFromInputImage(item); b != nil {
				pushBlock(&msgs, "user", b)
			}
		case "reasoning":
			if b := decodeThinkingEnvelope(objStr(item, "encrypted_content")); b != nil {
				pushAssistantThinkingBlock(&msgs, b)
			}
		default:
			// message 项或带 role 的项：system/developer 已在上面收进 system，这里跳过。
			role := objStr(item, "role")
			if role == "" {
				role = "user"
			}
			if role == "system" || role == "developer" {
				continue
			}
			anthRole := "user"
			if role == "assistant" {
				anthRole = "assistant"
			}
			switch c := item["content"].(type) {
			case string:
				if isMeaningfulText(c) {
					pushBlock(&msgs, anthRole, map[string]interface{}{"type": "text", "text": c})
				}
			case []interface{}:
				for _, p := range c {
					pm := asObj(p)
					switch objStr(pm, "type") {
					case "input_text", "output_text":
						if t := objStr(pm, "text"); isMeaningfulText(t) {
							pushBlock(&msgs, anthRole, map[string]interface{}{"type": "text", "text": t})
						}
					case "refusal":
						if t := objStr(pm, "refusal"); isMeaningfulText(t) {
							pushBlock(&msgs, anthRole, map[string]interface{}{"type": "text", "text": t})
						}
					case "input_image":
						if b := imageBlockFromInputImage(pm); b != nil {
							pushBlock(&msgs, anthRole, b)
						}
					case "input_file":
						if b := documentBlockFromInputFile(pm); b != nil {
							pushBlock(&msgs, anthRole, b)
						}
					}
				}
			}
		}
	}
	return msgs, nil
}

// toolResultContentFromResponsesItem 把 *_call_output 项的 output 字段转成
// Anthropic tool_result 的 content（字符串或块数组）与 is_error。
// 对照 cc-switch 同名函数：error marker 文本置 is_error；数组部件支持
// input_text/output_text/input_image/input_file（认不出的先试媒体剥离再序列化）；
// 任何值都可能藏图片媒体（MCP image 块、JSON 字符串、整串 data URL），剥成 image 块。
func toolResultContentFromResponsesItem(item map[string]interface{}) (interface{}, bool) {
	switch out := item["output"].(type) {
	case string:
		if content, isError, ok := alternateImageToolResultContent(out); ok {
			return content, isError
		}
		return out, false
	case []interface{}:
		var content []interface{}
		isError := false
		for _, p := range out {
			pm := asObj(p)
			switch objStr(pm, "type") {
			case "input_text", "output_text":
				if t := objStr(pm, "text"); t == toolResultErrorMarker {
					isError = true
				} else if t != "" {
					content = append(content, map[string]interface{}{"type": "text", "text": t})
				}
			case "input_image":
				if b := imageBlockFromInputImage(pm); b != nil {
					content = append(content, b)
				} else {
					content = append(content, map[string]interface{}{"type": "text", "text": canonicalJSON(pm)})
				}
			case "input_file":
				if b := documentBlockFromInputFile(pm); b != nil {
					content = append(content, b)
				} else {
					content = append(content, map[string]interface{}{"type": "text", "text": canonicalJSON(pm)})
				}
			default:
				// 其他形状（MCP image 块等）：先试媒体剥离，认不出序列化成文本。
				if ac, ae, ok := alternateImageToolResultContent(p); ok {
					isError = isError || ae
					content = append(content, ac...)
				} else {
					content = append(content, map[string]interface{}{"type": "text", "text": canonicalJSON(p)})
				}
			}
		}
		return content, isError
	case nil:
		// output 缺席：整个项序列化当文本（对照 cc-switch None 分支）。
		return canonicalJSON(item), false
	default:
		// 数字/对象等异形 output：先试媒体剥离，否则序列化成 JSON 字符串当文本。
		if content, isError, ok := alternateImageToolResultContent(out); ok {
			return content, isError
		}
		return canonicalJSON(out), false
	}
}

// imageBlockFromInputImage 把 Responses input_image（data URL 或 http URL）转成 Anthropic image 块。
func imageBlockFromInputImage(part map[string]interface{}) map[string]interface{} {
	url := objStr(part, "image_url")
	if url == "" {
		url = objStr(asObj(part["image_url"]), "url")
	}
	if url == "" {
		return nil
	}
	if strings.HasPrefix(url, "data:") {
		rest := url[len("data:"):]
		comma := strings.IndexByte(rest, ',')
		if comma < 0 {
			return nil
		}
		meta, data := rest[:comma], rest[comma+1:]
		mediaType := strings.SplitN(meta, ";", 2)[0]
		if mediaType == "" {
			mediaType = "image/png"
		}
		return map[string]interface{}{
			"type": "image",
			"source": map[string]interface{}{
				"type": "base64", "media_type": mediaType, "data": data,
			},
		}
	}
	if strings.HasPrefix(url, "http://") || strings.HasPrefix(url, "https://") {
		return map[string]interface{}{
			"type":   "image",
			"source": map[string]interface{}{"type": "url", "url": url},
		}
	}
	return nil
}

// ---- 消息数组规整（对照 cc-switch 同名 helper）----

// pushBlock 追加内容块：末尾消息同 role 则合并，否则新开一条消息。
func pushBlock(msgs *[]map[string]interface{}, role string, block map[string]interface{}) {
	if n := len(*msgs); n > 0 {
		last := (*msgs)[n-1]
		if objStr(last, "role") == role {
			if arr, ok := last["content"].([]interface{}); ok {
				last["content"] = append(arr, block)
				return
			}
		}
	}
	*msgs = append(*msgs, map[string]interface{}{
		"role": role, "content": []interface{}{block},
	})
}

// pushToolResultBlock 追加 tool_result：保持 Anthropic 要求的顺序——tool_result 块
// 必须排在 user 消息里任何 text/image 块之前。
func pushToolResultBlock(msgs *[]map[string]interface{}, block map[string]interface{}) {
	if n := len(*msgs); n > 0 {
		last := (*msgs)[n-1]
		if objStr(last, "role") == "user" {
			if arr, ok := last["content"].([]interface{}); ok {
				insertAt := len(arr)
				for i, b := range arr {
					if objStr(asObj(b), "type") != "tool_result" {
						insertAt = i
						break
					}
				}
				arr = append(arr, nil)
				copy(arr[insertAt+1:], arr[insertAt:])
				arr[insertAt] = block
				last["content"] = arr
				return
			}
		}
	}
	*msgs = append(*msgs, map[string]interface{}{
		"role": "user", "content": []interface{}{block},
	})
}

// pushAssistantThinkingBlock 插入 thinking 块：Anthropic 要求它在 assistant 消息
// 内容数组的最前（已有 thinking/redacted_thinking 块之后、其余块之前）。
func pushAssistantThinkingBlock(msgs *[]map[string]interface{}, block map[string]interface{}) {
	if n := len(*msgs); n > 0 {
		last := (*msgs)[n-1]
		if objStr(last, "role") == "assistant" {
			if arr, ok := last["content"].([]interface{}); ok {
				idx := 0
				for idx < len(arr) {
					t := objStr(asObj(arr[idx]), "type")
					if t != "thinking" && t != "redacted_thinking" {
						break
					}
					idx++
				}
				arr = append(arr, nil)
				copy(arr[idx+1:], arr[idx:])
				arr[idx] = block
				last["content"] = arr
				return
			}
		}
	}
	pushBlock(msgs, "assistant", block)
}

// ensureLeadingUserMessage 保证首条是 user：压缩/恢复的会话可能以 assistant 或
// function_call 开头，Anthropic 要求首条 user 否则 400。
func ensureLeadingUserMessage(msgs []map[string]interface{}) []map[string]interface{} {
	if len(msgs) == 0 || objStr(msgs[0], "role") == "user" {
		return msgs
	}
	head := map[string]interface{}{
		"role":    "user",
		"content": []interface{}{map[string]interface{}{"type": "text", "text": "(continuing the conversation)"}},
	}
	return append([]map[string]interface{}{head}, msgs...)
}

// dropIncompleteToolTurns 丢弃不再构成完整「assistant tool_use → user tool_result」
// 相邻配对的工具轮（压缩/恢复的会话常见）。Anthropic 要求每个 tool_use 都在紧随的
// user 消息里得到全部回答，否则 400。
func dropIncompleteToolTurns(msgs []map[string]interface{}) []map[string]interface{} {
	var out []map[string]interface{}
	for i := 0; i < len(msgs); {
		m := msgs[i]
		toolUseIDs := messageBlockIDs(m, "tool_use", "id")
		if objStr(m, "role") != "assistant" || len(toolUseIDs) == 0 {
			if objStr(m, "role") == "user" {
				m = dropToolResultBlocks(m)
			}
			if messageHasContent(m) {
				out = append(out, m)
			}
			i++
			continue
		}
		// assistant 带 tool_use：检查紧随的 user 是否完整回答。
		var paired map[string]interface{}
		if i+1 < len(msgs) && objStr(msgs[i+1], "role") == "user" {
			paired = msgs[i+1]
		}
		toolResultIDs := map[string]bool{}
		if paired != nil {
			for _, id := range messageBlockIDs(paired, "tool_result", "tool_use_id") {
				toolResultIDs[id] = true
			}
		}
		complete := paired != nil
		if complete {
			for _, id := range toolUseIDs {
				if id == "" || !toolResultIDs[id] {
					complete = false
					break
				}
			}
		}
		if complete {
			out = append(out, m, paired)
		} else if paired != nil {
			// 整个 assistant 工具轮丢弃；user 消息去掉 tool_result 后若有剩余内容则保留。
			if m2 := dropToolResultBlocks(paired); messageHasContent(m2) {
				out = append(out, m2)
			}
		}
		if paired != nil {
			i += 2
		} else {
			i++
		}
	}
	return out
}

// messageBlockIDs 收集消息里指定类型块的 id 字段值。
func messageBlockIDs(m map[string]interface{}, blockType, idField string) []string {
	var ids []string
	for _, b := range asArr(m["content"]) {
		bm := asObj(b)
		if objStr(bm, "type") == blockType {
			ids = append(ids, objStr(bm, idField))
		}
	}
	return ids
}

// dropToolResultBlocks 返回去掉 tool_result 块后的消息副本。
func dropToolResultBlocks(m map[string]interface{}) map[string]interface{} {
	arr := asArr(m["content"])
	if arr == nil {
		return m
	}
	var kept []interface{}
	for _, b := range arr {
		if objStr(asObj(b), "type") != "tool_result" {
			kept = append(kept, b)
		}
	}
	cp := make(map[string]interface{}, len(m))
	for k, v := range m {
		cp[k] = v
	}
	cp["content"] = kept
	return cp
}

func messageHasContent(m map[string]interface{}) bool {
	arr := asArr(m["content"])
	return arr == nil || len(arr) > 0
}

// dropEmptyMessages 丢掉 content 空数组的消息（Anthropic 对空 content 400）。
func dropEmptyMessages(msgs []map[string]interface{}) []map[string]interface{} {
	var out []map[string]interface{}
	for _, m := range msgs {
		if messageHasContent(m) {
			out = append(out, m)
		}
	}
	return out
}

// trimTrailingAssistantText 修剪末尾 assistant 消息最后的 text 块：
// 纯空白 prefill 直接删块，非空的去掉尾部空白（Anthropic 拒绝以空白结尾的 prefill）。
func trimTrailingAssistantText(msgs []map[string]interface{}) {
	if len(msgs) == 0 {
		return
	}
	last := msgs[len(msgs)-1]
	if objStr(last, "role") != "assistant" {
		return
	}
	arr := asArr(last["content"])
	if len(arr) == 0 {
		return
	}
	blk := asObj(arr[len(arr)-1])
	if objStr(blk, "type") != "text" {
		return
	}
	text := objStr(blk, "text")
	trimmed := strings.TrimRight(text, " \t\r\n")
	if trimmed == "" {
		last["content"] = arr[:len(arr)-1]
	} else if trimmed != text {
		blk["text"] = trimmed
	}
}

// effortToThinkingBudget 把 Codex 的 reasoning.effort 映射成 Anthropic thinking 预算。
// 不识别的值返回 0（不开 thinking，保持正常采样）。对照 cc-switch 同名函数。
func effortToThinkingBudget(effort string) int64 {
	switch strings.ToLower(strings.TrimSpace(effort)) {
	case "minimal", "low":
		return 2048
	case "medium":
		return 8192
	case "high":
		return 16384
	case "xhigh", "max", "ultra":
		return 24576
	}
	return 0
}

// ---- thinking 模型映射表（照抄 cc-switch thinking_optimizer.rs） ----

// normalizeThinkingModelName 归一化模型名供映射表匹配：小写，'.' 和 '_' 换成 '-'。
func normalizeThinkingModelName(model string) string {
	return strings.NewReplacer(".", "-", "_", "-").Replace(strings.ToLower(strings.TrimSpace(model)))
}

// thinkingModelContains 判断归一化后的模型名是否含任一子串。
func thinkingModelContains(model string, needles ...string) bool {
	n := normalizeThinkingModelName(model)
	for _, s := range needles {
		if strings.Contains(n, s) {
			return true
		}
	}
	return false
}

// usesAdaptiveThinking 映射表：走 adaptive thinking（thinking:{type:"adaptive"} +
// output_config.effort）而非 enabled+budget_tokens 的模型。
func usesAdaptiveThinking(model string) bool {
	return thinkingModelContains(model,
		"fable-5", "mythos-5", "mythos-preview", "sonnet-5",
		"opus-4-8", "opus-4-7", "opus-4-6", "sonnet-4-6")
}

// adaptiveThinkingIsDefault 映射表：不显式给 thinking 参数也默认开 adaptive 的模型。
func adaptiveThinkingIsDefault(model string) bool {
	return thinkingModelContains(model, "fable-5", "mythos-5", "mythos-preview", "sonnet-5")
}

// thinkingCannotBeDisabled 映射表：拒绝 thinking:{type:"disabled"} 的模型。
func thinkingCannotBeDisabled(model string) bool {
	return thinkingModelContains(model, "fable-5", "mythos-5")
}

// codexEffortToAnthropic 把 Codex 的 reasoning.effort 映射成 output_config.effort
// （adaptive 模型用）。不识别的值返回空串。对照 cc-switch 同名函数。
func codexEffortToAnthropic(effort string) string {
	switch strings.ToLower(strings.TrimSpace(effort)) {
	case "minimal", "low":
		return "low"
	case "medium":
		return "medium"
	case "high":
		return "high"
	case "xhigh", "max", "ultra":
		return "max"
	}
	return ""
}

// reasoningExplicitlyDisabled 判断 reasoning.effort 是否显式关闭思考。对照 cc-switch 同名函数。
func reasoningExplicitlyDisabled(effort string) bool {
	switch strings.ToLower(strings.TrimSpace(effort)) {
	case "none", "off", "disabled":
		return true
	}
	return false
}

// trailingTurnSupportsThinking 判断末尾一轮是否支持开 thinking。对照 cc-switch 同名函数：
// 全新的 user 提问可以开；工具结果续轮只有紧邻的上一条 assistant 带签名
// thinking/redacted_thinking 块（且 tool_result 的 id 全部与其 tool_use 配对）才可开——
// 否则 Anthropic 会因缺签名 thinking 回放而 400。
func trailingTurnSupportsThinking(msgs []map[string]interface{}) bool {
	if len(msgs) == 0 {
		return false
	}
	last := msgs[len(msgs)-1]
	if objStr(last, "role") != "user" {
		return false
	}
	var toolResultIDs []string
	if blocks, ok := last["content"].([]interface{}); ok {
		for _, b := range blocks {
			blk := asObj(b)
			if objStr(blk, "type") != "tool_result" {
				continue
			}
			id := objStr(blk, "tool_use_id")
			if id == "" {
				return false
			}
			toolResultIDs = append(toolResultIDs, id)
		}
	}
	if len(toolResultIDs) == 0 {
		return true
	}
	if len(msgs) < 2 {
		return false
	}
	paired := msgs[len(msgs)-2]
	if objStr(paired, "role") != "assistant" {
		return false
	}
	blocks, ok := paired["content"].([]interface{})
	if !ok {
		return false
	}
	hasSignedThinking := false
	toolUseIDs := map[string]bool{}
	for _, b := range blocks {
		blk := asObj(b)
		switch objStr(blk, "type") {
		case "thinking", "redacted_thinking":
			hasSignedThinking = true
		case "tool_use":
			if id := objStr(blk, "id"); id != "" {
				toolUseIDs[id] = true
			}
		}
	}
	if !hasSignedThinking {
		return false
	}
	for _, id := range toolResultIDs {
		if !toolUseIDs[id] {
			return false
		}
	}
	return true
}

// mapToolChoiceToAnthropic 转换 tool_choice：required→any、auto→auto、none→none；
// {type:function}→{type:tool}（带 namespace 时反解成拍平名）；{type:custom}→同名 tool；
// {type:tool_search}→代理工具名；其余形状（allowed_tools 等）降级 auto 避免 400。
func mapToolChoiceToAnthropic(tc interface{}, reg *toolRegistry) map[string]interface{} {
	switch v := tc.(type) {
	case string:
		switch v {
		case "required":
			return map[string]interface{}{"type": "any"}
		case "none":
			return map[string]interface{}{"type": "none"}
		}
		return map[string]interface{}{"type": "auto"}
	case map[string]interface{}:
		switch objStr(v, "type") {
		case "function":
			name := reg.chatNameForFunction(objStr(v, "name"), objStr(v, "namespace"))
			return map[string]interface{}{"type": "tool", "name": name}
		case "custom":
			return map[string]interface{}{"type": "tool", "name": objStr(v, "name")}
		case "tool_search":
			return map[string]interface{}{"type": "tool", "name": toolSearchProxyName}
		}
		return map[string]interface{}{"type": "auto"}
	}
	return map[string]interface{}{"type": "auto"}
}

// ---- 思考块信封 ----

// encodeThinkingEnvelope 把 Anthropic 签名 thinking/redacted_thinking 块编码成
// Responses reasoning.encrypted_content 字符串（带版本前缀的 base64url JSON）。
// 无签名（signature/data 为空）的块不编码——回放没意义。
func encodeThinkingEnvelope(block map[string]interface{}) string {
	switch objStr(block, "type") {
	case "thinking":
		if objStr(block, "signature") == "" {
			return ""
		}
	case "redacted_thinking":
		if objStr(block, "data") == "" {
			return ""
		}
	default:
		return ""
	}
	b, err := json.Marshal(block)
	if err != nil {
		return ""
	}
	return thinkingEnvelopePrefix + base64.RawURLEncoding.EncodeToString(b)
}

// decodeThinkingEnvelope 识别信封前缀并还原 thinking 块；不是我们的信封返回 nil。
func decodeThinkingEnvelope(s string) map[string]interface{} {
	if !strings.HasPrefix(s, thinkingEnvelopePrefix) {
		return nil
	}
	b, err := base64.RawURLEncoding.DecodeString(s[len(thinkingEnvelopePrefix):])
	if err != nil {
		return nil
	}
	var block map[string]interface{}
	if err := json.Unmarshal(b, &block); err != nil {
		return nil
	}
	// 复用编码器的校验：解出来的必须是有签名的 thinking 块，防止异形信封混进工具轮。
	if encodeThinkingEnvelope(block) == "" {
		return nil
	}
	return block
}

// ---- 响应翻译：Anthropic message JSON → Responses 对象（非流式路径） ----

// mapStopReasonToStatus 把 Anthropic stop_reason 映射成 Responses (status, incomplete_reason)。
// 对照 cc-switch map_anthropic_stop_reason_to_status。
func mapStopReasonToStatus(stop string) (string, string) {
	switch stop {
	case "max_tokens", "model_context_window_exceeded":
		return "incomplete", "max_output_tokens"
	case "refusal":
		return "incomplete", "content_filter"
	}
	return "completed", ""
}

// buildResponsesUsage 把 Anthropic usage 转成 Responses usage。
// Anthropic 的 input_tokens 不含缓存；Responses 报总输入、缓存作为子集：
// input_tokens = fresh + cache_read + cache_creation。
func buildResponsesUsage(usage map[string]interface{}) map[string]interface{} {
	fresh := toInt64(usage["input_tokens"])
	output := toInt64(usage["output_tokens"])
	cacheRead := toInt64(usage["cache_read_input_tokens"])
	cacheCreation := toInt64(usage["cache_creation_input_tokens"])
	var reasoning int64
	if d := asObj(usage["output_tokens_details"]); d != nil {
		reasoning = toInt64(d["thinking_tokens"])
	}
	input := fresh + cacheRead + cacheCreation
	result := map[string]interface{}{
		"input_tokens":          input,
		"output_tokens":         output,
		"total_tokens":          input + output,
		"output_tokens_details": map[string]interface{}{"reasoning_tokens": reasoning},
		"input_tokens_details":  map[string]interface{}{"cached_tokens": cacheRead},
	}
	if cacheCreation > 0 {
		// 官方嵌套字段 + 保留一个顶层兼容别名（对照 cc-switch 同款做法）。
		result["input_tokens_details"].(map[string]interface{})["cache_write_tokens"] = cacheCreation
		result["cache_creation_input_tokens"] = cacheCreation
	}
	return result
}

// anthropicToResponsesObject 把完整的 Anthropic message JSON 转成 Responses 对象。
// 对照 cc-switch anthropic_response_to_responses_with_context。
// model 参数是客户端原始 model 名（管线回传时已被改写回原名的场景之外兜底用）。
// reg 是请求侧建立的工具注册表：tool_use 块据此还原 custom/namespace/tool_search 身份。
func anthropicToResponsesObject(msg map[string]interface{}, model string, reg *toolRegistry) map[string]interface{} {
	id := objStr(msg, "id")
	var responseID string
	switch {
	case id == "":
		responseID = "resp_p429"
	case strings.HasPrefix(id, "resp_"):
		responseID = id
	default:
		responseID = "resp_" + id
	}
	if m := objStr(msg, "model"); m != "" {
		model = m
	}

	var output []interface{}
	var textParts []interface{}
	flushText := func() {
		if len(textParts) == 0 {
			return
		}
		output = append(output, map[string]interface{}{
			"id":      fmt.Sprintf("%s_msg_%d", responseID, len(output)),
			"type":    "message",
			"status":  "completed",
			"role":    "assistant",
			"content": textParts,
		})
		textParts = nil
	}

	for _, b := range asArr(msg["content"]) {
		blk := asObj(b)
		switch objStr(blk, "type") {
		case "text":
			if t := objStr(blk, "text"); t != "" {
				textParts = append(textParts, map[string]interface{}{
					"type": "output_text", "text": t, "annotations": []interface{}{},
				})
			}
		case "tool_use":
			flushText()
			callID := objStr(blk, "id")
			name := objStr(blk, "name")
			input := asObj(blk["input"])
			if input == nil {
				input = map[string]interface{}{}
			}
			args := canonicalJSON(sanitizeToolUseInput(name, input))
			itemID := toolCallItemID(reg, callID, name)
			if itemID == "fc_" || itemID == "ctc_" {
				// call_id 空时 id 退化成纯前缀：补输出序号兜底保证唯一。
				itemID = fmt.Sprintf("%s%s_%d", itemID, responseID, len(output))
			}
			output = append(output, toolCallItemFromRegistry(reg, itemID, "completed", callID, name, args))
		case "thinking", "redacted_thinking":
			flushText()
			if enc := encodeThinkingEnvelope(blk); enc != "" {
				item := map[string]interface{}{
					"id":                fmt.Sprintf("rs_%s_%d", responseID, len(output)),
					"type":              "reasoning",
					"summary":           []interface{}{},
					"encrypted_content": enc,
				}
				if t := objStr(blk, "thinking"); t != "" {
					item["summary"] = []interface{}{
						map[string]interface{}{"type": "summary_text", "text": t},
					}
				}
				output = append(output, item)
			}
		case "server_tool_use", "web_search_tool_result":
			// 搜索相关块：配对成 web_search_call 项（start 时块已完整，见流式路径同款逻辑）。
			flushText()
			if item := webSearchCallItem(blk, responseID, len(output)); item != nil {
				output = append(output, item)
			}
		}
	}
	flushText()
	if output == nil {
		output = []interface{}{}
	}

	status, incompleteReason := mapStopReasonToStatus(objStr(msg, "stop_reason"))
	result := map[string]interface{}{
		"id":         responseID,
		"object":     "response",
		"created_at": 0,
		"status":     status,
		"model":      model,
		"output":     output,
		"usage":      buildResponsesUsage(asObj(msg["usage"])),
		"error":      nil,
	}
	if incompleteReason != "" {
		result["incomplete_details"] = map[string]interface{}{"reason": incompleteReason}
	}
	return result
}

// functionCallItem 构造一个 Responses function_call 输出项。
func functionCallItem(itemID, status, callID, name, arguments string) map[string]interface{} {
	return map[string]interface{}{
		"id":        itemID,
		"type":      "function_call",
		"status":    status,
		"call_id":   callID,
		"name":      name,
		"arguments": arguments,
	}
}

// webSearchCallItem 把 Anthropic server_tool_use / web_search_tool_result 块转成
// Responses web_search_call 项。server_tool_use（带 query）转成 completed 的搜索调用；
// web_search_tool_result 块本身不单独成项（结果已在 server_tool_use 的动作里表达不了
// 全部细节，v1 把结果来源 URL 合并进 action.sources）。
func webSearchCallItem(blk map[string]interface{}, responseID string, idx int) map[string]interface{} {
	switch objStr(blk, "type") {
	case "server_tool_use":
		if objStr(blk, "name") != "web_search" {
			return nil
		}
		action := map[string]interface{}{"type": "search"}
		if q := objStr(asObj(blk["input"]), "query"); q != "" {
			action["query"] = q
		}
		return map[string]interface{}{
			"id":     fmt.Sprintf("ws_%s_%d", responseID, idx),
			"type":   "web_search_call",
			"status": "completed",
			"action": action,
		}
	case "web_search_tool_result":
		// 结果块：提取来源 URL 列表，作为一个 completed 调用项的 sources 呈现。
		var sources []interface{}
		for _, r := range asArr(blk["content"]) {
			rm := asObj(r)
			if u := objStr(rm, "url"); u != "" {
				sources = append(sources, map[string]interface{}{"type": "url", "url": u})
			}
		}
		if len(sources) == 0 {
			return nil
		}
		return map[string]interface{}{
			"id":     fmt.Sprintf("ws_%s_%d", responseID, idx),
			"type":   "web_search_call",
			"status": "completed",
			"action": map[string]interface{}{"type": "search", "sources": sources},
		}
	}
	return nil
}
