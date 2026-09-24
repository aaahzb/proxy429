package main

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"
)

// TestAppendContentAlignsToEventBoundary verifies that after content is truncated past the cap,
// the head aligns to an SSE event boundary (\n\n), not half a JSON like "ext"}}".
func TestAppendContentAlignsToEventBoundary(t *testing.T) {
	var f flight
	// Build a standard SSE event block: event line + data line + blank line (~110 bytes)
	event := []byte("event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":1,\"delta\":{\"type\":\"text_delta\",\"text\":\"hello\"}}\n\n")
	// Append enough times to exceed the cap (3000×110≈330KB > 256KB), triggering truncation alignment
	for i := 0; i < 3000; i++ {
		f.appendContent(event)
	}
	snap := f.snapshotContent()

	// Length must not exceed the cap
	if len(snap) > flightContentCap {
		t.Fatalf("content 长度 %d 超过 cap %d", len(snap), flightContentCap)
	}

	// The head must be a complete SSE line (starting with event: or data:), not half a JSON fragment
	first := string(snap)
	if len(first) > 60 {
		first = first[:60]
	}
	if !strings.HasPrefix(first, "event:") && !strings.HasPrefix(first, "data:") {
		t.Errorf("截断后开头不是完整 SSE 行: %q", first)
	}

	// The head must not show half-JSON traits (a "}} tail fragment, a lone "ext", etc.)
	if strings.HasPrefix(first, "ext") || strings.HasPrefix(first, "\"}") {
		t.Errorf("截断后开头是半截 JSON: %q", first)
	}
}

// serveFlightReq runs flightReqHandler once with a localhost RemoteAddr.
// id: the stream id to query. Returns the HTTP status code and response body bytes.
func serveFlightReq(t *testing.T, id uint64) (int, []byte) {
	t.Helper()
	r := httptest.NewRequest(http.MethodGet, "/__flightreq?id="+strconv.FormatUint(id, 10), nil)
	r.RemoteAddr = "127.0.0.1:43210" // httptest defaults to 192.0.2.1, which isLocalRequest rejects
	rec := httptest.NewRecorder()
	flightReqHandler(rec, r)
	res := rec.Result()
	defer res.Body.Close()
	body, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatalf("读响应体失败: %v", err)
	}
	return res.StatusCode, body
}

// TestFlightReqHandler verifies the request-body viewer endpoint: in-flight hit, finished-archive hit, unknown id 404,
// and all returns are the verbatim request body (byte-for-byte identical).
func TestFlightReqHandler(t *testing.T) {
	want := `{"model":"x","messages":[{"role":"user","content":"你好"}]}`
	inFlight := &flight{id: flights.nextID.Add(1), start: time.Now()}
	inFlight.setReqBody([]byte(want))
	flights.register(inFlight)
	defer flights.unregister(inFlight.id)

	// In-flight: the request body is queryable immediately after registration.
	code, body := serveFlightReq(t, inFlight.id)
	if code != http.StatusOK {
		t.Fatalf("在途流请求体 code=%d, want 200（body=%q）", code, body)
	}
	if string(body) != want {
		t.Errorf("在途流请求体=%q, want %q", body, want)
	}

	// Finished archive: after removal from the in-flight table, the hit comes from the finished archive (same order as the handler's closing sequence).
	addFinished(inFlight)
	flights.unregister(inFlight.id)
	code, body = serveFlightReq(t, inFlight.id)
	if code != http.StatusOK {
		t.Fatalf("完成流请求体 code=%d, want 200（body=%q）", code, body)
	}
	if string(body) != want {
		t.Errorf("完成流请求体=%q, want %q", body, want)
	}

	// Unknown id: 404 (also covers search sub-streams and other cases with no recorded request body).
	code, _ = serveFlightReq(t, flights.nextID.Add(1))
	if code != http.StatusNotFound {
		t.Errorf("未知 id code=%d, want 404", code)
	}
}

// TestSetReqBodyTruncation verifies an over-long request body keeps only the head (cap truncation),
// and the truncated content is byte-for-byte identical to a prefix of the original.
func TestSetReqBodyTruncation(t *testing.T) {
	var f flight
	big := bytes.Repeat([]byte("a"), flightContentCap+100)
	f.setReqBody(big)
	snap := f.snapshotReqBody()
	if len(snap) != flightContentCap {
		t.Fatalf("截断后长度=%d, want %d", len(snap), flightContentCap)
	}
	if !bytes.Equal(snap, big[:flightContentCap]) {
		t.Errorf("截断内容应为原文前 %d 字节", flightContentCap)
	}
}

