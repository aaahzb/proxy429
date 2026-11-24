package logview

import (
	"encoding/json"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"proxy429/internal/config"
	"proxy429/internal/platform"
)

// configGetHandler returns the config file path and raw content (JSON), for the 「配置」 tab to load into the editor.
func configGetHandler(w http.ResponseWriter, r *http.Request) {
	if !platform.IsLocalRequest(r) {
		http.Error(w, "forbidden (local only)", http.StatusForbidden)
		return
	}
	data, err := os.ReadFile(config.CurrentConfigPath())
	content := ""
	if err == nil {
		content = string(data)
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"path":    config.CurrentConfigPath(),
		"content": content,
		"exists":  err == nil,
	})
}

// ConfigPostHandler saves the edited config and reloads it.
// It first probes the content with config.ParseConfig — the same validation config.LoadConfig uses (JSON syntax, removed keys with
// migration hints, enum values) — writing to disk only on success. A rejected config never reaches the file, so it
// cannot brick the next startup. reloadConfig applies it after the write.
func ConfigPostHandler(w http.ResponseWriter, r *http.Request) {
	if !platform.IsLocalRequest(r) {
		http.Error(w, "forbidden (local only)", http.StatusForbidden)
		return
	}
	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "failed to read request body: "+err.Error(), http.StatusBadRequest)
		return
	}
	// Probe with config.ParseConfig (not bare json.Unmarshal): bad JSON and invalid enum values are rejected before anything
	// touches the disk. Removed keys are non-fatal at startup, but saving is interactive (the user sees this message in
	// #cfgMsg), so they are rejected here too — a removed key must not be written back to disk from the console.
	if _, warns, err := config.ParseConfig(body); err != nil {
		http.Error(w, "invalid config; not saved: "+err.Error(), http.StatusBadRequest)
		return
	} else if len(warns) > 0 {
		msgs := make([]string, 0, len(warns))
		for _, k := range warns {
			msgs = append(msgs, config.RemovedKeyWarningEN(k))
		}
		http.Error(w, "invalid config; not saved: "+strings.Join(msgs, "; "), http.StatusBadRequest)
		return
	}
	_, statErr := os.Stat(config.CurrentConfigPath()) // Didn't exist before the save → this save creates the file; the file list changes
	if err := os.WriteFile(config.CurrentConfigPath(), body, 0644); err != nil {
		http.Error(w, "failed to write file: "+err.Error(), http.StatusInternalServerError)
		return
	}
	if err := deps.ReloadConfig(); err != nil {
		// File saved but reload failed (very rare, since it was validated above): the old config is still running; tell the user.
		http.Error(w, "saved but reload failed: "+err.Error(), http.StatusInternalServerError)
		return
	}
	if os.IsNotExist(statErr) {
		// The current config file didn't exist before (e.g. deleted externally) and this save created it: notify the tray to rebuild the submenu
		deps.TrayCfgChanged()
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
}

// reloadHandler only reloads the config (without modifying the file), for the 「仅重载」 button.
func reloadHandler(w http.ResponseWriter, r *http.Request) {
	if !platform.IsLocalRequest(r) {
		http.Error(w, "forbidden (local only)", http.StatusForbidden)
		return
	}
	if err := deps.ReloadConfig(); err != nil {
		http.Error(w, "reload failed: "+err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
}

// configsHandler lists all .json files in the current config directory, for the web 「配置」 tab's dropdown switching.
// Returns the current file name, the directory, and the available file list. Localhost only.
func configsHandler(w http.ResponseWriter, r *http.Request) {
	if !platform.IsLocalRequest(r) {
		http.Error(w, "forbidden (local only)", http.StatusForbidden)
		return
	}
	cur := config.CurrentConfigPath()
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"current": filepath.Base(cur),
		"dir":     filepath.Dir(cur),
		"files":   config.ListConfigFiles(),
	})
}

