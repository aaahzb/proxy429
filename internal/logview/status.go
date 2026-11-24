package logview

import (
	"encoding/json"
	"log"
	"net/http"
	"path/filepath"
	"strconv"
	"time"

	"proxy429/internal/config"
	"proxy429/internal/platform"
	"proxy429/internal/recorder"
)

// flightInfo is one in-flight stream's display info (for the web 「状态」 tab's table).
type flightInfo struct {
	ID           uint64 `json:"id"`
	Model        string `json:"model"`
	Phase        int    `json:"phase"`                 // 0=awaiting response, 1=response received (forwarding started)
	Bytes        int64  `json:"bytes"`                 // Forwarded bytes
	Status       int    `json:"status"`                // HTTP status code (shown when stage=stageForward)
	Stage        int32  `json:"stage"`                 // Current stage: 0=request 1=routing 2=attemptN 3=forwarding (shows status code)
	Attempt      int32  `json:"attempt"`               // Current attempt number (from 1); 「尝试N」 is shown when stage=2
	AttemptMs    int64  `json:"attemptMs"`             // Milliseconds the current attempt has awaited the first byte ([尝试N:Xs] next to the yellow light); -1=in retry backoff (shows [退避中])
	RouteReason  int32  `json:"routeReason"`           // Route reason: 0=passthrough 1=pattern 2=classifier 3=fast 4=multimodal 5=search
	Translated   string `json:"translated"`            // Translation-port origin ("responses"=translated / "responses-raw"=native passthrough [implemented, not field-tested, undocumented]); the API column shows [translate]/[Response]
	CountTokens  bool   `json:"countTokens,omitempty"` // count_tokens probe stream; the model column shows a [count_tokens] prefix
	SearchPrompt string `json:"searchPrompt,omitempty"`
	SearchMarker bool   `json:"searchMarker,omitempty"` // Standalone search sub-agent marker hit; the model column shows [Search]
	Compaction   bool   `json:"compaction,omitempty"`   // Client context-compaction request; the model column shows [Compact]
	SearchLevel  string `json:"searchLevel,omitempty"`  // Injected summary level; the badge upgrades to [SearchSummary-<level>]
	ThinkFrom    string `json:"thinkFrom,omitempty"`    // Thinking value before the last body-side thinking rewrite (empty renders as auto)
	ThinkTo      string `json:"thinkTo,omitempty"`      // Rewrite target level (empty = none; the API column shows [from->to])
	ReqTrunc     bool   `json:"reqTrunc,omitempty"`     // Request body truncated to just the head 256KB (download button greyed out)
	HasFull      bool   `json:"hasFull,omitempty"`      // Full output copy still held (download output available; interactive JSON prefers it when present)
	StageMs      int64  `json:"stageMs"`                // Milliseconds the current light color has lasted (shown next to the status light; only resets on color change)
	Tools        string `json:"tools,omitempty"`        // Tool-call tag ("[Read*1][Edit*3]", omitted when no tools; accumulates live during forwarding)
	Stripped     int    `json:"stripped,omitempty"`     // Total replayed search blocks stripped (conversation watermark + 400 fallback; 0 is omitted); the model column shows a red [剥N]
	Think        string `json:"think,omitempty"`        // Shortest form of the thinking config actually sent upstream ("off"/"on N"/"adaptive"/effort word; which API family is carried by the column color); empty = no thinking field (column shows -)
	ReqDown      bool   `json:"reqDown,omitempty"`      // Downstream-side (downstream→proxy) request body recorded (recorded only when it differs from the upstream side; the viewer can switch link sides)
	RespDown     bool   `json:"respDown,omitempty"`     // Downstream-side (proxy→downstream) response content recorded (always present for translation streams and rebuilt-JSON streams)
	ReqDownTrunc bool   `json:"reqDownTrunc,omitempty"` // Downstream-side request body truncated to just the head 256KB (download greyed out after switching to the downstream side)
	HasFullDown  bool   `json:"hasFullDown,omitempty"`  // Downstream-side full output copy still held (the downstream side's download output; interactive JSON prefers it when present)
	Last         uint64 `json:"last,omitempty"`         // Lineage parent flight id (same-session message-hash prefix chain); omitted when none — the # column shows 「#N<-M」 when present
}

