package main

import (
	"strings"
	"testing"
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
