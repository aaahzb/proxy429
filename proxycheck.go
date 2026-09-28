package main

// proxycheck.go — loopback-hijack watchdog (READ-ONLY: never writes to the system).
//
// Codex's HTTP stack (reqwest) honors the OS system proxy but ignores its exception list on
// macOS (verified 2026-09: requests for 127.0.0.1 get forwarded to the proxy server -> 503
// while this proxy sees nothing). The working bypass is the NO_PROXY/no_proxy environment
// variable, installed by the Codex one-line setup script. That fix lives outside this process
// (launchd env + login item on macOS, a user env var on Windows) and can silently vanish —
// once observed after a macOS upgrade wiped the login item — so this watchdog re-checks
// periodically and surfaces the trap: red tray light + console status-tab banner + log line.
//
// Only checked when responses_listen is configured (i.e. Codex-style clients are expected);
// with no Responses entry point there is nothing to hijack and the warning would be noise.

import (
	"context"
	"log"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"sync/atomic"
	"time"
)

var hijackRiskFlag atomic.Bool
var hijackDetail atomic.Value // string: proxy address/source, for log lines

// proxyHijackRisk reports the latest watchdog verdict (read by the tray poll and /__logs/data).
func proxyHijackRisk() bool { return hijackRiskFlag.Load() }

// watchProxyHijack checks the trap at startup and then every 30s; transitions log one line.
func watchProxyHijack() {
	prev := false
	for {
		risk, detail := detectHijackRisk()
		hijackRiskFlag.Store(risk)
		if risk {
			hijackDetail.Store(detail)
		}
		switch {
		case risk && !prev:
			log.Printf("[proxycheck] system proxy is on but NO_PROXY does not exclude loopback (%s) — proxy-following clients (e.g. Codex) will hand 127.0.0.1 requests to the proxy server (503, zero logs here); re-run the Codex one-line setup on the console Config tab to auto-repair", detail)
		case !risk && prev:
			log.Printf("[proxycheck] loopback-hijack risk cleared")
		}
		prev = risk
		time.Sleep(30 * time.Second)
	}
}

// detectHijackRisk = system proxy enabled AND no working loopback bypass in sight.
func detectHijackRisk() (bool, string) {
	c := cfg.Load()
	if c == nil || strings.TrimSpace(c.ResponsesListen) == "" {
		return false, ""
	}
	switch runtime.GOOS {
	case "darwin":
		// reqwest reads macOS system proxy settings but ignores their ExceptionsList
		// (verified), so the exception list never counts as a bypass — only the launchd
		// environment (what GUI apps and new terminals inherit) does. Queried live via
		// launchctl: this process's own os.Getenv froze at its launch and would miss a
		// setenv applied afterwards.
		on, desc := darwinSystemProxy()
		if !on {
			return false, ""
		}
		if noProxyCoversLoopback(launchdEnv("NO_PROXY")) || noProxyCoversLoopback(launchdEnv("no_proxy")) {
			return false, ""
		}
		return true, desc
	case "windows":
		on, desc := windowsSystemProxy()
		if !on {
			return false, ""
		}
		// Bypass sources: process env, user/machine-scope NO_PROXY, or the exception list
		// (recent Windows reqwest builds do honor ProxyOverride — user-verified).
		if noProxyCoversLoopback(os.Getenv("NO_PROXY")) || noProxyCoversLoopback(os.Getenv("no_proxy")) {
			return false, ""
		}
		if noProxyCoversLoopback(regQueryValue(`HKCU\Environment`, "NO_PROXY")) ||
			noProxyCoversLoopback(regQueryValue(`HKLM\SYSTEM\CurrentControlSet\Control\Session Manager\Environment`, "NO_PROXY")) {
			return false, ""
		}
		if proxyOverrideCoversLoopback(regQueryValue(`HKCU\Software\Microsoft\Windows\CurrentVersion\Internet Settings`, "ProxyOverride")) {
			return false, ""
		}
		return true, desc
	default:
		// Linux & co: no user-scope env mechanism covers every session type (the one-line
		// script only advises there) — best effort is this process's own session env.
		for _, v := range []string{"https_proxy", "HTTPS_PROXY", "http_proxy", "HTTP_PROXY", "all_proxy", "ALL_PROXY"} {
			if os.Getenv(v) != "" {
				if noProxyCoversLoopback(os.Getenv("NO_PROXY")) || noProxyCoversLoopback(os.Getenv("no_proxy")) {
					return false, ""
				}
				return true, "env " + v
			}
		}
		return false, ""
	}
}

