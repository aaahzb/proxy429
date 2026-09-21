//go:build !windows && !darwin

package main

import (
	"os"
	"strings"
)

// detectSystemLang 依次看 LC_ALL / LC_MESSAGES / LANG 环境变量（如 zh_CN.UTF-8），
// zh 开头判中文，其余判英文；都没有回退英文。
func detectSystemLang() string {
	for _, key := range []string{"LC_ALL", "LC_MESSAGES", "LANG"} {
		if v := strings.TrimSpace(os.Getenv(key)); v != "" {
			if strings.HasPrefix(strings.ToLower(v), "zh") {
				return "zh"
			}
			return "en"
		}
	}
	return "en"
}
