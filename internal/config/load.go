package config

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"strings"
	"sync/atomic"
)

// configWarnings holds the names of removed config keys present in the running config (empty = clean). Set at startup
// and on every successful reload/switch; read by the tray poll (red icon + tooltip).
var configWarnings atomic.Value // []string

// SetConfigWarnings replaces the removed-keys warning set (nil/empty clears it).
func SetConfigWarnings(keys []string) {
	configWarnings.Store(keys)
}

// GetConfigWarnings returns the removed-keys warning set (nil when clean).
func GetConfigWarnings() []string {
	v, _ := configWarnings.Load().([]string)
	return v
}

// LoadConfig reads and parses the config file and applies defaults. Shared by main startup and tray reload.
// Removed config keys are non-fatal warnings: the proxy starts and runs with them inert (old behavior is NOT emulated);
// only malformed JSON or invalid values of live keys come back as errors.
func LoadConfig(path string) (*Config, []string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, nil, err
	}
	return ParseConfig(data)
}

// expandConfigVars resolves $NAME / ${NAME} references inside every JSON string value of the top-level config
// object against the top-level "vars" object (whose own values expand too, one level — replacement text is never
// re-scanned, so reference cycles are impossible). Object keys are never touched. A well-formed reference to an
// undefined name fails with the config path; "vars" members must be strings.
func expandConfigVars(top map[string]json.RawMessage) error {
	final := map[string]string{}
	if raw, ok := top["vars"]; ok {
		base, err := varStringMap(raw)
		if err != nil {
			return err
		}
		if base != nil {
			expanded, err := expandVarsInJSON(raw, "vars", func(name string) (string, bool) {
				v, ok := base[name]
				return v, ok
			})
			if err != nil {
				return err
			}
			final, err = varStringMap(expanded) // Values already validated as strings; cannot fail
			if err != nil {
				return err
			}
		}
	}
	lookup := func(name string) (string, bool) {
		v, ok := final[name]
		return v, ok
	}
	return expandVarsInMap(top, "", lookup)
}

// varStringMap decodes a "vars" object into name -> value. Returns nil for an absent/null object; anything that
// is not an object of strings is an error naming the offending member.
func varStringMap(raw json.RawMessage) (map[string]string, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return nil, nil
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(trimmed, &m); err != nil {
		return nil, fmt.Errorf("vars must be an object of string values: %v", err)
	}
	out := make(map[string]string, len(m))
	for k, v := range m {
		var s string
		if err := json.Unmarshal(v, &s); err != nil {
			return nil, fmt.Errorf("vars.%s must be a string", k)
		}
		out[k] = s
	}
	return out, nil
}

// expandVarsInMap expands every value of m in place; path is the dotted/bracketed location prefix for errors.
func expandVarsInMap(m map[string]json.RawMessage, path string, lookup func(string) (string, bool)) error {
	for k, v := range m {
		ev, err := expandVarsInJSON(v, joinConfigPath(path, k), lookup)
		if err != nil {
			return err
		}
		m[k] = ev
	}
	return nil
}

// joinConfigPath appends a key to a dotted/bracketed config path ("routes" -> "routes[0]" -> "routes[0].api").
func joinConfigPath(path, key string) string {
	if path == "" {
		return key
	}
	return path + "." + key
}