// postFullStore POSTs the 储存完整结构体 toggle with a localhost RemoteAddr.
// enabled: target toggle state. Returns the HTTP status code.
func postFullStore(t *testing.T, enabled bool) int {
	t.Helper()
	body := strings.NewReader(`{"enabled":` + strconv.FormatBool(enabled) + `}`)
	r := httptest.NewRequest(http.MethodPost, "/__fullstore", body)
	r.RemoteAddr = "127.0.0.1:43210" // httptest defaults to 192.0.2.1, which isLocalRequest rejects
	rec := httptest.NewRecorder()
	fullStoreHandler(rec, r)
	return rec.Result().StatusCode
}

// serveFlightFull requests /__flight?id=N&full=1 with a localhost RemoteAddr (download the raw output).
// id: the stream id to query. Returns the HTTP status code and response body bytes.
func serveFlightFull(t *testing.T, id uint64) (int, []byte) {
	t.Helper()
	r := httptest.NewRequest(http.MethodGet, "/__flight?id="+strconv.FormatUint(id, 10)+"&full=1", nil)
	r.RemoteAddr = "127.0.0.1:43210" // httptest defaults to 192.0.2.1, which isLocalRequest rejects
	rec := httptest.NewRecorder()
	flightHandler(rec, r)
	res := rec.Result()
	defer res.Body.Close()
	body, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatalf("读响应体失败: %v", err)
	}
	return res.StatusCode, body
}

// TestFullStoreToggle verifies the 储存完整结构体 toggle semantics:
// on = request body not truncated + appendContent additionally records a full copy + /__flight?full=1 can download the full output;
// off = full copies dropped immediately (reqBody cut back to cap, fullContent set to nil) + full=1 becomes 403.
func TestFullStoreToggle(t *testing.T) {
	defer fullStore.Store(false) // Reset afterwards so other cases aren't affected

	// Toggle on: request body over the cap is not truncated; output additionally records a full copy.
	if code := postFullStore(t, true); code != http.StatusOK {
		t.Fatalf("开启 code=%d, want 200", code)
	}
	f := &flight{id: flights.nextID.Add(1), start: time.Now()}
	big := bytes.Repeat([]byte("b"), flightContentCap+10)
	f.setReqBody(big)
	f.appendContent([]byte("event: message_start\ndata: {}\n\n"))
	f.appendContent([]byte("event: content_block_delta\ndata: {}\n\n"))
	flights.register(f)
	defer flights.unregister(f.id)
	if got := len(f.snapshotReqBody()); got != len(big) {
		t.Fatalf("开启时 reqBody 长度=%d, want 不截断 %d", got, len(big))
	}
	if f.reqTrunc() {
		t.Errorf("开启时 reqTrunc 应为 false（未截断）")
	}
	if !f.hasFullContent() {
		t.Errorf("开启时 hasFullContent 应为 true（appendContent 记了完整副本）")
	}

	// full=1 download: the two chunks' concatenated full output is returned.
	code, dl := serveFlightFull(t, f.id)
	if code != http.StatusOK {
		t.Fatalf("full=1 code=%d, want 200（body=%q）", code, dl)
	}
	want := "event: message_start\ndata: {}\n\nevent: content_block_delta\ndata: {}\n\n"
	if string(dl) != want {
		t.Errorf("full=1 下载内容=%q, want %q", dl, want)
	}

	// Toggle off: full copies dropped, reqBody cut back to cap, full=1 becomes 403.
	if code := postFullStore(t, false); code != http.StatusOK {
		t.Fatalf("关闭 code=%d, want 200", code)
	}
	if got := len(f.snapshotReqBody()); got != flightContentCap {
		t.Errorf("关闭后 reqBody 长度=%d, want 截回 %d", got, flightContentCap)
	}
	if !f.reqTrunc() {
		t.Errorf("关闭后 reqTrunc 应为 true（purge 截断置标，网页据此置灰下载按钮）")
	}
	if f.hasFullContent() {
		t.Errorf("关闭后 hasFullContent 应为 false")
	}
	code, _ = serveFlightFull(t, f.id)
	if code != http.StatusForbidden {
		t.Errorf("关闭后 full=1 code=%d, want 403", code)
	}
}

