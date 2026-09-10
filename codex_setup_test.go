package main

// codex-setup.ps1 / codex-setup.sh 既作为独立脚本发布，也被内嵌（codexSetupPS1/codexSetupSH）
// 经 /__codexsetup.ps1 与 /__codexsetup.sh 提供：服务端按 query 参数（model/base/catalog）
// 烤制 BAKED 锚点后下发，配置页给用户的只是一行 irm|iex / curl|bash 拉取命令
// （DeepSeek 文档同款格式）。锚点格式若被改动，服务端烤制会静默失效——
// 本测试锁定锚点唯一性、脚本关键标记与烤制/校验行为。

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestCodexSetupPS1Markers 锁定 Windows 版模板的锚点唯一性与关键标记。
func TestCodexSetupPS1Markers(t *testing.T) {
	// 服务端 bytes.Replace 只替换第一处，锚点必须各出现且仅出现一次。
	for _, anchor := range codexPS1Anchors {
		if n := bytes.Count(codexSetupPS1, []byte(anchor)); n != 1 {
			t.Errorf("锚点 %q 出现 %d 次（应为 1，服务端烤制依赖它做单行替换）", anchor, n)
		}
	}
	// 还原指令哨兵：网页「还原默认配置」选项烤进 BAKED_MODEL。
	if !bytes.Contains(codexSetupPS1, []byte("'__restore__'")) {
		t.Error("脚本缺少 __restore__ 还原哨兵")
	}
	// 脚本必须纯 ASCII（用户要求：部分系统上中文会乱码/异常）——无 BOM、无 >=0x80 字节。
	assertPureASCII(t, "codex-setup.ps1", codexSetupPS1)
	// 脚本要求代理 Responses 监听口默认值与配置模板演示值一致，防止两处漂移。
	if !bytes.Contains(codexSetupPS1, []byte("$DEFAULT_BASE_URL = 'http://127.0.0.1:8081/v1'")) {
		t.Error("脚本默认 base_url 不是 http://127.0.0.1:8081/v1")
	}
	// 系统代理体检块：定义一次 + 安装路径调用一次（503 隐形坑的自动修复，勿删）。
	if n := bytes.Count(codexSetupPS1, []byte("Ensure-NoProxyForLoopback")); n != 2 {
		t.Errorf("Ensure-NoProxyForLoopback 出现 %d 次（应为 2：定义+安装调用）", n)
	}
	// 英文名必须出现两次：全新安装追加 provider 块一次 + 快速切模型路径同步一次
	// （旧中文脚本装的配置靠后者升级到英文名）。
	if n := bytes.Count(codexSetupPS1, []byte("Proxy429 Local Proxy")); n != 2 {
		t.Errorf("Proxy429 Local Proxy 出现 %d 次（应为 2：install + switch 路径同步）", n)
	}
	if !bytes.Contains(codexSetupPS1, []byte("PROXY429_SKIP_PROXY_FIX")) {
		t.Error("脚本缺少 PROXY429_SKIP_PROXY_FIX 测试逃生门")
	}
}

