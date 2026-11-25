package logview

import (
	"proxy429/internal/config"
	"proxy429/internal/platform"

	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestUILangHandlerWritesProgramSettings: web language switching: POST writes program-settings.txt
// (in the config directory; program settings are separate from routing config) and takes effect hot; the config file is not touched byte-wise
// (auto-reformatting is eradicated); an empty string = follow the system (the key is deleted). An illegal value → 400 and program settings untouched.
func TestUILangHandlerWritesProgramSettings(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	orig := "{\n  \"upstream\": \"http://x\",\n  \"max_retries\": 3\n}\n"
	if err := os.WriteFile(path, []byte(orig), 0644); err != nil {
		t.Fatal(err)
	}
	oldPath := config.CurrentConfigPath()
	config.SetCurrentPath(path)
	defer config.SetCurrentPath(oldPath)
	oldLang := platform.CurrentUILang()
	defer platform.ApplyUILang(oldLang)

	post := func(body string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(http.MethodPost, "/__uilang", strings.NewReader(body))
		r.RemoteAddr = "127.0.0.1:12345"
		w := httptest.NewRecorder()
		uiLangHandler(w, r)
		return w
	}

	// Switch to English → ui-lang=en appears in program-settings.txt, the effective language becomes en, the config file untouched
	if w := post(`{"lang":"en"}`); w.Code != http.StatusOK {
		t.Fatalf("POST en 状态码=%d: %s", w.Code, w.Body.String())
	}
	if got := config.ReadProgramSetting("ui-lang"); got != "en" {
		t.Errorf("program-settings ui-lang=%q, want en", got)
	}
	if data, _ := os.ReadFile(path); string(data) != orig {
		t.Errorf("配置文件被改动:\n%q", data)
	}
	if got := platform.CurrentUILang(); got != "en" {
		t.Errorf("切换后 currentUILang=%q, want en", got)
	}

	// Empty string → key deleted, falling back to the system language; the config file still untouched
	if w := post(`{"lang":""}`); w.Code != http.StatusOK {
		t.Fatalf("POST 空 状态码=%d: %s", w.Code, w.Body.String())
	}
	if got := config.ReadProgramSetting("ui-lang"); got != "" {
		t.Errorf("传空串后 ui-lang 应被删除, got %q", got)
	}
	if data, _ := os.ReadFile(path); string(data) != orig {
		t.Errorf("配置文件被改动:\n%q", data)
	}

	// Illegal value → 400, program-settings.txt untouched
	before, _ := os.ReadFile(config.ProgramSettingsPath())
	if w := post(`{"lang":"fr"}`); w.Code != http.StatusBadRequest {
		t.Errorf("POST fr 状态码=%d, want 400", w.Code)
	}
	after, _ := os.ReadFile(config.ProgramSettingsPath())
	if string(before) != string(after) {
		t.Errorf("非法值不应改动 program-settings.txt")
	}
}

// TestRenderLogViewerEN English rendering: key static copy is replaced, the lang attribute becomes en, and the Chinese master page retains no leftover markers.
func TestRenderLogViewerEN(t *testing.T) {
	en := renderLogViewerEN(logViewerHTML)
	for _, want := range []string{
		`<html lang="en">`,
		`<title>Proxy429 Console</title>`,
		`data-tab="status">Status</div>`,
		`data-tab="logs">Logs</div>`,
		`data-tab="config">Config</div>`,
		`>Docs</button>`,
		`>Language`,
	} {
		if !strings.Contains(en, want) {
			t.Errorf("英文页缺少 %q", want)
		}
	}
	if strings.Contains(en, `<html lang="zh">`) {
		t.Errorf("英文页不应残留 lang=zh")
	}
	// The Chinese master page is unaffected
	if !strings.Contains(logViewerHTML, `<html lang="zh">`) {
		t.Errorf("中文母版 lang=zh 被改动")
	}
}

// scriptRanges returns the [start,end) byte ranges of every <script> body in page.
func scriptRanges(page string) [][2]int {
	var out [][2]int
	for off := 0; ; {
		i := strings.Index(page[off:], "<script>")
		if i < 0 {
			return out
		}
		body := off + i + len("<script>")
		j := strings.Index(page[body:], "</script>")
		if j < 0 {
			return out
		}
		out = append(out, [2]int{body, body + j})
		off = body + j + len("</script>")
	}
}

// TestRenderLogViewerENJSQuoteSafety: an enHTMLRepl value whose key sits inside a <script> block is
// injected into inline JS. An unpaired ASCII single quote there (an English apostrophe like
// "stream's") terminates the enclosing single-quoted literal and silently kills the whole script
// block: the page then shows nothing and every text grep still passes. HTML-context values may use
// apostrophes freely, so only script-block keys are checked.
func TestRenderLogViewerENJSQuoteSafety(t *testing.T) {
	ranges := scriptRanges(logViewerHTML)
	inScript := func(pos int) bool {
		for _, r := range ranges {
			if pos >= r[0] && pos < r[1] {
				return true
			}
		}
		return false
	}
	for _, pair := range enHTMLRepl {
		key, val := pair[0], pair[1]
		js := false
		for off := 0; ; {
			i := strings.Index(logViewerHTML[off:], key)
			if i < 0 {
				break
			}
			if inScript(off + i) {
				js = true
				break
			}
			off += i + 1
		}
		if !js {
			continue
		}
		if kn, vn := strings.Count(key, "'"), strings.Count(val, "'"); kn != vn {
			t.Errorf("enHTMLRepl 替换值会注入内联 JS，单引号个数必须与原文一致（%d -> %d）: %q -> %q", kn, vn, key, val)
		}
	}
}

// TestRenderLogViewerENJSSyntax parses every script block of the rendered English page with
// node --check when node is on PATH (GitHub runners ship it); without node the quote-safety test
// above is the backstop.
func TestRenderLogViewerENJSSyntax(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not on PATH; TestRenderLogViewerENJSQuoteSafety is the backstop")
	}
	page := renderLogViewerEN(logViewerHTML)
	dir := t.TempDir()
	for i, r := range scriptRanges(page) {
		f := filepath.Join(dir, fmt.Sprintf("script%d.js", i))
		if err := os.WriteFile(f, []byte(page[r[0]:r[1]]), 0o644); err != nil {
			t.Fatal(err)
		}
		if out, err := exec.Command(node, "--check", f).CombinedOutput(); err != nil {
			t.Errorf("英文页 script 块 %d 语法错误: %v\n%s", i, err, out)
		}
	}
}

