package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// TestSearchAndRespond verifies summary mode: mock Kimi upstream (step1 non-streaming search results + step2 streaming summary);
// searchAndRespond should build Kimi-format SSE (server_tool_use + web_search_tool_result + summary text_delta).
func TestSearchAndRespond(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/messages", func(w http.ResponseWriter, r *http.Request) {
		var m map[string]any
		body := readAll(t, r.Body)
		_ = json.Unmarshal(body, &m)
		stream, _ := m["stream"].(bool)
		if !stream {
			// step1: non-streaming JSON returning server_tool_use + web_search_tool_result.
			resp := map[string]any{
				"id":          "msg_step1",
				"model":       "kimi-for-coding",
				"stop_reason": "end_turn",
				"content": []map[string]any{
					{"type": "server_tool_use", "id": "srvtoolu_test1", "name": "web_search", "input": map[string]any{}},
					{"type": "web_search_tool_result", "tool_use_id": "srvtoolu_test1", "content": []map[string]any{
						{"type": "web_search_tool_result_content", "url": "https://example.com/1", "title": "Result 1 Title"},
					}},
				},
			}
			b, _ := json.Marshal(resp)
			w.Header().Set("content-type", "application/json")
			w.Write(b)
			return
		}
		// step2: streaming SSE returning the summary text.
		w.Header().Set("content-type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		fmt.Fprintf(w, "event: message_start\ndata: %s\n\n", `{"type":"message_start","message":{"model":"kimi-for-coding"}}`)
		fmt.Fprintf(w, "event: content_block_start\ndata: %s\n\n", `{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`)
		fmt.Fprintf(w, "event: content_block_delta\ndata: %s\n\n", `{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"Result 1: this is a detailed summary of the first search result"}}`)
		fmt.Fprintf(w, "event: content_block_stop\ndata: %s\n\n", `{"type":"content_block_stop","index":0}`)
		fmt.Fprintf(w, "event: message_delta\ndata: %s\n\n", `{"type":"message_delta","delta":{"stop_reason":"end_turn"}}`)
		fmt.Fprintf(w, "event: message_stop\ndata: %s\n\n", `{"type":"message_stop"}`)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	cfg.Store(&Config{})
	defer cfg.Store(&Config{})
	resetStats()

	sf := &SearchRoute{URL: srv.URL, API: "test-key", Model: "kimi-for-coding", SummaryMode: true}
	f := &flight{id: 1, start: time.Now()}
	flights.register(f)
	defer flights.unregister(f.id)

	body := []byte(`{"model":"claude-sonnet-4.6","stream":true,"messages":[{"role":"user","content":"search weather in Beijing"}],"tools":[{"type":"web_search_20250305","name":"web_search"}]}`)

	rec := httptest.NewRecorder()
	ok := searchAndRespond(rec, body, sf, f, "claude-sonnet-4.6")
	if !ok {
		t.Fatal("searchAndRespond 返回 false（应成功）")
	}

	out := rec.Body.String()
	checks := map[string]string{
		"server_tool_use":        "缺 server_tool_use 块（下拉触发）",
		"web_search_tool_result": "缺 web_search_tool_result 块（标题/URL）",
		"srvtoolu_test1":         "tool_use_id 未回填",
		"Result 1 Title":         "搜索结果标题未回填",
		"detailed summary":       "摘要文本未写入",
		"end_turn":               "缺 stop_reason=end_turn",
		"message_stop":           "缺 message_stop 收尾",
	}
	for key, msg := range checks {
		if !strings.Contains(out, key) {
			t.Errorf("%s（输出缺 %q）", msg, key)
		}
	}
	if rec.Code != http.StatusOK {
		t.Errorf("状态码 %d，期望 200", rec.Code)
	}
	if !strings.Contains(rec.Header().Get("content-type"), "text/event-stream") {
		t.Errorf("content-type=%s，期望含 text/event-stream", rec.Header().Get("content-type"))
	}
}

// TestSearchAndRespondStep1Fail verifies false is returned when step1 fails (the handler should degrade).
func TestSearchAndRespondStep1Fail(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/messages", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte(`{"error":"upstream down"}`))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	cfg.Store(&Config{})
	defer cfg.Store(&Config{})
	resetStats()

	sf := &SearchRoute{URL: srv.URL, API: "test-key", Model: "kimi-for-coding", SummaryMode: true}
	f := &flight{id: 2, start: time.Now()}
	flights.register(f)
	defer flights.unregister(f.id)

	body := []byte(`{"model":"claude-sonnet-4.6","stream":true,"messages":[{"role":"user","content":"q"}],"tools":[{"type":"web_search_20250305","name":"web_search"}]}`)
	rec := httptest.NewRecorder()
	if searchAndRespond(rec, body, sf, f, "claude-sonnet-4.6") {
		t.Fatal("step1 失败时应返回 false")
	}
}

// TestSearchAndRespondStep2Fallback verifies graded degradation on step2 failure: step2(thinking) fails -> mid (thinking off) retry succeeds.
// mock: step1 returns search results; stream+with-thinking returns 500; stream+without-thinking returns the mid summary.
func TestSearchAndRespondStep2Fallback(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/messages", func(w http.ResponseWriter, r *http.Request) {
		body := readAll(t, r.Body)
		var m map[string]any
		_ = json.Unmarshal(body, &m)
		stream, _ := m["stream"].(bool)
		if !stream {
			// step1: non-streaming search results
			resp := map[string]any{
				"id": "msg_step1", "model": "kimi-for-coding", "stop_reason": "end_turn",
				"content": []map[string]any{
					{"type": "server_tool_use", "id": "srvtoolu_test2", "name": "web_search", "input": map[string]any{}},
					{"type": "web_search_tool_result", "tool_use_id": "srvtoolu_test2", "content": []map[string]any{
						{"type": "web_search_tool_result_content", "url": "https://example.com/2", "title": "Result 2 Title"},
					}},
				},
			}
			b, _ := json.Marshal(resp)
			w.Header().Set("content-type", "application/json")
			w.Write(b)
			return
		}
		// stream=true: step2 (with thinking) fails, degraded mid (without thinking) returns the summary.
		if strings.Contains(string(body), `"thinking"`) {
			w.WriteHeader(http.StatusInternalServerError)
			w.Write([]byte(`{"error":"step2 down"}`))
			return
		}
		w.Header().Set("content-type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		fmt.Fprintf(w, "event: message_start\ndata: %s\n\n", `{"type":"message_start","message":{"model":"kimi-for-coding"}}`)
		fmt.Fprintf(w, "event: content_block_delta\ndata: %s\n\n", `{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"mid level summary"}}`)
		fmt.Fprintf(w, "event: message_stop\ndata: %s\n\n", `{"type":"message_stop"}`)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	cfg.Store(&Config{})
	defer cfg.Store(&Config{})
	resetStats()

	sf := &SearchRoute{URL: srv.URL, API: "test-key", Model: "kimi-for-coding", SummaryMode: true, SummaryThinking: true}
	f := &flight{id: 3, start: time.Now()}
	flights.register(f)
	defer flights.unregister(f.id)

	body := []byte(`{"model":"claude-sonnet-4.6","stream":true,"messages":[{"role":"user","content":"q"}],"tools":[{"type":"web_search_20250305","name":"web_search"}]}`)
	rec := httptest.NewRecorder()
	if !searchAndRespond(rec, body, sf, f, "claude-sonnet-4.6") {
		t.Fatal("应返回 true（降级 mid 成功）")
	}
	out := rec.Body.String()
	if !strings.Contains(out, "mid level summary") {
		t.Errorf("降级 mid 摘要缺失，输出: %s", out)
	}
}

// TestSearchAndRespondStep2AllFail verifies that when both step2 and mid fail, step1's search results are returned directly (no summary text).
func TestSearchAndRespondStep2AllFail(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/messages", func(w http.ResponseWriter, r *http.Request) {
		body := readAll(t, r.Body)
		var m map[string]any
		_ = json.Unmarshal(body, &m)
		stream, _ := m["stream"].(bool)
		if !stream {
			resp := map[string]any{
				"id": "msg_step1", "model": "kimi-for-coding", "stop_reason": "end_turn",
				"content": []map[string]any{
					{"type": "server_tool_use", "id": "srvtoolu_test3", "name": "web_search", "input": map[string]any{}},
					{"type": "web_search_tool_result", "tool_use_id": "srvtoolu_test3", "content": []map[string]any{
						{"type": "web_search_tool_result_content", "url": "https://example.com/3", "title": "Result 3 Title"},
					}},
				},
			}
			b, _ := json.Marshal(resp)
			w.Header().Set("content-type", "application/json")
			w.Write(b)
			return
		}
		// stream=true: both step2 and mid fail
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte(`{"error":"all fail"}`))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	cfg.Store(&Config{})
	defer cfg.Store(&Config{})
	resetStats()

	sf := &SearchRoute{URL: srv.URL, API: "test-key", Model: "kimi-for-coding", SummaryMode: true}
	f := &flight{id: 4, start: time.Now()}
	flights.register(f)
	defer flights.unregister(f.id)

	body := []byte(`{"model":"claude-sonnet-4.6","stream":true,"messages":[{"role":"user","content":"q"}],"tools":[{"type":"web_search_20250305","name":"web_search"}]}`)
	rec := httptest.NewRecorder()
	if !searchAndRespond(rec, body, sf, f, "claude-sonnet-4.6") {
		t.Fatal("应返回 true（返回 step1 结果）")
	}
	out := rec.Body.String()
	// Should contain step1's search results
	if !strings.Contains(out, "web_search_tool_result") || !strings.Contains(out, "Result 3 Title") {
		t.Errorf("step1 搜索结果缺失，输出: %s", out)
	}
	// Should have a message_stop ending
	if !strings.Contains(out, "message_stop") {
		t.Errorf("缺 message_stop 收尾")
	}
}

func readAll(t *testing.T, r interface{ Read([]byte) (int, error) }) []byte {
	t.Helper()
	buf := make([]byte, 0, 1024)
	tmp := make([]byte, 1024)
	for {
		n, err := r.Read(tmp)
		buf = append(buf, tmp[:n]...)
		if err != nil {
			break
		}
	}
	return buf
}

// TestSummaryLevelConfig verifies the four summary levels return different instructions and max_tokens.
func TestSummaryLevelConfig(t *testing.T) {
	cases := []struct {
		level   string
		wantMax int
		wantKey string // Keywords the instruction should contain
	}{
		{"low", 2048, "short"},
		{"", 2048, "short"}, // Empty defaults to low
		{"mid", 4096, "medium-detail"},
		{"high", 8192, "detailed comprehensive"},
		{"HIGH", 8192, "detailed comprehensive"}, // Case-insensitive
		{"max", 16384, "verbatim"},               // max: full restatement of steps/methods/code/formulas
		{"full", 16384, "verbatim"},              // full equals max
		{"unknown", 2048, "short"},               // Unknown falls back to low
	}
	for _, c := range cases {
		instr, max := summaryLevelConfig(c.level, "test query")
		if max != c.wantMax {
			t.Errorf("level=%q max_tokens=%d 期望 %d", c.level, max, c.wantMax)
		}
		if !strings.Contains(instr, c.wantKey) {
			t.Errorf("level=%q 指令缺关键词 %q: %s", c.level, c.wantKey, instr)
		}
	}
	// The four level instructions should all differ
	low, _ := summaryLevelConfig("low", "test query")
	mid, _ := summaryLevelConfig("mid", "test query")
	high, _ := summaryLevelConfig("high", "test query")
	mx, _ := summaryLevelConfig("max", "test query")
	if low == mid || mid == high || low == high {
		t.Error("三档指令不应相同")
	}
	if mx == high {
		t.Error("max 指令应与 high 不同（须含完整复述要求）")
	}
}
