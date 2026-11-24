// Package logview serves the localhost web console: the console page and its
// status/log/config endpoints, config-file management, the Codex one-click setup
// scripts, and the UI-language switch. The proxy-core operations it triggers are
// injected via Deps, so the package stays independent of the proxy pipeline.
package logview

import (
	"encoding/json"
	"log"
	"net/http"
	"strings"

	"proxy429/internal/config"
	"proxy429/internal/platform"
)

// Web console mount path (same port as the proxy, localhost only).
// The /__ prefix is chosen because the Anthropic API uses /v1/... — no conflict; Go's DefaultServeMux prefers exact matches over the / wildcard.
const (
	ViewerPath        = "/__logs"
	logDataPath       = "/__logs/data"
	configPath        = "/__config"         // GET returns the config content, POST saves and reloads
	reloadPath        = "/__reload"         // POST only reloads (the file is not modified)
	lazyPreviewPath   = "/__lazyapply"      // POST returns the JSON after lazyApply materialization (preview only)
	configsPath       = "/__configs"        // GET lists all .json files in the current config directory (for switching)
	switchPath        = "/__switch"         // POST switches to the given config file with immediate effect
	newConfigPath     = "/__newconfig"      // POST creates a new config file (blank template) and switches to it
	codexSetupPS1Path = "/__codexsetup.ps1" // GET the baked codex-setup.ps1 (Windows, pulled via irm|iex)
	codexSetupSHPath  = "/__codexsetup.sh"  // GET the baked codex-setup.sh (macOS/Linux, pulled via curl|bash)
	renameConfigPath  = "/__renameconfig"   // POST renames a config file
	delConfigPath     = "/__delconfig"      // POST deletes a config file (the one currently in use can't be deleted)
	resetStatsPath    = "/__resetstats"     // POST clears cumulative stats (switching configs no longer clears them automatically)
	clearLogsPath     = "/__clearlogs"      // POST clears the in-memory log buffer
	flightPath        = "/__flight"         // GET an in-flight stream's pass-through content (?id=N; finished streams query this too)
	flightReqPath     = "/__flightreq"      // GET a stream's downstream request body verbatim (?id=N; in-flight and finished both queryable)
	RecentFlightsPath = "/__recentflights"  // GET the recently finished streams list (summary)
	finishedCapPath   = "/__finishedcap"    // POST sets how many finished streams to keep, N
	fullStorePath     = "/__fullstore"      // POST toggles 储存完整结构体 (record full request bodies/output for download)
	uiLangPath        = "/__uilang"         // GET the current UI language / POST switches the language (writes program-settings.txt, effective hot)
)

// Deps are the proxy-core operations the console invokes.
type Deps struct {
	// Version is the build stamp shown in the page header and the status JSON ("dev" for bare go builds).
	Version string
	// ReloadConfig re-reads the active config file and applies it (stats are kept).
	ReloadConfig func() error
	// SwitchConfig switches to the given config file path with immediate effect (stats are kept).
	SwitchConfig func(string) error
	// ClearStats clears cumulative stats and the recently finished streams list.
	ClearStats func()
	// TrayCfgChanged tells the tray to rebuild its config-file submenu.
	TrayCfgChanged func()
}

var (
	noopReload = func() error { return nil }
	noopSwitch = func(string) error { return nil }
	noopVoid   = func() {}
)

var deps = Deps{
	ReloadConfig:   noopReload,
	SwitchConfig:   noopSwitch,
	ClearStats:     noopVoid,
	TrayCfgChanged: noopVoid,
}

// Configure wires the console to the proxy core; call once at startup, before serving.
// Nil callbacks keep their no-op defaults, so isolated tests never panic.
func Configure(d Deps) {
	if d.ReloadConfig == nil {
		d.ReloadConfig = noopReload
	}
	if d.SwitchConfig == nil {
		d.SwitchConfig = noopSwitch
	}
	if d.ClearStats == nil {
		d.ClearStats = noopVoid
	}
	if d.TrayCfgChanged == nil {
		d.TrayCfgChanged = noopVoid
	}
	deps = d
}

