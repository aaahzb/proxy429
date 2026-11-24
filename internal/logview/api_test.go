package logview

import (
	"encoding/json"
	"net/http/httptest"
	"proxy429/internal/recorder"
	"testing"
	"time"
)

// TestLogDataHeat pins the console contract: /__logs/data carries heatBase (the absolute slot number of
// heat[0]) and a 192-entry heat array ordered oldest-first, each entry
// [requests, output tokens, input tokens, bytes up, bytes down].
func TestLogDataHeat(t *testing.T) {
	recorder.HeatMu.Lock()
	recorder.Heat = [recorder.HeatSlots]recorder.HeatSlot{}
	recorder.HeatMu.Unlock()
	now := time.Now()
	recorder.HeatAdd(now, 3, 30, 300, 3000, 30000)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/__logs/data", nil)
	req.RemoteAddr = "127.0.0.1:1234" // isLocalRequest gate
	logDataHandler(rec, req)
	var d struct {
		HeatBase int64      `json:"heatBase"`
		Heat     [][5]int64 `json:"heat"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &d); err != nil {
		t.Fatalf("logData JSON: %v", err)
	}
	cur := now.Unix() / 900
	if d.HeatBase != cur-recorder.HeatSlots+1 {
		t.Errorf("heatBase=%d, want %d", d.HeatBase, cur-recorder.HeatSlots+1)
	}
	if len(d.Heat) != recorder.HeatSlots {
		t.Fatalf("heat 长度=%d, want %d", len(d.Heat), recorder.HeatSlots)
	}
	if got := d.Heat[recorder.HeatSlots-1]; got != [5]int64{3, 30, 300, 3000, 30000} {
		t.Errorf("最新槽=%v, want [3 30 300 3000 30000]", got)
	}
}

// TestLogDataClassifierSplit verifies /__logs/data exposes the per-source classifier counters
// (the status card's hover tooltip / click modal read them for the per-source breakdown).
func TestLogDataClassifierSplit(t *testing.T) {
	resetStats()
	recorder.Stats.ClassifierHits.Store(5)
	recorder.Stats.ClassifierRewrites.Store(3)
	recorder.Stats.ClassifierHitsCC.Store(3)
	recorder.Stats.ClassifierRewritesCC.Store(1)
	recorder.Stats.ClassifierHitsCodex.Store(2)
	recorder.Stats.ClassifierRewritesCodex.Store(2)

	req := httptest.NewRequest("GET", "/__logs/data", nil)
	req.RemoteAddr = "127.0.0.1:1234" // isLocalRequest gate
	rec := httptest.NewRecorder()
	logDataHandler(rec, req)
	if rec.Code != 200 {
		t.Fatalf("status=%d, want 200", rec.Code)
	}
	var d map[string]interface{}
	if err := json.NewDecoder(rec.Body).Decode(&d); err != nil {
		t.Fatalf("decode: %v", err)
	}
	for k, want := range map[string]float64{
		"classifiers": 5, "classifierNoThink": 3,
		"classifiersCC": 3, "classifierRewritesCC": 1,
		"classifiersCodex": 2, "classifierRewritesCodex": 2,
	} {
		if got, ok := d[k].(float64); !ok || got != want {
			t.Errorf("%s=%v, want %v", k, d[k], want)
		}
	}
}

// TestRecentFlightsNoStopField verifies the finished-streams JSON carries the noStop flag for the web badge.
func TestRecentFlightsNoStopField(t *testing.T) {
	resetStats()
	oldFinished := recorder.Finished
	defer func() { recorder.Finished = oldFinished }()
	recorder.Finished = []recorder.FinishedFlight{{ID: 1, NoStop: true}}

	req := httptest.NewRequest("GET", "/__recentflights", nil)
	req.RemoteAddr = "127.0.0.1:1"
	rec := httptest.NewRecorder()
	RecentFlightsHandler(rec, req)
	var d struct {
		List []struct {
			NoStop bool `json:"noStop"`
		} `json:"list"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &d); err != nil {
		t.Fatalf("响应非 JSON: %v", err)
	}
	if len(d.List) != 1 || !d.List[0].NoStop {
		t.Errorf("recentFlights 应带 noStop:true，实际 %s", rec.Body.String())
	}
}

