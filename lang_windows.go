//go:build windows

package main

import (
	"strings"

	"golang.org/x/sys/windows/registry"
)

// detectSystemLang 读当前用户的 Windows 显示语言（HKCU\Control Panel\International\
// LocaleName，如 zh-CN / en-US），zh 开头判中文，其余判英文；读不到回退英文。
// 注意：HKCU 是「运行本进程的用户」的 hive——服务/计划任务以其他账户跑时读到的是那个账户的区域设置。
func detectSystemLang() string {
	k, err := registry.OpenKey(registry.CURRENT_USER, `Control Panel\International`, registry.QUERY_VALUE)
	if err != nil {
		return "en"
	}
	defer k.Close()
	locale, _, err := k.GetStringValue("LocaleName")
	if err != nil {
		return "en"
	}
	if strings.HasPrefix(strings.ToLower(locale), "zh") {
		return "zh"
	}
	return "en"
}
