package main

import (
	"bytes"
	"encoding/binary"
	"image"
	"image/png"
	"log"
	"math"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"fyne.io/systray"
)

// trayStop 在 onExit 时关闭，通知 pollTrayColor 停止更新（避免退出后还调 systray API）。
var trayStop = make(chan struct{})

// setupTray 启动系统托盘/菜单栏图标，并阻塞主线程直到 systray.Quit。
// macOS 要求 UI 事件循环跑在主 OS 线程，故此函数必须在 main 里直接调用（不要 go），
// 不能塞进 goroutine。HTTP 服务等其它工作在各自 goroutine 里并发跑。
func setupTray() {
	systray.Run(onReady, onExit)
}

// onReady 是托盘就绪回调：设初始图标/tooltip、构建菜单、启动状态轮询。
func onReady() {
	systray.SetIcon(makeStatusIcon(0)) // 初始灰灯
	systray.SetTooltip(trayTip(0, 0))

	// 「查看日志」：用默认浏览器打开本地控制台页（状态/日志/配置，关标签页即隐藏，不影响代理）。
	// 配置编辑与重载已移入该网页，故托盘菜单只保留「查看日志」和「退出」两项，跨平台一致。
	mLogs := systray.AddMenuItem("查看日志", "")
	go func() {
		for range mLogs.ClickedCh {
			openLogViewer()
		}
	}()

	// 「切换配置」：子菜单列出当前配置目录下所有 .json 文件，点击即时切换并重载。
	// 当前配置打勾；切换失败（配置坏）则保持旧配置，勾选不变。「刷新列表」用于新增配置文件后重建菜单。
	mSwitch := systray.AddMenuItem("切换配置", "")
	var cfgItems []*systray.MenuItem
	// 先声明再赋值：闭包内部会调用 rebuildCfgMenu 重建菜单，短变量声明的作用域从语句结束才开始，
	// 直接 rebuildCfgMenu := func(){...rebuildCfgMenu()...} 会因变量尚未在作用域而编译失败。
	var rebuildCfgMenu func()
	rebuildCfgMenu = func() {
		for _, it := range cfgItems {
			it.Hide()
		}
		cfgItems = nil
		cur := filepath.Base(currentConfigPath())
		for _, name := range listConfigFiles() {
			n := name
			item := mSwitch.AddSubMenuItemCheckbox(n, "", n == cur)
			cfgItems = append(cfgItems, item)
			go func(it *systray.MenuItem, fname string) {
				for range it.ClickedCh {
					full := filepath.Join(filepath.Dir(currentConfigPath()), fname)
					if err := switchConfig(full); err != nil {
						log.Printf("[切换] %s 失败: %v", fname, err)
						continue
					}
					for _, x := range cfgItems {
						x.Uncheck()
					}
					it.Check()
				}
			}(item, n)
		}
		refreshItem := mSwitch.AddSubMenuItem("刷新列表", "")
		cfgItems = append(cfgItems, refreshItem)
		go func() {
			for range refreshItem.ClickedCh {
				rebuildCfgMenu()
			}
		}()
	}
	rebuildCfgMenu()

	systray.AddSeparator()

	mQuit := systray.AddMenuItem("退出代理", "")
	go func() {
		for range mQuit.ClickedCh {
			systray.Quit()
		}
	}()

	go pollTrayColor()
}

// onExit 是托盘退出回调（systray.Quit 后触发）：停掉状态轮询，Run 随即返回。
func onExit() {
	close(trayStop)
}

// pollTrayColor 每 200ms 检查状态，state/active/waiting 任一变化时更新图标颜色和 tooltip。
func pollTrayColor() {
	prevState, prevActive, prevWaiting := -1, -1, -1 // 哨兵：确保启动时设一次
	for {
		select {
		case <-trayStop:
			return
		case <-time.After(200 * time.Millisecond):
		}
		state, active, waiting := trayInfo()
		if state == prevState && active == prevActive && waiting == prevWaiting {
			continue
		}
		prevState, prevActive, prevWaiting = state, active, waiting
		systray.SetIcon(makeStatusIcon(state))
		systray.SetTooltip(trayTip(active, waiting))
	}
}

// openLogViewer 用默认浏览器打开本地控制台页（http://<listen>/__logs）。
func openLogViewer() {
	c := cfg.Load()
	if c == nil {
		return
	}
	openURL("http://" + c.Listen + logViewerPath)
}

// openURL 用系统默认程序打开 URL 或文件：macOS=open、Windows=cmd start、Linux=xdg-open。
func openURL(target string) {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", target)
	case "windows":
		cmd = exec.Command("cmd", "/c", "start", "", target)
	default: // linux 等
		cmd = exec.Command("xdg-open", target)
	}
	_ = cmd.Start()
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

// trayTip 按活跃/等待数生成多行 tooltip。两态并存时分行显示，避免一行过长。
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

// ---- 状态灯图标生成（跨平台，纯 Go 无 cgo/无 GDI） ----

