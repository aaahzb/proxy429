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

// trayStop closes on onExit, telling pollTrayColor to stop updating (avoiding systray API calls after exit).
var trayStop = make(chan struct{})

// trayCfgSwitched is the notification channel for config-list changes (capacity 1, non-blocking send, dropped when full — menu rebuilds
// are naturally idempotent, merging multiple changes into one): switchConfig (tray click / web switch / create-and-switch),
// web-side config delete/rename, and save-that-creates-a-file all post via notifyTrayCfgChanged;
// the tray rebuilds the 「切换配置」 submenu from it, keeping checkmarks and the file list current without a manual 「刷新列表」.
var trayCfgSwitched = make(chan struct{}, 1)

// notifyTrayCfgChanged posts a config-list-change notification without blocking: dropped directly when the tray isn't ready / is exiting,
// or when a notification is already backlogged (rebuilds are idempotent).
func notifyTrayCfgChanged() {
	select {
	case trayCfgSwitched <- struct{}{}:
	default:
	}
}

// setupTray starts the system-tray/menu-bar icon, blocking the main thread until systray.Quit.
// macOS requires the UI event loop on the main OS thread, so this function must be called directly in main (no go),
// not stuffed into a goroutine. The HTTP service and other work run concurrently in their own goroutines.
func setupTray() {
	systray.Run(onReady, onExit)
}

// onReady is the tray-ready callback: sets the initial icon/tooltip, builds the menu, starts status polling.
func onReady() {
	systray.SetIcon(makeStatusIcon(0)) // Initial grey light
	systray.SetTooltip(trayTip(0, 0))

	// 「查看日志」: opens the local console page in the default browser (status/logs/config; closing the tab just hides it, the proxy is unaffected).
	// Config editing and reload have moved into that web page, so the tray menu keeps only 「查看日志」 and 「退出」, consistent cross-platform.
	mLogs := systray.AddMenuItem(trayText("查看日志", "Open console"), "")
	go func() {
		for range mLogs.ClickedCh {
			openLogViewer()
		}
	}()

	// 「切换配置」: the submenu lists all .json files under the current config directory; clicking switches and reloads immediately.
	// The current config gets a checkmark; a failed switch (broken config) keeps the old config, checkmark unchanged. Web-initiated switches are
	// auto-rebuilt via trayCfgSwitched notifications; 「刷新列表」 is for manual rebuilds after dropping config files into the directory.
	mSwitch := systray.AddMenuItem(trayText("切换配置", "Switch config"), "")
	var cfgItems []*systray.MenuItem
	// Declare first, assign second: the closure calls rebuildCfgMenu to rebuild the menu inside itself, and a short variable declaration's scope starts only after the statement ends,
	// so rebuildCfgMenu := func(){...rebuildCfgMenu()...} directly would fail to compile — the variable isn't in scope yet.
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
						log.Printf("[switch] %s failed: %v", fname, err)
						continue
					}
					for _, x := range cfgItems {
						x.Uncheck()
					}
					it.Check()
				}
			}(item, n)
		}
		refreshItem := mSwitch.AddSubMenuItem(trayText("刷新列表", "Refresh list"), "")
		cfgItems = append(cfgItems, refreshItem)
		go func() {
			for range refreshItem.ClickedCh {
				rebuildCfgMenu()
			}
		}()
	}
	rebuildCfgMenu()
	// Web-initiated switches (switch config / create-and-switch) also go through switchConfig: the notification rebuilds the submenu, checkmarks and list refresh immediately.
	go func() {
		for {
			select {
			case <-trayStop:
				return
			case <-trayCfgSwitched:
				rebuildCfgMenu()
			}
		}
	}()

	systray.AddSeparator()

	mQuit := systray.AddMenuItem(trayText("退出代理", "Quit proxy"), "")
	go func() {
		for range mQuit.ClickedCh {
			systray.Quit()
		}
	}()

	go pollTrayColor()
}

// onExit is the tray-exit callback (triggered after systray.Quit): stops status polling, and Run returns right after.
func onExit() {
	close(trayStop)
}

