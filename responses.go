package main

// responses.go — OpenAI Responses API listener port.
// Translates Responses-protocol requests into the Anthropic Messages protocol, internally calls the main handler through the existing
// routing/retry/streaming pipeline, then translates Anthropic responses back to the Responses protocol (streaming see responses_stream.go).
// Translation rules reference cc-switch transform_codex_anthropic.rs (request direction) and
// anthropic_response_to_responses (response direction), trimmed to this proxy's needs.

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

// ctxKeyTranslated is the internal request context key: marks this request as coming from the Responses translation port.
// The main handler flags the flight's origin from it (the web API column shows [translate]/[Response]).
// Context rather than a header — copyHeaders would pass a header through to the upstream; context never leaks.
type ctxKeyTranslatedT struct{}

var ctxKeyTranslated ctxKeyTranslatedT

// ctxKeyConvID is the internal request context key: hands the Responses request's session identifier
// (prompt_cache_key, always present in Codex; second choice client_metadata.thread_id) to the main handler,
// for the "cache age" column's per-session anchoring. Same rationale as ctxKeyTranslated: context, not headers, so nothing leaks upstream.
type ctxKeyConvIDT struct{}

var ctxKeyConvID ctxKeyConvIDT

// ctxKeyThink is the internal request context key: hands the passthrough branch's thinking mode (reasoning.effort as-is,
// the status page's 「API」 column thinking value) to the main handler — a passthrough body is in Responses format, which the handler's
// extractThinkMode can't read (no thinking field), so it's carried separately via context.
// Not needed by the translation branch: the translated body's thinking is what actually goes upstream; the handler extracts it directly.
// Same as ctxKeyTranslated: context, not headers, so nothing leaks upstream.
type ctxKeyThinkT struct{}

var ctxKeyThink ctxKeyThinkT

// ctxKeyNone2Low is the internal request context key: the convertOff2Low upgrade mode (int, see below).
// The main handler badges the flight [off->low] from it (n2lStealth only) and falls back to a thinking-off
// retry once when the upstream 400-rejects thinking (covers both n2lStealth and n2lTryOn).
type ctxKeyNone2LowT struct{}

var ctxKeyNone2Low ctxKeyNone2LowT

// convertOff2Low upgrade modes (responsesToAnthropicTriple's 4th return value, passed through via ctxKeyNone2Low):
const (
	n2lNone    = 0 // Not upgraded
	n2lStealth = 1 // Implicit upgrade: downstream explicitly disabled thinking → sent as low upstream; thinking blocks stripped on return (downstream unaware) + [off->low] badge
	n2lTryOn   = 2 // Fallback thinking-on: tool-continuation history isn't replayable — send the requested thinking mode anyway (the default,
	// allowNoThinkBlock4Anthropic=true) and arm the main handler's one-shot thinking-off retry for a real rejection; under route convertOff2Low the level
	// gets a low floor (none/unknown → low, keeping K3 off the K2.8 no-thinking variant). Thinking blocks aren't stripped — the downstream
	// asked for thinking, so blocks ride the return carrying signatures and next-turn history self-heals
)

// ctxKeyReqDown is the internal request context key: the Responses translation port hands the downstream request body's original text
// (Responses format, before translation) to the main handler, stored as the downstream side (downstream→proxy) of dual-link recording.
// Not set by the passthrough branch — the body forwards verbatim; both sides identical, no double storage.
// Same as ctxKeyTranslated: context, not headers, so nothing leaks upstream.
type ctxKeyReqDownT struct{}

var ctxKeyReqDown ctxKeyReqDownT

// ctxKeyTranslated's values — the same three states as flight.translated:
// translatedResponses means a Responses request was translated to Anthropic through the main pipeline (API column [translate]);
// translatedResponsesRaw means the hit route has url_response_api, so the Responses original passes through untranslated (API column [Response]).
const (
	translatedResponses    = "responses"
	translatedResponsesRaw = "responses-raw"
)

// thinkingEnvelopePrefix is the thinking-block envelope prefix: an Anthropic signed thinking block JSON is
// base64url'd with this prefix and tucked into Responses reasoning.encrypted_content returned to the client;
// when the client replays history next turn, this prefix is recognized and the thinking block restored. Self-contained, no upstream decryption needed
// (borrowing cc-switch reasoning_bridge's idea, with our own prefix to avoid confusion with other envelopes).
const thinkingEnvelopePrefix = "p429-ant-thinking-v1:"

// defaultResponsesMaxTokens is the default max_tokens when a Responses request doesn't carry max_output_tokens
// (Anthropic requires it; missing = 400).
const defaultResponsesMaxTokens = 32000

// responsesSrv tracks the Responses listener port's runtime state, for dynamic start/stop on config reload/switch (no process restart needed).
var responsesSrv = struct {
	sync.Mutex
	addr string       // Currently listening address (empty = not listening)
	srv  *http.Server // The running server, for shutdown
}{}

