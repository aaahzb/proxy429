package main

import (
	"os"
	"testing"
)

// TestMain wires the console callbacks before any test runs, mirroring main()'s startup order:
// tests drive console handlers in-process and assert their effects on the proxy core and tray.
func TestMain(m *testing.M) {
	configureConsole()
	os.Exit(m.Run())
}