// RegisterHandlers mounts every console endpoint on the default mux (all endpoints are localhost-only).
func RegisterHandlers() {
	http.HandleFunc(ViewerPath, logViewerHandler)
	http.HandleFunc(logDataPath, logDataHandler)
	http.HandleFunc(configPath, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			ConfigPostHandler(w, r)
		} else {
			configGetHandler(w, r)
		}
	})
	http.HandleFunc(reloadPath, reloadHandler)
	http.HandleFunc(lazyPreviewPath, lazyApplyPreviewHandler)
	http.HandleFunc(configsPath, configsHandler)
	http.HandleFunc(switchPath, switchHandler)
	http.HandleFunc(newConfigPath, newConfigHandler)
	http.HandleFunc(codexSetupPS1Path, codexScriptHandler(codexSetupPS1, codexPS1Anchors, false))
	http.HandleFunc(codexSetupSHPath, codexScriptHandler(codexSetupSH, codexSHAnchors, true))
	http.HandleFunc(renameConfigPath, RenameConfigHandler)
	http.HandleFunc(delConfigPath, DelConfigHandler)
	http.HandleFunc(resetStatsPath, resetStatsHandler)
	http.HandleFunc(clearLogsPath, clearLogsHandler)
	http.HandleFunc(flightPath, FlightHandler)
	http.HandleFunc(flightReqPath, FlightReqHandler)
	http.HandleFunc(RecentFlightsPath, RecentFlightsHandler)
	http.HandleFunc(finishedCapPath, finishedCapHandler)
	http.HandleFunc(fullStorePath, FullStoreHandler)
	http.HandleFunc(uiLangPath, uiLangHandler)
}

// logViewerHandler returns the console HTML page (status/logs/config tabs).
func logViewerHandler(w http.ResponseWriter, r *http.Request) {
	if !platform.IsLocalRequest(r) {
		http.Error(w, "forbidden (local only)", http.StatusForbidden)
		return
	}
	page := logViewerHTML
	if platform.CurrentUILang() == "en" {
		page = renderLogViewerEN(page)
	} else {
		page = strings.Replace(page, "__DOC_BODY__", logViewerDocZH, 1)
	}
	// The version badge sits next to the language switch. The stamp is build-controlled ASCII
	// ("beta <hash>-<HHMM>" or "release vX.Y.Z.N"), safe to inline unescaped in either language.
	page = strings.Replace(page, "__VERSION__", deps.Version, 1)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write([]byte(page))
}

// uiLangHandler queries/switches the web console UI language.
// GET returns {"lang": current effective language, "source": "program"|"system"};
// POST {"lang":"zh"|"en"|""} writes the choice into the program-settings.txt program settings ("" = follow the system,
// the key is deleted), effective hot — the next page refresh shows the new language. Language is a program-level preference, separate
// from routing config: it doesn't write config*.json, nor trigger a config reload (program settings and upstream settings are maintained separately).
func uiLangHandler(w http.ResponseWriter, r *http.Request) {
	if !platform.IsLocalRequest(r) {
		http.Error(w, "forbidden (local only)", http.StatusForbidden)
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	if r.Method == http.MethodGet {
		source := "system"
		if l := config.ReadProgramSetting("ui-lang"); l == "zh" || l == "en" {
			source = "program"
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "lang": platform.CurrentUILang(), "source": source})
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var b struct {
		Lang string `json:"lang"`
	}
	if err := json.NewDecoder(r.Body).Decode(&b); err != nil {
		http.Error(w, "failed to parse JSON: "+err.Error(), http.StatusBadRequest)
		return
	}
	if b.Lang != "" && b.Lang != "zh" && b.Lang != "en" {
		http.Error(w, "lang must be zh / en / empty (follow system)", http.StatusBadRequest)
		return
	}
	if err := config.WriteProgramSetting("ui-lang", b.Lang); err != nil {
		http.Error(w, "failed to write program settings: "+err.Error(), http.StatusInternalServerError)
		return
	}
	platform.ApplyUILang(b.Lang) // Effective hot; no reloadConfig — language and routing config don't entangle
	log.Printf("[lang] UI language switched to %s (%s)", platform.CurrentUILang(), config.ProgramSettingsFile)
	_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "lang": platform.CurrentUILang()})
}
