package main

// mock_429.go：本地测试用，模拟三种上游行为，用 ?mode= 切换：
//   mode=429      → 返回 HTTP 429（测情况 A：状态码重试）
//   mode=bodyerr  → 返回 HTTP 200 + SSE 错误事件（测情况 B：体内错误重试）
//   mode=ok       → 返回 HTTP 200 + 正常 SSE（测情况 C：正常透传）
// 同时打印收到的 thinking/reasoning_effort/max_tokens，验证分类器改写是否生效。
// 用法（在项目根目录执行）：go run ./test，监听 127.0.0.1:9099。

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"time"
)

func main() {
	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mode := r.URL.Query().Get("mode")
		if mode == "" {
			mode = "429"
		}

		// 解析请求体，打印 thinking 相关字段，验证代理是否改写。
		var p map[string]interface{}
		if err := json.Unmarshal(body, &p); err == nil {
			thinking := p["thinking"]
			reasoningEffort := p["reasoning_effort"]
			maxTokens := p["max_tokens"]
			log.Printf("[MOCK] 收到 %s %s mode=%s thinking=%v reasoning_effort=%v max_tokens=%v key顺序=%v",
				r.Method, r.URL.Path, mode, thinking, reasoningEffort, maxTokens, topKeys(body))
		} else {
			log.Printf("[MOCK] 收到 %s %s mode=%s (body=%d字节)", r.Method, r.URL.Path, mode, len(body))
		}

		switch mode {
		case "bodyerr":
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(200)
			w.Write([]byte("event: error\ndata: {\"type\":\"error\",\"error\":{\"type\":\"rate_limit_error\",\"message\":\"rate limited\"}}\n\n"))
			return
		case "ok":
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(200)
			flusher, _ := w.(http.Flusher)
			// message_start 带 usage：input/cache_read/cache_creation/output 初始值，
			// 让代理的实时状态行能显示缓存命中/写入。
			w.Write([]byte("event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":10,\"cache_read_input_tokens\":5,\"cache_creation_input_tokens\":3,\"output_tokens\":1}}}\n\n"))
			if flusher != nil {
				flusher.Flush()
			}
			// 每段 delta 后跟一个 message_delta 更新 output_tokens（累积值），
			// 模拟流式输出 token 增长，状态行的「输出」和「tok/s」会跳动。
			for i := 1; i <= 5; i++ {
				w.Write([]byte("event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"delta\":{\"text\":\"hello\"}}\n\n"))
				w.Write([]byte(fmt.Sprintf("event: message_delta\ndata: {\"type\":\"message_delta\",\"usage\":{\"output_tokens\":%d}}\n\n", i*10)))
				if flusher != nil {
					flusher.Flush()
				}
				time.Sleep(100 * time.Millisecond)
			}
			w.Write([]byte("event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"))
			if flusher != nil {
				flusher.Flush()
			}
			return
		default: // 429
			w.Header().Set("Retry-After", "1")
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(429)
			w.Write([]byte(`{"error":{"type":"rate_limit_error","message":"mock 429"}}`))
			return
		}
	})
	log.Println("mock 服务器启动: 监听 127.0.0.1:9099 (?mode=429|bodyerr|ok)")
	log.Fatal(http.ListenAndServe("127.0.0.1:9099", nil))
}

// topKeys 用 Decoder 流式提取顶层 object 的 key 顺序（保留出现顺序），用于验证改写是否重排。
func topKeys(body []byte) []string {
	dec := json.NewDecoder(bytes.NewReader(body))
	if t, err := dec.Token(); err != nil || t != json.Delim('{') {
		return nil
	}
	var keys []string
	for dec.More() {
		tk, err := dec.Token()
		if err != nil {
			return keys
		}
		k, ok := tk.(string)
		if !ok {
			return keys
		}
		keys = append(keys, k)
		var v json.RawMessage
		if err := dec.Decode(&v); err != nil {
			return keys
		}
	}
	return keys
}