// TestLogViewerVersionBadge: the console header carries a version badge next to the language switch —
// beta builds show "beta <hash>-<HHMM>", release builds "release vX.Y.Z.N" (the stamp comes from build.sh
// via -ldflags). The __VERSION__ placeholder must always be substituted in the served page.
func TestLogViewerVersionBadge(t *testing.T) {
	old := platform.CurrentUILang()
	platform.ApplyUILang("zh")
	defer platform.ApplyUILang(old)
	oldV := deps.Version
	defer func() { deps.Version = oldV }()
	for _, v := range []string{"beta cd66927-2100", "release v2026.10.12.1"} {
		deps.Version = v
		r := httptest.NewRequest(http.MethodGet, "/__logs", nil)
		r.RemoteAddr = "127.0.0.1:12345" // isLocalRequest gate
		w := httptest.NewRecorder()
		logViewerHandler(w, r)
		body := w.Body.String()
		if !strings.Contains(body, `id="verBox"`) {
			t.Fatalf("页面缺少版本徽章 verBox")
		}
		if !strings.Contains(body, ">"+v+"<") {
			t.Errorf("版本徽章应原样显示 %q", v)
		}
		if strings.Contains(body, "__VERSION__") {
			t.Errorf("__VERSION__ 占位符未被替换")
		}
	}
}

// TestRenderLogViewerENNoChinese is the leak-proof backstop: no Chinese characters may remain in the English-rendered page
// (except JS comments — // line comments are stripped before the check). Adding Chinese copy without
// an English counterpart makes this test fail immediately.
func TestRenderLogViewerENNoChinese(t *testing.T) {
	en := renderLogViewerEN(logViewerHTML)
	// The I18N table in the page-footer i18n script deliberately uses Chinese as keys (for translating browser dialogs),
	// so the whole script block is exempt from the check
	if i := strings.Index(en, "// ---- 界面语言（i18n）----"); i >= 0 {
		if j := strings.Index(en[i:], "</script>"); j >= 0 {
			en = en[:i] + en[i+j:]
		}
	}
	// The thinkEn function body deliberately contains Chinese literals (matching the server-pushed runtime values 「关」「开 N」);
	// it's the translation logic itself, not copy to be translated, so the whole block is exempt
	if i := strings.Index(en, "function thinkEn("); i >= 0 {
		if j := strings.Index(en[i:], "\n}"); j >= 0 {
			en = en[:i] + en[i+j+2:]
		}
	}
	var b strings.Builder
	for _, line := range strings.Split(en, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "//") {
			continue
		}
		// Text after an inline // is also a comment (JS line comment); only the code part before it is checked
		if idx := strings.Index(line, "//"); idx >= 0 {
			line = line[:idx]
		}
		b.WriteString(line)
		b.WriteByte('\n')
	}
	body := b.String()
	// The language switcher's option labels are written in their native tongues by convention (中文/English); exempt
	body = strings.ReplaceAll(body, `<option value="zh">中文</option>`, `<option value="zh"></option>`)
	// Strip CSS block comments (/* ... */, the only block-comment kind in the page)
	for {
		i := strings.Index(body, "/*")
		if i < 0 {
			break
		}
		j := strings.Index(body[i:], "*/")
		if j < 0 {
			break
		}
		body = body[:i] + body[i+j+2:]
	}
	for _, r := range body {
		if r >= '\u4e00' && r <= '\u9fff' {
			idx := strings.IndexRune(body, r)
			lo := idx - 60
			if lo < 0 {
				lo = 0
			}
			hi := idx + 60
			if hi > len(body) {
				hi = len(body)
			}
			t.Fatalf("英文页残留中文 %q（上下文: %q）", string(r), body[lo:hi])
		}
	}
}