// reconcileResponsesServer aligns the Responses listener port to the configured address:
// unchanged address → no-op; changed (including added/disabled/re-addressed) → close the old listener first, then start the new.
// A listen failure only warns and disables; the main proxy is unaffected. Called from startup, config reload, and config switch.
func reconcileResponsesServer(listen string) {
	responsesSrv.Lock()
	defer responsesSrv.Unlock()
	if listen == responsesSrv.addr {
		return // Current state already equals the target (including both empty)
	}
	if responsesSrv.srv != nil {
		// Close and release the port immediately: the address changed, so in-flight requests on the old port are pointless to keep
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

// predictSearchTriple predicts this request's upstream attribution triple per the routing rules (the comparison baseline for search-envelope restoration).
// It replicates only the main path (fast literal name > routes wildcard > default upstream); classifier/text_only/no_search/
// enhanced-search conditional branches aren't predicted — a prediction miss at worst makes a restored envelope hit a 400, and the main pipeline's fail-soft strips and
// retries (see the handler's tool_call_id fallback); no wrong data is produced. When prediction is impossible (no route hit and
// default upstream empty) returns nil = envelopes always pass.
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

// matchRouteRule returns the first route matching model (skipping reserved-name patterns), nil on no hit.
// Translation-time prediction (search triple, route thinking shape) shares this single match point, same order
// and rules as the main handler's route loop. Note the fast literal-name lane doesn't go through this match (callers check fast themselves first).
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

// effectiveKey computes the auth token actually in effect for the upstream request: the route key when non-empty,
// otherwise the client's Authorization header value passes through (same semantics as the main handler's auth override).
func effectiveKey(routeAPI string, r *http.Request) string {
	if routeAPI != "" {
		return routeAPI
	}
	return r.Header.Get("Authorization")
}

// responsesHandler handles a Responses API request: translated to Anthropic, then internally calls the main handler.
func responsesHandler(w http.ResponseWriter, r *http.Request) {
	c := cfg.Load()
	// Same semantics as the main handler: the forwarding lane is always localhost-only.
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

	// Session identifier (for the status page's "cache age" column): Codex always carries prompt_cache_key (= session UUID),
	// second choice client_metadata.thread_id (same value as the former in Codex). Read-only; the request body isn't modified.
	convID, _ := body["prompt_cache_key"].(string)
	if convID == "" {
		if cm, ok := body["client_metadata"].(map[string]interface{}); ok {
			convID, _ = cm["thread_id"].(string)
		}
	}

	// Thinking mode (the status page's 「API」 column thinking value): passthrough branch only — passthrough is zero-modification, so the reasoning.effort
	// the upstream receives is exactly it; the translation branch doesn't go through here (the translated body's thinking is extracted by the main handler from the final body).
	think := responsesThinkMode(body)

	// Native-passthrough precheck: when the route matched by model has url_response_api, the Responses original isn't translated
	// and goes to the main handler as-is — the passthrough branch in the route loop switches the upstream to that route's url_response_api.
	// Monitoring stream/stats/retry pipeline identical to translated streams (the main handler parses per Responses semantics per the ctx flag).
	// The precheck only decides "translate or passthrough"; the real routing decision is the handler's (a 502 is reported if a config hot-reload removes the route).
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
		// Headers keep the client's originals: the upstream is a native Responses service; Authorization etc. are overridden by the target key on route hit.
		handler(w, r2)
		return
	}

	// Translation-time search-restoration context: time-rule strip counting / conversation watermark / restore-moment collection;
	// handed down with the internal request; the main handler feeds the strip count into the flight ([剥N] display)
	// and learns the conversation watermark from restore moments during 400-fallback stripping.
	replay := &searchReplayCtx{convID: convID}
	// The route-declared thinking shape (thinking parameter) and the convertOff2Low scope: at translation time only the client model
	// name is known; the adaptive/budget decision defaults to the client name's mapping table — when the target model's capability
	// disagrees, the route config overrides. convertOff2Low likewise resolves by client model name pre-match ("translate"/"all" enable it here).
	thinkStyle := ""
	off2Low := ""
	if rr := matchRouteRule(c, origModel); rr != nil {
		thinkStyle = rr.Thinking
		off2Low = rr.ConvertOff2Low
	}
	// The Codex menu's fast lane goes by the literal name "fast_route": its convertOff2Low comes from fast_route itself,
	// taking priority over any catch-all routes[] entry the name may have pre-matched above.
	if origModel == "fast_route" && c.FastRoute != nil {
		off2Low = c.FastRoute.ConvertOff2Low
	}
	anth, reg, n2l, err := responsesToAnthropicTriple(body, predictSearchTriple(c, r, origModel), replay, thinkStyle, off2Low == "translate" || off2Low == "all", allowNoThinkBlock4Anthropic(c))
	if err != nil {
		writeResponsesError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}
	// Upstream always goes streaming (same philosophy as convertAlltoStream): the web page can monitor the output; the return side
	// translates SSE live or collects and returns a one-shot Responses JSON per the client's needs.
	anth["stream"] = true
	// fast_route's entry name in the Codex menu is the literal "fast_route" (Codex doesn't validate catalog names against the config):
	// selecting it injects speed:"fast", and the main handler's fast branch takes over (rerouting to the fast_route upstream,
	// model rewritten to fast_route.model). "fast_route" is also a route-pattern reserved name, guarding against name-collision interception.
	if fr := c.FastRoute; fr != nil && fr.URL != "" && fr.Model != "" && origModel == "fast_route" {
		anth["speed"] = "fast"
	}
	newBody, err := json.Marshal(anth)
	if err != nil {
		writeResponsesError(w, http.StatusInternalServerError, "api_error", "marshal converted body failed")
		return
	}

	// Building the internal request: path swapped to /v1/messages, only auth headers kept (overridden by the target key on route hit);
	// Codex client's OpenAI-specific headers aren't taken along to bother the Anthropic upstream.
	r2 := r.Clone(r.Context())
	r2 = r2.WithContext(context.WithValue(r2.Context(), ctxKeyTranslated, translatedResponses))
	r2 = r2.WithContext(context.WithValue(r2.Context(), ctxKeySearchReplay, replay))
	if n2l != n2lNone {
		r2 = r2.WithContext(context.WithValue(r2.Context(), ctxKeyNone2Low, n2l))
	}
	// Dual-link recording: the downstream Responses original goes via context to the main handler for reqDown
	// (necessarily different from the translated body, always stored; not set by the passthrough branch — body forwards verbatim, both sides identical).
	r2 = r2.WithContext(context.WithValue(r2.Context(), ctxKeyReqDown, raw))
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
	// convertOff2Low implicit-upgrade stream (downstream explicit thinking-off → low): thinking blocks are stripped on return (streaming by the conv
	// state machine, non-streaming by finishBuffered wholesale conversion); the downstream still sees a thinking-off response; usage
	// untouched, passed through truthfully. History-fallback thinking-on (at the downstream-requested level) doesn't strip: the downstream asked for thinking; blocks ride the return
	// carrying signatures.
	tw.stripThinking = n2l == n2lStealth
	tw.conv.stripThinking = n2l == n2lStealth
	handler(tw, r2)
	tw.finish()
	// Dual-link recording tail back-fill: a non-streaming client's downstream-side return is produced by finish after the handler archives,
	// so the downstream-side bytes tapped into the flight are flushed into the finished archive (streaming clients already fully recorded during forwarding; flushing the same value is harmless).
	if tw.downFlight != nil {
		refreshArchivedDown(tw.downFlight)
	}
}

// writeResponsesError returns a Responses-protocol-style error JSON.
func writeResponsesError(w http.ResponseWriter, status int, typ, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]interface{}{
		"error": map[string]interface{}{"type": typ, "message": msg},
	})
}

// ---- Helpers: map value extraction ----

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

// ---- Request translation: Responses → Anthropic ----

// responsesToAnthropic translates a Responses API request body into an Anthropic Messages request body.
// Mirrors cc-switch responses_request_to_anthropic. The returned tool registry records the original identities of custom/
// namespace/tool_search tools; response translation (streaming and non-streaming) unpacks per it.
func responsesToAnthropic(body map[string]interface{}) (map[string]interface{}, *toolRegistry, error) {
	out, reg, _, err := responsesToAnthropicTriple(body, nil, nil, "", false, true) // Test-facing wrapper: exercises the default (allowNoThinkBlock4Anthropic=true) semantics
	return out, reg, err
}

