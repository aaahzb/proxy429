// Package docgen renders the embedded console usage doc (internal/logview/doc.go) from the tracked
// HTML fragments in docs/console-doc. The fragments are the single source of truth: tools/docgen
// writes doc.go from them, and TestGeneratedDocInSync fails whenever the checked-in file drifts.
package docgen

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Load reads doc-zh.html and doc-en.html from dir, normalising CRLF line endings so the render is
// identical on a CRLF checkout and in CI.
func Load(dir string) (zh, en string, err error) {
	if zh, err = loadFragment(filepath.Join(dir, "doc-zh.html")); err != nil {
		return "", "", err
	}
	if en, err = loadFragment(filepath.Join(dir, "doc-en.html")); err != nil {
		return "", "", err
	}
	return zh, en, nil
}

func loadFragment(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	s := strings.TrimPrefix(string(data), "\ufeff")
	s = strings.ReplaceAll(s, "\r\n", "\n")
	if strings.ContainsRune(s, '\r') {
		return "", fmt.Errorf("%s: lone CR left after normalising CRLF", path)
	}
	if strings.ContainsRune(s, '`') {
		return "", fmt.Errorf("%s: backtick cannot be embedded in a Go raw string", path)
	}
	if !strings.Contains(s, `<section class="docSec"`) {
		return "", fmt.Errorf("%s: fragment contains no docSec sections", path)
	}
	return strings.TrimRight(s, "\n"), nil
}

// Render returns the complete doc.go file contents for the two fragments.
func Render(zh, en string) string {
	return "package logview\n\n" +
		"// logViewerDocZH is the Chinese usage-doc body, rendered from docs/console-doc/doc-zh.html by\n" +
		"// tools/docgen (go run ./tools/docgen); edit the HTML fragment, not this file, and keep section\n" +
		"// ids and data-group boundaries in lockstep with logViewerDocEN. doc_test.go enforces that\n" +
		"// parity, the fragment/generated sync and the config-example key coverage.\n" +
		"const logViewerDocZH = `" + zh + "`\n\n" +
		"// logViewerDocEN is the English usage-doc body, rendered from docs/console-doc/doc-en.html by\n" +
		"// tools/docgen. The English page render replaces the doc placeholder with it before the\n" +
		"// static-copy replacer runs, so this fragment must contain no Chinese characters.\n" +
		"const logViewerDocEN = `" + en + "`\n"
}
