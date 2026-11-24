package main

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"proxy429/internal/config"
	"proxy429/internal/recorder"
	"proxy429/internal/translate"
	"strings"
	"testing"
	"time"
)

// testSearchBlockPair builds a pair of real search blocks (server_tool_use with query + result block with encrypted_content).
func testSearchBlockPair() (map[string]interface{}, map[string]interface{}) {
	use := map[string]interface{}{
		"type": "server_tool_use", "id": "srvtoolu_1", "name": "web_search",
		"input": map[string]interface{}{"query": "测试查询"},
	}
	res := map[string]interface{}{
		"type": "web_search_tool_result", "tool_use_id": "srvtoolu_1",
		"content": []interface{}{map[string]interface{}{
			"type": "web_search_result", "title": "测试结果",
			"url": "https://example.com/a", "encrypted_content": "ENC_BLOB_123",
		}},
	}
	return use, res
}

// TestSearchEnvelopeFailSoftRetry end-to-end fail-soft: a request carrying a replayed search block, the upstream first
// 400s with tool_call_id (registry doesn't recognize the old id) → the proxy strips blocks and retries immediately → the client gets 200,
// and the second upstream request body contains no search blocks.
func TestSearchEnvelopeFailSoftRetry(t *testing.T) {
	resetStats()
	var rawBodies []string
	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		rawBodies = append(rawBodies, string(b))
		if len(rawBodies) == 1 {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(400)
			fmt.Fprint(w, `{"type":"error","error":{"type":"invalid_request_error","message":"tool_call_id srvtoolu_1 is not found"}}`)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		fmt.Fprint(w, testSSEAllBlocks())
	}))
	defer mock.Close()

	config.Store(&config.Config{Upstream: mock.URL, MaxRetries: 0, TotalBudgetSec: 10})

	proxy := httptest.NewServer(http.HandlerFunc(handler))
	defer proxy.Close()

	reqBody := `{"model":"k3-256k","stream":true,"messages":[` +
		`{"role":"user","content":[{"type":"text","text":"搜下X"}]},` +
		`{"role":"assistant","content":[` +
		`{"type":"server_tool_use","id":"srvtoolu_1","name":"web_search","input":{"query":"X"}},` +
		`{"type":"web_search_tool_result","tool_use_id":"srvtoolu_1","content":[{"type":"web_search_result","title":"t","url":"https://a.cn","encrypted_content":"ENC"}]}]},` +
		`{"role":"user","content":[{"type":"text","text":"追问"}]}]}`
	resp, err := http.Post(proxy.URL+"/v1/messages", "application/json", strings.NewReader(reqBody))
	if err != nil {
		t.Fatalf("请求失败: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()

	// The 400 isn't passed to the client: the fail-soft retry succeeds with 200.
	if resp.StatusCode != 200 {
		t.Fatalf("客户端状态码=%d, want 200（fail-soft 应兜底）: %s", resp.StatusCode, body)
	}
	// The upstream receives exactly 2 requests: the first with search blocks, the retry stripped bare.
	if len(rawBodies) != 2 {
		t.Fatalf("上游请求数=%d, want 2", len(rawBodies))
	}
	if !strings.Contains(rawBodies[0], "server_tool_use") {
		t.Errorf("首次请求应带搜索块: %s", rawBodies[0])
	}
	if strings.Contains(rawBodies[1], "server_tool_use") || strings.Contains(rawBodies[1], "web_search_tool_result") {
		t.Errorf("重试请求应剥光搜索块: %s", rawBodies[1])
	}
	if !strings.Contains(rawBodies[1], "搜下X") || !strings.Contains(rawBodies[1], "追问") {
		t.Errorf("重试请求丢了正文: %s", rawBodies[1])
	}

	// [剥N] counting: the finished archive should record 2 400-fallback strips (stu+result each count 1).
	recorder.FinishedMu.Lock()
	ff := recorder.Finished[len(recorder.Finished)-1]
	recorder.FinishedMu.Unlock()
	if ff.SearchStripped != 2 {
		t.Errorf("完成流剥块计数=%d, want 2（400 兜底剥 stu+result）", ff.SearchStripped)
	}
}

// TestSearchEnvelopeCutoffLearning end-to-end: the Responses port replays an envelope → upstream 400
// tool_call_id → fallback stripping retries and learns the conversation watermark; same-conversation envelopes older than or exactly at the watermark get
// watermark-stripped directly next turn (upstream 200 on first shot, no search blocks, no 400), envelopes newer than the watermark restore as usual.
// [剥N] counting includes both watermark strips and 400-fallback strips.
func TestSearchEnvelopeCutoffLearning(t *testing.T) {
	resetStats()
	var rawBodies []string
	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		rawBodies = append(rawBodies, string(b))
		if len(rawBodies) == 1 {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(400)
			fmt.Fprint(w, `{"type":"error","error":{"type":"invalid_request_error","message":"tool_call_id srvtoolu_1 is not found"}}`)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		fmt.Fprint(w, testSSEAllBlocks())
	}))
	defer mock.Close()

	config.Store(&config.Config{Upstream: mock.URL, MaxRetries: 0, TotalBudgetSec: 10})
	proxy := httptest.NewServer(http.HandlerFunc(translate.ResponsesHandler(handler)))
	defer proxy.Close()

	tri := translate.NewSearchTriple(mock.URL, "k3-256k", "sk-test")
	use, res := testSearchBlockPair()
	// A per-run conversation id keeps the watermark table hermetic without reaching into translate's test hooks.
	convID := fmt.Sprintf("cutoff-learn-%d", time.Now().UnixNano())
	doReq := func(enc string) {
		body := `{"model":"k3-256k","prompt_cache_key":"` + convID + `","input":[` +
			`{"type":"message","role":"user","content":[{"type":"input_text","text":"搜下X"}]},` +
			`{"type":"reasoning","encrypted_content":"` + enc + `"},` +
			`{"type":"message","role":"user","content":[{"type":"input_text","text":"追问"}]}]}`
		req, _ := http.NewRequest("POST", proxy.URL+"/v1/responses", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "sk-test")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("请求失败: %v", err)
		}
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != 200 {
			t.Fatalf("客户端状态码=%d, want 200: %s", resp.StatusCode, b)
		}
	}
	lastStripped := func() int {
		recorder.FinishedMu.Lock()
		defer recorder.FinishedMu.Unlock()
		return recorder.Finished[len(recorder.Finished)-1].SearchStripped
	}

	// First round: a new envelope restores upstream → upstream 400 → fallback strips 2 blocks, retry 200, watermark set to the envelope's sealing moment.
	t0 := time.Now().Unix()
	doReq(translate.EncodeSearchEnvelopeAt(tri, use, res, t0))
	if len(rawBodies) != 2 {
		t.Fatalf("第一轮上游请求数=%d, want 2（400+兜底重试）", len(rawBodies))
	}
	if !strings.Contains(rawBodies[0], "server_tool_use") || strings.Contains(rawBodies[1], "server_tool_use") {
		t.Errorf("第一轮：首发应带搜索块、重试应剥光: %s / %s", rawBodies[0], rawBodies[1])
	}
	if n := lastStripped(); n != 2 {
		t.Errorf("第一轮剥块计数=%d, want 2（400 兜底）", n)
	}
	if w := translate.SearchCutoffFor(convID); w.Unix() != t0 {
		t.Errorf("对话水位=%v, want 封入时刻 %v", w, time.Unix(t0, 0))
	}

	// Second round: an older envelope from the same conversation → watermark-stripped directly; upstream 200 on first shot with no search blocks (zero 400s).
	doReq(translate.EncodeSearchEnvelopeAt(tri, use, res, t0-60))
	if len(rawBodies) != 3 {
		t.Fatalf("第二轮后上游请求数=%d, want 3（水位剥不撞 400）", len(rawBodies))
	}
	if strings.Contains(rawBodies[2], "server_tool_use") || strings.Contains(rawBodies[2], "web_search_tool_result") {
		t.Errorf("第二轮：水位剥后不应带搜索块上行: %s", rawBodies[2])
	}
	if n := lastStripped(); n != 2 {
		t.Errorf("第二轮剥块计数=%d, want 2（水位剥）", n)
	}

	// Equal-to-watermark round: an envelope sealed exactly at the watermark → watermark-stripped the same (same-age ones are certainly dead; no 400 hit).
	doReq(translate.EncodeSearchEnvelopeAt(tri, use, res, t0))
	if len(rawBodies) != 4 {
		t.Fatalf("等于水位轮后上游请求数=%d, want 4（水位剥不撞 400）", len(rawBodies))
	}
	if strings.Contains(rawBodies[3], "server_tool_use") || strings.Contains(rawBodies[3], "web_search_tool_result") {
		t.Errorf("等于水位轮：水位剥后不应带搜索块上行: %s", rawBodies[3])
	}
	if n := lastStripped(); n != 2 {
		t.Errorf("等于水位轮剥块计数=%d, want 2（水位剥）", n)
	}

	// Third round: an envelope newer than the watermark → restored as usual (search blocks go upstream; the mock already returns 200, no more stripping).
	doReq(translate.EncodeSearchEnvelopeAt(tri, use, res, t0+60))
	if len(rawBodies) != 5 {
		t.Fatalf("第三轮后上游请求数=%d, want 5", len(rawBodies))
	}
	if !strings.Contains(rawBodies[4], "server_tool_use") {
		t.Errorf("第三轮：新于水位的信封应还原上行: %s", rawBodies[4])
	}
	if n := lastStripped(); n != 0 {
		t.Errorf("第三轮剥块计数=%d, want 0（还原被上游收下）", n)
	}
}
