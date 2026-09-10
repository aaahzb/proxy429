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

// TestAppendContentAlignsToEventBoundary 验证超过 cap 的内容截断后，
// 开头对齐到 SSE event 边界（\n\n），不是半截 JSON 如 "ext"}}"。
func TestAppendContentAlignsToEventBoundary(t *testing.T) {
	var f flight
	// 构造标准 SSE event 块：event 行 + data 行 + 空行（约 110 字节）
	event := []byte("event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":1,\"delta\":{\"type\":\"text_delta\",\"text\":\"hello\"}}\n\n")
	// 追加足够多次超过 cap（3000×110≈330KB > 256KB），触发截断对齐
	for i := 0; i < 3000; i++ {
		f.appendContent(event)
	}
	snap := f.snapshotContent()

	// 长度不超过 cap
	if len(snap) > flightContentCap {
		t.Fatalf("content 长度 %d 超过 cap %d", len(snap), flightContentCap)
	}

	// 开头必须是完整 SSE 行（event: 或 data: 开头），不能是半截 JSON 片段
	first := string(snap)
	if len(first) > 60 {
		first = first[:60]
	}
	if !strings.HasPrefix(first, "event:") && !strings.HasPrefix(first, "data:") {
		t.Errorf("截断后开头不是完整 SSE 行: %q", first)
	}

	// 开头不应出现半截 JSON 的特征（"}} 结尾片段、孤立 "ext" 等）
	if strings.HasPrefix(first, "ext") || strings.HasPrefix(first, "\"}") {
		t.Errorf("截断后开头是半截 JSON: %q", first)
	}
}

