package main

import (
	"encoding/json"
	"io"
	"net/http"
)

// lazyApplyRouteKeys lists the fields a routes[] entry may inherit from lazyApply. Only fields the route
// object already accepts are listed; connection fields (url/api/model/pattern/url_response_api) never inherit.
var lazyApplyRouteKeys = []string{
	"text_only",
	"no_search",
	"thinking",
	"convertOff2Low",
	"enhance_search",
	"claudeSearchThinking",
	"claudeSearchSummary",
	"noBullshitOnEmptyClaudeSearch",
}

// lazyApplySearchFallbackKeys lists the fields search_fallback may inherit. A nested enhance_search never
// applies here: the fallback serves diverted requests through its own summary/thinking fields.
var lazyApplySearchFallbackKeys = []string{
	"convertOff2Low",
	"summary_mode",
	"summary_thinking",
	"summary_level",
	"claudeSearchThinking",
	"claudeSearchSummary",
	"noBullshitOnEmptyClaudeSearch",
}

// lazyApplyClassifierKeys lists the fields classifier_route may inherit.
var lazyApplyClassifierKeys = []string{
	"classifier_thinking",
	"classifier_thinking_cc",
	"classifier_thinking_codex",
}

// lazyApplyConvertOnlyKeys is shared by fast_route and multimodal_fallback.
var lazyApplyConvertOnlyKeys = []string{"convertOff2Low"}

// materializeLazyApply proves what lazyApply does to the edited JSON and returns it as pretty JSON.
// It validates the whole config first (parseConfig), then works on the raw text so $VAR references stay
// unexpanded and secrets never reach the browser. Every route-like object receives the lazyApply keys it
// supports and does not already write; noLazyApply:true skips an object entirely. The lazyApply object
// itself is kept, so copying the preview back into the editor is a behavior-preserving edit.
func materializeLazyApply(data []byte) ([]byte, error) {
	if _, _, err := parseConfig(data); err != nil {
		return nil, err
	}
	var top map[string]json.RawMessage
	if err := json.Unmarshal(data, &top); err != nil {
		return nil, err
	}
	rawLazy, ok := top["lazyApply"]
	if !ok || isJSONNull(rawLazy) {
		return marshalPrettyConfig(top)
	}
	var lazy map[string]json.RawMessage
	if err := json.Unmarshal(rawLazy, &lazy); err != nil {
		return nil, err
	}
	if len(lazy) == 0 {
		return marshalPrettyConfig(top)
	}
	var firstErr error
	fill := func(objectKey string, keys []string) {
		if firstErr != nil {
			return
		}
		rawObj, exists := top[objectKey]
		if !exists {
			return // lazyApply never creates an optional route-like object
		}
		var obj map[string]json.RawMessage
		if err := json.Unmarshal(rawObj, &obj); err != nil || obj == nil {
			return // null / non-object values keep the real parser's error path
		}
		if jsonBoolTrue(obj["noLazyApply"]) {
			return
		}
		for _, key := range keys {
			if _, written := obj[key]; written {
				continue // an explicitly written local value always wins, including false / "" / null
			}
			value, ok := lazy[key]
			if !ok {
				continue
			}
			obj[key] = value
		}
		encoded, err := json.Marshal(obj)
		if err != nil {
			firstErr = err
			return
		}
		top[objectKey] = encoded
	}
	fill("search_fallback", lazyApplySearchFallbackKeys)
	fill("fast_route", lazyApplyConvertOnlyKeys)
	fill("multimodal_fallback", lazyApplyConvertOnlyKeys)
	fill("classifier_route", lazyApplyClassifierKeys)
	if firstErr != nil {
		return nil, firstErr
	}
	if rawRoutes, exists := top["routes"]; exists {
		var entries []json.RawMessage
		if err := json.Unmarshal(rawRoutes, &entries); err == nil && entries != nil {
			changed := false
			for i, entry := range entries {
				var obj map[string]json.RawMessage
				if err := json.Unmarshal(entry, &obj); err != nil || obj == nil {
					continue
				}
				if jsonBoolTrue(obj["noLazyApply"]) {
					continue
				}
				before := len(obj)
				for _, key := range lazyApplyRouteKeys {
					if _, written := obj[key]; written {
						continue
					}
					value, ok := lazy[key]
					if !ok {
						continue
					}
					obj[key] = value
				}
				if len(obj) == before {
					continue
				}
				encoded, err := json.Marshal(obj)
				if err != nil {
					return nil, err
				}
				entries[i] = encoded
				changed = true
			}
			if changed {
				encoded, err := json.Marshal(entries)
				if err != nil {
					return nil, err
				}
				top["routes"] = encoded
			}
		}
	}
	return marshalPrettyConfig(top)
}

// marshalPrettyConfig renders a raw config map with stable indentation for the preview panel.
func marshalPrettyConfig(top map[string]json.RawMessage) ([]byte, error) {
	out, err := json.MarshalIndent(top, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(out, '\n'), nil
}

// isJSONNull reports whether raw is the JSON null literal (surrounding whitespace ignored).
func isJSONNull(raw json.RawMessage) bool {
	return len(raw) == 4 && string(raw) == "null"
}

// jsonBoolTrue reports whether raw is the JSON boolean true. Missing or malformed values report false;
// the real parser reports malformed noLazyApply values before the preview is rendered.
func jsonBoolTrue(raw json.RawMessage) bool {
	var b bool
	return json.Unmarshal(raw, &b) == nil && b
}

// lazyApplyPreviewHandler renders the edited config with lazyApply materialized, for the console's
// "preview lazyApply" button. It never writes the file and never publishes a config: the body is the
// editor's current text, and only the materialized JSON is returned. Localhost only, like every /__* route.
func lazyApplyPreviewHandler(w http.ResponseWriter, r *http.Request) {
	if !isLocalRequest(r) {
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
	out, err := materializeLazyApply(body)
	if err != nil {
		http.Error(w, "invalid config: "+err.Error(), http.StatusBadRequest)
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(out)
}