// responsesToAnthropicTriple is responsesToAnthropic plus this request's route-prediction triple
// (comparison baseline for search-envelope restoration; nil = unpredictable, envelopes always pass), the search-restoration context
// (time-rule strip counting / conversation watermark / restore-moment collection; nil = convert only, no stats), and the route-declared target
// model thinking shape thinkStyle (""/"auto"=look up by client model name; "adaptive"=force adaptive;
// "budget"=force enabled+budget_tokens — the route thinking parameter, solving target-model vs client-alias
// thinking-capability mismatches, e.g. the client says claude-fable-5 but actually routes to a budget-only Kimi).
// none2Low=true (route convertOff2Low = "translate"/"all") upgrades in two places (4th return value n2l; three states see the constants):
// ① downstream explicit thinking-off (effort none/off/disabled) → sent as low upstream, n2l=n2lStealth (thinking blocks stripped
// on return, downstream unaware; usage passed through truthfully); ② tool-continuation history not replayable (trailingTurnSupportsThinking
// =false) — thinking-off happens to trigger Kimi routing K3 to the K2.8 no-thinking variant (exactly what this parameter guards against), so there's
// no downgrading: send at the downstream-requested level as-is (none/unknown → low floor), n2l=n2lTryOn (no stripping: blocks ride the return
// carrying signatures, next-turn history self-heals).
// allowNoThink (top-level allowNoThinkBlock4Anthropic, default true) governs the history door independently of none2Low: true = the normal
// thinking decision goes upstream as-is with n2l=n2lTryOn armed — if the upstream really rejects "unsigned history with thinking on"
// (the rejection is level-independent), the main handler's one-shot fallback retreats to thinking-off and resends; false = cc-switch mode,
// thinking preemptively off over such histories (explicit thinking-off requests never take this door — they follow ① or plain disabled).
// Under none2Low, true keeps ②'s floor-low try-on verbatim.
// Motivation: Kimi's docs — "turning off thinking routes to the K2.8 Preview no-thinking variant"; keeping thinking on keeps K3 from being downgraded.
func responsesToAnthropicTriple(body map[string]interface{}, reqTriple *searchTriple, replay *searchReplayCtx, thinkStyle string, none2Low bool, allowNoThink bool) (map[string]interface{}, *toolRegistry, int, error) {
	result := map[string]interface{}{}
	if model := objStr(body, "model"); model != "" {
		result["model"] = model
	}

	// The tool registry is built before messages conversion: function_call replay needs it to resolve namespace names back.
	reg := buildToolRegistry(asArr(body["tools"]))

	// instructions + role=system/developer texts from input → system (joined with \n\n).
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

	// input → messages (string form = a single user text).
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
			return nil, nil, n2lNone, err
		}
	}

	// Normalization: drop incomplete tool turns first (Anthropic requires tool_use and tool_result adjacent-paired),
	// then guarantee the first message is user; trailing assistant text is trimmed per prefill rules.
	msgs = dropIncompleteToolTurns(msgs)
	msgs = dropEmptyMessages(msgs)
	msgs = ensureLeadingUserMessage(msgs)
	if len(msgs) == 0 {
		return nil, nil, n2lNone, fmt.Errorf("cannot convert request: empty messages")
	}
	trimTrailingAssistantText(msgs)
	msgs = dropEmptyMessages(msgs)
	if len(msgs) == 0 {
		return nil, nil, n2lNone, fmt.Errorf("cannot convert request: empty messages")
	}
	result["messages"] = msgs

	// max_output_tokens → max_tokens (required).
	maxTokens := toInt64(body["max_output_tokens"])
	if maxTokens <= 0 {
		maxTokens = defaultResponsesMaxTokens
	}
	result["max_tokens"] = maxTokens

	// reasoning.effort → thinking. Adaptive models (usesAdaptiveThinking mapping table) go
	// thinking:{type:"adaptive"} + output_config.effort; the rest go enabled+budget_tokens.
	// The decision defaults to the client-sent model name (route rewriting happens later, in the handler), consistent with cc-switch
	// reading body.model. Mirrors transform_codex_anthropic.rs 312-367.
	// When thinkStyle isn't auto, the route declaration overrides the decision: when the target model's capability disagrees with the client alias
	// (e.g. an alias called claude-fable-5 actually routing to a budget-only upstream) the route config wins.
	effort := objStr(asObj(body["reasoning"]), "effort")
	model := objStr(body, "model")
	adaptiveModel := usesAdaptiveThinking(model)
	cannotDisable := thinkingCannotBeDisabled(model)
	switch thinkStyle {
	case "adaptive":
		// Route declares the target model supports adaptive: force the adaptive form (turning thinking off is still allowed —
		// the target model's real capability is the configurer's responsibility; the fable/mythos can't-disable rule no longer applies).
		adaptiveModel = true
		cannotDisable = false
	case "budget":
		// Route declares the target model supports only classic enabled+budget_tokens.
		adaptiveModel = false
		cannotDisable = false
	}
	explicitlyDisabled := reasoningExplicitlyDisabled(effort)
	adaptiveEffort := codexEffortToAnthropic(effort)
	adaptiveShouldThink := adaptiveModel && (adaptiveThinkingIsDefault(model) || adaptiveEffort != "")
	historyValid := trailingTurnSupportsThinking(msgs)
	thinkingEnabled := false
	budget := effortToThinkingBudget(effort)
	// n2l: the convertOff2Low upgrade mode (three states see the constants) — the return side strips thinking blocks
	// per it for implicit upgrades (n2lStealth) (the main handler adds the [off->low] badge); both upgrade kinds retreat and retry on upstream 400 rejection.
	n2l := n2lNone
	// upgradeNoneToLow upgrades thinking-off to low: adaptive models get thinking:adaptive+effort:low;
	// budget models get enabled+2048 (capped at maxTokens/2; abandoned if the 1024 floor doesn't fit, staying off).
	// With thinkingEnabled on, temperature/top_p aren't passed through (same rule as the normal thinking path).
	upgradeNoneToLow := func() bool {
		if adaptiveModel {
			result["thinking"] = map[string]interface{}{"type": "adaptive"}
			result["output_config"] = map[string]interface{}{"effort": "low"}
			thinkingEnabled = true
			return true
		}
		b, ok := lowThinkingBudget(maxTokens) // Shared with the native port (off2low.go): same cap and floor
		if !ok {
			return false
		}
		budget = b
		thinkingEnabled = true // The budget model's thinking field is written by the unified tail after the switch
		return true
	}
	// tryRequestedThinking enables thinking at the downstream-requested level (history-fallback branch): adaptive models get
	// output_config.effort = the downstream level (none/unknown → low floor); budget models get the matching budget
	// (none/unknown → 2048; capped at maxTokens/2, abandoned if the 1024 floor doesn't fit).
	tryRequestedThinking := func() bool {
		if adaptiveModel {
			e := adaptiveEffort
			if e == "" {
				e = "low" // Downstream gave no level: low floor, keeping K3 on the thinking path
			}
			result["thinking"] = map[string]interface{}{"type": "adaptive"}
			result["output_config"] = map[string]interface{}{"effort": e}
			thinkingEnabled = true
			return true
		}
		b := effortToThinkingBudget(effort)
		if b <= 0 {
			b = effortToThinkingBudget("low")
		}
		if ceiling := maxTokens / 2; b > ceiling {
			b = ceiling
		}
		if b < 1024 {
			return false
		}
		budget = b
		thinkingEnabled = true // The budget model's thinking field is written by the unified tail after the switch
		return true
	}
	// A tool continuation missing a signed thinking block can't be replayed with thinking on (real Anthropic 400s). allowNoThink
	// (top-level allowNoThinkBlock4Anthropic, default true) picks the strategy for such histories: true = send the normal thinking
	// decision below anyway and arm the main handler's one-shot thinking-off retry (n2lTryOn); false = cc-switch mode (thinking
	// preemptively off). Models that can't disable thinking error out directly (cc-switch's wording copied).
	if !historyValid && cannotDisable {
		return nil, nil, n2lNone, fmt.Errorf("Anthropic model requires thinking, but the tool history has no signed thinking block to replay")
	}
	switch {
	case !historyValid && explicitlyDisabled:
		// The downstream asked for thinking off: an off request can't 400 on thinking, so allowNoThink doesn't apply here;
		// convertOff2Low's implicit upgrade (low + strip on return) still governs. Otherwise off stays off — adaptive
		// default-on models get an explicit disabled; budget models simply carry no thinking field.
		if none2Low && upgradeNoneToLow() {
			n2l = n2lStealth
		} else if adaptiveShouldThink {
			result["thinking"] = map[string]interface{}{"type": "disabled"}
		}
	case !historyValid && none2Low && allowNoThink:
		// convertOff2Low's anti-downgrade door (#11/#12 live incident: off = Kimi routes K3 to the K2.8 no-thinking variant):
		// send at the downstream-requested level (none/unknown → low floor); no stripping — blocks ride the return carrying
		// signatures, next-turn history self-heals; the main handler's one-shot fallback retreats to thinking-off on a real rejection.
		if tryRequestedThinking() {
			n2l = n2lTryOn
		}
	case !historyValid && !allowNoThink:
		// cc-switch mode (allowNoThinkBlock4Anthropic=false): thinking preemptively off over a non-replayable history.
		if adaptiveShouldThink {
			result["thinking"] = map[string]interface{}{"type": "disabled"}
		}
	case adaptiveShouldThink && (!explicitlyDisabled || cannotDisable):
		thinkingEnabled = true
		result["thinking"] = map[string]interface{}{"type": "adaptive"}
		if adaptiveEffort != "" {
			result["output_config"] = map[string]interface{}{"effort": adaptiveEffort}
		} else if explicitlyDisabled && cannotDisable {
			// Fable/Mythos can't disable thinking: use low to express Codex's explicit none.
			result["output_config"] = map[string]interface{}{"effort": "low"}
			if none2Low {
				n2l = n2lStealth // The downstream wanted thinking off: with convertOff2Low on, thinking blocks are stripped on the return side
			}
		}
		if !historyValid {
			// allowNoThink=true without convertOff2Low (the default): the normal decision goes upstream as-is, armed — the
			// upstream's rejection of "thinking on without thinking history" is level-independent, so the one-shot fallback covers it.
			n2l = n2lTryOn
		}
	case explicitlyDisabled:
		if none2Low && upgradeNoneToLow() {
			n2l = n2lStealth
		} else {
			result["thinking"] = map[string]interface{}{"type": "disabled"}
		}
	case budget > 0:
		// The budget caps at half of max_tokens (leaving room for the visible answer); if the 1024 floor doesn't fit, thinking isn't opened.
		if ceiling := maxTokens / 2; budget > ceiling {
			budget = ceiling
		}
		if budget >= 1024 {
			thinkingEnabled = true
			if !historyValid {
				n2l = n2lTryOn // Same allowNoThink arming as the adaptive arm (reached with !historyValid only in try-first mode)
			}
		}
	}
	if thinkingEnabled && !adaptiveModel {
		result["thinking"] = map[string]interface{}{"type": "enabled", "budget_tokens": budget}
	}
	// With thinking on, Anthropic requires dropping temperature/top_p (mutually exclusive); only passed through when off.
	if !thinkingEnabled {
		if v, ok := body["temperature"]; ok {
			result["temperature"] = v
		}
		if v, ok := body["top_p"]; ok {
			result["top_p"] = v
		}
	}

	// tools: converted via the registry (function/custom/namespace/tool_search/web_search each mapped).
	if len(reg.tools) > 0 {
		result["tools"] = reg.tools
		// tool_choice is only converted when tools is non-empty (Anthropic 400s on tool_choice without tools).
		if tc, ok := body["tool_choice"]; ok && tc != nil {
			mapped := mapToolChoiceToAnthropic(tc, reg)
			// Anthropic rejects forced tool_choice with thinking on: models that can't disable error out;
			// the rest keep the user's tool constraint and turn thinking off for this request (restoring temperature/top_p),
			// without silently weakening a required/specified choice. Mirrors cc-switch 398-425.
			if t := objStr(mapped, "type"); thinkingEnabled && (t == "any" || t == "tool") {
				if cannotDisable {
					return nil, nil, n2lNone, fmt.Errorf("Anthropic model requires adaptive thinking and cannot honor a forced tool_choice")
				}
				result["thinking"] = map[string]interface{}{"type": "disabled"}
				delete(result, "output_config")
				n2l = n2lNone // Thinking has been turned off: the upstream won't produce thinking blocks, so the return side needs no stripping
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
	return result, reg, n2l, nil
}

// responsesSystemText extracts the text of system/developer message items (content as a string or a parts array).
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

// convertInputToMessages re-nests the flat Responses input[] into Anthropic messages.
// Mirrors cc-switch convert_input_to_messages:
//   - input_text/output_text → text blocks of the corresponding role
//   - input_image → image blocks; input_file → document blocks; refusal → text blocks
//   - function_call → assistant tool_use blocks (merged into the preceding assistant message; with a
//     namespace, resolved back to the flattened name via the registry; input goes through Read sanitize)
//   - custom_tool_call → tool_use (input wrapped as {"input": raw value})
//   - tool_search_call → tool_use (proxy tool name, arguments object as input)
//   - function_call_output/custom_tool_call_output/tool_search_output → user
//     tool_result blocks (consecutive ones merged into the same user message)
//   - reasoning.encrypted_content with our envelope prefix → restored signed thinking blocks;
//     with the search envelope prefix and a triple matching this request's route prediction → full search blocks restored (see searchEnvelopePrefix)
func convertInputToMessages(items []interface{}, reg *toolRegistry, reqTriple *searchTriple, replay *searchReplayCtx) ([]map[string]interface{}, error) {
	var msgs []map[string]interface{}
	var reqMask []byte // Key-derived mask from the route prediction (nil = unpredictable; the envelope can't be unmasked and is naturally skipped)
	if reqTriple != nil {
		reqMask = reqTriple.mask
	}
	for _, it := range items {
		item := asObj(it)
		if item == nil {
			continue
		}
		itemType := objStr(item, "type")
		// Historically unfinished (incomplete) tool calls are dropped wholesale; replaying them 400s.
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
			// A custom tool's input is a raw value (usually a string): wrap it into {"input": ...} to match the wrapper schema.
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
			// Strip search query echo lines from history (see stripSearchQueryEcho), applied unconditionally to all text.
			if t := stripSearchQueryEcho(objStr(item, "text")); isMeaningfulText(t) {
				pushBlock(&msgs, "user", map[string]interface{}{"type": "text", "text": t})
			}
		case "input_image", "input_file":
			// Top-level bare attachment parts: when no standard shape is recognized, serialize to text as a fallback instead of silently dropping
			// (see pushMediaPart).
			pushMediaPart(&msgs, "user", item)
		case "reasoning":
			enc := objStr(item, "encrypted_content")
			if b := decodeThinkingEnvelope(enc); b != nil {
				pushAssistantThinkingBlock(&msgs, b)
			} else if tri, blocks, ts, ok := decodeSearchEnvelope(enc, reqMask); ok {
				// Search envelope: restored upstream only when it unmasks (same-origin key) AND the url matches too — a different-origin one can't
				// unmask anyway; skipping saves tokens; cross-model isn't blocked (field-tested to decrypt fine). reqMask
				// nil = this request's route key unpredictable; unmasking fails and it's naturally skipped (v1's pass-through
				// branch retired together with the plaintext payload).
				if reqTriple == nil || reqTriple.sameOrigin(tri) {
					// Proactive watermark stripping (no 400 hit, no retry budget burned): once this conversation has learned a watermark, envelopes no newer
					// than it are all stripped by default (the registry expires by age: a 400 hit means the oldest died,
					// so same-age and older ones are certainly dead). No fixed age ceiling — a 1.7h-old sealed id was
					// field-tested still alive, and a fixed ceiling would kill live envelopes. Old envelopes without ts (v2's first version, all
					// predating the ts era) are treated as oldest: stripped when a watermark exists, optimistically restored without one.
					// Strip counts and restore moments are both recorded into replay (nil = convert only, no stats).
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
						// Only envelopes with ts are collected as restore moments: ts-less envelopes taking part in the oldest-pick would poison watermark learning
						if replay != nil && !ts.IsZero() {
							replay.restored = append(replay.restored, ts)
						}
					}
				}
			}
		case "web_search_call", "server_tool_use", "web_search_tool_result":
			// web_search_call items aren't replayed (proxy-minted ids going upstream must 400; see
			// searchBlocksFromResponsesItem); Anthropic-shaped search-block shells are deleted wholesale, content-bearing
			// ones go upstream as-is. The only replay carrier of search content is the search envelope above.
			for _, b := range searchBlocksFromResponsesItem(item) {
				pushBlock(&msgs, "assistant", b)
			}
		default:
			// message items or role-bearing items: system/developer were already collected into system above; skipped here.
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

// toolResultContentFromResponsesItem converts a *_call_output item's output field into
// Anthropic tool_result content (string or block array) plus is_error.
// Mirrors cc-switch's same-named function: error-marker text sets is_error; array parts support
// input_text/output_text/input_image/input_file (unrecognized ones try media stripping first, then serialize);
// any value may hide image media (MCP image blocks, JSON strings, whole data URLs), stripped into image blocks.
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
				// Other shapes (MCP image blocks, etc.): try media stripping first; if unrecognized, serialize to text.
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
		// output absent: serialize the whole item as text (mirrors cc-switch's None branch).
		return canonicalJSON(item), false
	default:
		// Oddly-shaped output like numbers/objects: try media stripping first, otherwise serialize to a JSON string as text.
		if content, isError, ok := alternateImageToolResultContent(out); ok {
			return content, isError
		}
		return canonicalJSON(out), false
	}
}

// imageBlockFromInputImage converts a Responses input_image (data URL or http URL) into an Anthropic image block.
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

// ---- Message-array normalization (mirrors cc-switch's same-named helpers) ----

// pushBlock appends a content block: merged into the trailing message when same-role, otherwise a new message is started.
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

// pushMediaPart converts input_image/input_file parts into Anthropic image/document blocks and appends.
// Unrecognized shapes (blob:/file: local URLs, file_id cloud references, broken data URLs, etc.) serialize into
// a text block as fallback: the bytes being unreachable is an objective limitation, but a block silently vanishing isn't — same
// fallback semantics as the tool-result parts path (see toolResultContentFromResponsesItem).
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

// pushToolResultBlock appends a tool_result: preserving Anthropic's required order — tool_result blocks
// must come before any text/image blocks in the user message.
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

// pushAssistantThinkingBlock inserts a thinking block: Anthropic requires it at the front of the assistant message's
// content array (after existing thinking/redacted_thinking blocks, before all others).
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

// ensureLeadingUserMessage guarantees a leading user message: compacted/resumed sessions may start with an assistant or
// function_call; Anthropic requires user first, else 400.
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

// dropIncompleteToolTurns drops tool turns that no longer form complete "assistant tool_use → user tool_result"
// adjacent pairs (common in compacted/resumed sessions). Anthropic requires every tool_use to be fully answered
// in the immediately following user message, else 400.
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
		// Assistant carrying tool_use: check the immediately following user answers it completely.
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
			// The whole assistant tool turn is dropped; the user message is kept if it has content left after removing the tool_result.
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

// messageBlockIDs collects the id field values of blocks of the given types in a message.
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

// dropToolResultBlocks returns a copy of the message with tool_result blocks removed.
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

// dropEmptyMessages drops messages with an empty content array (Anthropic 400s on empty content).
func dropEmptyMessages(msgs []map[string]interface{}) []map[string]interface{} {
	var out []map[string]interface{}
	for _, m := range msgs {
		if messageHasContent(m) {
			out = append(out, m)
		}
	}
	return out
}

// trimTrailingAssistantText trims the last text block of the trailing assistant message:
// pure-whitespace prefill gets the block deleted; non-empty gets trailing whitespace removed (Anthropic rejects prefill ending in whitespace).
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

// effortToThinkingBudget maps Codex's reasoning.effort to an Anthropic thinking budget.
// Unrecognized values return 0 (thinking not opened, normal sampling kept). Mirrors cc-switch's same-named function.
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

// ---- Thinking model mapping tables (copied from cc-switch thinking_optimizer.rs) ----

// normalizeThinkingModelName normalizes a model name for mapping-table matching: lowercase, '.' and '_' become '-'.
func normalizeThinkingModelName(model string) string {
	return strings.NewReplacer(".", "-", "_", "-").Replace(strings.ToLower(strings.TrimSpace(model)))
}

// thinkingModelContains reports whether the normalized model name contains any of the substrings.
func thinkingModelContains(model string, needles ...string) bool {
	n := normalizeThinkingModelName(model)
	for _, s := range needles {
		if strings.Contains(n, s) {
			return true
		}
	}
	return false
}

// usesAdaptiveThinking mapping table: models that go adaptive thinking (thinking:{type:"adaptive"} +
// output_config.effort) rather than enabled+budget_tokens.
func usesAdaptiveThinking(model string) bool {
	return thinkingModelContains(model,
		"fable-5", "mythos-5", "mythos-preview", "sonnet-5",
		"opus-4-8", "opus-4-7", "opus-4-6", "sonnet-4-6")
}

// adaptiveThinkingIsDefault mapping table: models that default to adaptive on without an explicit thinking parameter.
func adaptiveThinkingIsDefault(model string) bool {
	return thinkingModelContains(model, "fable-5", "mythos-5", "mythos-preview", "sonnet-5")
}

// thinkingCannotBeDisabled mapping table: models that reject thinking:{type:"disabled"}.
func thinkingCannotBeDisabled(model string) bool {
	return thinkingModelContains(model, "fable-5", "mythos-5")
}

// codexEffortToAnthropic maps Codex's reasoning.effort to output_config.effort
// (for adaptive models). Unrecognized values return empty. Mirrors cc-switch's same-named function.
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

// reasoningExplicitlyDisabled reports whether reasoning.effort explicitly disables thinking. Mirrors cc-switch's same-named function.
func reasoningExplicitlyDisabled(effort string) bool {
	switch strings.ToLower(strings.TrimSpace(effort)) {
	case "none", "off", "disabled":
		return true
	}
	return false
}

// responsesThinkMode extracts a Responses request body's thinking config, for the status page's 「API」 column thinking-value display
// (passthrough branch — passthrough is zero-modification, so reasoning.effort is what actually goes upstream).
// Values take the shortest form (the column's green color carries the Responses family; no "effort·" prefix):
// reasoning.effort shows just the level word ("high"…), explicit-off values (none/off/disabled) merge into "关";
// no reasoning/effort field returns empty (column shows -).
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

// trailingTurnSupportsThinking reports whether the trailing turn supports turning thinking on. Mirrors cc-switch's same-named function:
// a brand-new user question can; a tool-result continuation only if the immediately preceding assistant carries signed
// thinking/redacted_thinking blocks (and the tool_result ids all pair with its tool_use) —
// otherwise Anthropic 400s on a missing signed-thinking replay.
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

// mapToolChoiceToAnthropic converts tool_choice: required→any, auto→auto, none→none;
// {type:function}→{type:tool} (with a namespace, resolved back to the flattened name); {type:custom}→same-named tool;
// {type:tool_search}→proxy tool name; other shapes (allowed_tools etc.) degrade to auto to avoid 400.
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

// ---- Thinking-block envelopes ----

// encodeThinkingEnvelope encodes an Anthropic signed thinking/redacted_thinking block into a
// Responses reasoning.encrypted_content string (version-prefixed base64url JSON).
// Blocks without a signature (empty signature/data) aren't encoded — replaying them is pointless.
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

// decodeThinkingEnvelope recognizes the envelope prefix and restores the thinking block; returns nil for foreign envelopes.
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
	// Reuses the encoder's validation: the decoded result must be a signed thinking block, keeping malformed envelopes out of tool turns.
	if encodeThinkingEnvelope(block) == "" {
		return nil
	}
	return block
}

