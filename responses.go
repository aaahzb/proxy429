package main

// responses.go — OpenAI Responses API 监听口。
// 把 Responses 协议请求翻译成 Anthropic Messages 协议，内部调用主 handler 走现有
// 路由/重试/流式管线，再把 Anthropic 响应翻译回 Responses 协议（流式见 responses_stream.go）。
// 翻译规则参考 cc-switch transform_codex_anthropic.rs（请求方向）与
// anthropic_response_to_responses（响应方向），按本代理需要裁剪。

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

// ctxKeyTranslated 是内部请求 context 的键：标记本请求来自 Responses 翻译口。
// 主 handler 据此给 flight 打来源标记（网页 API 列显示 [translate]/[Response]）。
// 用 context 而非 header——copyHeaders 会把 header 透传到上游，context 不会泄露。
type ctxKeyTranslatedT struct{}

var ctxKeyTranslated ctxKeyTranslatedT

// ctxKeyConvID 是内部请求 context 的键：把 Responses 请求的会话标识
// （prompt_cache_key，Codex 恒带；次选 client_metadata.thread_id）递给主 handler，
// 供"缓存年龄"列按会话锚定。同 ctxKeyTranslated 的理由：用 context 不用 header，不会漏到上游。
type ctxKeyConvIDT struct{}

var ctxKeyConvID ctxKeyConvIDT

// ctxKeyThink 是内部请求 context 的键：把透传分支的思考模式（reasoning.effort 原样，
// 状态页「API」列思考值）递给主 handler——透传 body 是 Responses 格式，handler 里的
// extractThinkMode 认不出（无 thinking 字段），故由 context 单独携带。
// 翻译分支不用：翻译后 body 的 thinking 就是实际发上游的，handler 直接提取。
// 同 ctxKeyTranslated：用 context 不用 header，不会漏到上游。
type ctxKeyThinkT struct{}

var ctxKeyThink ctxKeyThinkT

// ctxKeyTranslated 的取值：flight.translated 同款三态——
// translatedResponses 表示 Responses 请求被翻译成 Anthropic 走主管线（API 列 [translate]）；
// translatedResponsesRaw 表示命中路由配了 url_response_api，Responses 原文透传不翻译（API 列 [Response]）。
const (
	translatedResponses    = "responses"
	translatedResponsesRaw = "responses-raw"
)

// thinkingEnvelopePrefix 是思考块信封前缀：把 Anthropic 签名 thinking 块 JSON
// base64url 后加此前缀，塞进 Responses reasoning.encrypted_content 返回给客户端；
// 下轮客户端回放历史时识别此前缀还原 thinking 块。自包含、不依赖上游解密
// （抄 cc-switch reasoning_bridge 的思路，前缀换成自己的避免与别家信封混淆）。
const thinkingEnvelopePrefix = "p429-ant-thinking-v1:"

// defaultResponsesMaxTokens 是 Responses 请求没带 max_output_tokens 时的默认 max_tokens
// （Anthropic 必填，缺了 400）。
const defaultResponsesMaxTokens = 32000

// responsesSrv 跟踪 Responses 监听口的运行状态，供配置重载/切换时动态启停（不必重启进程）。
var responsesSrv = struct {
	sync.Mutex
	addr string       // 当前实际监听的地址（空 = 未在监听）
	srv  *http.Server // 运行中的 server，供关闭
}{}