// TestLogViewerHandlerZHDocBody: in the Chinese UI the docs-browser body must be replaced by logViewerDocZH,
// with no __DOC_BODY__ placeholder left (regression of 00c08ab: the zh branch missed the replacement).
func TestLogViewerHandlerZHDocBody(t *testing.T) {
	old := platform.CurrentUILang()
	platform.ApplyUILang("zh")
	defer platform.ApplyUILang(old)
	r := httptest.NewRequest(http.MethodGet, "/__logs", nil)
	r.RemoteAddr = "127.0.0.1:12345"
	w := httptest.NewRecorder()
	logViewerHandler(w, r)
	body := w.Body.String()
	if strings.Contains(body, "__DOC_BODY__") {
		t.Errorf("中文页残留 __DOC_BODY__ 占位")
	}
	if !strings.Contains(body, `<section class="docSec" id="doc-quickstart" data-group="开始使用">`) {
		t.Errorf("中文页缺少文档正文")
	}
}

// TestLogViewerTreeFallsBackToOnscreen pins the interactive JSON tree's data-source contract: when the fetched
// full copy cannot be parsed/assembled (e.g. only a tail was recorded because the toggle was opened mid-stream),
// the tree must retry with the on-screen content before giving up — anything on screen stays interactively viewable.
func TestLogViewerTreeFallsBackToOnscreen(t *testing.T) {
	if !strings.Contains(logViewerHTML, "tryTreeVal(lastRaw)") {
		t.Errorf("交互树缺少「完整副本整理不出时退回屏上内容再试」的回退（tryTreeVal(lastRaw)）")
	}
}

// TestLogViewerTreeExpandsFirstLevel pins the tree default-expansion contract: jsonTreeRoot triggers the
// root head click right after building, so the first level renders expanded while child containers stay
// collapsed summaries (deeper levels still build lazily on click). This is what the two JSON-format view
// states (request body, assembled non-stream output) land on by default.
func TestLogViewerTreeExpandsFirstLevel(t *testing.T) {
	if !strings.Contains(logViewerHTML, "if(head) head.onclick(); // 第一层默认展开") {
		t.Errorf("jsonTreeRoot 缺少「第一层默认展开」（构建后触发根头点击，子容器仍折叠懒展开）")
	}
}

// TestLogViewerUpstreamByteCards pins the status-card contract: the old bytes-out/rate pair is replaced
// by upstream-direction byte counters — bytes sent upstream (bytesUp) and bytes returned from upstream
// (bytesDown), both counted at the shared client's transport layer.
func TestLogViewerUpstreamByteCards(t *testing.T) {
	for _, want := range []string{
		`card('发给上游', fmtBytes(d.bytesUp))`,
		`card('上游返回', fmtBytes(d.bytesDown))`,
	} {
		if !strings.Contains(logViewerHTML, want) {
			t.Errorf("状态卡片缺少 %q", want)
		}
	}
	for _, gone := range []string{`card('流出'`, `card('速率'`} {
		if strings.Contains(logViewerHTML, gone) {
			t.Errorf("旧卡片应已移除: %q", gone)
		}
	}
}

// TestLogViewerInflightCardMerged pins the merged in-flight card: active and waiting share one card
// (title 活跃/等待, value "active / waiting") instead of two separate cards, to save card-row space.
func TestLogViewerInflightCardMerged(t *testing.T) {
	want := `card('活跃/等待', d.active + ' / ' + d.waiting)`
	if !strings.Contains(logViewerHTML, want) {
		t.Errorf("状态卡片缺少合并卡 %q", want)
	}
	for _, gone := range []string{`card('活跃', d.active)`, `card('等待', d.waiting)`} {
		if strings.Contains(logViewerHTML, gone) {
			t.Errorf("旧的分开卡片应已移除: %q", gone)
		}
	}
}

