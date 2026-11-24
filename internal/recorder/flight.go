package recorder

import (
	"bytes"
	"context"
	"log"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"proxy429/internal/translate"
)

// The downstream side of dual-link recording (proxy↔client): recorded only for streams that differ from the upstream side
// (translation streams always record, rebuilt-JSON streams record contentDown, native streams record reqDown only when rewritten);
// the rules mirror the upstream-side methods one to one.

// flight tracks one in-flight request for the web console's live streams view (#id + duration/bytes).
type Flight struct {
	ID            uint64       // Incrementing number
	Start         time.Time    // Time the downstream request arrived (flight created)
	Phase         atomic.Int32 // 0=awaiting response, 1=response received (forwarding started)
	Status        int          // HTTP status code (valid when phase=1)
	GaveUp        bool         // Retries/budget exhausted and a fallback error event has been passed downstream (writeSSEError sets this; status stays 0, not forged); the finished-streams status column shows [重试尽]
	Bytes         atomic.Int64 // Forwarded byte count
	OrigModel     string       // Client's original model; used to rewrite the response stream back after routing
	TargetModel   string       // Target model after routing; forward rewrites back when non-empty and ≠ origModel
	UpstreamModel string       // Model actually returned in the upstream response (captured by rewriteResponseModel); empty = unknown
	ModelLogged   atomic.Bool  // Whether the [改写] log line was printed; printed once
	Stage         atomic.Int32 // Current stage (stage*), shown in the web in-flight "status" column
	StageStart    atomic.Int64 // When the current light color started (unixnano; the color is stage mapped via stageColor); the web shows this color's duration next to the status light
	Attempt       atomic.Int32 // Current attempt number (from 1); the web shows [尝试N] when stage=stageAttempt
	AttemptStart  atomic.Int64 // When the current attempt's upstream request was sent (unixnano): the timing origin of [尝试N:Xs] next to the yellow light; 0 = in retry backoff (next attempt not sent yet)
	RouteReason   atomic.Int32 // Route reason for this request (route*), shown appended in the web "status" column
	Delivered     atomic.Bool  // Response fully delivered downstream (pass-through saw a clean upstream EOF / assembled JSON written in one shot / synthesized stream reached message_stop); 499 re-marking only applies to incomplete streams — clients often disconnect right after receiving the full stream (Codex especially), which is not an interruption
	ContentMu     sync.Mutex
	content       []byte // Last flightContentCap bytes of pass-through content (raw SSE), for the web in-flight stream viewer
	reqBody       []byte // Raw downstream request body (guarded by contentMu; with the 储存完整结构体 toggle off it's capped at flightContentCap keeping the head, on = untruncated), so the web can show "which request caused this stream"; translation-port streams store the body AFTER translation to Anthropic
	fullContent   []byte // Full pass-through content (guarded by contentMu, recorded only with the 储存完整结构体 toggle on, no cap), for downloading the raw output
	fullPartial   bool   // Content bytes flowed while the 储存完整结构体 toggle was off (opened mid-stream or flapped off-on), so no complete full copy exists for this stream: the partial copy is dropped and hasFull*/full=1 report "not recorded" (guarded by contentMu)
	reqTruncated  bool   // Whether the request body was truncated to just the head (guarded by contentMu): over cap at record time with full-store off, or truncated by purge when the toggle was switched off; the web greys out the download button accordingly

	// Downstream side of dual-link recording (proxy↔client): stored only when it differs from the upstream side
	// (content/reqBody/fullContent above) — Responses translation streams always store (the two protocols necessarily differ),
	// convertAlltoStream-rebuilt JSON streams store contentDown, native streams store reqDown only when rewritten
	// (classifier thinking rewrite / routed model etc.); all empty means both sides identical, and endpoints fall back
	// to the other side, reporting the actual side via the X-Proxy429-Side header. Guarded by contentMu.
	reqDown          []byte                     // Raw downstream→proxy request body (truncation/full rules same as reqBody)
	reqDownTruncated bool                       // Whether reqDown was truncated to just the head
	ContentDown      []byte                     // Proxy→downstream response content (capped at flightContentCap, keeps the tail)
	FullContentDown  []byte                     // Proxy→downstream full response (recorded only with the 储存完整结构体 toggle on)
	FullPartialDown  bool                       // Downstream-side mirror of fullPartial: down bytes that flowed with the toggle off mean no complete down full copy (guarded by contentMu)
	InTokens         int64                      // This stream's cumulative input_tokens (stored from lastInput when forward ends)
	CacheRead        int64                      // This stream's cumulative cache_read
	CacheCreation    int64                      // This stream's cumulative cache_creation (cache writes; part of the hit-rate denominator)
	OutTokens        int64                      // This stream's cumulative output_tokens
	FirstByteMs      int64                      // This stream's first-byte latency (ms), recorded only on the normal response path
	TPS              float64                    // This stream's streaming tok/s, recorded only on the normal response path
	SearchPrompt     string                     // Summary instruction injected into a standalone search sub-agent request (empty when none), shown on the status page via the model cell's hover tooltip
	SearchMarker     bool                       // Standalone search sub-agent marker hit (body carries "Perform a web search for the query"); the web model column shows a [Search] badge
	SearchLevel      string                     // Summary level actually injected into the marker request (empty = none); upgrades the badge to [SearchSummary-<level>]
	SearchThinkFrom  string                     // Thinking value before the search thinking rewrite (empty = no thinking field, shown as auto)
	SearchThinkTo    string                     // Configured level the search thinking rewrite targeted (empty = no rewrite); the API column shows [from->to]
	SearchGuardArmed bool                       // Marker request whose effective route enabled noBullshitOnEmptyClaudeSearch; only native Anthropic 200 SSE responses are buffered
	SearchGuard      bool                       // The upstream response was discarded and replaced with the fixed empty-search stream; the web status column shows [空搜]
	Translated       string                     // Translation-port origin tag ("responses"=translated stream, "responses-raw"=native passthrough via a route with url_response_api); the web API column shows [translate]/[Response]
	CountTokens      bool                       // count_tokens probe stream (countTokensPath); the response is just {"input_tokens":N}; the web model column shows a [count_tokens] prefix
	Compaction       atomic.Bool                // Request body carries a known client context-compaction prompt (detected at body read); the web model column shows [Comp]. Atomic because logDataHandler reads it from another goroutine
	SearchStripped   atomic.Int32               // Total replayed search blocks stripped (conversation-watermark strip + 400-fallback strip; the web shows a red [剥N] badge, the split only goes to the log)
	StripThinking    atomic.Bool                // convertOff2Low upgrade (off->low): thinking blocks are stripped from the response before writing downstream (the client keeps seeing a thinking-off response)
	NoStop           bool                       // Upstream 200 SSE stream ended without a terminal event (message_stop / response.completed|failed|incomplete) while the client stayed connected; the web status column shows a red [无stop] badge
	SearchReplay     *translate.SearchReplayCtx // Search-restore context of the Responses translation port (carried via ctx, read/written only by the handler goroutine); the 400-fallback strip learns the conversation watermark from restore timestamps

	// Session cache tracking (the status page's "缓存年龄" column): written once during routing, read by addFinished in the same goroutine.
	ConvID      string // Session identifier (Anthropic port: session_id inside metadata.user_id; Responses port: prompt_cache_key); empty = does not participate
	ConvAnchor  string // Anchor-key suffix ("route:<pattern>"/"classifier"/"fast", empty=default upstream): only same session + same anchor form one cache lineage
	UpstreamKey string // Upstream grouping key (final base URL after routing | model actually sent; other parameters ignored): the grouping dimension of observed cache-lifetime measurements

	// think is the shortest form of the thinking config in the request body actually sent upstream ("off"/"on <budget>"/"adaptive"/effort word),
	// for the status page's 「API」 column thinking value — which API family the vocabulary belongs to is carried by the column color, not the text. Empty = the request body carried no thinking field (column shows -).
	Think string

	// Conversation lineage (the web 「#N<-M」 parent tag): msgHashes holds one fnv64a hash per normalized
	// element of this request's messages/input array (capped at lineageHashCap); msgTail is a copy of the chain's
	// last normalized element (capped at lineageTailCap) for the tail-growth byte-prefix score; lastFlight is the
	// resolved parent flight id (0 = none / no session).
	MsgHashes  []uint64
	msgTail    []byte
	LastFlight uint64

	toolMu    sync.Mutex
	toolNames []string       // Names of tool calls in the response stream (in first-appearance order; guarded by toolMu)
	toolCalls map[string]int // Call count per tool (guarded by toolMu)
	toolEmpty map[string]int // Per-tool count of calls with an empty arguments object (guarded by toolMu)
}

