package main

// lineage_test.go pins the conversation-lineage feature (the web 「#N<-M」 parent tag): a request's parent
// is the earlier same-session flight scoring best on (leading byte-identical elements, then tail-element common
// byte prefix, then full containment, then newest id). Element identity ignores cache_control markers (clients
// migrate breakpoints between turns); interior tail growth is bridged by the tail byte-prefix score.
// The session id (convID) only partitions; the message-hash chain resolves lineage within the partition —
// that split is what makes sub-agent branches and rewinds visible instead of faking one sticky timeline.

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// anthLineageBody builds a minimal Anthropic-shaped body with the given message payloads (ASCII, no quoting needed).
func anthLineageBody(msgs ...string) []byte {
	var sb strings.Builder
	sb.WriteString(`{"model":"m","messages":[`)
	for i, m := range msgs {
		if i > 0 {
			sb.WriteByte(',')
		}
		sb.WriteString(`{"role":"user","content":"` + m + `"}`)
	}
	sb.WriteString(`]}`)
	return []byte(sb.String())
}

// respLineageBody builds a minimal Responses-shaped body (input array instead of messages).
func respLineageBody(items ...string) []byte {
	var sb strings.Builder
	sb.WriteString(`{"model":"m","input":[`)
	for i, it := range items {
		if i > 0 {
			sb.WriteByte(',')
		}
		sb.WriteString(`{"type":"message","content":"` + it + `"}`)
	}
	sb.WriteString(`]}`)
	return []byte(sb.String())
}

// setupLineageTest isolates the two global lineage candidate pools (active registry + finished ring).
func setupLineageTest(t *testing.T) {
	t.Helper()
	flights.mu.Lock()
	oldM := flights.m
	flights.m = make(map[uint64]*flight)
	flights.mu.Unlock()
	finishedMu.Lock()
	oldFinished := finished
	finished = nil
	finishedMu.Unlock()
	t.Cleanup(func() {
		flights.mu.Lock()
		flights.m = oldM
		flights.mu.Unlock()
		finishedMu.Lock()
		finished = oldFinished
		finishedMu.Unlock()
	})
}

// regLineageFlight registers a flight and runs the same lineage hook the request handler runs.
func regLineageFlight(t *testing.T, id uint64, convID string, body []byte) *flight {
	t.Helper()
	f := &flight{id: id, convID: convID, start: time.Now()}
	flights.register(f)
	computeLineage(f, body)
	return f
}

func TestLineageLinearChain(t *testing.T) {
	setupLineageTest(t)
	f1 := regLineageFlight(t, 1, "s", anthLineageBody("A"))
	if f1.lastFlight != 0 {
		t.Errorf("首请求 last=%d 应=0", f1.lastFlight)
	}
	f2 := regLineageFlight(t, 2, "s", anthLineageBody("A", "asst1", "B"))
	if f2.lastFlight != 1 {
		t.Errorf("#2 last=%d 应=1", f2.lastFlight)
	}
	f3 := regLineageFlight(t, 3, "s", anthLineageBody("A", "asst1", "B", "asst2", "C"))
	if f3.lastFlight != 2 {
		t.Errorf("#3 last=%d 应=2", f3.lastFlight)
	}
}

func TestLineageSubagentBranchSameSession(t *testing.T) {
	// Sub-agents inherit the session id but run disjoint content: the root links nowhere, its rounds link
	// within the sub-chain, and the main chain resumes across the gap — a tree, not one sticky line.
	setupLineageTest(t)
	regLineageFlight(t, 1, "s", anthLineageBody("A"))       // main #1
	f2 := regLineageFlight(t, 2, "s", anthLineageBody("P")) // sub-agent root: disjoint first message
	if f2.lastFlight != 0 {
		t.Errorf("子代理根 last=%d 应=0", f2.lastFlight)
	}
	f3 := regLineageFlight(t, 3, "s", anthLineageBody("P", "sub1")) // sub-agent round 2
	if f3.lastFlight != 2 {
		t.Errorf("子链 #3 last=%d 应=2", f3.lastFlight)
	}
	f4 := regLineageFlight(t, 4, "s", anthLineageBody("A", "asst1", "B")) // main chain resumes across the gap
	if f4.lastFlight != 1 {
		t.Errorf("主链 #4 last=%d 应=1", f4.lastFlight)
	}
}

