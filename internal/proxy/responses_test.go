package proxy

// responses_test.go — end-to-end tests through the real main pipeline: the Responses listener port
// (translate.ResponsesHandler) feeding the root handler.

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"proxy429/internal/config"
	"proxy429/internal/jsonutil"
	"proxy429/internal/translate"
)

// TestResponsesHandlerEndToEnd end-to-end: Responses request → translation → main pipeline → fake-upstream SSE →
// translated back to a Responses JSON (non-streaming client).
func TestResponsesHandlerEndToEnd(t *testing.T) {
	resetStats()
	var gotBody map[string]interface{}
	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		var m map[string]interface{}
		_ = json.Unmarshal(b, &m)
		gotBody = m
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		io.WriteString(w, testSSEAllBlocks())
	}))
	defer mock.Close()

	config.Store(&config.Config{
		Upstream:       mock.URL,
		MaxRetries:     0,
		TotalBudgetSec: 10,
		Routes: []config.RouteRule{
			{Pattern: "gpt-5*", URL: mock.URL, Model: "deepseek-v4-flash"},
		},
	})

	proxy := httptest.NewServer(http.HandlerFunc(translate.ResponsesHandler(handler)))
	defer proxy.Close()

	reqBody := `{"model":"gpt-5-codex","input":"hello","instructions":"Be helpful."}`
	resp, err := http.Post(proxy.URL+"/v1/responses", "application/json", strings.NewReader(reqBody))
	if err != nil {
		t.Fatalf("请求失败: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()

	// What the upstream receives should be the translated Anthropic request: system from instructions, stream forced true,
	// model rewritten by routing.
	if jsonutil.ObjStr(gotBody, "system") != "Be helpful." {
		t.Errorf("上游 system=%v", gotBody["system"])
	}
	if s, _ := gotBody["stream"].(bool); !s {
		t.Errorf("上游 stream=%v, want true（内部强制流式）", gotBody["stream"])
	}
	if jsonutil.ObjStr(gotBody, "model") != "deepseek-v4-flash" {
		t.Errorf("上游 model=%v, want 路由改写 deepseek-v4-flash", gotBody["model"])
	}
	msgs := jsonutil.AsArr(gotBody["messages"])
	if len(msgs) != 1 {
		t.Errorf("上游 messages=%v", gotBody["messages"])
	}

	// The client gets a Responses JSON.
	var m map[string]interface{}
	if err := json.Unmarshal(body, &m); err != nil {
		t.Fatalf("响应不是 JSON: %v (%s)", err, string(body))
	}
	if m["object"] != "response" {
		t.Fatalf("object=%v, body=%s", m["object"], string(body))
	}
	// model written back to the client's original name (the pipeline's model write-back logic).
	if m["model"] != "gpt-5-codex" {
		t.Errorf("model=%v, want gpt-5-codex", m["model"])
	}
	// Search envelope riding along (triple injected after routing settled): original 5 items + envelope reasoning item = 6.
	if len(jsonutil.AsArr(m["output"])) != 6 {
		t.Errorf("output 数=%d, want 6（含搜索信封项）", len(jsonutil.AsArr(m["output"])))
	}
}

// TestResponsesFastRouteEntry verifies the Codex menu's fast entry (model name literal "fast_route") gets
// speed:"fast" injected by the translation layer, and the main pipeline's fast branch takes over: the upstream receives the model
// rewritten to fast_route.model, with the speed field already removed.
func TestResponsesFastRouteEntry(t *testing.T) {
	resetStats()
	var gotBody map[string]interface{}
	fastMock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &gotBody)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		io.WriteString(w, testSSEAllBlocks())
	}))
	defer fastMock.Close()

	config.Store(&config.Config{
		MaxRetries: 0, TotalBudgetSec: 10,
		FastRoute: &config.FastRoute{URL: fastMock.URL, Model: "k3-256k"},
	})
	defer config.Store(&config.Config{})

	proxy := httptest.NewServer(http.HandlerFunc(translate.ResponsesHandler(handler)))
	defer proxy.Close()

	resp, err := http.Post(proxy.URL+"/v1/responses", "application/json",
		strings.NewReader(`{"model":"fast_route","input":"hi"}`))
	if err != nil {
		t.Fatalf("请求失败: %v", err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()

	if jsonutil.ObjStr(gotBody, "model") != "k3-256k" {
		t.Errorf("fast 通道上游 model=%v, want k3-256k（fast_route.model 改写）", gotBody["model"])
	}
	if _, has := gotBody["speed"]; has {
		t.Errorf("speed 字段应被 fast 分支移除，上游却收到: %v", gotBody["speed"])
	}
}