// ---- Search-block envelopes ----

// searchEnvelopePrefix is the search-block envelope prefix: one search's server_tool_use +
// web_search_tool_result blocks together with the attribution triple, XOR-obfuscated with a key-derived mask, base64url'd
// with this prefix, tucked into Responses reasoning.encrypted_content returned to the client (same channel as thinking
// envelopes). The client replays it verbatim next turn; the proxy unseals the envelope and restores the full search structure upstream — the model
// reads last search's content directly from it, no re-search with the same keywords needed. Since v2 the payload is stored obfuscated: envelopes lie around in client-side history
// (Codex session logs) without lying around as plaintext url/model/key hashes (user requirement; obfuscation, not encryption).
const searchEnvelopePrefix = "p429-ant-search-v2:"

// searchReplayCtx is the per-request context of search-envelope restoration (translation phase is single-goroutine, lock-free):
// convID is for querying/learning the conversation watermark; proactiveCutoff counts watermark-proactively-stripped blocks (part of
// [剥N], the split only goes to logs); restored collects the sealing moments of actually-restored envelopes (ts-less ones not collected) —
// the oldest of them is learned as the conversation watermark during 400-fallback stripping.
type searchReplayCtx struct {
	convID          string
	proactiveCutoff int // Number of blocks stripped by the conversation watermark
	restored        []time.Time
}

