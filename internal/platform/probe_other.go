//go:build !windows

package platform

import "os/exec"

// probeNoWindow is a no-op off Windows (console-window flashing is a Windows-only concern).
func probeNoWindow(cmd *exec.Cmd) {}
