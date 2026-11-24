package proxy

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"proxy429/internal/config"
	"proxy429/internal/logview"
)

// TestNonRetryableStatusDoesNotBurnBudget locks the fast-fail contract for deterministic upstream rejections:
// a non-200 status outside retry_status_codes passes through immediately — one upstream attempt, the error body
// intact — instead of being retried like an in-body 200 SSE error until the whole budget is gone (which froze
// clients for minutes before the same error surfaced).
func TestNonRetryableStatusDoesNotBurnBudget(t *testing.T) {
	resetStats()
	var hits atomic.Int32
	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		io.WriteString(w, `{"type":"error","error":{"type":"invalid_request_error","message":"The content[].thinking in the thinking mode must be passed back to the API."}}`)
	}))
	defer mock.Close()
	config.Store(&config.Config{
		Upstream:           "http://no-default.example.com",
		MaxRetries:         10,
		TotalBudgetSec:     180,
		BaseDelaySec:       0.5, // Real delays: the old retry-storm behavior waited >= 0.5s per retry, so the <5s assertion below is meaningful
		MaxDelaySec:        2,
		PingIntervalSec:    0.03, // ParseConfig would default this to 5s; a bare struct literal must set it or the retry ticker panics
		RecentSampleWindow: 5,
		Routes:             []config.RouteRule{{Pattern: "*", URL: mock.URL, API: "sk-x"}},
	})
	defer config.Store(&config.Config{})

	proxy := httptest.NewServer(http.HandlerFunc(handler))
	defer proxy.Close()
	start := time.Now()
	resp, err := http.Post(proxy.URL+"/v1/messages", "application/json", strings.NewReader(`{"model":"m","messages":[]}`))
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Errorf("a deterministic 400 must pass through immediately, took %v of a 180s budget", elapsed)
	}
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("client status = %d, want 400", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "application/json" {
		t.Errorf("the upstream Content-Type must be copied, got %q", ct)
	}
	if !strings.Contains(string(body), "invalid_request_error") {
		t.Errorf("the upstream error body must reach the client, got: %s", body)
	}
	if got := hits.Load(); got != 1 {
		t.Errorf("upstream should be hit exactly once, got %d", got)
	}
}

// TestRetryableStatusStillRetries is the control: a status on retry_status_codes keeps the old retry behavior
// (the fast-fail guard must not swallow real retryables).
func TestRetryableStatusStillRetries(t *testing.T) {
	resetStats()
	var hits atomic.Int32
	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusTooManyRequests)
		io.WriteString(w, `{"type":"error","error":{"type":"rate_limit_error","message":"slow down"}}`)
	}))
	defer mock.Close()
	config.Store(&config.Config{
		Upstream:           "http://no-default.example.com",
		MaxRetries:         1,
		TotalBudgetSec:     10,
		BaseDelaySec:       0.01,
		MaxDelaySec:        0.02,
		RetryStatusCodes:   []int{429},
		PingIntervalSec:    0.03, // ParseConfig would default this to 5s; a bare struct literal must set it or the retry ticker panics
		RecentSampleWindow: 5,
		Routes:             []config.RouteRule{{Pattern: "*", URL: mock.URL, API: "sk-x"}},
	})
	defer config.Store(&config.Config{})

	proxy := httptest.NewServer(http.HandlerFunc(handler))
	defer proxy.Close()
	resp, err := http.Post(proxy.URL+"/v1/messages", "application/json", strings.NewReader(`{"model":"m","messages":[]}`))
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if got := hits.Load(); got != 2 {
		t.Errorf("a retryable 429 should be retried once (2 attempts), got %d", got)
	}
	// The first retry opens the SSE keepalive stream (status 200 + pings), so exhaustion reports the 429 as an SSE
	// error event rather than a bare status; the client must still learn the final upstream verdict.
	if !strings.Contains(string(body), "upstream status 429") {
		t.Errorf("the exhausted 429 must surface in the SSE error, got status %d body: %s", resp.StatusCode, body)
	}
}

// TestOverloaded529ErrorBodyStillRetried pins the scope of the fast-fail guard to deterministic 4xx statuses:
// a 5xx outside retry_status_codes (Anthropic's 529 overloaded_error is the real-world case) hid an error in
// its body and used to be retried by Case B; it must keep that retry.
func TestOverloaded529ErrorBodyStillRetried(t *testing.T) {
	resetStats()
	var hits atomic.Int32
	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(529)
		io.WriteString(w, `{"type":"error","error":{"type":"overloaded_error","message":"Overloaded"}}`)
	}))
	defer mock.Close()
	config.Store(&config.Config{
		Upstream:           "http://no-default.example.com",
		MaxRetries:         1,
		TotalBudgetSec:     10,
		BaseDelaySec:       0.01,
		MaxDelaySec:        0.02,
		RetryStatusCodes:   []int{429, 500, 502, 503, 504}, // 529 deliberately absent: the in-body retry must still cover it
		PingIntervalSec:    0.03,
		RecentSampleWindow: 5,
		Routes:             []config.RouteRule{{Pattern: "*", URL: mock.URL, API: "sk-x"}},
	})
	defer config.Store(&config.Config{})

	proxy := httptest.NewServer(http.HandlerFunc(handler))
	defer proxy.Close()
	resp, err := http.Post(proxy.URL+"/v1/messages", "application/json", strings.NewReader(`{"model":"m","messages":[]}`))
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	io.ReadAll(resp.Body)
	resp.Body.Close()
	if got := hits.Load(); got != 2 {
		t.Errorf("an overloaded 529 error body must still be retried (2 attempts), got %d", got)
	}
}

