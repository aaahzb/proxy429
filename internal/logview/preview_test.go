package logview

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"proxy429/internal/config"
	"strings"
	"testing"
)

// TestLazyApplyPreviewHandler locks the console endpoint: local-only POST, no cache, parse errors
// rejected as 400, and non-POST methods refused.
func TestLazyApplyPreviewHandler(t *testing.T) {
	const valid = `{"upstream":"http://u","lazyApply":{"text_only":true},"routes":[{"pattern":"a","url":"http://a"}]}`
	call := func(method, remote, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, lazyPreviewPath, strings.NewReader(body))
		req.RemoteAddr = remote
		rec := httptest.NewRecorder()
		lazyApplyPreviewHandler(rec, req)
		return rec
	}

	rec := call(http.MethodPost, "127.0.0.1:1", valid)
	if rec.Code != http.StatusOK {
		t.Fatalf("valid preview status = %d, body %s", rec.Code, rec.Body.String())
	}
	if rec.Header().Get("Cache-Control") != "no-store" {
		t.Errorf("Cache-Control = %q, want no-store", rec.Header().Get("Cache-Control"))
	}
	if !strings.Contains(rec.Body.String(), `"text_only":true`) {
		t.Errorf("preview should contain the materialized field:\n%s", rec.Body.String())
	}
	if _, err := config.MaterializeLazyApply(rec.Body.Bytes()); err != nil {
		t.Errorf("preview output should itself be valid: %v", err)
	}

	if rec := call(http.MethodGet, "127.0.0.1:1", ""); rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("GET status = %d, want 405", rec.Code)
	}
	if rec := call(http.MethodPost, "192.168.1.5:1", valid); rec.Code != http.StatusForbidden {
		t.Errorf("remote status = %d, want 403", rec.Code)
	}
	rec = call(http.MethodPost, "127.0.0.1:1", `{"upstream":`)
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "invalid config") {
		t.Errorf("invalid config = %d %q, want 400 invalid config", rec.Code, rec.Body.String())
	}

	// No lazyApply: the preview returns the editor text unchanged.
	rec = call(http.MethodPost, "127.0.0.1:1", `{"upstream":"http://u"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("no-lazy preview status = %d, body %s", rec.Code, rec.Body.String())
	}
	var top map[string]json.RawMessage
	if err := json.Unmarshal(rec.Body.Bytes(), &top); err != nil {
		t.Errorf("no-lazy preview is not JSON: %v", err)
	}
	if _, ok := top["lazyApply"]; ok {
		t.Error("no-lazy preview must not invent a lazyApply object")
	}
}
