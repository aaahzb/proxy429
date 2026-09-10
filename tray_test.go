package main

import (
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// TestTrayState 验证三态优先级：active>0 绿(2) > waiting>0 黄(1) > 灰(0)。
// 并行请求时取高优先级，所以两者都设时应返回绿。
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
	// active 优先于 waiting：两者都设时取绿。
	stats.mu.Lock()
	stats.active = 1
	stats.mu.Unlock()
	if s := trayState(); s != 2 {
		t.Errorf("active+waiting want 2, got %d", s)
	}
}

// TestTrayTip 验证多行 tooltip：空闲/单态/两态并存。
func TestTrayTip(t *testing.T) {
	cases := []struct {
		active, waiting int
		want            string
	}{
		{0, 0, "Proxy429\nidle"},
		{2, 0, "Proxy429\nactive 2"},
		{0, 3, "Proxy429\nwaiting 3"},
		{2, 3, "Proxy429\nactive 2\nwaiting 3"},
	}
	for _, c := range cases {
		if got := trayTip(c.active, c.waiting); got != c.want {
			t.Errorf("trayTip(%d,%d)=%q want %q", c.active, c.waiting, got, c.want)
		}
	}
}

// TestSwitchConfigNotifiesTray 验证 switchConfig 成功后向托盘投递子菜单重建通知——
// 网页端切配置/新建配置不走托盘点击路径，托盘勾选靠这个通知刷新；失败切换不投递。
func TestSwitchConfigNotifiesTray(t *testing.T) {
	dir := t.TempDir()
	a := filepath.Join(dir, "a.json")
	b := filepath.Join(dir, "b.json")
	for _, p := range []string{a, b} {
		if err := os.WriteFile(p, []byte(`{"upstream":"http://127.0.0.1:1"}`), 0644); err != nil {
			t.Fatal(err)
		}
	}
	// 先清空通知通道，排除其它测试经 switchConfig 留下的积压干扰
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
	// 失败切换（文件不存在）不投递通知，勾选保持不动
	if err := switchConfig(filepath.Join(dir, "nope.json")); err == nil {
		t.Fatal("切换到不存在的文件应报错")
	}
	select {
	case <-trayCfgSwitched:
		t.Error("失败切换不应投递托盘重建通知")
	default:
	}
}

// TestDelConfigNotifiesTray 验证网页端删除配置文件成功后同样投递托盘重建通知——
// 否则托盘「切换配置」子菜单里被删的项要等手动「刷新列表」才消失；
// 删除当前在用配置被拒绝且不投递。
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
	// 先清空通知通道，排除其它测试留下的积压干扰
	select {
	case <-trayCfgSwitched:
	default:
	}

	call := func(name string) int {
		req := httptest.NewRequest("POST", "/__delconfig", strings.NewReader(`{"name":`+strconv.Quote(name)+`}`))
		req.RemoteAddr = "127.0.0.1:1" // isLocalRequest 要求本机
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

	// 删当前在用配置被拒绝，不投递通知
	if code := call("a.json"); code != 400 {
		t.Fatalf("删除当前配置应 400，实际 %d", code)
	}
	select {
	case <-trayCfgSwitched:
		t.Error("删除被拒绝时不应投递托盘重建通知")
	default:
	}
}

// TestRenameConfigNotifiesTray 验证网页端重命名配置文件后投递托盘重建通知——
// 文件名清单变了（重命名当前配置时勾选跟新名字），不等手动「刷新列表」。
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
	// 先清空通知通道，排除其它测试留下的积压干扰
	select {
	case <-trayCfgSwitched:
	default:
	}

	req := httptest.NewRequest("POST", "/__renameconfig", strings.NewReader(`{"old":"a.json","new":"b.json"}`))
	req.RemoteAddr = "127.0.0.1:1" // isLocalRequest 要求本机
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

// TestSaveConfigCreateNotifiesTray 验证「保存将创建」路径投递托盘重建通知——
// 当前配置文件此前不存在（如被外部删除）时保存会新建它，文件名清单变化；
// 文件已存在的普通保存不改变清单，不投递。
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
	// 先清空通知通道，排除其它测试留下的积压干扰
	select {
	case <-trayCfgSwitched:
	default:
	}

	call := func() int {
		req := httptest.NewRequest("POST", "/__config", strings.NewReader(`{"upstream":"http://127.0.0.1:1"}`))
		req.RemoteAddr = "127.0.0.1:1" // isLocalRequest 要求本机
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

	// 文件已存在的普通保存不投递
	if code := call(); code != 200 {
		t.Fatalf("再次保存应 200，实际 %d", code)
	}
	select {
	case <-trayCfgSwitched:
		t.Error("普通保存（文件已存在）不应投递托盘重建通知")
	default:
	}
}