// serveFlightReq 以本机 RemoteAddr 跑一遍 flightReqHandler。
// 参数 id：查询的流 id。返回 HTTP 状态码与响应体字节。
func serveFlightReq(t *testing.T, id uint64) (int, []byte) {
	t.Helper()
	r := httptest.NewRequest(http.MethodGet, "/__flightreq?id="+strconv.FormatUint(id, 10), nil)
	r.RemoteAddr = "127.0.0.1:43210" // httptest 默认 192.0.2.1，isLocalRequest 会拒
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

// TestFlightReqHandler 验证请求体查看端点：在途流命中、完成归档命中、未知 id 404，
// 且返回的都是请求体原文（逐字一致）。
func TestFlightReqHandler(t *testing.T) {
	want := `{"model":"x","messages":[{"role":"user","content":"你好"}]}`
	inFlight := &flight{id: flights.nextID.Add(1), start: time.Now()}
	inFlight.setReqBody([]byte(want))
	flights.register(inFlight)
	defer flights.unregister(inFlight.id)

	// 在途：注册后立即能查到请求体原文。
	code, body := serveFlightReq(t, inFlight.id)
	if code != http.StatusOK {
		t.Fatalf("在途流请求体 code=%d, want 200（body=%q）", code, body)
	}
	if string(body) != want {
		t.Errorf("在途流请求体=%q, want %q", body, want)
	}

	// 完成归档：从在途表移除后改从 finished 存档命中（与 handler 收尾顺序一致）。
	addFinished(inFlight)
	flights.unregister(inFlight.id)
	code, body = serveFlightReq(t, inFlight.id)
	if code != http.StatusOK {
		t.Fatalf("完成流请求体 code=%d, want 200（body=%q）", code, body)
	}
	if string(body) != want {
		t.Errorf("完成流请求体=%q, want %q", body, want)
	}

	// 未知 id：404（覆盖搜索子流等未记录请求体的情形）。
	code, _ = serveFlightReq(t, flights.nextID.Add(1))
	if code != http.StatusNotFound {
		t.Errorf("未知 id code=%d, want 404", code)
	}
}

// TestSetReqBodyTruncation 验证请求体超长时只留前段（cap 截断），
// 截断内容与原文字节前缀逐字节一致。
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

// postFullStore 以本机 RemoteAddr POST 开关「储存完整结构体」。
// 参数 enabled：目标开关状态。返回 HTTP 状态码。
func postFullStore(t *testing.T, enabled bool) int {
	t.Helper()
	body := strings.NewReader(`{"enabled":` + strconv.FormatBool(enabled) + `}`)
	r := httptest.NewRequest(http.MethodPost, "/__fullstore", body)
	r.RemoteAddr = "127.0.0.1:43210" // httptest 默认 192.0.2.1，isLocalRequest 会拒
	rec := httptest.NewRecorder()
	fullStoreHandler(rec, r)
	return rec.Result().StatusCode
}

// serveFlightFull 以本机 RemoteAddr 请求 /__flight?id=N&full=1（下载输出原文）。
// 参数 id：查询的流 id。返回 HTTP 状态码与响应体字节。
func serveFlightFull(t *testing.T, id uint64) (int, []byte) {
	t.Helper()
	r := httptest.NewRequest(http.MethodGet, "/__flight?id="+strconv.FormatUint(id, 10)+"&full=1", nil)
	r.RemoteAddr = "127.0.0.1:43210" // httptest 默认 192.0.2.1，isLocalRequest 会拒
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

// TestFullStoreToggle 验证「储存完整结构体」开关语义：
// 开 = 请求体不截断 + appendContent 额外记完整副本 + /__flight?full=1 可下载完整输出；
// 关 = 立即清空完整副本（reqBody 截回 cap、fullContent 置 nil）+ full=1 变 403。
func TestFullStoreToggle(t *testing.T) {
	defer fullStore.Store(false) // 复位，不影响其他用例

	// 开启：请求体超过 cap 也不截断，输出额外记完整副本。
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

	// full=1 下载：拿到两段拼接的完整输出。
	code, dl := serveFlightFull(t, f.id)
	if code != http.StatusOK {
		t.Fatalf("full=1 code=%d, want 200（body=%q）", code, dl)
	}
	want := "event: message_start\ndata: {}\n\nevent: content_block_delta\ndata: {}\n\n"
	if string(dl) != want {
		t.Errorf("full=1 下载内容=%q, want %q", dl, want)
	}

	// 关闭：清空完整副本，reqBody 截回 cap，full=1 变 403。
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

// TestSetStageColorDuration 验证状态灯持续时长（stageMs）按灯色计：
// 灯色变化（白→黄→绿）重置计时；同色内的阶段推进（路由→尝试、重试再进尝试）不重置。
func TestSetStageColorDuration(t *testing.T) {
	f := &flight{start: time.Now()}
	f.stageStart.Store(f.start.Add(-100 * time.Millisecond).UnixNano()) // 白灯已亮 100ms

	// 白→黄（路由）：灯色变化，计时重置。
	f.setStage(stageRoute)
	if ms := f.stageMs(); ms > 50 {
		t.Errorf("白→黄后 stageMs=%d, want ≈0（灯色变化应重置计时）", ms)
	}

	// 黄→黄（路由→尝试）：同色推进，计时不重置。
	time.Sleep(30 * time.Millisecond)
	before := f.stageMs()
	f.setStage(stageAttempt)
	if after := f.stageMs(); after < before {
		t.Errorf("路由→尝试同色推进不应重置：黄灯 %dms → %dms", before, after)
	}

	// 重试再进尝试（同阶段反复 setStage）：仍不重置。
	time.Sleep(20 * time.Millisecond)
	before = f.stageMs()
	f.setStage(stageAttempt)
	if after := f.stageMs(); after < before {
		t.Errorf("重试同色不应重置：黄灯 %dms → %dms", before, after)
	}

	// 黄→绿（收到响应开始转发）：灯色变化，计时重置。
	f.setStage(stageForward)
	if ms := f.stageMs(); ms > 50 {
		t.Errorf("黄→绿后 stageMs=%d, want ≈0（灯色变化应重置计时）", ms)
	}
}
