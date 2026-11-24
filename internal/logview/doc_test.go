package logview

import (
	"encoding/json"
	"regexp"
	"strings"
	"testing"

	"proxy429/internal/config"
)

var docSecRe = regexp.MustCompile(`<section class="docSec" id="([^"]+)" data-group="([^"]+)">`)

// docSections lists (id, group) in document order.
func docSections(body string) [][2]string {
	var out [][2]string
	for _, m := range docSecRe.FindAllStringSubmatch(body, -1) {
		out = append(out, [2]string{m[1], m[2]})
	}
	return out
}

// groupShape reports how many consecutive sections share each data-group value. Names differ per
// language, but the partition must not: the nav is generated from these boundaries.
func groupShape(secs [][2]string) []int {
	var out []int
	last := ""
	for i, s := range secs {
		if i == 0 || s[1] != last {
			out = append(out, 1)
			last = s[1]
		} else {
			out[len(out)-1]++
		}
	}
	return out
}

func TestDocSectionParity(t *testing.T) {
	zh := docSections(logViewerDocZH)
	en := docSections(logViewerDocEN)
	if len(zh) < 10 || len(en) < 10 {
		t.Fatalf("too few doc sections: zh=%d en=%d", len(zh), len(en))
	}
	if len(zh) != len(en) {
		t.Fatalf("section count mismatch: zh=%d en=%d", len(zh), len(en))
	}
	seen := map[string]bool{}
	for i := range zh {
		if zh[i][0] != en[i][0] {
			t.Errorf("section %d id mismatch: zh=%q en=%q", i, zh[i][0], en[i][0])
		}
		if seen[zh[i][0]] {
			t.Errorf("duplicate section id %q", zh[i][0])
		}
		seen[zh[i][0]] = true
	}
	if got, want := groupShape(zh), groupShape(en); !equalInts(got, want) {
		t.Errorf("group boundaries diverge: zh=%v en=%v", got, want)
	}
	for name, body := range map[string]string{"ZH": logViewerDocZH, "EN": logViewerDocEN} {
		if got := strings.Count(body, "<h3>"); got != len(zh) {
			t.Errorf("%s doc has %d <h3> headings, want one per section (%d)", name, got, len(zh))
		}
	}
}

// docChrome is the docs-browser skeleton the shared JS in page.go depends on; a fragment dropping one of
// these controls would break that language's browser while every other test stayed green.
var docChrome = []string{
	`<div class="docHead">`,
	`<div class="docTitle">`,
	`<input id="docSearch"`,
	`id="docSearchInfo"`,
	`onclick="docClose()"`,
	`<nav class="docNav" id="docNav"></nav>`,
	`<div class="docBody doc" id="docBody">`,
	`id="docNoResult"`,
}

func TestDocBrowserChrome(t *testing.T) {
	for _, side := range []struct{ name, doc string }{{"ZH", logViewerDocZH}, {"EN", logViewerDocEN}} {
		for _, want := range docChrome {
			if !strings.Contains(side.doc, want) {
				t.Errorf("%s doc is missing docs-browser chrome %q", side.name, want)
			}
		}
		if n := strings.Count(side.doc, `<pre data-copy="`); n < 3 {
			t.Errorf("%s doc has %d copyable blocks, want at least 3", side.name, n)
		}
	}
}

var hanRe = regexp.MustCompile(`\p{Han}`)

// TestDocEnglishHasNoHan checks the EN fragment directly: the page-level test strips everything after //
// (JS comments), and doc URL lines like http://... put the rest of the line in that blind spot.
func TestDocEnglishHasNoHan(t *testing.T) {
	loc := hanRe.FindStringIndex(logViewerDocEN)
	if loc == nil {
		return
	}
	lo, hi := loc[0]-40, loc[1]+40
	if lo < 0 {
		lo = 0
	}
	if hi > len(logViewerDocEN) {
		hi = len(logViewerDocEN)
	}
	t.Fatalf("EN doc contains Han text: %q", logViewerDocEN[lo:hi])
}

func equalInts(a, b []int) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// TestDocCoversConfigExampleKeys guards completeness: every key the shipped config example uses must
// be documented as a <code> key in both languages, so adding a config key without docs fails the build.
func TestDocCoversConfigExampleKeys(t *testing.T) {
	var example any
	if err := json.Unmarshal(config.ConfigExampleBytes, &example); err != nil {
		t.Fatalf("config example is not valid JSON: %v", err)
	}
	keys := map[string]bool{}
	var walk func(v any)
	walk = func(v any) {
		switch x := v.(type) {
		case map[string]any:
			for k, child := range x {
				keys[k] = true
				if k == "vars" {
					continue // names inside vars are user-defined
				}
				walk(child)
			}
		case []any:
			for _, child := range x {
				walk(child)
			}
		}
	}
	walk(example)
	if len(keys) < 30 {
		t.Fatalf("walked only %d config keys, example shape changed?", len(keys))
	}
	for _, side := range []struct{ name, doc string }{{"ZH", logViewerDocZH}, {"EN", logViewerDocEN}} {
		for k := range keys {
			if !strings.Contains(side.doc, "<code>"+k+"</code>") {
				t.Errorf("%s doc does not document config key %q", side.name, k)
			}
		}
	}
}