// TestCodexSetupSHMarkers 锁定 macOS/Linux 版模板的锚点唯一性与关键标记（与 ps1 对齐）。
func TestCodexSetupSHMarkers(t *testing.T) {
	for _, anchor := range codexSHAnchors {
		if n := bytes.Count(codexSetupSH, []byte(anchor)); n != 1 {
			t.Errorf("锚点 %q 出现 %d 次（应为 1，服务端烤制依赖它做单行替换）", anchor, n)
		}
	}
	// 脚本必须纯 ASCII（用户要求：部分系统上中文会乱码/异常）——无 BOM、无 >=0x80 字节。
	assertPureASCII(t, "codex-setup.sh", codexSetupSH)
	// 英文名必须出现两次：全新安装追加 provider 块一次 + 快速切模型路径同步一次
	// （旧中文脚本装的配置靠后者升级到英文名）。
	if n := bytes.Count(codexSetupSH, []byte("Proxy429 Local Proxy")); n != 2 {
		t.Errorf("Proxy429 Local Proxy 出现 %d 次（应为 2：install + switch 路径同步）", n)
	}
	if !bytes.HasPrefix(codexSetupSH, []byte("#!")) {
		t.Error("codex-setup.sh 缺 shebang")
	}
	for _, marker := range []string{
		"backup-proxy429",                // 备份目录名（与 ps1 一致，还原/体检都认它）
		"proxy429-models.json",           // 模型目录文件名（写进 model_catalog_json）
		"'__restore__'",                  // 还原哨兵
		"PROXY429_SKIP_PROXY_FIX",        // 代理体检逃生门
		"You are Codex, a coding agent.", // 目录条目 base_instructions（cc-switch 同款）
	} {
		if !bytes.Contains(codexSetupSH, []byte(marker)) {
			t.Errorf("codex-setup.sh 缺少关键标记 %q", marker)
		}
	}
}

// assertPureASCII 断言脚本无 BOM 且全部字节 < 0x80（中文在部分系统上会乱码/异常）。
// 参数 name 用于报错定位，b 为脚本内容。
func assertPureASCII(t *testing.T, name string, b []byte) {
	t.Helper()
	if bytes.HasPrefix(b, []byte{0xEF, 0xBB, 0xBF}) {
		t.Errorf("%s 不应带 BOM（iex/bash 都会把 U+FEFF 粘进首条命令名）", name)
	}
	for i, c := range b {
		if c >= 0x80 {
			t.Fatalf("%s 含非 ASCII 字节 0x%02x（偏移 %d）——脚本必须纯英文", name, c, i)
		}
	}
}

// serveCodexScript 以本机 RemoteAddr 跑一遍 handler，返回状态码与响应体。
// 参数 target：带 query 的请求路径。返回状态码与响应体字节。
func serveCodexScript(t *testing.T, h http.HandlerFunc, target string) (int, []byte) {
	t.Helper()
	r := httptest.NewRequest(http.MethodGet, target, nil)
	r.RemoteAddr = "127.0.0.1:43210" // httptest 默认 192.0.2.1，isLocalRequest 会拒
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	res := rec.Result()
	defer res.Body.Close()
	body, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatalf("读响应体失败: %v", err)
	}
	return res.StatusCode, body
}

// TestCodexScriptHandlerBake 验证五参数烤制结果与下发字节级约定（ps1 无 BOM、sh 无 CR）。
func TestCodexScriptHandlerBake(t *testing.T) {
	qs := "?base=http://127.0.0.1:8081/v1&model=claude-fable-5&catalog=gpt-5-codex,claude-fable-5&ctx=131072&compact=90"

	code, body := serveCodexScript(t, codexScriptHandler(codexSetupPS1, codexPS1Anchors, false), "/__codexsetup.ps1"+qs)
	if code != http.StatusOK {
		t.Fatalf("ps1 状态码 = %d（应为 200）", code)
	}
	for _, want := range []string{
		"$BAKED_BASE_URL = 'http://127.0.0.1:8081/v1'",
		"$BAKED_MODEL    = 'claude-fable-5'",
		"$BAKED_CATALOG  = 'gpt-5-codex,claude-fable-5'",
		"$BAKED_CONTEXT_WINDOW  = '131072'",
		"$BAKED_COMPACT_PERCENT = '90'",
	} {
		if !bytes.Contains(body, []byte(want)) {
			t.Errorf("ps1 烤制结果缺少 %q", want)
		}
	}
	if bytes.HasPrefix(body, []byte{0xEF, 0xBB, 0xBF}) {
		t.Error("ps1 响应体不应带 BOM（iex 会把 U+FEFF 粘进首条命令名）")
	}

	code, body = serveCodexScript(t, codexScriptHandler(codexSetupSH, codexSHAnchors, true), "/__codexsetup.sh"+qs)
	if code != http.StatusOK {
		t.Fatalf("sh 状态码 = %d（应为 200）", code)
	}
	for _, want := range []string{
		"BAKED_BASE_URL='http://127.0.0.1:8081/v1'",
		"BAKED_MODEL='claude-fable-5'",
		"BAKED_CATALOG='gpt-5-codex,claude-fable-5'",
		"BAKED_CONTEXT_WINDOW='131072'",
		"BAKED_COMPACT_PERCENT='90'",
	} {
		if !bytes.Contains(body, []byte(want)) {
			t.Errorf("sh 烤制结果缺少 %q", want)
		}
	}
	if bytes.ContainsRune(body, '\r') {
		t.Error("sh 响应体含 CR（服务端应归一为 LF，防 autocrlf 签出烤进 CRLF）")
	}

	// 不带 ctx/compact 时锚点保持空串，脚本内默认值（262144/95）生效。
	code, body = serveCodexScript(t, codexScriptHandler(codexSetupSH, codexSHAnchors, true), "/__codexsetup.sh?model=fable")
	if code != http.StatusOK {
		t.Fatalf("sh 无 ctx/compact 状态码 = %d（应为 200）", code)
	}
	for _, want := range []string{"BAKED_CONTEXT_WINDOW=''", "CONTEXT_WINDOW=262144", "COMPACT_PERCENT=95"} {
		if !bytes.Contains(body, []byte(want)) {
			t.Errorf("sh 缺省下发缺少 %q（缺省时锚点应留空、默认值兜底）", want)
		}
	}
}

