package main

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"time"

	"proxy429/internal/config"
	"proxy429/internal/jsonutil"
	"proxy429/internal/platform"
	"proxy429/internal/translate"
)

func handler(w http.ResponseWriter, r *http.Request) {
	c := config.Current()

	// Access control: the forwarding channel only ever allows localhost — even mistakenly setting listen to 0.0.0.0 won't
	// let LAN devices freeload on the API keys configured in routes. The management endpoints (/__logs, /__config, /__reload) have their own platform.IsLocalRequest guard.
	if !platform.IsLocalRequest(r) {
		http.Error(w, "forbidden (local only)", http.StatusForbidden)
		return
	}

	// Claude Code sends HEAD /, HEAD /api/hello etc. at startup to probe connectivity.
	// Upstreams answer 404/401 for such paths, which would be misread as network unreachable/auth failure and would pollute the in-flight/finished lists.
	// Normal Anthropic API requests are all POST; a HEAD is always a probe — answer 200 directly, without creating a flight or hitting the upstream.
	if r.Method == http.MethodHead {
		w.WriteHeader(http.StatusOK)
		return
	}
	// Browsers automatically request /favicon.ico when the console page refreshes (not an API request); answer 204 directly,
	// so it isn't treated as a forwarding request — creating a flight and an upstream auth error that pollute the in-flight/finished lists.
	if r.URL.Path == "/favicon.ico" {
		w.WriteHeader(http.StatusNoContent)
		return
	}

	// Create a flight for this request: the web console shows in-flight streams (#id/elapsed/bytes).
	// ctx cancels automatically when the client disconnects (r.Context()); retry waits build on it, so a disconnect aborts retries.
	// The upstream request builds on upCtx instead (created below once the flight exists).
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	f := &flight{
		id:    flights.nextID.Add(1),
		start: time.Now(),
	}
	f.stageStart.Store(f.start.UnixNano()) // Light-color timing starts at stream creation (the white light times from request receipt)
	flights.register(f)
	// The upstream request runs on its own context, decoupled from the client context: Codex closes the connection
	// the instant the terminal event (message_stop / response.completed) arrives, and severing the upstream then makes
	// the provider's nginx log a 499 for a response it had in fact fully sent. The watcher propagates a disconnect
	// only while delivery is incomplete (genuine mid-stream cancels still sever the upstream, so both sides record a
	// real 499); once delivered, it only arms a grace timer so the forwarding loop drains the upstream body to EOF.
	upCtx, upCancel := context.WithCancel(context.Background())
	defer upCancel()
	handlerDone := make(chan struct{})
	defer close(handlerDone)
	go func() {
		select {
		case <-ctx.Done():
			if !f.delivered.Load() {
				upCancel()
			} else {
				time.AfterFunc(10*time.Second, upCancel) // Delivered but the upstream hangs: cap the drain so nothing leaks
			}
		case <-handlerDone:
		}
	}()
	// Internal requests from the Responses translation port carry a context marker tagging the flight's origin;
	// the web API column shows [translate]/[Response] (alongside the route-reason tag).
	if v, ok := r.Context().Value(translate.CtxKeyTranslated).(string); ok {
		f.translated = v
	}
	// Internal requests from the Responses port carry the session identifier (prompt_cache_key) via context, anchoring the "缓存年龄" column per session.
	if v, ok := r.Context().Value(translate.CtxKeyConvID).(string); ok {
		f.convID = v
	}
	// The responses-raw passthrough port carries the thinking mode via context (reasoning.effort verbatim — passthrough modifies nothing,
	// so the upstream receives exactly it; the translation port's/direct port's thinking value is extracted from the Anthropic body after rewriting below).
	if v, ok := r.Context().Value(translate.CtxKeyThink).(string); ok {
		f.think = v
	}
	// convertOff2Low upgrade modes (the Responses translation port quietly upgrading off-thinking to low / sending the downstream's
	// requested effort when history is unreplayable; see the n2l constants): the API column shows a two-tone [off->low] badge for implicit upgrades; in the forwarding loop,
	// an upstream 400 rejecting thinking falls back to one retry with thinking off (both modes are covered). The Anthropic native port
	// sets the same mode directly at its own upgrade point below (convFlag=="all"), no ctx involved.
	n2lMode, _ := r.Context().Value(translate.CtxKeyNone2Low).(int)
	// Dual-link recording: the Responses translation port's downstream-side response (the bytes actually written proxy→client) is teed
	// in full into contentDown via translatingWriter's tap; on the native port w is not a translatingWriter,
	// so the type assertion skips naturally (native streams tee their own downstream side inside forward when the two sides can differ:
	// route model write-back and/or thinking strip; identical sides aren't stored twice).
	if t, ok := w.(interface{ SetDownTap(translate.FlightTap) }); ok {
		t.SetDownTap(f)
	}
	// The search-restore context brought by the Responses translation port: proactive watermark strips count into the flight (shown as [剥N]);
	// the replay pointer stays on the flight, and the 400-fallback strip learns the conversation watermark from restore timestamps
	// (see the tool_call_id branch in the forwarding loop).
	if v, ok := r.Context().Value(translate.CtxKeySearchReplay).(*translate.SearchReplayCtx); ok && v != nil {
		f.searchReplay = v
		if v.ProactiveCutoff > 0 {
			f.searchStripped.Store(int32(v.ProactiveCutoff))
			log.Printf("[strip] #%d conversation watermark stripped %d replayed search blocks", f.id, v.ProactiveCutoff)
		}
	}
	// Tag count_tokens probes: a finished stream's raw content is only {"input_tokens":N};
	// without the tag it looks like an "empty response" on the web, hard to tell a probe from an anomaly.
	if r.URL.Path == countTokensPath {
		f.countTokens = true
	}
	// Cleanup fallback: all return paths unregister via one defer, so nothing is left behind in the in-flight list by omission.
	// After unregistering, its pass-through content (if any) is archived for the web to review among finished streams.
	defer func() {
		markClientGone(f, ctx)
		flights.unregister(f.id)
		addFinished(f)
	}()

	// 1. Buffer the request body for replay on retries.
	body, err := io.ReadAll(r.Body)
	if err != nil {
		log.Printf("[request] #%d %s %s failed to read body: %v", f.id, r.Method, r.URL.Path, err)
		http.Error(w, "read body failed", http.StatusBadGateway)
		return
	}
	r.Body.Close()
	// Keep a copy of the request body for the web (which request caused this stream): this is the initial value of the proxy→upstream side;
	// each attempt of the retry loop refreshes it with the body actually sent (after a fallback rewrite, the last one wins); streams that die young
	// before dispatch (e.g. bad JSON) still have this entry body to inspect.
	f.setReqBody(body)
	// The downstream-side (downstream→proxy) request body of dual-link recording: the translation port brings the Responses original via context
	// (necessarily different from the translated body, so always stored); native streams defer to a comparison with the first upstream-bound body and are stored only on difference.
	var downBody []byte
	if v, ok := r.Context().Value(translate.CtxKeyReqDown).([]byte); ok && len(v) > 0 {
		f.setReqDown(v)
	} else {
		downBody = body
	}
	// Anthropic port: extract the session identifier from the request body's metadata.user_id (the Responses port already carries it via context; no double parsing).
	if f.convID == "" {
		f.convID = extractConvID(body)
	}
	// Conversation lineage: hash this request's message array and link the flight to its parent
	// (same session, longest fully-contained prefix), powering the web 「#N<-M」 tag.
	computeLineage(f, body)

	if c.LogRequestDetail {
		logRequestDetail(r, body)
	}
	// Extract model: shared by the [请求] log display and route matching. Empty for non-JSON / no model.
	origModel := extractModel(body)
	modelPart := ""
	if origModel != "" {
		modelPart = " model=" + origModel
	}
	if f.translated == translate.TranslatedResponsesRaw {
		modelPart += " [direct:responses]"
	} else if f.translated != "" {
		modelPart += " [translate:" + f.translated + "]"
	}
	log.Printf("[request] #%d %s %s%s (body=%d bytes) from %s", f.id, r.Method, r.URL.Path, modelPart, len(body), r.RemoteAddr)

	// 1.5 Apply the thinking policy on classifier hits, so classification returns fast.
	// The classifier source also feeds the routing decision: classifier routing takes priority over model routing.
	clsKind := detectClassifier(body)
	if clsKind != config.ClassifierNone {
		stats.classifierHits.Add(1) // Count on match: tallied whether or not rerouted/rewritten (observing safety-check request volume)
		if clsKind == config.ClassifierCodex {
			stats.classifierHitsCodex.Add(1)
		} else {
			stats.classifierHitsCC.Add(1)
		}
	}
	body, clsRewrote := maybeRewriteClassifier(body)

	// The status page's 「API」 column thinking value = the thinking config actually sent upstream: extracted after the classifier rewrite, so the translation port's
	// mapped thinking is what shows (e.g. Codex effort high → on 16384), not what the client originally sent.
	// A responses-raw passthrough body is Responses format (no thinking field); its reasoning.effort
	// was already carried in via ctx above and is not overwritten here.
	if f.think == "" {
		f.think = extractThinkMode(body)
	}
	// Implicit-upgrade streams (downstream explicitly off → low) get their thinking value overwritten to [off->low] (extractThinkMode
	// sees the upgraded low, which can't express the quiet "downstream off, upstream on" upgrade). History-fallback thinking-on
	// (sent at the downstream's requested effort) is not overwritten: showing the requested effort is truthful (the downstream wanted thinking).
	if n2lMode == translate.N2LStealth {
		f.think = "off->low"
	}

	// 1.6 Route matching.
	// Priority: classifier route > fast route > model route (by routes wildcard).
	// On no match: upstream=c.Upstream, authToken="" (pass through the client's token, the original logic).
	f.setStage(stageRoute)
	upstream := c.Upstream
	authToken := ""
	var targetModel string
	fastRouteHit := false
	convFlag := "" // The effective route's convertOff2Low value, captured where routing settles ("" = feature off)
	// Standalone search sub-agent handling (single-request search+summary): the effective levels are resolved
	// where routing settles; the request body is transformed after the routing block.
	searchInjectLevel := ""     // ""/low/mid/high/max: inject the summary instruction into the search request itself
	searchThinkLevel := ""      // ""/off/low/mid/high/max: rewrite the search request's thinking
	searchGuardEnabled := false // Effective route's noBullshitOnEmptyClaudeSearch; armed below only after the request marker is confirmed
	if clsKind != config.ClassifierNone && c.ClassifierRoute != nil && c.ClassifierRoute.URL != "" {
		// Classifier route: shunt safety-check requests to the designated upstream, saving main-model quota.
		// (url empty = no reroute: a bare {"classifier_thinking":"off"} configures the thinking rewrite only.)
		cr := c.ClassifierRoute
		upstream = cr.URL
		authToken = cr.API
		if cr.Model != "" && cr.Model != origModel {
			body = replaceModelValue(body, cr.Model)
			targetModel = cr.Model
		}
		log.Printf("[route] #%d classifier%s %s -> %s (model %s -> %s)", f.id, classifierTag(clsKind), origModel, cr.URL, origModel, cr.Model)
		f.routeReason.Store(routeClassifier)
		f.convAnchor = "classifier"
	} else if c.FastRoute != nil && isFastRequest(body, r) {
		// Fast route: non-classifier requests detected with "speed":"fast" are shunted to the designated upstream.
		fr := c.FastRoute
		upstream = fr.URL
		authToken = fr.API
		fastRouteHit = true
		convFlag = config.EffectiveConvertOff2Low(c, fr.ConvertOff2Low, fr.Presence.Written("convertOff2Low", fr.ConvertOff2Low != ""), fr.NoLazyApply)
		if fr.Model != "" && fr.Model != origModel {
			body = replaceModelValue(body, fr.Model)
			targetModel = fr.Model
		}
		body = removeSpeedField(body)
		log.Printf("[route] #%d fast %s -> %s (model %s -> %s)", f.id, origModel, fr.URL, origModel, fr.Model)
		f.routeReason.Store(routeFast)
		f.convAnchor = "fast"
	} else if len(c.Routes) > 0 && origModel != "" {
		for i := range c.Routes {
			// "Fallback"/"fast_route" are reserved names (the * catch-all route, the fast channel's name in the Codex menu):
			// patterns may not match them exactly; colliding routes don't take effect (config.LoadConfig warns), so they can't intercept Codex traffic.
			if config.IsReservedRoutePattern(c.Routes[i].Pattern) {
				continue
			}
			if config.MatchModel(c.Routes[i].Pattern, origModel) {
				rr := &c.Routes[i]
				// The cache anchor key is recorded once on the routing hit (the passthrough branch passes through here too, naturally covered):
				// only same session + same route form one cache lineage, so utility hops (title generation etc.) can't steal the main conversation's anchor.
				f.convAnchor = "route:" + rr.Pattern
				// Responses native passthrough: this request came from the Responses port and the pre-check found the matched route has url_response_api.
				// The upstream switches to that field (the Anthropic-only image/search fallbacks and enhance-search are meaningless for it — skipped).
				if f.responsesRaw() {
					if rr.URLResponseAPI == "" {
						// Present at pre-check, gone now — only a hot config reload could have removed the field; say 502 explicitly instead of silently misrouting.
						log.Printf("[error] #%d Responses passthrough request hit route %q but its url_response_api is gone (config hot-reloaded?); cannot forward", f.id, rr.Pattern)
						http.Error(w, "route lost url_response_api mid-request", http.StatusBadGateway)
						return
					}
					upstream = rr.URLResponseAPI
					authToken = rr.API
					if rr.Model != "" && rr.Model != origModel {
						body = replaceModelValue(body, rr.Model)
						targetModel = rr.Model
					}
					log.Printf("[route] #%d %s -> %s (Responses native passthrough, model %s -> %s)", f.id, origModel, rr.URLResponseAPI, origModel, rr.Model)
					f.routeReason.Store(routePattern)
					break
				}
				// Capability fallback: when the target upstream lacks image/search capability the request needs, switch to the corresponding fallback upstream.
				needImage := config.EffectiveRouteTextOnly(c, rr) && hasImage(body)
				needSearch := config.EffectiveRouteNoSearch(c, rr) && hasWebSearch(body)
				if needImage || needSearch {
					// applyFB assigns the selected fallback upstream to the current request (changing upstream/API/model).
					applyFB := func(url, api, model, label string, reason int32) {
						upstream = url
						authToken = api
						if model != "" && model != origModel {
							body = replaceModelValue(body, model)
							targetModel = model
						}
						f.routeReason.Store(reason)
						log.Printf("[route] #%d %s %s -> %s (model %s -> %s)", f.id, label, origModel, url, origModel, model)
					}
					sf := c.SearchFallback
					mf := c.MultimodalFallback
					// Search: hand the whole request to search_fallback, images or not (no evidence multimodal+search ever co-occur).
					//    The standalone search sub-agent behavior (summary injection / thinking rewrite, single-request mode)
					//    is resolved here and applied to the request body after the routing block.
					if needSearch && sf != nil {
						applyFB(sf.URL, sf.API, sf.Model, "search-fallback", routeSearch)
						// Per field: route claudeSearch* > search_fallback > lazyApply > built-in.
						effFB := config.EffectiveFallbackSearchSettings(c, rr, sf)
						convFlag = effFB.ConvertOff2Low // The fallback's own effective flag overwrites the matched route's
						searchInjectLevel, searchThinkLevel = config.ResolveSearchBehavior(effFB)
						searchGuardEnabled = config.EffectiveSearchGuard(c, rr, sf)
						break
					}
					// Pure images (no search): use multimodal_fallback when configured; search requests never land on mf (degrade to passthrough when there's no sf).
					if needImage && mf != nil && !needSearch {
						applyFB(mf.URL, mf.API, mf.Model, "image-fallback", routeMultimodal)
						convFlag = config.EffectiveConvertOff2Low(c, mf.ConvertOff2Low, mf.Presence.Written("convertOff2Low", mf.ConvertOff2Low != ""), mf.NoLazyApply)
						break
					}
					// Degrade: search without sf, or pure images without mf — pass through to the original route (the upstream handles it, possibly with an error).
				}
				upstream = rr.URL
				authToken = rr.API
				if rr.Model != "" && rr.Model != origModel {
					body = replaceModelValue(body, rr.Model)
					targetModel = rr.Model
				}
				log.Printf("[route] #%d %s -> %s (model %s -> %s)", f.id, origModel, rr.URL, origModel, rr.Model)
				f.routeReason.Store(routePattern)
				convFlag = config.EffectiveConvertOff2Low(c, rr.ConvertOff2Low, rr.Presence.Written("convertOff2Low", rr.ConvertOff2Low != ""), rr.NoLazyApply)
				searchGuardEnabled = config.EffectiveSearchGuard(c, rr, nil)
				// Enhance search / route-level claudeSearch*: when the route effectively supports search (no_search:false),
				// a standalone search sub-agent request gets the summary instruction injected into the request itself
				// and/or its thinking rewritten (single-request mode), served by the route's own upstream — the request
				// forwards to the route as usual; only the body transform after the routing block applies. Local
				// claudeSearch* fields win over the legacy enhance_search summary_* fields; lazyApply fills omitted fields.
				if hasWebSearch(body) && !config.EffectiveRouteNoSearch(c, rr) && !(config.EffectiveRouteTextOnly(c, rr) && hasImage(body)) {
					synth := config.EffectiveDirectSearchSettings(c, rr)
					searchInjectLevel, searchThinkLevel = config.ResolveSearchBehavior(synth)
					if searchInjectLevel != "" || searchThinkLevel != "" {
						log.Printf("[route] #%d enhanced search %s -> %s (model %s)", f.id, origModel, rr.URL, rr.Model)
					}
				}
				break // Ordered: stop at the first hit
			}
		}
	}

	f.origModel = origModel
	f.targetModel = targetModel

	// Standalone search sub-agent request (single-request search+summary): inject the summary instruction into the
	// search request itself and/or rewrite its thinking, then the normal forward path takes over — the upstream
	// searches and answers in one request and its response streams through untouched (no proxy-side synthesis; only
	// when a thinking rewrite turned thinking on for a client that never asked for it are the returning thinking
	// blocks stripped, convertOff2Low-style, so that client keeps seeing a thinking-off response).
	// Badge every marker hit ([Search] in the model column), even when no injection/rewrite applies.
	if hasSearchSubagentMarker(body) {
		f.searchMarker = true
		f.searchGuardArmed = searchGuardEnabled
		if searchThinkLevel != "" {
			thinkFrom := f.think
			if nb, ok := rewriteSearchThinking(body, searchThinkLevel); ok {
				body = nb
				f.searchThinkFrom = thinkFrom // Empty from renders as auto in the API column's [from->to] badge
				f.searchThinkTo = searchThinkLevel
				if searchThinkLevel != "off" && (thinkFrom == "" || thinkFrom == "off") {
					// The client never asked for thinking (absent or explicitly off) but the rewrite turned it on:
					// strip the returning thinking blocks (the convertOff2Low machinery) so the client stays unaware.
					f.stripThinking.Store(true)
				}
			} else {
				log.Printf("[searchsum] #%d thinking rewrite to %q abandoned (body unparsable or unknown level); request kept as sent", f.id, searchThinkLevel)
				searchThinkLevel = ""

			}
		}
		var instructions []string
		if searchInjectLevel != "" {
			instructions = append(instructions, summaryLevelConfig(searchInjectLevel, searchMarkerQuery(body)))
		}
		if f.searchGuardArmed {
			instructions = append(instructions, searchGuardInstruction)
		}
		if len(instructions) > 0 {
			instr := strings.Join(instructions, "\n\n")
			if nb, ok := injectAfterMarker(body, instr); ok {
				body = nb
				f.searchPrompt = instr // The status page shows the injected instruction on hover over the model cell
				f.searchLevel = searchInjectLevel
			}
		}
		if searchInjectLevel != "" || searchThinkLevel != "" {
			f.think = extractThinkMode(body) // The status page's 「API」 column reflects the rewritten thinking
			log.Printf("[searchsum] #%d standalone search sub-agent request: summary-level=%q thinking=%q (single-request)", f.id, searchInjectLevel, searchThinkLevel)
		}
	}

	// upstream may be left empty (with a pattern:"*" catch-all, the default upstream goes unused); still empty here
	// means no route matched and no default upstream is configured — retrying is pointless, so answer 502 with the reason spelled out.
	if upstream == "" {
		log.Printf("[error] #%d no route matched and default upstream is empty; cannot forward", f.id)
		http.Error(w, "no route matched and upstream is empty", http.StatusBadGateway)
		return
	}

	// The upstream grouping key is recorded once with the route's final values (the grouping dimension for observed cache lifetime: url+model, other parameters ignored).
	{
		m := targetModel
		if m == "" {
			m = origModel
		}
		f.upstreamKey = upstream + "|" + m
		// The search-envelope ownership triple is injected with the route's final values (only the Responses translation port's
		// translatingWriter has this method; the Anthropic port's type assertion fails and skips naturally). The key uses the token actually in effect:
		// when the route key is empty, the client's Authorization is what gets passed through — same semantics as the replay side's effectiveKey.
		if t, ok := w.(interface{ SetSearchTriple(string, string, string) }); ok {
			effKey := authToken
			if effKey == "" {
				effKey = r.Header.Get("Authorization")
			}
			t.SetSearchTriple(upstream, effKey, m)
		}
	}

	// convertOff2Low="all" native-port upgrade: an explicit downstream thinking-off is quietly sent upstream as low thinking,
	// with thinking blocks stripped from the response (the client stays unaware). Other Responses-port requests already carry
	// their own upgrade decision via ctx. The classifier "low" policy composes the same machinery (clsLow): maybeRewriteClassifier
	// already normalized thinking to off above (or passed the body through untouched when it already carried the target effect —
	// for an already-off request the upgrade below is the one real rewrite, so it is counted here rather than in the off phase).
	// Only the classifier path passes floorBump: a max_tokens too small for the 1024 budget floor is raised by 1024
	// upstream-side instead of abandoning the upgrade (guardian requests arrive with max_tokens as small as 64).
	clsLow := clsKind != config.ClassifierNone && config.ClassifierThinkingModeFor(c, clsKind) == "low"
	if clsLow || (convFlag == "all" && clsKind == config.ClassifierNone && f.translated == "" && r.URL.Path == "/v1/messages") {
		effModel := targetModel
		if effModel == "" {
			effModel = origModel
		}
		if nb, upgraded, abandoned, bumped := translate.MaybeUpgradeOffToLow(body, effModel, clsLow); upgraded {
			if clsLow && !clsRewrote {
				// The off phase passed this already-off request through untouched: the upgrade here is the one real rewrite, so it counts here.
				stats.classifierRewrites.Add(1)
				if clsKind == config.ClassifierCodex {
					stats.classifierRewritesCodex.Add(1)
				} else {
					stats.classifierRewritesCC.Add(1)
				}
			}
			if clsLow && bumped {
				log.Printf("[off->low] #%d classifier%s thinking-off quietly upgraded to low thinking (max_tokens raised by 1024 to fit the 1024-token budget floor; body %d->%d bytes); thinking blocks stripped on return", f.id, classifierTag(clsKind), len(body), len(nb))
			} else if clsLow {
				log.Printf("[off->low] #%d classifier%s thinking-off quietly upgraded to low thinking (body %d->%d bytes); thinking blocks stripped on return", f.id, classifierTag(clsKind), len(body), len(nb))
			} else {
				log.Printf("[off->low] #%d explicit thinking-off quietly upgraded to low thinking (body %d->%d bytes); thinking blocks stripped on return", f.id, len(body), len(nb))
			}
			body = nb
			n2lMode = translate.N2LStealth // Reuses the one-shot 400 fallback below (retreat to thinking-off if the upstream rejects thinking)
			f.think = "off->low"
			f.stripThinking.Store(true)
		} else if abandoned {
			// Reachable only with floorBump off (convertOff2Low): the classifier path raises max_tokens instead.
			log.Printf("[off->low] #%d upgrade abandoned: max_tokens too small for the 1024-token budget floor; request stays thinking-off", f.id)
		}
	}

	// 1.7 Global stream-conversion (convertAlltoStream): when on, all non-streaming requests are sent upstream as streams;
	// the complete SSE stream is collected and rebuilt into a non-streaming JSON returned to the client in one shot (transparent to the client; the web monitors streaming/first-byte/tok/s).
	// Only the Anthropic Messages API (/v1/messages) is converted: OpenAI-compatible paths have a different SSE format and can't be rebuilt.
	convertToStream := false
	if c.ConvertAllToStream && r.URL.Path == "/v1/messages" {
		if stream, ok := getBoolField(body, "stream"); !ok || !stream {
			body = forceStreamTrue(body)
			convertToStream = true
			log.Printf("[streamify] #%d non-streaming request sent upstream as streaming (stream:%v -> true)", f.id, stream)
		}
	}

	deadline := time.Now().Add(time.Duration(c.TotalBudgetSec * float64(time.Second)))
	// headersSent: whether the 200 + SSE headers have been sent to the client (set true on the first retry's keepalive).
	// Once true, later successful forwards skip WriteHeader/copyHeaders (headers already sent), and retries-exhausted sends an SSE error instead.
	headersSent := false
	var flusher http.Flusher // Assigned by startSSEKeepalive once keepalive starts
	pingInterval := time.Duration(c.PingIntervalSec * float64(time.Second))

	searchStripped := false // Whether a stripped-block retry already happened after the upstream rejected a replayed search id (fail-soft gets only one chance)
	for attempt := 0; attempt <= c.MaxRetries; attempt++ {
		// Entering the [尝试N] stage: the web 「状态」 column shows the attempt number (awaiting the upstream's first byte).
		f.attempt.Store(int32(attempt + 1))
		f.setStage(stageAttempt)
		// 2. Build the request bound upstream.
		// A passthrough stream's upstream is the base given in url_response_api (…/coding, …/v1, or a full …/v1/responses all work);
		// the client path must not be copied verbatim (it's /v1/responses — appending it to the base would double or misjoin) — only the difference is appended per the base's shape.
		upPath := r.URL.Path
		if f.responsesRaw() {
			upPath = responsesAPIPath(upstream)
		}
		// Dual-link recording: the proxy→upstream side takes the body actually sent on this attempt (overwriting the entry initial value;
		// a fallback rewrite naturally refreshes it); a native stream's downstream side is stored only when rewriting makes it differ from the upstream-bound body — identical text is never stored twice.
		f.setReqBody(body)
		if downBody != nil && !bytes.Equal(downBody, body) {
			f.setReqDown(downBody)
			downBody = nil // The client's original doesn't change; storing it once suffices
		}
		upReq, err := http.NewRequestWithContext(upCtx, r.Method, upstream+upPath, bytes.NewReader(body))
		if err != nil {
			log.Printf("[error] failed to build upstream request: %v", err)
			http.Error(w, "build request failed", http.StatusBadGateway)
			return
		}
		upReq.URL.RawQuery = r.URL.RawQuery
		copyHeaders(upReq.Header, r.Header)
		upReq.Header.Set("Accept-Encoding", "identity") // Disable compression so the response body is easy to inspect
		upReq.Host = ""                                 // Let Go set Host automatically from Upstream
		// On a route hit, overwrite the auth header with the target API key (deleting the original Authorization/x-api-key, so the client's token isn't passed through to the target upstream).
		if authToken != "" {
			upReq.Header.Del("Authorization")
			upReq.Header.Del("x-api-key")
			upReq.Header.Set("Authorization", "Bearer "+authToken)
		}
		// On a fast-route hit, delete the Anthropic-Beta header (fast-mode-2026-02-01 included); the upstream doesn't support it.
		if fastRouteHit {
			upReq.Header.Del("Anthropic-Beta")
		}

		// Sent upstream, entering the "awaiting first byte" stage: the status light turns yellow (waiting++).
		stats.mu.Lock()
		stats.waiting++
		stats.mu.Unlock()
		tSend := time.Now()                    // Request send time, for first-byte latency
		f.attemptStart.Store(tSend.UnixNano()) // [尝试N:Xs] times from this send moment (re-stamped here after backoff zeroed it)
		resp, err := client.Do(upReq)
		// Leave the waiting stage as soon as a response (or timeout/error) arrives.
		stats.mu.Lock()
		stats.waiting--
		stats.mu.Unlock()

		// Case 0: network-layer error.
		if err != nil {
			log.Printf("[attempt %d] #%d request error: %v", attempt+1, f.id, err)
			// A tray-icon double-click abort (ctx cancel) doesn't retry; end directly.
			if ctx.Err() != nil {
				if headersSent {
					return // Keepalive headers already sent, client disconnected; end directly
				}
				http.Error(w, "client canceled", http.StatusServiceUnavailable)
				return
			}
			if attempt < c.MaxRetries && time.Now().Before(deadline) {
				wait := computeBackoff(nil, attempt)
				if !time.Now().Add(wait).After(deadline) {
					log.Printf("[retry] waiting %v before retry (budget left %v)", wait, time.Until(deadline).Round(time.Millisecond))
					stats.statusRetries.Add(1) // Live status row count: retries (network error/timeout)
					stats.addModelRetry(f.realModel())
					f.attempt.Store(int32(attempt + 2)) // Advance the attempt number to the next try during backoff
					f.attemptStart.Store(0)             // In backoff: no attempt in flight; the yellow light shows [退避中]
					if !headersSent && !convertToStream {
						// A convertToStream client expects a non-streaming response; SSE keepalives would pollute it, so wait silently.
						flusher = startSSEKeepalive(w, f, "network error")
						headersSent = true
					}
					if !sleepWithPing(ctx, w, flusher, wait, pingInterval, f) {
						return // Client disconnected; stop retrying
					}
					continue
				}
			}
			// Retries exhausted on timeouts/network errors: pass through a 503 so Claude Code retries on its own (the SDK has built-in 5xx retries).
			log.Printf("[passthrough] timed out or retries exhausted; passing 503 through to the client: %v", err)
			if headersSent {
				writeSSEError(w, flusher, "upstream timeout/error: "+err.Error(), f)
				return
			}
			http.Error(w, "upstream timeout/error: "+err.Error(), http.StatusServiceUnavailable)
			return
		}

		// Print the status code unconditionally, so diagnosis can see exactly what came back.
		log.Printf("[attempt %d] upstream response status: %d", attempt+1, resp.StatusCode)

		// Case A: the HTTP status code itself demands a retry (a real 429/5xx).
		if shouldRetry(resp.StatusCode) {
			if attempt < c.MaxRetries && time.Now().Before(deadline) {
				wait := computeBackoff(resp, attempt)
				if !time.Now().Add(wait).After(deadline) {
					log.Printf("[retry] status %d; waiting %v before retry (budget left %v)", resp.StatusCode, wait, time.Until(deadline).Round(time.Millisecond))
					stats.statusRetries.Add(1) // Live status row count: retries (status code)
					stats.addModelRetry(f.realModel())
					f.attempt.Store(int32(attempt + 2)) // Advance the attempt number to the next try during backoff
					f.attemptStart.Store(0)             // In backoff: no attempt in flight; the yellow light shows [退避中]
					resp.Body.Close()
					if !headersSent && !convertToStream {
						// A convertToStream client expects a non-streaming response; SSE keepalives would pollute it, so wait silently.
						flusher = startSSEKeepalive(w, f, fmt.Sprintf("status %d", resp.StatusCode))
						headersSent = true
					}
					if !sleepWithPing(ctx, w, flusher, wait, pingInterval, f) {
						return // Client disconnected; stop retrying
					}
					continue
				}
			}
			log.Printf("[passthrough] attempts or budget exhausted; passing through status %d", resp.StatusCode)
			if headersSent {
				resp.Body.Close()
				writeSSEError(w, flusher, fmt.Sprintf("upstream status %d", resp.StatusCode), f)
				return
			}
			forward(w, resp, nil, bufio.NewReader(resp.Body), f, false)
			return
		}

		// Case B: status 200, but an error may hide in the response body (SSE stream).
		br := bufio.NewReader(resp.Body)
		head := peekHead(br, 8192)
		tFirstByte := time.Now() // The moment the first body data byte arrives ("first token out")

		if headHasError(head) {
			// The search-envelope restore was rejected by the upstream (Kimi's tool_call_id registry no longer recognizes this old id):
			// strip the replayed search blocks from the request and retry once immediately — a failed restore automatically degrades to "no restore".
			// No backoff budget is burned (attempt-- cancels the loop's increment, so it doesn't count as a retry and works even with MaxRetries=0),
			// and this 400 must not be passed to the client.
			if !searchStripped && bytes.Contains(head, []byte("tool_call_id")) {
				if nb, n, ok := translate.StripSearchBlocksInBody(body); ok {
					f.searchStripped.Add(int32(n))
					// Learn the conversation watermark: take the oldest seal-in timestamp among the envelopes restored this round — the registry
					// evicts by age, so if the oldest was rejected, everything older is dead too; strip by default from now on, no more 400s.
					water := ""
					if f.searchReplay != nil && len(f.searchReplay.Restored) > 0 {
						oldest := f.searchReplay.Restored[0]
						for _, ts := range f.searchReplay.Restored[1:] {
							if ts.Before(oldest) {
								oldest = ts
							}
						}
						translate.LearnSearchCutoff(f.searchReplay.ConvID, oldest)
						water = fmt.Sprintf(", conversation watermark learned as %s", oldest.Format("01-02 15:04:05"))
					}
					log.Printf("[fallback] #%d upstream rejected the replayed search id (HTTP %d); stripped %d search blocks and retried%s", f.id, resp.StatusCode, n, water)
					stats.statusRetries.Add(1)
					stats.addModelRetry(f.realModel())
					body = nb
					searchStripped = true
					attempt--
					resp.Body.Close()
					continue
				}
			}
			// Thinking over a no-thinking-block history was rejected by the upstream (the n2l modes send thinking on purpose here — the
			// stealth upgrade's low and the try-on mode's requested level; the rejection is effort-agnostic, rejecting "unsigned history
			// with thinking on"): switch thinking back off and retry once immediately — the downstream wanted thinking off anyway (or at
			// most accepts it off), so it's semantically lossless; no backoff budget burned (attempt--, same as the search-strip fallback).
			// After the fallback the upstream receives thinking-off, so the response carries no thinking blocks and response-side stripping has nothing to do;
			// the badge falls back to truthful display as well.
			if n2lMode != translate.N2LNone && bytes.Contains(head, []byte("thinking")) {
				if nb, ok := disableThinkingInBody(body); ok {
					if n2lMode == translate.N2LTryOn {
						log.Printf("[fallback] #%d upstream rejected thinking over a no-thinking-block history (HTTP %d); reverted to thinking-off and retried", f.id, resp.StatusCode)
					} else {
						log.Printf("[fallback] #%d upstream rejected the low-thinking upgrade (HTTP %d); reverted to thinking-off and retried", f.id, resp.StatusCode)
					}
					stats.statusRetries.Add(1)
					stats.addModelRetry(f.realModel())
					body = nb
					// A native-port upgrade may also have set reasoning_effort:"low"; the belt-and-braces off form is "none"
					// (jsonutil.SetTopLevelJSONValue appends the key when absent — harmless, same shape as the classifier rewrite).
					if nb2, ok2 := jsonutil.SetTopLevelJSONValue(body, "reasoning_effort", []byte(`"none"`)); ok2 {
						body = nb2
					}
					n2lMode = translate.N2LNone
					f.think = extractThinkMode(body)
					f.stripThinking.Store(false) // The retried response is genuinely thinking-off; there is nothing left to strip
					attempt--
					resp.Body.Close()
					continue
				}
			}
			log.Printf("[error] status 200 but response body contains an error: %s", firstLine(head))
			if attempt < c.MaxRetries && time.Now().Before(deadline) {
				wait := computeBackoff(resp, attempt)
				if !time.Now().Add(wait).After(deadline) {
					log.Printf("[retry] waiting %v before retry (budget left %v)", wait, time.Until(deadline).Round(time.Millisecond))
					stats.statusRetries.Add(1) // Live status row count: retries (in-body error on 200)
					stats.addModelRetry(f.realModel())
					f.attempt.Store(int32(attempt + 2)) // Advance the attempt number to the next try during backoff
					f.attemptStart.Store(0)             // In backoff: no attempt in flight; the yellow light shows [退避中]
					resp.Body.Close()
					if !headersSent && !convertToStream {
						// A convertToStream client expects a non-streaming response; SSE keepalives would pollute it, so wait silently.
						flusher = startSSEKeepalive(w, f, "in-body error")
						headersSent = true
					}
					if !sleepWithPing(ctx, w, flusher, wait, pingInterval, f) {
						return // Client disconnected; stop retrying
					}
					continue
				}
			}
			log.Printf("[passthrough] attempts or budget exhausted; passing through the in-body error")
			if headersSent {
				resp.Body.Close()
				writeSSEError(w, flusher, "upstream stream error after retries", f)
				return
			}
			forward(w, resp, head, br, f, false)
			return
		}

		// Case C: normal response, passed through. First-byte latency / streaming duration / output_tokens enter the sliding window.
		log.Printf("[response] passed through normally (%d attempts)", attempt+1)
		// With fast_route configured, inject fake fast rate-limit headers into every normal response,
		// so Claude Code's /fast pre-check believes fast mode is available.
		if c.FastRoute != nil {
			now := time.Now().Format(time.RFC3339)
			resp.Header.Set("anthropic-fast-output-tokens-remaining", "999999")
			resp.Header.Set("anthropic-fast-input-tokens-remaining", "999999")
			resp.Header.Set("anthropic-fast-output-tokens-reset", now)
			resp.Header.Set("anthropic-fast-input-tokens-reset", now)
		}
		// A convertToStream request: the upstream streams SSE back; collect the full stream, rebuild non-streaming JSON, and return it in one shot
		// (the web in-flight view stays live; stats match ordinary streaming requests). If the upstream didn't stream (JSON), fall through to passthrough below.
		if convertToStream && strings.Contains(resp.Header.Get("Content-Type"), "text/event-stream") {
			ok, out := collectStreamToJSON(w, resp, head, br, f)
			if ok {
				// Stats match ordinary streaming passthrough: first-byte latency / streaming duration / tok/s.
				tEnd := time.Now()
				stats.pushFirstByte(int64(tFirstByte.Sub(tSend).Milliseconds()))
				stats.pushThroughput(int64(tEnd.Sub(tFirstByte).Milliseconds()), out)
				firstByteMs := tFirstByte.Sub(tSend).Milliseconds()
				streamMs := tEnd.Sub(tFirstByte).Milliseconds()
				var tps float64
				if streamMs > 0 {
					tps = float64(out) / (float64(streamMs) / 1000.0)
				}
				f.firstByteMs = firstByteMs
				f.tps = tps
				log.Printf("[flight] #%d TTFT %.2fs stream %.2fs %d tok %.1f tok/s",
					f.id, float64(firstByteMs)/1000, float64(streamMs)/1000, out, tps)
				return
			}
			// Stream broke midway: the client has received nothing yet; after backoff, retry the whole thing (resend the streaming request, drain again).
			log.Printf("[streamify] #%d upstream stream incomplete; retrying", f.id)
			stats.statusRetries.Add(1)
			stats.addModelRetry(f.realModel())
			if attempt < c.MaxRetries && time.Now().Before(deadline) {
				wait := computeBackoff(resp, attempt)
				if !time.Now().Add(wait).After(deadline) {
					f.attempt.Store(int32(attempt + 2))
					f.attemptStart.Store(0) // In backoff: no attempt in flight; the yellow light shows [退避中]
					if !sleepWithPing(ctx, w, flusher, wait, pingInterval, f) {
						return // Client disconnected; stop retrying
					}
					continue
				}
			}
			// Retries exhausted: the client expects non-streaming JSON; pass through a 502 and let it handle it.
			log.Printf("[passthrough] attempts or budget exhausted; streamified flow incomplete, passing through 502")
			http.Error(w, "upstream stream interrupted after retries", http.StatusBadGateway)
			return
		}
		stats.pushFirstByte(int64(tFirstByte.Sub(tSend).Milliseconds()))
		out := forward(w, resp, head, br, f, headersSent)
		tEnd := time.Now()
		stats.pushThroughput(int64(tEnd.Sub(tFirstByte).Milliseconds()), out)
		// Per-stream stats: this stream's first-byte latency + streaming tok/s (distinct from the status row's sliding-window averages).
		firstByteMs := tFirstByte.Sub(tSend).Milliseconds()
		streamMs := tEnd.Sub(tFirstByte).Milliseconds()
		var tps float64
		if streamMs > 0 {
			tps = float64(out) / (float64(streamMs) / 1000.0)
		}
		f.firstByteMs = firstByteMs
		f.tps = tps
		log.Printf("[flight] #%d TTFT %.2fs stream %.2fs %d tok %.1f tok/s",
			f.id, float64(firstByteMs)/1000, float64(streamMs)/1000, out, tps)
		return
	}
}
