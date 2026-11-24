package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"proxy429/internal/config"
	"proxy429/internal/recorder"
	"proxy429/internal/translate"
	"strconv"
	"strings"
	"testing"
	"time"
)

// ensureFlightsMap initializes the global in-flight table (the real startup path builds it in main; unit tests run
// back-to-back in file order within one process, and this file sorts before the other table-building cases, so it must self-build idempotently).
func ensureFlightsMap() {
	recorder.Flights.Mu.Lock()
	if recorder.Flights.M == nil {
		recorder.Flights.M = make(map[uint64]*recorder.Flight)
	}
	recorder.Flights.Mu.Unlock()
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
	oldFinished := recorder.Finished
	defer func() { recorder.Finished = oldFinished }()
	recorder.Finished = nil

	// A stream with both sides recorded (simulating a translation-stream shape).
	dual := &recorder.Flight{ID: recorder.Flights.NextID.Add(1), Start: time.Now()}
	dual.SetReqBody([]byte(`{"up":1}`))
	dual.SetReqDown([]byte(`{"down":1}`))
	dual.AppendContent([]byte("UP-SSE"))
	dual.AppendContentDown([]byte("DOWN-JSON"))
	recorder.Flights.Register(dual)
	defer recorder.Flights.Unregister(dual.ID)

	// A stream with only the upstream side (simulating an unrewritten native-stream shape).
	upOnly := &recorder.Flight{ID: recorder.Flights.NextID.Add(1), Start: time.Now()}
	upOnly.SetReqBody([]byte(`{"up":2}`))
	upOnly.AppendContent([]byte("UP-ONLY"))
	recorder.Flights.Register(upOnly)
	defer recorder.Flights.Unregister(upOnly.ID)

	cases := []struct {
		name     string
		id       uint64
		side     string
		wantSide string
		wantBody string
	}{
		{"双侧-缺省为up", dual.ID, "", "up", "UP-SSE"},
		{"双侧-up", dual.ID, "up", "up", "UP-SSE"},
		{"双侧-down", dual.ID, "down", "down", "DOWN-JSON"},
		{"单侧-缺省up", upOnly.ID, "", "up", "UP-ONLY"},
		{"单侧-down回退up", upOnly.ID, "down", "up", "UP-ONLY"},
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
	code, served, body := serveFlightReqSide(t, dual.ID, "down")
	if code != 200 || served != "down" || string(body) != `{"down":1}` {
		t.Errorf("请求体 down 侧: code=%d side=%q body=%q, want 200/down/{\"down\":1}", code, served, body)
	}
	code, served, body = serveFlightReqSide(t, upOnly.ID, "down")
	if code != 200 || served != "up" || string(body) != `{"up":2}` {
		t.Errorf("请求体 down 回退: code=%d side=%q body=%q, want 200/up/{\"up\":2}", code, served, body)
	}

	// After archiving to finished (removed from the in-flight table), both sides and the fallback remain queryable.
	recorder.AddFinished(dual)
	recorder.AddFinished(upOnly)
	recorder.Flights.Unregister(dual.ID)
	recorder.Flights.Unregister(upOnly.ID)
	code, served, body = serveFlightSide(t, dual.ID, "down", false)
	if code != 200 || served != "down" || string(body) != "DOWN-JSON" {
		t.Errorf("归档 down 侧: code=%d side=%q body=%q, want 200/down/DOWN-JSON", code, served, body)
	}
	code, served, body = serveFlightSide(t, upOnly.ID, "down", false)
	if code != 200 || served != "up" || string(body) != "UP-ONLY" {
		t.Errorf("归档 down 回退: code=%d side=%q body=%q, want 200/up/UP-ONLY", code, served, body)
	}
}

// TestFlightSideFullDownload verifies the full=1 full-output download picks the copy by side: the downstream-side copy is downloadable
// when the 储存完整结构体 toggle is on and differing content exists; without a downstream-side copy it falls back to the upstream side and says so truthfully.
func TestFlightSideFullDownload(t *testing.T) {
	ensureFlightsMap()
	defer recorder.FullStore.Store(false)
	recorder.FullStore.Store(true)

	f := &recorder.Flight{ID: recorder.Flights.NextID.Add(1), Start: time.Now()}
	f.AppendContent([]byte("FULL-UP"))
	f.AppendContentDown([]byte("FULL-DOWN"))
	recorder.Flights.Register(f)
	defer recorder.Flights.Unregister(f.ID)

	code, served, body := serveFlightSide(t, f.ID, "down", true)
	if code != 200 || served != "down" || string(body) != "FULL-DOWN" {
		t.Errorf("full down: code=%d side=%q body=%q, want 200/down/FULL-DOWN", code, served, body)
	}
	code, served, body = serveFlightSide(t, f.ID, "up", true)
	if code != 200 || served != "up" || string(body) != "FULL-UP" {
		t.Errorf("full up: code=%d side=%q body=%q, want 200/up/FULL-UP", code, served, body)
	}

	// No downstream-side full copy: side=down falls back to the upstream side.
	g := &recorder.Flight{ID: recorder.Flights.NextID.Add(1), Start: time.Now()}
	g.AppendContent([]byte("ONLY-UP"))
	recorder.Flights.Register(g)
	defer recorder.Flights.Unregister(g.ID)
	code, served, body = serveFlightSide(t, g.ID, "down", true)
	if code != 200 || served != "up" || string(body) != "ONLY-UP" {
		t.Errorf("full down 回退: code=%d side=%q body=%q, want 200/up/ONLY-UP", code, served, body)
	}
}

// TestDualSideTranslatedFlow end-to-end verifies dual-link recording of a translation stream: the downstream side stores the client's Responses original
// and the Responses-shaped response actually received; the upstream side stores the translated Anthropic request body and the upstream's raw SSE stream.
func TestDualSideTranslatedFlow(t *testing.T) {
	resetStats()
	oldFinished := recorder.Finished
	defer func() { recorder.Finished = oldFinished }()
	recorder.Finished = nil

	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		io.WriteString(w, testSSEAllBlocks())
	}))
	defer mock.Close()

	config.Store(&config.Config{
		Upstream:       mock.URL,
		MaxRetries:     0,
		TotalBudgetSec: 10,
		Routes:         []config.RouteRule{{Pattern: "gpt-5*", URL: mock.URL, Model: "deepseek-v4-flash"}},
	})
	defer config.Store(&config.Config{})

	proxy := httptest.NewServer(http.HandlerFunc(translate.ResponsesHandler(handler)))
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

	if len(recorder.Finished) != 1 {
		t.Fatalf("归档流数=%d, want 1", len(recorder.Finished))
	}
	ff := recorder.Finished[0]
	// Downstream-side request body = the client's Responses original (verbatim); upstream side = the translated Anthropic body.
	if string(ff.ReqDown) != raw {
		t.Errorf("reqDown=%q, want 客户端原文 %q", ff.ReqDown, raw)
	}
	if !strings.Contains(string(ff.ReqBody), `"messages"`) || !strings.Contains(string(ff.ReqBody), `"deepseek-v4-flash"`) {
		t.Errorf("reqBody 应为翻译并路由改写后的 Anthropic 体: %s", ff.ReqBody)
	}
	// Downstream-side response = the Responses JSON the client actually received; upstream side = the upstream Anthropic SSE raw stream.
	if !strings.Contains(string(ff.ContentDown), `"object":"response"`) {
		t.Errorf("contentDown 应为 Responses JSON: %.200s", ff.ContentDown)
	}
	if !strings.Contains(string(ff.Content), "message_start") {
		t.Errorf("content 应为上游 Anthropic SSE: %.200s", ff.Content)
	}
	if !strings.Contains(string(ff.Content), `"model":"deepseek-v4-flash"`) {
		t.Errorf("content 应保留上游原始模型名（未经写回）: %.200s", ff.Content)
	}
}