// TestCodexScriptHandlerValidation 验证 query 白名单校验与 __restore__ 哨兵放行。
func TestCodexScriptHandlerValidation(t *testing.T) {
	ps1 := codexScriptHandler(codexSetupPS1, codexPS1Anchors, false)
	bad := map[string]string{
		"model 含单引号":     "/__codexsetup.ps1?model=x'y",
		"base 非 http(s)": "/__codexsetup.ps1?base=javascript:x",
		"catalog 空段":     "/__codexsetup.ps1?catalog=a,,b",
		"catalog 含空格":    "/__codexsetup.ps1?catalog=a%20b",
		"catalog 超 64 段": "/__codexsetup.ps1?catalog=" + string(bytes.Repeat([]byte("a,"), 65)) + "a",
		"ctx 非数字":      "/__codexsetup.ps1?ctx=abc",
		"ctx 越界":       "/__codexsetup.ps1?ctx=100",
		"compact 越界":   "/__codexsetup.ps1?compact=100",
		"compact 带引号":  "/__codexsetup.ps1?compact=9'0",
	}
	for name, target := range bad {
		if code, _ := serveCodexScript(t, ps1, target); code != http.StatusBadRequest {
			t.Errorf("%s：状态码 = %d（应为 400）", name, code)
		}
	}
	// 还原哨兵是合法 model 值，且原样烤进 BAKED_MODEL。
	want := "BAKED_MODEL='__restore__'"
	code, body := serveCodexScript(t, codexScriptHandler(codexSetupSH, codexSHAnchors, true), "/__codexsetup.sh?model=__restore__")
	if code != http.StatusOK {
		t.Fatalf("model=__restore__ 状态码 = %d（应为 200）", code)
	}
	if !bytes.Contains(body, []byte(want)) {
		t.Errorf("model=__restore__ 烤制结果缺少 %q", want)
	}
}

// TestCodexScriptHandlerLocalOnly 验证脚本端点限本机访问。
func TestCodexScriptHandlerLocalOnly(t *testing.T) {
	h := codexScriptHandler(codexSetupPS1, codexPS1Anchors, false)
	rec := httptest.NewRecorder()
	// httptest.NewRequest 默认 RemoteAddr=192.0.2.1:1234（非本机）。
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/__codexsetup.ps1", nil))
	if rec.Code != http.StatusForbidden {
		t.Errorf("非本机访问状态码 = %d（应为 403）", rec.Code)
	}
}