// logData is the JSON returned by /__logs/data: recent logs + full status counters + the in-flight stream list.
type logData struct {
	Lines         []string                   `json:"lines"`
	Active        int                        `json:"active"`
	Waiting       int                        `json:"waiting"`
	Version       string                     `json:"version"`
	CacheRead     int64                      `json:"cacheRead"`
	CacheCreation int64                      `json:"cacheCreation"`
	InputTokens   int64                      `json:"inputTokens"`
	OutputTokens  int64                      `json:"outputTokens"`
	ModelStats    []recorder.ModelUsageEntry `json:"modelStats"`
	CacheObs      []recorder.CacheObsRow     `json:"cacheObs"`  // Observed cache lifetimes (by upstream URL+model); the cache-hit popup's second table
	BytesUp       int64                      `json:"bytesUp"`   // Bytes sent upstream (request bodies; each retry attempt counted)
	BytesDown     int64                      `json:"bytesDown"` // Bytes returned from upstream (response bodies, counted as read)
	HeatBase      int64                      `json:"heatBase"`  // Absolute slot number (unix/900) of Heat[0]; the 活跃度 heatmap window base
	Heat          [][5]int64                 `json:"heat"`      // 192 quarter-hour slots oldest-first, each [requests, output tokens, input tokens, bytes up, bytes down]
	Retries       int64                      `json:"retries"`
	Classifiers   int64                      `json:"classifiers"`
	AvgFirstByte  float64                    `json:"avgFirstByte"` // ms
	Tps           float64                    `json:"tps"`          // tok/s
	Flights       []flightInfo               `json:"flights"`
	CurrentCfg    string                     `json:"currentCfg"`           // Currently active config file name
	FinishedCap   int32                      `json:"finishedCap"`          // How many finished streams to keep, N (adjustable on the status page)
	FullStore     bool                       `json:"fullStore"`            // The 储存完整结构体 toggle (switchable on the status page, default off)
	HijackRisk    bool                       `json:"hijackRisk,omitempty"` // Loopback-hijack trap armed (system proxy on, NO_PROXY missing) -> status-tab red banner

	// ClassifierNoThink is the thinking-rewrite count: +1 only when a body is actually rewritten (off or low).
	// (Classifiers is the count of requests matching a classifier signature: tallied whether or not rerouted/rewritten.)
	ClassifierNoThink int64 `json:"classifierNoThink"`

	// Per-source split of the classifier counters (Claude Code security monitor vs Codex guardian).
	ClassifiersCC           int64 `json:"classifiersCC"`
	ClassifiersCodex        int64 `json:"classifiersCodex"`
	ClassifierRewritesCC    int64 `json:"classifierRewritesCC"`
	ClassifierRewritesCodex int64 `json:"classifierRewritesCodex"`
}