// expandVarsInJSON expands references in every string value reachable from node (object values and array
// elements). Non-string scalars pass through as their original bytes; containers holding them re-marshal with
// each RawMessage embedded verbatim, so numeric text is never round-tripped.
func expandVarsInJSON(node json.RawMessage, path string, lookup func(string) (string, bool)) (json.RawMessage, error) {
	trimmed := bytes.TrimSpace(node)
	if len(trimmed) == 0 {
		return node, nil
	}
	switch trimmed[0] {
	case '"':
		var s string
		if err := json.Unmarshal(trimmed, &s); err != nil {
			return node, nil
		}
		out, err := expandVarRefs(s, path, lookup)
		if err != nil {
			return nil, err
		}
		b, _ := json.Marshal(out)
		return json.RawMessage(b), nil
	case '{':
		var m map[string]json.RawMessage
		if err := json.Unmarshal(trimmed, &m); err != nil {
			return node, nil
		}
		if err := expandVarsInMap(m, path, lookup); err != nil {
			return nil, err
		}
		b, _ := json.Marshal(m)
		return json.RawMessage(b), nil
	case '[':
		var a []json.RawMessage
		if err := json.Unmarshal(trimmed, &a); err != nil {
			return node, nil
		}
		for i, v := range a {
			ev, err := expandVarsInJSON(v, fmt.Sprintf("%s[%d]", path, i), lookup)
			if err != nil {
				return nil, err
			}
			a[i] = ev
		}
		b, _ := json.Marshal(a)
		return json.RawMessage(b), nil
	default:
		return node, nil
	}
}

// expandVarRefs replaces $NAME and ${NAME} references using lookup. A name is ASCII letters/digits/underscore
// starting with a letter or underscore. '$' not introducing a well-formed reference stays literal; a well-formed
// reference to an undefined name is an error naming the config path.
func expandVarRefs(s, path string, lookup func(string) (string, bool)) (string, error) {
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); {
		if s[i] != '$' || i+1 >= len(s) {
			b.WriteByte(s[i])
			i++
			continue
		}
		var name string
		var n int // length of the whole "$name" token consumed
		if s[i+1] == '{' {
			rest := s[i+2:]
			end := strings.IndexByte(rest, '}')
			if end < 0 {
				b.WriteByte(s[i])
				i++
				continue // Unclosed brace: literal
			}
			if !isVarName(rest[:end]) {
				b.WriteByte(s[i])
				i++
				continue // Malformed braced name: literal
			}
			name, n = rest[:end], end+3
		} else {
			j := i + 1
			for j < len(s) && isVarNameChar(s[j]) {
				j++
			}
			if j == i+1 || !isVarNameStart(s[i+1]) {
				b.WriteByte(s[i])
				i++
				continue // Not a well-formed reference: literal
			}
			name, n = s[i+1:j], j-i
		}
		v, ok := lookup(name)
		if !ok {
			return "", fmt.Errorf("undefined variable %q in %s", name, path)
		}
		b.WriteString(v)
		i += n
	}
	return b.String(), nil
}

func isVarNameStart(c byte) bool {
	return c == '_' || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
}

func isVarNameChar(c byte) bool {
	return isVarNameStart(c) || (c >= '0' && c <= '9')
}

func isVarName(s string) bool {
	if s == "" || !isVarNameStart(s[0]) {
		return false
	}
	for i := 1; i < len(s); i++ {
		if !isVarNameChar(s[i]) {
			return false
		}
	}
	return true
}

