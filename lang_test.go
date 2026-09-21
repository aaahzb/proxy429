// lang_test.go — 界面语言：program-settings.txt 存取、系统语言探测回退、启动迁移、网页切换写程序设置。
package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestLoadConfigUILangIgnored 界面语言已迁出配置文件：Config 不再有 ui_lang 字段，
// 配置里残留的 ui_lang（含旧版会报错的非法值）按未知字段忽略，加载不报错。
func TestLoadConfigUILangIgnored(t *testing.T) {
	for _, lang := range []string{"zh", "en", "fr", ""} {
		body := `{"upstream":"http://x","ui_lang":"` + lang + `"}`
		p := filepath.Join(t.TempDir(), "config.json")
		if err := os.WriteFile(p, []byte(body), 0644); err != nil {
			t.Fatal(err)
		}
		if _, err := loadConfig(p); err != nil {
			t.Errorf("ui_lang=%q 应被忽略且加载成功: %v", lang, err)
		}
	}
}

// TestApplyUILangFallback 配置未指定语言时回退到系统探测（结果必为 zh/en 之一）。
func TestApplyUILangFallback(t *testing.T) {
	applyUILang("")
	if got := currentUILang(); got != "zh" && got != "en" {
		t.Errorf("currentUILang=%q, want zh 或 en", got)
	}
	applyUILang("en")
	if got := currentUILang(); got != "en" {
		t.Errorf("applyUILang(en) 后 currentUILang=%q", got)
	}
	applyUILang("zh")
	if got := currentUILang(); got != "zh" {
		t.Errorf("applyUILang(zh) 后 currentUILang=%q", got)
	}
}

// TestUILangHandlerWritesProgramSettings 网页切换语言：POST 写 program-settings.txt
// （配置目录下，程序设置与路由配置分离）并热生效；配置文件逐字节不被改动
// （根除自动重排）；传空串 = 跟随系统（删该键）。非法值 → 400 且程序设置不动。
func TestUILangHandlerWritesProgramSettings(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	orig := "{\n  \"upstream\": \"http://x\",\n  \"max_retries\": 3\n}\n"
	if err := os.WriteFile(path, []byte(orig), 0644); err != nil {
		t.Fatal(err)
	}
	configMu.Lock()
	oldPath := configFilePath
	configFilePath = path
	configMu.Unlock()
	defer func() {
		configMu.Lock()
		configFilePath = oldPath
		configMu.Unlock()
	}()
	oldLang := currentUILang()
	defer applyUILang(oldLang)

	post := func(body string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(http.MethodPost, "/__uilang", strings.NewReader(body))
		r.RemoteAddr = "127.0.0.1:12345"
		w := httptest.NewRecorder()
		uiLangHandler(w, r)
		return w
	}

	// 切英文 → program-settings.txt 出现 ui-lang=en，生效语言变 en，配置文件不动
	if w := post(`{"lang":"en"}`); w.Code != http.StatusOK {
		t.Fatalf("POST en 状态码=%d: %s", w.Code, w.Body.String())
	}
	if got := readProgramSetting("ui-lang"); got != "en" {
		t.Errorf("program-settings ui-lang=%q, want en", got)
	}
	if data, _ := os.ReadFile(path); string(data) != orig {
		t.Errorf("配置文件被改动:\n%q", data)
	}
	if got := currentUILang(); got != "en" {
		t.Errorf("切换后 currentUILang=%q, want en", got)
	}

	// 传空串 → 删键回退系统语言，配置文件仍不动
	if w := post(`{"lang":""}`); w.Code != http.StatusOK {
		t.Fatalf("POST 空 状态码=%d: %s", w.Code, w.Body.String())
	}
	if got := readProgramSetting("ui-lang"); got != "" {
		t.Errorf("传空串后 ui-lang 应被删除, got %q", got)
	}
	if data, _ := os.ReadFile(path); string(data) != orig {
		t.Errorf("配置文件被改动:\n%q", data)
	}

	// 非法值 → 400，program-settings.txt 不被改动
	before, _ := os.ReadFile(programSettingsPath())
	if w := post(`{"lang":"fr"}`); w.Code != http.StatusBadRequest {
		t.Errorf("POST fr 状态码=%d, want 400", w.Code)
	}
	after, _ := os.ReadFile(programSettingsPath())
	if string(before) != string(after) {
		t.Errorf("非法值不应改动 program-settings.txt")
	}
}

// TestRenderLogViewerEN 英文渲染：关键静态文案被替换、lang 属性变 en、中文母版不含残留标记。
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
	// 中文母版不受影响
	if !strings.Contains(logViewerHTML, `<html lang="zh">`) {
		t.Errorf("中文母版 lang=zh 被改动")
	}
}

