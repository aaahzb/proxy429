// Package proxy is the proxy core: config loading, the forwarding pipeline, the search guard,
// the system tray, and the wiring that starts the HTTP server and web console.
package proxy

import (
	"encoding/json"
	"flag"
	"io"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"time"

	"fyne.io/systray"

	"proxy429/internal/config"
	"proxy429/internal/jsonutil"
	"proxy429/internal/logview"
	"proxy429/internal/platform"
	"proxy429/internal/recorder"
	"proxy429/internal/translate"
)

// Version is the build stamp, injected by build.sh via -ldflags "-X proxy429/internal/proxy.Version=...":
// "release vX.Y.Z.N" when HEAD is exactly a version tag (CD tag builds), otherwise "beta <git-short>-<HHMM>";
// bare go builds show "dev".
var Version = "dev"

// resolveProgramUILang determines the web console UI language at startup: program-settings.txt's ui-lang
// wins; otherwise a one-time migration moves any legacy ui_lang left in config files into program-settings.txt
// and deletes it from the config (jsonutil.SetTopLevelJSONValue text-level surgery, every other byte untouched); otherwise follow the OS.
// After migration, no code path writes to existing config files programmatically (the config editor saves the user's verbatim text).
func resolveProgramUILang() {
	if l := config.ReadProgramSetting("ui-lang"); l == "zh" || l == "en" {
		platform.ApplyUILang(l)
		return
	}
	if data, err := os.ReadFile(config.CurrentConfigPath()); err == nil {
		var probe struct {
			UILang string `json:"ui_lang"` // Legacy field: only used for migration; the Config struct no longer carries it
		}
		if json.Unmarshal(data, &probe) == nil && (probe.UILang == "zh" || probe.UILang == "en") {
			if err := config.WriteProgramSetting("ui-lang", probe.UILang); err != nil {
				log.Printf("[lang] failed to write %s: %v", config.ProgramSettingsFile, err)
			}
			if out, ok := jsonutil.SetTopLevelJSONValue(data, "ui_lang", nil); ok {
				if err := os.WriteFile(config.CurrentConfigPath(), out, 0644); err != nil {
					log.Printf("[lang] failed to remove migrated ui_lang from %s: %v", filepath.Base(config.CurrentConfigPath()), err)
				}
			}
			log.Printf("[lang] migrated ui_lang=%q from %s to %s", probe.UILang, filepath.Base(config.CurrentConfigPath()), config.ProgramSettingsFile)
			platform.ApplyUILang(probe.UILang)
			return
		}
	}
	platform.ApplyUILang("")
}

// clearStats clears cumulative stats, latency/throughput samples, and the "recently finished streams" list (including each one's archived pass-through content),
// and rebuilds sample capacity per the given config's RecentSampleWindow. In-flight streams and stream numbering are kept (avoids id collisions).
// Called only by the web "clear stats" button (switches/reloads don't clear stats; stats persist across configs).
func clearStats(c *config.Config) {
	recorder.Stats.Mu.Lock()
	recorder.Stats.CacheRead = 0
	recorder.Stats.CacheCreation = 0
	recorder.Stats.InputTokens = 0
	recorder.Stats.OutputTokens = 0
	recorder.Stats.ModelStats = nil
	recorder.Stats.Mu.Unlock()
	recorder.Stats.BytesUp.Store(0)
	recorder.Stats.BytesDown.Store(0)
	recorder.Stats.StatusRetries.Store(0)
	recorder.Stats.ClassifierRewrites.Store(0)
	recorder.Stats.ClassifierHits.Store(0)
	recorder.Stats.ClassifierHitsCC.Store(0)
	recorder.Stats.ClassifierHitsCodex.Store(0)
	recorder.Stats.ClassifierRewritesCC.Store(0)
	recorder.Stats.ClassifierRewritesCodex.Store(0)
	recorder.Stats.ResetSampleCap(c.RecentSampleWindow)
	recorder.FinishedMu.Lock()
	recorder.Finished = nil
	recorder.CacheObsMap = map[string]*recorder.CacheObsEntry{} // Clear the empirical cache-lifetime observations too
	recorder.FinishedMu.Unlock()
	// 清空统计连带清掉活跃度热图，并重写持久化文件为空。
	recorder.HeatMu.Lock()
	recorder.Heat = [recorder.HeatSlots]recorder.HeatSlot{}
	recorder.HeatMu.Unlock()
	_ = recorder.HeatSave()
}

