package main

import (
	"sort"
	"strings"
	"time"
)

// Observed upstream cache-lifetime measurement (status page 「缓存命中」 popup's observed table; display-only, in-memory).
//
// Two adjacent finished streams of the same session + same upstream (obsKey = convID|upstreamKey) yield one observation:
//   - latter hit rate ≥95% → alive observation: the cache lived at least as long as the interval between the two streams' starts; take max as the observed lower bound;
//   - former had hits + latter hit rate <50% → dead observation: the cache did not survive that interval; take min as the observed upper bound
//     (strict zero not required: the upstream caches common prefixes like system prompts, so scattered hits can persist after the session cache dies);
//   - the middle band (50%~95%) is not observed: can't tell cache expiry from input drift;
//   - input volume <1024 tokens is not observed: small requests have noisy hit rates;
//   - interval ≤0 (clock jumps etc.) is not observed;
//   - on contradiction (the upstream cache lifetime changed mid-way, or the cache was evicted early), the newer observation wins
//     and the refuted side is discarded and re-measured — the display can never invert (e.g. "≥18min, <16min").
// It measures a lower bound, not the true TTL: a conversation the user stops following up on never produces a death signal.
// cacheObsMap shares finishedMu with finished and is cleared together by clearStats (restarting clears it naturally).

type cacheObsKind int

const (
	obsNone  cacheObsKind = iota // Does not constitute an observation
	obsAlive                     // Alive observation: the cache lived at least as long as the interval
	obsDead                      // Dead observation: the cache did not survive the interval
)

const cacheAliveHitRate = 0.95 // A latter-stream hit rate ≥95% counts as "still within the cache"

const cacheDeadHitRate = 0.50 // Former had hits + latter hit rate <50% counts as cache lost (residual hits on common prefixes like system prompts don't count as alive)

const cacheObsMinTokens = 1024 // Input volume (input+cacheRead+cacheCreation) below this is not observed

const cacheObsMaxKeyLen = 512 // Truncate abnormally long obsKeys (malicious session_id) to prevent memory bloat

type cacheObsEntry struct {
	aliveMax time.Duration // Max alive-observation interval (observed lower bound: the cache survived at least this long)
	deadMin  time.Duration // Min dead-observation interval (observed upper bound: the cache did not survive this long); 0 = no dead observation yet
	samples  int           // Observation count (alive + dead)

	// aliveAt/deadAt are the moments each bound's value last changed: reset when the bound's value changes (including contradiction invalidation),
	// and the invalidated-to-remeasure side is zeroed — when a bound doesn't exist, its formation time doesn't either.
	aliveAt time.Time
	deadAt  time.Time
}

var cacheObsMap = map[string]*cacheObsEntry{} // Key = upstreamKey (url|model); guarded by finishedMu

// classifyCacheObservation decides what observation a finished stream forms relative to the previous one of the same session + same upstream.
// Death doesn't require a strict zero (the upstream caches common prefixes like system prompts, so scattered cache_read
// can persist after the session cache expires); a hit rate <50% counts as "cache lost". When it returns obsNone, the second return value is meaningless.
func classifyCacheObservation(prevCacheRead, curCacheRead, curInput, curCacheCreation int64, interval time.Duration) (cacheObsKind, time.Duration) {
	if interval <= 0 || curInput+curCacheRead+curCacheCreation < cacheObsMinTokens {
		return obsNone, 0
	}
	hitRate := float64(curCacheRead) / float64(curInput+curCacheRead+curCacheCreation)
	// Check death first: the former having hits proves the cache was written; the latter's hit rate <50% proves the session cache didn't survive until now
	// (the two branches are mutually exclusive: a hit rate can't be both <50% and ≥95%; the order is only for readability).
	if prevCacheRead > 0 && hitRate < cacheDeadHitRate {
		return obsDead, interval
	}
	if hitRate >= cacheAliveHitRate {
		return obsAlive, interval
	}
	return obsNone, 0
}