// TestLogViewerInputCardUncached pins the status input card's scope: the card counts only fresh input
// tokens that missed the cache, so the label and hover title spell out the total-input formula — this
// is what keeps it from being mistaken for a smaller version of the per-stream total-input column
// (whose values also include cache read + cache write).
func TestLogViewerInputCardUncached(t *testing.T) {
	want := `card('输入（未命中）', fmtNum(d.inputTokens), '未命中缓存的新鲜输入 token（中断的流不计入）；总输入 = 未命中 + 缓存读取 + 缓存写入，与各流 token 列「入」同构成')`
	if !strings.Contains(logViewerHTML, want) {
		t.Errorf("状态卡片缺少带未命中说明的输入卡 %q", want)
	}
	if strings.Contains(logViewerHTML, `card('输入', fmtNum(d.inputTokens))`) {
		t.Errorf("旧的裸「输入」卡应已移除（会与单流总输入口径混淆）")
	}
	if !strings.Contains(logViewerHTML, `' title="'+escAttr(t)+'"'`) {
		t.Errorf("card() 的 title 必须经 escAttr 注入（裸引号会破坏属性）")
	}
	en := renderLogViewerEN(logViewerHTML)
	if !strings.Contains(en, `card('Input (uncached)'`) {
		t.Errorf("英文页缺少 Input (uncached) 卡")
	}
}

// TestLogViewerHeatmap pins the activity-heatmap contract: an 活跃度 button on the status row opens a
// GitHub-style mosaic modal (32 columns x 6 rows, one cell per 15 minutes, last 48h) with a three-way
// coloring-metric selector (requests / input+output tokens / output tokens), fed by the heatBase/heat
// fields of /__logs/data and rendered client-side.
func TestLogViewerHeatmap(t *testing.T) {
	for _, want := range []string{
		`id="heatModal"`,
		`>活跃度</button>`,
		`function heatHTML()`,
		`latestHeat = d.heat`,
		`heatBase`,
		`function heatSetMetric(`,
		`>请求次数</button>`,
		`>输入+输出</button>`,
		`>输出</button>`,
		`>活跃度</div>`,
		`id="heatMetricRow"`,
		`('+'+(c/4)+'h')`,                          // tick labels use h in both languages; 时 wrapped mid-label
		`text-align:center;white-space:nowrap`,     // tick labels must not wrap inside their 12px cell
		`nowSlot-191`,                              // rolling 48h window: oldest cell = now-191 slots, newest = now
		`'-48h','-40h','-32h','-24h','-16h','-8h'`, // row labels are hours-ago, language-neutral
	} {
		if !strings.Contains(logViewerHTML, want) {
			t.Errorf("活跃度热图缺少 %q", want)
		}
	}
	// The coloring metric defaults to output tokens: the JS var defaults to 2 and the active
	// border sits on 输出 (not 请求次数).
	if !strings.Contains(logViewerHTML, "var heatMetric = 2;") {
		t.Errorf("活跃度默认口径应为输出（缺 var heatMetric = 2;）")
	}
	if !strings.Contains(logViewerHTML, `<button id="heatM2" class="ghost" style="margin-left:4px;padding:1px 8px;font-size:12px;border-color:#39d353"`) {
		t.Errorf("输出按钮应带默认高亮边框")
	}
	if strings.Contains(logViewerHTML, `<button id="heatM0" class="ghost" style="padding:1px 8px;font-size:12px;border-color:#39d353"`) {
		t.Errorf("请求次数按钮不应再带默认高亮边框")
	}
	// The window must roll continuously (one cell out per 15 minutes), never flip a whole day at midnight.
	if strings.Contains(logViewerHTML, "ydaySlot") {
		t.Errorf("热图窗口应按当前时刻滚动，不应再按日历日对齐（发现 ydaySlot）")
	}
	// 活跃度 must not sit next to 清空统计 (misclick hazard): it lives at the far end of the toolbar row.
	iReset := strings.Index(logViewerHTML, `id="resetStatsBtn"`)
	iFull := strings.Index(logViewerHTML, `id="fullStoreChk"`)
	iHeat := strings.Index(logViewerHTML, `id="heatBtn"`)
	if !(iReset >= 0 && iFull > iReset && iHeat > iFull) {
		t.Errorf("活跃度按钮应排在工具行末尾（resetStatsBtn < fullStoreChk < heatBtn），实际 %d/%d/%d", iReset, iFull, iHeat)
	}
}