// makeStatusIcon 按状态生成托盘图标字节：2=绿(流式中)，1=黄(已发待回)，0=灰(空闲)。
// macOS/Linux 用 PNG；Windows 用 BMP-entry ICO（LoadImageW 必定支持，PNG-entry 不稳）。
// 画一个 32x32 抗锯齿实心圆灯，替代旧版 GDI 16x16 纯色方块。
func makeStatusIcon(state int) []byte {
	var r, g, b byte
	switch state {
	case 2:
		r, g, b = 10, 200, 30 // 绿：流式转发中
	case 1:
		r, g, b = 230, 180, 30 // 黄：已发上游、等首字节
	default:
		r, g, b = 150, 150, 150 // 灰：空闲
	}
	const s = 32
	pix := statusCirclePixels(s, r, g, b) // RGBA，top-down
	if runtime.GOOS == "windows" {
		return rgbaToICO(s, s, pix)
	}
	return rgbaToPNG(s, s, pix)
}

// statusCirclePixels 生成 s×s 的 RGBA 像素：居中实心圆，1px 抗锯齿边缘，圆外透明。
func statusCirclePixels(s int, r, g, b byte) []byte {
	cx, cy := float64(s-1)/2, float64(s-1)/2
	rad := float64(s-1)/2 - 1
	pix := make([]byte, s*s*4)
	for y := 0; y < s; y++ {
		for x := 0; x < s; x++ {
			d := math.Sqrt((float64(x)-cx)*(float64(x)-cx) + (float64(y)-cy)*(float64(y)-cy))
			var a float64
			switch {
			case d <= rad-1:
				a = 1
			case d >= rad+1:
				a = 0
			default:
				a = (rad + 1 - d) / 2
			}
			i := (y*s + x) * 4
			pix[i+0] = r
			pix[i+1] = g
			pix[i+2] = b
			pix[i+3] = byte(a * 255)
		}
	}
	return pix
}

// rgbaToPNG 把 RGBA 像素编码成 PNG（macOS/Linux 托盘图标用）。
func rgbaToPNG(w, h int, pix []byte) []byte {
	img := &image.NRGBA{Pix: pix, Stride: w * 4, Rect: image.Rect(0, 0, w, h)}
	var buf bytes.Buffer
	_ = png.Encode(&buf, img)
	return buf.Bytes()
}

// rgbaToICO 把 RGBA 像素封装成单条目 BMP-ICO（Windows 托盘图标用）。
// 结构：ICONDIR(6) + ICONDIRENTRY(16) + BITMAPINFOHEADER(40) + XOR DIB(BGRA,bottom-up) + AND 掩码(1bpp,全0)。
// AND 掩码全 0 表示不透明，透明度交给 XOR 的 alpha 通道处理。
func rgbaToICO(w, h int, pix []byte) []byte {
	xorSize := w * h * 4
	andRowSize := ((w+7)/8 + 3) &^ 3 // 1bpp 每行按 4 字节对齐
	andSize := andRowSize * h
	const bihSize = 40
	imgSize := bihSize + xorSize + andSize
	ico := make([]byte, 6+16+imgSize)

	// ICONDIR
	binary.LittleEndian.PutUint16(ico[0:2], 0) // reserved
	binary.LittleEndian.PutUint16(ico[2:4], 1) // type=1 icon
	binary.LittleEndian.PutUint16(ico[4:6], 1) // count=1
	// ICONDIRENTRY
	ico[6] = byte(w)
	ico[7] = byte(h)
	ico[8] = 0                                                 // colorCount
	ico[9] = 0                                                 // reserved
	binary.LittleEndian.PutUint16(ico[10:12], 1)               // planes
	binary.LittleEndian.PutUint16(ico[12:14], 32)              // bitCount
	binary.LittleEndian.PutUint32(ico[14:18], uint32(imgSize)) // bytesInRes
	binary.LittleEndian.PutUint32(ico[18:22], 22)              // imageOffset = 6+16
	// BITMAPINFOHEADER
	off := 22
	binary.LittleEndian.PutUint32(ico[off+0:off+4], bihSize)                   // biSize
	binary.LittleEndian.PutUint32(ico[off+4:off+8], uint32(w))                 // biWidth
	binary.LittleEndian.PutUint32(ico[off+8:off+12], uint32(2*h))              // biHeight（图像+AND掩码，故翻倍）
	binary.LittleEndian.PutUint16(ico[off+12:off+14], 1)                       // biPlanes
	binary.LittleEndian.PutUint16(ico[off+14:off+16], 32)                      // biBitCount
	binary.LittleEndian.PutUint32(ico[off+16:off+20], 0)                       // biCompression=BI_RGB
	binary.LittleEndian.PutUint32(ico[off+20:off+24], uint32(xorSize+andSize)) // biSizeImage
	// 其余字段（biXPelsPerMeter 等）为 0，make 已置零
	off += bihSize
	// XOR DIB：BGRA、bottom-up
	for y := h - 1; y >= 0; y-- {
		for x := 0; x < w; x++ {
			src := (y*w + x) * 4
			ico[off+0] = pix[src+2] // B
			ico[off+1] = pix[src+1] // G
			ico[off+2] = pix[src+0] // R
			ico[off+3] = pix[src+3] // A
			off += 4
		}
	}
	// AND 掩码：全 0（不透明），已由 make 置零
	return ico
}
