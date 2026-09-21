//go:build darwin

package main

import (
	"os/exec"
	"strings"
)

// detectSystemLang 读 macOS 当前用户的 AppleLocale（如 zh_CN / en_US），
// zh 开头判中文，其余判英文；读不到回退英文。
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