func TestLineageRetryIdenticalBody(t *testing.T) {
	setupLineageTest(t)
	regLineageFlight(t, 1, "s", anthLineageBody("A", "B"))
	f2 := regLineageFlight(t, 2, "s", anthLineageBody("A", "B")) // client re-sent the identical body
	if f2.lastFlight != 1 {
		t.Errorf("重试 #2 last=%d 应=1", f2.lastFlight)
	}
	f3 := regLineageFlight(t, 3, "s", anthLineageBody("A", "B", "C")) // #1 and #2 both fully contained: newest wins
	if f3.lastFlight != 2 {
		t.Errorf("#3 last=%d 应=2（并列取最新）", f3.lastFlight)
	}
}

func TestLineageNoSession(t *testing.T) {
	setupLineageTest(t)
	regLineageFlight(t, 1, "", anthLineageBody("A"))
	f2 := regLineageFlight(t, 2, "", anthLineageBody("A", "B"))
	if f2.lastFlight != 0 {
		t.Errorf("无会话标识不应定父，last=%d", f2.lastFlight)
	}
}

func TestLineageCrossSessionSameContent(t *testing.T) {
	setupLineageTest(t)
	regLineageFlight(t, 1, "s1", anthLineageBody("A"))
	f2 := regLineageFlight(t, 2, "s2", anthLineageBody("A", "B")) // same content, another session
	if f2.lastFlight != 0 {
		t.Errorf("跨会话同内容不应定父，last=%d", f2.lastFlight)
	}
}

func TestLineageParentAlreadyFinished(t *testing.T) {
	// The usual case: the parent request completed (and left the active registry) before the child arrives.
	setupLineageTest(t)
	f1 := regLineageFlight(t, 1, "s", anthLineageBody("A"))
	flights.unregister(1)
	finishedMu.Lock()
	finished = append(finished, finishedFlight{id: 1, convID: "s", msgHashes: f1.msgHashes})
	finishedMu.Unlock()
	f2 := regLineageFlight(t, 2, "s", anthLineageBody("A", "B"))
	if f2.lastFlight != 1 {
		t.Errorf("父流已归档 last=%d 应=1", f2.lastFlight)
	}
}

func TestLineageResponsesInputArray(t *testing.T) {
	setupLineageTest(t)
	regLineageFlight(t, 1, "conv-1", respLineageBody("a"))
	f2 := regLineageFlight(t, 2, "conv-1", respLineageBody("a", "b", "c"))
	if f2.lastFlight != 1 {
		t.Errorf("Responses input 链 last=%d 应=1", f2.lastFlight)
	}
}

func TestLineageDegenerateBodies(t *testing.T) {
	setupLineageTest(t)
	f1 := regLineageFlight(t, 1, "s", []byte("not json"))
	if len(f1.msgHashes) != 0 || f1.lastFlight != 0 {
		t.Errorf("非 JSON 不应产生血缘：hashes=%d last=%d", len(f1.msgHashes), f1.lastFlight)
	}
	f2 := regLineageFlight(t, 2, "s", []byte(`{"model":"m"}`))
	if len(f2.msgHashes) != 0 || f2.lastFlight != 0 {
		t.Errorf("无 messages/input 不应产生血缘：hashes=%d last=%d", len(f2.msgHashes), f2.lastFlight)
	}
}

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

// anthRawBody builds an Anthropic-shaped body from raw message JSON literals (for tests that
// need block-array content, cache_control markers, etc. — things anthLineageBody can't express).
func anthRawBody(msgs ...string) []byte {
	return []byte(`{"model":"m","messages":[` + strings.Join(msgs, ",") + `]}`)
}