// TestDualSideDirectFlow end-to-end verifies the downstream-side recording rules of the native Anthropic port:
// when the body isn't rewritten, both sides are identical and not stored twice (reqDown/contentDown stay empty; endpoints treat them as fallback);
// only when the body is rewritten (routed model change) is reqDown stored separately.
func TestDualSideDirectFlow(t *testing.T) {
	resetStats()
	oldFinished := recorder.Finished
	defer func() { recorder.Finished = oldFinished }()
	recorder.Finished = nil

	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		io.WriteString(w, testSSEAllBlocks())
	}))
	defer mock.Close()

	proxy := httptest.NewServer(http.HandlerFunc(handler))
	defer proxy.Close()

	config.Store(&config.Config{Upstream: mock.URL, MaxRetries: 0, TotalBudgetSec: 10})
	defer config.Store(&config.Config{})

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
	findFF := func(sub string) *recorder.FinishedFlight {
		for i := range recorder.Finished {
			if strings.Contains(string(recorder.Finished[i].ReqBody), sub) {
				return &recorder.Finished[i]
			}
		}
		return nil
	}

	// No route rewrite: the body passes through verbatim; identical on both sides, not stored twice.
	post("claude-x1")
	ff := findFF("claude-x1")
	if ff == nil {
		t.Fatalf("透传流未归档（finished=%d 条）", len(recorder.Finished))
	}
	if ff.ReqDown != nil {
		t.Errorf("同文流不应双存 reqDown: %q", ff.ReqDown)
	}
	if len(ff.ContentDown) != 0 {
		t.Errorf("同文流不应双存 contentDown（%d 字节）", len(ff.ContentDown))
	}

	if ff.ContentDown != nil {
		t.Errorf("同文流 contentDown 应为 nil（空非 nil 切片会让 respDown 旗标说谎）: %#v", ff.ContentDown)
	}
	// Route-rewritten model: the body changed; the downstream side stores the client's original, the upstream side the rewritten body.
	config.Store(&config.Config{
		Upstream:       mock.URL,
		MaxRetries:     0,
		TotalBudgetSec: 10,
		Routes:         []config.RouteRule{{Pattern: "claude-y*", URL: mock.URL, Model: "k3-256k"}},
	})
	post("claude-y1")
	ff = findFF("k3-256k")
	if ff == nil {
		t.Fatalf("改写流未归档（finished=%d 条）", len(recorder.Finished))
	}
	if ff.ReqDown == nil || !strings.Contains(string(ff.ReqDown), `"claude-y1"`) {
		t.Errorf("改写流 reqDown 应存客户端原文: %q", ff.ReqDown)
	}
}