// ctxKeySearchReplay is the internal request context key: hands the translation-time search-restoration context
// to the main handler (strip counts into the flight, watermark learning on 400). Same rationale as
// ctxKeyTranslated: context, not headers, so nothing leaks upstream.
type ctxKeySearchReplayT struct{}

var ctxKeySearchReplay ctxKeySearchReplayT

// searchCutoff records the envelope watermark per conversation (lower bound of sealing moments): envelopes no newer than the watermark are all stripped by default
// (the registry expires by age: a 400 hit means the oldest died, so same-age and older ones are certainly dead). Learned during 400-fallback
// stripping — no a-priori assumption about search ids' real lifetimes, fully self-adaptive per conversation measurement;
// only rises, never falls (the newer the watermark, the more gets stripped; old info has been superseded by new info).
var searchCutoff = struct {
	sync.Mutex
	m map[string]time.Time
}{m: make(map[string]time.Time)}

// searchCutoffFor queries the conversation watermark; no record returns the zero value (no envelope is blocked).
func searchCutoffFor(convID string) time.Time {
	searchCutoff.Lock()
	defer searchCutoff.Unlock()
	return searchCutoff.m[convID]
}

// learnSearchCutoff raises the conversation watermark to ts (only rises, never falls).
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

// searchTriple is a search envelope's attribution info: url+model+key hash. KeyH stores only the key's
// first 16 hex of sha256, never the bare key. Model is only a troubleshooting reference, not part of the restoration comparison — see sameOrigin.
// mask is the key-derived XOR mask (json:"-" never envelope-serialized): both encode and decode need it;
// it's derived by newSearchTriple together once routing is settled.
type searchTriple struct {
	URL   string `json:"u"`
	Model string `json:"m"`
	KeyH  string `json:"k"`
	mask  []byte `json:"-"`
}