// expandRoutePatterns desugars routes[] entries whose pattern is an array into N adjacent copies (one pattern per
// copy, every other field identical): an array entry is exactly equivalent to hand-written duplicate routes, so all
// matchers, cache anchors, and validations keep seeing plain string patterns. Runs on the raw JSON tree after vars
// expansion, so $VAR references inside pattern arrays are already resolved. Single-string, missing, and null patterns
// pass through untouched; an empty array or a non-string member is an error naming the route (1-based).
func expandRoutePatterns(top map[string]json.RawMessage) error {
	raw, ok := top["routes"]
	if !ok {
		return nil
	}
	var entries []json.RawMessage
	if err := json.Unmarshal(raw, &entries); err != nil {
		return nil // Not an array: leave it for the final struct decode to report
	}
	out := make([]json.RawMessage, 0, len(entries))
	for i, entry := range entries {
		trimmed := bytes.TrimSpace(entry)
		if len(trimmed) == 0 || trimmed[0] != '{' {
			out = append(out, entry)
			continue
		}
		var m map[string]json.RawMessage
		if err := json.Unmarshal(trimmed, &m); err != nil {
			out = append(out, entry)
			continue
		}
		pr, ok := m["pattern"]
		if !ok {
			out = append(out, entry)
			continue
		}
		pr = bytes.TrimSpace(pr)
		if len(pr) == 0 || pr[0] != '[' {
			out = append(out, entry) // String / null / anything non-array: keep today's behavior
			continue
		}
		var pats []json.RawMessage
		if err := json.Unmarshal(pr, &pats); err != nil {
			out = append(out, entry)
			continue
		}
		if len(pats) == 0 {
			return fmt.Errorf("route #%d pattern array is empty", i+1)
		}
		for _, p := range pats {
			pt := bytes.TrimSpace(p)
			// First-byte check instead of unmarshaling into a string: Go's unmarshal of null into a string
			// succeeds silently, which would let [null] through.
			if len(pt) == 0 || pt[0] != '"' {
				return fmt.Errorf("route #%d pattern must be a string or an array of strings", i+1)
			}
			cp := make(map[string]json.RawMessage, len(m))
			for k, v := range m {
				cp[k] = v
			}
			cp["pattern"] = p
			b, err := json.Marshal(cp)
			if err != nil {
				return err
			}
			out = append(out, b)
		}
	}
	b, err := json.Marshal(out)
	if err != nil {
		return err
	}
	top["routes"] = b
	return nil
}

// removedConfigKeys lists top-level config keys removed in this version, each with its migration hint. A config
// containing them still loads and runs (the removed keys stay inert — their old behavior is NOT emulated); they surface
// as warnings (startup log lines naming the key, red tray icon; the console Docs describe the migration). Only the web
// save handler rejects them outright (HTTP 400), so a removed key never gets written back to disk from the console.
var removedConfigKeys = []struct{ key, hint string }{
	{"classifier_thinking_disabled", `set "classifier_thinking": "off" inside classifier_route instead`},
	{"classifier_max_tokens", `max_tokens is no longer modified on classifier requests`},
	{"translateNone2Low", `set "convertOff2Low": "translate" or "all" on routes[]/fast_route/multimodal_fallback/search_fallback entries instead`},
	{"search_debug_dir", `search-summary two-step mode was removed; search requests are ordinary forwarded streams, fully inspectable in the console`},
}

// RemovedKeyWarningEN renders the stable English warning line for a removed key (startup log, web save rejection).
func RemovedKeyWarningEN(key string) string {
	for _, rk := range removedConfigKeys {
		if rk.key == key {
			return fmt.Sprintf("config key %q was removed: %s", key, rk.hint)
		}
	}
	return fmt.Sprintf("config key %q was removed", key)
}