// reloadConfig re-reads the current config file and atomically swaps the active config, without clearing stats (only the "clear stats" button clears).
// On failure the active config keeps its old value. Removed keys in the reloaded file succeed but refresh the warning set (red tray icon).
func reloadConfig() error {
	c, warns, err := config.LoadConfig(config.CurrentConfigPath())
	if err != nil {
		log.Printf("[reload] failed: %v", err)
		return err
	}
	config.Store(c)
	config.SetConfigWarnings(warns)
	translate.ReconcileResponsesServer(c.ResponsesListen, handler) // Responses port starts/stops dynamically with config reload
	log.Printf("[reload] config reloaded: http://%s -> %s (max retries %d, classifier thinking: cc=%q codex=%q, removed-key warnings=%d)",
		c.Listen, c.Upstream, c.MaxRetries, config.ClassifierThinkingModeFor(c, config.ClassifierCC), config.ClassifierThinkingModeFor(c, config.ClassifierCodex), len(warns))
	return nil
}

// switchConfig switches to the given config file with immediate effect: after loading succeeds, the config path and active config are updated.
// Stats are NOT cleared (they persist across configs; the "clear stats" button clears independently). On failure the config path and active config stay unchanged.
func switchConfig(newPath string) error {
	c, warns, err := config.LoadConfig(newPath)
	if err != nil {
		log.Printf("[switch] failed to load %s: %v", newPath, err)
		return err
	}
	config.SetCurrentPath(newPath)
	config.WriteActiveConfigState(newPath)
	config.Store(c)
	config.SetConfigWarnings(warns)
	translate.ReconcileResponsesServer(c.ResponsesListen, handler) // Responses port starts/stops dynamically with the config
	// Notify the tray to rebuild the "switch config" submenu and refresh checkmarks (web-initiated switches don't go through tray clicks)
	notifyTrayCfgChanged()
	log.Printf("[switch] switched to %s: http://%s -> %s (max retries %d)",
		filepath.Base(newPath), c.Listen, c.Upstream, c.MaxRetries)
	return nil
}

// configureConsole wires the web console's callbacks into the proxy core; must run before the server serves.
func configureConsole() {
	logview.Configure(logview.Deps{
		Version:        Version,
		ReloadConfig:   reloadConfig,
		SwitchConfig:   switchConfig,
		ClearStats:     func() { clearStats(config.Current()) },
		TrayCfgChanged: notifyTrayCfgChanged,
	})
}

// noTrayFlag selects headless mode: no system tray (CI runners and smoke rigs have no tray to click).
// The HTTP server and web console still run; the process stops on Ctrl+C / SIGTERM like a bare server.
var noTrayFlag = flag.Bool("no-tray", false, "run without the system tray (headless/CI); stop with Ctrl+C")

