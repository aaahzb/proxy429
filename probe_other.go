//go:build !windows

package main

import "os/exec"

// probeNoWindow is a no-op off Windows (console-window flashing is a Windows-only concern).
func probeNoWindow(cmd *exec.Cmd) {}
