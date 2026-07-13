//go:build windows

package main

import (
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
	"unsafe"
)

// 托盘相关全局状态。consoleHwnd 是控制台窗口句柄，trayHwnd 是接收托盘消息的隐藏窗口。
var (
	consoleHwnd uintptr
	trayHwnd    uintptr
	trayConfig  string

	// taskbarCreatedMsg 是 "TaskbarCreated" 注册消息 ID。
	// explorer.exe 重启后会把托盘清空并广播此消息，应用收到后需重新添加图标。
	taskbarCreatedMsg uint32

	// trayCurIcon 是当前托盘颜色图标句柄，切换颜色时销毁上一个，避免 GDI 句柄泄漏。
	trayIconMu  sync.Mutex
	trayCurIcon uintptr
)

// 自定义消息与菜单项 ID。
const (
	wmTrayCallback uint32 = 0x8001 // WM_APP(0x8000) + 1，托盘事件回调
	idmShow        uint32 = 1001
	idmOpenConfig  uint32 = 1002
	idmReload      uint32 = 1004 // 刷新重载配置（重新读 config.json + 清空统计）
	idmExit        uint32 = 1003
)

// Windows 消息与 API 常量。
const (
	WM_DESTROY       = 0x0002
	WM_COMMAND       = 0x0111
	WM_LBUTTONDBLCLK = 0x0203
	WM_RBUTTONUP     = 0x0205

	NIM_ADD     = 0x00000000
	NIM_MODIFY  = 0x00000001
	NIM_DELETE  = 0x00000002
	NIF_MESSAGE = 0x00000001
	NIF_ICON    = 0x00000002
	NIF_TIP     = 0x00000004

	MF_STRING       = 0x00000000
	MF_SEPARATOR    = 0x00000800
	TPM_RIGHTBUTTON = 0x0002
	TPM_RETURNCMD   = 0x0100

	SW_HIDE    = 0
	SW_RESTORE = 9

	idiApplication = 32512 // IDI_APPLICATION
)

var (
	kernel32 = syscall.NewLazyDLL("kernel32.dll")
	user32   = syscall.NewLazyDLL("user32.dll")
	shell32  = syscall.NewLazyDLL("shell32.dll")
	gdi32    = syscall.NewLazyDLL("gdi32.dll")

	procCreateBitmap       = gdi32.NewProc("CreateBitmap")
	procDeleteObject       = gdi32.NewProc("DeleteObject")
	procCreateIconIndirect = user32.NewProc("CreateIconIndirect")
	procDestroyIcon        = user32.NewProc("DestroyIcon")

	procGetConsoleWindow      = kernel32.NewProc("GetConsoleWindow")
	procGetModuleHandleW      = kernel32.NewProc("GetModuleHandleW")
	procRegisterClassExW      = user32.NewProc("RegisterClassExW")
	procCreateWindowExW       = user32.NewProc("CreateWindowExW")
	procDefWindowProcW        = user32.NewProc("DefWindowProcW")
	procGetMessageW           = user32.NewProc("GetMessageW")
	procTranslateMessage      = user32.NewProc("TranslateMessage")
	procDispatchMessageW      = user32.NewProc("DispatchMessageW")
	procPostQuitMessage       = user32.NewProc("PostQuitMessage")
	procLoadIconW             = user32.NewProc("LoadIconW")
	procCreatePopupMenu       = user32.NewProc("CreatePopupMenu")
	procAppendMenuW           = user32.NewProc("AppendMenuW")
	procTrackPopupMenu        = user32.NewProc("TrackPopupMenu")
	procDestroyMenu           = user32.NewProc("DestroyMenu")
	procGetCursorPos          = user32.NewProc("GetCursorPos")
	procSendMessageW          = user32.NewProc("SendMessageW")
	procSetForegroundWindow   = user32.NewProc("SetForegroundWindow")
	procShowWindow            = user32.NewProc("ShowWindow")
	procIsIconic              = user32.NewProc("IsIconic")
	procRegisterWindowMessage = user32.NewProc("RegisterWindowMessageW")
	procShellNotifyIcon       = shell32.NewProc("Shell_NotifyIconW")
)

// WNDCLASSEX，布局须与 Windows 一致（x64）。
type wndClassEx struct {
	CbSize        uint32
	Style         uint32
	LpfnWndProc   uintptr
	CbClsExtra    int32
	CbWndExtra    int32
	HInstance     uintptr
	HIcon         uintptr
	HCursor       uintptr
	HbrBackground uintptr
	LpszMenuName  *uint16
	LpszClassName *uint16
	HIconSm       uintptr
}

// MSG，窗口消息。
type msg struct {
	Hwnd    uintptr
	Message uint32
	WParam  uintptr
	LParam  uintptr
	Time    uint32
	Pt      point
}

type point struct {
	X, Y int32
}

