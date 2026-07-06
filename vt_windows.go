//go:build windows

package main

import (
	"os"
	"syscall"
	"unsafe"
)

// enableVT 在 Windows 上启用控制台虚拟终端处理，
// 让 stderr 上的 \r、\033[K 等 ANSI 转义序列被正确解释（而不是原样打印成乱码）。
// 失败时静默返回——现代 Windows Terminal / PowerShell 多半已默认支持。
func enableVT() {
	const enableVirtualTerminalProcessing = 0x0004
	kernel32 := syscall.NewLazyDLL("kernel32.dll")
	getMode := kernel32.NewProc("GetConsoleMode")
	setMode := kernel32.NewProc("SetConsoleMode")
	h := syscall.Handle(os.Stderr.Fd())
	var mode uint32
	r, _, _ := getMode.Call(uintptr(h), uintptr(unsafe.Pointer(&mode)))
	if r == 0 {
		return
	}
	setMode.Call(uintptr(h), uintptr(mode|enableVirtualTerminalProcessing))
}
