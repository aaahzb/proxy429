package config

// Config is the proxy's configuration, mirroring config.json.
type Config struct {
	Listen                   string            `json:"listen"`
	Upstream                 string            `json:"upstream"`
	Vars                     map[string]string `json:"vars,omitempty"` // Named string variables: any string value in this file can reference them as $NAME / ${NAME}; resolved at load (the file keeps the references)
	MaxRetries               int               `json:"max_retries"`
	BaseDelaySec             float64           `json:"base_delay_s"`
	MaxDelaySec              float64           `json:"max_delay_s"`
	TotalBudgetSec           float64           `json:"total_budget_s"`
	RetryStatusCodes         []int             `json:"retry_status_codes"`
	RespectRetryAfter        bool              `json:"respect_retry_after"`
	UpstreamHeaderTimeoutSec float64           `json:"upstream_header_timeout_s"` // Max seconds to wait for the upstream's first byte; on timeout the request is considered stuck and resent internally. 0 = default 70s
	PingIntervalSec          float64           `json:"ping_interval_s"`           // Interval (seconds) for SSE ping keep-alives sent to the client during 429 retries; 0 = default 5s
	LogRequestDetail         bool              `json:"log_request_detail"`
	LogFile                  string            `json:"log_file"`                      // Log file path; when set, logs are also appended to this file so they can be viewed/copied without a console (e.g. RemoteApp)
	RecentSampleWindow       int               `json:"recent_sample_window"`          // Window size for the status row's "last X" latency/throughput stats; 0 = default 20
	LazyApply                *LazyApplyConfig  `json:"lazyApply,omitempty"`           // Global defaults for behavior fields shared by route-like objects; a local explicit value wins, noLazyApply opts an object out
	Routes                   []RouteRule       `json:"routes"`                        // Model routing rules; empty = no routing, use the default upstream
	ClassifierRoute          *ClassifierRoute  `json:"classifier_route,omitempty"`    // Dedicated route for classifier requests; classifier hits are routed here regardless of the original model; empty = disabled
	FastRoute                *FastRoute        `json:"fast_route,omitempty"`          // Dedicated route for fast-mode requests; non-classifier requests carrying "speed":"fast" are routed here; empty = disabled
	MultimodalFallback       *MultimodalRoute  `json:"multimodal_fallback,omitempty"` // Multimodal fallback route; requests containing images that hit a text_only model are rerouted here; empty = disabled
	SearchFallback           *SearchRoute      `json:"search_fallback,omitempty"`     // Search fallback route; requests with search tools that hit a no_search upstream are rerouted here; empty = disabled
	ConvertAllToStream       bool              `json:"convertAlltoStream"`            // Global stream conversion: when on, all non-streaming requests are sent upstream as streams, then rebuilt into a single non-streaming JSON response (transparent to the client; the web console shows live output/first-token/tok/s)
	ResponsesListen          string            `json:"responses_listen"`              // OpenAI Responses API listener (e.g. 127.0.0.1:8081); empty = disabled. Translates Responses-protocol requests into Anthropic and feeds the main pipeline, for Codex CLI and similar tools. Starts/stops dynamically on save/reload

	// AllowNoThinkBlock4Anthropic (Responses translation port only; nil/unset = true): a tool-continuation whose history has no
	// replayable signed thinking block gets 400'd by real Anthropic when thinking is on. true (default) = send the requested thinking
	// mode anyway; the main handler's one-shot fallback retries with thinking off if the upstream really rejects it. false = cc-switch
	// mode: thinking preemptively off over such histories. Explicit thinking-off requests never take this door (convertOff2Low still
	// governs those); the native port doesn't check history at all.
	AllowNoThinkBlock4Anthropic *bool `json:"allowNoThinkBlock4Anthropic,omitempty"`
}

// The web console UI language is a program-level preference, not routing config: stored in program-settings.txt
// in the config directory (same family as active-config.txt, see below); config files no longer carry ui_lang (legacy field migrated once at startup).

