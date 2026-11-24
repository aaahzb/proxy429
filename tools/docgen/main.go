// Command docgen regenerates internal/logview/doc.go from the tracked console doc fragments in
// docs/console-doc. Run from anywhere inside the repository:
//
//	go run ./tools/docgen          # rewrite internal/logview/doc.go
//	go run ./tools/docgen -check   # exit 1 when doc.go does not match the fragments
package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"proxy429/internal/docgen"
)

func main() {
	check := flag.Bool("check", false, "verify internal/logview/doc.go is up to date instead of writing it")
	flag.Parse()

	root, err := findRoot()
	if err != nil {
		fmt.Fprintln(os.Stderr, "docgen:", err)
		os.Exit(1)
	}
	zh, en, err := docgen.Load(filepath.Join(root, "docs", "console-doc"))
	if err != nil {
		fmt.Fprintln(os.Stderr, "docgen:", err)
		os.Exit(1)
	}
	want := docgen.Render(zh, en)
	out := filepath.Join(root, "internal", "logview", "doc.go")

	if *check {
		got, err := os.ReadFile(out)
		if err != nil {
			fmt.Fprintln(os.Stderr, "docgen:", err)
			os.Exit(1)
		}
		if strings.ReplaceAll(string(got), "\r\n", "\n") != want {
			fmt.Fprintln(os.Stderr, "docgen: internal/logview/doc.go is stale; run go run ./tools/docgen")
			os.Exit(1)
		}
		fmt.Println("docgen: doc.go is in sync with docs/console-doc")
		return
	}
	if err := os.WriteFile(out, []byte(want), 0o644); err != nil {
		fmt.Fprintln(os.Stderr, "docgen:", err)
		os.Exit(1)
	}
	fmt.Printf("docgen: wrote %s (%d bytes)\n", out, len(want))
}

// findRoot walks up from the working directory to the module root (the directory holding go.mod).
func findRoot() (string, error) {
	dir, err := os.Getwd()
	if err != nil {
		return "", err
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", fmt.Errorf("go.mod not found above %s", dir)
		}
		dir = parent
	}
}
