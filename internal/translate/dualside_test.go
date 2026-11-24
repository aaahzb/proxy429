package translate

// dualside_test.go — the translating writer's downstream-side tap: every byte written to the client must reach the
// injected FlightTap, in both streaming (per-event emit) and non-streaming (one-shot JSON finish) modes.

import (
	"net/http/httptest"
	"strings"
	"testing"
)

// fakeTap records the downstream-side bytes the writer tees off.
type fakeTap struct{ down []byte }

func (f *fakeTap) AppendContentDown(b []byte) { f.down = append(f.down, b...) }
func (f *fakeTap) RefreshArchivedDown()       {}

// TestTranslatingWriterDownTap verifies the downstream-side tap covers translatingWriter's write paths:
// the bytes written to the client in both streaming (emit per event) and non-streaming (finish one-shot JSON) modes are teed in equal measure into
// the tap. The tap wraps dst itself, and emit/finish/finishBuffered/error writes all go through dst.Write,
// so byte-for-byte equality in both modes proves full path coverage.
func TestTranslatingWriterDownTap(t *testing.T) {
	// Streaming client: SSE events pass through the tap one by one.
	rec := httptest.NewRecorder()
	tw := newTranslatingWriter(rec, true, "gpt-5-codex", nil)
	var tap fakeTap
	tw.SetDownTap(&tap)
	tw.Header().Set("Content-Type", "text/event-stream")
	tw.WriteHeader(200)
	if _, err := tw.Write([]byte(testSSEAllBlocks())); err != nil {
		t.Fatalf("Write: %v", err)
	}
	tw.finish()
	if string(tap.down) != rec.Body.String() {
		t.Errorf("流式 tap 与客户端实收不一致：tap=%d 字节, 客户端=%d 字节", len(tap.down), rec.Body.Len())
	}
	if !strings.Contains(string(tap.down), "response.completed") {
		t.Errorf("流式 contentDown 应含 response.completed 事件")
	}

	// Non-streaming client: the one-shot Responses JSON produced by finish also passes through the tap.
	rec2 := httptest.NewRecorder()
	tw2 := newTranslatingWriter(rec2, false, "gpt-5-codex", nil)
	var tap2 fakeTap
	tw2.SetDownTap(&tap2)
	tw2.Header().Set("Content-Type", "text/event-stream")
	tw2.WriteHeader(200)
	if _, err := tw2.Write([]byte(testSSEAllBlocks())); err != nil {
		t.Fatalf("Write: %v", err)
	}
	tw2.finish()
	if string(tap2.down) != rec2.Body.String() {
		t.Errorf("非流式 tap 与客户端实收不一致：tap=%d 字节, 客户端=%d 字节", len(tap2.down), rec2.Body.Len())
	}
	if !strings.Contains(string(tap2.down), `"object":"response"`) {
		t.Errorf("非流式 contentDown 应含一次性 Responses JSON")
	}
}
