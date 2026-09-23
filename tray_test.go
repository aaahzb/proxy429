package main

import (
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// TestTrayState verifies the three-state priority: active>0 green (2) > waiting>0 yellow (1) > grey (0).
// With parallel requests the higher priority wins, so setting both should return green.
func TestTrayState(t *testing.T) {
	resetStats()
	if s := trayState(); s != 0 {
		t.Errorf("空闲 want 0, got %d", s)
	}
	stats.mu.Lock()
	stats.waiting = 1
	stats.mu.Unlock()
	if s := trayState(); s != 1 {
		t.Errorf("waiting want 1, got %d", s)
	}
	// active beats waiting: with both set, green wins.
	stats.mu.Lock()
	stats.active = 1
	stats.mu.Unlock()
	if s := trayState(); s != 2 {
		t.Errorf("active+waiting want 2, got %d", s)
	}
}

// TestTrayTip verifies the multi-line tooltip: idle / single state / both states coexisting, plus the config-problem line
// (removed keys present) prepended before the activity lines.
func TestTrayTip(t *testing.T) {
	prev := currentUILang()
	applyUILang("zh") // Pin the language so the problem-line assertion is deterministic
	defer applyUILang(prev)
	cases := []struct {
		active, waiting int
		problem         bool
		want            string
	}{
		{0, 0, false, "Proxy429\nidle"},
		{2, 0, false, "Proxy429\nactive 2"},
		{0, 3, false, "Proxy429\nwaiting 3"},
		{2, 3, false, "Proxy429\nactive 2\nwaiting 3"},
		{0, 0, true, "Proxy429\n⚠ 配置含已删除的键，详见控制台文档\nidle"},
		{2, 0, true, "Proxy429\n⚠ 配置含已删除的键，详见控制台文档\nactive 2"},
	}
	for _, c := range cases {
		if got := trayTip(c.active, c.waiting, c.problem); got != c.want {
			t.Errorf("trayTip(%d,%d,%v)=%q want %q", c.active, c.waiting, c.problem, got, c.want)
		}
	}
}

// TestSwitchConfigNotifiesTray verifies switchConfig posts a submenu-rebuild notification to the tray on success —
// web-side config switches/creates don't go through the tray-click path, so tray checkmarks refresh via this notification; failed switches don't post.
func TestSwitchConfigNotifiesTray(t *testing.T) {
	dir := t.TempDir()
	a := filepath.Join(dir, "a.json")
	b := filepath.Join(dir, "b.json")
	for _, p := range []string{a, b} {
		if err := os.WriteFile(p, []byte(`{"upstream":"http://127.0.0.1:1"}`), 0644); err != nil {
			t.Fatal(err)
		}
	}
	// Drain the notification channel first, excluding backlog left by other tests via switchConfig
	select {
	case <-trayCfgSwitched:
	default:
	}
	if err := switchConfig(b); err != nil {
		t.Fatalf("switchConfig: %v", err)
	}
	defer func() {
		configMu.Lock()
		configFilePath = ""
		configMu.Unlock()
		cfg.Store(&Config{})
	}()
	select {
	case <-trayCfgSwitched:
	default:
		t.Error("switchConfig 成功后应投递托盘重建通知")
	}
	if filepath.Base(currentConfigPath()) != "b.json" {
		t.Errorf("currentConfigPath=%q, want b.json", currentConfigPath())
	}
	// A failed switch (file doesn't exist) posts no notification; checkmarks stay put
	if err := switchConfig(filepath.Join(dir, "nope.json")); err == nil {
		t.Fatal("切换到不存在的文件应报错")
	}
	select {
	case <-trayCfgSwitched:
		t.Error("失败切换不应投递托盘重建通知")
	default:
	}
}

// TestDelConfigNotifiesTray verifies web-side config-file deletion also posts a tray-rebuild notification on success —
// otherwise the deleted entry in the tray's 「切换配置」 submenu would linger until a manual 「刷新列表」;
// deleting the config currently in use is refused and posts nothing.
func TestDelConfigNotifiesTray(t *testing.T) {
	dir := t.TempDir()
	a := filepath.Join(dir, "a.json")
	b := filepath.Join(dir, "b.json")
	for _, p := range []string{a, b} {
		if err := os.WriteFile(p, []byte(`{"upstream":"http://127.0.0.1:1"}`), 0644); err != nil {
			t.Fatal(err)
		}
	}
	configMu.Lock()
	configFilePath = a
	configMu.Unlock()
	defer func() {
		configMu.Lock()
		configFilePath = ""
		configMu.Unlock()
	}()
	// Drain the notification channel first, excluding backlog left by other tests
	select {
	case <-trayCfgSwitched:
	default:
	}

	call := func(name string) int {
		req := httptest.NewRequest("POST", "/__delconfig", strings.NewReader(`{"name":`+strconv.Quote(name)+`}`))
		req.RemoteAddr = "127.0.0.1:1" // isLocalRequest requires localhost
		rec := httptest.NewRecorder()
		delConfigHandler(rec, req)
		return rec.Code
	}

	if code := call("b.json"); code != 200 {
		t.Fatalf("删除 b.json 应 200，实际 %d", code)
	}
	if _, err := os.Stat(b); !os.IsNotExist(err) {
		t.Errorf("b.json 应已删除")
	}
	select {
	case <-trayCfgSwitched:
	default:
		t.Error("删除配置成功后应投递托盘重建通知")
	}

	// Deleting the config in use is refused; no notification posted
	if code := call("a.json"); code != 400 {
		t.Fatalf("删除当前配置应 400，实际 %d", code)
	}
	select {
	case <-trayCfgSwitched:
		t.Error("删除被拒绝时不应投递托盘重建通知")
	default:
	}
}

// TestRenameConfigNotifiesTray verifies web-side config-file renaming posts a tray-rebuild notification —
// the file list changed (when renaming the current config the checkmark follows the new name), not waiting for a manual 「刷新列表」.
func TestRenameConfigNotifiesTray(t *testing.T) {
	dir := t.TempDir()
	a := filepath.Join(dir, "a.json")
	if err := os.WriteFile(a, []byte(`{"upstream":"http://127.0.0.1:1"}`), 0644); err != nil {
		t.Fatal(err)
	}
	configMu.Lock()
	configFilePath = a
	configMu.Unlock()
	defer func() {
		configMu.Lock()
		configFilePath = ""
		configMu.Unlock()
	}()
	// Drain the notification channel first, excluding backlog left by other tests
	select {
	case <-trayCfgSwitched:
	default:
	}

	req := httptest.NewRequest("POST", "/__renameconfig", strings.NewReader(`{"old":"a.json","new":"b.json"}`))
	req.RemoteAddr = "127.0.0.1:1" // isLocalRequest requires localhost
	rec := httptest.NewRecorder()
	renameConfigHandler(rec, req)
	if rec.Code != 200 {
		t.Fatalf("重命名应 200，实际 %d: %s", rec.Code, rec.Body.String())
	}
	if filepath.Base(currentConfigPath()) != "b.json" {
		t.Errorf("重命名当前配置后 currentConfigPath=%q, want b.json", currentConfigPath())
	}
	select {
	case <-trayCfgSwitched:
	default:
		t.Error("重命名配置后应投递托盘重建通知")
	}
}

// TestSaveConfigCreateNotifiesTray verifies the "save will create" path posts a tray-rebuild notification —
// when the current config file didn't exist before (e.g. deleted externally), saving creates it, changing the file list;
// an ordinary save of an existing file doesn't change the list and posts nothing.
func TestSaveConfigCreateNotifiesTray(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "c.json")
	configMu.Lock()
	configFilePath = p
	configMu.Unlock()
	defer func() {
		configMu.Lock()
		configFilePath = ""
		configMu.Unlock()
		cfg.Store(&Config{})
	}()
	// Drain the notification channel first, excluding backlog left by other tests
	select {
	case <-trayCfgSwitched:
	default:
	}

	call := func() int {
		req := httptest.NewRequest("POST", "/__config", strings.NewReader(`{"upstream":"http://127.0.0.1:1"}`))
		req.RemoteAddr = "127.0.0.1:1" // isLocalRequest requires localhost
		rec := httptest.NewRecorder()
		configPostHandler(rec, req)
		return rec.Code
	}

	if code := call(); code != 200 {
		t.Fatalf("保存应 200，实际 %d", code)
	}
	if _, err := os.Stat(p); err != nil {
		t.Fatalf("保存应新建文件: %v", err)
	}
	select {
	case <-trayCfgSwitched:
	default:
		t.Error("保存新建配置文件后应投递托盘重建通知")
	}

	// An ordinary save of an existing file posts nothing
	if code := call(); code != 200 {
		t.Fatalf("再次保存应 200，实际 %d", code)
	}
	select {
	case <-trayCfgSwitched:
		t.Error("普通保存（文件已存在）不应投递托盘重建通知")
	default:
	}
}

