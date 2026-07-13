//go:build !windows

package main

// setupTray 在非 Windows 平台是空操作（托盘仅 Windows 实现）。
func setupTray(configPath string) {}