// TestRenderLogViewerENNoChinese 兜底防漏：英文渲染后的页面里不允许残留任何
// 中文字符（JS 注释除外——渲染前剥掉 // 行注释再查）。新增中文文案没补英文
// 对照时本测试会立刻报出来。
func TestRenderLogViewerENNoChinese(t *testing.T) {
	en := renderLogViewerEN(logViewerHTML)
	// 页尾 i18n 脚本的 I18N 对照表故意以中文为键（翻译浏览器弹窗用），
	// 整段脚本豁免检查
	if i := strings.Index(en, "// ---- 界面语言（i18n）----"); i >= 0 {
		if j := strings.Index(en[i:], "</script>"); j >= 0 {
			en = en[:i] + en[i+j:]
		}
	}
	// thinkEn 函数体故意含中文字面量（匹配服务端下发的「关」「开 N」运行时值），
	// 是翻译逻辑本身而非待翻译文案，整段豁免
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
		// 行内 // 之后也是注释（JS 行注释），只查注释前的代码部分
		if idx := strings.Index(line, "//"); idx >= 0 {
			line = line[:idx]
		}
		b.WriteString(line)
		b.WriteByte('\n')
	}
	body := b.String()
	// 语言切换器的选项标签按惯例用母语书写（中文/English），豁免
	body = strings.ReplaceAll(body, `<option value="zh">中文</option>`, `<option value="zh"></option>`)
	// 剥掉 CSS 块注释（/* ... */，页面里只有 CSS 用这种注释）
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

