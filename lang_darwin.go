//go:build darwin

package main

import (
	"os/exec"
	"strings"
)

// detectSystemLang reads the current macOS user's AppleLocale (e.g. zh_CN / en_US):
// a zh prefix means Chinese, anything else English; unreadable falls back to English.
func detectSystemLang() string {
	out, err := exec.Command("defaults", "read", "-g", "AppleLocale").Output()
	if err != nil {
		return "en"
	}
	if strings.HasPrefix(strings.ToLower(strings.TrimSpace(string(out))), "zh") {
		return "zh"
	}
	return "en"
}