// NOTIFYICONDATA，托盘图标结构（全量字段，x64 大小 976）。
type notifyIconData struct {
	CbSize           uint32
	HWnd             uintptr
	UID              uint32
	UFlags           uint32
	UCallbackMessage uint32
	HIcon            uintptr
	SzTip            [128]uint16
	DwState          uint32
	DwStateMask      uint32
	SzInfo           [256]uint16
	UVersion         uint32
	SzInfoTitle      [64]uint16
	DwInfoFlags      uint32
	GuidItem         [16]byte
	HBalloonIcon     uintptr
}

// ICONINFO，CreateIconIndirect 的参数（x64 布局：4+4+4+pad4+8+8 = 32）。
type iconInfo struct {
	FIcon    int32   // 1=图标，0=光标
	XHotspot uint32  // 光标热点（图标不用）
	YHotspot uint32
	HbmMask  uintptr // 单色 AND 掩码
	HbmColor uintptr // 彩色 XOR 掩码
}

// setupTray 创建隐藏消息窗口和托盘图标，并在固定线程上跑消息循环。
// 必须在单独 goroutine 里调用，内部 LockOSThread 把消息循环钉在同一个 OS 线程。
func setupTray(configPath string) {
	runtime.LockOSThread()

	if abs, err := filepath.Abs(configPath); err == nil {
		trayConfig = abs
	} else {
		trayConfig = configPath
	}

	if h, _, _ := procGetConsoleWindow.Call(); h != 0 {
		consoleHwnd = h
	}

	hInst, _, _ := procGetModuleHandleW.Call(0)
	className, _ := syscall.UTF16PtrFromString("Proxy429Tray")

	wcls := wndClassEx{
		CbSize:        uint32(unsafe.Sizeof(wndClassEx{})),
		LpfnWndProc:   syscall.NewCallback(trayWndProc),
		HInstance:     hInst,
		LpszClassName: className,
	}
	procRegisterClassExW.Call(uintptr(unsafe.Pointer(&wcls)))

	hwnd, _, _ := procCreateWindowExW.Call(
		0,
		uintptr(unsafe.Pointer(className)),
		0, 0,
		0, 0, 0, 0,
		0, 0, hInst, 0,
	)
	if hwnd == 0 {
		log.Printf("[托盘] 创建窗口失败")
		return
	}
	trayHwnd = hwnd

	// 注册 "TaskbarCreated" 消息：explorer 重启后清空托盘并广播它，应用据此重建图标。
	tbName, _ := syscall.UTF16PtrFromString("TaskbarCreated")
	if r, _, _ := procRegisterWindowMessage.Call(uintptr(unsafe.Pointer(tbName))); r != 0 {
		taskbarCreatedMsg = uint32(r)
	}

	if !addTrayIcon(hwnd) {
		log.Printf("[托盘] 添加托盘图标失败")
	}
	// 初始即设为颜色图标（覆盖 addTrayIcon 的系统默认图标）。
	applyTray(hwnd)

	// 轮询控制台最小化：检测到最小化就隐藏窗口到托盘。
	go pollMinimize()
	// 轮询活跃流数，变化时切换托盘图标颜色（状态灯：灰=空闲，绿=活跃）。
	go pollTrayColor(hwnd)

	// 消息循环（固定线程）。
	var m msg
	for {
		ret, _, _ := procGetMessageW.Call(uintptr(unsafe.Pointer(&m)), 0, 0, 0)
		if int32(ret) <= 0 { // 0=WM_QUIT，-1=错误
			break
		}
		procTranslateMessage.Call(uintptr(unsafe.Pointer(&m)))
		procDispatchMessageW.Call(uintptr(unsafe.Pointer(&m)))
	}
}

// addTrayIcon 把图标加到系统托盘。
func addTrayIcon(hwnd uintptr) bool {
	hIcon, _, _ := procLoadIconW.Call(0, idiApplication) // 系统默认图标

	var nid notifyIconData
	nid.CbSize = uint32(unsafe.Sizeof(nid))
	nid.HWnd = hwnd
	nid.UID = 1
	nid.UFlags = NIF_MESSAGE | NIF_ICON | NIF_TIP
	nid.UCallbackMessage = wmTrayCallback
	nid.HIcon = hIcon
	copy(nid.SzTip[:], syscall.StringToUTF16("Proxy429"))
	ret, _, _ := procShellNotifyIcon.Call(NIM_ADD, uintptr(unsafe.Pointer(&nid)))
	return ret != 0
}

// removeTrayIcon 退出前删掉托盘图标，避免残留幽灵图标。
func removeTrayIcon() {
	if trayHwnd == 0 {
		return
	}
	var nid notifyIconData
	nid.CbSize = uint32(unsafe.Sizeof(nid))
	nid.HWnd = trayHwnd
	nid.UID = 1
	procShellNotifyIcon.Call(NIM_DELETE, uintptr(unsafe.Pointer(&nid)))
}

