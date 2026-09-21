// lang.go — 网页控制台界面语言：默认跟随操作系统语言（探测不到用英文），
// 网页顶栏可手动切换；切换把 ui_lang 写回当前配置文件，之后每次启动继承该选择，
// 直到再次手动改动（改回「跟随系统」则删除该字段恢复自动探测）。
package main

import (
	"sync/atomic"
)

// uiLang 是当前生效的界面语言（"zh"/"en"），仅经 applyUILang 写入、页面 handler 读取。
var uiLang atomic.Value

// applyUILang 应用配置里的语言选择：空值回退到操作系统语言探测。
func applyUILang(cfgLang string) {
	if cfgLang == "zh" || cfgLang == "en" {
		uiLang.Store(cfgLang)
		return
	}
	uiLang.Store(detectSystemLang())
}

// currentUILang 返回当前生效的界面语言（main 启动时必已初始化，兜底英文）。
func currentUILang() string {
	if v := uiLang.Load(); v != nil {
		return v.(string)
	}
	return "en"
}