// Main runs the proxy: parse flags, load config, start the HTTP server and console, then block on the
// system tray (or forever in -no-tray headless mode). It occupies the process main thread: macOS requires
// the tray event loop there.
func Main() {
	configPath := flag.String("config", "", "config file path (empty = look up ./config.json then the user config dir)")
	flag.Parse()
	config.SetCurrentPath(config.ResolveConfigPath(*configPath))

	c, warns, err := config.LoadConfig(config.CurrentConfigPath())
	if err != nil {
		log.Fatalf("failed to read %s: %v", config.CurrentConfigPath(), err)
	}
	config.Store(c)
	config.SetConfigWarnings(warns)
	recorder.Stats.ResetSampleCap(c.RecentSampleWindow) // Initialize the "last X" latency/throughput sliding-window capacity
	resolveProgramUILang()                              // First launch: program-settings.txt wins, legacy config ui_lang is migrated, otherwise follow the OS
	// The flight registry and log buffer are always initialized (handlers always register flights; the map must not be nil).
	recorder.Flights.M = make(map[uint64]*recorder.Flight)
	recorder.LogBuf.Lines = make([]string, 0, recorder.MaxLogBuf)

	// Set a "first-byte timeout" for upstream requests: beyond it the request is considered stuck and resent internally (case-0 retry).
	// ResponseHeaderTimeout, not client.Timeout, so only the first-byte wait is limited and streaming Bodies are never cut.
	// Set once at startup; reload doesn't rebuild the Transport (avoiding concurrent field mutation against in-flight requests).
	// The trafficRT wrapper counts upstream-bound bytes for the status cards; every upstream call shares this client.
	base := http.RoundTripper(http.DefaultTransport)
	if c.UpstreamHeaderTimeoutSec > 0 {
		tr := http.DefaultTransport.(*http.Transport).Clone()
		tr.ResponseHeaderTimeout = time.Duration(c.UpstreamHeaderTimeoutSec * float64(time.Second))
		base = tr
	}
	recorder.Client.Transport = recorder.TrafficRT{Base: base}

	// Log output: always into logRing (polled by the web 「查看日志」 page) + stderr.
	// The program has no terminal UI: Windows builds with the GUI subsystem have no console, macOS .apps have no terminal — writing stderr is harmless;
	// when log_file is explicitly configured, logs are additionally persisted for later troubleshooting.
	var logOut io.Writer = io.MultiWriter(recorder.LogBufAppender{}, os.Stderr)
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
		log.Printf("[config] WARNING: %s (the key is inert; the proxy runs without its old behavior)", config.RemovedKeyWarningEN(k))
	}

	log.Printf("proxy started %s: listening http://%s -> forwarding to %s (max retries %d, classifier thinking: cc=%q codex=%q)",
		Version, c.Listen, c.Upstream, c.MaxRetries, config.ClassifierThinkingModeFor(c, config.ClassifierCC), config.ClassifierThinkingModeFor(c, config.ClassifierCodex))

	// Wire the web console to the proxy core before the server starts serving.
	configureConsole()
	// The HTTP server runs in a goroutine: the tray event loop (systray.Run) must occupy the main thread (macOS requires UI on the main thread),
	// so the main thread's blocking spot belongs to the tray and HTTP runs in the background.
	go runServer(c)
	// Optional: the OpenAI Responses API listener (translates into Anthropic and feeds the main pipeline), independent of the main port.
	// On config reload/switch, reconcileResponsesServer starts/stops it per the new config — no process restart needed.
	translate.ReconcileResponsesServer(c.ResponsesListen, handler)
	// Loopback-hijack watchdog (read-only): system proxy on + NO_PROXY missing -> red tray light + console banner,
	// so a silently-vanished codex NO_PROXY fix is noticed before Codex 503s, not after.
	go platform.WatchProxyHijack(func() string {
		if c := config.Current(); c != nil {
			return c.ResponsesListen
		}
		return ""
	})
	if *noTrayFlag {
		// Headless: no tray event loop to occupy the main thread; block here and let
		// Ctrl+C / SIGTERM end the process with their default behavior.
		log.Printf("no-tray mode: system tray disabled; Ctrl+C to stop")
		select {}
	}
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
func runServer(c *config.Config) {
	// The web console hangs on the same port under /__* (localhost only); Claude Code uses /v1/... — no conflict.
	logview.RegisterHandlers()
	http.HandleFunc("/", handler)
	// 活跃度热图：装载 48h 窗口内的持久化数据（缺失/损坏文件即从空开始），并启动每分钟脏落盘。
	recorder.HeatLoad()
	go recorder.HeatSaverLoop()
	err := http.ListenAndServe(c.Listen, nil)
	log.Fatal(err)
}