// pollMinimize 每 200ms 检查控制台是否被最小化，是则隐藏到托盘。
// 用轮询而非 subclass 控制台窗口，避开 conhost 跨进程 subclass 的不稳定风险。
func pollMinimize() {
	for {
		time.Sleep(200 * time.Millisecond)
		if consoleHwnd != 0 {
			if ret, _, _ := procIsIconic.Call(consoleHwnd); ret != 0 {
				procShowWindow.Call(consoleHwnd, SW_HIDE)
			}
		}
	}
}

// trayWndProc 是隐藏窗口的窗口过程，处理托盘事件和菜单命令。
// 参数全用 uintptr 以满足 syscall.NewCallback 的签名要求。
func trayWndProc(hwnd, msg, wparam, lparam uintptr) uintptr {
	// explorer 重启后广播 TaskbarCreated，收到就重建托盘图标。
	if taskbarCreatedMsg != 0 && uint32(msg) == taskbarCreatedMsg {
		removeTrayIcon() // 先清可能残留的旧条目，再重新添加
		addTrayIcon(hwnd)
		// 重建后立即恢复颜色图标（addTrayIcon 用的是系统默认图标）。
		applyTray(hwnd)
		return 0
	}
	switch uint32(msg) {
	case wmTrayCallback:
		switch uint32(lparam) {
		case WM_LBUTTONDBLCLK:
			showConsole()
		case WM_RBUTTONUP:
			showTrayMenu(hwnd)
		}
		return 0
	case WM_COMMAND:
		switch uint32(wparam) & 0xFFFF {
		case idmShow:
			showConsole()
		case idmOpenConfig:
			openConfig()
		case idmReload:
			reloadConfig()
		case idmExit:
			removeTrayIcon()
			os.Exit(0)
		}
		return 0
	case WM_DESTROY:
		procPostQuitMessage.Call(0)
		return 0
	}
	ret, _, _ := procDefWindowProcW.Call(hwnd, msg, wparam, lparam)
	return ret
}

// showConsole 恢复并前置控制台窗口。
func showConsole() {
	if consoleHwnd == 0 {
		return
	}
	procShowWindow.Call(consoleHwnd, SW_RESTORE)
	procSetForegroundWindow.Call(consoleHwnd)
}

// showTrayMenu 在光标位置弹出右键菜单。
func showTrayMenu(hwnd uintptr) {
	menu, _, _ := procCreatePopupMenu.Call()
	if menu == 0 {
		return
	}
	defer procDestroyMenu.Call(menu)

	appendMenu(menu, MF_STRING, uintptr(idmShow), "显示窗口")
	appendMenu(menu, MF_STRING, uintptr(idmOpenConfig), "打开配置文件")
	appendMenu(menu, MF_STRING, uintptr(idmReload), "刷新重载配置")
	appendMenu(menu, MF_SEPARATOR, 0, "")
	appendMenu(menu, MF_STRING, uintptr(idmExit), "退出代理")

	var pt point
	procGetCursorPos.Call(uintptr(unsafe.Pointer(&pt)))
	// SetForegroundWindow 是 TrackPopupMenu 能正常关闭的必要前置。
	procSetForegroundWindow.Call(hwnd)
	cmd, _, _ := procTrackPopupMenu.Call(
		menu,
		TPM_RIGHTBUTTON|TPM_RETURNCMD,
		uintptr(uint32(pt.X)), uintptr(uint32(pt.Y)), 0, hwnd, 0,
	)
	if cmd != 0 {
		procSendMessageW.Call(hwnd, WM_COMMAND, cmd, 0)
	}
}

// appendMenu 封装 AppendMenuW，自动处理 UTF16 转换。
func appendMenu(menu, flags, id uintptr, text string) {
	t, _ := syscall.UTF16PtrFromString(text)
	procAppendMenuW.Call(menu, flags, id, uintptr(unsafe.Pointer(t)))
}

// openConfig 用关联程序打开 config.json。
func openConfig() {
	if trayConfig == "" {
		return
	}
	// cmd /c start 用默认关联程序打开文件。
	_ = exec.Command("cmd", "/c", "start", "", trayConfig).Start()
}

