package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"time"

	"proxy429/internal/config"
)

// ---- Activity heatmap (活跃度 heatmap) ----
// heatSlots 15-minute slots in a ring keyed by absolute slot number (unix/900), covering the last 48h.
// Writing a position whose stored slot number is stale zeroes it first, so the ring self-rotates.
const heatSlots = 192

// heatSlot holds one 15-minute cell of activity: finished requests (count_tokens probes excluded; interrupted
// streams counted — the activity happened), output and input tokens, and upstream-direction wire bytes.
type heatSlot struct {
	Slot int64 `json:"slot"` // absolute 15-minute slot number (unix/900); 0 = never written
	Req  int64 `json:"req"`
	Out  int64 `json:"out"`
	In   int64 `json:"in"`
	Up   int64 `json:"up"`
	Down int64 `json:"down"`
}

var (
	heatMu    sync.Mutex // guards heat/heatDirty; its own lock so writers under stats.mu/finishedMu never nest
	heat      [heatSlots]heatSlot
	heatDirty bool // set by heatAdd; the saver loop persists at most once a minute
)

// heatAdd records activity into the slot containing at, zeroing the position first if it holds a stale slot.
func heatAdd(at time.Time, req, out, in, up, down int64) {
	slot := at.Unix() / 900
	heatMu.Lock()
	e := &heat[slot%heatSlots]
	if e.Slot != slot {
		*e = heatSlot{Slot: slot}
	}
	e.Req += req
	e.Out += out
	e.In += in
	e.Up += up
	e.Down += down
	heatDirty = true
	heatMu.Unlock()
}

// heatAt returns the [req, out, up, down] counters of the slot containing t, or zeros when the ring holds a
// stale or never-written slot at that position.
func heatAt(t time.Time) [5]int64 {
	slot := t.Unix() / 900
	heatMu.Lock()
	defer heatMu.Unlock()
	if e := heat[slot%heatSlots]; e.Slot == slot {
		return [5]int64{e.Req, e.Out, e.In, e.Up, e.Down}
	}
	return [5]int64{}
}

// snapshotHeat returns the oldest retained slot number and the 192 slots (oldest-first) for the console heatmap.
func snapshotHeat() (int64, [][5]int64) {
	cur := time.Now().Unix() / 900
	base := cur - heatSlots + 1
	out := make([][5]int64, heatSlots)
	heatMu.Lock()
	for i := range out {
		if e := heat[(base+int64(i))%heatSlots]; e.Slot == base+int64(i) {
			out[i] = [5]int64{e.Req, e.Out, e.In, e.Up, e.Down}
		}
	}
	heatMu.Unlock()
	return base, out
}

// heatFile is the on-disk form of heatmap.dat, persisted next to the active config.json (like the
// active-config state file) so the heatmap survives restarts and exe swaps.
type heatFile struct {
	V     int        `json:"v"`
	Slots []heatSlot `json:"slots"`
}

// heatPath returns the heatmap.dat path beside the active config, or "" when no config path is set (tests).
// A .dat extension (not .json) keeps it out of config.ListConfigFiles results — the same trick as the
// active-config state file; a .json sibling would show up in the tray and web config switchers.
func heatPath() string {
	p := config.CurrentConfigPath()
	if p == "" {
		return ""
	}
	return filepath.Join(filepath.Dir(p), "heatmap.dat")
}

// heatLegacyPath is the pre-rename heatmap.json location, migrated to heatmap.dat on load.
func heatLegacyPath() string {
	p := heatPath()
	if p == "" {
		return ""
	}
	return filepath.Join(filepath.Dir(p), "heatmap.json")
}

// heatSave writes the non-empty slots to heatmap.dat. No-op without a config path.
func heatSave() error {
	p := heatPath()
	if p == "" {
		return nil
	}
	f := heatFile{V: 2} // v2: 15-minute slot scale (v1 half-hour files are rejected on load)
	heatMu.Lock()
	for _, e := range heat {
		if e.Slot != 0 && (e.Req != 0 || e.Out != 0 || e.In != 0 || e.Up != 0 || e.Down != 0) {
			f.Slots = append(f.Slots, e)
		}
	}
	heatMu.Unlock()
	data, err := json.Marshal(f)
	if err != nil {
		return err
	}
	if err := os.WriteFile(p, data, 0644); err != nil {
		return err
	}
	// Best-effort sweep of the pre-rename legacy file so it stops showing up in config switchers.
	_ = os.Remove(heatLegacyPath())
	return nil
}

// heatLoad restores slots inside the 48h window from heatmap.dat; stale or future slots are dropped, and a
// missing or corrupt file just starts empty. A legacy heatmap.json (from before the rename) is moved to
// heatmap.dat first so its data survives the upgrade.
func heatLoad() error {
	p := heatPath()
	if p == "" {
		return nil
	}
	data, err := os.ReadFile(p)
	if err != nil {
		if os.Rename(heatLegacyPath(), p) == nil {
			data, err = os.ReadFile(p)
		}
		if err != nil {
			return nil
		}
	}
	var f heatFile
	if err := json.Unmarshal(data, &f); err != nil {
		return nil
	}
	if f.V != 2 { // v1 used the half-hour slot scale; its slot numbers are meaningless here
		return nil
	}
	cur := time.Now().Unix() / 900
	heatMu.Lock()
	for _, e := range f.Slots {
		if e.Slot >= cur-heatSlots+1 && e.Slot <= cur {
			heat[e.Slot%heatSlots] = e
		}
	}
	heatMu.Unlock()
	return nil
}

// heatSaverLoop persists a dirty heatmap once a minute; started once at startup.
func heatSaverLoop() {
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()
	for range ticker.C {
		heatMu.Lock()
		dirty := heatDirty
		heatMu.Unlock()
		if !dirty {
			continue
		}
		if err := heatSave(); err == nil {
			heatMu.Lock()
			heatDirty = false
			heatMu.Unlock()
		}
	}
}
