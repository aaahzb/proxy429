package main

import (
	"io"
	"net/http"

	"proxy429/internal/config"
	"proxy429/internal/platform"
)

// LazyApplyPreviewHandler renders the edited config with lazyApply materialized, for the console's
// "preview lazyApply" button. It never writes the file and never publishes a config: the body is the
// editor's current text, and only the insertion-edited JSON is returned. Localhost only, like every
// /__* route.
func LazyApplyPreviewHandler(w http.ResponseWriter, r *http.Request) {
	if !platform.IsLocalRequest(r) {
		http.Error(w, "forbidden (local only)", http.StatusForbidden)
		return
	}
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "failed to read request body: "+err.Error(), http.StatusBadRequest)
		return
	}
	out, err := config.MaterializeLazyApply(body)
	if err != nil {
		http.Error(w, "invalid config: "+err.Error(), http.StatusBadRequest)
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(out)
}
