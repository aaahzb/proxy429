package main

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// TestExtractConvID 验证 Anthropic 口会话标识提取：Claude Code 的 metadata.user_id 是
// JSON 字符串（取其中 session_id）；裸 API 的自由文本原样返回；无 metadata 返回空。
// 含巨型 messages 数组在前的用例，验证能跨过大字段定位到后面的 metadata。
func TestExtractConvID(t *testing.T) {
	// 巨型 messages 在 metadata 之前，验证 locateTopFields 的跨字段定位
	bigMessages := `"messages":[` + strings.Repeat(`{"role":"user","content":"`+strings.Repeat("x", 1024)+`"},`, 64) + `{"role":"user","content":"尾"}]`
	cases := []struct {
		name string
		body string
		want string
	}{
		{
			"Claude Code JSON 形态 user_id 取 session_id",
			`{"model":"claude-x","metadata":{"user_id":"{\"device_id\":\"d\",\"account_uuid\":\"a\",\"session_id\":\"sid-123\"}"}}`,
			"sid-123",
		},
		{
			"巨型 messages 在前 metadata 在后",
			`{"model":"claude-x",` + bigMessages + `,"metadata":{"user_id":"{\"session_id\":\"sid-big\"}"}}`,
			"sid-big",
		},
		{
			"纯字符串 user_id 原样返回",
			`{"metadata":{"user_id":"free-text-session"}}`,
			"free-text-session",
		},
		{
			"JSON 形态但无 session_id 时原样返回整串（稳定即可关联）",
			`{"metadata":{"user_id":"{\"foo\":1}"}}`,
			`{"foo":1}`,
		},
		{"无 metadata", `{"model":"x","messages":[]}`, ""},
		{"metadata 无 user_id", `{"metadata":{"other":"v"}}`, ""},
		{"metadata 非对象", `{"metadata":"str"}`, ""},
		{"非法 JSON", `{oops`, ""},
		{"空 body", ``, ""},
	}
	for _, c := range cases {
		if got := extractConvID([]byte(c.body)); got != c.want {
			t.Errorf("%s: extractConvID = %q，期望 %q", c.name, got, c.want)
		}
	}
}

// TestConvKeyOf 验证锚定键：无会话标识返回空（不参与缓存年龄显示与保护），有则 convID|convAnchor。
func TestConvKeyOf(t *testing.T) {
	f := &flight{}
	if got := convKeyOf(f); got != "" {
		t.Errorf("无 convID 应返回空，实际 %q", got)
	}
	f.convID = "sid-1"
	f.convAnchor = "route:claude-*"
	if got := convKeyOf(f); got != "sid-1|route:claude-*" {
		t.Errorf("锚定键 = %q", got)
	}
}

// TestAddFinished499Anchor 验证 499 流的锚定规则：黄灯（等首字节 stage<3）被下游断开
// 不刷新锚（归档 convKey 置空显 "-"，该键锚停留再上一次同键流）；绿灯（转发中 stage=3）
// 断开照常刷新锚（本行照常显年龄）。
func TestAddFinished499Anchor(t *testing.T) {
	oldFinished, oldCap := finished, finishedCap.Load()
	defer func() {
		finished = oldFinished
		finishedCap.Store(oldCap)
	}()

	mkFlight := func(id uint64, status, stage int32) *flight {
		f := &flight{id: id, convID: "s1", convAnchor: "route:a", status: int(status)}
		f.stage.Store(stage)
		f.start = time.Now() // 保护窗口 = start + 固定 5min，窗口内受保护
		return f
	}

	// 黄灯 499：归档后无锚定键；cap=1 触发裁剪时受保护的是旧行 #1（#2 无键不抢锚，两行都留）
	finished = nil
	finishedCap.Store(200)
	addFinished(mkFlight(1, 200, 3))
	addFinished(mkFlight(2, 499, 2))
	if got := finished[1].convKey; got != "" {
		t.Errorf("黄灯 499 应置空锚定键，实际 %q", got)
	}
	finishedCap.Store(1)
	trimFinishedLocked()
	if len(finished) != 2 || finished[0].id != 1 {
		t.Fatalf("黄灯 499 不抢锚，#1 应受保护留 2 行，实际 %+v", finished)
	}

	// 绿灯 499：照常锚定；cap=1 时 #3 是该键最新且在保护窗口内受保护，旧行 #1 被裁
	finished = nil
	finishedCap.Store(200)
	addFinished(mkFlight(1, 200, 3))
	addFinished(mkFlight(3, 499, 3))
	if got := finished[1].convKey; got != "s1|route:a" {
		t.Errorf("绿灯 499 应保留锚定键，实际 %q", got)
	}
	finishedCap.Store(1)
	trimFinishedLocked()
	if len(finished) != 1 || finished[0].id != 3 {
		t.Fatalf("绿灯 499 照常锚定，应只留 #3，实际 %+v", finished)
	}
}