// TestRecentFlightsCompactionFields verifies the finished-streams JSON carries the compaction badge flag and the
// compact result derived from the stream's usage; a row without parsed usage keeps an empty result (no numbers),
// and a non-compaction row never carries a result even when its usage would produce one.
func TestRecentFlightsCompactionFields(t *testing.T) {
	resetStats()
	oldFinished := recorder.Finished
	defer func() { recorder.Finished = oldFinished }()
	recorder.Finished = []recorder.FinishedFlight{
		{ID: 1, Compaction: true, InTokens: 1000, CacheRead: 169000, OutTokens: 20000},
		{ID: 2, Compaction: true},
		{ID: 3, InTokens: 1000, CacheRead: 169000, OutTokens: 20000},
	}

	req := httptest.NewRequest("GET", "/__recentflights", nil)
	req.RemoteAddr = "127.0.0.1:1"
	rec := httptest.NewRecorder()
	RecentFlightsHandler(rec, req)
	var d struct {
		List []struct {
			ID          uint64 `json:"id"`
			Compaction  bool   `json:"compaction"`
			CompactSave string `json:"compactSave"`
		} `json:"list"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &d); err != nil {
		t.Fatalf("响应非 JSON: %v", err)
	}
	if len(d.List) != 3 {
		t.Fatalf("recentFlights 应有 3 行，实际 %s", rec.Body.String())
	}
	// Newest first: ID 3 (plain), ID 2 (no usage), ID 1 (170k->20k, 88%).
	if got := d.List[0]; got.ID != 3 || got.Compaction || got.CompactSave != "" {
		t.Errorf("non-compaction row = %+v, want compaction false and empty compactSave", got)
	}
	if got := d.List[1]; got.ID != 2 || !got.Compaction || got.CompactSave != "" {
		t.Errorf("row without usage = %+v, want compaction true and empty compactSave", got)
	}
	if got := d.List[2]; got.ID != 1 || !got.Compaction || got.CompactSave != "170k->20k, 88%" {
		t.Errorf("row with usage = %+v, want compaction true and compactSave 170k->20k, 88%%", got)
	}
}

// resetStats clears the global recorder state so tests don't affect each other.
func resetStats() {
	recorder.Stats.Mu.Lock()
	recorder.Stats.Active = 0
	recorder.Stats.Waiting = 0
	recorder.Stats.CacheRead = 0
	recorder.Stats.CacheCreation = 0
	recorder.Stats.InputTokens = 0
	recorder.Stats.OutputTokens = 0
	recorder.Stats.BytesUp.Store(0)
	recorder.Stats.BytesDown.Store(0)
	recorder.Stats.StatusRetries.Store(0)
	recorder.Stats.ClassifierRewrites.Store(0)
	recorder.Stats.ClassifierHits.Store(0)
	recorder.Stats.ClassifierHitsCC.Store(0)
	recorder.Stats.ClassifierHitsCodex.Store(0)
	recorder.Stats.ClassifierRewritesCC.Store(0)
	recorder.Stats.ClassifierRewritesCodex.Store(0)
	recorder.Stats.ResetSampleCap(0)
	recorder.Stats.Mu.Unlock()

	recorder.Flights.Mu.Lock()
	recorder.Flights.M = make(map[uint64]*recorder.Flight)
	recorder.Flights.NextID.Store(0)
	recorder.Flights.Mu.Unlock()

	recorder.LogBuf.Mu.Lock()
	recorder.LogBuf.Lines = make([]string, 0, recorder.MaxLogBuf)
	recorder.LogBuf.Head = 0
	recorder.LogBuf.Len = 0
	recorder.LogBuf.Mu.Unlock()

	recorder.FinishedMu.Lock()
	recorder.Finished = nil
	recorder.CacheObsMap = map[string]*recorder.CacheObsEntry{}
	recorder.FinishedMu.Unlock()
}