// makeColorIcon 用 GDI 生成一个 16x16 纯色图标（托盘状态灯用）。
// 像素用 32bpp BGRA；AND 掩码全 0 表示整块不透明。返回图标句柄，失败返回 0。
// 调用方负责 DestroyIcon（共享系统图标除外）。
func makeColorIcon(r, g, b byte) uintptr {
	const w, h = 16, 16
	pixels := make([]byte, w*h*4)
	for i := 0; i < w*h; i++ {
		pixels[i*4+0] = b // B
		pixels[i*4+1] = g // G
		pixels[i*4+2] = r // R
		pixels[i*4+3] = 0 // A（CreateIconIndirect 以 AND 掩码定透明，alpha 留 0）
	}
	hbmColor, _, _ := procCreateBitmap.Call(
		uintptr(w), uintptr(h), 1, 32,
		uintptr(unsafe.Pointer(&pixels[0])),
	)
	mask := make([]byte, (w*h+7)/8) // 1bpp，全 0 = 全不透明
	hbmMask, _, _ := procCreateBitmap.Call(
		uintptr(w), uintptr(h), 1, 1,
		uintptr(unsafe.Pointer(&mask[0])),
	)
	ii := iconInfo{FIcon: 1, HbmMask: hbmMask, HbmColor: hbmColor}
	hIcon, _, _ := procCreateIconIndirect.Call(uintptr(unsafe.Pointer(&ii)))
	procDeleteObject.Call(hbmColor)
	procDeleteObject.Call(hbmMask)
	return hIcon
}

// updateTrayIcon 用 NIM_MODIFY 更新托盘图标和 tooltip（不动回调）。
func updateTrayIcon(hwnd, hIcon uintptr, tip string) {
	var nid notifyIconData
	nid.CbSize = uint32(unsafe.Sizeof(nid))
	nid.HWnd = hwnd
	nid.UID = 1
	nid.UFlags = NIF_ICON | NIF_TIP
	nid.HIcon = hIcon
	copy(nid.SzTip[:], syscall.StringToUTF16(tip))
	procShellNotifyIcon.Call(NIM_MODIFY, uintptr(unsafe.Pointer(&nid)))
}

// trayInfo 一次锁读当前状态：state(0灰/1黄/2绿)、active(流式中)、waiting(已发待回)。
// tooltip 需要具体数量，故一并返回。
func trayInfo() (state, active, waiting int) {
	stats.mu.Lock()
	defer stats.mu.Unlock()
	active = stats.active
	waiting = stats.waiting
	switch {
	case active > 0:
		state = 2
	case waiting > 0:
		state = 1
	}
	return
}

// trayState 返回颜色状态（2绿/1黄/0灰），给测试和颜色判断用。
func trayState() int {
	state, _, _ := trayInfo()
	return state
}

// trayTip 按活跃/等待数生成多行 tooltip。
// 两态并存时分行显示，避免一行过长。用 \n 换行（Windows 7+ 的 szTip 支持多行）。
func trayTip(active, waiting int) string {
	lines := []string{"Proxy429"}
	if active == 0 && waiting == 0 {
		lines = append(lines, "idle")
	} else {
		if active > 0 {
			lines = append(lines, "active "+strconv.Itoa(active))
		}
		if waiting > 0 {
			lines = append(lines, "waiting "+strconv.Itoa(waiting))
		}
	}
	return strings.Join(lines, "\n")
}

// applyTray 读当前状态，更新托盘图标颜色和 tooltip。轮询/初始/重建共用。
func applyTray(hwnd uintptr) {
	state, active, waiting := trayInfo()
	setTrayColorIcon(hwnd, state, trayTip(active, waiting))
}

// setTrayColorIcon 按状态生成图标并更新托盘（图标颜色 + tooltip）：2=绿(流式中)，1=黄(已发待回)，0=灰(空闲)。
// 旧颜色图标在替换后销毁，避免 GDI 句柄泄漏。
func setTrayColorIcon(hwnd uintptr, state int, tip string) {
	var r, g, b byte
	switch state {
	case 2:
		r, g, b = 10, 200, 30 // 绿：流式转发中
	case 1:
		r, g, b = 230, 180, 30 // 黄：已发上游、等首字节
	default:
		r, g, b = 150, 150, 150 // 灰：空闲
	}
	newIcon := makeColorIcon(r, g, b)
	if newIcon == 0 {
		return
	}
	trayIconMu.Lock()
	old := trayCurIcon
	trayCurIcon = newIcon
	updateTrayIcon(hwnd, newIcon, tip)
	trayIconMu.Unlock()
	if old != 0 {
		procDestroyIcon.Call(old) // NIM_MODIFY 已替换，旧图标可安全销毁
	}
}

// pollTrayColor 每 200ms 检查状态，state/active/waiting 任一变化时更新托盘（图标颜色 + tooltip）。
// 数量变化也要更新 tooltip，故跟踪三个值。
func pollTrayColor(hwnd uintptr) {
	prevState, prevActive, prevWaiting := -1, -1, -1 // 哨兵：确保启动时设一次
	for {
		time.Sleep(200 * time.Millisecond)
		state, active, waiting := trayInfo()
		if state == prevState && active == prevActive && waiting == prevWaiting {
			continue
		}
		prevState, prevActive, prevWaiting = state, active, waiting
		applyTray(hwnd)
	}
}