// ParseConfig parses and validates config bytes (shared by LoadConfig and the web save probe). Removed keys come back as
// non-fatal warnings (the config still loads; removed keys are simply inert), while malformed JSON and invalid values of
// live keys come back as errors. Defaults are applied and caveats logged.
func ParseConfig(data []byte) (*Config, []string, error) {
	var top map[string]json.RawMessage
	if err := json.Unmarshal(data, &top); err != nil {
		return nil, nil, err
	}
	var warnings []string
	for _, rk := range removedConfigKeys {
		if _, ok := top[rk.key]; ok {
			warnings = append(warnings, rk.key)
		}
	}
	// Resolve $VAR references before struct decoding: the in-memory Config carries final values while the file
	// (and the web console, which edits raw text) keeps the references verbatim.
	if err := expandConfigVars(top); err != nil {
		return nil, nil, err
	}
	if err := expandRoutePatterns(top); err != nil {
		return nil, nil, err
	}
	// lazyApply and its nested enhance_search are the only strictly decoded objects: a typo there would
	// silently disable a global default. Decode it separately so the error names the exact path.
	var lazyApply *LazyApplyConfig
	var lazyApplyPresence ConfigPresence
	if raw, ok := top["lazyApply"]; ok {
		var err error
		lazyApply, lazyApplyPresence, err = decodeLazyApply(raw)
		if err != nil {
			return nil, nil, err
		}
	}
	full, err := json.Marshal(top)
	if err != nil {
		return nil, nil, err
	}
	var c Config
	if err := json.Unmarshal(full, &c); err != nil {
		return nil, nil, err
	}
	if lazyApply != nil {
		lazyApply.Presence = lazyApplyPresence
	}
	c.LazyApply = lazyApply
	attachConfigPresence(top, &c)
	if err := ValidateConfigEnums(&c); err != nil {
		return nil, nil, err
	}
	if c.UpstreamHeaderTimeoutSec <= 0 {
		c.UpstreamHeaderTimeoutSec = 70 // Default 70s: resend internally if the upstream's first byte times out
	}
	if c.PingIntervalSec <= 0 {
		c.PingIntervalSec = 5 // Default 5s: SSE ping keep-alive interval sent to the client during 429 retries
	}
	if c.RecentSampleWindow <= 0 {
		c.RecentSampleWindow = 20 // Default: first-token latency and token/s over the last 20 requests
	}
	if c.Upstream == "" && !HasCatchAllRoute(c.Routes) {
		log.Printf("[config] warning: upstream is empty and routes has no pattern:\"*\" catch-all; unmatched requests will get 502")
	}
	for i := range c.Routes {
		if IsReservedRoutePattern(c.Routes[i].Pattern) {
			log.Printf("[config] warning: route #%d pattern exactly matches reserved name %q (Codex menu reserved names: * catch-all=Fallback, fast lane=fast_route); this route is disabled, please rename it", i+1, c.Routes[i].Pattern)
		}
	}
	return &c, warnings, nil
}

// keysPresence turns a decoded JSON object into key-presence metadata. A nil map (JSON null) becomes an
// empty non-nil map, so "present but no keys" stays distinguishable from "no metadata at all".
func keysPresence(m map[string]json.RawMessage) ConfigPresence {
	p := make(ConfigPresence, len(m))
	for k := range m {
		p[k] = struct{}{}
	}
	return p
}

// decodeLazyApply strictly decodes the top-level lazyApply object and its nested enhance_search object.
// Unknown fields (including connection fields such as url/api/model/pattern/url_response_api) are rejected
// with a path-specific error; the rest of the configuration keeps its existing tolerant decoding.
func decodeLazyApply(raw json.RawMessage) (*LazyApplyConfig, ConfigPresence, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return nil, nil, nil
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(trimmed, &m); err != nil {
		return nil, nil, fmt.Errorf("lazyApply: %w", err)
	}
	if er, ok := m["enhance_search"]; ok {
		if et := bytes.TrimSpace(er); len(et) > 0 && !bytes.Equal(et, []byte("null")) {
			var es EnhanceSearchConfig
			dec := json.NewDecoder(bytes.NewReader(et))
			dec.DisallowUnknownFields()
			if err := dec.Decode(&es); err != nil {
				return nil, nil, fmt.Errorf("lazyApply.enhance_search: %w", err)
			}
		}
	}
	var la LazyApplyConfig
	dec := json.NewDecoder(bytes.NewReader(trimmed))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&la); err != nil {
		return nil, nil, fmt.Errorf("lazyApply: %w", err)
	}
	return &la, keysPresence(m), nil
}

// attachConfigPresence copies the raw expanded JSON's key presence onto the typed configuration objects.
// It runs after both the variable expansion and the route-pattern-array expansion, so every expanded route
// receives the same Presence as its source object.
func attachConfigPresence(top map[string]json.RawMessage, c *Config) {
	attach := func(key string, dst *ConfigPresence) {
		raw, ok := top[key]
		if !ok {
			return
		}
		var m map[string]json.RawMessage
		if err := json.Unmarshal(raw, &m); err != nil {
			return
		}
		*dst = keysPresence(m)
	}
	if raw, ok := top["routes"]; ok {
		var entries []json.RawMessage
		if json.Unmarshal(raw, &entries) == nil {
			for i := range c.Routes {
				if i >= len(entries) {
					break
				}
				var m map[string]json.RawMessage
				if err := json.Unmarshal(entries[i], &m); err != nil {
					c.Routes[i].Presence = ConfigPresence{}
					continue
				}
				c.Routes[i].Presence = keysPresence(m)
			}
		}
	}
	if c.ClassifierRoute != nil {
		attach("classifier_route", &c.ClassifierRoute.Presence)
	}
	if c.FastRoute != nil {
		attach("fast_route", &c.FastRoute.Presence)
	}
	if c.MultimodalFallback != nil {
		attach("multimodal_fallback", &c.MultimodalFallback.Presence)
	}
	if c.SearchFallback != nil {
		attach("search_fallback", &c.SearchFallback.Presence)
	}
}

