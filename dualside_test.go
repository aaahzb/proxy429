package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"
)

// ensureFlightsMap initializes the global in-flight table (the real startup path builds it in main; unit tests run
// back-to-back in file order within one process, and this file sorts before the other table-building cases, so it must self-build idempotently).
func ensureFlightsMap() {
	flights.mu.Lock()
	if flights.m == nil {
		flights.m = make(map[uint64]*flight)
	}
	flights.mu.Unlock()
}

// serveFlightSide requests /__flight?id=N[&full=1][&side=S] with a localhost RemoteAddr,
// returning the HTTP status code, the X-Proxy429-Side response header (the side the server actually returned), and the response body bytes.
func serveFlightSide(t *testing.T, id uint64, side string, full bool) (int, string, []byte) {
	t.Helper()
	url := "/__flight?id=" + strconv.FormatUint(id, 10)
	if full {
		url += "&full=1"
	}
	if side != "" {
		url += "&side=" + side
	}
	r := httptest.NewRequest(http.MethodGet, url, nil)
	r.RemoteAddr = "127.0.0.1:43210" // httptest defaults to 192.0.2.1, which isLocalRequest rejects
	rec := httptest.NewRecorder()
	flightHandler(rec, r)
	res := rec.Result()
	defer res.Body.Close()
	body, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatalf("读响应体失败: %v", err)
	}
	return res.StatusCode, res.Header.Get("X-Proxy429-Side"), body
}

// serveFlightReqSide does the same against /__flightreq (the request-body viewer endpoint).
func serveFlightReqSide(t *testing.T, id uint64, side string) (int, string, []byte) {
	t.Helper()
	url := "/__flightreq?id=" + strconv.FormatUint(id, 10)
	if side != "" {
		url += "&side=" + side
	}
	r := httptest.NewRequest(http.MethodGet, url, nil)
	r.RemoteAddr = "127.0.0.1:43210"
	rec := httptest.NewRecorder()
	flightReqHandler(rec, r)
	res := rec.Result()
	defer res.Body.Close()
	body, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatalf("读响应体失败: %v", err)
	}
	return res.StatusCode, res.Header.Get("X-Proxy429-Side"), body
}

// TestFlightSideEndpoints verifies the dual-link viewer endpoints: side=up/down each return their own side and the header truthfully reports the actual side;
// streams without a downstream-side record auto-fall back to the upstream side for side=down (X-Proxy429-Side: up); in-flight and finished-archive queries behave identically.
func TestFlightSideEndpoints(t *testing.T) {
	ensureFlightsMap()
	oldFinished := finished
	defer func() { finished = oldFinished }()
	finished = nil

	// A stream with both sides recorded (simulating a translation-stream shape).
	dual := &flight{id: flights.nextID.Add(1), start: time.Now()}
	dual.setReqBody([]byte(`{"up":1}`))
	dual.setReqDown([]byte(`{"down":1}`))
	dual.appendContent([]byte("UP-SSE"))
	dual.appendContentDown([]byte("DOWN-JSON"))
	flights.register(dual)
	defer flights.unregister(dual.id)

	// A stream with only the upstream side (simulating an unrewritten native-stream shape).
	upOnly := &flight{id: flights.nextID.Add(1), start: time.Now()}
	upOnly.setReqBody([]byte(`{"up":2}`))
	upOnly.appendContent([]byte("UP-ONLY"))
	flights.register(upOnly)
	defer flights.unregister(upOnly.id)

	cases := []struct {
		name     string
		id       uint64
		side     string
		wantSide string
		wantBody string
	}{
		{"双侧-缺省为up", dual.id, "", "up", "UP-SSE"},
		{"双侧-up", dual.id, "up", "up", "UP-SSE"},
		{"双侧-down", dual.id, "down", "down", "DOWN-JSON"},
		{"单侧-缺省up", upOnly.id, "", "up", "UP-ONLY"},
		{"单侧-down回退up", upOnly.id, "down", "up", "UP-ONLY"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			code, served, body := serveFlightSide(t, tc.id, tc.side, false)
			if code != http.StatusOK {
				t.Fatalf("code=%d, want 200（body=%q）", code, body)
			}
			if served != tc.wantSide {
				t.Errorf("X-Proxy429-Side=%q, want %q", served, tc.wantSide)
			}
			if string(body) != tc.wantBody {
				t.Errorf("body=%q, want %q", body, tc.wantBody)
			}
		})
	}

	// The request-body endpoint follows the same rules: each side returns its own; a down-only miss falls back to up.
	code, served, body := serveFlightReqSide(t, dual.id, "down")
	if code != 200 || served != "down" || string(body) != `{"down":1}` {
		t.Errorf("请求体 down 侧: code=%d side=%q body=%q, want 200/down/{\"down\":1}", code, served, body)
	}
	code, served, body = serveFlightReqSide(t, upOnly.id, "down")
	if code != 200 || served != "up" || string(body) != `{"up":2}` {
		t.Errorf("请求体 down 回退: code=%d side=%q body=%q, want 200/up/{\"up\":2}", code, served, body)
	}

	// After archiving to finished (removed from the in-flight table), both sides and the fallback remain queryable.
	addFinished(dual)
	addFinished(upOnly)
	flights.unregister(dual.id)
	flights.unregister(upOnly.id)
	code, served, body = serveFlightSide(t, dual.id, "down", false)
	if code != 200 || served != "down" || string(body) != "DOWN-JSON" {
		t.Errorf("归档 down 侧: code=%d side=%q body=%q, want 200/down/DOWN-JSON", code, served, body)
	}
	code, served, body = serveFlightSide(t, upOnly.id, "down", false)
	if code != 200 || served != "up" || string(body) != "UP-ONLY" {
		t.Errorf("归档 down 回退: code=%d side=%q body=%q, want 200/up/UP-ONLY", code, served, body)
	}
}