// newSearchTriple builds the attribution triple: api is the actually-effective auth token (effectiveKey semantics);
// only its hash is stored, and the envelope obfuscation mask is derived.
func newSearchTriple(url, model, api string) *searchTriple {
	return &searchTriple{URL: url, Model: model, KeyH: hashSearchKey(api), mask: searchEnvMask(api)}
}

// searchEnvMask derives a 32-byte obfuscation mask from the api key (domain-separated, not same-source as KeyH).
func searchEnvMask(api string) []byte {
	sum := sha256.Sum256([]byte("p429-search-mask\x00" + api))
	return sum[:]
}

// xorSearchMask applies the cyclic XOR mask (obfuscate/de-obfuscate are the same function). Pure XOR, no encryption overhead.
func xorSearchMask(mask, p []byte) []byte {
	out := make([]byte, len(p))
	for i := range p {
		out[i] = p[i] ^ mask[i%len(mask)]
	}
	return out
}

// sameOrigin reports whether two attributions are the same upstream source: only the url+key legs are compared. The model leg doesn't participate —
// field-tested 2026-09-10: cross-model replay on the same endpoint and key (k3-256k's search blocks handed to
// kimi-for-coding follow-ups) returned 200 with zero re-search and the model gave body-level summaries; cross-model restoration isn't blocked.
func (t *searchTriple) sameOrigin(o *searchTriple) bool {
	return t.URL == o.URL && t.KeyH == o.KeyH
}

