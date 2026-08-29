package main

// codex-setup.ps1 既作为独立脚本发布，也被内嵌（codexSetupPS1）经 /__codexsetup 提供给
// 网页控制台实时生成：页面 JS 只做两处单行替换（$BAKED_BASE_URL / $BAKED_MODEL 锚点），
// 锚点格式若被改动，网页生成会静默失效——本测试锁定锚点唯一性与 BOM。

import (
	"bytes"
	"strings"
	"testing"
)

func TestCodexSetupTemplateMarkers(t *testing.T) {
	// JS 的 String.replace 只替换第一处，锚点必须各出现且仅出现一次。
	for _, anchor := range []string{"$BAKED_BASE_URL = ''", "$BAKED_MODEL    = ''", "$BAKED_CATALOG  = ''"} {
		n := bytes.Count(codexSetupPS1, []byte(anchor))
		if n != 1 {
			t.Errorf("锚点 %q 出现 %d 次（应为 1，网页生成依赖它做单行替换）", anchor, n)
		}
	}
	// 还原指令哨兵：网页「还原默认配置」选项烤进 BAKED_MODEL。
	if !strings.Contains(string(codexSetupPS1), "'__restore__'") {
		t.Error("脚本缺少 __restore__ 还原哨兵")
	}
	// PS 5.1 把无 BOM 的 .ps1 按 ANSI(GBK) 读取，中文 UI 会乱码——文件必须带 BOM。
	if !bytes.HasPrefix(codexSetupPS1, []byte{0xEF, 0xBB, 0xBF}) {
		t.Error("codex-setup.ps1 缺 UTF-8 BOM（PS 5.1 下中文会乱码）")
	}
	// 脚本要求代理Responses监听口默认值与配置模板演示值一致，防止两处漂移。
	if !strings.Contains(string(codexSetupPS1), "$DEFAULT_BASE_URL = 'http://127.0.0.1:8081/v1'") {
		t.Error("脚本默认 base_url 不是 http://127.0.0.1:8081/v1")
	}
}
