//go:build windows

package main

import (
	"strings"

	"golang.org/x/sys/windows/registry"
)

// detectSystemLang reads the current user's Windows display language (HKCU\Control Panel\International\
// LocaleName, e.g. zh-CN / en-US): a zh prefix means Chinese, anything else English; unreadable falls back to English.
// Note: HKCU is the hive of the user running this process — services/scheduled tasks running
// under another account read that account's locale.
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
