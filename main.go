package main

import (
	"bufio"
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"math"
	"math/rand"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"fyne.io/systray"
)

// Config is the proxy's configuration, mirroring config.json.
type Config struct {
	Listen                   string           `json:"listen"`
	Upstream                 string           `json:"upstream"`
	MaxRetries               int              `json:"max_retries"`
	BaseDelaySec             float64          `json:"base_delay_s"`
	MaxDelaySec              float64          `json:"max_delay_s"`
	TotalBudgetSec           float64          `json:"total_budget_s"`
	RetryStatusCodes         []int            `json:"retry_status_codes"`
	RespectRetryAfter        bool             `json:"respect_retry_after"`
	UpstreamHeaderTimeoutSec float64          `json:"upstream_header_timeout_s"` // Max seconds to wait for the upstream's first byte; on timeout the request is considered stuck and resent internally. 0 = default 70s
	PingIntervalSec          float64          `json:"ping_interval_s"`           // Interval (seconds) for SSE ping keep-alives sent to the client during 429 retries; 0 = default 5s
	LogRequestDetail         bool             `json:"log_request_detail"`
	LogFile                  string           `json:"log_file"`                      // Log file path; when set, logs are also appended to this file so they can be viewed/copied without a console (e.g. RemoteApp)
	RecentSampleWindow       int              `json:"recent_sample_window"`          // Window size for the status row's "last X" latency/throughput stats; 0 = default 20
	Routes                   []RouteRule      `json:"routes"`                        // Model routing rules; empty = no routing, use the default upstream
	ClassifierRoute          *ClassifierRoute `json:"classifier_route,omitempty"`    // Dedicated route for classifier requests; classifier hits are routed here regardless of the original model; empty = disabled
	FastRoute                *FastRoute       `json:"fast_route,omitempty"`          // Dedicated route for fast-mode requests; non-classifier requests carrying "speed":"fast" are routed here; empty = disabled
	MultimodalFallback       *MultimodalRoute `json:"multimodal_fallback,omitempty"` // Multimodal fallback route; requests containing images that hit a text_only model are rerouted here; empty = disabled
	SearchFallback           *SearchRoute     `json:"search_fallback,omitempty"`     // Search fallback route; requests with search tools that hit a no_search upstream are rerouted here; empty = disabled
	SearchDebugDir           string           `json:"search_debug_dir,omitempty"`    // Search debug directory; when set, the raw request/response of each search-summary step is written there for troubleshooting
	ConvertAllToStream       bool             `json:"convertAlltoStream"`            // Global stream conversion: when on, all non-streaming requests are sent upstream as streams, then rebuilt into a single non-streaming JSON response (transparent to the client; the web console shows live output/first-token/tok/s)
	ResponsesListen          string           `json:"responses_listen"`              // OpenAI Responses API listener (e.g. 127.0.0.1:8081); empty = disabled. Translates Responses-protocol requests into Anthropic and feeds the main pipeline, for Codex CLI and similar tools. Starts/stops dynamically on save/reload

	// AllowNoThinkBlock4Anthropic (Responses translation port only; nil/unset = true): a tool-continuation whose history has no
	// replayable signed thinking block gets 400'd by real Anthropic when thinking is on. true (default) = send the requested thinking
	// mode anyway; the main handler's one-shot fallback retries with thinking off if the upstream really rejects it. false = cc-switch
	// mode: thinking preemptively off over such histories. Explicit thinking-off requests never take this door (convertOff2Low still
	// governs those); the native port doesn't check history at all.
	AllowNoThinkBlock4Anthropic *bool `json:"allowNoThinkBlock4Anthropic,omitempty"`
}

// The web console UI language is a program-level preference, not routing config: stored in program-settings.txt
// in the config directory (same family as active-config.txt, see below); config files no longer carry ui_lang (legacy field migrated once at startup).

// allowNoThinkBlock4Anthropic resolves the Config toggle: nil config / nil field = true (the try-first default; see the field's comment).
func allowNoThinkBlock4Anthropic(c *Config) bool {
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
	EnhanceSearch  *EnhanceSearchConfig `json:"enhance_search,omitempty"`   // Enhanced search: enabled when non-nil. For requests with search tools, skip the main model and use this route's own upstream in kimi summary mode
	URLResponseAPI string               `json:"url_response_api,omitempty"` // Native Responses API upstream base URL: when set, Responses-port requests hitting this route are passed through untranslated (affects only the Responses port; Anthropic-port traffic still uses url). Implemented but not battle-tested, hence undocumented
	Thinking       string               `json:"thinking,omitempty"`         // Thinking shape of the target model (Responses translation only): ""/"auto" = look up by client model name; "adaptive" = force adaptive+effort; "budget" = force enabled+budget_tokens
	ConvertOff2Low string               `json:"convertOff2Low,omitempty"`   // Per-route thinking-off conversion: ""/unset = off (requests go through untouched); "translate" = Responses translation port only; "all" = translation port + Anthropic native port. When on, an explicit downstream thinking-off is quietly sent upstream as low thinking, with thinking blocks stripped from the response (client unaware)
}

// EnhanceSearchConfig holds optional enhanced-search parameters inside a routes entry. When the route hits and the request carries search tools,
// if EnhanceSearch is non-nil, skip the main model and use the route's own url/api/model in kimi summary mode
// (step1 search + step2 summary + synthesized response). Equivalent to no_search via search_fallback.summary_mode,
// except the search upstream is the route's own. The SummaryMode field only marks internal conversion to SearchRoute.
type EnhanceSearchConfig struct {
	SummaryThinking bool   `json:"summary_thinking"` // Whether step2 summary uses thinking (default off, faster summaries)
	SummaryLevel    string `json:"summary_level"`    // Summary detail level: low (default) / mid / high / max
}

// ClassifierRoute defines the dedicated route for classifier requests: requests matching the classifier (safety check) signature
// are routed to the specified upstream regardless of the original model. Used to push Claude Code's lightweight safety checks to a cheap model, saving main-model quota.
type ClassifierRoute struct {
	URL                string `json:"url"`                           // Target upstream base URL
	API                string `json:"api"`                           // Target API key; empty = pass through the client's token
	Model              string `json:"model"`                         // Target model name to rewrite to; empty = leave the model field unchanged
	ClassifierThinking string `json:"classifier_thinking,omitempty"` // Classifier thinking policy: ""/unset = leave the request's thinking untouched; "off" = rewrite the body to thinking-off so classification returns fast
}

// FastRoute defines the dedicated route for fast-mode requests: non-classifier requests carrying "speed":"fast" go to the specified upstream.
// Claude Code /fast doesn't change the model name, only speeds up output; the proxy injects fake fast rate-limit headers into responses.
type FastRoute struct {
	URL            string `json:"url"`                      // Target upstream base URL
	API            string `json:"api"`                      // Target API key; empty = pass through the client's token
	Model          string `json:"model"`                    // Target model name to rewrite to; empty = leave the model field unchanged
	ConvertOff2Low string `json:"convertOff2Low,omitempty"` // Same semantics as RouteRule.ConvertOff2Low
}

// MultimodalRoute defines the multimodal fallback: when a request contains images but hits a text_only text-only model,
// it is automatically rerouted to the multimodal upstream specified here.
type MultimodalRoute struct {
	URL            string `json:"url"`                      // Target upstream base URL
	API            string `json:"api"`                      // Target API key; empty = pass through the client's token
	Model          string `json:"model"`                    // Target model name to rewrite to; empty = leave the model field unchanged
	ConvertOff2Low string `json:"convertOff2Low,omitempty"` // Same semantics as RouteRule.ConvertOff2Low
}

// SearchRoute defines the search fallback: when a request carries search tools but hits a no_search search-incapable upstream,
// it is automatically rerouted to the search-capable upstream specified here.
type SearchRoute struct {
	URL             string `json:"url"`                      // Target upstream base URL
	API             string `json:"api"`                      // Target API key; empty = pass through the client's token
	Model           string `json:"model"`                    // Target model name to rewrite to; empty = leave the model field unchanged
	ConvertOff2Low  string `json:"convertOff2Low,omitempty"` // Same semantics as RouteRule.ConvertOff2Low
	SummaryMode     bool   `json:"summary_mode"`             // Search sub-agent requests: step1 search + step2 detailed summary, synthesize a standard web_search response for the client (no whole-request forwarding, no main model)
	SummaryThinking bool   `json:"summary_thinking"`         // Whether the step2 summary request uses thinking under summary_mode (default off, faster summaries)
	SummaryLevel    string `json:"summary_level"`            // Summary detail under summary_mode: low (default, brief) / mid (medium) / high (thorough)
}

// Version is the proxy version, injected at build time via -ldflags "-X main.Version=<git-short>"; default dev.
var Version = "dev"

// debugSearchDirOverride is injected via ldflags (debug builds): forces search-summary step raws into this directory, ignoring config.search_debug_dir.
var debugSearchDirOverride string

var cfg atomic.Pointer[Config]

// configFilePath is the config file path, set at main startup and reused by tray reload.
// Rewritten by switchConfig on config switches, hence guarded by configMu for concurrent access.
var configFilePath string

// configMu guards concurrent access to configFilePath:
// config switching (write) and reload/web config reads (read) can race; the lock avoids reading a half-updated path.
var configMu sync.RWMutex

// currentConfigPath returns the current config file path under lock.
func currentConfigPath() string {
	configMu.RLock()
	defer configMu.RUnlock()
	return configFilePath
}

// listConfigFiles scans the current config directory for .json files (excluding config.example.json)
// and returns the sorted file names (no directories). Feeds the tray "switch config" submenu and the web dropdown.
func listConfigFilesIn(dir string) []string {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var files []string
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if !strings.HasSuffix(name, ".json") || name == "config.example.json" {
			continue
		}
		files = append(files, name)
	}
	sort.Strings(files)
	return files
}

// listConfigFiles lists every .json config in the current config directory (for the web "switch config" menu).
func listConfigFiles() []string {
	return listConfigFilesIn(filepath.Dir(currentConfigPath()))
}

// configWarnings holds the names of removed config keys present in the running config (empty = clean). Set at startup
// and on every successful reload/switch; read by the tray poll (red icon + tooltip).
var configWarnings atomic.Value // []string

// setConfigWarnings replaces the removed-keys warning set (nil/empty clears it).
func setConfigWarnings(keys []string) {
	configWarnings.Store(keys)
}

// getConfigWarnings returns the removed-keys warning set (nil when clean).
func getConfigWarnings() []string {
	v, _ := configWarnings.Load().([]string)
	return v
}

// loadConfig reads and parses the config file and applies defaults. Shared by main startup and tray reload.
// Removed config keys are non-fatal warnings: the proxy starts and runs with them inert (old behavior is NOT emulated);
// only malformed JSON or invalid values of live keys come back as errors.
func loadConfig(path string) (*Config, []string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, nil, err
	}
	return parseConfig(data)
}

// removedConfigKeys lists top-level config keys removed in this version, each with its migration hint. A config
// containing them still loads and runs (the removed keys stay inert — their old behavior is NOT emulated); they surface
// as warnings (startup log lines naming the key, red tray icon; the console Docs describe the migration). Only the web
// save handler rejects them outright (HTTP 400), so a removed key never gets written back to disk from the console.
var removedConfigKeys = []struct{ key, hint string }{
	{"classifier_thinking_disabled", `set "classifier_thinking": "off" inside classifier_route instead`},
	{"classifier_max_tokens", `max_tokens is no longer modified on classifier requests`},
	{"translateNone2Low", `set "convertOff2Low": "translate" or "all" on routes[]/fast_route/multimodal_fallback/search_fallback entries instead`},
}

// removedKeyWarningEN renders the stable English warning line for a removed key (startup log, web save rejection).
func removedKeyWarningEN(key string) string {
	for _, rk := range removedConfigKeys {
		if rk.key == key {
			return fmt.Sprintf("config key %q was removed: %s", key, rk.hint)
		}
	}
	return fmt.Sprintf("config key %q was removed", key)
}