// hashSearchKey computes the comparison hash of an api key (first 16 hex of sha256). The parameter is the actually-effective
// auth token: the route key when non-empty, otherwise the client's passed-through Authorization header value.
func hashSearchKey(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])[:16]
}

// encodeSearchEnvelope encodes one search's two Anthropic blocks plus the attribution triple into an envelope string.
// Not encoded when the triple/blocks are missing or the server_tool_use has no query (empty search) — a shell has no replay value.
func encodeSearchEnvelope(t *searchTriple, useBlk, resBlk map[string]interface{}) string {
	if t == nil || useBlk == nil || resBlk == nil {
		return ""
	}
	if objStr(asObj(useBlk["input"]), "query") == "" {
		return ""
	}
	// id normalization: Kimi streaming search's server_tool_use block id starts with tool_, but the search registry
	// only registers the result block's srvtoolu_ id — replaying by stu.id misses the registry and 400s with
	// tool_call_id is not found (2026-09-10 controlled experiment: verbatim streaming replay 400'd; rewriting stu.id
	// to result.tool_use_id returned 200; non-streaming search has both naturally identical, unaffected).
	// The envelope is the only replay carrier; normalization happens at sealing time; the shallow copy doesn't mutate the caller's shared blocks.
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
		"ts": time.Now().Unix(), // Sealing moment: the basis for the restore side's per-conversation watermark stripping
	})
	if err != nil {
		return ""
	}
	// Obfuscated storage: with the mask missing, rather emit no envelope than a plaintext payload.
	if len(t.mask) == 0 {
		return ""
	}
	return searchEnvelopePrefix + base64.RawURLEncoding.EncodeToString(xorSearchMask(t.mask, payload))
}

// decodeSearchEnvelope recognizes the search envelope prefix and restores the triple, the two content blocks, and the sealing moment ts
// (old envelopes without a ts field return the zero value — the restore side treats them as oldest: stripped when a watermark exists, optimistically restored without one).
// mask is this request's route-prediction key-derived mask (nil/wrong → the XOR result isn't JSON, naturally
// ok=false — the key leg's comparison is contained in the de-obfuscation itself); foreign envelopes or wrong block shapes
// return ok=false.
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

// stripSearchBlocksInBody strips all replayed search structures from an Anthropic request body
// (server_tool_use/web_search_tool_result blocks): post-strip shell messages are deleted wholesale, adjacent same-role
// messages merged (deleting a message can make user user adjacent). n is the number of search blocks stripped (each of the two block kinds counts 1,
// deleted shell messages don't count extra), feeding the [剥N] display and log splits. ok=false when there are no search blocks at all (no retry needed).
// Used for the fail-soft retry after a search-envelope restoration is rejected by the upstream (400 tool_call_id).
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
			stripped = true // A message reduced to only search blocks → delete
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

// mergeSameRoleMessages merges adjacent same-role messages whose content are both arrays (normalization after stripping/deletion).
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

// ---- Response translation: Anthropic message JSON → Responses object (non-streaming path) ----

// mapStopReasonToStatus maps an Anthropic stop_reason to Responses (status, incomplete_reason).
// Mirrors cc-switch map_anthropic_stop_reason_to_status.
func mapStopReasonToStatus(stop string) (string, string) {
	switch stop {
	case "max_tokens", "model_context_window_exceeded":
		return "incomplete", "max_output_tokens"
	case "refusal":
		return "incomplete", "content_filter"
	}
	return "completed", ""
}

// buildResponsesUsage converts Anthropic usage to Responses usage.
// Anthropic's input_tokens excludes cache; Responses reports total input with cache as a subset:
// input_tokens = fresh + cache_read + cache_creation.
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
		// Official nested field + a top-level compatibility alias kept (mirrors cc-switch's same practice).
		result["input_tokens_details"].(map[string]interface{})["cache_write_tokens"] = cacheCreation
		result["cache_creation_input_tokens"] = cacheCreation
	}
	return result
}

// anthropicToResponsesObject converts a complete Anthropic message JSON into a Responses object.
// Mirrors cc-switch anthropic_response_to_responses_with_context.
// The model parameter is the client's original model name (fallback for scenarios where the pipeline's return hasn't already written it back).
// reg is the request-side tool registry: tool_use blocks recover their custom/namespace/tool_search identities from it.
// triple is the search envelope's attribution triple (nil = no search envelopes, e.g. direct Anthropic-port calls).
// stripThinking=true (convertOff2Low upgrade streams) drops thinking/redacted_thinking blocks
// (no reasoning items produced); usage is unaffected, passed through truthfully.
func anthropicToResponsesObject(msg map[string]interface{}, model string, reg *toolRegistry, triple *searchTriple, stripThinking bool) map[string]interface{} {
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
	var lastSearchUse map[string]interface{} // The most recent server_tool_use block (paired into an envelope when the result block arrives)
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
			// Search query echo lines are deleted whole (see stripSearchQueryEcho); the block is dropped if nothing remains.
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
				// When call_id is empty the id degenerates to a bare prefix: back-fill the output index to keep it unique.
				itemID = fmt.Sprintf("%s%s_%d", itemID, responseID, len(output))
			}
			output = append(output, toolCallItemFromRegistry(reg, itemID, "completed", callID, name, args))
		case "thinking", "redacted_thinking":
			if stripThinking {
				continue // convertOff2Low: thinking blocks stripped; the downstream sees a thinking-off response
			}
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
			// Search-related blocks: paired into a web_search_call item (the block is complete at start; see the streaming path's same logic).
			flushText()
			if item := webSearchCallItem(blk, responseID, len(output)); item != nil {
				output = append(output, item)
			}
			// Search-block envelope: when the result block arrives it's bagged with the preceding server_tool_use, riding along as an extra
			// reasoning item — kept by the client, restored on next-turn replay (see searchEnvelopePrefix).
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

