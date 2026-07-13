//go:build windows

package main

import (
	"log"
	"os"
	"syscall"
	"unsafe"
)

// enableVT 在 Windows 上启用控制台虚拟终端处理，
// 让 stderr 上的 \r、\033[K 等 ANSI 转义序列被正确解释（而不是原样打印成乱码）。
// 返回是否成功启用；失败时打印诊断原因——旧实现静默返回，会导致 VT 没启用却无感知，
// 状态行原地刷新/清屏全部失效（Windows 10 conhost 默认不开 VT，必须主动 SetConsoleMode）。
func enableVT() bool {
	const enableVirtualTerminalProcessing = 0x0004
	kernel32 := syscall.NewLazyDLL("kernel32.dll")
	getMode := kernel32.NewProc("GetConsoleMode")
	setMode := kernel32.NewProc("SetConsoleMode")
	h := syscall.Handle(os.Stderr.Fd())
	var mode uint32
	r, _, e1 := getMode.Call(uintptr(h), uintptr(unsafe.Pointer(&mode)))
	if r == 0 {
		log.Printf("[VT] GetConsoleMode 失败，stderr 可能不是控制台（被重定向？）: %v", e1)
		return false
	}
	r2, _, e2 := setMode.Call(uintptr(h), uintptr(mode|enableVirtualTerminalProcessing))
	if r2 == 0 {
		log.Printf("[VT] SetConsoleMode 失败: %v", e2)
		return false
	}
	log.Printf("[VT] 已启用虚拟终端处理（ANSI 转义生效）")
	return true
}