// TestDualSideModelWriteBack verifies the response-side recordings of a native stream whose model was route-rewritten:
// the upstream side must hold the upstream's exact raw bytes (its own model name intact, before write-back), and the
// downstream side must hold what the client actually received (the model written back to the client's original) —
// so the /__flight side=down endpoint serves a real downstream copy (X-Proxy429-Side: down) instead of falling back.
func TestDualSideModelWriteBack(t *testing.T) {
	resetStats()
	ensureFlightsMap()
	oldFinished := recorder.Finished
	defer func() { recorder.Finished = oldFinished }()
	recorder.Finished = nil

	// The upstream's SSE carries its own model name in message_start (testSSEAllBlocks: deepseek-v4-flash),
	// which the proxy must write back to the client's original model because the route rewrote the request model.
	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		io.WriteString(w, testSSEAllBlocks())
	}))
	defer mock.Close()

	config.Store(&config.Config{
		Upstream:       mock.URL,
		MaxRetries:     0,
		TotalBudgetSec: 10,
		Routes:         []config.RouteRule{{Pattern: "claude-y*", URL: mock.URL, Model: "k3-256k"}},
	})
	defer config.Store(&config.Config{})

	proxy := httptest.NewServer(http.HandlerFunc(handler))
	defer proxy.Close()

	body := `{"model":"claude-y1","max_tokens":100,"messages":[{"role":"user","content":"hi"}],"stream":true}`
	resp, err := http.Post(proxy.URL+"/v1/messages", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("请求失败: %v", err)
	}
	got, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("状态=%d, want 200: %s", resp.StatusCode, got)
	}
	// The client must see its own model name written back, never the upstream's.
	if !strings.Contains(string(got), `"model":"claude-y1"`) || strings.Contains(string(got), "deepseek-v4-flash") {
		t.Errorf("客户端实收应写回 claude-y1 且不含上游模型名: %.200s", got)
	}

	if len(recorder.Finished) != 1 {
		t.Fatalf("归档流数=%d, want 1", len(recorder.Finished))
	}
	ff := &recorder.Finished[0]
	// Upstream side: the exact bytes the upstream sent — its own model name intact, no written-back name anywhere.
	if !strings.Contains(string(ff.Content), `"model":"deepseek-v4-flash"`) {
		t.Errorf("上游侧应保留上游原始模型名 deepseek-v4-flash: %.200s", ff.Content)
	}
	if strings.Contains(string(ff.Content), "claude-y1") {
		t.Errorf("上游侧不应含写回后的客户端模型名: %.200s", ff.Content)
	}
	// Downstream side: the bytes the client actually received — model written back, upstream name gone.
	if len(ff.ContentDown) == 0 {
		t.Fatalf("模型写回流的下游侧应单独记录响应（contentDown 为空）")
	}
	if !strings.Contains(string(ff.ContentDown), `"model":"claude-y1"`) || strings.Contains(string(ff.ContentDown), "deepseek-v4-flash") {
		t.Errorf("下游侧应为写回后的客户端实收: %.200s", ff.ContentDown)
	}
	// The side endpoint must serve the downstream side truthfully (no silent fallback to up).
	code, served, sbody := serveFlightSide(t, ff.ID, "down", false)
	if code != 200 || served != "down" || !strings.Contains(string(sbody), `"model":"claude-y1"`) {
		t.Errorf("side=down: code=%d side=%q, want 200/down（含写回模型名）body=%.120s", code, served, sbody)
	}
}