// TestFlightSideFullDownload verifies the full=1 full-output download picks the copy by side: the downstream-side copy is downloadable
// when the 储存完整结构体 toggle is on and differing content exists; without a downstream-side copy it falls back to the upstream side and says so truthfully.
func TestFlightSideFullDownload(t *testing.T) {
	ensureFlightsMap()
	defer fullStore.Store(false)
	fullStore.Store(true)

	f := &flight{id: flights.nextID.Add(1), start: time.Now()}
	f.appendContent([]byte("FULL-UP"))
	f.appendContentDown([]byte("FULL-DOWN"))
	flights.register(f)
	defer flights.unregister(f.id)

	code, served, body := serveFlightSide(t, f.id, "down", true)
	if code != 200 || served != "down" || string(body) != "FULL-DOWN" {
		t.Errorf("full down: code=%d side=%q body=%q, want 200/down/FULL-DOWN", code, served, body)
	}
	code, served, body = serveFlightSide(t, f.id, "up", true)
	if code != 200 || served != "up" || string(body) != "FULL-UP" {
		t.Errorf("full up: code=%d side=%q body=%q, want 200/up/FULL-UP", code, served, body)
	}

	// No downstream-side full copy: side=down falls back to the upstream side.
	g := &flight{id: flights.nextID.Add(1), start: time.Now()}
	g.appendContent([]byte("ONLY-UP"))
	flights.register(g)
	defer flights.unregister(g.id)
	code, served, body = serveFlightSide(t, g.id, "down", true)
	if code != 200 || served != "up" || string(body) != "ONLY-UP" {
		t.Errorf("full down 回退: code=%d side=%q body=%q, want 200/up/ONLY-UP", code, served, body)
	}
}

// TestTranslatingWriterDownTap verifies the downstream-side tap covers translatingWriter's write paths:
// the bytes written to the client in both streaming (emit per event) and non-streaming (finish one-shot JSON) modes are teed in equal measure into
// contentDown. The tap wraps dst itself, and emit/finish/finishBuffered/error writes all go through dst.Write,
// so byte-for-byte equality in both modes proves full path coverage.
func TestTranslatingWriterDownTap(t *testing.T) {
	// Streaming client: SSE events pass through the tap one by one.
	rec := httptest.NewRecorder()
	tw := newTranslatingWriter(rec, true, "gpt-5-codex", nil)
	var f flight
	tw.setDownTap(&f)
	tw.Header().Set("Content-Type", "text/event-stream")
	tw.WriteHeader(200)
	if _, err := tw.Write([]byte(testSSEAllBlocks())); err != nil {
		t.Fatalf("Write: %v", err)
	}
	tw.finish()
	if got := f.snapshotContentDown(); string(got) != rec.Body.String() {
		t.Errorf("流式 tap 与客户端实收不一致：tap=%d 字节, 客户端=%d 字节", len(got), rec.Body.Len())
	}
	if !strings.Contains(string(f.snapshotContentDown()), "response.completed") {
		t.Errorf("流式 contentDown 应含 response.completed 事件")
	}

	// Non-streaming client: the one-shot Responses JSON produced by finish also passes through the tap.
	rec2 := httptest.NewRecorder()
	tw2 := newTranslatingWriter(rec2, false, "gpt-5-codex", nil)
	var f2 flight
	tw2.setDownTap(&f2)
	tw2.Header().Set("Content-Type", "text/event-stream")
	tw2.WriteHeader(200)
	if _, err := tw2.Write([]byte(testSSEAllBlocks())); err != nil {
		t.Fatalf("Write: %v", err)
	}
	tw2.finish()
	if got := f2.snapshotContentDown(); string(got) != rec2.Body.String() {
		t.Errorf("非流式 tap 与客户端实收不一致：tap=%d 字节, 客户端=%d 字节", len(got), rec2.Body.Len())
	}
	if !strings.Contains(string(f2.snapshotContentDown()), `"object":"response"`) {
		t.Errorf("非流式 contentDown 应含一次性 Responses JSON")
	}
}