// AllowNoThinkBlock4Anthropic resolves the Config toggle: nil config / nil field = true (the try-first default; see the field's comment).
func AllowNoThinkBlock4Anthropic(c *Config) bool {
	return c == nil || c.AllowNoThinkBlock4Anthropic == nil || *c.AllowNoThinkBlock4Anthropic
}

// RouteRule defines one model route: matched requests go to the specified upstream with the model name and API key replaced.
// Pattern wildcards the model name with *; on hit, URL overrides the default upstream, API overrides the client token, Model rewrites the request body's model field.
type RouteRule struct {
	Pattern        string               `json:"pattern"`                    // Model-name wildcard, only * supported (matches any characters of any length), e.g. "claude-opus*"
	URL            string               `json:"url"`                        // Target upstream base URL, e.g. https://api.deepseek.com
	API            string               `json:"api"`                        // Target API key, sent as Authorization: Bearer; empty = pass through the client's token
	Model          string               `json:"model"`                      // Target model name to rewrite to; empty = leave the model field unchanged
	TextOnly       bool                 `json:"text_only"`                  // Target model is text-only; requests containing images fall back to multimodal_fallback
	NoSearch       bool                 `json:"no_search"`                  // Target upstream does not support search; requests with search tools fall back to search_fallback
	EnhanceSearch  *EnhanceSearchConfig `json:"enhance_search,omitempty"`   // Enhanced search: enabled when non-nil. Standalone search sub-agent requests get the summary instruction injected into the request itself, served by this route's own upstream (single-request mode)
	URLResponseAPI string               `json:"url_response_api,omitempty"` // Native Responses API upstream base URL: when set, Responses-port requests hitting this route are passed through untranslated (affects only the Responses port; Anthropic-port traffic still uses url). Implemented but not battle-tested, hence undocumented
	Thinking       string               `json:"thinking,omitempty"`         // Thinking shape of the target model (Responses translation only): ""/"auto" = look up by client model name; "adaptive" = force adaptive+effort; "budget" = force enabled+budget_tokens
	ConvertOff2Low string               `json:"convertOff2Low,omitempty"`   // Per-route thinking-off conversion: ""/unset = off (requests go through untouched); "translate" = Responses translation port only; "all" = translation port + Anthropic native port. When on, an explicit downstream thinking-off is quietly sent upstream as low thinking, with thinking blocks stripped from the response (client unaware)

	ClaudeSearchThinking          string `json:"claudeSearchThinking,omitempty"`          // Standalone search sub-agent requests hitting this route: rewrite thinking to off/low/mid/high/max; unset = fall back (to enhance_search legacy when this route serves, to search_fallback's own setting when diverted there)
	ClaudeSearchSummary           string `json:"claudeSearchSummary,omitempty"`           // Same requests: forceLow/forceMid/forceHigh inject the matching summary instruction, noForce disables injection; unset = fall back as above. A set value beats this route's enhance_search legacy fields and (per field) search_fallback's own
	NoBullshitOnEmptyClaudeSearch *bool  `json:"noBullshitOnEmptyClaudeSearch,omitempty"` // Same requests: replace an all-empty web-search response with a fixed no-results stream; unset = off here, or inherit search_fallback when diverted. Explicit false beats a fallback true
	NoLazyApply                   bool   `json:"noLazyApply,omitempty"`                   // This route ignores every lazyApply default; its own explicit fields stay authoritative

	Presence ConfigPresence `json:"-"` // Source-key presence captured after variable/pattern expansion
}

// EnhanceSearchConfig holds optional enhanced-search parameters inside a routes entry. When the route hits and the request carries
// search tools, standalone search sub-agent requests get the summary instruction injected into the search request itself
// (single-request mode), served by the route's own url/api/model — the upstream's response passes through untouched.
// Equivalent to no_search via search_fallback, except the search upstream is the route's own.
type EnhanceSearchConfig struct {
	SummaryThinking bool   `json:"summary_thinking"` // Rewrite the injected search request's thinking to low (default off)
	SummaryLevel    string `json:"summary_level"`    // Injected summary instruction's detail level: low (default) / mid / high / max
}

