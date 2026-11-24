package config

import (
	"os"
	"path/filepath"
	"testing"
)

// TestLoadConfigUILangIgnored: the UI language has moved out of config files: Config no longer has a ui_lang field;
// a leftover ui_lang in a config (including legacy values that used to be rejected) is ignored as an unknown field; loading doesn't fail.
func TestLoadConfigUILangIgnored(t *testing.T) {
	for _, lang := range []string{"zh", "en", "fr", ""} {
		body := `{"upstream":"http://x","ui_lang":"` + lang + `"}`
		p := filepath.Join(t.TempDir(), "config.json")
		if err := os.WriteFile(p, []byte(body), 0644); err != nil {
			t.Fatal(err)
		}
		if _, _, err := LoadConfig(p); err != nil {
			t.Errorf("ui_lang=%q 应被忽略且加载成功: %v", lang, err)
		}
	}
}

// TestProgramSettingsRoundTrip program-settings read/write: set/change/delete keys; comments and other keys' lines
// are preserved verbatim; deleting a nonexistent key doesn't error; LF line endings and a trailing newline throughout.
func TestProgramSettingsRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	if err := os.WriteFile(path, []byte(`{"upstream":"http://x"}`), 0644); err != nil {
		t.Fatal(err)
	}
	oldPath := CurrentConfigPath()
	SetCurrentPath(path)
	defer SetCurrentPath(oldPath)

	if got := ReadProgramSetting("ui-lang"); got != "" {
		t.Fatalf("初始应无 ui-lang, got %q", got)
	}
	if err := WriteProgramSetting("ui-lang", "zh"); err != nil {
		t.Fatal(err)
	}
	// Manually add a comment line and another key, verifying set/delete don't break them
	data, _ := os.ReadFile(ProgramSettingsPath())
	text := "# 程序设置（与路由配置分离）\n" + string(data) + "other=1\n"
	if err := os.WriteFile(ProgramSettingsPath(), []byte(text), 0644); err != nil {
		t.Fatal(err)
	}
	if err := WriteProgramSetting("ui-lang", "en"); err != nil { // Change a value
		t.Fatal(err)
	}
	want := "# 程序设置（与路由配置分离）\nui-lang=en\nother=1\n"
	if data, _ := os.ReadFile(ProgramSettingsPath()); string(data) != want {
		t.Errorf("改值后应为:\n%q\ngot:\n%q", want, data)
	}
	if err := WriteProgramSetting("ui-lang", ""); err != nil { // Delete a key
		t.Fatal(err)
	}
	want = "# 程序设置（与路由配置分离）\nother=1\n"
	if data, _ := os.ReadFile(ProgramSettingsPath()); string(data) != want {
		t.Errorf("删键后应为:\n%q\ngot:\n%q", want, data)
	}
	if err := WriteProgramSetting("no-such-key", ""); err != nil { // Delete a nonexistent key
		t.Errorf("删不存在的键不应报错: %v", err)
	}
	if got := ReadProgramSetting("other"); got != "1" {
		t.Errorf("readProgramSetting(other)=%q, want 1", got)
	}
}