// pollTrayColor checks state every 200ms, updating the icon color and tooltip when state/active/waiting change.
func pollTrayColor() {
	prevState, prevActive, prevWaiting := -1, -1, -1 // Sentinel: ensures one set at startup
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

// openLogViewer opens the local console page (http://<listen>/__logs) in the default browser.
func openLogViewer() {
	c := cfg.Load()
	if c == nil {
		return
	}
	openURL("http://" + c.Listen + logViewerPath)
}

// openURL opens a URL or file with the system default handler: macOS=open, Windows=cmd start, Linux=xdg-open.
func openURL(target string) {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", target)
	case "windows":
		cmd = exec.Command("cmd", "/c", "start", "", target)
	default: // linux etc.
		cmd = exec.Command("xdg-open", target)
	}
	_ = cmd.Start()
}

// trayInfo reads the current state under one lock: state (0 grey/1 yellow/2 green), active (streaming), waiting (sent, awaiting reply).
// The tooltip needs the concrete counts, so they're returned together.
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

// trayState returns the color state (2 green/1 yellow/0 grey), for tests and color decisions.
func trayState() int {
	state, _, _ := trayInfo()
	return state
}

// trayTip builds a multi-line tooltip from active/waiting counts. When both coexist they're shown on separate lines, avoiding an over-long line.
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

// trayText picks tray-menu copy per the current UI language (same uiLang switch as the web console).
func trayText(zh, en string) string {
	if currentUILang() == "en" {
		return en
	}
	return zh
}

// ---- Status-light icon generation (cross-platform, pure Go, no cgo/no GDI) ----

// makeStatusIcon generates tray-icon bytes per state: 2=green (streaming), 1=yellow (sent, awaiting reply), 0=grey (idle).
// PNG for macOS/Linux; BMP-entry ICO for Windows (LoadImageW certainly supports it; PNG-entry is flaky).
// Draws a 32x32 anti-aliased solid circle light, replacing the old GDI 16x16 flat square.
func makeStatusIcon(state int) []byte {
	var r, g, b byte
	switch state {
	case 2:
		r, g, b = 10, 200, 30 // Green: streaming forward in progress
	case 1:
		r, g, b = 230, 180, 30 // Yellow: sent upstream, awaiting first byte
	default:
		r, g, b = 150, 150, 150 // Grey: idle
	}
	const s = 32
	pix := statusCirclePixels(s, r, g, b) // RGBA, top-down
	if runtime.GOOS == "windows" {
		return rgbaToICO(s, s, pix)
	}
	return rgbaToPNG(s, s, pix)
}

// statusCirclePixels generates s×s RGBA pixels: a centered solid circle, 1px anti-aliased edge, transparent outside.
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

// rgbaToPNG encodes RGBA pixels into a PNG (for macOS/Linux tray icons).
func rgbaToPNG(w, h int, pix []byte) []byte {
	img := &image.NRGBA{Pix: pix, Stride: w * 4, Rect: image.Rect(0, 0, w, h)}
	var buf bytes.Buffer
	_ = png.Encode(&buf, img)
	return buf.Bytes()
}

// rgbaToICO wraps RGBA pixels into a single-entry BMP-ICO (for Windows tray icons).
// Structure: ICONDIR(6) + ICONDIRENTRY(16) + BITMAPINFOHEADER(40) + XOR DIB (BGRA, bottom-up) + AND mask (1bpp, all zero).
// An all-zero AND mask means opaque; transparency is handled by the XOR channel's alpha.
func rgbaToICO(w, h int, pix []byte) []byte {
	xorSize := w * h * 4
	andRowSize := ((w+7)/8 + 3) &^ 3 // 1bpp rows are 4-byte aligned
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
	binary.LittleEndian.PutUint32(ico[off+8:off+12], uint32(2*h))              // biHeight (image + AND mask, hence doubled)
	binary.LittleEndian.PutUint16(ico[off+12:off+14], 1)                       // biPlanes
	binary.LittleEndian.PutUint16(ico[off+14:off+16], 32)                      // biBitCount
	binary.LittleEndian.PutUint32(ico[off+16:off+20], 0)                       // biCompression=BI_RGB
	binary.LittleEndian.PutUint32(ico[off+20:off+24], uint32(xorSize+andSize)) // biSizeImage
	// The remaining fields (biXPelsPerMeter etc.) are 0, already zeroed by make
	off += bihSize
	// XOR DIB: BGRA, bottom-up
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
	// AND mask: all zero (opaque), already zeroed by make
	return ico
}
