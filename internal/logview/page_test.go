package logview

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestFlightInfoLastJSON(t *testing.T) {
	b, _ := json.Marshal(flightInfo{ID: 80, Last: 73})
	if !strings.Contains(string(b), `"last":73`) {
		t.Errorf("flightInfo 应携带 last 字段：%s", b)
	}
	b2, _ := json.Marshal(flightInfo{ID: 80})
	if strings.Contains(string(b2), "last") {
		t.Errorf("无父流应省略 last 字段：%s", b2)
	}
}

func TestFlightInfoCompactionJSON(t *testing.T) {
	b, _ := json.Marshal(flightInfo{ID: 80, Compaction: true})
	if !strings.Contains(string(b), `"compaction":true`) {
		t.Errorf("flightInfo 应携带 compaction 字段：%s", b)
	}
	b2, _ := json.Marshal(flightInfo{ID: 80})
	if strings.Contains(string(b2), "compaction") {
		t.Errorf("非压缩流应省略 compaction 字段：%s", b2)
	}
}

func TestLogViewerLineageLastTag(t *testing.T) {
	// The web # column renders the parent tag compactly as #N<-M (HTML-escaped as &lt;- in innerHTML
	// contexts) and the JS reads the last field from both flight lists.
	if !strings.Contains(logViewerHTML, "&lt;-") {
		t.Error("页面缺少 <- 父流标记渲染")
	}
	if !strings.Contains(logViewerHTML, "f.last") {
		t.Error("页面 JS 未读取 last 字段")
	}
}

// TestTokensColumn pins the console's token in/out column layout: a new column between API and
// bytes on in-flight rows, and the bytes column replaced on finished rows with the byte count as
// the cell's hover title.
func TestTokensColumn(t *testing.T) {
	if n := strings.Count(logViewerHTML, "<th>token 入/出"); n != 2 {
		t.Errorf("both stream tables should carry the token in/out header, found %d", n)
	}
	if !strings.Contains(logViewerHTML, "apiCell(f.translated,f.think,f.thinkFrom,f.thinkTo)+tokensCell(f.tokens)+'<td>'+fmtBytes(f.bytes)+'</td><td>'") {
		t.Error("in-flight rows should render the token cell between API and the retained bytes column")
	}
	if !strings.Contains(logViewerHTML, "tokensCell(f.tokens, f.bytes)+'<td>'+(f.total||'-')") {
		t.Error("finished rows should replace the bytes column with the token cell and keep bytes as its hover title")
	}
	en := renderLogViewerEN(logViewerHTML)
	if n := strings.Count(en, "<th>token in/out"); n != 2 {
		t.Errorf("English tables should carry the token in/out header, found %d", n)
	}
	if strings.Contains(en, "<th>token 入/出") {
		t.Error("Chinese token header leaked into the English console")
	}
	if !strings.Contains(en, "Hover a cell to see that stream's byte count") {
		t.Error("English finished header tooltip should explain the hover byte count")
	}
}

func TestCompactionConsoleBadge(t *testing.T) {
	if !strings.Contains(logViewerHTML, "f.compaction") || !strings.Contains(logViewerHTML, "f.compactEffort") {
		t.Error("model cell should render the [Compact] badge from f.compaction and upgrade it from f.compactEffort")
	}
	if !strings.Contains(logViewerHTML, `model = model + '<span style="color:#ce9178">['+esc(f.compactSave)+']</span>'`) {
		t.Error("finished rows should append the compact result after the model name")
	}
	if !strings.Contains(logViewerHTML, `if(f.compaction) model = '<span style="color:#ce9178">[Compact'+(f.compactEffort?'-'+esc(f.compactEffort):'')+']</span>' + model`) {
		t.Error("the [Compact] badge should stay ahead of the model name and carry the configured level")
	}
	en := renderLogViewerEN(logViewerHTML)
	if !strings.Contains(en, "Context-compaction badge") {
		t.Error("English console doc should describe the [Compact] badge")
	}
	if strings.Contains(en, "上下文压缩标记") {
		t.Error("Chinese compaction doc leaked into the English console")
	}
}

func TestFlightInfoCompactEffortJSON(t *testing.T) {
	b, _ := json.Marshal(flightInfo{ID: 1, Compaction: true, CompactEffort: "max"})
	if !strings.Contains(string(b), `"compactEffort":"max"`) {
		t.Errorf("flightInfo should carry compactEffort: %s", b)
	}
	b2, _ := json.Marshal(flightInfo{ID: 1})
	if strings.Contains(string(b2), "compactEffort") {
		t.Errorf("a flight without a compactEffort pin should omit the field: %s", b2)
	}
}

func TestSearchGuardConsoleBadge(t *testing.T) {
	if !strings.Contains(logViewerHTML, `[空搜]`) || !strings.Contains(logViewerHTML, `f.searchGuard`) {
		t.Error("Chinese finished-status cell should contain the red [空搜] badge driven by f.searchGuard")
	}
	en := renderLogViewerEN(logViewerHTML)
	if !strings.Contains(en, `[empty-search]`) {
		t.Error("English console should translate the badge to [empty-search]")
	}
	if strings.Contains(en, `[空搜]`) {
		t.Error("Chinese empty-search badge leaked into the English console")
	}
}