// TestTransient429ErrorBodyRetriedWithoutStatusList covers the 429 exception in deterministic4xx: with an empty
// retry_status_codes the status-only path retries nothing, but a 429 whose body hides an error is transient and
// must keep the pre-existing in-body retry (the same applies to 408/425, not exercised here).
func TestTransient429ErrorBodyRetriedWithoutStatusList(t *testing.T) {
	resetStats()
	var hits atomic.Int32
	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusTooManyRequests)
		io.WriteString(w, `{"type":"error","error":{"type":"rate_limit_error","message":"slow down"}}`)
	}))
	defer mock.Close()
	config.Store(&config.Config{
		Upstream:           "http://no-default.example.com",
		MaxRetries:         1,
		TotalBudgetSec:     10,
		BaseDelaySec:       0.01,
		MaxDelaySec:        0.02,
		PingIntervalSec:    0.03,
		RecentSampleWindow: 5,
		Routes:             []config.RouteRule{{Pattern: "*", URL: mock.URL, API: "sk-x"}},
	})
	defer config.Store(&config.Config{})

	proxy := httptest.NewServer(http.HandlerFunc(handler))
	defer proxy.Close()
	resp, err := http.Post(proxy.URL+"/v1/messages", "application/json", strings.NewReader(`{"model":"m","messages":[]}`))
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	io.ReadAll(resp.Body)
	resp.Body.Close()
	if got := hits.Load(); got != 2 {
		t.Errorf("a 429 error body must still be retried without retry_status_codes (2 attempts), got %d", got)
	}
}

// TestFastFailAfterKeepaliveRecordsUpstreamStatus covers the headersSent branch of the fast-fail guard: a listed
// 429 opens the SSE keepalive stream, then the next attempt returns a deterministic 400. The client must get one
// SSE error event carrying the upstream detail, and the finished flight must record the real status 400 rather
// than the [重试尽] (retries exhausted) flag, which would misdescribe what happened.
func TestFastFailAfterKeepaliveRecordsUpstreamStatus(t *testing.T) {
	resetStats()
	var hits atomic.Int32
	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if hits.Add(1) == 1 {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusTooManyRequests)
			io.WriteString(w, `{"type":"error","error":{"type":"rate_limit_error","message":"slow down"}}`)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		io.WriteString(w, `{"type":"error","error":{"type":"invalid_request_error","message":"bad thinking history"}}`)
	}))
	defer mock.Close()
	config.Store(&config.Config{
		Upstream:           "http://no-default.example.com",
		MaxRetries:         5,
		TotalBudgetSec:     10,
		BaseDelaySec:       0.01,
		MaxDelaySec:        0.02,
		RetryStatusCodes:   []int{429},
		PingIntervalSec:    0.03,
		RecentSampleWindow: 5,
		Routes:             []config.RouteRule{{Pattern: "*", URL: mock.URL, API: "sk-x"}},
	})
	defer config.Store(&config.Config{})

	proxy := httptest.NewServer(http.HandlerFunc(handler))
	defer proxy.Close()
	resp, err := http.Post(proxy.URL+"/v1/messages", "application/json", strings.NewReader(`{"model":"keepalive-400","messages":[]}`))
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if got := hits.Load(); got != 2 {
		t.Fatalf("upstream attempts = %d, want 2 (429 retried, 400 fast-failed)", got)
	}
	text := string(body)
	if !strings.Contains(text, "upstream status 400") || !strings.Contains(text, "invalid_request_error") {
		t.Errorf("the SSE error must carry the upstream verdict and detail, got: %s", text)
	}
	if n := strings.Count(text, "event: error"); n != 1 {
		t.Errorf("the client must get exactly one SSE error event, got %d: %s", n, text)
	}
	row := recentRowWithModel(t, "keepalive-400")
	if status, _ := row["status"].(float64); status != 400 {
		t.Errorf("the finished flight must record the real upstream status 400, got %v", row["status"])
	}
	if gaveUp, _ := row["gaveUp"].(bool); gaveUp {
		t.Errorf("a fast-failed deterministic 4xx is not a spent retry budget; the row must not claim [retries exhausted]")
	}
}

// recentRowWithModel polls the recent-flights API (flights are archived asynchronously as they end; the API is
// not routed through handler, so call its handler directly) and returns the finished-flight object whose model
// field matches, decoded so assertions do not depend on JSON escaping.
func recentRowWithModel(t *testing.T, model string) map[string]any {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		req := httptest.NewRequest("GET", logview.RecentFlightsPath, nil)
		req.RemoteAddr = "127.0.0.1:1" // The API only answers localhost requests
		rec := httptest.NewRecorder()
		logview.RecentFlightsHandler(rec, req)
		var tree any
		if json.Unmarshal(rec.Body.Bytes(), &tree) == nil {
			if row := findModelRow(tree, model); row != nil {
				return row
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("no finished flight for model %q appeared", model)
	return nil
}

// findModelRow walks the decoded /__recentflights tree depth-first for the first object whose model matches.
func findModelRow(v any, model string) map[string]any {
	switch x := v.(type) {
	case map[string]any:
		if m, _ := x["model"].(string); m == model {
			return x
		}
		for _, c := range x {
			if row := findModelRow(c, model); row != nil {
				return row
			}
		}
	case []any:
		for _, c := range x {
			if row := findModelRow(c, model); row != nil {
				return row
			}
		}
	}
	return nil
}