func TestLineageCacheControlMigration(t *testing.T) {
	// Claude Code migrates ephemeral cache breakpoints between turns: the same logical element
	// arrives carrying the marker in one request and bare in the next. Hashing must ignore markers,
	// or every such turn breaks the chain (this is what killed lineage in production).
	setupLineageTest(t)
	regLineageFlight(t, 1, "s", anthRawBody(`{"role":"user","content":[{"type":"text","text":"A","cache_control":{"type":"ephemeral"}}]}`))
	f2 := regLineageFlight(t, 2, "s", anthRawBody(
		`{"role":"user","content":[{"type":"text","text":"A"}]}`,
		`{"role":"assistant","content":[{"type":"text","text":"a1"}]}`,
	))
	if f2.lastFlight != 1 {
		t.Errorf("断点迁移不应断链：#2 last=%d 应=1", f2.lastFlight)
	}
	// Marker in first position and carrying a ttl member — same normalization must apply.
	regLineageFlight(t, 3, "s", anthRawBody(`{"role":"user","content":[{"cache_control":{"type":"ephemeral","ttl":"1h"},"type":"text","text":"B"}]}`))
	f4 := regLineageFlight(t, 4, "s", anthRawBody(
		`{"role":"user","content":[{"type":"text","text":"B"}]}`,
		`{"role":"assistant","content":[{"type":"text","text":"b1"}]}`,
	))
	if f4.lastFlight != 3 {
		t.Errorf("首键位置/ttl 标记不应断链：#4 last=%d 应=3", f4.lastFlight)
	}
}

func TestLineageTailElementGrowth(t *testing.T) {
	// Between turns the client grows the LAST message's content array interiorly (new blocks land
	// before a trailing block, so the grown element is not even a byte prefix extension). The grown
	// parent must outrank a shorter fully-contained ancestor via the tail element's common byte prefix.
	setupLineageTest(t)
	regLineageFlight(t, 1, "s", anthRawBody(`{"role":"user","content":[{"type":"text","text":"root"}]}`))
	regLineageFlight(t, 2, "s", anthRawBody(
		`{"role":"user","content":[{"type":"text","text":"root"}]}`,
		`{"role":"user","content":[{"type":"text","text":"X"},{"type":"text","text":"TAILBLOCK"}]}`,
	))
	f3 := regLineageFlight(t, 3, "s", anthRawBody(
		`{"role":"user","content":[{"type":"text","text":"root"}]}`,
		`{"role":"user","content":[{"type":"text","text":"X"},{"type":"text","text":"NEWBLOCK"},{"type":"text","text":"TAILBLOCK"}]}`,
	))
	if f3.lastFlight != 2 {
		t.Errorf("尾元素内部生长应链向生长父：#3 last=%d 应=2", f3.lastFlight)
	}
}

func TestLineageRewindPrefersContainedAncestor(t *testing.T) {
	// Rewind (fork off an earlier point): the child shares a 2-element prefix with both the branch
	// tip [A,B,C,D] and the forked-from state [A,B]; equal scores must prefer the fully contained one.
	setupLineageTest(t)
	regLineageFlight(t, 1, "s", anthLineageBody("A", "B", "C", "D"))
	regLineageFlight(t, 2, "s", anthLineageBody("A", "B"))
	f3 := regLineageFlight(t, 3, "s", anthLineageBody("A", "B", "X"))
	if f3.lastFlight != 2 {
		t.Errorf("回溯分叉应链向被完整包含的祖先：#3 last=%d 应=2", f3.lastFlight)
	}
}

func TestLineageShapePrefixNotEnough(t *testing.T) {
	// Two unrelated same-session openings share the JSON shape's leading bytes but no whole element:
	// zero full elements must never link, no matter how long the tail byte prefix looks.
	setupLineageTest(t)
	regLineageFlight(t, 1, "s", anthLineageBody("A"))
	f2 := regLineageFlight(t, 2, "s", anthLineageBody("B"))
	if f2.lastFlight != 0 {
		t.Errorf("仅有 JSON 外壳字节相同不应定父：#2 last=%d 应=0", f2.lastFlight)
	}
}