// ValidateConfigEnums rejects config values outside their supported sets (route thinking shapes, route effort levels,
// classifier thinking policy, convertOff2Low scope), so a typo fails loudly at load/save time instead of silently doing nothing.
func ValidateConfigEnums(c *Config) error {
	if err := validateLazyApplyEnums(c.LazyApply); err != nil {
		return err
	}
	if cr := c.ClassifierRoute; cr != nil {
		switch cr.ClassifierThinking {
		case "", "off", "low":
		default:
			return fmt.Errorf("classifier_route has invalid classifier_thinking value %q: only \"off\" and \"low\" are supported (unset = leave the request's thinking untouched)", cr.ClassifierThinking)
		}
		switch cr.ClassifierThinkingCC {
		case "", "off", "low":
		default:
			return fmt.Errorf("classifier_route has invalid classifier_thinking_cc value %q: only \"off\" and \"low\" are supported (unset = fall back to classifier_thinking)", cr.ClassifierThinkingCC)
		}
		switch cr.ClassifierThinkingCodex {
		case "", "off", "low":
		default:
			return fmt.Errorf("classifier_route has invalid classifier_thinking_codex value %q: only \"off\" and \"low\" are supported (unset = fall back to classifier_thinking)", cr.ClassifierThinkingCodex)
		}
	}
	checkOff2Low := func(owner, v string) error {
		switch v {
		case "", "translate", "all":
			return nil
		default:
			return fmt.Errorf("%s has invalid convertOff2Low value %q: only \"translate\" / \"all\" are supported (unset = off)", owner, v)
		}
	}
	for i := range c.Routes {
		switch c.Routes[i].Thinking {
		case "", "auto", "adaptive", "budget":
		default:
			return fmt.Errorf("route #%d (pattern %q) has invalid thinking value %q: only auto / adaptive / budget are supported", i+1, c.Routes[i].Pattern, c.Routes[i].Thinking)
		}
		switch c.Routes[i].Effort {
		case "", "low", "mid", "high", "max":
		default:
			return fmt.Errorf("route #%d (pattern %q) has invalid effort value %q: only low / mid / high / max are supported (unset = leave the request's effort untouched)", i+1, c.Routes[i].Pattern, c.Routes[i].Effort)
		}
		switch c.Routes[i].ClaudeSearchThinking {
		case "", "off", "low", "mid", "high", "max":
		default:
			return fmt.Errorf("route #%d (pattern %q) has invalid claudeSearchThinking value %q: only off / low / mid / high / max are supported (unset = leave the request's thinking untouched)", i+1, c.Routes[i].Pattern, c.Routes[i].ClaudeSearchThinking)
		}
		switch c.Routes[i].ClaudeSearchSummary {
		case "", "noForce", "forceLow", "forceMid", "forceHigh":
		default:
			return fmt.Errorf("route #%d (pattern %q) has invalid claudeSearchSummary value %q: only noForce / forceLow / forceMid / forceHigh are supported (unset = no summary-prompt injection)", i+1, c.Routes[i].Pattern, c.Routes[i].ClaudeSearchSummary)
		}
		if err := checkOff2Low(fmt.Sprintf("route #%d (pattern %q)", i+1, c.Routes[i].Pattern), c.Routes[i].ConvertOff2Low); err != nil {
			return err
		}
	}
	if c.FastRoute != nil {
		if err := checkOff2Low("fast_route", c.FastRoute.ConvertOff2Low); err != nil {
			return err
		}
	}
	if c.MultimodalFallback != nil {
		if err := checkOff2Low("multimodal_fallback", c.MultimodalFallback.ConvertOff2Low); err != nil {
			return err
		}
	}
	if c.SearchFallback != nil {
		if err := checkOff2Low("search_fallback", c.SearchFallback.ConvertOff2Low); err != nil {
			return err
		}
		switch c.SearchFallback.Thinking {
		case "", "auto", "adaptive", "budget":
		default:
			return fmt.Errorf("search_fallback has invalid thinking value %q: only auto / adaptive / budget are supported", c.SearchFallback.Thinking)
		}
		switch c.SearchFallback.ClaudeSearchThinking {
		case "", "off", "low", "mid", "high", "max":
		default:
			return fmt.Errorf("search_fallback has invalid claudeSearchThinking value %q: only off / low / mid / high / max are supported (unset = leave the request's thinking untouched)", c.SearchFallback.ClaudeSearchThinking)
		}
		switch c.SearchFallback.ClaudeSearchSummary {
		case "", "noForce", "forceLow", "forceMid", "forceHigh":
		default:
			return fmt.Errorf("search_fallback has invalid claudeSearchSummary value %q: only noForce / forceLow / forceMid / forceHigh are supported (unset = no summary-prompt injection)", c.SearchFallback.ClaudeSearchSummary)
		}
	}
	return nil
}