// parseConfig parses and validates config bytes (shared by loadConfig and the web save probe). Removed keys come back as
// non-fatal warnings (the config still loads; removed keys are simply inert), while malformed JSON and invalid values of
// live keys come back as errors. Defaults are applied and caveats logged.
func parseConfig(data []byte) (*Config, []string, error) {
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
	var c Config
	if err := json.Unmarshal(data, &c); err != nil {
		return nil, nil, err
	}
	if err := validateConfigEnums(&c); err != nil {
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
	if c.Upstream == "" && !hasCatchAllRoute(c.Routes) {
		log.Printf("[config] warning: upstream is empty and routes has no pattern:\"*\" catch-all; unmatched requests will get 502")
	}
	for i := range c.Routes {
		if isReservedRoutePattern(c.Routes[i].Pattern) {
			log.Printf("[config] warning: route #%d pattern exactly matches reserved name %q (Codex menu reserved names: * catch-all=Fallback, fast lane=fast_route); this route is disabled, please rename it", i+1, c.Routes[i].Pattern)
		}
	}
	return &c, warnings, nil
}

// validateConfigEnums rejects config values outside their supported sets (route thinking shapes, classifier thinking
// policy, convertOff2Low scope), so a typo fails loudly at load/save time instead of silently doing nothing.
func validateConfigEnums(c *Config) error {
	if cr := c.ClassifierRoute; cr != nil {
		switch cr.ClassifierThinking {
		case "", "off":
		default:
			return fmt.Errorf("classifier_route has invalid classifier_thinking value %q: only \"off\" is supported (unset = leave the request's thinking untouched)", cr.ClassifierThinking)
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
	}
	return nil
}

// defaultCacheTTL is the fixed protection window for finished-stream trimming: the newest row of each anchor key
// cannot be evicted by finishedCap within 5 minutes of stream start. The upstream cache lifetime is empirically
// dynamic (see cache observations), so there is no configurable "TTL" — the cache_time option is gone,
// leaving this one fixed window, enough to cover typical conversation pacing.
const defaultCacheTTL = 5 * time.Minute

// configExampleBytes is the embedded default config template, written to the user config directory on first run.
//
//go:embed config.example.json
var configExampleBytes []byte

// codexSetupPS1 / codexSetupSH are the embedded Codex one-click setup script templates (Windows / macOS·Linux),
// served via /__codexsetup.ps1 and /__codexsetup.sh: the server bakes their BAKED anchors per query params
// (model/base/catalog) before sending; the web console only hands the user a one-line fetch command (same format as DeepSeek's docs).
//
//go:embed codex-setup.ps1
var codexSetupPS1 []byte

//go:embed codex-setup.sh
var codexSetupSH []byte

// activeConfigStateFile records the last selected routing config's file name (basename), stored next to the routing configs.
// A .txt extension (not .json) keeps it out of listConfigFiles results and avoids clashing with user-created .json files.
const activeConfigStateFile = "active-config.txt"

// classifierSystemPrefix is the fixed system-field prefix of Claude Code classifier (safety check) requests.
// The proxy uses it to recognize classifier requests, route them to a cheap model, and disable thinking. Hardcoded: a client implementation detail that users shouldn't configure.
const classifierSystemPrefix = "You are a security monitor"

// readActiveConfigState reads the state file in the config directory and returns the full path of the last selected config it records.
// Returns an empty string when the state file is missing, empty, or points to a file that no longer exists; the caller falls back to the default config.json.
func readActiveConfigState(dir string) string {
	data, err := os.ReadFile(filepath.Join(dir, activeConfigStateFile))
	if err != nil {
		return ""
	}
	name := strings.TrimSpace(string(data))
	if name == "" {
		return ""
	}
	full := filepath.Join(dir, name)
	if _, err := os.Stat(full); err != nil {
		return ""
	}
	return full
}

// writeActiveConfigState writes the given config's file name (basename) into the state file in its directory, for restore on next launch.
// Write failures are only logged; the switch itself is unaffected.
func writeActiveConfigState(path string) {
	statePath := filepath.Join(filepath.Dir(path), activeConfigStateFile)
	if err := os.WriteFile(statePath, []byte(filepath.Base(path)), 0644); err != nil {
		log.Printf("[config] failed to write state file: %v", err)
	}
}

// programSettingsFile is the dedicated carrier of program-level settings: preferences like the UI language that
// have nothing to do with any upstream stay separate from routing config and never enter config*.json (legacy versions
// wrote ui_lang into the config file; since migrated out). The file lives in the current config directory, same family
// as active-config.txt; line-based key=value, lines starting with # are comments, extensible.
const programSettingsFile = "program-settings.txt"

// programSettingsPath returns the full path of the program-settings file (follows the current config directory).
func programSettingsPath() string {
	return filepath.Join(filepath.Dir(currentConfigPath()), programSettingsFile)
}

// readProgramSetting returns the value of key in program settings (trimmed); missing file or key returns "".
func readProgramSetting(key string) string {
	data, err := os.ReadFile(programSettingsPath())
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if k, v, ok := strings.Cut(line, "="); ok && strings.TrimSpace(k) == key {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

// writeProgramSetting sets (non-empty value) or deletes (empty value) a program-settings key.
// All other lines (comments, order, formatting) are preserved byte-for-byte; a missing key with a non-empty value is appended at the end.
// Always LF line endings with a trailing newline; write failures are returned for the caller to surface.
func writeProgramSetting(key, value string) error {
	data, _ := os.ReadFile(programSettingsPath())
	lines := strings.Split(strings.ReplaceAll(string(data), "\r\n", "\n"), "\n")
	out := make([]string, 0, len(lines)+2)
	replaced := false
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if trimmed != "" && !strings.HasPrefix(trimmed, "#") {
			if k, _, ok := strings.Cut(trimmed, "="); ok && strings.TrimSpace(k) == key {
				replaced = true
				if value != "" {
					out = append(out, key+"="+value)
				}
				continue // value empty: delete the whole line
			}
		}
		out = append(out, line)
	}
	if !replaced && value != "" {
		for len(out) > 0 && out[len(out)-1] == "" { // Strip trailing blank lines before appending, so the key doesn't drag blank lines along
			out = out[:len(out)-1]
		}
		out = append(out, key+"="+value, "")
	}
	return os.WriteFile(programSettingsPath(), []byte(strings.Join(out, "\n")), 0644)
}

// resolveProgramUILang determines the web console UI language at startup: program-settings.txt's ui-lang
// wins; otherwise a one-time migration moves any legacy ui_lang left in config files into program-settings.txt
// and deletes it from the config (setTopLevelJSONValue text-level surgery, every other byte untouched); otherwise follow the OS.
// After migration, no code path writes to existing config files programmatically (the config editor saves the user's verbatim text).
func resolveProgramUILang() {
	if l := readProgramSetting("ui-lang"); l == "zh" || l == "en" {
		applyUILang(l)
		return
	}
	if data, err := os.ReadFile(currentConfigPath()); err == nil {
		var probe struct {
			UILang string `json:"ui_lang"` // Legacy field: only used for migration; the Config struct no longer carries it
		}
		if json.Unmarshal(data, &probe) == nil && (probe.UILang == "zh" || probe.UILang == "en") {
			if err := writeProgramSetting("ui-lang", probe.UILang); err != nil {
				log.Printf("[lang] failed to write %s: %v", programSettingsFile, err)
			}
			if out, ok := setTopLevelJSONValue(data, "ui_lang", nil); ok {
				if err := os.WriteFile(currentConfigPath(), out, 0644); err != nil {
					log.Printf("[lang] failed to remove migrated ui_lang from %s: %v", filepath.Base(currentConfigPath()), err)
				}
			}
			log.Printf("[lang] migrated ui_lang=%q from %s to %s", probe.UILang, filepath.Base(currentConfigPath()), programSettingsFile)
			applyUILang(probe.UILang)
			return
		}
	}
	applyUILang("")
}

// resolveConfigPath decides the config file path, by priority:
//  1. -config explicitly given (highest, used as-is)
//  2. ./config.json exists (terminal running in the project directory)
//  3. user config directory proxy429/ (.app double-click, post-install runs)
//
// Rule 3 lets a double-clicked macOS .app find its config: a .app's cwd is / with no ./config.json,
// so it lands on ~/Library/Application Support/proxy429/ (macOS),
// ~/.config/proxy429/ (Linux), %AppData%/proxy429/ (Windows).
//
// Once the directory is settled, restore order:
//   - the config recorded in active-config.txt still exists → restore it;
//   - else any .json config already in the directory → use the first (alphabetical), don't auto-generate config.json;
//   - else first run: no config in the directory → write the embedded default template as config.json.
//
// This way users who never used the name config.json (only their own file names) won't find a surprise standard template after a restart.
func resolveConfigPath(flagPath string) string {
	if flagPath != "" {
		return flagPath
	}
	var dir, defaultPath string
	if _, err := os.Stat("config.json"); err == nil {
		dir = "."
		defaultPath = "config.json"
	} else {
		ud, err := os.UserConfigDir()
		if err != nil || ud == "" {
			return "config.json"
		}
		dir = filepath.Join(ud, "proxy429")
		defaultPath = filepath.Join(dir, "config.json")
	}
	// Restore the last selected config (if the file still exists), so a restart returns to the config in use at shutdown.
	if active := readActiveConfigState(dir); active != "" {
		return active
	}
	// The directory already holds other config files (user runs non-config.json names) — don't auto-generate config.json,
	// fall back to the first config in the directory (alphabetical).
	if files := listConfigFilesIn(dir); len(files) > 0 {
		return filepath.Join(dir, files[0])
	}
	// First run: no config in the directory — create it and write the embedded default template, avoiding a config-less startup failure after a .app double-click.
	// The user then sets their own upstream/key in the web "Config" tab.
	if dir != "." {
		if mkErr := os.MkdirAll(dir, 0755); mkErr == nil {
			if wErr := os.WriteFile(defaultPath, configExampleBytes, 0644); wErr == nil {
				log.Printf("[config] first run; default config created: %s", defaultPath)
			}
		}
	}
	return defaultPath
}

// clearStats clears cumulative stats, latency/throughput samples, and the "recently finished streams" list (including each one's archived pass-through content),
// and rebuilds sample capacity per the given config's RecentSampleWindow. In-flight streams and stream numbering are kept (avoids id collisions).
// Called only by the web "clear stats" button (switches/reloads don't clear stats; stats persist across configs).
func clearStats(c *Config) {
	stats.mu.Lock()
	stats.cacheRead = 0
	stats.cacheCreation = 0
	stats.inputTokens = 0
	stats.outputTokens = 0
	stats.modelStats = nil
	stats.mu.Unlock()
	stats.bytesForward.Store(0)
	stats.statusRetries.Store(0)
	stats.classifierRewrites.Store(0)
	stats.classifierHits.Store(0)
	stats.resetSampleCap(c.RecentSampleWindow)
	finishedMu.Lock()
	finished = nil
	cacheObsMap = map[string]*cacheObsEntry{} // Clear the empirical cache-lifetime observations too
	finishedMu.Unlock()
}

// reloadConfig re-reads the current config file and atomically swaps the global cfg, without clearing stats (only the "clear stats" button clears).
// On failure the global cfg keeps the old config. Removed keys in the reloaded file succeed but refresh the warning set (red tray icon).
func reloadConfig() error {
	c, warns, err := loadConfig(currentConfigPath())
	if err != nil {
		log.Printf("[reload] failed: %v", err)
		return err
	}
	cfg.Store(c)
	setConfigWarnings(warns)
	reconcileResponsesServer(c.ResponsesListen) // Responses port starts/stops dynamically with config reload
	log.Printf("[reload] config reloaded: http://%s -> %s (max retries %d, classifier thinking-off=%v, removed-key warnings=%d)",
		c.Listen, c.Upstream, c.MaxRetries, classifierThinkingOff(c), len(warns))
	return nil
}

// switchConfig switches to the given config file with immediate effect: after loading succeeds, configFilePath + cfg.Store are updated.
// Stats are NOT cleared (they persist across configs; the "clear stats" button clears independently). On failure configFilePath stays and the old config keeps running.
func switchConfig(newPath string) error {
	c, warns, err := loadConfig(newPath)
	if err != nil {
		log.Printf("[switch] failed to load %s: %v", newPath, err)
		return err
	}
	configMu.Lock()
	configFilePath = newPath
	configMu.Unlock()
	writeActiveConfigState(newPath)
	cfg.Store(c)
	setConfigWarnings(warns)
	reconcileResponsesServer(c.ResponsesListen) // Responses port starts/stops dynamically with the config
	// Notify the tray to rebuild the "switch config" submenu and refresh checkmarks (web-initiated switches don't go through tray clicks)
	notifyTrayCfgChanged()
	log.Printf("[switch] switched to %s: http://%s -> %s (max retries %d)",
		filepath.Base(newPath), c.Listen, c.Upstream, c.MaxRetries)
	return nil
}

// client has no total timeout (streaming responses can be long); first-byte timeout is Transport.ResponseHeaderTimeout
// (set in main from upstream_header_timeout_s); a timeout goes to case-0 resend. Retry pacing is governed by total_budget_s.
var client = &http.Client{
	Timeout: 0,
}

// ---- Live status row ----
// Token counters from concurrent requests aggregate into the global stats; a dedicated goroutine refreshes one line
// in place every 100ms (\r to line start + \033[K to clear the tail), adding no log lines. log output goes through
// clearLineWriter: the status row is cleared before writing a log, so logs scroll up and the status row stays last.

// throughputSample records one successful stream's streaming duration and output, windowed at stream end for the status row's token/s.
// First-token latency uses a separate []int64 ring buffer, windowed on the first body byte, so "first token" updates in real time without waiting for stream end.
type throughputSample struct {
	streamMs     int64 // Streaming duration: first byte -> last byte
	outputTokens int64 // This stream's output_tokens
}

// modelUsage is one real upstream model's cumulative token usage, aggregated by model name for the status page breakdown.
type modelUsage struct {
	cacheRead     int64 // Cache-hit tokens (cache_read_input_tokens)
	cacheCreation int64 // Cache-write tokens (cache_creation_input_tokens)
	input         int64 // Miss tokens (input_tokens)
	output        int64 // Output tokens
	retries       int64 // Retries triggered for this model (network errors/status codes/in-body errors)
}

// liveStats aggregates counters across all streams; the web console and tray status light read it for live state.
type liveStats struct {
	mu                 sync.Mutex
	active             int                    // Streams currently being passed through
	waiting            int                    // Requests sent upstream, awaiting first byte (status light yellow)
	cacheRead          int64                  // Cumulative cache-hit tokens (cache_read_input_tokens)
	cacheCreation      int64                  // Cumulative cache-write tokens (cache_creation_input_tokens)
	inputTokens        int64                  // Cumulative input tokens
	outputTokens       int64                  // Cumulative output tokens (sum of each stream's current accumulation, grows with streams)
	modelStats         map[string]*modelUsage // Token usage aggregated by real upstream model name
	bytesForward       atomic.Int64           // Cumulative forwarded bytes, growing live during streams (ARK doesn't send tokens mid-stream; this shows live progress)
	statusRetries      atomic.Int64           // Retries since startup (status codes/timeouts/network errors/in-body errors, +1 per retry)
	classifierRewrites atomic.Int64           // Classifier hits with thinking disabled since startup
	classifierHits     atomic.Int64           // Requests matching the classifier (safety-check) signature since startup: counted whether or not rerouted/de-thought

	sampleMu  sync.Mutex         // Guards the sliding windows below (separate from mu, so long streams don't hold the lock)
	fbSamples []int64            // First-token latency ring buffer (pushed on first byte; the status row's "first token" updates live)
	fbHead    int                // fbSamples next write position
	tpSamples []throughputSample // Throughput ring buffer (pushed at stream end), for token/s
	tpHead    int                // tpSamples next write position
	sampleCap int                // Ring buffer capacity (= RecentSampleWindow, shared by both windows)
}

var stats liveStats

// resetSampleCap resets the sliding-window capacity and clears existing samples. Shared by main startup and reload:
// capacity follows the config, so ring-buffer indices must reset in sync.
func (s *liveStats) resetSampleCap(cap int) {
	s.sampleMu.Lock()
	defer s.sampleMu.Unlock()
	s.fbSamples = nil
	s.fbHead = 0
	s.tpSamples = nil
	s.tpHead = 0
	s.sampleCap = cap
}

// addModelUsage adds one stream's token usage under its real upstream model name (called once at stream end).
func (s *liveStats) addModelUsage(model string, in, cr, cc, out int64) {
	if model == "" || (in == 0 && cr == 0 && cc == 0 && out == 0) {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.modelStats == nil {
		s.modelStats = make(map[string]*modelUsage)
	}
	m := s.modelStats[model]
	if m == nil {
		m = &modelUsage{}
		s.modelStats[model] = m
	}
	m.input += in
	m.cacheRead += cr
	m.cacheCreation += cc
	m.output += out
}

// addModelRetry adds one retry under its real upstream model name (called on network error/status code/in-body error).
func (s *liveStats) addModelRetry(model string) {
	if model == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.modelStats == nil {
		s.modelStats = make(map[string]*modelUsage)
	}
	m := s.modelStats[model]
	if m == nil {
		m = &modelUsage{}
		s.modelStats[model] = m
	}
	m.retries++
}

// modelUsageEntry is a JSON snapshot entry of modelStats, feeding the web per-model cache-hit breakdown.
type modelUsageEntry struct {
	Model         string `json:"model"`
	CacheRead     int64  `json:"cacheRead"`     // Cache-hit tokens
	CacheCreation int64  `json:"cacheCreation"` // Cache-write tokens (part of the hit-rate denominator)
	Input         int64  `json:"input"`         // Miss tokens
	Output        int64  `json:"output"`        // Output tokens
	Retries       int64  `json:"retries"`       // Retry count
}

// snapshotModelStats returns usage snapshots aggregated by model, sorted by total tokens (input+cacheRead+cacheCreation+output) descending.
func (s *liveStats) snapshotModelStats() []modelUsageEntry {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.modelStats) == 0 {
		return nil
	}
	out := make([]modelUsageEntry, 0, len(s.modelStats))
	for name, m := range s.modelStats {
		out = append(out, modelUsageEntry{Model: name, CacheRead: m.cacheRead, CacheCreation: m.cacheCreation, Input: m.input, Output: m.output, Retries: m.retries})
	}
	sort.Slice(out, func(i, j int) bool {
		ti := out[i].Input + out[i].CacheRead + out[i].CacheCreation + out[i].Output
		tj := out[j].Input + out[j].CacheRead + out[j].CacheCreation + out[j].Output
		return ti > tj
	})
	return out
}

// pushFirstByte writes first-token latency into the ring buffer. Called on the first body byte,
// so the status row's "first token" updates the moment streaming starts, without waiting for the whole stream.
func (s *liveStats) pushFirstByte(firstByteMs int64) {
	s.sampleMu.Lock()
	defer s.sampleMu.Unlock()
	if s.sampleCap <= 0 {
		return
	}
	if len(s.fbSamples) < s.sampleCap {
		s.fbSamples = append(s.fbSamples, firstByteMs)
		return
	}
	s.fbSamples[s.fbHead] = firstByteMs
	s.fbHead = (s.fbHead + 1) % s.sampleCap
}

// pushThroughput writes streaming duration and output_tokens into the ring buffer; called at stream end, feeds token/s.
func (s *liveStats) pushThroughput(streamMs, outputTokens int64) {
	s.sampleMu.Lock()
	defer s.sampleMu.Unlock()
	if s.sampleCap <= 0 {
		return
	}
	sm := throughputSample{streamMs: streamMs, outputTokens: outputTokens}
	if len(s.tpSamples) < s.sampleCap {
		s.tpSamples = append(s.tpSamples, sm)
		return
	}
	s.tpSamples[s.tpHead] = sm
	s.tpHead = (s.tpHead + 1) % s.sampleCap
}

// recentLatency computes the average first-token latency (ms) and token/s over the last X samples (weighted throughput:
// ΣoutputTokens / ΣstreamDuration). First-token latency comes from fbSamples (windowed on first byte),
// token/s from tpSamples (windowed at stream end). Missing samples or zero total duration yield 0 for the respective field.
func (s *liveStats) recentLatency() (avgFirstByteMs float64, tps float64) {
	s.sampleMu.Lock()
	defer s.sampleMu.Unlock()
	if len(s.fbSamples) > 0 {
		var sumFB int64
		for _, v := range s.fbSamples {
			sumFB += v
		}
		avgFirstByteMs = float64(sumFB) / float64(len(s.fbSamples))
	}
	var sumST, sumOut int64
	for _, sm := range s.tpSamples {
		sumST += sm.streamMs
		sumOut += sm.outputTokens
	}
	if sumST > 0 {
		tps = float64(sumOut) / (float64(sumST) / 1000.0)
	}
	return
}

// toInt64 safely converts a JSON-parsed number (float64 / json.Number etc.) to int64.
func toInt64(v interface{}) int64 {
	switch n := v.(type) {
	case float64:
		return int64(n)
	case int:
		return int64(n)
	case int64:
		return n
	case json.Number:
		if i, err := n.Int64(); err == nil {
			return i
		}
	}
	return 0
}

// humanNum compresses large numbers to 1.2k / 3.40M forms for single-line display.
func humanNum(n int64) string {
	if n < 0 {
		n = 0
	}
	switch {
	case n < 1000:
		return strconv.FormatInt(n, 10)
	case n < 1000000:
		return fmt.Sprintf("%.1fk", float64(n)/1000)
	default:
		return fmt.Sprintf("%.2fM", float64(n)/1000000)
	}
}

// humanBytes compresses byte counts to 2.3KB / 1.20MB forms for live traffic display.
func humanBytes(n int64) string {
	if n < 0 {
		n = 0
	}
	switch {
	case n < 1024:
		return fmt.Sprintf("%dB", n)
	case n < 1024*1024:
		return fmt.Sprintf("%.1fKB", float64(n)/1024)
	default:
		return fmt.Sprintf("%.2fMB", float64(n)/(1024*1024))
	}
}

// cacheHitRate returns the cache hit rate, matching Claude Code's cache-hit algorithm:
// cache_read / (input + cache_read + cache_creation).
// The denominator is total input (fresh + hit + written): cache writes aren't hits but consume input; omitting them inflates the rate.
// A zero denominator (no usage data, e.g. non-streaming responses or unparsed usage) returns "-".
func cacheHitRate(cacheRead, input, cacheCreation int64) string {
	denom := input + cacheRead + cacheCreation
	if denom == 0 {
		return "-"
	}
	return fmt.Sprintf("%.1f%%", float64(cacheRead)*100/float64(denom))
}

// fmtMs formats milliseconds as a seconds string; ≤0 (unrecorded) returns "-".
func fmtMs(ms int64) string {
	if ms <= 0 {
		return "-"
	}
	return fmt.Sprintf("%.2fs", float64(ms)/1000)
}

// fmtFirstByte formats first-token latency milliseconds as a seconds string; ≤0 (unrecorded, e.g. error fallback streams) returns "-".
func fmtFirstByte(ms int64) string {
	return fmtMs(ms)
}

// fmtTps formats tok/s as a string; ≤0 (no output or unrecorded) returns "-".
func fmtTps(tps float64) string {
	if tps <= 0 {
		return "-"
	}
	return fmt.Sprintf("%.1f", tps)
}

// maxLogBuf is the log ring buffer's max line count, and the scrollback limit of the web "view logs" page.
const maxLogBuf = 500

// flight tracks one in-flight request for the web console's live streams view (#id + duration/bytes).
type flight struct {
	id            uint64       // Incrementing number
	start         time.Time    // Time the downstream request arrived (flight created)
	phase         atomic.Int32 // 0=awaiting response, 1=response received (forwarding started)
	status        int          // HTTP status code (valid when phase=1)
	gaveUp        bool         // Retries/budget exhausted and a fallback error event has been passed downstream (writeSSEError sets this; status stays 0, not forged); the finished-streams status column shows [重试尽]
	bytes         atomic.Int64 // Forwarded byte count
	origModel     string       // Client's original model; used to rewrite the response stream back after routing
	targetModel   string       // Target model after routing; forward rewrites back when non-empty and ≠ origModel
	upstreamModel string       // Model actually returned in the upstream response (captured by rewriteResponseModel); empty = unknown
	modelLogged   atomic.Bool  // Whether the [改写] log line was printed; printed once
	stage         atomic.Int32 // Current stage (stage*), shown in the web in-flight "status" column
	stageStart    atomic.Int64 // When the current light color started (unixnano; the color is stage mapped via stageColor); the web shows this color's duration next to the status light
	attempt       atomic.Int32 // Current attempt number (from 1); the web shows [尝试N] when stage=stageAttempt
	attemptStart  atomic.Int64 // When the current attempt's upstream request was sent (unixnano): the timing origin of [尝试N:Xs] next to the yellow light; 0 = in retry backoff (next attempt not sent yet)
	routeReason   atomic.Int32 // Route reason for this request (route*), shown appended in the web "status" column
	delivered     atomic.Bool  // Response fully delivered downstream (pass-through saw a clean upstream EOF / assembled JSON written in one shot / synthesized stream reached message_stop); 499 re-marking only applies to incomplete streams — clients often disconnect right after receiving the full stream (Codex especially), which is not an interruption
	contentMu     sync.Mutex
	content       []byte // Last flightContentCap bytes of pass-through content (raw SSE), for the web in-flight stream viewer
	reqBody       []byte // Raw downstream request body (guarded by contentMu; with the 储存完整结构体 toggle off it's capped at flightContentCap keeping the head, on = untruncated), so the web can show "which request caused this stream"; translation-port streams store the body AFTER translation to Anthropic
	fullContent   []byte // Full pass-through content (guarded by contentMu, recorded only with the 储存完整结构体 toggle on, no cap), for downloading the raw output
	reqTruncated  bool   // Whether the request body was truncated to just the head (guarded by contentMu): over cap at record time with full-store off, or truncated by purge when the toggle was switched off; the web greys out the download button accordingly

	// Downstream side of dual-link recording (proxy↔client): stored only when it differs from the upstream side
	// (content/reqBody/fullContent above) — Responses translation streams always store (the two protocols necessarily differ),
	// convertAlltoStream-rebuilt JSON streams store contentDown, native streams store reqDown only when rewritten
	// (classifier thinking-off / routed model etc.); all empty means both sides identical, and endpoints fall back
	// to the other side, reporting the actual side via the X-Proxy429-Side header. Guarded by contentMu.
	reqDown          []byte           // Raw downstream→proxy request body (truncation/full rules same as reqBody)
	reqDownTruncated bool             // Whether reqDown was truncated to just the head
	contentDown      []byte           // Proxy→downstream response content (capped at flightContentCap, keeps the tail)
	fullContentDown  []byte           // Proxy→downstream full response (recorded only with the 储存完整结构体 toggle on)
	searchDebug      bool             // Search-summary mode: while forwarding, append the main model's response SSE to cfg.SearchDebugDir
	inTokens         int64            // This stream's cumulative input_tokens (stored from lastInput when forward ends)
	cacheRead        int64            // This stream's cumulative cache_read
	cacheCreation    int64            // This stream's cumulative cache_creation (cache writes; part of the hit-rate denominator)
	outTokens        int64            // This stream's cumulative output_tokens
	firstByteMs      int64            // This stream's first-byte latency (ms), recorded only on the normal response path
	tps              float64          // This stream's streaming tok/s, recorded only on the normal response path
	searchPrompt     string           // step2 summary instruction text (non-empty only for search-summary sub-streams), shown at the very front of the status page's in-flight/finished streams
	translated       string           // Translation-port origin tag ("responses"=translated stream, "responses-raw"=native passthrough via a route with url_response_api); the web API column shows [translate]/[Response]
	countTokens      bool             // count_tokens probe stream (countTokensPath); the response is just {"input_tokens":N}; the web model column shows a [count_tokens] prefix
	searchStripped   atomic.Int32     // Total replayed search blocks stripped (conversation-watermark strip + 400-fallback strip; the web shows a red [剥N] badge, the split only goes to the log)
	stripThinking    atomic.Bool      // convertOff2Low upgrade (off->low): thinking blocks are stripped from the response before writing downstream (the client keeps seeing a thinking-off response)
	searchReplay     *searchReplayCtx // Search-restore context of the Responses translation port (carried via ctx, read/written only by the handler goroutine); the 400-fallback strip learns the conversation watermark from restore timestamps

	// Session cache tracking (the status page's "缓存年龄" column): written once during routing, read by addFinished in the same goroutine.
	convID      string // Session identifier (Anthropic port: session_id inside metadata.user_id; Responses port: prompt_cache_key); empty = does not participate
	convAnchor  string // Anchor-key suffix ("route:<pattern>"/"classifier"/"fast", empty=default upstream): only same session + same anchor form one cache lineage
	upstreamKey string // Upstream grouping key (final base URL after routing | model actually sent; other parameters ignored): the grouping dimension of observed cache-lifetime measurements

	// think is the shortest form of the thinking config in the request body actually sent upstream ("off"/"on <budget>"/"adaptive"/effort word),
	// for the status page's 「API」 column thinking value — which API family the vocabulary belongs to is carried by the column color, not the text. Empty = the request body carried no thinking field (column shows -).
	think string

	toolMu    sync.Mutex
	toolNames []string       // Names of tool calls in the response stream (in first-appearance order; guarded by toolMu)
	toolCalls map[string]int // Call count per tool (guarded by toolMu)
	toolEmpty map[string]int // Per-tool count of calls with an empty arguments object (guarded by toolMu)
}

// realModel returns this stream's real upstream model name: prefer the one the response actually returned, fall back to the route target, then the original model.
// Used for per-model token/retry stats; matches the value logic of addModelUsage in forward.
func (f *flight) realModel() string {
	if f.upstreamModel != "" {
		return f.upstreamModel
	}
	if f.targetModel != "" {
		return f.targetModel
	}
	return f.origModel
}

// responsesRaw reports whether this stream is a Responses native passthrough (the matched route has url_response_api):
// a passthrough stream's response is Responses-protocol SSE — usage/tool counts/terminal markers are parsed by Responses semantics,
// and the web API column shows [Response] instead of [translate].
func (f *flight) responsesRaw() bool { return f.translated == translatedResponsesRaw }

// noteToolCall records one tool call (the name from a tool_use/server_tool_use in the response stream's
// content_block_start). The web's 「最近完成的流」 appends [Read*1][Edit*3]-style tags after the model column.
func (f *flight) noteToolCall(name string) {
	if name == "" {
		return
	}
	f.toolMu.Lock()
	if f.toolCalls == nil {
		f.toolCalls = make(map[string]int)
	}
	if f.toolCalls[name] == 0 {
		f.toolNames = append(f.toolNames, name) // Record first appearances only, preserving order
	}
	f.toolCalls[name]++
	f.toolMu.Unlock()
}

// noteToolCallEmpty marks one call of this tool as having an empty arguments object (for streams, decided at block end/item done,
// e.g. Kimi's no-args server_tool_use on an empty search). Affects only a single call's *0 tag; multiple same-name calls
// still show the raw count *N.
func (f *flight) noteToolCallEmpty(name string) {
	f.toolMu.Lock()
	if f.toolCalls[name] > 0 {
		if f.toolEmpty == nil {
			f.toolEmpty = make(map[string]int)
		}
		f.toolEmpty[name]++
	}
	f.toolMu.Unlock()
}

// toolCallsTag formats the recorded tool calls as "[Read*1][Edit*3][web_search*0]": in first-appearance
// order; N>1 same-name calls show *N (raw count); a single call shows *1, or *0 when its arguments object is empty —
// telling empty searches from real ones at a glance. Returns an empty string when there are no tool calls.
func (f *flight) toolCallsTag() string {
	f.toolMu.Lock()
	defer f.toolMu.Unlock()
	var b strings.Builder
	for _, n := range f.toolNames {
		suffix := 1
		if c := f.toolCalls[n]; c > 1 {
			suffix = c // Multiple calls show the raw count (the *0 empty-args check only applies to single calls)
		} else if f.toolEmpty[n] > 0 {
			suffix = 0
		}
		b.WriteByte('[')
		b.WriteString(n)
		b.WriteByte('*')
		b.WriteString(strconv.Itoa(suffix))
		b.WriteByte(']')
	}
	return b.String()
}

// flight stage enum: the progress shown in the web in-flight streams' 「状态」 column.
// stageForward shows the HTTP status code (response being passed through); other stages show their respective Chinese labels.
const (
	stageRequest int32 = iota // 请求 (request): reading body / preparing
	stageRoute                // 路由 (routing): matching route rules
	stageAttempt              // 尝试N (attempt N): sent upstream, awaiting first byte (including retry waits for the next attempt)
	stageForward              // 响应 (response): response received, passing through; shows the status code
)

// stageColor maps a stage to a web status-light color number: 0=white (request) 1=yellow (routing/awaiting first byte) 2=green (forwarding),
// one-to-one with the web flightDot palette. The duration shown next to the status light is per color, not per stage.
func stageColor(s int32) int32 {
	if s >= stageForward {
		return 2
	}
	if s >= stageRoute {
		return 1
	}
	return 0
}

// setStage advances the current stage; stageStart resets only when the light color changes (white→yellow→green each timed separately).
// Stage advances within the same color (routing→attempt, retry re-entering attempt) don't interrupt the timer, so "how long the yellow light has been on" stays continuous and truthful.
func (f *flight) setStage(s int32) {
	if stageColor(s) != stageColor(f.stage.Load()) {
		f.stageStart.Store(time.Now().UnixNano())
	}
	f.stage.Store(s)
}

// stageMs returns how many milliseconds the current light color has lasted, shown next to the web status light.
func (f *flight) stageMs() int64 {
	return time.Since(time.Unix(0, f.stageStart.Load())).Milliseconds()
}

// attemptMs returns how many milliseconds the current attempt has awaited the first byte (the web's [尝试N:Xs] next to the yellow light);
// returns -1 when attemptStart=0 (in retry backoff, next attempt not sent yet).
func (f *flight) attemptMs() int64 {
	t := f.attemptStart.Load()
	if t == 0 {
		return -1
	}
	return time.Since(time.Unix(0, t)).Milliseconds()
}

// flight route-reason enum: appended after the stage in the web 「状态」 column (e.g. 「尝试1·搜索」).
// routePassthrough is the default zero value: no route matched, pass through to the default upstream.
const (
	routePassthrough int32 = iota // 透传 (passthrough): no route matched
	routePattern                  // pattern: matched a routes[] wildcard
	routeClassifier               // 分类器 (classifier): classifier_route
	routeFast                     // fast: fast_route
	routeMultimodal               // 多模态 (multimodal): multimodal_fallback
	routeSearch                   // 搜索 (search): search_fallback
)

// flightRegistry manages all in-flight requests.
type flightRegistry struct {
	mu     sync.RWMutex
	m      map[uint64]*flight
	nextID atomic.Uint64
}

var flights flightRegistry

func (fr *flightRegistry) register(f *flight) {
	fr.mu.Lock()
	fr.m[f.id] = f
	fr.mu.Unlock()
}

func (fr *flightRegistry) unregister(id uint64) {
	fr.mu.Lock()
	delete(fr.m, id)
	fr.mu.Unlock()
}

func (fr *flightRegistry) snapshot() []*flight {
	fr.mu.RLock()
	defer fr.mu.RUnlock()
	out := make([]*flight, 0, len(fr.m))
	for _, f := range fr.m {
		out = append(out, f)
	}
	// Sort by ID for a stable render order.
	sort.Slice(out, func(i, j int) bool { return out[i].id < out[j].id })
	return out
}

// flightContentCap caps one in-flight stream's content buffer: only the most recent bytes are kept for the web viewer,
// avoiding memory bloat from long streams. 256KB covers the full output of most streams; longer ones keep only the tail.
const flightContentCap = 256 * 1024

// appendContent appends a chunk of pass-through bytes to the flight's content buffer (keeps only the tail when over the cap).
// Called in forward's writeAndCount after w.Write — tees a copy for the web viewer without affecting pass-through.
func (f *flight) appendContent(data []byte) {
	f.contentMu.Lock()
	defer f.contentMu.Unlock()
	if fullStore.Load() {
		f.fullContent = append(f.fullContent, data...) // Full copy: uncapped, for download
	}
	f.content = append(f.content, data...)
	if len(f.content) > flightContentCap {
		f.content = f.content[len(f.content)-flightContentCap:]
		// The cut point may land mid-SSE-line, leaving half a JSON at the start (e.g. `ext"}}`).
		// Align to the next event boundary (blank line), dropping the incomplete leading event so the raw view starts with a complete SSE line.
		if i := bytes.Index(f.content, []byte("\n\n")); i >= 0 {
			f.content = f.content[i+2:]
		}
	}
}

// snapshotContent returns a copy of the current content buffer, for the web endpoints.
func (f *flight) snapshotContent() []byte {
	f.contentMu.Lock()
	defer f.contentMu.Unlock()
	out := make([]byte, len(f.content))
	copy(out, f.content)
	return out
}

// setReqBody records the raw downstream request body. With the 储存完整结构体 toggle off, an over-cap body is truncated keeping the head
// (the model/system/tools at the head locate a request better than the tail); with it on, no truncation, for full download.
// Called once when the handler finishes reading the body; never changes afterwards.
func (f *flight) setReqBody(b []byte) {
	truncated := false
	if !fullStore.Load() && len(b) > flightContentCap {
		b = b[:flightContentCap]
		truncated = true
	}
	f.contentMu.Lock()
	f.reqBody = b
	f.reqTruncated = truncated
	f.contentMu.Unlock()
}

// snapshotReqBody returns a copy of the raw request body (nil if not recorded), for the web endpoints.
func (f *flight) snapshotReqBody() []byte {
	f.contentMu.Lock()
	defer f.contentMu.Unlock()
	if f.reqBody == nil {
		return nil
	}
	out := make([]byte, len(f.reqBody))
	copy(out, f.reqBody)
	return out
}

// snapshotFullContent returns a copy of the full pass-through content (nil if not recorded), for the download endpoint.
func (f *flight) snapshotFullContent() []byte {
	f.contentMu.Lock()
	defer f.contentMu.Unlock()
	if f.fullContent == nil {
		return nil
	}
	out := make([]byte, len(f.fullContent))
	copy(out, f.fullContent)
	return out
}

// purgeFull releases the full copies when the 储存完整结构体 toggle is switched off: fullContent is set to nil, reqBody cut back to the cap
// (the cut sets the reqTruncated flag; the web greys out the download button accordingly — a truncated copy must not be downloadable as a full one).
func (f *flight) purgeFull() {
	f.contentMu.Lock()
	f.fullContent = nil
	if len(f.reqBody) > flightContentCap {
		f.reqBody = f.reqBody[:flightContentCap]
		f.reqTruncated = true
	}
	// Downstream-side mirror: the full copy is likewise cleared and the downstream request body likewise cut back to the cap.
	f.fullContentDown = nil
	if len(f.reqDown) > flightContentCap {
		f.reqDown = f.reqDown[:flightContentCap]
		f.reqDownTruncated = true
	}
	f.contentMu.Unlock()
}

// reqTrunc returns whether the request body was truncated to just the head, for the web endpoints.
func (f *flight) reqTrunc() bool {
	f.contentMu.Lock()
	defer f.contentMu.Unlock()
	return f.reqTruncated
}

// ---- Downstream side of dual-link recording (proxy↔client) ----
// Semantics per the flight struct field comments: recorded only for streams that differ from the upstream side (translation streams always record,
// rebuilt-JSON streams record contentDown, native streams record reqDown only when rewritten); the rules mirror the upstream-side methods one to one.

// appendContentDown appends a chunk of proxy→downstream bytes to the downstream-side content buffer (keeps the tail over cap, aligns to SSE boundaries).
func (f *flight) appendContentDown(data []byte) {
	f.contentMu.Lock()
	defer f.contentMu.Unlock()
	if fullStore.Load() {
		f.fullContentDown = append(f.fullContentDown, data...)
	}
	f.contentDown = append(f.contentDown, data...)
	if len(f.contentDown) > flightContentCap {
		f.contentDown = f.contentDown[len(f.contentDown)-flightContentCap:]
		// Same as appendContent: when the cut lands mid-SSE-line, align to the next event boundary.
		if i := bytes.Index(f.contentDown, []byte("\n\n")); i >= 0 {
			f.contentDown = f.contentDown[i+2:]
		}
	}
}

// snapshotContentDown returns a copy of the downstream-side content buffer (empty if not recorded), for the web endpoints.
func (f *flight) snapshotContentDown() []byte {
	f.contentMu.Lock()
	defer f.contentMu.Unlock()
	out := make([]byte, len(f.contentDown))
	copy(out, f.contentDown)
	return out
}

// setReqDown records the raw downstream→proxy request body; truncation rules same as setReqBody.
func (f *flight) setReqDown(b []byte) {
	truncated := false
	if !fullStore.Load() && len(b) > flightContentCap {
		b = b[:flightContentCap]
		truncated = true
	}
	f.contentMu.Lock()
	f.reqDown = b
	f.reqDownTruncated = truncated
	f.contentMu.Unlock()
}

// snapshotReqDown returns a copy of the downstream-side raw request body (nil if not recorded).
func (f *flight) snapshotReqDown() []byte {
	f.contentMu.Lock()
	defer f.contentMu.Unlock()
	if f.reqDown == nil {
		return nil
	}
	out := make([]byte, len(f.reqDown))
	copy(out, f.reqDown)
	return out
}

// snapshotFullContentDown returns a copy of the downstream-side full response (nil if not recorded), for the download endpoint.
func (f *flight) snapshotFullContentDown() []byte {
	f.contentMu.Lock()
	defer f.contentMu.Unlock()
	if f.fullContentDown == nil {
		return nil
	}
	out := make([]byte, len(f.fullContentDown))
	copy(out, f.fullContentDown)
	return out
}

// hasReqDown / hasContentDown / reqDownTrunc / hasFullContentDown feed the web endpoints'
// downstream-side availability (viewer side-switching and download greying).
func (f *flight) hasReqDown() bool {
	f.contentMu.Lock()
	defer f.contentMu.Unlock()
	return f.reqDown != nil
}

// hasContentDown returns whether the downstream-side response has any content yet (nil and empty both count as not recorded).
func (f *flight) hasContentDown() bool {
	f.contentMu.Lock()
	defer f.contentMu.Unlock()
	return len(f.contentDown) > 0
}

// reqDownTrunc returns whether the downstream-side request body was truncated to just the head.
func (f *flight) reqDownTrunc() bool {
	f.contentMu.Lock()
	defer f.contentMu.Unlock()
	return f.reqDownTruncated
}

// hasFullContentDown returns whether the downstream-side full output copy is still held.
func (f *flight) hasFullContentDown() bool {
	f.contentMu.Lock()
	defer f.contentMu.Unlock()
	return f.fullContentDown != nil
}

// hasFullContent returns whether the full output copy is still held, for the web to judge download/interact button availability.
func (f *flight) hasFullContent() bool {
	f.contentMu.Lock()
	defer f.contentMu.Unlock()
	return f.fullContent != nil
}

// finishedFlight is an archived snapshot of an ended flight, for the web to review recently finished stream output.
// Archived only when the flight ends with non-empty content (there was streamed output); content is a snapshotContent copy.
type finishedFlight struct {
	id            uint64
	model         string // origModel -> targetModel (-> upstreamModel if the upstream returned one and it differs)
	upstreamModel string // Model name actually returned in the upstream response (empty = not returned / same as targetModel)
	routeReason   int32  // Route reason (route*); the web model column shows it as a [tag] prefix
	status        int    // HTTP status code
	gaveUp        bool   // Retries/budget exhausted and a fallback error event has been passed through (status is 0 at this point); the web status column shows [重试尽]
	attempts      int32  // Attempt count (=retries+1; 1 = succeeded first try); the web status column appends [重试N次] when >1
	bytes         int64
	stage         int32 // Stage at end
	ended         time.Time
	totalMs       int64  // Total duration: downstream request received to response fully sent back downstream (= internal processing like translation/routing/buffering + first-byte wait + streaming + closing internal processing)
	content       []byte // Pass-through content copy, capped at flightContentCap
	reqBody       []byte // Raw downstream request body copy, capped at flightContentCap (uncapped with the 储存完整结构体 toggle on; nil if not recorded)
	fullContent   []byte // Full pass-through content copy (recorded only with the 储存完整结构体 toggle on; nil if not recorded)
	reqTruncated  bool   // Whether the request body was truncated to just the head (for the web to grey out the download button)
	// Dual-link downstream-side (proxy↔client) mirror: populated only for streams that differ from the upstream side (rules per the flight field comments).
	reqDown          []byte  // Raw downstream→proxy request body (nil = not recorded; endpoints fall back to the other side)
	reqDownTruncated bool    // Whether reqDown was truncated to just the head
	contentDown      []byte  // Proxy→downstream response content (nil = not recorded)
	fullContentDown  []byte  // Proxy→downstream full response (nil = not recorded)
	inTokens         int64   // This stream's cumulative input_tokens
	cacheRead        int64   // This stream's cumulative cache_read
	cacheCreation    int64   // This stream's cumulative cache_creation
	outTokens        int64   // This stream's cumulative output_tokens
	firstByteMs      int64   // This stream's first-byte latency (ms)
	tps              float64 // This stream's streaming tok/s
	searchPrompt     string  // step2 summary instruction text (non-empty only for search-summary sub-streams)
	translated       string  // Translation-port origin tag ("responses"=translated, "responses-raw"=native passthrough); the web API column shows [translate]/[Response]
	countTokens      bool    // count_tokens probe stream; the web model column shows a [count_tokens] prefix
	tools            string  // Tool-call tag ("[Read*1][Edit*3]", empty when no tools), appended in the web model column
	searchStripped   int     // Total replayed search blocks stripped (conversation-watermark strip + 400-fallback strip; 0 = none); the web cache-hit column shows a red [剥N]
	convKey          string  // Session cache anchor key (convID|convAnchor); empty = this stream joins neither the "缓存年龄" display nor trim protection
	// start anchors the stream start time: cache writes/refreshes happen when the upstream processes the input (≈stream start); both "缓存年龄" and the trim-protection window anchor to it.
	start       time.Time
	obsKey      string // Observed cache-lifetime pairing key (convID|upstreamKey); empty = no observation (yellow-light 499 / no session / no grouping key)
	upstreamKey string // Upstream grouping key (post-routing url|model): the 「缓存命中」 popup's observed table groups by it
	think       string // Shortest form of the thinking config actually sent upstream (status page 「API」 column thinking value); empty = request body carried no thinking field
}

var (
	finishedMu  sync.Mutex
	finished    []finishedFlight
	finishedCap atomic.Int32 // How many finished streams to keep; adjustable on the status page; init sets 10
)

// fullStore is the 储存完整结构体 toggle (switchable on the status page, default off, resets on restart):
// on = flights additionally record full request bodies/output (no 256KB cap) for download; off = full copies are dropped immediately.
var fullStore atomic.Bool

func init() {
	finishedCap.Store(10)
}

// markClientGone corrects the status code to upstream conventions when the flight is archived (goal: match the upstream provider's dashboard):
//   - upstream finished sending (delivered): keep 200 whether or not downstream is still there (the upstream dashboard also shows 200);
//   - upstream stream started but not finished (status==200, not delivered): record 499 — a downstream cancel propagates via ctx into an upstream
//     disconnect, and the upstream dashboard likewise records 499 (nginx's client-closed-request convention);
//   - ended before any upstream response due to a downstream cancel (status==0 and ctx cancelled): the request reached the upstream but
//     was aborted; the upstream dashboard likewise shows 499. Local errors (body read failure etc., ctx not cancelled) are unaffected.
//
// Called from the handler's closing defer — at that point ctx can only be cancelled by a downstream disconnect (the handler's own cancel
// runs after this, by defer LIFO).
func markClientGone(f *flight, ctx context.Context) {
	if f.delivered.Load() {
		return
	}
	if f.status == 200 || (f.status == 0 && ctx.Err() != nil) {
		f.status = 499
	}
}

// addFinished archives a flight's pass-through content when it ends, for the web to review recently finished streams.
// Empty content is not skipped: failed/retries-exhausted/non-streaming requests also enter the list, making it possible to see why a stream produced no output.
// Oldest entries are dropped beyond finishedCap. Called from the handler defer, single goroutine.
// refreshArchivedDown refreshes a finished archive's downstream-side copy from the flight's current downstream records: translation-port
// non-streaming responses are produced by tw.finish AFTER the handler returns (archives), so the archived snapshot misses that tail and needs this backfill.
// Does nothing if the archive no longer holds the stream (e.g. finishedCap=0 evicted it immediately).
func refreshArchivedDown(f *flight) {
	finishedMu.Lock()
	defer finishedMu.Unlock()
	for i := range finished {
		if finished[i].id == f.id {
			finished[i].contentDown = f.snapshotContentDown()
			finished[i].fullContentDown = f.snapshotFullContentDown()
			return
		}
	}
}

func addFinished(f *flight) {
	content := f.snapshotContent()
	model := f.origModel
	if f.targetModel != "" && f.targetModel != f.origModel {
		model = f.origModel + " -> " + f.targetModel
	}
	// When the model actually returned by the upstream differs from the route target, append a third segment so the user sees the real landing spot.
	if f.upstreamModel != "" && f.upstreamModel != f.targetModel && f.upstreamModel != f.origModel {
		model = model + " -> " + f.upstreamModel
	}
	now := time.Now()
	convKey := convKeyOf(f)
	obsKey := obsKeyOf(f)
	// A 499 from a downstream disconnect during yellow light (awaiting first byte, stage<3): the upstream may not have processed the input and written the cache, so conservatively don't refresh the anchor —
	// treat as anchorless (this row shows "-", the key's anchor stays on the previous same-key stream). A 499 during green light (stage=3 forwarding):
	// the upstream streaming output proves the input was processed and the cache written, so refresh the anchor as usual.
	if f.status == 499 && f.stage.Load() < 3 {
		convKey = ""
		obsKey = "" // Whether the cache was written is uncertain, so it doesn't count for lifetime observation either (this stream is no one's cur, and after archiving no one's prev)
	}
	ff := finishedFlight{
		id:               f.id,
		model:            model,
		upstreamModel:    f.upstreamModel,
		routeReason:      f.routeReason.Load(),
		status:           f.status,
		gaveUp:           f.gaveUp,
		attempts:         f.attempt.Load(),
		bytes:            f.bytes.Load(),
		stage:            f.stage.Load(),
		ended:            now,
		totalMs:          now.Sub(f.start).Milliseconds(),
		content:          content,
		reqBody:          f.snapshotReqBody(),
		fullContent:      f.snapshotFullContent(),
		reqTruncated:     f.reqTrunc(),
		reqDown:          f.snapshotReqDown(),
		reqDownTruncated: f.reqDownTrunc(),
		contentDown:      f.snapshotContentDown(),
		fullContentDown:  f.snapshotFullContentDown(),
		inTokens:         f.inTokens,
		cacheRead:        f.cacheRead,
		cacheCreation:    f.cacheCreation,
		outTokens:        f.outTokens,
		firstByteMs:      f.firstByteMs,
		tps:              f.tps,
		searchPrompt:     f.searchPrompt,
		translated:       f.translated,
		countTokens:      f.countTokens,
		tools:            f.toolCallsTag(),
		searchStripped:   int(f.searchStripped.Load()),
		convKey:          convKey,
		start:            f.start,
		obsKey:           obsKey,
		upstreamKey:      f.upstreamKey,
		think:            f.think,
	}
	finishedMu.Lock()
	// Observed cache-lifetime measurement: pair with the previous finished stream of the same session + same upstream (finished is ascending; scan backwards for the latest same-key entry).
	// The interval anchors to the two streams' start times (cache writes/reads both happen near stream start).
	if ff.obsKey != "" {
		for i := len(finished) - 1; i >= 0; i-- {
			if finished[i].obsKey == ff.obsKey {
				if kind, iv := classifyCacheObservation(finished[i].cacheRead, ff.cacheRead, ff.inTokens, ff.cacheCreation, f.start.Sub(finished[i].start)); kind != obsNone {
					recordCacheObsLocked(ff.upstreamKey, kind, iv)
				}
				break
			}
		}
	}
	finished = append(finished, ff)
	trimFinishedLocked()
	finishedMu.Unlock()
	log.Printf("[flight] #%d archived %d bytes stage=%d", f.id, len(content), ff.stage)
}

// convKeyOf returns this stream's session cache anchor key; empty when there is no session identifier (count_tokens probes, bare API calls without metadata).
func convKeyOf(f *flight) string {
	if f.convID == "" {
		return ""
	}
	return f.convID + "|" + f.convAnchor
}

// ---- Observed upstream cache-lifetime measurement (status page 「缓存命中」 popup's observed table; display-only, in-memory) ----
//
// Two adjacent finished streams of the same session + same upstream (obsKey = convID|upstreamKey) yield one observation:
//   - latter hit rate ≥95% → alive observation: the cache lived at least as long as the interval between the two streams' starts; take max as the observed lower bound;
//   - former had hits + latter hit rate <50% → dead observation: the cache did not survive that interval; take min as the observed upper bound
//     (strict zero not required: the upstream caches common prefixes like system prompts, so scattered hits can persist after the session cache dies);
//   - the middle band (50%~95%) is not observed: can't tell cache expiry from input drift;
//   - input volume <1024 tokens is not observed: small requests have noisy hit rates;
//   - interval ≤0 (clock jumps etc.) is not observed;
//   - on contradiction (the upstream cache lifetime changed mid-way, or the cache was evicted early), the newer observation wins
//     and the refuted side is discarded and re-measured — the display can never invert (e.g. "≥18min, <16min").
// It measures a lower bound, not the true TTL: a conversation the user stops following up on never produces a death signal.
// cacheObsMap shares finishedMu with finished and is cleared together by clearStats (restarting clears it naturally).

type cacheObsKind int

const (
	obsNone  cacheObsKind = iota // Does not constitute an observation
	obsAlive                     // Alive observation: the cache lived at least as long as the interval
	obsDead                      // Dead observation: the cache did not survive the interval
)

const cacheAliveHitRate = 0.95 // A latter-stream hit rate ≥95% counts as "still within the cache"
const cacheDeadHitRate = 0.50  // Former had hits + latter hit rate <50% counts as cache lost (residual hits on common prefixes like system prompts don't count as alive)
const cacheObsMinTokens = 1024 // Input volume (input+cacheRead+cacheCreation) below this is not observed
const cacheObsMaxKeyLen = 512  // Truncate abnormally long obsKeys (malicious session_id) to prevent memory bloat

type cacheObsEntry struct {
	aliveMax time.Duration // Max alive-observation interval (observed lower bound: the cache survived at least this long)
	deadMin  time.Duration // Min dead-observation interval (observed upper bound: the cache did not survive this long); 0 = no dead observation yet
	samples  int           // Observation count (alive + dead)

	// aliveAt/deadAt are the moments each bound's value last changed: reset when the bound's value changes (including contradiction invalidation),
	// and the invalidated-to-remeasure side is zeroed — when a bound doesn't exist, its formation time doesn't either.
	aliveAt time.Time
	deadAt  time.Time
}

var cacheObsMap = map[string]*cacheObsEntry{} // Key = upstreamKey (url|model); guarded by finishedMu

// classifyCacheObservation decides what observation a finished stream forms relative to the previous one of the same session + same upstream.
// Death doesn't require a strict zero (the upstream caches common prefixes like system prompts, so scattered cache_read
// can persist after the session cache expires); a hit rate <50% counts as "cache lost". When it returns obsNone, the second return value is meaningless.
func classifyCacheObservation(prevCacheRead, curCacheRead, curInput, curCacheCreation int64, interval time.Duration) (cacheObsKind, time.Duration) {
	if interval <= 0 || curInput+curCacheRead+curCacheCreation < cacheObsMinTokens {
		return obsNone, 0
	}
	hitRate := float64(curCacheRead) / float64(curInput+curCacheRead+curCacheCreation)
	// Check death first: the former having hits proves the cache was written; the latter's hit rate <50% proves the session cache didn't survive until now
	// (the two branches are mutually exclusive: a hit rate can't be both <50% and ≥95%; the order is only for readability).
	if prevCacheRead > 0 && hitRate < cacheDeadHitRate {
		return obsDead, interval
	}
	if hitRate >= cacheAliveHitRate {
		return obsAlive, interval
	}
	return obsNone, 0
}

// recordCacheObsLocked accumulates one observation into the upstream's entry: alive takes max (lower bound only rises), dead takes min (upper bound only falls).
// On contradiction the newer observation wins and the refuted side is discarded and re-measured: if the upstream cache lifetime grew mid-way (a new alive observation crossed the old upper bound),
// the upper bound is discarded; if it shrank or the cache was evicted early (a new dead observation fell below the old lower bound), the lower bound is discarded.
// Each bound's formation time is accounted separately: it moves only when the bound's value changes (including being invalidated to zero), not when a supporting observation is merely added.
// Invariant: when both sides are non-zero, aliveMax < deadMin (the display never inverts). Caller must hold finishedMu.
func recordCacheObsLocked(key string, kind cacheObsKind, interval time.Duration) {
	e := cacheObsMap[key]
	if e == nil {
		e = &cacheObsEntry{}
		cacheObsMap[key] = e
	}
	now := time.Now()
	if kind == obsAlive {
		if interval > e.aliveMax {
			e.aliveMax = interval
			e.aliveAt = now // Restart the clock only when the lower bound's value changes
		}
		if e.deadMin > 0 && e.deadMin <= e.aliveMax {
			e.deadMin = 0 // Old upper bound refuted by new alive evidence (TTL grew / old upper bound was noise) — discard and re-measure
			e.deadAt = time.Time{}
			e.samples = 0 // Observation count zeroed and restarted in step: only observations supporting the current bounds count
		}
	}
	if kind == obsDead {
		if e.deadMin == 0 || interval < e.deadMin {
			e.deadMin = interval
			e.deadAt = now // Restart the clock only when the upper bound's value changes
		}
		if e.aliveMax >= e.deadMin {
			e.aliveMax = 0 // Old lower bound refuted by new dead evidence (TTL shrank / early eviction) — discard and re-measure
			e.aliveAt = time.Time{}
			e.samples = 0 // Observation count zeroed and restarted in step: only observations supporting the current bounds count
		}
	}
	e.samples++
}

// obsKeyOf returns the observed cache-lifetime pairing key (convID|upstreamKey); an empty string means "not observed" when either side is missing (count_tokens probes,
// bare API calls without metadata, missing upstream grouping key).
func obsKeyOf(f *flight) string {
	if f.convID == "" || f.upstreamKey == "" {
		return ""
	}
	k := f.convID + "|" + f.upstreamKey
	if len(k) > cacheObsMaxKeyLen {
		k = k[:cacheObsMaxKeyLen]
	}
	return k
}

// cacheObsRow is a JSON snapshot entry of observed cache lifetimes (for the cache-hit popup's 「实测缓存时间」 table).
type cacheObsRow struct {
	URL     string `json:"url"`     // Upstream base URL (scheme stripped to shorten the display)
	Model   string `json:"model"`   // Model actually sent
	Alive   string `json:"alive"`   // Lower-bound reading (e.g. "23min"); "" when there is no alive observation
	Dead    string `json:"dead"`    // Upper-bound reading; "" when there is no dead observation
	Samples int    `json:"samples"` // Observation count (alive + dead)

	// AliveAge/DeadAge are how long each bound's value has stood (the clock restarts when the bound's value changes), counting up as m:ss;
	// "" when the corresponding bound has no observation (Alive/Dead is "").
	AliveAge string `json:"aliveAge"`
	DeadAge  string `json:"deadAge"`
}

// snapshotCacheObs returns observed cache-lifetime snapshots for all upstreams, sorted by URL+model (stable display).
func snapshotCacheObs() []cacheObsRow {
	finishedMu.Lock()
	defer finishedMu.Unlock()
	if len(cacheObsMap) == 0 {
		return nil
	}
	now := time.Now()
	out := make([]cacheObsRow, 0, len(cacheObsMap))
	for key, e := range cacheObsMap {
		url, model, _ := strings.Cut(key, "|")
		url = strings.TrimPrefix(url, "https://")
		url = strings.TrimPrefix(url, "http://")
		row := cacheObsRow{URL: url, Model: model, Samples: e.samples}
		if e.aliveMax > 0 {
			row.Alive = fmtObsDur(e.aliveMax)
			if !e.aliveAt.IsZero() {
				row.AliveAge = fmtCacheAge(now.Sub(e.aliveAt))
			}
		}
		if e.deadMin > 0 {
			row.Dead = fmtObsDur(e.deadMin)
			if !e.deadAt.IsZero() {
				row.DeadAge = fmtCacheAge(now.Sub(e.deadAt))
			}
		}
		out = append(out, row)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].URL != out[j].URL {
			return out[i].URL < out[j].URL
		}
		return out[i].Model < out[j].Model
	})
	return out
}

// trimFinishedLocked trims finished to finishedCap entries: the eviction window is the oldest len-cap rows;
// protected rows inside the window are skipped, not evicted (with no compensating extension — when trimming 14 rows to 10 and 1 of the 4 window rows is protected, only 3 are dropped, leaving 11).
// Protected = the newest row of each anchor key (session+route) still inside the protection window (now < start + defaultCacheTTL);
// it carries the status page's "缓存年龄", and evicting it would hide that (row count may exceed cap in this case).
// Rows past the protection window or superseded by a newer same-key row (showing -) are unprotected and evicted FIFO as usual. Caller must hold finishedMu.
func trimFinishedLocked() {
	capN := int(finishedCap.Load())
	if len(finished) <= capN {
		return
	}
	// Index of each key's newest row (finished is time-ascending; later entries overwriting earlier ones during the walk yields the newest).
	latest := make(map[string]int)
	for i := range finished {
		if finished[i].convKey != "" {
			latest[finished[i].convKey] = i
		}
	}
	now := time.Now()
	protected := make(map[int]bool, len(latest))
	for _, i := range latest {
		if now.Before(finished[i].start.Add(defaultCacheTTL)) {
			protected[i] = true
		}
	}
	window := len(finished) - capN // Eviction window: only the oldest window rows are eligible
	out := finished[:0]            // In-place filter: the write pointer never passes the read pointer, safe
	for i, ff := range finished {
		if i < window && !protected[i] {
			continue
		}
		out = append(out, ff)
	}
	finished = out
}

// ---- Log ring buffer ----

// logRing keeps the most recent log lines, for redrawing the lower half of the screen on terminal resizes.
type logRing struct {
	mu    sync.Mutex
	lines []string
	head  int // Next write position
	len   int // Current stored count
}

var logBuf logRing

func (r *logRing) append(line string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.len < cap(r.lines) {
		r.lines = append(r.lines, line)
		r.len++
	} else {
		r.lines[r.head] = line
		r.head = (r.head + 1) % cap(r.lines)
	}
}

// length returns the number of stored log lines.
func (r *logRing) length() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.len
}