// TestRecentFlightsCacheRefreshed 验证「已刷新」冻结显示：同键有在途流吐字（stage=3 转发中）时，
// 该键最新完成行停止递增、冻结显示 [m:ss]（= 在途流开始那一刻旧锚的年龄，不随轮询变）；
// 黄灯在途流（stage<3）与异键在途流不触发；在途流结束后恢复实时递增。
func TestRecentFlightsCacheRefreshed(t *testing.T) {
	resetStats() // 初始化 flights.m 等全局（仅在 main() 里做的初始化，测试需显式重置）
	oldFinished := finished
	defer func() { finished = oldFinished }()

	key := "s1|route:a"
	t0 := time.Now()
	finished = []finishedFlight{{id: 1, convKey: key, start: t0.Add(-3 * time.Minute)}} // 缓存写于 3 分钟前

	call := func() string {
		req := httptest.NewRequest("GET", "/__recentflights", nil)
		req.RemoteAddr = "127.0.0.1:1" // isLocalRequest 要求本机
		rec := httptest.NewRecorder()
		recentFlightsHandler(rec, req)
		var d struct {
			List []struct {
				CacheAge string `json:"cacheAge"`
			} `json:"list"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &d); err != nil {
			t.Fatalf("响应非 JSON: %v", err)
		}
		if len(d.List) != 1 {
			t.Fatalf("期望 1 行，实际 %d", len(d.List))
		}
		return d.List[0].CacheAge
	}
	register := func(id uint64, convID, anchor string, stage int32, start time.Time) {
		f := &flight{id: id, convID: convID, convAnchor: anchor, start: start}
		f.stage.Store(stage)
		flights.register(f)
	}
	defer flights.unregister(90)
	defer flights.unregister(91)
	defer flights.unregister(92)

	if got := call(); !strings.Contains(got, ":") {
		t.Errorf("无在途流应为实时年龄（约 3:0x），实际 %q", got)
	}
	register(90, "s1", "route:a", 2, t0) // 黄灯在途（等首字节）：不触发
	if got := call(); !strings.Contains(got, ":") {
		t.Errorf("黄灯在途流不应触发冻结显示，实际 %q", got)
	}
	register(91, "other", "route:a", 3, t0) // 绿灯但异键：不触发
	if got := call(); !strings.Contains(got, ":") {
		t.Errorf("异键绿灯在途流不应触发冻结显示，实际 %q", got)
	}
	// 同键绿灯（转发中吐字）：冻结显示旧锚在在途流开始那一刻的年龄 = 3min-40s = "2:20"
	register(92, "s1", "route:a", 3, t0.Add(-40*time.Second))
	if got := call(); got != "[2:20]" {
		t.Errorf("同键绿灯在途流应冻结显示 [2:20]，实际 %q", got)
	}
	flights.unregister(92)
	if got := call(); !strings.Contains(got, ":") {
		t.Errorf("在途流结束后应恢复实时递增，实际 %q", got)
	}
}

// TestTrimFinishedLocked 验证完成流裁剪：裁员窗口 = 最旧的 len-cap 行，窗口内
// "每锚定键最新一行且仍在保护窗口（start + defaultCacheTTL）内"的行受保护不丢（不顺延补偿，行数可超 cap）；
// 超窗口行、被同键更新行刷新的行、无键行照常 FIFO。
func TestTrimFinishedLocked(t *testing.T) {
	oldFinished, oldCap := finished, finishedCap.Load()
	defer func() {
		finished = oldFinished
		finishedCap.Store(oldCap)
	}()

	fresh := time.Now()                 // 保护窗口内（start 距今 < 5min）
	stale := time.Now().Add(-time.Hour) // 已出保护窗口
	mk := func(id uint64, key string, start time.Time) finishedFlight {
		return finishedFlight{id: id, convKey: key, start: start}
	}

	t.Run("用户例子：14 行留 10，最旧 1 行窗口内 S2 受保护，留 11 行", func(t *testing.T) {
		// 行 0（最旧）= S2 最新且在窗口内；行 1..13 = S1（行 13 为 S1 最新）
		finished = []finishedFlight{mk(0, "S2|route:a", fresh)}
		for i := 1; i <= 13; i++ {
			finished = append(finished, mk(uint64(i), "S1|route:b", fresh))
		}
		finishedCap.Store(10)
		trimFinishedLocked()
		if len(finished) != 11 {
			t.Fatalf("应留 11 行，实际 %d", len(finished))
		}
		if finished[0].convKey != "S2|route:a" {
			t.Errorf("首行应是被保护的 S2，实际 %q", finished[0].convKey)
		}
		// 丢掉的应是 S1 最旧的 3 行（id 1..3），S1 最新 10 行（id 4..13）全留
		for i, ff := range finished[1:] {
			if wantID := uint64(i + 4); ff.id != wantID {
				t.Errorf("finished[%d].id = %d，期望 %d", i+1, ff.id, wantID)
			}
		}
	})

	t.Run("窗口内被同键更新行刷新的行不保护", func(t *testing.T) {
		// 同键两行都在保护窗口内，旧的（非最新）在裁员窗口内照样被裁
		finished = []finishedFlight{mk(0, "K|r", fresh), mk(1, "K|r", fresh)}
		finishedCap.Store(1)
		trimFinishedLocked()
		if len(finished) != 1 || finished[0].id != 1 {
			t.Fatalf("应只剩最新行 id=1，实际 %+v", finished)
		}
	})

	t.Run("出保护窗口的行不保护", func(t *testing.T) {
		// 行 0 是 K 最新但已出保护窗口；行 1 无键。cap=1 时行 0 在裁员窗口内被裁
		finished = []finishedFlight{mk(0, "K|r", stale), mk(1, "", fresh)}
		finishedCap.Store(1)
		trimFinishedLocked()
		if len(finished) != 1 || finished[0].id != 1 {
			t.Fatalf("超窗口行应被裁，实际 %+v", finished)
		}
	})

	t.Run("无键行不保护", func(t *testing.T) {
		finished = []finishedFlight{mk(0, "", fresh), mk(1, "K|r", fresh)}
		finishedCap.Store(1)
		trimFinishedLocked()
		if len(finished) != 1 || finished[0].id != 1 {
			t.Fatalf("无键行应被裁，实际 %+v", finished)
		}
	})

	t.Run("行数不超 cap 时不动作", func(t *testing.T) {
		finished = []finishedFlight{mk(0, "", stale), mk(1, "K|r", stale)}
		finishedCap.Store(10)
		trimFinishedLocked()
		if len(finished) != 2 {
			t.Fatalf("不应裁剪，实际 %d 行", len(finished))
		}
	})

	t.Run("裁员窗口内全是无键/超窗口行时退化为纯 FIFO", func(t *testing.T) {
		finished = []finishedFlight{mk(0, "", stale), mk(1, "K|r", stale), mk(2, "K|r", fresh), mk(3, "", stale)}
		finishedCap.Store(2)
		trimFinishedLocked()
		// 裁员窗口 = 行 0..1：行 0 无键、行 1 非 K 最新且超窗口，都被裁；留 id 2、3
		if len(finished) != 2 || finished[0].id != 2 || finished[1].id != 3 {
			t.Fatalf("应留 id 2、3，实际 %+v", finished)
		}
	})
}

// TestObsKeyOf 验证实测缓存存活配对键：convID 或 upstreamKey 任一侧为空返回空（不参与观测），
// 两侧都有则拼接 convID|upstreamKey；恶意超长会话标识截断防内存膨胀。
func TestObsKeyOf(t *testing.T) {
	f := &flight{}
	if got := obsKeyOf(f); got != "" {
		t.Errorf("convID/upstreamKey 都空应返回空，实际 %q", got)
	}
	f.convID = "sid-1"
	if got := obsKeyOf(f); got != "" {
		t.Errorf("upstreamKey 空应返回空，实际 %q", got)
	}
	f.upstreamKey = "https://api.x.com|model-a"
	if got := obsKeyOf(f); got != "sid-1|https://api.x.com|model-a" {
		t.Errorf("配对键 = %q", got)
	}
	f.convID = strings.Repeat("x", 600)
	if got := obsKeyOf(f); len(got) != cacheObsMaxKeyLen {
		t.Errorf("超长配对键应截断到 %d，实际 %d", cacheObsMaxKeyLen, len(got))
	}
}

// TestClassifyCacheObservation 验证实测缓存存活的观测判定：后条命中率 ≥95% → 存活观测；
// 前条命中过 + 后条命中率 <50% → 死亡观测（不看严格归零，系统提示词残留命中不算活着）；
// 中间地带（50%~95%）、体量 <1024、间隔非正、前条未命中过的低命中 均不观测。
// 体量检查优先于存活/死亡判定（小请求噪声大）。
func TestClassifyCacheObservation(t *testing.T) {
	cases := []struct {
		name                        string
		prevCR, curCR, curIn, curCC int64
		interval                    time.Duration
		want                        cacheObsKind
	}{
		{"存活：命中率 100%", 2000, 5000, 100, 0, 5 * time.Minute, obsAlive},
		{"存活：命中率恰 95% 边界（总量 2000）", 2000, 1900, 100, 0, 3 * time.Minute, obsAlive},
		{"中间地带 94% 不观测", 2000, 1880, 120, 0, 3 * time.Minute, obsNone},
		{"中间地带 60% 不观测", 1000, 1200, 800, 0, 5 * time.Minute, obsNone},
		{"死亡：前条命中过 + 后条零命中", 1000, 0, 3000, 0, 8 * time.Minute, obsDead},
		{"死亡：残留命中 20% 也算（系统提示词缓存）", 1000, 400, 1600, 0, 8 * time.Minute, obsDead},
		{"命中率恰 50% 边界不观测", 1000, 1000, 1000, 0, 8 * time.Minute, obsNone},
		{"前条未命中过的零命中不算死亡（前条没写过缓存）", 0, 0, 3000, 0, 8 * time.Minute, obsNone},
		{"体量不足 1024 不观测（命中率虽达标，总体量 1000）", 1000, 950, 50, 0, 5 * time.Minute, obsNone},
		{"间隔为零不观测", 1000, 5000, 100, 0, 0, obsNone},
		{"间隔为负不观测", 1000, 5000, 100, 0, -time.Minute, obsNone},
		{"体量不足优先于死亡判定", 1000, 0, 500, 0, 8 * time.Minute, obsNone},
	}
	for _, c := range cases {
		got, gotIv := classifyCacheObservation(c.prevCR, c.curCR, c.curIn, c.curCC, c.interval)
		if got != c.want {
			t.Errorf("%s: kind = %v，期望 %v", c.name, got, c.want)
		}
		if got != obsNone && gotIv != c.interval {
			t.Errorf("%s: interval = %v，期望 %v", c.name, gotIv, c.interval)
		}
	}
}

// TestRecordCacheObsLocked 验证观测累积：存活间隔取 max（实测下界只升）、
// 死亡间隔取 min（实测上界只降）、观测数累计；不同上游键互不干扰。
// 两侧矛盾（上游 TTL 中途变化/提前驱逐）时以较新观测为准，被否一侧作废重测。
func TestRecordCacheObsLocked(t *testing.T) {
	oldMap := cacheObsMap
	defer func() { cacheObsMap = oldMap }()
	cacheObsMap = map[string]*cacheObsEntry{}

	recordCacheObsLocked("u|m", obsAlive, 5*time.Minute)
	if cacheObsMap["u|m"].changedAt.IsZero() {
		t.Error("首次成界应记结论时间 changedAt")
	}
	stale := time.Now().Add(-time.Hour) // 哨兵：数值不变的观测必须保持 changedAt 不动

	cacheObsMap["u|m"].changedAt = stale
	recordCacheObsLocked("u|m", obsAlive, 3*time.Minute) // 较小不抬低下界，结论时间不重置
	if !cacheObsMap["u|m"].changedAt.Equal(stale) {
		t.Error("下界数值未变化不应重置结论时间")
	}
	recordCacheObsLocked("u|m", obsAlive, 9*time.Minute) // 较大抬高下界，结论时间重置
	if !cacheObsMap["u|m"].changedAt.After(stale) {
		t.Error("下界抬高应重置结论时间")
	}
	recordCacheObsLocked("u|m", obsDead, 20*time.Minute)
	recordCacheObsLocked("u|m", obsDead, 30*time.Minute) // 较大不抬升上界
	recordCacheObsLocked("u2|m2", obsAlive, time.Minute)

	e := cacheObsMap["u|m"]
	if e.aliveMax != 9*time.Minute {
		t.Errorf("aliveMax = %v，期望 9min", e.aliveMax)
	}
	if e.deadMin != 20*time.Minute {
		t.Errorf("deadMin = %v，期望 20min", e.deadMin)
	}
	if e.samples != 5 {
		t.Errorf("samples = %d，期望 5", e.samples)
	}
	if e2 := cacheObsMap["u2|m2"]; e2.aliveMax != time.Minute || e2.samples != 1 {
		t.Errorf("另一上游键应独立累积，实际 %+v", e2)
	}

	// TTL 中途变长：新存活观测 25min 越过旧上界 20min → 上界作废，下界照常抬高；观测数归零重计（本次为第 1 次）
	cacheObsMap["u|m"].changedAt = stale
	recordCacheObsLocked("u|m", obsAlive, 25*time.Minute)
	if e.aliveMax != 25*time.Minute || e.deadMin != 0 {
		t.Errorf("新存活越过旧上界：上界应作废，实际 aliveMax=%v deadMin=%v", e.aliveMax, e.deadMin)
	}
	if e.samples != 1 {
		t.Errorf("交叉作废后 samples = %d，期望 1（归零重计，本次观测为第 1 次）", e.samples)
	}
	if !e.changedAt.After(stale) {
		t.Error("上界作废重测应重置结论时间")
	}
	// 与下界不矛盾的死亡观测照常成立（活过 25min、40min 时已死，两侧并存），次数累加
	recordCacheObsLocked("u|m", obsDead, 40*time.Minute)
	if e.aliveMax != 25*time.Minute || e.deadMin != 40*time.Minute {
		t.Errorf("不矛盾的死亡应两侧并存，实际 aliveMax=%v deadMin=%v", e.aliveMax, e.deadMin)
	}
	if e.samples != 2 {
		t.Errorf("不矛盾观测后 samples = %d，期望 2", e.samples)
	}
	// TTL 中途变短（或提前驱逐）：新死亡观测 10min 跌破旧下界 25min → 下界作废，观测数再归零
	cacheObsMap["u|m"].changedAt = stale
	recordCacheObsLocked("u|m", obsDead, 10*time.Minute)
	if e.aliveMax != 0 || e.deadMin != 10*time.Minute {
		t.Errorf("新死亡跌破旧下界：下界应作废，实际 aliveMax=%v deadMin=%v", e.aliveMax, e.deadMin)
	}
	if e.samples != 1 {
		t.Errorf("再次交叉作废后 samples = %d，期望 1", e.samples)
	}
	if !e.changedAt.After(stale) {
		t.Error("下界作废重测应重置结论时间")
	}
}

// TestSnapshotCacheObs 锁定实测缓存时间快照：键拆成 URL（剥 scheme）+模型、下界/上界格式化为
// mm:ss（"00:30"，无观测侧留空）、观测次数透传、按 URL+模型排序；空表返回 nil。
func TestSnapshotCacheObs(t *testing.T) {
	oldMap := cacheObsMap
	defer func() { cacheObsMap = oldMap }()

	cacheObsMap = map[string]*cacheObsEntry{}
	if got := snapshotCacheObs(); got != nil {
		t.Errorf("无观测应返回 nil，实际 %+v", got)
	}

	cacheObsMap = map[string]*cacheObsEntry{
		"https://api.kimi.com/coding|k3-256k": {aliveMax: 23*time.Minute + 30*time.Second, deadMin: 41*time.Minute + 5*time.Second, samples: 12},
		"http://a.com|m":                      {aliveMax: 102 * time.Minute, samples: 3},
		"https://b.com|d":                     {deadMin: 7*time.Minute + 9*time.Second, samples: 2},
	}
	got := snapshotCacheObs()
	if len(got) != 3 {
		t.Fatalf("应有 3 行，实际 %+v", got)
	}
	// 排序：a.com < api.kimi.com < b.com
	want := []cacheObsRow{
		{URL: "a.com", Model: "m", Alive: "102:00", Dead: "", Samples: 3},
		{URL: "api.kimi.com/coding", Model: "k3-256k", Alive: "23:30", Dead: "41:05", Samples: 12},
		{URL: "b.com", Model: "d", Alive: "", Dead: "07:09", Samples: 2},
	}
	for i, w := range want {
		if got[i] != w {
			t.Errorf("行 %d = %+v，期望 %+v", i, got[i], w)
		}
	}

	// 结论形成列：changedAt 至今的时长（m:ss）；零值（尚无结论）留空
	cacheObsMap["https://b.com|d"].changedAt = time.Now().Add(-90 * time.Second)
	got = snapshotCacheObs()
	if got[2].Age != "1:30" || got[0].Age != "" || got[1].Age != "" {
		t.Errorf("结论形成列：b.com 应 1:30、无 changedAt 的两行应空，实际 %+v", got)
	}
}