// ClassifierRoute defines the dedicated route for classifier requests: requests matching the classifier (safety check) signature
// are routed to the specified upstream regardless of the original model. Used to push Claude Code's lightweight safety checks to a cheap model, saving main-model quota.
type ClassifierRoute struct {
	URL                     string `json:"url"`                                 // Target upstream base URL
	API                     string `json:"api"`                                 // Target API key; empty = pass through the client's token
	Model                   string `json:"model"`                               // Target model name to rewrite to; empty = leave the model field unchanged
	ClassifierThinking      string `json:"classifier_thinking,omitempty"`       // Shared classifier thinking policy: ""/unset = leave the request's thinking untouched; "off" = rewrite the body to thinking-off so classification returns fast; "low" = lower adaptive thinking to effort low (non-adaptive shapes pass through untouched)
	ClassifierThinkingCC    string `json:"classifier_thinking_cc,omitempty"`    // Claude Code override of classifier_thinking; unset = fall back to the shared value
	ClassifierThinkingCodex string `json:"classifier_thinking_codex,omitempty"` // Codex guardian override of classifier_thinking; unset = fall back to the shared value
	NoLazyApply             bool   `json:"noLazyApply,omitempty"`               // This route ignores every lazyApply default; its own explicit fields stay authoritative

	Presence ConfigPresence `json:"-"` // Source-key presence captured after variable/pattern expansion
}

// FastRoute defines the dedicated route for fast-mode requests: non-classifier requests carrying "speed":"fast" go to the specified upstream.
// Claude Code /fast doesn't change the model name, only speeds up output; the proxy injects fake fast rate-limit headers into responses.
type FastRoute struct {
	URL            string `json:"url"`                      // Target upstream base URL
	API            string `json:"api"`                      // Target API key; empty = pass through the client's token
	Model          string `json:"model"`                    // Target model name to rewrite to; empty = leave the model field unchanged
	ConvertOff2Low string `json:"convertOff2Low,omitempty"` // Same semantics as RouteRule.ConvertOff2Low
	NoLazyApply    bool   `json:"noLazyApply,omitempty"`    // This route ignores every lazyApply default

	Presence ConfigPresence `json:"-"` // Source-key presence captured after variable expansion
}

// MultimodalRoute defines the multimodal fallback: when a request contains images but hits a text_only text-only model,
// it is automatically rerouted to the multimodal upstream specified here.
type MultimodalRoute struct {
	URL            string `json:"url"`                      // Target upstream base URL
	API            string `json:"api"`                      // Target API key; empty = pass through the client's token
	Model          string `json:"model"`                    // Target model name to rewrite to; empty = leave the model field unchanged
	ConvertOff2Low string `json:"convertOff2Low,omitempty"` // Same semantics as RouteRule.ConvertOff2Low
	NoLazyApply    bool   `json:"noLazyApply,omitempty"`    // This route ignores every lazyApply default

	Presence ConfigPresence `json:"-"` // Source-key presence captured after variable expansion
}

