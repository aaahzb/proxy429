//go:build !windows && !darwin

package main

import (
	"os"
	"strings"
)

// detectSystemLang checks LC_ALL / LC_MESSAGES / LANG in order (e.g. zh_CN.UTF-8):
// a zh prefix means Chinese, anything else English; unset falls back to English.
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