// TestSetTopLevelJSONValue 锁定文本级顶层键编辑：改值/删除/追加都只动目标键，
// 其余字段的内容、顺序与排版（缩进、换行风格）逐字节保留；入参不被原地修改。
func TestSetTopLevelJSONValue(t *testing.T) {
	cases := []struct {
		name string
		src  string
		key  string
		raw  []byte
		want string
	}{
		{"改值-多行", "{\n  \"a\": 1,\n  \"ui_lang\": \"zh\",\n  \"b\": 2\n}\n", "ui_lang", []byte(`"en"`), "{\n  \"a\": 1,\n  \"ui_lang\": \"en\",\n  \"b\": 2\n}\n"},
		{"改值-紧凑", `{"a":1,"ui_lang":"zh"}`, "ui_lang", []byte(`"en"`), `{"a":1,"ui_lang":"en"}`},
		{"删除-中间键", "{\n  \"a\": 1,\n  \"ui_lang\": \"zh\",\n  \"b\": 2\n}\n", "ui_lang", nil, "{\n  \"a\": 1,\n  \"b\": 2\n}\n"},
		{"删除-首键", "{\n  \"ui_lang\": \"zh\",\n  \"a\": 1\n}\n", "ui_lang", nil, "{\n  \"a\": 1\n}\n"},
		{"删除-尾键紧凑", `{"a":1,"ui_lang":"zh"}`, "ui_lang", nil, `{"a":1}`},
		{"删除-唯一键", `{"ui_lang":"zh"}`, "ui_lang", nil, `{}`},
		{"删除-不存在的键", `{"a":1}`, "ui_lang", nil, `{"a":1}`},
		{"追加-多行", "{\n  \"a\": 1\n}\n", "ui_lang", []byte(`"en"`), "{\n  \"a\": 1,\n  \"ui_lang\": \"en\"\n}\n"},
		{"追加-空对象", `{}`, "ui_lang", []byte(`"en"`), "{\n  \"ui_lang\": \"en\"\n}"},
		{"追加-CRLF", "{\r\n  \"a\": 1\r\n}\r\n", "ui_lang", []byte(`"en"`), "{\r\n  \"a\": 1,\r\n  \"ui_lang\": \"en\"\r\n}\r\n"},
		{"值可以是对象", `{"a":1,"thinking":{"type":"enabled"}}`, "thinking", []byte(`{"type":"disabled"}`), `{"a":1,"thinking":{"type":"disabled"}}`},
		{"不误伤嵌套同名键", `{"ui_lang":"zh","nested":{"ui_lang":"fr"}}`, "ui_lang", []byte(`"en"`), `{"ui_lang":"en","nested":{"ui_lang":"fr"}}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			src := []byte(tc.src)
			before := string(src)
			got, ok := setTopLevelJSONValue(src, tc.key, tc.raw)
			if !ok {
				t.Fatalf("ok=false")
			}
			if string(got) != tc.want {
				t.Errorf("got  %q\nwant %q", got, tc.want)
			}
			var probe any
			if err := json.Unmarshal(got, &probe); err != nil {
				t.Errorf("输出不是合法 JSON: %v", err)
			}
			if string(src) != before {
				t.Errorf("入参被原地修改")
			}
		})
	}
	if _, ok := setTopLevelJSONValue([]byte(`[1,2]`), "ui_lang", []byte(`"en"`)); ok {
		t.Errorf("顶层数组应 ok=false")
	}
}

// TestLogViewerHandlerZHDocBody 中文界面下文档弹窗正文必须被 logViewerDocZH 替换，
// 不得残留 __DOC_BODY__ 占位（00c08ab 的回归：zh 分支漏了替换）。
func TestLogViewerHandlerZHDocBody(t *testing.T) {
	old := currentUILang()
	applyUILang("zh")
	defer applyUILang(old)
	r := httptest.NewRequest(http.MethodGet, "/__logs", nil)
	r.RemoteAddr = "127.0.0.1:12345"
	w := httptest.NewRecorder()
	logViewerHandler(w, r)
	body := w.Body.String()
	if strings.Contains(body, "__DOC_BODY__") {
		t.Errorf("中文页残留 __DOC_BODY__ 占位")
	}
	if !strings.Contains(body, "全局流式化 convertAlltoStream") {
		t.Errorf("中文页缺少文档正文")
	}
}

// TestResolveProgramUILangMigration 启动迁移：program-settings.txt 无 ui-lang 而旧配置
// 文件带 ui_lang 时，迁移到 program-settings.txt、从配置里删掉该键（文本级，其余排版
// 逐字节保留）并立即生效；program-settings.txt 已有值时优先，不再看配置、不再动配置。
func TestResolveProgramUILangMigration(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	orig := "{\n  \"max_retries\": 3,\n  \"upstream\": \"http://x\",\n  \"ui_lang\": \"zh\"\n}\n"
	if err := os.WriteFile(path, []byte(orig), 0644); err != nil {
		t.Fatal(err)
	}
	configMu.Lock()
	oldPath := configFilePath
	configFilePath = path
	configMu.Unlock()
	defer func() {
		configMu.Lock()
		configFilePath = oldPath
		configMu.Unlock()
	}()
	oldLang := currentUILang()
	defer applyUILang(oldLang)

	resolveProgramUILang()
	if got := currentUILang(); got != "zh" {
		t.Errorf("迁移后 currentUILang=%q, want zh", got)
	}
	if got := readProgramSetting("ui-lang"); got != "zh" {
		t.Errorf("program-settings ui-lang=%q, want zh", got)
	}
	want := "{\n  \"max_retries\": 3,\n  \"upstream\": \"http://x\"\n}\n"
	if data, _ := os.ReadFile(path); string(data) != want {
		t.Errorf("迁移后配置应为:\n%q\ngot:\n%q", want, data)
	}

	// program-settings.txt 已有值优先：配置再带 ui_lang 也不覆盖程序设置、不动配置
	if err := os.WriteFile(path, []byte(orig), 0644); err != nil {
		t.Fatal(err)
	}
	if err := writeProgramSetting("ui-lang", "en"); err != nil {
		t.Fatal(err)
	}
	resolveProgramUILang()
	if got := currentUILang(); got != "en" {
		t.Errorf("program-settings 优先: currentUILang=%q, want en", got)
	}
	if data, _ := os.ReadFile(path); string(data) != orig {
		t.Errorf("program-settings 已有值时不应再动配置:\n%q", data)
	}
}

// TestProgramSettingsRoundTrip 程序设置读写：设值/改值/删键，注释与其他键的行
// 原样保留；删除不存在的键不出错；统一 LF 与末尾换行。
func TestProgramSettingsRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	if err := os.WriteFile(path, []byte(`{"upstream":"http://x"}`), 0644); err != nil {
		t.Fatal(err)
	}
	configMu.Lock()
	oldPath := configFilePath
	configFilePath = path
	configMu.Unlock()
	defer func() {
		configMu.Lock()
		configFilePath = oldPath
		configMu.Unlock()
	}()

	if got := readProgramSetting("ui-lang"); got != "" {
		t.Fatalf("初始应无 ui-lang, got %q", got)
	}
	if err := writeProgramSetting("ui-lang", "zh"); err != nil {
		t.Fatal(err)
	}
	// 手工加注释行与另一个键，验证改值/删键都不破坏它们
	data, _ := os.ReadFile(programSettingsPath())
	text := "# 程序设置（与路由配置分离）\n" + string(data) + "other=1\n"
	if err := os.WriteFile(programSettingsPath(), []byte(text), 0644); err != nil {
		t.Fatal(err)
	}
	if err := writeProgramSetting("ui-lang", "en"); err != nil { // 改值
		t.Fatal(err)
	}
	want := "# 程序设置（与路由配置分离）\nui-lang=en\nother=1\n"
	if data, _ := os.ReadFile(programSettingsPath()); string(data) != want {
		t.Errorf("改值后应为:\n%q\ngot:\n%q", want, data)
	}
	if err := writeProgramSetting("ui-lang", ""); err != nil { // 删键
		t.Fatal(err)
	}
	want = "# 程序设置（与路由配置分离）\nother=1\n"
	if data, _ := os.ReadFile(programSettingsPath()); string(data) != want {
		t.Errorf("删键后应为:\n%q\ngot:\n%q", want, data)
	}
	if err := writeProgramSetting("no-such-key", ""); err != nil { // 删不存在的键
		t.Errorf("删不存在的键不应报错: %v", err)
	}
	if got := readProgramSetting("other"); got != "1" {
		t.Errorf("readProgramSetting(other)=%q, want 1", got)
	}
}