// SearchRoute defines the search fallback: when a request carries search tools but hits a no_search search-incapable upstream,
// it is automatically rerouted to the search-capable upstream specified here.
type SearchRoute struct {
	URL             string `json:"url"`                      // Target upstream base URL
	API             string `json:"api"`                      // Target API key; empty = pass through the client's token
	Model           string `json:"model"`                    // Target model name to rewrite to; empty = leave the model field unchanged
	ConvertOff2Low  string `json:"convertOff2Low,omitempty"` // Same semantics as RouteRule.ConvertOff2Low
	Thinking        string `json:"thinking,omitempty"`       // Thinking shape of the target model (Responses translation only): ""/"auto" = look up by client model name; "adaptive" = force adaptive+effort; "budget" = force enabled+budget_tokens
	SummaryMode     bool   `json:"summary_mode"`             // Standalone search sub-agent requests: inject the summary instruction (at summary_level) into the search request itself — single request, the upstream's response passed through (legacy; claudeSearchSummary overrides)
	SummaryThinking bool   `json:"summary_thinking"`         // Legacy thinking switch for search sub-agent requests: true rewrites thinking to low (claudeSearchThinking overrides)
	SummaryLevel    string `json:"summary_level"`            // Summary detail under summary_mode: low (default, brief) / mid (medium) / high (thorough) / max (verbatim steps/code/formulas)

	ClaudeSearchThinking          string `json:"claudeSearchThinking,omitempty"`          // Standalone search sub-agent requests: rewrite thinking to off / low / mid / high / max (beats summary_thinking; unset = legacy behavior)
	ClaudeSearchSummary           string `json:"claudeSearchSummary,omitempty"`           // Standalone search sub-agent requests: forceLow / forceMid / forceHigh inject the matching summary instruction into the search request itself; noForce disables injection (beats summary_mode; unset = legacy behavior)
	NoBullshitOnEmptyClaudeSearch *bool  `json:"noBullshitOnEmptyClaudeSearch,omitempty"` // Standalone search sub-agent requests: replace an all-empty web-search response with a fixed no-results stream; unset = off unless the matched route explicitly overrides it
	NoLazyApply                   bool   `json:"noLazyApply,omitempty"`                   // This fallback ignores every lazyApply default; its own explicit fields stay authoritative

	Presence ConfigPresence `json:"-"` // Source-key presence captured after variable expansion
}

// ConfigPresence records which JSON object keys were actually present, so lazyApply resolution can
// distinguish an omitted key from an explicit false / "" / null. A nil map means "no JSON metadata"
// (programmatic Go literals keep the existing non-zero-means-set convention); a non-nil empty map
// means a real JSON object that contained no recognized keys.
type ConfigPresence map[string]struct{}

// Written reports whether key was present in the source JSON. Without Presence metadata it falls
// back to the caller-provided non-zero test, keeping Go-literal configs working.
func (p ConfigPresence) Written(key string, nonzero bool) bool {
	if p == nil {
		return nonzero
	}
	_, ok := p[key]
	return ok
}

// LazyApplyConfig supplies defaults for behavior fields that route-like objects already accept.
// Resolution is per field: an explicitly written local value wins over lazyApply, and an omitted
// local key inherits lazyApply. noLazyApply on a route-like object disables all of its defaults.
type LazyApplyConfig struct {
	TextOnly       bool   `json:"text_only,omitempty"`      // Default for routes[].text_only
	NoSearch       bool   `json:"no_search,omitempty"`      // Default for routes[].no_search
	Thinking       string `json:"thinking,omitempty"`       // Default for routes[].thinking (Responses translation pre-match)
	ConvertOff2Low string `json:"convertOff2Low,omitempty"` // Default for every route-like object that accepts convertOff2Low

	EnhanceSearch *EnhanceSearchConfig `json:"enhance_search,omitempty"` // Default direct-route enhance_search (object; a local object or null wins whole)

	SummaryMode     bool   `json:"summary_mode,omitempty"`     // Default for search_fallback.summary_mode
	SummaryThinking bool   `json:"summary_thinking,omitempty"` // Default for search_fallback.summary_thinking
	SummaryLevel    string `json:"summary_level,omitempty"`    // Default for search_fallback.summary_level

	ClaudeSearchThinking          string `json:"claudeSearchThinking,omitempty"`          // Default for matching claudeSearchThinking fields
	ClaudeSearchSummary           string `json:"claudeSearchSummary,omitempty"`           // Default for matching claudeSearchSummary fields
	NoBullshitOnEmptyClaudeSearch *bool  `json:"noBullshitOnEmptyClaudeSearch,omitempty"` // Default for matching empty-search guard fields

	ClassifierThinking      string `json:"classifier_thinking,omitempty"`       // Default for classifier_route.classifier_thinking
	ClassifierThinkingCC    string `json:"classifier_thinking_cc,omitempty"`    // Default for classifier_route.classifier_thinking_cc
	ClassifierThinkingCodex string `json:"classifier_thinking_codex,omitempty"` // Default for classifier_route.classifier_thinking_codex

	Presence ConfigPresence `json:"-"` // Source-key presence captured after variable expansion
}
