// lang_test.go — UI language: program-settings.txt persistence, system-language detection fallback, startup migration, web switching writes program settings.
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

// TestLoadConfigUILangIgnored: the UI language has moved out of config files: Config no longer has a ui_lang field;
// a leftover ui_lang in a config (including legacy values that used to be rejected) is ignored as an unknown field; loading doesn't fail.
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

// TestApplyUILangFallback: when the config doesn't specify a language, fall back to system detection (the result is always zh or en).
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

	// Switch to English → ui-lang=en appears in program-settings.txt, the effective language becomes en, the config file untouched
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

	// Empty string → key deleted, falling back to the system language; the config file still untouched
	if w := post(`{"lang":""}`); w.Code != http.StatusOK {
		t.Fatalf("POST 空 状态码=%d: %s", w.Code, w.Body.String())
	}
	if got := readProgramSetting("ui-lang"); got != "" {
		t.Errorf("传空串后 ui-lang 应被删除, got %q", got)
	}
	if data, _ := os.ReadFile(path); string(data) != orig {
		t.Errorf("配置文件被改动:\n%q", data)
	}

	// Illegal value → 400, program-settings.txt untouched
	before, _ := os.ReadFile(programSettingsPath())
	if w := post(`{"lang":"fr"}`); w.Code != http.StatusBadRequest {
		t.Errorf("POST fr 状态码=%d, want 400", w.Code)
	}
	after, _ := os.ReadFile(programSettingsPath())
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

// TestSetTopLevelJSONValue locks text-level top-level key editing: set/delete/append touch only the target key;
// every other field's content, order, and formatting (indentation, newline style) is preserved byte for byte; the input is not modified in place.
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

// TestLogViewerHandlerZHDocBody: in the Chinese UI the doc popup body must be replaced by logViewerDocZH,
// with no __DOC_BODY__ placeholder left (regression of 00c08ab: the zh branch missed the replacement).
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

	// An existing value in program-settings.txt wins: even if the config still carries ui_lang it neither overrides the program setting nor gets modified
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

// TestProgramSettingsRoundTrip program-settings read/write: set/change/delete keys; comments and other keys' lines
// are preserved verbatim; deleting a nonexistent key doesn't error; LF line endings and a trailing newline throughout.
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
	// Manually add a comment line and another key, verifying set/delete don't break them
	data, _ := os.ReadFile(programSettingsPath())
	text := "# 程序设置（与路由配置分离）\n" + string(data) + "other=1\n"
	if err := os.WriteFile(programSettingsPath(), []byte(text), 0644); err != nil {
		t.Fatal(err)
	}
	if err := writeProgramSetting("ui-lang", "en"); err != nil { // Change a value
		t.Fatal(err)
	}
	want := "# 程序设置（与路由配置分离）\nui-lang=en\nother=1\n"
	if data, _ := os.ReadFile(programSettingsPath()); string(data) != want {
		t.Errorf("改值后应为:\n%q\ngot:\n%q", want, data)
	}
	if err := writeProgramSetting("ui-lang", ""); err != nil { // Delete a key
		t.Fatal(err)
	}
	want = "# 程序设置（与路由配置分离）\nother=1\n"
	if data, _ := os.ReadFile(programSettingsPath()); string(data) != want {
		t.Errorf("删键后应为:\n%q\ngot:\n%q", want, data)
	}
	if err := writeProgramSetting("no-such-key", ""); err != nil { // Delete a nonexistent key
		t.Errorf("删不存在的键不应报错: %v", err)
	}
	if got := readProgramSetting("other"); got != "1" {
		t.Errorf("readProgramSetting(other)=%q, want 1", got)
	}
}
