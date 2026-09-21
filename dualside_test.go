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

// ensureFlightsMap 初始化全局在途流表（真实启动路径由 main 建表；单测同进程按文件序
// 连跑，本文件字母序早于其他建表用例，须自建幂等）。
func ensureFlightsMap() {
	flights.mu.Lock()
	if flights.m == nil {
		flights.m = make(map[uint64]*flight)
	}
	flights.mu.Unlock()
}

// serveFlightSide 以本机 RemoteAddr 请求 /__flight?id=N[&full=1][&side=S]，
// 返回 HTTP 状态码、X-Proxy429-Side 响应头（服务端实际返回的链路侧）与响应体字节。
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
	r.RemoteAddr = "127.0.0.1:43210" // httptest 默认 192.0.2.1，isLocalRequest 会拒
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

// serveFlightReqSide 同上，打 /__flightreq（请求体查看端点）。
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

// TestFlightSideEndpoints 验证双链路查看端点：side=up/down 各回各侧且头部如实告知实际侧；
// 下游侧无记录的流 side=down 自动回退上游侧（X-Proxy429-Side: up）；在途与完成归档两查一致。
func TestFlightSideEndpoints(t *testing.T) {
	ensureFlightsMap()
	oldFinished := finished
	defer func() { finished = oldFinished }()
	finished = nil

	// 双侧都有记录的流（模拟翻译流形态）。
	dual := &flight{id: flights.nextID.Add(1), start: time.Now()}
	dual.setReqBody([]byte(`{"up":1}`))
	dual.setReqDown([]byte(`{"down":1}`))
	dual.appendContent([]byte("UP-SSE"))
	dual.appendContentDown([]byte("DOWN-JSON"))
	flights.register(dual)
	defer flights.unregister(dual.id)

	// 只有上游侧的流（模拟未被改写的原生流形态）。
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

	// 请求体端点同规则：双侧各回各侧，单侧 down 回退 up。
	code, served, body := serveFlightReqSide(t, dual.id, "down")
	if code != 200 || served != "down" || string(body) != `{"down":1}` {
		t.Errorf("请求体 down 侧: code=%d side=%q body=%q, want 200/down/{\"down\":1}", code, served, body)
	}
	code, served, body = serveFlightReqSide(t, upOnly.id, "down")
	if code != 200 || served != "up" || string(body) != `{"up":2}` {
		t.Errorf("请求体 down 回退: code=%d side=%q body=%q, want 200/up/{\"up\":2}", code, served, body)
	}

	// 完成归档后（从在途表移除）两侧与回退依旧可查。
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

// TestFlightSideFullDownload 验证 full=1 完整输出下载按侧取副本：下游侧副本在
// 「储存完整结构体」开启且有差异内容时可下；无下游侧副本时回退上游侧并如实告知。
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

	// 无下游侧完整副本：side=down 回退到上游侧。
	g := &flight{id: flights.nextID.Add(1), start: time.Now()}
	g.appendContent([]byte("ONLY-UP"))
	flights.register(g)
	defer flights.unregister(g.id)
	code, served, body = serveFlightSide(t, g.id, "down", true)
	if code != 200 || served != "up" || string(body) != "ONLY-UP" {
		t.Errorf("full down 回退: code=%d side=%q body=%q, want 200/up/ONLY-UP", code, served, body)
	}
}

// TestTranslatingWriterDownTap 验证下游侧 tap 覆盖 translatingWriter 的写出路径：
// 流式（emit 逐事件）与非流式（finish 一次性 JSON）写进客户端的字节都被等量 tee 进
// contentDown。tap 包装的是 dst 本身，emit/finish/finishBuffered/错误写出都经 dst.Write，
// 两种模式下的逐字节相等即证明全路径覆盖。
func TestTranslatingWriterDownTap(t *testing.T) {
	// 流式客户端：SSE 事件逐条过 tap。
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

	// 非流式客户端：finish 输出的一次性 Responses JSON 同样过 tap。
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

// TestDualSideTranslatedFlow 端到端验证翻译流双链路记录：下游侧存客户端 Responses 原文
// 与实收的 Responses 形态回传；上游侧存翻译后的 Anthropic 请求体与上游 SSE 原始流。
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
	// 下游侧请求体 = 客户端 Responses 原文（逐字）；上游侧 = 翻译后的 Anthropic 体。
	if string(ff.reqDown) != raw {
		t.Errorf("reqDown=%q, want 客户端原文 %q", ff.reqDown, raw)
	}
	if !strings.Contains(string(ff.reqBody), `"messages"`) || !strings.Contains(string(ff.reqBody), `"deepseek-v4-flash"`) {
		t.Errorf("reqBody 应为翻译并路由改写后的 Anthropic 体: %s", ff.reqBody)
	}
	// 下游侧回传 = 客户端实收的 Responses JSON；上游侧 = 上游 Anthropic SSE 原始流。
	if !strings.Contains(string(ff.contentDown), `"object":"response"`) {
		t.Errorf("contentDown 应为 Responses JSON: %.200s", ff.contentDown)
	}
	if !strings.Contains(string(ff.content), "message_start") {
		t.Errorf("content 应为上游 Anthropic SSE: %.200s", ff.content)
	}
}

// TestDualSideDirectFlow 端到端验证原生 Anthropic 口的下游侧记录规则：
// 体未被改写时两侧同文不双存（reqDown/contentDown 为空，端点按回退处理）；
// 体被改写（路由改模型）时才单独存 reqDown。
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
	// 按请求体内容找归档流（addFinished 最新在前，不按位置猜；finished 是值切片，返回指针须取下标）。
	findFF := func(sub string) *finishedFlight {
		for i := range finished {
			if strings.Contains(string(finished[i].reqBody), sub) {
				return &finished[i]
			}
		}
		return nil
	}

	// 无路由改写：体原样透传，两侧同文不双存。
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

	// 路由改写模型：体被改动，下游侧存客户端原文，上游侧是改写后体。
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