// clear drops all stored logs (keeping the underlying array's capacity).
func (r *logRing) clear() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.lines = r.lines[:0]
	r.head = 0
	r.len = 0
}

// window returns n log lines starting offset lines up from the bottom (chronological, oldest to newest).
// offset=0 is equivalent to recent(n); offset>0 skips the newest offset lines before taking n.
func (r *logRing) window(offset, n int) []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	total := r.len
	if total == 0 || n <= 0 {
		return nil
	}
	if offset >= total {
		offset = total - 1
	}
	end := total - offset // Window's right end (exclusive, counted from the oldest)
	start := end - n
	if start < 0 {
		start = 0
	}
	count := end - start
	if count <= 0 {
		return nil
	}
	out := make([]string, 0, count)
	capN := cap(r.lines)
	for i := start; i < end; i++ {
		idx := (r.head + i) % capN
		out = append(out, r.lines[idx])
	}
	return out
}

// ---- Log writing ----

// logBufAppender only stores logs into the ring buffer (polled by the web 「查看日志」 page), writing to no stream.
// The program has no terminal UI (tray + web only); all logs go into the buffer for the web; whether they also land on disk is up to log_file.
type logBufAppender struct{}

func (logBufAppender) Write(p []byte) (int, error) {
	logBuf.append(string(p))
	return len(p), nil
}

// parseSSEStats parses one SSE line (data: {...}) and accumulates the usage token counts into the global stats.
// output_tokens is a per-stream cumulative value, accumulated by delta (new - lastOutput) so concurrent streams don't overwrite each other.
// On seeing a message_delta with usage, set *sawDeltaUsage=true: the real usage breakdown arrives in that event
// (ARK's start is all zeros; Kimi's start.input includes cache_read and start.cr=0);
// if the stream breaks before it arrives, the caller should roll back this stream's accumulated deltas (see rollbackUsageStats). Pass nil to skip the flag.
func parseSSEStats(line []byte, lastOutput, lastInput, lastCacheRead, lastCacheCreation *int64, sawDeltaUsage *bool) {
	s := bytes.TrimRight(line, "\r\n")
	if !bytes.HasPrefix(s, []byte("data:")) {
		return
	}
	payload := bytes.TrimSpace(bytes.TrimPrefix(s, []byte("data:")))
	if len(payload) == 0 || bytes.Equal(payload, []byte("[DONE]")) {
		return
	}
	var ev map[string]interface{}
	if json.Unmarshal(payload, &ev) != nil {
		return
	}
	var usage map[string]interface{}
	switch ev["type"] {
	case "message_start":
		if msg, ok := ev["message"].(map[string]interface{}); ok {
			usage, _ = msg["usage"].(map[string]interface{})
		}
	case "message_delta":
		usage, _ = ev["usage"].(map[string]interface{})
		if usage != nil && sawDeltaUsage != nil {
			*sawDeltaUsage = true
		}
	}
	if usage == nil {
		return
	}
	stats.mu.Lock()
	defer stats.mu.Unlock()
	// Each usage field takes its last-seen value in this stream (later values overwrite earlier ones); the global total only gains the new-minus-last delta.
	// So once a response completes, both the per-stream and global figures hold each field's final value.
	// Upstream semantics differ: ARK's message_start is all zeros, the real values arrive in message_delta;
	// Kimi's message_start.input includes cache_read while message_delta.input is pure input (smaller) —
	// taking the last value yields the standard-semantics input_tokens. Missing fields keep their previous value (never overwritten with 0).
	// Note: ARK's usage has no cache_creation_input_tokens; cache writes for that upstream count as 0.
	if v, ok := usage["input_tokens"]; ok {
		in := toInt64(v)
		if in != *lastInput {
			stats.inputTokens += in - *lastInput
			*lastInput = in
		}
	}
	if v, ok := usage["cache_read_input_tokens"]; ok {
		cr := toInt64(v)
		if cr != *lastCacheRead {
			stats.cacheRead += cr - *lastCacheRead
			*lastCacheRead = cr
		}
	}
	if v, ok := usage["cache_creation_input_tokens"]; ok {
		cc := toInt64(v)
		if cc != *lastCacheCreation {
			stats.cacheCreation += cc - *lastCacheCreation
			*lastCacheCreation = cc
		}
	}
	if v, ok := usage["output_tokens"]; ok {
		out := toInt64(v)
		if out != *lastOutput {
			stats.outputTokens += out - *lastOutput
			*lastOutput = out
		}
	}
}

// toolCallStart is the parse result of a tool_use/server_tool_use in content_block_start:
// index tracks the same call across lines (input_json_delta/content_block_stop align by it);
// input is the arguments carried by the start event itself (tool_use always carries {}, real arguments arrive in later deltas; server_tool_use
// may already be complete, and Kimi's empty search has no input).
type toolCallStart struct {
	index int
	name  string
	input json.RawMessage
}

// parseToolCallStart parses a tool-call block-start event from an SSE data line: returns a result only for content_block_start
// with block type tool_use/server_tool_use, otherwise ok=false. A substring pre-filter runs before JSON parsing,
// avoiding a full deserialization of every line.
func parseToolCallStart(line []byte) (ts toolCallStart, ok bool) {
	s := bytes.TrimSpace(line)
	if !bytes.HasPrefix(s, []byte("data:")) {
		return ts, false
	}
	payload := bytes.TrimSpace(s[len("data:"):])
	if !bytes.Contains(payload, []byte("content_block_start")) || !bytes.Contains(payload, []byte("tool_use")) {
		return ts, false
	}
	var ev struct {
		Type         string `json:"type"`
		Index        int    `json:"index"`
		ContentBlock struct {
			Type  string          `json:"type"`
			Name  string          `json:"name"`
			Input json.RawMessage `json:"input"`
		} `json:"content_block"`
	}
	if json.Unmarshal(payload, &ev) != nil || ev.Type != "content_block_start" {
		return ts, false
	}
	switch ev.ContentBlock.Type {
	case "tool_use", "server_tool_use":
		return toolCallStart{index: ev.Index, name: ev.ContentBlock.Name, input: ev.ContentBlock.Input}, true
	}
	return ts, false
}

// parseToolCallName extracts the tool-call name from an SSE data line (the name part of parseToolCallStart).
func parseToolCallName(line []byte) string {
	if ts, ok := parseToolCallStart(line); ok {
		return ts.name
	}
	return ""
}

// parseToolArgsDelta parses an input_json_delta from an SSE data line, returning the owning block's index and the argument fragment.
func parseToolArgsDelta(line []byte) (index int, partial string, ok bool) {
	s := bytes.TrimSpace(line)
	if !bytes.HasPrefix(s, []byte("data:")) {
		return 0, "", false
	}
	payload := bytes.TrimSpace(s[len("data:"):])
	if !bytes.Contains(payload, []byte("input_json_delta")) {
		return 0, "", false
	}
	var ev struct {
		Type  string `json:"type"`
		Index int    `json:"index"`
		Delta struct {
			Type    string `json:"type"`
			Partial string `json:"partial_json"`
		} `json:"delta"`
	}
	if json.Unmarshal(payload, &ev) != nil || ev.Type != "content_block_delta" || ev.Delta.Type != "input_json_delta" {
		return 0, "", false
	}
	return ev.Index, ev.Delta.Partial, true
}

// parseToolBlockStop parses a content_block_stop from an SSE data line, returning the ending block's index.
func parseToolBlockStop(line []byte) (index int, ok bool) {
	s := bytes.TrimSpace(line)
	if !bytes.HasPrefix(s, []byte("data:")) {
		return 0, false
	}
	payload := bytes.TrimSpace(s[len("data:"):])
	if !bytes.Contains(payload, []byte("content_block_stop")) {
		return 0, false
	}
	var ev struct {
		Type  string `json:"type"`
		Index int    `json:"index"`
	}
	if json.Unmarshal(payload, &ev) != nil || ev.Type != "content_block_stop" {
		return 0, false
	}
	return ev.Index, true
}