// TestDualSideTranslatedFlow end-to-end verifies dual-link recording of a translation stream: the downstream side stores the client's Responses original
// and the Responses-shaped response actually received; the upstream side stores the translated Anthropic request body and the upstream's raw SSE stream.
func TestDualSideTranslatedFlow(t *testing.T) {
	resetStats()
	oldFinished := finished
	defer func() { finished = oldFinished }()
	finished = nil

	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		io.WriteString(w, testSSEAllBlocks())
	}))
	defer mock.Close()

	cfg.Store(&Config{
		Upstream:       mock.URL,
		MaxRetries:     0,
		TotalBudgetSec: 10,
		Routes:         []RouteRule{{Pattern: "gpt-5*", URL: mock.URL, Model: "deepseek-v4-flash"}},
	})
	defer cfg.Store(&Config{})

	proxy := httptest.NewServer(http.HandlerFunc(responsesHandler))
	defer proxy.Close()

	raw := `{"model":"gpt-5-codex","input":"hi"}`
	resp, err := http.Post(proxy.URL+"/v1/responses", "application/json", strings.NewReader(raw))
	if err != nil {
		t.Fatalf("请求失败: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("状态=%d, want 200: %s", resp.StatusCode, body)
	}

	if len(finished) != 1 {
		t.Fatalf("归档流数=%d, want 1", len(finished))
	}
	ff := finished[0]
	// Downstream-side request body = the client's Responses original (verbatim); upstream side = the translated Anthropic body.
	if string(ff.reqDown) != raw {
		t.Errorf("reqDown=%q, want 客户端原文 %q", ff.reqDown, raw)
	}
	if !strings.Contains(string(ff.reqBody), `"messages"`) || !strings.Contains(string(ff.reqBody), `"deepseek-v4-flash"`) {
		t.Errorf("reqBody 应为翻译并路由改写后的 Anthropic 体: %s", ff.reqBody)
	}
	// Downstream-side response = the Responses JSON the client actually received; upstream side = the upstream Anthropic SSE raw stream.
	if !strings.Contains(string(ff.contentDown), `"object":"response"`) {
		t.Errorf("contentDown 应为 Responses JSON: %.200s", ff.contentDown)
	}
	if !strings.Contains(string(ff.content), "message_start") {
		t.Errorf("content 应为上游 Anthropic SSE: %.200s", ff.content)
	}
}

// TestDualSideDirectFlow end-to-end verifies the downstream-side recording rules of the native Anthropic port:
// when the body isn't rewritten, both sides are identical and not stored twice (reqDown/contentDown stay empty; endpoints treat them as fallback);
// only when the body is rewritten (routed model change) is reqDown stored separately.
func TestDualSideDirectFlow(t *testing.T) {
	resetStats()
	oldFinished := finished
	defer func() { finished = oldFinished }()
	finished = nil

	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		io.WriteString(w, testSSEAllBlocks())
	}))
	defer mock.Close()

	proxy := httptest.NewServer(http.HandlerFunc(handler))
	defer proxy.Close()

	cfg.Store(&Config{Upstream: mock.URL, MaxRetries: 0, TotalBudgetSec: 10})
	defer cfg.Store(&Config{})

	post := func(model string) {
		t.Helper()
		body := `{"model":"` + model + `","max_tokens":100,"messages":[{"role":"user","content":"hi"}],"stream":true}`
		resp, err := http.Post(proxy.URL+"/v1/messages", "application/json", strings.NewReader(body))
		if err != nil {
			t.Fatalf("请求失败: %v", err)
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		if resp.StatusCode != 200 {
			t.Fatalf("model=%s 状态=%d, want 200", model, resp.StatusCode)
		}
	}
	// Find the archived stream by request-body content (addFinished is newest-first; don't guess by position; finished is a value slice, so a returned pointer must come from indexing).
	findFF := func(sub string) *finishedFlight {
		for i := range finished {
			if strings.Contains(string(finished[i].reqBody), sub) {
				return &finished[i]
			}
		}
		return nil
	}

	// No route rewrite: the body passes through verbatim; identical on both sides, not stored twice.
	post("claude-x1")
	ff := findFF("claude-x1")
	if ff == nil {
		t.Fatalf("透传流未归档（finished=%d 条）", len(finished))
	}
	if ff.reqDown != nil {
		t.Errorf("同文流不应双存 reqDown: %q", ff.reqDown)
	}
	if len(ff.contentDown) != 0 {
		t.Errorf("同文流不应双存 contentDown（%d 字节）", len(ff.contentDown))
	}

	// Route-rewritten model: the body changed; the downstream side stores the client's original, the upstream side the rewritten body.
	cfg.Store(&Config{
		Upstream:       mock.URL,
		MaxRetries:     0,
		TotalBudgetSec: 10,
		Routes:         []RouteRule{{Pattern: "claude-y*", URL: mock.URL, Model: "k3-256k"}},
	})
	post("claude-y1")
	ff = findFF("k3-256k")
	if ff == nil {
		t.Fatalf("改写流未归档（finished=%d 条）", len(finished))
	}
	if ff.reqDown == nil || !strings.Contains(string(ff.reqDown), `"claude-y1"`) {
		t.Errorf("改写流 reqDown 应存客户端原文: %q", ff.reqDown)
	}
}