// recordCacheObsLocked accumulates one observation into the upstream's entry: alive takes max (lower bound only rises), dead takes min (upper bound only falls).
// On contradiction the newer observation wins and the refuted side is discarded and re-measured: if the upstream cache lifetime grew mid-way (a new alive observation crossed the old upper bound),
// the upper bound is discarded; if it shrank or the cache was evicted early (a new dead observation fell below the old lower bound), the lower bound is discarded.
// Each bound's formation time is accounted separately: it moves only when the bound's value changes (including being invalidated to zero), not when a supporting observation is merely added.
// Invariant: when both sides are non-zero, aliveMax < deadMin (the display never inverts). Caller must hold finishedMu.
func recordCacheObsLocked(key string, kind cacheObsKind, interval time.Duration) {
	e := cacheObsMap[key]
	if e == nil {
		e = &cacheObsEntry{}
		cacheObsMap[key] = e
	}
	now := time.Now()
	if kind == obsAlive {
		if interval > e.aliveMax {
			e.aliveMax = interval
			e.aliveAt = now // Restart the clock only when the lower bound's value changes
		}
		if e.deadMin > 0 && e.deadMin <= e.aliveMax {
			e.deadMin = 0 // Old upper bound refuted by new alive evidence (TTL grew / old upper bound was noise) — discard and re-measure
			e.deadAt = time.Time{}
			e.samples = 0 // Observation count zeroed and restarted in step: only observations supporting the current bounds count
		}
	}
	if kind == obsDead {
		if e.deadMin == 0 || interval < e.deadMin {
			e.deadMin = interval
			e.deadAt = now // Restart the clock only when the upper bound's value changes
		}
		if e.aliveMax >= e.deadMin {
			e.aliveMax = 0 // Old lower bound refuted by new dead evidence (TTL shrank / early eviction) — discard and re-measure
			e.aliveAt = time.Time{}
			e.samples = 0 // Observation count zeroed and restarted in step: only observations supporting the current bounds count
		}
	}
	e.samples++
}

// obsKeyOf returns the observed cache-lifetime pairing key (convID|upstreamKey); an empty string means "not observed" when either side is missing (count_tokens probes,
// bare API calls without metadata, missing upstream grouping key).
func obsKeyOf(f *flight) string {
	if f.convID == "" || f.upstreamKey == "" {
		return ""
	}
	k := f.convID + "|" + f.upstreamKey
	if len(k) > cacheObsMaxKeyLen {
		k = k[:cacheObsMaxKeyLen]
	}
	return k
}

// cacheObsRow is a JSON snapshot entry of observed cache lifetimes (for the cache-hit popup's 「实测缓存时间」 table).
type cacheObsRow struct {
	URL     string `json:"url"`     // Upstream base URL (scheme stripped to shorten the display)
	Model   string `json:"model"`   // Model actually sent
	Alive   string `json:"alive"`   // Lower-bound reading (e.g. "23min"); "" when there is no alive observation
	Dead    string `json:"dead"`    // Upper-bound reading; "" when there is no dead observation
	Samples int    `json:"samples"` // Observation count (alive + dead)

	// AliveAge/DeadAge are how long each bound's value has stood (the clock restarts when the bound's value changes), counting up as m:ss;
	// "" when the corresponding bound has no observation (Alive/Dead is "").
	AliveAge string `json:"aliveAge"`
	DeadAge  string `json:"deadAge"`
}

// snapshotCacheObs returns observed cache-lifetime snapshots for all upstreams, sorted by URL+model (stable display).
func snapshotCacheObs() []cacheObsRow {
	finishedMu.Lock()
	defer finishedMu.Unlock()
	if len(cacheObsMap) == 0 {
		return nil
	}
	now := time.Now()
	out := make([]cacheObsRow, 0, len(cacheObsMap))
	for key, e := range cacheObsMap {
		url, model, _ := strings.Cut(key, "|")
		url = strings.TrimPrefix(url, "https://")
		url = strings.TrimPrefix(url, "http://")
		row := cacheObsRow{URL: url, Model: model, Samples: e.samples}
		if e.aliveMax > 0 {
			row.Alive = fmtObsDur(e.aliveMax)
			if !e.aliveAt.IsZero() {
				row.AliveAge = fmtCacheAge(now.Sub(e.aliveAt))
			}
		}
		if e.deadMin > 0 {
			row.Dead = fmtObsDur(e.deadMin)
			if !e.deadAt.IsZero() {
				row.DeadAge = fmtCacheAge(now.Sub(e.deadAt))
			}
		}
		out = append(out, row)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].URL != out[j].URL {
			return out[i].URL < out[j].URL
		}
		return out[i].Model < out[j].Model
	})
	return out
}