// TestSetStageColorDuration verifies the status-light duration (stageMs) counts per light color:
// a color change (white→yellow→green) resets the timer; stage advances within the same color (routing→attempt, retry re-entering attempt) don't.
func TestSetStageColorDuration(t *testing.T) {
	f := &flight{start: time.Now()}
	f.stageStart.Store(f.start.Add(-100 * time.Millisecond).UnixNano()) // White light has been on for 100ms

	// White→yellow (routing): color changed, timer resets.
	f.setStage(stageRoute)
	if ms := f.stageMs(); ms > 50 {
		t.Errorf("白→黄后 stageMs=%d, want ≈0（灯色变化应重置计时）", ms)
	}

	// Yellow→yellow (routing→attempt): same-color advance, timer not reset.
	time.Sleep(30 * time.Millisecond)
	before := f.stageMs()
	f.setStage(stageAttempt)
	if after := f.stageMs(); after < before {
		t.Errorf("路由→尝试同色推进不应重置：黄灯 %dms → %dms", before, after)
	}

	// Retry re-entering attempt (repeated setStage on the same stage): still not reset.
	time.Sleep(20 * time.Millisecond)
	before = f.stageMs()
	f.setStage(stageAttempt)
	if after := f.stageMs(); after < before {
		t.Errorf("重试同色不应重置：黄灯 %dms → %dms", before, after)
	}

	// Yellow→green (response received, forwarding): color changed, timer resets.
	f.setStage(stageForward)
	if ms := f.stageMs(); ms > 50 {
		t.Errorf("黄→绿后 stageMs=%d, want ≈0（灯色变化应重置计时）", ms)
	}
}

// TestFullStoreMidStreamToggleNotFull verifies a full output copy is only claimed when recording covered the
// stream from its first byte: opening the 储存完整结构体 toggle mid-stream (or flapping it, leaving a gap) must not
// produce a "full" copy — otherwise the download button and the interactive tree's full=1 preference would serve
// a head-less tail as the complete output (the viewer tooltip/404 already call this case "started before the switch was on").
func TestFullStoreMidStreamToggleNotFull(t *testing.T) {
	defer fullStore.Store(false)

	flights.mu.Lock()
	if flights.m == nil {
		flights.m = make(map[uint64]*flight)
	}
	flights.mu.Unlock()

	// Mid-stream toggle-on: the head flowed with the switch off → no full copy on either side.
	postFullStore(t, false)
	f := &flight{id: flights.nextID.Add(1), start: time.Now()}
	f.appendContent([]byte("event: message_start\ndata: {}\n\n"))
	f.appendContentDown([]byte("event: message_start\ndata: {}\n\n"))
	postFullStore(t, true) // the user opens the switch only now
	f.appendContent([]byte("event: content_block_delta\ndata: {}\n\n"))
	f.appendContentDown([]byte("event: content_block_delta\ndata: {}\n\n"))
	if f.hasFullContent() {
		t.Errorf("中途开启后 hasFullContent 应为 false（缺头的半截副本不能算完整输出）")
	}
	if f.hasFullContentDown() {
		t.Errorf("中途开启后 hasFullContentDown 应为 false（缺头的半截副本不能算完整输出）")
	}
	if got := f.snapshotFullContent(); got != nil {
		t.Errorf("中途开启后 snapshotFullContent 应为 nil, got %d 字节", len(got))
	}
	if got := f.snapshotFullContentDown(); got != nil {
		t.Errorf("中途开启后 snapshotFullContentDown 应为 nil, got %d 字节", len(got))
	}
	// The capped on-screen copies are unaffected — the viewer keeps the complete 256KB copy to view and interact with.
	if !bytes.Contains(f.snapshotContent(), []byte("message_start")) {
		t.Errorf("截断副本应仍含头部 message_start: %q", f.snapshotContent())
	}
	if !bytes.Contains(f.snapshotContentDown(), []byte("message_start")) {
		t.Errorf("下游侧截断副本应仍含头部 message_start: %q", f.snapshotContentDown())
	}
	// The download endpoint must answer 404 for such a stream, not serve the tail as "full".
	flights.register(f)
	code, dl := serveFlightFull(t, f.id)
	if code != http.StatusNotFound {
		t.Errorf("中途开启流的 full=1 应为 404, got %d（body=%q）", code, dl)
	}
	flights.unregister(f.id)

	// Off-and-on flapping leaves a gap mid-copy → likewise not a full copy.
	postFullStore(t, true)
	g := &flight{id: flights.nextID.Add(1), start: time.Now()}
	g.appendContent([]byte("event: message_start\ndata: {}\n\n"))
	postFullStore(t, false)
	g.appendContent([]byte("event: content_block_delta\ndata: {}\n\n"))
	postFullStore(t, true)
	g.appendContent([]byte("event: message_stop\ndata: {}\n\n"))
	if g.hasFullContent() {
		t.Errorf("关-开拨动留下缺口后 hasFullContent 应为 false（有缺口的副本不能算完整输出）")
	}

	// Control: the switch on before the stream's first byte still records a complete full copy.
	postFullStore(t, true)
	h := &flight{id: flights.nextID.Add(1), start: time.Now()}
	h.appendContent([]byte("event: message_start\ndata: {}\n\n"))
	h.appendContent([]byte("event: message_stop\ndata: {}\n\n"))
	if !h.hasFullContent() {
		t.Errorf("开关先于流开启时 hasFullContent 应为 true（正常路径不受标志位影响）")
	}
}
