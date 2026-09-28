//go:build windows

package main

import (
	"os/exec"
	"syscall"
)

// probeNoWindow keeps a periodic probe (reg query) from flashing a console window
// in the GUI-subsystem build.
func probeNoWindow(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: 0x08000000} // CREATE_NO_WINDOW
}