// functionCallItem builds a Responses function_call output item.
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

// kimiSearchPreamble is the empty-search preamble text Kimi (k3-256k) gives away at the start of every response when the request carries the web_search tool
// (query empty). Online probes prove its anthropic endpoint emits an "empty-search triple":
// this preamble text block + an id-less/input-less server_tool_use + a content-empty web_search_tool_result,
// even when the model didn't search at all. Worse, the model imitates this pattern from history: the preamble repeats multiple times, glued
// right onto the body's start (observed: "Search results for query: Search results for query: 我确认一下…").
// Handling rules (same set for downstream responses and upstream request replay; see stripSearchQueryEcho): pure preambles and
// "single preamble+query" echo lines are deleted whole (query echo injected into the context induces repeated same-kind searches);
// ≥2 consecutive bare preambles glued before the body (the imitation signature) are stripped bare keeping the body; empty web_search structures are neither produced nor replayed.
const kimiSearchPreamble = "Search results for query: "

// stripKimiSearchPreamble removes repeated preambles at the text's start, returning the remainder.
func stripKimiSearchPreamble(text string) string {
	for strings.HasPrefix(text, kimiSearchPreamble) {
		text = strings.TrimPrefix(text, kimiSearchPreamble)
	}
	return text
}

// countPreambleRun returns the number of consecutive complete preambles at the text's start.
func countPreambleRun(text string) int {
	n := 0
	for strings.HasPrefix(text, kimiSearchPreamble) {
		text = strings.TrimPrefix(text, kimiSearchPreamble)
		n++
	}
	return n
}

// stripRepeatedPreamble is stripKimiSearchPreamble's restrained version: only when ≥2 consecutive repeats
// open the text (the model's imitation signature, field-tested always ×2/×3) is the whole run stripped; exactly one preamble+text is a real search's query
// display spot, kept as-is. Dropping stripped-empty/pure-preamble text is the caller's job.
func stripRepeatedPreamble(text string) string {
	if countPreambleRun(text) < 2 {
		return text
	}
	return stripKimiSearchPreamble(text)
}

// isPreambleRun reports whether the streaming accumulated text may still be "pure preamble" (a few preamble repeats + a preamble prefix).
// If so, keep holding and don't forward; once it diverges (body text follows), the preamble can be stripped and the rest back-sent.
func isPreambleRun(accum string) bool {
	return strings.HasPrefix(kimiSearchPreamble, stripKimiSearchPreamble(accum))
}

// stripSearchQueryEcho deletes search query echo lines from text: whole lines starting with "Search results for query: "
// (prefix+query deleted together, including the bare-preamble form without the trailing space); repeated bare preambles glued
// at a line's start (the model's imitation signature, ≥2) are stripped bare per stripRepeatedPreamble's rules, keeping the same-line body. Line-leading blank lines
// left by deletion are removed too; text without echo lines returns identical through Split/Join, not a byte touched.
// Pure function, idempotent: the same text processed at any moment gives the same result — the precondition for stable request-body prefix caching;
// the upstream replay direction applies it unconditionally to every text segment of every message of every request.
func stripSearchQueryEcho(text string) string {
	lines := strings.Split(text, "\n")
	kept := lines[:0]
	dropped := false
	for _, ln := range lines {
		if strings.HasPrefix(ln, kimiSearchPreamble) {
			if countPreambleRun(ln) >= 2 {
				ln = stripRepeatedPreamble(ln) // Imitation signature: strip bare preambles, keep the same-line body
			} else {
				dropped = true // A single leading preamble = a query echo line: deleted whole
				continue
			}
		} else if strings.TrimRight(ln, " \t\r") == strings.TrimSpace(kimiSearchPreamble) {
			dropped = true // Bare preamble without the trailing space
			continue
		}
		kept = append(kept, ln)
	}
	if dropped {
		for len(kept) > 0 && strings.TrimSpace(kept[0]) == "" {
			kept = kept[1:] // Line-leading blank lines left by deletion
		}
	}
	return strings.Join(kept, "\n")
}

// holdSearchQueryEchoText reports whether the streaming accumulated text still needs holding: still on the preamble-fragment runway
// (isPreambleRun), or stripped-empty per stripSearchQueryEcho (echo lines not fully received / no body text
// in the block yet) — in both cases no event may be sent; back-sending starts only when non-empty body diverges.
func holdSearchQueryEchoText(accum string) bool {
	return isPreambleRun(accum) || strings.TrimSpace(stripSearchQueryEcho(accum)) == ""
}

// webSearchCallItem converts Anthropic server_tool_use / web_search_tool_result blocks into
// Responses web_search_call items. server_tool_use (with query) becomes a completed search call;
// the web_search_tool_result block itself doesn't become a separate item (the result can't be fully expressed in server_tool_use's action;
// v1 merges the result's source URLs into action.sources).
func webSearchCallItem(blk map[string]interface{}, responseID string, idx int) map[string]interface{} {
	switch objStr(blk, "type") {
	case "server_tool_use":
		if objStr(blk, "name") != "web_search" {
			return nil
		}
		q := objStr(asObj(blk["input"]), "query")
		if q == "" {
			// The empty-search triple's server_tool_use has no id and no input (see kimiSearchPreamble):
			// no information content, dropped. A real search always carries a query; its paired result block's sources items are still produced.
			return nil
		}
		return map[string]interface{}{
			"id":     fmt.Sprintf("ws_%s_%d", responseID, idx),
			"type":   "web_search_call",
			"status": "completed",
			"action": map[string]interface{}{"type": "search", "query": q},
		}
	case "web_search_tool_result":
		// Result block: the source URL list extracted, presented as a completed call item's sources.
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

// searchBlocksFromResponsesItem restores search structures from replayed history into Anthropic content blocks.
// web_search_call items are never restored: their ids are proxy-minted (ws_+response id), never registered in the upstream search
// registry; converting to server_tool_use upstream must 400 — production proof: follow-ups 33 seconds after a search block's
// birth (#3) and 54 minutes after (#19) were both rejected on first attempt, and the fail-soft collective punishment stripped the envelope-restored real
// search pair along, forcing the model to re-search. Search content is carried only by search envelopes (whose blocks bear natively registered ids,
// field-tested replaying 200); call items are client-side display pieces only. Ones already in Anthropic shape
// server_tool_use/web_search_tool_result (defensive coverage, ids natively registered) with content
// go upstream as-is; shells (no input/no content) are deleted.
func searchBlocksFromResponsesItem(item map[string]interface{}) []map[string]interface{} {
	switch objStr(item, "type") {
	case "web_search_call":
		return nil
	case "server_tool_use":
		// Non-Responses-protocol items (defensive coverage): already in Anthropic block shape; content-bearing
		// ones go upstream as-is; empty calls (shells like Kimi's empty-search triple with no id and no input) are deleted.
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
