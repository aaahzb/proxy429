package main

// codex-setup.ps1 / codex-setup.sh are published both as standalone scripts and embedded (codexSetupPS1/codexSetupSH),
// served via /__codexsetup.ps1 and /__codexsetup.sh: the server bakes BAKED anchors per the query parameters (model/base/catalog)
// and hands the result down; the config page gives the user only a one-line irm|iex / curl|bash pull command
// (same format as DeepSeek's docs). If the anchor format is ever changed, the server-side baking silently breaks —
// this test locks anchor uniqueness, key script markers, and the bake/validation behavior.

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestCodexSetupPS1Markers locks the Windows template's anchor uniqueness and key markers.
func TestCodexSetupPS1Markers(t *testing.T) {
	// The server's bytes.Replace only replaces the first occurrence; each anchor must appear exactly once.
	for _, anchor := range codexPS1Anchors {
		if n := bytes.Count(codexSetupPS1, []byte(anchor)); n != 1 {
			t.Errorf("锚点 %q 出现 %d 次（应为 1，服务端烤制依赖它做单行替换）", anchor, n)
		}
	}
	// Restore-instruction sentinel: the web's 「还原默认配置」 option is baked into BAKED_MODEL.
	if !bytes.Contains(codexSetupPS1, []byte("'__restore__'")) {
		t.Error("脚本缺少 __restore__ 还原哨兵")
	}
	// Scripts must be pure ASCII (user requirement: Chinese garbles/misbehaves on some systems) — no BOM, no byte >= 0x80.
	assertPureASCII(t, "codex-setup.ps1", codexSetupPS1)
	// The script requires the proxy Responses listener's default value to match the config template's demo value, guarding against drift between the two.
	if !bytes.Contains(codexSetupPS1, []byte("$DEFAULT_BASE_URL = 'http://127.0.0.1:8081/v1'")) {
		t.Error("脚本默认 base_url 不是 http://127.0.0.1:8081/v1")
	}
	// System-proxy health-check block: defined once + invoked once on the install path (auto-repair of the invisible-503 pitfall; do not delete).
	if n := bytes.Count(codexSetupPS1, []byte("Ensure-NoProxyForLoopback")); n != 2 {
		t.Errorf("Ensure-NoProxyForLoopback 出现 %d 次（应为 2：定义+安装调用）", n)
	}
	// The English name must appear twice: once for the fresh-install provider block append + once for the quick model-switch path sync
	// (configs installed by the old Chinese script are upgraded to the English name via the latter).
	if n := bytes.Count(codexSetupPS1, []byte("Proxy429 Local Proxy")); n != 2 {
		t.Errorf("Proxy429 Local Proxy 出现 %d 次（应为 2：install + switch 路径同步）", n)
	}
	if !bytes.Contains(codexSetupPS1, []byte("PROXY429_SKIP_PROXY_FIX")) {
		t.Error("脚本缺少 PROXY429_SKIP_PROXY_FIX 测试逃生门")
	}
}

// TestCodexSetupSHMarkers locks the macOS/Linux template's anchor uniqueness and key markers (aligned with ps1).
func TestCodexSetupSHMarkers(t *testing.T) {
	for _, anchor := range codexSHAnchors {
		if n := bytes.Count(codexSetupSH, []byte(anchor)); n != 1 {
			t.Errorf("锚点 %q 出现 %d 次（应为 1，服务端烤制依赖它做单行替换）", anchor, n)
		}
	}
	// Scripts must be pure ASCII (user requirement: Chinese garbles/misbehaves on some systems) — no BOM, no byte >= 0x80.
	assertPureASCII(t, "codex-setup.sh", codexSetupSH)
	// The English name must appear twice: once for the fresh-install provider block append + once for the quick model-switch path sync
	// (configs installed by the old Chinese script are upgraded to the English name via the latter).
	if n := bytes.Count(codexSetupSH, []byte("Proxy429 Local Proxy")); n != 2 {
		t.Errorf("Proxy429 Local Proxy 出现 %d 次（应为 2：install + switch 路径同步）", n)
	}
	if !bytes.HasPrefix(codexSetupSH, []byte("#!")) {
		t.Error("codex-setup.sh 缺 shebang")
	}
	for _, marker := range []string{
		"backup-proxy429",                // Backup directory name (same as ps1; both restore and health-check recognize it)
		"proxy429-models.json",           // Model-catalog file name (written into model_catalog_json)
		"'__restore__'",                  // Restore sentinel
		"PROXY429_SKIP_PROXY_FIX",        // Proxy health-check escape hatch
		"You are Codex, a coding agent.", // Catalog entry base_instructions (same as cc-switch)
	} {
		if !bytes.Contains(codexSetupSH, []byte(marker)) {
			t.Errorf("codex-setup.sh 缺少关键标记 %q", marker)
		}
	}
}

// assertPureASCII asserts the script has no BOM and all bytes < 0x80 (Chinese garbles/misbehaves on some systems).
// name is used to locate failures in error messages; b is the script content.
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

// serveCodexScript runs the handler once with a localhost RemoteAddr and returns the status code and response body.
// target: the request path with query. Returns the status code and response body bytes.
func serveCodexScript(t *testing.T, h http.HandlerFunc, target string) (int, []byte) {
	t.Helper()
	r := httptest.NewRequest(http.MethodGet, target, nil)
	r.RemoteAddr = "127.0.0.1:43210" // httptest defaults to 192.0.2.1, which isLocalRequest rejects
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

// TestCodexScriptHandlerBake verifies the five-parameter bake result and the byte-level delivery contract (ps1 without BOM, sh without CR).
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

	// Without ctx/compact the anchors stay empty strings and the script's built-in defaults (262144/95) apply.
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

// TestCodexScriptHandlerValidation verifies the query whitelist validation and the __restore__ sentinel pass-through.
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
	// The restore sentinel is a legal model value and is baked into BAKED_MODEL verbatim.
	want := "BAKED_MODEL='__restore__'"
	code, body := serveCodexScript(t, codexScriptHandler(codexSetupSH, codexSHAnchors, true), "/__codexsetup.sh?model=__restore__")
	if code != http.StatusOK {
		t.Fatalf("model=__restore__ 状态码 = %d（应为 200）", code)
	}
	if !bytes.Contains(body, []byte(want)) {
		t.Errorf("model=__restore__ 烤制结果缺少 %q", want)
	}
}

// TestCodexScriptHandlerLocalOnly verifies the script endpoints are localhost-only.
func TestCodexScriptHandlerLocalOnly(t *testing.T) {
	h := codexScriptHandler(codexSetupPS1, codexPS1Anchors, false)
	rec := httptest.NewRecorder()
	// httptest.NewRequest defaults to RemoteAddr=192.0.2.1:1234 (not localhost).
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/__codexsetup.ps1", nil))
	if rec.Code != http.StatusForbidden {
		t.Errorf("非本机访问状态码 = %d（应为 403）", rec.Code)
	}
}