// isEmptyArgsJSON reports whether a JSON text is equivalent to an "empty arguments object": empty string, {}, or null
// (all whitespace ignored). Dedicated to the *0 tag decision.
func isEmptyArgsJSON(s string) bool {
	s = strings.Map(func(r rune) rune {
		switch r {
		case ' ', '\t', '\n', '\r':
			return -1
		}
		return r
	}, s)
	return s == "" || s == "{}" || s == "null"
}

// openToolCall tracks one in-progress streaming tool call so the block end can judge whether the arguments object is empty
// (the *0 tag). Argument fragments keep only the first 64 bytes: anything longer can't be an empty object, and large arguments
// (an Edit's full diff) don't deserve a full copy.
type openToolCall struct {
	name    string
	hasArgs bool   // The start event already carried a non-empty input (complete server_tool_use block)
	buf     []byte // input_json_delta fragments (≤64 bytes, judged via isEmptyArgsJSON at stop)
}

// notePartial accumulates argument fragments, capped at 64 bytes.
func (t *openToolCall) notePartial(p string) {
	if len(t.buf) >= 64 {
		return
	}
	n := 64 - len(t.buf)
	if n > len(p) {
		n = len(p)
	}
	t.buf = append(t.buf, p[:n]...)
}

// argsEmpty reports whether the arguments object was empty at block end.
func (t *openToolCall) argsEmpty() bool {
	return !t.hasArgs && isEmptyArgsJSON(string(t.buf))
}

// parseResponsesStreamStats is the Responses-protocol version of parseSSEStats (for url_response_api passthrough streams).
// In Responses SSE, usage appears only once, in the terminal event (response.completed/response.incomplete);
// and OpenAI semantics have input_tokens including the cache total, with cached_tokens the hit portion — split into
// fresh = input - cached (floor 0) + cr = cached, aligning with Anthropic semantics before joining the global aggregation.
// On seeing the terminal usage, set *sawDeltaUsage=true (same semantics as Anthropic's message_delta usage:
// the real usage has arrived; if the stream breaks first, the caller rolls back — the trackers are all 0 in that case, so the rollback is a no-op).
func parseResponsesStreamStats(line []byte, lastOutput, lastInput, lastCacheRead, lastCacheCreation *int64, sawDeltaUsage *bool) {
	s := bytes.TrimSpace(line)
	if !bytes.HasPrefix(s, []byte("data:")) {
		return
	}
	payload := bytes.TrimSpace(s[len("data:"):])
	// Pre-filter: only terminal events get a full deserialization (response.created etc. carry no usage or all zeros; skipped directly).
	if !bytes.Contains(payload, []byte("response.completed")) && !bytes.Contains(payload, []byte("response.incomplete")) {
		return
	}
	var ev struct {
		Type     string `json:"type"`
		Response struct {
			Usage *struct {
				InputTokens  int64 `json:"input_tokens"`
				OutputTokens int64 `json:"output_tokens"`
				// cache_creation_input_tokens is not a standard OpenAI field; some Anthropic-compatible endpoints carry it. Missing = 0.
				CacheCreation     int64 `json:"cache_creation_input_tokens"`
				InputTokenDetails struct {
					CachedTokens int64 `json:"cached_tokens"`
				} `json:"input_tokens_details"`
			} `json:"usage"`
		} `json:"response"`
	}
	if json.Unmarshal(payload, &ev) != nil {
		return
	}
	if (ev.Type != "response.completed" && ev.Type != "response.incomplete") || ev.Response.Usage == nil {
		return
	}
	u := ev.Response.Usage
	in := u.InputTokens - u.InputTokenDetails.CachedTokens
	if in < 0 {
		in = 0 // Defensive: don't let anomalous upstream semantics push the global input negative
	}
	cr := u.InputTokenDetails.CachedTokens
	cc := u.CacheCreation
	out := u.OutputTokens
	if sawDeltaUsage != nil {
		*sawDeltaUsage = true
	}
	stats.mu.Lock()
	defer stats.mu.Unlock()
	// Same pattern as parseSSEStats: later values overwrite earlier ones, the global total only gains the delta (usage arrives only once, but keeping the same shape lets the trackers be reused).
	if in != *lastInput {
		stats.inputTokens += in - *lastInput
		*lastInput = in
	}
	if cr != *lastCacheRead {
		stats.cacheRead += cr - *lastCacheRead
		*lastCacheRead = cr
	}
	if cc != *lastCacheCreation {
		stats.cacheCreation += cc - *lastCacheCreation
		*lastCacheCreation = cc
	}
	if out != *lastOutput {
		stats.outputTokens += out - *lastOutput
		*lastOutput = out
	}
}

// parseResponsesToolStart is the Responses-protocol version of parseToolCallStart: it extracts (itemID, tool name)
// from response.output_item.added — function_call/custom_tool_call take item.name;
// web_search_call (a server-side tool, no name) fixedly returns "web_search".
// itemID aligns with output_item.done (the *0 empty-args decision). Pre-filter before parsing, avoiding per-line deserialization.
func parseResponsesToolStart(line []byte) (itemID, name string, ok bool) {
	s := bytes.TrimSpace(line)
	if !bytes.HasPrefix(s, []byte("data:")) {
		return "", "", false
	}
	payload := bytes.TrimSpace(s[len("data:"):])
	if !bytes.Contains(payload, []byte("output_item.added")) {
		return "", "", false
	}
	var ev struct {
		Type string `json:"type"`
		Item struct {
			ID   string `json:"id"`
			Type string `json:"type"`
			Name string `json:"name"`
		} `json:"item"`
	}
	if json.Unmarshal(payload, &ev) != nil || ev.Type != "response.output_item.added" {
		return "", "", false
	}
	switch ev.Item.Type {
	case "function_call", "custom_tool_call":
		return ev.Item.ID, ev.Item.Name, true
	case "web_search_call":
		return ev.Item.ID, "web_search", true
	}
	return "", "", false
}

// parseResponsesToolName extracts the tool-call name from an SSE data line (the name part of parseResponsesToolStart).
func parseResponsesToolName(line []byte) string {
	if _, name, ok := parseResponsesToolStart(line); ok {
		return name
	}
	return ""
}

// parseResponsesToolDone extracts (itemID, arguments-object-empty) from a response.output_item.done event:
// function_call checks the final arguments, custom_tool_call checks input, web_search_call checks action
// (no query and no sources = empty). done events carry complete arguments, so no cross-line accumulation is needed. Other item types return ok=false.
func parseResponsesToolDone(line []byte) (itemID string, argsEmpty bool, ok bool) {
	s := bytes.TrimSpace(line)
	if !bytes.HasPrefix(s, []byte("data:")) {
		return "", false, false
	}
	payload := bytes.TrimSpace(s[len("data:"):])
	if !bytes.Contains(payload, []byte("output_item.done")) {
		return "", false, false
	}
	var ev struct {
		Type string `json:"type"`
		Item struct {
			ID        string          `json:"id"`
			Type      string          `json:"type"`
			Arguments string          `json:"arguments"`
			Input     json.RawMessage `json:"input"`
			Action    json.RawMessage `json:"action"`
		} `json:"item"`
	}
	if json.Unmarshal(payload, &ev) != nil || ev.Type != "response.output_item.done" {
		return "", false, false
	}
	switch ev.Item.Type {
	case "function_call":
		return ev.Item.ID, isEmptyArgsJSON(ev.Item.Arguments), true
	case "custom_tool_call":
		// A custom tool's input is a bare value: empty = absent / null / empty string.
		return ev.Item.ID, len(ev.Item.Input) == 0 || isEmptyArgsJSON(string(ev.Item.Input)) || string(ev.Item.Input) == `""`, true
	case "web_search_call":
		var action struct {
			Query   string        `json:"query"`
			Sources []interface{} `json:"sources"`
		}
		json.Unmarshal(ev.Item.Action, &action)
		return ev.Item.ID, action.Query == "" && len(action.Sources) == 0, true
	}
	return "", false, false
}

// rollbackUsageStats rolls back all usage deltas this stream has added to the global stats.
// parseSSEStats's streaming deltas sum exactly to the trackers' current values, so subtracting them wholesale is an exact undo.
// Used for interrupted streams (client disconnect / upstream dropout, before the message_delta with the real breakdown arrived):
// all that has been counted is message_start's estimated usage — Kimi's start.input measurably includes cache_read
// and start.cr=0, so keeping it would count the entire context as missed input, severely dragging down the aggregate hit rate.
func rollbackUsageStats(in, cr, cc, out int64) {
	stats.mu.Lock()
	stats.inputTokens -= in
	stats.cacheRead -= cr
	stats.cacheCreation -= cc
	stats.outputTokens -= out
	stats.mu.Unlock()
}

// parseNonStreamUsage extracts the usage field from a non-streaming JSON response body (classifier and similar requests return non-streaming JSON).
// Supports Anthropic non-streaming (usage.input_tokens/cache_read_input_tokens/cache_creation_input_tokens/output_tokens),
// OpenAI non-streaming (usage.prompt_tokens/completion_tokens, no cache_*, the two cache fields return 0),
// and Responses non-streaming (when a passthrough-stream client sends stream:false, the upstream returns a whole response object:
// usage.input_tokens includes the cache total and input_tokens_details.cached_tokens is the hit portion, split with the same semantics as streaming).
// Returns ok=false when nothing can be extracted.
func parseNonStreamUsage(content []byte) (input, cacheRead, cacheCreation, output int64, ok bool) {
	var obj map[string]interface{}
	if json.Unmarshal(content, &obj) != nil {
		return 0, 0, 0, 0, false
	}
	u, _ := obj["usage"].(map[string]interface{})
	if u == nil {
		return 0, 0, 0, 0, false
	}
	// The Responses shape must be checked first: it also has top-level input_tokens/output_tokens;
	// the criterion is the presence of input_tokens_details (neither Anthropic's nor OpenAI's usage has this sub-object).
	if det, _ := u["input_tokens_details"].(map[string]interface{}); det != nil {
		cached := toInt64(det["cached_tokens"])
		input = toInt64(u["input_tokens"]) - cached
		if input < 0 {
			input = 0
		}
		cacheRead = cached
		cacheCreation = toInt64(u["cache_creation_input_tokens"])
		output = toInt64(u["output_tokens"])
		if input == 0 && cacheRead == 0 && cacheCreation == 0 && output == 0 {
			return 0, 0, 0, 0, false
		}
		return input, cacheRead, cacheCreation, output, true
	}
	input = toInt64(u["input_tokens"])
	cacheRead = toInt64(u["cache_read_input_tokens"])
	cacheCreation = toInt64(u["cache_creation_input_tokens"])
	output = toInt64(u["output_tokens"])
	if input == 0 && cacheRead == 0 && cacheCreation == 0 && output == 0 {
		// OpenAI style: prompt_tokens / completion_tokens (no cache_*)
		input = toInt64(u["prompt_tokens"])
		output = toInt64(u["completion_tokens"])
	}
	if input == 0 && cacheRead == 0 && cacheCreation == 0 && output == 0 {
		return 0, 0, 0, 0, false
	}
	return input, cacheRead, cacheCreation, output, true
}

// shouldRetry reports whether the given status code is on the retry list.
func shouldRetry(code int) bool {
	c := cfg.Load()
	for _, rc := range c.RetryStatusCodes {
		if rc == code {
			return true
		}
	}
	return false
}

// computeBackoff computes the wait before this retry:
// prefer the upstream's Retry-After header (seconds), otherwise exponential backoff + jitter.
func computeBackoff(resp *http.Response, attempt int) time.Duration {
	c := cfg.Load()
	if c.RespectRetryAfter && resp != nil {
		if ra := resp.Header.Get("Retry-After"); ra != "" {
			if secs, err := strconv.Atoi(ra); err == nil {
				return time.Duration(secs) * time.Second
			}
		}
	}
	backoffSec := c.BaseDelaySec * math.Pow(2, float64(attempt))
	if backoffSec > c.MaxDelaySec {
		backoffSec = c.MaxDelaySec
	}
	// Add jitter so simultaneous retries don't thundering-herd.
	jitter := backoffSec * 0.3 * rand.Float64()
	return time.Duration((backoffSec + jitter) * float64(time.Second))
}

// copyHeaders copies src's headers to dst, skipping hop-by-hop headers and Content-Length.
// Content-Length must be skipped: rewriting the request body changes the length; let Go recompute it from the body.
func copyHeaders(dst, src http.Header) {
	for k, vv := range src {
		if k == "Connection" || k == "Keep-Alive" ||
			k == "Transfer-Encoding" || k == "Upgrade" ||
			k == "Content-Length" {
			continue
		}
		for _, v := range vv {
			dst.Add(k, v)
		}
	}
}

// peekHead reads the start of the response body: until the first SSE event ends (blank line), maxBytes are read,
// or EOF. Returns the bytes read (they have been consumed from br and must be put back when forwarding).
func peekHead(br *bufio.Reader, maxBytes int) []byte {
	var buf bytes.Buffer
	for buf.Len() < maxBytes {
		line, err := br.ReadBytes('\n')
		buf.Write(line)
		if err != nil {
			break // EOF or error: return what was read
		}
		// SSE events are separated by blank lines: content seen + a blank line means the first event is complete.
		if len(bytes.TrimRight(line, "\r\n")) == 0 && buf.Len() > 2 {
			break
		}
	}
	return buf.Bytes()
}

// headHasError checks whether the response head is an error event (rate limiting etc.).
// The error event of Responses passthrough streams is response.failed (the type field in the SSE data); recognized as well.
func headHasError(head []byte) bool {
	s := strings.ToLower(string(head))
	return strings.Contains(s, "event: error") ||
		strings.Contains(s, "event:error") ||
		strings.Contains(s, `"type":"error"`) ||
		strings.Contains(s, `"type": "error"`) ||
		strings.Contains(s, "response.failed") ||
		strings.Contains(s, "rate_limit") ||
		strings.Contains(s, "overloaded") ||
		strings.Contains(s, "too many requests")
}

// firstLine takes the first line of a byte stream (truncated to 200 chars), for logging.
func firstLine(b []byte) string {
	s := string(b)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	if len(s) > 200 {
		s = s[:200]
	}
	return strings.TrimSpace(s)
}

// truncate collapses a string to one line and cuts it to n chars, for logging.
func truncate(s string, n int) string {
	s = strings.ReplaceAll(s, "\n", " ")
	if len(s) > n {
		return s[:n] + "..."
	}
	return s
}

// fieldSpan records a field's byte positions within a top-level JSON object (based on the original body, quotes/colon included in the locating).
// keyStart/keyEnd are the key's span (including the double quotes); valStart/valEnd are the value's byte span.
type fieldSpan struct {
	name     string
	keyStart int
	keyEnd   int
	valStart int
	valEnd   int
}

// locateTopFields streaming-parses a top-level JSON object and returns each field's byte position in the original body (appearance order preserved).
// It reads keys with json.Decoder's Token and values into RawMessage with Decode, then calibrates the value's byte span using InputOffset +
// the RawMessage content. Only top-level keys match; same-named fields in nested objects are untouched.
// On locating failure it returns ok=false; the caller then passes the original body through (forwarding unaffected).
func locateTopFields(body []byte) ([]fieldSpan, bool) {
	dec := json.NewDecoder(bytes.NewReader(body))
	if t, err := dec.Token(); err != nil || t != json.Delim('{') {
		return nil, false
	}
	var spans []fieldSpan
	for dec.More() {
		tk, err := dec.Token()
		if err != nil {
			return nil, false
		}
		key, ok := tk.(string)
		if !ok {
			return nil, false
		}
		var v json.RawMessage
		if err := dec.Decode(&v); err != nil {
			return nil, false
		}
		valStart, valEnd, ok := locateValueRange(body, v, dec.InputOffset())
		if !ok {
			return nil, false
		}
		keyStart, keyEnd, ok := findKeySpan(body, valStart, key)
		if !ok {
			return nil, false
		}
		spans = append(spans, fieldSpan{name: key, keyStart: keyStart, keyEnd: keyEnd, valStart: valStart, valEnd: valEnd})
	}
	return spans, true
}

// locateValueRange derives the value's byte span from the Decoder's InputOffset and calibrates it with the RawMessage content.
// After Decode, InputOffset usually points at the value's end, but scanp/trailing whitespace can shift it a few bytes;
// so an exact match (body slice == RawMessage) is attempted within ±8 bytes nearby; no match means locating failed.
func locateValueRange(body, v []byte, off int64) (valStart, valEnd int, ok bool) {
	approx := int(off)
	for d := 0; d <= 8; d++ {
		for _, end := range []int{approx + d, approx - d} {
			start := end - len(v)
			if start >= 0 && end >= 0 && end <= len(body) && bytes.Equal(body[start:end], v) {
				return start, end, true
			}
		}
	}
	return 0, 0, false
}

// findKeySpan searches backwards from the value's start for the "key": pattern, locating the key's byte span (including the double quotes).
// The target field names are all simple identifiers without escapes, so plain literal matching suffices; LastIndex takes the most recent occurrence.
func findKeySpan(body []byte, valStart int, key string) (keyStart, keyEnd int, ok bool) {
	needle := []byte(`"` + key + `":`)
	end := valStart
	if end > len(body) {
		end = len(body)
	}
	idx := bytes.LastIndex(body[:end], needle)
	if idx < 0 {
		return 0, 0, false
	}
	return idx, idx + len(key) + 2, true // +2 = opening " + key + closing "
}

// classifierSystemMatches reports whether the system field (raw bytes) starts with the given prefix.
// system may be a string or an Anthropic-style [{type,text}] array.
func classifierSystemMatches(body []byte, spans []fieldSpan, prefix string) bool {
	for _, s := range spans {
		if s.name != "system" {
			continue
		}
		sysRaw := body[s.valStart:s.valEnd]
		var str string
		if err := json.Unmarshal(sysRaw, &str); err == nil {
			return strings.HasPrefix(str, prefix)
		}
		var arr []map[string]interface{}
		if err := json.Unmarshal(sysRaw, &arr); err == nil {
			for _, m := range arr {
				if t, _ := m["text"].(string); strings.HasPrefix(t, prefix) {
					return true
				}
			}
		}
		return false
	}
	return false
}

// fieldDeleteRange computes the byte span for deleting a field (key, colon, value included), handling the surrounding commas correctly:
// if a comma follows the field, delete through it; otherwise if a comma precedes the field (after skipping whitespace), delete from that comma.
// This avoids leaving illegal shapes like ,, / {, / ,} behind.
func fieldDeleteRange(body []byte, f *fieldSpan) (int, int) {
	start := f.keyStart
	end := f.valEnd
	if end < len(body) && body[end] == ',' {
		return start, end + 1 // Delete through the trailing comma
	}
	// A } follows the field (or whitespace + }): search backwards for the previous field's trailing comma
	i := start - 1
	for i >= 0 && (body[i] == ' ' || body[i] == '\t' || body[i] == '\n' || body[i] == '\r') {
		i--
	}
	if i >= 0 && body[i] == ',' {
		return i, end
	}
	return start, end
}

// lastCloseBrace returns the position of the last } in body (skipping trailing whitespace), or -1 if none.
func lastCloseBrace(body []byte) int {
	for i := len(body) - 1; i >= 0; i-- {
		if body[i] == '}' {
			return i
		}
		if body[i] != ' ' && body[i] != '\t' && body[i] != '\n' && body[i] != '\r' {
			return -1
		}
	}
	return -1
}

// applyClassifierEdits performs text replacements/deletions/appends on the original body, touching only the target fields and preserving every other byte.
// thinking->{"type":"disabled"}, reasoning_effort->"none", reasoning->deleted;
// fields absent from the original are appended before the top-level closing }. Edits are spliced by ascending start, all based on the original body, without interfering with each other.
func applyClassifierEdits(body []byte, spans []fieldSpan) []byte {
	have := map[string]*fieldSpan{}
	for i := range spans {
		s := &spans[i]
		switch s.name {
		case "thinking", "reasoning_effort", "reasoning", "max_tokens":
			have[s.name] = s
		}
	}

	type edit struct {
		start, end int
		repl       []byte
	}
	var edits []edit

	// Replace/delete existing target fields
	if r, ok := have["reasoning"]; ok {
		ds, de := fieldDeleteRange(body, r)
		edits = append(edits, edit{ds, de, nil})
	}
	if t, ok := have["thinking"]; ok {
		edits = append(edits, edit{t.valStart, t.valEnd, []byte(`{"type":"disabled"}`)})
	}
	if re, ok := have["reasoning_effort"]; ok {
		edits = append(edits, edit{re.valStart, re.valEnd, []byte(`"none"`)})
	}

	// Append fields absent from the original body: inserted before the top-level closing } (original field order preserved, new fields at the end)
	var appendFields []byte
	if _, ok := have["thinking"]; !ok {
		appendFields = append(appendFields, []byte(`"thinking":{"type":"disabled"}`)...)
	}
	if _, ok := have["reasoning_effort"]; !ok {
		if len(appendFields) > 0 {
			appendFields = append(appendFields, ',')
		}
		appendFields = append(appendFields, []byte(`"reasoning_effort":"none"`)...)
	}
	if len(appendFields) > 0 {
		braceOff := bytes.IndexByte(body, '{')
		closeOff := lastCloseBrace(body)
		if braceOff < 0 || closeOff <= braceOff {
			return body
		}
		// If the object is empty (only whitespace between { }), insert after {; otherwise insert before } with a leading comma
		empty := true
		for i := braceOff + 1; i < closeOff; i++ {
			if c := body[i]; c != ' ' && c != '\t' && c != '\n' && c != '\r' {
				empty = false
				break
			}
		}
		if empty {
			edits = append(edits, edit{braceOff + 1, braceOff + 1, appendFields})
		} else {
			edits = append(edits, edit{closeOff, closeOff, append([]byte{','}, appendFields...)})
		}
	}

	if len(edits) == 0 {
		return body
	}

	// Splice in segments by ascending start (based on the original body, edits don't interfere with each other)
	sort.Slice(edits, func(i, j int) bool { return edits[i].start < edits[j].start })
	var buf bytes.Buffer
	prev := 0
	for _, e := range edits {
		if e.start < prev {
			continue // Defensive: skip overlapping edits
		}
		buf.Write(body[prev:e.start])
		buf.Write(e.repl)
		prev = e.end
	}
	buf.Write(body[prev:])
	return buf.Bytes()
}

// isFastRequest reports whether a request comes from Claude Code's /fast mode.
// The explicit top-level "speed":"fast" field in the request body is authoritative; the Anthropic-Beta request header is only a pre-check hint
// and never triggers routing, so a lingering header after /fast off doesn't keep traffic on the fast route.
func isFastRequest(body []byte, r *http.Request) bool {
	return getStringField(body, "speed") == "fast"
}

// removeSpeedField removes the top-level "speed" field from a JSON body, preserving a legal JSON structure.
// Upstreams don't support the speed field; passing it through could cause rejection or undefined behavior. Uses streaming field locating,
// unaffected by field order or whitespace.
func removeSpeedField(body []byte) []byte {
	spans, ok := locateTopFields(body)
	if !ok {
		return body
	}
	for i, s := range spans {
		if s.name != "speed" {
			continue
		}
		return deleteFieldSpan(body, spans, i)
	}
	return body
}

// getStringField reads a field's string value from the body's top-level JSON object; missing or non-string returns empty.
func getStringField(body []byte, key string) string {
	spans, ok := locateTopFields(body)
	if !ok {
		return ""
	}
	for _, s := range spans {
		if s.name != key {
			continue
		}
		var v string
		if err := json.Unmarshal(body[s.valStart:s.valEnd], &v); err != nil {
			return ""
		}
		return v
	}
	return ""
}

// getBoolField reads a field's boolean value from the body's top-level JSON object; missing or non-boolean returns ok=false.
func getBoolField(body []byte, key string) (val bool, ok bool) {
	spans, ok2 := locateTopFields(body)
	if !ok2 {
		return false, false
	}
	for _, s := range spans {
		if s.name != key {
			continue
		}
		var b bool
		if err := json.Unmarshal(body[s.valStart:s.valEnd], &b); err != nil {
			return false, false
		}
		return b, true
	}
	return false, false
}

// forceStreamTrue rewrites the request body's top-level "stream" field to true, appending it at the object's end if missing.
// Uses streaming field locating + text replacement without wholesale re-serialization; untouched fields keep their exact bytes (key order and formatting included),
// preserving upstream cache hits. body: the original request body. Returns the rewritten body.
func forceStreamTrue(body []byte) []byte {
	spans, ok := locateTopFields(body)
	if !ok {
		return body
	}
	for _, s := range spans {
		if s.name != "stream" {
			continue
		}
		// Already true: return the original body, avoiding a pointless rewrite.
		if bytes.Equal(bytes.TrimSpace(body[s.valStart:s.valEnd]), []byte("true")) {
			return body
		}
		out := make([]byte, 0, len(body))
		out = append(out, body[:s.valStart]...)
		out = append(out, "true"...)
		out = append(out, body[s.valEnd:]...)
		return out
	}
	// stream field missing: append ,"stream":true before the object's closing } (no comma for an empty object).
	if len(body) == 0 || body[len(body)-1] != '}' {
		return body
	}
	sep := ","
	if len(spans) == 0 {
		sep = ""
	}
	out := make([]byte, 0, len(body)+len(sep)+len(`"stream":true}`))
	out = append(out, body[:len(body)-1]...)
	out = append(out, sep...)
	out = append(out, `"stream":true}`...)
	return out
}

// deleteFieldSpan deletes the idx-th field of a top-level JSON object, keeping the other fields' order and formatting.
func deleteFieldSpan(body []byte, spans []fieldSpan, idx int) []byte {
	if len(spans) == 0 || idx < 0 || idx >= len(spans) {
		return body
	}
	s := spans[idx]
	// Span to delete: from this field's keyStart to its valEnd.
	// The surrounding commas are handled afterwards to keep the JSON legal.
	start := s.keyStart
	end := s.valEnd

	if idx == 0 {
		// First field: after deletion, drop the immediately following comma (if any).
		if end < len(body) && body[end] == ',' {
			end++
		}
	} else {
		// Non-first field: a comma precedes it; delete the comma too (start moves back one).
		start--
	}

	out := make([]byte, 0, len(body)-(end-start))
	out = append(out, body[:start]...)
	out = append(out, body[end:]...)
	return out
}

