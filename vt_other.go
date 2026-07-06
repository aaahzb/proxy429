//go:build !windows

package main

// enableVT 在非 Windows 平台是空操作：POSIX 终端原生支持 ANSI 转义序列。
func enableVT() {}