// realModel returns this stream's real upstream model name: prefer the one the response actually returned, fall back to the route target, then the original model.
// Used for per-model token/retry stats; matches the value logic of addModelUsage in forward.
func (f *Flight) RealModel() string {
	if f.UpstreamModel != "" {
		return f.UpstreamModel
	}
	if f.TargetModel != "" {
		return f.TargetModel
	}
	return f.OrigModel
}

// responsesRaw reports whether this stream is a Responses native passthrough (the matched route has url_response_api):
// a passthrough stream's response is Responses-protocol SSE — usage/tool counts/terminal markers are parsed by Responses semantics,
// and the web API column shows [Response] instead of [translate].
func (f *Flight) ResponsesRaw() bool { return f.Translated == translate.TranslatedResponsesRaw }

// noteToolCall records one tool call (the name from a tool_use/server_tool_use in the response stream's
// content_block_start). The web's 「最近完成的流」 appends [Read*1][Edit*3]-style tags after the model column.
func (f *Flight) NoteToolCall(name string) {
	if name == "" {
		return
	}
	f.toolMu.Lock()
	if f.toolCalls == nil {
		f.toolCalls = make(map[string]int)
	}
	if f.toolCalls[name] == 0 {
		f.toolNames = append(f.toolNames, name) // Record first appearances only, preserving order
	}
	f.toolCalls[name]++
	f.toolMu.Unlock()
}