// logDataHandler returns the most recent maxLogBuf log lines and the full status (JSON), polled by the page every 500ms.
func logDataHandler(w http.ResponseWriter, r *http.Request) {
	if !platform.IsLocalRequest(r) {
		http.Error(w, "forbidden (local only)", http.StatusForbidden)
		return
	}
	d := logData{Lines: recorder.LogBuf.Window(0, recorder.MaxLogBuf), Version: deps.Version, CurrentCfg: filepath.Base(config.CurrentConfigPath()), FinishedCap: recorder.FinishedCap.Load(), FullStore: recorder.FullStore.Load(), HijackRisk: platform.ProxyHijackRisk()}
	recorder.Stats.Mu.Lock()
	d.Active = recorder.Stats.Active
	d.Waiting = recorder.Stats.Waiting
	d.CacheRead = recorder.Stats.CacheRead
	d.CacheCreation = recorder.Stats.CacheCreation
	d.InputTokens = recorder.Stats.InputTokens
	d.OutputTokens = recorder.Stats.OutputTokens
	recorder.Stats.Mu.Unlock()
	d.ModelStats = recorder.Stats.SnapshotModelStats()
	d.CacheObs = recorder.SnapshotCacheObs()
	d.HeatBase, d.Heat = recorder.SnapshotHeat()
	d.BytesUp = recorder.Stats.BytesUp.Load()
	d.BytesDown = recorder.Stats.BytesDown.Load()
	d.Retries = recorder.Stats.StatusRetries.Load()
	d.Classifiers = recorder.Stats.ClassifierHits.Load()
	d.ClassifierNoThink = recorder.Stats.ClassifierRewrites.Load()
	d.ClassifiersCC = recorder.Stats.ClassifierHitsCC.Load()
	d.ClassifiersCodex = recorder.Stats.ClassifierHitsCodex.Load()
	d.ClassifierRewritesCC = recorder.Stats.ClassifierRewritesCC.Load()
	d.ClassifierRewritesCodex = recorder.Stats.ClassifierRewritesCodex.Load()
	d.AvgFirstByte, d.Tps = recorder.Stats.RecentLatency()

	// In-flight stream list: sorted by ID (snapshot is already sorted), showing model and forwarding progress.
	for _, f := range recorder.Flights.Snapshot() {
		model := f.OrigModel
		if f.TargetModel != "" && f.TargetModel != f.OrigModel {
			model = f.OrigModel + " -> " + f.TargetModel
		}
		d.Flights = append(d.Flights, flightInfo{
			ID:           f.ID,
			Model:        model,
			Phase:        int(f.Phase.Load()),
			Bytes:        f.Bytes.Load(),
			Status:       f.Status,
			Stage:        f.Stage.Load(),
			Attempt:      f.Attempt.Load(),
			AttemptMs:    f.AttemptMs(),
			RouteReason:  f.RouteReason.Load(),
			Translated:   f.Translated,
			CountTokens:  f.CountTokens,
			SearchPrompt: f.SearchPrompt,
			SearchMarker: f.SearchMarker,
			Compaction:   f.Compaction.Load(),
			SearchLevel:  f.SearchLevel,
			ThinkFrom:    f.ThinkFrom,
			ThinkTo:      f.ThinkTo,
			Tools:        f.ToolCallsTag(),             // Tool calls accumulate live; the in-flight model column fills in progressively with the 500ms polls
			Stripped:     int(f.SearchStripped.Load()), // Watermark strips are booked at stream creation; 400-fallback strips accrue during forwarding
			ReqTrunc:     f.ReqTrunc(),
			HasFull:      f.HasFullContent(),
			ReqDown:      f.HasReqDown(),
			RespDown:     f.HasContentDown(),
			ReqDownTrunc: f.ReqDownTrunc(),
			HasFullDown:  f.HasFullContentDown(),
			StageMs:      f.StageMs(),
			Think:        f.Think,
			Last:         f.LastFlight,
		})
	}
	if d.Flights == nil {
		d.Flights = []flightInfo{}
	}

	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(d)
}