// TestConfigPostRemovedKeysRejected locks the save-side strictness: removed keys are non-fatal at startup (degraded run),
// but the web save handler refuses to write them to disk — HTTP 400 with the migration hint (shown inline in #cfgMsg),
// and the file is never created/touched.
func TestConfigPostRemovedKeysRejected(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "c.json")
	configMu.Lock()
	configFilePath = p
	configMu.Unlock()
	defer func() {
		configMu.Lock()
		configFilePath = ""
		configMu.Unlock()
		cfg.Store(&Config{})
	}()

	req := httptest.NewRequest("POST", "/__config", strings.NewReader(`{"upstream":"http://127.0.0.1:1","translateNone2Low":true}`))
	req.RemoteAddr = "127.0.0.1:1" // isLocalRequest requires localhost
	rec := httptest.NewRecorder()
	configPostHandler(rec, req)
	if rec.Code != 400 {
		t.Fatalf("含已删除键的保存应 400，实际 %d", rec.Code)
	}
	if body := rec.Body.String(); !strings.Contains(body, `config key "translateNone2Low" was removed`) || !strings.Contains(body, "convertOff2Low") {
		t.Errorf("400 文案应含键名与迁移提示: %s", body)
	}
	if _, err := os.Stat(p); !os.IsNotExist(err) {
		t.Error("被拒的配置不应写盘")
	}
}