// setTopLevelJSONValue precisely sets one key's value at the top level of a JSON text (rawValue is complete JSON
// value text, e.g. `"en"`, `{"type":"disabled"}`); rawValue=nil deletes the key.
// Fields are located via locateTopFields (same-named nested keys untouched), and all editing is text-level: every other field's
// content, order, indentation, and newline style is preserved byte for byte. Returns a new slice; the input is not modified.
// ok=false when the top level is not a legal JSON object; the caller should abandon the write.
func setTopLevelJSONValue(body []byte, key string, rawValue []byte) ([]byte, bool) {
	spans, ok := locateTopFields(body)
	if !ok {
		return nil, false
	}
	idx := -1
	for i := range spans {
		if spans[i].name == key {
			idx = i
			break
		}
	}
	if idx >= 0 {
		s := spans[idx]
		if rawValue != nil {
			// Set value: replace only the [valStart,valEnd) span; the key name and all other bytes are preserved
			out := make([]byte, 0, len(body)-(s.valEnd-s.valStart)+len(rawValue))
			out = append(out, body[:s.valStart]...)
			out = append(out, rawValue...)
			out = append(out, body[s.valEnd:]...)
			return out, true
		}
		// Delete key: clean up the separating comma along with it, leaving no blank line or dangling comma
		start, end := s.keyStart, s.valEnd
		if idx == 0 {
			// First field: first back off the key's own line indentation, then consume the comma right after the value plus one following newline
			for start > 0 && (body[start-1] == ' ' || body[start-1] == '\t') {
				start--
			}
			for end < len(body) && (body[end] == ' ' || body[end] == '\t') {
				end++
			}
			if end < len(body) && body[end] == ',' {
				end++
			}
			for end < len(body) && (body[end] == ' ' || body[end] == '\t') {
				end++
			}
			if end < len(body) && body[end] == '\r' {
				end++
			}
			if end < len(body) && body[end] == '\n' {
				end++
			}
		} else {
			// Non-first field: consume the whitespace/newline before the key plus the preceding comma (the key's whole line disappears with it)
			for start > 0 && (body[start-1] == ' ' || body[start-1] == '\t' || body[start-1] == '\r' || body[start-1] == '\n') {
				start--
			}
			if start > 0 && body[start-1] == ',' {
				start--
			}
		}
		out := make([]byte, 0, len(body)-(end-start))
		out = append(out, body[:start]...)
		out = append(out, body[end:]...)
		return out, true
	}
	if rawValue == nil {
		return body, true // The key to delete doesn't exist; nothing to do
	}
	// Append a new key: insert before the top-level closing '}', following the first top-level key's line indentation and the file's newline style
	closePos := len(bytes.TrimRight(body, " \t\r\n")) - 1
	if closePos < 0 || body[closePos] != '}' {
		return nil, false
	}
	indent := "  "
	if len(spans) > 0 {
		lineStart := bytes.LastIndexByte(body[:spans[0].keyStart], '\n') + 1
		ind := body[lineStart:spans[0].keyStart]
		allWS := len(ind) > 0
		for _, c := range ind {
			if c != ' ' && c != '\t' {
				allWS = false
				break
			}
		}
		if allWS {
			indent = string(ind)
		}
	}
	nl := "\n"
	if bytes.Contains(body, []byte("\r\n")) {
		nl = "\r\n"
	}
	out := make([]byte, 0, len(body)+len(key)+len(rawValue)+16)
	// The comma must follow the last non-whitespace byte (the previous field's line end); the new key gets its own line;
	// the whitespace before the original closing '}' is replaced by the unified nl below
	trimEnd := len(bytes.TrimRight(body[:closePos], " \t\r\n"))
	out = append(out, body[:trimEnd]...)
	if len(spans) > 0 {
		out = append(out, ',')
	}
	out = append(out, nl...)
	out = append(out, indent...)
	out = append(out, '"')
	out = append(out, key...)
	out = append(out, []byte(`": `)...)
	out = append(out, rawValue...)
	out = append(out, nl...)
	out = append(out, body[closePos:]...) // The closing '}' and the original trailing whitespace
	return out, true
}

// disableThinkingInBody rewrites the request body's top-level thinking to {"type":"disabled"} and deletes
// output_config: the one-shot fallback surgery after a convertOff2Low upgrade is rejected by an upstream 400
// (the downstream wanted thinking off anyway, so the fallback is semantically lossless). Text-level editing; all other fields preserved.
func disableThinkingInBody(body []byte) ([]byte, bool) {
	nb, ok := setTopLevelJSONValue(body, "thinking", []byte(`{"type":"disabled"}`))
	if !ok {
		return nil, false
	}
	return setTopLevelJSONValue(nb, "output_config", nil)
}

// isClassifierRequest reports whether body is a classifier request (system field prefix match).
// Shares the same check with maybeRewriteClassifier but only checks, never rewrites; used for routing decisions.
// Independent of classifier_thinking: classifier routing works even when no thinking policy is configured.
func isClassifierRequest(c *Config, body []byte) bool {
	if !bytes.Contains(body, []byte(classifierSystemPrefix)) {
		return false
	}
	spans, ok := locateTopFields(body)
	if !ok {
		return false
	}
	return classifierSystemMatches(body, spans, classifierSystemPrefix)
}

// classifierThinkingOff reports whether classifier requests get their thinking rewritten off
// (classifier_route.classifier_thinking == "off"; unset or no classifier_route = the request's thinking goes through untouched).
func classifierThinkingOff(c *Config) bool {
	return c.ClassifierRoute != nil && c.ClassifierRoute.ClassifierThinking == "off"
}

// maybeRewriteClassifier turns thinking off when a classifier request matches, so classification returns fast.
// A bytes.Contains pre-filter keeps normal requests (without the prefix substring) away from JSON parsing — near-zero cost.
// On a match, json.Decoder streaming-locates the target fields' byte positions, then text replacement — no wholesale re-serialization;
// untouched fields keep their exact bytes (key order and formatting included), preserving upstream cache hits.
func maybeRewriteClassifier(body []byte) []byte {
	c := cfg.Load()
	if !classifierThinkingOff(c) {
		return body
	}
	if !bytes.Contains(body, []byte(classifierSystemPrefix)) {
		return body
	}
	spans, ok := locateTopFields(body)
	if !ok {
		return body
	}
	if !classifierSystemMatches(body, spans, classifierSystemPrefix) {
		return body
	}
	newBody := applyClassifierEdits(body, spans)
	if len(newBody) == len(body) && bytes.Equal(newBody, body) {
		return body
	}
	log.Printf("[rewrite] classifier signature hit; thinking disabled (body %d->%d bytes)", len(body), len(newBody))
	stats.classifierRewrites.Add(1) // Live status row count: classifier thinking-off count
	return newBody
}

// logRequestDetail parses the request body and prints the stream/tools/system prefixes, for diagnosing classifier fingerprints.
// Called only when log_request_detail=true; off by default.
func logRequestDetail(r *http.Request, body []byte) {
	var p map[string]interface{}
	if err := json.Unmarshal(body, &p); err != nil {
		log.Printf("[detail] %s %s (non-JSON)", r.Method, r.URL.Path)
		return
	}
	stream, _ := p["stream"].(bool)
	tools := 0
	toolNames := []string{}
	if arr, ok := p["tools"].([]interface{}); ok {
		tools = len(arr)
		for _, t := range arr {
			if m, ok := t.(map[string]interface{}); ok {
				name, _ := m["name"].(string)
				typ, _ := m["type"].(string)
				if name != "" {
					toolNames = append(toolNames, name)
				} else if typ != "" {
					toolNames = append(toolNames, typ)
				}
			}
		}
	}
	sysPrefix := ""
	switch s := p["system"].(type) {
	case string:
		sysPrefix = truncate(s, 60)
	case []interface{}:
		for _, blk := range s {
			if m, ok := blk.(map[string]interface{}); ok {
				if t, _ := m["text"].(string); t != "" {
					sysPrefix = truncate(t, 60)
					break
				}
			}
		}
	}
	log.Printf("[detail] %s %s stream=%v tools=%d %v sys=%q image=%v search=%v", r.Method, r.URL.Path, stream, tools, toolNames, sysPrefix, hasImage(body), hasWebSearch(body))
}

// locateModel locates the string value span of the request body's top-level "model" field.
// Lightweight: after locating the "model" key it reads the string literal right after it, without parsing the whole JSON — no per-request Unmarshal.
// body: the request body bytes. Returns value=model name, start/end=the value's byte span in body (quotes excluded), ok=whether it was located.
func locateModel(body []byte) (value string, start, end int, ok bool) {
	key := []byte(`"model"`)
	i := bytes.Index(body, key)
	if i < 0 {
		return "", 0, 0, false
	}
	i += len(key)
	// Skip the colon and whitespace to land on the value.
	for i < len(body) {
		c := body[i]
		if c == ':' || c == ' ' || c == '\t' || c == '\n' || c == '\r' {
			i++
			continue
		}
		break
	}
	if i >= len(body) || body[i] != '"' {
		return "", 0, 0, false
	}
	i++ // Skip the opening quote
	start = i
	for i < len(body) && body[i] != '"' {
		if body[i] == '\\' && i+1 < len(body) { // Skip escaped characters
			i += 2
			continue
		}
		i++
	}
	return string(body[start:i]), start, i, true
}

// extractModel extracts the request body's "model" field value, only for display in the [请求] log line. Returns empty on failure.
func extractModel(body []byte) string {
	v, _, _, ok := locateModel(body)
	if !ok {
		return ""
	}
	return v
}

// extractConvID extracts the session identifier of an Anthropic-port request body, anchoring the status page's "缓存年龄" column per session.
// Claude Code always carries a top-level metadata.user_id whose value is a JSON string
// ({"device_id":"...","account_uuid":"...","session_id":"<uuid>"}); the session_id inside is used.
// Free text stuffed by a bare API caller is used as-is (stability is enough for correlation). No metadata → empty.
// locateTopFields locates the top-level metadata and only that small object is unmarshaled — the body is never fully parsed; read-only.
func extractConvID(body []byte) string {
	spans, ok := locateTopFields(body)
	if !ok {
		return ""
	}
	for _, s := range spans {
		if s.name != "metadata" {
			continue
		}
		var md struct {
			UserID string `json:"user_id"`
		}
		if err := json.Unmarshal(body[s.valStart:s.valEnd], &md); err != nil || md.UserID == "" {
			return ""
		}
		if strings.HasPrefix(md.UserID, "{") {
			var inner struct {
				SessionID string `json:"session_id"`
			}
			if err := json.Unmarshal([]byte(md.UserID), &inner); err == nil && inner.SessionID != "" {
				return inner.SessionID
			}
		}
		return md.UserID
	}
	return ""
}

// extractThinkMode extracts the thinking config of an Anthropic-format request body, so the status page's 「API」 column thinking value shows
// the shape actually sent upstream (the call site is after the classifier's thinking-off rewrite; the translation port passes in the already-mapped body).
// Values take the shortest form (which API family the vocabulary belongs to is carried by the column color, not by prefix text):
// thinking.type=disabled → "off"; enabled → "on <budget_tokens>" (just "on" without a budget);
// adaptive without effort → "adaptive"; with output_config.effort → just the effort word ("high"/"max"…);
// output_config.effort without thinking → likewise just the effort word; unknown types display as-is.
// No thinking/output_config field → empty (column shows -).
// locateTopFields locates the relevant small objects for unmarshaling; the body is never fully parsed; read-only.
func extractThinkMode(body []byte) string {
	spans, ok := locateTopFields(body)
	if !ok {
		return ""
	}
	mode := ""
	effort := ""
	for _, s := range spans {
		switch s.name {
		case "thinking":
			var t struct {
				Type   string `json:"type"`
				Budget int64  `json:"budget_tokens"`
			}
			if err := json.Unmarshal(body[s.valStart:s.valEnd], &t); err != nil {
				continue
			}
			switch t.Type {
			case "disabled":
				mode = "off"
			case "enabled":
				if t.Budget > 0 {
					mode = fmt.Sprintf("on %d", t.Budget)
				} else {
					mode = "on"
				}
			default:
				mode = t.Type // adaptive and unknown types display as-is
			}
		case "output_config":
			var oc struct {
				Effort string `json:"effort"`
			}
			if err := json.Unmarshal(body[s.valStart:s.valEnd], &oc); err == nil {
				effort = oc.Effort
			}
		}
	}
	// Effort word alone: adaptive-with-effort and effort-only both collapse to the effort itself (a prefix like "adaptive·high" would be redundant;
	// the web uses color to tell which API family the vocabulary belongs to).
	if effort != "" && (mode == "" || mode == "adaptive") {
		return effort
	}
	return mode
}

// replaceModelValue replaces the request body's "model" field value with newModel, for rewriting on route hits.
// locateModel locates the value span, then byte splicing; the length change is picked up when bytes.NewReader recomputes Content-Length later.
// body: original request body; newModel: target model name. Returns the rewritten body. Returns the original when no model field is located.
func replaceModelValue(body []byte, newModel string) []byte {
	_, start, end, ok := locateModel(body)
	if !ok {
		return body
	}
	out := make([]byte, 0, len(body)-end+start+len(newModel))
	out = append(out, body[:start]...)
	out = append(out, []byte(newModel)...)
	out = append(out, body[end:]...)
	return out
}

// hasImage reports whether the request body contains an Anthropic image content block.
// An Anthropic image block looks like {"type":"image","source":{"type":"base64",...}}; "type":"image" appears only in image blocks within a request body.
// Uses bytes.Contains for zero-allocation substring detection instead of full deserialization, following the streaming-locate/text-replace forwarding-safety principle.
// body: the request body. Returns whether an image block is present.
func hasImage(body []byte) bool {
	return bytes.Contains(body, []byte(`"type":"image"`))
}

// hasWebSearch reports whether the request body carries an Anthropic server-side web_search tool (the upstream performs the search).
// Only "type":"web_search_YYYYMMDD" server-side tools are recognized; the client-side WebSearch tool is deliberately not recognized
// (Claude Code carries its definition in every request, so it can't distinguish a real intent to search and would misjudge every request as a search request).
// body: the request body. Returns whether a server-side web_search tool is present.
func hasWebSearch(body []byte) bool {
	return bytes.Contains(body, []byte(`"type":"web_search`))
}

// searchStep1Body builds the step-1 search request: copies the original body, changes model to sf.Model, forces stream to false.
// body: original request body; sf: search route config. Returns the new request body or an error.
func searchStep1Body(body []byte, sf *SearchRoute) ([]byte, error) {
	var m map[string]any
	if err := json.Unmarshal(body, &m); err != nil {
		return nil, err
	}
	if sf.Model != "" {
		m["model"] = sf.Model
	}
	m["stream"] = false
	return json.Marshal(m)
}

// searchStep2Body builds the step-2 summary request: messages=[user(original last user), assistant(r1.content), user(summary instruction)].
// summaryLevelConfig returns the step-2 summary instruction and max_tokens per detail level.
// low=brief summary (default), mid=medium detail (key facts included), high=thorough (all data points/citations/context).
// level: the configured summary_level value; query: the user's original question (search intent), carried into the instruction so the summary focuses on relevant content. Returns (instruction text, max_tokens).
func summaryLevelConfig(level, query string) (string, int) {
	// The instruction carries the user's original question (search intent): the summary focuses on question-relevant content while still covering each result's key information,
	// avoiding a brainless generic summary. An empty query degrades to a generic summary.
	q := strings.TrimSpace(query)
	if q == "" {
		q = "search"
	}
	// Search results often contain multiple dates/times ("today/latest/recent/now"); carrying the current date-time lets the model weight time-sensitive content correctly.
	now := time.Now().In(time.FixedZone("UTC+8", 8*60*60)).Format("2006-01-02 15:04")
	base := "Current date and time is " + now + " (UTC+8, Beijing time).\n" +
		"The user's original question (the search intent) is: \"%s\".\n" +
		"Based on the web_search_tool_result above, %s IN ORDER. " +
		"While summarizing each result, pay special attention to and explicitly call out information that addresses or relates to the user's question above, but still cover the other key content of each result. " +
		"Format each as 'Result N: <summary>' where N starts at 1 and goes in order across all web_search_tool_result blocks. " +
		"Do not answer the original question, just summarize each result."
	switch strings.ToLower(strings.TrimSpace(level)) {
	case "mid", "medium":
		return fmt.Sprintf(base, q, "write a medium-detail plaintext summary for EACH search result, including key facts and relevant data points"), 4096
	case "max", "full":
		// max: on top of high, steps/methods/code/formulas must be reproduced verbatim in full; max_tokens is raised to fit complete code and formulas.
		return fmt.Sprintf(base, q, "write a detailed comprehensive plaintext summary for EACH search result, including all key facts, data points, direct quotes, dates, numbers, and relevant context; be thorough. When you encounter any steps, methods, code, or formulas, you MUST reproduce them completely and verbatim in full detail"), 16384
	case "high":
		return fmt.Sprintf(base, q, "write a detailed comprehensive plaintext summary for EACH search result, including all key facts, data points, direct quotes, dates, numbers, and relevant context; be thorough"), 8192
	default: // low / empty / unknown
		return fmt.Sprintf(base, q, "write a short plaintext summary for EACH search result"), 2048
	}
}

// searchStep2Body builds the step-2 summary request: messages=[user(original last user), assistant(r1.content), user(summary instruction)].
// r1: step-1 response map; origBody: original request body (its last user text is taken); sf: search route config. Returns the new request body or an error.
func searchStep2Body(r1 map[string]any, origBody []byte, sf *SearchRoute, level string, thinking bool) ([]byte, string, error) {
	lastUser, ok := searchLastUserText(origBody)
	if !ok {
		lastUser = "search"
	}
	content, _ := r1["content"].([]any)
	instr, maxTokens := summaryLevelConfig(level, lastUser)
	m := map[string]any{
		"model":      sf.Model,
		"max_tokens": maxTokens,
		"stream":     true,
		"messages": []map[string]any{
			{"role": "user", "content": lastUser},
			{"role": "assistant", "content": content},
			{"role": "user", "content": instr},
		},
	}
	if thinking {
		m["thinking"] = map[string]any{"type": "enabled", "budget_tokens": 2048}
	}
	body, err := json.Marshal(m)
	return body, instr, err
}

// searchLastUserText takes the text content of the last user message in the request body.
// body: the request body. Returns the text and whether it was found.
func searchLastUserText(body []byte) (string, bool) {
	var m map[string]any
	if err := json.Unmarshal(body, &m); err != nil {
		return "", false
	}
	msgs, _ := m["messages"].([]any)
	for i := len(msgs) - 1; i >= 0; i-- {
		mm, _ := msgs[i].(map[string]any)
		if mm == nil || mm["role"] != "user" {
			continue
		}
		switch c := mm["content"].(type) {
		case string:
			return c, true
		case []any:
			var sb strings.Builder
			for _, b := range c {
				bb, _ := b.(map[string]any)
				if bb != nil && bb["type"] == "text" {
					if t, ok := bb["text"].(string); ok {
						sb.WriteString(t)
					}
				}
			}
			if sb.Len() > 0 {
				return sb.String(), true
			}
		}
	}
	return "", false
}

// searchPostFlight sends a search/summary internal request and registers its progress on the flight (visible on the status page).
// stream=false reads the whole response at once (step-1 search); stream=true reads SSE streaming and tees to the flight (step-2 summary).
// Streaming mode accumulates a minimal response map (model/stop_reason/content[text]).
// url/api: upstream address and key; reqBody: request body; f: the associated flight; stream: whether to stream.
// Returns (minimal response map, raw bytes, error).
func searchPostFlight(url, api string, reqBody []byte, f *flight, stream bool, modelHint string) (map[string]any, []byte, error) {
	req, err := http.NewRequest("POST", url+"/v1/messages", bytes.NewReader(reqBody))
	if err != nil {
		return nil, nil, err
	}
	req.Header.Set("content-type", "application/json")
	req.Header.Set("anthropic-version", "2023-06-01")
	req.Header.Set("Authorization", "Bearer "+api)
	tSend := time.Now()
	// Counted into the tray status light: yellow while awaiting the upstream's first byte (waiting++), consistent with the main forwarding path.
	stats.mu.Lock()
	stats.waiting++
	stats.mu.Unlock()
	resp, err := client.Do(req)
	stats.mu.Lock()
	stats.waiting--
	stats.mu.Unlock()
	if err != nil {
		return nil, nil, err
	}
	defer resp.Body.Close()
	if f != nil {
		f.status = resp.StatusCode
		f.phase.Store(1)
		f.setStage(stageForward)
	}
	if resp.StatusCode >= 400 {
		data, _ := io.ReadAll(resp.Body)
		if f != nil {
			f.appendContent(data)
			f.bytes.Add(int64(len(data)))
		}
		return nil, data, fmt.Errorf("upstream %d: %s", resp.StatusCode, string(data))
	}
	// Started reading the response (step1 whole / step2 streaming): the status light turns green, returning to idle when done or on error.
	// Fixes search-summary mode showing grey the whole time (active/waiting previously went uncounted, so it looked idle even while the summary model was streaming).
	stats.mu.Lock()
	stats.active++
	stats.mu.Unlock()
	defer func() {
		stats.mu.Lock()
		stats.active--
		stats.mu.Unlock()
	}()
	if !stream {
		tFirstByte := time.Now()
		data, err := io.ReadAll(resp.Body)
		if err != nil {
			return nil, nil, err
		}
		if f != nil {
			f.appendContent(data)
			f.bytes.Add(int64(len(data)))
		}
		var m map[string]any
		if err := json.Unmarshal(data, &m); err != nil {
			return nil, data, fmt.Errorf("parse response: %w (body=%s)", err, string(data))
		}
		if f != nil {
			if in, cr, cc, out, ok := parseNonStreamUsage(data); ok {
				f.inTokens = in
				f.cacheRead = cr
				f.cacheCreation = cc
				f.outTokens = out
				stats.mu.Lock()
				stats.inputTokens += in
				stats.cacheRead += cr
				stats.cacheCreation += cc
				stats.outputTokens += out
				stats.mu.Unlock()
			}
			realModel := modelHint
			if mm, ok := m["model"].(string); ok && mm != "" {
				realModel = mm
			}
			stats.addModelUsage(realModel, f.inTokens, f.cacheRead, f.cacheCreation, f.outTokens)
			f.firstByteMs = tFirstByte.Sub(tSend).Milliseconds()
			log.Printf("[flight] #%d search step1 TTFT %.2fs %d+%d/%d tok",
				f.id, float64(f.firstByteMs)/1000, f.inTokens, f.cacheRead, f.outTokens)
		}
		return m, data, nil
	}
	// Streaming: read SSE while teeing to the flight, accumulating the minimal response map in parallel.
	var raw bytes.Buffer
	var model, stopReason string
	var textBuf strings.Builder
	var lastOutput, lastInput, lastCacheRead, lastCacheCreation int64
	var sawDeltaUsage bool // A message_delta with usage has been seen (the real usage breakdown has arrived)
	var tFirstByte time.Time
	br := bufio.NewReader(resp.Body)
	for {
		line, err := br.ReadBytes('\n')
		if len(line) > 0 {
			if tFirstByte.IsZero() {
				tFirstByte = time.Now()
			}
			raw.Write(line)
			if f != nil {
				f.appendContent(line)
				f.bytes.Add(int64(len(line)))
				parseSSEStats(line, &lastOutput, &lastInput, &lastCacheRead, &lastCacheCreation, &sawDeltaUsage)
			}
			t := bytes.TrimSpace(line)
			if bytes.HasPrefix(t, []byte("data:")) {
				payload := bytes.TrimSpace(t[5:])
				if len(payload) > 0 {
					var ev map[string]any
					if json.Unmarshal(payload, &ev) == nil {
						switch ev["type"] {
						case "message_start":
							if msg, ok := ev["message"].(map[string]any); ok {
								if mm, ok := msg["model"].(string); ok && model == "" {
									model = mm
								}
							}
						case "content_block_delta":
							if d, ok := ev["delta"].(map[string]any); ok {
								if d["type"] == "text_delta" {
									if s, ok := d["text"].(string); ok {
										textBuf.WriteString(s)
									}
								}
							}
						case "message_delta":
							if d, ok := ev["delta"].(map[string]any); ok {
								if s, ok := d["stop_reason"].(string); ok {
									stopReason = s
								}
							}
						}
					}
				}
			}
		}
		if err != nil {
			break
		}
	}
	if f != nil {
		if sawDeltaUsage {
			f.inTokens = lastInput
			f.cacheRead = lastCacheRead
			f.cacheCreation = lastCacheCreation
			f.outTokens = lastOutput
		} else {
			// Interrupted stream: roll back the counted message_start estimated usage; the aggregation counts only complete responses
			rollbackUsageStats(lastInput, lastCacheRead, lastCacheCreation, lastOutput)
		}
		tEnd := time.Now()
		if !tFirstByte.IsZero() {
			f.firstByteMs = tFirstByte.Sub(tSend).Milliseconds()
			streamMs := tEnd.Sub(tFirstByte).Milliseconds()
			if streamMs > 0 {
				f.tps = float64(lastOutput) / (float64(streamMs) / 1000.0)
			}
			log.Printf("[flight] #%d search step2 TTFT %.2fs stream %.2fs %d tok %.1f tok/s",
				f.id, float64(f.firstByteMs)/1000, float64(streamMs)/1000, lastOutput, f.tps)
		}
		realModel := model
		if realModel == "" {
			realModel = modelHint
		}
		stats.addModelUsage(realModel, f.inTokens, f.cacheRead, f.cacheCreation, f.outTokens)
	}
	m := map[string]any{
		"model":       model,
		"stop_reason": stopReason,
		"content":     []any{map[string]any{"type": "text", "text": textBuf.String()}},
	}
	return m, raw.Bytes(), nil
}

// searchExtractResults extracts the server_tool_use id and all web_search_tool_result result entries from the step-1 response.
// r1: step-1 response map. Returns (toolUseID, results, error).
func searchExtractResults(r1 map[string]any) (string, []map[string]any, error) {
	content, _ := r1["content"].([]any)
	toolUseID := ""
	var results []map[string]any
	for _, b := range content {
		bb, _ := b.(map[string]any)
		if bb == nil {
			continue
		}
		t, _ := bb["type"].(string)
		if t == "server_tool_use" && toolUseID == "" {
			if id, ok := bb["id"].(string); ok {
				toolUseID = id
			}
		}
		if t == "web_search_tool_result" {
			items, _ := bb["content"].([]any)
			for _, it := range items {
				im, _ := it.(map[string]any)
				if im != nil {
					results = append(results, im)
				}
			}
		}
	}
	if toolUseID == "" {
		toolUseID = "srvtoolu_proxy_search"
	}
	return toolUseID, results, nil
}

// searchParseSummaries splits the step-2 response text into summaries by "Result N:", returning n of them.
// r2: step-2 response map; n: expected count. Returns the summary slice in order (unmatched positions are empty strings).
func searchParseSummaries(r2 map[string]any, n int) []string {
	out := make([]string, n)
	text := ""
	if content, ok := r2["content"].([]any); ok {
		for _, b := range content {
			bb, _ := b.(map[string]any)
			if bb != nil && bb["type"] == "text" {
				if t, ok := bb["text"].(string); ok {
					text += t
				}
			}
		}
	}
	re := regexp.MustCompile(`(?im)^\s*\**\s*Result\s*(\d+)\s*[:\.\)]\s*\**\s*`)
	idx := re.FindAllStringSubmatchIndex(text, -1)
	if len(idx) == 0 {
		if n > 0 {
			out[0] = strings.TrimSpace(text)
		}
		return out
	}
	for i, m := range idx {
		numStart, numEnd := m[2], m[3]
		num, _ := strconv.Atoi(text[numStart:numEnd])
		start := m[1]
		var end int
		if i+1 < len(idx) {
			end = idx[i+1][0]
		} else {
			end = len(text)
		}
		s := strings.TrimSpace(text[start:end])
		if num >= 1 && num <= n {
			out[num-1] = s
		}
	}
	return out
}