// noteToolCallEmpty marks one call of this tool as having an empty arguments object (for streams, decided at block end/item done,
// e.g. Kimi's no-args server_tool_use on an empty search). Affects only a single call's *0 tag; multiple same-name calls
// still show the raw count *N.
func (f *Flight) NoteToolCallEmpty(name string) {
	f.toolMu.Lock()
	if f.toolCalls[name] > 0 {
		if f.toolEmpty == nil {
			f.toolEmpty = make(map[string]int)
		}
		f.toolEmpty[name]++
	}
	f.toolMu.Unlock()
}

// toolCallsTag formats the recorded tool calls as "[Read*1][Edit*3][web_search*0]": in first-appearance
// order; N>1 same-name calls show *N (raw count); a single call shows *1, or *0 when its arguments object is empty —
// telling empty searches from real ones at a glance. Returns an empty string when there are no tool calls.
func (f *Flight) ToolCallsTag() string {
	f.toolMu.Lock()
	defer f.toolMu.Unlock()
	var b strings.Builder
	for _, n := range f.toolNames {
		suffix := 1
		if c := f.toolCalls[n]; c > 1 {
			suffix = c // Multiple calls show the raw count (the *0 empty-args check only applies to single calls)
		} else if f.toolEmpty[n] > 0 {
			suffix = 0
		}
		b.WriteByte('[')
		b.WriteString(n)
		b.WriteByte('*')
		b.WriteString(strconv.Itoa(suffix))
		b.WriteByte(']')
	}
	return b.String()
}

// flight stage enum: the progress shown in the web in-flight streams' 「状态」 column.
// stageForward shows the HTTP status code (response being passed through); other stages show their respective Chinese labels.
const (
	stageRequest int32 = iota // 请求 (request): reading body / preparing
	StageRoute                // 路由 (routing): matching route rules
	StageAttempt              // 尝试N (attempt N): sent upstream, awaiting first byte (including retry waits for the next attempt)
	StageForward              // 响应 (response): response received, passing through; shows the status code
)

// stageColor maps a stage to a web status-light color number: 0=white (request) 1=yellow (routing/awaiting first byte) 2=green (forwarding),
// one-to-one with the web flightDot palette. The duration shown next to the status light is per color, not per stage.
func stageColor(s int32) int32 {
	if s >= StageForward {
		return 2
	}
	if s >= StageRoute {
		return 1
	}
	return 0
}

// setStage advances the current stage; stageStart resets only when the light color changes (white→yellow→green each timed separately).
// Stage advances within the same color (routing→attempt, retry re-entering attempt) don't interrupt the timer, so "how long the yellow light has been on" stays continuous and truthful.
func (f *Flight) SetStage(s int32) {
	if stageColor(s) != stageColor(f.Stage.Load()) {
		f.StageStart.Store(time.Now().UnixNano())
	}
	f.Stage.Store(s)
}

// stageMs returns how many milliseconds the current light color has lasted, shown next to the web status light.
func (f *Flight) StageMs() int64 {
	return time.Since(time.Unix(0, f.StageStart.Load())).Milliseconds()
}

// attemptMs returns how many milliseconds the current attempt has awaited the first byte (the web's [尝试N:Xs] next to the yellow light);
// returns -1 when attemptStart=0 (in retry backoff, next attempt not sent yet).
func (f *Flight) AttemptMs() int64 {
	t := f.AttemptStart.Load()
	if t == 0 {
		return -1
	}
	return time.Since(time.Unix(0, t)).Milliseconds()
}

// flight route-reason enum: appended after the stage in the web 「状态」 column (e.g. 「尝试1·搜索」).
// routePassthrough is the default zero value: no route matched, pass through to the default upstream.
const (
	routePassthrough int32 = iota // 透传 (passthrough): no route matched
	RoutePattern                  // pattern: matched a routes[] wildcard
	RouteClassifier               // 分类器 (classifier): classifier_route
	RouteFast                     // fast: fast_route
	RouteMultimodal               // 多模态 (multimodal): multimodal_fallback
	RouteSearch                   // 搜索 (search): search_fallback
)

// flightRegistry manages all in-flight requests.
type flightRegistry struct {
	Mu     sync.RWMutex
	M      map[uint64]*Flight
	NextID atomic.Uint64
}