// switchHandler switches to the given config file with immediate effect. body: {"name":"work.json"}.
// name must be a bare file name (no path separators/..), preventing path traversal from switching to files outside the directory. Localhost only.
func switchHandler(w http.ResponseWriter, r *http.Request) {
	if !platform.IsLocalRequest(r) {
		http.Error(w, "forbidden (local only)", http.StatusForbidden)
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var body struct {
		Name string `json:"name"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "parse failed: "+err.Error(), http.StatusBadRequest)
		return
	}
	name := body.Name
	// Path-traversal guard: only bare file names allowed, no separators or ..
	if name == "" || strings.ContainsAny(name, `/\`) || strings.Contains(name, "..") {
		http.Error(w, "invalid config file name", http.StatusBadRequest)
		return
	}
	full := filepath.Join(filepath.Dir(config.CurrentConfigPath()), name)
	if err := deps.SwitchConfig(full); err != nil {
		http.Error(w, "switch failed: "+err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "current": name})
}

// validConfigName validates and normalizes a config file name: only bare file names (no path separators/..),
// auto-appending the .json suffix. Returns the normalized name and whether it's legal.
func validConfigName(name string) (string, bool) {
	name = strings.TrimSpace(name)
	if name == "" || strings.ContainsAny(name, `/\`) || strings.Contains(name, "..") {
		return "", false
	}
	if !strings.HasSuffix(name, ".json") {
		name += ".json"
	}
	return name, true
}

// readConfigTemplate returns the blank template for new configs.
// It uses the compile-time embedded config.example.json (config.ConfigExampleBytes) directly, guaranteeing it's always the latest version shipped with the exe,
// and never reads the disk copy — a disk config.example.json may be an old version not updated with the exe, producing stale new configs.
// To view/edit the reference template, see config.example.json in the release directory.
func readConfigTemplate() []byte {
	return config.ConfigExampleBytes
}

// newConfigHandler creates a new config file: writes config.example.json as the blank template under the given name,
// switching to the new config immediately after creation. Refuses if the file already exists. Localhost only.
func newConfigHandler(w http.ResponseWriter, r *http.Request) {
	if !platform.IsLocalRequest(r) {
		http.Error(w, "forbidden (local only)", http.StatusForbidden)
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var body struct {
		Name string `json:"name"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "parse failed: "+err.Error(), http.StatusBadRequest)
		return
	}
	name, ok := validConfigName(body.Name)
	if !ok {
		http.Error(w, "invalid config file name (plain file name only, no path)", http.StatusBadRequest)
		return
	}
	full := filepath.Join(filepath.Dir(config.CurrentConfigPath()), name)
	if _, err := os.Stat(full); err == nil {
		http.Error(w, "file already exists: "+name, http.StatusConflict)
		return
	}
	if err := os.WriteFile(full, readConfigTemplate(), 0644); err != nil {
		http.Error(w, "write failed: "+err.Error(), http.StatusInternalServerError)
		return
	}
	if err := deps.SwitchConfig(full); err != nil {
		http.Error(w, "created but switch failed: "+err.Error(), http.StatusInternalServerError)
		return
	}
	log.Printf("[config] created %s", full)
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "current": name})
}

// RenameConfigHandler renames a config file. If the renamed file is the config currently in use, configFilePath is updated in sync,
// so subsequent saves/reloads point at the new file. Refuses if the target name already exists. Localhost only.
func RenameConfigHandler(w http.ResponseWriter, r *http.Request) {
	if !platform.IsLocalRequest(r) {
		http.Error(w, "forbidden (local only)", http.StatusForbidden)
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var body struct {
		Old string `json:"old"`
		New string `json:"new"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "parse failed: "+err.Error(), http.StatusBadRequest)
		return
	}
	oldName, ok := validConfigName(body.Old)
	if !ok {
		http.Error(w, "invalid source file name", http.StatusBadRequest)
		return
	}
	newName, ok := validConfigName(body.New)
	if !ok {
		http.Error(w, "invalid target file name", http.StatusBadRequest)
		return
	}
	dir := filepath.Dir(config.CurrentConfigPath())
	oldFull := filepath.Join(dir, oldName)
	newFull := filepath.Join(dir, newName)
	if _, err := os.Stat(oldFull); err != nil {
		http.Error(w, "source file does not exist: "+oldName, http.StatusBadRequest)
		return
	}
	if _, err := os.Stat(newFull); err == nil {
		http.Error(w, "target already exists: "+newName, http.StatusConflict)
		return
	}
	if err := os.Rename(oldFull, newFull); err != nil {
		http.Error(w, "rename failed: "+err.Error(), http.StatusInternalServerError)
		return
	}
	// If the renamed file is the current config, update the active path so subsequent reads/writes point at the new file
	renamed := config.RenameCurrentPath(oldFull, newFull)
	if renamed {
		config.WriteActiveConfigState(newFull)
	}
	log.Printf("[config] renamed %s -> %s", oldName, newName)
	// The file list changed (if the renamed one is the current config, the tray checkmark follows the new name): notify the tray to rebuild the submenu
	deps.TrayCfgChanged()
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "current": filepath.Base(config.CurrentConfigPath())})
}

// DelConfigHandler deletes a config file. Deleting the config currently in use is refused (switch away first).
// Localhost only. The frontend double-confirms against accidents.
func DelConfigHandler(w http.ResponseWriter, r *http.Request) {
	if !platform.IsLocalRequest(r) {
		http.Error(w, "forbidden (local only)", http.StatusForbidden)
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var body struct {
		Name string `json:"name"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "parse failed: "+err.Error(), http.StatusBadRequest)
		return
	}
	name, ok := validConfigName(body.Name)
	if !ok {
		http.Error(w, "invalid config file name", http.StatusBadRequest)
		return
	}
	full := filepath.Join(filepath.Dir(config.CurrentConfigPath()), name)
	if full == config.CurrentConfigPath() {
		http.Error(w, "cannot delete the active config; switch to another config first", http.StatusBadRequest)
		return
	}
	if _, err := os.Stat(full); err != nil {
		http.Error(w, "file does not exist: "+name, http.StatusBadRequest)
		return
	}
	if err := os.Remove(full); err != nil {
		http.Error(w, "delete failed: "+err.Error(), http.StatusInternalServerError)
		return
	}
	log.Printf("[config] deleted %s", full)
	// Notify the tray to rebuild the 「切换配置」 submenu without the deleted entry (same notification as switchConfig's success path)
	deps.TrayCfgChanged()
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
}
