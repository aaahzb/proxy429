package proxy

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"proxy429/internal/config"
	"proxy429/internal/logview"
	"proxy429/internal/recorder"
	"strings"
	"sync"
	"testing"
	"time"
)

// TestIsCompactionRequest pins the client compaction prompts the console recognizes: the three known
// client prompts are positive, while an ordinary message and the post-compaction resume injection are not.
func TestIsCompactionRequest(t *testing.T) {
	tests := []struct {
		name string
		body string
		want bool
	}{
		{name: "claude code compact prompt", body: `{"messages":[{"role":"user","content":"CRITICAL: Respond with TEXT ONLY. Do NOT call any tools."}]}`, want: true},
		{name: "claude code legacy compact prompt", body: `{"messages":[{"role":"user","content":"Your task is to create a detailed summary of the conversation so far, paying close attention to the user's explicit requests."}]}`, want: true},
		{name: "codex checkpoint compaction", body: `{"input":[{"role":"user","content":"You are performing a CONTEXT CHECKPOINT COMPACTION. Create a handoff summary for another LLM."}]}`, want: true},
		{name: "normal message", body: `{"model":"claude-opus-4-8","messages":[{"role":"user","content":"refactor this function"}]}`, want: false},
		{name: "post-compaction resume injection", body: `{"messages":[{"role":"user","content":"This session is being continued from a previous conversation that ran out of context. The summary below covers the earlier portion."}]}`, want: false},
		{name: "empty body", body: "", want: false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := isCompactionRequest([]byte(tc.body)); got != tc.want {
				t.Errorf("isCompactionRequest(%q) = %v, want %v", tc.body, got, tc.want)
			}
		})
	}
}

const compactionRequest = `{"model":"claude-opus-4-8","stream":true,"max_tokens":32000,"messages":[{"role":"user","content":"CRITICAL: Respond with TEXT ONLY"}]}`

// compactionUpstreamSSE is a complete upstream stream whose usage yields the 170k->20k, 88% compaction result:
// message_start carries the total context (input 1000 + cache_read 169000), message_delta the summary output (20000).
const compactionUpstreamSSE = "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_compact\",\"type\":\"message\",\"role\":\"assistant\",\"model\":\"compact-model\",\"content\":[],\"usage\":{\"input_tokens\":1000,\"cache_read_input_tokens\":169000,\"cache_creation_input_tokens\":0,\"output_tokens\":1}}}\n\n" +
	"event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":20000}}\n\n" +
	"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"

// consoleMuxOnce registers the console endpoints on the default mux once per test binary.
var consoleMuxOnce sync.Once

// compactionConsoleServer serves the real console endpoints (the /__logs/data handler is unexported).
func compactionConsoleServer(t *testing.T) *httptest.Server {
	t.Helper()
	consoleMuxOnce.Do(logview.RegisterHandlers)
	srv := httptest.NewServer(http.DefaultServeMux)
	t.Cleanup(srv.Close)
	return srv
}

// TestCompactionBadgeEndToEnd drives one compaction request through the real handler with a gated upstream:
// while the request is in flight, /__logs/data must flag it with compaction:true; after the stream finishes,
// /__recentflights must flag the archived row and carry the 170k->20k, 88% result derived from the fixture usage.
func TestCompactionBadgeEndToEnd(t *testing.T) {
	resetStats()
	recorder.FinishedMu.Lock()
	oldFinished := recorder.Finished
	recorder.Finished = nil
	recorder.FinishedMu.Unlock()
	defer func() {
		recorder.FinishedMu.Lock()
		recorder.Finished = oldFinished
		recorder.FinishedMu.Unlock()
	}()
	oldCfg := config.Current()
	defer config.Store(oldCfg)

	started := make(chan struct{})
	release := make(chan struct{})
	var startOnce, releaseOnce sync.Once
	releaseGate := func() { releaseOnce.Do(func() { close(release) }) }
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.ReadAll(r.Body)
		startOnce.Do(func() { close(started) })
		<-release
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, compactionUpstreamSSE)
	}))
	defer upstream.Close()

	config.Store(&config.Config{Upstream: upstream.URL, MaxRetries: 0, TotalBudgetSec: 10, RecentSampleWindow: 5})
	recorder.Stats.ResetSampleCap(5)

	proxy := httptest.NewServer(http.HandlerFunc(handler))
	defer proxy.Close()
	console := compactionConsoleServer(t)
	// Failure paths must open the gate before any server closes, or Close waits on the blocked upstream handler forever
	// (registered last so it runs first).
	defer releaseGate()

	type clientResult struct {
		body string
		err  error
	}
	clientDone := make(chan clientResult, 1)
	go func() {
		resp, err := http.Post(proxy.URL+"/v1/messages", "application/json", strings.NewReader(compactionRequest))
		if err != nil {
			clientDone <- clientResult{err: err}
			return
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		clientDone <- clientResult{body: string(b)}
	}()

	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("the upstream never received the compaction request")
	}

	deadline := time.Now().Add(5 * time.Second)
	var inFlight string
	for {
		resp, err := http.Get(console.URL + "/__logs/data")
		if err == nil {
			b, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			inFlight = string(b)
			if strings.Contains(inFlight, `"compaction":true`) {
				break
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("in-flight JSON never exposed compaction:true; last response: %s", inFlight)
		}
		time.Sleep(20 * time.Millisecond)
	}

	releaseGate()
	var result clientResult
	select {
	case result = <-clientDone:
	case <-time.After(10 * time.Second):
		t.Fatal("the client request did not finish after releasing the upstream gate")
	}
	if result.err != nil {
		t.Fatalf("proxy request failed: %v", result.err)
	}
	if !strings.Contains(result.body, "message_stop") {
		t.Fatalf("client did not receive the upstream stream: %s", result.body)
	}

	deadline = time.Now().Add(5 * time.Second)
	var recent string
	for {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, logview.RecentFlightsPath, nil)
		req.RemoteAddr = "127.0.0.1:1"
		logview.RecentFlightsHandler(rec, req)
		recent = rec.Body.String()
		recorder.FinishedMu.Lock()
		archived := len(recorder.Finished)
		recorder.FinishedMu.Unlock()
		if archived == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the finished stream was never archived; recent response: %s", recent)
		}
		time.Sleep(20 * time.Millisecond)
	}
	var decoded struct {
		List []struct {
			Compaction  bool   `json:"compaction"`
			CompactSave string `json:"compactSave"`
		} `json:"list"`
	}
	if err := json.Unmarshal([]byte(recent), &decoded); err != nil {
		t.Fatalf("recentFlights is not JSON: %v", err)
	}
	if len(decoded.List) != 1 || !decoded.List[0].Compaction || decoded.List[0].CompactSave != "170k->20k, 88%" {
		t.Errorf("finished row = %+v, want compaction true and compactSave 170k->20k, 88%%", decoded.List)
	}
}