var Flights flightRegistry

func (fr *flightRegistry) Register(f *Flight) {
	fr.Mu.Lock()
	fr.M[f.ID] = f
	fr.Mu.Unlock()
}

func (fr *flightRegistry) Unregister(id uint64) {
	fr.Mu.Lock()
	delete(fr.M, id)
	fr.Mu.Unlock()
}

func (fr *flightRegistry) Snapshot() []*Flight {
	fr.Mu.RLock()
	defer fr.Mu.RUnlock()
	out := make([]*Flight, 0, len(fr.M))
	for _, f := range fr.M {
		out = append(out, f)
	}
	// Sort by ID for a stable render order.
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// flightContentCap caps one in-flight stream's content buffer: only the most recent bytes are kept for the web viewer,
// avoiding memory bloat from long streams. 256KB covers the full output of most streams; longer ones keep only the tail.
const FlightContentCap = 256 * 1024

// appendContent appends a chunk of pass-through bytes to the flight's content buffer (keeps only the tail when over the cap).
// Called in forward's writeAndCount after w.Write — tees a copy for the web viewer without affecting pass-through.
func (f *Flight) AppendContent(data []byte) {
	f.ContentMu.Lock()
	defer f.ContentMu.Unlock()
	if FullStore.Load() {
		if !f.fullPartial {
			f.fullContent = append(f.fullContent, data...) // Full copy: uncapped, for download
		}
	} else if !f.fullPartial {
		// Bytes flowed while the toggle was off (opened mid-stream / flapped off-on): no complete full copy can
		// exist for this stream, so drop any partial copy and stop full recording — hasFull*/full=1 report "not recorded".
		f.fullPartial = true
		f.fullContent = nil
	}
	f.content = append(f.content, data...)
	if len(f.content) > FlightContentCap {
		f.content = f.content[len(f.content)-FlightContentCap:]
		// The cut point may land mid-SSE-line, leaving half a JSON at the start (e.g. `ext"}}`).
		// Align to the next event boundary (blank line), dropping the incomplete leading event so the raw view starts with a complete SSE line.
		if i := bytes.Index(f.content, []byte("\n\n")); i >= 0 {
			f.content = f.content[i+2:]
		}
	}
}

// snapshotContent returns a copy of the current content buffer, for the web endpoints.
func (f *Flight) SnapshotContent() []byte {
	f.ContentMu.Lock()
	defer f.ContentMu.Unlock()
	out := make([]byte, len(f.content))
	copy(out, f.content)
	return out
}

// setReqBody records the raw downstream request body. With the 储存完整结构体 toggle off, an over-cap body is truncated keeping the head
// (the model/system/tools at the head locate a request better than the tail); with it on, no truncation, for full download.
// Called once when the handler finishes reading the body; never changes afterwards.
func (f *Flight) SetReqBody(b []byte) {
	truncated := false
	if !FullStore.Load() && len(b) > FlightContentCap {
		b = b[:FlightContentCap]
		truncated = true
	}
	f.ContentMu.Lock()
	f.reqBody = b
	f.reqTruncated = truncated
	f.ContentMu.Unlock()
}

// snapshotReqBody returns a copy of the raw request body (nil if not recorded), for the web endpoints.
func (f *Flight) SnapshotReqBody() []byte {
	f.ContentMu.Lock()
	defer f.ContentMu.Unlock()
	if f.reqBody == nil {
		return nil
	}
	out := make([]byte, len(f.reqBody))
	copy(out, f.reqBody)
	return out
}

// snapshotFullContent returns a copy of the full pass-through content (nil if not recorded), for the download endpoint.
func (f *Flight) SnapshotFullContent() []byte {
	f.ContentMu.Lock()
	defer f.ContentMu.Unlock()
	if f.fullContent == nil {
		return nil
	}
	out := make([]byte, len(f.fullContent))
	copy(out, f.fullContent)
	return out
}

// purgeFull releases the full copies when the 储存完整结构体 toggle is switched off: fullContent is set to nil, reqBody cut back to the cap
// (the cut sets the reqTruncated flag; the web greys out the download button accordingly — a truncated copy must not be downloadable as a full one).
func (f *Flight) PurgeFull() {
	f.ContentMu.Lock()
	f.fullContent = nil
	if len(f.reqBody) > FlightContentCap {
		f.reqBody = f.reqBody[:FlightContentCap]
		f.reqTruncated = true
	}
	// Downstream-side mirror: the full copy is likewise cleared and the downstream request body likewise cut back to the cap.
	f.FullContentDown = nil
	if len(f.reqDown) > FlightContentCap {
		f.reqDown = f.reqDown[:FlightContentCap]
		f.reqDownTruncated = true
	}
	f.ContentMu.Unlock()
}

// reqTrunc returns whether the request body was truncated to just the head, for the web endpoints.
func (f *Flight) ReqTrunc() bool {
	f.ContentMu.Lock()
	defer f.ContentMu.Unlock()
	return f.reqTruncated
}

// appendContentDown appends a chunk of proxy→downstream bytes to the downstream-side content buffer (keeps the tail over cap, aligns to SSE boundaries).
func (f *Flight) AppendContentDown(data []byte) {
	f.ContentMu.Lock()
	defer f.ContentMu.Unlock()
	if FullStore.Load() {
		if !f.FullPartialDown {
			f.FullContentDown = append(f.FullContentDown, data...)
		}
	} else if !f.FullPartialDown {
		// Same as appendContent: down bytes flowed with the toggle off → drop the partial down full copy, stop full recording.
		f.FullPartialDown = true
		f.FullContentDown = nil
	}
	f.ContentDown = append(f.ContentDown, data...)
	if len(f.ContentDown) > FlightContentCap {
		f.ContentDown = f.ContentDown[len(f.ContentDown)-FlightContentCap:]
		// Same as appendContent: when the cut lands mid-SSE-line, align to the next event boundary.
		if i := bytes.Index(f.ContentDown, []byte("\n\n")); i >= 0 {
			f.ContentDown = f.ContentDown[i+2:]
		}
	}
}

// snapshotContentDown returns a copy of the downstream-side content buffer (nil if not recorded), for the web endpoints.
func (f *Flight) SnapshotContentDown() []byte {
	f.ContentMu.Lock()
	defer f.ContentMu.Unlock()
	if len(f.ContentDown) == 0 {
		return nil // nil = not recorded (the finishedFlight field contract, same as snapshotReqDown); a non-nil empty slice would falsely read as recorded
	}
	out := make([]byte, len(f.ContentDown))
	copy(out, f.ContentDown)
	return out
}

// setReqDown records the raw downstream→proxy request body; truncation rules same as setReqBody.
func (f *Flight) SetReqDown(b []byte) {
	truncated := false
	if !FullStore.Load() && len(b) > FlightContentCap {
		b = b[:FlightContentCap]
		truncated = true
	}
	f.ContentMu.Lock()
	f.reqDown = b
	f.reqDownTruncated = truncated
	f.ContentMu.Unlock()
}

// snapshotReqDown returns a copy of the downstream-side raw request body (nil if not recorded).
func (f *Flight) SnapshotReqDown() []byte {
	f.ContentMu.Lock()
	defer f.ContentMu.Unlock()
	if f.reqDown == nil {
		return nil
	}
	out := make([]byte, len(f.reqDown))
	copy(out, f.reqDown)
	return out
}

// snapshotFullContentDown returns a copy of the downstream-side full response (nil if not recorded), for the download endpoint.
func (f *Flight) SnapshotFullContentDown() []byte {
	f.ContentMu.Lock()
	defer f.ContentMu.Unlock()
	if f.FullContentDown == nil {
		return nil
	}
	out := make([]byte, len(f.FullContentDown))
	copy(out, f.FullContentDown)
	return out
}

// hasReqDown / hasContentDown / reqDownTrunc / hasFullContentDown feed the web endpoints'
// downstream-side availability (viewer side-switching and download greying).
func (f *Flight) HasReqDown() bool {
	f.ContentMu.Lock()
	defer f.ContentMu.Unlock()
	return f.reqDown != nil
}

// hasContentDown returns whether the downstream-side response has any content yet (nil and empty both count as not recorded).
func (f *Flight) HasContentDown() bool {
	f.ContentMu.Lock()
	defer f.ContentMu.Unlock()
	return len(f.ContentDown) > 0
}

// reqDownTrunc returns whether the downstream-side request body was truncated to just the head.
func (f *Flight) ReqDownTrunc() bool {
	f.ContentMu.Lock()
	defer f.ContentMu.Unlock()
	return f.reqDownTruncated
}

// hasFullContentDown returns whether the downstream-side full output copy is still held.
func (f *Flight) HasFullContentDown() bool {
	f.ContentMu.Lock()
	defer f.ContentMu.Unlock()
	return f.FullContentDown != nil
}

// hasFullContent returns whether the full output copy is still held, for the web to judge download/interact button availability.
func (f *Flight) HasFullContent() bool {
	f.ContentMu.Lock()
	defer f.ContentMu.Unlock()
	return f.fullContent != nil
}

// finishedFlight is an archived snapshot of an ended flight, for the web to review recently finished stream output.
// Archived only when the flight ends with non-empty content (there was streamed output); content is a snapshotContent copy.
type FinishedFlight struct {
	ID            uint64
	Model         string // origModel -> targetModel (-> upstreamModel if the upstream returned one and it differs)
	upstreamModel string // Model name actually returned in the upstream response (empty = not returned / same as targetModel)
	RouteReason   int32  // Route reason (route*); the web model column shows it as a [tag] prefix
	Status        int    // HTTP status code
	GaveUp        bool   // Retries/budget exhausted and a fallback error event has been passed through (status is 0 at this point); the web status column shows [重试尽]
	NoStop        bool   // Upstream 200 SSE stream ended without a terminal event while the client stayed connected; the web status column shows a red [无stop] badge
	SearchGuard   bool   // The upstream response was replaced after every web search result was empty; the web status column shows a red [空搜] badge
	Attempts      int32  // Attempt count (=retries+1; 1 = succeeded first try); the web status column appends [重试N次] when >1
	Bytes         int64
	Stage         int32 // Stage at end
	Ended         time.Time
	TotalMs       int64  // Total duration: downstream request received to response fully sent back downstream (= internal processing like translation/routing/buffering + first-byte wait + streaming + closing internal processing)
	Content       []byte // Pass-through content copy, capped at flightContentCap
	ReqBody       []byte // Raw downstream request body copy, capped at flightContentCap (uncapped with the 储存完整结构体 toggle on; nil if not recorded)
	FullContent   []byte // Full pass-through content copy (recorded only with the 储存完整结构体 toggle on; nil if not recorded)
	ReqTruncated  bool   // Whether the request body was truncated to just the head (for the web to grey out the download button)
	// Dual-link downstream-side (proxy↔client) mirror: populated only for streams that differ from the upstream side (rules per the flight field comments).
	ReqDown          []byte  // Raw downstream→proxy request body (nil = not recorded; endpoints fall back to the other side)
	ReqDownTruncated bool    // Whether reqDown was truncated to just the head
	ContentDown      []byte  // Proxy→downstream response content (nil = not recorded)
	FullContentDown  []byte  // Proxy→downstream full response (nil = not recorded)
	InTokens         int64   // This stream's cumulative input_tokens
	CacheRead        int64   // This stream's cumulative cache_read
	CacheCreation    int64   // This stream's cumulative cache_creation
	OutTokens        int64   // This stream's cumulative output_tokens
	FirstByteMs      int64   // This stream's first-byte latency (ms)
	TPS              float64 // This stream's streaming tok/s
	SearchPrompt     string  // Summary instruction injected into a standalone search sub-agent request (empty when none)
	SearchMarker     bool    // Marker hit (the model column shows [Search])
	Compaction       bool    // The request carried a known client context-compaction prompt (the model column shows [Comp]); finished rows append the compact result via CompactSave
	SearchLevel      string  // Injected summary level (empty = none; the badge upgrades to [SearchSummary-<level>])
	SearchThinkFrom  string  // Thinking value before the rewrite (empty = no thinking field, shown as auto)
	SearchThinkTo    string  // Rewrite target level (empty = no rewrite; the API column shows [from->to])
	Translated       string  // Translation-port origin tag ("responses"=translated, "responses-raw"=native passthrough); the web API column shows [translate]/[Response]
	CountTokens      bool    // count_tokens probe stream; the web model column shows a [count_tokens] prefix
	Tools            string  // Tool-call tag ("[Read*1][Edit*3]", empty when no tools), appended in the web model column
	SearchStripped   int     // Total replayed search blocks stripped (conversation-watermark strip + 400-fallback strip; 0 = none); the web cache-hit column shows a red [剥N]
	ConvKey          string  // Session cache anchor key (convID|convAnchor); empty = this stream joins neither the "缓存年龄" display nor trim protection
	// start anchors the stream start time: cache writes/refreshes happen when the upstream processes the input (≈stream start); both "缓存年龄" and the trim-protection window anchor to it.
	Start       time.Time
	obsKey      string // Observed cache-lifetime pairing key (convID|upstreamKey); empty = no observation (yellow-light 499 / no session / no grouping key)
	upstreamKey string // Upstream grouping key (post-routing url|model): the 「缓存命中」 popup's observed table groups by it
	Think       string // Shortest form of the thinking config actually sent upstream (status page 「API」 column thinking value); empty = request body carried no thinking field

	// Lineage fields: kept after archival so the finished ring still answers parent lookups and shows the 「#N<-M」 tag.
	ConvID     string   // Session identifier (copied from the flight; finishedFlight otherwise only carries convKey)
	MsgHashes  []uint64 // Per-element content hashes of the request's normalized message array (see flight.msgHashes)
	msgTail    []byte   // Normalized last chain element, capped (see flight.msgTail)
	LastFlight uint64   // Resolved lineage parent flight id (0 = none)
}

var (
	FinishedMu  sync.Mutex
	Finished    []FinishedFlight
	FinishedCap atomic.Int32 // How many finished streams to keep; adjustable on the status page; init sets 10
)

// fullStore is the 储存完整结构体 toggle (switchable on the status page, default off, resets on restart):
// on = flights additionally record full request bodies/output (no 256KB cap) for download; off = full copies are dropped immediately.
var FullStore atomic.Bool

func init() {
	FinishedCap.Store(10)
}

// markClientGone corrects the status code to upstream conventions when the flight is archived (goal: match the upstream provider's dashboard):
//   - upstream finished sending (delivered): keep 200 whether or not downstream is still there (the upstream dashboard also shows 200);
//   - upstream stream started but not finished (status==200, not delivered): record 499 — a downstream cancel propagates via ctx into an upstream
//     disconnect, and the upstream dashboard likewise records 499 (nginx's client-closed-request convention);
//   - ended before any upstream response due to a downstream cancel (status==0 and ctx cancelled): the request reached the upstream but
//     was aborted; the upstream dashboard likewise shows 499. Local errors (body read failure etc., ctx not cancelled) are unaffected.
//
// Called from the handler's closing defer — at that point ctx can only be cancelled by a downstream disconnect (the handler's own cancel
// runs after this, by defer LIFO).
func MarkClientGone(f *Flight, ctx context.Context) {
	if f.Delivered.Load() {
		return
	}
	if f.Status == 200 || (f.Status == 0 && ctx.Err() != nil) {
		f.Status = 499
	}
}

// addFinished archives a flight's pass-through content when it ends, for the web to review recently finished streams.
// Empty content is not skipped: failed/retries-exhausted/non-streaming requests also enter the list, making it possible to see why a stream produced no output.
// Oldest entries are dropped beyond finishedCap. Called from the handler defer, single goroutine.
// refreshArchivedDown refreshes a finished archive's downstream-side copy from the flight's current downstream records: translation-port
// non-streaming responses are produced by tw.finish AFTER the handler returns (archives), so the archived snapshot misses that tail and needs this backfill.
// Does nothing if the archive no longer holds the stream (e.g. finishedCap=0 evicted it immediately).
func refreshArchivedDown(f *Flight) {
	FinishedMu.Lock()
	defer FinishedMu.Unlock()
	for i := range Finished {
		if Finished[i].ID == f.ID {
			Finished[i].ContentDown = f.SnapshotContentDown()
			Finished[i].FullContentDown = f.SnapshotFullContentDown()
			return
		}
	}
}

// RefreshArchivedDown is the translate FlightTap back-fill hook: called after archiving to refresh the archived
// downstream-side copy (the method set of *Flight provides the tee half via AppendContentDown).
func (f *Flight) RefreshArchivedDown() { refreshArchivedDown(f) }

func AddFinished(f *Flight) {
	content := f.SnapshotContent()
	model := f.OrigModel
	if f.TargetModel != "" && f.TargetModel != f.OrigModel {
		model = f.OrigModel + " -> " + f.TargetModel
	}
	// When the model actually returned by the upstream differs from the route target, append a third segment so the user sees the real landing spot.
	if f.UpstreamModel != "" && f.UpstreamModel != f.TargetModel && f.UpstreamModel != f.OrigModel {
		model = model + " -> " + f.UpstreamModel
	}
	now := time.Now()
	convKey := ConvKeyOf(f)
	obsKey := ObsKeyOf(f)
	// A 499 from a downstream disconnect during yellow light (awaiting first byte, stage<3): the upstream may not have processed the input and written the cache, so conservatively don't refresh the anchor —
	// treat as anchorless (this row shows "-", the key's anchor stays on the previous same-key stream). A 499 during green light (stage=3 forwarding):
	// the upstream streaming output proves the input was processed and the cache written, so refresh the anchor as usual.
	if f.Status == 499 && f.Stage.Load() < 3 {
		convKey = ""
		obsKey = "" // Whether the cache was written is uncertain, so it doesn't count for lifetime observation either (this stream is no one's cur, and after archiving no one's prev)
	}
	ff := FinishedFlight{
		ID:               f.ID,
		Model:            model,
		upstreamModel:    f.UpstreamModel,
		RouteReason:      f.RouteReason.Load(),
		Status:           f.Status,
		GaveUp:           f.GaveUp,
		NoStop:           f.NoStop,
		SearchGuard:      f.SearchGuard,
		Attempts:         f.Attempt.Load(),
		Bytes:            f.Bytes.Load(),
		Stage:            f.Stage.Load(),
		Ended:            now,
		TotalMs:          now.Sub(f.Start).Milliseconds(),
		Content:          content,
		ReqBody:          f.SnapshotReqBody(),
		FullContent:      f.SnapshotFullContent(),
		ReqTruncated:     f.ReqTrunc(),
		ReqDown:          f.SnapshotReqDown(),
		ReqDownTruncated: f.ReqDownTrunc(),
		ContentDown:      f.SnapshotContentDown(),
		FullContentDown:  f.SnapshotFullContentDown(),
		InTokens:         f.InTokens,
		CacheRead:        f.CacheRead,
		CacheCreation:    f.CacheCreation,
		OutTokens:        f.OutTokens,
		FirstByteMs:      f.FirstByteMs,
		TPS:              f.TPS,
		SearchPrompt:     f.SearchPrompt,
		SearchMarker:     f.SearchMarker,
		Compaction:       f.Compaction.Load(),
		SearchLevel:      f.SearchLevel,
		SearchThinkFrom:  f.SearchThinkFrom,
		SearchThinkTo:    f.SearchThinkTo,
		Translated:       f.Translated,
		CountTokens:      f.CountTokens,
		Tools:            f.ToolCallsTag(),
		SearchStripped:   int(f.SearchStripped.Load()),
		ConvKey:          convKey,
		Start:            f.Start,
		obsKey:           obsKey,
		upstreamKey:      f.UpstreamKey,
		Think:            f.Think,
		ConvID:           f.ConvID,
		MsgHashes:        f.MsgHashes,
		msgTail:          f.msgTail,
		LastFlight:       f.LastFlight,
	}
	FinishedMu.Lock()
	// Observed cache-lifetime measurement: pair with the previous finished stream of the same session + same upstream (finished is ascending; scan backwards for the latest same-key entry).
	// The interval anchors to the two streams' start times (cache writes/reads both happen near stream start).
	if ff.obsKey != "" {
		for i := len(Finished) - 1; i >= 0; i-- {
			if Finished[i].obsKey == ff.obsKey {
				if kind, iv := ClassifyCacheObservation(Finished[i].CacheRead, ff.CacheRead, ff.InTokens, ff.CacheCreation, f.Start.Sub(Finished[i].Start)); kind != ObsNone {
					RecordCacheObsLocked(ff.upstreamKey, kind, iv)
				}
				break
			}
		}
	}
	Finished = append(Finished, ff)
	TrimFinishedLocked()
	FinishedMu.Unlock()
	// 活跃度热图：流归档时记一格（count_tokens 探针不计，与 token 统计同口径；499 中断流计入——活动确实发生了）。
	if !ff.CountTokens {
		HeatAdd(ff.Ended, 1, ff.OutTokens, ff.InTokens, 0, 0)
	}
	log.Printf("[flight] #%d archived %d bytes stage=%d", f.ID, len(content), ff.Stage)
}

// convKeyOf returns this stream's session cache anchor key; empty when there is no session identifier (count_tokens probes, bare API calls without metadata).
func ConvKeyOf(f *Flight) string {
	if f.ConvID == "" {
		return ""
	}
	return f.ConvID + "|" + f.ConvAnchor
}

// trimFinishedLocked trims finished to finishedCap entries: the eviction window is the oldest len-cap rows;
// protected rows inside the window are skipped, not evicted (with no compensating extension — when trimming 14 rows to 10 and 1 of the 4 window rows is protected, only 3 are dropped, leaving 11).
// Protected = the newest row of each anchor key (session+route) still inside the protection window (now < start + defaultCacheTTL);
// it carries the status page's "缓存年龄", and evicting it would hide that (row count may exceed cap in this case).
// Rows past the protection window or superseded by a newer same-key row (showing -) are unprotected and evicted FIFO as usual. Caller must hold finishedMu.
func TrimFinishedLocked() {
	capN := int(FinishedCap.Load())
	if len(Finished) <= capN {
		return
	}
	// Index of each key's newest row (finished is time-ascending; later entries overwriting earlier ones during the walk yields the newest).
	latest := make(map[string]int)
	for i := range Finished {
		if Finished[i].ConvKey != "" {
			latest[Finished[i].ConvKey] = i
		}
	}
	now := time.Now()
	protected := make(map[int]bool, len(latest))
	for _, i := range latest {
		if now.Before(Finished[i].Start.Add(defaultCacheTTL)) {
			protected[i] = true
		}
	}
	window := len(Finished) - capN // Eviction window: only the oldest window rows are eligible
	out := Finished[:0]            // In-place filter: the write pointer never passes the read pointer, safe
	for i, ff := range Finished {
		if i < window && !protected[i] {
			continue
		}
		out = append(out, ff)
	}
	Finished = out
}

// defaultCacheTTL is the fixed protection window for finished-stream trimming: the newest row of each anchor key
// cannot be evicted by FinishedCap within 5 minutes of stream start. The upstream cache lifetime is empirically
// dynamic (see cache observations), so there is no configurable "TTL" — the cache_time option is gone,
// leaving this one fixed window, enough to cover typical conversation pacing.
const defaultCacheTTL = 5 * time.Minute