// logSearchResponse prints a search response's model/stop_reason/block types/text preview.
// fid: stream ID; tag: stage label; r: response map.
func logSearchResponse(fid uint64, tag string, r map[string]any) {
	if e, ok := r["error"]; ok {
		log.Printf("[searchsum-debug] #%d %s response contains error=%v", fid, tag, e)
	}
	stop, _ := r["stop_reason"].(string)
	model, _ := r["model"].(string)
	var blockTypes []string
	var text string
	if content, ok := r["content"].([]any); ok {
		for _, b := range content {
			bb, _ := b.(map[string]any)
			if bb == nil {
				continue
			}
			t, _ := bb["type"].(string)
			blockTypes = append(blockTypes, t)
			if t == "text" {
				if s, ok := bb["text"].(string); ok {
					text += s
				}
			}
		}
	}
	log.Printf("[searchsum-debug] #%d %s model=%s stop=%s blocks=%v", fid, tag, model, stop, blockTypes)
	if text != "" {
		preview := text
		if len(preview) > 500 {
			preview = preview[:500] + "...(truncated)"
		}
		log.Printf("[searchsum-debug] #%d %s text:\n%s", fid, tag, preview)
	}
}

// searchDebugWrite writes a raw blob to <search debug dir>/#<fid>_<tag>; skipped when the dir is empty.
// fid: stream ID; tag: file tag; data: raw bytes.
func searchDebugWrite(fid uint64, tag string, data []byte) {
	dir := debugSearchDirOverride
	if dir == "" {
		if c := cfg.Load(); c != nil {
			dir = c.SearchDebugDir
		}
	}
	if dir == "" || len(data) == 0 {
		return
	}
	os.MkdirAll(dir, 0755)
	fname := fmt.Sprintf("%s%c#%d_%s", dir, os.PathSeparator, fid, tag)
	if err := os.WriteFile(fname, data, 0644); err != nil {
		log.Printf("[searchsum-debug] #%d failed to write %s: %v", fid, tag, err)
	}
}

// searchDebugAppend appends a raw blob to <search debug dir>/#<fid>_<tag>.
// fid: stream ID; tag: file tag; data: raw bytes.
func searchDebugAppend(fid uint64, tag string, data []byte) {
	dir := debugSearchDirOverride
	if dir == "" {
		if c := cfg.Load(); c != nil {
			dir = c.SearchDebugDir
		}
	}
	if dir == "" || len(data) == 0 {
		return
	}
	os.MkdirAll(dir, 0755)
	fname := fmt.Sprintf("%s%c#%d_%s", dir, os.PathSeparator, fid, tag)
	f, err := os.OpenFile(fname, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		return
	}
	defer f.Close()
	f.Write(data)
}

// rewriteResponseModel rewrites the "model" field value in SSE data: lines back to origModel.
// It doesn't rely on the model name the upstream actually returned (which may differ from the targetModel written in the request); any data: line containing a model field is rewritten.
// Non-data: lines are returned unchanged, as are lines where no model field is located or the value already equals origModel.
// Returns (rewritten line, model value actually returned by the upstream, whether a rewrite happened).
func rewriteResponseModel(line []byte, origModel string) ([]byte, string, bool) {
	prefix := []byte("data:")
	if !bytes.HasPrefix(line, prefix) {
		return line, "", false
	}
	jsonStart := len(prefix)
	for jsonStart < len(line) && (line[jsonStart] == ' ' || line[jsonStart] == '\t') {
		jsonStart++
	}
	curModel, _, _, ok := locateModel(line[jsonStart:])
	if !ok || curModel == origModel {
		return line, "", false
	}
	newJSON := replaceModelValue(line[jsonStart:], origModel)
	out := make([]byte, 0, jsonStart+len(newJSON))
	out = append(out, line[:jsonStart]...)
	out = append(out, newJSON...)
	return out, curModel, true
}

// isReservedRoutePattern reports whether a pattern exactly matches a reserved name (colliding routes don't take effect): "Fallback" is
// the * catch-all route, "fast_route" is the fast channel's entry name in the Codex menu — Codex scripts send these names as
// model (fast_route gets speed:"fast" injected by the translation layer and goes down the fast branch); a colliding route would intercept that traffic.
func isReservedRoutePattern(p string) bool {
	return p == "Fallback" || p == "fast_route"
}

// matchPassthroughResponsesRoute pre-checks whether a Responses-port request should pass through natively: the model matches
// the route table's first ordered non-reserved pattern (same rule as the handler's routing loop), and the matched route has url_response_api.
// Only for responsesHandler's "translate or passthrough" decision; the actual upstream switch happens again in the handler's routing loop.
func matchPassthroughResponsesRoute(c *Config, model string) *RouteRule {
	if model == "" {
		return nil
	}
	for i := range c.Routes {
		if isReservedRoutePattern(c.Routes[i].Pattern) {
			continue
		}
		if matchModel(c.Routes[i].Pattern, model) && c.Routes[i].URLResponseAPI != "" {
			return &c.Routes[i]
		}
	}
	return nil
}

// responsesAPIPath computes the join path for native passthrough: url_response_api may hold a base (…/coding),
// one with /v1, or a full …/v1/responses; only the missing difference is appended, never doubled. Used with upstream+upPath in the handler.
func responsesAPIPath(base string) string {
	b := strings.TrimRight(base, "/")
	if strings.HasSuffix(b, "/v1/responses") {
		return ""
	}
	if strings.HasSuffix(b, "/v1") {
		return "/responses"
	}
	return "/v1/responses"
}

// Split the pattern by *: the first segment must be a prefix of name, the last a suffix, and the middle segments must appear in order in what remains.
// pattern: the *-containing pattern; name: the actual model name. Returns whether it matches.
func matchModel(pattern, name string) bool {
	parts := strings.Split(pattern, "*")
	if len(parts) == 1 {
		return pattern == name // No *: exact match
	}
	// First segment as prefix.
	if !strings.HasPrefix(name, parts[0]) {
		return false
	}
	name = name[len(parts[0]):]
	// Middle segments in order.
	for i := 1; i < len(parts)-1; i++ {
		idx := strings.Index(name, parts[i])
		if idx < 0 {
			return false
		}
		name = name[idx+len(parts[i]):]
	}
	// Last segment as suffix.
	return strings.HasSuffix(name, parts[len(parts)-1])
}

// hasCatchAllRoute reports whether routes contains a pattern:"*" catch-all rule (matches every model).
// With one, the top-level upstream may be left empty — every model-bearing request is taken over by the catch-all route and the default upstream goes unused.
func hasCatchAllRoute(routes []RouteRule) bool {
	for i := range routes {
		if routes[i].Pattern == "*" {
			return true
		}
	}
	return false
}

// writeSSEPing writes an Anthropic-standard SSE ping event to the client and flushes, keeping the connection alive during 429 retries.
// Claude Code ignores ping events (Anthropic's official streams intersperse pings too); no message content is produced.
// Responses passthrough streams get an SSE comment line (": ping") instead — every SSE client ignores comment lines,
// whereas feeding an Anthropic-shaped ping JSON to a native Responses client could trigger parse warnings.
// w: response writer; flusher: flush after write when non-nil; f: this flight (for protocol shape, may be nil).
func writeSSEPing(w http.ResponseWriter, flusher http.Flusher, f *flight) {
	if f != nil && f.responsesRaw() {
		w.Write([]byte(": ping\n\n"))
	} else {
		w.Write([]byte("event: ping\ndata: {\"type\":\"ping\"}\n\n"))
	}
	if flusher != nil {
		flusher.Flush()
	}
}

// writeSSEError is the upstream-failure fallback after a 200 header has been sent: it writes an Anthropic SSE error event and flushes.
// Once the 200 header is sent, the status code can no longer pass a 429 through; an SSE error is the only way for the client to recognize the failure (used when retries run out).
// Responses passthrough streams get a response.failed event instead (native Responses clients recognize that shape).
// w: response writer; flusher: flush after write when non-nil; msg: error description; f: this flight (a copy is teed for web review).
func writeSSEError(w http.ResponseWriter, flusher http.Flusher, msg string, f *flight) {
	var buf bytes.Buffer
	b, _ := json.Marshal(msg) // JSON-encode the message so special characters don't break the SSE
	if f != nil && f.responsesRaw() {
		buf.WriteString("event: response.failed\ndata: {\"type\":\"response.failed\",\"response\":{\"id\":\"resp_p429\",\"object\":\"response\",\"status\":\"failed\",\"error\":{\"code\":\"server_error\",\"message\":")
		buf.Write(b)
		buf.WriteString("}}}\n\n")
	} else {
		buf.WriteString("event: error\ndata: {\"type\":\"error\",\"error\":{\"type\":\"overloaded_error\",\"message\":")
		buf.Write(b)
		buf.WriteString("}}\n\n")
	}
	data := buf.Bytes()
	w.Write(data)
	if flusher != nil {
		flusher.Flush()
	}
	if f != nil {
		f.gaveUp = true       // The finished-streams status column shows [重试尽] from this flag (distinguishing it from other status=0 shapes)
		f.appendContent(data) // Tee a copy for web review (the retries-exhausted error is recorded too)
	}
}

// startSSEKeepalive sends 200 + SSE headers + the first ping to the client on the first retry, starting the keepalive stream.
// Pings keep flowing during subsequent retries so Claude Code doesn't time out on the long silence and report an API error.
// Returns the flusher for later pings/forwarding. f: this flight (id for logging); reason: retry reason (logging).
func startSSEKeepalive(w http.ResponseWriter, f *flight, reason string) http.Flusher {
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)
	flusher, _ := w.(http.Flusher)
	writeSSEPing(w, flusher, f)
	log.Printf("[keepalive] #%d retrying (%s); SSE ping keepalive on", f.id, reason)
	return flusher
}

// sleepWithPing pings periodically to keep alive during the backoff wait, and can be interrupted by a client disconnect (ctx cancel).
// Replaces the original time.Sleep(wait): that one was uninterruptible — after a client timeout-disconnect the proxy kept sleeping and retrying, burning upstream quota for nothing.
// ctx: the client connection's context; w/flusher: for pings; wait: backoff duration; interval: ping interval; f: flight.
// A nil flusher means wait silently without pings: under convertAlltoStream the client expects a non-streaming response,
// so writing any SSE byte early would pollute the response; all we can do is wait (the client connection stays up, just dataless).
// Returning false means ctx was cancelled (client disconnected): stop retrying and end.
func sleepWithPing(ctx context.Context, w http.ResponseWriter, flusher http.Flusher, wait, interval time.Duration, f *flight) bool {
	timer := time.NewTimer(wait)
	defer timer.Stop()
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			log.Printf("[keepalive] #%d client disconnected; stopping retries", f.id)
			return false
		case <-ticker.C:
			if flusher != nil {
				writeSSEPing(w, flusher, f)
			}
		case <-timer.C:
			return true
		}
	}
}

// newSearchSubFlight builds a sub-flight for search-summary step1/step2 and registers it on the status page (visible among in-flight streams).
// label: the tag shown in the model column. Returns the new, already-registered flight (the caller owns unregister+addFinished).
func newSearchSubFlight(label string) *flight {
	f := &flight{
		id:        flights.nextID.Add(1),
		start:     time.Now(),
		origModel: label,
	}
	f.routeReason.Store(routeSearch)
	f.stageStart.Store(f.start.UnixNano())   // Light-color timing starts at stream creation
	f.attemptStart.Store(f.start.UnixNano()) // A sub-flight is about to send upstream the moment it's created; [尝试N] timing shares that start
	f.setStage(stageAttempt)
	flights.register(f)
	return f
}

// searchAndRespond runs summary mode: step1 search (non-streaming, fetching web_search results) + step2 summary (streaming, producing the detailed summary),
// building a Kimi-format SSE response (server_tool_use + web_search_tool_result + summary text) streamed back to the client.
// The main upstream is not called. Returns true on success (the handler should return directly); false on failure (the handler degrades to plain forwarding).
// w: client response; body: original request body; sf: search route config; f: current flight; origModel: original model name (backfilled into the model field).
func searchAndRespond(w http.ResponseWriter, body []byte, sf *SearchRoute, f *flight, origModel string) bool {
	fid := f.id
	f.routeReason.Store(routeSearch)
	f.setStage(stageForward)
	f.phase.Store(1)

	// step1: search (non-streaming), yielding server_tool_use + web_search_tool_result.
	step1Body, err := searchStep1Body(body, sf)
	if err != nil {
		log.Printf("[searchsum] #%d step1 build failed: %v", fid, err)
		return false
	}
	searchDebugWrite(fid, "step1_req.json", step1Body)
	step1F := newSearchSubFlight("search-step1·" + sf.Model)
	step1F.think = extractThinkMode(step1Body) // Status page 「API」 column thinking value = the config actually sent upstream
	r1, r1Raw, err := searchPostFlight(sf.URL, sf.API, step1Body, step1F, false, sf.Model)
	flights.unregister(step1F.id)
	addFinished(step1F)
	if err != nil {
		log.Printf("[searchsum] #%d step1 failed: %v", fid, err)
		return false
	}
	searchDebugWrite(fid, "step1_resp.json", r1Raw)
	logSearchResponse(fid, "step1", r1)
	toolUseID, results, err := searchExtractResults(r1)
	if err != nil || len(results) == 0 {
		log.Printf("[searchsum] #%d step1 no search results (results=%d err=%v)", fid, len(results), err)
		return false
	}

	// step1 succeeded: immediately send 200 + SSE headers and start ping keepalive, so a slow step2 summary (especially max+thinking)
	// doesn't leave the client dataless long enough to time out. A failed step1 still returns false and degrades to the fallback; a failure after headers can only send an SSE error.
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)
	f.status = http.StatusOK
	flusher, canFlush := w.(http.Flusher)
	pingInterval := 5 * time.Second
	if cc := cfg.Load(); cc != nil && cc.PingIntervalSec > 0 {
		pingInterval = time.Duration(cc.PingIntervalSec * float64(time.Second))
	}
	pingStop := make(chan struct{})
	pingDone := make(chan struct{})
	go func() {
		defer close(pingDone)
		// The synthesized stream is Anthropic-shaped (in this function f.translated is at most a "responses" translation, never passthrough),
		// so passing f makes writeSSEPing take the Anthropic ping branch automatically.
		writeSSEPing(w, flusher, f) // Send the first ping immediately so the client sees the connection is alive as early as possible
		ticker := time.NewTicker(pingInterval)
		defer ticker.Stop()
		for {
			select {
			case <-pingStop:
				return
			case <-ticker.C:
				writeSSEPing(w, flusher, f)
			}
		}
	}()
	pingStopped := false
	stopPing := func() {
		if pingStopped {
			return
		}
		pingStopped = true
		close(pingStop)
		<-pingDone // Wait for the goroutine to exit, avoiding concurrent writes to w later
	}
	defer stopPing() // Fallback: make sure pings are stopped before any return

	// step2: graded summary attempts. step1's results are cached, so even if every summary attempt fails, the search results can be returned directly (only descriptions are missing).
	// Attempt 1: configured level+thinking; on failure attempt 2: mid+thinking-off; on further failure summaryText stays empty (step1 results are returned).
	summaryText := ""
	for _, att := range []struct {
		level    string
		thinking bool
		label    string
	}{
		{sf.SummaryLevel, sf.SummaryThinking, "summary-step2"},
		{"mid", false, "summary-step2-fallback-mid"},
	} {
		step2Body, instr, err := searchStep2Body(r1, body, sf, att.level, att.thinking)
		if err != nil {
			log.Printf("[searchsum] #%d %s build failed: %v", fid, att.label, err)
			continue
		}
		searchDebugWrite(fid, att.label+"_req.json", step2Body)
		step2F := newSearchSubFlight(att.label + "·" + sf.Model)
		step2F.searchPrompt = instr
		step2F.think = extractThinkMode(step2Body) // Status page 「API」 column thinking value = the config actually sent upstream
		r2, r2Raw, err := searchPostFlight(sf.URL, sf.API, step2Body, step2F, true, sf.Model)
		flights.unregister(step2F.id)
		addFinished(step2F)
		if err != nil {
			log.Printf("[searchsum] #%d %s failed: %v", fid, att.label, err)
			continue
		}
		searchDebugWrite(fid, att.label+"_resp.sse", r2Raw)
		logSearchResponse(fid, att.label, r2)
		if content, ok := r2["content"].([]any); ok {
			for _, b := range content {
				bb, _ := b.(map[string]any)
				if bb != nil && bb["type"] == "text" {
					if t, ok := bb["text"].(string); ok {
						summaryText += t
					}
				}
			}
		}
		if summaryText != "" {
			break
		}
		log.Printf("[searchsum] #%d %s empty summary; trying next level", fid, att.label)
	}

	// Send the real content: stop pings, take exclusive ownership of w. An empty summary returns only step1's search results (no text block).
	stopPing()
	if summaryText == "" {
		log.Printf("[searchsum] #%d all summary attempts failed; returning step1 search results (no summary text)", fid)
	}
	msgID := fmt.Sprintf("msg_proxy_%d", fid)
	// writeSSE writes one SSE event and flushes, teeing to the flight + global traffic stats in passing.
	writeSSE := func(event string, data map[string]any) {
		payload, _ := json.Marshal(data)
		fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, payload)
		line := make([]byte, 0, len(event)+len(payload)+12)
		line = append(line, []byte("event: ")...)
		line = append(line, event...)
		line = append(line, []byte("\ndata: ")...)
		line = append(line, payload...)
		line = append(line, '\n', '\n')
		f.appendContent(line)
		f.bytes.Add(int64(len(line)))
		stats.bytesForward.Add(int64(len(line)))
		if canFlush {
			flusher.Flush()
		}
	}
	// 1) message_start
	writeSSE("message_start", map[string]any{
		"type": "message_start",
		"message": map[string]any{
			"id":            msgID,
			"type":          "message",
			"role":          "assistant",
			"model":         origModel,
			"content":       []any{},
			"stop_reason":   nil,
			"stop_sequence": nil,
			"usage":         map[string]any{"input_tokens": 1, "output_tokens": 1},
		},
	})
	// 2) server_tool_use (index 0)
	f.noteToolCall("web_search") // The synthesized stream is self-built and doesn't go through writeAndCount parsing, so count directly
	writeSSE("content_block_start", map[string]any{
		"type":  "content_block_start",
		"index": 0,
		"content_block": map[string]any{
			"type":  "server_tool_use",
			"id":    toolUseID,
			"name":  "web_search",
			"input": map[string]any{},
		},
	})
	writeSSE("content_block_stop", map[string]any{"type": "content_block_stop", "index": 0})
	// 3) web_search_tool_result (index 1; the raw results are backfilled, the dropdown shows title/url)
	writeSSE("content_block_start", map[string]any{
		"type":  "content_block_start",
		"index": 1,
		"content_block": map[string]any{
			"type":        "web_search_tool_result",
			"tool_use_id": toolUseID,
			"content":     results,
		},
	})
	writeSSE("content_block_stop", map[string]any{"type": "content_block_stop", "index": 1})
	// 4) Summary text (index 2): sent only when the summary succeeded; skipped when all summary attempts failed, so the client receives just the search results.
	if summaryText != "" {
		writeSSE("content_block_start", map[string]any{
			"type":          "content_block_start",
			"index":         2,
			"content_block": map[string]any{"type": "text", "text": ""},
		})
		for _, ch := range splitSummaryChunks(summaryText, 200) {
			writeSSE("content_block_delta", map[string]any{
				"type":  "content_block_delta",
				"index": 2,
				"delta": map[string]any{"type": "text_delta", "text": ch},
			})
		}
		writeSSE("content_block_stop", map[string]any{"type": "content_block_stop", "index": 2})
	}
	// 5) message_delta + message_stop to close
	writeSSE("message_delta", map[string]any{
		"type":  "message_delta",
		"delta": map[string]any{"stop_reason": "end_turn", "stop_sequence": nil},
		"usage": map[string]any{"output_tokens": estimateTokens(summaryText)},
	})
	writeSSE("message_stop", map[string]any{"type": "message_stop"})
	f.delivered.Store(true) // The synthesized stream has been fully written
	log.Printf("[searchsum] #%d done: results=%d summary=%d bytes", fid, len(results), len(summaryText))
	return true
}

// splitSummaryChunks cuts text into segments of at most maxLen (breaking at newline/period boundaries where possible), for streaming text_deltas.
// text: the original text; maxLen: per-segment byte cap. Returns the segments.
func splitSummaryChunks(text string, maxLen int) []string {
	if len(text) <= maxLen {
		return []string{text}
	}
	var out []string
	for len(text) > maxLen {
		cut := maxLen
		for i := maxLen; i > maxLen/2 && i < len(text); i-- {
			if text[i] == '\n' {
				cut = i + 1
				break
			}
		}
		out = append(out, text[:cut])
		text = text[cut:]
	}
	if text != "" {
		out = append(out, text)
	}
	return out
}

// estimateTokens roughly estimates the token count (3 chars/token, a compromise between Chinese and English).
// text: the original text. Returns the estimated token count.
func estimateTokens(text string) int {
	return len(text)/3 + 1
}

// forward passes the response through to the client: first puts back the peeked head, then streams the rest.
// While forwarding, it parses the SSE usage fields line by line, accumulating into the global stats for the status row;
// the forwarded bytes are simultaneously added to flight.bytes for the upper-half icon display, and phase=1 is marked.
// Returns this stream's final output_tokens, for the handler's token/s sample.
func forward(w http.ResponseWriter, resp *http.Response, head []byte, br *bufio.Reader, f *flight, headersSent bool) int64 {
	defer resp.Body.Close()
	// convertOff2Low native-port upgrade (off->low): strip thinking blocks from the response so the client keeps seeing the
	// thinking-off shape it asked for. Only on a 200 (error bodies pass through untouched), never on Responses passthrough.
	var stripper *thinkingStripper
	if f.stripThinking.Load() && !f.responsesRaw() && resp.StatusCode == http.StatusOK {
		stripper = newThinkingStripper(resp.Header.Get("Content-Type"))
	}
	if !headersSent {
		copyHeaders(w.Header(), resp.Header)
		if stripper != nil && !stripper.sse {
			// JSON-mode stripping changes the body length: drop the upstream's Content-Length, let net/http re-frame.
			w.Header().Del("Content-Length")
		}
		w.WriteHeader(resp.StatusCode)
	}

	flusher, canFlush := w.(http.Flusher)

	// Response received, forwarding starts: the icon switches from "awaiting first byte (elapsed)" to "forwarding (bytes)".
	f.phase.Store(1)
	f.status = resp.StatusCode
	f.setStage(stageForward) // The web 「状态」 column switches to showing the status code

	// Counted into active streams; the exit defer subtracts one.
	stats.mu.Lock()
	stats.active++
	stats.mu.Unlock()

	var lastOutput int64                 // This stream's cumulative output_tokens, for computing deltas
	var lastInput int64                  // This stream's cumulative input_tokens
	var lastCacheRead int64              // This stream's cumulative cache_read
	var lastCacheCreation int64          // This stream's cumulative cache_creation
	var sawDeltaUsage bool               // A message_delta with usage has been seen (the real usage breakdown has arrived)
	interrupted := false                 // Whether this stream broke midway (usage rolled back, not counted in the aggregation)
	openTools := map[int]*openToolCall{} // Anthropic: block index → in-progress tool call (for the *0 empty-args decision, survives across chunks)
	respTools := map[string]string{}     // Responses passthrough: item_id → tool name (aligns with output_item.done for the empty-args decision)
	defer func() {
		counted := false
		if lastInput == 0 && lastCacheRead == 0 && lastCacheCreation == 0 && lastOutput == 0 {
			// Non-streaming JSON response: SSE yields no usage, so extract it from the body (classifier and similar requests)
			if in, cr, cc, out, ok := parseNonStreamUsage(f.snapshotContent()); ok {
				f.inTokens = in
				f.cacheRead = cr
				f.cacheCreation = cc
				f.outTokens = out
				stats.mu.Lock()
				stats.inputTokens += in
				stats.cacheRead += cr
				stats.cacheCreation += cc
				stats.outputTokens += out
				stats.mu.Unlock()
				counted = true
			}
		} else if sawDeltaUsage {
			// Stream completed normally (real breakdown obtained): count into the aggregation.
			f.inTokens = lastInput
			f.cacheRead = lastCacheRead
			f.cacheCreation = lastCacheCreation
			f.outTokens = lastOutput
			counted = true
		} else {
			// Stream interrupted: only message_start's estimated usage has been counted globally; roll it back wholesale —
			// the aggregation counts only complete responses (consistent with Claude Code). f.* zeroed; the per-stream hit rate shows "-".
			rollbackUsageStats(lastInput, lastCacheRead, lastCacheCreation, lastOutput)
			f.inTokens, f.cacheRead, f.cacheCreation, f.outTokens = 0, 0, 0, 0
			interrupted = true
		}
		// Real upstream model: prefer the model name the upstream response actually returned (the upstream may route the request onward to another model);
		// when the upstream returned no model, fall back to the route target targetModel, or origModel for passthrough.
		realModel := f.upstreamModel
		if realModel == "" {
			realModel = f.targetModel
		}
		if realModel == "" {
			realModel = f.origModel
		}
		if counted {
			stats.addModelUsage(realModel, f.inTokens, f.cacheRead, f.cacheCreation, f.outTokens)
		}
		if interrupted {
			log.Printf("[interrupted] #%d response status %d size %s (stream incomplete; usage excluded from aggregates)", f.id, f.status, humanBytes(f.bytes.Load()))
		} else {
			log.Printf("[done] #%d response status %d size %s cache-hit %s", f.id, f.status, humanBytes(f.bytes.Load()), cacheHitRate(f.cacheRead, f.inTokens, f.cacheCreation))
		}
		stats.mu.Lock()
		stats.active--
		stats.mu.Unlock()
	}()
	// writeAndCount forwards a chunk of bytes and parses its data: lines to update stats.
	// With the convertOff2Low stripper active the order is: tee the original upstream-side bytes (model already written back) →
	// strip thinking blocks → write the stripped bytes downstream (teed to the downstream side) → byte counts post-strip → stats parse the original.
	writeAndCount := func(data []byte) {
		if len(data) == 0 {
			return
		}
		if f.targetModel != "" && f.targetModel != f.origModel {
			lines := bytes.Split(data, []byte("\n"))
			modified := false
			var actualModel string
			for i, line := range lines {
				newLine, am, mod := rewriteResponseModel(line, f.origModel)
				if mod {
					modified = true
					if actualModel == "" {
						actualModel = am
					}
				}
				lines[i] = newLine
			}
			if modified {
				data = bytes.Join(lines, []byte("\n"))
				if f.upstreamModel == "" && actualModel != "" {
					f.upstreamModel = actualModel
				}
				if !f.modelLogged.Swap(true) {
					log.Printf("[rewrite] #%d response stream model rewritten back %s -> %s", f.id, actualModel, f.origModel)
				}
			}
		}
		f.appendContent(data) // Tee a copy for the web upstream side (the full pre-strip truth, kept regardless of stripping)
		if f.searchDebug {
			searchDebugAppend(f.id, "main_resp.sse", data) // Record the main model's raw response when search-summary degrades
		}
		out := data
		if stripper != nil {
			out = stripper.feed(data) // convertOff2Low: thinking blocks dropped, kept block indexes renumbered (the client sees thinking-off)
		}
		if len(out) > 0 {
			w.Write(out)
			if canFlush {
				flusher.Flush()
			}
			n := int64(len(out))
			stats.bytesForward.Add(n) // Live traffic (bytes), growing with each forwarded chunk
			f.bytes.Add(n)            // Per-stream bytes, for the icon display (post-strip: what the client actually received)
			if stripper != nil {
				f.appendContentDown(out) // A stripped stream differs per side: the downstream side gets its own tee
			}
		}
		responsesRaw := f.responsesRaw() // Computed once outside the loop: passthrough streams parse by Responses semantics
		for _, line := range bytes.Split(data, []byte("\n")) {
			if responsesRaw {
				parseResponsesStreamStats(line, &lastOutput, &lastInput, &lastCacheRead, &lastCacheCreation, &sawDeltaUsage)
				if itemID, name, ok := parseResponsesToolStart(line); ok {
					f.noteToolCall(name) // Tool-call counting: the finished-streams list shows tags like [exec_command*1]
					respTools[itemID] = name
				}
				if itemID, empty, ok := parseResponsesToolDone(line); ok {
					if name := respTools[itemID]; name != "" {
						if empty {
							f.noteToolCallEmpty(name) // Arguments object empty: a single call's tag shows *0
						}
						delete(respTools, itemID)
					}
				}
				// response.completed/failed/incomplete = Responses-protocol terminal markers (the counterpart of Anthropic's
				// message_stop; same semantics as below: mark delivered as soon as it completes, don't wait for a clean EOF, so Codex's quick disconnect isn't misrecorded as 499).
				if bytes.HasPrefix(bytes.TrimSpace(line), []byte("data:")) &&
					(bytes.Contains(line, []byte("response.completed")) || bytes.Contains(line, []byte("response.failed")) || bytes.Contains(line, []byte("response.incomplete"))) {
					f.delivered.Store(true)
				}
				continue
			}
			parseSSEStats(line, &lastOutput, &lastInput, &lastCacheRead, &lastCacheCreation, &sawDeltaUsage)
			if ts, ok := parseToolCallStart(line); ok {
				f.noteToolCall(ts.name) // Tool-call counting: the finished-streams list shows [Read*1][Edit*3]
				tr := &openToolCall{name: ts.name}
				if !isEmptyArgsJSON(string(ts.input)) {
					tr.hasArgs = true // A server_tool_use's start is already the complete block: a non-empty input settles it immediately
				}
				openTools[ts.index] = tr
			}
			if idx, partial, ok := parseToolArgsDelta(line); ok {
				if tr := openTools[idx]; tr != nil {
					tr.notePartial(partial) // Accumulate argument fragments (≤64B); empty-args decided at stop
				}
			}
			if idx, ok := parseToolBlockStop(line); ok {
				if tr := openTools[idx]; tr != nil {
					if tr.argsEmpty() {
						f.noteToolCallEmpty(tr.name)
					}
					delete(openTools, idx)
				}
			}
			// message_stop = the stream is semantically fully sent; mark delivered immediately: clients often disconnect right after completion
			// (Codex closes the connection as soon as response.completed arrives); once ctx is cancelled the upstream EOF becomes unreadable,
			// and waiting for a clean EOF to mark would be too late (normal translation-port streams were once misrecorded as 499).
			if bytes.HasPrefix(bytes.TrimSpace(line), []byte("data:")) && bytes.Contains(line, []byte("message_stop")) {
				f.delivered.Store(true)
			}
		}
	}

	writeAndCount(head)
	for {
		line, err := br.ReadBytes('\n')
		if len(line) > 0 {
			writeAndCount(line)
		}
		if err != nil {
			if err == io.EOF {
				// A cleanly exhausted upstream stream means the last chunk has also been written downstream; mark fully delivered
				// (the closing defer uses this to tell "disconnected after completion" from "disconnected midway"; only the latter is re-recorded as 499).
				f.delivered.Store(true)
			}
			break
		}
	}
	if stripper != nil {
		// Flush what the stripper still holds: a partial trailing line (SSE), or the whole transformed document (JSON mode).
		if tail := stripper.flush(); len(tail) > 0 {
			w.Write(tail)
			if canFlush {
				flusher.Flush()
			}
			stats.bytesForward.Add(int64(len(tail)))
			f.bytes.Add(int64(len(tail)))
			f.appendContentDown(tail)
		}
		if stripper.stripped > 0 {
			log.Printf("[off->low] #%d stripped %d thinking block(s) from the response (client sees thinking-off)", f.id, stripper.stripped)
		}
	}
	// Tool calls still open at stream end (missing content_block_stop, e.g. a truncated stream) get their empty-args verdict from what was received,
	// a best-effort *0 backfill; for normally ended streams openTools is already empty here.
	for _, tr := range openTools {
		if tr.argsEmpty() {
			f.noteToolCallEmpty(tr.name)
		}
	}
	return lastOutput
}

