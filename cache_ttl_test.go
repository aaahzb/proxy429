package main

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// TestExtractConvID verifies Anthropic-port session identifier extraction: Claude Code's metadata.user_id is a
// JSON string (its session_id is taken); a bare API caller's free text passes through as-is; no metadata returns empty.
// Includes a case with a giant messages array up front, verifying the scan can skip over big fields to reach metadata.
func TestExtractConvID(t *testing.T) {
	// Giant messages precede metadata, verifying locateTopFields' cross-field locating
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

// TestConvKeyOf verifies the anchor key: no session identifier returns empty (no cache-age display or protection), otherwise convID|convAnchor.
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

// TestAddFinished499Anchor verifies the anchoring rules for 499 streams: a yellow-light (awaiting first byte, stage<3) downstream disconnect
// does not refresh the anchor (the archive's convKey is emptied, showing "-", and the key's anchor stays on the previous same-key stream); a green-light
// (forwarding, stage=3) disconnect refreshes the anchor as usual (this row shows its age normally).
func TestAddFinished499Anchor(t *testing.T) {
	oldFinished, oldCap := finished, finishedCap.Load()
	defer func() {
		finished = oldFinished
		finishedCap.Store(oldCap)
	}()

	mkFlight := func(id uint64, status, stage int32) *flight {
		f := &flight{id: id, convID: "s1", convAnchor: "route:a", status: int(status)}
		f.stage.Store(stage)
		f.start = time.Now() // Protection window = start + fixed 5min; protected within the window
		return f
	}

	// Yellow-light 499: no anchor key after archiving; when cap=1 triggers a trim, the protected row is the old #1 (#2 has no key and doesn't steal the anchor; both rows stay)
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

	// Green-light 499: anchored as usual; at cap=1, #3 is the key's newest row and still inside the protection window, so the old row #1 is evicted
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

// TestRecentFlightsCacheRefreshed verifies the 「已刷新」 frozen display: while a same-key in-flight stream is streaming (stage=3 forwarding),
// that key's newest finished row stops counting up and freezes as [m:ss] (= the old anchor's age at the moment the in-flight stream started, unchanged by polling);
// yellow-light in-flight streams (stage<3) and different-key in-flight streams don't trigger it; real-time counting resumes once the in-flight stream ends.
func TestRecentFlightsCacheRefreshed(t *testing.T) {
	resetStats() // Initialize flights.m and other globals (initialization normally done only in main(); tests must reset explicitly)
	oldFinished := finished
	defer func() { finished = oldFinished }()

	key := "s1|route:a"
	t0 := time.Now()
	finished = []finishedFlight{{id: 1, convKey: key, start: t0.Add(-3 * time.Minute)}} // Cache written 3 minutes ago

	call := func() string {
		req := httptest.NewRequest("GET", "/__recentflights", nil)
		req.RemoteAddr = "127.0.0.1:1" // isLocalRequest requires localhost
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
	register(90, "s1", "route:a", 2, t0) // Yellow-light in-flight (awaiting first byte): does not trigger
	if got := call(); !strings.Contains(got, ":") {
		t.Errorf("黄灯在途流不应触发冻结显示，实际 %q", got)
	}
	register(91, "other", "route:a", 3, t0) // Green light but different key: does not trigger
	if got := call(); !strings.Contains(got, ":") {
		t.Errorf("异键绿灯在途流不应触发冻结显示，实际 %q", got)
	}
	// Same-key green light (forwarding/streaming): frozen display of the old anchor's age at the in-flight stream's start = 3min-40s = "2:20"
	register(92, "s1", "route:a", 3, t0.Add(-40*time.Second))
	if got := call(); got != "[2:20]" {
		t.Errorf("同键绿灯在途流应冻结显示 [2:20]，实际 %q", got)
	}
	flights.unregister(92)
	if got := call(); !strings.Contains(got, ":") {
		t.Errorf("在途流结束后应恢复实时递增，实际 %q", got)
	}
}

// TestTrimFinishedLocked verifies finished-stream trimming: the eviction window is the oldest len-cap rows; within the window,
// rows that are "the newest of their anchor key and still inside the protection window (start + defaultCacheTTL)" are protected from eviction (no compensating extension; row count may exceed cap);
// rows past the window, rows superseded by a newer same-key row, and keyless rows are evicted FIFO as usual.
func TestTrimFinishedLocked(t *testing.T) {
	oldFinished, oldCap := finished, finishedCap.Load()
	defer func() {
		finished = oldFinished
		finishedCap.Store(oldCap)
	}()

	fresh := time.Now()                 // Inside the protection window (start is < 5min ago)
	stale := time.Now().Add(-time.Hour) // Past the protection window
	mk := func(id uint64, key string, start time.Time) finishedFlight {
		return finishedFlight{id: id, convKey: key, start: start}
	}

	t.Run("用户例子：14 行留 10，最旧 1 行窗口内 S2 受保护，留 11 行", func(t *testing.T) {
		// Row 0 (oldest) = S2's newest and inside the window; rows 1..13 = S1 (row 13 is S1's newest)
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
		// The evicted ones should be S1's oldest 3 rows (id 1..3); S1's newest 10 rows (id 4..13) all stay
		for i, ff := range finished[1:] {
			if wantID := uint64(i + 4); ff.id != wantID {
				t.Errorf("finished[%d].id = %d，期望 %d", i+1, ff.id, wantID)
			}
		}
	})

	t.Run("窗口内被同键更新行刷新的行不保护", func(t *testing.T) {
		// Both same-key rows are inside the protection window; the older one (not the newest) is still evicted when inside the eviction window
		finished = []finishedFlight{mk(0, "K|r", fresh), mk(1, "K|r", fresh)}
		finishedCap.Store(1)
		trimFinishedLocked()
		if len(finished) != 1 || finished[0].id != 1 {
			t.Fatalf("应只剩最新行 id=1，实际 %+v", finished)
		}
	})

	t.Run("出保护窗口的行不保护", func(t *testing.T) {
		// Row 0 is K's newest but past the protection window; row 1 has no key. At cap=1, row 0 is inside the eviction window and gets evicted
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
		// Eviction window = rows 0..1: row 0 has no key, row 1 is not K's newest and past the window — both evicted; ids 2 and 3 stay
		if len(finished) != 2 || finished[0].id != 2 || finished[1].id != 3 {
			t.Fatalf("应留 id 2、3，实际 %+v", finished)
		}
	})
}

// TestObsKeyOf verifies the observed cache-lifetime pairing key: an empty convID or upstreamKey on either side returns empty (no observation);
// with both present it joins convID|upstreamKey; maliciously over-long session identifiers are truncated to prevent memory bloat.
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

// TestClassifyCacheObservation verifies the observed cache-lifetime classification: latter hit rate ≥95% → alive observation;
// former had hits + latter hit rate <50% → dead observation (strict zero not required; residual hits on system prompts don't count as alive);
// the middle band (50%~95%), volume <1024, non-positive interval, and a low-hit-rate latter whose former never hit are all not observed.
// The volume check precedes the alive/dead decision (small requests are noisy).
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

// TestRecordCacheObsLocked verifies observation accumulation: alive interval takes max (observed lower bound only rises),
// dead interval takes min (observed upper bound only falls), observation count accumulates; different upstream keys don't interfere.
// On contradiction (upstream TTL changed mid-way / early eviction) the newer observation wins and the refuted side is discarded and re-measured.
func TestRecordCacheObsLocked(t *testing.T) {
	oldMap := cacheObsMap
	defer func() { cacheObsMap = oldMap }()
	cacheObsMap = map[string]*cacheObsEntry{}

	recordCacheObsLocked("u|m", obsAlive, 5*time.Minute)
	if cacheObsMap["u|m"].aliveAt.IsZero() {
		t.Error("首次成界应记下界形成时间 aliveAt")
	}
	stale := time.Now().Add(-time.Hour) // Sentinel: an observation that doesn't change the value must leave the formation time untouched

	cacheObsMap["u|m"].aliveAt = stale
	recordCacheObsLocked("u|m", obsAlive, 3*time.Minute) // A smaller value doesn't raise the lower bound; formation time not reset
	if !cacheObsMap["u|m"].aliveAt.Equal(stale) {
		t.Error("下界数值未变化不应重置形成时间")
	}
	recordCacheObsLocked("u|m", obsAlive, 9*time.Minute) // A larger value raises the lower bound; formation time resets
	if !cacheObsMap["u|m"].aliveAt.After(stale) {
		t.Error("下界抬高应重置形成时间")
	}
	recordCacheObsLocked("u|m", obsDead, 20*time.Minute)
	recordCacheObsLocked("u|m", obsDead, 30*time.Minute) // A larger value doesn't lower the upper bound
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

	// TTL grew mid-way: a new alive observation of 25min crosses the old upper bound of 20min → the upper bound is discarded (its formation time zeroed too), the lower bound rises as usual; the observation count restarts (this one is #1)
	cacheObsMap["u|m"].aliveAt = stale
	recordCacheObsLocked("u|m", obsAlive, 25*time.Minute)
	if e.aliveMax != 25*time.Minute || e.deadMin != 0 {
		t.Errorf("新存活越过旧上界：上界应作废，实际 aliveMax=%v deadMin=%v", e.aliveMax, e.deadMin)
	}
	if !e.deadAt.IsZero() {
		t.Error("上界作废重测时其形成时间应清零（界不存在则形成时间不存在）")
	}
	if e.samples != 1 {
		t.Errorf("交叉作废后 samples = %d，期望 1（归零重计，本次观测为第 1 次）", e.samples)
	}
	if !e.aliveAt.After(stale) {
		t.Error("下界抬高应重置下界形成时间")
	}
	// A dead observation not contradicting the lower bound stands as usual (alive at 25min, dead by 40min; both sides coexist), count accumulates; the lower bound's formation time doesn't move
	cacheObsMap["u|m"].aliveAt = stale
	recordCacheObsLocked("u|m", obsDead, 40*time.Minute)
	if e.aliveMax != 25*time.Minute || e.deadMin != 40*time.Minute {
		t.Errorf("不矛盾的死亡应两侧并存，实际 aliveMax=%v deadMin=%v", e.aliveMax, e.deadMin)
	}
	if e.samples != 2 {
		t.Errorf("不矛盾观测后 samples = %d，期望 2", e.samples)
	}
	if !e.aliveAt.Equal(stale) {
		t.Error("新增死亡观测不应动下界形成时间")
	}
	if e.deadAt.IsZero() {
		t.Error("上界成立应记上界形成时间 deadAt")
	}
	// TTL shrank mid-way (or early eviction): a new dead observation of 10min falls below the old lower bound of 25min → the lower bound is discarded (formation time zeroed), observation count restarts again
	cacheObsMap["u|m"].deadAt = stale
	recordCacheObsLocked("u|m", obsDead, 10*time.Minute)
	if e.aliveMax != 0 || e.deadMin != 10*time.Minute {
		t.Errorf("新死亡跌破旧下界：下界应作废，实际 aliveMax=%v deadMin=%v", e.aliveMax, e.deadMin)
	}
	if !e.aliveAt.IsZero() {
		t.Error("下界作废重测时其形成时间应清零（界不存在则形成时间不存在）")
	}
	if e.samples != 1 {
		t.Errorf("再次交叉作废后 samples = %d，期望 1", e.samples)
	}
	if !e.deadAt.After(stale) {
		t.Error("上界压低应重置上界形成时间")
	}
}

// TestSnapshotCacheObs locks the observed cache-lifetime snapshot: the key splits into URL (scheme stripped) + model, lower/upper bounds format as
// mm:ss ("00:30", ≥1h shows h:mm:ss like "1:42:00", a side without observations stays empty), observation count passes through, sorted by URL+model; an empty table returns nil.
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
	// Sort order: a.com < api.kimi.com < b.com
	want := []cacheObsRow{
		{URL: "a.com", Model: "m", Alive: "1:42:00", Dead: "", Samples: 3},
		{URL: "api.kimi.com/coding", Model: "k3-256k", Alive: "23:30", Dead: "41:05", Samples: 12},
		{URL: "b.com", Model: "d", Alive: "", Dead: "07:09", Samples: 2},
	}
	for i, w := range want {
		if got[i] != w {
			t.Errorf("行 %d = %+v，期望 %+v", i, got[i], w)
		}
	}

	// ≥formed/<formed columns: how long each bound's value has stood (m:ss); when a bound doesn't exist its formation time stays empty too,
	// even if the entry still holds that side's timestamp (defensive: the display rule only looks at whether the bound exists)
	cacheObsMap["https://b.com|d"].deadAt = time.Now().Add(-90 * time.Second)
	cacheObsMap["https://b.com|d"].aliveAt = time.Now().Add(-3 * time.Minute) // No lower bound; this timestamp must not be displayed
	got = snapshotCacheObs()
	if got[2].DeadAge != "1:30" || got[2].AliveAge != "" {
		t.Errorf("b.com 应 <形成 1:30、≥形成空（无下界），实际 %+v", got[2])
	}
	if got[0].AliveAge != "" || got[0].DeadAge != "" || got[1].AliveAge != "" || got[1].DeadAge != "" {
		t.Errorf("无时间戳的两行两列形成时间应全空，实际 %+v", got)
	}
}