// validateDefaultSummaryLevel accepts exactly the levels summaryLevelConfig consumes (including its aliases).
func validateDefaultSummaryLevel(owner, level string) error {
	switch level {
	case "", "low", "mid", "medium", "high", "max", "full":
		return nil
	default:
		return fmt.Errorf("%s has invalid summary_level value %q: only low / mid / medium / high / max / full are supported", owner, level)
	}
}

// validateLazyApplyEnums validates every enum in lazyApply, including the nested enhance_search object.
func validateLazyApplyEnums(la *LazyApplyConfig) error {
	if la == nil {
		return nil
	}
	switch la.Thinking {
	case "", "auto", "adaptive", "budget":
	default:
		return fmt.Errorf("lazyApply has invalid thinking value %q: only auto / adaptive / budget are supported", la.Thinking)
	}
	switch la.ConvertOff2Low {
	case "", "translate", "all":
	default:
		return fmt.Errorf("lazyApply has invalid convertOff2Low value %q: only \"translate\" / \"all\" are supported (unset = off)", la.ConvertOff2Low)
	}
	switch la.ClaudeSearchThinking {
	case "", "off", "low", "mid", "high", "max":
	default:
		return fmt.Errorf("lazyApply has invalid claudeSearchThinking value %q: only off / low / mid / high / max are supported", la.ClaudeSearchThinking)
	}
	switch la.ClaudeSearchSummary {
	case "", "noForce", "forceLow", "forceMid", "forceHigh":
	default:
		return fmt.Errorf("lazyApply has invalid claudeSearchSummary value %q: only noForce / forceLow / forceMid / forceHigh are supported", la.ClaudeSearchSummary)
	}
	for _, f := range []struct{ name, value string }{
		{"classifier_thinking", la.ClassifierThinking},
		{"classifier_thinking_cc", la.ClassifierThinkingCC},
		{"classifier_thinking_codex", la.ClassifierThinkingCodex},
	} {
		switch f.value {
		case "", "off", "low":
		default:
			return fmt.Errorf("lazyApply has invalid %s value %q: only \"off\" and \"low\" are supported", f.name, f.value)
		}
	}
	if err := validateDefaultSummaryLevel("lazyApply.summary_level", la.SummaryLevel); err != nil {
		return err
	}
	if la.EnhanceSearch != nil {
		if err := validateDefaultSummaryLevel("lazyApply.enhance_search.summary_level", la.EnhanceSearch.SummaryLevel); err != nil {
			return err
		}
	}
	return nil
}