// collectStreamToJSON drains the SSE stream returned by the upstream, rebuilds an Anthropic non-streaming message JSON as-is,
// and writes it to the client in one shot. Under convertAlltoStream the request was sent upstream as a stream, but the client still expects
// non-streaming JSON, so the stream must be fully buffered first (the web in-flight view stays live: every chunk is teed to the flight).
// Rebuild rules (streaming is an event stream, non-streaming is whole blocks; the structures differ and need reassembly):
//   - message_start's message object is kept as the skeleton (id/type/role/model etc.);
//   - each content_block_start's block skeleton is kept by index — web_search_tool_result (with
//     encrypted_content), server_tool_use and similar wholly inlined fields stay untouched;
//   - delta events only append to known fields: text_delta->text, thinking_delta->thinking,
//     signature_delta->signature, input_json_delta->input (tool_use arguments, parsed into an object once complete);
//   - usage merge: message_start as the base, message_delta overwriting (last-value semantics, consistent with the stats logic).
//
// w: client response writer; resp/head/br: the upstream response (head is what peekHead already read, handled first);
// f: this flight. Returns (whether message_stop was fully received, output token count).
// On an incomplete stream nothing is written to the client, so the caller can safely retry the whole thing (resend the streaming request, drain again).
func collectStreamToJSON(w http.ResponseWriter, resp *http.Response, head []byte, br *bufio.Reader, f *flight) (bool, int64) {
	defer resp.Body.Close()
	f.phase.Store(1)
	f.status = resp.StatusCode
	f.setStage(stageForward)
	stats.mu.Lock()
	stats.active++
	stats.mu.Unlock()
	defer func() {
		stats.mu.Lock()
		stats.active--
		stats.mu.Unlock()
	}()

	var msgObj map[string]interface{}          // message_start's message skeleton
	var usageObj map[string]interface{}        // Merged usage: start as the base, delta overwriting
	blocks := map[int]map[string]interface{}{} // index -> content block (start skeleton + delta accumulation)
	var lastOutput, lastInput, lastCacheRead, lastCacheCreation int64
	complete := false

	// strVal takes a string field from a map (missing or non-string returns an empty string).
	strVal := func(m map[string]interface{}, k string) string {
		if m == nil {
			return ""
		}
		if v, ok := m[k].(string); ok {
			return v
		}
		return ""
	}

	// handleChunk processes a chunk of raw SSE bytes (possibly multiple lines): tees each line to the web and stats, and parses the event structure.
	handleChunk := func(chunk []byte) {
		for _, line := range bytes.SplitAfter(chunk, []byte("\n")) {
			if len(line) == 0 {
				continue
			}
			stats.bytesForward.Add(int64(len(line)))
			f.bytes.Add(int64(len(line)))
			f.appendContent(line)
			if f.searchDebug {
				searchDebugAppend(f.id, "main_resp.sse", line)
			}
			parseSSEStats(line, &lastOutput, &lastInput, &lastCacheRead, &lastCacheCreation, nil)

			s := bytes.TrimRight(line, "\r\n")
			if !bytes.HasPrefix(s, []byte("data:")) {
				continue
			}
			payload := bytes.TrimSpace(bytes.TrimPrefix(s, []byte("data:")))
			if len(payload) == 0 || bytes.Equal(payload, []byte("[DONE]")) {
				continue
			}
			var ev map[string]interface{}
			if json.Unmarshal(payload, &ev) != nil {
				continue
			}
			switch ev["type"] {
			case "message_start":
				if msg, ok := ev["message"].(map[string]interface{}); ok {
					msgObj = msg
					if u, ok := msg["usage"].(map[string]interface{}); ok {
						usageObj = make(map[string]interface{}, len(u))
						for k, v := range u {
							usageObj[k] = v
						}
					}
					if m, ok := msg["model"].(string); ok && m != "" && f.upstreamModel == "" {
						f.upstreamModel = m
					}
				}
			case "content_block_start":
				if cb, ok := ev["content_block"].(map[string]interface{}); ok {
					blocks[int(toInt64(ev["index"]))] = cb
					if t, _ := cb["type"].(string); t == "tool_use" || t == "server_tool_use" {
						f.noteToolCall(strVal(cb, "name")) // Tool-call counting (the JSON-assembly path doesn't go through writeAndCount)
					}
				}
			case "content_block_delta":
				b := blocks[int(toInt64(ev["index"]))]
				d, _ := ev["delta"].(map[string]interface{})
				if b == nil || d == nil {
					continue
				}
				switch d["type"] {
				case "text_delta":
					b["text"] = strVal(b, "text") + strVal(d, "text")
				case "thinking_delta":
					b["thinking"] = strVal(b, "thinking") + strVal(d, "thinking")
				case "signature_delta":
					b["signature"] = strVal(b, "signature") + strVal(d, "signature")
				case "input_json_delta":
					b["input"] = strVal(b, "input") + strVal(d, "partial_json")
				}
			case "message_delta":
				if d, ok := ev["delta"].(map[string]interface{}); ok && msgObj != nil {
					if sr, ok := d["stop_reason"].(string); ok {
						msgObj["stop_reason"] = sr
					}
					if ss, ok := d["stop_sequence"]; ok {
						msgObj["stop_sequence"] = ss
					}
				}
				if u, ok := ev["usage"].(map[string]interface{}); ok {
					if usageObj == nil {
						usageObj = make(map[string]interface{}, len(u))
					}
					for k, v := range u {
						usageObj[k] = v
					}
				}
			case "message_stop":
				complete = true
			}
		}
	}

	handleChunk(head)
	for {
		line, err := br.ReadBytes('\n')
		if len(line) > 0 {
			handleChunk(line)
		}
		if err != nil {
			break
		}
	}
	if !complete {
		// Incompletely received streams don't count in the aggregation (they only have message_start's estimated usage); rolled back, ready for a wholesale retry
		rollbackUsageStats(lastInput, lastCacheRead, lastCacheCreation, lastOutput)
		log.Printf("[streamify] #%d upstream stream incomplete (no message_stop); nothing written to client, whole request retried", f.id)
		return false, lastOutput
	}

	// Rebuild the non-streaming message JSON. model is written back to the client's original model (route rewriting is transparent to the client).
	if msgObj == nil {
		// Abnormal streams missing message_start: build a minimal skeleton so the returned structure stays complete.
		msgObj = map[string]interface{}{"type": "message", "role": "assistant"}
	}

	// Blocks are backfilled into content sorted by index; a string input (assembled from input_json_deltas) is parsed into an object when possible.
	indexes := make([]int, 0, len(blocks))
	for i := range blocks {
		indexes = append(indexes, i)
	}
	sort.Ints(indexes)
	content := make([]interface{}, 0, len(indexes))
	for _, i := range indexes {
		b := blocks[i]
		// convertOff2Low upgrade: thinking blocks never reach the assembled response (the client asked for thinking-off)
		if f.stripThinking.Load() {
			if t, _ := b["type"].(string); t == "thinking" || t == "redacted_thinking" {
				continue
			}
		}
		if s, ok := b["input"].(string); ok && s != "" {
			var obj interface{}
			if json.Unmarshal([]byte(s), &obj) == nil {
				b["input"] = obj
			}
		}
		// A tool block's input is final at this point: judge emptiness and backfill the *0 tag (start only counted; empty-args looks at the final state).
		if t, _ := b["type"].(string); t == "tool_use" || t == "server_tool_use" {
			empty := false
			switch inp := b["input"].(type) {
			case nil:
				empty = true
			case string:
				empty = isEmptyArgsJSON(inp)
			case map[string]interface{}:
				empty = len(inp) == 0
			}
			if empty {
				f.noteToolCallEmpty(strVal(b, "name"))
			}
		}
		content = append(content, b)
	}
	msgObj["content"] = content
	if usageObj != nil {
		msgObj["usage"] = usageObj
	}
	// model is written back to the client's original model (route rewriting is transparent to the client).
	if f.targetModel != "" && f.targetModel != f.origModel {
		msgObj["model"] = f.origModel
		if f.upstreamModel != "" && !f.modelLogged.Swap(true) {
			log.Printf("[rewrite] #%d response stream model rewritten back %s -> %s", f.id, f.upstreamModel, f.origModel)
		}
	}
	out, err := json.Marshal(msgObj)
	if err != nil {
		return false, lastOutput
	}

	// Write the complete response in one shot (the client expects non-streaming JSON; not a single byte may be written before it's complete).
	f.inTokens = lastInput
	f.cacheRead = lastCacheRead
	f.cacheCreation = lastCacheCreation
	f.outTokens = lastOutput
	stats.addModelUsage(f.realModel(), f.inTokens, f.cacheRead, f.cacheCreation, f.outTokens)
	log.Printf("[done] #%d response status %d size %s cache-hit %s", f.id, f.status, humanBytes(f.bytes.Load()), cacheHitRate(f.cacheRead, f.inTokens, f.cacheCreation))

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	w.Write(out)
	if flusher, ok := w.(http.Flusher); ok {
		flusher.Flush()
	}
	stats.bytesForward.Add(int64(len(out)))
	f.bytes.Add(int64(len(out)))
	f.appendContentDown(out) // Tee the rebuilt non-streaming JSON to the downstream side (the actual proxy→client shape; the upstream-side SSE lines are teed line by line into content by handleChunk)
	// The assembled JSON has been written in one shot = fully delivered (499 re-marking only looks at incompletely sent streams)
	f.delivered.Store(true)
	log.Printf("[streamify] #%d stream complete; non-streaming JSON returned to client in one piece (%d output tokens)", f.id, lastOutput)
	return true, lastOutput
}

// handler is the entry point of all requests: buffer the request body → turn off thinking on classifier hits →
// forward with retries → stream the response through.
// Three retry cases:
//
//	A. the upstream's HTTP status code itself is 429/5xx;
//	B. status 200, but the error hides in the SSE response body (event:error / rate_limit etc.);
//	C. normal response, passed straight through.
//
// countTokensPath is Anthropic's token-counting endpoint: Claude Code calls it periodically for context-length estimation;
// the response is just {"input_tokens":N}, generating no content. These streams are forwarded upstream as usual, merely tagged so the web can tell them apart.
const countTokensPath = "/v1/messages/count_tokens"

