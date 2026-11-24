// lang_test.go — startup UI-language resolution: program-settings.txt wins, legacy config ui_lang is migrated, otherwise follow the OS.
package main

import (
	"os"
	"path/filepath"
	"testing"

	"proxy429/internal/config"
	"proxy429/internal/platform"
)

// TestResolveProgramUILangMigration startup migration: when program-settings.txt has no ui-lang but a legacy config
// file carries ui_lang, migrate it into program-settings.txt and delete the key from the config (text-level; every other byte
// of formatting preserved), effective immediately; when program-settings.txt already has a value it wins — the config is neither read nor touched.
func TestResolveProgramUILangMigration(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	orig := "{\n  \"max_retries\": 3,\n  \"upstream\": \"http://x\",\n  \"ui_lang\": \"zh\"\n}\n"
	if err := os.WriteFile(path, []byte(orig), 0644); err != nil {
		t.Fatal(err)
	}
	oldPath := config.CurrentConfigPath()
	config.SetCurrentPath(path)
	defer config.SetCurrentPath(oldPath)
	oldLang := platform.CurrentUILang()
	defer platform.ApplyUILang(oldLang)

	resolveProgramUILang()
	if got := platform.CurrentUILang(); got != "zh" {
		t.Errorf("迁移后 currentUILang=%q, want zh", got)
	}
	if got := config.ReadProgramSetting("ui-lang"); got != "zh" {
		t.Errorf("program-settings ui-lang=%q, want zh", got)
	}
	want := "{\n  \"max_retries\": 3,\n  \"upstream\": \"http://x\"\n}\n"
	if data, _ := os.ReadFile(path); string(data) != want {
		t.Errorf("迁移后配置应为:\n%q\ngot:\n%q", want, data)
	}

	// An existing value in program-settings.txt wins: even if the config still carries ui_lang it neither overrides the program setting nor gets modified
	if err := os.WriteFile(path, []byte(orig), 0644); err != nil {
		t.Fatal(err)
	}
	if err := config.WriteProgramSetting("ui-lang", "en"); err != nil {
		t.Fatal(err)
	}
	resolveProgramUILang()
	if got := platform.CurrentUILang(); got != "en" {
		t.Errorf("program-settings 优先: currentUILang=%q, want en", got)
	}
	if data, _ := os.ReadFile(path); string(data) != orig {
		t.Errorf("program-settings 已有值时不应再动配置:\n%q", data)
	}
}
