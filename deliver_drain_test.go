package main

import (
	"bufio"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// SSE fixtures: a complete stream ending in message_stop, and a partial stream with no terminal event.
const drainSSEFull = "data: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":1,\"output_tokens\":1}}}\n\n" +
	"data: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"hi\"}}\n\n" +
	"data: {\"type\":\"message_stop\"}\n\n"

const drainSSEPartial = "data: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":1,\"output_tokens\":1}}}\n\n" +
	"data: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"hi\"}}\n\n"

func finishedCount() int {
	finishedMu.Lock()
	defer finishedMu.Unlock()
	return len(finished)
}

// lastFinishedStatus waits for a new finished stream beyond baseN and returns its archived status.
func lastFinishedStatus(t *testing.T, baseN int) int {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		finishedMu.Lock()
		n := len(finished)
		st := 0
		if n > baseN {
			st = finished[n-1].status
		}
		finishedMu.Unlock()
		if n > baseN {
			return st
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("流未在 3s 内归档")
	return 0
}

func drainTestProxy(t *testing.T, upstream *httptest.Server) *httptest.Server {
	t.Helper()
	cfg.Store(&Config{Upstream: upstream.URL, MaxRetries: 0, TotalBudgetSec: 10, RecentSampleWindow: 5})
	stats.resetSampleCap(5)
	proxy := httptest.NewServer(http.HandlerFunc(handler))
	t.Cleanup(proxy.Close)
	return proxy
}

// drainTestPost issues one streaming request and returns the response plus a line reader on its body.
func drainTestPost(t *testing.T, proxy *httptest.Server) (*http.Response, *bufio.Reader) {
	t.Helper()
	resp, err := http.Post(proxy.URL+"/v1/messages", "application/json",
		strings.NewReader("{\"model\":\"m\",\"max_tokens\":100,\"stream\":true,\"messages\":[]}"))
	if err != nil {
		t.Fatalf("请求失败: %v", err)
	}
	return resp, bufio.NewReader(resp.Body)
}

// readUntilLine reads lines until one carries the marker; fails on stream end first.
func readUntilLine(t *testing.T, br *bufio.Reader, marker string) {
	t.Helper()
	for {
		line, err := br.ReadString('\n')
		if strings.Contains(line, marker) {
			return
		}
		if err != nil {
			t.Fatalf("流在见到 %s 前结束: %v", marker, err)
		}
	}
}

// TestPostDeliveryDisconnectDrainsUpstream pins the Codex quirk fix: the client closes the connection right
// after the terminal event (message_stop). Delivery is complete, so the cancel must NOT propagate upstream —
// the proxy keeps draining the upstream body to EOF and the provider observes a clean completion (no 499).
func TestPostDeliveryDisconnectDrainsUpstream(t *testing.T) {
	resetStats()
	var sawCancel atomic.Bool
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		fmt.Fprint(w, drainSSEFull)
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		// The quirk window: the client has disconnected by now. If the cancel propagated upstream,
		// this request context fires; a healthy drain lets the tail timer win and the handler ends at EOF.
		select {
		case <-r.Context().Done():
			sawCancel.Store(true)
		case <-time.After(600 * time.Millisecond):
		}
	}))
	defer upstream.Close()

	proxy := drainTestProxy(t, upstream)
	baseN := finishedCount()
	resp, br := drainTestPost(t, proxy)
	readUntilLine(t, br, "message_stop")
	// writeAndCount writes downstream before the parse marks delivered; let the parse land before disconnecting.
	time.Sleep(80 * time.Millisecond)
	resp.Body.Close()

	if st := lastFinishedStatus(t, baseN); st != 200 {
		t.Errorf("已完成投递后下游才断开 status=%d, want 200", st)
	}
	if sawCancel.Load() {
		t.Error("上游观察到了完成投递后的断连（取消不应再传导，代理应读到 EOF 收尾）")
	}
}

// TestMidStreamDisconnectPropagates499 is the control: the client disconnects before any terminal event.
// The cancel MUST propagate upstream (the provider legitimately records 499) and the flight records 499.
func TestMidStreamDisconnectPropagates499(t *testing.T) {
	resetStats()
	var sawCancel atomic.Bool
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		fmt.Fprint(w, drainSSEPartial) // No message_stop: the stream is mid-flight.
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		select {
		case <-r.Context().Done():
			sawCancel.Store(true)
		case <-time.After(2 * time.Second):
		}
	}))
	defer upstream.Close()

	proxy := drainTestProxy(t, upstream)
	baseN := finishedCount()
	resp, br := drainTestPost(t, proxy)
	readUntilLine(t, br, "text_delta")
	resp.Body.Close() // Genuine mid-stream disconnect.

	if st := lastFinishedStatus(t, baseN); st != 499 {
		t.Errorf("中途断开 status=%d, want 499", st)
	}
	for i := 0; i < 100 && !sawCancel.Load(); i++ {
		time.Sleep(10 * time.Millisecond)
	}
	if !sawCancel.Load() {
		t.Error("上游未观察到中途断连（投递完成前取消必须传导）")
	}
}