func handler(w http.ResponseWriter, r *http.Request) {
	c := cfg.Load()

	// Access control: the forwarding channel only ever allows localhost — even mistakenly setting listen to 0.0.0.0 won't
	// let LAN devices freeload on the API keys configured in routes. The management endpoints (/__logs, /__config, /__reload) have their own isLocalRequest guard.
	if !isLocalRequest(r) {
		http.Error(w, "forbidden (local only)", http.StatusForbidden)
		return
	}

	// Claude Code sends HEAD /, HEAD /api/hello etc. at startup to probe connectivity.
	// Upstreams answer 404/401 for such paths, which would be misread as network unreachable/auth failure and would pollute the in-flight/finished lists.
	// Normal Anthropic API requests are all POST; a HEAD is always a probe — answer 200 directly, without creating a flight or hitting the upstream.
	if r.Method == http.MethodHead {
		w.WriteHeader(http.StatusOK)
		return
	}
	// Browsers automatically request /favicon.ico when the console page refreshes (not an API request); answer 204 directly,
	// so it isn't treated as a forwarding request — creating a flight and an upstream auth error that pollute the in-flight/finished lists.
	if r.URL.Path == "/favicon.ico" {
		w.WriteHeader(http.StatusNoContent)
		return
	}

	// Create a flight for this request: the web console shows in-flight streams (#id/elapsed/bytes).
	// ctx cancels automatically when the client disconnects (r.Context()); both the upstream request and retry waits build on it, so a disconnect aborts retries.
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	f := &flight{
		id:    flights.nextID.Add(1),
		start: time.Now(),
	}
	f.stageStart.Store(f.start.UnixNano()) // Light-color timing starts at stream creation (the white light times from request receipt)
	flights.register(f)
	// Internal requests from the Responses translation port carry a context marker tagging the flight's origin;
	// the web API column shows [translate]/[Response] (alongside the route-reason tag).
	if v, ok := r.Context().Value(ctxKeyTranslated).(string); ok {
		f.translated = v
	}
	// Internal requests from the Responses port carry the session identifier (prompt_cache_key) via context, anchoring the "缓存年龄" column per session.
	if v, ok := r.Context().Value(ctxKeyConvID).(string); ok {
		f.convID = v
	}
	// The responses-raw passthrough port carries the thinking mode via context (reasoning.effort verbatim — passthrough modifies nothing,
	// so the upstream receives exactly it; the translation port's/direct port's thinking value is extracted from the Anthropic body after rewriting below).
	if v, ok := r.Context().Value(ctxKeyThink).(string); ok {
		f.think = v
	}
	// convertOff2Low upgrade modes (the Responses translation port quietly upgrading off-thinking to low / sending the downstream's
	// requested effort when history is unreplayable; see the n2l constants): the API column shows a two-tone [off->low] badge for implicit upgrades; in the forwarding loop,
	// an upstream 400 rejecting thinking falls back to one retry with thinking off (both modes are covered). The Anthropic native port
	// sets the same mode directly at its own upgrade point below (convFlag=="all"), no ctx involved.
	n2lMode, _ := r.Context().Value(ctxKeyNone2Low).(int)
	// Dual-link recording: the Responses translation port's downstream-side response (the bytes actually written proxy→client) is teed
	// in full into contentDown via translatingWriter's tap; on the native port w is not a translatingWriter,
	// so the type assertion skips naturally (native streams are identical on both sides; the upstream-side copy suffices).
	if t, ok := w.(interface{ setDownTap(*flight) }); ok {
		t.setDownTap(f)
	}
	// The search-restore context brought by the Responses translation port: proactive watermark strips count into the flight (shown as [剥N]);
	// the replay pointer stays on the flight, and the 400-fallback strip learns the conversation watermark from restore timestamps
	// (see the tool_call_id branch in the forwarding loop).
	if v, ok := r.Context().Value(ctxKeySearchReplay).(*searchReplayCtx); ok && v != nil {
		f.searchReplay = v
		if v.proactiveCutoff > 0 {
			f.searchStripped.Store(int32(v.proactiveCutoff))
			log.Printf("[strip] #%d conversation watermark stripped %d replayed search blocks", f.id, v.proactiveCutoff)
		}
	}
	// Tag count_tokens probes: a finished stream's raw content is only {"input_tokens":N};
	// without the tag it looks like an "empty response" on the web, hard to tell a probe from an anomaly.
	if r.URL.Path == countTokensPath {
		f.countTokens = true
	}
	// Cleanup fallback: all return paths unregister via one defer, so nothing is left behind in the in-flight list by omission.
	// After unregistering, its pass-through content (if any) is archived for the web to review among finished streams.
	defer func() {
		markClientGone(f, ctx)
		flights.unregister(f.id)
		addFinished(f)
	}()

	// 1. Buffer the request body for replay on retries.
	body, err := io.ReadAll(r.Body)
	if err != nil {
		log.Printf("[request] #%d %s %s failed to read body: %v", f.id, r.Method, r.URL.Path, err)
		http.Error(w, "read body failed", http.StatusBadGateway)
		return
	}
	r.Body.Close()
	// Keep a copy of the request body for the web (which request caused this stream): this is the initial value of the proxy→upstream side;
	// each attempt of the retry loop refreshes it with the body actually sent (after a fallback rewrite, the last one wins); streams that die young
	// before dispatch (e.g. bad JSON) still have this entry body to inspect.
	f.setReqBody(body)
	// The downstream-side (downstream→proxy) request body of dual-link recording: the translation port brings the Responses original via context
	// (necessarily different from the translated body, so always stored); native streams defer to a comparison with the first upstream-bound body and are stored only on difference.
	var downBody []byte
	if v, ok := r.Context().Value(ctxKeyReqDown).([]byte); ok && len(v) > 0 {
		f.setReqDown(v)
	} else {
		downBody = body
	}
	// Anthropic port: extract the session identifier from the request body's metadata.user_id (the Responses port already carries it via context; no double parsing).
	if f.convID == "" {
		f.convID = extractConvID(body)
	}

	if c.LogRequestDetail {
		logRequestDetail(r, body)
	}
	// Extract model: shared by the [请求] log display and route matching. Empty for non-JSON / no model.
	origModel := extractModel(body)
	modelPart := ""
	if origModel != "" {
		modelPart = " model=" + origModel
	}
	if f.translated == translatedResponsesRaw {
		modelPart += " [direct:responses]"
	} else if f.translated != "" {
		modelPart += " [translate:" + f.translated + "]"
	}
	log.Printf("[request] #%d %s %s%s (body=%d bytes) from %s", f.id, r.Method, r.URL.Path, modelPart, len(body), r.RemoteAddr)

	// 1.5 Turn off thinking on classifier hits, so classification returns fast.
	// isClassifier also feeds the routing decision: classifier routing takes priority over model routing.
	isClassifier := isClassifierRequest(c, body)
	if isClassifier {
		stats.classifierHits.Add(1) // Count on match: tallied whether or not rerouted/de-thought (observing Claude Code's safety-check request volume)
	}
	body = maybeRewriteClassifier(body)

	// The status page's 「API」 column thinking value = the thinking config actually sent upstream: extracted after the classifier rewrite, so the translation port's
	// mapped thinking is what shows (e.g. Codex effort high → on 16384), not what the client originally sent.
	// A responses-raw passthrough body is Responses format (no thinking field); its reasoning.effort
	// was already carried in via ctx above and is not overwritten here.
	if f.think == "" {
		f.think = extractThinkMode(body)
	}
	// Implicit-upgrade streams (downstream explicitly off → low) get their thinking value overwritten to [off->low] (extractThinkMode
	// sees the upgraded low, which can't express the quiet "downstream off, upstream on" upgrade). History-fallback thinking-on
	// (sent at the downstream's requested effort) is not overwritten: showing the requested effort is truthful (the downstream wanted thinking).
	if n2lMode == n2lStealth {
		f.think = "off->low"
	}

	// 1.6 Route matching.
	// Priority: classifier route > fast route > model route (by routes wildcard).
	// On no match: upstream=c.Upstream, authToken="" (pass through the client's token, the original logic).
	f.setStage(stageRoute)
	upstream := c.Upstream
	authToken := ""
	var targetModel string
	fastRouteHit := false
	searchSummaryMode := false // Search-summary mode: step1+step2 self-built response, bypassing the main upstream.
	convFlag := ""             // The effective route's convertOff2Low value, captured where routing settles ("" = feature off)
	var summarySF *SearchRoute // Built from the matched RouteRule when enhance-search triggers; otherwise c.SearchFallback is used
	if isClassifier && c.ClassifierRoute != nil && c.ClassifierRoute.URL != "" {
		// Classifier route: shunt safety-check requests to the designated upstream, saving main-model quota.
		// (url empty = no reroute: a bare {"classifier_thinking":"off"} configures the thinking rewrite only.)
		cr := c.ClassifierRoute
		upstream = cr.URL
		authToken = cr.API
		if cr.Model != "" && cr.Model != origModel {
			body = replaceModelValue(body, cr.Model)
			targetModel = cr.Model
		}
		log.Printf("[route] #%d classifier %s -> %s (model %s -> %s)", f.id, origModel, cr.URL, origModel, cr.Model)
		f.routeReason.Store(routeClassifier)
		f.convAnchor = "classifier"
	} else if c.FastRoute != nil && isFastRequest(body, r) {
		// Fast route: non-classifier requests detected with "speed":"fast" are shunted to the designated upstream.
		fr := c.FastRoute
		upstream = fr.URL
		authToken = fr.API
		fastRouteHit = true
		convFlag = fr.ConvertOff2Low
		if fr.Model != "" && fr.Model != origModel {
			body = replaceModelValue(body, fr.Model)
			targetModel = fr.Model
		}
		body = removeSpeedField(body)
		log.Printf("[route] #%d fast %s -> %s (model %s -> %s)", f.id, origModel, fr.URL, origModel, fr.Model)
		f.routeReason.Store(routeFast)
		f.convAnchor = "fast"
	} else if len(c.Routes) > 0 && origModel != "" {
		for i := range c.Routes {
			// "Fallback"/"fast_route" are reserved names (the * catch-all route, the fast channel's name in the Codex menu):
			// patterns may not match them exactly; colliding routes don't take effect (loadConfig warns), so they can't intercept Codex traffic.
			if isReservedRoutePattern(c.Routes[i].Pattern) {
				continue
			}
			if matchModel(c.Routes[i].Pattern, origModel) {
				rr := &c.Routes[i]
				// The cache anchor key is recorded once on the routing hit (the passthrough branch passes through here too, naturally covered):
				// only same session + same route form one cache lineage, so utility hops (title generation etc.) can't steal the main conversation's anchor.
				f.convAnchor = "route:" + rr.Pattern
				// Responses native passthrough: this request came from the Responses port and the pre-check found the matched route has url_response_api.
				// The upstream switches to that field (the Anthropic-only image/search fallbacks and enhance-search are meaningless for it — skipped).
				if f.responsesRaw() {
					if rr.URLResponseAPI == "" {
						// Present at pre-check, gone now — only a hot config reload could have removed the field; say 502 explicitly instead of silently misrouting.
						log.Printf("[error] #%d Responses passthrough request hit route %q but its url_response_api is gone (config hot-reloaded?); cannot forward", f.id, rr.Pattern)
						http.Error(w, "route lost url_response_api mid-request", http.StatusBadGateway)
						return
					}
					upstream = rr.URLResponseAPI
					authToken = rr.API
					if rr.Model != "" && rr.Model != origModel {
						body = replaceModelValue(body, rr.Model)
						targetModel = rr.Model
					}
					log.Printf("[route] #%d %s -> %s (Responses native passthrough, model %s -> %s)", f.id, origModel, rr.URLResponseAPI, origModel, rr.Model)
					f.routeReason.Store(routePattern)
					break
				}
				// Capability fallback: when the target upstream lacks image/search capability the request needs, switch to the corresponding fallback upstream.
				needImage := rr.TextOnly && hasImage(body)
				needSearch := rr.NoSearch && hasWebSearch(body)
				if needImage || needSearch {
					// applyFB assigns the selected fallback upstream to the current request (changing upstream/API/model).
					applyFB := func(url, api, model, label string, reason int32) {
						upstream = url
						authToken = api
						if model != "" && model != origModel {
							body = replaceModelValue(body, model)
							targetModel = model
						}
						f.routeReason.Store(reason)
						log.Printf("[route] #%d %s %s -> %s (model %s -> %s)", f.id, label, origModel, url, origModel, model)
					}
					sf := c.SearchFallback
					mf := c.MultimodalFallback
					// Search: hand the whole request to search_fallback, images or not (no evidence multimodal+search ever co-occur).
					//    summary_mode takes priority (step1 search + step2 summary); otherwise forward the whole request.
					if needSearch && sf != nil {
						if sf.SummaryMode {
							searchSummaryMode = true
							f.routeReason.Store(routeSearch)
							log.Printf("[route] #%d search-summary mode %s -> %s (model %s -> %s)", f.id, origModel, sf.URL, origModel, sf.Model)
							break
						}
						applyFB(sf.URL, sf.API, sf.Model, "search-fallback", routeSearch)
						convFlag = sf.ConvertOff2Low // The fallback's own flag overwrites the matched route's
						break
					}
					// Pure images (no search): use multimodal_fallback when configured; search requests never land on mf (degrade to passthrough when there's no sf).
					if needImage && mf != nil && !needSearch {
						applyFB(mf.URL, mf.API, mf.Model, "image-fallback", routeMultimodal)
						convFlag = mf.ConvertOff2Low // The fallback's own flag overwrites the matched route's
						break
					}
					// Degrade: search without sf, or pure images without mf — pass through to the original route (the upstream handles it, possibly with an error).
				}
				upstream = rr.URL
				authToken = rr.API
				if rr.Model != "" && rr.Model != origModel {
					body = replaceModelValue(body, rr.Model)
					targetModel = rr.Model
				}
				log.Printf("[route] #%d %s -> %s (model %s -> %s)", f.id, origModel, rr.URL, origModel, rr.Model)
				f.routeReason.Store(routePattern)
				convFlag = rr.ConvertOff2Low
				// Enhance search: when the route supports search (no_search:false) and has enhance_search configured, a request carrying search tools
				// skips the main model and uses this route's own upstream in kimi summary mode (equivalent to no_search going through search_fallback.summary_mode,
				// except the search upstream is the route's own url/api/model and the summary parameters come from the route's enhance_search).
				if rr.EnhanceSearch != nil && hasWebSearch(body) && !rr.NoSearch && !(rr.TextOnly && hasImage(body)) {
					searchSummaryMode = true
					summarySF = &SearchRoute{
						URL:             rr.URL,
						API:             rr.API,
						Model:           rr.Model,
						ConvertOff2Low:  rr.ConvertOff2Low,
						SummaryMode:     true,
						SummaryLevel:    rr.EnhanceSearch.SummaryLevel,
						SummaryThinking: rr.EnhanceSearch.SummaryThinking,
					}
					log.Printf("[route] #%d enhanced search %s -> %s (model %s)", f.id, origModel, rr.URL, rr.Model)
				}
				break // Ordered: stop at the first hit
			}
		}
	}

	f.origModel = origModel
	f.targetModel = targetModel

	// Search-summary mode: step1 search + step2 summary + self-built Kimi-format response, bypassing the main upstream.
	// On success, return directly; on failure, degrade to search_fallback whole-request forwarding (Kimi mode).
	if searchSummaryMode {
		sf := summarySF
		if sf == nil {
			sf = c.SearchFallback
		}
		if sf != nil && searchAndRespond(w, body, sf, f, origModel) {
			return
		}
		log.Printf("[searchsum] #%d failed; falling back to routing the whole request to %s", f.id, func() string {
			if sf != nil {
				return sf.URL
			}
			return upstream
		}())
		if sf != nil {
			upstream = sf.URL
			authToken = sf.API
			if sf.Model != "" && sf.Model != origModel {
				body = replaceModelValue(body, sf.Model)
				targetModel = sf.Model
			}
			convFlag = sf.ConvertOff2Low // The degraded whole-request forwarding answers to the fallback route's own flag
			f.searchDebug = true         // When degrading to Kimi whole-request forwarding, record the main model's raw response for comparison
		}
	}

	// upstream may be left empty (with a pattern:"*" catch-all, the default upstream goes unused); still empty here
	// means no route matched and no default upstream is configured — retrying is pointless, so answer 502 with the reason spelled out.
	if upstream == "" {
		log.Printf("[error] #%d no route matched and default upstream is empty; cannot forward", f.id)
		http.Error(w, "no route matched and upstream is empty", http.StatusBadGateway)
		return
	}

	// The upstream grouping key is recorded once with the route's final values (the grouping dimension for observed cache lifetime: url+model, other parameters ignored).
	// A successful search-summary mode returns early, never reaching here; the degraded-forwarding branch has already rewritten upstream/targetModel to their final values.
	{
		m := targetModel
		if m == "" {
			m = origModel
		}
		f.upstreamKey = upstream + "|" + m
		// The search-envelope ownership triple is injected with the route's final values (only the Responses translation port's
		// translatingWriter has this method; the Anthropic port's type assertion fails and skips naturally). The key uses the token actually in effect:
		// when the route key is empty, the client's Authorization is what gets passed through — same semantics as the replay side's effectiveKey.
		if t, ok := w.(interface{ setSearchTriple(string, string, string) }); ok {
			effKey := authToken
			if effKey == "" {
				effKey = r.Header.Get("Authorization")
			}
			t.setSearchTriple(upstream, effKey, m)
		}
	}

	// convertOff2Low="all" native-port upgrade: an explicit downstream thinking-off is quietly sent upstream as low thinking,
	// with thinking blocks stripped from the response (the client stays unaware). The classifier's thinking is governed by
	// classifier_thinking exclusively, and Responses-port requests already carry their own upgrade decision via ctx — both excluded.
	if convFlag == "all" && !isClassifier && f.translated == "" && r.URL.Path == "/v1/messages" {
		effModel := targetModel
		if effModel == "" {
			effModel = origModel
		}
		if nb, upgraded, abandoned := maybeUpgradeOffToLow(body, effModel); upgraded {
			log.Printf("[off->low] #%d explicit thinking-off quietly upgraded to low thinking (body %d->%d bytes); thinking blocks stripped on return", f.id, len(body), len(nb))
			body = nb
			n2lMode = n2lStealth // Reuses the one-shot 400 fallback below (retreat to thinking-off if the upstream rejects thinking)
			f.think = "off->low"
			f.stripThinking.Store(true)
		} else if abandoned {
			log.Printf("[off->low] #%d upgrade abandoned: max_tokens too small for the 1024-token budget floor; request stays thinking-off", f.id)
		}
	}

	// 1.7 Global stream-conversion (convertAlltoStream): when on, all non-streaming requests are sent upstream as streams;
	// the complete SSE stream is collected and rebuilt into a non-streaming JSON returned to the client in one shot (transparent to the client; the web monitors streaming/first-byte/tok/s).
	// Only the Anthropic Messages API (/v1/messages) is converted: OpenAI-compatible paths have a different SSE format and can't be rebuilt.
	// Search-summary mode builds its own response and never touches the main upstream; already-streaming requests stream back anyway — neither is affected.
	convertToStream := false
	if c.ConvertAllToStream && !searchSummaryMode && r.URL.Path == "/v1/messages" {
		if stream, ok := getBoolField(body, "stream"); !ok || !stream {
			body = forceStreamTrue(body)
			convertToStream = true
			log.Printf("[streamify] #%d non-streaming request sent upstream as streaming (stream:%v -> true)", f.id, stream)
		}
	}

	deadline := time.Now().Add(time.Duration(c.TotalBudgetSec * float64(time.Second)))
	// headersSent: whether the 200 + SSE headers have been sent to the client (set true on the first retry's keepalive).
	// Once true, later successful forwards skip WriteHeader/copyHeaders (headers already sent), and retries-exhausted sends an SSE error instead.
	headersSent := false
	var flusher http.Flusher // Assigned by startSSEKeepalive once keepalive starts
	pingInterval := time.Duration(c.PingIntervalSec * float64(time.Second))

	searchStripped := false // Whether a stripped-block retry already happened after the upstream rejected a replayed search id (fail-soft gets only one chance)
	for attempt := 0; attempt <= c.MaxRetries; attempt++ {
		// Entering the [尝试N] stage: the web 「状态」 column shows the attempt number (awaiting the upstream's first byte).
		f.attempt.Store(int32(attempt + 1))
		f.setStage(stageAttempt)
		// 2. Build the request bound upstream.
		// A passthrough stream's upstream is the base given in url_response_api (…/coding, …/v1, or a full …/v1/responses all work);
		// the client path must not be copied verbatim (it's /v1/responses — appending it to the base would double or misjoin) — only the difference is appended per the base's shape.
		upPath := r.URL.Path
		if f.responsesRaw() {
			upPath = responsesAPIPath(upstream)
		}
		// Dual-link recording: the proxy→upstream side takes the body actually sent on this attempt (overwriting the entry initial value;
		// a fallback rewrite naturally refreshes it); a native stream's downstream side is stored only when rewriting makes it differ from the upstream-bound body — identical text is never stored twice.
		f.setReqBody(body)
		if downBody != nil && !bytes.Equal(downBody, body) {
			f.setReqDown(downBody)
			downBody = nil // The client's original doesn't change; storing it once suffices
		}
		upReq, err := http.NewRequestWithContext(ctx, r.Method, upstream+upPath, bytes.NewReader(body))
		if err != nil {
			log.Printf("[error] failed to build upstream request: %v", err)
			http.Error(w, "build request failed", http.StatusBadGateway)
			return
		}
		upReq.URL.RawQuery = r.URL.RawQuery
		copyHeaders(upReq.Header, r.Header)
		upReq.Header.Set("Accept-Encoding", "identity") // Disable compression so the response body is easy to inspect
		upReq.Host = ""                                 // Let Go set Host automatically from Upstream
		// On a route hit, overwrite the auth header with the target API key (deleting the original Authorization/x-api-key, so the client's token isn't passed through to the target upstream).
		if authToken != "" {
			upReq.Header.Del("Authorization")
			upReq.Header.Del("x-api-key")
			upReq.Header.Set("Authorization", "Bearer "+authToken)
		}
		// On a fast-route hit, delete the Anthropic-Beta header (fast-mode-2026-02-01 included); the upstream doesn't support it.
		if fastRouteHit {
			upReq.Header.Del("Anthropic-Beta")
		}

		// Sent upstream, entering the "awaiting first byte" stage: the status light turns yellow (waiting++).
		stats.mu.Lock()
		stats.waiting++
		stats.mu.Unlock()
		tSend := time.Now()                    // Request send time, for first-byte latency
		f.attemptStart.Store(tSend.UnixNano()) // [尝试N:Xs] times from this send moment (re-stamped here after backoff zeroed it)
		resp, err := client.Do(upReq)
		// Leave the waiting stage as soon as a response (or timeout/error) arrives.
		stats.mu.Lock()
		stats.waiting--
		stats.mu.Unlock()

		// Case 0: network-layer error.
		if err != nil {
			log.Printf("[attempt %d] #%d request error: %v", attempt+1, f.id, err)
			// A tray-icon double-click abort (ctx cancel) doesn't retry; end directly.
			if ctx.Err() != nil {
				if headersSent {
					return // Keepalive headers already sent, client disconnected; end directly
				}
				http.Error(w, "client canceled", http.StatusServiceUnavailable)
				return
			}
			if attempt < c.MaxRetries && time.Now().Before(deadline) {
				wait := computeBackoff(nil, attempt)
				if !time.Now().Add(wait).After(deadline) {
					log.Printf("[retry] waiting %v before retry (budget left %v)", wait, time.Until(deadline).Round(time.Millisecond))
					stats.statusRetries.Add(1) // Live status row count: retries (network error/timeout)
					stats.addModelRetry(f.realModel())
					f.attempt.Store(int32(attempt + 2)) // Advance the attempt number to the next try during backoff
					f.attemptStart.Store(0)             // In backoff: no attempt in flight; the yellow light shows [退避中]
					if !headersSent && !convertToStream {
						// A convertToStream client expects a non-streaming response; SSE keepalives would pollute it, so wait silently.
						flusher = startSSEKeepalive(w, f, "network error")
						headersSent = true
					}
					if !sleepWithPing(ctx, w, flusher, wait, pingInterval, f) {
						return // Client disconnected; stop retrying
					}
					continue
				}
			}
			// Retries exhausted on timeouts/network errors: pass through a 503 so Claude Code retries on its own (the SDK has built-in 5xx retries).
			log.Printf("[passthrough] timed out or retries exhausted; passing 503 through to the client: %v", err)
			if headersSent {
				writeSSEError(w, flusher, "upstream timeout/error: "+err.Error(), f)
				return
			}
			http.Error(w, "upstream timeout/error: "+err.Error(), http.StatusServiceUnavailable)
			return
		}

		// Print the status code unconditionally, so diagnosis can see exactly what came back.
		log.Printf("[attempt %d] upstream response status: %d", attempt+1, resp.StatusCode)

		// Case A: the HTTP status code itself demands a retry (a real 429/5xx).
		if shouldRetry(resp.StatusCode) {
			if attempt < c.MaxRetries && time.Now().Before(deadline) {
				wait := computeBackoff(resp, attempt)
				if !time.Now().Add(wait).After(deadline) {
					log.Printf("[retry] status %d; waiting %v before retry (budget left %v)", resp.StatusCode, wait, time.Until(deadline).Round(time.Millisecond))
					stats.statusRetries.Add(1) // Live status row count: retries (status code)
					stats.addModelRetry(f.realModel())
					f.attempt.Store(int32(attempt + 2)) // Advance the attempt number to the next try during backoff
					f.attemptStart.Store(0)             // In backoff: no attempt in flight; the yellow light shows [退避中]
					resp.Body.Close()
					if !headersSent && !convertToStream {
						// A convertToStream client expects a non-streaming response; SSE keepalives would pollute it, so wait silently.
						flusher = startSSEKeepalive(w, f, fmt.Sprintf("status %d", resp.StatusCode))
						headersSent = true
					}
					if !sleepWithPing(ctx, w, flusher, wait, pingInterval, f) {
						return // Client disconnected; stop retrying
					}
					continue
				}
			}
			log.Printf("[passthrough] attempts or budget exhausted; passing through status %d", resp.StatusCode)
			if headersSent {
				resp.Body.Close()
				writeSSEError(w, flusher, fmt.Sprintf("upstream status %d", resp.StatusCode), f)
				return
			}
			forward(w, resp, nil, bufio.NewReader(resp.Body), f, false)
			return
		}

		// Case B: status 200, but an error may hide in the response body (SSE stream).
		br := bufio.NewReader(resp.Body)
		head := peekHead(br, 8192)
		tFirstByte := time.Now() // The moment the first body data byte arrives ("first token out")

		if headHasError(head) {
			// The search-envelope restore was rejected by the upstream (Kimi's tool_call_id registry no longer recognizes this old id):
			// strip the replayed search blocks from the request and retry once immediately — a failed restore automatically degrades to "no restore".
			// No backoff budget is burned (attempt-- cancels the loop's increment, so it doesn't count as a retry and works even with MaxRetries=0),
			// and this 400 must not be passed to the client.
			if !searchStripped && bytes.Contains(head, []byte("tool_call_id")) {
				if nb, n, ok := stripSearchBlocksInBody(body); ok {
					f.searchStripped.Add(int32(n))
					// Learn the conversation watermark: take the oldest seal-in timestamp among the envelopes restored this round — the registry
					// evicts by age, so if the oldest was rejected, everything older is dead too; strip by default from now on, no more 400s.
					water := ""
					if f.searchReplay != nil && len(f.searchReplay.restored) > 0 {
						oldest := f.searchReplay.restored[0]
						for _, ts := range f.searchReplay.restored[1:] {
							if ts.Before(oldest) {
								oldest = ts
							}
						}
						learnSearchCutoff(f.searchReplay.convID, oldest)
						water = fmt.Sprintf(", conversation watermark learned as %s", oldest.Format("01-02 15:04:05"))
					}
					log.Printf("[fallback] #%d upstream rejected the replayed search id (HTTP %d); stripped %d search blocks and retried%s", f.id, resp.StatusCode, n, water)
					stats.statusRetries.Add(1)
					stats.addModelRetry(f.realModel())
					body = nb
					searchStripped = true
					attempt--
					resp.Body.Close()
					continue
				}
			}
			// Thinking over a no-thinking-block history was rejected by the upstream (the n2l modes send thinking on purpose here — the
			// stealth upgrade's low and the try-on mode's requested level; the rejection is effort-agnostic, rejecting "unsigned history
			// with thinking on"): switch thinking back off and retry once immediately — the downstream wanted thinking off anyway (or at
			// most accepts it off), so it's semantically lossless; no backoff budget burned (attempt--, same as the search-strip fallback).
			// After the fallback the upstream receives thinking-off, so the response carries no thinking blocks and response-side stripping has nothing to do;
			// the badge falls back to truthful display as well.
			if n2lMode != n2lNone && bytes.Contains(head, []byte("thinking")) {
				if nb, ok := disableThinkingInBody(body); ok {
					if n2lMode == n2lTryOn {
						log.Printf("[fallback] #%d upstream rejected thinking over a no-thinking-block history (HTTP %d); reverted to thinking-off and retried", f.id, resp.StatusCode)
					} else {
						log.Printf("[fallback] #%d upstream rejected the low-thinking upgrade (HTTP %d); reverted to thinking-off and retried", f.id, resp.StatusCode)
					}
					stats.statusRetries.Add(1)
					stats.addModelRetry(f.realModel())
					body = nb
					// A native-port upgrade may also have set reasoning_effort:"low"; the belt-and-braces off form is "none"
					// (setTopLevelJSONValue appends the key when absent — harmless, same shape as the classifier rewrite).
					if nb2, ok2 := setTopLevelJSONValue(body, "reasoning_effort", []byte(`"none"`)); ok2 {
						body = nb2
					}
					n2lMode = n2lNone
					f.think = extractThinkMode(body)
					f.stripThinking.Store(false) // The retried response is genuinely thinking-off; there is nothing left to strip
					attempt--
					resp.Body.Close()
					continue
				}
			}
			log.Printf("[error] status 200 but response body contains an error: %s", firstLine(head))
			if attempt < c.MaxRetries && time.Now().Before(deadline) {
				wait := computeBackoff(resp, attempt)
				if !time.Now().Add(wait).After(deadline) {
					log.Printf("[retry] waiting %v before retry (budget left %v)", wait, time.Until(deadline).Round(time.Millisecond))
					stats.statusRetries.Add(1) // Live status row count: retries (in-body error on 200)
					stats.addModelRetry(f.realModel())
					f.attempt.Store(int32(attempt + 2)) // Advance the attempt number to the next try during backoff
					f.attemptStart.Store(0)             // In backoff: no attempt in flight; the yellow light shows [退避中]
					resp.Body.Close()
					if !headersSent && !convertToStream {
						// A convertToStream client expects a non-streaming response; SSE keepalives would pollute it, so wait silently.
						flusher = startSSEKeepalive(w, f, "in-body error")
						headersSent = true
					}
					if !sleepWithPing(ctx, w, flusher, wait, pingInterval, f) {
						return // Client disconnected; stop retrying
					}
					continue
				}
			}
			log.Printf("[passthrough] attempts or budget exhausted; passing through the in-body error")
			if headersSent {
				resp.Body.Close()
				writeSSEError(w, flusher, "upstream stream error after retries", f)
				return
			}
			forward(w, resp, head, br, f, false)
			return
		}

		// Case C: normal response, passed through. First-byte latency / streaming duration / output_tokens enter the sliding window.
		log.Printf("[response] passed through normally (%d attempts)", attempt+1)
		// With fast_route configured, inject fake fast rate-limit headers into every normal response,
		// so Claude Code's /fast pre-check believes fast mode is available.
		if c.FastRoute != nil {
			now := time.Now().Format(time.RFC3339)
			resp.Header.Set("anthropic-fast-output-tokens-remaining", "999999")
			resp.Header.Set("anthropic-fast-input-tokens-remaining", "999999")
			resp.Header.Set("anthropic-fast-output-tokens-reset", now)
			resp.Header.Set("anthropic-fast-input-tokens-reset", now)
		}
		// A convertToStream request: the upstream streams SSE back; collect the full stream, rebuild non-streaming JSON, and return it in one shot
		// (the web in-flight view stays live; stats match ordinary streaming requests). If the upstream didn't stream (JSON), fall through to passthrough below.
		if convertToStream && strings.Contains(resp.Header.Get("Content-Type"), "text/event-stream") {
			ok, out := collectStreamToJSON(w, resp, head, br, f)
			if ok {
				// Stats match ordinary streaming passthrough: first-byte latency / streaming duration / tok/s.
				tEnd := time.Now()
				stats.pushFirstByte(int64(tFirstByte.Sub(tSend).Milliseconds()))
				stats.pushThroughput(int64(tEnd.Sub(tFirstByte).Milliseconds()), out)
				firstByteMs := tFirstByte.Sub(tSend).Milliseconds()
				streamMs := tEnd.Sub(tFirstByte).Milliseconds()
				var tps float64
				if streamMs > 0 {
					tps = float64(out) / (float64(streamMs) / 1000.0)
				}
				f.firstByteMs = firstByteMs
				f.tps = tps
				log.Printf("[flight] #%d TTFT %.2fs stream %.2fs %d tok %.1f tok/s",
					f.id, float64(firstByteMs)/1000, float64(streamMs)/1000, out, tps)
				return
			}
			// Stream broke midway: the client has received nothing yet; after backoff, retry the whole thing (resend the streaming request, drain again).
			log.Printf("[streamify] #%d upstream stream incomplete; retrying", f.id)
			stats.statusRetries.Add(1)
			stats.addModelRetry(f.realModel())
			if attempt < c.MaxRetries && time.Now().Before(deadline) {
				wait := computeBackoff(resp, attempt)
				if !time.Now().Add(wait).After(deadline) {
					f.attempt.Store(int32(attempt + 2))
					f.attemptStart.Store(0) // In backoff: no attempt in flight; the yellow light shows [退避中]
					if !sleepWithPing(ctx, w, flusher, wait, pingInterval, f) {
						return // Client disconnected; stop retrying
					}
					continue
				}
			}
			// Retries exhausted: the client expects non-streaming JSON; pass through a 502 and let it handle it.
			log.Printf("[passthrough] attempts or budget exhausted; streamified flow incomplete, passing through 502")
			http.Error(w, "upstream stream interrupted after retries", http.StatusBadGateway)
			return
		}
		stats.pushFirstByte(int64(tFirstByte.Sub(tSend).Milliseconds()))
		out := forward(w, resp, head, br, f, headersSent)
		tEnd := time.Now()
		stats.pushThroughput(int64(tEnd.Sub(tFirstByte).Milliseconds()), out)
		// Per-stream stats: this stream's first-byte latency + streaming tok/s (distinct from the status row's sliding-window averages).
		firstByteMs := tFirstByte.Sub(tSend).Milliseconds()
		streamMs := tEnd.Sub(tFirstByte).Milliseconds()
		var tps float64
		if streamMs > 0 {
			tps = float64(out) / (float64(streamMs) / 1000.0)
		}
		f.firstByteMs = firstByteMs
		f.tps = tps
		log.Printf("[flight] #%d TTFT %.2fs stream %.2fs %d tok %.1f tok/s",
			f.id, float64(firstByteMs)/1000, float64(streamMs)/1000, out, tps)
		return
	}
}

func main() {
	configPath := flag.String("config", "", "config file path (empty = look up ./config.json then the user config dir)")
	flag.Parse()
	configFilePath = resolveConfigPath(*configPath)

	c, warns, err := loadConfig(configFilePath)
	if err != nil {
		log.Fatalf("failed to read %s: %v", configFilePath, err)
	}
	cfg.Store(c)
	setConfigWarnings(warns)
	stats.resetSampleCap(c.RecentSampleWindow) // Initialize the "last X" latency/throughput sliding-window capacity
	resolveProgramUILang()                     // First launch: program-settings.txt wins, legacy config ui_lang is migrated, otherwise follow the OS
	// The flight registry and log buffer are always initialized (handlers always register flights; the map must not be nil).
	flights.m = make(map[uint64]*flight)
	logBuf.lines = make([]string, 0, maxLogBuf)

	// Set a "first-byte timeout" for upstream requests: beyond it the request is considered stuck and resent internally (case-0 retry).
	// ResponseHeaderTimeout, not client.Timeout, so only the first-byte wait is limited and streaming Bodies are never cut.
	// Set once at startup; reload doesn't rebuild the Transport (avoiding concurrent field mutation against in-flight requests).
	if c.UpstreamHeaderTimeoutSec > 0 {
		tr := http.DefaultTransport.(*http.Transport).Clone()
		tr.ResponseHeaderTimeout = time.Duration(c.UpstreamHeaderTimeoutSec * float64(time.Second))
		client.Transport = tr
	}

	// Log output: always into logRing (polled by the web 「查看日志」 page) + stderr.
	// The program has no terminal UI: Windows builds with the GUI subsystem have no console, macOS .apps have no terminal — writing stderr is harmless;
	// when log_file is explicitly configured, logs are additionally persisted for later troubleshooting.
	var logOut io.Writer = io.MultiWriter(logBufAppender{}, os.Stderr)
	if c.LogFile != "" {
		if f, err := os.OpenFile(c.LogFile, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644); err == nil {
			logOut = io.MultiWriter(logOut, f)
			log.Printf("[log] also writing to file %s", c.LogFile)
		} else {
			log.Printf("[log] failed to open log file %s: %v", c.LogFile, err)
		}
	}
	log.SetOutput(logOut)

	// Startup removed-key warnings are logged only now: earlier log lines bypass logRing (the standard logger is not
	// redirected yet), and a GUI-subsystem exe has no console for stderr, so they would be invisible everywhere.
	for _, k := range warns {
		log.Printf("[config] WARNING: %s (the key is inert; the proxy runs without its old behavior)", removedKeyWarningEN(k))
	}

	log.Printf("proxy started v%s: listening http://%s -> forwarding to %s (max retries %d, classifier thinking-off=%v)",
		Version, c.Listen, c.Upstream, c.MaxRetries, classifierThinkingOff(c))

	// The HTTP server runs in a goroutine: the tray event loop (systray.Run) must occupy the main thread (macOS requires UI on the main thread),
	// so the main thread's blocking spot belongs to the tray and HTTP runs in the background.
	go runServer(c)
	// Optional: the OpenAI Responses API listener (translates into Anthropic and feeds the main pipeline), independent of the main port.
	// On config reload/switch, reconcileResponsesServer starts/stops it per the new config — no process restart needed.
	reconcileResponsesServer(c.ResponsesListen)
	// Ctrl+C -> graceful tray exit: systray.Quit triggers onExit to stop status polling; once Run returns, main exits.
	go func() {
		sigCh := make(chan os.Signal, 1)
		signal.Notify(sigCh, os.Interrupt)
		<-sigCh
		systray.Quit()
	}()
	// The tray event loop, blocking the main thread until systray.Quit (triggered by the menu 「退出代理」 or Ctrl+C).
	setupTray()
}

// runServer registers the handlers and listens; a listen failure (e.g. port in use) log.Fatals the whole process.
func runServer(c *Config) {
	// The web console hangs on the same port under /__* (localhost only); Claude Code uses /v1/... — no conflict.
	// Must be registered before "/": Go's DefaultServeMux prefers exact matches over the / wildcard.
	http.HandleFunc(logViewerPath, logViewerHandler)
	http.HandleFunc(logDataPath, logDataHandler)
	http.HandleFunc(configPath, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			configPostHandler(w, r)
		} else {
			configGetHandler(w, r)
		}
	})
	http.HandleFunc(reloadPath, reloadHandler)
	http.HandleFunc(configsPath, configsHandler)
	http.HandleFunc(switchPath, switchHandler)
	http.HandleFunc(newConfigPath, newConfigHandler)
	http.HandleFunc(codexSetupPS1Path, codexScriptHandler(codexSetupPS1, codexPS1Anchors, false))
	http.HandleFunc(codexSetupSHPath, codexScriptHandler(codexSetupSH, codexSHAnchors, true))
	http.HandleFunc(renameConfigPath, renameConfigHandler)
	http.HandleFunc(delConfigPath, delConfigHandler)
	http.HandleFunc(resetStatsPath, resetStatsHandler)
	http.HandleFunc(clearLogsPath, clearLogsHandler)
	http.HandleFunc(flightPath, flightHandler)
	http.HandleFunc(flightReqPath, flightReqHandler)
	http.HandleFunc(recentFlightsPath, recentFlightsHandler)
	http.HandleFunc(finishedCapPath, finishedCapHandler)
	http.HandleFunc(fullStorePath, fullStoreHandler)
	http.HandleFunc(uiLangPath, uiLangHandler)
	http.HandleFunc("/", handler)
	err := http.ListenAndServe(c.Listen, nil)
	log.Fatal(err)
}