// reconcileResponsesServer 把 Responses 监听口对齐到配置地址：
// 地址不变则不动；变了（含新增/停用/改地址）先关旧监听再起新的。
// 监听失败只告警禁用，不影响主代理。启动、配置重载、切换配置三处都会调用。
func reconcileResponsesServer(listen string) {
	responsesSrv.Lock()
	defer responsesSrv.Unlock()
	if listen == responsesSrv.addr {
		return // 现状已是目标状态（含都为空）
	}
	if responsesSrv.srv != nil {
		// 立即关闭并释放端口：地址都变了，旧端口上的进行中请求留着也没意义
		_ = responsesSrv.srv.Close()
		responsesSrv.srv = nil
		responsesSrv.addr = ""
		log.Printf("[Responses] listener closed after config change")
	}
	if listen == "" {
		return
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/responses", responsesHandler)
	mux.HandleFunc("/responses", responsesHandler)
	ln, err := net.Listen("tcp", listen)
	if err != nil {
		log.Printf("[Responses] listen %s failed: %v (Responses API disabled; main proxy unaffected)", listen, err)
		return
	}
	srv := &http.Server{Handler: mux}
	responsesSrv.srv = srv
	responsesSrv.addr = listen
	log.Printf("[Responses] OpenAI Responses API listening on http://%s (requests translated to Anthropic via the main pipeline)", listen)
	go func() {
		if err := srv.Serve(ln); err != nil && err != http.ErrServerClosed {
			log.Printf("[Responses] server exited: %v", err)
		}
	}()
}

// predictSearchTriple 按路由规则预测本请求的上游归属三元组（搜索信封还原的比对基准）。
// 只复刻主路径（fast 字面名 > routes 通配 > 默认 upstream）；分类器/text_only/no_search/
// 增强搜索等条件分支不预测——预测偏差顶多让信封还原后撞 400，主管线 fail-soft 会剥掉
// 重试（见 handler 的 tool_call_id 兜底），不会错出数据。完全预测不了（无路由命中且
// 默认 upstream 为空）返回 nil = 信封一律放行。
func predictSearchTriple(c *Config, r *http.Request, model string) *searchTriple {
	if fr := c.FastRoute; fr != nil && fr.URL != "" && fr.Model != "" && model == "fast_route" {
		return newSearchTriple(fr.URL, fr.Model, effectiveKey(fr.API, r))
	}
	if rr := matchRouteRule(c, model); rr != nil {
		m := rr.Model
		if m == "" {
			m = model
		}
		return newSearchTriple(rr.URL, m, effectiveKey(rr.API, r))
	}
	if c.Upstream == "" {
		return nil
	}
	return newSearchTriple(c.Upstream, model, effectiveKey("", r))
}

// matchRouteRule 返回第一条命中 model 的路由（跳过保留名 pattern），未命中返回 nil。
// 翻译期预测（搜索三元组、路由 thinking 形态）共用这一个匹配点，与主 handler 的
// 路由循环同序同规则。注意 fast 字面名通道不经此匹配（调用方各自先判 fast）。
func matchRouteRule(c *Config, model string) *RouteRule {
	for i := range c.Routes {
		rr := &c.Routes[i]
		if isReservedRoutePattern(rr.Pattern) {
			continue
		}
		if matchModel(rr.Pattern, model) {
			return rr
		}
	}
	return nil
}

// effectiveKey 算上游请求实际生效的鉴权 token：路由 key 非空用路由 key，
// 空则透传客户端 Authorization 头值（与主 handler 的鉴权覆盖逻辑同口径）。
func effectiveKey(routeAPI string, r *http.Request) string {
	if routeAPI != "" {
		return routeAPI
	}
	return r.Header.Get("Authorization")
}

// responsesHandler 处理一个 Responses API 请求：翻译成 Anthropic 后内部调用主 handler。
func responsesHandler(w http.ResponseWriter, r *http.Request) {
	c := cfg.Load()
	// 与主 handler 同语义：转发通道永远仅本机可连。
	if !isLocalRequest(r) {
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

	// 会话标识（状态页"缓存年龄"列用）：Codex 恒带 prompt_cache_key（= 会话 UUID），
	// 次选 client_metadata.thread_id（Codex 中与前者同值）。只读，不改请求体。
	convID, _ := body["prompt_cache_key"].(string)
	if convID == "" {
		if cm, ok := body["client_metadata"].(map[string]interface{}); ok {
			convID, _ = cm["thread_id"].(string)
		}
	}

	// 思考模式（状态页「API」列思考值）：仅透传分支用——透传零修改，上游收到的 reasoning.effort
	// 就是它；翻译分支不走这里（翻译后 body 的 thinking 由主 handler 从最终 body 提取）。
	think := responsesThinkMode(body)

	// 原生透传预检：model 命中的路由配了 url_response_api 时，Responses 原文不翻译，
	// 原样交给主 handler——路由循环里的透传分支会把上游切成该路由的 url_response_api。
	// 监控流/统计/重试管线与翻译流完全相同（主 handler 按 ctx 标记换 Responses 口径解析）。
	// 预检只定"翻译还是透传"；真正的路由决策以 handler 为准（配置热重载致路由消失时 handler 报 502）。
	if matchPassthroughResponsesRoute(c, origModel) != nil {
		r2 := r.Clone(r.Context())
		r2 = r2.WithContext(context.WithValue(r2.Context(), ctxKeyTranslated, translatedResponsesRaw))
		if convID != "" {
			r2 = r2.WithContext(context.WithValue(r2.Context(), ctxKeyConvID, convID))
		}
		if think != "" {
			r2 = r2.WithContext(context.WithValue(r2.Context(), ctxKeyThink, think))
		}
		r2.Method = http.MethodPost
		r2.URL.Path = "/v1/responses"
		r2.Body = io.NopCloser(bytes.NewReader(raw))
		r2.ContentLength = int64(len(raw))
		// 头保留客户端原样：上游就是原生 Responses 服务，Authorization 等在路由命中时被目标 key 覆盖。
		handler(w, r2)
		return
	}

	// 翻译期的搜索还原上下文：时间规则剥块计数/对话水位/还原时刻收集；
	// 随内部请求下发，主 handler 把剥块计数进 flight（[剥N] 显示），
	// 400 兜底剥块时拿还原时刻学对话水位。
	replay := &searchReplayCtx{convID: convID}
	// 路由声明的思考形态（thinking 参数）：翻译时只知道客户端 model 名，adaptive/budget
	// 判定默认查客户端名的映射表——目标模型能力与此不一致时按路由配置覆盖。
	thinkStyle := ""
	if rr := matchRouteRule(c, origModel); rr != nil {
		thinkStyle = rr.Thinking
	}
	anth, reg, err := responsesToAnthropicTriple(body, predictSearchTriple(c, r, origModel), replay, thinkStyle)
	if err != nil {
		writeResponsesError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}
	// 上游永远走流式（与 convertAlltoStream 同哲学）：网页可监控吐字，回传侧再按客户端需要
	// 实时翻译 SSE 或收集后一次性返回 Responses JSON。
	anth["stream"] = true
	// fast_route 在 Codex 菜单里的条目名就是字面名 "fast_route"（Codex 不对照配置校验目录名）：
	// 选中即注入 speed:"fast"，由主 handler 的 fast 分支接管（改走 fast_route 上游、
	// model 改写为 fast_route.model）。"fast_route" 同时是路由 pattern 保留名，防撞名截流。
	if fr := c.FastRoute; fr != nil && fr.URL != "" && fr.Model != "" && origModel == "fast_route" {
		anth["speed"] = "fast"
	}
	newBody, err := json.Marshal(anth)
	if err != nil {
		writeResponsesError(w, http.StatusInternalServerError, "api_error", "marshal converted body failed")
		return
	}

	// 构造内部请求：路径换成 /v1/messages，头只保留鉴权（路由命中时会被目标 key 覆盖），
	// 不带 Codex 客户端的 OpenAI 专用头去骚扰 Anthropic 上游。
	r2 := r.Clone(r.Context())
	r2 = r2.WithContext(context.WithValue(r2.Context(), ctxKeyTranslated, translatedResponses))
	r2 = r2.WithContext(context.WithValue(r2.Context(), ctxKeySearchReplay, replay))
	if convID != "" {
		r2 = r2.WithContext(context.WithValue(r2.Context(), ctxKeyConvID, convID))
	}
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
	return responsesToAnthropicTriple(body, nil, nil, "")
}

// responsesToAnthropicTriple 同 responsesToAnthropic，额外带本请求的路由预测三元组
// （搜索信封还原的比对基准；nil = 预测不了，信封一律放行）、搜索还原上下文
// （时间规则剥块计数/对话水位/还原时刻收集；nil = 只转换不统计）与路由声明的目标
// 模型思考形态 thinkStyle（""/"auto"=按客户端 model 名查表；"adaptive"=强制 adaptive；
// "budget"=强制 enabled+budget_tokens——路由 thinking 参数，解决路由目标模型与客户端
// 别名的思考能力不一致，如客户端叫 claude-fable-5 实际路由到只支持 budget 的 Kimi）。
func responsesToAnthropicTriple(body map[string]interface{}, reqTriple *searchTriple, replay *searchReplayCtx, thinkStyle string) (map[string]interface{}, *toolRegistry, error) {
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
		msgs, err = convertInputToMessages(inp, reg, reqTriple, replay)
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
	// 判定默认用客户端发来的 model 名（路由改写在更后面的 handler 里发生），与 cc-switch
	// 读 body.model 一致。对照 transform_codex_anthropic.rs 312-367。
	// thinkStyle 非 auto 时按路由声明覆盖判定结果：目标模型能力与客户端别名不一致时
	// （如别名叫 claude-fable-5 实际路由到只支持 budget 的上游）以路由配置为准。
	effort := objStr(asObj(body["reasoning"]), "effort")
	model := objStr(body, "model")
	adaptiveModel := usesAdaptiveThinking(model)
	cannotDisable := thinkingCannotBeDisabled(model)
	switch thinkStyle {
	case "adaptive":
		// 路由声明目标模型支持 adaptive：强制 adaptive 口径（关思考仍允许——
		// 目标模型的真实能力由配置方负责，不再套 fable/mythos 的关不掉规则）。
		adaptiveModel = true
		cannotDisable = false
	case "budget":
		// 路由声明目标模型只支持经典 enabled+budget_tokens。
		adaptiveModel = false
		cannotDisable = false
	}
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
//   - reasoning.encrypted_content 带我们信封前缀 → 还原签名 thinking 块；
//     带搜索信封前缀且三元组与本请求路由预测一致 → 还原完整搜索块（见 searchEnvelopePrefix）
func convertInputToMessages(items []interface{}, reg *toolRegistry, reqTriple *searchTriple, replay *searchReplayCtx) ([]map[string]interface{}, error) {
	var msgs []map[string]interface{}
	var reqMask []byte // 路由预测的 key 派生掩码（nil = 预测不了，信封解不开自然跳过）
	if reqTriple != nil {
		reqMask = reqTriple.mask
	}
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
			// 剥掉历史里的搜索 query 回声行（见 stripSearchQueryEcho），全文无条件应用。
			if t := stripSearchQueryEcho(objStr(item, "text")); isMeaningfulText(t) {
				pushBlock(&msgs, "user", map[string]interface{}{"type": "text", "text": t})
			}
		case "input_image", "input_file":
			// 顶层裸附件部件：认不出标准形态时序列化成文本兜底，不静默丢
			// （见 pushMediaPart）。
			pushMediaPart(&msgs, "user", item)
		case "reasoning":
			enc := objStr(item, "encrypted_content")
			if b := decodeThinkingEnvelope(enc); b != nil {
				pushAssistantThinkingBlock(&msgs, b)
			} else if tri, blocks, ts, ok := decodeSearchEnvelope(enc, reqMask); ok {
				// 搜索信封：解得开（key 同源）且 url 也同源才还原上行——不同源还原
				// 也解不开，跳过省 token；跨模型不拦（实测照常解密）。reqMask 为
				// nil = 本请求预测不了路由 key，解不开混淆自然跳过（v1 的放行
				// 分支随明文 payload 一起退役）。
				if reqTriple == nil || reqTriple.sameOrigin(tri) {
					// 水位主动剥（不撞 400 不烧重试）：本对话已学到水位时，不比水位
					// 新的信封默认全剥（注册表按龄淘汰，撞过 400 说明最老的死了，
					// 同龄与更老的必死）。不设固定年龄上限——实测封入 1.7h 的 id
					// 仍存活，固定上限会误杀活信封。无 ts 的老信封（v2 初版，全部
					// 早于 ts 时代）视同最老：有水位剥、无水位乐观还原。
					// 剥块计数与还原时刻都记进 replay（nil = 只转换不统计）。
					var cutoff time.Time
					if replay != nil && replay.convID != "" {
						cutoff = searchCutoffFor(replay.convID)
					}
					switch {
					case !cutoff.IsZero() && (ts.IsZero() || !ts.After(cutoff)):
						replay.proactiveCutoff += len(blocks)
					default:
						for _, b := range blocks {
							pushBlock(&msgs, "assistant", b)
						}
						// 还原时刻只收带 ts 的：无 ts 信封参与取最老会毒化水位学习
						if replay != nil && !ts.IsZero() {
							replay.restored = append(replay.restored, ts)
						}
					}
				}
			}
		case "web_search_call", "server_tool_use", "web_search_tool_result":
			// web_search_call 调用项不回放（代理自造 id 上行必 400，见
			// searchBlocksFromResponsesItem）；Anthropic 形状的搜索块空壳整条删除、
			// 有内容的原样上行。搜索内容的唯一回放载体是上面的搜索信封。
			for _, b := range searchBlocksFromResponsesItem(item) {
				pushBlock(&msgs, "assistant", b)
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
				if t := stripSearchQueryEcho(c); isMeaningfulText(t) {
					pushBlock(&msgs, anthRole, map[string]interface{}{"type": "text", "text": t})
				}
			case []interface{}:
				for _, p := range c {
					pm := asObj(p)
					switch objStr(pm, "type") {
					case "input_text", "output_text":
						if t := stripSearchQueryEcho(objStr(pm, "text")); isMeaningfulText(t) {
							pushBlock(&msgs, anthRole, map[string]interface{}{"type": "text", "text": t})
						}
					case "refusal":
						if t := stripSearchQueryEcho(objStr(pm, "refusal")); isMeaningfulText(t) {
							pushBlock(&msgs, anthRole, map[string]interface{}{"type": "text", "text": t})
						}
					case "input_image", "input_file":
						pushMediaPart(&msgs, anthRole, pm)
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

// pushMediaPart 把 input_image/input_file 部件转成 Anthropic image/document 块追加。
// 认不出的形态（blob:/file: 本地 URL、file_id 云端引用、残缺 data URL 等）序列化成
// 文本块兜底：字节流拿不到是客观限制，但整块静默消失不是——与工具结果部件路径的
// 兜底口径一致（见 toolResultContentFromResponsesItem）。
func pushMediaPart(msgs *[]map[string]interface{}, role string, pm map[string]interface{}) {
	var b map[string]interface{}
	switch objStr(pm, "type") {
	case "input_image":
		b = imageBlockFromInputImage(pm)
	case "input_file":
		b = documentBlockFromInputFile(pm)
	}
	if b == nil {
		b = map[string]interface{}{"type": "text", "text": canonicalJSON(pm)}
	}
	pushBlock(msgs, role, b)
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

// responsesThinkMode 提取 Responses 请求体的思考配置，供状态页「API」列思考值显示
// （透传分支用——透传零修改，reasoning.effort 就是实际发上游的）。
// 值取最短形态（Responses 口径由列绿色承担，不带 "effort·" 前缀）：
// reasoning.effort 只显档位词（"high"…），显式关闭值（none/off/disabled）归并为 "关"；
// 无 reasoning/effort 字段返回空（列显 -）。
func responsesThinkMode(body map[string]interface{}) string {
	effort := objStr(asObj(body["reasoning"]), "effort")
	if effort == "" {
		return ""
	}
	if reasoningExplicitlyDisabled(effort) {
		return "关"
	}
	return effort
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

// ---- 搜索块信封 ----

// searchEnvelopePrefix 是搜索块信封前缀：把一次搜索的 server_tool_use +
// web_search_tool_result 两块连同归属三元组，经 key 派生掩码异或混淆后 base64url
// 加此前缀，塞进 Responses reasoning.encrypted_content 返回给客户端（与 thinking
// 信封同管道）。客户端下一轮原样回传，代理解信封把完整搜索结构还原上行——模型据此
// 直接读上次搜索内容，不必原关键字重搜。v2 起 payload 混淆存储：信封在客户端历史
// （Codex 会话记录）里躺着，不躺明文 url/模型/key 哈希（用户要求，混淆非加密）。
const searchEnvelopePrefix = "p429-ant-search-v2:"

// searchReplayCtx 是搜索信封还原的每次请求上下文（翻译期单 goroutine，免锁）：
// convID 用于查/学对话水位；proactiveCutoff 计数水位主动剥的块（[剥N] 的一部分，
// 拆分只写日志）；restored 收集实际还原上行的信封封入时刻（无 ts 不收）——
// 400 兜底剥块时取最老的一个学成对话水位。
type searchReplayCtx struct {
	convID          string
	proactiveCutoff int // 对话水位剥的块数
	restored        []time.Time
}

// ctxKeySearchReplay 是内部请求 context 的键：把翻译期的搜索还原上下文
// 带给主 handler（剥块计数进 flight、400 时学水位）。同 ctxKeyTranslated
// 的理由：用 context 不用 header，不会漏到上游。
type ctxKeySearchReplayT struct{}

var ctxKeySearchReplay ctxKeySearchReplayT

// searchCutoff 按对话记录信封水位（封入时刻下界）：不比水位新的信封默认全剥
// （注册表按龄淘汰：撞过 400 说明最老的死了，同龄与更老的必死）。400 兜底
// 剥块时学习——对搜索 id 真实存活期不设任何先验，完全按对话实测自适应；
// 只升不降（水位越新剥得越多，旧信息已被新信息覆盖）。
var searchCutoff = struct {
	sync.Mutex
	m map[string]time.Time
}{m: make(map[string]time.Time)}

// searchCutoffFor 查对话水位；无记录返回零值（不拦任何信封）。
func searchCutoffFor(convID string) time.Time {
	searchCutoff.Lock()
	defer searchCutoff.Unlock()
	return searchCutoff.m[convID]
}

// learnSearchCutoff 把对话水位抬到 ts（只升不降）。
func learnSearchCutoff(convID string, ts time.Time) {
	if convID == "" || ts.IsZero() {
		return
	}
	searchCutoff.Lock()
	defer searchCutoff.Unlock()
	if ts.After(searchCutoff.m[convID]) {
		searchCutoff.m[convID] = ts
	}
}

// searchTriple 是搜索信封的归属信息：url+模型+key 哈希。KeyH 只存 key 的
// sha256 前 16 hex，不裸存 key。Model 只作排查参考，不参与还原比对——见 sameOrigin。
// mask 是 key 派生的异或掩码（json:"-" 永不信封序列化）：encode/decode 都要它，
// 路由定案后由 newSearchTriple 一并派生。
type searchTriple struct {
	URL   string `json:"u"`
	Model string `json:"m"`
	KeyH  string `json:"k"`
	mask  []byte `json:"-"`
}

// newSearchTriple 建归属三元组：api 是实际生效的鉴权 token（effectiveKey 口径），
// 只存其哈希，并派生信封混淆掩码。
func newSearchTriple(url, model, api string) *searchTriple {
	return &searchTriple{URL: url, Model: model, KeyH: hashSearchKey(api), mask: searchEnvMask(api)}
}

// searchEnvMask 从 api key 派生 32 字节混淆掩码（域分隔，不与 KeyH 同源）。
func searchEnvMask(api string) []byte {
	sum := sha256.Sum256([]byte("p429-search-mask\x00" + api))
	return sum[:]
}

// xorSearchMask 循环异或掩码（混淆/解混淆同一函数）。纯异或，无加密开销。
func xorSearchMask(mask, p []byte) []byte {
	out := make([]byte, len(p))
	for i := range p {
		out[i] = p[i] ^ mask[i%len(mask)]
	}
	return out
}

// sameOrigin 判断两归属是否同一上游来源：只比 url+key 两腿。模型腿不参与——
// 2026-09-10 实测同 endpoint 同 key 跨模型回放（k3-256k 的搜索块丢给
// kimi-for-coding 追问）200 零重搜、模型给出正文级摘要，跨模型不拦还原。
func (t *searchTriple) sameOrigin(o *searchTriple) bool {
	return t.URL == o.URL && t.KeyH == o.KeyH
}

// hashSearchKey 算 api key 的比对哈希（sha256 前 16 hex）。参数是实际生效的
// 鉴权 token：路由 key 非空用路由 key，空则是透传的客户端 Authorization 头值。
func hashSearchKey(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])[:16]
}

// encodeSearchEnvelope 把一次搜索的两个 Anthropic 块与归属三元组编码成信封字符串。
// 三元组/块缺失或 server_tool_use 无 query（空搜索）时不编码——空壳没有回放价值。
func encodeSearchEnvelope(t *searchTriple, useBlk, resBlk map[string]interface{}) string {
	if t == nil || useBlk == nil || resBlk == nil {
		return ""
	}
	if objStr(asObj(useBlk["input"]), "query") == "" {
		return ""
	}
	// id 归一：Kimi 流式搜索的 server_tool_use 块 id 是 tool_ 开头，但搜索注册表
	// 只登记结果块的 srvtoolu_ id——回放时按 stu.id 查注册表，查不到就 400
	// tool_call_id is not found（2026-09-10 受控实验：流式原样回放 400，把 stu.id
	// 改写成 result.tool_use_id 后 200；非流式搜索两者天生一致，不受影响）。
	// 信封是唯一的回放载体，在封入时归一；浅拷贝不改调用方共享的块。
	if tid := objStr(resBlk, "tool_use_id"); tid != "" && objStr(useBlk, "id") != tid {
		cp := make(map[string]interface{}, len(useBlk)+1)
		for k, v := range useBlk {
			cp[k] = v
		}
		cp["id"] = tid
		useBlk = cp
	}
	payload, err := json.Marshal(map[string]interface{}{
		"t":  t,
		"b":  []map[string]interface{}{useBlk, resBlk},
		"ts": time.Now().Unix(), // 封入时刻：还原侧按对话水位剥块的依据
	})
	if err != nil {
		return ""
	}
	// 混淆存储：掩码缺失宁可不出信封，也不发明文 payload。
	if len(t.mask) == 0 {
		return ""
	}
	return searchEnvelopePrefix + base64.RawURLEncoding.EncodeToString(xorSearchMask(t.mask, payload))
}

// decodeSearchEnvelope 识别搜索信封前缀并还原三元组、两个内容块与封入时刻 ts
// （无 ts 字段的老信封返回零值——还原侧视同最老：有水位剥、无水位乐观还原）。
// mask 是本请求路由预测的 key 派生掩码（nil/不对 → 异或出来不是 JSON，自然
// ok=false——key 腿的比对就含在解混淆里）；不是我们的信封、或块形态不对，
// 返回 ok=false。
func decodeSearchEnvelope(s string, mask []byte) (*searchTriple, []map[string]interface{}, time.Time, bool) {
	if !strings.HasPrefix(s, searchEnvelopePrefix) || len(mask) == 0 {
		return nil, nil, time.Time{}, false
	}
	b, err := base64.RawURLEncoding.DecodeString(s[len(searchEnvelopePrefix):])
	if err != nil {
		return nil, nil, time.Time{}, false
	}
	b = xorSearchMask(mask, b)
	var payload struct {
		T  *searchTriple            `json:"t"`
		B  []map[string]interface{} `json:"b"`
		Ts int64                    `json:"ts"`
	}
	if err := json.Unmarshal(b, &payload); err != nil || payload.T == nil || len(payload.B) != 2 {
		return nil, nil, time.Time{}, false
	}
	if objStr(payload.B[0], "type") != "server_tool_use" ||
		objStr(payload.B[1], "type") != "web_search_tool_result" {
		return nil, nil, time.Time{}, false
	}
	var ts time.Time
	if payload.Ts > 0 {
		ts = time.Unix(payload.Ts, 0)
	}
	return payload.T, payload.B, ts, true
}

// stripSearchBlocksInBody 从 Anthropic 请求体剥掉所有回放的搜索结构
// （server_tool_use/web_search_tool_result 块）：剥后空壳消息整条删除、相邻同 role
// 消息合并（删消息可能造成 user user 相邻）。n 是剥掉的搜索块数（两种块各算 1，
// 删空壳消息不另计），供 [剥N] 显示与日志拆分。没有任何搜索块时 ok=false（无需重试）。
// 用于搜索信封还原被上游拒（400 tool_call_id）后的 fail-soft 重试。
func stripSearchBlocksInBody(body []byte) (nb []byte, n int, ok bool) {
	var m map[string]interface{}
	if err := json.Unmarshal(body, &m); err != nil {
		return nil, 0, false
	}
	msgs := asArr(m["messages"])
	if msgs == nil {
		return nil, 0, false
	}
	stripped := false
	var newMsgs []interface{}
	for _, mi := range msgs {
		msg := asObj(mi)
		content := asArr(msg["content"])
		if msg == nil || content == nil {
			newMsgs = append(newMsgs, mi)
			continue
		}
		var nc []interface{}
		for _, c := range content {
			if t := objStr(asObj(c), "type"); t == "server_tool_use" || t == "web_search_tool_result" {
				stripped = true
				n++
				continue
			}
			nc = append(nc, c)
		}
		if len(nc) == 0 {
			stripped = true // 整条消息只剩搜索块 → 删
			continue
		}
		msg["content"] = nc
		newMsgs = append(newMsgs, msg)
	}
	if !stripped {
		return nil, 0, false
	}
	m["messages"] = mergeSameRoleMessages(newMsgs)
	out, err := json.Marshal(m)
	if err != nil {
		return nil, 0, false
	}
	return out, n, true
}

// mergeSameRoleMessages 合并相邻同 role 且 content 都是数组的消息（剥块/删消息后的规整）。
func mergeSameRoleMessages(msgs []interface{}) []interface{} {
	var out []interface{}
	for _, mi := range msgs {
		msg := asObj(mi)
		if len(out) > 0 && msg != nil {
			prev := asObj(out[len(out)-1])
			pc, pok := prev["content"].([]interface{})
			cc, cok := msg["content"].([]interface{})
			if pok && cok && objStr(prev, "role") != "" && objStr(prev, "role") == objStr(msg, "role") {
				prev["content"] = append(pc, cc...)
				continue
			}
		}
		out = append(out, mi)
	}
	return out
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
// triple 是搜索信封的归属三元组（nil = 不出搜索信封，如 Anthropic 口直接调用）。
func anthropicToResponsesObject(msg map[string]interface{}, model string, reg *toolRegistry, triple *searchTriple) map[string]interface{} {
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
	var lastSearchUse map[string]interface{} // 最近一个 server_tool_use 块（搜索结果块到达时配对封信封）
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
			// 搜索 query 回声行整行删除（见 stripSearchQueryEcho）；剥完为空则丢弃。
			if t := stripSearchQueryEcho(objStr(blk, "text")); strings.TrimSpace(t) != "" {
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
			// 搜索块信封：结果块到达时与前面的 server_tool_use 配对封袋，作为额外
			// reasoning 项随行——客户端保管，下轮回放时还原（见 searchEnvelopePrefix）。
			if objStr(blk, "type") == "server_tool_use" {
				lastSearchUse = blk
			} else if lastSearchUse != nil {
				if enc := encodeSearchEnvelope(triple, lastSearchUse, blk); enc != "" {
					output = append(output, map[string]interface{}{
						"id":                fmt.Sprintf("rs_%s_env%d", responseID, len(output)),
						"type":              "reasoning",
						"summary":           []interface{}{},
						"encrypted_content": enc,
					})
				}
				lastSearchUse = nil
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

// kimiSearchPreamble 是 Kimi（k3-256k）在请求带 web_search 工具时每轮响应开头白送的
// 空搜索前言文本（query 为空）。在线探针实证其 anthropic 端点会发「空搜索三连」：
// 此前言 text 块 + 无 id/input 的 server_tool_use + content 为空的 web_search_tool_result，
// 哪怕模型根本没搜索。更麻烦的是模型会从历史里模仿这个模式：前言会重复多次并直接粘在
// 正文开头（实测 "Search results for query: Search results for query: 我确认一下…"）。
// 处理规则（下游响应与上游请求回放同套，见 stripSearchQueryEcho）：纯前言与
// 「单条前言+query」的回声行整行删除（query 回声灌进上下文会诱发连续同类搜索）；
// ≥2 条连续裸前言粘在正文前（模仿签名）剥光留正文；空的 web_search 结构不产出/不回放。
const kimiSearchPreamble = "Search results for query: "

// stripKimiSearchPreamble 去掉文本开头重复出现的前言，返回剩余部分。
func stripKimiSearchPreamble(text string) string {
	for strings.HasPrefix(text, kimiSearchPreamble) {
		text = strings.TrimPrefix(text, kimiSearchPreamble)
	}
	return text
}

// countPreambleRun 返回文本开头连续完整前言的条数。
func countPreambleRun(text string) int {
	n := 0
	for strings.HasPrefix(text, kimiSearchPreamble) {
		text = strings.TrimPrefix(text, kimiSearchPreamble)
		n++
	}
	return n
}

// stripRepeatedPreamble 是 stripKimiSearchPreamble 的克制版：只有开头连续重复 ≥2 条
// （模型模仿的签名，实测均为 ×2/×3）才整段剥掉；恰好一条前言+文本是真搜索的 query
// 展示位，原样保留。剥完为空/纯前言的丢弃由调用方负责。
func stripRepeatedPreamble(text string) string {
	if countPreambleRun(text) < 2 {
		return text
	}
	return stripKimiSearchPreamble(text)
}

// isPreambleRun 报告流式累积文本是否仍可能是"纯前言"（前言的若干次重复 + 一个前言前缀）。
// 满足则继续憋着不转发；一旦岔开（接的是正文）即可剥离前言后补发。
func isPreambleRun(accum string) bool {
	return strings.HasPrefix(kimiSearchPreamble, stripKimiSearchPreamble(accum))
}

// stripSearchQueryEcho 删除文本里的搜索 query 回声行：以「Search results for query: 」
// 开头的整行（前缀+query 一起删，含无尾空格的裸前言形态）；行首粘连的重复裸前言
// （模型模仿签名，≥2 条）按 stripRepeatedPreamble 规则剥光留同行正文。删除留下的
// 行首空行一并去掉；不含回声行的文本经 Split/Join 恒等返回，一个字节都不动。
// 纯函数、幂等：同一文本任何时刻处理结果一致——这是请求体前缀缓存稳定的前提，
// 上游回放方向对每个请求、每条消息的每段文本无条件应用。
func stripSearchQueryEcho(text string) string {
	lines := strings.Split(text, "\n")
	kept := lines[:0]
	dropped := false
	for _, ln := range lines {
		if strings.HasPrefix(ln, kimiSearchPreamble) {
			if countPreambleRun(ln) >= 2 {
				ln = stripRepeatedPreamble(ln) // 模仿签名：剥裸前言留同行正文
			} else {
				dropped = true // 单条前言开头 = query 回声行：整行删除
				continue
			}
		} else if strings.TrimRight(ln, " \t\r") == strings.TrimSpace(kimiSearchPreamble) {
			dropped = true // 无尾空格的裸前言
			continue
		}
		kept = append(kept, ln)
	}
	if dropped {
		for len(kept) > 0 && strings.TrimSpace(kept[0]) == "" {
			kept = kept[1:] // 删除留下的行首空行
		}
	}
	return strings.Join(kept, "\n")
}

// holdSearchQueryEchoText 报告流式累积文本是否仍需憋着不转发：仍在前言碎片跑道
// （isPreambleRun），或按 stripSearchQueryEcho 剥完为空（回声行还没收完/块内还没有
// 正文）——两种情况都不能发任何事件；岔出非空正文时补发才开始。
func holdSearchQueryEchoText(accum string) bool {
	return isPreambleRun(accum) || strings.TrimSpace(stripSearchQueryEcho(accum)) == ""
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
		q := objStr(asObj(blk["input"]), "query")
		if q == "" {
			// 空搜索三连的 server_tool_use 无 id 无 input（见 kimiSearchPreamble）：
			// 没有信息量，丢弃。真搜索必带 query；其配套结果块的 sources 项照常生成。
			return nil
		}
		return map[string]interface{}{
			"id":     fmt.Sprintf("ws_%s_%d", responseID, idx),
			"type":   "web_search_call",
			"status": "completed",
			"action": map[string]interface{}{"type": "search", "query": q},
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

// searchBlocksFromResponsesItem 把回放历史里的搜索结构还原成 Anthropic 内容块。
// web_search_call 调用项一律不还原：它的 id 是代理自造的（ws_+响应 id），上游搜索
// 注册表从未登记，转成 server_tool_use 上行必 400——生产实证：搜索块出生 33 秒的
// 追问（#3）与 54 分钟的追问（#19）第一尝试都被拒，fail-soft 连坐把信封还原的真
// 搜索对一起剥掉，模型被迫重搜。搜索内容只由搜索信封承载（其块带原生注册 id，
// 实测回放 200），调用项只是客户端侧的展示件。已是 Anthropic 形状的
// server_tool_use/web_search_tool_result（防御性覆盖，id 为原生注册 id）有内容
// 的原样上行，空壳（无 input/无 content）删除。
func searchBlocksFromResponsesItem(item map[string]interface{}) []map[string]interface{} {
	switch objStr(item, "type") {
	case "web_search_call":
		return nil
	case "server_tool_use":
		// 非 Responses 协议项（防御性覆盖）：已是 Anthropic 块形状，带 input 内容
		// 的原样上行；空调用（Kimi 空搜索三连那种无 id 无 input 的壳）删除。
		inp := asObj(item["input"])
		if len(inp) == 0 {
			return nil
		}
		return []map[string]interface{}{{
			"type": "server_tool_use", "id": objStr(item, "id"),
			"name": objStr(item, "name"), "input": inp,
		}}
	case "web_search_tool_result":
		content := asArr(item["content"])
		if len(content) == 0 {
			return nil
		}
		tid := objStr(item, "tool_use_id")
		if tid == "" {
			tid = objStr(item, "id")
		}
		return []map[string]interface{}{{
			"type": "web_search_tool_result", "tool_use_id": tid, "content": content,
		}}
	}
	return nil
}
