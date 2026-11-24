// Command proxy429 is the desktop entry point: it hands control to the proxy core, which runs the
// HTTP server and web console plus the system tray (headless with -no-tray).
package main

import "proxy429/internal/proxy"

func main() {
	proxy.Main()
}