// resetStatsHandler clears cumulative stats (bytes/tokens/retries/classifierRewrites/latency samples)
// and the 「最近完成的流」 list. Switching configs no longer clears stats automatically; the 「清空统计」 button does it manually. Localhost only.
func resetStatsHandler(w http.ResponseWriter, r *http.Request) {
	if !platform.IsLocalRequest(r) {
		http.Error(w, "forbidden (local only)", http.StatusForbidden)
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	deps.ClearStats()
	log.Printf("[stats] cleared")
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
}

// clearLogsHandler clears the in-memory log buffer (web viewing only; doesn't affect the log_file on-disk file). Localhost only.
func clearLogsHandler(w http.ResponseWriter, r *http.Request) {
	if !platform.IsLocalRequest(r) {
		http.Error(w, "forbidden (local only)", http.StatusForbidden)
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	recorder.LogBuf.Clear()
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
}

// FlightHandler returns the given in-flight stream's recent pass-through content (raw SSE), for the web's click-to-view.
// 404 when the stream doesn't exist (already ended). Localhost only.
// pickFlightSide picks the record per the viewer's requested link side (side=up|down): in dual-link recording the downstream side
// is stored only when it differs from the upstream side (translation streams always store), a missing side falls back to the other;
// the second return value is the actual side, reported to the caller via the X-Proxy429-Side header (the viewer shows a fallback hint from it).
func pickFlightSide(side string, up, down []byte) ([]byte, string) {
	if side == "down" {
		if len(down) > 0 {
			return down, "down"
		}
		return up, "up"
	}
	if len(up) > 0 {
		return up, "up"
	}
	return down, "down"
}

func FlightHandler(w http.ResponseWriter, r *http.Request) {
	if !platform.IsLocalRequest(r) {
		http.Error(w, "forbidden (local only)", http.StatusForbidden)
		return
	}
	id, err := strconv.ParseUint(r.URL.Query().Get("id"), 10, 64)
	if err != nil {
		http.Error(w, "invalid id", http.StatusBadRequest)
		return
	}
	side := r.URL.Query().Get("side") // up=proxy↔upstream (default) / down=downstream↔proxy
	if r.URL.Query().Get("full") == "1" {
		// Download the raw output: full content is recorded only with the 储存完整结构体 toggle on.
		if !recorder.FullStore.Load() {
			http.Error(w, "store-full-payloads is off", http.StatusForbidden)
			return
		}
		recorder.Flights.Mu.RLock()
		f0, ok0 := recorder.Flights.M[id]
		recorder.Flights.Mu.RUnlock()
		var up, down []byte
		if ok0 {
			up = f0.SnapshotFullContent()
			down = f0.SnapshotFullContentDown()
		} else {
			recorder.FinishedMu.Lock()
			for _, ff := range recorder.Finished {
				if ff.ID == id {
					up = ff.FullContent
					down = ff.FullContentDown
					break
				}
			}
			recorder.FinishedMu.Unlock()
		}
		full, served := pickFlightSide(side, up, down)
		if len(full) == 0 {
			http.Error(w, "no full output recorded for this stream (started before the switch was on, or no proxied content)", http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("X-Proxy429-Side", served)
		_, _ = w.Write(full)
		return
	}
	recorder.Flights.Mu.RLock()
	f, ok := recorder.Flights.M[id]
	recorder.Flights.Mu.RUnlock()
	var up, down []byte
	if ok {
		up = f.SnapshotContent()
		down = f.SnapshotContentDown()
	} else {
		// Not in-flight: check the finished-streams archive (content is a static snapshot).
		recorder.FinishedMu.Lock()
		for _, ff := range recorder.Finished {
			if ff.ID == id {
				up = ff.Content
				down = ff.ContentDown
				break
			}
		}
		recorder.FinishedMu.Unlock()
		if up == nil && down == nil {
			http.Error(w, "stream already finished or does not exist", http.StatusNotFound)
			return
		}
	}
	content, served := pickFlightSide(side, up, down)
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Proxy429-Side", served)
	_, _ = w.Write(content)
}

// FlightReqHandler returns the given stream's downstream request body verbatim, so the web can show "which request caused this stream".
// Both in-flight and finished are queried; 404 when the stream doesn't exist or wasn't recorded (search sub-streams, body-read failure). Localhost only.
func FlightReqHandler(w http.ResponseWriter, r *http.Request) {
	if !platform.IsLocalRequest(r) {
		http.Error(w, "forbidden (local only)", http.StatusForbidden)
		return
	}
	id, err := strconv.ParseUint(r.URL.Query().Get("id"), 10, 64)
	if err != nil {
		http.Error(w, "invalid id", http.StatusBadRequest)
		return
	}
	recorder.Flights.Mu.RLock()
	f, ok := recorder.Flights.M[id]
	recorder.Flights.Mu.RUnlock()
	var up, down []byte
	if ok {
		up = f.SnapshotReqBody()
		down = f.SnapshotReqDown()
	} else {
		// Not in-flight: check the finished-streams archive (reqBody is a static snapshot).
		recorder.FinishedMu.Lock()
		for _, ff := range recorder.Finished {
			if ff.ID == id {
				up = ff.ReqBody
				down = ff.ReqDown
				break
			}
		}
		recorder.FinishedMu.Unlock()
	}
	// side=up (default, the body actually sent proxy→upstream) / side=down (downstream→proxy original, stored only for rewritten/translated streams), missing side falls back.
	body, served := pickFlightSide(r.URL.Query().Get("side"), up, down)
	if body == nil {
		http.Error(w, "stream does not exist or no request body recorded", http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Proxy429-Side", served)
	_, _ = w.Write(body)
}

// RecentFlightsHandler returns the recently finished streams list (summary, no content), newest first. Localhost only.
// The cacheAge column's semantics: each anchor key's (session+route) newest row shows the cache age (time since the cache was created, counting up);
// older rows superseded by a newer same-key row and rows without a session identifier (count_tokens/bare API) show "-";
// while a same-key in-flight stream is streaming, the display freezes as [m:ss] (the old anchor's age at the in-flight stream's start; the number doesn't change).
func RecentFlightsHandler(w http.ResponseWriter, r *http.Request) {
	if !platform.IsLocalRequest(r) {
		http.Error(w, "forbidden (local only)", http.StatusForbidden)
		return
	}
	// While a same-key in-flight stream is streaming (stage=3 forwarding), that key's cache was actually just refreshed by the upstream — the previous finished row
	// freezes showing the old anchor's age at the moment the refresh happened (the in-flight stream's start), like [4:32]; the new anchor officially takes effect
	// once that in-flight stream finishes archiving. With multiple concurrent same-key streaming streams, the earliest-started one wins (the first refresh moment).
	// Exception: an in-flight stream that started BEFORE the newest finished row (concurrent same-key streams) does not freeze the row —
	// the finished row is already the newer refresh, so plain counting age shows (freezing would print a negative bracket like [-9:00]).
	streaming := make(map[string]time.Time)
	for _, f := range recorder.Flights.Snapshot() {
		if f.Stage.Load() == 3 {
			if k := recorder.ConvKeyOf(f); k != "" {
				if ts, ok := streaming[k]; !ok || f.Start.Before(ts) {
					streaming[k] = f.Start
				}
			}
		}
	}
	recorder.FinishedMu.Lock()
	now := time.Now()
	// Index of each key's newest row (finished is ascending; later entries overwrite earlier ones). After eviction an old row may get "promoted" to visible-newest,
	// showing its own age — lifetimes are dynamic, and a large age only means the session hasn't written cache in a long time.
	latest := make(map[string]int)
	for i := range recorder.Finished {
		if recorder.Finished[i].ConvKey != "" {
			latest[recorder.Finished[i].ConvKey] = i
		}
	}
	out := make([]map[string]any, 0, len(recorder.Finished))
	for i := len(recorder.Finished) - 1; i >= 0; i-- {
		ff := recorder.Finished[i]
		cacheAge := "-"
		if ff.ConvKey != "" && latest[ff.ConvKey] == i {
			if ts, ok := streaming[ff.ConvKey]; ok && ts.After(ff.Start) {
				cacheAge = "[" + recorder.FmtCacheAge(ts.Sub(ff.Start)) + "]"
			} else {
				cacheAge = recorder.FmtCacheAge(now.Sub(ff.Start))
			}
		}
		compactSave := ""
		if ff.Compaction {
			compactSave = recorder.CompactSave(ff.InTokens, ff.CacheRead, ff.CacheCreation, ff.OutTokens)
		}
		out = append(out, map[string]any{
			"id":            ff.ID,
			"model":         ff.Model,
			"routeReason":   ff.RouteReason,
			"translated":    ff.Translated,
			"countTokens":   ff.CountTokens,
			"think":         ff.Think,
			"last":          ff.LastFlight,
			"status":        ff.Status,
			"gaveUp":        ff.GaveUp,
			"noStop":        ff.NoStop,
			"searchGuard":   ff.SearchGuard,
			"attempts":      ff.Attempts,
			"bytes":         ff.Bytes,
			"total":         recorder.FmtMs(ff.TotalMs),
			"stage":         ff.Stage,
			"ended":         ff.Ended.Format("15:04:05"),
			"hitRate":       recorder.CacheHitRate(ff.CacheRead, ff.InTokens, ff.CacheCreation),
			"cacheAge":      cacheAge,
			"cacheRead":     ff.CacheRead,
			"cacheCreation": ff.CacheCreation,
			"inTokens":      ff.InTokens,
			"firstByte":     recorder.FmtFirstByte(ff.FirstByteMs),
			"tps":           recorder.FmtTps(ff.TPS),
			"searchPrompt":  ff.SearchPrompt,
			"searchMarker":  ff.SearchMarker,
			"compaction":    ff.Compaction,
			"compactSave":   compactSave,
			"searchLevel":   ff.SearchLevel,
			"thinkFrom":     ff.ThinkFrom,
			"thinkTo":       ff.ThinkTo,
			"tools":         ff.Tools,
			"stripped":      ff.SearchStripped,
			"reqTrunc":      ff.ReqTruncated,
			"hasFull":       ff.FullContent != nil,
			"reqDown":       ff.ReqDown != nil,
			"respDown":      ff.ContentDown != nil,
			"reqDownTrunc":  ff.ReqDownTruncated,
			"hasFullDown":   ff.FullContentDown != nil,
		})
	}
	recorder.FinishedMu.Unlock()
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(map[string]any{"list": out})
}

// finishedCapHandler sets how many finished streams to keep, N (0..200), trimming finished immediately. Localhost only.
func finishedCapHandler(w http.ResponseWriter, r *http.Request) {
	if !platform.IsLocalRequest(r) {
		http.Error(w, "forbidden (local only)", http.StatusForbidden)
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var b struct {
		N int `json:"n"`
	}
	if err := json.NewDecoder(r.Body).Decode(&b); err != nil {
		http.Error(w, "invalid JSON: "+err.Error(), http.StatusBadRequest)
		return
	}
	if b.N < 0 {
		b.N = 0
	}
	if b.N > 200 {
		b.N = 200
	}
	recorder.FinishedCap.Store(int32(b.N))
	recorder.FinishedMu.Lock()
	recorder.TrimFinishedLocked()
	recorder.FinishedMu.Unlock()
	log.Printf("[flight] keep-finished set to %d", b.N)
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "n": b.N})
}

// FullStoreHandler toggles 「储存完整结构体」: when on, newly started requests record full request bodies and output
// (no 256KB cap, for download); switching off immediately drops the full copies of in-flight/archived streams (reqBody cut back to cap),
// and the web download button disappears accordingly. Localhost only.
func FullStoreHandler(w http.ResponseWriter, r *http.Request) {
	if !platform.IsLocalRequest(r) {
		http.Error(w, "forbidden (local only)", http.StatusForbidden)
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var b struct {
		Enabled bool `json:"enabled"`
	}
	if err := json.NewDecoder(r.Body).Decode(&b); err != nil {
		http.Error(w, "invalid JSON: "+err.Error(), http.StatusBadRequest)
		return
	}
	recorder.FullStore.Store(b.Enabled)
	if !b.Enabled {
		// Switching off releases the full copies: in-flight streams are purged one by one, archives likewise.
		for _, f := range recorder.Flights.Snapshot() {
			f.PurgeFull()
		}
		recorder.FinishedMu.Lock()
		for i := range recorder.Finished {
			recorder.Finished[i].FullContent = nil
			if len(recorder.Finished[i].ReqBody) > recorder.FlightContentCap {
				recorder.Finished[i].ReqBody = recorder.Finished[i].ReqBody[:recorder.FlightContentCap]
				recorder.Finished[i].ReqTruncated = true
			}
		}
		recorder.FinishedMu.Unlock()
	}
	log.Printf("[fullstore] store-full-payloads set to %v", b.Enabled)
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "enabled": b.Enabled})
}
