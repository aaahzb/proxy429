package main

import "testing"

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
