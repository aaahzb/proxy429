package config

import (
	_ "embed"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
)

var cfg atomic.Pointer[Config]

// Current returns the active config (nil before the startup load).
func Current() *Config { return cfg.Load() }

// Store atomically replaces the active config.
func Store(c *Config) { cfg.Store(c) }

// SetCurrentPath updates the active config file path under lock (startup, config switch, rename).
func SetCurrentPath(p string) {
	configMu.Lock()
	configFilePath = p
	configMu.Unlock()
}

// RenameCurrentPath atomically repoints the active config path from oldFull to
// newFull when it currently equals oldFull; reports whether it did.
func RenameCurrentPath(oldFull, newFull string) bool {
	configMu.Lock()
	defer configMu.Unlock()
	if configFilePath != oldFull {
		return false
	}
	configFilePath = newFull
	return true
}

// configFilePath is the config file path, set at main startup and reused by tray reload.
// Rewritten by switchConfig on config switches, hence guarded by configMu for concurrent access.
var configFilePath string

// configMu guards concurrent access to configFilePath:
// config switching (write) and reload/web config reads (read) can race; the lock avoids reading a half-updated path.
var configMu sync.RWMutex

// CurrentConfigPath returns the current config file path under lock.
func CurrentConfigPath() string {
	configMu.RLock()
	defer configMu.RUnlock()
	return configFilePath
}

// ListConfigFiles scans the current config directory for .json files (excluding config.example.json)
// and returns the sorted file names (no directories). Feeds the tray "switch config" submenu and the web dropdown.
func ListConfigFilesIn(dir string) []string {
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

// ListConfigFiles lists every .json config in the current config directory (for the web "switch config" menu).
func ListConfigFiles() []string {
	return ListConfigFilesIn(filepath.Dir(CurrentConfigPath()))
}

// ConfigExampleBytes is the embedded default config template, written to the user config directory on first run.
//
//go:embed config.example.json
var ConfigExampleBytes []byte

// activeConfigStateFile records the last selected routing config's file name (basename), stored next to the routing configs.
// A .txt extension (not .json) keeps it out of ListConfigFiles results and avoids clashing with user-created .json files.
const activeConfigStateFile = "active-config.txt"

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

// WriteActiveConfigState writes the given config's file name (basename) into the state file in its directory, for restore on next launch.
// Write failures are only logged; the switch itself is unaffected.
func WriteActiveConfigState(path string) {
	statePath := filepath.Join(filepath.Dir(path), activeConfigStateFile)
	if err := os.WriteFile(statePath, []byte(filepath.Base(path)), 0644); err != nil {
		log.Printf("[config] failed to write state file: %v", err)
	}
}

// ProgramSettingsFile is the dedicated carrier of program-level settings: preferences like the UI language that
// have nothing to do with any upstream stay separate from routing config and never enter config*.json (legacy versions
// wrote ui_lang into the config file; since migrated out). The file lives in the current config directory, same family
// as active-config.txt; line-based key=value, lines starting with # are comments, extensible.
const ProgramSettingsFile = "program-settings.txt"

// ProgramSettingsPath returns the full path of the program-settings file (follows the current config directory).
func ProgramSettingsPath() string {
	return filepath.Join(filepath.Dir(CurrentConfigPath()), ProgramSettingsFile)
}

// ReadProgramSetting returns the value of key in program settings (trimmed); missing file or key returns "".
func ReadProgramSetting(key string) string {
	data, err := os.ReadFile(ProgramSettingsPath())
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

// WriteProgramSetting sets (non-empty value) or deletes (empty value) a program-settings key.
// All other lines (comments, order, formatting) are preserved byte-for-byte; a missing key with a non-empty value is appended at the end.
// Always LF line endings with a trailing newline; write failures are returned for the caller to surface.
func WriteProgramSetting(key, value string) error {
	data, _ := os.ReadFile(ProgramSettingsPath())
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
	return os.WriteFile(ProgramSettingsPath(), []byte(strings.Join(out, "\n")), 0644)
}

// ResolveConfigPath decides the config file path, by priority:
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
func ResolveConfigPath(flagPath string) string {
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
	if files := ListConfigFilesIn(dir); len(files) > 0 {
		return filepath.Join(dir, files[0])
	}
	// First run: no config in the directory — create it and write the embedded default template, avoiding a config-less startup failure after a .app double-click.
	// The user then sets their own upstream/key in the web "Config" tab.
	if dir != "." {
		if mkErr := os.MkdirAll(dir, 0755); mkErr == nil {
			if wErr := os.WriteFile(defaultPath, ConfigExampleBytes, 0644); wErr == nil {
				log.Printf("[config] first run; default config created: %s", defaultPath)
			}
		}
	}
	return defaultPath
}
