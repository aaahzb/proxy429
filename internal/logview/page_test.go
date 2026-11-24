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

func TestCompactionConsoleBadge(t *testing.T) {
	if !strings.Contains(logViewerHTML, "[Comp]") || !strings.Contains(logViewerHTML, "f.compaction") {
		t.Error("model cell should render the [Comp] badge from f.compaction")
	}
	if !strings.Contains(logViewerHTML, `model = model + '<span style="color:#ce9178">['+esc(f.compactSave)+']</span>'`) {
		t.Error("finished rows should append the compact result after the model name")
	}
	if !strings.Contains(logViewerHTML, `if(f.compaction) model = '<span style="color:#ce9178">[Comp]</span>' + model`) {
		t.Error("the [Comp] badge should stay ahead of the model name")
	}
	en := renderLogViewerEN(logViewerHTML)
	if !strings.Contains(en, "Context-compaction badge") {
		t.Error("English console doc should describe the [Comp] badge")
	}
	if strings.Contains(en, "上下文压缩标记") {
		t.Error("Chinese compaction doc leaked into the English console")
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
