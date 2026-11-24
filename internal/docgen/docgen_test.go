package docgen

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestGeneratedDocInSync is the drift guard: internal/logview/doc.go must be exactly what Render
// produces from the tracked fragments, so a hand edit or a forgotten regeneration fails go test.
func TestGeneratedDocInSync(t *testing.T) {
	zh, en, err := Load(filepath.Join("..", "..", "docs", "console-doc"))
	if err != nil {
		t.Fatalf("load fragments: %v", err)
	}
	want := Render(zh, en)
	raw, err := os.ReadFile(filepath.Join("..", "..", "internal", "logview", "doc.go"))
	if err != nil {
		t.Fatalf("read doc.go: %v", err)
	}
	got := strings.ReplaceAll(string(raw), "\r\n", "\n")
	if got == want {
		return
	}
	i := 0
	for i < len(got) && i < len(want) && got[i] == want[i] {
		i++
	}
	line := 1 + strings.Count(got[:i], "\n")
	lo, hi := i-60, i+60
	if lo < 0 {
		lo = 0
	}
	if hi > len(got) {
		hi = len(got)
	}
	t.Fatalf("internal/logview/doc.go diverges from the fragments at line %d; run go run ./tools/docgen\nnear: %q", line, got[lo:hi])
}