// noProxyCoversLoopback reports whether a NO_PROXY-style comma list bypasses loopback targets.
func noProxyCoversLoopback(v string) bool {
	for _, e := range strings.Split(v, ",") {
		switch strings.TrimSpace(e) {
		case "127.0.0.1", "localhost", "::1", "*":
			return true
		}
	}
	return false
}

// ---- macOS ----

// darwinSystemProxy parses live scutil --proxy output: HTTP/HTTPS proxy on -> true + address.
func darwinSystemProxy() (bool, string) {
	out, err := runProbe("scutil", "--proxy")
	if err != nil {
		return false, ""
	}
	return scutilProxyEnabled(out)
}

// scutilProxyEnabled is the pure parser behind darwinSystemProxy (kept separate for tests).
func scutilProxyEnabled(out string) (bool, string) {
	enabled := false
	host, port := "", ""
	for _, ln := range strings.Split(out, "\n") {
		ln = strings.TrimSpace(ln)
		switch {
		case ln == "HTTPEnable : 1" || ln == "HTTPSEnable : 1":
			enabled = true
		case strings.HasPrefix(ln, "HTTPProxy : "):
			host = strings.TrimPrefix(ln, "HTTPProxy : ")
		case strings.HasPrefix(ln, "HTTPPort : "):
			port = strings.TrimPrefix(ln, "HTTPPort : ")
		}
	}
	desc := host
	if host != "" && port != "" {
		desc = host + ":" + port
	}
	return enabled, desc
}

// launchdEnv reads a launchd session variable (what GUI apps and new terminals inherit).
func launchdEnv(name string) string {
	out, err := runProbe("launchctl", "getenv", name)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(out)
}

// ---- Windows ----

func windowsSystemProxy() (bool, string) {
	const key = `HKCU\Software\Microsoft\Windows\CurrentVersion\Internet Settings`
	if regQueryValue(key, "ProxyEnable") != "0x1" {
		return false, ""
	}
	server := regQueryValue(key, "ProxyServer")
	if server == "" {
		return false, ""
	}
	return true, server
}

// regQueryValue runs reg query <key> /v <name> and returns the data field ("" when absent).
func regQueryValue(key, name string) string {
	out, err := runProbe("reg", "query", key, "/v", name)
	if err != nil {
		return ""
	}
	return regQueryValueData(out, name)
}

// regQueryValueData is the pure parser behind regQueryValue (kept separate for tests):
// the value line looks like "    ProxyEnable    REG_DWORD    0x1"; data = last field.
// (Values with spaces in their data would truncate — ProxyServer/ProxyOverride/NO_PROXY
// are space-free in practice.)
func regQueryValueData(out, name string) string {
	for _, ln := range strings.Split(out, "\n") {
		if f := strings.Fields(ln); len(f) >= 3 && f[0] == name {
			return f[len(f)-1]
		}
	}
	return ""
}

// proxyOverrideCoversLoopback parses the Windows proxy exception list (';'-separated;
// <-loopback> is the built-in loopback macro).
func proxyOverrideCoversLoopback(v string) bool {
	for _, e := range strings.Split(v, ";") {
		switch strings.TrimSpace(e) {
		case "127.0.0.1", "localhost", "::1", "*", "<-loopback>":
			return true
		}
	}
	return false
}

// ---- shared ----

// runProbe runs a read-only system query with a hard timeout (defense against a hung
// scutil/launchctl/reg). probeNoWindow hides the console window on Windows.
func runProbe(name string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, name, args...)
	probeNoWindow(cmd)
	out, err := cmd.Output()
	return string(out), err
}
